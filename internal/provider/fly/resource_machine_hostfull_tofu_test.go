package fly

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestMachineHostFull_NextPlanReplacesOnlyThatMachine holds the private-state
// mark end to end, against the real tofu: an update its host refuses fails
// the apply and marks the machine; the next plan replaces that machine and
// no other; and the plan after that is clean, so the mark does not carry
// onto the replacement.
//
// Its negative control is deleting the SetKey in Update's host-full case: the
// second plan is an in-place update of org[1], which its host refuses again.
func TestMachineHostFull_NextPlanReplacesOnlyThatMachine(t *testing.T) {
	t.Parallel()
	h := newTofuHarness(t)

	h.writeMachines(t, "img:v1")
	h.mustApply(t)
	before := h.fly.live()
	h.fly.refuseUpdate(before["1"].id)

	h.writeMachines(t, "img:v2")
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil || !strings.Contains(out, "Error: Machine Host Full") {
		t.Fatalf("apply against a full host: err %v, want it to fail on Machine Host Full:\n%s", err, out)
	}

	if got, want := h.planActions(t), map[string]string{"fly_machine.org[1]": "create,delete"}; !mapsEqual(got, want) {
		t.Errorf("plan after the refusal = %v, want %v: a replace of the refused machine and nothing else", got, want)
	}
	h.mustApply(t)
	after := h.fly.live()
	for _, slot := range []string{"0", "2"} {
		if after[slot].id != before[slot].id {
			t.Errorf("org[%s] was replaced (%s → %s); only the machine on a full host should be", slot, before[slot].id, after[slot].id)
		}
	}
	if after["1"].id == before["1"].id || after["1"].image != "img:v2" {
		t.Errorf("org[1] = %+v, want a new machine on img:v2 in place of %s", after["1"], before["1"].id)
	}
	if got, want := h.fly.destroyed(), []string{before["1"].id}; !slices.Equal(got, want) {
		t.Errorf("destroyed %v, want %v", got, want)
	}

	if got := h.planActions(t); len(got) != 0 {
		t.Errorf("plan after the replace = %v, want none: the mark carried onto the new machine", got)
	}
}

// TestMachineHostFull_LegacyMachineIsReplacedAtItsMovedAddress starts from
// the pre-count state a moved block adopts as [0], and refuses the update in
// the apply that moves it. The mark has to survive the move for the next plan
// to replace fly_machine.org[0].
func TestMachineHostFull_LegacyMachineIsReplacedAtItsMovedAddress(t *testing.T) {
	t.Parallel()
	h := newTofuHarness(t)

	h.writeLegacyMachine(t, "img:v1")
	h.mustApply(t)
	legacy := h.fly.live()["0"]
	h.fly.refuseUpdate(legacy.id)

	h.writeMachines(t, "img:v2")
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil || !strings.Contains(out, "Error: Machine Host Full") {
		t.Fatalf("moving apply against a full host: err %v, want it to fail on Machine Host Full:\n%s", err, out)
	}

	if got, want := h.planActions(t), map[string]string{"fly_machine.org[0]": "create,delete"}; !mapsEqual(got, want) {
		t.Errorf("plan after the refusal = %v, want %v", got, want)
	}
	h.mustApply(t)
	if got := h.fly.live()["0"]; got.id == legacy.id {
		t.Errorf("org[0] kept the legacy machine %s on a full host", legacy.id)
	}
	if got := h.planActions(t); len(got) != 0 {
		t.Errorf("plan after the replace = %v, want none", got)
	}
}

// TestMachineHostFull_RefusedReplacementKeepsTheMarkOnTheOldMachine starts
// from a replace whose create is refused too. create_before_destroy keeps
// the old machine, and the mark has to stay with it, so the plan after the
// failed replace still replaces it rather than falling back to an in-place
// update the full host would refuse again.
func TestMachineHostFull_RefusedReplacementKeepsTheMarkOnTheOldMachine(t *testing.T) {
	t.Parallel()
	h := newTofuHarness(t)

	h.writeMachines(t, "img:v1")
	h.mustApply(t)
	before := h.fly.live()
	h.fly.refuseUpdate(before["1"].id)

	h.writeMachines(t, "img:v2")
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil {
		t.Fatalf("apply against a full host succeeded:\n%s", out)
	}

	h.fly.refuseCreates(true)
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil {
		t.Fatalf("replace with its create refused succeeded:\n%s", out)
	}
	if got := h.fly.live()["1"].id; got != before["1"].id {
		t.Fatalf("org[1] is %s after a refused replace, want the old %s kept", got, before["1"].id)
	}

	h.fly.refuseCreates(false)
	if got, want := h.planActions(t), map[string]string{"fly_machine.org[1]": "create,delete"}; !mapsEqual(got, want) {
		t.Errorf("plan after the refused replace = %v, want %v: the kept machine lost its mark", got, want)
	}
	h.mustApply(t)
	if got := h.fly.live()["1"]; got.id == before["1"].id || got.image != "img:v2" {
		t.Errorf("org[1] = %+v, want a new machine on img:v2", got)
	}
}

// TestMachineHostFull_NamedMachineIsReplacedUnderItsName holds the replace
// for a machine with a name, which Fly keeps unique per app. Under
// create_before_destroy the replacement finds the name held by the machine
// it replaces, and has to take it rather than adopt it: adopting hands tofu
// the old machine's ID as the new object, and the deposed delete then
// destroys the only machine left. Destroy-then-create frees the name first.
//
// Its negative control is making replacingForHostFull always false: the
// create_before_destroy row ends with org[1] gone from Fly.
func TestMachineHostFull_NamedMachineIsReplacedUnderItsName(t *testing.T) {
	t.Parallel()
	for _, cbd := range []bool{true, false} {
		t.Run(fmt.Sprintf("create_before_destroy=%v", cbd), func(t *testing.T) {
			t.Parallel()
			h := newTofuHarness(t)

			h.writeNamedMachines(t, "img:v1", cbd)
			h.mustApply(t)
			before := h.fly.live()
			h.fly.refuseUpdate(before["1"].id)

			h.writeNamedMachines(t, "img:v2", cbd)
			if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil || !strings.Contains(out, "Error: Machine Host Full") {
				t.Fatalf("apply against a full host: err %v, want it to fail on Machine Host Full:\n%s", err, out)
			}
			h.mustApply(t)

			after := h.fly.live()
			if got := after["1"]; got.id == "" || got.id == before["1"].id || got.name != "vm-1" || got.image != "img:v2" {
				t.Errorf("org[1] = %+v, want a new machine named vm-1 on img:v2 in place of %s; live: %+v", got, before["1"].id, after)
			}
			if got, want := h.fly.destroyed(), []string{before["1"].id}; !slices.Equal(got, want) {
				t.Errorf("destroyed %v, want %v", got, want)
			}
			if got := h.planActions(t); len(got) != 0 {
				t.Errorf("plan after the replace = %v, want none", got)
			}
		})
	}
}

// TestMachineHostFull_UntaggedMachineIsNotMarked: when the metadata write
// fails, a named replacement could not tell the machine it replaces from its
// own, so Update marks nothing and the next plan is the in-place update
// again. Its negative control is marking despite the failed write, which
// makes that plan a replace.
func TestMachineHostFull_UntaggedMachineIsNotMarked(t *testing.T) {
	t.Parallel()
	h := newTofuHarness(t)

	h.writeNamedMachines(t, "img:v1", true)
	h.mustApply(t)
	h.fly.refuseUpdate(h.fly.live()["1"].id)
	h.fly.refuseTags()

	h.writeNamedMachines(t, "img:v2", true)
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err == nil || !strings.Contains(out, "Tagging it for replacement failed") {
		t.Fatalf("apply with the tag refused: err %v, want it to say the tag failed:\n%s", err, out)
	}
	if got, want := h.planActions(t), map[string]string{"fly_machine.org[1]": "update"}; !mapsEqual(got, want) {
		t.Errorf("plan after an untagged refusal = %v, want %v", got, want)
	}
}

// TestMachineHostFull_UnclassifiedRefusalWarnsInTheApplyOutput holds the
// signal that Fly moved the refusal off 409 where a run log shows it: the
// warning diagnostic tofu prints in its normal output, with no TF_LOG. Its
// negative control is dropping the AddWarning in Update.
func TestMachineHostFull_UnclassifiedRefusalWarnsInTheApplyOutput(t *testing.T) {
	t.Parallel()
	h := newTofuHarness(t)

	h.writeMachines(t, "img:v1")
	h.mustApply(t)
	h.fly.refuseUpdateWith(h.fly.live()["1"].id, http.StatusBadRequest)

	h.writeMachines(t, "img:v2")
	out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color")
	if err == nil {
		t.Fatalf("apply against a refused update succeeded:\n%s", out)
	}
	if !strings.Contains(out, "Warning: "+unclassifiedHostFull) {
		t.Errorf("apply output has no %q warning:\n%s", unclassifiedHostFull, out)
	}
	if got, want := h.planActions(t), map[string]string{"fly_machine.org[1]": "update"}; !mapsEqual(got, want) {
		t.Errorf("plan after an unclassified refusal = %v, want %v: nothing is marked", got, want)
	}
}

// tofuHarness is one work dir driven by the real tofu, with this provider
// built from source behind a dev override and a fake Machines API.
type tofuHarness struct {
	bin, dir string
	env      []string
	fly      *tofuFly
}

func newTofuHarness(t *testing.T) *tofuHarness {
	t.Helper()
	bin := os.Getenv("TOFU_BINARY")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("tofu"); err != nil {
			t.Skip("no tofu binary on PATH or in TOFU_BINARY")
		}
	}

	plugins := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(plugins, "terraform-provider-fly"), ".")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build provider: %v\n%s", err, out)
	}

	dir := t.TempDir()
	rc := fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n", "registry.opentofu.org/ampbase-io/fly", plugins)
	if err := os.WriteFile(filepath.Join(dir, ".tofurc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tofuHarness{
		bin: bin,
		dir: dir,
		env: []string{"TF_CLI_CONFIG_FILE=" + filepath.Join(dir, ".tofurc"), "CHECKPOINT_DISABLE=1", "HOME=" + dir},
		fly: newTofuFly(t),
	}
}

// tofu runs one command and returns its stdout and stderr together.
func (h *tofuHarness) tofu(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := h.run(t, &out, &out, args...)
	return out.String(), err
}

// run runs one command with its streams where the caller wants them: plan
// JSON is stdout alone, since the dev override's warning goes to stderr.
func (h *tofuHarness) run(t *testing.T, stdout, stderr *bytes.Buffer, args ...string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), h.bin, args...)
	cmd.Dir = h.dir
	cmd.Env = h.env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (h *tofuHarness) mustApply(t *testing.T) {
	t.Helper()
	if out, err := h.tofu(t, "apply", "-input=false", "-auto-approve", "-no-color"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
}

// planActions is every resource change the next plan makes, by address, as
// tofu's own plan JSON names its actions.
func (h *tofuHarness) planActions(t *testing.T) map[string]string {
	t.Helper()
	if out, err := h.tofu(t, "plan", "-input=false", "-no-color", "-out=plan.tfplan"); err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	var raw, stderr bytes.Buffer
	if err := h.run(t, &raw, &stderr, "show", "-json", "plan.tfplan"); err != nil {
		t.Fatalf("show -json: %v\n%s", err, stderr.String())
	}
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(raw.Bytes(), &plan); err != nil {
		t.Fatalf("parse plan json: %v\n%s", err, raw.String())
	}
	got := map[string]string{}
	for _, rc := range plan.ResourceChanges {
		if a := strings.Join(rc.Change.Actions, ","); a != "no-op" {
			got[rc.Address] = a
		}
	}
	return got
}

// writeMachines is three machines at fly_machine.org[n], as ampbase's runtime
// half declares them: no name, create_before_destroy, and the moved block
// that adopts a pre-count machine as [0]. SLOT tells the fake them apart.
func (h *tofuHarness) writeMachines(t *testing.T, image string) {
	t.Helper()
	h.writeModule(t, fmt.Sprintf(`
moved {
  from = fly_machine.org
  to   = fly_machine.org[0]
}

resource "fly_machine" "org" {
  count = 3
  app   = "ampbase-org-test"
  image = %q
  env   = { SLOT = "${count.index}" }

  lifecycle {
    create_before_destroy = true
  }
}
`, image))
}

// writeNamedMachines is three machines named vm-0..vm-2, the shape of a
// cluster module that names its machines.
func (h *tofuHarness) writeNamedMachines(t *testing.T, image string, createBeforeDestroy bool) {
	t.Helper()
	h.writeModule(t, fmt.Sprintf(`
resource "fly_machine" "org" {
  count = 3
  app   = "ampbase-org-test"
  name  = "vm-${count.index}"
  image = %q
  env   = { SLOT = "${count.index}" }

  lifecycle {
    create_before_destroy = %v
  }
}
`, image, createBeforeDestroy))
}

// writeLegacyMachine is the singular machine the moved block exists for.
func (h *tofuHarness) writeLegacyMachine(t *testing.T, image string) {
	t.Helper()
	h.writeModule(t, fmt.Sprintf(`
resource "fly_machine" "org" {
  app   = "ampbase-org-test"
  image = %q
  env   = { SLOT = "0" }

  lifecycle {
    create_before_destroy = true
  }
}
`, image))
}

func (h *tofuHarness) writeModule(t *testing.T, body string) {
	t.Helper()
	hcl := fmt.Sprintf(`terraform {
  required_providers {
    fly = { source = "ampbase-io/fly" }
  }
}

provider "fly" {
  org_slug  = "test-org"
  api_token = "test-token"
  base_url  = %q
}
`, h.fly.URL) + body
	if err := os.WriteFile(filepath.Join(h.dir, "main.tf"), []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// tofuFly is the part of the Machines API fly_machine drives over a plan and
// an apply: list, create, show, update, metadata, wait, start, stop, delete.
// A machine whose ID is refused answers every update the way a full host
// does. Names are unique, as Fly keeps them: a create under a name a live
// machine holds is a 422.
type tofuFly struct {
	*httptest.Server

	mu             sync.Mutex
	machines       map[string]*tofuFlyMachine
	refused        map[string]int
	createsRefused bool
	tagsRefused    bool
	gone           []string
	next           int
}

type tofuFlyMachine struct {
	id, name, slot, image, instance string
	metadata                        map[string]string
}

func newTofuFly(t *testing.T) *tofuFly {
	t.Helper()
	f := &tofuFly{machines: map[string]*tofuFlyMachine{}, refused: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *tofuFly) refuseUpdate(id string) {
	f.refuseUpdateWith(id, http.StatusConflict)
}

// refuseUpdateWith refuses every update of id with the host-full message
// under status.
func (f *tofuFly) refuseUpdateWith(id string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[id] = status
}

// refuseCreates makes every create answer the way a full host does, or stops.
func (f *tofuFly) refuseCreates(refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createsRefused = refuse
}

// refuseTags makes every metadata write fail.
func (f *tofuFly) refuseTags() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagsRefused = true
}

func (f *tofuFly) live() map[string]tofuFlyMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]tofuFlyMachine{}
	for _, m := range f.machines {
		out[m.slot] = *m
	}
	return out
}

func (f *tofuFly) destroyed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gone)
}

type tofuFlyConfig struct {
	Name   string `json:"name"`
	Config struct {
		Image    string            `json:"image"`
		Env      map[string]string `json:"env"`
		Metadata map[string]string `json:"metadata"`
	} `json:"config"`
}

func (f *tofuFly) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/apps/ampbase-org-test/machines"), "/")
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		list := make([]map[string]any, 0, len(f.machines))
		for _, m := range f.machines {
			list = append(list, m.wire())
		}
		writeJSON(w, http.StatusOK, list)
	case len(parts) == 1 && r.Method == http.MethodPost:
		var body tofuFlyConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if f.createsRefused {
			writeErr(w, http.StatusConflict, hostFullBody)
			return
		}
		if f.nameTaken(body.Name) {
			writeErr(w, http.StatusUnprocessableEntity, "a machine named "+body.Name+" already exists")
			return
		}
		f.next++
		m := &tofuFlyMachine{
			id: fmt.Sprintf("m-%d", f.next), name: body.Name, slot: body.Config.Env["SLOT"],
			image: body.Config.Image, instance: fmt.Sprintf("i-%d", f.next), metadata: body.Config.Metadata,
		}
		f.machines[m.id] = m
		writeJSON(w, http.StatusOK, m.wire())
	case len(parts) < 2:
		http.NotFound(w, r)
	default:
		f.machine(w, r, parts[1], parts[2:])
	}
}

func (f *tofuFly) nameTaken(name string) bool {
	if name == "" {
		return false
	}
	for _, m := range f.machines {
		if m.name == name {
			return true
		}
	}
	return false
}

func (f *tofuFly) machine(w http.ResponseWriter, r *http.Request, id string, action []string) {
	m, ok := f.machines[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "machine not found")
		return
	}
	switch {
	case len(action) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, m.wire())
	case len(action) == 0 && r.Method == http.MethodDelete:
		delete(f.machines, id)
		f.gone = append(f.gone, id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case len(action) == 2 && action[0] == "metadata" && r.Method == http.MethodPost && f.tagsRefused:
		writeErr(w, http.StatusBadRequest, "metadata refused")
	case len(action) == 2 && action[0] == "metadata" && r.Method == http.MethodPost:
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if m.metadata == nil {
			m.metadata = map[string]string{}
		}
		m.metadata[action[1]] = body.Value
		w.WriteHeader(http.StatusNoContent)
	case len(action) == 0 && r.Method == http.MethodPost && f.refused[id] != 0:
		writeErr(w, f.refused[id], hostFullBody)
	case len(action) == 0 && r.Method == http.MethodPost:
		var body tofuFlyConfig
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		f.next++
		m.image = body.Config.Image
		m.metadata = body.Config.Metadata
		m.instance = fmt.Sprintf("i-%d", f.next)
		writeJSON(w, http.StatusOK, m.wire())
	default:
		// wait, start and stop: every machine here is already started.
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (m *tofuFlyMachine) wire() map[string]any {
	return map[string]any{
		"id":          m.id,
		"name":        m.name,
		"state":       "started",
		"instance_id": m.instance,
		"region":      "iad",
		"private_ip":  "fdaa::1",
		"config":      map[string]any{"image": m.image, "env": map[string]string{"SLOT": m.slot}, "metadata": m.metadata},
		"image_ref": map[string]any{
			"repository": strings.Split(m.image, ":")[0],
			"digest":     fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(m.image))),
		},
	}
}
