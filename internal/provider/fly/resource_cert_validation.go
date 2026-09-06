package fly

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource              = (*certValidationResource)(nil)
	_ resource.ResourceWithConfigure = (*certValidationResource)(nil)
)

// defaultCertValidationTimeout is the deadline a fly_cert_validation
// resource waits for the underlying cert to reach configured=true.
// 10 minutes covers the common case (DNS propagation + ACME challenge
// round-trip) with margin; cases that need longer can override via the
// `validation_timeout` attribute.
const defaultCertValidationTimeout = 10 * time.Minute

type certValidationResource struct {
	pd *providerData
}

type certValidationResourceModel struct {
	ID                     types.String `tfsdk:"id"`
	CertID                 types.String `tfsdk:"cert_id"`
	ValidationDependencies types.Set    `tfsdk:"validation_dependencies"`
	ValidationTimeout      types.String `tfsdk:"validation_timeout"`
	Configured             types.Bool   `tfsdk:"configured"`
	Status                 types.String `tfsdk:"status"`
}

func NewCertValidationResource() resource.Resource {
	return &certValidationResource{}
}

func (r *certValidationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cert_validation"
}

func (r *certValidationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Waits for a `fly_cert` to reach `configured = true` and records that fact in Terraform state. Mirrors the `aws_acm_certificate` + `aws_acm_certificate_validation` two-resource pattern so the same apply that creates the cert and the DNS records also gates downstream resources on the cert actually being valid.\n\n" +
			"`Create` polls the Fly API (with a one-shot `CheckCertificate` bump to nudge Fly's validator) until the cert is configured or `validation_timeout` elapses. `Delete` is a no-op — this resource is a state gate, not a cloud resource; the actual cert is owned by `fly_cert`.\n\n" +
			"### Why this resource exists\n\n" +
			"Without it, `fly_cert.app` returns from Create the instant Fly accepts the ACME request — before DNS propagates and before validation succeeds. `tofu state show fly_cert.app` would show `configured = false` indefinitely until the next refresh cycle, and downstream resources (machine deploys, app smoke tests) couldn't gate on the cert being live. This resource fixes that by making the wait a first-class state transition.\n\n" +
			"### Example\n\n" +
			"```hcl\n" +
			"resource \"fly_cert\" \"app\" {\n" +
			"  app      = fly_app.app.name\n" +
			"  hostname = \"staging.example.com\"\n" +
			"}\n\n" +
			"resource \"cloudflare_dns_record\" \"app\" {\n" +
			"  zone_id = var.cloudflare_zone_id\n" +
			"  name    = \"staging\"\n" +
			"  type    = \"CNAME\"\n" +
			"  content = fly_cert.app.cname\n" +
			"  ttl     = 60\n" +
			"  proxied = false\n" +
			"}\n\n" +
			"resource \"fly_cert_validation\" \"app\" {\n" +
			"  cert_id = fly_cert.app.id\n" +
			"  validation_dependencies = [cloudflare_dns_record.app.id]\n" +
			"}\n" +
			"```",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Identifier — mirrors `cert_id`.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"cert_id": schema.StringAttribute{
				Description: "The `id` of the `fly_cert` resource to wait on (format: `<app>/<hostname>`).",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"validation_dependencies": schema.SetAttribute{
				Description: "Opaque set of strings whose only purpose is to express a DAG dependency on resources that must exist before validation can succeed (typically Cloudflare DNS record IDs). Changing the set replaces this resource and re-runs the wait — mirrors `aws_acm_certificate_validation.validation_record_fqdns`.",
				Optional:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.Set{
					setplanmodifier.RequiresReplace(),
				},
			},
			"validation_timeout": schema.StringAttribute{
				Description: "Maximum time to wait for the cert to reach `configured = true`. Go duration string (e.g. `\"10m\"`, `\"30m\"`); defaults to 10 minutes. Set higher for hostnames behind slow DNS providers or for apex hostnames where multiple A/AAAA records must all propagate.",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// Computed cert state captured at validation time. Read does
			// NOT refresh these — flapping cert configured-state would
			// otherwise show up as constant TF drift on this resource,
			// when the natural place to observe such drift is on
			// fly_cert.app.configured. This matches the
			// aws_acm_certificate_validation pattern: one-shot gate, not
			// a continuous health check.
			"configured": schema.BoolAttribute{
				Description: "True once the cert reached configured state. Captured at validation time; the underlying cert's current configured-state is on `fly_cert.<name>.configured`.",
				Computed:    true,
			},
			"status": schema.StringAttribute{
				Description: "Fly cert status at the moment validation completed.",
				Computed:    true,
			},
		},
	}
}

func (r *certValidationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *certValidationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan certValidationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	app, hostname, ok := splitCertID(plan.CertID.ValueString())
	if !ok {
		resp.Diagnostics.AddAttributeError(
			path.Root("cert_id"),
			"Invalid cert_id Format",
			fmt.Sprintf("cert_id must be `<app>/<hostname>` (the `id` attribute of a fly_cert resource), got %q", plan.CertID.ValueString()),
		)
		return
	}

	timeout := defaultCertValidationTimeout
	if !plan.ValidationTimeout.IsNull() && !plan.ValidationTimeout.IsUnknown() {
		d, err := time.ParseDuration(plan.ValidationTimeout.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("validation_timeout"),
				"Invalid validation_timeout",
				fmt.Sprintf("expected Go duration string (e.g. \"10m\"), got %q: %s", plan.ValidationTimeout.ValueString(), err),
			)
			return
		}
		timeout = d
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	info, err := r.pd.client.WaitForCertificateConfigured(waitCtx, app, hostname)
	if err != nil {
		resp.Diagnostics.AddError("Cert Validation Did Not Complete",
			fmt.Sprintf("cert %s/%s did not reach configured=true within %s: %s. Check that the DNS records referenced by fly_cert.<name>.cname / a_records / aaaa_records / acme_challenge_target are correctly created and have propagated.",
				app, hostname, timeout, err))
		return
	}

	plan.ID = plan.CertID
	plan.Configured = types.BoolValue(info.Configured)
	plan.Status = types.StringValue(info.Status)
	plan.ValidationTimeout = types.StringValue(timeout.String())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *certValidationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state certValidationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Confirm the underlying cert still exists. If it was deleted out of
	// band, this resource is meaningless — drop from state so the next
	// apply re-creates the cert and re-runs validation. We deliberately
	// do NOT refresh `configured` / `status` here: a transient
	// configured=false on the cert (e.g. DNS hiccup) would otherwise
	// flap this resource on every refresh. Drift on those values is
	// owned by fly_cert.<name>.
	app, hostname, ok := splitCertID(state.CertID.ValueString())
	if !ok {
		resp.State.RemoveResource(ctx)
		return
	}
	info, err := r.pd.client.GetCertificate(ctx, app, hostname)
	if err != nil {
		resp.Diagnostics.AddError("Read Cert Error", err.Error())
		return
	}
	if info == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *certValidationResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All settable attributes are RequiresReplace; Update is never called.
	resp.Diagnostics.AddError("Update Not Supported", "fly_cert_validation is replace-only")
}

func (r *certValidationResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// No-op: this resource is a state gate, not a cloud resource. The
	// underlying cert is owned by fly_cert.<name>.Delete.
}

// splitCertID parses an `<app>/<hostname>` cert ID into its parts.
// Uses strings.Cut — added in Go 1.18 specifically to replace the
// strings.SplitN(s, sep, 2) pattern. Returns ok=false on missing
// separator or empty halves so callers can produce a clean error
// diagnostic instead of partial state.
func splitCertID(id string) (app, hostname string, ok bool) {
	app, hostname, ok = strings.Cut(id, "/")
	if !ok || app == "" || hostname == "" {
		return "", "", false
	}
	return app, hostname, true
}
