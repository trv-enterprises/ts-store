// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RegistryFileName is the central key registry, one per data directory.
// It replaces the per-store keys.json files (issue #138); those are
// migrated into it automatically on first boot and then left in place,
// unread, so a downgrade still finds them.
const RegistryFileName = "keys.registry.json"

// RegistryVersion is the on-disk schema version. Bumped only for a
// breaking layout change; readers reject anything newer rather than
// guessing at fields they do not understand.
const RegistryVersion = 1

// RegistryKey is one API key and everything it is allowed to do. The
// plaintext is never stored — only its SHA-256 hash, as before.
type RegistryKey struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
	Grants    []Grant   `json:"grants"`
}

// Permits reports whether this key may perform access on storeName.
func (k *RegistryKey) Permits(storeName string, access Access) bool {
	for _, g := range k.Grants {
		if g.Permits(storeName, access) {
			return true
		}
	}
	return false
}

// ReadableStores filters names down to those this key may read. Used by
// GET /api/stores so a caller only discovers what it may actually see.
func (k *RegistryKey) ReadableStores(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if k.Permits(n, AccessRead) {
			out = append(out, n)
		}
	}
	return out
}

// Registry is the on-disk shape of the central key file.
type Registry struct {
	Version int           `json:"version"`
	Keys    []RegistryKey `json:"keys"`
}

// registryPath returns the registry file path for this manager.
func (m *Manager) registryPath() string {
	return filepath.Join(m.basePath, RegistryFileName)
}

// loadRegistryLocked reads and parses the registry. A missing file is
// not an error — it yields an empty registry, which is what a fresh
// install (or a pre-#138 install before migration) looks like. Lock must
// be held.
//
// INVARIANT: every mutating operation goes through this (a fresh parse),
// never through getRegistryLocked. Readers hold pointers into the cached
// *Registry under only the read lock, so mutating a cached RegistryKey
// in place would be a data race. Writers build a new object, persist it,
// and drop the cache; the next reader reloads.
func (m *Manager) loadRegistryLocked() (*Registry, error) {
	data, err := os.ReadFile(m.registryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{Version: RegistryVersion}, nil
		}
		return nil, err
	}

	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, ErrKeyFileCorrupt
	}
	// Refuse to operate on a registry written by a newer ts-store: we
	// would silently ignore grant semantics we do not implement, which
	// fails open. Better to stop than to under-enforce.
	if reg.Version > RegistryVersion {
		return nil, fmt.Errorf("%w: registry version %d is newer than supported version %d",
			ErrKeyFileCorrupt, reg.Version, RegistryVersion)
	}
	return &reg, nil
}

// saveRegistryLocked writes the registry atomically. Unlike the old
// per-store key files, a truncated write here would lock every key out
// of every store, so this goes through write-temp-then-rename rather
// than a plain os.WriteFile. Lock must be held.
func (m *Manager) saveRegistryLocked(reg *Registry) error {
	reg.Version = RegistryVersion
	// Stable order keeps the file diffable and makes the size-based
	// staleness check meaningful across rewrites.
	sort.Slice(reg.Keys, func(i, j int) bool { return reg.Keys[i].ID < reg.Keys[j].ID })

	data, err := marshalRegistry(reg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.basePath, 0755); err != nil {
		return err
	}
	return writeFileAtomic(m.registryPath(), data, 0600)
}

// marshalRegistry renders the registry as it is stored on disk.
func marshalRegistry(reg *Registry) ([]byte, error) {
	return json.MarshalIndent(reg, "", "  ")
}

// writeFileAtomic writes data to path via a temp file in the same
// directory followed by a rename, so a crash mid-write leaves either the
// old file or the new one — never a truncated one. The temp file is
// created with the target permissions so the secret is never briefly
// world-readable.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// fsync before rename: the rename is atomic, but without the sync a
	// crash can leave the renamed file with unflushed (zero) contents.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// byHash indexes a registry's keys for O(1) lookup, built once per
// load rather than per request. Pre-#138 the scan was over one store's
// keys (usually exactly one); the central registry holds every key on
// the server, so a linear scan per request would grow with deployment
// size for no reason.
func (r *Registry) byHash() map[string]*RegistryKey {
	idx := make(map[string]*RegistryKey, len(r.Keys))
	for i := range r.Keys {
		idx[r.Keys[i].Hash] = &r.Keys[i]
	}
	return idx
}

// getRegistryLocked returns the registry, reloading from disk when the
// file changed. Same contract as the old per-store cache (issue #11):
// the key CLI runs as a separate process writing the registry directly,
// so a running server must notice a revoke without a restart.
//
// The freshness check is mtime+size. A same-size edit within the
// filesystem's mtime resolution could be missed, so writers must go
// through saveRegistryLocked, which drops the cache explicitly. Lock
// must be held (the cache is mutated).
func (m *Manager) getRegistryLocked() (*Registry, error) {
	path := m.registryPath()

	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No registry yet: fall through to an empty one rather than
			// failing every request on a fresh install.
			m.regCache = nil
			return &Registry{Version: RegistryVersion}, nil
		}
		m.regCache = nil
		return nil, err
	}

	if c := m.regCache; c != nil && c.modTime.Equal(st.ModTime()) && c.size == st.Size() {
		return c.registry, nil
	}

	reg, err := m.loadRegistryLocked()
	if err != nil {
		m.regCache = nil
		return nil, err
	}
	m.regCache = &cachedRegistry{
		registry: reg,
		index:    reg.byHash(),
		modTime:  st.ModTime(),
		size:     st.Size(),
	}
	return reg, nil
}

// lookupLocked resolves a plaintext key via the cached hash index,
// falling back to a scan when the cache is cold (a fresh install with no
// registry file yet). Lock must be held.
func (m *Manager) lookupLocked(apiKey string) (*RegistryKey, error) {
	reg, err := m.getRegistryLocked()
	if err != nil {
		return nil, err
	}
	hash := hashKey(apiKey)

	if c := m.regCache; c != nil && c.index != nil {
		if k, ok := c.index[hash]; ok {
			return k, nil
		}
		return nil, ErrInvalidKey
	}
	for i := range reg.Keys {
		if reg.Keys[i].Hash == hash {
			return &reg.Keys[i], nil
		}
	}
	return nil, ErrInvalidKey
}

// Resolve finds the registry key matching the supplied plaintext. It
// performs no authorization — callers pair it with Permits, or use
// Authorize, which does both.
func (m *Manager) Resolve(apiKey string) (*RegistryKey, error) {
	if k, ok := m.resolveFast(apiKey); ok {
		return k, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveLocked(apiKey)
}

// resolveFast attempts a lookup under the read lock. It succeeds only
// when the cache is warm AND the file on disk is unchanged, so a revoke
// written by the CLI is still noticed on the very next request. Returns
// ok=false to mean "escalate to the write lock", including for an
// unknown key — a miss must be re-checked against a possibly-stale
// cache before it becomes a 401.
func (m *Manager) resolveFast(apiKey string) (*RegistryKey, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	c := m.regCache
	if c == nil || c.index == nil {
		return nil, false
	}
	// Cheap freshness check. If the file moved on, fall through to the
	// write path, which reloads.
	st, err := os.Stat(m.registryPath())
	if err != nil || !c.modTime.Equal(st.ModTime()) || c.size != st.Size() {
		return nil, false
	}
	k, ok := c.index[hashKey(apiKey)]
	return k, ok
}

func (m *Manager) resolveLocked(apiKey string) (*RegistryKey, error) {
	return m.lookupLocked(apiKey)
}

// Authorize resolves a key and checks it may perform access on
// storeName. This is the single entry point for both the HTTP
// middleware and the Unix socket, so the two cannot diverge on what a
// key is allowed to do.
//
// Returns ErrInvalidKey for an unknown key and ErrForbidden when the key
// is real but lacks the grant — callers map those to 401 and 403
// respectively.
func (m *Manager) Authorize(storeName, apiKey string, access Access) (*RegistryKey, error) {
	key, err := m.Resolve(apiKey)
	if err != nil {
		return nil, err
	}
	if !key.Permits(storeName, access) {
		return nil, ErrForbidden
	}
	return key, nil
}
