package fly

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestMachineDestroyedOutOfBand pins how the resource treats a machine
// destroyed behind Terraform's back, in both shapes the Machines API reports
// one: a 200 whose state is "destroyed", and a 404. Read must remove it from
// state — a null state after refresh is what makes the next plan create a
// replacement rather than an in-place update Fly refuses — and Delete must
// succeed against it, so a destroy run without refresh still completes.
func TestMachineDestroyedOutOfBand(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name string
		// seed is the state the machine is left in by the out-of-band
		// destroy; empty means it is gone entirely and the API 404s.
		seed string
	}{
		{"reported destroyed", "destroyed"},
		{"not found", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prior := mkState(t, s, machineResourceModel{
				ID:    types.StringValue("m-gone"),
				App:   types.StringValue("app"),
				Image: types.StringValue("img:v1"),
				State: types.StringValue("started"),
			})

			t.Run("read", func(t *testing.T) {
				t.Parallel()
				f := newMachinesFake(t)
				if tc.seed != "" {
					f.seed("m-gone", tc.seed)
				}
				resp := &resource.ReadResponse{State: prior}
				f.resource(t).Read(context.Background(), resource.ReadRequest{State: prior}, resp)

				if resp.Diagnostics.HasError() {
					t.Fatalf("read errored: %v", resp.Diagnostics)
				}
				if !resp.State.Raw.IsNull() {
					var kept machineResourceModel
					resp.State.Get(context.Background(), &kept)
					t.Fatalf("read kept the machine in state with state = %q; the next plan would update it in place instead of adding it", kept.State.ValueString())
				}
			})

			t.Run("delete", func(t *testing.T) {
				t.Parallel()
				f := newMachinesFake(t)
				if tc.seed != "" {
					f.seed("m-gone", tc.seed)
				}
				resp := &resource.DeleteResponse{State: prior}
				f.resource(t).Delete(context.Background(), resource.DeleteRequest{State: prior}, resp)

				if resp.Diagnostics.HasError() {
					t.Fatalf("delete of a machine already destroyed errored: %v", resp.Diagnostics)
				}
				if !f.destroyCalled() {
					t.Errorf("delete issued no MachinesDelete; calls: %v", f.callPaths())
				}
			})
		})
	}
}

// TestMachineReadKeepsLiveMachine is the other side of the destroyed check:
// a machine in any non-terminal state stays in state with its state read
// back, so the removal cannot be satisfied by dropping everything.
func TestMachineReadKeepsLiveMachine(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	for _, live := range []string{"started", machineStateStopped, "suspended", "destroying"} {
		t.Run(live, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			f.seed("m-live", live)
			prior := mkState(t, s, machineResourceModel{
				ID:    types.StringValue("m-live"),
				App:   types.StringValue("app"),
				Image: types.StringValue("img:v1"),
			})
			resp := &resource.ReadResponse{State: prior}
			f.resource(t).Read(context.Background(), resource.ReadRequest{State: prior}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("read errored: %v", resp.Diagnostics)
			}
			if resp.State.Raw.IsNull() {
				t.Fatalf("read removed a %s machine from state", live)
			}
			var out machineResourceModel
			if diags := resp.State.Get(context.Background(), &out); diags.HasError() {
				t.Fatalf("read state: %v", diags)
			}
			if out.State.ValueString() != live {
				t.Errorf("state attr = %q, want %q", out.State.ValueString(), live)
			}
		})
	}
}
