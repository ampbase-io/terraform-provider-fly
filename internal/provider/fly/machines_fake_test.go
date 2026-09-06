package fly

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

// machinesFake is an httptest-backed fake of the Fly Machines API, scoped to
// the subset of endpoints the fly_machine resource drives:
// create/get/update/start/stop/wait/list/delete, plus volume get for the
// pre-create mount check. It is the provider-side analogue of the repo's
// gofakes3 S3 fakes — flyio.New(WithBaseURL(fake.url())) points the real
// client at it, so tests exercise the genuine Create/Update code paths without
// a live Fly.
//
// Every request is recorded in call order so tests can assert control-flow
// invariants — notably that the desired_state park fallback issues StopMachine
// only after the settle-wait fails, and never for a machine that reaches
// "stopped" on its own (the invariant PR #430's review flagged as untestable
// without a harness).
//
// Kept deliberately generic — no concurrency- or suspension-specific logic —
// so later provider work (Phase 6's guest resize, future lifecycle tests)
// reuses it.
type machinesFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	machines map[string]*fakeMachine
	volumes  map[string]string // volume ID -> attached_machine_id
	calls    []fakeCall
	nextID   int
	nextInst int

	// lastCreate / lastUpdate capture the decoded request bodies of the most
	// recent MachinesCreate / MachinesUpdate so tests can assert what was sent
	// on the wire (e.g. the concurrency round-trip).
	lastCreate *machines.CreateMachineRequest
	lastUpdate *machines.UpdateMachineRequest

	// Injection knobs applied to machines minted by MachinesCreate, so Create
	// failure paths can be driven without a pre-seeded machine to mutate.
	createState      string // initial state; "" defaults to "started"
	createShowStatus int    // MachinesShow status for created machines (0 = OK)
	createStopStatus int    // MachinesStop status for created machines (0 = OK)
}

// fakeCall is one recorded request: method + cleaned path + the `state` query
// value (populated for the /wait endpoint so ordering assertions can tell a
// wait-for-stopped from a wait-for-started).
type fakeCall struct {
	Method string
	Path   string
	State  string
}

type fakeMachine struct {
	id         string
	name       string
	state      string
	instanceID string
	region     string
	privateIP  string

	// postUpdateState is the state the machine rests in after an
	// UpdateMachine, as reported by MachinesShow and MachinesWait. When it
	// differs from the state a wait asks for, that wait 408s (server-side
	// timeout). Empty leaves the state unchanged.
	postUpdateState string
	// replacingPolls is how many MachinesShow calls after an UpdateMachine
	// report "replacing" before the machine reports postUpdateState — the
	// replacing → rest transition the provider's settle wait rides out. Set
	// it to more than one so a test proves the wait actually loops.
	replacingPolls int
	// showFailAfterUpdate, when non-zero, makes the first MachinesShow after
	// an UpdateMachine return that HTTP status, once. A non-transient status
	// (409) is how a test makes the settle wait fail without waiting out
	// machineSettleTimeout. showFailArmed is the pending one-off.
	showFailAfterUpdate int
	showFailArmed       int

	// showStatus, when non-zero, makes MachinesShow return that HTTP status.
	// Used to force a fast terminal failure in the WaitForChecks phase so the
	// Create cleanup path is exercised without waiting out machineStartTimeout.
	showStatus int
	// stopStatus, when non-zero, makes MachinesStop return that HTTP status.
	stopStatus int
}

func newMachinesFake(t *testing.T) *machinesFake {
	t.Helper()
	f := &machinesFake{
		machines: make(map[string]*fakeMachine),
		volumes:  make(map[string]string),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a real flyio.Client pointed at the fake.
func (f *machinesFake) client(t *testing.T) *flyio.Client {
	t.Helper()
	c, err := flyio.New("test-org", flyio.WithToken("t"), flyio.WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	return c
}

// resource returns a machineResource wired to the fake, ready to drive
// Create/Update against.
func (f *machinesFake) resource(t *testing.T) *machineResource {
	t.Helper()
	return &machineResource{pd: &providerData{client: f.client(t), orgSlug: "test-org"}}
}

// seed inserts a machine so Update tests have something to update. Returns the
// machine so the test can set postUpdateState / injection knobs before the run.
func (f *machinesFake) seed(id, state string) *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextInst++
	m := &fakeMachine{
		id:         id,
		name:       id,
		state:      state,
		instanceID: fmt.Sprintf("inst-%d", f.nextInst),
		region:     "iad",
		privateIP:  "fdaa::1",
	}
	f.machines[id] = m
	return m
}

// get looks up a seeded machine (e.g. to read final state after a run).
func (f *machinesFake) get(id string) *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machines[id]
}

// callPaths returns the recorded (method + path) pairs in order.
func (f *machinesFake) callPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Method + " " + c.Path
	}
	return out
}

// stopCalled reports whether MachinesStop was ever hit.
func (f *machinesFake) stopCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/stop") {
			return true
		}
	}
	return false
}

// startCalled reports whether MachinesStart was ever hit.
func (f *machinesFake) startCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/start") {
			return true
		}
	}
	return false
}

// destroyCalled reports whether MachinesDelete was ever hit.
func (f *machinesFake) destroyCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Method == http.MethodDelete {
			return true
		}
	}
	return false
}

// actionFollowsSettle asserts the ordering invariant of the cold-machine
// paths: every MachinesStart/MachinesStop that follows an UpdateMachine is
// preceded by at least one MachinesShow after that update, i.e. the provider
// looks at where the machine came to rest before acting on it, and never
// starts or stops a machine still mid-replace. Returns false if a start or
// stop appears with no show since the last update.
func (f *machinesFake) actionFollowsSettle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	observed := true
	for _, c := range f.calls {
		isAction := strings.HasSuffix(c.Path, "/start") || strings.HasSuffix(c.Path, "/stop")
		switch {
		case c.Method == http.MethodPost && !isAction && strings.Contains(c.Path, "/machines/"):
			observed = false // UpdateMachine
		case c.Method == http.MethodGet && !strings.HasSuffix(c.Path, "/wait") && strings.Contains(c.Path, "/machines/"):
			observed = true // MachinesShow
		case c.Method == http.MethodPost && isAction && !observed:
			return false
		}
	}
	return true
}

func (f *machinesFake) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1"), "/"), "/")
	f.record(r)

	// Routes: /apps/{app}/machines[/{id}[/{action}]] and /apps/{app}/volumes/{id}
	switch {
	case len(parts) == 4 && parts[2] == "volumes":
		f.handleVolume(w, parts[3])
	case len(parts) == 3 && parts[2] == "machines":
		f.handleCollection(w, r)
	case len(parts) == 4 && parts[2] == "machines":
		f.handleMachine(w, r, parts[3])
	case len(parts) == 5 && parts[2] == "machines":
		f.handleAction(w, parts[3], parts[4], r)
	default:
		http.NotFound(w, r)
	}
}

func (f *machinesFake) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{
		Method: r.Method,
		Path:   strings.TrimRight(strings.TrimPrefix(r.URL.Path, "/v1"), "/"),
		State:  r.URL.Query().Get("state"),
	})
}

func (f *machinesFake) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		f.createMachine(w, r)
	case http.MethodGet:
		f.listMachines(w)
	default:
		http.NotFound(w, r)
	}
}

func (f *machinesFake) createMachine(w http.ResponseWriter, r *http.Request) {
	var body machines.CreateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "decode create body")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreate = &body
	f.nextID++
	f.nextInst++
	id := fmt.Sprintf("m%d", f.nextID)
	name := id
	if body.Name != nil {
		name = *body.Name
	}
	region := "iad"
	if body.Region != nil {
		region = *body.Region
	}
	state := f.createState
	if state == "" {
		state = "started" // a freshly created machine boots to started
	}
	m := &fakeMachine{
		id:         id,
		name:       name,
		state:      state,
		instanceID: fmt.Sprintf("inst-%d", f.nextInst),
		region:     region,
		privateIP:  "fdaa::1",
		showStatus: f.createShowStatus,
		stopStatus: f.createStopStatus,
	}
	f.machines[id] = m
	// Create response is decoded into flyio.Machine{ID, InstanceID}.
	writeJSON(w, http.StatusOK, map[string]string{"id": m.id, "instance_id": m.instanceID})
}

func (f *machinesFake) listMachines(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]machines.Machine, 0, len(f.machines))
	for _, m := range f.machines {
		out = append(out, m.wire())
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *machinesFake) handleMachine(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		f.showMachine(w, id)
	case http.MethodPost:
		f.updateMachine(w, r, id)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.machines, id)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *machinesFake) showMachine(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "machine not found")
		return
	}
	if m.showStatus != 0 {
		writeErr(w, m.showStatus, "injected show failure")
		return
	}
	if m.showFailArmed != 0 {
		status := m.showFailArmed
		m.showFailArmed = 0
		writeErr(w, status, "injected one-off show failure")
		return
	}
	wire := m.wire()
	if m.replacingPolls > 0 {
		m.replacingPolls--
		wire.State = sp("replacing")
	}
	writeJSON(w, http.StatusOK, wire)
}

func (f *machinesFake) updateMachine(w http.ResponseWriter, r *http.Request, id string) {
	var body machines.UpdateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "decode update body")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUpdate = &body
	m, ok := f.machines[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "machine not found")
		return
	}
	f.nextInst++
	m.instanceID = fmt.Sprintf("inst-%d", f.nextInst)
	if m.postUpdateState != "" {
		m.state = m.postUpdateState
	}
	m.showFailArmed = m.showFailAfterUpdate
	writeJSON(w, http.StatusOK, m.wire())
}

func (f *machinesFake) handleAction(w http.ResponseWriter, id, action string, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "machine not found")
		return
	}
	switch action {
	case "start":
		m.state = "started"
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case "stop":
		if m.stopStatus != 0 {
			writeErr(w, m.stopStatus, "injected stop failure")
			return
		}
		m.state = machineStateStopped
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case "wait":
		want := r.URL.Query().Get("state")
		if m.state == want {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		// Server-side wait timeout: the machine is not (yet) in the requested
		// state. The provider treats 408 as a permanent signal (not retryable),
		// so the wait fails fast rather than blocking on the ctx deadline.
		writeErr(w, http.StatusRequestTimeout, fmt.Sprintf("machine in state %q, waited for %q", m.state, want))
	default:
		http.NotFound(w, r)
	}
}

func (f *machinesFake) handleVolume(w http.ResponseWriter, id string) {
	f.mu.Lock()
	attached := f.volumes[id]
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"id":                  id,
		"state":               "created",
		"attached_machine_id": attached,
	})
}

func (m *fakeMachine) wire() machines.Machine {
	return machines.Machine{
		Id:         sp(m.id),
		Name:       sp(m.name),
		InstanceId: sp(m.instanceID),
		State:      sp(m.state),
		Region:     sp(m.region),
		PrivateIp:  sp(m.privateIP),
		// Config left nil: no checks configured, so WaitForChecks returns
		// immediately. Concurrency assertions read the request body, not this.
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func sp(s string) *string { return &s }
