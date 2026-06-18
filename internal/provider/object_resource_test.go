// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"encoding/json"
	"testing"
)

func TestSubsetMatches(t *testing.T) {
	cases := []struct {
		name        string
		prior, cfg  string
		wantMatched bool
	}{
		{
			name:        "config subset of full device object — match (0-diff)",
			prior:       `{"id":3,"name":"web_servers","type":"host","descr":"","address":["10.0.0.1"]}`,
			cfg:         `{"name":"web_servers","type":"host","address":["10.0.0.1"]}`,
			wantMatched: true,
		},
		{
			name:        "declared key drifted — no match (update)",
			prior:       `{"id":3,"name":"web_servers_OLD","type":"host"}`,
			cfg:         `{"name":"web_servers","type":"host"}`,
			wantMatched: false,
		},
		{
			name:        "declared key missing on device — no match",
			prior:       `{"id":3,"type":"host"}`,
			cfg:         `{"name":"web_servers","type":"host"}`,
			wantMatched: false,
		},
		{
			name:        "key order / whitespace insensitive — match",
			prior:       `{"name":"web_servers","type":"host"}`,
			cfg:         "{\n  \"type\": \"host\",\n  \"name\": \"web_servers\"\n}",
			wantMatched: true,
		},
		{
			name:        "nested object value compared structurally — match",
			prior:       `{"dnsserver":{"v4":"1.1.1.1","v6":"::1"},"id":0}`,
			cfg:         `{"dnsserver":{"v6":"::1","v4":"1.1.1.1"}}`,
			wantMatched: true,
		},
		{
			name:        "nested object value drift — no match",
			prior:       `{"dnsserver":{"v4":"1.1.1.1"}}`,
			cfg:         `{"dnsserver":{"v4":"8.8.8.8"}}`,
			wantMatched: false,
		},
		{
			name:        "list value compared in order — match",
			prior:       `{"address":["10.0.0.1","10.0.0.2"],"id":1}`,
			cfg:         `{"address":["10.0.0.1","10.0.0.2"]}`,
			wantMatched: true,
		},
		{
			name:        "list order drift — no match",
			prior:       `{"address":["10.0.0.2","10.0.0.1"]}`,
			cfg:         `{"address":["10.0.0.1","10.0.0.2"]}`,
			wantMatched: false,
		},
		{
			name:        "nested object subset of prior object — match",
			prior:       `{"dnsserver":{"v4":"1.1.1.1","v6":"::1","ttl":60}}`,
			cfg:         `{"dnsserver":{"v4":"1.1.1.1"}}`,
			wantMatched: true,
		},
		{
			name:        "array-of-objects: declared server subset of live server (metadata) — match",
			prior:       `{"name":"authentik","id":23,"servers":[{"name":"authentik","address":"192.168.1.8","port":"9000","status":"active","id":0,"parent_id":23,"ssl":false,"weight":1}]}`,
			cfg:         `{"name":"authentik","servers":[{"name":"authentik","address":"192.168.1.8","port":"9000","status":"active"}]}`,
			wantMatched: true,
		},
		{
			name:        "array-of-objects: declared server field drift — no match",
			prior:       `{"servers":[{"name":"authentik","address":"192.168.1.8","port":"9000","id":0}]}`,
			cfg:         `{"servers":[{"name":"authentik","address":"192.168.1.8","port":"443"}]}`,
			wantMatched: false,
		},
		{
			name:        "array-of-objects: declared server missing on live — no match",
			prior:       `{"servers":[{"name":"authentik","address":"192.168.1.8"}]}`,
			cfg:         `{"servers":[{"name":"authentik","address":"192.168.1.8","port":"9000"}]}`,
			wantMatched: false,
		},
		{
			name:        "array length mismatch (server removed) — no match",
			prior:       `{"servers":[{"name":"a"},{"name":"b"}]}`,
			cfg:         `{"servers":[{"name":"a"}]}`,
			wantMatched: false,
		},
		{
			name:        "invalid prior JSON — no match (fall back to diff)",
			prior:       `not json`,
			cfg:         `{"a":1}`,
			wantMatched: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subsetMatches(tc.prior, tc.cfg); got != tc.wantMatched {
				t.Fatalf("subsetMatches() = %v, want %v", got, tc.wantMatched)
			}
		})
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
