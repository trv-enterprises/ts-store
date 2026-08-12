// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"fmt"
	"strings"
)

// Access is one class of operation a grant may permit. The three classes
// are deliberately coarse — authorization is store-granular, never
// per-series or per-field (issue #138).
type Access string

const (
	// AccessRead covers query/range/stats/schema-read and stream-out —
	// including the push-connection lifecycle (issue #154): a push
	// connection only delivers data the key could already poll, so
	// consumers manage their own subscriptions.
	AccessRead Access = "read"
	// AccessWrite covers ingest.
	AccessWrite Access = "write"
	// AccessManage is store-scoped administration: alert rules, rollups,
	// schema mutation, reset. Alerts belong to the store rather than the
	// server, so managing them is a per-store grant and does not require
	// the server admin key.
	AccessManage Access = "manage"
)

// AllAccess is every access class, in escalating order. Used for the
// legacy migration, which must reproduce the old all-or-nothing store
// key exactly.
var AllAccess = []Access{AccessRead, AccessWrite, AccessManage}

// ParseAccess validates an access class name.
func ParseAccess(s string) (Access, error) {
	switch Access(strings.TrimSpace(strings.ToLower(s))) {
	case AccessRead:
		return AccessRead, nil
	case AccessWrite:
		return AccessWrite, nil
	case AccessManage:
		return AccessManage, nil
	}
	return "", fmt.Errorf("unknown access class %q (want read, write, or manage)", s)
}

// Grant ties a set of access classes to a set of stores.
//
// Stores is matched as an exact name, a prefix glob ("sensors-*"), or
// "*" for every store. Globs are required rather than optional: an
// enumerated store list goes stale the moment an ingester auto-creates a
// store, which is exactly when a rollup target or a new collector needs
// to authenticate.
type Grant struct {
	Stores string   `json:"stores"`
	Access []Access `json:"access"`
}

// Validate checks a grant is well-formed. Called at key-create time so a
// malformed grant is rejected before it is persisted rather than
// silently never matching.
func (g Grant) Validate() error {
	if strings.TrimSpace(g.Stores) == "" {
		return fmt.Errorf("grant stores pattern is required")
	}
	// Only a trailing "*" is meaningful. Rejecting interior wildcards
	// keeps matching a prefix comparison and avoids implying a full glob
	// engine that callers would then rely on.
	if i := strings.IndexByte(g.Stores, '*'); i >= 0 && i != len(g.Stores)-1 {
		return fmt.Errorf("grant stores pattern %q: '*' is only valid as a trailing wildcard", g.Stores)
	}
	if len(g.Access) == 0 {
		return fmt.Errorf("grant for %q: at least one access class is required", g.Stores)
	}
	for _, a := range g.Access {
		if _, err := ParseAccess(string(a)); err != nil {
			return fmt.Errorf("grant for %q: %w", g.Stores, err)
		}
	}
	return nil
}

// MatchesStore reports whether this grant's pattern covers storeName.
func (g Grant) MatchesStore(storeName string) bool {
	if g.Stores == "*" {
		return true
	}
	if strings.HasSuffix(g.Stores, "*") {
		return strings.HasPrefix(storeName, strings.TrimSuffix(g.Stores, "*"))
	}
	return g.Stores == storeName
}

// Permits reports whether this grant covers storeName at the given
// access class. Access classes are independent flags, not a hierarchy:
// "manage" does not imply "read". A key that manages alerts but must not
// read data is a real configuration, so callers list every class they
// want.
func (g Grant) Permits(storeName string, access Access) bool {
	if !g.MatchesStore(storeName) {
		return false
	}
	for _, a := range g.Access {
		if a == access {
			return true
		}
	}
	return false
}

// ParseGrant parses the CLI grant syntax "<access[,access...]>:<pattern>",
// e.g. "read:*" or "read,write:sensors-*". The access list comes first so
// the store pattern — which may itself contain a colon-free glob — is the
// unambiguous remainder.
func ParseGrant(s string) (Grant, error) {
	access, stores, found := strings.Cut(s, ":")
	if !found {
		return Grant{}, fmt.Errorf("invalid grant %q: want <access>:<store-pattern>, e.g. read,write:sensors-*", s)
	}
	stores = strings.TrimSpace(stores)
	if stores == "" {
		return Grant{}, fmt.Errorf("invalid grant %q: store pattern is empty", s)
	}

	var classes []Access
	seen := make(map[Access]bool)
	for _, part := range strings.Split(access, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		a, err := ParseAccess(part)
		if err != nil {
			return Grant{}, fmt.Errorf("invalid grant %q: %w", s, err)
		}
		if !seen[a] {
			seen[a] = true
			classes = append(classes, a)
		}
	}
	if len(classes) == 0 {
		return Grant{}, fmt.Errorf("invalid grant %q: no access classes given", s)
	}

	g := Grant{Stores: stores, Access: classes}
	if err := g.Validate(); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// String renders a grant in the same syntax ParseGrant accepts.
func (g Grant) String() string {
	parts := make([]string, 0, len(g.Access))
	for _, a := range g.Access {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, ",") + ":" + g.Stores
}
