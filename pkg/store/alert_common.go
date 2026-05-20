// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"fmt"
)

// MaxExternalRefBytes caps the size of AlertCommon.ExternalRef. Loose
// enough to fit a JSON-encoded compound key (dashboard id + component id +
// namespace) but tight enough that a rogue config can't bloat the alert
// payload on every fire.
const MaxExternalRefBytes = 512

// AlertCommon holds the rule + dispatch policy fields shared by every
// alert transport (webhook, WS, MQTT). It is embedded into each
// transport-specific alert config; transport-specific fields (URL,
// broker, etc.) live on the parent.
type AlertCommon struct {
	Name        string `json:"name"`
	Condition   string `json:"condition"`
	Cooldown    string `json:"cooldown,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`

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

// Validate checks the user-supplied fields against the constraints
// enforced at the API boundary. Called once at create time.
func (c AlertCommon) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.Condition == "" {
		return fmt.Errorf("alert %q: condition is required", c.Name)
	}
	if len(c.ExternalRef) > MaxExternalRefBytes {
		return fmt.Errorf("alert %q: external_ref exceeds %d bytes (got %d)",
			c.Name, MaxExternalRefBytes, len(c.ExternalRef))
	}
	if bytes.IndexByte([]byte(c.ExternalRef), 0) >= 0 {
		return fmt.Errorf("alert %q: external_ref must not contain NUL bytes", c.Name)
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
