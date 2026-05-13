// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"fmt"
)

// MaxExternalRefBytes caps the size of AlertRuleConfig.ExternalRef. Loose
// enough to fit a JSON-encoded compound key (dashboard id + component id +
// namespace) but tight enough that a rogue rule can't bloat the alert
// payload on every fire.
const MaxExternalRefBytes = 512

// AlertRuleConfig defines a single alert rule that is shared by all alert
// transports (webhook, WS, MQTT). The transport-specific dispatch target
// lives on the parent alert resource, not on the rule.
type AlertRuleConfig struct {
	Name      string `json:"name"`               // Rule name/identifier
	Condition string `json:"condition"`          // e.g., "temperature > 80"
	Cooldown  string `json:"cooldown,omitempty"` // Min time between alerts (e.g., "5m")

	// ExternalRef is an opaque, free-form string echoed on every alert
	// payload this rule fires. ts-store does not parse or interpret it —
	// it's a pass-through pointer for the consumer (e.g. a dashboard
	// component id, a Grafana slug, a JSON-encoded compound key). Capped
	// at MaxExternalRefBytes bytes; NUL bytes rejected. Optional.
	ExternalRef string `json:"external_ref,omitempty"`
}

// Validate checks the rule's user-supplied fields against the constraints
// enforced at the API boundary. Called once at create time; the result is
// not re-checked at fire time.
func (r AlertRuleConfig) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("rule name is required")
	}
	if r.Condition == "" {
		return fmt.Errorf("rule %q: condition is required", r.Name)
	}
	if len(r.ExternalRef) > MaxExternalRefBytes {
		return fmt.Errorf("rule %q: external_ref exceeds %d bytes (got %d)",
			r.Name, MaxExternalRefBytes, len(r.ExternalRef))
	}
	if bytes.IndexByte([]byte(r.ExternalRef), 0) >= 0 {
		return fmt.Errorf("rule %q: external_ref must not contain NUL bytes", r.Name)
	}
	return nil
}
