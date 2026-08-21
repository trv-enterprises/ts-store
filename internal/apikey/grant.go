// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"fmt"
	"strings"
)

// Access is one class of operation a grant may permit. The classes are
// deliberately coarse — authorization is store-granular, never per-series
// or per-field (issue #138).
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
	// AccessAdmin is store LIFECYCLE: creating stores today, and (phase 2
	// of issue #157) deleting and resetting them. It deliberately grants
	// NO access to data or configuration — an admin-only key can bring a
	// store into existence and never read, write, or configure it. Paired
	// with a store pattern it expresses something the server-wide admin
	// key cannot: "may create stores named sensors-*, and nothing else".
	AccessAdmin Access = "admin"
)

// AllAccess is the set granted to a store's own initial key and to
// legacy keys imported by the pre-#138 migration: read, write, manage on
// exactly that store.
//
// AccessAdmin is deliberately NOT a member. Adding it would silently give
// every existing store key — and every migrated legacy key — the ability
// to create stores, which no key has today. Lifecycle authority has to be
// granted explicitly (issue #157).
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
	case AccessAdmin:
		return AccessAdmin, nil
	}
	return "", fmt.Errorf("unknown access class %q (want read, write, manage, or admin)", s)
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

// CoversPattern reports whether this grant's store pattern covers every
// store the other pattern could match — i.e. other is a subset of g.Stores.
//
// This is pattern containment, not equality, and it is the guard that keeps
// API key minting from escalating privilege (issue #176). Comparing only
// access CLASSES would let a key holding admin:sensors-* mint admin:*, which
// is precisely the escalation the constraint exists to prevent.
//
//	"*"          covers everything
//	"sensors-*"  covers "sensors-*", "sensors-x", "sensors-x-*"
//	             but NOT "*" or "sens-*" (which match names it cannot)
//	"sensors-x"  covers only itself
func (g Grant) CoversPattern(other string) bool {
	if g.Stores == "*" {
		return true
	}
	if !strings.HasSuffix(g.Stores, "*") {
		// An exact-name grant covers only that exact name. A wildcard
		// other could match names outside it, so it is never covered.
		return g.Stores == other
	}
	prefix := strings.TrimSuffix(g.Stores, "*")
	// other must be constrained to at least this prefix. That holds for a
	// literal name starting with it, and for a glob whose own prefix starts
	// with it ("sensors-a*" ⊂ "sensors-*"). A bare "*" never qualifies,
	// since TrimSuffix leaves "" which cannot start with a non-empty prefix.
	return strings.HasPrefix(strings.TrimSuffix(other, "*"), prefix)
}

// PermitsGrant reports whether the grants held by a key are sufficient to
// ISSUE the requested grant: every requested access class must be covered by
// some held grant whose store pattern also covers the requested pattern.
//
// Note the check is per class rather than per grant — a key holding
// read:sensors-* plus write:sensors-* may mint read,write:sensors-x, which no
// single held grant covers alone.
func PermitsGrant(held []Grant, requested Grant) bool {
	if strings.TrimSpace(requested.Stores) == "" || len(requested.Access) == 0 {
		return false
	}
	for _, want := range requested.Access {
		covered := false
		for _, h := range held {
			if !h.CoversPattern(requested.Stores) {
				continue
			}
			for _, a := range h.Access {
				if a == want {
					covered = true
					break
				}
			}
			if covered {
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}
