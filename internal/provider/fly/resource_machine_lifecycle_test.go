package fly

import (
	"context"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

// machineSchema returns the fly_machine resource schema for building
// tfsdk.Plan / tfsdk.State values that drive Create/Update directly.
func machineSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewMachineResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func mkPlan(t *testing.T, s rschema.Schema, m machineResourceModel) tfsdk.Plan {
	t.Helper()
	m = normalizeModel(m)
	p := tfsdk.Plan{Schema: s}
	if diags := p.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("build plan: %v", diags)
	}
	return p
}

func mkState(t *testing.T, s rschema.Schema, m machineResourceModel) tfsdk.State {
	t.Helper()
	m = normalizeModel(m)
	st := tfsdk.State{Schema: s}
	if diags := st.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("build state: %v", diags)
	}
	return st
}

// normalizeModel gives the collection-typed attributes a concrete element
// type. A zero types.Map/types.List is null but carries no element type, which
// fails Set's schema-type check; a test that leaves env/metadata or a
// container's cmd/entrypoint/env unset means "null collection of strings", so
// materialize that.
func normalizeModel(m machineResourceModel) machineResourceModel {
	if m.Env.IsNull() {
		m.Env = types.MapNull(types.StringType)
	}
	if m.Metadata.IsNull() {
		m.Metadata = types.MapNull(types.StringType)
	}
	for i := range m.Container {
		c := &m.Container[i]
		if c.Cmd.IsNull() {
			c.Cmd = types.ListNull(types.StringType)
		}
		if c.Entrypoint.IsNull() {
			c.Entrypoint = types.ListNull(types.StringType)
		}
		if c.Env.IsNull() {
			c.Env = types.MapNull(types.StringType)
		}
	}
	return m
}

// TestMachineRejectsDuplicateHealthcheckName is the seam test for
// validateHealthcheckNameUniqueness. The helper's own test passes with the
// call site deleted from both Create and Update — this one drives the real
// entry points and asserts the plan is refused before any Machines API call,
// so its counterfactual is exactly that deletion.
func TestMachineRejectsDuplicateHealthcheckName(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	dup := []containerModel{{
		Name:  types.StringValue("vault"),
		Image: types.StringValue("img:v1"),
		Healthcheck: []containerHealthcheckModel{
			{Name: types.StringValue("listening"), TCP: &tcpHealthcheckModel{Port: types.Int64Value(8200)}},
			{Name: types.StringValue("listening"), TCP: &tcpHealthcheckModel{Port: types.Int64Value(8201)}},
		},
	}}

	assertRefused := func(t *testing.T, f *machinesFake, diags diag.Diagnostics) {
		t.Helper()
		if !diags.HasError() {
			t.Fatalf("diagnostics = %v, want the duplicate rejected", diags)
		}
		if got := diags.Errors()[0].Summary(); got != "Duplicate Healthcheck Name" {
			t.Errorf("first error = %q, want Duplicate Healthcheck Name", got)
		}
		if calls := f.callPaths(); len(calls) != 0 {
			t.Errorf("Machines API calls = %v, want none — the guard runs before the machine is touched", calls)
		}
	}

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		f := newMachinesFake(t)
		resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
		f.resource(t).Create(context.Background(), resource.CreateRequest{
			Plan: mkPlan(t, s, machineResourceModel{
				App:       types.StringValue("app"),
				Image:     types.StringValue("img:v1"),
				Container: dup,
			}),
		}, resp)
		assertRefused(t, f, resp.Diagnostics)
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		f := newMachinesFake(t)
		f.seed("m-dup", "started")
		resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
		f.resource(t).Update(context.Background(), resource.UpdateRequest{
			Plan: mkPlan(t, s, machineResourceModel{
				App:       types.StringValue("app"),
				Image:     types.StringValue("img:v2"),
				Container: dup,
			}),
			State: mkState(t, s, machineResourceModel{
				ID:    types.StringValue("m-dup"),
				App:   types.StringValue("app"),
				Image: types.StringValue("img:v1"),
			}),
		}, resp)
		assertRefused(t, f, resp.Diagnostics)
	})
}

// TestMachineUpdate_ParkPath drives the real Update() across every
// preUpdate.State the org-suspension park path can see, and pins the exact
// invariant PR #430's review called out as untestable without a harness: the
// explicit stop never races the replacing→rest transition. A cold machine the
// update leaves stopped gets NO StopMachine; one that came to rest anywhere
// else, or whose settle could not be observed, gets a StopMachine, and only
// after the provider has looked at where it settled.
func TestMachineUpdate_ParkPath(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name       string
		preState   string
		postUpdate string // state the machine rests in after the update
		replacing  int    // MachinesShow polls reporting "replacing" first
		showFail   int    // one-off MachinesShow status on the first show after the update
		wantStop   bool
		// wantSettleBeforeStop asserts the ordering: any StopMachine must be
		// preceded by a MachinesShow after the update. Only meaningful for
		// the cold paths (a running machine is stopped after a
		// wait-for-started restart, which is a different, expected stop).
		wantSettleBeforeStop bool
	}{
		{"running restarts then explicit stop", "started", "started", 0, 0, true, false},
		{"stopped is left stopped", machineStateStopped, machineStateStopped, 2, 0, false, true},
		{"suspended is left stopped", "suspended", machineStateStopped, 2, 0, false, true},
		{"suspended rests suspended then explicit stop", "suspended", "suspended", 1, 0, true, true},
		{"autostarted after update then explicit stop", machineStateStopped, "started", 1, 0, true, true},
		{"settle unobservable then fallback stop", machineStateStopped, machineStateStopped, 0, http.StatusConflict, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			m := f.seed("m-park", tc.preState)
			m.postUpdateState = tc.postUpdate
			m.replacingPolls = tc.replacing
			m.showFailAfterUpdate = tc.showFail
			r := f.resource(t)

			resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
			r.Update(context.Background(), resource.UpdateRequest{
				Plan: mkPlan(t, s, machineResourceModel{
					App:          types.StringValue("app"),
					Image:        types.StringValue("img:v2"),
					DesiredState: types.StringValue(machineStateStopped),
				}),
				State: mkState(t, s, machineResourceModel{
					ID:    types.StringValue("m-park"),
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v1"),
				}),
			}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("update errored: %v", resp.Diagnostics)
			}
			if got := f.stopCalled(); got != tc.wantStop {
				t.Errorf("stopCalled = %v, want %v; calls: %v", got, tc.wantStop, f.callPaths())
			}
			if tc.wantSettleBeforeStop && !f.actionFollowsSettle() {
				t.Errorf("StopMachine raced the settle: issued with no MachinesShow since the update; calls: %v", f.callPaths())
			}
			if f.startCalled() {
				t.Errorf("park path must never start the machine; calls: %v", f.callPaths())
			}
			fm := f.get("m-park")
			if fm == nil || fm.state != machineStateStopped {
				t.Errorf("final machine state = %+v, want stopped", fm)
			}
			if fm != nil && fm.replacingPolls != 0 {
				t.Errorf("settle wait stopped polling with %d replacing reports unread", fm.replacingPolls)
			}
			var out machineResourceModel
			if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
				t.Fatalf("read state: %v", diags)
			}
			if out.State.ValueString() != machineStateStopped {
				t.Errorf("state attr = %q, want stopped", out.State.ValueString())
			}
		})
	}
}

// TestMachineUpdate_NonParkPath pins the ordinary image-rollout path: a
// machine that was already started just re-verifies (no stop/start), while a
// cold machine is watched through the update's replacing transition to
// whichever state it rests in — stopped (the measured outcome, for a
// suspended machine too), suspended, or started if traffic autostarted it —
// and is then explicitly started only if it is not already, so its checks
// run against the new config.
func TestMachineUpdate_NonParkPath(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name       string
		preState   string
		postUpdate string
		replacing  int
		wantStart  bool
	}{
		{"already started re-verifies without start", "started", "started", 0, false},
		{"stopped is left stopped then started", machineStateStopped, machineStateStopped, 2, true},
		{"suspended is left stopped then started", "suspended", machineStateStopped, 2, true},
		{"suspended rests suspended then started", "suspended", "suspended", 1, true},
		{"autostarted after update is not started twice", "suspended", "started", 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			m := f.seed("m-roll", tc.preState)
			m.postUpdateState = tc.postUpdate
			m.replacingPolls = tc.replacing
			r := f.resource(t)

			resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
			r.Update(context.Background(), resource.UpdateRequest{
				Plan: mkPlan(t, s, machineResourceModel{
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v2"),
				}),
				State: mkState(t, s, machineResourceModel{
					ID:    types.StringValue("m-roll"),
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v1"),
				}),
			}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("update errored: %v", resp.Diagnostics)
			}
			if f.stopCalled() {
				t.Errorf("non-park update must not park (StopMachine); calls: %v", f.callPaths())
			}
			if got := f.startCalled(); got != tc.wantStart {
				t.Errorf("startCalled = %v, want %v; calls: %v", got, tc.wantStart, f.callPaths())
			}
			if !f.actionFollowsSettle() {
				t.Errorf("StartMachine raced the settle: issued with no MachinesShow since the update; calls: %v", f.callPaths())
			}
			fm := f.get("m-roll")
			if fm == nil || fm.state != "started" {
				t.Errorf("final machine state = %+v, want started", fm)
			}
			if fm != nil && fm.replacingPolls != 0 {
				t.Errorf("settle wait stopped polling with %d replacing reports unread", fm.replacingPolls)
			}
			var out machineResourceModel
			if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
				t.Fatalf("read state: %v", diags)
			}
			if out.State.ValueString() != "started" {
				t.Errorf("state attr = %q, want started", out.State.ValueString())
			}
		})
	}
}

// TestMachineConcurrency_RoundTrip pins that a service.concurrency block maps
// to FlyMachineService.Concurrency in the actual create/update request body,
// and that omitting the block sends no concurrency field at all (absent, not a
// zeroed struct).
func TestMachineConcurrency_RoundTrip(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	withConcurrency := []serviceModel{{
		InternalPort: types.Int64Value(8080),
		Concurrency: []concurrencyModel{{
			Type:      types.StringValue("connections"),
			SoftLimit: types.Int64Value(250),
			HardLimit: types.Int64Value(500),
		}},
	}}
	assertConcurrency := func(t *testing.T, svcs []machines.FlyMachineService) {
		t.Helper()
		if len(svcs) != 1 {
			t.Fatalf("services len = %d, want 1", len(svcs))
		}
		c := svcs[0].Concurrency
		if c == nil {
			t.Fatal("service.Concurrency = nil, want populated")
		}
		if c.Type == nil || *c.Type != "connections" {
			t.Errorf("concurrency.type = %v, want connections", c.Type)
		}
		if c.SoftLimit == nil || *c.SoftLimit != 250 {
			t.Errorf("concurrency.soft_limit = %v, want 250", c.SoftLimit)
		}
		if c.HardLimit == nil || *c.HardLimit != 500 {
			t.Errorf("concurrency.hard_limit = %v, want 500", c.HardLimit)
		}
	}

	t.Run("create carries concurrency", func(t *testing.T) {
		t.Parallel()
		f := newMachinesFake(t)
		r := f.resource(t)
		resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
		r.Create(context.Background(), resource.CreateRequest{
			Plan: mkPlan(t, s, machineResourceModel{
				App:     types.StringValue("app"),
				Name:    types.StringValue("m-new"),
				Image:   types.StringValue("img:v1"),
				Service: withConcurrency,
			}),
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("create errored: %v", resp.Diagnostics)
		}
		if f.lastCreate == nil || f.lastCreate.Config == nil {
			t.Fatal("no create body captured")
		}
		assertConcurrency(t, f.lastCreate.Config.Services)
	})

	t.Run("update carries concurrency", func(t *testing.T) {
		t.Parallel()
		f := newMachinesFake(t)
		m := f.seed("m-upd", "started")
		m.postUpdateState = "started"
		r := f.resource(t)
		resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
		r.Update(context.Background(), resource.UpdateRequest{
			Plan: mkPlan(t, s, machineResourceModel{
				App:     types.StringValue("app"),
				Image:   types.StringValue("img:v2"),
				Service: withConcurrency,
			}),
			State: mkState(t, s, machineResourceModel{
				ID:    types.StringValue("m-upd"),
				App:   types.StringValue("app"),
				Image: types.StringValue("img:v1"),
			}),
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("update errored: %v", resp.Diagnostics)
		}
		if f.lastUpdate == nil || f.lastUpdate.Config == nil {
			t.Fatal("no update body captured")
		}
		assertConcurrency(t, f.lastUpdate.Config.Services)
	})

	t.Run("omitted block sends no concurrency", func(t *testing.T) {
		t.Parallel()
		f := newMachinesFake(t)
		r := f.resource(t)
		resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
		r.Create(context.Background(), resource.CreateRequest{
			Plan: mkPlan(t, s, machineResourceModel{
				App:   types.StringValue("app"),
				Name:  types.StringValue("m-plain"),
				Image: types.StringValue("img:v1"),
				Service: []serviceModel{{
					InternalPort: types.Int64Value(8080),
				}},
			}),
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("create errored: %v", resp.Diagnostics)
		}
		if f.lastCreate == nil || f.lastCreate.Config == nil {
			t.Fatal("no create body captured")
		}
		if len(f.lastCreate.Config.Services) != 1 {
			t.Fatalf("services len = %d, want 1", len(f.lastCreate.Config.Services))
		}
		if c := f.lastCreate.Config.Services[0].Concurrency; c != nil {
			t.Errorf("concurrency = %+v, want nil (absent, not zeroed) when block omitted", c)
		}
	})
}

// TestMachineCreate_BootThenPark pins the Create-side desired_state="stopped"
// park: the machine boots to started, then is explicitly stopped, and final
// state reflects stopped.
func TestMachineCreate_BootThenPark(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)
	f := newMachinesFake(t)
	r := f.resource(t)

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: mkPlan(t, s, machineResourceModel{
			App:          types.StringValue("app"),
			Name:         types.StringValue("m-park"),
			Image:        types.StringValue("img:v1"),
			DesiredState: types.StringValue(machineStateStopped),
		}),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("create errored: %v", resp.Diagnostics)
	}
	if !f.stopCalled() {
		t.Errorf("boot-then-park must StopMachine; calls: %v", f.callPaths())
	}
	var out machineResourceModel
	if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if out.State.ValueString() != machineStateStopped {
		t.Errorf("state attr = %q, want stopped", out.State.ValueString())
	}
}

// TestMachineCreate_CleanupOnFailure pins that a failed post-create
// verification destroys the machine (cleanupFailedMachine) rather than leaking
// it, across the start-wait, checks, and stop failure points.
func TestMachineCreate_CleanupOnFailure(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name    string
		park    bool
		arrange func(f *machinesFake)
	}{
		{
			name: "start-wait failure",
			// Machine never reaches "started" → WaitForState(started) 408s.
			arrange: func(f *machinesFake) { f.createState = "creating" },
		},
		{
			name: "checks failure",
			// Started, but MachinesShow errors during WaitForChecks — a
			// permanent error terminates the wait fast (no machineStartTimeout).
			arrange: func(f *machinesFake) { f.createShowStatus = 400 },
		},
		{
			name: "stop failure during park",
			park: true,
			// Boots, but the park-stop is rejected.
			arrange: func(f *machinesFake) { f.createStopStatus = 500 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			tc.arrange(f)
			r := f.resource(t)

			plan := machineResourceModel{
				App:   types.StringValue("app"),
				Name:  types.StringValue("m-fail"),
				Image: types.StringValue("img:v1"),
			}
			if tc.park {
				plan.DesiredState = types.StringValue(machineStateStopped)
			}
			resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
			r.Create(context.Background(), resource.CreateRequest{Plan: mkPlan(t, s, plan)}, resp)

			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected create to fail; calls: %v", f.callPaths())
			}
			if !f.destroyCalled() {
				t.Errorf("failed create must destroy the machine (cleanupFailedMachine); calls: %v", f.callPaths())
			}
		})
	}
}
