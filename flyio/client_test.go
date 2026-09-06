package flyio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v5"

	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

func newTestClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	opts = append([]Option{WithToken("test-token")}, opts...)
	c, err := New("test-org", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestCreateApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth = %q, want Bearer test-token", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "ampbase-org-abc" {
			t.Errorf("name = %q", body["name"])
		}
		if _, ok := body["network"]; ok {
			t.Errorf("network present in body, want omitted: %q", body["network"])
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.CreateApp(t.Context(), CreateAppInput{Name: "ampbase-org-abc"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
}

// TestCreateAppNetwork locks in that the per-app `network` attribute
// is forwarded to AppsCreate when set.
func TestCreateAppNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "ampbase-vault" {
			t.Errorf("name = %q, want ampbase-vault", body["name"])
		}
		if body["network"] != "vault-net" {
			t.Errorf("network = %q, want vault-net", body["network"])
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.CreateApp(t.Context(), CreateAppInput{Name: "ampbase-vault", Network: "vault-net"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
}

func TestCreateAppIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.CreateApp(t.Context(), CreateAppInput{Name: "ampbase-org-abc"}); err != nil {
		t.Fatalf("CreateApp (idempotent): %v", err)
	}
}

func TestDeleteApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/ampbase-org-abc" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteApp(t.Context(), "ampbase-org-abc"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}

func TestDeleteAppIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteApp(t.Context(), "ampbase-org-gone"); err != nil {
		t.Fatalf("DeleteApp (404 idempotent): %v", err)
	}
}

func TestCreateMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/apps/ampbase-org-abc/machines" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			SkipLaunch *bool `json:"skip_launch"`
			Config     *struct {
				Image *string `json:"image"`
			} `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.SkipLaunch == nil || !*body.SkipLaunch {
			t.Error("skip_launch not set in body")
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Machine{ID: "mach-123"})
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	skipLaunch := true
	image := "registry.fly.io/ampbase-console:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-org-abc", CreateMachineInput{
		SkipLaunch: &skipLaunch,
		Config: &MachineConfig{
			Image: &image,
			Env:   map[string]string{"TIGRIS_BUCKET": "ampbase-org-abc"},
		},
	})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m.ID != "mach-123" {
		t.Errorf("machine id = %q, want %q", m.ID, "mach-123")
	}
}

// TestCreateMachineDecodesImageDigest covers the field a caller that names a
// mutable tag has no other way to learn: what Fly resolved that tag to. The
// body here is the API's own shape rather than a re-encoded Machine, so a
// wrong or missing json tag fails instead of round-tripping.
func TestCreateMachineDecodesImageDigest(t *testing.T) {
	t.Parallel()

	const digest = "sha256:9f2c1e2e7a1d4b6c8e0f3a5d7b9c1e3f5a7d9b1c3e5f7a9d1b3c5e7f9a1b3c5e"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mach-123","instance_id":"inst-1","image_ref":` +
			`{"registry":"registry.fly.io","repository":"ampbase-intel-signal",` +
			`"tag":"latest","digest":"` + digest + `"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "registry.fly.io/ampbase-intel-signal:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-intel-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	switch {
	case m.ImageRef.Digest != digest:
		t.Errorf("digest = %q, want %q", m.ImageRef.Digest, digest)
	case m.ImageRef.Repository != "ampbase-intel-signal":
		t.Errorf("repository = %q, want the pushed repo", m.ImageRef.Repository)
	case m.ImageRef.Tag != "latest":
		t.Errorf("tag = %q, want the tag the caller named", m.ImageRef.Tag)
	}
}

// TestCreateMachineRetriesOn5xx pins the retry contract for CreateMachine
// added alongside AllocateFlycast's: a transient Fly Machines API 5xx is
// retried, and a later 2xx wins. Same empirical motivation — the cutover
// observed a ~60% 500 rate on adjacent provisioning POSTs against the
// same hosts; assume MachinesCreate sees the same transience.
func TestCreateMachineRetriesOn5xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>500</html>"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Machine{ID: "mach-retry"})
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-org-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m == nil || m.ID != "mach-retry" {
		t.Fatalf("machine = %+v, want id mach-retry", m)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two 500s + one 201)", calls)
	}
}

// TestCreateMachineRetriesOnManifestUnknown pins the specific 400 carve-
// out for Docker registry MANIFEST_UNKNOWN: a freshly-pushed image isn't
// immediately visible at the machines-API pull endpoint and the first
// machine create after a push can race the registry. A later retry sees
// the manifest and succeeds. Without this, the operator has to manually
// re-run apply after a fresh image push.
func TestCreateMachineRetriesOnManifestUnknown(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"failed to get manifest registry.fly.io/ampbase-foo:abc123: request failed: not found [http 404]: {\"errors\":[{\"code\":\"MANIFEST_UNKNOWN\",\"message\":\"manifest unknown\",\"detail\":\"unknown tag=abc123\"}]}\n"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Machine{ID: "mach-after-manifest-settle"})
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-org-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m == nil || m.ID != "mach-after-manifest-settle" {
		t.Fatalf("machine = %+v, want id mach-after-manifest-settle", m)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two MANIFEST_UNKNOWNs + one 200)", calls)
	}
}

// TestCreateMachineNoRetryOn4xx pins that 4xx is terminal — request shape
// is wrong (or hits the "duplicate named machine" 422 success path), so
// retrying just delays the failure. Specifically guards against a stray
// 4xx being treated as 5xx and amplifying load on bad requests.
func TestCreateMachineNoRetryOn4xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid config"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	_, err := c.CreateMachine(t.Context(), "ampbase-org-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err == nil {
		t.Fatal("CreateMachine succeeded, want error")
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want 1 (4xx must not retry)", calls)
	}
}

// TestUpdateMachineRetriesOnManifestUnknown pins the same registry
// push→pull consistency tail on the update path that CreateMachine
// already absorbs. Observed twice during the RFC-011 Addendum staging
// cutover: a freshly-pushed image label from the fly-image module
// raced UpdateMachine's manifest validation and erred 400/MANIFEST_UNKNOWN
// — but the same image is visible to a retry seconds later. Without the
// retry the operator has to manually re-run apply after every rebuild.
func TestUpdateMachineRetriesOnManifestUnknown(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"failed to resolve image for container \"app\": failed to get manifest registry.fly.io/ampbase-foo:abc123: request failed: not found [http 404]: {\"errors\":[{\"code\":\"MANIFEST_UNKNOWN\",\"message\":\"manifest unknown\",\"detail\":\"unknown tag=abc123\"}]}\n"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(machines.Machine{Id: new("mach-after-manifest-settle"), InstanceId: new("inst")})
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	info, err := c.UpdateMachine(t.Context(), "ampbase-org-abc", "mach-id", machines.UpdateMachineRequest{
		Config: &MachineConfig{Image: &image},
	})
	if err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	if info == nil || info.ID != "mach-after-manifest-settle" {
		t.Fatalf("info = %+v, want id mach-after-manifest-settle", info)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two MANIFEST_UNKNOWNs + one 200)", calls)
	}
}

// TestUpdateMachineNoRetryOn4xx pins that a non-MANIFEST_UNKNOWN 4xx
// stays terminal — request-shape errors like bad config schemas
// shouldn't loop and amplify load on Fly. Mirrors
// TestCreateMachineNoRetryOn4xx.
func TestUpdateMachineNoRetryOn4xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid config"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	_, err := c.UpdateMachine(t.Context(), "ampbase-org-abc", "mach-id", machines.UpdateMachineRequest{
		Config: &MachineConfig{Image: &image},
	})
	if err == nil {
		t.Fatal("UpdateMachine succeeded, want error")
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want 1 (non-manifest 4xx must not retry)", calls)
	}
}

func TestAllocateFlycast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-org-abc/ip_assignments" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["type"] != "private_v6" {
			t.Errorf("type = %q, want private_v6", body["type"])
		}
		if _, ok := body["network"]; ok {
			t.Errorf("network present in body, want omitted: %q", body["network"])
		}
		if _, ok := body["org_slug"]; ok {
			t.Errorf("org_slug present in body, want omitted: %q", body["org_slug"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ip":"fdaa:0:1::1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ip, err := c.AllocateFlycast(t.Context(), "ampbase-org-abc", "", "")
	if err != nil {
		t.Fatalf("AllocateFlycast: %v", err)
	}
	if ip != "fdaa:0:1::1" {
		t.Errorf("ip = %q, want fdaa:0:1::1", ip)
	}
}

// TestAllocateFlycastCrossOrg locks in the body shape used by
// RFC-014 Addendum cross-org allocations: network targets a per-app
// private network (e.g. "vault-net" or "org-{org_id}"), and org_slug
// names the peer Fly org granted reach to the allocated IP.
func TestAllocateFlycastCrossOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-vault/ip_assignments" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != "private_v6" {
			t.Errorf("type = %q, want private_v6", body["type"])
		}
		if body["network"] != "vault-net" {
			t.Errorf("network = %q, want vault-net", body["network"])
		}
		if body["org_slug"] != "ampbase-customers-staging" {
			t.Errorf("org_slug = %q, want ampbase-customers-staging", body["org_slug"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ip":"fdaa:0:2::1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ip, err := c.AllocateFlycast(t.Context(), "ampbase-vault", "vault-net", "ampbase-customers-staging")
	if err != nil {
		t.Fatalf("AllocateFlycast: %v", err)
	}
	if ip != "fdaa:0:2::1" {
		t.Errorf("ip = %q, want fdaa:0:2::1", ip)
	}
}

// TestAllocateFlycastNetworkOnly covers the same-org but per-network
// allocation shape: targeting "vault-net" without granting any peer
// org reach. This is how the Vault and Tokenizer Flycasts are
// allocated today within the control-plane Fly org.
func TestAllocateFlycastNetworkOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-vault/ip_assignments" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != "private_v6" {
			t.Errorf("type = %q, want private_v6", body["type"])
		}
		if body["network"] != "vault-net" {
			t.Errorf("network = %q, want vault-net", body["network"])
		}
		if _, ok := body["org_slug"]; ok {
			t.Errorf("org_slug present in body, want omitted: %q", body["org_slug"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ip":"fdaa:0:3::1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ip, err := c.AllocateFlycast(t.Context(), "ampbase-vault", "vault-net", "")
	if err != nil {
		t.Fatalf("AllocateFlycast: %v", err)
	}
	if ip != "fdaa:0:3::1" {
		t.Errorf("ip = %q, want fdaa:0:3::1", ip)
	}
}

// shortenFlyAPIBackoff swaps the package-level backoff factory for a
// millisecond-scale variant during a test, restoring the original on
// cleanup. Keeps the retry semantics intact (same number of attempts
// via flyAPIMaxTries) while making retry-exhausted tests finish in
// milliseconds.
func shortenFlyAPIBackoff(t *testing.T) {
	t.Helper()
	orig := newFlyAPIBackoff
	newFlyAPIBackoff = func() backoff.BackOff {
		b := backoff.NewExponentialBackOff()
		b.InitialInterval = time.Millisecond
		b.MaxInterval = time.Millisecond
		b.RandomizationFactor = 0
		return b
	}
	t.Cleanup(func() { newFlyAPIBackoff = orig })
}

// TestAllocateFlycastRetriesOn5xx pins the retry contract: a transient
// Fly Machines API 5xx on the IP-allocation POST is retried, and a
// later 2xx wins. Empirical motivation: during the RFC-014 Addendum
// staging cutover, ~60% of cross-org grant POSTs hit a Rails-style 500
// with no useful body, but the next attempt seconds later succeeded
// with the same request shape — pure transient with no shape fix.
func TestAllocateFlycastRetriesOn5xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>500</html>"))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ip":"fdaa:0:4::1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ip, err := c.AllocateFlycast(t.Context(), "ampbase-tokenizer-staging", "org-01jtest", "ampbase-customers-staging")
	if err != nil {
		t.Fatalf("AllocateFlycast: %v", err)
	}
	if ip != "fdaa:0:4::1" {
		t.Errorf("ip = %q, want fdaa:0:4::1", ip)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two 500s + one 201)", calls)
	}
}

// TestAllocateFlycastRetriesExhausted covers the failure path: every
// retry attempt 5xxs. The final error wraps the last ResponseError so
// callers can extract the status code + Fly request ID for support.
func TestAllocateFlycastRetriesExhausted(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("fly-request-id", "01TESTREQ123-iad")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>500</html>"))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	_, err := c.AllocateFlycast(t.Context(), "ampbase-tokenizer-staging", "org-01jtest", "ampbase-customers-staging")
	if err == nil {
		t.Fatal("AllocateFlycast succeeded, want error")
	}
	if calls != flyAPIMaxTries {
		t.Errorf("server received %d calls, want %d (one per backoff attempt)", calls, flyAPIMaxTries)
	}

	var re *ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("error type = %T, want unwrap to *ResponseError; err: %v", err, err)
	}
	if re.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", re.StatusCode)
	}
	if re.RequestID != "01TESTREQ123-iad" {
		t.Errorf("RequestID = %q, want 01TESTREQ123-iad — fly-request-id header must propagate for support escalation", re.RequestID)
	}
}

// TestAllocateFlycastNoRetryOn4xx pins that 4xx is treated as terminal:
// the request shape is wrong (or the API rejects it for a reason that
// will not change), so retrying just delays the failure. Specifically
// covers the 404 the Machines API returns when the peer-side network
// in a cross-org grant doesn't exist yet.
func TestAllocateFlycastNoRetryOn4xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404</html>"))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	_, err := c.AllocateFlycast(t.Context(), "ampbase-tokenizer-staging", "org-01jtest", "ampbase-customers-staging")
	if err == nil {
		t.Fatal("AllocateFlycast succeeded, want error")
	}
	if calls != 1 {
		t.Errorf("server received %d calls, want 1 (4xx must not retry)", calls)
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(err) = false, want true; err: %v", err)
	}
}

func TestSetSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-org-abc/secrets" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Values["AWS_ACCESS_KEY_ID"] != "AKID" {
			t.Errorf("AWS_ACCESS_KEY_ID = %q, want %q", body.Values["AWS_ACCESS_KEY_ID"], "AKID")
		}
		if body.Values["AWS_SECRET_ACCESS_KEY"] != "SECRET" {
			t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want %q", body.Values["AWS_SECRET_ACCESS_KEY"], "SECRET")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"secrets":[{"name":"AWS_ACCESS_KEY_ID","digest":"sha256:abc"},{"name":"AWS_SECRET_ACCESS_KEY","digest":"sha256:def"}],"version":42}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	result, err := c.SetSecrets(t.Context(), "ampbase-org-abc", map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKID",
		"AWS_SECRET_ACCESS_KEY": "SECRET",
	})
	if err != nil {
		t.Fatalf("SetSecrets: %v", err)
	}
	if len(result.Secrets) != 2 {
		t.Fatalf("got %d secrets, want 2", len(result.Secrets))
	}
	if result.Version != 42 {
		t.Errorf("Version = %d, want 42", result.Version)
	}
}

func TestDeleteSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/ampbase-org-abc/secrets/AWS_ACCESS_KEY_ID" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteSecret(t.Context(), "ampbase-org-abc", "AWS_ACCESS_KEY_ID"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

func TestDeleteSecretIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteSecret(t.Context(), "app", "GONE"); err != nil {
		t.Fatalf("DeleteSecret (404 idempotent): %v", err)
	}
}

func TestListMachines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/ampbase-org-abc/machines" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id":"mach-1","state":"started","host_status":"ok","region":"iad","image_ref":{"repository":"registry.fly.io/ampbase-console","tag":"release","digest":"sha256:abc123"}},
			{"id":"mach-2","state":"stopped","host_status":"unknown","region":"ord"}
		]`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ms, err := c.ListMachines(t.Context(), "ampbase-org-abc")
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d, want 2", len(ms))
	}
	if ms[0].ID != "mach-1" || ms[0].State != "started" || ms[0].HostStatus != "ok" {
		t.Errorf("ms[0] = %+v", ms[0])
	}
	if ms[0].ImageRef.Digest != "sha256:abc123" || ms[0].ImageRef.Repository != "registry.fly.io/ampbase-console" {
		t.Errorf("ms[0].ImageRef = %+v", ms[0].ImageRef)
	}
	if ms[1].ID != "mach-2" || ms[1].State != "stopped" {
		t.Errorf("ms[1] = %+v", ms[1])
	}
}

func TestListMachinesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ms, err := c.ListMachines(t.Context(), "ampbase-org-gone")
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 0 {
		t.Fatalf("len = %d, want 0", len(ms))
	}
}

func TestGetMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/ampbase-org-abc/machines/mach-1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mach-1","state":"stopped","host_status":"ok","region":"iad","image_ref":{"repository":"reg","tag":"v1","digest":"sha256:def"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	m, err := c.GetMachine(t.Context(), "ampbase-org-abc", "mach-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if m.ID != "mach-1" || m.State != "stopped" || m.Region != "iad" {
		t.Errorf("machine = %+v", m)
	}
	if m.ImageRef.Digest != "sha256:def" {
		t.Errorf("digest = %q", m.ImageRef.Digest)
	}
}

// TestGetMachine_PopulatesMounts pins that the machine's volume mounts
// decode into MachineInfo.Mounts. The provider's Read relies on this to
// detect a Fly host migration that forks the mounted volume to a new ID.
func TestGetMachine_PopulatesMounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mach-1","state":"started","region":"iad","config":{"mounts":[{"volume":"vol_new","path":"/var/lib/clickhouse","name":"clickhouse_data_1"}]}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	m, err := c.GetMachine(t.Context(), "ampbase-org-abc", "mach-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if len(m.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want 1", m.Mounts)
	}
	if got := m.Mounts[0]; got.Volume != "vol_new" || got.Path != "/var/lib/clickhouse" || got.Name != "clickhouse_data_1" {
		t.Errorf("mount = %+v", got)
	}
}

func TestDestroyMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/ampbase-org-abc/machines/mach-1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("force") != "true" {
			t.Error("force param not set")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DestroyMachine(t.Context(), "ampbase-org-abc", "mach-1", true); err != nil {
		t.Fatalf("DestroyMachine: %v", err)
	}
}

func TestDestroyMachineIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DestroyMachine(t.Context(), "ampbase-org-abc", "mach-gone", false); err != nil {
		t.Fatalf("DestroyMachine (404 idempotent): %v", err)
	}
}

func TestDestroyMachineNoForce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") != "" {
			t.Error("force param should not be set")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DestroyMachine(t.Context(), "app", "mach-1", false); err != nil {
		t.Fatalf("DestroyMachine: %v", err)
	}
}

func TestWaitForState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/ampbase-org-abc/machines/mach-1/wait" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("state") != "started" {
			t.Errorf("state = %q, want started", r.URL.Query().Get("state"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForState(t.Context(), "ampbase-org-abc", "mach-1", "started", ""); err != nil {
		t.Fatalf("WaitForState: %v", err)
	}
}

// TestWaitForStateRetriesOn5xx pins that a transient Fly Machines API
// 5xx on the wait endpoint is retried rather than failing the deploy.
func TestWaitForStateRetriesOn5xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForState(t.Context(), "app", "mach-1", "started", ""); err != nil {
		t.Fatalf("WaitForState: %v", err)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two 502s + one 200)", calls)
	}
}

// TestWaitForStateNoRetryOn408 pins that 408 (server-side wait timeout)
// is NOT retried — it's a real "machine not yet there" signal that
// callers loop on via ctx, not a transient API failure.
func TestWaitForStateTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte("wait timeout"))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	err := c.WaitForState(t.Context(), "app", "mach-1", "started", "")
	if err == nil {
		t.Fatal("expected error on 408")
	}
	var re *ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ResponseError, got %T", err)
	}
	if re.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status code = %d, want %d", re.StatusCode, http.StatusRequestTimeout)
	}
}

// TestWaitForStateRetriesOnTransportError pins that a transport-level failure
// on the wait long-poll (e.g. the HTTP client's per-request timeout firing
// before a slow-to-start machine reaches "started") is re-polled while ctx
// still has budget, rather than giving up on the first attempt. This is the
// bug that orphaned machines when several were created at once.
func TestWaitForStateRetriesOnTransportError(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			// Drop the connection so the client sees a transport error,
			// standing in for the long-poll client timeout.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter is not a Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForState(t.Context(), "app", "mach-1", "started", ""); err != nil {
		t.Fatalf("WaitForState should re-poll transport errors within ctx: %v", err)
	}
	if calls != 3 {
		t.Errorf("server received %d calls, want 3 (two transport errors + one 200)", calls)
	}
}

// TestWaitForStateStopsWhenContextDone pins that once the caller's context is
// cancelled, WaitForState stops re-polling instead of looping.
func TestWaitForStateStopsWhenContextDone(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForState(ctx, "app", "mach-1", "started", ""); err == nil {
		t.Fatal("expected error once ctx is done")
	}
	// WaitForState is bounded by ctx, not flyAPIMaxTries: with 1ms backoff it
	// must keep re-polling past the generic 6-try cap until the deadline fires.
	// (If maxTries still bounded it, this would stop at ~6 calls regardless of
	// the 200ms budget.)
	if n := calls.Load(); n <= flyAPIMaxTries {
		t.Errorf("made %d calls, want > flyAPIMaxTries (%d) — proves ctx, not maxTries, stopped the loop", n, flyAPIMaxTries)
	}
}

// TestWaitForChecksRetriesOnTransportError pins that WaitForChecks re-polls a
// transport-level failure (dropped connection / client timeout on GetMachine)
// while ctx has budget rather than aborting and destroying a machine that was
// about to pass — the same gap this PR closed in WaitForState, in the same
// concurrent-create window.
func TestWaitForChecksRetriesOnTransportError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter is not a Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"mach-1","state":"started",
			"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
			"checks":[{"name":"service-8081","status":"passing","output":"200 OK"}]
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForChecks(t.Context(), "app", "mach-1"); err != nil {
		t.Fatalf("WaitForChecks should re-poll transport errors within ctx: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server received %d calls, want 3 (two transport errors + one passing)", n)
	}
}

func TestResponseError(t *testing.T) {
	// CreateMachine now retries on 5xx; use the short backoff so this test
	// finishes in milliseconds rather than minutes while still exercising
	// that the final *ResponseError surfaces with status/body intact.
	shortenFlyAPIBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("something broke"))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	_, err := c.CreateMachine(t.Context(), "app", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var re *ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ResponseError, got %T", err)
	}
	if re.StatusCode != http.StatusInternalServerError {
		t.Errorf("status code = %d, want %d", re.StatusCode, http.StatusInternalServerError)
	}
	if re.Body != "something broke" {
		t.Errorf("body = %q", re.Body)
	}
}

func TestIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	_, err := c.GetMachine(t.Context(), "gone-app", "mach-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsNotFound(err) {
		t.Error("expected IsNotFound to return true")
	}
}

func TestIsNotFoundFalseFor500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	_, err := c.GetMachine(t.Context(), "app", "mach-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if IsNotFound(err) {
		t.Error("expected IsNotFound to return false for 500")
	}
}

// TestListIPAssignments locks in that the response is a wrapper object
// {"ips": [...]}, not a bare array. The Fly Machines API was returning a
// wrapper while the client decoded into []IPAssignment, breaking every
// terraform refresh on existing org state.
func TestListIPAssignments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/ampbase-org-abc/ip_assignments" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ips":[{"ip":"fdaa:0:1::1","type":"private_v6"},{"ip":"fdaa:0:1::2","type":"private_v6"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ips, err := c.ListIPAssignments(t.Context(), "ampbase-org-abc")
	if err != nil {
		t.Fatalf("ListIPAssignments: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("len = %d, want 2", len(ips))
	}
	if ips[0].IP != "fdaa:0:1::1" || ips[1].IP != "fdaa:0:1::2" {
		t.Errorf("ips = %+v", ips)
	}
}

func TestListIPAssignmentsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ips, err := c.ListIPAssignments(t.Context(), "ampbase-org-abc")
	if err != nil {
		t.Fatalf("ListIPAssignments: %v", err)
	}
	if len(ips) != 0 {
		t.Errorf("len = %d, want 0", len(ips))
	}
}

// TestListSecrets locks in the wrapper-object response shape, mirroring
// TestListIPAssignments. Same regression class.
func TestListSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/ampbase-org-abc/secrets" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":[{"name":"AWS_REGION","digest":"sha256:aaa"},{"name":"NATS_HUB_TOKEN","digest":"sha256:bbb"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	secrets, err := c.ListSecrets(t.Context(), "ampbase-org-abc")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("len = %d, want 2", len(secrets))
	}
	if secrets[0].Name != "AWS_REGION" || secrets[0].Digest != "sha256:aaa" {
		t.Errorf("secrets[0] = %+v", secrets[0])
	}
	if secrets[1].Name != "NATS_HUB_TOKEN" || secrets[1].Digest != "sha256:bbb" {
		t.Errorf("secrets[1] = %+v", secrets[1])
	}
}

func TestListSecretsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	secrets, err := c.ListSecrets(t.Context(), "ampbase-org-abc")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(secrets) != 0 {
		t.Errorf("len = %d, want 0", len(secrets))
	}
}

// TestWaitForRestFollowsTheNewInstance pins the two things the settle wait
// has to get right: a rest state reported by the pre-update instance does
// not satisfy it, and a transition ("replacing") on the new instance is
// waited through until the machine rests — in whichever state that is.
func TestWaitForRestFollowsTheNewInstance(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			// The pre-update instance, still reporting its old rest state.
			_, _ = w.Write([]byte(`{"id":"mach-1","instance_id":"inst-old","state":"stopped"}`))
		case 2:
			_, _ = w.Write([]byte(`{"id":"mach-1","instance_id":"inst-new","state":"replacing"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"mach-1","instance_id":"inst-new","state":"suspended"}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	rest, err := c.WaitForRest(ctx, "app", "mach-1", "inst-new")
	if err != nil {
		t.Fatalf("WaitForRest: %v", err)
	}
	if rest != "suspended" {
		t.Errorf("rest = %q, want suspended", rest)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 API calls (old instance, replacing, rest), got %d", got)
	}
}

func TestWaitForChecksNoChecksConfigured(t *testing.T) {
	// Machine has no checks configured — WaitForChecks returns immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mach-1","state":"started","config":{"image":"img:latest","services":[{"internal_port":8081}]}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForChecks(t.Context(), "app", "mach-1"); err != nil {
		t.Fatalf("WaitForChecks: %v", err)
	}
}

func TestWaitForChecksAlreadyPassing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"mach-1","state":"started",
			"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
			"checks":[{"name":"service-8081","status":"passing","output":"200 OK"}]
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.WaitForChecks(t.Context(), "app", "mach-1"); err != nil {
		t.Fatalf("WaitForChecks: %v", err)
	}
}

func TestWaitForChecksEventuallyPasses(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			// First two calls: check is critical (service still starting).
			_, _ = w.Write([]byte(`{
				"id":"mach-1","state":"started",
				"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
				"checks":[{"name":"service-8081","status":"critical","output":"connection refused"}]
			}`))
		} else {
			// Third call: check passes.
			_, _ = w.Write([]byte(`{
				"id":"mach-1","state":"started",
				"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
				"checks":[{"name":"service-8081","status":"passing","output":"200 OK"}]
			}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := c.WaitForChecks(ctx, "app", "mach-1"); err != nil {
		t.Fatalf("WaitForChecks: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 API calls, got %d", got)
	}
}

// TestWaitForChecksRetriesOn5xx pins that a transient 5xx from the
// underlying GetMachine call during the wait loop is treated as
// retryable rather than terminating the wait — previously a single 500
// in the poll loop killed the deploy. The wait continues, and once the
// API recovers and checks pass, the wait returns nil.
func TestWaitForChecksRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		switch n {
		case 1:
			// First poll: machine present, checks still critical.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"mach-1","state":"started",
				"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
				"checks":[{"name":"service-8081","status":"critical","output":"connection refused"}]
			}`))
		case 2:
			// Second poll: transient Fly API 5xx — must not kill the wait.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>500</html>"))
		default:
			// Subsequent polls: checks pass.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"mach-1","state":"started",
				"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
				"checks":[{"name":"service-8081","status":"passing","output":"200 OK"}]
			}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := c.WaitForChecks(ctx, "app", "mach-1"); err != nil {
		t.Fatalf("WaitForChecks: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 API calls (critical, 500, passing), got %d", got)
	}
}

func TestWaitForChecksTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"mach-1","state":"started",
			"config":{"image":"img:latest","services":[{"internal_port":8081,"checks":[{"type":"http","port":8081,"path":"/health"}]}]},
			"checks":[{"name":"service-8081","status":"critical","output":"connection refused"}]
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := c.WaitForChecks(ctx, "app", "mach-1")
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestCreateVolume locks in the request shape (POST /apps/{app}/volumes
// with name/region/size_gb) and response decoding. The RFC-021 cluster
// bootstrap pattern (RFC-021 §"Per-org Terraform configuration") relies
// on this for the ClickHouse Keeper and replica volumes.
func TestCreateVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-keeper-staging/volumes" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Name      *string `json:"name"`
			Region    *string `json:"region"`
			SizeGb    *int    `json:"size_gb"`
			Encrypted *bool   `json:"encrypted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Name == nil || *body.Name != "keeper_data" {
			t.Errorf("name = %v, want keeper_data", body.Name)
		}
		if body.Region == nil || *body.Region != "iad" {
			t.Errorf("region = %v, want iad", body.Region)
		}
		if body.SizeGb == nil || *body.SizeGb != 3 {
			t.Errorf("size_gb = %v, want 3", body.SizeGb)
		}
		if body.Encrypted == nil || !*body.Encrypted {
			t.Errorf("encrypted = %v, want true", body.Encrypted)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_abc123","name":"keeper_data","region":"iad","size_gb":3,"encrypted":true,"state":"created","zone":"qaz1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	enc := true
	v, err := c.CreateVolume(t.Context(), "ampbase-keeper-staging", CreateVolumeInput{
		Name:      "keeper_data",
		Region:    "iad",
		SizeGB:    3,
		Encrypted: &enc,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v.ID != "vol_abc123" {
		t.Errorf("id = %q, want vol_abc123", v.ID)
	}
	if v.SizeGB != 3 {
		t.Errorf("size_gb = %d, want 3", v.SizeGB)
	}
	if !v.Encrypted {
		t.Error("encrypted = false, want true")
	}
}

// TestCreateVolumeRetriesOn5xx pins the retry contract for CreateVolume
// (same shape as CreateMachine and AllocateFlycast — a transient 5xx is
// retried, and a later 2xx wins).
func TestCreateVolumeRetriesOn5xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>500</html>"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_retry","name":"data","region":"iad","size_gb":1}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	v, err := c.CreateVolume(t.Context(), "app", CreateVolumeInput{Name: "data", Region: "iad", SizeGB: 1})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v.ID != "vol_retry" {
		t.Errorf("id = %q, want vol_retry", v.ID)
	}
	if calls != 2 {
		t.Errorf("server received %d calls, want 2 (one 500 + one 200)", calls)
	}
}

// TestGetVolumeNotFound exercises the "deleted out-of-band" code path —
// 404 collapses to nil, nil so the provider's Read can RemoveResource.
func TestGetVolumeNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	v, err := c.GetVolume(t.Context(), "app", "vol_gone")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if v != nil {
		t.Errorf("volume = %+v, want nil", v)
	}
}

// TestExtendVolume exercises the PUT /volumes/{id}/extend path used when
// size_gb increases in TF. The endpoint returns a wrapper object with the
// updated volume inside; we expect the wrapper's `volume` field to be
// surfaced as the returned VolumeInfo.
func TestExtendVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/apps/app/volumes/vol_abc/extend" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			SizeGb *int `json:"size_gb"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.SizeGb == nil || *body.SizeGb != 10 {
			t.Errorf("size_gb = %v, want 10", body.SizeGb)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"needs_restart":false,"volume":{"id":"vol_abc","size_gb":10,"state":"ready"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	v, err := c.ExtendVolume(t.Context(), "app", "vol_abc", 10)
	if err != nil {
		t.Fatalf("ExtendVolume: %v", err)
	}
	if v.SizeGB != 10 {
		t.Errorf("size_gb = %d, want 10", v.SizeGB)
	}
}

// TestAllocateIPPublicTypes pins that the generic AllocateIP wrapper
// supports the non-Flycast IP types needed by environment provisioning
// (RFC-022) AND translates the provider's friendly enum (`public_v4` /
// `public_v6`) to the bare API value (`v4` / `v6`) the Fly Machines API
// actually accepts. Without translation a `public_v6` POST returns
// 400 "invalid ip address type" — the fly_ip resource silently never
// allocates and fly_cert_validation hangs at 10m on a missing AAAA.
// Public types ignore network/peerOrgSlug — verify those stay omitted.
func TestAllocateIPPublicTypes(t *testing.T) {
	cases := []struct {
		name     string
		ipType   string
		wantType string
	}{
		{"shared v4", "shared_v4", "shared_v4"},
		{"dedicated v4", "public_v4", "v4"},
		{"public v6", "public_v6", "v6"},
		{"private v6", "private_v6", "private_v6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Type    *string `json:"type"`
					Network *string `json:"network"`
					OrgSlug *string `json:"org_slug"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Type == nil || *body.Type != tc.wantType {
					t.Errorf("type = %v, want %s", body.Type, tc.wantType)
				}
				if body.Network != nil {
					t.Errorf("network sent for public type: %v", body.Network)
				}
				if body.OrgSlug != nil {
					t.Errorf("org_slug sent for public type: %v", body.OrgSlug)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"ip":"203.0.113.1"}`))
			}))
			defer srv.Close()

			c := newTestClient(t, WithBaseURL(srv.URL))
			ip, err := c.AllocateIP(t.Context(), "ampbase-amp-staging", tc.ipType, "", "")
			if err != nil {
				t.Fatalf("AllocateIP: %v", err)
			}
			if ip != "203.0.113.1" {
				t.Errorf("ip = %q", ip)
			}
		})
	}
}

// TestCreateAcmeCertificate pins the wire shape (POST .../certificates/acme
// with hostname) and dns_requirements decoding. The cluster bootstrap and
// env modules use the returned acme_challenge / cname targets to drive
// downstream Cloudflare DNS records without a second tofu apply.
func TestCreateAcmeCertificate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/ampbase-amp-staging/certificates/acme" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Hostname *string `json:"hostname"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Hostname == nil || *body.Hostname != "staging.ampbase.io" {
			t.Errorf("hostname = %v", body.Hostname)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"hostname":"ampbase.io",
			"configured":false,
			"status":"awaiting_configuration",
			"acme_requested":true,
			"dns_provider":"cloudflare",
			"dns_requirements":{
				"cname":"",
				"a":["66.241.124.1","66.241.124.2"],
				"aaaa":["2a09:8280:1::1","2a09:8280:1::2"],
				"acme_challenge":{"name":"_acme-challenge.ampbase.io","target":"abc.flydns.net"},
				"ownership":{"name":"_fly-ownership.ampbase.io","app_value":"app-token-xyz"}
			},
			"validation":{"dns_configured":false,"http_configured":false,"alpn_configured":false}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	info, err := c.CreateAcmeCertificate(t.Context(), "ampbase-amp-staging", "staging.ampbase.io")
	if err != nil {
		t.Fatalf("CreateAcmeCertificate: %v", err)
	}
	// Apex case: cname is empty, a/aaaa lists populated. Confirms the
	// apex-hostname code path the docs cross-check surfaced as a gap.
	if info.CNAME != "" {
		t.Errorf("cname should be empty for apex, got %q", info.CNAME)
	}
	if len(info.ARecords) != 2 || info.ARecords[0] != "66.241.124.1" {
		t.Errorf("a_records = %v", info.ARecords)
	}
	if len(info.AAAARecords) != 2 || info.AAAARecords[1] != "2a09:8280:1::2" {
		t.Errorf("aaaa_records = %v", info.AAAARecords)
	}
	if info.AcmeChallenge.Target != "abc.flydns.net" {
		t.Errorf("acme target = %q", info.AcmeChallenge.Target)
	}
	if info.Ownership.AppValue != "app-token-xyz" {
		t.Errorf("ownership = %q", info.Ownership.AppValue)
	}
	if info.Configured {
		t.Error("configured should be false on initial create")
	}
}

// TestCreateAcmeCertificateRetriesOn5xx pins the retry contract for the
// new cert endpoint, matching CreateMachine / AllocateFlycast. A
// transient 5xx is retried, a later 2xx wins.
func TestCreateAcmeCertificateRetriesOn5xx(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<html>500</html>`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"hostname":"staging.ampbase.io","configured":false,"status":"awaiting_configuration"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	info, err := c.CreateAcmeCertificate(t.Context(), "app", "staging.ampbase.io")
	if err != nil {
		t.Fatalf("CreateAcmeCertificate: %v", err)
	}
	if info.Hostname != "staging.ampbase.io" {
		t.Errorf("hostname = %q", info.Hostname)
	}
	if calls != 2 {
		t.Errorf("server received %d calls, want 2 (one 500 + one 201)", calls)
	}
}

// TestCreateAcmeCertificateIdempotent pins the "already exists" path:
// a 422 from the API maps to GetCertificate + return existing state,
// matching CreateApp's idempotency. Lets `tofu apply` re-runs after a
// partial failure succeed instead of erroring out — without this, the
// operator would have to run `tofu import` before proceeding.
func TestCreateAcmeCertificateIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"cert already exists"}`))
			return
		}
		// GET fallback returns the existing cert state.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hostname":"staging.ampbase.io","configured":true,"status":"ready","dns_requirements":{"cname":"app.fly.dev"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	info, err := c.CreateAcmeCertificate(t.Context(), "app", "staging.ampbase.io")
	if err != nil {
		t.Fatalf("CreateAcmeCertificate (idempotent): %v", err)
	}
	if !info.Configured {
		t.Error("existing cert state should have configured=true from the lookup")
	}
	if info.CNAME != "app.fly.dev" {
		t.Errorf("cname = %q", info.CNAME)
	}
}

// TestGetCertificateNotFound exercises the "deleted out-of-band" path —
// the provider's Read returns RemoveResource on this signal.
func TestGetCertificateNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	info, err := c.GetCertificate(t.Context(), "app", "gone.example.com")
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if info != nil {
		t.Errorf("expected nil info, got %+v", info)
	}
}

// TestWaitForCertificateConfigured pins the polling contract used by the
// fly_cert_validation resource. First poll sees awaiting_configuration,
// second poll sees configured=true. Also exercises the initial
// CheckCertificate bump that nudges Fly's validator on entry.
func TestWaitForCertificateConfigured(t *testing.T) {
	var getCalls, checkCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			// AppCertificatesCheck is a POST against /check — the bump
			// at the start of the wait loop. Reports not-yet-configured;
			// the subsequent GET polls converge.
			checkCalls++
			_, _ = w.Write([]byte(`{"hostname":"staging.ampbase.io","configured":false,"status":"awaiting_configuration"}`))
		case http.MethodGet:
			getCalls++
			if getCalls < 2 {
				_, _ = w.Write([]byte(`{"hostname":"staging.ampbase.io","configured":false,"status":"awaiting_configuration"}`))
				return
			}
			_, _ = w.Write([]byte(`{"hostname":"staging.ampbase.io","configured":true,"status":"ready","validation":{"dns_configured":true,"http_configured":true}}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	info, err := c.WaitForCertificateConfigured(ctx, "app", "staging.ampbase.io")
	if err != nil {
		t.Fatalf("WaitForCertificateConfigured: %v", err)
	}
	if !info.Configured {
		t.Errorf("info.Configured = false, want true")
	}
	if checkCalls != 1 {
		t.Errorf("expected 1 CheckCertificate bump, got %d", checkCalls)
	}
	if getCalls < 2 {
		t.Errorf("expected at least 2 GetCertificate polls, got %d", getCalls)
	}
}

// TestWaitForCertificateConfiguredCertGone pins that the wait
// permanently fails (instead of polling to deadline) when the cert
// disappears mid-wait — e.g. someone running `fly certs remove` while
// tofu apply is mid-wait. The TF resource will surface this as an
// error and not block the apply forever.
func TestWaitForCertificateConfiguredCertGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both the recheck POST and the polling GETs return 404.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.WaitForCertificateConfigured(ctx, "app", "gone.example.com")
	if err == nil {
		t.Fatal("expected error when cert is missing")
	}
	if !contains(err.Error(), "no longer exists") {
		t.Errorf("error %q should mention the cert is gone", err.Error())
	}
}

// TestDeleteCertificateIdempotent pins the standard 404-as-success
// pattern used across the client.
func TestDeleteCertificateIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteCertificate(t.Context(), "app", "gone.example.com"); err != nil {
		t.Fatalf("DeleteCertificate (404 idempotent): %v", err)
	}
}

// TestDeleteVolumeIdempotent pins that DELETE returns nil on 404 — same
// pattern as DeleteApp/DestroyMachine, lets tofu destroy succeed when the
// volume is already gone (e.g. mid-recovery or manual cleanup).
func TestDeleteVolumeIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	if err := c.DeleteVolume(t.Context(), "app", "vol_gone"); err != nil {
		t.Fatalf("DeleteVolume (404 idempotent): %v", err)
	}
}

// TestDeleteVolumeAttachedRewritesError pins the docs-flagged paper cut
// ("Volumes attached to Machines can't be destroyed"): when the API
// rejects a delete and the volume is still attached, we rewrite the
// error to name the blocking machine so a tofu destroy race surfaces
// a clear diagnostic instead of a generic 4xx.
func TestDeleteVolumeAttachedRewritesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"volume in use"}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"vol_abc","attached_machine_id":"mach_xyz","size_gb":3,"state":"created"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	err := c.DeleteVolume(t.Context(), "app", "vol_abc")
	if err == nil {
		t.Fatal("DeleteVolume should have failed with attached volume")
	}
	if msg := err.Error(); !contains(msg, "mach_xyz") {
		t.Errorf("error %q should name the blocking machine mach_xyz", msg)
	}
}

// TestCreateVolumeForwardsCompute pins the docs-flagged host-pinning fix:
// compute must reach the API body so Fly places the volume on a host
// that can later schedule the matching machine. Per the Fly docs, "If
// you create a Machine first and then create a volume, even in the
// same region, there's a good chance they'll end up on different hosts.
// In that case, the volume attachment will fail."
func TestCreateVolumeForwardsCompute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Compute *struct {
				CpuKind  *string `json:"cpu_kind"`
				Cpus     *int    `json:"cpus"`
				MemoryMb *int    `json:"memory_mb"`
			} `json:"compute"`
			RequireUniqueZone *bool `json:"require_unique_zone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Compute == nil || body.Compute.CpuKind == nil || *body.Compute.CpuKind != "shared" {
			t.Errorf("compute.cpu_kind = %v, want shared", body.Compute)
		}
		if body.Compute == nil || body.Compute.MemoryMb == nil || *body.Compute.MemoryMb != 1024 {
			t.Errorf("compute.memory_mb = %v, want 1024", body.Compute)
		}
		if body.RequireUniqueZone == nil || !*body.RequireUniqueZone {
			t.Errorf("require_unique_zone = %v, want true", body.RequireUniqueZone)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_hint","name":"keeper_data","region":"iad","size_gb":3}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	cpuKind := "shared"
	cpus := 2
	mem := 1024
	unique := true
	_, err := c.CreateVolume(t.Context(), "app", CreateVolumeInput{
		Name:              "keeper_data",
		Region:            "iad",
		SizeGB:            3,
		RequireUniqueZone: &unique,
		Compute: &machines.FlyMachineGuest{
			CpuKind:  &cpuKind,
			Cpus:     &cpus,
			MemoryMb: &mem,
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
}

// TestCreateVolumeForwardsUniqueZoneAppWide pins the app-wide unique-
// zone field — the alternative to shared-name HA placement. Lets the
// cluster module give each volume a descriptive per-machine name and
// still spread across distinct hardware zones.
func TestCreateVolumeForwardsUniqueZoneAppWide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UniqueZoneAppWide *bool `json:"unique_zone_app_wide"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.UniqueZoneAppWide == nil || !*body.UniqueZoneAppWide {
			t.Errorf("unique_zone_app_wide = %v, want true", body.UniqueZoneAppWide)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_appwide","name":"keeper_data_1","region":"iad","size_gb":3}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	appWide := true
	_, err := c.CreateVolume(t.Context(), "app", CreateVolumeInput{
		Name:              "keeper_data_1",
		Region:            "iad",
		SizeGB:            3,
		UniqueZoneAppWide: &appWide,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
}

// TestWaitForVolumeReady pins the "creating → created" wait that the
// volume resource's Create runs so the returned id is safe to consume
// in the same apply (cluster bootstrap: fly_machine.mount.volume =
// fly_volume.X.id). First poll sees "creating", second sees "created".
func TestWaitForVolumeReady(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		if calls < 2 {
			_, _ = w.Write([]byte(`{"id":"vol_abc","state":"creating","size_gb":3}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"vol_abc","state":"created","size_gb":3,"zone":"qaz1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.WaitForVolumeReady(ctx, "app", "vol_abc"); err != nil {
		t.Fatalf("WaitForVolumeReady: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 polls, got %d", calls)
	}
}

// TestWaitForVolumeReadyTransient5xx pins that a transient 5xx during
// the poll does NOT terminate the wait — Fly's Machines API has a
// documented ~60% 5xx rate during cutover periods and we don't want a
// volume-readiness wait to flake on the same noise the rest of the
// client retries past.
func TestWaitForVolumeReadyTransient5xx(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<html>500</html>`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_abc","state":"created"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.WaitForVolumeReady(ctx, "app", "vol_abc"); err != nil {
		t.Fatalf("WaitForVolumeReady should have recovered through transient 5xx: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 polls, got %d", calls)
	}
}

// TestWaitForVolumeReadyTerminalState pins that destroyed/destroying
// short-circuits the wait with a permanent error rather than polling
// to deadline — failing fast surfaces a bad volume ID to the operator
// instead of blocking the apply.
func TestWaitForVolumeReadyTerminalState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vol_abc","state":"destroyed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	err := c.WaitForVolumeReady(t.Context(), "app", "vol_abc")
	if err == nil {
		t.Fatal("expected error for terminal state")
	}
	if !contains(err.Error(), "destroyed") {
		t.Errorf("error %q should mention destroyed state", err.Error())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestCreateMachineSurfacesMachineLimit covers the 422 whose remedy is an
// email. Fly's per-org machine limit and a duplicate machine name arrive with
// the same status code, and a named create treats 422 as "already exists" —
// so without the body check a launcher at the org cap reads every refusal as
// a success and records a run it never started.
func TestCreateMachineSurfacesMachineLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Your organization has reached its machine limit. Please contact billing@fly.io"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	name, image := "intel-run", "img:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-intel-abc", CreateMachineInput{
		Name:   &name,
		Config: &MachineConfig{Image: &image},
	})
	switch {
	case m != nil:
		t.Errorf("machine = %+v, want nil when the org is at its machine limit", m)
	case !IsMachineLimit(err):
		t.Errorf("err = %v, want a machine-limit error", err)
	}
}

// TestCreateMachineNamedDuplicateStillSucceeds is the other half: the
// idempotent path the tofu provider depends on must keep returning (nil, nil)
// so the caller looks the existing machine up by name.
func TestCreateMachineNamedDuplicateStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"machine name already taken"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	name, image := "org-machine", "img:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-org-abc", CreateMachineInput{
		Name:   &name,
		Config: &MachineConfig{Image: &image},
	})
	if err != nil || m != nil {
		t.Fatalf("CreateMachine = (%+v, %v), want (nil, nil) for a duplicate name", m, err)
	}
}

// TestCreateMachineRetriesOn429 pins the classification flyctl uses
// (internal/flapsutil/retry.go): a rate-limit response is transient. Treating
// it as permanent fails the first call of a burst rather than the last.
func TestCreateMachineRetriesOn429(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Machine{ID: "mach-429"})
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	m, err := c.CreateMachine(t.Context(), "ampbase-intel-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m == nil || m.ID != "mach-429" {
		t.Fatalf("machine = %+v, want the machine created after the 429", m)
	}
}

// TestCreateMachineDoesNotRetryUnauthorized keeps 403 out of the retryable
// set. This client holds a static token and cannot reissue anything, so a
// retry re-sends the same rejected credential — six times, per call, for every
// org in a sweep.
func TestCreateMachineDoesNotRetryUnauthorized(t *testing.T) {
	shortenFlyAPIBackoff(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, WithBaseURL(srv.URL))
	image := "img:latest"
	_, err := c.CreateMachine(t.Context(), "ampbase-intel-abc", CreateMachineInput{
		Config: &MachineConfig{Image: &image},
	})
	switch {
	case !IsUnauthorized(err):
		t.Errorf("err = %v, want an unauthorized error", err)
	case calls != 1:
		t.Errorf("server received %d calls, want 1", calls)
	}
}
