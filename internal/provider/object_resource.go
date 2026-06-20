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
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*objectResource)(nil)
	_ resource.ResourceWithConfigure   = (*objectResource)(nil)
	_ resource.ResourceWithImportState = (*objectResource)(nil)
)

// NewObjectResource constructs the generic pfrest_object resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct {
	client *pfrest.Client
}

// objectModel is the state/plan shape for pfrest_object.
type objectModel struct {
	ID        types.String `tfsdk:"id"`
	Endpoint  types.String `tfsdk:"endpoint"`
	Singleton types.Bool   `tfsdk:"singleton"`
	ObjectID  types.String `tfsdk:"object_id"`
	Body      types.String `tfsdk:"body"`
	Apply     types.Bool   `tfsdk:"apply"`
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
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
			"body": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON object of the declared (managed) attributes. State holds the full " +
					"device object; drift is detected only on these keys.",
				PlanModifiers: []planmodifier.String{subsetSuppress{}},
			},
			"apply": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Send `?apply=true` on every write (create/update/delete) so pfSense commits " +
					"the change AND reloads the affected subsystem synchronously, instead of leaving it staged as a " +
					"pending change. REQUIRED for object types whose writes are otherwise not persisted/listed until " +
					"applied (e.g. `routing/gateway`), and it makes the server-assigned `id` reflect the committed " +
					"array position. Default `false` (write only — pair with a separate `pfrest_reconcile` apply). " +
					"Set `true` ONLY for state-preserving applies (firewall filter, NAT, routing, DNS); NEVER for " +
					"interface/VLAN/WireGuard/IPsec writes, whose apply bounces the management path.",
				Default: booldefault.StaticBool(false),
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

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := []byte(m.Body.ValueString())
	if !json.Valid(body) {
		resp.Diagnostics.AddError("Invalid body", "`body` must be valid JSON")
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
	// Store the full device object (compacted). The subset plan modifier
	// reconciles it against the declared config body at plan time.
	compact, err := compactJSON(data)
	if err != nil {
		resp.Diagnostics.AddError("pfrest read: invalid JSON from device", err.Error())
		return
	}
	m.Body = types.StringValue(compact)
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

	body := []byte(m.Body.ValueString())
	if !json.Valid(body) {
		resp.Diagnostics.AddError("Invalid body", "`body` must be valid JSON")
		return
	}
	endpoint := normPath(m.Endpoint.ValueString())
	apply := r.applyOn(m)

	if r.isSingleton(m) {
		if _, err := r.client.Patch(endpoint, withApply(nil, apply), body); err != nil {
			resp.Diagnostics.AddError("pfrest update (singleton PATCH) failed", err.Error())
			return
		}
		m.ID = m.Endpoint
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	// Collection: PATCH the endpoint with body + {id: object_id}. pfSense
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
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("body"), "{}")...)
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
// subset plan modifier — suppress diff when every declared key already matches
// the full device object held in prior state. This is what lets a subset
// `body` import/refresh to 0-diff without clobbering unmanaged device fields.
// ---------------------------------------------------------------------------

type subsetSuppress struct{}

func (subsetSuppress) Description(context.Context) string {
	return "Suppress diff when all declared JSON keys already match the device object in state."
}
func (subsetSuppress) MarkdownDescription(context.Context) string {
	return (subsetSuppress{}).Description(nil)
}

func (subsetSuppress) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return // create — nothing to reconcile against
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	// All declared (config) keys already match the device object in prior state:
	// keep the full prior object and show no diff. Otherwise leave the planned
	// (config) value in place so the drift surfaces as an update.
	if subsetMatches(req.StateValue.ValueString(), req.ConfigValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// subsetMatches reports whether the config JSON object is a recursive value-subset
// of the prior JSON object: every config key must be present in prior, and nested
// objects/arrays are matched with the SAME subset semantics. This lets a declared
// array element that omits server-assigned metadata (e.g. a haproxy backend server
// declaring only name/address/port while the device adds id/parent_id/weight/ssl)
// still match its live counterpart and import to 0-diff. Arrays must have equal
// length and match element-wise by index; scalars match structurally. Invalid JSON
// on either side returns false so the caller falls back to a normal diff.
func subsetMatches(prior, cfg string) bool {
	var p, c map[string]json.RawMessage
	if json.Unmarshal([]byte(prior), &p) != nil {
		return false
	}
	if json.Unmarshal([]byte(cfg), &c) != nil {
		return false
	}
	for k, cv := range c {
		pv, ok := p[k]
		if !ok || !subsetRaw(cv, pv) {
			return false
		}
	}
	return true
}

// subsetRaw decodes two raw JSON values and reports whether cfg is a recursive
// value-subset of prior.
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
