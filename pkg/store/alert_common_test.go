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
			name:    "message too large",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", Message: strings.Repeat("x", MaxMessageTemplateBytes+1)},
			wantErr: "message exceeds",
		},
		{
			name:    "message contains NUL",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", Message: "a\x00b"},
			wantErr: "NUL bytes",
		},
		{
			name: "message at the cap is accepted",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", Message: strings.Repeat("x", MaxMessageTemplateBytes)},
		},
		{
			// Field existence is a render-time concern: for JSON stores
			// the field set is unknown until data arrives, so validating
			// it here would reject valid templates.
			name: "message referencing an unknown field is accepted",
			c:    AlertCommon{Name: "hot", Condition: "t > 80", Message: "temp is {no_such_field}"},
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

		// --- staleness rules (issue #134) ---
		{
			name: "staleness ok",
			c:    AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m"},
		},
		{
			name: "staleness with cooldown and external_ref",
			c: AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m",
				Cooldown: "30m", ExternalRef: "dash:tile:7"},
		},
		{
			name: "staleness with explicit now policy",
			c:    AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m", RestartPolicy: "now"},
		},
		{
			name: "explicit condition rule_type behaves as before",
			c:    AlertCommon{Name: "hot", RuleType: RuleTypeCondition, Condition: "t > 80"},
		},
		{
			name:    "staleness without max_age",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness},
			wantErr: "max_age is required",
		},
		{
			name:    "staleness rejects condition",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m", Condition: "t > 80"},
			wantErr: "condition is not valid",
		},
		{
			name:    "staleness rejects restart_policy resume",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m", RestartPolicy: "resume"},
			wantErr: "has no cursor to resume from",
		},
		{
			// resume is rejected first, which transitively bars max_replay
			// (it is only legal alongside resume).
			name:    "staleness rejects max_replay",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "5m", MaxReplay: "1h"},
			wantErr: "max_replay only valid",
		},
		{
			name:    "staleness with unparseable max_age",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "soon"},
			wantErr: "invalid max_age",
		},
		{
			name:    "staleness with zero max_age",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "0s"},
			wantErr: "must be positive",
		},
		{
			// ParseDuration rejects negatives itself, so this surfaces as
			// an invalid-value error rather than the positivity check.
			name:    "staleness with negative max_age",
			c:       AlertCommon{Name: "quiet", RuleType: RuleTypeStaleness, MaxAge: "-5m"},
			wantErr: "invalid max_age",
		},
		{
			name:    "condition rule rejects max_age",
			c:       AlertCommon{Name: "hot", Condition: "t > 80", MaxAge: "5m"},
			wantErr: "max_age is only valid",
		},
		{
			name:    "unknown rule_type",
			c:       AlertCommon{Name: "hot", RuleType: "sliding_window", Condition: "t > 80"},
			wantErr: "rule_type must be",
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
