// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider implements the pfrest OpenTofu/Terraform provider — a native
// client for the pfSense REST API v2 (pfSense-pkg-RESTAPI, https://pfrest.org).
// It is generic over the API surface (the pfrest_object resource/data source
// address any /api/v2 endpoint), giving 100% feature coverage without
// per-feature code.
package provider

import (
	"context"

	"github.com/JamesonRGrieve/tofu-pfrest/internal/pfrest"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*pfrestProvider)(nil)

// New returns the provider factory for a given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &pfrestProvider{version: version} }
}

type pfrestProvider struct {
	version string
}

type providerModel struct {
	Host     types.String `tfsdk:"host"`
	APIKey   types.String `tfsdk:"api_key"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func (p *pfrestProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	// Single-token type name -> resources are `pfrest_object`, so Terraform's
	// prefix-before-first-underscore inference resolves the local name cleanly
	// (the source address is still jamesonrgrieve/pfrest).
	resp.TypeName = "pfrest"
	resp.Version = p.version
}

func (p *pfrestProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Native provider for pfSense via the REST API v2 " +
			"(pfSense-pkg-RESTAPI / pfrest.org). Stateless `X-API-Key` auth.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "pfSense address (host or host:port), no scheme.",
			},
			"api_key": schema.StringAttribute{
				Required:            true,
				Sensitive:           true,
				MarkdownDescription: "pfSense REST API key, sent as the `X-API-Key` header on every request.",
			},
			"insecure": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Skip TLS verification (default true — pfSense ships a self-signed cert). " +
					"Set false only with a trusted cert installed.",
			},
		},
	}
}

func (p *pfrestProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	insecure := true
	if !cfg.Insecure.IsNull() && !cfg.Insecure.IsUnknown() {
		insecure = cfg.Insecure.ValueBool()
	}
	client := pfrest.NewClient(pfrest.Config{
		Host:     cfg.Host.ValueString(),
		APIKey:   cfg.APIKey.ValueString(),
		Insecure: insecure,
	})
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *pfrestProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource}
}

func (p *pfrestProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}
