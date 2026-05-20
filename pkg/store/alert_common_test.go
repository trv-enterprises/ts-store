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
		{
			name: "restart_policy now",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", RestartPolicy: "now"},
		},
		{
			name: "restart_policy resume",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", RestartPolicy: "resume"},
		},
		{
			name: "restart_policy resume with max_replay",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", RestartPolicy: "resume", MaxReplay: "1h"},
		},
		{
			name:    "restart_policy invalid",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", RestartPolicy: "later"},
			wantErr: "restart_policy must be",
		},
		{
			name:    "max_replay without resume",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", MaxReplay: "1h"},
			wantErr: "max_replay only valid",
		},
		{
			name:    "max_replay with now policy",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", RestartPolicy: "now", MaxReplay: "1h"},
			wantErr: "max_replay only valid",
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
