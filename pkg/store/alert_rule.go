// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

// AlertRuleConfig defines a single alert rule that is shared by all alert
// transports (webhook, WS, MQTT). The transport-specific dispatch target
// lives on the parent alert resource, not on the rule.
type AlertRuleConfig struct {
	Name      string `json:"name"`               // Rule name/identifier
	Condition string `json:"condition"`          // e.g., "temperature > 80"
	Cooldown  string `json:"cooldown,omitempty"` // Min time between alerts (e.g., "5m")
}
