// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The migration contract (issue #138): every key that authenticated
// before the upgrade must authenticate IDENTICALLY after it, with no
// reconfiguration by any client. A silent failure here locks every
// existing deployment out of every store at once, so these tests are
// deliberately concrete about what "identically" means.

// writeLegacyKeyFile creates a pre-#138 <store>/keys.json holding the
// given plaintext keys, hashed the way the old code hashed them.
func writeLegacyKeyFile(t *testing.T, base, storeName, linkedSource string, plaintexts ...string) {
	t.Helper()
	dir := filepath.Join(base, storeName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	type legacyEntry struct {
		ID        string `json:"id"`
		Hash      string `json:"hash"`
		CreatedAt string `json:"created_at"`
		Note      string `json:"note"`
	}
	kf := struct {
		StoreName    string        `json:"store_name"`
		LinkedSource string        `json:"linked_source,omitempty"`
		Keys         []legacyEntry `json:"keys"`
	}{StoreName: storeName, LinkedSource: linkedSource}

	for _, p := range plaintexts {
		kf.Keys = append(kf.Keys, legacyEntry{
			// The legacy ID was fullKey[8:16].
			ID:        p[len(KeyPrefix) : len(KeyPrefix)+8],
			Hash:      hashKey(p),
			CreatedAt: "2026-01-15T10:30:00Z",
			Note:      "legacy",
		})
	}

	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// legacyKey builds a plaintext key of the shape the old code generated.
func legacyKey(suffix string) string {
	return KeyPrefix + "aaaaaaaa-bbbb-cccc-dddd-" + suffix
}

// TestMigrationPreservesFullStoreAccess is the core contract: a legacy
// store key could do everything on its own store — read, write, and
// alert/rollup CRUD — and must still be able to afterwards.
func TestMigrationPreservesFullStoreAccess(t *testing.T) {
	base := t.TempDir()
	key := legacyKey("000000000001")
	writeLegacyKeyFile(t, base, "sensors", "", key)

	m := NewManager(base)
	n, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated %d keys, want 1", n)
	}

	for _, access := range AllAccess {
		if _, err := m.Authorize("sensors", key, access); err != nil {
			t.Errorf("legacy key lost %s access after migration: %v", access, err)
		}
	}
}

// TestMigrationScopesToItsOwnStore: a legacy key was only ever valid on
// its own store, and must not become a skeleton key for the server.
func TestMigrationScopesToItsOwnStore(t *testing.T) {
	base := t.TempDir()
	sensorsKey := legacyKey("000000000001")
	logsKey := legacyKey("000000000002")
	writeLegacyKeyFile(t, base, "sensors", "", sensorsKey)
	writeLegacyKeyFile(t, base, "logs", "", logsKey)

	m := NewManager(base)
	if _, err := m.MigrateLegacyKeys(); err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}

	if _, err := m.Authorize("logs", sensorsKey, AccessRead); err != ErrForbidden {
		t.Errorf("sensors key reached logs after migration: %v", err)
	}
	if _, err := m.Authorize("sensors", logsKey, AccessRead); err != ErrForbidden {
		t.Errorf("logs key reached sensors after migration: %v", err)
	}
}

// TestMigrationCarriesLinkedStores: a rollup target linked to its source
// held no keys of its own and validated against the source. After
// migration the source's key must still reach the target — via a grant
// rather than a link.
func TestMigrationCarriesLinkedStores(t *testing.T) {
	base := t.TempDir()
	srcKey := legacyKey("000000000001")
	writeLegacyKeyFile(t, base, "source", "", srcKey)
	writeLegacyKeyFile(t, base, "target-1h", "source") // linked, no keys

	m := NewManager(base)
	if _, err := m.MigrateLegacyKeys(); err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}

	if _, err := m.Authorize("source", srcKey, AccessRead); err != nil {
		t.Errorf("source key lost access to its own store: %v", err)
	}
	if _, err := m.Authorize("target-1h", srcKey, AccessRead); err != nil {
		t.Errorf("source key lost access to its linked rollup target: %v", err)
	}
}

// TestMigrationIsIdempotent: booting repeatedly must not duplicate keys.
func TestMigrationIsIdempotent(t *testing.T) {
	base := t.TempDir()
	key := legacyKey("000000000001")
	writeLegacyKeyFile(t, base, "sensors", "", key)

	m := NewManager(base)
	first, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	second, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if first != 1 || second != 0 {
		t.Errorf("migrations imported %d then %d, want 1 then 0", first, second)
	}

	keys, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("registry holds %d keys after two migrations, want 1", len(keys))
	}
}

// TestMigrationSurvivesCorruptStore: one unreadable key file must not
// strand every other store's keys.
func TestMigrationSurvivesCorruptStore(t *testing.T) {
	base := t.TempDir()
	goodKey := legacyKey("000000000001")
	writeLegacyKeyFile(t, base, "good", "", goodKey)

	bad := filepath.Join(base, "corrupt")
	if err := os.MkdirAll(bad, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, KeyFileName), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(base)
	n, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("a corrupt key file aborted the whole migration: %v", err)
	}
	if n != 1 {
		t.Errorf("migrated %d keys, want 1", n)
	}
	if _, err := m.Authorize("good", goodKey, AccessRead); err != nil {
		t.Errorf("healthy store's key was stranded by a corrupt sibling: %v", err)
	}
}

// TestMigrationLeavesLegacyFilesInPlace: the old files are the only
// other copy of the hashes and make a downgrade work, so migration must
// not delete them.
func TestMigrationLeavesLegacyFilesInPlace(t *testing.T) {
	base := t.TempDir()
	writeLegacyKeyFile(t, base, "sensors", "", legacyKey("000000000001"))

	m := NewManager(base)
	if _, err := m.MigrateLegacyKeys(); err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}

	if _, err := os.Stat(filepath.Join(base, "sensors", KeyFileName)); err != nil {
		t.Errorf("migration deleted the legacy key file: %v", err)
	}
}

// TestMigrationNoopOnFreshInstall: no stores, no legacy files, no error.
func TestMigrationNoopOnFreshInstall(t *testing.T) {
	m := NewManager(t.TempDir())
	n, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("MigrateLegacyKeys on a fresh install: %v", err)
	}
	if n != 0 {
		t.Errorf("migrated %d keys on a fresh install, want 0", n)
	}
}

// TestMigrationPreservesMultipleKeysPerStore: the legacy model allowed
// several keys in one file; all must come across.
func TestMigrationPreservesMultipleKeysPerStore(t *testing.T) {
	base := t.TempDir()
	k1 := legacyKey("000000000001")
	k2 := legacyKey("000000000002")
	writeLegacyKeyFile(t, base, "sensors", "", k1, k2)

	m := NewManager(base)
	n, err := m.MigrateLegacyKeys()
	if err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated %d keys, want 2", n)
	}
	for i, k := range []string{k1, k2} {
		if _, err := m.Authorize("sensors", k, AccessWrite); err != nil {
			t.Errorf("key %d lost access after migration: %v", i+1, err)
		}
	}
}

// TestMigrationDoesNotClobberExistingRegistry: a hand-added scoped key
// must survive a migration run.
func TestMigrationDoesNotClobberExistingRegistry(t *testing.T) {
	base := t.TempDir()
	m := NewManager(base)

	scoped, _, err := m.Create("dashboard", []Grant{{Stores: "*", Access: []Access{AccessRead}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	legacy := legacyKey("000000000001")
	writeLegacyKeyFile(t, base, "sensors", "", legacy)
	if _, err := m.MigrateLegacyKeys(); err != nil {
		t.Fatalf("MigrateLegacyKeys: %v", err)
	}

	if _, err := m.Authorize("anything", scoped, AccessRead); err != nil {
		t.Errorf("pre-existing scoped key was clobbered by migration: %v", err)
	}
	if _, err := m.Authorize("sensors", legacy, AccessRead); err != nil {
		t.Errorf("legacy key not imported alongside existing registry: %v", err)
	}
}
