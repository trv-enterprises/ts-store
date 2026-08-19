// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"strings"
	"testing"
)

func TestGrantMatchesStore(t *testing.T) {
	tests := []struct {
		pattern string
		store   string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"sensors", "sensors", true},
		{"sensors", "sensors-disks", false},
		{"sensors", "Sensors", false}, // case-sensitive, like store names
		{"sensors-*", "sensors-disks", true},
		{"sensors-*", "sensors-", true},
		{"sensors-*", "sensors", false}, // prefix requires the separator
		{"sensors-*", "nas-disks", false},
	}
	for _, tt := range tests {
		g := Grant{Stores: tt.pattern, Access: []Access{AccessRead}}
		if got := g.MatchesStore(tt.store); got != tt.want {
			t.Errorf("Grant{%q}.MatchesStore(%q) = %v, want %v", tt.pattern, tt.store, got, tt.want)
		}
	}
}

// TestAccessClassesAreIndependent: manage does not imply read. A key
// that administers alerts without reading data is a real configuration,
// so the classes are flags rather than a hierarchy.
func TestAccessClassesAreIndependent(t *testing.T) {
	g := Grant{Stores: "s", Access: []Access{AccessManage}}
	if !g.Permits("s", AccessManage) {
		t.Error("manage grant does not permit manage")
	}
	if g.Permits("s", AccessRead) {
		t.Error("manage grant implied read")
	}
	if g.Permits("s", AccessWrite) {
		t.Error("manage grant implied write")
	}
}

func TestParseGrant(t *testing.T) {
	tests := []struct {
		in      string
		wantErr string
		stores  string
		access  []Access
	}{
		{in: "read:*", stores: "*", access: []Access{AccessRead}},
		{in: "read,write:sensors-*", stores: "sensors-*", access: []Access{AccessRead, AccessWrite}},
		{in: "read,write,manage:home", stores: "home", access: []Access{AccessRead, AccessWrite, AccessManage}},
		{in: "admin:sensors-*", stores: "sensors-*", access: []Access{AccessAdmin}},
		{in: " read , write : home ", stores: "home", access: []Access{AccessRead, AccessWrite}},
		// Duplicates collapse rather than erroring — harmless intent.
		{in: "read,read:home", stores: "home", access: []Access{AccessRead}},

		{in: "read", wantErr: "want <access>:<store-pattern>"},
		{in: "read:", wantErr: "store pattern is empty"},
		{in: ":home", wantErr: "no access classes given"},
		// "admin" became a real class in #157; keep an unknown-class case
		// so that error path stays covered.
		{in: "owner:home", wantErr: "unknown access class"},
		{in: "read:sen*ors", wantErr: "trailing wildcard"},
	}

	for _, tt := range tests {
		g, err := ParseGrant(tt.in)
		if tt.wantErr != "" {
			if err == nil {
				t.Errorf("ParseGrant(%q): want error containing %q, got nil", tt.in, tt.wantErr)
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseGrant(%q) error = %q, want substring %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseGrant(%q): %v", tt.in, err)
			continue
		}
		if g.Stores != tt.stores {
			t.Errorf("ParseGrant(%q).Stores = %q, want %q", tt.in, g.Stores, tt.stores)
		}
		if len(g.Access) != len(tt.access) {
			t.Errorf("ParseGrant(%q).Access = %v, want %v", tt.in, g.Access, tt.access)
			continue
		}
		for i := range g.Access {
			if g.Access[i] != tt.access[i] {
				t.Errorf("ParseGrant(%q).Access = %v, want %v", tt.in, g.Access, tt.access)
				break
			}
		}
	}
}

// TestGrantRoundTrip: String() output must parse back to the same grant,
// since `key list` prints it and users copy it into `key create`.
func TestGrantRoundTrip(t *testing.T) {
	for _, spec := range []string{"read:*", "read,write:sensors-*", "read,write,manage:home"} {
		g, err := ParseGrant(spec)
		if err != nil {
			t.Fatalf("ParseGrant(%q): %v", spec, err)
		}
		again, err := ParseGrant(g.String())
		if err != nil {
			t.Fatalf("ParseGrant(%q) [round trip]: %v", g.String(), err)
		}
		if again.String() != g.String() {
			t.Errorf("round trip changed %q -> %q", g.String(), again.String())
		}
	}
}

func TestValidateSuppliedKey(t *testing.T) {
	valid := KeyPrefix + "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{name: "valid", key: valid},
		{name: "empty", key: "", wantErr: "required"},
		{name: "no prefix", key: "a1b2c3d4-e5f6-7890-abcd-ef1234567890", wantErr: "must start with"},
		{name: "too short", key: KeyPrefix + "abc", wantErr: "at least"},
		// The pre-#138 ID derivation was fullKey[8:16], which panicked
		// on a key this short. Rejecting it is the guard; deriveKeyID
		// no longer slices, which is the fix.
		{name: "shorter than legacy ID slice", key: "tsstore_", wantErr: "at least"},
		{name: "whitespace", key: KeyPrefix + "a1b2c3d4 e5f6-7890-abcd-ef1234567890", wantErr: "whitespace"},
		{name: "trailing newline", key: valid + "\n", wantErr: "whitespace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSuppliedKey(tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSuppliedKey(%q) = %v, want nil", tt.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSuppliedKey(%q) = nil, want error containing %q", tt.key, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateSuppliedKey(%q) = %q, want substring %q", tt.key, err, tt.wantErr)
			}
		})
	}
}

// TestDeriveKeyIDNeverPanics is the regression guard for the pre-#138
// fullKey[8:16] slice, which panicked on short input.
func TestDeriveKeyIDNeverPanics(t *testing.T) {
	for _, k := range []string{"", "a", "tsstore_", "tsstore_abc", strings.Repeat("x", 500)} {
		id := deriveKeyID(k)
		if len(id) != 8 {
			t.Errorf("deriveKeyID(%q) = %q, want 8 chars", k, id)
		}
	}
}

func TestAdoptOperatorMintedKey(t *testing.T) {
	m := NewManager(t.TempDir())
	supplied := KeyPrefix + "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	entry, err := m.Adopt(supplied, "from 1Password", fullGrant("nas-disks"))
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if entry.ID == "" {
		t.Error("adopted key has no ID")
	}

	// The supplied key authenticates exactly like a generated one.
	if _, err := m.Authorize("nas-disks", supplied, AccessWrite); err != nil {
		t.Errorf("adopted key does not authenticate: %v", err)
	}
	// And is scoped by its grants like any other.
	if _, err := m.Authorize("other", supplied, AccessRead); err != ErrForbidden {
		t.Errorf("adopted key escaped its grants: %v", err)
	}
}

func TestAdoptRejectsMalformedKeys(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, bad := range []string{"", "hunter2", "no-prefix-but-long-enough-to-pass-length", "tsstore_short"} {
		if _, err := m.Adopt(bad, "", fullGrant("s")); err == nil {
			t.Errorf("Adopt(%q) succeeded, want rejection", bad)
		}
	}
}

func TestAdoptRejectsDuplicate(t *testing.T) {
	m := NewManager(t.TempDir())
	supplied := KeyPrefix + "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	if _, err := m.Adopt(supplied, "first", fullGrant("a")); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := m.Adopt(supplied, "second", fullGrant("b")); err != ErrKeyExists {
		t.Errorf("duplicate Adopt = %v, want ErrKeyExists", err)
	}
}

func TestCreateRequiresGrants(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, _, err := m.Create("no grants", nil); err == nil {
		t.Error("Create with no grants succeeded, want rejection")
	}
	if _, _, err := m.Create("bad grant", []Grant{{Stores: "", Access: []Access{AccessRead}}}); err == nil {
		t.Error("Create with an empty store pattern succeeded, want rejection")
	}
}

func TestWildcardGrantReachesEveryStore(t *testing.T) {
	m := NewManager(t.TempDir())
	key, _, err := m.Create("dashboard", []Grant{{Stores: "*", Access: []Access{AccessRead}}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, store := range []string{"a", "b", "sensors-disks", "anything-at-all"} {
		if _, err := m.Authorize(store, key, AccessRead); err != nil {
			t.Errorf("wildcard read key denied on %q: %v", store, err)
		}
		if _, err := m.Authorize(store, key, AccessWrite); err != ErrForbidden {
			t.Errorf("read-only wildcard key got write on %q: %v", store, err)
		}
	}
}

func TestPrefixGrantScoping(t *testing.T) {
	m := NewManager(t.TempDir())
	key, _, err := m.Create("collector", []Grant{{
		Stores: "sensors-*", Access: []Access{AccessRead, AccessWrite},
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := m.Authorize("sensors-disks", key, AccessWrite); err != nil {
		t.Errorf("prefix grant denied a matching store: %v", err)
	}
	if _, err := m.Authorize("logs", key, AccessRead); err != ErrForbidden {
		t.Errorf("prefix grant leaked to a non-matching store: %v", err)
	}
	if _, err := m.Authorize("sensors-disks", key, AccessManage); err != ErrForbidden {
		t.Errorf("prefix grant leaked manage access: %v", err)
	}
}

func TestReadableStoresFilters(t *testing.T) {
	m := NewManager(t.TempDir())
	key, _, err := m.Create("k", []Grant{
		{Stores: "sensors-*", Access: []Access{AccessRead}},
		{Stores: "logs", Access: []Access{AccessWrite}}, // write only: not readable
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rk, err := m.Resolve(key)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := rk.ReadableStores([]string{"sensors-a", "sensors-b", "logs", "other"})
	want := map[string]bool{"sensors-a": true, "sensors-b": true}
	if len(got) != len(want) {
		t.Fatalf("ReadableStores = %v, want %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("ReadableStores included %q", n)
		}
	}
}

// TestRegistryRejectsNewerVersion: under-enforcing grants we do not
// understand would fail open, so a future-versioned registry is an
// error rather than a best-effort parse.
func TestRegistryRejectsNewerVersion(t *testing.T) {
	base := t.TempDir()
	m := NewManager(base)
	if _, _, err := m.Create("k", fullGrant("s")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	reg, err := m.loadRegistryLocked()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg.Version = RegistryVersion + 1
	// Write directly; saveRegistryLocked would stamp the current version.
	data, err := marshalRegistry(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(m.registryPath(), data, 0600); err != nil {
		t.Fatal(err)
	}

	fresh := NewManager(base)
	if _, err := fresh.List(); err == nil {
		t.Error("a newer-versioned registry was accepted; grants could be under-enforced")
	}
}
