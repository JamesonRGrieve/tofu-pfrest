// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"encoding/json"
	"net/url"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// applyOn resolves the Optional+Computed `apply` bool to a concrete value for any
// input state (null when unset in config, unknown while computed pre-apply). Create/
// Update persist types.BoolValue(applyOn(m)) into state, so the result must ALWAYS be
// a known bool — an unknown left in the result taints the resource ("Provider
// returned invalid result object after apply ... unknown value for .apply").
func TestApplyOn(t *testing.T) {
	r := &objectResource{}
	cases := []struct {
		name  string
		apply types.Bool
		want  bool
	}{
		{name: "null (apply unset in config)", apply: types.BoolNull(), want: false},
		{name: "unknown (computed, pre-apply)", apply: types.BoolUnknown(), want: false},
		{name: "explicit false", apply: types.BoolValue(false), want: false},
		{name: "explicit true", apply: types.BoolValue(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.applyOn(objectModel{Apply: tc.apply})
			if got != tc.want {
				t.Fatalf("applyOn(%v) = %v, want %v", tc.apply, got, tc.want)
			}
		})
	}
}

// resolveApply must resolve an UNKNOWN plan (create with apply unset) to a known
// bool — else the result taints ("unknown value for .apply after apply") — while
// echoing a KNOWN plan (null on an update, or an explicit bool) unchanged, since the
// apply result must equal a known plan ("inconsistent result: .apply was null, but
// now cty.False"). Both halves are load-bearing; each caused a real apply failure.
func TestResolveApply(t *testing.T) {
	cases := []struct {
		name    string
		planned types.Bool
		apply   bool
		want    types.Bool
	}{
		{name: "unknown plan (create, unset) → resolved false", planned: types.BoolUnknown(), apply: false, want: types.BoolValue(false)},
		{name: "unknown plan (create, apply=true resolved) → true", planned: types.BoolUnknown(), apply: true, want: types.BoolValue(true)},
		{name: "known null plan (update, unset) → echo null", planned: types.BoolNull(), apply: false, want: types.BoolNull()},
		{name: "known false plan → echo false", planned: types.BoolValue(false), apply: true, want: types.BoolValue(false)},
		{name: "known true plan → echo true", planned: types.BoolValue(true), apply: false, want: types.BoolValue(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveApply(tc.planned, tc.apply)
			if !got.Equal(tc.want) {
				t.Fatalf("resolveApply(%v, %v) = %v, want %v", tc.planned, tc.apply, got, tc.want)
			}
			if got.IsUnknown() {
				t.Fatalf("resolveApply must never return unknown, got %v", got)
			}
		})
	}
}

func TestWithApply(t *testing.T) {
	t.Run("apply=false leaves nil query nil", func(t *testing.T) {
		if got := withApply(nil, false); got != nil {
			t.Fatalf("withApply(nil,false) = %v, want nil", got)
		}
	})
	t.Run("apply=true on nil query adds apply=true", func(t *testing.T) {
		got := withApply(nil, true)
		if got.Get("apply") != "true" {
			t.Fatalf("withApply(nil,true).apply = %q, want true", got.Get("apply"))
		}
	})
	t.Run("apply=true preserves existing id and adds apply", func(t *testing.T) {
		got := withApply(url.Values{"id": []string{"7"}}, true)
		if got.Get("id") != "7" {
			t.Fatalf("id = %q, want 7", got.Get("id"))
		}
		if got.Get("apply") != "true" {
			t.Fatalf("apply = %q, want true", got.Get("apply"))
		}
	})
	t.Run("apply=false preserves existing id unchanged", func(t *testing.T) {
		in := url.Values{"id": []string{"7"}}
		got := withApply(in, false)
		if got.Get("id") != "7" || got.Has("apply") {
			t.Fatalf("withApply(id=7,false) = %v, want id=7 and no apply", got)
		}
	})
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// reconcileBody produces the planned body: EXACTLY the declared (config) keys
// (a NestedType map's planned key set must equal config's — unmanaged keys are
// dropped, and kept out of state by Read's projection). For each declared key it
// keeps the prior value on a recursive value-subset match (0-diff, incl. a thin
// array element that omits server metadata), else takes config. Each planned[k]
// is config[k] or prior[k] — legal per element for a NestedType map.
func TestReconcileBody(t *testing.T) {
	cases := []struct {
		name       string
		prior, cfg map[string]string
		want       map[string]string
	}{
		{
			name:  "config subset — declared keys keep prior; unmanaged NOT in result (dropped)",
			prior: map[string]string{"id": `3`, "name": `"web"`, "descr": `"edge"`, "address": `["10.0.0.1"]`},
			cfg:   map[string]string{"name": `"web"`, "address": `["10.0.0.1"]`},
			want:  map[string]string{"name": `"web"`, "address": `["10.0.0.1"]`},
		},
		{
			name:  "declared scalar changed — that key takes config; only declared keys present",
			prior: map[string]string{"id": `0`, "name": `"fe"`, "a_extaddr": `[{"extaddr":"wan_ipv4"}]`},
			cfg:   map[string]string{"name": `"fe2"`},
			want:  map[string]string{"name": `"fe2"`},
		},
		{
			name:  "thin declared server subset of live (server metadata) — key kept at prior full value",
			prior: map[string]string{"name": `"p"`, "servers": `[{"name":"s","address":"1.1.1.1","port":"80","id":0,"weight":1}]`},
			cfg:   map[string]string{"name": `"p"`, "servers": `[{"name":"s","address":"1.1.1.1","port":"80"}]`},
			want:  map[string]string{"name": `"p"`, "servers": `[{"name":"s","address":"1.1.1.1","port":"80","id":0,"weight":1}]`},
		},
		{
			name:  "declared array grows — that key takes config (only declared keys present)",
			prior: map[string]string{"id": `0`, "ha_acls": `[{"name":"a"}]`, "a_extaddr": `[{"extaddr":"wan_ipv4"}]`},
			cfg:   map[string]string{"ha_acls": `[{"name":"a"},{"name":"b"}]`},
			want:  map[string]string{"ha_acls": `[{"name":"a"},{"name":"b"}]`},
		},
		{
			name:  "newly declared key not on device — taken from config",
			prior: map[string]string{"name": `"p"`},
			cfg:   map[string]string{"name": `"p"`, "descr": `"added"`},
			want:  map[string]string{"name": `"p"`, "descr": `"added"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileBody(tc.prior, tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("reconcileBody() keys=%v, want=%v", sortedKeys(got), sortedKeys(tc.want))
			}
			for k, wv := range tc.want {
				gv, ok := got[k]
				if !ok || !jsonEqual(json.RawMessage(gv), json.RawMessage(wv)) {
					t.Fatalf("reconcileBody()[%q] = %q, want %q", k, gv, wv)
				}
			}
		})
	}
}

// objectToBody splits a full object into per-key JSON fragments; bodyToJSON
// reassembles them type-exactly. The round-trip must be structurally identical.
func TestObjectBodyRoundTrip(t *testing.T) {
	full := `{"name":"web","enabled":"1","weight":5,"active":true,"servers":[{"name":"s","port":"80"}]}`
	bm, err := objectToBody([]byte(full))
	if err != nil {
		t.Fatalf("objectToBody: %v", err)
	}
	for k, want := range map[string]string{"name": `"web"`, "enabled": `"1"`, "weight": `5`, "active": `true`} {
		if !jsonEqual(json.RawMessage(bm[k]), json.RawMessage(want)) {
			t.Fatalf("objectToBody[%q] = %q, want %q", k, bm[k], want)
		}
	}
	out, err := bodyToJSON(bm)
	if err != nil {
		t.Fatalf("bodyToJSON: %v", err)
	}
	if !jsonEqual(json.RawMessage(out), json.RawMessage(full)) {
		t.Fatalf("round-trip mismatch:\n got  %s\n want %s", out, full)
	}
	if _, err := bodyToJSON(map[string]string{"bad": `not-json`}); err == nil {
		t.Fatalf("bodyToJSON should reject a non-JSON value")
	}
}

func TestNormPath(t *testing.T) {
	for in, want := range map[string]string{
		"firewall/alias":  "/firewall/alias",
		"/firewall/alias": "/firewall/alias",
		" system/dns ":    "/system/dns",
		"/system/dns":     "/system/dns",
	} {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractID(t *testing.T) {
	cases := []struct {
		name   string
		data   string
		wantID string
		wantOK bool
	}{
		{name: "integer id (most collections)", data: `{"id":3,"name":"a"}`, wantID: "3", wantOK: true},
		{name: "zero id", data: `{"id":0,"name":"a"}`, wantID: "0", wantOK: true},
		{name: "string id", data: `{"id":"wan","descr":"x"}`, wantID: "wan", wantOK: true},
		{name: "no id field", data: `{"name":"a"}`, wantID: "", wantOK: false},
		{name: "invalid json", data: `nope`, wantID: "", wantOK: false},
		{name: "array (not an object)", data: `[1,2]`, wantID: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := extractID(json.RawMessage(tc.data))
			if ok != tc.wantOK || id != tc.wantID {
				t.Fatalf("extractID(%s) = (%q,%v), want (%q,%v)", tc.data, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

func TestWithID(t *testing.T) {
	// Integer id is injected as a JSON number.
	out, err := withID([]byte(`{"name":"a"}`), "3")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if n, ok := m["id"].(float64); !ok || int(n) != 3 {
		t.Fatalf("withID numeric id: got %#v", m["id"])
	}
	if m["name"] != "a" {
		t.Fatalf("withID dropped a field: %#v", m)
	}

	// Non-integer id is injected as a JSON string.
	out, err = withID([]byte(`{"descr":"x"}`), "wan")
	if err != nil {
		t.Fatal(err)
	}
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if s, ok := m["id"].(string); !ok || s != "wan" {
		t.Fatalf("withID string id: got %#v", m["id"])
	}

	// Invalid body is an error.
	if _, err := withID([]byte(`not json`), "1"); err == nil {
		t.Fatal("withID(invalid body) expected error")
	}
}

func TestIDQuery(t *testing.T) {
	q := idQuery("3")
	if got := q.Encode(); got != "id=3" {
		t.Fatalf("idQuery(3).Encode() = %q, want %q", got, "id=3")
	}
}

func TestCompactJSON(t *testing.T) {
	out, err := compactJSON([]byte("{\n \"b\": 2,\n \"a\": 1\n}"))
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal of a map sorts keys; whitespace is removed.
	if out != `{"a":1,"b":2}` {
		t.Fatalf("compactJSON = %q", out)
	}
}
