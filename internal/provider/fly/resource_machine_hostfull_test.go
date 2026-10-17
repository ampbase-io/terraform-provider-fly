package fly

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

// hostFullBody is the refusal a staging pool machine got on every runtime
// apply for 18 days: its host had no memory left to take it back.
const hostFullBody = "aborted: could not reserve resource for machine: insufficient memory available to fulfill request on the current host"

// TestMachineUpdate_HostFullWithAVolumeIsNotReplaced pins the diagnostic for
// a refusal no replace can fix: a volume places the replacement on the same
// host, so Update reports the full host and marks nothing. The marking path,
// which needs the private state only the framework can initialise, is held
// against real tofu in resource_machine_hostfull_tofu_test.go.
func TestMachineUpdate_HostFullWithAVolumeIsNotReplaced(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name        string
		status      int
		body        string
		wantSummary string
	}{
		{"host full is its own diagnostic", http.StatusConflict, hostFullBody, flyio.HostFullSummary},
		{"host closed to deploys is the same refusal", http.StatusConflict, "deploys to this host are temporarily disabled", flyio.HostFullSummary},
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
					Mount: []mountModel{{
						Volume:                 types.StringValue("vol_1"),
						Path:                   types.StringValue("/data"),
						Name:                   types.StringNull(),
						ReadoptOnHostMigration: types.BoolNull(),
					}},
				}),
			}, resp)

			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one error", resp.Diagnostics)
			}
			if got := errs[0].Summary(); got != tc.wantSummary {
				t.Errorf("summary = %q, want %q", got, tc.wantSummary)
			}
			if tc.wantSummary == flyio.HostFullSummary && !strings.Contains(errs[0].Detail(), "has a volume") {
				t.Errorf("detail = %q, want it to say the volume is why it is not replaced", errs[0].Detail())
			}
			if f.startCalled() {
				t.Errorf("a refused update must not go on to start the machine; calls: %v", f.callPaths())
			}
		})
	}
}

// TestMachineUpdate_WarnsOnHostFullTextItDoesNotClassify holds the signal
// that Fly moved the refusal off 409: the text under any other status marks
// nothing and is logged at WARN. Its negative control is deleting the WARN in
// Update's generic error case, which leaves warns at 0 on the first row.
func TestMachineUpdate_WarnsOnHostFullTextItDoesNotClassify(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	cases := []struct {
		name     string
		status   int
		body     string
		wantWarn bool
	}{
		{"the text under another status", http.StatusBadRequest, hostFullBody, true},
		{"the text under the classified status", http.StatusConflict, hostFullBody, false},
		{"another refusal", http.StatusBadRequest, "invalid config", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMachinesFake(t)
			m := f.seed("m-full", "suspended")
			m.updateStatus = tc.status
			m.updateBody = tc.body

			var logs bytes.Buffer
			ctx := tflogtest.RootLogger(context.Background(), &logs)
			resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
			f.resource(t).Update(ctx, resource.UpdateRequest{
				Plan: mkPlan(t, s, machineResourceModel{App: types.StringValue("app"), Image: types.StringValue("img:v2")}),
				// A volume keeps the classified row off the marking path,
				// whose private state only the framework can initialise.
				State: mkState(t, s, machineResourceModel{
					ID:    types.StringValue("m-full"),
					App:   types.StringValue("app"),
					Image: types.StringValue("img:v1"),
					Mount: []mountModel{{
						Volume:                 types.StringValue("vol_1"),
						Path:                   types.StringValue("/data"),
						Name:                   types.StringNull(),
						ReadoptOnHostMigration: types.BoolNull(),
					}},
				}),
			}, resp)

			entries, err := tflogtest.MultilineJSONDecode(&logs)
			if err != nil {
				t.Fatalf("decode logs: %v", err)
			}
			warns := 0
			for _, e := range entries {
				if e["@level"] == "warn" && strings.Contains(fmt.Sprint(e["@message"]), "host-full refusal under a status") {
					warns++
				}
			}
			if got := warns == 1; got != tc.wantWarn {
				t.Errorf("warns = %d, want %v; logs:\n%v", warns, tc.wantWarn, entries)
			}
			if !resp.Diagnostics.HasError() {
				t.Error("a refused update reported no error")
			}
		})
	}
}
