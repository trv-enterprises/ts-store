// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"fmt"

	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/internal/rules"
)

// MaxExternalRefBytes caps the size of AlertCommon.ExternalRef. Loose
// enough to fit a JSON-encoded compound key (dashboard id + component id +
// namespace) but tight enough that a rogue config can't bloat the alert
// payload on every fire.
const MaxExternalRefBytes = 512

// MaxMessageTemplateBytes caps AlertCommon.Message. Aliased from the
// package that does the rendering rather than restated, so the limit
// enforced at write time and the limit the renderer documents cannot
// drift apart.
const MaxMessageTemplateBytes = rules.MaxMessageTemplateBytes

// Rule types. RuleTypeCondition is the default and covers every alert
// that existed before issue #134: a predicate over the fields of a
// record that arrived. RuleTypeStaleness is the inverse — it fires on
// the *absence* of records, which no condition can express because
// absence has no record and therefore no fields to compare.
const (
	RuleTypeCondition = "condition"
	RuleTypeStaleness = "staleness"
)

// AlertCommon holds the rule + dispatch policy fields shared by every
// alert transport (webhook, WS, MQTT). It is embedded into each
// transport-specific alert config; transport-specific fields (URL,
// broker, etc.) live on the parent.
type AlertCommon struct {
	Name string `json:"name"`

	// RuleType selects how this alert decides to fire. Empty means
	// "condition" so every alert persisted before #134 keeps working
	// without migration.
	//   "condition" — Condition is evaluated against each arriving
	//                 record (the original behavior).
	//   "staleness" — the store's newest timestamp is compared to
	//                 MaxAge on every poll tick, including ticks where
	//                 nothing arrived. Condition is not used.
	RuleType string `json:"rule_type,omitempty"`

	Condition string `json:"condition,omitempty"`

	// MaxAge is how long the store may go without a new record before a
	// staleness alert fires (duration string, e.g. "5m"). Required when
	// RuleType is "staleness", rejected otherwise.
	//
	// Deliberately has no default: a collector polling every 60s should
	// alert after a few missed polls, but an event-driven source (a door
	// contact) can be legitimately silent for days. Any global default
	// would flood one of those two cases.
	MaxAge string `json:"max_age,omitempty"`

	Cooldown    string `json:"cooldown,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`

	// Message is a template rendered into the alert payload's `message`
	// field on each fire (issue #144), giving receivers one
	// human-readable sentence instead of each assembling its own from
	// rule_name/condition/data.
	//
	//   "Server Room 5's temperature {temp} exceeded the max"
	//
	// Placeholders are {field} for any field of the triggering record,
	// plus the built-ins {store}, {rule_name}, {condition},
	// {timestamp} and {external_ref}, which shadow same-named record
	// fields. `{{` is a literal brace. Optional format specs:
	// {field:.2f} for float precision, {field:time} to render an epoch
	// as RFC3339 UTC.
	//
	// An unknown or misspelled field renders empty and never fails the
	// alert — a formatting mistake must not suppress the thing it was
	// describing. Field existence is therefore NOT validated here: for
	// JSON stores the field set is unknown until data arrives.
	Message string `json:"message,omitempty"`

	// RestartPolicy controls how the worker resumes after a process
	// restart.
	//   "" or "now" — start from wall-clock now (default). The worker
	//                 does not read or write a cursor. Best for
	//                 high-frequency metric streams where a brief gap
	//                 is acceptable.
	//   "resume"   — read the cursor on Start and replay records
	//                 since lastTs. If MaxReplay is set, bound the
	//                 resume window by now - MaxReplay. Best for
	//                 event streams (e.g. journal logs) where a
	//                 missed match is meaningful.
	RestartPolicy string `json:"restart_policy,omitempty"`

	// MaxReplay bounds how far back a resume-policy worker will replay.
	// Only meaningful when RestartPolicy == "resume". Empty means
	// unbounded (replay everything since the cursor).
	MaxReplay string `json:"max_replay,omitempty"`
}

// EffectiveRuleType returns the rule type with the empty-means-condition
// default applied, so callers never branch on "" themselves.
func (c AlertCommon) EffectiveRuleType() string {
	if c.RuleType == "" {
		return RuleTypeCondition
	}
	return c.RuleType
}

// IsStaleness reports whether this alert fires on absent data rather
// than on the contents of an arriving record.
func (c AlertCommon) IsStaleness() bool {
	return c.EffectiveRuleType() == RuleTypeStaleness
}

// Validate checks the user-supplied fields against the constraints
// enforced at the API boundary. Called once at create time.
func (c AlertCommon) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}

	switch c.EffectiveRuleType() {
	case RuleTypeCondition:
		if c.Condition == "" {
			return fmt.Errorf("alert %q: condition is required", c.Name)
		}
		if c.MaxAge != "" {
			return fmt.Errorf("alert %q: max_age is only valid when rule_type=%q", c.Name, RuleTypeStaleness)
		}
	case RuleTypeStaleness:
		// A staleness rule fires when no record arrived, so there is no
		// record to evaluate a condition against. Rejecting rather than
		// ignoring keeps the persisted config an honest description of
		// what the alert actually does.
		if c.Condition != "" {
			return fmt.Errorf("alert %q: condition is not valid when rule_type=%q (staleness fires on absent data, which has no fields to compare)", c.Name, RuleTypeStaleness)
		}
		if c.MaxAge == "" {
			return fmt.Errorf("alert %q: max_age is required when rule_type=%q", c.Name, RuleTypeStaleness)
		}
		// Parse here so a typo is a 400 at create rather than a worker
		// that starts cleanly and never fires. A non-positive max_age
		// would report every store as permanently stale.
		d, err := duration.ParseDuration(c.MaxAge)
		if err != nil {
			return fmt.Errorf("alert %q: invalid max_age %q: %w", c.Name, c.MaxAge, err)
		}
		if d <= 0 {
			return fmt.Errorf("alert %q: max_age %q must be positive", c.Name, c.MaxAge)
		}
		// Same reasoning as condition: a staleness rule has no cursor,
		// because it is driven by the clock rather than by a scan
		// position. "resume" and max_replay would be silently inert.
		if c.RestartPolicy == "resume" {
			return fmt.Errorf("alert %q: restart_policy=\"resume\" is not valid when rule_type=%q (a staleness rule has no cursor to resume from)", c.Name, RuleTypeStaleness)
		}
	default:
		return fmt.Errorf("alert %q: rule_type must be %q or %q (got %q)",
			c.Name, RuleTypeCondition, RuleTypeStaleness, c.RuleType)
	}

	if len(c.ExternalRef) > MaxExternalRefBytes {
		return fmt.Errorf("alert %q: external_ref exceeds %d bytes (got %d)",
			c.Name, MaxExternalRefBytes, len(c.ExternalRef))
	}
	if bytes.IndexByte([]byte(c.ExternalRef), 0) >= 0 {
		return fmt.Errorf("alert %q: external_ref must not contain NUL bytes", c.Name)
	}
	// Cap the template SOURCE, not its rendered output: the source is
	// what can be bounded cheaply at write time, while the rendered
	// length varies per fire with the data.
	if len(c.Message) > MaxMessageTemplateBytes {
		return fmt.Errorf("alert %q: message exceeds %d bytes (got %d)",
			c.Name, MaxMessageTemplateBytes, len(c.Message))
	}
	if bytes.IndexByte([]byte(c.Message), 0) >= 0 {
		return fmt.Errorf("alert %q: message must not contain NUL bytes", c.Name)
	}
	switch c.RestartPolicy {
	case "", "now", "resume":
	default:
		return fmt.Errorf("alert %q: restart_policy must be \"now\" or \"resume\" (got %q)",
			c.Name, c.RestartPolicy)
	}
	if c.MaxReplay != "" && c.RestartPolicy != "resume" {
		return fmt.Errorf("alert %q: max_replay only valid when restart_policy=\"resume\"", c.Name)
	}
	return nil
}
