package fly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// secretsFake is an httptest-backed fake of the Fly app-secrets API:
// set (POST /apps/{app}/secrets), list (GET) and delete (DELETE .../{name}).
// It keeps the app-wide secrets version Fly keeps — every set bumps it — and
// records every set so a test can assert which value reached Fly.
//
// The digest it reports is the SHA-256 of the value. That is the fake's own
// modeling and nothing here depends on it: the resource never compares a
// digest against anything, it only carries what the API returns.
type secretsFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	values  map[string]string
	version int
	sets    []map[string]string
	deletes []string
}

func newSecretsFake(t *testing.T) *secretsFake {
	t.Helper()
	f := &secretsFake{values: make(map[string]string)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// seed plants a secret as a previous apply left it, at the given version.
func (f *secretsFake) seed(name, value string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[name] = value
	f.version = version
}

func (f *secretsFake) lastSet() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) == 0 {
		return nil
	}
	return f.sets[len(f.sets)-1]
}

func (f *secretsFake) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sets)
}

func (f *secretsFake) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

func (f *secretsFake) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(parts) == 3 && parts[2] == "secrets" && r.Method == http.MethodPost:
		var body struct {
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "decode secrets body")
			return
		}
		for name, value := range body.Values {
			f.values[name] = value
		}
		f.version++
		f.sets = append(f.sets, body.Values)
		writeJSON(w, http.StatusOK, map[string]any{"secrets": f.wire(), "version": f.version})
	case len(parts) == 3 && parts[2] == "secrets" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"secrets": f.wire()})
	case len(parts) == 4 && parts[2] == "secrets" && r.Method == http.MethodDelete:
		delete(f.values, parts[3])
		f.deletes = append(f.deletes, parts[3])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *secretsFake) wire() []map[string]string {
	out := make([]map[string]string, 0, len(f.values))
	for name, value := range f.values {
		sum := sha256.Sum256([]byte(value))
		out = append(out, map[string]string{"name": name, "digest": hex.EncodeToString(sum[:])})
	}
	return out
}

// secretHarness drives fly_secret through the provider's wire protocol —
// PlanResourceChange, ApplyResourceChange, ReadResource — rather than calling
// the resource's methods, because the bug it exists for lives in the plan
// step: whether a changed value produces a planned state that differs from
// the prior one. A test that called Update directly would pass against code
// under which Update is never reached.
type secretHarness struct {
	t   *testing.T
	srv tfprotov6.ProviderServer
	typ tftypes.Object
}

const secretTypeName = "fly_secret"

func newSecretHarness(t *testing.T, f *secretsFake) *secretHarness {
	t.Helper()
	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}

	provType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"api_token": tftypes.String,
		"org_slug":  tftypes.String,
		"base_url":  tftypes.String,
	}}
	cfg, err := tfprotov6.NewDynamicValue(provType, tftypes.NewValue(provType, map[string]tftypes.Value{
		"api_token": tftypes.NewValue(tftypes.String, "t"),
		"org_slug":  tftypes.NewValue(tftypes.String, "test-org"),
		"base_url":  tftypes.NewValue(tftypes.String, f.srv.URL),
	}))
	if err != nil {
		t.Fatalf("encode provider config: %v", err)
	}
	resp, err := srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &cfg})
	if err != nil {
		t.Fatalf("ConfigureProvider: %v", err)
	}
	failOnDiagnostics(t, "ConfigureProvider", resp.Diagnostics)

	var sr resource.SchemaResponse
	NewSecretResource().Schema(context.Background(), resource.SchemaRequest{}, &sr)
	typ, ok := sr.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("fly_secret schema type is not an object")
	}
	return &secretHarness{t: t, srv: srv, typ: typ}
}

func failOnDiagnostics(t *testing.T, step string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("%s: %s: %s", step, d.Summary, d.Detail)
		}
	}
}

type secretConfig struct {
	app, name, value, valueVersion string
}

// config renders the resource block as core hands it to the provider: the
// four configured attributes set, the computed ones null.
func (h *secretHarness) config(c secretConfig) tftypes.Value {
	return tftypes.NewValue(h.typ, map[string]tftypes.Value{
		"id":               tftypes.NewValue(tftypes.String, nil),
		"app":              tftypes.NewValue(tftypes.String, c.app),
		"name":             tftypes.NewValue(tftypes.String, c.name),
		"value_wo":         tftypes.NewValue(tftypes.String, c.value),
		"value_wo_version": tftypes.NewValue(tftypes.String, c.valueVersion),
		"digest":           tftypes.NewValue(tftypes.String, nil),
		"version":          tftypes.NewValue(tftypes.Number, nil),
	})
}

// proposedNew builds the proposed new state the way core does before it
// calls PlanResourceChange: every attribute takes its configured value, and a
// computed attribute the config leaves null keeps its prior value. The
// write-only value is passed through like any other; the framework nulls it
// in the planned state before comparing against the prior, which is exactly
// why value_wo_version has to carry the diff.
func (h *secretHarness) proposedNew(prior, config tftypes.Value) tftypes.Value {
	h.t.Helper()
	if prior.IsNull() {
		return config
	}
	// objectAttrs hands back the value's own map, so the copy keeps the
	// config the framework sees free of the prior's computed values.
	priorAttrs, attrs := objectAttrs(h.t, prior), maps.Clone(objectAttrs(h.t, config))
	for _, computed := range []string{"id", "digest", "version"} {
		if attrs[computed].IsNull() {
			attrs[computed] = priorAttrs[computed]
		}
	}
	return tftypes.NewValue(h.typ, attrs)
}

func (h *secretHarness) dynamic(v tftypes.Value) *tfprotov6.DynamicValue {
	h.t.Helper()
	dv, err := tfprotov6.NewDynamicValue(h.typ, v)
	if err != nil {
		h.t.Fatalf("encode value: %v", err)
	}
	return &dv
}

func (h *secretHarness) decode(dv *tfprotov6.DynamicValue) tftypes.Value {
	h.t.Helper()
	v, err := dv.Unmarshal(h.typ)
	if err != nil {
		h.t.Fatalf("decode value: %v", err)
	}
	return v
}

// apply is one plan-then-apply of the resource, with core's own test for
// whether anything happens: the resource is applied only when the planned
// state differs from the prior state. A plan that equals its prior is the
// no-op core prints as "No changes" — and is what a rotation produced before
// this fix. A planned replacement fails the test outright, because a secret
// that is destroyed and re-created would drop the app's secrets version
// chain that fly_machine.min_secrets_version restarts on.
func (h *secretHarness) apply(prior tftypes.Value, c secretConfig) (state tftypes.Value, applied bool) {
	h.t.Helper()
	ctx := context.Background()
	config := h.config(c)

	planResp, err := h.srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         secretTypeName,
		PriorState:       h.dynamic(prior),
		ProposedNewState: h.dynamic(h.proposedNew(prior, config)),
		Config:           h.dynamic(config),
	})
	if err != nil {
		h.t.Fatalf("PlanResourceChange: %v", err)
	}
	failOnDiagnostics(h.t, "PlanResourceChange", planResp.Diagnostics)
	if len(planResp.RequiresReplace) > 0 {
		h.t.Fatalf("plan requires replacement at %v; a secret change must be an in-place update", planResp.RequiresReplace)
	}
	planned := h.decode(planResp.PlannedState)
	if planned.Equal(prior) {
		return prior, false
	}

	applyResp, err := h.srv.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     secretTypeName,
		PriorState:   h.dynamic(prior),
		PlannedState: planResp.PlannedState,
		Config:       h.dynamic(config),
	})
	if err != nil {
		h.t.Fatalf("ApplyResourceChange: %v", err)
	}
	failOnDiagnostics(h.t, "ApplyResourceChange", applyResp.Diagnostics)
	state = h.decode(applyResp.NewState)

	// Core's post-apply check: a value the plan stated as known must come
	// back unchanged, or the apply fails with "Provider produced inconsistent
	// result after apply". This is what a UseStateForUnknown on digest would
	// trip on every rotation, since Fly reports a new digest for a new value.
	newAttrs := objectAttrs(h.t, state)
	for name, plannedAttr := range objectAttrs(h.t, planned) {
		if plannedAttr.IsFullyKnown() && !plannedAttr.Equal(newAttrs[name]) {
			h.t.Fatalf("inconsistent result after apply: %s planned as %v, applied as %v", name, plannedAttr, newAttrs[name])
		}
	}
	return state, true
}

// refresh is the Read that core runs before every plan.
func (h *secretHarness) refresh(state tftypes.Value) tftypes.Value {
	h.t.Helper()
	resp, err := h.srv.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{
		TypeName:     secretTypeName,
		CurrentState: h.dynamic(state),
	})
	if err != nil {
		h.t.Fatalf("ReadResource: %v", err)
	}
	failOnDiagnostics(h.t, "ReadResource", resp.Diagnostics)
	return h.decode(resp.NewState)
}

func (h *secretHarness) nullState() tftypes.Value {
	return tftypes.NewValue(h.typ, nil)
}

// upgrade runs the version-0 -> 1 state migration the way core does when it
// meets a state stamped with an older schema version than the provider's.
func (h *secretHarness) upgrade(rawJSON string) tftypes.Value {
	h.t.Helper()
	resp, err := h.srv.UpgradeResourceState(context.Background(), &tfprotov6.UpgradeResourceStateRequest{
		TypeName: secretTypeName,
		Version:  0,
		RawState: &tfprotov6.RawState{JSON: []byte(rawJSON)},
	})
	if err != nil {
		h.t.Fatalf("UpgradeResourceState: %v", err)
	}
	failOnDiagnostics(h.t, "UpgradeResourceState", resp.Diagnostics)
	return h.decode(resp.UpgradedState)
}

// assertNoValueInState is the property this resource exists to hold: the
// secret reaches Fly and nothing else. Checked on every state the harness
// gets back, not just the one a test is about.
func assertNoValueInState(t *testing.T, step string, state tftypes.Value) {
	t.Helper()
	if state.IsNull() {
		return
	}
	if v := objectAttrs(t, state)["value_wo"]; !v.IsNull() {
		t.Errorf("%s: state holds value_wo = %v; the secret must never be persisted", step, v)
	}
}

var sidecarSecret = secretConfig{
	app:          "ampbase-org-abc",
	name:         "INGEST_SIDECAR_SECRET_ACCESS_KEY",
	value:        "key-1",
	valueVersion: "tid_1", // the key ID beside the secret, which moves with it
}

// TestSecret_RotationIsAnUpdate is the seam the signal org fell through: the
// source of a fly_secret's value (tigris_access_key.sidecar, re-created by a
// heal) changes between two applies, and the second apply must carry the new
// value to Fly. The value itself is write-only and cannot carry that diff —
// the framework nulls it in the planned state — so the rotation rides on
// value_wo_version, which the template sets to the key's own ID.
func TestSecret_RotationIsAnUpdate(t *testing.T) {
	t.Parallel()
	f := newSecretsFake(t)
	h := newSecretHarness(t, f)

	first, applied := h.apply(h.nullState(), sidecarSecret)
	if !applied {
		t.Fatal("the first apply planned no change, so the secret was never created")
	}
	assertNoValueInState(t, "create", first)
	firstVersion := int64Attr(t, objectAttrs(t, first)["version"])
	first = h.refresh(first)
	assertNoValueInState(t, "refresh", first)

	rotated := sidecarSecret
	rotated.value = "key-2"
	rotated.valueVersion = "tid_2"
	second, applied := h.apply(first, rotated)
	if !applied {
		t.Fatal("rotating the source value and its version planned no change, so Fly keeps key-1")
	}
	assertNoValueInState(t, "update", second)

	if got := f.lastSet()[sidecarSecret.name]; got != "key-2" {
		t.Errorf("Fly received %q, want the rotated key-2", got)
	}
	attrs := objectAttrs(t, second)
	if got := int64Attr(t, attrs["version"]); got <= firstVersion {
		t.Errorf("version = %d after the rotation, want above the create's %d so min_secrets_version moves", got, firstVersion)
	}
	if got := stringAttr(t, attrs["value_wo_version"]); got != "tid_2" {
		t.Errorf("state value_wo_version = %q, want tid_2", got)
	}
	if got, want := stringAttr(t, attrs["id"]), stringAttr(t, objectAttrs(t, first)["id"]); got != want {
		t.Errorf("id = %q after the rotation, want %q unchanged", got, want)
	}
	if f.deleteCount() != 0 {
		t.Error("the rotation deleted the secret, so it was a replacement rather than an update")
	}
}

// TestSecret_UnchangedValuePlansNothing: a re-apply with the same value and
// version must not re-set the secret, because every set bumps the app's
// secrets version and a moved version restarts every machine that references
// it.
func TestSecret_UnchangedValuePlansNothing(t *testing.T) {
	t.Parallel()
	f := newSecretsFake(t)
	h := newSecretHarness(t, f)

	state, applied := h.apply(h.nullState(), sidecarSecret)
	if !applied {
		t.Fatal("the first apply planned no change")
	}
	state = h.refresh(state)

	if _, applied := h.apply(state, sidecarSecret); applied {
		t.Error("an unchanged value planned a change; every sweep would restart the org's machines")
	}
	if n := f.setCount(); n != 1 {
		t.Errorf("Fly saw %d sets, want 1", n)
	}
}

// TestSecret_ValueChangeWithoutVersionIsInvisible pins the contract the
// schema description states rather than a behaviour anyone wants: with the
// value out of state there is nothing for a value-only change to diff
// against, so it plans no change and Fly keeps the old value. This is the
// price of not persisting the secret, and the reason value_wo_version is
// Required. A future design that detects the change some other way should
// delete this test deliberately, not find it failing.
func TestSecret_ValueChangeWithoutVersionIsInvisible(t *testing.T) {
	t.Parallel()
	f := newSecretsFake(t)
	h := newSecretHarness(t, f)

	state, applied := h.apply(h.nullState(), sidecarSecret)
	if !applied {
		t.Fatal("the first apply planned no change")
	}
	state = h.refresh(state)

	drifted := sidecarSecret
	drifted.value = "key-2"
	if _, applied := h.apply(state, drifted); applied {
		t.Error("a value change with an unchanged value_wo_version planned a change; the provider has no way to see one, so something is persisting the value")
	}
	if got := f.lastSet()[sidecarSecret.name]; got != "key-1" {
		t.Errorf("Fly holds %q, want key-1 untouched", got)
	}
}

// TestSecret_UpgradeDropsPersistedValue is the upgrade path from the schema
// that held the secret in state. Both version-0 shapes exist in the wild —
// value set (the persisted schema) and value null (the write-only schema
// before it) — and both must come out with no value, both new attributes
// null, and every other attribute intact. The first plan after the upgrade
// then sees null -> configured value_wo_version and re-sends the secret once,
// in place: never a replacement, so the machine restarts through
// min_secrets_version and the resource's identity survives.
func TestSecret_UpgradeDropsPersistedValue(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(sidecarSecret.value))
	digest := hex.EncodeToString(sum[:])
	id := sidecarSecret.app + "/" + sidecarSecret.name

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"persisted value", `"key-1"`},
		{"write-only null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSecretsFake(t)
			f.seed(sidecarSecret.name, sidecarSecret.value, 3)
			h := newSecretHarness(t, f)

			raw := `{"id":"` + id + `","app":"` + sidecarSecret.app + `","name":"` + sidecarSecret.name +
				`","value":` + tc.value + `,"digest":"` + digest + `","version":3}`
			prior := h.upgrade(raw)
			assertNoValueInState(t, "upgrade", prior)
			attrs := objectAttrs(t, prior)
			if !attrs["value_wo_version"].IsNull() {
				t.Errorf("upgraded value_wo_version = %v, want null so the first plan re-sends once", attrs["value_wo_version"])
			}
			if got := stringAttr(t, attrs["digest"]); got != digest {
				t.Errorf("upgraded digest = %q, want %q carried over", got, digest)
			}
			if got := int64Attr(t, attrs["version"]); got != 3 {
				t.Errorf("upgraded version = %d, want 3 carried over", got)
			}
			prior = h.refresh(prior)

			state, applied := h.apply(prior, sidecarSecret)
			if !applied {
				t.Fatal("upgraded state planned no change, so the version trigger never enters state and the next rotation is lost too")
			}
			assertNoValueInState(t, "post-upgrade apply", state)
			if got := f.lastSet()[sidecarSecret.name]; got != sidecarSecret.value {
				t.Errorf("Fly received %q, want %q", got, sidecarSecret.value)
			}
			attrs = objectAttrs(t, state)
			if got := int64Attr(t, attrs["version"]); got != 4 {
				t.Errorf("version = %d, want 4: one set on top of the seeded 3", got)
			}
			if got := stringAttr(t, attrs["id"]); got != id {
				t.Errorf("id = %q changed across the upgrade", got)
			}
			if f.deleteCount() != 0 {
				t.Error("the upgrade deleted the secret; a replacement would break the version chain")
			}
		})
	}
}

// TestMachine_MinSecretsVersionMovesInPlace pins the other end of the
// rotation: the org template feeds fly_secret.*.version into
// fly_machine.min_secrets_version, and that attribute must be an in-place
// update (Fly restarts the machine with the new secrets) rather than a
// replacement. Both halves are checked — the schema carries no plan modifier
// that could force a replace, and Update sends the moved version on the
// UpdateMachine call without a destroy.
func TestMachine_MinSecretsVersionMovesInPlace(t *testing.T) {
	t.Parallel()
	s := machineSchema(t)

	attr, ok := s.Attributes["min_secrets_version"].(rschema.Int64Attribute)
	if !ok {
		t.Fatal("min_secrets_version is not an Int64Attribute")
	}
	if len(attr.PlanModifiers) != 0 {
		t.Errorf("min_secrets_version carries plan modifiers %v; a RequiresReplace here would turn every secret rotation into a machine replacement", attr.PlanModifiers)
	}

	f := newMachinesFake(t)
	f.seed("m-rot", "started")
	r := f.resource(t)

	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan: mkPlan(t, s, machineResourceModel{
			App:               types.StringValue("app"),
			Image:             types.StringValue("img:v1"),
			MinSecretsVersion: types.Int64Value(4),
		}),
		State: mkState(t, s, machineResourceModel{
			ID:                types.StringValue("m-rot"),
			App:               types.StringValue("app"),
			Image:             types.StringValue("img:v1"),
			MinSecretsVersion: types.Int64Value(3),
		}),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update errored: %v", resp.Diagnostics)
	}
	if f.lastUpdate == nil || f.lastUpdate.MinSecretsVersion == nil || *f.lastUpdate.MinSecretsVersion != 4 {
		t.Errorf("UpdateMachine min_secrets_version = %v, want 4", deref(f.lastUpdate.MinSecretsVersion))
	}
	if f.destroyCalled() {
		t.Errorf("moving min_secrets_version destroyed the machine; calls: %v", f.callPaths())
	}
}
