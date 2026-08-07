// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import "testing"

// The linked-key-file mechanism retired in #138: a rollup target no
// longer defers validation to its source, it gets an explicit grant on
// the source's key instead. These tests pin the BEHAVIOR that mechanism
// provided — a client holding the source key can reach the derived
// target — which must survive the change even though the implementation
// did not.

func TestExtendGrantsReachesRollupTarget(t *testing.T) {
	m := NewManager(t.TempDir())

	srcKey, _, err := m.Create("initial", fullGrant("source"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Before the target exists, the source key has no business there.
	if _, err := m.Authorize("target", srcKey, AccessRead); err != ErrForbidden {
		t.Errorf("source key reached target before it was granted: %v", err)
	}

	// Auto-creating a rollup target extends the source's keys to it.
	if err := m.ExtendGrants("source", "target"); err != nil {
		t.Fatalf("ExtendGrants: %v", err)
	}

	for _, access := range AllAccess {
		if _, err := m.Authorize("target", srcKey, access); err != nil {
			t.Errorf("source key should have %s on the target: %v", access, err)
		}
	}

	// A bogus key still does not.
	if _, err := m.Authorize("target", "tsstore_00000000-0000-0000-0000-000000000000", AccessRead); err != ErrInvalidKey {
		t.Errorf("bogus key: want ErrInvalidKey, got %v", err)
	}
}

// TestExtendGrantsPreservesAccessClasses: a read-only key on the source
// must not gain write on the target. The old linked-file mechanism
// couldn't express this — it shared the whole key file — so this is
// strictly tighter than the behavior it replaced.
func TestExtendGrantsPreservesAccessClasses(t *testing.T) {
	m := NewManager(t.TempDir())

	roKey, _, err := m.Create("read-only", []Grant{{Stores: "source", Access: []Access{AccessRead}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.ExtendGrants("source", "target"); err != nil {
		t.Fatalf("ExtendGrants: %v", err)
	}

	if _, err := m.Authorize("target", roKey, AccessRead); err != nil {
		t.Errorf("read-only key should read the target: %v", err)
	}
	if _, err := m.Authorize("target", roKey, AccessWrite); err != ErrForbidden {
		t.Errorf("read-only key gained write on the target: %v", err)
	}
}

// TestExtendGrantsSkipsUnrelatedKeys: a key with no access to the source
// must not be handed the target.
func TestExtendGrantsSkipsUnrelatedKeys(t *testing.T) {
	m := NewManager(t.TempDir())

	otherKey, _, err := m.Create("elsewhere", fullGrant("unrelated"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := m.Create("source key", fullGrant("source")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.ExtendGrants("source", "target"); err != nil {
		t.Fatalf("ExtendGrants: %v", err)
	}

	if _, err := m.Authorize("target", otherKey, AccessRead); err != ErrForbidden {
		t.Errorf("unrelated key reached the target: %v", err)
	}
}

// TestExtendGrantsIsIdempotent: re-running (a rollup target recreated
// with force_recreate) must not pile up duplicate grants.
func TestExtendGrantsIsIdempotent(t *testing.T) {
	m := NewManager(t.TempDir())

	_, entry, err := m.Create("k", fullGrant("source"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := m.ExtendGrants("source", "target"); err != nil {
			t.Fatalf("ExtendGrants: %v", err)
		}
	}

	keys, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if k.ID != entry.ID {
			continue
		}
		if len(k.Grants) != 2 {
			t.Errorf("expected 2 grants (source + target), got %d: %v", len(k.Grants), k.Grants)
		}
	}
}

// TestExtendGrantsNoopUnderWildcard: a key already covering the target
// via a wildcard needs no additional grant.
func TestExtendGrantsNoopUnderWildcard(t *testing.T) {
	m := NewManager(t.TempDir())

	_, entry, err := m.Create("wild", []Grant{{Stores: "*", Access: append([]Access(nil), AllAccess...)}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.ExtendGrants("source", "target"); err != nil {
		t.Fatalf("ExtendGrants: %v", err)
	}

	keys, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if k.ID == entry.ID && len(k.Grants) != 1 {
			t.Errorf("wildcard key gained a redundant grant: %v", k.Grants)
		}
	}
}

// TestRevokeStoreGrantsDropsExactNotWildcard: deleting a store drops
// grants naming it, but a wildcard describes a namespace and must
// survive.
func TestRevokeStoreGrantsDropsExactNotWildcard(t *testing.T) {
	m := NewManager(t.TempDir())

	exactKey, _, err := m.Create("exact", fullGrant("doomed"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wildKey, _, err := m.Create("wild", []Grant{{Stores: "*", Access: []Access{AccessRead}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.RevokeStoreGrants("doomed"); err != nil {
		t.Fatalf("RevokeStoreGrants: %v", err)
	}

	// The exact-grant key had only that store, so it is gone entirely.
	if _, err := m.Resolve(exactKey); err != ErrInvalidKey {
		t.Errorf("key with only the deleted store's grant survived: %v", err)
	}
	// The wildcard key is untouched.
	if _, err := m.Authorize("something-else", wildKey, AccessRead); err != nil {
		t.Errorf("wildcard key was damaged by a store deletion: %v", err)
	}
}
