// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package apikey handles API key generation, hashing, and validation.
//
// Keys live in a single central registry (keys.registry.json) and carry
// grants — (store pattern, access classes) pairs — rather than being
// scoped to one store by which directory they sit in (issue #138).
// Pre-#138 per-store keys.json files are imported automatically by
// MigrateLegacyKeys on first boot; see migrate.go.
package apikey

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// KeyPrefix is prepended to all generated API keys.
	KeyPrefix = "tsstore_"
	// KeyFileName is the legacy per-store key file. Read once by the
	// migration, never written.
	KeyFileName = "keys.json"

	// MinKeyLength is the floor for an operator-supplied key, matching
	// the admin key's floor. Generated keys are 44 chars, so this only
	// binds on adopted keys.
	MinKeyLength = 20
)

var (
	ErrKeyNotFound    = errors.New("API key not found")
	ErrInvalidKey     = errors.New("invalid API key")
	ErrKeyFileCorrupt = errors.New("key file is corrupt")
	// ErrForbidden means the key is valid but lacks the grant needed for
	// the requested store and access class. Distinct from ErrInvalidKey
	// so callers can answer 403 rather than 401.
	ErrForbidden = errors.New("key lacks required access")
	// ErrKeyExists means the supplied key is already in the registry.
	ErrKeyExists = errors.New("API key already exists")
)

// KeyEntry is the public description of a stored key. It carries no
// plaintext — only the hash, as before.
type KeyEntry struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note"`
	Grants    []Grant   `json:"grants,omitempty"`
}

// cachedRegistry is the parsed registry plus the file identity it came
// from, so reads detect changes written by another process (the key CLI
// writes the registry directly).
type cachedRegistry struct {
	registry *Registry
	// index maps key hash -> key, so a lookup is O(1) rather than a scan
	// over every key on the server.
	index   map[string]*RegistryKey
	modTime time.Time
	size    int64
}

// Manager handles API key operations.
//
// Locking: mu guards basePath-relative state and the cache. Reads take
// the read lock on the fast path (cache warm and the file unchanged) and
// only escalate to the write lock when the registry must be reloaded.
// This matters because every authenticated request on the server now
// goes through one Manager, where pre-#138 the work was spread across
// per-store files.
type Manager struct {
	mu       sync.RWMutex
	basePath string
	regCache *cachedRegistry
}

// NewManager creates a new API key manager rooted at basePath.
func NewManager(basePath string) *Manager {
	return &Manager{basePath: basePath}
}

// Create mints a new key with the given grants and returns the plaintext
// (only returned once) alongside its entry.
func (m *Manager) Create(note string, grants []Grant) (string, *KeyEntry, error) {
	return m.create("", note, grants)
}

// CreateForStore mints a key granting full access to exactly one store —
// the authority a pre-#138 per-store key carried. It is the common case
// for store creation and for tests, and saves callers spelling out the
// all-classes grant every time.
func (m *Manager) CreateForStore(storeName, note string) (string, *KeyEntry, error) {
	return m.Create(note, []Grant{{
		Stores: storeName,
		Access: append([]Access(nil), AllAccess...),
	}})
}

// Adopt registers an operator-minted key instead of generating one, so a
// key can originate in a secrets vault rather than being captured from
// ts-store's output (issue #137, folded into #138). The plaintext is
// hashed and discarded exactly as a generated key would be.
func (m *Manager) Adopt(plaintext, note string, grants []Grant) (*KeyEntry, error) {
	if err := ValidateSuppliedKey(plaintext); err != nil {
		return nil, err
	}
	_, entry, err := m.create(plaintext, note, grants)
	return entry, err
}

// create is the shared mint/adopt path. An empty plaintext means
// "generate one".
func (m *Manager) create(plaintext, note string, grants []Grant) (string, *KeyEntry, error) {
	if len(grants) == 0 {
		return "", nil, fmt.Errorf("at least one grant is required")
	}
	for _, g := range grants {
		if err := g.Validate(); err != nil {
			return "", nil, err
		}
	}

	if plaintext == "" {
		plaintext = KeyPrefix + uuid.New().String()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return "", nil, err
	}

	hash := hashKey(plaintext)
	for _, k := range reg.Keys {
		if k.Hash == hash {
			return "", nil, ErrKeyExists
		}
	}

	entry := RegistryKey{
		ID:        deriveKeyID(plaintext),
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		Note:      note,
		Grants:    grants,
	}
	reg.Keys = append(reg.Keys, entry)

	if err := m.saveRegistryLocked(reg); err != nil {
		return "", nil, err
	}
	m.regCache = nil

	return plaintext, toKeyEntry(entry), nil
}

// Revoke removes a key by ID.
func (m *Manager) Revoke(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return err
	}

	kept := make([]RegistryKey, 0, len(reg.Keys))
	found := false
	for _, k := range reg.Keys {
		if k.ID == keyID {
			found = true
			continue
		}
		kept = append(kept, k)
	}
	if !found {
		return ErrKeyNotFound
	}
	reg.Keys = kept

	if err := m.saveRegistryLocked(reg); err != nil {
		return err
	}
	m.regCache = nil
	return nil
}

// List returns every key in the registry (hashes and grants, never
// plaintext).
func (m *Manager) List() ([]KeyEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.getRegistryLocked()
	if err != nil {
		return nil, err
	}
	out := make([]KeyEntry, 0, len(reg.Keys))
	for _, k := range reg.Keys {
		out = append(out, *toKeyEntry(k))
	}
	return out, nil
}

// ListForStore returns the keys holding any grant that matches
// storeName. Used by `tsstore key list <store>` to answer "what can
// reach this store?".
func (m *Manager) ListForStore(storeName string) ([]KeyEntry, error) {
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make([]KeyEntry, 0, len(all))
	for _, k := range all {
		for _, g := range k.Grants {
			if g.MatchesStore(storeName) {
				out = append(out, k)
				break
			}
		}
	}
	return out, nil
}

// GrantStore adds an access grant for storeName to an existing key.
// Used when a rollup target is auto-created: the source key must reach
// the target, which pre-#138 was done with a linked key file.
func (m *Manager) GrantStore(keyID, storeName string, access []Access) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return err
	}
	for i := range reg.Keys {
		if reg.Keys[i].ID != keyID {
			continue
		}
		// Already covered by an existing pattern (e.g. a "*" grant) —
		// adding a redundant exact grant would just be noise.
		covered := true
		for _, a := range access {
			if !reg.Keys[i].Permits(storeName, a) {
				covered = false
				break
			}
		}
		if covered {
			return nil
		}
		reg.Keys[i].Grants = append(reg.Keys[i].Grants, Grant{
			Stores: storeName,
			Access: append([]Access(nil), access...),
		})
		if err := m.saveRegistryLocked(reg); err != nil {
			return err
		}
		m.regCache = nil
		return nil
	}
	return ErrKeyNotFound
}

// ExtendGrants gives every key that can reach sourceStore the same
// access on targetStore. Used when a rollup target is auto-created, so a
// client holding the source's key can read the derived store — the
// guarantee the pre-#138 linked key file provided, expressed as grants.
//
// Each key gets exactly the classes it already held on the source, so a
// read-only key does not gain write on the target. Keys already covered
// by a wildcard are left untouched.
func (m *Manager) ExtendGrants(sourceStore, targetStore string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return err
	}

	changed := false
	for i := range reg.Keys {
		k := &reg.Keys[i]

		// Collect the classes this key holds on the source.
		var classes []Access
		for _, a := range AllAccess {
			if k.Permits(sourceStore, a) {
				classes = append(classes, a)
			}
		}
		if len(classes) == 0 {
			continue // can't reach the source; no business on the target
		}

		// Already covered (e.g. by a "*" grant)? Nothing to add.
		covered := true
		for _, a := range classes {
			if !k.Permits(targetStore, a) {
				covered = false
				break
			}
		}
		if covered {
			continue
		}

		k.Grants = append(k.Grants, Grant{Stores: targetStore, Access: classes})
		changed = true
	}

	if !changed {
		return nil
	}
	if err := m.saveRegistryLocked(reg); err != nil {
		return err
	}
	m.regCache = nil
	return nil
}

// RevokeStoreGrants drops grants naming storeName exactly, and removes
// any key left with no grants at all. Called when a store is deleted so
// the registry does not accumulate grants for stores that no longer
// exist. Wildcard grants are left alone — they are about a namespace,
// not this one store.
func (m *Manager) RevokeStoreGrants(storeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return err
	}

	changed := false
	kept := make([]RegistryKey, 0, len(reg.Keys))
	for _, k := range reg.Keys {
		grants := make([]Grant, 0, len(k.Grants))
		for _, g := range k.Grants {
			if g.Stores == storeName {
				changed = true
				continue
			}
			grants = append(grants, g)
		}
		if len(grants) == 0 {
			// The key existed only for this store; drop it entirely.
			changed = true
			continue
		}
		k.Grants = grants
		kept = append(kept, k)
	}
	if !changed {
		return nil
	}
	reg.Keys = kept
	if err := m.saveRegistryLocked(reg); err != nil {
		return err
	}
	m.regCache = nil
	return nil
}

// toKeyEntry converts the on-disk shape to the public one.
func toKeyEntry(k RegistryKey) *KeyEntry {
	return &KeyEntry{
		ID:        k.ID,
		Hash:      k.Hash,
		CreatedAt: k.CreatedAt,
		Note:      k.Note,
		Grants:    k.Grants,
	}
}

// deriveKeyID produces a short, stable, human-usable handle for a key.
//
// Pre-#138 this was fullKey[8:16] — the first block of the UUID — which
// panics on any key shorter than 16 chars. Since operator-supplied keys
// are now accepted, the ID is derived from the hash instead: total
// length no longer matters, and the ID still cannot be reversed into the
// key.
func deriveKeyID(fullKey string) string {
	return hashKey(fullKey)[:8]
}

// hashKey creates a SHA-256 hash of an API key.
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// ValidateKeyFormat reports whether a key has the expected shape:
// the tsstore_ prefix and at least prefix+UUID length.
//
// Note this does not verify the body is a well-formed UUID — it is a
// cheap shape gate in front of the registry lookup, not a parser.
func ValidateKeyFormat(key string) bool {
	if len(key) < len(KeyPrefix)+36 {
		return false
	}
	return strings.HasPrefix(key, KeyPrefix)
}

// ValidateSuppliedKey checks an operator-minted key before it is
// adopted. Supplied keys must match the same format generated keys use,
// so that every code path which sees a key — HTTP, the Unix socket, the
// CLI — treats them identically. The length floor is redundant under the
// prefix rule (a prefixed UUID is 44 chars) but is stated explicitly so
// the entropy requirement survives any future loosening of the format.
func ValidateSuppliedKey(key string) error {
	if key == "" {
		return fmt.Errorf("API key is required")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("API key must not contain whitespace")
	}
	if len(key) < MinKeyLength {
		return fmt.Errorf("API key must be at least %d characters", MinKeyLength)
	}
	if !ValidateKeyFormat(key) {
		return fmt.Errorf("API key must start with %q and be at least %d characters (mint one as %q + a UUID)",
			KeyPrefix, len(KeyPrefix)+36, KeyPrefix)
	}
	return nil
}

// parseLegacyTime parses a created_at from a legacy key file, falling
// back to now for an unparseable or missing value — a bad timestamp is
// cosmetic and must never cost someone their key during migration.
func parseLegacyTime(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Now().UTC()
}
