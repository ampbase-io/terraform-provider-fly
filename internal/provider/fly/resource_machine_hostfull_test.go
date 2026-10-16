package fly

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

// hostFullBody is the refusal a staging pool machine got on every runtime
// apply for 18 days: its host had no memory left to take it back.
const hostFullBody = "aborted: could not reserve resource for machine: insufficient memory available to fulfill request on the current host"

// TestMachineUpdate_HostFullAsksForReplace pins the diagnostic a caller keys
// its replace on. The counterfactual is the generic "Update Machine Error"
// that v0.3.1 reported, which no caller can tell apart from a bad config.
func TestMachineUpdate_HostFullAsksForReplace(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name        string
		status      int
		body        string
		wantSummary string
	}{
		{"host full is its own diagnostic", http.StatusConflict, hostFullBody, flyio.HostFullSummary},
		{"host closed to deploys is the same remedy", http.StatusConflict, "deploys to this host are temporarily disabled", flyio.HostFullSummary},
		{"another conflict stays generic", http.StatusConflict, "machine is being replaced", "Update Machine Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			m := f.seed("m-full", "suspended")
			m.updateStatus = tc.status
			m.updateBody = tc.body

			resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
			f.resource(t).Update(context.Background(), resource.UpdateRequest{
				Plan: mkPlan(t, s, machineResourceModel{
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v2"),
				}),
				State: mkState(t, s, machineResourceModel{
					ID:    types.StringValue("m-full"),
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v1"),
				}),
			}, resp)

			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one error", resp.Diagnostics)
			}
			if got := errs[0].Summary(); got != tc.wantSummary {
				t.Errorf("summary = %q, want %q", got, tc.wantSummary)
			}
			if tc.wantSummary == flyio.HostFullSummary && !strings.Contains(errs[0].Detail(), "-replace=") {
				t.Errorf("detail = %q, want it to name -replace as the remedy", errs[0].Detail())
			}
			if f.startCalled() {
				t.Errorf("a refused update must not go on to start the machine; calls: %v", f.callPaths())
			}
		})
	}
}
