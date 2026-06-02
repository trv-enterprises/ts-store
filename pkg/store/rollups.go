// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	rollupConfigsFileName = "rollup_configs.json"
	rollupMetaFileName    = "rollup_meta.json"
)

// RollupConfig defines a rollup: a background worker reads this (source) store
// over closed time windows, aggregates each window, and writes one record per
// window into a target store. Persisted in the SOURCE store's directory.
type RollupConfig struct {
	ID             string    `json:"id"`
	Name           string    `json:"name,omitempty"`
	TargetStore    string    `json:"target_store,omitempty"` // if empty, derived as <source>-<canonical-window>
	WindowDuration string    `json:"window"`                 // "1m", "1h" (duration.ParseDuration)
	AggFields      string    `json:"agg_fields,omitempty"`   // "cpu:avg+max,mem:avg"
	AggDefault     string    `json:"agg_default,omitempty"`  // fallback func, e.g. "avg"
	PollInterval   string    `json:"poll_interval,omitempty"`
	RestartPolicy  string    `json:"restart_policy,omitempty"` // "resume" (default) | "now"
	Retention      string    `json:"retention,omitempty"`      // how long to keep rollup rows, e.g. "1y", "90d"
	EdgeTolerance  float64   `json:"edge_tolerance,omitempty"` // max over-retention fraction; default 0.10
	CreatedAt      time.Time `json:"created_at"`
}

// RollupConfigs holds all rollup configs for a (source) store.
type RollupConfigs struct {
	Rollups []RollupConfig `json:"rollups"`
}

// LoadRollupConfigs loads rollup configs from the source store's directory.
func (s *Store) LoadRollupConfigs() (*RollupConfigs, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadRollupConfigsLocked()
}

func (s *Store) loadRollupConfigsLocked() (*RollupConfigs, error) {
	configPath := filepath.Join(s.path, rollupConfigsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &RollupConfigs{Rollups: []RollupConfig{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config RollupConfigs
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (s *Store) saveRollupConfigsLocked(config *RollupConfigs) error {
	configPath := filepath.Join(s.path, rollupConfigsFileName)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// AddRollupConfig appends a rollup config and persists.
func (s *Store) AddRollupConfig(cfg RollupConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadRollupConfigsLocked()
	if err != nil {
		return err
	}
	config.Rollups = append(config.Rollups, cfg)
	return s.saveRollupConfigsLocked(config)
}

// RemoveRollupConfig removes a rollup config by ID.
func (s *Store) RemoveRollupConfig(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadRollupConfigsLocked()
	if err != nil {
		return err
	}
	for i, r := range config.Rollups {
		if r.ID == id {
			config.Rollups = append(config.Rollups[:i], config.Rollups[i+1:]...)
			return s.saveRollupConfigsLocked(config)
		}
	}
	return ErrObjectNotFound
}

// GetRollupConfig returns a rollup config by ID.
func (s *Store) GetRollupConfig(id string) (*RollupConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, err := s.loadRollupConfigsLocked()
	if err != nil {
		return nil, err
	}
	for _, r := range config.Rollups {
		if r.ID == id {
			rc := r
			return &rc, nil
		}
	}
	return nil, ErrObjectNotFound
}

// RollupMeta marks a store as a rollup target and records what it is a rollup
// of. Written as a sidecar (rollup_meta.json) in the TARGET store's directory
// at auto-create time. Its presence is what makes the store report role=rollup.
type RollupMeta struct {
	RollupOf   string `json:"rollup_of"` // source store name
	Window     string `json:"window"`    // canonical window duration
	AggFields  string `json:"agg_fields,omitempty"`
	AggDefault string `json:"agg_default,omitempty"`
}

// WriteRollupMeta writes the rollup sidecar into this (target) store's dir.
func (s *Store) WriteRollupMeta(meta RollupMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	metaPath := filepath.Join(s.path, rollupMetaFileName)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

// ReadRollupMeta returns the rollup sidecar for this store, or nil if the store
// is not a rollup target (no sidecar present).
func (s *Store) ReadRollupMeta() (*RollupMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readRollupMetaAt(s.path)
}

// readRollupMetaAt reads a rollup sidecar from a store directory without
// requiring the store to be open. Returns (nil, nil) when absent.
func readRollupMetaAt(storeDir string) (*RollupMeta, error) {
	data, err := os.ReadFile(filepath.Join(storeDir, rollupMetaFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta RollupMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// ReadRollupMetaAt is the exported form for callers (e.g. the service's store
// listing) that need to inspect a closed store's rollup sidecar by path.
func ReadRollupMetaAt(storeDir string) (*RollupMeta, error) {
	return readRollupMetaAt(storeDir)
}
