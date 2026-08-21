// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import "testing"

// TestCoversPatternContainment pins the store-pattern subset rule. The cases
// that matter are the escalations: a narrowly-scoped pattern must never be
// judged to cover a broader one.
func TestCoversPatternContainment(t *testing.T) {
	tests := []struct {
		held, requested string
		want            bool
	}{
		// Wildcard holds everything.
		{"*", "*", true},
		{"*", "sensors-*", true},
		{"*", "anything", true},

		// Prefix glob covers itself, narrower globs, and matching names.
		{"sensors-*", "sensors-*", true},
		{"sensors-*", "sensors-garage", true},
		{"sensors-*", "sensors-a*", true},
		{"sensors-*", "sensors-", true},

		// ESCALATIONS — each of these must be refused.
		{"sensors-*", "*", false},              // narrow -> everything
		{"sensors-*", "billing", false},        // outside the prefix
		{"sensors-*", "sens-*", false},         // broader glob matching non-sensors names
		{"sensors-*", "sensor*", false},        // one char short: matches "sensorX"
		{"sensors-garage", "*", false},         // exact -> everything
		{"sensors-garage", "sensors-*", false}, // exact -> glob
		{"sensors-garage", "sensors-shed", false},

		// Exact covers only itself.
		{"home", "home", true},
		{"home", "home-x", false},
	}

	for _, tt := range tests {
		g := Grant{Stores: tt.held, Access: []Access{AccessRead}}
		if got := g.CoversPattern(tt.requested); got != tt.want {
			t.Errorf("Grant{%q}.CoversPattern(%q) = %v, want %v", tt.held, tt.requested, got, tt.want)
		}
	}
}

// TestPermitsGrantNoEscalation is the security property behind the key API:
// a key may issue no more authority than it already holds (issue #176).
func TestPermitsGrantNoEscalation(t *testing.T) {
	// The motivating case: an admin-only provisioner. It can create stores
	// in its namespace and must NOT be able to mint itself data access.
	provisioner := []Grant{{Stores: "sensors-*", Access: []Access{AccessAdmin}}}

	if !PermitsGrant(provisioner, Grant{Stores: "sensors-garage", Access: []Access{AccessAdmin}}) {
		t.Error("admin:sensors-* should be able to issue admin:sensors-garage")
	}
	if PermitsGrant(provisioner, Grant{Stores: "sensors-garage", Access: []Access{AccessRead}}) {
		t.Error("ESCALATION: admin-only key issued read access — admin conveys no data access (#157)")
	}
	if PermitsGrant(provisioner, Grant{Stores: "*", Access: []Access{AccessAdmin}}) {
		t.Error("ESCALATION: admin:sensors-* issued admin:* — narrower pattern must not widen")
	}

	// A read-only dashboard key cannot mint write or manage.
	readOnly := []Grant{{Stores: "*", Access: []Access{AccessRead}}}
	if !PermitsGrant(readOnly, Grant{Stores: "anything", Access: []Access{AccessRead}}) {
		t.Error("read:* should be able to issue read on any store")
	}
	for _, a := range []Access{AccessWrite, AccessManage, AccessAdmin} {
		if PermitsGrant(readOnly, Grant{Stores: "anything", Access: []Access{a}}) {
			t.Errorf("ESCALATION: read:* issued %q", a)
		}
	}

	// Classes combine ACROSS held grants: neither grant alone covers
	// read,write on sensors-x, but together they do.
	split := []Grant{
		{Stores: "sensors-*", Access: []Access{AccessRead}},
		{Stores: "sensors-*", Access: []Access{AccessWrite}},
	}
	if !PermitsGrant(split, Grant{Stores: "sensors-x", Access: []Access{AccessRead, AccessWrite}}) {
		t.Error("held classes should combine across grants")
	}
	// ...but a class held only on a DIFFERENT pattern does not transfer.
	mixed := []Grant{
		{Stores: "sensors-*", Access: []Access{AccessRead}},
		{Stores: "billing", Access: []Access{AccessWrite}},
	}
	if PermitsGrant(mixed, Grant{Stores: "sensors-x", Access: []Access{AccessRead, AccessWrite}}) {
		t.Error("ESCALATION: write held only on billing was applied to sensors-x")
	}

	// The bootstrap key holds everything, so it may issue anything — that
	// is what makes it the bootstrap key.
	bootstrap := []Grant{{Stores: "*", Access: []Access{AccessRead, AccessWrite, AccessManage, AccessAdmin}}}
	for _, req := range []Grant{
		{Stores: "*", Access: []Access{AccessRead, AccessWrite, AccessManage, AccessAdmin}},
		{Stores: "sensors-*", Access: []Access{AccessAdmin}},
		{Stores: "one-store", Access: []Access{AccessManage}},
	} {
		if !PermitsGrant(bootstrap, req) {
			t.Errorf("bootstrap key refused issuing %v", req)
		}
	}
}

// TestPermitsGrantRejectsMalformed: an empty pattern or no classes is not a
// grant, and must never be treated as permitted.
func TestPermitsGrantRejectsMalformed(t *testing.T) {
	held := []Grant{{Stores: "*", Access: AllAccess}}
	if PermitsGrant(held, Grant{Stores: "", Access: []Access{AccessRead}}) {
		t.Error("empty store pattern was permitted")
	}
	if PermitsGrant(held, Grant{Stores: "x"}) {
		t.Error("grant with no access classes was permitted")
	}
	if PermitsGrant(nil, Grant{Stores: "x", Access: []Access{AccessRead}}) {
		t.Error("a key holding no grants issued one")
	}
}
