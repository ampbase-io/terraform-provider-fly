// Package flyio provides a thin wrapper over the generated Fly.io Machines API client.
package flyio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	backoff "github.com/cenkalti/backoff/v5"

	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

// defaultBaseURL is the Machines API server. Operation paths carry the
// /v1 prefix themselves, as Fly's OpenAPI document declares them, so a
// base URL never includes it.
const defaultBaseURL = "https://api.machines.dev"

// Type aliases re-export generated types so callers import only "flyio".
type (
	CreateMachineInput  = machines.CreateMachineRequest
	MachineConfig       = machines.FlyMachineConfig
	MachineGuest        = machines.FlyMachineGuest
	MachineRestart      = machines.FlyMachineRestart
	MachineService      = machines.FlyMachineService
	MachineServiceCheck = machines.FlyMachineServiceCheck
	MachinePort         = machines.FlyMachinePort
)

// RestartPolicyNo never restarts a machine, whether its process exited
// cleanly or crashed. Re-exported because a one-shot machine has to set it:
// flyd's default is on-failure, and a machine that restarts never reaches the
// exited state auto_destroy fires on.
const RestartPolicyNo = machines.FlyMachineRestartPolicyNo

// Machine is the response from creating a machine.
type Machine struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	// ImageRef is what Fly resolved the config's image to. A caller that
	// names a mutable tag gets the digest behind it here, and only here:
	// the tag it asked for identifies no particular build.
	ImageRef MachineImageRef `json:"image_ref"`
}

// Client is a thin wrapper over the generated Machines API client.
type Client struct {
	gen     *machines.Client
	orgSlug string
}

// New creates a Client for the given Fly.io org. The API token defaults to
// the FLY_API_TOKEN environment variable; use WithToken to override.
func New(orgSlug string, opts ...Option) (*Client, error) {
	cfg := config{
		token:   os.Getenv("FLY_API_TOKEN"),
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.token == "" {
		return nil, fmt.Errorf("flyio: API token is required (set FLY_API_TOKEN or use WithToken)")
	}

	gen, err := machines.NewClient(cfg.baseURL,
		machines.WithHTTPClient(cfg.http),
		machines.WithRequestEditorFn(bearerAuth(cfg.token)),
	)
	if err != nil {
		return nil, fmt.Errorf("flyio: invalid base URL %q: %w", cfg.baseURL, err)
	}

	return &Client{gen: gen, orgSlug: orgSlug}, nil
}

type config struct {
	token   string
	baseURL string
	http    machines.HttpRequestDoer
}

// Option configures a Client.
type Option func(*config)

// WithToken overrides the default API token (FLY_API_TOKEN env var).
func WithToken(token string) Option {
	return func(c *config) { c.token = token }
}

// WithBaseURL overrides the Machines API server URL. Without the /v1 path:
// from inside a Fly machine, http://_api.internal:4280.
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.http = hc }
}

func bearerAuth(token string) machines.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

// ListApps returns all apps in the client's org.
func (c *Client) ListApps(ctx context.Context) ([]AppInfo, error) {
	resp, err := c.gen.AppsList(ctx, &machines.AppsListParams{
		OrgSlug: c.orgSlug,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var result machines.ListAppsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode apps list: %w", err)
	}
	out := make([]AppInfo, len(result.Apps))
	for i, app := range result.Apps {
		if app.Id != nil {
			out[i].ID = *app.Id
		}
		if app.Name != nil {
			out[i].Name = *app.Name
		}
		if app.Status != nil {
			out[i].Status = *app.Status
		}
		if app.Organization != nil && app.Organization.Slug != nil {
			out[i].OrgSlug = *app.Organization.Slug
		}
		if app.Network != nil {
			out[i].Network = *app.Network
		}
	}
	return out, nil
}

// CreateAppInput is the body for CreateApp. Org and Network are optional:
// an empty Org creates the app in the client's org, and an empty Network
// places it on that org's default network. Both are fixed at create time;
// changing either means recreating the app.
type CreateAppInput struct {
	Name    string
	Org     string
	Network string
}

// CreateApp creates a Fly.io app. Idempotent: returns nil if the app already exists.
//
// Retries on Fly Machines API 5xx responses via retryFlyAPI. The 422
// "already exists" response is already treated as success, so a retry
// after a transient 5xx that did create the app is also a no-op.
func (c *Client) CreateApp(ctx context.Context, input CreateAppInput) error {
	org := input.Org
	if org == "" {
		org = c.orgSlug
	}
	body := machines.CreateAppRequest{
		Name:    &input.Name,
		OrgSlug: &org,
	}
	if input.Network != "" {
		body.Network = &input.Network
	}
	_, err := retryFlyAPI(ctx, func() (struct{}, bool, error) {
		resp, err := c.gen.AppsCreate(ctx, body)
		if err != nil {
			return struct{}{}, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusUnprocessableEntity:
			return struct{}{}, false, nil
		case isTransient(resp):
			return struct{}{}, true, responseError(resp)
		default:
			return struct{}{}, false, responseError(resp)
		}
	})
	return err
}

// DeleteApp deletes a Fly.io app. Idempotent: returns nil if the app does not exist.
func (c *Client) DeleteApp(ctx context.Context, appName string) error {
	resp, err := c.gen.AppsDelete(ctx, appName)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp)
}

// CreateMachine creates a machine in the given app. When a Name is set on
// the input, creation is idempotent: a 422 (duplicate name) returns nil, nil
// instead of an error — except for the org machine limit, which shares that
// status code and is returned as an error IsMachineLimit reports on.
//
// Retries on Fly Machines API 5xx responses via retryFlyAPI, plus the
// 400/MANIFEST_UNKNOWN failure (see isManifestUnknown) which is the
// registry consistency lag after a fresh image push. Creation is not
// strictly idempotent when input.Name is unset: a 5xx that actually
// created a machine plus a successful retry leaves an orphan in the app.
// Callers that need idempotency set input.Name, which gives the
// 422-as-success path; an orphan is a smaller blast radius than a deploy
// stalled on a transient error. The MANIFEST_UNKNOWN retry has no such
// leak risk — the failure happens at the manifest-fetch stage, before any
// machine is provisioned.
func (c *Client) CreateMachine(ctx context.Context, appName string, input CreateMachineInput) (*Machine, error) {
	return retryFlyAPI(ctx, func() (*Machine, bool, error) {
		resp, err := c.gen.MachinesCreate(ctx, appName, input)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		// Two unrelated conditions share 422, and one of them has a
		// one-email fix: the org's machine limit, which a named create would
		// otherwise report as "the machine already exists" and a launcher
		// would read as success. Read the body before deciding.
		case resp.StatusCode == http.StatusUnprocessableEntity:
			err := responseError(resp)
			if IsMachineLimit(err) || input.Name == nil {
				return nil, false, err
			}
			return nil, false, nil
		case resp.StatusCode == http.StatusOK:
			var m Machine
			if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
				return nil, false, fmt.Errorf("decode machine response: %w", err)
			}
			return &m, false, nil
		case isTransient(resp):
			return nil, true, responseError(resp)
		default:
			err := responseError(resp)
			return nil, isManifestUnknown(err), err
		}
	})
}

// flyAPIMaxTries caps the total attempt count for Fly Machines API
// requests wrapped in retryFlyAPI. 6 attempts on the schedule below put the
// worst case under ~93 seconds (2 + 4 + 8 + 16 + 32, each jittered by up to
// ×1.5): inside a Terraform resource's patience, and long enough to outlast
// the registry's push→pull consistency window that a machine create or
// update on a freshly pushed image can land in. On breach the call fails and
// the resource reports the last response.
const flyAPIMaxTries = 6

// newFlyAPIBackoff builds the backoff schedule for Fly Machines API 5xx
// retries. Package var so tests can swap in a millisecond-scale schedule
// without changing the retry semantics.
//
// The Machines API answers a small share of well-formed provisioning calls
// with a transient 5xx that an identical retry seconds later clears, so
// every provisioning-critical call shares this schedule. MaxInterval moves
// in lockstep with flyAPIMaxTries so the later attempts spread far enough
// to clear the registry consistency tail.
//
// The schedule is jittered because 429 is in the retryable set below and
// carries no Retry-After: a fleet of calls throttled at the same instant
// would otherwise re-arrive together on every attempt.
var newFlyAPIBackoff = func() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 2 * time.Second
	b.MaxInterval = 32 * time.Second
	return b
}

// retryFlyAPI runs fn with the shared Fly Machines API 5xx retry policy.
// fn returns (value, retryable, err): when err != nil, retryable controls
// whether the loop retries (true → transient, false → wrapped permanent).
// fn must not return retryable=true with err=nil — that would loop forever.
func retryFlyAPI[T any](ctx context.Context, fn func() (T, bool, error)) (T, error) {
	return backoff.Retry(ctx, func() (T, error) {
		v, retryable, err := fn()
		if err != nil && !retryable {
			return v, backoff.Permanent(err)
		}
		return v, err
	}, backoff.WithBackOff(newFlyAPIBackoff()), backoff.WithMaxTries(flyAPIMaxTries))
}

// isTransient reports whether a response carries a status worth another
// attempt: a server-side error (500-599), or 429.
//
// 429 is here because flyctl puts it there — `internal/flapsutil/retry.go`
// classifies `http.StatusTooManyRequests` alongside 5xx. Treating it as
// permanent fails the first call of a burst rather than the last.
func isTransient(resp *http.Response) bool {
	return isTransientStatus(resp.StatusCode)
}

func isTransientStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code > 499 && code < 600)
}

// AllocateFlycast allocates a private IPv6 (flycast) address for the app
// and returns the assigned IP address. The IP is owned by the org of
// the API token used (i.e. the org of this client).
//
// network targets a specific per-app private network; empty string uses
// the app's default network. peerOrgSlug names another Fly org granted
// reach to the allocated IP (cross-org Flycast); empty string allocates a
// same-org IP.
//
// Retries on Fly Machines API 5xx responses with exponential backoff
// (see newFlyAPIBackoff). 4xx errors are wrapped in backoff.Permanent
// and propagate immediately (request shape is wrong; retrying will not
// help). Network errors are similarly permanent — the caller already
// runs inside tofu's per-resource retry budget.
func (c *Client) AllocateFlycast(ctx context.Context, appName, network, peerOrgSlug string) (string, error) {
	return c.AllocateIP(ctx, appName, "private_v6", network, peerOrgSlug)
}

// apiIPType maps the provider's friendly `type` enum to the value the
// Fly Machines API actually accepts at POST /apps/{app}/ip_assignments.
// The API enum is `v4` / `v6` / `shared_v4` / `private_v6`; the
// provider surfaces `public_v4` / `public_v6` / `shared_v4` /
// `private_v6` for readability. Without this translation a
// `public_v6` request returns 400 "invalid ip address type" — the
// fly_ip resource silently never allocates and any AAAA-dependent
// downstream (e.g. fly_cert_validation) hangs until timeout.
func apiIPType(providerType string) string {
	switch providerType {
	case "public_v4":
		return "v4"
	case "public_v6":
		return "v6"
	default:
		return providerType
	}
}

// AllocateIP allocates an IP address of the given type for the app and
// returns the assigned IP address. Supported types: "private_v6"
// (flycast), "public_v4" (dedicated public IPv4), "shared_v4" (shared
// public IPv4, the default for new apps), and "public_v6" (public IPv6).
//
// network and peerOrgSlug are only meaningful for "private_v6" — Fly
// silently ignores them for public types, but the caller (the TF
// provider) validates this at plan time to fail loudly on misuse.
//
// Same 5xx retry contract as AllocateFlycast.
func (c *Client) AllocateIP(ctx context.Context, appName, ipType, network, peerOrgSlug string) (string, error) {
	body := machines.AssignIPRequest{Type: new(machines.IPAssignmentType(apiIPType(ipType)))}
	if network != "" {
		body.Network = new(network)
	}
	if peerOrgSlug != "" {
		body.OrgSlug = new(peerOrgSlug)
	}

	return retryFlyAPI(ctx, func() (string, bool, error) {
		resp, err := c.gen.AppIPAssignmentsCreate(ctx, appName, body)
		if err != nil {
			return "", false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
			var assigned machines.IPAssignment
			if err := json.NewDecoder(resp.Body).Decode(&assigned); err != nil {
				return "", false, fmt.Errorf("decode ip assignment response: %w", err)
			}
			if assigned.Ip == nil {
				return "", false, fmt.Errorf("allocate ip for %s: API returned no IP address", appName)
			}
			return *assigned.Ip, false, nil
		case isTransient(resp):
			return "", true, responseError(resp)
		default:
			return "", false, responseError(resp)
		}
	})
}

// SecretsUpdateResult is the outcome of a SetSecrets call: metadata for
// each secret plus the new app-wide secrets version. Machines created
// with min_secrets_version >= this value will have the new secrets
// activated at boot (rather than staged).
type SecretsUpdateResult struct {
	Secrets []SecretInfo
	Version int
}

// SetSecrets sets app secrets. Existing secrets not in the map are unchanged.
// Returns metadata for all secrets and the new app-wide secrets version.
//
// Retries on Fly Machines API 5xx responses via retryFlyAPI. The operation
// is effectively idempotent (overwrites values); a retry after a transient
// 5xx that already applied the values just re-applies the same values.
// The version bump on retry is harmless — version is monotonic and the
// caller uses it to gate machine activation, not for state matching.
func (c *Client) SetSecrets(ctx context.Context, appName string, secrets map[string]string) (*SecretsUpdateResult, error) {
	return retryFlyAPI(ctx, func() (*SecretsUpdateResult, bool, error) {
		resp, err := c.gen.SecretsUpdate(ctx, appName, machines.AppSecretsUpdateRequest{
			Values: secrets,
		})
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
			var result machines.AppSecretsUpdateResp
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return nil, false, fmt.Errorf("decode secrets update response: %w", err)
			}
			out := &SecretsUpdateResult{
				Secrets: make([]SecretInfo, len(result.Secrets)),
			}
			for i, s := range result.Secrets {
				if s.Name != nil {
					out.Secrets[i].Name = *s.Name
				}
				if s.Digest != nil {
					out.Secrets[i].Digest = *s.Digest
				}
			}
			if result.Version != nil {
				out.Version = *result.Version
			}
			return out, false, nil
		case isTransient(resp):
			return nil, true, responseError(resp)
		default:
			return nil, false, responseError(resp)
		}
	})
}

// DeleteSecret deletes a single app secret. Idempotent: returns nil on 404.
func (c *Client) DeleteSecret(ctx context.Context, appName, name string) error {
	resp, err := c.gen.SecretDelete(ctx, appName, name)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp)
}

// MachineInfo captures the subset of machine state needed for reconciliation.
type MachineInfo struct {
	ID         string
	Name       string
	InstanceID string // unique per version of the machine; changes on every update
	State      string // "started", "stopped", "created", "destroyed"
	HostStatus string // "ok", "unreachable", "unknown"
	ImageRef   MachineImageRef
	Region     string
	PrivateIP  string
	// ProcessGroup is the Fly process group from machine metadata
	// (config.metadata["fly_process_group"]) — e.g. "api" or "worker"
	// for the amp app. Empty when the machine has no process-group
	// metadata.
	ProcessGroup string
	// Mounts is the machine's volume mounts as the API currently reports
	// them. Fly can change the mounted volume out from under Terraform —
	// a host migration forks the volume to a new ID and re-mounts it —
	// so the provider's Read reads these back to detect (and, with the
	// volume side's readopt, heal) that drift.
	Mounts []MachineMount
}

// MachineMount is a single volume mount on a machine as reported by the
// Machines API.
type MachineMount struct {
	Volume string
	Path   string
	Name   string
}

// MachineImageRef identifies the image running on a machine.
type MachineImageRef struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
}

func machineInfoFrom(m machines.Machine) MachineInfo {
	info := MachineInfo{}
	if m.Id != nil {
		info.ID = *m.Id
	}
	if m.Name != nil {
		info.Name = *m.Name
	}
	if m.InstanceId != nil {
		info.InstanceID = *m.InstanceId
	}
	if m.State != nil {
		info.State = *m.State
	}
	if m.HostStatus != nil {
		info.HostStatus = string(*m.HostStatus)
	}
	if m.Region != nil {
		info.Region = *m.Region
	}
	if m.PrivateIp != nil {
		info.PrivateIP = *m.PrivateIp
	}
	if m.Config != nil {
		info.ProcessGroup = m.Config.Metadata["fly_process_group"]
		for _, mt := range m.Config.Mounts {
			mm := MachineMount{}
			if mt.Volume != nil {
				mm.Volume = *mt.Volume
			}
			if mt.Path != nil {
				mm.Path = *mt.Path
			}
			if mt.Name != nil {
				mm.Name = *mt.Name
			}
			info.Mounts = append(info.Mounts, mm)
		}
	}
	if m.ImageRef != nil {
		if m.ImageRef.Repository != nil {
			info.ImageRef.Repository = *m.ImageRef.Repository
		}
		if m.ImageRef.Tag != nil {
			info.ImageRef.Tag = *m.ImageRef.Tag
		}
		if m.ImageRef.Digest != nil {
			info.ImageRef.Digest = *m.ImageRef.Digest
		}
	}
	return info
}

// ListMachines returns all machines in the given app.
func (c *Client) ListMachines(ctx context.Context, appName string) ([]MachineInfo, error) {
	resp, err := c.gen.MachinesList(ctx, appName, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var ms []machines.Machine
	if err := json.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode machines list: %w", err)
	}
	out := make([]MachineInfo, len(ms))
	for i, m := range ms {
		out[i] = machineInfoFrom(m)
	}
	return out, nil
}

// GetMachine returns details for a single machine.
func (c *Client) GetMachine(ctx context.Context, appName, machineID string) (*MachineInfo, error) {
	m, err := c.getMachineRaw(ctx, appName, machineID)
	if err != nil {
		return nil, err
	}
	info := machineInfoFrom(*m)
	return &info, nil
}

// WaitForChecks polls GetMachine until all service-level checks report
// "passing". If the machine has no checks configured, returns immediately.
// Callers control the timeout via ctx.
//
// A transient Fly Machines API 5xx on the GetMachine poll — or a
// transport-level error (dropped connection, DNS blip, client timeout) — is
// treated as retryable while ctx has budget; the wait continues rather than
// terminating. 4xx errors (e.g. machine destroyed mid-wait) terminate the wait.
func (c *Client) WaitForChecks(ctx context.Context, appName, machineID string) error {
	switch m, err := c.getMachineRaw(ctx, appName, machineID); {
	case err != nil && !isTransientFlyErr(err):
		return err
	case err == nil && !hasChecksConfigured(m):
		return nil
	case err == nil && checksAllPassing(m.Checks):
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Second
	b.MaxInterval = 5 * time.Second
	b.RandomizationFactor = 0

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		switch m, err := c.getMachineRaw(ctx, appName, machineID); {
		case err != nil && isTransientFlyErr(err) && ctx.Err() == nil:
			// 5xx or a transport-level error (dropped connection, DNS blip,
			// client timeout) — retry while ctx has budget. This is the same
			// failure mode WaitForState guards against, hit in the same
			// several-machines-created-at-once window (WaitForChecks runs right
			// after WaitForState), so a blip here must not abort the wait and
			// destroy a machine that was about to pass.
			return struct{}{}, err
		case err != nil:
			return struct{}{}, backoff.Permanent(err)
		case checksAllPassing(m.Checks):
			return struct{}{}, nil
		default:
			return struct{}{}, fmt.Errorf("checks not yet passing")
		}
	}, backoff.WithBackOff(b))
	return err
}

// isTransientErr reports whether err wraps a *ResponseError carrying a status
// worth retrying — the class WaitForChecks and other wait loops treat as
// retryable rather than terminating. Same rule as isTransient above, applied
// to an error rather than a response.
func isTransientErr(err error) bool {
	re, ok := errors.AsType[*ResponseError](err)
	return ok && isTransientStatus(re.StatusCode)
}

// isTransientFlyErr reports whether err is worth retrying while the caller's
// context still has budget. A 5xx HTTP response is transient (see
// newFlyAPIBackoff), and so is a transport-level error that carries no HTTP
// response at all — a dropped connection, DNS failure, or the HTTP client's
// per-request timeout firing on a long-poll before the machine is ready. A
// genuine non-5xx HTTP response (4xx, 408) is permanent: retrying won't change
// it.
func isTransientFlyErr(err error) bool {
	re, ok := errors.AsType[*ResponseError](err)
	if !ok {
		return true // transport error — no HTTP response
	}
	return isTransientStatus(re.StatusCode)
}

// isManifestUnknown reports whether err wraps a *ResponseError that's a
// Fly Machines API 400 carrying a Docker registry "MANIFEST_UNKNOWN"
// failure. The Fly registry has eventual consistency between push
// completion (what flyctl returns success on) and the manifest being
// visible at the pull endpoint the machines API hits when creating a
// machine. In the gap, a freshly-pushed `registry.fly.io/<app>:<tag>`
// 404s the manifest lookup even though the push succeeded — symptom
// looks like:
//
//	fly machines: 400 Bad Request (body: {"error":"failed to get
//	manifest registry.fly.io/...:<tag>: ... [http 404]:
//	{\"errors\":[{\"code\":\"MANIFEST_UNKNOWN\", ...
//
// Treat as transient and retry on the same backoff as 5xx. Empirically
// the lag is < 10s, which fits comfortably inside the existing
// retryFlyAPI window.
func isManifestUnknown(err error) bool {
	var re *ResponseError
	if !errors.As(err, &re) || re.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(re.Body, "MANIFEST_UNKNOWN")
}

func (c *Client) getMachineRaw(ctx context.Context, appName, machineID string) (*machines.Machine, error) {
	resp, err := c.gen.MachinesShow(ctx, appName, machineID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var m machines.Machine
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode machine response: %w", err)
	}
	return &m, nil
}

// hasChecksConfigured reports whether the machine has any health checks
// defined — either top-level checks or service-level checks.
func hasChecksConfigured(m *machines.Machine) bool {
	switch {
	case m.Config == nil:
		return false
	case len(m.Config.Checks) > 0:
		return true
	default:
		return slices.ContainsFunc(m.Config.Services, func(svc machines.FlyMachineService) bool {
			return len(svc.Checks) > 0
		})
	}
}

// checksAllPassing reports whether every check in the slice has status "passing".
// Returns false when the slice is empty (checks haven't reported yet).
func checksAllPassing(checks []machines.CheckStatus) bool {
	switch {
	case len(checks) == 0:
		return false
	default:
		return !slices.ContainsFunc(checks, func(c machines.CheckStatus) bool {
			return c.Status == nil || *c.Status != "passing"
		})
	}
}

// DestroyMachine destroys a machine. Idempotent: returns nil if the machine
// does not exist (404). When force is true, a running machine is killed
// without requiring a separate stop call.
func (c *Client) DestroyMachine(ctx context.Context, appName, machineID string, force bool) error {
	params := &machines.MachinesDeleteParams{}
	if force {
		params.Force = &force
	}
	resp, err := c.gen.MachinesDelete(ctx, appName, machineID, params)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp)
}

// WaitForState blocks until the machine reaches the desired state. Callers
// control the timeout via ctx (e.g., context.WithTimeout). If the Fly API's
// server-side wait times out (408), a *ResponseError is returned.
//
// When version is non-empty it is passed to the API so the wait targets a
// specific machine version (instance_id). This prevents a stale "started"
// from a previous version from satisfying the wait after an update.
//
// Retries on Fly Machines API 5xx responses and transport-level failures so a
// transient blip doesn't fail an otherwise-fine deploy. 408 (server-side wait
// timeout) is NOT retried — it's a real signal that the machine hasn't reached
// the desired state, and callers loop on it via ctx.
//
// The retry is bounded only by the caller's ctx, not by flyAPIMaxTries: this is
// a wait loop whose job is to keep polling until the machine reaches the state
// or the deadline fires. Reusing the generic maxTries cap (tuned for one-shot
// calls) could give up while ctx still has budget — WaitForChecks already uses
// this ctx-only pattern.
func (c *Client) WaitForState(ctx context.Context, appName, machineID, state, version string) error {
	params := &machines.MachinesWaitParams{
		State: (*machines.MachinesWaitParamsState)(&state),
	}
	if version != "" {
		params.Version = &version
	}
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		resp, err := c.gen.MachinesWait(ctx, appName, machineID, params)
		if err != nil {
			// MachinesWait long-polls for the state transition. The HTTP
			// client's per-request timeout (or a transient network error) can
			// fire while the machine is still coming up, even though the
			// caller's overall wait budget remains — a real case when several
			// machines are created at once and one is slow to reach "started".
			// Re-poll while ctx still has budget; only give up once the
			// caller's context itself is done (deadline exceeded / cancelled).
			if ctx.Err() != nil {
				return struct{}{}, backoff.Permanent(err)
			}
			return struct{}{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK:
			return struct{}{}, nil
		case isTransient(resp):
			return struct{}{}, responseError(resp)
		default:
			return struct{}{}, backoff.Permanent(responseError(resp))
		}
	}, backoff.WithBackOff(newFlyAPIBackoff()))
	return err
}

// WaitForRest polls GetMachine until the machine reports a rest state —
// "started", "stopped" or "suspended" — on the given instance, and returns
// that state. Callers control the timeout via ctx.
//
// This is the wait for "the update has settled, wherever it settled". The
// /wait endpoint takes one target state and answers 409 when that target is
// unreachable from the machine's current state (a stopped machine cannot
// reach "suspended" without starting first), so a caller that does not know
// which rest state an update leaves a machine in cannot use it: a wrong
// guess is an instant permanent failure, not a timeout. The instance pin
// keeps a report from the pre-update instance from satisfying the wait.
//
// Transient 5xx and transport errors are retried while ctx has budget, as
// in WaitForChecks; any other error terminates the wait.
func (c *Client) WaitForRest(ctx context.Context, appName, machineID, instanceID string) (string, error) {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Second
	b.MaxInterval = 5 * time.Second
	b.RandomizationFactor = 0

	return backoff.Retry(ctx, func() (string, error) {
		switch m, err := c.getMachineRaw(ctx, appName, machineID); {
		case err != nil && isTransientFlyErr(err) && ctx.Err() == nil:
			return "", err
		case err != nil:
			return "", backoff.Permanent(err)
		default:
			info := machineInfoFrom(*m)
			if info.InstanceID == instanceID && isRestState(info.State) {
				return info.State, nil
			}
			return "", fmt.Errorf("machine %s is %q on instance %s, not yet at rest on %s",
				machineID, info.State, info.InstanceID, instanceID)
		}
	}, backoff.WithBackOff(b))
}

// isRestState reports whether s is a state a machine stays in on its own.
// Everything else the API reports (replacing, starting, stopping,
// suspending, ...) is a transition toward one of these.
func isRestState(s string) bool {
	switch s {
	case "started", "stopped", "suspended":
		return true
	default:
		return false
	}
}

// GetApp returns basic app info. Returns nil, nil if the app does not exist.
func (c *Client) GetApp(ctx context.Context, appName string) (*AppInfo, error) {
	resp, err := c.gen.AppsShow(ctx, appName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var app machines.App
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return nil, fmt.Errorf("decode app response: %w", err)
	}
	info := &AppInfo{Name: appName}
	if app.Id != nil {
		info.ID = *app.Id
	}
	if app.Status != nil {
		info.Status = *app.Status
	}
	if app.Organization != nil && app.Organization.Slug != nil {
		info.OrgSlug = *app.Organization.Slug
	}
	if app.Network != nil {
		info.Network = *app.Network
	}
	if app.InternalNumericId != nil {
		info.InternalNumericID = int64(*app.InternalNumericId)
	}
	return info, nil
}

// AppInfo captures basic app metadata.
type AppInfo struct {
	ID      string
	Name    string
	Status  string
	OrgSlug string
	Network string // per-app private network ("" for the org's default)
	// InternalNumericID is Fly's own numeric app id, the identifier an
	// Apps macaroon caveat names. Zero when Apps_show omits it.
	InternalNumericID int64
}

// StartMachine starts a stopped or suspended machine.
func (c *Client) StartMachine(ctx context.Context, appName, machineID string) error {
	resp, err := c.gen.MachinesStart(ctx, appName, machineID)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return responseError(resp)
}

// StopMachine stops a running machine with the default signal and timeout.
// Unlike autostop (a Fly Proxy idle policy scoped to service traffic), this
// is an explicit park: the machine stays stopped until StartMachine or an
// UpdateMachine-triggered restart. Used by the fly provider's desired_state
// attribute for suspended orgs, whose machines have no services left for
// the proxy to manage.
func (c *Client) StopMachine(ctx context.Context, appName, machineID string) error {
	resp, err := c.gen.MachinesStop(ctx, appName, machineID, machines.MachinesStopJSONRequestBody{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return responseError(resp)
}

// UpdateMachine updates a machine's configuration. The machine is restarted
// with the new config.
//
// Retries on Fly Machines API 5xx responses via retryFlyAPI, plus the
// 400/MANIFEST_UNKNOWN failure (see isManifestUnknown) — Fly's UpdateMachine
// validates the new image's manifest at the registry before committing the
// config change, and a freshly pushed image can race the registry's
// push→pull consistency window. Idempotent: Fly's UpdateMachine is
// replay-safe at the API layer (sending the same config repeatedly converges
// on the same machine state).
func (c *Client) UpdateMachine(ctx context.Context, appName, machineID string, input machines.UpdateMachineRequest) (*MachineInfo, error) {
	return retryFlyAPI(ctx, func() (*MachineInfo, bool, error) {
		resp, err := c.gen.MachinesUpdate(ctx, appName, machineID, input)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK:
			var m machines.Machine
			if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
				return nil, false, fmt.Errorf("decode update machine response: %w", err)
			}
			info := machineInfoFrom(m)
			return &info, false, nil
		case isTransient(resp):
			return nil, true, responseError(resp)
		default:
			err := responseError(resp)
			return nil, isManifestUnknown(err), err
		}
	})
}

// IPAssignmentInfo captures IP assignment metadata.
type IPAssignmentInfo struct {
	IP string
}

// ListIPAssignments returns all IP assignments for the given app.
func (c *Client) ListIPAssignments(ctx context.Context, appName string) ([]IPAssignmentInfo, error) {
	resp, err := c.gen.AppIPAssignmentsList(ctx, appName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	// /apps/{app_name}/ip_assignments returns a wrapper object
	// {"ips": [...]}, not a bare array, per the OpenAPI spec.
	var wrapper machines.ListIPAssignmentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("decode ip assignments: %w", err)
	}
	out := make([]IPAssignmentInfo, len(wrapper.Ips))
	for i, ip := range wrapper.Ips {
		if ip.Ip != nil {
			out[i].IP = *ip.Ip
		}
	}
	return out, nil
}

// DeleteIPAssignment deletes an IP assignment. Idempotent: returns nil on 404.
func (c *Client) DeleteIPAssignment(ctx context.Context, appName, ip string) error {
	resp, err := c.gen.AppIPAssignmentsDelete(ctx, appName, ip)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp)
}

// SecretInfo captures app secret metadata. Values are never returned by the
// list/get endpoints — only the digest (SHA-256).
type SecretInfo struct {
	Name   string
	Digest string
}

// ListSecrets returns metadata for all secrets in the given app.
func (c *Client) ListSecrets(ctx context.Context, appName string) ([]SecretInfo, error) {
	resp, err := c.gen.SecretsList(ctx, appName, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	// /apps/{app_name}/secrets returns a wrapper object
	// {"secrets": [...]}, not a bare array, per the OpenAPI spec.
	var wrapper machines.AppSecrets
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("decode secrets list: %w", err)
	}
	out := make([]SecretInfo, len(wrapper.Secrets))
	for i, s := range wrapper.Secrets {
		if s.Name != nil {
			out[i].Name = *s.Name
		}
		if s.Digest != nil {
			out[i].Digest = *s.Digest
		}
	}
	return out, nil
}

// VolumeInfo captures the subset of Fly volume state needed for reconciliation.
type VolumeInfo struct {
	ID                string
	Name              string
	Region            string
	SizeGB            int
	Encrypted         bool
	Fstype            string
	State             string
	AttachedMachineID string
	AutoBackupEnabled bool
	SnapshotRetention int
	Zone              string
}

// CreateVolumeInput is the body for CreateVolume. Region and SizeGB are
// required by the Fly API; other fields default to Fly's defaults
// (encrypted=true, fstype="ext4", auto_backup_enabled=true,
// require_unique_zone=true, snapshot_retention=5).
//
// Compute hints Fly's volume placement at create time so the volume
// lands on a host that can later schedule a machine with that guest
// shape. Without it, even a same-region machine attachment can fail
// with "no matching host" because Fly volumes are host-pinned, not
// just region-pinned. Per the Fly docs: "Volumes are tied to specific
// physical hosts. A Machine can only mount to a volume that exists on
// the same host."
//
// UniqueZoneAppWide widens the zone-uniqueness constraint from same-
// name volumes to ALL volumes in the app (per the OpenAPI spec field
// name; not documented on fly.io/docs at time of writing). With this
// set true, distinct-named volumes still get placed on distinct
// hardware zones, which means cluster modules can give each volume a
// descriptive per-machine name (keeper_data_1, keeper_data_2, ...) and
// still get HA placement.
type CreateVolumeInput struct {
	Name              string
	Region            string
	SizeGB            int
	Encrypted         *bool
	Fstype            string
	AutoBackupEnabled *bool
	SnapshotRetention *int
	RequireUniqueZone *bool
	UniqueZoneAppWide *bool
	Compute           *machines.FlyMachineGuest
}

// CreateVolume creates a volume in the given app and returns its info.
//
// Retries on Fly Machines API 5xx responses via retryFlyAPI. Unlike
// CreateMachine, volumes have no name-based idempotency on the API side
// — a retry after a transient 5xx that already created a volume produces
// a second volume with the same name. We accept that risk for the same
// reasons as CreateMachine: production 5xx rates make non-retrying
// behavior worse, and orphan volumes are cheap to detect (TF state
// drift on next plan) and clean up.
func (c *Client) CreateVolume(ctx context.Context, appName string, input CreateVolumeInput) (*VolumeInfo, error) {
	body := machines.CreateVolumeRequest{}
	if input.Name != "" {
		body.Name = &input.Name
	}
	if input.Region != "" {
		body.Region = &input.Region
	}
	if input.SizeGB > 0 {
		body.SizeGb = &input.SizeGB
	}
	if input.Encrypted != nil {
		body.Encrypted = input.Encrypted
	}
	if input.Fstype != "" {
		body.Fstype = &input.Fstype
	}
	if input.AutoBackupEnabled != nil {
		body.AutoBackupEnabled = input.AutoBackupEnabled
	}
	if input.SnapshotRetention != nil {
		body.SnapshotRetention = input.SnapshotRetention
	}
	if input.RequireUniqueZone != nil {
		body.RequireUniqueZone = input.RequireUniqueZone
	}
	if input.UniqueZoneAppWide != nil {
		body.UniqueZoneAppWide = input.UniqueZoneAppWide
	}
	if input.Compute != nil {
		body.Compute = input.Compute
	}

	return retryFlyAPI(ctx, func() (*VolumeInfo, bool, error) {
		resp, err := c.gen.VolumesCreate(ctx, appName, body)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
			var v machines.Volume
			if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
				return nil, false, fmt.Errorf("decode volume response: %w", err)
			}
			info := volumeInfoFrom(v)
			return &info, false, nil
		case isTransient(resp):
			return nil, true, responseError(resp)
		default:
			return nil, false, responseError(resp)
		}
	})
}

// WaitForVolumeReady polls GetVolume until the volume's state is "created"
// (or the optional context deadline is reached). Fly's MachinesCreate
// blocks on volume readiness for mounts referenced in the request, so
// this is belt-and-suspenders against the edge case where a not-yet-
// ready volume ID is consumed before the volume's row finishes
// transitioning. Empirically the create-to-ready window is sub-second
// for unforked volumes; for snapshot-restored / forked volumes it can
// take longer (the docs show a "restoring" state without bounding it).
//
// The poll backs off exponentially from 250ms to a 2s cap to keep API
// load minimal while keeping latency low on the common (fast) path.
// 4xx during the wait is terminal (volume gone / inaccessible); 5xx is
// retryable.
func (c *Client) WaitForVolumeReady(ctx context.Context, appName, volumeID string) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 250 * time.Millisecond
	b.MaxInterval = 2 * time.Second
	b.RandomizationFactor = 0

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		info, err := c.GetVolume(ctx, appName, volumeID)
		if err != nil {
			if isTransientErr(err) {
				return struct{}{}, err
			}
			return struct{}{}, backoff.Permanent(err)
		}
		if info == nil {
			return struct{}{}, backoff.Permanent(fmt.Errorf("volume %s disappeared before reaching ready state", volumeID))
		}
		switch info.State {
		case "created", "ready":
			return struct{}{}, nil
		case "destroyed", "destroying":
			return struct{}{}, backoff.Permanent(fmt.Errorf("volume %s in terminal state %q", volumeID, info.State))
		default:
			// "creating", "restoring", or any other transitional state — keep polling.
			return struct{}{}, fmt.Errorf("volume %s state=%q", volumeID, info.State)
		}
	}, backoff.WithBackOff(b))
	return err
}

// GetVolume returns details for a single volume. Returns nil, nil with no
// error when the volume does not exist (404) so callers can distinguish
// "deleted out-of-band" from API errors.
func (c *Client) GetVolume(ctx context.Context, appName, volumeID string) (*VolumeInfo, error) {
	resp, err := c.gen.VolumesGetById(ctx, appName, volumeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var v machines.Volume
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode volume response: %w", err)
	}
	info := volumeInfoFrom(v)
	return &info, nil
}

// ListVolumes returns all volumes in the given app.
func (c *Client) ListVolumes(ctx context.Context, appName string) ([]VolumeInfo, error) {
	resp, err := c.gen.VolumesList(ctx, appName, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var vs []machines.Volume
	if err := json.NewDecoder(resp.Body).Decode(&vs); err != nil {
		return nil, fmt.Errorf("decode volumes list: %w", err)
	}
	out := make([]VolumeInfo, len(vs))
	for i, v := range vs {
		out[i] = volumeInfoFrom(v)
	}
	return out, nil
}

// UpdateVolume updates a volume's non-immutable fields (auto-backup,
// snapshot retention). Size changes go through ExtendVolume.
func (c *Client) UpdateVolume(ctx context.Context, appName, volumeID string, autoBackup *bool, snapshotRetention *int) (*VolumeInfo, error) {
	body := machines.UpdateVolumeRequest{}
	if autoBackup != nil {
		body.AutoBackupEnabled = autoBackup
	}
	if snapshotRetention != nil {
		body.SnapshotRetention = snapshotRetention
	}
	resp, err := c.gen.VolumesUpdate(ctx, appName, volumeID, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var v machines.Volume
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode volume response: %w", err)
	}
	info := volumeInfoFrom(v)
	return &info, nil
}

// ExtendVolume grows a volume to the given size. Shrinking is not supported
// by the Fly API; callers must enforce size_gb-only-increases at plan time.
func (c *Client) ExtendVolume(ctx context.Context, appName, volumeID string, sizeGB int) (*VolumeInfo, error) {
	body := machines.ExtendVolumeRequest{SizeGb: &sizeGB}
	resp, err := c.gen.VolumesExtend(ctx, appName, volumeID, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var result machines.ExtendVolumeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode extend volume response: %w", err)
	}
	if result.Volume == nil {
		return nil, fmt.Errorf("extend volume %s: API returned no volume payload", volumeID)
	}
	info := volumeInfoFrom(*result.Volume)
	return &info, nil
}

// DeleteVolume deletes a volume. Idempotent: returns nil on 404.
//
// Fly rejects deletion of a volume that's currently attached to a machine
// ("Volumes attached to Machines can't be destroyed" per the docs). When
// that rejection happens, we read the volume back and rewrite the error
// to name the blocking machine, so a `terraform destroy` that races the
// machine teardown surfaces a clear diagnostic instead of a generic 4xx.
func (c *Client) DeleteVolume(ctx context.Context, appName, volumeID string) error {
	resp, err := c.gen.VolumeDelete(ctx, appName, volumeID)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	apiErr := responseError(resp)
	// On 4xx, check whether the rejection is the "still attached" case
	// and rewrite the error if so. GetVolume failures fall through to
	// the raw API error — better to surface something than to mask it.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		if info, getErr := c.GetVolume(ctx, appName, volumeID); getErr == nil && info != nil && info.AttachedMachineID != "" {
			return fmt.Errorf("volume %s is attached to machine %s; destroy the machine first (terraform destroy may need to re-run after the machine teardown propagates): %w",
				volumeID, info.AttachedMachineID, apiErr)
		}
	}
	return apiErr
}

func volumeInfoFrom(v machines.Volume) VolumeInfo {
	info := VolumeInfo{}
	if v.Id != nil {
		info.ID = *v.Id
	}
	if v.Name != nil {
		info.Name = *v.Name
	}
	if v.Region != nil {
		info.Region = *v.Region
	}
	if v.SizeGb != nil {
		info.SizeGB = *v.SizeGb
	}
	if v.Encrypted != nil {
		info.Encrypted = *v.Encrypted
	}
	if v.Fstype != nil {
		info.Fstype = *v.Fstype
	}
	if v.State != nil {
		info.State = *v.State
	}
	if v.AttachedMachineId != nil {
		info.AttachedMachineID = *v.AttachedMachineId
	}
	if v.AutoBackupEnabled != nil {
		info.AutoBackupEnabled = *v.AutoBackupEnabled
	}
	if v.SnapshotRetention != nil {
		info.SnapshotRetention = *v.SnapshotRetention
	}
	if v.Zone != nil {
		info.Zone = *v.Zone
	}
	return info
}

// CertificateInfo captures the subset of Fly cert state needed to drive
// declarative DNS provisioning (Cloudflare records that point at the
// app + satisfy ACME validation). The shape collapses the nested
// dns_requirements structure into flat strings since most cluster
// modules only need CNAME / acme-challenge targets to wire records.
type CertificateInfo struct {
	Hostname         string
	Configured       bool
	Status           string
	AcmeRequested    bool
	DNSProvider      string
	CNAME            string // dns_requirements.cname — CNAME target for the hostname
	ARecords         []string
	AAAARecords      []string
	AcmeChallenge    AcmeChallenge
	Ownership        OwnershipVerification
	DnsConfigured    bool
	HttpConfigured   bool
	AlpnConfigured   bool
	RateLimitedUntil string // empty when not rate-limited
}

// AcmeChallenge mirrors the dns_requirements.acme_challenge block —
// name is the hostname (e.g. _acme-challenge.subdomain.example.com),
// target is the value (CNAME target the operator must create).
type AcmeChallenge struct {
	Name   string
	Target string
}

// OwnershipVerification mirrors dns_requirements.ownership — used when
// Fly needs a TXT record to prove the operator owns the hostname.
type OwnershipVerification struct {
	Name     string
	AppValue string
	OrgValue string
}

// CreateAcmeCertificate requests a Fly-managed ACME certificate for the
// hostname. Returns immediately after the POST — the cert is in a
// pending state until DNS propagates and ACME challenges succeed.
// Callers should poll GetCertificate / CheckCertificate to observe
// progress, or use WaitForCertificateConfigured; this call does not block
// because the caller is typically also the one creating the DNS records
// the cert needs.
//
// Idempotent: if a cert for the same hostname already exists, Fly
// returns 422 (or 409 in some paths). We treat that as success and
// read the existing cert back via GetCertificate so a re-run of
// `tofu apply` after a partial failure doesn't error out — mirrors
// CreateApp's "already exists" handling.
//
// Same 5xx retry contract as CreateMachine.
func (c *Client) CreateAcmeCertificate(ctx context.Context, appName, hostname string) (*CertificateInfo, error) {
	body := machines.AppCertificatesAcmeCreateJSONRequestBody{Hostname: &hostname}
	return retryFlyAPI(ctx, func() (*CertificateInfo, bool, error) {
		resp, err := c.gen.AppCertificatesAcmeCreate(ctx, appName, body)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
			var detail machines.CertificateDetail
			if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
				return nil, false, fmt.Errorf("decode cert response: %w", err)
			}
			info := certificateInfoFrom(detail)
			return &info, false, nil
		case resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusConflict:
			// Cert already exists — adopt the existing state instead of
			// failing the create. Common path on `tofu apply` re-runs
			// after a partial failure.
			existing, getErr := c.GetCertificate(ctx, appName, hostname)
			if getErr != nil {
				return nil, false, fmt.Errorf("create returned %d but lookup of existing cert failed: %w", resp.StatusCode, getErr)
			}
			if existing == nil {
				// 422/409 without an existing cert means the request shape
				// was rejected for some other reason (e.g. invalid
				// hostname). Surface the original 4xx with detail.
				return nil, false, responseError(resp)
			}
			return existing, false, nil
		case isTransient(resp):
			return nil, true, responseError(resp)
		default:
			return nil, false, responseError(resp)
		}
	})
}

// WaitForCertificateConfigured polls GetCertificate (with an initial
// CheckCertificate bump to nudge Fly's validator) until the cert
// reaches configured=true or the context deadline expires. The Fly API
// does not surface a terminal "validation failed permanently" state —
// certs that can't validate stay in "awaiting_configuration" until DNS
// resolves or the operator gives up — so the only exit besides success
// is context cancellation.
//
// Used by the fly_cert_validation TF resource to materialize the
// "cert is actually serving" invariant in state, mirroring the
// aws_acm_certificate_validation pattern.
func (c *Client) WaitForCertificateConfigured(ctx context.Context, appName, hostname string) (*CertificateInfo, error) {
	// One-shot recheck to bump Fly's validator instead of waiting for
	// the natural retry interval. Failure here is non-fatal — fall
	// through to the poll and let it converge on its own.
	if info, err := c.CheckCertificate(ctx, appName, hostname); err == nil && info.Configured {
		return info, nil
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 2 * time.Second
	b.MaxInterval = 15 * time.Second
	b.RandomizationFactor = 0

	return backoff.Retry(ctx, func() (*CertificateInfo, error) {
		info, err := c.GetCertificate(ctx, appName, hostname)
		if err != nil {
			if isTransientErr(err) {
				return nil, err
			}
			return nil, backoff.Permanent(err)
		}
		if info == nil {
			return nil, backoff.Permanent(fmt.Errorf("certificate for %s on app %s no longer exists", hostname, appName))
		}
		if info.Configured {
			return info, nil
		}
		return nil, fmt.Errorf("cert %s status=%q (dns=%v http=%v alpn=%v)", hostname, info.Status, info.DnsConfigured, info.HttpConfigured, info.AlpnConfigured)
	}, backoff.WithBackOff(b))
}

// GetCertificate returns the current state of a cert by hostname. Returns
// nil, nil with no error when the cert does not exist (404).
func (c *Client) GetCertificate(ctx context.Context, appName, hostname string) (*CertificateInfo, error) {
	resp, err := c.gen.AppCertificatesShow(ctx, appName, hostname)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var detail machines.CertificateDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("decode cert response: %w", err)
	}
	info := certificateInfoFrom(detail)
	return &info, nil
}

// CheckCertificate triggers Fly to re-validate DNS for the cert. Useful
// after the DNS records resolve, to bump the cert out of "awaiting
// configuration" before the natural retry interval. The response is the
// updated CertificateDetail.
func (c *Client) CheckCertificate(ctx context.Context, appName, hostname string) (*CertificateInfo, error) {
	resp, err := c.gen.AppCertificatesCheck(ctx, appName, hostname)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var detail machines.CertificateDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("decode cert check response: %w", err)
	}
	info := certificateInfoFrom(detail)
	return &info, nil
}

// DeleteCertificate removes a cert. Idempotent: returns nil on 404.
func (c *Client) DeleteCertificate(ctx context.Context, appName, hostname string) error {
	resp, err := c.gen.AppCertificatesDelete(ctx, appName, hostname)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return responseError(resp)
}

func certificateInfoFrom(d machines.CertificateDetail) CertificateInfo {
	info := CertificateInfo{}
	if d.Hostname != nil {
		info.Hostname = *d.Hostname
	}
	if d.Configured != nil {
		info.Configured = *d.Configured
	}
	if d.Status != nil {
		info.Status = *d.Status
	}
	if d.AcmeRequested != nil {
		info.AcmeRequested = *d.AcmeRequested
	}
	if d.DnsProvider != nil {
		info.DNSProvider = *d.DnsProvider
	}
	if d.RateLimitedUntil != nil {
		info.RateLimitedUntil = *d.RateLimitedUntil
	}
	if d.DnsRequirements != nil {
		if d.DnsRequirements.Cname != nil {
			info.CNAME = *d.DnsRequirements.Cname
		}
		info.ARecords = append([]string(nil), d.DnsRequirements.A...)
		info.AAAARecords = append([]string(nil), d.DnsRequirements.Aaaa...)
		if d.DnsRequirements.AcmeChallenge != nil {
			if d.DnsRequirements.AcmeChallenge.Name != nil {
				info.AcmeChallenge.Name = *d.DnsRequirements.AcmeChallenge.Name
			}
			if d.DnsRequirements.AcmeChallenge.Target != nil {
				info.AcmeChallenge.Target = *d.DnsRequirements.AcmeChallenge.Target
			}
		}
		if d.DnsRequirements.Ownership != nil {
			if d.DnsRequirements.Ownership.Name != nil {
				info.Ownership.Name = *d.DnsRequirements.Ownership.Name
			}
			if d.DnsRequirements.Ownership.AppValue != nil {
				info.Ownership.AppValue = *d.DnsRequirements.Ownership.AppValue
			}
			if d.DnsRequirements.Ownership.OrgValue != nil {
				info.Ownership.OrgValue = *d.DnsRequirements.Ownership.OrgValue
			}
		}
	}
	if d.Validation != nil {
		if d.Validation.DnsConfigured != nil {
			info.DnsConfigured = *d.Validation.DnsConfigured
		}
		if d.Validation.HttpConfigured != nil {
			info.HttpConfigured = *d.Validation.HttpConfigured
		}
		if d.Validation.AlpnConfigured != nil {
			info.AlpnConfigured = *d.Validation.AlpnConfigured
		}
	}
	return info
}

// ResponseError is returned when the Fly Machines API returns a non-success
// status code. Callers can use errors.As to extract the status code.
type ResponseError struct {
	StatusCode int
	Status     string
	Body       string
	// RequestID is Fly's per-request trace ID (fly-request-id header).
	// Useful when escalating server-side 5xx errors to Fly support —
	// they look it up in their internal logs to identify the cause.
	RequestID string
}

func (e *ResponseError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("fly machines: %s (request_id: %s, body: %s)", e.Status, e.RequestID, e.Body)
	}
	return fmt.Sprintf("fly machines: %s (body: %s)", e.Status, e.Body)
}

// IsNotFound reports whether the error is a 404 response from the Fly API.
func IsNotFound(err error) bool {
	var re *ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

// IsPreconditionFailed reports whether the error is a 412 Precondition Failed
// response from the Fly API (e.g., machine is being replaced).
func IsPreconditionFailed(err error) bool {
	var re *ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusPreconditionFailed
}

// IsMachineLimit reports whether err is Fly's refusal to create or update a
// machine because the org has reached its machine limit. The limit is dynamic
// per org and raisable on request, so this is the one create failure whose
// remedy is an email rather than a code change — worth separating from every
// other 422 at the call site.
//
// A substring match, like IsCapacityErr: the condition arrives as a 422 whose
// body is a message, and no code distinguishes it from a duplicate machine
// name. A false negative reports a generic create failure; a false positive
// reports a limit that is not there. Both are visible in the quoted body.
func IsMachineLimit(err error) bool {
	re, ok := errors.AsType[*ResponseError](err)
	return ok && re.StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(re.Body), "machine limit")
}

// IsUnauthorized reports whether the Fly API rejected the client's token
// (401/403). Separated because the remedy is a credential rotation and no
// amount of retrying reaches it: this client holds a static token, so unlike
// flyctl it cannot reissue a macaroon whose third-party discharges have
// expired — which is what a mid-run 403 on a personal `fly auth token` looks
// like. Use an org token from `fly tokens create`.
func IsUnauthorized(err error) bool {
	re, ok := errors.AsType[*ResponseError](err)
	return ok && (re.StatusCode == http.StatusUnauthorized || re.StatusCode == http.StatusForbidden)
}

// IsCapacityErr reports whether err looks like a Fly region-capacity failure,
// i.e. the chosen region can't currently place a machine, so a caller can
// fall back to another region.
//
// The check is a conservative substring match: capacity failures can surface
// through a wrapped Terraform run's error as text rather than a typed
// *ResponseError, so there is no status code to key on. A false negative
// fails the current attempt; a false positive costs one fallback attempt.
func IsCapacityErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range []string{"insufficient capacity", "no available machines", "capacity"} {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	return &ResponseError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       string(bytes.TrimSpace(b)),
		RequestID:  resp.Header.Get("fly-request-id"),
	}
}
