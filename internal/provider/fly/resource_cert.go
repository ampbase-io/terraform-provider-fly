package fly

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

var (
	_ resource.Resource                = (*certResource)(nil)
	_ resource.ResourceWithConfigure   = (*certResource)(nil)
	_ resource.ResourceWithImportState = (*certResource)(nil)
)

type certResource struct {
	pd *providerData
}

type certResourceModel struct {
	ID       types.String `tfsdk:"id"`
	App      types.String `tfsdk:"app"`
	Hostname types.String `tfsdk:"hostname"`

	// Computed — surfaced so a downstream DNS module (Cloudflare /
	// integrations/github) can wire records without a second tofu apply.
	Configured          types.Bool   `tfsdk:"configured"`
	Status              types.String `tfsdk:"status"`
	DNSProvider         types.String `tfsdk:"dns_provider"`
	CNAME               types.String `tfsdk:"cname"`
	ARecords            types.List   `tfsdk:"a_records"`
	AAAARecords         types.List   `tfsdk:"aaaa_records"`
	AcmeChallengeName   types.String `tfsdk:"acme_challenge_name"`
	AcmeChallengeTarget types.String `tfsdk:"acme_challenge_target"`
	OwnershipName       types.String `tfsdk:"ownership_name"`
	OwnershipAppValue   types.String `tfsdk:"ownership_app_value"`
	DnsConfigured       types.Bool   `tfsdk:"dns_configured"`
	HttpConfigured      types.Bool   `tfsdk:"http_configured"`
	AlpnConfigured      types.Bool   `tfsdk:"alpn_configured"`
	RateLimitedUntil    types.String `tfsdk:"rate_limited_until"`
}

func NewCertResource() resource.Resource {
	return &certResource{}
}

func (r *certResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cert"
}

func (r *certResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Fly.io ACME (Let's Encrypt) certificate for a custom hostname.\n\n" +
			"Cert creation is async — the cert is in a pending state until DNS propagates and the ACME challenge succeeds. **`fly_cert.Create` returns immediately after the API call** rather than waiting for `configured = true`, because the same `tofu apply` is typically what creates the Cloudflare DNS records the cert validation depends on. To make `tofu apply` block until the cert is actually live (and record that fact in state so downstream resources can gate on it), pair this resource with `fly_cert_validation` — the AWS-ACM-style two-resource pattern. Without the validation resource, `configured` will refresh from `false` to `true` on a subsequent `tofu refresh` once DNS settles, but the current apply will finish before that happens.\n\n" +
			"### DNS records to create\n\n" +
			"Different hostnames need different records; subscribe to the computed attributes appropriate to your case:\n" +
			"  * **Subdomain** (e.g. `staging.example.com`): single `CNAME` to `cname` (typically `<app>.fly.dev`).\n" +
			"  * **Apex** (e.g. `example.com`): `A` records to `a_records[*]` and `AAAA` records to `aaaa_records[*]`. CNAME at the apex is not RFC-valid.\n" +
			"  * **Wildcard** (e.g. `*.example.com`): a `_acme-challenge.example.com` CNAME pointing at `acme_challenge_target` is **mandatory** — wildcards require DNS-01 validation, no HTTP-01 fallback.\n" +
			"  * **Ownership verification** (when Fly requires it): a `_fly-ownership.<hostname>` TXT record with `ownership_app_value`. Surfaces empty when Fly doesn't need it.\n\n" +
			"### Let's Encrypt rate limits\n\n" +
			"Per the docs: 50 certs per registered domain per week, 5 duplicate certs per week, 5 failed validations per hostname per hour. Avoid churning `tofu apply` against the same hostname during DNS debugging — each create attempt counts.\n\n" +
			"### Example (full three-resource flow)\n\n" +
			"```hcl\n" +
			"resource \"fly_cert\" \"app\" {\n" +
			"  app      = fly_app.app.name\n" +
			"  hostname = \"staging.example.com\"\n" +
			"}\n\n" +
			"resource \"cloudflare_dns_record\" \"app_cname\" {\n" +
			"  zone_id = var.cloudflare_zone_id\n" +
			"  name    = \"staging\"\n" +
			"  type    = \"CNAME\"\n" +
			"  content = fly_cert.app.cname\n" +
			"  ttl     = 60\n" +
			"  proxied = false\n" +
			"}\n\n" +
			"# Gates the apply on the cert actually being live. Drop this\n" +
			"# resource if you don't care about state matching reality.\n" +
			"resource \"fly_cert_validation\" \"app\" {\n" +
			"  cert_id                 = fly_cert.app.id\n" +
			"  validation_dependencies = [cloudflare_dns_record.app_cname.id]\n" +
			"}\n" +
			"```",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Identifier: `<app>/<hostname>`.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app": schema.StringAttribute{
				Description: "Application name the cert belongs to.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"hostname": schema.StringAttribute{
				Description: "Hostname the cert is issued for (e.g. \"staging.example.com\"). Wildcard certs (`*.example.com`) are supported when DNS-01 ownership verification is configured.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},

			// Computed cert state.
			"configured": schema.BoolAttribute{
				Description: "True once Fly has validated the cert and it's serving traffic. Will be false on first apply (DNS hasn't propagated yet); re-run `tofu refresh` after DNS settles.",
				Computed:    true,
			},
			"status": schema.StringAttribute{
				Description: "Fly's cert status string (e.g. \"awaiting_configuration\", \"ready\").",
				Computed:    true,
			},
			"dns_provider": schema.StringAttribute{
				Description: "DNS provider Fly detected for the hostname (e.g. \"cloudflare\").",
				Computed:    true,
			},

			// DNS requirements — surfaced individually so a downstream
			// DNS module can wire records without unpacking nested objects.
			"cname": schema.StringAttribute{
				Description: "CNAME target the hostname should point to (typically `<app>.fly.dev`). Empty for apex hostnames — use `a_records` / `aaaa_records` instead since CNAMEs at the apex aren't RFC-valid.",
				Computed:    true,
			},
			"a_records": schema.ListAttribute{
				Description: "IPv4 addresses the apex A records should point at. Surfaces Fly's anycast IPs for the app. Empty for non-apex hostnames (use `cname` instead).",
				Computed:    true,
				ElementType: types.StringType,
			},
			"aaaa_records": schema.ListAttribute{
				Description: "IPv6 addresses the apex AAAA records should point at. Surfaces Fly's anycast IPs for the app. Empty for non-apex hostnames (use `cname` instead).",
				Computed:    true,
				ElementType: types.StringType,
			},
			"acme_challenge_name": schema.StringAttribute{
				Description: "Hostname for the ACME DNS-01 challenge record (typically `_acme-challenge.<hostname>`).",
				Computed:    true,
			},
			"acme_challenge_target": schema.StringAttribute{
				Description: "Target value for the ACME DNS-01 challenge CNAME.",
				Computed:    true,
			},
			"ownership_name": schema.StringAttribute{
				Description: "Hostname for the ownership-verification TXT record (when Fly requires one).",
				Computed:    true,
			},
			"ownership_app_value": schema.StringAttribute{
				Description: "Value for the app-level ownership-verification TXT record.",
				Computed:    true,
			},

			// Validation state.
			"dns_configured": schema.BoolAttribute{
				Description: "True once DNS records satisfy Fly's validation.",
				Computed:    true,
			},
			"http_configured": schema.BoolAttribute{
				Description: "True once HTTP-01 ACME validation succeeds.",
				Computed:    true,
			},
			"alpn_configured": schema.BoolAttribute{
				Description: "True once TLS-ALPN-01 ACME validation succeeds.",
				Computed:    true,
			},
			"rate_limited_until": schema.StringAttribute{
				Description: "RFC 3339 timestamp set by Fly when a Let's Encrypt rate limit blocks new cert requests for this hostname. **Non-empty means rate-limited** — the value is the earliest time a retry would succeed. Empty means no rate limit is active. See the rate-limit warning in the resource description.",
				Computed:    true,
			},
		},
	}
}

func (r *certResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *certResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan certResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	appName := plan.App.ValueString()
	hostname := plan.Hostname.ValueString()

	info, err := r.pd.client.CreateAcmeCertificate(ctx, appName, hostname)
	if err != nil {
		resp.Diagnostics.AddError("Create Certificate Error", err.Error())
		return
	}

	plan.ID = types.StringValue(appName + "/" + hostname)
	resp.Diagnostics.Append(setCertState(ctx, &plan, info)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *certResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state certResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	info, err := r.pd.client.GetCertificate(ctx, state.App.ValueString(), state.Hostname.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read Certificate Error", err.Error())
		return
	}
	if info == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(setCertState(ctx, &state, info)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *certResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All settable attributes (app, hostname) are RequiresReplace; Update is never called.
	resp.Diagnostics.AddError("Update Not Supported", "fly_cert does not support in-place updates; both app and hostname require replacement")
}

func (r *certResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state certResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.pd.client.DeleteCertificate(ctx, state.App.ValueString(), state.Hostname.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete Certificate Error", err.Error())
	}
}

func (r *certResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	app, hostname, ok := strings.Cut(req.ID, "/")
	if !ok || app == "" || hostname == "" {
		resp.Diagnostics.AddError("Invalid Import ID",
			"Expected '<app>/<hostname>', got "+req.ID)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app"), app)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("hostname"), hostname)...)
}

func setCertState(ctx context.Context, m *certResourceModel, info *flyio.CertificateInfo) diag.Diagnostics {
	m.Hostname = types.StringValue(info.Hostname)
	m.Configured = types.BoolValue(info.Configured)
	m.Status = types.StringValue(info.Status)
	m.DNSProvider = types.StringValue(info.DNSProvider)
	m.CNAME = types.StringValue(info.CNAME)
	m.AcmeChallengeName = types.StringValue(info.AcmeChallenge.Name)
	m.AcmeChallengeTarget = types.StringValue(info.AcmeChallenge.Target)
	m.OwnershipName = types.StringValue(info.Ownership.Name)
	m.OwnershipAppValue = types.StringValue(info.Ownership.AppValue)
	m.DnsConfigured = types.BoolValue(info.DnsConfigured)
	m.HttpConfigured = types.BoolValue(info.HttpConfigured)
	m.AlpnConfigured = types.BoolValue(info.AlpnConfigured)
	m.RateLimitedUntil = types.StringValue(info.RateLimitedUntil)

	var diags diag.Diagnostics
	aList, d := types.ListValueFrom(ctx, types.StringType, info.ARecords)
	diags.Append(d...)
	if d.HasError() {
		return diags
	}
	m.ARecords = aList

	aaaaList, d := types.ListValueFrom(ctx, types.StringType, info.AAAARecords)
	diags.Append(d...)
	if d.HasError() {
		return diags
	}
	m.AAAARecords = aaaaList
	return diags
}
