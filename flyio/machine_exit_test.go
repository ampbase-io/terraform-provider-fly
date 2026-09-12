package flyio

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oomEvents is a machine the kernel killed: the exit payload flat under
// `request`, with the `exit` event NOT last so ordering by timestamp is what
// picks it, and the destroy event every finished machine also has.
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

// vanishedEvents is a machine destroyed without ever having exited: four
// events, none an exit, and no fifth one coming.
const vanishedEvents = `[
 {"id":"01A","type":"destroy","status":"destroyed","source":"flyd","timestamp":1789175279753},
 {"id":"01B","type":"destroy","status":"destroying","source":"flyd","timestamp":1789175279326},
 {"id":"01C","type":"launch","status":"created","source":"flyd","timestamp":1789175278137},
 {"id":"01D","type":"launch","status":"pending","source":"user","timestamp":1789175278025}
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

// TestMachineOutcomeReadsOOMKill covers the two field-name traps: exit_code
// rather than guest_exit_code (0 for a guest the kernel killed), and the exit
// event picked by timestamp rather than by position.
func TestMachineOutcomeReadsOOMKill(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, oomEvents)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	switch {
	case out.Exit == nil:
		t.Fatal("Exit = nil, want the exit event")
	case out.Exit.ExitCode != 137:
		t.Errorf("ExitCode = %d, want 137 (guest_exit_code was 0)", out.Exit.ExitCode)
	case !out.Exit.OOMKilled:
		t.Error("OOMKilled = false, want true")
	case out.Exit.ExitedAt.IsZero():
		t.Error("ExitedAt is zero, want the event's exited_at")
	}
}

// TestMachineOutcomeExitWinsOverDestroy: the destroy event is present on the
// successful path too, so a reader that answered "gone, never exited" on any
// destroy would lose the exit code of every machine that finished.
func TestMachineOutcomeExitWinsOverDestroy(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, oomEvents)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	switch {
	case !out.Destroyed:
		t.Error("Destroyed = false, want true — the log carries a destroy event")
	case out.NeverExited():
		t.Error("NeverExited() = true beside a real exit event; a finished run must settle as exited")
	}
}

// TestMachineOutcomeReadsMonitorNesting covers the second nesting fly-go
// accepts and checks first (machine_types.go:313-321). Both occur.
func TestMachineOutcomeReadsMonitorNesting(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, monitorEvents)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	if out.Exit == nil || out.Exit.ExitCode != 42 {
		t.Fatalf("Exit = %+v, want exit_code 42 from the newest event", out.Exit)
	}
}

// TestMachineOutcomeStillRunning is the third state: no exit, and nothing
// saying there will not be one. It must be neither of the other two — a
// caller that stopped polling here would report a machine that is running.
func TestMachineOutcomeStillRunning(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, `[{"id":"01A","type":"start","status":"started","timestamp":1755900000100}]`)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	switch {
	case out.Exit != nil:
		t.Errorf("Exit = %+v, want nil while the machine carries no exit event", out.Exit)
	case out.Destroyed:
		t.Error("Destroyed = true with no destroy event in the log")
	case out.NeverExited():
		t.Error("NeverExited() = true for a machine that is still running")
	}
}

// TestMachineOutcomeDestroyedWithoutExit is the state the old decode struct
// could not express, and the one `status` is load-bearing for: stop carrying
// it and Destroyed reads false, which is the bug.
func TestMachineOutcomeDestroyedWithoutExit(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, vanishedEvents)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	switch {
	case out.Exit != nil:
		t.Errorf("Exit = %+v, want nil — none of the four events carries one", out.Exit)
	case !out.Destroyed:
		t.Error("Destroyed = false, want true — the log ends in a destroyed event")
	case !out.NeverExited():
		t.Error("NeverExited() = false; the caller will poll for an event that cannot arrive")
	}
}

// TestMachineOutcomeDestroyingIsNotTerminal keeps the mid-destroy state out of
// the terminal one: fly-go's Machine.IsActive (machine_types.go:133) counts
// "destroying" as gone, but such a machine can still write its exit event.
func TestMachineOutcomeDestroyingIsNotTerminal(t *testing.T) {
	t.Parallel()
	c := eventsServer(t, `[{"id":"01A","type":"destroy","status":"destroying","timestamp":1789175279326}]`)

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	if err != nil {
		t.Fatalf("MachineOutcome: %v", err)
	}
	if out.Destroyed || out.NeverExited() {
		t.Errorf("outcome = %+v, want not-yet-terminal while the machine is destroying", out)
	}
}

// TestMachineOutcomeNotFound keeps the 404 an error: it is the one answer the
// API gives to both a pruned record and an id that never existed, so nothing
// may turn it into an outcome. It is also not how a destroyed machine is
// detected — the API answers 200 for one.
func TestMachineOutcomeNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"machine not found"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, WithBaseURL(srv.URL))

	out, err := c.MachineOutcome(t.Context(), "example-app", "d891234567e089")
	switch {
	case out != nil:
		t.Errorf("MachineOutcome = %+v, want nil on 404", out)
	case !IsNotFound(err):
		t.Errorf("MachineOutcome err = %v, want a 404 the caller can classify", err)
	}
}
