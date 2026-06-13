// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"
	"net/url"

	"github.com/JamesonRGrieve/tofu-pfrest/internal/pfrest"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*objectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*objectDataSource)(nil)
)

// NewObjectDataSource constructs the generic pfrest_object data source.
func NewObjectDataSource() datasource.DataSource { return &objectDataSource{} }

type objectDataSource struct {
	client *pfrest.Client
}

type objectDataModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	ObjectID types.String `tfsdk:"object_id"`
	Response types.String `tfsdk:"response"`
}

func (d *objectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (d *objectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Read any pfSense REST API v2 resource by its `/api/v2` endpoint path. " +
			"Set `object_id` to read a single collection item (`?id=`); omit it to read a singleton or a " +
			"plural list endpoint.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Resource path under `/api/v2` (leading slash optional), e.g. `firewall/alias`, `system/dns`, `firewall/aliases`.",
			},
			"object_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Collection item id to read via `?id=`. Omit for singletons / list endpoints.",
			},
			"response": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The `data` field of the pfSense response envelope, as compact JSON.",
			},
		},
	}
}

func (d *objectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*pfrest.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *pfrest.Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *objectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m objectDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var query url.Values
	if !m.ObjectID.IsNull() && m.ObjectID.ValueString() != "" {
		query = idQuery(m.ObjectID.ValueString())
	}
	data, err := d.client.Get(normPath(m.Endpoint.ValueString()), query)
	if err != nil {
		resp.Diagnostics.AddError("pfrest read failed", err.Error())
		return
	}
	compact, err := compactJSON(data)
	if err != nil {
		resp.Diagnostics.AddError("pfrest read: invalid JSON from device", err.Error())
		return
	}
	m.Response = types.StringValue(compact)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
