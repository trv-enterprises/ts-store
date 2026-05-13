// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"strings"
	"testing"
)

func TestAlertRuleConfigValidateBasic(t *testing.T) {
	t.Run("valid minimal rule", func(t *testing.T) {
		r := AlertRuleConfig{Name: "hot", Condition: "t > 80"}
		if err := r.Validate(); err != nil {
			t.Errorf("expected ok, got: %v", err)
		}
	})
	t.Run("missing name", func(t *testing.T) {
		r := AlertRuleConfig{Condition: "t > 80"}
		if err := r.Validate(); err == nil {
			t.Errorf("expected error for empty name")
		}
	})
	t.Run("missing condition", func(t *testing.T) {
		r := AlertRuleConfig{Name: "hot"}
		if err := r.Validate(); err == nil {
			t.Errorf("expected error for empty condition")
		}
	})
}

func TestAlertRuleConfigValidateExternalRef(t *testing.T) {
	t.Run("empty is fine (optional)", func(t *testing.T) {
		r := AlertRuleConfig{Name: "n", Condition: "x > 0", ExternalRef: ""}
		if err := r.Validate(); err != nil {
			t.Errorf("empty external_ref: %v", err)
		}
	})
	t.Run("at exactly the cap", func(t *testing.T) {
		r := AlertRuleConfig{Name: "n", Condition: "x > 0",
			ExternalRef: strings.Repeat("a", MaxExternalRefBytes)}
		if err := r.Validate(); err != nil {
			t.Errorf("at-cap external_ref: %v", err)
		}
	})
	t.Run("one byte over the cap", func(t *testing.T) {
		r := AlertRuleConfig{Name: "n", Condition: "x > 0",
			ExternalRef: strings.Repeat("a", MaxExternalRefBytes+1)}
		if err := r.Validate(); err == nil {
			t.Errorf("expected size-limit error")
		}
	})
	t.Run("contains NUL byte", func(t *testing.T) {
		r := AlertRuleConfig{Name: "n", Condition: "x > 0",
			ExternalRef: "before\x00after"}
		if err := r.Validate(); err == nil {
			t.Errorf("expected NUL-byte error")
		}
	})
	t.Run("JSON-encoded compound key is fine", func(t *testing.T) {
		r := AlertRuleConfig{Name: "n", Condition: "x > 0",
			ExternalRef: `{"dashboard_id":"foo","component_id":"bar"}`}
		if err := r.Validate(); err != nil {
			t.Errorf("compound key: %v", err)
		}
	})
}
