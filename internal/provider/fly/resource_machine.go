package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

var (
	_ resource.Resource                 = (*machineResource)(nil)
	_ resource.ResourceWithConfigure    = (*machineResource)(nil)
	_ resource.ResourceWithImportState  = (*machineResource)(nil)
	_ resource.ResourceWithUpgradeState = (*machineResource)(nil)
)

// machineStartTimeout bounds how long Create/Update wait for a machine to
// reach "started". Generous because a fresh machine can be slow to boot when
// several are created at once, and WaitForState re-polls within this budget
// rather than giving up on the first long-poll's client timeout. On breach
// Create destroys the machine and reports the wait error; Update leaves it
// for the next plan.
const machineStartTimeout = 120 * time.Second

// nameImmutableAfterCreate suppresses post-create config-side diffs on
// the machine `name` attribute. Fly's Machines API silently no-ops the
// `name` field on UpdateMachine, so a config change post-create that
// disagrees with state would result in "Provider produced inconsistent
// result after apply" — the framework's post-Update consistency check
// rejects (planned=new-name, actual-from-API=old-name).
//
// On Create (StateValue.IsNull()), this is a no-op — the config value
// flows through unchanged. On Update (StateValue is set), this forces
// the plan to the state value, silently absorbing config drift. A
// warning is emitted when config and state disagree so the operator
// knows the change was ignored.
//
// To genuinely rename a machine, the operator must `tofu taint`
// the resource (forcing destroy+recreate) since Fly itself offers no
// rename path.
type nameImmutableAfterCreate struct{}

func (m nameImmutableAfterCreate) Description(_ context.Context) string {
	return "Suppress post-create config diffs on `name` because Fly's UpdateMachine API silently no-ops renames."
}

func (m nameImmutableAfterCreate) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m nameImmutableAfterCreate) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Create path: state has no prior value; use config value as-is.
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return
	}
	// Update path: pin the plan to the state value so the framework's
	// post-apply consistency check sees no diff.
	if !req.ConfigValue.IsNull() && !req.ConfigValue.IsUnknown() &&
		req.ConfigValue.ValueString() != req.StateValue.ValueString() {
		resp.Diagnostics.AddWarning(
			"Machine name change ignored",
			fmt.Sprintf("fly_machine.name was changed in configuration from %q to %q, but Fly's Machines API does not support rename. The state value (%q) is preserved. To force a new machine with the new name, `tofu taint` this resource.",
				req.StateValue.ValueString(), req.ConfigValue.ValueString(), req.StateValue.ValueString()),
		)
	}
	resp.PlanValue = req.StateValue
}

type machineResource struct {
	pd *providerData
}

type machineResourceModel struct {
	ID                types.String        `tfsdk:"id"`
	App               types.String        `tfsdk:"app"`
	Name              types.String        `tfsdk:"name"`
	Region            types.String        `tfsdk:"region"`
	Image             types.String        `tfsdk:"image"`
	Guest             *guestModel         `tfsdk:"guest"`
	Env               types.Map           `tfsdk:"env"`
	Metadata          types.Map           `tfsdk:"metadata"`
	MinSecretsVersion types.Int64         `tfsdk:"min_secrets_version"`
	DesiredState      types.String        `tfsdk:"desired_state"`
	Service           []serviceModel      `tfsdk:"service"`
	File              []fileModel         `tfsdk:"file"`
	Mount             []mountModel        `tfsdk:"mount"`
	Restart           *restartModel       `tfsdk:"restart"`
	Container         []containerModel    `tfsdk:"container"`
	Metrics           *metricsModel       `tfsdk:"metrics"`
	Check             []machineCheckModel `tfsdk:"check"`

	// Health check configuration.
	HealthCheckURL     types.String `tfsdk:"health_check_url"`
	HealthCheckTimeout types.String `tfsdk:"health_check_timeout"`

	// Computed.
	InstanceID  types.String `tfsdk:"instance_id"`
	State       types.String `tfsdk:"state"`
	PrivateIP   types.String `tfsdk:"private_ip"`
	ImageDigest types.String `tfsdk:"image_digest"`
}

type fileModel struct {
	GuestPath  types.String `tfsdk:"guest_path"`
	RawValue   types.String `tfsdk:"raw_value"`
	SecretName types.String `tfsdk:"secret_name"`
	Mode       types.Int64  `tfsdk:"mode"`
}

type mountModel struct {
	Volume                 types.String `tfsdk:"volume"`
	Path                   types.String `tfsdk:"path"`
	Name                   types.String `tfsdk:"name"`
	ReadoptOnHostMigration types.Bool   `tfsdk:"readopt_on_host_migration"`
}

type restartModel struct {
	Policy     types.String `tfsdk:"policy"`
	MaxRetries types.Int64  `tfsdk:"max_retries"`
}

type serviceModel struct {
	InternalPort types.Int64        `tfsdk:"internal_port"`
	Protocol     types.String       `tfsdk:"protocol"`
	Autostart    types.Bool         `tfsdk:"autostart"`
	Autostop     types.String       `tfsdk:"autostop"`
	Concurrency  []concurrencyModel `tfsdk:"concurrency"`
	Port         []portModel        `tfsdk:"port"`
	Check        []checkModel       `tfsdk:"check"`
}

// concurrencyModel is the Fly Proxy load-balancing block on a service: at
// `soft_limit` open connections the proxy starts waking another machine; at
// `hard_limit` it stops routing new connections to this one. With
// `type = "connections"` the proxy counts open sockets rather than requests,
// which is what a service holding long-lived WebSockets wants.
//
// The block is modeled as a ListNestedBlock capped at one element (not a
// SingleNestedBlock) so it is genuinely optional: a SingleNestedBlock is
// always present as an object, so its Required attributes are enforced even
// when the block is omitted — which made an omitted `concurrency` fail
// validation on every service that didn't set it. A zero-length list is the
// clean "not set" state; MaxItems=1 keeps it single.
//
// All three fields are Required *within the block*. That is deliberate: it
// keeps `concurrency` non-Computed. If any field were Optional+Computed to
// absorb a server-filled default (e.g. Fly defaulting `type`), the provider
// would owe a Read round-trip to reconcile that default — and none exists,
// because setMachineState never reads the service block back from the API for
// any field. Requiring the fields and setting `type` explicitly sidesteps that
// entirely.
type concurrencyModel struct {
	Type      types.String `tfsdk:"type"`
	SoftLimit types.Int64  `tfsdk:"soft_limit"`
	HardLimit types.Int64  `tfsdk:"hard_limit"`
}

type portModel struct {
	Port     types.Int64 `tfsdk:"port"`
	Handlers types.List  `tfsdk:"handlers"`
}

type checkModel struct {
	Type     types.String `tfsdk:"type"`
	Port     types.Int64  `tfsdk:"port"`
	Path     types.String `tfsdk:"path"`
	Interval types.String `tfsdk:"interval"`
	Timeout  types.String `tfsdk:"timeout"`
	Method   types.String `tfsdk:"method"`
}

// machineCheckModel is a top-level check, the Machines API's
// `config.checks` map rather than a `services[].checks` entry. Both feed
// the same `machine.checks[]` status list that Create and Update block on
// (client.WaitForChecks), so either gates an apply — the difference is
// that a service check also controls Fly Proxy routing, and a machine
// check exists on machines that publish no service at all — a quorum
// member reached over .internal DNS, say, with no `service` block anywhere.
//
// `name` is the map key, hence required and unique per machine.
//
// A subset of machines.FlyMachineCheck, matching how checkModel narrows
// FlyMachineServiceCheck. `kind` is left out deliberately rather than for
// brevity: it would read as a way to declare a check that does not gate,
// and it is not one — WaitForChecks waits for every check to report
// passing without consulting kind, so an `informational` check would
// still block the apply. Expose it only alongside that filter.
type machineCheckModel struct {
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Port        types.Int64  `tfsdk:"port"`
	Path        types.String `tfsdk:"path"`
	Interval    types.String `tfsdk:"interval"`
	Timeout     types.String `tfsdk:"timeout"`
	GracePeriod types.String `tfsdk:"grace_period"`
	Method      types.String `tfsdk:"method"`
}

type guestModel struct {
	CPUKind  types.String `tfsdk:"cpu_kind"`
	CPUs     types.Int64  `tfsdk:"cpus"`
	MemoryMB types.Int64  `tfsdk:"memory_mb"`
}

// containerModel is the multi-container shape Fly's Machines API
// accepts. When set, the machine runs all listed containers on the
// same firecracker microVM, sharing the network namespace and the
// /.fly/api unix socket: per-machine OIDC identity is shared across
// containers, so a sidecar is the SAME principal to upstream services as
// its host.
//
// With no `container` blocks the machine runs the top-level `image`
// with the top-level `env` and `file`s: the single-container shape. Once
// any `container` block is declared, Fly ignores all three top-level
// fields — measured, not documented — so the main service has to be
// declared as a container too, and the top-level `image` stays only
// because the schema requires one.
type containerModel struct {
	Name        types.String                `tfsdk:"name"`
	Image       types.String                `tfsdk:"image"`
	Cmd         types.List                  `tfsdk:"cmd"`
	Entrypoint  types.List                  `tfsdk:"entrypoint"`
	Env         types.Map                   `tfsdk:"env"`
	File        []fileModel                 `tfsdk:"file"`
	DependsOn   []dependencyModel           `tfsdk:"depends_on"`
	Restart     *restartModel               `tfsdk:"restart"`
	Secret      []secretRefModel            `tfsdk:"secret"`
	Healthcheck []containerHealthcheckModel `tfsdk:"healthcheck"`
}

type dependencyModel struct {
	Name      types.String `tfsdk:"name"`
	Condition types.String `tfsdk:"condition"`
}

// containerHealthcheckModel is one entry in a container's `healthchecks`
// list, and what satisfies another container's
// `depends_on { condition = "healthy" }`. A machine-level `check` does not,
// however alike the two read in English: it reaches `passing` while the
// gated container never starts, silently. Both halves are measured against
// real Fly by dev/fly-container-exit-probe.sh.
//
// The times are seconds, not the Go duration strings the machine-level
// block takes, because that is the wire type here
// (FlyContainerHealthcheck.Interval is *int). The unit is in the name so
// that a block copied from one to the other fails validation instead of
// meaning something different.
type containerHealthcheckModel struct {
	Name               types.String          `tfsdk:"name"`
	Kind               types.String          `tfsdk:"kind"`
	IntervalSeconds    types.Int64           `tfsdk:"interval_seconds"`
	TimeoutSeconds     types.Int64           `tfsdk:"timeout_seconds"`
	GracePeriodSeconds types.Int64           `tfsdk:"grace_period_seconds"`
	SuccessThreshold   types.Int64           `tfsdk:"success_threshold"`
	FailureThreshold   types.Int64           `tfsdk:"failure_threshold"`
	TCP                *tcpHealthcheckModel  `tfsdk:"tcp"`
	HTTP               *httpHealthcheckModel `tfsdk:"http"`
	Exec               *execHealthcheckModel `tfsdk:"exec"`
}

// The three transports are SingleNestedAttributes rather than blocks so that
// exactly-one-of is enforceable in the schema: a block collection is empty,
// never null, when omitted, and ExactlyOneOf counts null.
type tcpHealthcheckModel struct {
	Port types.Int64 `tfsdk:"port"`
}

type execHealthcheckModel struct {
	Command types.List `tfsdk:"command"`
}

type httpHealthcheckModel struct {
	Port          types.Int64  `tfsdk:"port"`
	Path          types.String `tfsdk:"path"`
	Method        types.String `tfsdk:"method"`
	Scheme        types.String `tfsdk:"scheme"`
	TLSServerName types.String `tfsdk:"tls_server_name"`
	TLSSkipVerify types.Bool   `tfsdk:"tls_skip_verify"`
}

// secretRefModel injects an app-level fly_secret into this container
// as an env var, optionally under a different name than the secret's.
// The rename is what lets each replica hold its own app-level secret
// (`API_KEY_REPLICA_1`, `API_KEY_REPLICA_2`, ...) while every container
// reads the same env var (`API_KEY`), keeping the in-container config
// free of per-machine knobs.
type secretRefModel struct {
	EnvVar types.String `tfsdk:"env_var"`
	Name   types.String `tfsdk:"name"`
}

// metricsModel pins Fly's scrape target on this machine. Fly's
// per-region Prometheus polls <port><path> at the machine's 6PN address
// every 15s and decorates the series with app/instance/region — those
// labels reflect the HOST machine, so co-locating a sidecar that
// re-exposes infra metrics here gives correct source attribution
// without manual relabeling.
type metricsModel struct {
	Port  types.Int64  `tfsdk:"port"`
	Path  types.String `tfsdk:"path"`
	HTTPS types.Bool   `tfsdk:"https"`
}

func NewMachineResource() resource.Resource {
	return &machineResource{}
}

func (r *machineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine"
}

func (r *machineResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// Version 1: service.concurrency changed wire shape from object
		// (SingleNestedBlock) to list (ListNestedBlock). States written at
		// version 0 are migrated by UpgradeState. A future version bump must
		// update the 0-upgrader too: framework upgraders emit the *current*
		// schema shape, not the next incremental one.
		Version:     1,
		Description: "Manages a Fly.io machine. Supports create_before_destroy lifecycle.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Machine ID.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"instance_id": schema.StringAttribute{
				Description: "Machine version ID. Changes on every update; used by the Wait API.",
				Computed:    true,
			},
			"app": schema.StringAttribute{
				Description: "Application name the machine belongs to.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Description: "Machine name. Used for idempotent creation. Immutable after create: Fly's Machines API accepts a `name` field in UpdateMachine but silently ignores it, so a post-create config change to `name` is absorbed with a warning rather than planned — importing a flyctl-deployed machine (auto-generated `weathered-frog-7982`) into a resource declaring `web-1` keeps the live name in state instead of forcing destroy+recreate. To genuinely rename, taint and recreate the machine.",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					nameImmutableAfterCreate{},
				},
			},
			"region": schema.StringAttribute{
				Description: "Fly.io region (e.g. \"iad\", \"ord\").",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"image": schema.StringAttribute{
				Description: "Docker image to run. Ignored by Fly once any `container` block is declared, but still required; set it to the main container's image.",
				Required:    true,
			},
			"env": schema.MapAttribute{
				Description: "Environment variables. Ignored by Fly once any `container` block is declared; set `env` on the container instead.",
				Optional:    true,
				ElementType: types.StringType,
			},
			"metadata": schema.MapAttribute{
				Description: "Machine metadata key/value pairs. The reserved `fly_process_group` key controls which `<group>.process.<app>.internal` 6PN DNS name resolves to this machine. Changes apply via UpdateMachine in-place (machine restart) — Fly's DNS picks up the new group without a fresh machine, confirmed by the feature owner.",
				Optional:    true,
				ElementType: types.StringType,
			},
			"min_secrets_version": schema.Int64Attribute{
				Description: "Minimum app secrets version the machine must be started with. Used to activate staged secrets on a new machine — set to the version returned by fly_secret.*.version.",
				Optional:    true,
			},
			// desired_state is a pure config attribute: Read never projects
			// the machine's live state into it (autostop legitimately
			// suspends idle machines — that must not read as drift), and it
			// is deliberately NOT Computed, so existing state files that
			// predate the attribute plan clean as long as the config leaves
			// it null.
			"desired_state": schema.StringAttribute{
				Description: "State to converge the machine to after create/update: \"started\" (default) starts the machine and waits for checks; \"stopped\" stops it and skips check waits. Null behaves as \"started\". Use \"stopped\" to park a machine that has no services for Fly Proxy to manage.",
				Optional:    true,
				Validators: []validator.String{
					stringvalidator.OneOf("started", "stopped"),
				},
			},
			"health_check_url": schema.StringAttribute{
				Description: "URL to poll after creation (e.g. \"http://app.flycast:8081/health\"). Machine is not marked created until this returns 200.",
				Optional:    true,
			},
			"health_check_timeout": schema.StringAttribute{
				Description: "Maximum time to wait for the health check (Go duration, default \"60s\").",
				Optional:    true,
			},
			// state and image_digest intentionally OMIT UseStateForUnknown:
			// when image changes (or autostop suspends the machine between
			// applies), the post-apply values diverge from prior state and
			// the framework's "predict from prior state" default would
			// trigger "Provider produced inconsistent result after apply"
			// errors. Marking these "(known after apply)" on every plan
			// keeps the provider contract honest — Update returns whatever
			// the Machines API reports and tofu accepts it.
			"state": schema.StringAttribute{
				Description: "Current machine state (started, stopped, etc.).",
				Computed:    true,
			},
			"private_ip": schema.StringAttribute{
				Description: "Internal 6PN address. May change if Fly migrates the machine.",
				Computed:    true,
			},
			"image_digest": schema.StringAttribute{
				Description: "SHA-256 digest of the running image.",
				Computed:    true,
			},
		},
		Blocks: map[string]schema.Block{
			"guest": schema.SingleNestedBlock{
				Description: "Machine size configuration.",
				Attributes: map[string]schema.Attribute{
					"cpu_kind": schema.StringAttribute{
						Description: "CPU kind: \"shared\" or \"performance\".",
						Optional:    true,
						Computed:    true,
						Default:     stringdefault.StaticString("shared"),
					},
					"cpus": schema.Int64Attribute{
						Description: "Number of CPUs.",
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(1),
					},
					"memory_mb": schema.Int64Attribute{
						Description: "Memory in megabytes.",
						Optional:    true,
						Computed:    true,
						Default:     int64default.StaticInt64(256),
					},
				},
			},
			"service": schema.ListNestedBlock{
				Description: "Fly proxy service configuration. Required for Flycast routing.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"internal_port": schema.Int64Attribute{
							Description: "Port the application listens on.",
							Required:    true,
						},
						"protocol": schema.StringAttribute{
							Description: "Protocol: \"tcp\" or \"udp\".",
							Optional:    true,
						},
						"autostart": schema.BoolAttribute{
							Description: "Automatically start machine on incoming request.",
							Optional:    true,
						},
						"autostop": schema.StringAttribute{
							Description: "Autostop behavior: \"off\", \"stop\", or \"suspend\".",
							Optional:    true,
						},
					},
					Blocks: map[string]schema.Block{
						"concurrency": schema.ListNestedBlock{
							Description: "Fly Proxy load-balancing thresholds for this service (at most one block). When present, all three attributes are required; omit the block entirely to leave Fly's defaults. Modeled as a size-1 list rather than a single block so it is genuinely optional — a single nested block would enforce its required attributes even when omitted. Use `type = \"connections\"` for a service holding long-lived connections such as WebSockets, so the proxy counts open sockets — it wakes another machine at `soft_limit` and stops routing new connections at `hard_limit`. Like the rest of the `service` block, these values are write-only: they are never read back from the Machines API into state, so Terraform will not detect a server-side clamp or coercion of the limits as drift.",
							Validators: []validator.List{
								listvalidator.SizeAtMost(1),
							},
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"type": schema.StringAttribute{
										Description: "What the proxy counts: \"connections\" (persistent sockets) or \"requests\". Set explicitly — see concurrencyModel for why this stays Required rather than Optional+Computed.",
										Required:    true,
									},
									"soft_limit": schema.Int64Attribute{
										Description: "Count at which the proxy starts spilling load to (and waking) another pool machine.",
										Required:    true,
									},
									"hard_limit": schema.Int64Attribute{
										Description: "Count at which the proxy stops routing new connections to this machine.",
										Required:    true,
									},
								},
							},
						},
						"port": schema.ListNestedBlock{
							Description: "Port handler configuration for Fly proxy routing (Flycast).",
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"port": schema.Int64Attribute{
										Description: "Port number the proxy listens on.",
										Required:    true,
									},
									"handlers": schema.ListAttribute{
										Description: "Protocol handlers (e.g. [\"http\"]).",
										Optional:    true,
										ElementType: types.StringType,
									},
								},
							},
						},
						"check": schema.ListNestedBlock{
							Description: "Health check configuration.",
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"type": schema.StringAttribute{
										Description: "Check type: \"tcp\" or \"http\".",
										Required:    true,
									},
									"port": schema.Int64Attribute{
										Description: "Port to check.",
										Optional:    true,
									},
									"path": schema.StringAttribute{
										Description: "HTTP path to check.",
										Optional:    true,
									},
									"interval": schema.StringAttribute{
										Description: "Check interval (e.g. \"10s\").",
										Optional:    true,
									},
									"timeout": schema.StringAttribute{
										Description: "Check timeout (e.g. \"2s\").",
										Optional:    true,
									},
									"method": schema.StringAttribute{
										Description: "HTTP method (e.g. \"GET\").",
										Optional:    true,
									},
								},
							},
						},
					},
				},
			},
			"file": schema.ListNestedBlock{
				Description: "Per-machine file written into the guest at create/update time. Exactly one of `raw_value` or `secret_name` must be set. This is how a cluster ships per-machine config (a Raft peer list, a node ID) without baking it into the image. Ignored by Fly once any `container` block is declared; use the container's `file` blocks instead.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"guest_path": schema.StringAttribute{
							Description: "Absolute path inside the guest where the file is written (e.g. \"/etc/clickhouse-keeper/keeper_config.xml\").",
							Required:    true,
						},
						"raw_value": schema.StringAttribute{
							Description: "Base64-encoded file contents. Use `base64encode(templatefile(...))` in HCL. Mutually exclusive with `secret_name`.",
							Optional:    true,
							Validators: []validator.String{
								stringvalidator.ExactlyOneOf(
									path.MatchRelative().AtParent().AtName("raw_value"),
									path.MatchRelative().AtParent().AtName("secret_name"),
								),
							},
						},
						"secret_name": schema.StringAttribute{
							Description: "Name of an app secret whose value is the base64-encoded file contents. Mutually exclusive with `raw_value`.",
							Optional:    true,
						},
						"mode": schema.Int64Attribute{
							Description: "File mode bits (chmod(2)). Defaults to Fly's default (0644).",
							Optional:    true,
						},
					},
				},
			},
			"mount": schema.ListNestedBlock{
				Description: "Persistent volume mount. The referenced `fly_volume` must be in the same region AND on the same physical host as the machine — set the volume's `compute` block to match this machine's `guest` so Fly pre-places the volume on a compatible host.\n\n" +
					"Fly's Machines API does NOT support changing the `volume` of an existing mount in place (\"you'll need to destroy the Machine and create a new one\" per the docs). Plan-time enforcement would require RequiresReplace on a list-of-blocks which the framework doesn't model cleanly; a mid-apply API rejection is the fallback. To swap volumes, taint the machine.\n\n" +
					"Avoid `create_before_destroy` on a machine with mounts: Fly forbids two machines attaching to one volume, so the new machine's Create would fail the `validateMountsUnattached` guard while the old machine still holds the volume. The cluster bootstrap pattern (sequential, in-place replacement) is the supported flow.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"volume": schema.StringAttribute{
							Description: "Volume ID to attach (the `id` attribute of a `fly_volume` resource). Cannot be changed in place — taint the machine to swap volumes.",
							Required:    true,
						},
						"path": schema.StringAttribute{
							Description: "Absolute mount path inside the guest (e.g. \"/var/lib/clickhouse-keeper\").",
							Required:    true,
						},
						"name": schema.StringAttribute{
							Description: "Optional display name for the mount. Defaults to the volume's name.",
							Optional:    true,
						},
						"readopt_on_host_migration": schema.BoolAttribute{
							Description: "When true, `Read` reconciles this mount's `volume` from the live machine so a Fly host migration (which forks the volume to a new ID and re-mounts it) surfaces as state drift instead of staying invisible. Set this ONLY in tandem with `readopt_on_host_migration = true` on the paired `fly_volume`: the volume resource re-anchors its `id` to the fork on the same refresh, so config (`mount.volume = fly_volume.id`) and the refreshed mount agree on the new ID and no illegal in-place volume swap is planned. If this were on while the paired volume stayed on the old ID (flag off, or an ambiguous re-anchor decline), the mount would move to the fork while config still pointed at the orphan — producing exactly the mid-apply `UpdateMachine` volume-swap Fly rejects. Defaults to false, which preserves the prior behavior (mounts are never read back) for every machine that doesn't opt in.",
							Optional:    true,
							Computed:    true,
							Default:     booldefault.StaticBool(false),
						},
					},
				},
			},
			"restart": schema.SingleNestedBlock{
				Description: "Machine restart policy. Defaults to Fly's default (\"on-failure\" with 10 max retries) when unset.",
				Attributes: map[string]schema.Attribute{
					"policy": schema.StringAttribute{
						Description: "Restart policy: \"no\", \"always\", \"on-failure\", or \"spot-price\".",
						Optional:    true,
						Validators: []validator.String{
							stringvalidator.OneOf("no", "always", "on-failure", "spot-price"),
						},
					},
					"max_retries": schema.Int64Attribute{
						Description: "Maximum restart attempts when policy is \"on-failure\". Ignored for other policies.",
						Optional:    true,
					},
				},
			},
			"container": schema.ListNestedBlock{
				Description: "A container to run on the machine. With no `container` blocks the machine runs the top-level `image` with the top-level `env` and `file`s. Once any `container` block is declared, Fly ignores those three top-level fields entirely, so the main service must be declared as a container too; keep the top-level `image` set (the schema requires it) to the main service's image. Every container shares the same firecracker microVM, network namespace, and `/.fly/api` socket.\n\n" +
					"Use this for tightly-coupled co-processes (e.g. an OTel Collector that scrapes the main service on `127.0.0.1`). Fly OIDC identity is per-machine, not per-container — every container on a machine is the same principal upstream. Never use a sidecar as an auth boundary.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Description: "Container name within the machine. Must be unique per machine (Fly enforces); used in `depends_on.name` and surfaces in `flyctl logs --container <name>`.",
							Required:    true,
						},
						"image": schema.StringAttribute{
							Description: "Docker image for this container.",
							Required:    true,
						},
						"cmd": schema.ListAttribute{
							Description: "Override the image's default CMD (argv tail).",
							Optional:    true,
							ElementType: types.StringType,
						},
						"entrypoint": schema.ListAttribute{
							Description: "Override the image's default ENTRYPOINT (argv head).",
							Optional:    true,
							ElementType: types.StringType,
						},
						"env": schema.MapAttribute{
							Description: "Environment variables scoped to this container only. Merged with the machine-level `env` at runtime (container-level wins on collision).",
							Optional:    true,
							ElementType: types.StringType,
						},
					},
					Blocks: map[string]schema.Block{
						"file": schema.ListNestedBlock{
							Description: "Per-container files written into the container's filesystem at create/update time. Same semantics as the machine-level `file` block but scoped to this container only (so a sidecar's config doesn't leak into the main container's image).",
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"guest_path": schema.StringAttribute{
										Description: "Absolute path inside the container where the file is written.",
										Required:    true,
									},
									"raw_value": schema.StringAttribute{
										Description: "Base64-encoded file contents.",
										Optional:    true,
										Validators: []validator.String{
											stringvalidator.ExactlyOneOf(
												path.MatchRelative().AtParent().AtName("raw_value"),
												path.MatchRelative().AtParent().AtName("secret_name"),
											),
										},
									},
									"secret_name": schema.StringAttribute{
										Description: "Name of an app secret whose value is the base64-encoded file contents.",
										Optional:    true,
									},
									"mode": schema.Int64Attribute{
										Description: "File mode bits.",
										Optional:    true,
									},
								},
							},
						},
						"secret": schema.ListNestedBlock{
							Description: "Inject an app-level fly_secret into this container as an env var. Use when a sidecar needs a per-machine secret — define one secret per machine with a unique name (`API_KEY_REPLICA_<N>`) and use this block to rename it to the same env var (`API_KEY`) inside every container.",
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"env_var": schema.StringAttribute{
										Description: "Name of the env var inside the container.",
										Required:    true,
									},
									"name": schema.StringAttribute{
										Description: "Source secret name (must match a fly_secret on the app). Omit to use the same name as env_var.",
										Optional:    true,
									},
								},
							},
						},
						"depends_on": schema.ListNestedBlock{
							Description: "Ordering constraints — this container starts only after every named dependency reaches the requested condition. The named container must also be declared on this machine.",
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"name": schema.StringAttribute{
										Description: "Name of another container on this machine that this container depends on.",
										Required:    true,
									},
									"condition": schema.StringAttribute{
										Description: "Required dependency state: \"started\", \"healthy\", or \"exited_successfully\".",
										Optional:    true,
										Validators: []validator.String{
											stringvalidator.OneOf("started", "healthy", "exited_successfully"),
										},
									},
								},
							},
						},
						"restart": schema.SingleNestedBlock{
							Description: "Container-level restart policy. Inherits the machine policy when unset. \"spot-price\" is not supported at the container level.",
							Attributes: map[string]schema.Attribute{
								"policy": schema.StringAttribute{
									Description: "Restart policy: \"no\", \"always\", or \"on-failure\".",
									Optional:    true,
									Validators: []validator.String{
										stringvalidator.OneOf("no", "always", "on-failure"),
									},
								},
								"max_retries": schema.Int64Attribute{
									Description: "Maximum restart attempts when policy is \"on-failure\".",
									Optional:    true,
								},
							},
						},
						"healthcheck": schema.ListNestedBlock{
							Description: "Per-container health check. A readiness check here is what makes another container's `depends_on { condition = \"healthy\" }` resolve; the machine-level `check` block does not satisfy it — a machine check reaching `passing` leaves the gated container unstarted with no error and no event.\n\n" +
								"Exposes `tcp`, `http` and `exec`, exactly one per block. Two API fields are deliberately absent rather than missing: `headers` on an http check (a header on a health probe is normally a credential, and there is no secret reference for one — it would sit in plaintext HCL and in state), and `unhealthy` (the API's only value is \"stop\", so the knob would have one setting).",
							NestedObject: schema.NestedBlockObject{
								Validators: []validator.Object{exactlyOneTransport{}},
								Attributes: map[string]schema.Attribute{
									"name": schema.StringAttribute{
										Description: "Check name. Must be unique within the container.",
										Required:    true,
									},
									"kind": schema.StringAttribute{
										Description: "Check kind: \"readiness\" or \"liveness\". Use \"readiness\" for a check that another container's `depends_on { condition = \"healthy\" }` should wait on.",
										Optional:    true,
										Validators: []validator.String{
											stringvalidator.OneOf("readiness", "liveness"),
										},
									},
									"interval_seconds": schema.Int64Attribute{
										Description: "Seconds between checks. Seconds, not a duration string — the machine-level `check` block's `interval` is the duration-string one.",
										Optional:    true,
									},
									"timeout_seconds": schema.Int64Attribute{
										Description: "Seconds a single check may take before it counts as failing.",
										Optional:    true,
									},
									"grace_period_seconds": schema.Int64Attribute{
										Description: "Seconds to wait after the container starts before checking. Covers boot time that would otherwise be reported as failure.",
										Optional:    true,
									},
									"success_threshold": schema.Int64Attribute{
										Description: "Consecutive successes before the container counts as healthy.",
										Optional:    true,
									},
									"failure_threshold": schema.Int64Attribute{
										Description: "Consecutive failures before the container counts as unhealthy.",
										Optional:    true,
									},
									"tcp": schema.SingleNestedAttribute{
										Description: "TCP connect check: the container is healthy while the port accepts a connection.",
										Optional:    true,
										Attributes: map[string]schema.Attribute{
											"port": schema.Int64Attribute{
												Description: "Port to check.",
												Required:    true,
											},
										},
									},
									"http": schema.SingleNestedAttribute{
										Description: "HTTP request check. Use for a service that listens before it is ready to serve, where a TCP connect would report healthy too early.",
										Optional:    true,
										Attributes: map[string]schema.Attribute{
											"port": schema.Int64Attribute{
												Description: "Port to check.",
												Required:    true,
											},
											"path": schema.StringAttribute{
												Description: "Path to request.",
												Optional:    true,
											},
											"method": schema.StringAttribute{
												Description: "HTTP method. Defaults to Fly's default (GET) when unset.",
												Optional:    true,
											},
											"scheme": schema.StringAttribute{
												Description: "\"http\" or \"https\". Defaults to Fly's default (http) when unset.",
												Optional:    true,
												Validators: []validator.String{
													stringvalidator.OneOf("http", "https"),
												},
											},
											"tls_server_name": schema.StringAttribute{
												Description: "Hostname to validate the TLS certificate against. Only meaningful when scheme is \"https\".",
												Optional:    true,
											},
											"tls_skip_verify": schema.BoolAttribute{
												Description: "Skip TLS certificate validation. Only meaningful when scheme is \"https\"; the usual case is a health endpoint behind a self-signed certificate on loopback.",
												Optional:    true,
											},
										},
									},
									"exec": schema.SingleNestedAttribute{
										Description: "Command check: the container is healthy while the command exits 0. The command runs inside this container, so its image has to contain it.",
										Optional:    true,
										Attributes: map[string]schema.Attribute{
											"command": schema.ListAttribute{
												Description: "Command and arguments, e.g. [\"vault\", \"status\"]. An argv list, not a shell line — use [\"/bin/sh\", \"-c\", \"...\"] when you need shell semantics.",
												Required:    true,
												ElementType: types.StringType,
											},
										},
									},
								},
							},
						},
					},
				},
			},
			"metrics": schema.SingleNestedBlock{
				Description: "Per-machine Prometheus scrape target. When set, Fly's managed Prometheus polls `http(s)://<machine-6pn>:<port><path>` every 15s and tags the resulting series with `app` / `instance` / `region` from the HOST machine's identity. Co-locate a sidecar that re-exposes the main service's metrics to get correct source attribution for free.",
				Attributes: map[string]schema.Attribute{
					"port": schema.Int64Attribute{
						Description: "Port the Fly scraper connects to on the machine's 6PN address.",
						Optional:    true,
					},
					"path": schema.StringAttribute{
						Description: "URL path to scrape. Defaults to Fly's default (\"/metrics\") when unset.",
						Optional:    true,
					},
					"https": schema.BoolAttribute{
						Description: "Use https instead of http. Defaults to false.",
						Optional:    true,
					},
				},
			},
			"check": schema.ListNestedBlock{
				Description: "Machine-level health checks (the Machines API's `config.checks`). Create and Update block until every check reports `passing`, so a check here serializes a rolling apply: the next machine is not touched until this one is healthy. Use for machines that publish no `service` — a service's own `check` block is the right place otherwise, since it additionally gates Fly Proxy routing.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Description: "Check name. Becomes the key in the API's checks map, so it must be unique within the machine.",
							Required:    true,
						},
						"type": schema.StringAttribute{
							Description: "Check type: \"tcp\" or \"http\".",
							Required:    true,
						},
						"port": schema.Int64Attribute{
							Description: "Port to check on the machine's 6PN address.",
							Required:    true,
						},
						"path": schema.StringAttribute{
							Description: "HTTP path to request. Only meaningful for type \"http\".",
							Optional:    true,
						},
						"method": schema.StringAttribute{
							Description: "HTTP method. Only meaningful for type \"http\"; defaults to GET.",
							Optional:    true,
						},
						"interval": schema.StringAttribute{
							Description: "Time between checks (e.g. \"10s\").",
							Optional:    true,
						},
						"timeout": schema.StringAttribute{
							Description: "Maximum time a single check may take before it counts as failing (e.g. \"5s\").",
							Optional:    true,
						},
						"grace_period": schema.StringAttribute{
							Description: "Time to wait after the machine starts before checking (e.g. \"10s\"). Covers boot time that would otherwise be reported as failure.",
							Optional:    true,
						},
					},
				},
			},
		},
	}
}

// UpgradeState migrates version-0 states to the current schema. Version 0
// stored `service[*].concurrency` as an object (SingleNestedBlock); version 1
// stores it as a size-1 list (ListNestedBlock). Everything else is unchanged.
func (r *machineResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: {StateUpgrader: r.upgradeStateV0},
	}
}

// upgradeStateV0 works on the raw state JSON instead of a PriorSchema decode
// because version-0 states exist in BOTH concurrency shapes: object-shaped
// states written under the SingleNestedBlock schema, and list-shaped states
// written by binaries built after the ListNestedBlock change but before this
// upgrader existed (those stamped version 0 too). A PriorSchema decode could
// only read one of the two.
func (r *machineResource) upgradeStateV0(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
	if req.RawState == nil || len(req.RawState.JSON) == 0 {
		resp.Diagnostics.AddError(
			"Missing Raw State for fly_machine Upgrade",
			"The prior state was not available in JSON form, so the version 0 -> 1 concurrency migration cannot run. This state may predate Terraform 0.12; refresh it with the previous provider version first.",
		)
		return
	}

	upgraded, err := listifyConcurrency(req.RawState.JSON)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_machine State",
			fmt.Sprintf("Rewriting service.concurrency from the version 0 object shape to the version 1 list shape failed: %s", err),
		)
		return
	}

	// Decode the rewritten JSON against the current schema. This both
	// validates the migration output (a shape this upgrader missed fails
	// loudly here rather than corrupting state) and yields the value to
	// re-encode canonically for the response.
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	typ := schemaResp.Schema.Type().TerraformType(ctx)
	val, err := (tfprotov6.RawState{JSON: upgraded}).Unmarshal(typ)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_machine State",
			fmt.Sprintf("The migrated state does not match the current resource schema: %s", err),
		)
		return
	}
	dv, err := tfprotov6.NewDynamicValue(typ, val)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_machine State",
			fmt.Sprintf("Re-encoding the migrated state failed: %s", err),
		)
		return
	}
	resp.DynamicValue = &dv
}

// listifyConcurrency rewrites each service's `concurrency` from the version-0
// object shape to the version-1 list shape, touching nothing else: values are
// carried as json.RawMessage so numbers and strings round-trip byte-for-byte.
//
//	{...}   -> [{...}]  (state applied under the SingleNestedBlock schema)
//	null    -> []       (matches what the framework stores for zero blocks)
//	[...]   -> [...]    (already list-shaped; left as-is)
func listifyConcurrency(stateJSON []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return nil, fmt.Errorf("decode state object: %w", err)
	}
	svcRaw, ok := state["service"]
	if !ok || jsonIsNull(svcRaw) {
		return stateJSON, nil
	}
	var services []map[string]json.RawMessage
	if err := json.Unmarshal(svcRaw, &services); err != nil {
		return nil, fmt.Errorf("decode service list: %w", err)
	}
	for _, svc := range services {
		conc := bytes.TrimSpace(svc["concurrency"])
		switch {
		case len(conc) == 0 || jsonIsNull(conc):
			svc["concurrency"] = json.RawMessage("[]")
		case conc[0] == '{':
			wrapped := make(json.RawMessage, 0, len(conc)+2)
			wrapped = append(wrapped, '[')
			wrapped = append(wrapped, conc...)
			wrapped = append(wrapped, ']')
			svc["concurrency"] = wrapped
		}
	}
	newSvc, err := json.Marshal(services)
	if err != nil {
		return nil, fmt.Errorf("encode service list: %w", err)
	}
	state["service"] = newSvc
	return json.Marshal(state)
}

func jsonIsNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func (r *machineResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T", req.ProviderData))
		return
	}
	r.pd = pd
}

func (r *machineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan machineResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	input := buildCreateMachineInput(&plan)
	appName := plan.App.ValueString()

	// Defensive pre-create validation on mount blocks. Fly's MachinesCreate
	// will reject conflicting attachments at the API layer, but the error
	// message is generic; checking here surfaces the actual cause (HCL bug
	// where the same volume ID was wired to two machines, or two mount
	// blocks within one machine share a volume) before any API call. In the
	// shared-volume-name + count.index cluster pattern, one stray
	// fly_volume.data[0] in place of count.index would otherwise collapse
	// two replicas onto one volume.
	if diags := validateMountUniqueness(plan.Mount); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}
	if diags := validateContainerNameUniqueness(plan.Container); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}
	if diags := validateHealthcheckNameUniqueness(plan.Container); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}
	if diags := validateCheckNameUniqueness(plan.Check); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}
	if diags := r.validateMountsUnattached(ctx, appName, plan.Mount); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}

	m, err := client.CreateMachine(ctx, appName, input)
	if err != nil {
		resp.Diagnostics.AddError("Create Machine Error", err.Error())
		return
	}

	// Idempotent path: machine with this name already exists.
	// Look it up by listing.
	if m == nil {
		info, err := r.findMachineByName(ctx, client, appName, plan.Name.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Find Machine Error", err.Error())
			return
		}
		if info == nil {
			resp.Diagnostics.AddError("Find Machine Error", "machine not found after idempotent create")
			return
		}
		setMachineState(&plan, info)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	plan.ID = types.StringValue(m.ID)

	// Wait for machine to reach "started" state.
	waitCtx, cancel := context.WithTimeout(ctx, machineStartTimeout)
	defer cancel()
	if err := client.WaitForState(waitCtx, appName, m.ID, "started", m.InstanceID); err != nil {
		r.cleanupFailedMachine(&resp.Diagnostics, appName, m.ID, "start-wait failure")
		resp.Diagnostics.AddError("Wait For Started Error",
			fmt.Sprintf("machine %s did not reach started state: %s", m.ID, err))
		return
	}

	// Park the machine when the plan asks for "stopped":
	// stop after the successful boot and skip the check/health waits — a
	// parked machine typically has no services left for checks to cover.
	if plan.DesiredState.ValueString() == machineStateStopped {
		if err := r.stopAndWait(ctx, appName, m.ID); err != nil {
			r.cleanupFailedMachine(&resp.Diagnostics, appName, m.ID, "stop failure")
			resp.Diagnostics.AddError("Stop Machine Error",
				fmt.Sprintf("machine %s did not reach stopped state: %s", m.ID, err))
			return
		}
		info, err := client.GetMachine(ctx, appName, m.ID)
		if err != nil {
			resp.Diagnostics.AddError("Read Machine Error", err.Error())
			return
		}
		setMachineState(&plan, info)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// Wait for all Fly service-level checks to pass before marking as created.
	checksCtx, checksCancel := context.WithTimeout(ctx, machineStartTimeout)
	defer checksCancel()
	if err := client.WaitForChecks(checksCtx, appName, m.ID); err != nil {
		r.cleanupFailedMachine(&resp.Diagnostics, appName, m.ID, "check failure")
		resp.Diagnostics.AddError("Wait For Checks Error",
			fmt.Sprintf("machine %s checks did not pass: %s", m.ID, err))
		return
	}

	// Health check: poll the configured URL before marking as created.
	if !plan.HealthCheckURL.IsNull() && !plan.HealthCheckURL.IsUnknown() {
		if err := r.pollHealth(ctx, &plan); err != nil {
			r.cleanupFailedMachine(&resp.Diagnostics, appName, m.ID, "health check failure")
			resp.Diagnostics.AddError("Health Check Failed",
				fmt.Sprintf("machine %s health check failed: %s", m.ID, err))
			return
		}
	}

	// Read back full state.
	info, err := client.GetMachine(ctx, appName, m.ID)
	if err != nil {
		resp.Diagnostics.AddError("Read Machine Error", err.Error())
		return
	}
	setMachineState(&plan, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *machineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state machineResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	info, err := client.GetMachine(ctx, state.App.ValueString(), state.ID.ValueString())
	if err != nil {
		if flyio.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read Machine Error", err.Error())
		return
	}

	setMachineState(&state, info)
	refreshMountVolumes(&state, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// refreshMountVolumes reconciles a mount block's volume ID against what
// the machine actually mounts, matching blocks by path. Fly can swap the
// mounted volume without Terraform's involvement — a host migration forks
// the volume to a new ID and re-mounts it — and without this the drift is
// invisible (setMachineState reads back only scalar machine fields, not
// blocks), so `plan -refresh-only` reports "No changes" while state
// points at an orphaned volume.
//
// Gated per-mount on readopt_on_host_migration. This MUST stay in lockstep
// with the paired fly_volume's re-anchor decision: config resolves
// mount.volume to fly_volume.id, so if this refreshed the mount to the
// fork while the paired volume kept its old id (its flag off, or its
// re-anchor declined as ambiguous), config and state would disagree and
// the next apply would attempt an in-place mount-volume swap that Fly
// rejects mid-apply. Defaulting the flag off means an un-opted-in machine
// behaves exactly as before (mounts never read back) — no new failure
// mode. Only opt in where the paired fly_volume also opts in and has an
// app-unique name.
//
// Only the volume field is refreshed: path is the join key, and name is
// server-defaulted (Fly fills it from the volume when the config omits
// it), so reading it back would manufacture a spurious diff on an
// optional block field. Blocks whose path the API doesn't report are
// left untouched rather than dropped.
func refreshMountVolumes(model *machineResourceModel, info *flyio.MachineInfo) {
	if len(model.Mount) == 0 || len(info.Mounts) == 0 {
		return
	}
	byPath := make(map[string]string, len(info.Mounts))
	for _, m := range info.Mounts {
		byPath[m.Path] = m.Volume
	}
	for i := range model.Mount {
		if !model.Mount[i].ReadoptOnHostMigration.ValueBool() {
			continue
		}
		if vol, ok := byPath[model.Mount[i].Path.ValueString()]; ok && vol != "" {
			model.Mount[i].Volume = types.StringValue(vol)
		}
	}
}

func (r *machineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan machineResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state machineResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if diags := validateCheckNameUniqueness(plan.Check); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}
	if diags := validateHealthcheckNameUniqueness(plan.Container); diags != nil {
		resp.Diagnostics.Append(diags...)
		return
	}

	client := r.pd.client

	appName := plan.App.ValueString()
	machineID := state.ID.ValueString()

	// Read pre-update state: the update does not bring a cold machine up,
	// so a stopped or suspended one must be started explicitly afterward
	// for the new config to be verified via health checks.
	preUpdate, err := client.GetMachine(ctx, appName, machineID)
	if err != nil {
		resp.Diagnostics.AddError("Read Machine Error",
			fmt.Sprintf("failed to read machine %s before update: %s", machineID, err))
		return
	}

	updateReq := buildUpdateMachineInput(&plan)
	info, err := client.UpdateMachine(ctx, appName, machineID, updateReq)
	if err != nil {
		resp.Diagnostics.AddError("Update Machine Error", err.Error())
		return
	}

	// Converge to "stopped" when the plan asks for it,
	// skipping the start/check waits below — a parked machine has no
	// services for them to cover.
	if plan.DesiredState.ValueString() == machineStateStopped {
		if err := r.parkOnUpdate(ctx, appName, machineID, preUpdate.State, info.InstanceID); err != nil {
			resp.Diagnostics.AddError("Stop Machine Error",
				fmt.Sprintf("machine %s: %s", machineID, err))
			return
		}
		info, err = client.GetMachine(ctx, appName, machineID)
		if err != nil {
			resp.Diagnostics.AddError("Read Machine Error", err.Error())
			return
		}
		plan.ID = state.ID
		setMachineState(&plan, info)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// The update leaves a cold machine stopped — a suspended one included,
	// which is the console's usual idle state under autostop = "suspend" —
	// unless traffic autostarts it first, which a machine with autostart
	// can see at any moment. So watch for whichever rest state it reaches
	// on the new instance rather than waiting on one: the update passes
	// through "replacing" first and a start issued mid-replace is lost.
	// Then start it, if nothing else has, so its checks run against the
	// new config.
	if !updateLaunches(preUpdate.State) {
		settleCtx, settleCancel := context.WithTimeout(ctx, machineSettleTimeout)
		defer settleCancel()
		rest, err := client.WaitForRest(settleCtx, appName, machineID, info.InstanceID)
		if err != nil {
			resp.Diagnostics.AddError("Wait For Settle Error",
				fmt.Sprintf("machine %s did not settle after update: %s", machineID, err))
			return
		}
		if rest != "started" {
			if err := client.StartMachine(ctx, appName, machineID); err != nil {
				resp.Diagnostics.AddError("Start Machine Error",
					fmt.Sprintf("failed to start machine %s after update: %s", machineID, err))
				return
			}
		}
	}

	// Wait for the new version to reach "started" state after update.
	waitCtx, cancel := context.WithTimeout(ctx, machineStartTimeout)
	defer cancel()
	if err := client.WaitForState(waitCtx, appName, machineID, "started", info.InstanceID); err != nil {
		resp.Diagnostics.AddError("Wait For Started Error",
			fmt.Sprintf("machine %s did not reach started state after update: %s", machineID, err))
		return
	}

	// Wait for all Fly service-level checks to pass after update.
	// No rollback on failure: the machine exists and may recover on its
	// own; state is left intact for the next plan to detect and retry.
	checksCtx, checksCancel := context.WithTimeout(ctx, machineStartTimeout)
	defer checksCancel()
	if err := client.WaitForChecks(checksCtx, appName, machineID); err != nil {
		resp.Diagnostics.AddError("Wait For Checks Error",
			fmt.Sprintf("machine %s checks did not pass after update: %s", machineID, err))
		return
	}

	// Read back full state (private_ip or other fields may have changed).
	info, err = client.GetMachine(ctx, appName, machineID)
	if err != nil {
		resp.Diagnostics.AddError("Read Machine Error", err.Error())
		return
	}

	plan.ID = state.ID
	setMachineState(&plan, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// ImportState parses the composite import ID "<app_name>/<machine_id>"
// into the resource's `app` + `id` attributes, then defers to Read
// (called by the framework after ImportState) for everything else.
// The composite form is required because Fly machine IDs are unique
// only within an app — without the app prefix, the Read call wouldn't
// know which app to query.
//
// Used by `import` blocks in HCL to bring flyctl-deployed machines under
// Terraform management without destroying and recreating them.
func (r *machineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	appName, machineID, err := parseImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app"), appName)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), machineID)...)
}

// parseImportID splits a "<app_name>/<machine_id>" composite import
// ID. Extracted from ImportState so the parsing logic is unit-testable
// without standing up a full tfsdk.State.
func parseImportID(id string) (string, string, error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected import ID in the form \"<app_name>/<machine_id>\" (e.g. \"my-app/2872174b330338\"), got %q", id)
	}
	return parts[0], parts[1], nil
}

func (r *machineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state machineResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	if err := client.DestroyMachine(ctx, state.App.ValueString(), state.ID.ValueString(), true); err != nil {
		resp.Diagnostics.AddError("Delete Machine Error", err.Error())
	}
}

func (r *machineResource) findMachineByName(ctx context.Context, client *flyio.Client, appName, name string) (*flyio.MachineInfo, error) {
	ms, err := client.ListMachines(ctx, appName)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.Name == name && m.State != "destroyed" {
			return &m, nil
		}
	}
	return nil, nil
}

func buildCreateMachineInput(plan *machineResourceModel) flyio.CreateMachineInput {
	input := flyio.CreateMachineInput{
		Config: &machines.FlyMachineConfig{},
	}

	if !plan.Name.IsNull() && !plan.Name.IsUnknown() {
		name := plan.Name.ValueString()
		input.Name = &name
	}
	if !plan.Region.IsNull() && !plan.Region.IsUnknown() {
		region := plan.Region.ValueString()
		input.Region = &region
	}

	image := plan.Image.ValueString()
	input.Config.Image = &image

	guest := buildGuest(plan)
	if guest != nil {
		input.Config.Guest = guest
	}

	env := buildEnv(plan)
	if env != nil {
		input.Config.Env = env
	}

	metadata := buildMetadata(plan)
	if metadata != nil {
		input.Config.Metadata = metadata
	}

	if !plan.MinSecretsVersion.IsNull() && !plan.MinSecretsVersion.IsUnknown() {
		v := int(plan.MinSecretsVersion.ValueInt64())
		input.MinSecretsVersion = &v
	}

	input.Config.Services = buildServices(plan.Service)
	input.Config.Files = buildFiles(plan.File)
	input.Config.Mounts = buildMounts(plan.Mount)
	input.Config.Restart = buildRestart(plan.Restart)
	input.Config.Containers = buildContainers(plan.Container)
	input.Config.Metrics = buildMetrics(plan.Metrics)
	input.Config.Checks = buildChecks(plan.Check)

	return input
}

func buildUpdateMachineInput(plan *machineResourceModel) machines.UpdateMachineRequest {
	req := machines.UpdateMachineRequest{
		Config: &machines.FlyMachineConfig{},
	}

	// req.Name intentionally NOT set: Fly's UpdateMachine accepts the
	// `name` field in its request schema but silently no-ops renames, so
	// sending it produces "Provider produced inconsistent result after
	// apply" — the framework compares the planned name against the
	// post-update API-returned name, which is unchanged. The
	// nameImmutableAfterCreate plan modifier above suppresses config-side
	// name diffs entirely so a planned name never differs from state.

	image := plan.Image.ValueString()
	req.Config.Image = &image

	guest := buildGuest(plan)
	if guest != nil {
		req.Config.Guest = guest
	}

	env := buildEnv(plan)
	if env != nil {
		req.Config.Env = env
	}

	metadata := buildMetadata(plan)
	if metadata != nil {
		req.Config.Metadata = metadata
	}

	if !plan.MinSecretsVersion.IsNull() && !plan.MinSecretsVersion.IsUnknown() {
		v := int(plan.MinSecretsVersion.ValueInt64())
		req.MinSecretsVersion = &v
	}

	req.Config.Services = buildServices(plan.Service)
	req.Config.Files = buildFiles(plan.File)
	req.Config.Mounts = buildMounts(plan.Mount)
	req.Config.Restart = buildRestart(plan.Restart)
	req.Config.Containers = buildContainers(plan.Container)
	req.Config.Metrics = buildMetrics(plan.Metrics)
	req.Config.Checks = buildChecks(plan.Check)

	return req
}

func buildGuest(plan *machineResourceModel) *machines.FlyMachineGuest {
	if plan.Guest == nil {
		return nil
	}
	g := plan.Guest
	guest := &machines.FlyMachineGuest{}
	if !g.CPUKind.IsNull() {
		kind := g.CPUKind.ValueString()
		guest.CpuKind = &kind
	}
	if !g.CPUs.IsNull() {
		cpus := int(g.CPUs.ValueInt64())
		guest.Cpus = &cpus
	}
	if !g.MemoryMB.IsNull() {
		mem := int(g.MemoryMB.ValueInt64())
		guest.MemoryMb = &mem
	}
	return guest
}

func buildEnv(plan *machineResourceModel) map[string]string {
	if plan.Env.IsNull() || plan.Env.IsUnknown() {
		return nil
	}
	env := make(map[string]string)
	for k, v := range plan.Env.Elements() {
		if sv, ok := v.(types.String); ok {
			env[k] = sv.ValueString()
		}
	}
	return env
}

func buildMetadata(plan *machineResourceModel) map[string]string {
	if plan.Metadata.IsNull() || plan.Metadata.IsUnknown() {
		return nil
	}
	md := make(map[string]string)
	for k, v := range plan.Metadata.Elements() {
		if sv, ok := v.(types.String); ok {
			md[k] = sv.ValueString()
		}
	}
	return md
}

func buildFiles(files []fileModel) []machines.FlyFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]machines.FlyFile, len(files))
	for i, f := range files {
		gp := f.GuestPath.ValueString()
		out[i] = machines.FlyFile{GuestPath: &gp}
		if !f.RawValue.IsNull() && !f.RawValue.IsUnknown() {
			rv := f.RawValue.ValueString()
			out[i].RawValue = &rv
		}
		if !f.SecretName.IsNull() && !f.SecretName.IsUnknown() {
			sn := f.SecretName.ValueString()
			out[i].SecretName = &sn
		}
		if !f.Mode.IsNull() && !f.Mode.IsUnknown() {
			m := int(f.Mode.ValueInt64())
			out[i].Mode = &m
		}
	}
	return out
}

func buildContainers(containers []containerModel) []machines.FlyContainerConfig {
	if len(containers) == 0 {
		return nil
	}
	out := make([]machines.FlyContainerConfig, len(containers))
	for i, c := range containers {
		name := c.Name.ValueString()
		image := c.Image.ValueString()
		fc := machines.FlyContainerConfig{
			Name:  &name,
			Image: &image,
		}
		if cmd := stringListValues(c.Cmd); len(cmd) > 0 {
			fc.Cmd = cmd
		}
		if ep := stringListValues(c.Entrypoint); len(ep) > 0 {
			fc.Entrypoint = ep
		}
		if env := stringMapValues(c.Env); len(env) > 0 {
			fc.Env = env
		}
		if files := buildFiles(c.File); len(files) > 0 {
			fc.Files = files
		}
		if deps := buildDependsOn(c.DependsOn); len(deps) > 0 {
			fc.DependsOn = deps
		}
		if secrets := buildContainerSecrets(c.Secret); len(secrets) > 0 {
			fc.Secrets = secrets
		}
		fc.Healthchecks = buildHealthchecks(c.Healthcheck)
		fc.Restart = buildRestart(c.Restart)
		out[i] = fc
	}
	return out
}

func buildContainerSecrets(secrets []secretRefModel) []machines.FlyMachineSecret {
	if len(secrets) == 0 {
		return nil
	}
	out := make([]machines.FlyMachineSecret, len(secrets))
	for i, s := range secrets {
		ev := s.EnvVar.ValueString()
		out[i] = machines.FlyMachineSecret{EnvVar: &ev}
		if !s.Name.IsNull() && !s.Name.IsUnknown() {
			n := s.Name.ValueString()
			out[i].Name = &n
		}
	}
	return out
}

func buildDependsOn(deps []dependencyModel) []machines.FlyContainerDependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]machines.FlyContainerDependency, len(deps))
	for i, d := range deps {
		name := d.Name.ValueString()
		out[i] = machines.FlyContainerDependency{Name: &name}
		if !d.Condition.IsNull() && !d.Condition.IsUnknown() {
			cond := machines.FlyContainerDependencyCondition(d.Condition.ValueString())
			out[i].Condition = &cond
		}
	}
	return out
}

// `name` and the transports' `port` are Required within their blocks, so
// they are always known when Create or Update builds a request and are set
// directly, the way buildConcurrency treats its own required fields. The
// null-aware helpers below are for the optional fields only.
func buildHealthchecks(hcs []containerHealthcheckModel) []machines.FlyContainerHealthcheck {
	if len(hcs) == 0 {
		return nil
	}
	out := make([]machines.FlyContainerHealthcheck, len(hcs))
	for i, hc := range hcs {
		out[i] = machines.FlyContainerHealthcheck{
			Name:             new(hc.Name.ValueString()),
			Kind:             enumPtr[machines.FlyContainerHealthcheckKind](hc.Kind),
			Interval:         intPtr(hc.IntervalSeconds),
			Timeout:          intPtr(hc.TimeoutSeconds),
			GracePeriod:      intPtr(hc.GracePeriodSeconds),
			SuccessThreshold: intPtr(hc.SuccessThreshold),
			FailureThreshold: intPtr(hc.FailureThreshold),
			Tcp:              buildTCPHealthcheck(hc.TCP),
			Http:             buildHTTPHealthcheck(hc.HTTP),
			Exec:             buildExecHealthcheck(hc.Exec),
		}
	}
	return out
}

func buildTCPHealthcheck(t *tcpHealthcheckModel) *machines.FlyTCPHealthcheck {
	if t == nil {
		return nil
	}
	return &machines.FlyTCPHealthcheck{Port: new(int(t.Port.ValueInt64()))}
}

func buildExecHealthcheck(e *execHealthcheckModel) *machines.FlyExecHealthcheck {
	if e == nil {
		return nil
	}
	return &machines.FlyExecHealthcheck{Command: stringListValues(e.Command)}
}

func buildHTTPHealthcheck(h *httpHealthcheckModel) *machines.FlyHTTPHealthcheck {
	if h == nil {
		return nil
	}
	return &machines.FlyHTTPHealthcheck{
		Port:          new(int(h.Port.ValueInt64())),
		Path:          strPtr(h.Path),
		Method:        strPtr(h.Method),
		Scheme:        enumPtr[machines.FlyContainerHealthcheckScheme](h.Scheme),
		TlsServerName: strPtr(h.TLSServerName),
		TlsSkipVerify: boolPtr(h.TLSSkipVerify),
	}
}

// intPtr, strPtr, enumPtr and boolPtr render an unset optional as an
// omitted field rather than a zero one. The wire types are `omitempty`
// pointers, so absent and zero are distinguishable — sending 0 for an
// interval or a threshold states a value where the intent was to leave it
// unset.
//
// enumPtr is strPtr for the fields the API models as named string types;
// the caller names the type because nothing in the argument determines it.
func intPtr(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	return new(int(v.ValueInt64()))
}

func strPtr(v types.String) *string {
	return enumPtr[string](v)
}

func enumPtr[T ~string](v types.String) *T {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	return new(T(v.ValueString()))
}

func boolPtr(v types.Bool) *bool {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	return new(v.ValueBool())
}

func buildMetrics(m *metricsModel) *machines.FlyMachineMetrics {
	if m == nil {
		return nil
	}
	out := &machines.FlyMachineMetrics{}
	if !m.Port.IsNull() && !m.Port.IsUnknown() {
		p := int(m.Port.ValueInt64())
		out.Port = &p
	}
	if !m.Path.IsNull() && !m.Path.IsUnknown() {
		path := m.Path.ValueString()
		out.Path = &path
	}
	if !m.HTTPS.IsNull() && !m.HTTPS.IsUnknown() {
		h := m.HTTPS.ValueBool()
		out.Https = &h
	}
	if out.Port == nil && out.Path == nil && out.Https == nil {
		return nil
	}
	return out
}

func stringListValues(l types.List) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	elems := l.Elements()
	out := make([]string, len(elems))
	for i, e := range elems {
		if sv, ok := e.(types.String); ok {
			out[i] = sv.ValueString()
		}
	}
	return out
}

func stringMapValues(m types.Map) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := make(map[string]string, len(m.Elements()))
	for k, v := range m.Elements() {
		if sv, ok := v.(types.String); ok {
			out[k] = sv.ValueString()
		}
	}
	return out
}

// validateContainerNameUniqueness rejects container blocks that share
// a name on the same machine. Fly's Machines API rejects them at the
// server with a generic error; surfacing here gives the operator a
// clean diagnostic naming which name collided, parallel to
// validateMountUniqueness.
func validateContainerNameUniqueness(containers []containerModel) diag.Diagnostics {
	if len(containers) <= 1 {
		return nil
	}
	seen := make(map[string]struct{}, len(containers))
	var diags diag.Diagnostics
	for _, c := range containers {
		if c.Name.IsNull() || c.Name.IsUnknown() {
			continue
		}
		name := c.Name.ValueString()
		if _, ok := seen[name]; ok {
			diags.AddError(
				"Duplicate Container Name",
				fmt.Sprintf("container name %q appears in more than one container block on this machine. Container names must be unique per machine (Fly's Machines API enforces this at the server).", name),
			)
			return diags
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateHealthcheckNameUniqueness rejects healthcheck blocks that share a
// name within one container. machines.FlyContainerHealthcheck.Name carries
// the constraint ("Must be unique within the container") but the field is a
// list rather than a map, so whether Fly rejects a duplicate or silently
// keeps one of them is not something the generated types settle. Rejecting
// here means no config depends on the answer.
//
// Names collide per container, not per machine: two containers may each
// have a check called "listening".
func validateHealthcheckNameUniqueness(containers []containerModel) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, c := range containers {
		if name, dup := duplicateHealthcheckName(c.Healthcheck); dup {
			diags.AddError(
				"Duplicate Healthcheck Name",
				fmt.Sprintf("healthcheck name %q appears in more than one healthcheck block on container %q. Healthcheck names must be unique within a container.", name, c.Name.ValueString()),
			)
			return diags
		}
	}
	return nil
}

// duplicateHealthcheckName reports the first name declared twice in one
// container. It returns a flag rather than an empty-string sentinel because
// `name` is Required but not non-empty, so "" is a name a config can use.
func duplicateHealthcheckName(hcs []containerHealthcheckModel) (string, bool) {
	if len(hcs) < 2 {
		return "", false
	}
	seen := make(map[string]struct{}, len(hcs))
	for _, hc := range hcs {
		if hc.Name.IsNull() || hc.Name.IsUnknown() {
			continue
		}
		name := hc.Name.ValueString()
		if _, ok := seen[name]; ok {
			return name, true
		}
		seen[name] = struct{}{}
	}
	return "", false
}

// healthcheckTransports is the set a healthcheck picks exactly one from,
// in the order the diagnostic lists them.
var healthcheckTransports = []string{"tcp", "http", "exec"}

// exactlyOneTransport enforces that pick. It sits on the healthcheck object
// rather than on one of the three attributes because that is the shape of
// the rule, and because neither attribute-level placement survives contact
// with a real config: objectvalidator.ExactlyOneOf on one attribute reports
// every violation against that attribute however the config went wrong, and
// on all three it reports the same violation three times. Both render the
// set from AtParent() expressions, so the operator is told about
// `container[0].healthcheck[0].tcp.<.http`.
//
// The library validator cannot be moved here as-is: it counts the value it
// is attached to, and would read the healthcheck itself as a fourth member
// of its own set.
type exactlyOneTransport struct{}

func (v exactlyOneTransport) Description(context.Context) string {
	return "exactly one of " + strings.Join(healthcheckTransports, ", ") + " must be set"
}

func (v exactlyOneTransport) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v exactlyOneTransport) ValidateObject(_ context.Context, req validator.ObjectRequest, resp *validator.ObjectResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	attrs := req.ConfigValue.Attributes()
	var set []string
	for _, name := range healthcheckTransports {
		v, ok := attrs[name]
		if !ok || v.IsUnknown() {
			return
		}
		if !v.IsNull() {
			set = append(set, name)
		}
	}

	switch len(set) {
	case 1:
		return
	case 0:
		resp.Diagnostics.AddAttributeError(req.Path,
			"Missing Healthcheck Transport",
			fmt.Sprintf("A healthcheck declares exactly one of %s. This one declares none, so nothing would probe the container.",
				strings.Join(healthcheckTransports, ", ")))
	default:
		resp.Diagnostics.AddAttributeError(req.Path,
			"Conflicting Healthcheck Transports",
			fmt.Sprintf("A healthcheck declares exactly one of %s. This one declares %s and %s.",
				strings.Join(healthcheckTransports, ", "),
				strings.Join(set[:len(set)-1], ", "), set[len(set)-1]))
	}
}

// validateCheckNameUniqueness rejects top-level check blocks that share a
// name. Unlike the container and mount guards, there is no API-level
// rejection to fall back on: the name is a map key, so a duplicate is
// silently dropped and the machine comes up gated on fewer checks than
// the config declares — a check that reads as present and is not there.
func validateCheckNameUniqueness(checks []machineCheckModel) diag.Diagnostics {
	if len(checks) <= 1 {
		return nil
	}
	seen := make(map[string]struct{}, len(checks))
	var diags diag.Diagnostics
	for _, c := range checks {
		if c.Name.IsNull() || c.Name.IsUnknown() {
			continue
		}
		name := c.Name.ValueString()
		if _, ok := seen[name]; ok {
			diags.AddError(
				"Duplicate Check Name",
				fmt.Sprintf("check name %q appears in more than one check block on this machine. Check names are the keys of the Machines API's checks map, so a duplicate would silently replace the earlier block rather than being rejected.", name),
			)
			return diags
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateMountUniqueness rejects mount lists that reference the same
// volume ID more than once within a single machine. Fly would reject
// these at the API level; surfacing here gives a clearer diagnostic.
func validateMountUniqueness(mounts []mountModel) diag.Diagnostics {
	if len(mounts) <= 1 {
		return nil
	}
	seen := make(map[string]string, len(mounts))
	var diags diag.Diagnostics
	for _, m := range mounts {
		if m.Volume.IsNull() || m.Volume.IsUnknown() {
			continue
		}
		id := m.Volume.ValueString()
		if firstPath, ok := seen[id]; ok {
			diags.AddError(
				"Duplicate Volume in Mounts",
				fmt.Sprintf("volume %s is referenced by two mount blocks on the same machine (paths %s and %s). Each fly_volume should be mounted at most once per machine.",
					id, firstPath, m.Path.ValueString()),
			)
			return diags
		}
		seen[id] = m.Path.ValueString()
	}
	return nil
}

// validateMountsUnattached checks that every volume referenced by the
// plan's mounts is currently detached (attached_machine_id == ""). The
// common failure mode this catches: HCL that index-keys volumes against
// machines but typo'd one index, sending two machines at the same
// volume. Fly's CreateMachine would 4xx; this gives a cleaner error
// naming the conflicting machine.
//
// One GetVolume per mount per Create call. The cost is bounded by the
// number of mounts per machine (1-2 in practice for the cluster
// bootstrap), and it only runs once per machine lifecycle.
func (r *machineResource) validateMountsUnattached(ctx context.Context, appName string, mounts []mountModel) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, m := range mounts {
		if m.Volume.IsNull() || m.Volume.IsUnknown() {
			continue
		}
		volumeID := m.Volume.ValueString()
		info, err := r.pd.client.GetVolume(ctx, appName, volumeID)
		if err != nil {
			diags.AddError("Pre-Create Volume Check Failed",
				fmt.Sprintf("could not verify volume %s availability before mount: %s", volumeID, err))
			return diags
		}
		if info == nil {
			diags.AddError("Volume Not Found",
				fmt.Sprintf("volume %s referenced by mount does not exist; check the fly_volume resource was created first", volumeID))
			return diags
		}
		if info.AttachedMachineID != "" {
			diags.AddError("Volume Already Attached",
				fmt.Sprintf("volume %s is already attached to machine %s; each fly_volume should be referenced by at most one fly_machine.mount.volume (likely an HCL bug where count.index was missed for index-keyed volumes).",
					volumeID, info.AttachedMachineID))
			return diags
		}
	}
	return nil
}

func buildMounts(mounts []mountModel) []machines.FlyMachineMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]machines.FlyMachineMount, len(mounts))
	for i, m := range mounts {
		vol := m.Volume.ValueString()
		path := m.Path.ValueString()
		out[i] = machines.FlyMachineMount{
			Volume: &vol,
			Path:   &path,
		}
		if !m.Name.IsNull() && !m.Name.IsUnknown() {
			n := m.Name.ValueString()
			out[i].Name = &n
		}
	}
	return out
}

func buildRestart(r *restartModel) *machines.FlyMachineRestart {
	if r == nil {
		return nil
	}
	out := &machines.FlyMachineRestart{}
	if !r.Policy.IsNull() && !r.Policy.IsUnknown() {
		p := machines.FlyMachineRestartPolicy(r.Policy.ValueString())
		out.Policy = &p
	}
	if !r.MaxRetries.IsNull() && !r.MaxRetries.IsUnknown() {
		v := int(r.MaxRetries.ValueInt64())
		out.MaxRetries = &v
	}
	if out.Policy == nil && out.MaxRetries == nil {
		return nil
	}
	return out
}

func setMachineState(model *machineResourceModel, info *flyio.MachineInfo) {
	model.ID = types.StringValue(info.ID)
	model.Name = types.StringValue(info.Name)
	model.InstanceID = types.StringValue(info.InstanceID)
	model.State = types.StringValue(info.State)
	model.Region = types.StringValue(info.Region)
	model.PrivateIP = types.StringValue(info.PrivateIP)
	if info.ImageRef.Digest != "" {
		model.ImageDigest = types.StringValue(info.ImageRef.Digest)
	}
}

// machineStateStopped is the desired_state value (and Machines API state)
// for a parked machine.
const machineStateStopped = "stopped"

// machineSettleTimeout bounds the wait for a cold machine to come to rest
// after UpdateMachine. The replacing → stopped transition takes seconds;
// 60s is the budget the previous single-target /wait had (Fly's default
// long-poll), kept so the failure mode is unchanged: on breach the update
// errors, tofu keeps the prior state, and the next apply re-sends the same
// config.
const machineSettleTimeout = 60 * time.Second

// updateLaunches reports whether UpdateMachine itself brings the machine
// up in the new config — a running or booting machine reboots into it and
// a failed one is relaunched — so no explicit start follows. Every other
// state is cold and stays cold. Same partition as flyctl's shouldSkipLaunch
// (internal/command/deploy/machines_launchinput.go).
func updateLaunches(state string) bool {
	switch state {
	case "started", "starting", "failed":
		return true
	default:
		return false
	}
}

// cleanupFailedMachine destroys a machine that failed its post-create
// verification, so a create_before_destroy replace preserves the old machine
// and no untracked machine leaks (Create returns no state on error). The
// context is detached because the caller's ctx may already be cancelled. A
// destroy failure is only a warning — the caller still reports the original
// failure that triggered cleanup.
func (r *machineResource) cleanupFailedMachine(diags *diag.Diagnostics, appName, machineID, reason string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.pd.client.DestroyMachine(cleanupCtx, appName, machineID, true); err != nil {
		diags.AddWarning("Cleanup Failed",
			fmt.Sprintf("failed to destroy machine %s after %s: %s", machineID, reason, err))
	}
}

// stopAndWait explicitly stops a machine and waits for it to reach
// "stopped". Used for desired_state convergence; distinct from autostop,
// which is a Fly Proxy idle policy that a service-less machine never gets.
func (r *machineResource) stopAndWait(ctx context.Context, appName, machineID string) error {
	if err := r.pd.client.StopMachine(ctx, appName, machineID); err != nil {
		return fmt.Errorf("stop machine: %w", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, machineSettleTimeout)
	defer cancel()
	if err := r.pd.client.WaitForState(stopCtx, appName, machineID, machineStateStopped, ""); err != nil {
		return fmt.Errorf("wait for stopped: %w", err)
	}
	return nil
}

// parkOnUpdate converges a just-updated machine to "stopped". A machine
// the update launches restarts into the new config (replacing → started),
// so it must settle at "started" and then be explicitly stopped so the
// stop doesn't race the restart. A cold machine
// is left stopped by the update — a suspended one included — so once it
// has settled there is usually nothing more to do; if it came to rest
// anywhere else it gets an explicit stop then, never before the settle,
// so the stop can't race the replacing transition. The explicit stop is
// also the fallback when the settle never arrives. Check/health waits are
// skipped throughout — a parked machine has no services to cover.
func (r *machineResource) parkOnUpdate(ctx context.Context, appName, machineID, preState, instanceID string) error {
	if updateLaunches(preState) {
		startCtx, cancel := context.WithTimeout(ctx, machineStartTimeout)
		defer cancel()
		if err := r.pd.client.WaitForState(startCtx, appName, machineID, "started", instanceID); err != nil {
			return fmt.Errorf("did not restart after update: %w", err)
		}
		return r.stopAndWait(ctx, appName, machineID)
	}

	settleCtx, cancel := context.WithTimeout(ctx, machineSettleTimeout)
	defer cancel()
	switch rest, settleErr := r.pd.client.WaitForRest(settleCtx, appName, machineID, instanceID); {
	case settleErr == nil && rest == machineStateStopped:
		return nil
	case settleErr == nil:
		return r.stopAndWait(ctx, appName, machineID)
	default:
		if err := r.stopAndWait(ctx, appName, machineID); err != nil {
			return fmt.Errorf("did not settle after update (%v) and explicit stop failed: %w", settleErr, err)
		}
		return nil
	}
}

// buildConcurrency maps a service's concurrency block to the wire type.
// The block is a size-1 list, so an empty slice is the "not set" state and
// sends no `concurrency` field at all (not a zeroed one). The three
// attributes are Required within the block, so they are always known when the
// block is present — set them directly with no IsNull guards.
func buildConcurrency(cs []concurrencyModel) *machines.FlyMachineServiceConcurrency {
	if len(cs) == 0 {
		return nil
	}
	c := cs[0]
	t := c.Type.ValueString()
	soft := int(c.SoftLimit.ValueInt64())
	hard := int(c.HardLimit.ValueInt64())
	return &machines.FlyMachineServiceConcurrency{
		Type:      &t,
		SoftLimit: &soft,
		HardLimit: &hard,
	}
}

// buildChecks renders the top-level `check` blocks into the API's checks
// map. Returns nil for an empty list so the field is omitted rather than
// sent as an empty map — Update replaces the whole config, and an empty
// map would read as "remove every check".
func buildChecks(checks []machineCheckModel) map[string]machines.FlyMachineCheck {
	if len(checks) == 0 {
		return nil
	}
	result := make(map[string]machines.FlyMachineCheck, len(checks))
	for _, chk := range checks {
		c := machines.FlyMachineCheck{}
		if !chk.Type.IsNull() {
			t := chk.Type.ValueString()
			c.Type = &t
		}
		if !chk.Port.IsNull() {
			p := int(chk.Port.ValueInt64())
			c.Port = &p
		}
		if !chk.Path.IsNull() {
			p := chk.Path.ValueString()
			c.Path = &p
		}
		if !chk.Method.IsNull() {
			m := chk.Method.ValueString()
			c.Method = &m
		}
		c.Interval = strPtr(chk.Interval)
		c.Timeout = strPtr(chk.Timeout)
		c.GracePeriod = strPtr(chk.GracePeriod)
		result[chk.Name.ValueString()] = c
	}
	return result
}

func buildServices(services []serviceModel) []machines.FlyMachineService {
	if len(services) == 0 {
		return nil
	}
	result := make([]machines.FlyMachineService, len(services))
	for i, svc := range services {
		s := machines.FlyMachineService{}
		if !svc.InternalPort.IsNull() {
			port := int(svc.InternalPort.ValueInt64())
			s.InternalPort = &port
		}
		if !svc.Protocol.IsNull() {
			proto := svc.Protocol.ValueString()
			s.Protocol = &proto
		}
		if !svc.Autostart.IsNull() {
			v := svc.Autostart.ValueBool()
			s.Autostart = &v
		}
		if !svc.Autostop.IsNull() {
			v := machines.FlyMachineServiceAutostop(svc.Autostop.ValueString())
			s.Autostop = &v
		}
		s.Concurrency = buildConcurrency(svc.Concurrency)
		for _, p := range svc.Port {
			mp := machines.FlyMachinePort{}
			if !p.Port.IsNull() {
				v := int(p.Port.ValueInt64())
				mp.Port = &v
			}
			if !p.Handlers.IsNull() {
				elems := p.Handlers.Elements()
				handlers := make([]string, len(elems))
				for j, e := range elems {
					handlers[j] = e.(types.String).ValueString()
				}
				mp.Handlers = handlers
			}
			s.Ports = append(s.Ports, mp)
		}
		for _, chk := range svc.Check {
			c := machines.FlyMachineServiceCheck{}
			if !chk.Type.IsNull() {
				t := chk.Type.ValueString()
				c.Type = &t
			}
			if !chk.Port.IsNull() {
				p := int(chk.Port.ValueInt64())
				c.Port = &p
			}
			if !chk.Path.IsNull() {
				p := chk.Path.ValueString()
				c.Path = &p
			}
			c.Interval = strPtr(chk.Interval)
			c.Timeout = strPtr(chk.Timeout)
			if !chk.Method.IsNull() {
				m := chk.Method.ValueString()
				c.Method = &m
			}
			s.Checks = append(s.Checks, c)
		}
		result[i] = s
	}
	return result
}

// pollHealth polls a health endpoint with exponential backoff after a
// machine is created until it returns 200 or the timeout expires.
func (r *machineResource) pollHealth(ctx context.Context, plan *machineResourceModel) error {
	url := plan.HealthCheckURL.ValueString()

	timeout := 60 * time.Second
	if !plan.HealthCheckTimeout.IsNull() && !plan.HealthCheckTimeout.IsUnknown() {
		d, err := time.ParseDuration(plan.HealthCheckTimeout.ValueString())
		if err != nil {
			return fmt.Errorf("invalid health_check_timeout: %w", err)
		}
		timeout = d
	}

	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond
	const maxBackoff = 5 * time.Second

	httpClient := &http.Client{Timeout: 5 * time.Second}

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s polling %s", timeout, url)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build health check request: %w", err)
		}

		resp, err := httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
