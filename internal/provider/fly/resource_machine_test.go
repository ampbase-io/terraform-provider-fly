package fly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

// TestRefreshMountVolumes pins the Read-time reconciliation of a mount's
// volume ID against what the machine actually mounts — the Fly
// host-migration case where the volume is forked to a new ID out of
// band. Matching is by path; the refresh is gated per-mount on
// readopt_on_host_migration (so it stays in lockstep with the paired
// fly_volume's re-anchor and can't fabricate an illegal in-place volume
// swap); only the volume field moves; name is left alone.
func TestRefreshMountVolumes(t *testing.T) {
	t.Parallel()

	mount := func(path, vol string, readopt bool) mountModel {
		return mountModel{
			Volume:                 types.StringValue(vol),
			Path:                   types.StringValue(path),
			Name:                   types.StringNull(),
			ReadoptOnHostMigration: types.BoolValue(readopt),
		}
	}

	t.Run("opted-in mount adopts forked volume id by path", func(t *testing.T) {
		t.Parallel()
		model := &machineResourceModel{Mount: []mountModel{mount("/var/lib/clickhouse", "vol_old", true)}}
		refreshMountVolumes(model, &flyio.MachineInfo{Mounts: []flyio.MachineMount{
			{Path: "/var/lib/clickhouse", Volume: "vol_new", Name: "clickhouse_data_1"},
		}})
		if got := model.Mount[0].Volume.ValueString(); got != "vol_new" {
			t.Fatalf("volume not reconciled: got %q, want vol_new", got)
		}
		if !model.Mount[0].Name.IsNull() {
			t.Fatalf("name should stay null (server-defaulted), got %q", model.Mount[0].Name.ValueString())
		}
	})

	// The reviewer's item 1: a mount that has NOT opted in must be left
	// exactly as-is, so state never diverges from the paired fly_volume
	// (which likewise won't re-anchor) — otherwise config (mount.volume =
	// fly_volume.id, still the orphan) and state (the fork) disagree and
	// the next apply attempts a mount-volume swap Fly rejects.
	t.Run("un-opted-in mount is left untouched even when forked live", func(t *testing.T) {
		t.Parallel()
		model := &machineResourceModel{Mount: []mountModel{mount("/var/lib/clickhouse", "vol_old", false)}}
		refreshMountVolumes(model, &flyio.MachineInfo{Mounts: []flyio.MachineMount{
			{Path: "/var/lib/clickhouse", Volume: "vol_new"},
		}})
		if got := model.Mount[0].Volume.ValueString(); got != "vol_old" {
			t.Fatalf("un-opted-in mount was refreshed: got %q, want vol_old", got)
		}
	})

	t.Run("leaves volume when path is not reported", func(t *testing.T) {
		t.Parallel()
		model := &machineResourceModel{Mount: []mountModel{mount("/var/lib/clickhouse", "vol_old", true)}}
		refreshMountVolumes(model, &flyio.MachineInfo{Mounts: []flyio.MachineMount{
			{Path: "/some/other/path", Volume: "vol_unrelated"},
		}})
		if got := model.Mount[0].Volume.ValueString(); got != "vol_old" {
			t.Fatalf("unrelated mount changed volume: got %q, want vol_old", got)
		}
	})

	t.Run("no mounts reported is a no-op", func(t *testing.T) {
		t.Parallel()
		model := &machineResourceModel{Mount: []mountModel{mount("/var/lib/clickhouse", "vol_old", true)}}
		refreshMountVolumes(model, &flyio.MachineInfo{})
		if got := model.Mount[0].Volume.ValueString(); got != "vol_old" {
			t.Fatalf("empty API mounts mutated state: got %q, want vol_old", got)
		}
	})

	// The reviewer's item 4: multi-mount machines must not cross-contaminate
	// via the byPath map, and the per-mount gate must be respected
	// independently. Two mounts, both forked live; only the opted-in one
	// (data) moves, the un-opted-in one (logs) stays put.
	t.Run("multi-mount refreshes only opted-in paths", func(t *testing.T) {
		t.Parallel()
		model := &machineResourceModel{Mount: []mountModel{
			mount("/var/lib/clickhouse", "vol_data_old", true),
			mount("/var/log/clickhouse", "vol_logs_old", false),
		}}
		refreshMountVolumes(model, &flyio.MachineInfo{Mounts: []flyio.MachineMount{
			{Path: "/var/lib/clickhouse", Volume: "vol_data_new"},
			{Path: "/var/log/clickhouse", Volume: "vol_logs_new"},
		}})
		if got := model.Mount[0].Volume.ValueString(); got != "vol_data_new" {
			t.Errorf("opted-in data mount: got %q, want vol_data_new", got)
		}
		if got := model.Mount[1].Volume.ValueString(); got != "vol_logs_old" {
			t.Errorf("un-opted-in logs mount: got %q, want vol_logs_old", got)
		}
	})
}

// TestValidateMountUniqueness pins the in-memory duplicate-volume-on-
// same-machine check. The cluster bootstrap pattern uses count.index
// to wire each volume to a unique machine; a typo like "always
// volume[0]" or two mount blocks against the same volume must surface
// a clear diagnostic before the API call.
func TestValidateMountUniqueness(t *testing.T) {
	t.Run("unique mounts pass", func(t *testing.T) {
		diags := validateMountUniqueness([]mountModel{
			{Volume: types.StringValue("vol_a"), Path: types.StringValue("/data1")},
			{Volume: types.StringValue("vol_b"), Path: types.StringValue("/data2")},
		})
		if diags.HasError() {
			t.Errorf("unique mounts should not error: %v", diags)
		}
	})

	t.Run("duplicate volume rejected", func(t *testing.T) {
		diags := validateMountUniqueness([]mountModel{
			{Volume: types.StringValue("vol_a"), Path: types.StringValue("/data1")},
			{Volume: types.StringValue("vol_a"), Path: types.StringValue("/data2")},
		})
		if !diags.HasError() {
			t.Fatal("duplicate volume should error")
		}
		for _, d := range diags {
			if d.Severity().String() == "Error" && d.Summary() == "Duplicate Volume in Mounts" {
				return
			}
		}
		t.Errorf("missing expected error diagnostic, got %v", diags)
	})

	t.Run("empty and single-mount no-op", func(t *testing.T) {
		if diags := validateMountUniqueness(nil); diags.HasError() {
			t.Errorf("nil mounts should not error")
		}
		if diags := validateMountUniqueness([]mountModel{
			{Volume: types.StringValue("vol_a"), Path: types.StringValue("/data")},
		}); diags.HasError() {
			t.Errorf("single mount should not error")
		}
	})
}

// TestValidateMountsUnattached pins the docs-flagged paper cut #2: when
// a volume is already attached to another machine, the second
// machine.Create surfaces a clean diagnostic naming the conflicting
// machine instead of leaking a generic 4xx from Fly. Covers the case
// the user flagged for the shared-volume-name cluster pattern.
func TestValidateMountsUnattached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/v1/apps/app/volumes/vol_free":
			_, _ = w.Write([]byte(`{"id":"vol_free","state":"created","attached_machine_id":""}`))
		case "/v1/apps/app/volumes/vol_taken":
			_, _ = w.Write([]byte(`{"id":"vol_taken","state":"created","attached_machine_id":"mach_existing"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := flyio.New("test-org", flyio.WithToken("t"), flyio.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	r := &machineResource{pd: &providerData{client: c, orgSlug: "test-org"}}

	t.Run("unattached passes", func(t *testing.T) {
		diags := r.validateMountsUnattached(context.Background(), "app", []mountModel{
			{Volume: types.StringValue("vol_free"), Path: types.StringValue("/data")},
		})
		if diags.HasError() {
			t.Errorf("unattached volume should pass: %v", diags)
		}
	})

	t.Run("already attached rejected", func(t *testing.T) {
		diags := r.validateMountsUnattached(context.Background(), "app", []mountModel{
			{Volume: types.StringValue("vol_taken"), Path: types.StringValue("/data")},
		})
		if !diags.HasError() {
			t.Fatal("attached volume should reject")
		}
		// The diagnostic must name the blocking machine so the operator
		// can find the HCL bug — generic "rejected" wouldn't help.
		var found bool
		for _, d := range diags {
			if d.Severity().String() == "Error" &&
				d.Summary() == "Volume Already Attached" &&
				containsStr(d.Detail(), "mach_existing") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected diagnostic naming mach_existing, got %v", diags)
		}
	})
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestMachineSchema_HasNewBlocks pins the RFC-021 follow-up surface area:
// fly_machine must expose files / mounts / restart blocks and a metadata
// attribute so the ClickHouse Keeper+replica cluster bootstrap can be
// expressed in HCL without falling back to fly_machine_update --file-local
// or bash post-processing.
func TestMachineSchema_HasNewBlocks(t *testing.T) {
	r := NewMachineResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	if _, ok := resp.Schema.Attributes["metadata"]; !ok {
		t.Error("missing metadata attribute (fly_process_group lives here)")
	}

	requiredBlocks := []string{"file", "mount", "restart", "container", "metrics", "check"}
	for _, name := range requiredBlocks {
		if _, ok := resp.Schema.Blocks[name]; !ok {
			t.Errorf("missing block %q", name)
		}
	}

	// The RFC-006 Addendum concurrency block is nested inside `service`;
	// assert its presence so a schema regression can't silently drop it while
	// the lifecycle round-trip test still compiles.
	svc, ok := resp.Schema.Blocks["service"].(schema.ListNestedBlock)
	if !ok {
		t.Fatalf("service block is %T, want schema.ListNestedBlock", resp.Schema.Blocks["service"])
	}
	// concurrency must be a ListNestedBlock (size-capped at 1), NOT a
	// SingleNestedBlock: a SingleNestedBlock is always present, so its
	// Required attributes are enforced even when the block is omitted, which
	// broke every service that didn't set concurrency (clickhouse, vault).
	conc, ok := svc.NestedObject.Blocks["concurrency"]
	if !ok {
		t.Fatal("missing service.concurrency block")
	}
	if _, ok := conc.(schema.ListNestedBlock); !ok {
		t.Errorf("service.concurrency is %T, want schema.ListNestedBlock so an omitted block is truly optional", conc)
	}
}

// TestBuildCreateMachineInput_Containers pins the sidecar wiring used
// by the RFC-011 Addendum infra metric collectors: the top-level image
// is the main service, and one or more `container` blocks become
// additional containers in the same firecracker microVM.
func TestBuildCreateMachineInput_Containers(t *testing.T) {
	ctx := context.Background()
	cmd, diags := types.ListValueFrom(ctx, types.StringType, []string{
		"--config", "/etc/otelcol/config.yaml",
	})
	if diags.HasError() {
		t.Fatalf("cmd ListValueFrom: %v", diags)
	}
	env, diags := types.MapValueFrom(ctx, types.StringType, map[string]string{
		"OPAMP_AGENT_KEY": "agent_xxx",
	})
	if diags.HasError() {
		t.Fatalf("env MapValueFrom: %v", diags)
	}

	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-clickhouse-staging"),
		Image: types.StringValue("registry.fly.io/ampbase-clickhouse-staging:latest"),
		Container: []containerModel{
			{
				Name:  types.StringValue("otelcol"),
				Image: types.StringValue("registry.fly.io/ampbase-otelcol-sidecar:latest"),
				Cmd:   cmd,
				Env:   env,
				File: []fileModel{
					{
						GuestPath: types.StringValue("/etc/otelcol/baseline.yaml"),
						RawValue:  types.StringValue(base64.StdEncoding.EncodeToString([]byte("receivers: {}\n"))),
					},
				},
				DependsOn: []dependencyModel{
					{
						Name:      types.StringValue("app"),
						Condition: types.StringValue("started"),
					},
				},
			},
		},
	}

	got := buildCreateMachineInput(plan)
	if got.Config == nil {
		t.Fatal("config nil")
	}
	if len(got.Config.Containers) != 1 {
		t.Fatalf("containers len = %d, want 1", len(got.Config.Containers))
	}
	c := got.Config.Containers[0]
	if c.Name == nil || *c.Name != "otelcol" {
		t.Errorf("container name = %v, want otelcol", c.Name)
	}
	if c.Image == nil || *c.Image != "registry.fly.io/ampbase-otelcol-sidecar:latest" {
		t.Errorf("container image = %v", c.Image)
	}
	if len(c.Cmd) != 2 || c.Cmd[0] != "--config" {
		t.Errorf("container cmd = %v, want [--config /etc/otelcol/config.yaml]", c.Cmd)
	}
	if c.Env["OPAMP_AGENT_KEY"] != "agent_xxx" {
		t.Errorf("container env[OPAMP_AGENT_KEY] = %q, want agent_xxx", c.Env["OPAMP_AGENT_KEY"])
	}
	if len(c.Files) != 1 || c.Files[0].GuestPath == nil || *c.Files[0].GuestPath != "/etc/otelcol/baseline.yaml" {
		t.Errorf("container file = %v, want /etc/otelcol/baseline.yaml", c.Files)
	}
	if len(c.DependsOn) != 1 || c.DependsOn[0].Name == nil || *c.DependsOn[0].Name != "app" {
		t.Errorf("container depends_on = %v, want [app started]", c.DependsOn)
	}
	if c.DependsOn[0].Condition == nil || string(*c.DependsOn[0].Condition) != "started" {
		t.Errorf("container depends_on.condition = %v, want started", c.DependsOn[0].Condition)
	}
}

// TestBuildCreateMachineInput_Metrics pins the [[metrics]] wiring used
// by the addendum's "sidecar inherits host identity for free" pattern:
// the machine-level metrics block tells Fly's scraper which port/path
// on this machine's 6PN address to poll, and Fly stamps the resulting
// series with the host's app/instance/region.
func TestBuildCreateMachineInput_Metrics(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-clickhouse-staging"),
		Image: types.StringValue("img:latest"),
		Metrics: &metricsModel{
			Port: types.Int64Value(9464),
			Path: types.StringValue("/metrics"),
		},
	}
	got := buildCreateMachineInput(plan)
	if got.Config == nil || got.Config.Metrics == nil {
		t.Fatal("metrics nil")
	}
	if got.Config.Metrics.Port == nil || *got.Config.Metrics.Port != 9464 {
		t.Errorf("metrics.port = %v, want 9464", got.Config.Metrics.Port)
	}
	if got.Config.Metrics.Path == nil || *got.Config.Metrics.Path != "/metrics" {
		t.Errorf("metrics.path = %v, want /metrics", got.Config.Metrics.Path)
	}
}

// TestValidateContainerNameUniqueness pins the duplicate-container-
// name guard. Mirrors validateMountUniqueness: Fly's Machines API
// rejects duplicates with a generic error; this surfaces the actual
// cause (which name collided) before the API call.
func TestValidateContainerNameUniqueness(t *testing.T) {
	t.Run("unique names pass", func(t *testing.T) {
		diags := validateContainerNameUniqueness([]containerModel{
			{Name: types.StringValue("otelcol")},
			{Name: types.StringValue("logshipper")},
		})
		if diags.HasError() {
			t.Errorf("unique container names should not error: %v", diags)
		}
	})

	t.Run("duplicate name rejected", func(t *testing.T) {
		diags := validateContainerNameUniqueness([]containerModel{
			{Name: types.StringValue("otelcol")},
			{Name: types.StringValue("otelcol")},
		})
		if !diags.HasError() {
			t.Fatal("duplicate container name should error")
		}
		for _, d := range diags {
			if d.Severity().String() == "Error" &&
				d.Summary() == "Duplicate Container Name" &&
				containsStr(d.Detail(), `"otelcol"`) {
				return
			}
		}
		t.Errorf("missing expected error diagnostic, got %v", diags)
	})

	t.Run("empty and single-container no-op", func(t *testing.T) {
		if diags := validateContainerNameUniqueness(nil); diags.HasError() {
			t.Errorf("nil containers should not error")
		}
		if diags := validateContainerNameUniqueness([]containerModel{
			{Name: types.StringValue("otelcol")},
		}); diags.HasError() {
			t.Errorf("single container should not error")
		}
	})
}

// TestBuildCreateMachineInput_NoContainersUnchanged confirms the single-
// container shape is preserved when no `container` blocks are declared
// — the prior contract for ClickHouse + Keeper machines (which run a
// single image with no sidecar) must keep producing nil Containers so
// the Machines API does not see an unexpected zero-length list.
func TestBuildCreateMachineInput_NoContainersUnchanged(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-keeper-staging"),
		Image: types.StringValue("img:latest"),
	}
	got := buildCreateMachineInput(plan)
	if got.Config.Containers != nil {
		t.Errorf("Containers = %v, want nil when no container blocks", got.Config.Containers)
	}
	if got.Config.Metrics != nil {
		t.Errorf("Metrics = %v, want nil when no metrics block", got.Config.Metrics)
	}
}

// TestBuildUpdateMachineInput_OmitsName pins that UpdateMachineRequest
// intentionally does NOT carry the name field. Fly's API accepts it in
// the schema but silently no-ops renames; sending it triggered
// "Provider produced inconsistent result after apply" against staging
// Vault because the framework compared the planned new name against
// the API-returned old name. The nameImmutableAfterCreate plan
// modifier (tested below) is the user-facing companion fix — it
// suppresses the diff at plan time so this codepath never sees a
// renamed plan in the first place.
func TestBuildUpdateMachineInput_OmitsName(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-vault-staging"),
		Image: types.StringValue("img:latest"),
		Name:  types.StringValue("vault-1"),
	}
	got := buildUpdateMachineInput(plan)
	if got.Name != nil {
		t.Errorf("UpdateMachineRequest.Name = %v, want nil — sending name to Fly's UpdateMachine triggers post-apply consistency errors", got.Name)
	}
}

// TestNameImmutableAfterCreate_PassesThroughOnCreate confirms the
// plan modifier is a no-op when state is null (first apply). Without
// this, every fresh machine would have its name forced to null/empty,
// breaking the predictable `replica-N` / `vault-N` naming we rely on
// for new resources.
func TestNameImmutableAfterCreate_PassesThroughOnCreate(t *testing.T) {
	m := nameImmutableAfterCreate{}
	req := planmodifier.StringRequest{
		ConfigValue: types.StringValue("vault-1"),
		PlanValue:   types.StringValue("vault-1"),
		StateValue:  types.StringNull(),
	}
	resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
	m.PlanModifyString(context.Background(), req, resp)

	if resp.PlanValue.ValueString() != "vault-1" {
		t.Errorf("create-path PlanValue = %q, want vault-1 (config value should flow through unchanged when state is null)", resp.PlanValue.ValueString())
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() > 0 {
		t.Errorf("create path should produce no diagnostics, got %v", resp.Diagnostics)
	}
}

// TestNameImmutableAfterCreate_PinsStateOnUpdate is the load-bearing
// test for the Vault staging adoption path. State has the live name
// (e.g. weathered-frog-7982); config declares a different one
// (vault-1). Without the modifier this would crash the apply with
// "Provider produced inconsistent result after apply". With it, the
// plan silently keeps the state value and emits a warning so the
// operator knows the change was ignored.
func TestNameImmutableAfterCreate_PinsStateOnUpdate(t *testing.T) {
	m := nameImmutableAfterCreate{}
	req := planmodifier.StringRequest{
		ConfigValue: types.StringValue("vault-1"),
		PlanValue:   types.StringValue("vault-1"),
		StateValue:  types.StringValue("weathered-frog-7982"),
	}
	resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
	m.PlanModifyString(context.Background(), req, resp)

	if resp.PlanValue.ValueString() != "weathered-frog-7982" {
		t.Errorf("update-path PlanValue = %q, want weathered-frog-7982 (state value should win over config)", resp.PlanValue.ValueString())
	}
	if resp.Diagnostics.WarningsCount() != 1 {
		t.Errorf("expected one warning about ignored name change, got %d warnings: %v", resp.Diagnostics.WarningsCount(), resp.Diagnostics)
	}
}

// TestNameImmutableAfterCreate_NoWarningWhenConfigMatchesState avoids
// a warning spam case: when config matches state (the operator never
// tried to rename), the modifier should be a quiet no-op even though
// it still pins plan to state.
func TestNameImmutableAfterCreate_NoWarningWhenConfigMatchesState(t *testing.T) {
	m := nameImmutableAfterCreate{}
	req := planmodifier.StringRequest{
		ConfigValue: types.StringValue("vault-1"),
		PlanValue:   types.StringValue("vault-1"),
		StateValue:  types.StringValue("vault-1"),
	}
	resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
	m.PlanModifyString(context.Background(), req, resp)

	if resp.Diagnostics.WarningsCount() != 0 {
		t.Errorf("matching config + state should not warn, got %d warnings: %v", resp.Diagnostics.WarningsCount(), resp.Diagnostics)
	}
}

// TestParseImportID pins the "<app>/<machine_id>" composite parser
// used by ImportState. The composite form is required because Fly
// machine IDs are unique only within an app — without the app prefix
// the post-import Read call has no way to query the right app. The
// Vault staging adoption path (terraform/vault-machines/) exercises
// this on every fresh import.
func TestParseImportID(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		app, id, err := parseImportID("ampbase-vault-staging/2872174b330338")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if app != "ampbase-vault-staging" {
			t.Errorf("app = %q, want ampbase-vault-staging", app)
		}
		if id != "2872174b330338" {
			t.Errorf("id = %q, want 2872174b330338", id)
		}
	})

	t.Run("rejects missing separator", func(t *testing.T) {
		_, _, err := parseImportID("2872174b330338")
		if err == nil {
			t.Fatal("expected error on missing separator")
		}
	})

	t.Run("rejects empty app", func(t *testing.T) {
		_, _, err := parseImportID("/2872174b330338")
		if err == nil {
			t.Fatal("expected error on empty app prefix")
		}
	})

	t.Run("rejects empty machine ID", func(t *testing.T) {
		_, _, err := parseImportID("ampbase-vault-staging/")
		if err == nil {
			t.Fatal("expected error on empty machine ID")
		}
	})

	t.Run("rejects empty string", func(t *testing.T) {
		_, _, err := parseImportID("")
		if err == nil {
			t.Fatal("expected error on empty input")
		}
	})

	t.Run("accepts slash in machine id portion", func(t *testing.T) {
		// SplitN(2) only splits on the first slash; if Fly ever uses
		// slashes inside machine IDs (it doesn't today), the parser
		// must still produce a coherent split rather than rejecting.
		app, id, err := parseImportID("ampbase-vault-staging/abc/def")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if app != "ampbase-vault-staging" {
			t.Errorf("app = %q, want ampbase-vault-staging", app)
		}
		if id != "abc/def" {
			t.Errorf("id = %q, want abc/def", id)
		}
	})
}

// TestBuildCreateMachineInput_Files locks in that file blocks are forwarded
// to FlyMachineConfig.Files with the guest_path/raw_value/mode mapping the
// chat bootstrap (RFC-021 followup) needs for per-machine XML configs.
func TestBuildCreateMachineInput_Files(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-keeper-staging"),
		Image: types.StringValue("registry.fly.io/ampbase-keeper-staging:latest"),
		File: []fileModel{
			{
				GuestPath: types.StringValue("/etc/clickhouse-keeper/keeper_config.xml"),
				RawValue:  types.StringValue(base64.StdEncoding.EncodeToString([]byte("<clickhouse/>"))),
				Mode:      types.Int64Value(0o644),
			},
		},
	}

	got := buildCreateMachineInput(plan)
	if got.Config == nil {
		t.Fatal("config nil")
	}
	if len(got.Config.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(got.Config.Files))
	}
	f := got.Config.Files[0]
	if f.GuestPath == nil || *f.GuestPath != "/etc/clickhouse-keeper/keeper_config.xml" {
		t.Errorf("guest_path = %v", f.GuestPath)
	}
	if f.RawValue == nil || *f.RawValue == "" {
		t.Error("raw_value missing")
	}
	if f.Mode == nil || *f.Mode != 0o644 {
		t.Errorf("mode = %v, want 0o644", f.Mode)
	}
}

// TestBuildCreateMachineInput_MountsAndRestart locks in volume mount +
// restart-policy wiring. Restart policy "always" is what the Keeper
// bootstrap needs so the entrypoint comes back after the post-config push.
func TestBuildCreateMachineInput_MountsAndRestart(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-keeper-staging"),
		Image: types.StringValue("img:latest"),
		Mount: []mountModel{
			{
				Volume: types.StringValue("vol_abc"),
				Path:   types.StringValue("/var/lib/clickhouse-keeper"),
			},
		},
		Restart: &restartModel{
			Policy: types.StringValue("always"),
		},
	}

	got := buildCreateMachineInput(plan)
	if len(got.Config.Mounts) != 1 {
		t.Fatalf("mounts len = %d, want 1", len(got.Config.Mounts))
	}
	m := got.Config.Mounts[0]
	if m.Volume == nil || *m.Volume != "vol_abc" {
		t.Errorf("volume = %v, want vol_abc", m.Volume)
	}
	if m.Path == nil || *m.Path != "/var/lib/clickhouse-keeper" {
		t.Errorf("path = %v", m.Path)
	}
	if got.Config.Restart == nil || got.Config.Restart.Policy == nil || string(*got.Config.Restart.Policy) != "always" {
		t.Errorf("restart policy = %v, want always", got.Config.Restart)
	}
}

// TestBuildCreateMachineInput_Metadata locks in that the metadata map
// (and specifically fly_process_group, which controls
// <group>.process.<app>.internal DNS) is forwarded to FlyMachineConfig.
// Single keeper-per-group is how the cluster avoids the chicken-and-egg
// between Raft hostnames and machine IDs at plan time.
func TestBuildCreateMachineInput_Metadata(t *testing.T) {
	mdElems := map[string]types.String{
		"fly_process_group": types.StringValue("keeper-1"),
	}
	asAttr := make(map[string]types.String, len(mdElems))
	for k, v := range mdElems {
		asAttr[k] = v
	}
	mdMap, diags := types.MapValueFrom(context.Background(), types.StringType, asAttr)
	if diags.HasError() {
		t.Fatalf("MapValueFrom: %v", diags)
	}
	plan := &machineResourceModel{
		App:      types.StringValue("ampbase-keeper-staging"),
		Image:    types.StringValue("img:latest"),
		Metadata: mdMap,
	}

	got := buildCreateMachineInput(plan)
	if got.Config.Metadata["fly_process_group"] != "keeper-1" {
		t.Errorf("metadata.fly_process_group = %q, want keeper-1", got.Config.Metadata["fly_process_group"])
	}
}

// TestMachineSchema_Version pins the schema version that gates the
// UpgradeState path: version-0 states carry service.concurrency as an object
// and must be migrated, so silently dropping the version (back to the 0
// default) would make tofu decode old states with the new schema and fail
// with `invalid JSON, expected "[", got "{"` on every org.
func TestMachineSchema_Version(t *testing.T) {
	t.Parallel()

	r := NewMachineResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Schema.Version != 1 {
		t.Errorf("schema version = %d, want 1 (bumping it requires updating upgradeStateV0 to emit the new shape)", resp.Schema.Version)
	}
}

func TestListifyConcurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "object becomes single-element list",
			in:   `{"id":"m1","service":[{"internal_port":4318,"concurrency":{"type":"connections","soft_limit":180,"hard_limit":220}}]}`,
			want: `{"id":"m1","service":[{"internal_port":4318,"concurrency":[{"type":"connections","soft_limit":180,"hard_limit":220}]}]}`,
		},
		{
			name: "list shape passes through (state written by pre-upgrader v1-shaped binary)",
			in:   `{"service":[{"concurrency":[{"type":"requests","soft_limit":1,"hard_limit":2}]}]}`,
			want: `{"service":[{"concurrency":[{"type":"requests","soft_limit":1,"hard_limit":2}]}]}`,
		},
		{
			name: "null becomes empty list",
			in:   `{"service":[{"internal_port":8200,"concurrency":null}]}`,
			want: `{"service":[{"internal_port":8200,"concurrency":[]}]}`,
		},
		{
			name: "missing key becomes empty list",
			in:   `{"service":[{"internal_port":9092}]}`,
			want: `{"service":[{"internal_port":9092,"concurrency":[]}]}`,
		},
		{
			name: "mixed services each handled independently",
			in:   `{"service":[{"concurrency":{"type":"connections","soft_limit":9,"hard_limit":10}},{"concurrency":null}]}`,
			want: `{"service":[{"concurrency":[{"type":"connections","soft_limit":9,"hard_limit":10}]},{"concurrency":[]}]}`,
		},
		{
			name: "no service key untouched",
			in:   `{"id":"m1","app":"a"}`,
			want: `{"id":"m1","app":"a"}`,
		},
		{
			name: "null service untouched",
			in:   `{"id":"m1","service":null}`,
			want: `{"id":"m1","service":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := listifyConcurrency([]byte(tt.in))
			if err != nil {
				t.Fatalf("listifyConcurrency: %v", err)
			}
			// Compare decoded values, not bytes: map round-trips reorder keys.
			var gotV, wantV any
			if err := json.Unmarshal(got, &gotV); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, got)
			}
			if err := json.Unmarshal([]byte(tt.want), &wantV); err != nil {
				t.Fatalf("bad want fixture: %v", err)
			}
			if !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("listifyConcurrency mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// TestUpgradeStateV0 runs the registered 0->1 upgrader end to end against a
// realistic version-0 org-machine state (object-shaped concurrency on the
// OpAMP pool service, none on a second service) and decodes the response
// DynamicValue against the CURRENT schema — the same decode tofu performs, so
// a migration output that doesn't fit the schema fails here, not in prod.
func TestUpgradeStateV0(t *testing.T) {
	t.Parallel()

	v0State := `{
		"id": "9080e123456789",
		"instance_id": "01KXABCDEF",
		"app": "ampbase-org-01KVS9W8CEJJHWKTB906XSPYH8",
		"name": "org-0",
		"region": "iad",
		"image": "registry.fly.io/ampbase-console:latest",
		"state": "started",
		"service": [
			{
				"internal_port": 8080,
				"protocol": "tcp",
				"autostart": true,
				"autostop": "suspend",
				"concurrency": {"type": "connections", "soft_limit": 180, "hard_limit": 220}
			},
			{
				"internal_port": 4318,
				"protocol": "tcp",
				"concurrency": null
			}
		]
	}`

	r, ok := NewMachineResource().(*machineResource)
	if !ok {
		t.Fatalf("NewMachineResource is %T, want *machineResource", NewMachineResource())
	}
	upgrader, ok := r.UpgradeState(context.Background())[0]
	if !ok {
		t.Fatal("no StateUpgrader registered for version 0")
	}

	var resp resource.UpgradeStateResponse
	upgrader.StateUpgrader(context.Background(),
		resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(v0State)}},
		&resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("upgrade diagnostics: %v", resp.Diagnostics)
	}
	if resp.DynamicValue == nil {
		t.Fatal("upgrader set no DynamicValue")
	}

	var schemaResp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &schemaResp)
	val, err := resp.DynamicValue.Unmarshal(schemaResp.Schema.Type().TerraformType(context.Background()))
	if err != nil {
		t.Fatalf("upgraded state does not decode against current schema: %v", err)
	}

	attrs := objectAttrs(t, val)
	if got := stringAttr(t, attrs["image"]); got != "registry.fly.io/ampbase-console:latest" {
		t.Errorf("image = %q, want the fixture value to survive untouched", got)
	}

	var services []tftypes.Value
	if err := attrs["service"].As(&services); err != nil {
		t.Fatalf("service: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("len(service) = %d, want 2", len(services))
	}

	pool := objectAttrs(t, services[0])
	var conc []tftypes.Value
	if err := pool["concurrency"].As(&conc); err != nil {
		t.Fatalf("service[0].concurrency: %v", err)
	}
	if len(conc) != 1 {
		t.Fatalf("len(service[0].concurrency) = %d, want the object migrated to a single-element list", len(conc))
	}
	cattrs := objectAttrs(t, conc[0])
	if got := stringAttr(t, cattrs["type"]); got != "connections" {
		t.Errorf("concurrency.type = %q, want connections", got)
	}
	if got := int64Attr(t, cattrs["soft_limit"]); got != 180 {
		t.Errorf("concurrency.soft_limit = %d, want 180", got)
	}
	if got := int64Attr(t, cattrs["hard_limit"]); got != 220 {
		t.Errorf("concurrency.hard_limit = %d, want 220", got)
	}

	otlp := objectAttrs(t, services[1])
	var otlpConc []tftypes.Value
	if err := otlp["concurrency"].As(&otlpConc); err != nil {
		t.Fatalf("service[1].concurrency: %v", err)
	}
	if len(otlpConc) != 0 {
		t.Errorf("len(service[1].concurrency) = %d, want null migrated to an empty list", len(otlpConc))
	}
}

func objectAttrs(t *testing.T, v tftypes.Value) map[string]tftypes.Value {
	t.Helper()
	var attrs map[string]tftypes.Value
	if err := v.As(&attrs); err != nil {
		t.Fatalf("as object: %v", err)
	}
	return attrs
}

func stringAttr(t *testing.T, v tftypes.Value) string {
	t.Helper()
	var s string
	if err := v.As(&s); err != nil {
		t.Fatalf("as string: %v", err)
	}
	return s
}

func int64Attr(t *testing.T, v tftypes.Value) int64 {
	t.Helper()
	var f big.Float
	if err := v.As(&f); err != nil {
		t.Fatalf("as number: %v", err)
	}
	i, _ := f.Int64()
	return i
}

// TestBuildMachineInput_Checks pins the machine-level `check` block onto
// the Machines API's `config.checks` map, on both Create and Update.
// Update carries its own assertion because that is the path that puts
// checks onto machines which already exist — a builder wired into Create
// alone would leave every rolling apply ungated while looking correct.
func TestBuildMachineInput_Checks(t *testing.T) {
	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-clickhouse-keeper-staging"),
		Image: types.StringValue("img:latest"),
		Check: []machineCheckModel{{
			Name:        types.StringValue("client-port"),
			Type:        types.StringValue("tcp"),
			Port:        types.Int64Value(9181),
			Interval:    types.StringValue("10s"),
			Timeout:     types.StringValue("5s"),
			GracePeriod: types.StringValue("20s"),
		}},
	}

	assertCheck := func(t *testing.T, checks map[string]machines.FlyMachineCheck) {
		t.Helper()
		c, ok := checks["client-port"]
		if !ok {
			t.Fatalf("checks keyed %v, want a \"client-port\" entry — name is the map key", checks)
		}
		if c.Type == nil || *c.Type != "tcp" {
			t.Errorf("type = %v, want tcp", c.Type)
		}
		if c.Port == nil || *c.Port != 9181 {
			t.Errorf("port = %v, want 9181", c.Port)
		}
		if c.Interval == nil || *c.Interval != "10s" {
			t.Errorf("interval = %v, want 10s", c.Interval)
		}
		if c.Timeout == nil || *c.Timeout != "5s" {
			t.Errorf("timeout = %v, want 5s", c.Timeout)
		}
		if c.GracePeriod == nil || *c.GracePeriod != "20s" {
			t.Errorf("grace_period = %v, want 20s", c.GracePeriod)
		}
	}

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		got := buildCreateMachineInput(plan)
		if got.Config == nil {
			t.Fatal("config nil")
		}
		assertCheck(t, got.Config.Checks)
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		got := buildUpdateMachineInput(plan)
		if got.Config == nil {
			t.Fatal("config nil")
		}
		assertCheck(t, got.Config.Checks)
	})
}

// TestBuildChecks_OmittedWhenEmpty guards the nil-vs-empty-map
// distinction. Update replaces the whole machine config, so an empty map
// would serialize as "this machine has no checks" and silently strip
// them; nil omits the field.
func TestBuildChecks_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	if got := buildChecks(nil); got != nil {
		t.Errorf("buildChecks(nil) = %v, want nil so the field is omitted", got)
	}
	if got := buildChecks([]machineCheckModel{}); got != nil {
		t.Errorf("buildChecks(empty) = %v, want nil so the field is omitted", got)
	}
}

// TestValidateCheckNameUniqueness pins the duplicate-name guard. Unlike
// the container and mount guards there is no API-level rejection behind
// it: the name is a map key, so a duplicate is silently dropped and the
// machine gates on fewer checks than the config declares.
func TestValidateCheckNameUniqueness(t *testing.T) {
	t.Parallel()
	t.Run("unique names pass", func(t *testing.T) {
		t.Parallel()
		diags := validateCheckNameUniqueness([]machineCheckModel{
			{Name: types.StringValue("client-port")},
			{Name: types.StringValue("raft-port")},
		})
		if diags != nil {
			t.Errorf("diags = %v, want nil", diags)
		}
	})

	t.Run("duplicate names rejected", func(t *testing.T) {
		t.Parallel()
		diags := validateCheckNameUniqueness([]machineCheckModel{
			{Name: types.StringValue("client-port")},
			{Name: types.StringValue("client-port")},
		})
		if diags == nil {
			t.Fatal("diags = nil, want an error — the second block would replace the first in the checks map")
		}
		if !diags.HasError() {
			t.Errorf("diags = %v, want an error", diags)
		}
	})
}

// deref renders a pointer field as its value. Printing the pointer gives an
// address, which is useless in exactly the moment the message has to name
// the value that was wrong.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestBuildContainerHealthchecks pins the container `healthcheck` block
// onto FlyContainerConfig.Healthchecks, on both Create and Update. Update
// carries its own assertion because that is the path that adds a
// healthcheck to a container that already exists — the state every machine
// is in when an ordering gate is introduced to a running app.
//
// The second check leaves every time and threshold unset, which is the
// intPtr contract: an omitted field, not a zero one. The wire types are
// `omitempty` pointers, so sending 0 would state a value rather than leave
// the field to Fly.
func TestBuildContainerHealthchecks(t *testing.T) {
	t.Parallel()

	execCommand, diags := types.ListValueFrom(context.Background(), types.StringType,
		[]string{"vault", "status"})
	if diags.HasError() {
		t.Fatalf("exec command ListValueFrom: %v", diags)
	}

	plan := &machineResourceModel{
		App:   types.StringValue("ampbase-vault-staging"),
		Image: types.StringValue("img:latest"),
		Container: []containerModel{{
			Name:  types.StringValue("vault"),
			Image: types.StringValue("registry.fly.io/ampbase-vault:latest"),
			Healthcheck: []containerHealthcheckModel{
				{
					Name:               types.StringValue("listening"),
					Kind:               types.StringValue("readiness"),
					IntervalSeconds:    types.Int64Value(5),
					TimeoutSeconds:     types.Int64Value(2),
					GracePeriodSeconds: types.Int64Value(10),
					SuccessThreshold:   types.Int64Value(1),
					FailureThreshold:   types.Int64Value(3),
					TCP:                &tcpHealthcheckModel{Port: types.Int64Value(8200)},
				},
				{
					Name: types.StringValue("active"),
					Exec: &execHealthcheckModel{Command: execCommand},
				},
				{
					Name: types.StringValue("unsealed"),
					HTTP: &httpHealthcheckModel{
						Port:          types.Int64Value(8200),
						Path:          types.StringValue("/v1/sys/health"),
						Method:        types.StringValue("GET"),
						Scheme:        types.StringValue("https"),
						TLSServerName: types.StringValue("vault.internal"),
						TLSSkipVerify: types.BoolValue(true),
					},
				},
			},
		}},
	}

	assertHealthchecks := func(t *testing.T, containers []machines.FlyContainerConfig) {
		t.Helper()
		if len(containers) != 1 {
			t.Fatalf("containers len = %d, want 1", len(containers))
		}
		hcs := containers[0].Healthchecks
		if len(hcs) != 3 {
			t.Fatalf("healthchecks len = %d, want 3", len(hcs))
		}

		tcpCheck := hcs[0]
		if tcpCheck.Name == nil || *tcpCheck.Name != "listening" {
			t.Errorf("healthcheck[0].name = %q, want listening", deref(tcpCheck.Name))
		}
		if tcpCheck.Kind == nil || *tcpCheck.Kind != machines.FlyContainerHealthcheckKindReadiness {
			t.Errorf("healthcheck[0].kind = %q, want readiness", deref(tcpCheck.Kind))
		}
		if tcpCheck.Tcp == nil || tcpCheck.Tcp.Port == nil || *tcpCheck.Tcp.Port != 8200 {
			t.Errorf("healthcheck[0].tcp = %+v, want port 8200", tcpCheck.Tcp)
		}
		if tcpCheck.Http != nil {
			t.Errorf("healthcheck[0].http = %+v, want nil", tcpCheck.Http)
		}
		if tcpCheck.Exec != nil {
			t.Errorf("healthcheck[0].exec = %+v, want nil", tcpCheck.Exec)
		}
		if tcpCheck.Interval == nil || *tcpCheck.Interval != 5 {
			t.Errorf("healthcheck[0].interval = %v, want 5 (seconds, not a duration string)", deref(tcpCheck.Interval))
		}
		if tcpCheck.Timeout == nil || *tcpCheck.Timeout != 2 {
			t.Errorf("healthcheck[0].timeout = %v, want 2", deref(tcpCheck.Timeout))
		}
		if tcpCheck.GracePeriod == nil || *tcpCheck.GracePeriod != 10 {
			t.Errorf("healthcheck[0].grace_period = %v, want 10", deref(tcpCheck.GracePeriod))
		}
		if tcpCheck.SuccessThreshold == nil || *tcpCheck.SuccessThreshold != 1 {
			t.Errorf("healthcheck[0].success_threshold = %v, want 1", deref(tcpCheck.SuccessThreshold))
		}
		if tcpCheck.FailureThreshold == nil || *tcpCheck.FailureThreshold != 3 {
			t.Errorf("healthcheck[0].failure_threshold = %v, want 3", deref(tcpCheck.FailureThreshold))
		}

		execCheck := hcs[1]
		if execCheck.Exec == nil {
			t.Fatalf("healthcheck[1].exec = nil, want the exec transport")
		}
		if got := execCheck.Exec.Command; len(got) != 2 || got[0] != "vault" || got[1] != "status" {
			t.Errorf("healthcheck[1].exec.command = %v, want [vault status]", got)
		}
		if execCheck.Tcp != nil || execCheck.Http != nil {
			t.Errorf("healthcheck[1] tcp = %+v http = %+v, want both nil", execCheck.Tcp, execCheck.Http)
		}

		httpCheck := hcs[2]
		if httpCheck.Http == nil {
			t.Fatalf("healthcheck[2].http = nil, want the http transport")
		}
		if httpCheck.Http.Port == nil || *httpCheck.Http.Port != 8200 {
			t.Errorf("healthcheck[2].http.port = %v, want 8200", deref(httpCheck.Http.Port))
		}
		if httpCheck.Http.Path == nil || *httpCheck.Http.Path != "/v1/sys/health" {
			t.Errorf("healthcheck[2].http.path = %q, want /v1/sys/health", deref(httpCheck.Http.Path))
		}
		if httpCheck.Http.Method == nil || *httpCheck.Http.Method != "GET" {
			t.Errorf("healthcheck[2].http.method = %q, want GET", deref(httpCheck.Http.Method))
		}
		if httpCheck.Http.Scheme == nil || *httpCheck.Http.Scheme != machines.HTTPS {
			t.Errorf("healthcheck[2].http.scheme = %q, want https", deref(httpCheck.Http.Scheme))
		}
		if httpCheck.Http.TlsServerName == nil || *httpCheck.Http.TlsServerName != "vault.internal" {
			t.Errorf("healthcheck[2].http.tls_server_name = %q, want vault.internal", deref(httpCheck.Http.TlsServerName))
		}
		if httpCheck.Http.TlsSkipVerify == nil || !*httpCheck.Http.TlsSkipVerify {
			t.Errorf("healthcheck[2].http.tls_skip_verify = %v, want true", deref(httpCheck.Http.TlsSkipVerify))
		}
		if httpCheck.Tcp != nil {
			t.Errorf("healthcheck[2].tcp = %+v, want nil", httpCheck.Tcp)
		}
		if httpCheck.Kind != nil {
			t.Errorf("healthcheck[2].kind = %q, want nil — the block set no kind, so none should be sent", deref(httpCheck.Kind))
		}
		for name, got := range map[string]*int{
			"interval":          httpCheck.Interval,
			"timeout":           httpCheck.Timeout,
			"grace_period":      httpCheck.GracePeriod,
			"success_threshold": httpCheck.SuccessThreshold,
			"failure_threshold": httpCheck.FailureThreshold,
		} {
			if got != nil {
				t.Errorf("healthcheck[2].%s = %d, want nil — an unset optional is an omitted field, not a zero one", name, *got)
			}
		}
	}

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		got := buildCreateMachineInput(plan)
		if got.Config == nil {
			t.Fatal("config nil")
		}
		assertHealthchecks(t, got.Config.Containers)
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		got := buildUpdateMachineInput(plan)
		if got.Config == nil {
			t.Fatal("config nil")
		}
		assertHealthchecks(t, got.Config.Containers)
	})
}

// TestBuildHealthchecks_OmittedWhenEmpty guards the nil-vs-empty-slice
// distinction. Update replaces the whole machine config, so an empty slice
// would serialize as "this container has no healthchecks" and strip the
// gate every dependent container waits on; nil omits the field.
func TestBuildHealthchecks_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	if got := buildHealthchecks(nil); got != nil {
		t.Errorf("buildHealthchecks(nil) = %v, want nil so the field is omitted", got)
	}
	if got := buildHealthchecks([]containerHealthcheckModel{}); got != nil {
		t.Errorf("buildHealthchecks(empty) = %v, want nil so the field is omitted", got)
	}
}

// TestValidateHealthcheckNameUniqueness pins the duplicate-name guard. The
// API documents the constraint but the field is a list, so what Fly does
// with a duplicate is not settled by the generated types — rejecting here
// means no config depends on the answer.
func TestValidateHealthcheckNameUniqueness(t *testing.T) {
	t.Parallel()

	t.Run("unique names pass", func(t *testing.T) {
		t.Parallel()
		diags := validateHealthcheckNameUniqueness([]containerModel{{
			Name: types.StringValue("vault"),
			Healthcheck: []containerHealthcheckModel{
				{Name: types.StringValue("listening")},
				{Name: types.StringValue("unsealed")},
			},
		}})
		if diags != nil {
			t.Errorf("diags = %v, want nil", diags)
		}
	})

	t.Run("duplicate names rejected", func(t *testing.T) {
		t.Parallel()
		diags := validateHealthcheckNameUniqueness([]containerModel{{
			Name: types.StringValue("vault"),
			Healthcheck: []containerHealthcheckModel{
				{Name: types.StringValue("listening")},
				{Name: types.StringValue("listening")},
			},
		}})
		if diags == nil || !diags.HasError() {
			t.Fatalf("diags = %v, want an error naming the collision", diags)
		}
		for _, d := range diags {
			if d.Summary() == "Duplicate Healthcheck Name" && containsStr(d.Detail(), `"listening"`) {
				return
			}
		}
		t.Errorf("missing expected error diagnostic, got %v", diags)
	})

	t.Run("names collide per container, not per machine", func(t *testing.T) {
		t.Parallel()
		diags := validateHealthcheckNameUniqueness([]containerModel{
			{
				Name:        types.StringValue("vault"),
				Healthcheck: []containerHealthcheckModel{{Name: types.StringValue("listening")}},
			},
			{
				Name:        types.StringValue("registrar"),
				Healthcheck: []containerHealthcheckModel{{Name: types.StringValue("listening")}},
			},
		})
		if diags != nil {
			t.Errorf("diags = %v, want nil — two containers may each have a check called listening", diags)
		}
	})
}

// TestValidateResourceConfig_HealthcheckTransport drives the real provider
// server's config validation, which is the only place exactlyOneTransport
// runs. A healthcheck with no transport is the shape that matters: it names
// nothing to probe, so rejecting it at plan time is what keeps the silent
// hang this block exists to remove from returning through a config that
// merely looks complete.
//
// Each row asserts the summary, not just that something failed — the two
// wrong shapes are wrong for different reasons and say so.
func TestValidateResourceConfig_HealthcheckTransport(t *testing.T) {
	t.Parallel()

	const configTemplate = `{
		"app": "ampbase-vault-staging",
		"image": "registry.fly.io/ampbase-vault:latest",
		"container": [
			{
				"name": "vault",
				"image": "registry.fly.io/ampbase-vault:latest",
				"healthcheck": [{"name": "listening", %s}]
			}
		]
	}`

	const (
		missing     = "Missing Healthcheck Transport"
		conflicting = "Conflicting Healthcheck Transports"
	)

	tests := []struct {
		name string
		// transport is spliced into one healthcheck block; wantSummary is
		// the diagnostic summary it must produce, empty for a clean config.
		transport   string
		wantSummary string
		wantDetail  string
	}{
		{"tcp alone", `"tcp": {"port": 8200}`, "", ""},
		{"http alone", `"http": {"port": 8200, "path": "/v1/sys/health"}`, "", ""},
		{"exec alone", `"exec": {"command": ["vault", "status"]}`, "", ""},
		{"none", `"kind": "readiness"`, missing, "declares none"},
		{"tcp and http", `"tcp": {"port": 8200}, "http": {"port": 8200}`, conflicting, "declares tcp and http"},
		{"tcp and exec", `"tcp": {"port": 8200}, "exec": {"command": ["vault", "status"]}`, conflicting, "declares tcp and exec"},
		{"all three", `"tcp": {"port": 8200}, "http": {"port": 8200}, "exec": {"command": ["x"]}`, conflicting, "declares tcp, http and exec"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diags := validateMachineConfig(t, fmt.Sprintf(configTemplate, tt.transport))
			var errs []*tfprotov6.Diagnostic
			for _, d := range diags {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					errs = append(errs, d)
				}
			}

			if tt.wantSummary == "" {
				for _, d := range errs {
					t.Errorf("config rejected with %q: %s, want it accepted", d.Summary, d.Detail)
				}
				return
			}

			// One diagnostic, not one per transport attribute: the rule lives
			// on the healthcheck object, so a violation is reported once.
			if len(errs) != 1 {
				t.Fatalf("got %d error diagnostics, want exactly 1: %v", len(errs), errs)
			}
			if errs[0].Summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", errs[0].Summary, tt.wantSummary)
			}
			// Match the computed clause alone; the surrounding sentence is
			// fixed text that would pass on its own.
			if !containsStr(errs[0].Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to name %q", errs[0].Detail, tt.wantDetail)
			}
		})
	}
}

// validateMachineConfig runs one fly_machine config through the provider
// server's ValidateResourceConfig — the same call tofu makes at plan time,
// and the only path that executes schema-level validators.
func validateMachineConfig(t *testing.T, configJSON string) []*tfprotov6.Diagnostic {
	t.Helper()
	ctx := context.Background()

	typ := machineSchema(t).Type().TerraformType(ctx)
	val, err := (tfprotov6.RawState{JSON: []byte(configJSON)}).Unmarshal(typ)
	if err != nil {
		t.Fatalf("config does not decode against the schema: %v", err)
	}
	dv, err := tfprotov6.NewDynamicValue(typ, val)
	if err != nil {
		t.Fatalf("encoding config: %v", err)
	}

	srv := providerserver.NewProtocol6(New("test")())()
	resp, err := srv.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName: "fly_machine",
		Config:   &dv,
	})
	if err != nil {
		t.Fatalf("ValidateResourceConfig: %v", err)
	}
	return resp.Diagnostics
}
