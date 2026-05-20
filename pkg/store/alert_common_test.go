// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"strings"
	"testing"
)

func TestAlertCommon_Validate(t *testing.T) {
	tests := []struct {
		name    string
		c       AlertCommon
		wantErr string // substring; "" means expect no error
	}{
		{
			name: "ok",
			c:    AlertCommon{Name: "hot", Condition: "t > 80"},
		},
		{
			name: "ok with optional fields",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", Cooldown: "5m", ExternalRef: "dash:tile:42"},
		},
		{
			name:    "missing name",
			c:       AlertCommon{Condition: "t > 80"},
			wantErr: "name is required",
		},
		{
			name:    "missing condition",
			c:       AlertCommon{Name: "hot"},
			wantErr: "condition is required",
		},
		{
			name:    "external_ref too large",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", ExternalRef: strings.Repeat("x", MaxExternalRefBytes+1)},
			wantErr: "external_ref exceeds",
		},
		{
			name:    "external_ref contains NUL",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", ExternalRef: "a\x00b"},
			wantErr: "NUL bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
