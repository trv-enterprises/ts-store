// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package apikey handles API key generation, hashing, and validation.
package apikey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// KeyPrefix is prepended to all generated API keys
	KeyPrefix = "tsstore_"
	// KeyFileName is the name of the key file in each store directory
	KeyFileName = "keys.json"
)

var (
	ErrKeyNotFound    = errors.New("API key not found")
	ErrInvalidKey     = errors.New("invalid API key")
	ErrKeyFileCorrupt = errors.New("key file is corrupt")
)

// KeyEntry represents a stored API key (hash only).
type KeyEntry struct {
	ID        string    `json:"id"`         // Key identifier (first 8 chars of key)
	Hash      string    `json:"hash"`       // SHA-256 hash of full key
	CreatedAt time.Time `json:"created_at"` // When the key was created
	Note      string    `json:"note"`       // Optional note about the key
}

// KeyFile represents the structure of the keys.json file.
//
// LinkedSource, when set, makes this store defer key validation to another
// store's key file (the "source"). A linked store holds no keys of its own;
// it validates against the source so that rotating/adding/revoking the
// source's keys applies to the linked store automatically (single source of
// truth, no copied hashes that could drift). Used by auto-created rollup
// target stores, which share their source store's keys.
type KeyFile struct {
	StoreName    string     `json:"store_name"`
	LinkedSource string     `json:"linked_source,omitempty"`
	Keys         []KeyEntry `json:"keys"`
}

// cachedKeyFile is a parsed key file plus the file identity it was loaded
// from, so reads can detect on-disk changes made by another process (the
// key CLI writes keys.json directly).
type cachedKeyFile struct {
	keyFile *KeyFile
	modTime time.Time
	size    int64
}

// Manager handles API key operations for stores.
type Manager struct {
	mu       sync.RWMutex
	basePath string
	cache    map[string]cachedKeyFile // storeName -> parsed keys.json + file identity
}

// NewManager creates a new API key manager.
func NewManager(basePath string) *Manager {
	return &Manager{
		basePath: basePath,
		cache:    make(map[string]cachedKeyFile),
	}
}

// Generate creates a new API key for a store.
// Returns the full key (only returned once) and the key entry.
func (m *Manager) Generate(storeName, note string) (string, *KeyEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Generate new UUID-based key
	fullKey := KeyPrefix + uuid.New().String()

	// Create hash
	hash := hashKey(fullKey)

	// Create entry
	entry := &KeyEntry{
		ID:        fullKey[len(KeyPrefix) : len(KeyPrefix)+8], // First 8 chars of UUID
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		Note:      note,
	}

	// Load or create key file
	keyFile, err := m.loadKeyFileLocked(storeName)
	if err != nil && !os.IsNotExist(err) {
		return "", nil, err
	}
	if keyFile == nil {
		keyFile = &KeyFile{
			StoreName: storeName,
			Keys:      []KeyEntry{},
		}
	}

	// Add new key
	keyFile.Keys = append(keyFile.Keys, *entry)

	// Save key file
	if err := m.saveKeyFileLocked(storeName, keyFile); err != nil {
		return "", nil, err
	}

	// Drop the cache entry; the next read reloads with fresh file identity
	delete(m.cache, storeName)

	return fullKey, entry, nil
}

// Validate checks if an API key is valid for a store.
// Returns the key entry if valid. If the store's key file has a LinkedSource,
// validation is delegated to the source store's key file (one hop only) so
// linked stores share their source's keys without copying hashes.
func (m *Manager) Validate(storeName, apiKey string) (*KeyEntry, error) {
	// Full lock: getKeyFileLocked mutates the cache (it also did under the
	// previous RLock, which was a data race).
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get key file (from cache or disk)
	keyFile, err := m.getKeyFileLocked(storeName)
	if err != nil {
		return nil, err
	}

	// Follow a single link to the source store's key file. One hop only:
	// links are created solely for rollup targets, whose source is a real
	// (unlinked) store, so deeper chains never arise.
	if keyFile.LinkedSource != "" {
		src, err := m.getKeyFileLocked(keyFile.LinkedSource)
		if err != nil {
			return nil, err
		}
		keyFile = src
	}

	// Hash the provided key
	hash := hashKey(apiKey)

	// Find matching key
	for _, entry := range keyFile.Keys {
		if entry.Hash == hash {
			return &entry, nil
		}
	}

	return nil, ErrInvalidKey
}

// CreateLinkedKeyFile writes a key file for storeName that holds no keys of its
// own and defers validation to sourceStore. Used when auto-creating a rollup
// target so it shares its source store's API keys.
func (m *Manager) CreateLinkedKeyFile(storeName, sourceStore string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyFile := &KeyFile{
		StoreName:    storeName,
		LinkedSource: sourceStore,
		Keys:         []KeyEntry{},
	}
	if err := m.saveKeyFileLocked(storeName, keyFile); err != nil {
		return err
	}
	// Drop the cache entry; the next read reloads with fresh file identity
	delete(m.cache, storeName)
	return nil
}

// LinkedDependents returns the names of stores whose key file links to
// sourceStore. Scans store subdirectories under basePath. Used to refuse
// deleting a store that other (linked) stores depend on for auth.
func (m *Manager) LinkedDependents(sourceStore string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.basePath)
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == sourceStore {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.basePath, entry.Name(), KeyFileName))
		if err != nil {
			continue
		}
		var kf KeyFile
		if err := json.Unmarshal(data, &kf); err != nil {
			continue
		}
		if kf.LinkedSource == sourceStore {
			deps = append(deps, entry.Name())
		}
	}
	return deps, nil
}

// Revoke removes an API key by its ID.
func (m *Manager) Revoke(storeName, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyFile, err := m.loadKeyFileLocked(storeName)
	if err != nil {
		return err
	}

	// Find and remove the key
	found := false
	newKeys := make([]KeyEntry, 0, len(keyFile.Keys))
	for _, entry := range keyFile.Keys {
		if entry.ID != keyID {
			newKeys = append(newKeys, entry)
		} else {
			found = true
		}
	}

	if !found {
		return ErrKeyNotFound
	}

	keyFile.Keys = newKeys

	// Save updated key file
	if err := m.saveKeyFileLocked(storeName, keyFile); err != nil {
		return err
	}

	// Drop the cache entry; the next read reloads with fresh file identity
	delete(m.cache, storeName)

	return nil
}

// List returns all key entries for a store (hashes only, not full keys).
func (m *Manager) List(storeName string) ([]KeyEntry, error) {
	// Full lock: getKeyFileLocked mutates the cache.
	m.mu.Lock()
	defer m.mu.Unlock()

	keyFile, err := m.getKeyFileLocked(storeName)
	if err != nil {
		return nil, err
	}

	return keyFile.Keys, nil
}

// Regenerate revokes all existing keys and generates a new one.
// Returns the new full key.
func (m *Manager) Regenerate(storeName, note string) (string, *KeyEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Generate new UUID-based key
	fullKey := KeyPrefix + uuid.New().String()

	// Create hash
	hash := hashKey(fullKey)

	// Create entry
	entry := &KeyEntry{
		ID:        fullKey[len(KeyPrefix) : len(KeyPrefix)+8],
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		Note:      note,
	}

	// Create new key file with only the new key
	keyFile := &KeyFile{
		StoreName: storeName,
		Keys:      []KeyEntry{*entry},
	}

	// Save key file
	if err := m.saveKeyFileLocked(storeName, keyFile); err != nil {
		return "", nil, err
	}

	// Drop the cache entry; the next read reloads with fresh file identity
	delete(m.cache, storeName)

	return fullKey, entry, nil
}

// DeleteKeyFile removes the key file for a store (used when deleting a store).
func (m *Manager) DeleteKeyFile(storeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.cache, storeName)

	keyPath := m.keyFilePath(storeName)
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// keyFilePath returns the path to the key file for a store.
func (m *Manager) keyFilePath(storeName string) string {
	return filepath.Join(m.basePath, storeName, KeyFileName)
}

// loadKeyFileLocked loads the key file from disk. Lock must be held.
func (m *Manager) loadKeyFileLocked(storeName string) (*KeyFile, error) {
	keyPath := m.keyFilePath(storeName)

	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	var keyFile KeyFile
	if err := json.Unmarshal(data, &keyFile); err != nil {
		return nil, ErrKeyFileCorrupt
	}

	return &keyFile, nil
}

// saveKeyFileLocked saves the key file to disk. Lock must be held.
func (m *Manager) saveKeyFileLocked(storeName string, keyFile *KeyFile) error {
	keyPath := m.keyFilePath(storeName)

	// Ensure directory exists
	dir := filepath.Dir(keyPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(keyFile, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(keyPath, data, 0600) // Restricted permissions
}

// getKeyFileLocked gets the key file from cache, reloading from disk when the
// file changed on disk. Key management (tsstore key revoke/regenerate) runs as
// a separate process writing keys.json directly, so a running server must
// notice changes — a revoked key has to stop working without a restart.
// Write lock must be held (the cache is mutated).
func (m *Manager) getKeyFileLocked(storeName string) (*KeyFile, error) {
	keyPath := m.keyFilePath(storeName)

	st, err := os.Stat(keyPath)
	if err != nil {
		delete(m.cache, storeName)
		return nil, err
	}

	if c, ok := m.cache[storeName]; ok && c.modTime.Equal(st.ModTime()) && c.size == st.Size() {
		return c.keyFile, nil
	}

	keyFile, err := m.loadKeyFileLocked(storeName)
	if err != nil {
		delete(m.cache, storeName)
		return nil, err
	}

	m.cache[storeName] = cachedKeyFile{keyFile: keyFile, modTime: st.ModTime(), size: st.Size()}
	return keyFile, nil
}

// hashKey creates a SHA-256 hash of an API key.
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// ValidateKeyFormat checks if a key has the correct format.
func ValidateKeyFormat(key string) bool {
	if len(key) < len(KeyPrefix)+36 { // prefix + UUID
		return false
	}
	return key[:len(KeyPrefix)] == KeyPrefix
}
