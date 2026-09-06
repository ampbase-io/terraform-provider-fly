package fly

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

var (
	_ resource.Resource                = (*appResource)(nil)
	_ resource.ResourceWithConfigure   = (*appResource)(nil)
	_ resource.ResourceWithImportState = (*appResource)(nil)
)

type appResource struct {
	pd *providerData
}

type appResourceModel struct {
	Name    types.String `tfsdk:"name"`
	Org     types.String `tfsdk:"org"`
	Network types.String `tfsdk:"network"`
	ID      types.String `tfsdk:"id"`
}

func NewAppResource() resource.Resource {
	return &appResource{}
}

func (r *appResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app"
}

func (r *appResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Fly.io application.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Description: "Application name. Must be globally unique.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			// Non-empty when set: an explicit "" would be indistinguishable
			// from unset and fall back to the provider's org silently, which
			// is the shape of mistake this attribute's Create path exists to
			// rule out.
			"org": schema.StringAttribute{
				Description: "Organization slug. Defaults to the provider's org_slug.",
				Optional:    true,
				Computed:    true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"network": schema.StringAttribute{
				Description: "Per-app private network (e.g. \"vault-net\"). Empty/unset places the app on the org's default network. Set at create time only.",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"id": schema.StringAttribute{
				Description: "Application ID.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *appResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *appResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan appResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	// The configured org has to reach the create call, not just state: an
	// org recorded in state but never sent would be read back as the
	// provider's org on the next refresh, and the RequiresReplace on `org`
	// would then recreate the app in the wrong org on every apply.
	org := r.pd.orgSlug
	if !plan.Org.IsNull() && !plan.Org.IsUnknown() {
		org = plan.Org.ValueString()
	}
	appName := plan.Name.ValueString()
	err := client.CreateApp(ctx, flyio.CreateAppInput{
		Name:    appName,
		Org:     org,
		Network: plan.Network.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Create App Error", err.Error())
		return
	}

	// Read back to get the ID.
	app, err := client.GetApp(ctx, appName)
	if err != nil {
		resp.Diagnostics.AddError("Read App Error", err.Error())
		return
	}
	if app == nil {
		resp.Diagnostics.AddError("Read App Error", "app not found after creation")
		return
	}

	plan.ID = types.StringValue(app.ID)
	plan.Org = types.StringValue(app.OrgSlug)
	plan.Network = types.StringValue(app.Network)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *appResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state appResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	app, err := client.GetApp(ctx, state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read App Error", err.Error())
		return
	}
	if app == nil {
		// App was deleted outside of Terraform.
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(app.ID)
	state.Org = types.StringValue(app.OrgSlug)
	state.Network = types.StringValue(app.Network)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *appResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

func (r *appResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All attributes require replace; Update is never called.
	resp.Diagnostics.AddError("Update Not Supported", "fly_app does not support in-place updates")
}

func (r *appResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state appResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client

	if err := client.DeleteApp(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete App Error", err.Error())
	}
}
