package flyio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

// machineEventsLimit is how many events to ask for. The endpoint's documented
// maximum, against a default of 20; a one-shot machine writes six events and a
// restarting one writes as many as it restarts.
//
// The maximum rather than the default because what binds here is truncation,
// and truncation is silent: the exit event drops off the end of the window and
// a machine that certainly exited reads as one that carries no exit state.
const machineEventsLimit = 50

// machineStateDestroyed is spelled as fly-go spells the machine state it
// mirrors (machine_types.go:28, MachineStateDestroyed).
const machineStateDestroyed = "destroyed"

// MachineExit is a machine's exit state, as its own event log records it.
// Readable after the machine has been destroyed, which is what lets a
// fire-and-forget launcher learn how a one-shot machine ended.
type MachineExit struct {
	// ExitCode is the machine's exit code — deliberately not guest_exit_code,
	// which reports 0 for a guest the kernel killed.
	//
	// int32 rather than fly-go's int: a process exit status does not reach
	// that width, and a value that did would be a payload this cannot
	// represent — better refused at the decode than narrowed silently on its
	// way into a run record.
	ExitCode int32
	// OOMKilled distinguishes a kernel OOM kill (exit_code 137 with this
	// true) from a process that exited 137 on its own.
	OOMKilled bool
	ExitedAt  time.Time
}

// MachineOutcome is how a machine ended, as its event log records it: exited
// (Exit set), gone without ever having exited (Destroyed, no Exit), or not yet
// known (neither). A machine can be destroyed having never run a process, and
// its log then carries no exit event, ever.
type MachineOutcome struct {
	// Exit is the machine's exit state, or nil when its log carries none.
	Exit *MachineExit
	// Destroyed reports a terminal destroy event, which a machine that
	// exited normally also has. Only the pair names the outcome.
	Destroyed bool
}

// NeverExited reports the state a caller must stop polling on: no later read
// can produce an exit event.
//
// Exit is checked first and wins, or every machine that exited and was then
// destroyed — which is all of them — would read as one that never exited.
func (o *MachineOutcome) NeverExited() bool {
	return o.Exit == nil && o.Destroyed
}

// MachineOutcome reads how a machine ended from its event log. The returned
// outcome is never nil when err is nil.
//
// The events endpoint rather than the machine record's embedded array: that
// holds only the five most recent events and a one-shot run produces six, so
// one lease or health check drops the exit event silently.
//
// A 404 is returned as an error and nothing is concluded from it: the API
// reports a pruned record and an id that never existed identically, and the
// caller's own record of the launch is what tells those apart. It is not how
// a destroyed machine is detected — the API answers 200 for one, on this
// endpoint and on the machine record both, while it retains them.
func (c *Client) MachineOutcome(ctx context.Context, appName, machineID string) (*MachineOutcome, error) {
	limit := machineEventsLimit
	resp, err := c.gen.MachinesListEvents(ctx, appName, machineID, &machines.MachinesListEventsParams{Limit: &limit})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var events []machineEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("decode machine events: %w", err)
	}
	return &MachineOutcome{Exit: newestExit(events), Destroyed: destroyed(events)}, nil
}

// destroyed reports whether the log leaves the machine terminally gone.
//
// Status rather than Type: fly-go pins the status values as constants
// (machine_types.go:28-29) and carries none for the type values.
//
// "destroyed" alone, though fly-go's Machine.IsActive (machine_types.go:133)
// treats "destroying" as gone too: a machine mid-destroy can still write an
// exit event, one already destroyed cannot.
func destroyed(events []machineEvent) bool {
	return slices.ContainsFunc(events, func(e machineEvent) bool {
		return e.status() == machineStateDestroyed
	})
}

// newestExit picks the exit state from the newest event carrying one, ordered
// by the events' own timestamps rather than by the response's order. Machines
// launched here set restart = "no" and so produce at most one exit event; the
// scan is what keeps that from being an assumption about the API's ordering.
func newestExit(events []machineEvent) *MachineExit {
	var newest *machineEvent
	for i := range events {
		e := &events[i]
		if e.exit() == nil || (newest != nil && e.Timestamp <= newest.Timestamp) {
			continue
		}
		newest = e
	}
	if newest == nil {
		return nil
	}
	x := newest.exit()
	return &MachineExit{ExitCode: x.ExitCode, OOMKilled: x.OOMKilled, ExitedAt: x.ExitedAt}
}

// machineEvent is the generated type with the only two fields it cannot carry
// overridden. Embedded rather than retyped so the rest stays in step with the
// spec — an earlier hand-written copy silently stopped carrying `status`.
type machineEvent struct {
	machines.MachineEvent

	// These shadow the embedded fields of the same JSON name (an outer field
	// at depth 0 wins over an embedded one at depth 1). Do not drop them for
	// the generated forms:
	//
	// Request is map[string]interface{} there — the OpenAPI schema declares
	// it untyped — so the exit payload is unreadable. The shape below is
	// fly-go's machine_types.go (MachineRequest / MachineExitEvent).
	//
	// Timestamp is *int there, and a millisecond epoch (~1.79e12) overflows
	// a 32-bit int. This provider ships 386 and arm builds (.goreleaser.yml),
	// where decoding into the generated field fails outright.
	Request   *machineRequest `json:"request"`
	Timestamp int64           `json:"timestamp"`
}

type machineRequest struct {
	ExitEvent *machineExitEvent `json:"exit_event"`
	// The capitalized tag is real, and fly-go's GetExitCode checks this
	// nesting before the flat one (machine_types.go:313-321), so both occur.
	MonitorEvent *struct {
		ExitEvent *machineExitEvent `json:"exit_event"`
	} `json:"MonitorEvent"`
}

type machineExitEvent struct {
	ExitCode  int32     `json:"exit_code"`
	OOMKilled bool      `json:"oom_killed"`
	ExitedAt  time.Time `json:"exited_at"`
}

// status is the event's status, or "" where the response carried none.
func (e machineEvent) status() string {
	if e.Status == nil {
		return ""
	}
	return *e.Status
}

func (e machineEvent) exit() *machineExitEvent {
	switch {
	case e.Request == nil:
		return nil
	case e.Request.MonitorEvent != nil && e.Request.MonitorEvent.ExitEvent != nil:
		return e.Request.MonitorEvent.ExitEvent
	default:
		return e.Request.ExitEvent
	}
}
