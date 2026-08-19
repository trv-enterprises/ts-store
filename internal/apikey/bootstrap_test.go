// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"strings"
	"testing"
)

func bootstrapEntry(t *testing.T, m *Manager) *KeyEntry {
	t.Helper()
	keys, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *KeyEntry
	for i := range keys {
		if strings.HasPrefix(keys[i].Note, "Bootstrap key") {
			if found != nil {
				t.Fatalf("more than one bootstrap key in the registry")
			}
			k := keys[i]
			found = &k
		}
	}
	return found
}

// TestEnsureBootstrapGeneratesOnEmptyRegistry: with no config key and no
// registry, the server mints its own and hands back the plaintext once —
// the first-boot experience that needs no configuration at all.
func TestEnsureBootstrapGeneratesOnEmptyRegistry(t *testing.T) {
	m := NewManager(t.TempDir())

	res, err := m.EnsureBootstrap("")
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	if res.Action != "generated" {
		t.Errorf("Action = %q, want generated", res.Action)
	}
	if !ValidateKeyFormat(res.Plaintext) {
		t.Errorf("generated key has the wrong shape: %q", res.Plaintext)
	}

	// It must actually work, with full authority.
	key, err := m.Resolve(res.Plaintext)
	if err != nil {
		t.Fatalf("Resolve generated bootstrap key: %v", err)
	}
	for _, a := range []Access{AccessRead, AccessWrite, AccessManage, AccessAdmin} {
		if !key.Permits("any-store", a) {
			t.Errorf("bootstrap key lacks %q on an arbitrary store", a)
		}
	}
}

// TestEnsureBootstrapPreservesWhenConfigEmpty is the property that lets a
// deployment DROP TSSTORE_ADMIN_KEY after first boot: a restart with no
// config key must keep the existing key working, not mint a second one.
func TestEnsureBootstrapPreservesWhenConfigEmpty(t *testing.T) {
	m := NewManager(t.TempDir())

	first, err := m.EnsureBootstrap("")
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}

	for i := 0; i < 3; i++ {
		res, err := m.EnsureBootstrap("")
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if res.Action != "preserved" {
			t.Errorf("restart %d: Action = %q, want preserved", i, res.Action)
		}
		if res.Plaintext != "" {
			t.Error("preserve must not return a plaintext — the registry holds only a hash")
		}
	}

	if _, err := m.Resolve(first.Plaintext); err != nil {
		t.Errorf("original bootstrap key stopped working after restarts: %v", err)
	}
	if bootstrapEntry(t, m) == nil {
		t.Error("bootstrap key vanished")
	}
}

// TestEnsureBootstrapAdoptsAndRotates: supplying a config key adopts it, and
// supplying a DIFFERENT one rotates — replacing the entry rather than
// accumulating one per boot, and retiring the old key.
func TestEnsureBootstrapAdoptsAndRotates(t *testing.T) {
	m := NewManager(t.TempDir())

	oldKey := KeyPrefix + "11111111-2222-3333-4444-555555555555"
	newKey := KeyPrefix + "66666666-7777-8888-9999-000000000000"

	res, err := m.EnsureBootstrap(oldKey)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Action != "adopted" {
		t.Errorf("Action = %q, want adopted", res.Action)
	}
	if res.Plaintext != "" {
		t.Error("adopt must not echo the plaintext — the caller already has it")
	}
	if _, err := m.Resolve(oldKey); err != nil {
		t.Fatalf("adopted key does not resolve: %v", err)
	}

	// Same key again is an ordinary restart, not a rotation.
	if res, err = m.EnsureBootstrap(oldKey); err != nil || res.Action != "preserved" {
		t.Errorf("re-supplying the same key: action=%q err=%v, want preserved", res.Action, err)
	}

	// A different key rotates.
	if res, err = m.EnsureBootstrap(newKey); err != nil {
		t.Fatalf("rotate: %v", err)
	} else if res.Action != "adopted" {
		t.Errorf("rotate Action = %q, want adopted", res.Action)
	}
	if _, err := m.Resolve(newKey); err != nil {
		t.Errorf("rotated-in key does not resolve: %v", err)
	}
	if _, err := m.Resolve(oldKey); err == nil {
		t.Error("the old bootstrap key still resolves after rotation — it must be replaced, not kept")
	}
	if bootstrapEntry(t, m) == nil {
		t.Error("no bootstrap key after rotation")
	}
}

// TestEnsureBootstrapLeavesOperatorKeysAlone: rotation replaces only the
// entry carrying the Bootstrap field. This is why Bootstrap is a dedicated
// field and not a magic note — deleting a user's key on restart would be a
// data-loss bug that only shows up in production.
func TestEnsureBootstrapLeavesOperatorKeysAlone(t *testing.T) {
	m := NewManager(t.TempDir())

	if _, err := m.EnsureBootstrap(KeyPrefix + "11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// A user key that deliberately imitates the bootstrap note.
	userKey, _, err := m.Create("Bootstrap key (adopted from config)", []Grant{{
		Stores: "sensors-*", Access: []Access{AccessRead},
	}})
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}

	if _, err := m.EnsureBootstrap(KeyPrefix + "66666666-7777-8888-9999-000000000000"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := m.Resolve(userKey); err != nil {
		t.Errorf("rotation destroyed an operator key that merely shared the note: %v", err)
	}
}

// TestEnsureBootstrapRefusesExistingRegistryKey: adopting a key already
// registered as an ordinary scoped key would silently promote it to full
// authority — privilege escalation by configuration accident.
func TestEnsureBootstrapRefusesExistingRegistryKey(t *testing.T) {
	m := NewManager(t.TempDir())

	scoped := KeyPrefix + "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := m.Adopt(scoped, "dashboard", []Grant{{
		Stores: "sensors-*", Access: []Access{AccessRead},
	}}); err != nil {
		t.Fatalf("adopt scoped key: %v", err)
	}

	if _, err := m.EnsureBootstrap(scoped); err == nil {
		t.Fatal("adopting an existing scoped key as bootstrap was allowed — silent privilege escalation")
	}

	// And the scoped key must still be exactly as scoped as before.
	key, err := m.Resolve(scoped)
	if err != nil {
		t.Fatalf("scoped key stopped resolving: %v", err)
	}
	if key.Permits("billing", AccessAdmin) {
		t.Error("the scoped key gained authority it never had")
	}
}

// TestEnsureBootstrapRejectsMalformedConfigKey: a too-short or wrongly
// prefixed admin key fails loudly at startup rather than being adopted into
// the registry as a weak credential.
func TestEnsureBootstrapRejectsMalformedConfigKey(t *testing.T) {
	m := NewManager(t.TempDir())

	for _, bad := range []string{"short", "no-prefix-but-long-enough-to-pass-length", KeyPrefix + "tiny"} {
		if _, err := m.EnsureBootstrap(bad); err == nil {
			t.Errorf("malformed config key %q was accepted", bad)
		}
	}
}
