// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"

	"github.com/JamesonRGrieve/tofu-pfrest/internal/pfrest"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource              = (*reconcileResource)(nil)
	_ resource.ResourceWithConfigure = (*reconcileResource)(nil)
)

// NewReconcileResource constructs the pfrest_reconcile resource: an
// unconditional apply that POSTs a set of `/api/v2` apply paths on every run. It
// manages no remote object — it exists to heal config-vs-live drift Terraform
// cannot detect. pfSense's REST API applies most writes immediately, so the
// generic pfrest_object covers the normal create/update path; what it cannot
// heal is a config-only divergence introduced WITHOUT a tofu-driven write — a
// manual edit that staged a change without applying, an offline/partial
// adoption, or a reboot loading stale running state. A plan with 0 object
// changes never re-applies, so that drift lingers. Pairing this with a
// `triggers` map holding `timestamp()` POSTs the deferred-apply endpoints every
// run, reconciling live state to config. Use ONLY for seamless reloads
// (firewall filter / DNS resolver); never for interface/VLAN/WireGuard/IPsec
// applies that bounce the management path.
func NewReconcileResource() resource.Resource { return &reconcileResource{} }

type reconcileResource struct {
	client *pfrest.Client
}

type reconcileModel struct {
	ID        types.String `tfsdk:"id"`
	Endpoints types.List   `tfsdk:"endpoints"`
	Triggers  types.Map    `tfsdk:"triggers"`
}

func (r *reconcileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reconcile"
}

func (r *reconcileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Unconditional reconcile/apply. POSTs each `/api/v2` apply path in `endpoints` " +
			"(e.g. `firewall/apply`, `services/dns_resolver/apply`) with an empty body on every create/update — " +
			"it manages no remote object. Pair with a `triggers` map containing `timestamp()` so it re-applies on " +
			"every run, healing config-vs-live drift Terraform cannot detect (the provider tracks config, and " +
			"pfSense applies most writes inline, so a 0-change plan otherwise never re-applies an out-of-band " +
			"divergence). Use ONLY for seamless reloads (firewall filter `pfctl -f`, DNS resolver); NEVER for " +
			"`interface/apply`, WireGuard, or IPsec applies — those can drop the management path mid-apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Static resource id (`reconcile`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"endpoints": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Ordered list of `/api/v2` apply paths to POST (parameterless deferred-apply calls).",
			},
			"triggers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Arbitrary key/value map; any change re-runs the apply. Set a key to `timestamp()` to fire every run.",
			},
		},
	}
}

func (r *reconcileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*pfrest.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *pfrest.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

// runReconcile applies each endpoint in order via apply and returns one warning
// string per failed endpoint plus allFailed=true when every endpoint failed. A
// per-endpoint failure is tolerated (best-effort: an optional apply may be
// absent on a given box); total failure means the device is unreachable or every
// path is wrong, which the caller escalates to an error. Pure (apply is
// injected) so the aggregation is unit-testable without a live device.
func runReconcile(endpoints []string, apply func(path string) error) (warnings []string, allFailed bool) {
	failed := 0
	for _, ep := range endpoints {
		p := normPath(ep)
		if err := apply(p); err != nil {
			failed++
			warnings = append(warnings, fmt.Sprintf("%s: %s", p, err.Error()))
		}
	}
	return warnings, len(endpoints) > 0 && failed == len(endpoints)
}

func (r *reconcileResource) reconcile(ctx context.Context, m reconcileModel, diags *diag.Diagnostics) {
	var eps []string
	diags.Append(m.Endpoints.ElementsAs(ctx, &eps, false)...)
	if diags.HasError() {
		return
	}
	// pfSense REST apply endpoints take no parameters; POST an empty JSON object.
	warnings, allFailed := runReconcile(eps, func(p string) error {
		_, err := r.client.Post(p, []byte("{}"))
		return err
	})
	for _, w := range warnings {
		diags.AddWarning("pfSense reconcile endpoint failed", w)
	}
	if allFailed {
		diags.AddError("pfSense reconcile failed",
			"every reconcile endpoint failed — the device is likely unreachable or all apply paths are invalid")
	}
}

func (r *reconcileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.reconcile(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// No remote object to read; keep prior state verbatim.
	var m reconcileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.reconcile(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Manages no remote object — nothing to delete.
}
