// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// legacyKeyFile is the pre-#138 per-store keys.json shape. Kept here
// (rather than in the live model) because it is only ever read, once,
// by the migration.
type legacyKeyFile struct {
	StoreName    string `json:"store_name"`
	LinkedSource string `json:"linked_source,omitempty"`
	Keys         []struct {
		ID        string `json:"id"`
		Hash      string `json:"hash"`
		CreatedAt string `json:"created_at"`
		Note      string `json:"note"`
	} `json:"keys"`
}

// MigrateLegacyKeys imports pre-#138 per-store keys.json files into the
// central registry. It runs once on first boot and is a no-op afterwards.
//
// Transparency is the entire contract: every key that authenticated
// before the upgrade must authenticate identically after it, with no
// reconfiguration by any client. A store key could previously do
// everything on its own store — read, write, and alert/rollup CRUD — so
// each legacy key is imported with a single grant of all three access
// classes scoped to exactly that store name.
//
// The legacy files are deliberately NOT deleted. They cost nothing to
// leave in place, they make a downgrade work, and they are the only
// copy of the hashes if the registry is ever lost.
//
// Returns the number of keys imported.
func (m *Manager) MigrateLegacyKeys() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := m.loadRegistryLocked()
	if err != nil {
		return 0, err
	}

	// Index what the registry already holds so a re-run is idempotent
	// and a hand-added grant is never clobbered by a legacy import.
	existing := make(map[string]bool, len(reg.Keys))
	for _, k := range reg.Keys {
		existing[k.Hash] = true
	}

	entries, err := os.ReadDir(m.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // fresh install, nothing to migrate
		}
		return 0, err
	}

	// Resolve links first: a linked store (a rollup target) holds no
	// keys of its own and defers to its source. In the registry that
	// becomes an extra grant on the source's key rather than a separate
	// mechanism, so we need the full link map before building grants.
	links := make(map[string]string) // linked store -> source store
	legacy := make(map[string]*legacyKeyFile)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		kf, err := readLegacyKeyFile(filepath.Join(m.basePath, e.Name(), KeyFileName))
		if err != nil {
			// A missing file is normal (not every directory is a store).
			// A corrupt one is worth surfacing but must not abort the
			// whole migration and strand every other store's keys.
			if !os.IsNotExist(err) {
				log.Printf("apikey: migration: skipping %s: %v", e.Name(), err)
			}
			continue
		}
		legacy[e.Name()] = kf
		if kf.LinkedSource != "" {
			links[e.Name()] = kf.LinkedSource
		}
	}

	// Build, for each source store, the set of store names its keys must
	// cover: the store itself plus anything that linked to it.
	coverage := make(map[string][]string, len(legacy))
	for name, kf := range legacy {
		if kf.LinkedSource != "" {
			continue // holds no keys of its own
		}
		coverage[name] = []string{name}
	}
	for linked, source := range links {
		coverage[source] = append(coverage[source], linked)
	}

	imported := 0
	for storeName, kf := range legacy {
		if kf.LinkedSource != "" {
			continue
		}
		stores := coverage[storeName]
		for _, lk := range kf.Keys {
			if lk.Hash == "" || existing[lk.Hash] {
				continue
			}
			grants := make([]Grant, 0, len(stores))
			for _, s := range stores {
				grants = append(grants, Grant{Stores: s, Access: append([]Access(nil), AllAccess...)})
			}
			reg.Keys = append(reg.Keys, RegistryKey{
				ID:        lk.ID,
				Hash:      lk.Hash,
				CreatedAt: parseLegacyTime(lk.CreatedAt),
				Note:      legacyNote(lk.Note, storeName),
				Grants:    grants,
			})
			existing[lk.Hash] = true
			imported++
		}
	}

	if imported == 0 {
		return 0, nil
	}
	if err := m.saveRegistryLocked(reg); err != nil {
		return 0, fmt.Errorf("write registry: %w", err)
	}
	m.regCache = nil
	return imported, nil
}

// readLegacyKeyFile parses one pre-#138 keys.json.
func readLegacyKeyFile(path string) (*legacyKeyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf legacyKeyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, ErrKeyFileCorrupt
	}
	return &kf, nil
}

// legacyNote preserves the original note, tagging untagged keys with the
// store they came from so `key list` output stays intelligible after the
// migration flattens per-store files into one registry.
func legacyNote(note, storeName string) string {
	if note == "" {
		return "migrated from " + storeName
	}
	return note + " (migrated from " + storeName + ")"
}
