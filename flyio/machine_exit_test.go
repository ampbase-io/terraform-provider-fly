package flyio

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oomEvents is the event log of a machine the kernel killed, in the shape the
// events endpoint returns it — the exit payload flat under `request`, with the
// `exit` event NOT last in the array so ordering by timestamp is what picks it.
const oomEvents = `[
 {"id":"01A","type":"destroy","status":"destroyed","source":"flyd","timestamp":1755900000900},
 {"id":"01B","type":"exit","status":"stopped","source":"flyd","timestamp":1755900000500,
  "request":{"exit_event":{"exit_code":137,"guest_exit_code":0,"oom_killed":true,
   "requested_stop":false,"exited_at":"2026-08-22T19:20:00Z"}}},
 {"id":"01C","type":"start","status":"started","source":"flyd","timestamp":1755900000100}
]`

// monitorEvents nests the same payload the other way fly-go accepts it, and
// carries an older flat exit beside it — the newer MonitorEvent one must win.
const monitorEvents = `[
 {"id":"01A","type":"exit","status":"stopped","timestamp":1755900000100,
  "request":{"exit_event":{"exit_code":1,"oom_killed":false}}},
 {"id":"01B","type":"exit","status":"stopped","timestamp":1755900000800,
  "request":{"MonitorEvent":{"exit_event":{"exit_code":42,"oom_killed":false,
   "requested_stop":false,"exited_at":"2026-08-22T19:25:00Z"}}}}
]`

func eventsServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("path = %q, want the events endpoint", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "50" {
			t.Errorf("limit = %q, want 50 — the embedded events array truncates the exit event", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, WithBaseURL(srv.URL))
}

// TestMachineExitReadsOOMKill pins the fields a run record needs from a
// destroyed machine, including the two field-name traps: exit_code rather than
// guest_exit_code (which reports 0 for a guest the kernel killed), and the
// exit event picked by timestamp rather than by position.
func TestMachineExitReadsOOMKill(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, oomEvents)

	exit, err := c.MachineExit(t.Context(), "ampbase-intel-x", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineExit: %v", err)
	}
	switch {
	case exit == nil:
		t.Fatal("MachineExit = nil, want the exit event")
	case exit.ExitCode != 137:
		t.Errorf("ExitCode = %d, want 137 (guest_exit_code was 0)", exit.ExitCode)
	case !exit.OOMKilled:
		t.Error("OOMKilled = false, want true")
	case exit.ExitedAt.IsZero():
		t.Error("ExitedAt is zero, want the event's exited_at")
	}
}

// TestMachineExitReadsMonitorNesting covers the second nesting fly-go accepts.
// Both branches occur, and a reader that handles only the flat one finds no
// exit state on a machine that certainly exited.
func TestMachineExitReadsMonitorNesting(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, monitorEvents)

	exit, err := c.MachineExit(t.Context(), "ampbase-intel-x", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineExit: %v", err)
	}
	if exit == nil || exit.ExitCode != 42 {
		t.Fatalf("MachineExit = %+v, want exit_code 42 from the newest event", exit)
	}
}

// TestMachineExitNoExitEvent is the still-running case, which must be nil
// rather than a zero-valued exit — an exit_code of 0 means the run succeeded.
func TestMachineExitNoExitEvent(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, `[{"id":"01A","type":"start","status":"started","timestamp":1755900000100}]`)

	exit, err := c.MachineExit(t.Context(), "ampbase-intel-x", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineExit: %v", err)
	}
	if exit != nil {
		t.Errorf("MachineExit = %+v, want nil while the machine carries no exit event", exit)
	}
}

// TestMachineExitNotFound keeps the 404 an error. It is the one answer the
// Machines API gives to both a pruned record and a machine that never existed,
// so nothing here may turn it into an outcome.
func TestMachineExitNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"machine not found"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, WithBaseURL(srv.URL))

	exit, err := c.MachineExit(t.Context(), "ampbase-intel-x", "d891234567e089")
	switch {
	case exit != nil:
		t.Errorf("MachineExit = %+v, want nil on 404", exit)
	case !IsNotFound(err):
		t.Errorf("MachineExit err = %v, want a 404 the caller can classify", err)
	}
}
