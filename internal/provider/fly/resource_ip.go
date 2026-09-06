package fly

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*ipResource)(nil)
	_ resource.ResourceWithConfigure      = (*ipResource)(nil)
	_ resource.ResourceWithValidateConfig = (*ipResource)(nil)
	_ resource.ResourceWithImportState    = (*ipResource)(nil)
)

type ipResource struct {
	pd *providerData
}

type ipResourceModel struct {
	ID      types.String `tfsdk:"id"`
	App     types.String `tfsdk:"app"`
	Type    types.String `tfsdk:"type"`
	Network types.String `tfsdk:"network"`
	OrgSlug types.String `tfsdk:"org_slug"`
	Address types.String `tfsdk:"address"`
}

func NewIPResource() resource.Resource {
	return &ipResource{}
}

func (r *ipResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ip"
}

func (r *ipResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Fly.io IP assignment. Supports both private Flycast IPs and public IPs (dedicated v4, shared v4, public v6) so an environment module can stamp out both internal cross-app routing and public app surface in one apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "IP assignment identifier (the IP address).",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app": schema.StringAttribute{
				Description: "Application name.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"type": schema.StringAttribute{
				Description: "IP type:\n" +
					"  * `private_v6` — Flycast (internal IPv6, reachable from other apps in the same org or via cross-org peering). Free.\n" +
					"  * `shared_v4` — shared public IPv4 (Anycast). Free. **Auto-allocated** by Fly when an app is first deployed with public services configured in `fly.toml`; explicitly declaring this resource is only needed when provisioning machines via the Machines API (without a `fly.toml` deploy).\n" +
					"  * `public_v4` — dedicated public IPv4. **Billed monthly** per the Fly pricing page.\n" +
					"  * `public_v6` — public IPv6 (Anycast). Free. **Auto-allocated** for public-service apps; same caveat as `shared_v4` when using the Machines API directly.\n\n" +
					"`network` and `org_slug` only apply to `private_v6` and are validated against the chosen type in `ValidateConfig`.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.OneOf("private_v6", "shared_v4", "public_v4", "public_v6"),
				},
			},
			"network": schema.StringAttribute{
				Description: "Target per-app private network (e.g. \"internal-net\"). Empty/unset uses the app's default network. Only meaningful when `type = \"private_v6\"`.",
				Optional:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"org_slug": schema.StringAttribute{
				Description: "Peer Fly org granted reach to this IP (cross-org Flycast). Empty/unset is same-org only. Only meaningful when `type = \"private_v6\"`.",
				Optional:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"address": schema.StringAttribute{
				Description: "Allocated IP address.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

// ValidateConfig surfaces network/org_slug-vs-type mismatches at plan time
// so a misconfigured public IP (e.g. `type = "public_v4"` with a network
// set) fails on `tofu plan` instead of silently producing surprising
// behavior at apply when the Fly API ignores those fields. The check
// logic is in validateIPTypeFieldCompatibility for direct unit testing
// without the tfsdk.Config plumbing.
func (r *ipResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config ipResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.Type.IsNull() || config.Type.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(validateIPTypeFieldCompatibility(
		config.Type.ValueString(),
		!config.Network.IsNull() && !config.Network.IsUnknown(),
		!config.OrgSlug.IsNull() && !config.OrgSlug.IsUnknown(),
	)...)
}

// validateIPTypeFieldCompatibility is the pure-function core of
// ValidateConfig — rejects network / org_slug values when paired with
// any non-private_v6 type. Extracted so tests can drive the logic
// without tfsdk.Config construction.
func validateIPTypeFieldCompatibility(ipType string, networkSet, orgSlugSet bool) diag.Diagnostics {
	if ipType == "private_v6" {
		return nil
	}
	var diags diag.Diagnostics
	if networkSet {
		diags.AddAttributeError(
			path.Root("network"),
			"network Only Applies to Flycast",
			fmt.Sprintf("type = %q does not accept a `network` value (Fly silently ignores it). Drop the attribute or change type to \"private_v6\".", ipType),
		)
	}
	if orgSlugSet {
		diags.AddAttributeError(
			path.Root("org_slug"),
			"org_slug Only Applies to Flycast",
			fmt.Sprintf("type = %q does not accept a peer `org_slug` (cross-org reach is a Flycast concept). Drop the attribute or change type to \"private_v6\".", ipType),
		)
	}
	return diags
}

func (r *ipResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *ipResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ipResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	appName := plan.App.ValueString()
	addr, err := client.AllocateIP(ctx, appName, plan.Type.ValueString(), plan.Network.ValueString(), plan.OrgSlug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Allocate IP Error", err.Error())
		return
	}

	plan.ID = types.StringValue(addr)
	plan.Address = types.StringValue(addr)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ipResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ipResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	ips, err := client.ListIPAssignments(ctx, state.App.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("List IPs Error", err.Error())
		return
	}

	addr := state.Address.ValueString()
	for _, ip := range ips {
		if ip.IP == addr {
			return // Still exists, state unchanged.
		}
	}

	// IP was deleted outside of Terraform.
	resp.State.RemoveResource(ctx)
}

func (r *ipResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Update Not Supported", "fly_ip does not support in-place updates")
}

// ImportState parses a "<app>/<address>" ID and seeds state with both
// fields plus the inferred `type` so a subsequent Read can refresh the
// rest. `type` is required at plan time (RequiresReplace), so we infer
// it from the address shape: IPv4 (contains "." but not ":") maps to
// `public_v4` (the operator can rewrite to `shared_v4` after import if
// needed — Fly distinguishes only at allocation time); IPv6 with the
// flycast prefix (fdaa:) maps to `private_v6`; other IPv6 maps to
// `public_v6`. Wrong inference forces a replace on first plan, which
// is a recoverable diagnostic, not data loss.
func (r *ipResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	app, addr, ok := strings.Cut(req.ID, "/")
	if !ok || app == "" || addr == "" {
		resp.Diagnostics.AddError("Invalid Import ID",
			"Expected '<app>/<ip_address>', got "+req.ID)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app"), app)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("address"), addr)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), addr)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type"), inferIPType(addr))...)
}

// inferIPType maps an IP address to one of the four supported `type`
// enum values for import state seeding. The heuristic is best-effort:
// the API doesn't distinguish dedicated vs shared v4 in the listing
// response, so we default v4 to `public_v4` and let the operator
// correct after import if the IP is actually shared.
func inferIPType(addr string) string {
	switch {
	case strings.Contains(addr, ":") && strings.HasPrefix(addr, "fdaa:"):
		return "private_v6"
	case strings.Contains(addr, ":"):
		return "public_v6"
	default:
		return "public_v4"
	}
}

func (r *ipResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ipResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	if err := client.DeleteIPAssignment(ctx, state.App.ValueString(), state.Address.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete IP Error", err.Error())
	}
}
