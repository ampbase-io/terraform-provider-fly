package fly

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

var (
	_ resource.Resource                 = (*secretResource)(nil)
	_ resource.ResourceWithConfigure    = (*secretResource)(nil)
	_ resource.ResourceWithUpgradeState = (*secretResource)(nil)
)

type secretResource struct {
	pd *providerData
}

type secretResourceModel struct {
	ID             types.String `tfsdk:"id"`
	App            types.String `tfsdk:"app"`
	Name           types.String `tfsdk:"name"`
	ValueWO        types.String `tfsdk:"value_wo"`
	ValueWOVersion types.String `tfsdk:"value_wo_version"`
	Digest         types.String `tfsdk:"digest"`
	Version        types.Int64  `tfsdk:"version"`
}

func NewSecretResource() resource.Resource {
	return &secretResource{}
}

func (r *secretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// Version 1: `value` (persisted, sensitive) became `value_wo`
		// (write-only) plus `value_wo_version`. UpgradeState drops the
		// persisted value from version-0 states.
		Version: 1,
		Description: "Manages a single Fly.io app secret.\n\n" +
			"Fly never returns a secret's value, only a digest, so the provider cannot tell from the API whether the value it holds is current. " +
			"The value is therefore write-only — never held in plan or state — and `value_wo_version` is the rotation trigger: change it whenever `value_wo` changes, and the secret is re-sent in place. " +
			"A changed `value_wo` with an unchanged `value_wo_version` plans no change and Fly keeps the old value.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Secret identifier (app/name).",
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
			"name": schema.StringAttribute{
				Description: "Secret name (e.g. AWS_ACCESS_KEY_ID).",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			// Write-only, so the framework nulls it in every planned state
			// and every state it returns: a changed value alone can never
			// plan a diff, which is why value_wo_version exists and is
			// Required rather than Optional — a config that omits the
			// trigger would silently never rotate.
			"value_wo": schema.StringAttribute{
				Description: "Secret value. Write-only: sent to Fly on create and on every change of `value_wo_version`, and never stored in plan or state.",
				Required:    true,
				Sensitive:   true,
				WriteOnly:   true,
			},
			"value_wo_version": schema.StringAttribute{
				Description: "Rotation trigger for `value_wo`. Any string; a change plans an in-place update that re-sends the value. " +
					"It is held in plain state, so use a non-secret marker that moves with the value where one exists (an access key ID beside its secret, the value itself when it is not secret), " +
					"and a hash such as `sha256(var.token)` only for a machine-generated value that cannot be guessed from its hash.",
				Required: true,
			},
			// No UseStateForUnknown: a rotation changes the digest Fly
			// reports, and a plan that pinned the prior digest as known would
			// make every rotation an inconsistent result after apply.
			"digest": schema.StringAttribute{
				Description: "Digest of the secret value as reported by the API.",
				Computed:    true,
			},
			"version": schema.Int64Attribute{
				Description: "App-wide secrets version after this secret was set. Reference from fly_machine.min_secrets_version to activate staged secrets on a new machine.",
				Computed:    true,
			},
		},
	}
}

// UpgradeState migrates version-0 states, which carried the secret under a
// persisted `value` attribute, to the current shape.
func (r *secretResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: {StateUpgrader: r.upgradeStateV0},
	}
}

// upgradeStateV0 works on the raw state JSON because the version-0 schema is
// gone: it held `value`, which no longer exists. The value is dropped rather
// than carried anywhere, and both new attributes start null — so the first
// plan after the upgrade updates the secret once against whatever the config
// says, in place, the same one-time re-send the persisted shape's own upgrade
// performed. State written while `value` was write-only holds it null and
// takes the same path.
func (r *secretResource) upgradeStateV0(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
	if req.RawState == nil || len(req.RawState.JSON) == 0 {
		resp.Diagnostics.AddError(
			"Missing Raw State for fly_secret Upgrade",
			"The prior state was not available in JSON form, so the version 0 -> 1 migration cannot run.",
		)
		return
	}

	upgraded, err := dropPersistedValue(req.RawState.JSON)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_secret State",
			fmt.Sprintf("Rewriting the version 0 state failed: %s", err),
		)
		return
	}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	typ := schemaResp.Schema.Type().TerraformType(ctx)
	val, err := (tfprotov6.RawState{JSON: upgraded}).Unmarshal(typ)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_secret State",
			fmt.Sprintf("The migrated state does not match the current resource schema: %s", err),
		)
		return
	}
	dv, err := tfprotov6.NewDynamicValue(typ, val)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Upgrade fly_secret State",
			fmt.Sprintf("Re-encoding the migrated state failed: %s", err),
		)
		return
	}
	resp.DynamicValue = &dv
}

// dropPersistedValue removes `value` from a version-0 state object and adds
// the two version-1 attributes as null, leaving every other attribute's bytes
// untouched.
func dropPersistedValue(stateJSON []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return nil, fmt.Errorf("decode state object: %w", err)
	}
	delete(state, "value")
	state["value_wo"] = json.RawMessage("null")
	state["value_wo_version"] = json.RawMessage("null")
	return json.Marshal(state)
}

func (r *secretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// configValue reads the write-only value, which the framework strips from
// the plan and only ever hands over in the config.
func configValue(ctx context.Context, cfg tfsdk.Config) (string, bool) {
	var value types.String
	if diags := cfg.GetAttribute(ctx, path.Root("value_wo"), &value); diags.HasError() {
		return "", false
	}
	if value.IsNull() || value.IsUnknown() {
		return "", false
	}
	return value.ValueString(), true
}

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	value, ok := configValue(ctx, req.Config)
	if !ok {
		resp.Diagnostics.AddAttributeError(path.Root("value_wo"), "Missing Secret Value", "value_wo must be set in the configuration.")
		return
	}

	appName := plan.App.ValueString()
	secretName := plan.Name.ValueString()

	result, err := r.pd.client.SetSecrets(ctx, appName, map[string]string{secretName: value})
	if err != nil {
		resp.Diagnostics.AddError("Set Secret Error", err.Error())
		return
	}

	plan.ID = types.StringValue(appName + "/" + secretName)
	plan.Digest = types.StringValue(digestFromSecrets(result.Secrets, secretName))
	plan.Version = types.Int64Value(int64(result.Version))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	secrets, err := r.pd.client.ListSecrets(ctx, state.App.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("List Secrets Error", err.Error())
		return
	}

	name := state.Name.ValueString()
	for _, s := range secrets {
		if s.Name == name {
			state.Digest = types.StringValue(s.Digest)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}

	// Secret was deleted outside of Terraform.
	resp.State.RemoveResource(ctx)
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan secretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state secretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	value, ok := configValue(ctx, req.Config)
	if !ok {
		resp.Diagnostics.AddAttributeError(path.Root("value_wo"), "Missing Secret Value", "value_wo must be set in the configuration.")
		return
	}

	appName := plan.App.ValueString()
	secretName := plan.Name.ValueString()

	result, err := r.pd.client.SetSecrets(ctx, appName, map[string]string{secretName: value})
	if err != nil {
		resp.Diagnostics.AddError("Update Secret Error", err.Error())
		return
	}

	plan.ID = state.ID
	plan.Digest = types.StringValue(digestFromSecrets(result.Secrets, secretName))
	plan.Version = types.Int64Value(int64(result.Version))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.pd.client.DeleteSecret(ctx, state.App.ValueString(), state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete Secret Error", err.Error())
	}
}

func digestFromSecrets(secrets []flyio.SecretInfo, name string) string {
	for _, s := range secrets {
		if s.Name == name {
			return s.Digest
		}
	}
	return ""
}
