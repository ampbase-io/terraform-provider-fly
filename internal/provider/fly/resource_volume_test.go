package fly

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

// TestVolumeResource_Schema pins the fly_volume contract that the cluster
// HCL (RFC-021 followup) relies on: app/name/region/size_gb required and
// id/state/zone/attached_machine_id computed.
func TestVolumeResource_Schema(t *testing.T) {
	r := NewVolumeResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	if resp.Schema.Description == "" {
		t.Error("schema description is empty")
	}

	required := []string{"app", "name", "region", "size_gb"}
	for _, name := range required {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("missing required attribute %q", name)
			continue
		}
		switch a := attr.(type) {
		case schema.StringAttribute:
			if !a.Required {
				t.Errorf("attribute %q should be required", name)
			}
		case schema.Int64Attribute:
			if !a.Required {
				t.Errorf("attribute %q should be required", name)
			}
		}
	}

	computed := []string{"id", "state", "attached_machine_id", "zone"}
	for _, name := range computed {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("missing computed attribute %q", name)
			continue
		}
		sa, ok := attr.(schema.StringAttribute)
		if !ok {
			continue
		}
		if !sa.Computed {
			t.Errorf("attribute %q should be computed", name)
		}
	}

	// name/region/encrypted/fstype must require replace — changing them
	// destroys data. size_gb must NOT require replace — it's the in-place
	// extend path. Encrypted's plan modifier is a bool, so we just
	// confirm via attribute existence here and rely on integration tests
	// (a separate PR's acceptance suite) for the replace semantics.
	immutable := []string{"name", "region", "fstype"}
	for _, name := range immutable {
		sa, ok := resp.Schema.Attributes[name].(schema.StringAttribute)
		if !ok {
			continue
		}
		if len(sa.PlanModifiers) == 0 {
			t.Errorf("attribute %q should have RequiresReplace plan modifier", name)
		}
	}

	// size_gb must be mutable in-place (extend path).
	size, ok := resp.Schema.Attributes["size_gb"].(schema.Int64Attribute)
	if !ok {
		t.Fatal("size_gb is not Int64Attribute")
	}
	if len(size.PlanModifiers) != 0 {
		t.Errorf("size_gb should not have plan modifiers (in-place extend); got %d", len(size.PlanModifiers))
	}

	// Host-pinning fix from the docs review: require_unique_zone (default
	// true, the documented Fly HA pattern) and the compute block must be
	// present so the cluster bootstrap can colocate volumes with their
	// machines. compute_image was considered but rejected as weird UX —
	// guest shape is the placement signal that matters.
	if _, ok := resp.Schema.Attributes["require_unique_zone"]; !ok {
		t.Error("missing require_unique_zone attribute (RFC-021 followup docs review)")
	}
	if _, ok := resp.Schema.Attributes["unique_zone_app_wide"]; !ok {
		t.Error("missing unique_zone_app_wide attribute (distinct-named HA pattern)")
	}
	if _, ok := resp.Schema.Blocks["compute"]; !ok {
		t.Error("missing compute block (host-pinning hint)")
	}
}

// TestVolumeResource_ImplementsModifyPlan pins the plan-time shrink
// rejection contract. The reviewer flagged that the prior apply-time
// check meant `tofu plan` showed a clean diff for a shrink, then the
// apply failed — ModifyPlan moves the error to where operators look
// first.
func TestVolumeResource_ImplementsModifyPlan(t *testing.T) {
	var _ resource.ResourceWithModifyPlan = (*volumeResource)(nil)
}

// TestVolumeResource_ImportStateRejectsBadID pins the import-ID format
// contract: "<app>/<volume_id>" or a clear diagnostic.
func TestVolumeResource_ImportStateRejectsBadID(t *testing.T) {
	r := NewVolumeResource().(*volumeResource)
	resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: schema.Schema{}}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "no-slash-here"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("import ID without '/' should error")
	}
	var found bool
	for _, d := range resp.Diagnostics {
		if d.Severity().String() == "Error" && d.Summary() == "Invalid Import ID" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing expected diagnostic, got %v", resp.Diagnostics)
	}
}

// TestPickReplacementVolume pins the host-migration re-anchor rule: adopt
// the fork only when exactly one same-named, currently-attached sibling
// exists. Ambiguity (zero or many) declines rather than guessing, so a
// misfire can never silently point the resource at the wrong volume.
func TestPickReplacementVolume(t *testing.T) {
	t.Parallel()

	const (
		name    = "clickhouse_data_1"
		tracked = "vol_old"
	)

	tests := []struct {
		desc string
		vols []flyio.VolumeInfo
		want string // "" means expect nil
	}{
		{
			desc: "single attached same-named fork is adopted",
			vols: []flyio.VolumeInfo{
				{ID: "vol_old", Name: name, AttachedMachineID: ""},
				{ID: "vol_new", Name: name, AttachedMachineID: "d895903b52d4d8"},
			},
			want: "vol_new",
		},
		{
			desc: "no attached sibling declines",
			vols: []flyio.VolumeInfo{
				{ID: "vol_old", Name: name, AttachedMachineID: ""},
				{ID: "vol_stale", Name: name, AttachedMachineID: ""},
			},
			want: "",
		},
		{
			desc: "two attached same-named siblings is ambiguous",
			vols: []flyio.VolumeInfo{
				{ID: "vol_a", Name: name, AttachedMachineID: "m1"},
				{ID: "vol_b", Name: name, AttachedMachineID: "m2"},
			},
			want: "",
		},
		{
			desc: "attached but different name is ignored",
			vols: []flyio.VolumeInfo{
				{ID: "vol_new", Name: "clickhouse_data_2", AttachedMachineID: "m1"},
			},
			want: "",
		},
		{
			desc: "tracked volume attached does not match itself",
			vols: []flyio.VolumeInfo{
				{ID: "vol_old", Name: name, AttachedMachineID: "m1"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			got := pickReplacementVolume(tc.vols, name, tracked)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("expected no replacement, got %s", got.ID)
			case tc.want != "" && (got == nil || got.ID != tc.want):
				t.Fatalf("expected %s, got %v", tc.want, got)
			}
		})
	}
}

// TestVolumeSchema_HasReadoptAttr pins that the opt-in host-migration
// recovery knob is exposed and optional (not required, so existing HCL
// keeps working).
func TestVolumeSchema_HasReadoptAttr(t *testing.T) {
	t.Parallel()
	r := NewVolumeResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	attr, ok := resp.Schema.Attributes["readopt_on_host_migration"]
	if !ok {
		t.Fatal("readopt_on_host_migration attribute missing")
	}
	if attr.IsRequired() {
		t.Error("readopt_on_host_migration must be optional so existing volumes need no change")
	}
}
