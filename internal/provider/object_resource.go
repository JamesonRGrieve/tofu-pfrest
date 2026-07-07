// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/JamesonRGrieve/tofu-pfrest/internal/pfrest"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                 = (*objectResource)(nil)
	_ resource.ResourceWithConfigure    = (*objectResource)(nil)
	_ resource.ResourceWithImportState  = (*objectResource)(nil)
	_ resource.ResourceWithUpgradeState = (*objectResource)(nil)
)

// schemaVersion is bumped whenever the persisted shape changes. v1 moved `body`
// from a monolithic JSON string to a per-key map(string) (each value the JSON
// encoding of that field) so the plan diffs per managed key and unmanaged
// upstream keys never render `→ null`. See UpgradeState.
const schemaVersion = 1

// NewObjectResource constructs the generic pfrest_object resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct {
	client *pfrest.Client
}

// objectModel is the state/plan shape for pfrest_object. `Body` is a per-key
// map: each value is the compact JSON encoding of that field's value (scalar,
// array, or object), so the plan diffs one managed key at a time and the wire
// round-trip is type-exact.
type objectModel struct {
	ID        types.String `tfsdk:"id"`
	Endpoint  types.String `tfsdk:"endpoint"`
	Singleton types.Bool   `tfsdk:"singleton"`
	ObjectID  types.String `tfsdk:"object_id"`
	Body      types.Map    `tfsdk:"body"`
	Apply     types.Bool   `tfsdk:"apply"`
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: schemaVersion,
		MarkdownDescription: "A generic pfSense REST API v2 resource addressed by its `/api/v2` endpoint path. " +
			"Covers 100% of the pfSense REST API: any collection item (`firewall/alias`, " +
			"`firewall/rule`, `interface/vlan`, …) where the server assigns an `id` on POST, or any " +
			"singleton (`system/dns`, `system/hostname`, …) updated in place with PATCH. " +
			"`body` declares only the keys this resource manages; device-returned keys outside `body` are " +
			"ignored for drift, so a subset declaration imports to 0-diff and never clobbers unmanaged fields.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource id — `<endpoint>` for singletons, `<endpoint>|<object_id>` for collection items.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"endpoint": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The `/api/v2` path of the resource (leading slash optional), e.g. " +
					"`firewall/alias`, `firewall/rule`, `system/dns`. ForceNew.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"singleton": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether `endpoint` is a singleton (PATCHed in place, no create/delete, e.g. " +
					"`system/dns`) rather than a collection item (POST creates, server assigns `id`; " +
					"GET/PATCH/DELETE address by `?id=`). Default `false` (collection). ForceNew.",
				Default:       booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{requiresReplaceBool{}},
			},
			"object_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The pfSense-assigned object id for a collection item, captured from `data.id` " +
					"on create. Empty for singletons.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"body": schema.MapNestedAttribute{
				Optional: true,
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"json": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The JSON encoding of this field's value — a scalar (`\"web\"`, `1`, `true`), an array, or an object.",
						},
					},
				},
				MarkdownDescription: "The declared (managed) attributes, one map entry per top-level key, each a " +
					"`{ json = <jsonencode(value)> }` object — author it as " +
					"`{ for k, v in {…} : k => { json = jsonencode(v) } }`. Modeling it as a map of nested objects " +
					"(NOT a `map(string)`) is deliberate: Terraform validates a nested-object map PER ELEMENT, so the " +
					"subset plan modifier can legally keep each unmanaged key at its prior value (never `→ null`) while " +
					"taking only the genuinely-changed managed keys from config — one key at a time. State holds the " +
					"full live object; a `map(string)` body would instead be validated whole-value and reject the " +
					"per-key merge. A declared subset that matches the live object imports/re-plans to 0-diff; a changed " +
					"key surfaces alone; the wire PATCH carries only the changed keys.",
				PlanModifiers: []planmodifier.Map{subsetSuppressMap{}},
			},
			"apply": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Send `?apply=true` on every write (create/update/delete) so pfSense commits " +
					"the change AND reloads the affected subsystem synchronously, instead of leaving it staged as a " +
					"pending change. REQUIRED for object types whose writes are otherwise not persisted/listed until " +
					"applied (e.g. `routing/gateway`), and it makes the server-assigned `id` reflect the committed " +
					"array position. Unset ⇒ treated as false (write only — pair with a separate `pfrest_reconcile` apply). " +
					"Set `true` ONLY for state-preserving applies (firewall filter, NAT, routing, DNS); NEVER for " +
					"interface/VLAN/WireGuard/IPsec writes, whose apply bounces the management path.",
				// No StaticBool default: a default would rewrite an imported resource's
				// null `apply` to `false` at plan time, churning EVERY adopted object
				// (null->false) and — under subsetSuppress — re-PATCHing its full body to
				// the live device. UseStateForUnknown instead mirrors prior state when the
				// attribute is unset, so an imported null stays null (0-diff) while an
				// explicit config value is still honored. applyOn() reads null as false.
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// applyOn reports whether this object should send `?apply=true` on writes.
func (r *objectResource) applyOn(m objectModel) bool {
	return !m.Apply.IsNull() && !m.Apply.IsUnknown() && m.Apply.ValueBool()
}

// withApply returns q (creating it if nil) with `apply=true` added when apply is
// set; otherwise q unchanged. pfSense reads the flag from the query string.
func withApply(q url.Values, apply bool) url.Values {
	if !apply {
		return q
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("apply", "true")
	return q
}

func (r *objectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// normPath ensures a leading slash and trims surrounding whitespace.
func normPath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// idQuery builds the `?id=<id>` query the pfSense REST API uses to address a
// single collection item.
func idQuery(id string) url.Values {
	return url.Values{"id": []string{id}}
}

// extractID pulls the server-assigned id from a created/returned object's raw
// `data`. pfSense returns the full object including its `id` field (a number
// for most collections, a string for a few). It is rendered back to a string
// for state.
func extractID(data json.RawMessage) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return "", false
	}
	raw, ok := obj["id"]
	if !ok {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch n := v.(type) {
	case float64:
		// Integer ids: render without a trailing ".0".
		return fmt.Sprintf("%d", int64(n)), true
	case string:
		return n, true
	default:
		return strings.Trim(string(raw), `"`), true
	}
}

func (r *objectResource) isSingleton(m objectModel) bool {
	return !m.Singleton.IsNull() && !m.Singleton.IsUnknown() && m.Singleton.ValueBool()
}

// ---------------------------------------------------------------------------
// body <-> JSON bridge. `body` is a map(string): one entry per managed top-level
// key, each value the compact JSON encoding of that field's value. State holds
// ONLY the managed keys (Read projects the live object down to them), so the plan
// diffs per key and unmanaged upstream keys never render →null or reach the wire.
// ---------------------------------------------------------------------------

// bodyMap decodes the map(string) body value into a Go map of JSON fragments.
// bodyElem is the nested object each body map entry holds: the JSON encoding of
// that field's value. A map of these (vs a map(string)) is what makes the body a
// NestedType, which Terraform validates PER ELEMENT — the property that lets the
// plan modifier legally keep some keys at prior while taking others from config.
type bodyElem struct {
	JSON types.String `tfsdk:"json"`
}

// bodyElemType is the attr.Type of a bodyElem, needed to build a types.Map value.
var bodyElemType = types.ObjectType{AttrTypes: map[string]attr.Type{"json": types.StringType}}

func bodyMap(ctx context.Context, v types.Map) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := map[string]string{}
	if v.IsNull() || v.IsUnknown() {
		return out, diags
	}
	elems := map[string]bodyElem{}
	diags.Append(v.ElementsAs(ctx, &elems, false)...)
	for k, e := range elems {
		out[k] = e.JSON.ValueString()
	}
	return out, diags
}

// bodyToJSON reconstructs the upstream JSON object from the body map, splicing
// each value in verbatim as a JSON fragment so types round-trip exactly. Errors
// if any value is not valid JSON.
func bodyToJSON(bm map[string]string) ([]byte, error) {
	obj := make(map[string]json.RawMessage, len(bm))
	for k, v := range bm {
		if !json.Valid([]byte(v)) {
			return nil, fmt.Errorf("body[%q] is not valid JSON: %s", k, v)
		}
		obj[k] = json.RawMessage(v)
	}
	return json.Marshal(obj)
}

// objectToBody converts a full upstream JSON object to a body map: every
// top-level key -> compact JSON encoding of its value.
func objectToBody(full []byte) (map[string]string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(full, &obj); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(obj))
	for k, raw := range obj {
		c, err := compactJSON(raw)
		if err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, nil
}

// tfBody wraps a Go body map as a types.Map value.
func tfBody(ctx context.Context, bm map[string]string) (types.Map, diag.Diagnostics) {
	elems := make(map[string]bodyElem, len(bm))
	for k, v := range bm {
		elems[k] = bodyElem{JSON: types.StringValue(v)}
	}
	return types.MapValueFrom(ctx, bodyElemType, elems)
}

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	bm, d := bodyMap(ctx, m.Body)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, jerr := bodyToJSON(bm)
	if jerr != nil {
		resp.Diagnostics.AddError("Invalid body", jerr.Error())
		return
	}
	endpoint := normPath(m.Endpoint.ValueString())

	apply := r.applyOn(m)

	if r.isSingleton(m) {
		// Singleton: there is nothing to create — PATCH the endpoint into the
		// declared shape. The endpoint is the id; no object_id.
		if _, err := r.client.Patch(endpoint, withApply(nil, apply), body); err != nil {
			resp.Diagnostics.AddError("pfrest create (singleton PATCH) failed", err.Error())
			return
		}
		m.ObjectID = types.StringValue("")
		m.ID = m.Endpoint
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	// Collection: POST creates the object and the server assigns the id, which
	// it returns in data.id.
	data, err := r.client.Post(endpoint, withApply(nil, apply), body)
	if err != nil {
		resp.Diagnostics.AddError("pfrest create (POST) failed", err.Error())
		return
	}
	id, ok := extractID(data)
	if !ok {
		resp.Diagnostics.AddError("pfrest create: no id in response",
			fmt.Sprintf("POST %s returned no `data.id`: %s", endpoint, string(data)))
		return
	}
	m.ObjectID = types.StringValue(id)
	m.ID = types.StringValue(m.Endpoint.ValueString() + "|" + id)
	// Store the declared body verbatim; the next refresh (Read) replaces it with
	// the full device object.
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	endpoint := normPath(m.Endpoint.ValueString())
	var query url.Values
	if !r.isSingleton(m) {
		query = idQuery(m.ObjectID.ValueString())
	}
	data, err := r.client.Get(endpoint, query)
	if err != nil {
		if pfrest.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("pfrest read failed", err.Error())
		return
	}
	// Project the live object down to the MANAGED keys — those already in prior
	// state — so state tracks only the declared keys and a steady-state plan diffs
	// per key with NO `→ null`. (A NestedType map's planned key set must equal
	// config's, so unmanaged keys must not live in state.) On a fresh import (no
	// prior keys) store the full object, so a subset config first drops the
	// unmanaged keys once (migration-style) and then settles to 0-diff.
	full, perr := objectToBody(data)
	if perr != nil {
		resp.Diagnostics.AddError("pfrest read: invalid JSON from device", perr.Error())
		return
	}
	managed, dmk := bodyMap(ctx, m.Body)
	resp.Diagnostics.Append(dmk...)
	if resp.Diagnostics.HasError() {
		return
	}
	stored := full
	if len(managed) > 0 {
		stored = make(map[string]string, len(managed))
		for k := range managed {
			if v, ok := full[k]; ok {
				stored[k] = v
			}
		}
	}
	bv, db := tfBody(ctx, stored)
	resp.Diagnostics.Append(db...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.Body = bv
	if r.isSingleton(m) {
		m.ObjectID = types.StringValue("")
		m.ID = m.Endpoint
	} else {
		m.ID = types.StringValue(m.Endpoint.ValueString() + "|" + m.ObjectID.ValueString())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// object_id is computed and carried across an update; pull it from prior state.
	var prior objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ObjectID = prior.ObjectID

	planBM, dp := bodyMap(ctx, m.Body)
	resp.Diagnostics.Append(dp...)
	priorBM, dpr := bodyMap(ctx, prior.Body)
	resp.Diagnostics.Append(dpr...)
	if resp.Diagnostics.HasError() {
		return
	}
	endpoint := normPath(m.Endpoint.ValueString())
	apply := r.applyOn(m)

	// The wire PATCH carries ONLY the managed keys whose value changed vs prior (or
	// are newly declared), so unchanged fields are never re-sent. This matters
	// because RESTAPI >= 2.8.2 re-validates every field it receives and REJECTS
	// legacy/immutable ones it accepts at rest — notably a haproxy frontend's
	// interface-symbol `a_extaddr` binds (wan_ipv4/opt1_ipv4) 400 "extaddr must be
	// one of [custom,...]". The device merges the delta, preserving untouched
	// fields. Keys only in prior are a managed-set-shrink (migration/projection)
	// and are left on the device untouched. An empty delta is a true no-op → skip
	// the write; a standalone reload is `pfrest_reconcile`'s job.
	delta := map[string]string{}
	for k, pv := range planBM {
		if ov, ok := priorBM[k]; !ok || !jsonEqual(json.RawMessage(pv), json.RawMessage(ov)) {
			delta[k] = pv
		}
	}
	if len(delta) == 0 {
		if r.isSingleton(m) {
			m.ID = m.Endpoint
		} else {
			m.ID = types.StringValue(m.Endpoint.ValueString() + "|" + m.ObjectID.ValueString())
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	body, jerr := bodyToJSON(delta)
	if jerr != nil {
		resp.Diagnostics.AddError("pfrest update: cannot encode delta", jerr.Error())
		return
	}

	if r.isSingleton(m) {
		if _, err := r.client.Patch(endpoint, withApply(nil, apply), body); err != nil {
			resp.Diagnostics.AddError("pfrest update (singleton PATCH) failed", err.Error())
			return
		}
		m.ID = m.Endpoint
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	// Collection: PATCH the endpoint with the delta + {id: object_id}. pfSense
	// addresses the PATCH target by the `id` field in the body; we also send it
	// as a query param for robustness.
	id := m.ObjectID.ValueString()
	patchBody, err := withID(body, id)
	if err != nil {
		resp.Diagnostics.AddError("pfrest update: cannot inject id", err.Error())
		return
	}
	if _, err := r.client.Patch(endpoint, withApply(idQuery(id), apply), patchBody); err != nil {
		resp.Diagnostics.AddError("pfrest update (PATCH) failed", err.Error())
		return
	}
	m.ID = types.StringValue(m.Endpoint.ValueString() + "|" + id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.isSingleton(m) {
		// Singletons cannot be deleted — just drop from state.
		return
	}
	endpoint := normPath(m.Endpoint.ValueString())
	if _, err := r.client.Delete(endpoint, withApply(idQuery(m.ObjectID.ValueString()), r.applyOn(m))); err != nil {
		if pfrest.NotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("pfrest delete failed", err.Error())
	}
}

func (r *objectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id forms (body is populated on the following Read):
	//   <endpoint>             — singleton (e.g. "system/dns")
	//   <endpoint>|<object_id> — collection item (e.g. "firewall/alias|3")
	parts := strings.SplitN(req.ID, "|", 2)
	endpoint := parts[0]
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("endpoint"), endpoint)...)
	// body is Computed; the following Read populates it with the full live object.
	empty, _ := tfBody(ctx, map[string]string{})
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("body"), empty)...)
	if len(parts) == 2 && parts[1] != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("singleton"), false)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("object_id"), parts[1])...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("singleton"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("object_id"), "")...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), endpoint)...)
}

// objectModelV0 is the pre-v1 state shape: `body` was a single JSON string
// holding the full device object. See UpgradeState.
type objectModelV0 struct {
	ID        types.String `tfsdk:"id"`
	Endpoint  types.String `tfsdk:"endpoint"`
	Singleton types.Bool   `tfsdk:"singleton"`
	ObjectID  types.String `tfsdk:"object_id"`
	Body      types.String `tfsdk:"body"`
	Apply     types.Bool   `tfsdk:"apply"`
}

// UpgradeState migrates v0 state (JSON-string `body`) to v1 (map `body`): the
// stored full-object JSON string is parsed into one map entry per top-level key,
// each value the compact JSON of that field. The subset plan modifier then keeps
// every key at prior on the next plan, so the migration is 0-diff (no reconcile
// apply) and no unmanaged key churns.
func (r *objectResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: &schema.Schema{
				Attributes: map[string]schema.Attribute{
					"id":        schema.StringAttribute{Computed: true},
					"endpoint":  schema.StringAttribute{Required: true},
					"singleton": schema.BoolAttribute{Optional: true, Computed: true},
					"object_id": schema.StringAttribute{Computed: true},
					"body":      schema.StringAttribute{Required: true},
					"apply":     schema.BoolAttribute{Optional: true, Computed: true},
				},
			},
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var old objectModelV0
				resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
				if resp.Diagnostics.HasError() {
					return
				}
				bm, err := objectToBody([]byte(old.Body.ValueString()))
				if err != nil {
					resp.Diagnostics.AddError("pfrest state upgrade: invalid v0 body JSON", err.Error())
					return
				}
				bv, d := tfBody(ctx, bm)
				resp.Diagnostics.Append(d...)
				if resp.Diagnostics.HasError() {
					return
				}
				resp.Diagnostics.Append(resp.State.Set(ctx, &objectModel{
					ID:        old.ID,
					Endpoint:  old.Endpoint,
					Singleton: old.Singleton,
					ObjectID:  old.ObjectID,
					Body:      bv,
					Apply:     old.Apply,
				})...)
			},
		},
	}
}

// withID returns the JSON body with an "id" key set to id (numeric if id parses
// as an integer, else string). The pfSense PATCH convention identifies the
// target collection item by the `id` field in the request body.
func withID(body []byte, id string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["id"] = idJSON(id)
	return json.Marshal(obj)
}

// idJSON renders id as a JSON number when it is an integer, else as a JSON
// string — matching how pfSense types collection ids.
func idJSON(id string) json.RawMessage {
	if _, err := jsonInt(id); err == nil {
		return json.RawMessage(id)
	}
	b, _ := json.Marshal(id)
	return b
}

func jsonInt(s string) (int64, error) {
	var n int64
	// json.Unmarshal validates integer-ness without locale surprises.
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// requiresReplaceBool — RequiresReplace for the `singleton` bool. (The bool
// plan-modifier package's RequiresReplace exists, but reimplementing the tiny
// piece keeps the dependency surface identical to the reference provider.)
// ---------------------------------------------------------------------------

type requiresReplaceBool struct{}

func (requiresReplaceBool) Description(context.Context) string {
	return "Changing this attribute forces resource replacement."
}
func (requiresReplaceBool) MarkdownDescription(context.Context) string {
	return (requiresReplaceBool{}).Description(nil)
}
func (requiresReplaceBool) PlanModifyBool(_ context.Context, req planmodifier.BoolRequest, resp *planmodifier.BoolResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if !req.StateValue.Equal(req.PlanValue) {
		resp.RequiresReplace = true
	}
}

// ---------------------------------------------------------------------------
// subset plan modifier (map) — reconcile the declared managed keys against the
// full prior device object held in state, PER KEY. Because `body` is a map,
// Terraform validates the plan element-by-element, so keeping some elements at
// their prior value while taking others from config is legal (each planned[k] is
// either config[k] or prior[k]) — the per-element merge a monolithic string
// forbade (there core rejects any whole-value that is neither config nor the
// exact prior). The result: every unmanaged key stays at prior (never →null); a
// declared key that is a recursive value-subset of the live value stays at prior
// (0-diff adoption/import/upgrade); and only a genuinely-changed declared key
// surfaces — one key at a time. The wire PATCH (Update) then carries only those
// changed keys, so an unchanged `a_extaddr` (which RESTAPI 2.8.2 rejects on
// re-send) never ships and no unmanaged field is ever cleared on the device.
// ---------------------------------------------------------------------------

type subsetSuppressMap struct{}

func (subsetSuppressMap) Description(context.Context) string {
	return "Keep unmanaged and subset-matched keys at their prior value; diff only genuinely-changed declared keys."
}
func (subsetSuppressMap) MarkdownDescription(context.Context) string {
	return (subsetSuppressMap{}).Description(nil)
}

func (subsetSuppressMap) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, resp *planmodifier.MapResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return // create — nothing to reconcile against
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	prior, dp := bodyMap(ctx, req.StateValue)
	cfg, dc := bodyMap(ctx, req.ConfigValue)
	resp.Diagnostics.Append(dp...)
	resp.Diagnostics.Append(dc...)
	if resp.Diagnostics.HasError() {
		return
	}
	mv, d := tfBody(ctx, reconcileBody(prior, cfg))
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.PlanValue = mv
}

// reconcileBody produces the planned body: exactly the DECLARED (config) keys —
// never the unmanaged ones — so the map's key set equals config's (Terraform
// rejects a planned map whose element count differs from config). For each
// declared key it keeps the prior value when the config value is a recursive
// value-subset of it (no real change — this preserves a thin declared array
// element that omits server-assigned metadata), else it takes the config value.
// Each planned[k] is therefore config[k] or prior[k] — which a NestedType map
// validates PER ELEMENT (a plain map(string) would be rejected whole-value).
// Unmanaged keys stay out of state via Read's projection, so they never render
// `→ null` on a steady-state plan.
func reconcileBody(prior, cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, cv := range cfg {
		if pv, ok := prior[k]; ok && subsetRaw(json.RawMessage(cv), json.RawMessage(pv)) {
			out[k] = pv
		} else {
			out[k] = cv
		}
	}
	return out
}

// subsetRaw decodes two raw JSON values and reports whether cfg is a recursive
// value-subset of prior. This lets a declared array element that omits
// server-assigned metadata (e.g. a haproxy backend server declaring only
// name/address/port while the device adds id/parent_id/weight/ssl) still match
// its live counterpart, so the managed key stays 0-diff.
func subsetRaw(cfg, prior json.RawMessage) bool {
	var cv, pv any
	if json.Unmarshal(cfg, &cv) != nil || json.Unmarshal(prior, &pv) != nil {
		return false
	}
	return subsetValue(cv, pv)
}

// subsetValue reports whether the decoded config value is a recursive value-subset
// of the decoded prior value. Objects: every cfg key must subset-match prior's.
// Arrays: equal length, element-wise subset. Scalars: structural equality.
func subsetValue(cfg, prior any) bool {
	switch c := cfg.(type) {
	case map[string]any:
		p, ok := prior.(map[string]any)
		if !ok {
			return false
		}
		for k, cval := range c {
			pval, ok := p[k]
			if !ok || !subsetValue(cval, pval) {
				return false
			}
		}
		return true
	case []any:
		p, ok := prior.([]any)
		if !ok || len(p) != len(c) {
			return false
		}
		for i := range c {
			if !subsetValue(c[i], p[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(cfg, prior)
	}
}

// jsonEqual compares two raw JSON values structurally (object-key-order
// insensitive), so a re-serialization difference is not mistaken for a change.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// compactJSON re-serializes raw JSON in compact, key-sorted-by-encoder form.
func compactJSON(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
