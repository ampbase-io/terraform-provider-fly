// Package fly implements a Terraform provider for the Fly.io Machines API.
//
// The provider exposes seven resources:
//   - fly_app             — application lifecycle
//   - fly_machine         — VMs (with files/mounts/restart/metadata for stateful clusters)
//   - fly_ip              — private_v6 (Flycast) + public IPv4/IPv6 allocations
//   - fly_secret          — app secrets
//   - fly_volume          — persistent volumes (host-pinned via the compute block)
//   - fly_cert            — ACME (Let's Encrypt) certificates with flat DNS validation outputs
//   - fly_cert_validation — state gate that records a cert reached configured=true
//
// Each wraps the corresponding operations in the generated Machines API
// client (internal/flyio/machines/).
package fly

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
)

var _ provider.Provider = (*FlyProvider)(nil)

// FlyProvider implements the Fly.io Machines API Terraform provider.
type FlyProvider struct {
	version string
}

// FlyProviderModel maps the provider schema to Go types.
type FlyProviderModel struct {
	APIToken types.String `tfsdk:"api_token"`
	OrgSlug  types.String `tfsdk:"org_slug"`
	BaseURL  types.String `tfsdk:"base_url"`
}

// providerData is passed to resources via the Configure phase.
type providerData struct {
	client  *flyio.Client
	orgSlug string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &FlyProvider{version: version}
	}
}

func (p *FlyProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "fly"
	resp.Version = p.version
}

func (p *FlyProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage Fly.io Machines API resources: apps, machines, IPs, secrets, volumes, ACME certificates, and cert validation gates.",
		Attributes: map[string]schema.Attribute{
			"api_token": schema.StringAttribute{
				Description: "Fly.io API token. Defaults to the FLY_API_TOKEN environment variable.",
				Optional:    true,
				Sensitive:   true,
			},
			"org_slug": schema.StringAttribute{
				Description: "Fly.io organization slug.",
				Required:    true,
			},
			"base_url": schema.StringAttribute{
				Description: "Override the Machines API base URL.",
				Optional:    true,
			},
		},
	}
}

func (p *FlyProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config FlyProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	token := os.Getenv("FLY_API_TOKEN")
	if !config.APIToken.IsNull() {
		token = config.APIToken.ValueString()
	}
	if token == "" {
		resp.Diagnostics.AddError(
			"Missing API Token",
			"Set the api_token provider attribute or the FLY_API_TOKEN environment variable.",
		)
		return
	}

	var opts []flyio.Option
	opts = append(opts, flyio.WithToken(token))
	if !config.BaseURL.IsNull() {
		opts = append(opts, flyio.WithBaseURL(config.BaseURL.ValueString()))
	}

	orgSlug := config.OrgSlug.ValueString()
	client, err := flyio.New(orgSlug, opts...)
	if err != nil {
		resp.Diagnostics.AddError("Client Creation Error", err.Error())
		return
	}

	pd := &providerData{
		client:  client,
		orgSlug: orgSlug,
	}

	resp.DataSourceData = pd
	resp.ResourceData = pd
}

func (p *FlyProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewAppResource,
		NewMachineResource,
		NewIPResource,
		NewSecretResource,
		NewVolumeResource,
		NewCertResource,
		NewCertValidationResource,
	}
}

func (p *FlyProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
