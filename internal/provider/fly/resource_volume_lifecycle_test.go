package fly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

// volumesFake is an httptest-backed fake of the subset of the Fly Volumes API
// the fly_volume resource drives during a host-migration re-anchor: list (to
// find the fork) and get-by-id (the Update read-back). volumeInfoFrom decodes
// the same wire shape the real client expects, so tests exercise the genuine
// Read/Update/ModifyPlan code paths against flyio.New(WithBaseURL(...)).
type volumesFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	vols    map[string]flyio.VolumeInfo // id -> volume; absent id => 404
	updates []map[string]any            // bodies of PUT /apps/{app}/volumes/{id}, in order
}

func newVolumesFake(t *testing.T) *volumesFake {
	t.Helper()
	f := &volumesFake{vols: make(map[string]flyio.VolumeInfo)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *volumesFake) add(v flyio.VolumeInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vols[v.ID] = v
}

func (f *volumesFake) resource(t *testing.T) *volumeResource {
	t.Helper()
	c, err := flyio.New("test-org", flyio.WithToken("t"), flyio.WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	return &volumeResource{pd: &providerData{client: c, orgSlug: "test-org"}}
}

func (f *volumesFake) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1"), "/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(parts) == 3 && parts[2] == "volumes": // GET /apps/{app}/volumes
		out := make([]map[string]any, 0, len(f.vols))
		for _, v := range f.vols {
			out = append(out, volWire(v))
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 5 && parts[2] == "volumes" && parts[4] == "extend": // PUT /apps/{app}/volumes/{id}/extend
		v, ok := f.vols[parts[3]]
		if !ok {
			writeErr(w, http.StatusNotFound, "volume not found")
			return
		}
		var body struct {
			SizeGB int `json:"size_gb"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		v.SizeGB = body.SizeGB
		f.vols[v.ID] = v
		writeJSON(w, http.StatusOK, map[string]any{"needs_restart": false, "volume": volWire(v)})
	case len(parts) == 4 && parts[2] == "volumes" && r.Method == http.MethodPut: // PUT /apps/{app}/volumes/{id}
		v, ok := f.vols[parts[3]]
		if !ok {
			writeErr(w, http.StatusNotFound, "volume not found")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		f.updates = append(f.updates, body)
		// Fly's validation: an update that carries a retention outside 1-60
		// is refused, and the provider has sent 0 here for an unknown.
		if retention, ok := body["snapshot_retention"].(float64); ok {
			if retention < 1 || retention > 60 {
				writeErr(w, http.StatusBadRequest, "snapshot_retention must be between 1 and 60 inclusive")
				return
			}
			v.SnapshotRetention = int(retention)
		}
		if enabled, ok := body["auto_backup_enabled"].(bool); ok {
			v.AutoBackupEnabled = enabled
		}
		f.vols[v.ID] = v
		writeJSON(w, http.StatusOK, volWire(v))
	case len(parts) == 4 && parts[2] == "volumes": // GET /apps/{app}/volumes/{id}
		v, ok := f.vols[parts[3]]
		if !ok {
			writeErr(w, http.StatusNotFound, "volume not found")
			return
		}
		writeJSON(w, http.StatusOK, volWire(v))
	default:
		http.NotFound(w, r)
	}
}

func volWire(v flyio.VolumeInfo) map[string]any {
	return map[string]any{
		"id":                  v.ID,
		"name":                v.Name,
		"region":              v.Region,
		"size_gb":             v.SizeGB,
		"state":               v.State,
		"zone":                v.Zone,
		"attached_machine_id": v.AttachedMachineID,
		"auto_backup_enabled": v.AutoBackupEnabled,
		"snapshot_retention":  v.SnapshotRetention,
	}
}

// volumeSchema returns the fly_volume schema for building tfsdk.Plan/State.
func volumeSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewVolumeResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func mkVolPlan(t *testing.T, s rschema.Schema, m volumeResourceModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: s}
	if diags := p.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("build plan: %v", diags)
	}
	return p
}

func mkVolState(t *testing.T, s rschema.Schema, m volumeResourceModel) tfsdk.State {
	t.Helper()
	st := tfsdk.State{Schema: s}
	if diags := st.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("build state: %v", diags)
	}
	return st
}

// TestVolumeModifyPlan_ReanchorOnFlip pins the fix for the incident where a
// keeper volume errored "volume does not exist" mid-apply: on the apply that
// first flips readopt_on_host_migration to true, the Read-path re-anchor is
// blind (prior state reads the flag false), so ModifyPlan must re-anchor id
// to the live fork keyed on the *planned* flag before the machine mount
// resolves the reference.
func TestVolumeModifyPlan_ReanchorOnFlip(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t)
	f.add(flyio.VolumeInfo{ID: "vol_old", Name: "keeper_data_3", AttachedMachineID: ""})
	f.add(flyio.VolumeInfo{ID: "vol_new", Name: "keeper_data_3", AttachedMachineID: "m-new", Zone: "b1ce", State: "created"})
	r := f.resource(t)

	// Prior state: tracked vol_old, detached (host migration forked it), flag
	// still false because this is the apply that first enables it.
	state := volumeResourceModel{
		ID:                types.StringValue("vol_old"),
		App:               types.StringValue("app"),
		Name:              types.StringValue("keeper_data_3"),
		SizeGB:            types.Int64Value(10),
		AttachedMachineID: types.StringValue(""),
	}
	// Plan: flag now true (from config), same size.
	plan := state
	plan.ReadoptOnHostMigration = types.BoolValue(true)

	resp := &resource.ModifyPlanResponse{Plan: mkVolPlan(t, s, plan)}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		State: mkVolState(t, s, state),
		Plan:  mkVolPlan(t, s, plan),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan errored: %v", resp.Diagnostics)
	}
	if len(resp.Diagnostics.Warnings()) == 0 {
		t.Error("expected a re-anchor warning naming the old and new volume")
	}
	var out volumeResourceModel
	if diags := resp.Plan.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read planned model: %v", diags)
	}
	if out.ID.ValueString() != "vol_new" {
		t.Errorf("planned id = %q, want vol_new (re-anchored to the fork)", out.ID.ValueString())
	}
	if !out.AttachedMachineID.IsUnknown() {
		t.Errorf("attached_machine_id = %v, want unknown (known after apply)", out.AttachedMachineID)
	}
}

// TestVolumeModifyPlan_NoopWhenAttached pins that the steady state pays no
// ListVolumes probe and leaves the plan alone: a volume the last refresh saw
// still attached did not migrate.
func TestVolumeModifyPlan_NoopWhenAttached(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t) // empty: any list/get would 404, proving no probe
	r := f.resource(t)

	state := volumeResourceModel{
		ID:                types.StringValue("vol_live"),
		App:               types.StringValue("app"),
		Name:              types.StringValue("keeper_data_3"),
		SizeGB:            types.Int64Value(10),
		AttachedMachineID: types.StringValue("m-live"),
	}
	plan := state
	plan.ReadoptOnHostMigration = types.BoolValue(true)

	resp := &resource.ModifyPlanResponse{Plan: mkVolPlan(t, s, plan)}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		State: mkVolState(t, s, state),
		Plan:  mkVolPlan(t, s, plan),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan errored: %v", resp.Diagnostics)
	}
	var out volumeResourceModel
	if diags := resp.Plan.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read planned model: %v", diags)
	}
	if out.ID.ValueString() != "vol_live" {
		t.Errorf("planned id = %q, want vol_live unchanged", out.ID.ValueString())
	}
}

// TestVolumeModifyPlan_SkipsOnForcedReplace pins that a re-anchor never runs
// on a destroy+create plan (e.g. a coincident region change). There state.ID
// is the volume being destroyed, so re-anchoring it onto some live sibling
// would set a planned id that Create can't reproduce. The empty fake proves
// no ListVolumes probe is issued.
func TestVolumeModifyPlan_SkipsOnForcedReplace(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t) // empty: any probe would 404
	r := f.resource(t)

	state := volumeResourceModel{
		ID:                types.StringValue("vol_old"),
		App:               types.StringValue("app"),
		Name:              types.StringValue("keeper_data_3"),
		SizeGB:            types.Int64Value(10),
		AttachedMachineID: types.StringValue(""),
	}
	plan := state
	plan.ReadoptOnHostMigration = types.BoolValue(true)

	resp := &resource.ModifyPlanResponse{
		Plan: mkVolPlan(t, s, plan),
		// Simulate a schema-level RequiresReplace (e.g. region change) already
		// recorded before the resource ModifyPlan hook runs.
		RequiresReplace: path.Paths{path.Root("region")},
	}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		State: mkVolState(t, s, state),
		Plan:  mkVolPlan(t, s, plan),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan errored: %v", resp.Diagnostics)
	}
	var out volumeResourceModel
	if diags := resp.Plan.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read planned model: %v", diags)
	}
	if out.ID.ValueString() != "vol_old" {
		t.Errorf("planned id = %q, want vol_old untouched on a forced replace", out.ID.ValueString())
	}
}

// TestVolumeUpdate_UsesPlanID pins that Update reads back the planned id, not
// the stale state id. After a plan-time re-anchor plan.ID is the fork and
// state.ID is the destroyed pre-fork volume; reading state.ID would 404 and
// fail with "volume disappeared during update".
func TestVolumeUpdate_UsesPlanID(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t)
	// Only the fork exists; the stale id 404s.
	f.add(flyio.VolumeInfo{ID: "vol_new", Name: "keeper_data_3", AttachedMachineID: "m-new", SizeGB: 10, Zone: "b1ce", State: "created"})
	r := f.resource(t)

	plan := volumeResourceModel{
		ID:                types.StringValue("vol_new"), // re-anchored by ModifyPlan
		App:               types.StringValue("app"),
		Name:              types.StringValue("keeper_data_3"),
		SizeGB:            types.Int64Value(10),
		AutoBackupEnabled: types.BoolValue(true),
	}
	state := plan
	state.ID = types.StringValue("vol_old") // stale, already destroyed

	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  mkVolPlan(t, s, plan),
		State: mkVolState(t, s, state),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored (read back the stale state id?): %v", resp.Diagnostics)
	}
	var out volumeResourceModel
	if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if out.ID.ValueString() != "vol_new" {
		t.Errorf("persisted id = %q, want vol_new", out.ID.ValueString())
	}
}

// TestVolumeUpdate_SizeOnlyLeavesRetention pins the fix for a size-only
// change failing with "snapshot_retention must be between 1 and 60": with
// snapshot_retention unset in config, the framework plans the computed
// value unknown on any update, and an unknown Int64 reads as 0. Update
// must extend the volume and send no retention at all.
func TestVolumeUpdate_SizeOnlyLeavesRetention(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t)
	f.add(flyio.VolumeInfo{ID: "vol_1", Name: "ci_cache_1", SizeGB: 50, Zone: "b1ce", State: "created", AutoBackupEnabled: true, SnapshotRetention: 5})
	r := f.resource(t)

	state := volumeResourceModel{
		ID:                types.StringValue("vol_1"),
		App:               types.StringValue("app"),
		Name:              types.StringValue("ci_cache_1"),
		SizeGB:            types.Int64Value(50),
		AutoBackupEnabled: types.BoolValue(true),
		SnapshotRetention: types.Int64Value(5),
	}
	plan := state
	plan.SizeGB = types.Int64Value(100)
	plan.SnapshotRetention = types.Int64Unknown()

	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  mkVolPlan(t, s, plan),
		State: mkVolState(t, s, state),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored (sent the unknown retention as 0?): %v", resp.Diagnostics)
	}
	if len(f.updates) != 0 {
		t.Errorf("PUT /volumes/{id} bodies = %v, want none for a size-only change", f.updates)
	}
	var out volumeResourceModel
	if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if out.SizeGB.ValueInt64() != 100 {
		t.Errorf("persisted size_gb = %d, want 100", out.SizeGB.ValueInt64())
	}
	if out.SnapshotRetention.ValueInt64() != 5 {
		t.Errorf("persisted snapshot_retention = %d, want Fly's 5", out.SnapshotRetention.ValueInt64())
	}
}

// TestVolumeRead_AdoptsForkWhenGone pins the hardening: when the tracked
// volume is gone, Read adopts an unambiguous same-named fork instead of
// removing the resource — even with the flag off — so the flip apply can't
// destructively recreate a live volume.
func TestVolumeRead_AdoptsForkWhenGone(t *testing.T) {
	t.Parallel()
	s := volumeSchema(t)
	f := newVolumesFake(t)
	// vol_old absent (gone); one attached same-named fork present.
	f.add(flyio.VolumeInfo{ID: "vol_new", Name: "keeper_data_3", AttachedMachineID: "m-new", SizeGB: 10, Zone: "b1ce", State: "created"})
	r := f.resource(t)

	state := volumeResourceModel{
		ID:                     types.StringValue("vol_old"),
		App:                    types.StringValue("app"),
		Name:                   types.StringValue("keeper_data_3"),
		SizeGB:                 types.Int64Value(10),
		ReadoptOnHostMigration: types.BoolValue(false), // flag off — still adopts, gone is destructive otherwise
	}

	resp := &resource.ReadResponse{State: mkVolState(t, s, state)}
	r.Read(context.Background(), resource.ReadRequest{State: mkVolState(t, s, state)}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read errored: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("resource was removed from state instead of adopting the fork")
	}
	var out volumeResourceModel
	if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if out.ID.ValueString() != "vol_new" {
		t.Errorf("state id = %q, want vol_new (adopted fork)", out.ID.ValueString())
	}
}
