package fly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

// appNumericID is what the fake's Apps_show returns for every app. Distinct
// from any zero value a dropped attribute would leave behind.
const appNumericID = 214748364700

// appsFake serves the two Apps endpoints fly_app.Create drives — POST /apps
// and GET /apps/{name} — and reports back whatever org the create named, so
// a test can tell an org that reached Fly from one that only reached state.
type appsFake struct {
	srv *httptest.Server

	mu   sync.Mutex
	apps map[string]map[string]string // name -> {org_slug, network}
}

func newAppsFake(t *testing.T) *appsFake {
	t.Helper()
	f := &appsFake{apps: make(map[string]map[string]string)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *appsFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "decode create body")
			return
		}
		f.apps[body["name"]] = body
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && len(r.URL.Path) > len("/v1/apps/"):
		app, ok := f.apps[r.URL.Path[len("/v1/apps/"):]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":                  "app-" + app["name"],
			"name":                app["name"],
			"organization":        map[string]string{"slug": app["org_slug"]},
			"network":             app["network"],
			"internal_numeric_id": appNumericID,
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *appsFake) resource(t *testing.T) *appResource {
	t.Helper()
	client, err := flyio.New("provider-org", flyio.WithToken("t"), flyio.WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return &appResource{pd: &providerData{client: client, orgSlug: "provider-org"}}
}

func appSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewAppResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func appPlan(t *testing.T, s rschema.Schema, m appResourceModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: s}
	if diags := p.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("build plan: %v", diags)
	}
	return p
}

// TestApp_ConfiguredOrgReachesFly: `org` is Optional and the client carries
// the provider's org as its default, so the one way to get this wrong is to
// record the configured org in state without sending it. That passes the
// post-apply consistency check, and then the next refresh reads the real org
// back, `org` is RequiresReplace, and every apply recreates the app in the
// wrong org. The counterfactual is the create call ignoring plan.Org.
func TestApp_ConfiguredOrgReachesFly(t *testing.T) {
	t.Parallel()
	f := newAppsFake(t)
	r := f.resource(t)
	s := appSchema(t)

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: appPlan(t, s, appResourceModel{
			Name:    types.StringValue("other-app"),
			Org:     types.StringValue("other-org"),
			Network: types.StringUnknown(),
			ID:      types.StringUnknown(),
		}),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create errored: %v", resp.Diagnostics)
	}

	f.mu.Lock()
	sent := f.apps["other-app"]["org_slug"]
	f.mu.Unlock()
	if sent != "other-org" {
		t.Errorf("Fly received org_slug %q, want the configured other-org", sent)
	}

	var state appResourceModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if got := state.Org.ValueString(); got != "other-org" {
		t.Errorf("state org = %q, want other-org", got)
	}

	// The refresh that follows must agree with what Create stored, or the
	// RequiresReplace on org plans a recreate.
	readResp := &resource.ReadResponse{State: tfsdk.State{Schema: s}}
	r.Read(context.Background(), resource.ReadRequest{State: resp.State}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read errored: %v", readResp.Diagnostics)
	}
	var refreshed appResourceModel
	if diags := readResp.State.Get(context.Background(), &refreshed); diags.HasError() {
		t.Fatalf("read refreshed state: %v", diags)
	}
	if got := refreshed.Org.ValueString(); got != "other-org" {
		t.Errorf("refreshed org = %q, want other-org; the next plan would replace the app", got)
	}
}

// TestApp_DefaultOrgIsTheProviders: with `org` unset the app lands in the
// provider's org, and state records that org rather than null so the
// Optional+Computed attribute plans clean afterwards.
func TestApp_DefaultOrgIsTheProviders(t *testing.T) {
	t.Parallel()
	f := newAppsFake(t)
	r := f.resource(t)
	s := appSchema(t)

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: appPlan(t, s, appResourceModel{
			Name:    types.StringValue("default-app"),
			Org:     types.StringUnknown(),
			Network: types.StringUnknown(),
			ID:      types.StringUnknown(),
		}),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create errored: %v", resp.Diagnostics)
	}

	f.mu.Lock()
	sent := f.apps["default-app"]["org_slug"]
	f.mu.Unlock()
	if sent != "provider-org" {
		t.Errorf("Fly received org_slug %q, want the provider's provider-org", sent)
	}
	var state appResourceModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if got := state.Org.ValueString(); got != "provider-org" {
		t.Errorf("state org = %q, want provider-org", got)
	}
}

// TestApp_EmptyOrgIsRejected: an explicit `org = ""` would otherwise be
// indistinguishable from unset and fall back to the provider's org without
// a word, so the schema refuses it at validation time.
func TestApp_EmptyOrgIsRejected(t *testing.T) {
	t.Parallel()
	s := appSchema(t)
	attr, ok := s.Attributes["org"].(rschema.StringAttribute)
	if !ok {
		t.Fatal("org is not a StringAttribute")
	}
	req := validator.StringRequest{
		Path:        path.Root("org"),
		ConfigValue: types.StringValue(""),
	}
	var resp validator.StringResponse
	for _, v := range attr.Validators {
		v.ValidateString(context.Background(), req, &resp)
	}
	if !resp.Diagnostics.HasError() {
		t.Error(`org = "" passed validation; it would silently create the app in the provider's org`)
	}
}

// TestApp_NumericIDReachesStateOnCreateAndImport: Apps_show returns
// internal_numeric_id and it is the identifier an Apps macaroon caveat names,
// so a consumer attenuating a token to one app reads it off this resource
// rather than calling Fly itself. Create has to record it, and so does Read,
// which is the only path that fills it in after an import — ImportState
// passes through `name` alone, so an import whose Read forgets the number
// leaves a resource whose app cannot be named until it is replaced.
//
// The second half starts from the import shape (name set, everything else
// null) rather than from Create's state on purpose: a refresh of an already
// populated state carries the value through whether or not Read assigns it,
// so that version of the assertion holds with Read's line deleted.
func TestApp_NumericIDReachesStateOnCreateAndImport(t *testing.T) {
	t.Parallel()
	f := newAppsFake(t)
	r := f.resource(t)
	s := appSchema(t)

	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: appPlan(t, s, appResourceModel{
			Name:              types.StringValue("numeric-app"),
			Org:               types.StringUnknown(),
			Network:           types.StringUnknown(),
			ID:                types.StringUnknown(),
			InternalNumericID: types.Int64Unknown(),
		}),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create errored: %v", resp.Diagnostics)
	}
	var created appResourceModel
	if diags := resp.State.Get(context.Background(), &created); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}
	if got := created.InternalNumericID; got.IsNull() || got.ValueInt64() != appNumericID {
		t.Errorf("created internal_numeric_id = %v, want %d", got, appNumericID)
	}

	imported := tfsdk.State{Schema: s}
	if diags := imported.Set(context.Background(), &appResourceModel{
		Name:              types.StringValue("numeric-app"),
		Org:               types.StringNull(),
		Network:           types.StringNull(),
		ID:                types.StringNull(),
		InternalNumericID: types.Int64Null(),
	}); diags.HasError() {
		t.Fatalf("build imported state: %v", diags)
	}
	readResp := &resource.ReadResponse{State: tfsdk.State{Schema: s}}
	r.Read(context.Background(), resource.ReadRequest{State: imported}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read errored: %v", readResp.Diagnostics)
	}
	var refreshed appResourceModel
	if diags := readResp.State.Get(context.Background(), &refreshed); diags.HasError() {
		t.Fatalf("read refreshed state: %v", diags)
	}
	if got := refreshed.InternalNumericID; got.IsNull() || got.ValueInt64() != appNumericID {
		t.Errorf("imported internal_numeric_id = %v, want %d", got, appNumericID)
	}
}

// TestApp_NumericIDIsNotConfigurable: the number is Fly's, assigned at
// create. An Optional attribute here would let a config name an app id that
// is not the app's, which is the one mistake a token-attenuating consumer
// cannot detect — the caveat would be well-formed and name someone else.
func TestApp_NumericIDIsNotConfigurable(t *testing.T) {
	t.Parallel()
	attr, ok := appSchema(t).Attributes["internal_numeric_id"].(rschema.Int64Attribute)
	if !ok {
		t.Fatal("internal_numeric_id is not an Int64Attribute")
	}
	if attr.Optional || attr.Required {
		t.Error("internal_numeric_id is settable from config; it is Fly's own identifier")
	}
	if !attr.Computed {
		t.Error("internal_numeric_id is not Computed; nothing would populate it")
	}
}
