package flyio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// MachineExit reads a machine's exit state from its event log, returning
// (nil, nil) when the log carries none — a machine still running, or one whose
// events have not caught up.
//
// The events endpoint rather than the machine record's embedded array: that
// holds only the five most recent events and a one-shot run produces six, so
// one lease or health check drops the exit event silently.
//
// A 404 is returned as an error and nothing is concluded from it: the API
// reports a pruned record and an id that never existed identically, and the
// caller's own record of the launch is what tells those apart.
func (c *Client) MachineExit(ctx context.Context, appName, machineID string) (*MachineExit, error) {
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
	return newestExit(events), nil
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

// machineEvent is one entry from the events endpoint. Hand-written because the
// OpenAPI schema declares MachineEvent.Request as an untyped object, so the
// generated client decodes the exit payload into an interface{} nothing can
// read. The shape below is superfly/fly-go's `machine_types.go`
// (MachineEvent / MachineRequest / MachineExitEvent), which is what flyd
// writes.
type machineEvent struct {
	Timestamp int64 `json:"timestamp"`
	Request   *struct {
		ExitEvent *machineExitEvent `json:"exit_event"`
		// The capitalized tag is real, and fly-go's GetExitCode checks this
		// nesting before the flat one, so both occur.
		MonitorEvent *struct {
			ExitEvent *machineExitEvent `json:"exit_event"`
		} `json:"MonitorEvent"`
	} `json:"request"`
}

type machineExitEvent struct {
	ExitCode  int32     `json:"exit_code"`
	OOMKilled bool      `json:"oom_killed"`
	ExitedAt  time.Time `json:"exited_at"`
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
