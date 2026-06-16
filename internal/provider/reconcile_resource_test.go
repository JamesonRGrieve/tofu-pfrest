// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"errors"
	"testing"
)

func TestRunReconcile(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []string
		apply     func(string) error
		wantWarn  int
		wantAll   bool
	}{
		{
			name:      "all ok",
			endpoints: []string{"firewall/apply", "services/dns_resolver/apply"},
			apply:     func(string) error { return nil },
			wantWarn:  0, wantAll: false,
		},
		{
			name:      "no endpoints is a no-op",
			endpoints: nil,
			apply:     func(string) error { return errors.New("should not be called") },
			wantWarn:  0, wantAll: false,
		},
		{
			name:      "error on the only endpoint escalates",
			endpoints: []string{"firewall/apply"},
			apply:     func(string) error { return errors.New("pfrest POST /firewall/apply: HTTP 500") },
			wantWarn:  1, wantAll: true,
		},
		{
			name:      "leading slash is normalized before apply",
			endpoints: []string{"/firewall/apply"},
			apply: func(p string) error {
				if p != "/firewall/apply" {
					return errors.New("path not normalized: " + p)
				}
				return nil
			},
			wantWarn: 0, wantAll: false,
		},
		{
			name:      "partial failure warns but does not escalate",
			endpoints: []string{"firewall/apply", "services/dns_resolver/apply"},
			apply: func(p string) error {
				if p == "/services/dns_resolver/apply" {
					return errors.New("HTTP 404") // endpoint absent on this box
				}
				return nil
			},
			wantWarn: 1, wantAll: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, all := runReconcile(tt.endpoints, tt.apply)
			if len(warns) != tt.wantWarn {
				t.Errorf("warnings = %d (%v), want %d", len(warns), warns, tt.wantWarn)
			}
			if all != tt.wantAll {
				t.Errorf("allFailed = %v, want %v", all, tt.wantAll)
			}
		})
	}
}
