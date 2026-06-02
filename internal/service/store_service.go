// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package service contains business logic for the API server.
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tviviano/ts-store/internal/alerts"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/mqtt"
	"github.com/tviviano/ts-store/internal/rollups"
	"github.com/tviviano/ts-store/internal/ws"
	"github.com/tviviano/ts-store/pkg/store"
)

var (
	ErrStoreAlreadyOpen = errors.New("store is already open")
	ErrStoreNotOpen     = errors.New("store is not open")
)

// StoreService manages store lifecycle and operations.
type StoreService struct {
	mu           sync.RWMutex
	cfg          *config.Config
	keyManager   *apikey.Manager
	stores          map[string]*store.Store      // storeName -> Store
	wsManagers      map[string]*ws.Manager       // storeName -> WS Manager
	mqttManagers    map[string]*mqtt.Manager     // storeName -> MQTT Manager
	alertsManagers  map[string]*alerts.Manager   // storeName -> Alerts Manager
	rollupsManagers map[string]*rollups.Manager  // storeName -> Rollups Manager (keyed by SOURCE store)
}

// NewStoreService creates a new store service.
func NewStoreService(cfg *config.Config, keyManager *apikey.Manager) *StoreService {
	return &StoreService{
		cfg:            cfg,
		keyManager:     keyManager,
		stores:          make(map[string]*store.Store),
		wsManagers:      make(map[string]*ws.Manager),
		mqttManagers:    make(map[string]*mqtt.Manager),
		alertsManagers:  make(map[string]*alerts.Manager),
		rollupsManagers: make(map[string]*rollups.Manager),
	}
}

// CreateStoreRequest contains parameters for creating a new store.
type CreateStoreRequest struct {
	Name           string `json:"name" binding:"required"`
	NumBlocks      uint32 `json:"num_blocks,omitempty"`
	DataBlockSize  uint32 `json:"data_block_size,omitempty"`
	IndexBlockSize uint32 `json:"index_block_size,omitempty"`
	DataType       string `json:"data_type,omitempty"`    // binary, text, json, schema (default: json)
	NumPartitions  uint32 `json:"num_partitions,omitempty"` // V2: number of partitions (default: 6)
	TotalSize      int64  `json:"total_size,omitempty"`     // V2: total size in bytes
	StorageType    string `json:"storage_type,omitempty"`   // "v1" or "v2" (default: v2)
}

// CreateStoreResponse contains the result of store creation.
type CreateStoreResponse struct {
	Name   string `json:"name"`
	APIKey string `json:"api_key"` // Only returned once!
	KeyID  string `json:"key_id"`
}

// Create creates a new store and generates an API key.
func (s *StoreService) Create(req *CreateStoreRequest) (*CreateStoreResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build store config with defaults
	cfg := store.Config{
		Name:           req.Name,
		Path:           s.cfg.Store.BasePath,
		NumBlocks:      s.cfg.Store.NumBlocks,
		DataBlockSize:  s.cfg.Store.DataBlockSize,
		IndexBlockSize: s.cfg.Store.IndexBlockSize,
		DataType:       store.DataTypeJSON,            // default
		StorageType:    store.StorageTypeV2Partitioned, // default to V2
		NumPartitions:  6,                             // default partitions
	}

	// Override with request values if provided
	if req.NumBlocks > 0 {
		cfg.NumBlocks = req.NumBlocks
	}
	if req.DataBlockSize > 0 {
		cfg.DataBlockSize = req.DataBlockSize
	}
	if req.IndexBlockSize > 0 {
		cfg.IndexBlockSize = req.IndexBlockSize
	}
	if req.DataType != "" {
		dataType, err := store.ParseDataType(req.DataType)
		if err != nil {
			return nil, err
		}
		cfg.DataType = dataType
	}

	// V2 partitioned storage options
	if req.StorageType == "v1" {
		cfg.StorageType = store.StorageTypeV1Circular
	}
	if req.NumPartitions > 0 {
		cfg.NumPartitions = req.NumPartitions
	}
	if req.TotalSize > 0 {
		cfg.TotalSize = req.TotalSize
	}

	// Create the store
	st, err := store.Create(cfg)
	if err != nil {
		return nil, err
	}

	// Generate API key
	apiKey, keyEntry, err := s.keyManager.Generate(req.Name, "Initial key")
	if err != nil {
		st.Delete() // Cleanup on failure
		return nil, err
	}

	// Keep store open
	s.stores[req.Name] = st

	// Create and start WS manager for this store
	wsManager := ws.NewManager(st, req.Name)
	s.wsManagers[req.Name] = wsManager
	go wsManager.LoadAndStart()

	// Create and start MQTT manager for this store
	mqttManager := mqtt.NewManager(st, req.Name)
	s.mqttManagers[req.Name] = mqttManager
	go mqttManager.LoadAndStart()

	// Create and start alerts manager for this store
	alertsManager := alerts.NewManager(st, req.Name)
	s.alertsManagers[req.Name] = alertsManager
	go alertsManager.LoadAndStart()

	// Create and start rollups manager for this store (as a rollup SOURCE).
	rollupsManager := rollups.NewManager(st, req.Name, s)
	s.rollupsManagers[req.Name] = rollupsManager
	go rollupsManager.LoadAndStart()

	return &CreateStoreResponse{
		Name:   req.Name,
		APIKey: apiKey,
		KeyID:  keyEntry.ID,
	}, nil
}

// Open opens an existing store.
func (s *StoreService) Open(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.stores[name]; ok {
		return ErrStoreAlreadyOpen
	}

	st, err := store.Open(s.cfg.Store.BasePath, name)
	if err != nil {
		return err
	}

	s.stores[name] = st

	// Create and start WS manager for this store
	wsManager := ws.NewManager(st, name)
	s.wsManagers[name] = wsManager
	go wsManager.LoadAndStart()

	// Create and start MQTT manager for this store
	mqttManager := mqtt.NewManager(st, name)
	s.mqttManagers[name] = mqttManager
	go mqttManager.LoadAndStart()

	// Create and start alerts manager for this store
	alertsManager := alerts.NewManager(st, name)
	s.alertsManagers[name] = alertsManager
	go alertsManager.LoadAndStart()

	// Create and start rollups manager for this store (as a rollup SOURCE).
	rollupsManager := rollups.NewManager(st, name, s)
	s.rollupsManagers[name] = rollupsManager
	go rollupsManager.LoadAndStart()

	return nil
}

// Close closes a store.
func (s *StoreService) Close(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.stores[name]
	if !ok {
		return ErrStoreNotOpen
	}

	// Stop rollups manager first (depends on store).
	if manager, ok := s.rollupsManagers[name]; ok {
		manager.Stop()
		delete(s.rollupsManagers, name)
	}

	// Stop alerts manager first (depends on store).
	if manager, ok := s.alertsManagers[name]; ok {
		manager.Stop()
		delete(s.alertsManagers, name)
	}

	// Stop MQTT manager
	if manager, ok := s.mqttManagers[name]; ok {
		manager.Stop()
		delete(s.mqttManagers, name)
	}

	// Stop WS manager
	if manager, ok := s.wsManagers[name]; ok {
		manager.Stop()
		delete(s.wsManagers, name)
	}

	if err := st.Close(); err != nil {
		return err
	}

	delete(s.stores, name)
	return nil
}

// Delete deletes a store and its API keys. Refuses if other (linked) stores
// depend on this store's API keys — e.g. rollup targets that link here for auth.
func (s *StoreService) Delete(name string) error {
	if deps, err := s.keyManager.LinkedDependents(name); err == nil && len(deps) > 0 {
		return fmt.Errorf("cannot delete %q: %d store(s) link to its API keys (%s); remove those rollups first",
			name, len(deps), strings.Join(deps, ", "))
	}
	return s.deleteStoreInternal(name)
}

// deleteStoreInternal deletes a store without the linked-dependents guard. Used
// by Delete (after the guard passes) and by the rollups Provider when recreating
// a rollup target the caller owns.
func (s *StoreService) deleteStoreInternal(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop rollups manager first (depends on store).
	if manager, ok := s.rollupsManagers[name]; ok {
		manager.Stop()
		delete(s.rollupsManagers, name)
	}

	// Stop alerts manager first (depends on store).
	if manager, ok := s.alertsManagers[name]; ok {
		manager.Stop()
		delete(s.alertsManagers, name)
	}

	// Stop MQTT manager
	if manager, ok := s.mqttManagers[name]; ok {
		manager.Stop()
		delete(s.mqttManagers, name)
	}

	// Stop WS manager
	if manager, ok := s.wsManagers[name]; ok {
		manager.Stop()
		delete(s.wsManagers, name)
	}

	// Close if open
	if st, ok := s.stores[name]; ok {
		st.Delete()
		delete(s.stores, name)
	} else {
		// Try to delete directly
		if err := store.DeleteStore(s.cfg.Store.BasePath, name); err != nil {
			return err
		}
	}

	// Delete API keys
	s.keyManager.DeleteKeyFile(name)

	return nil
}

// Get returns an open store by name.
func (s *StoreService) Get(name string) (*store.Store, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, ok := s.stores[name]
	if !ok {
		return nil, ErrStoreNotOpen
	}

	return st, nil
}

// GetOrOpen returns an open store, opening it if necessary.
func (s *StoreService) GetOrOpen(name string) (*store.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st, ok := s.stores[name]; ok {
		return st, nil
	}

	st, err := store.Open(s.cfg.Store.BasePath, name)
	if err != nil {
		return nil, err
	}

	s.stores[name] = st

	// Create and start WS manager for this store
	wsManager := ws.NewManager(st, name)
	s.wsManagers[name] = wsManager
	go wsManager.LoadAndStart()

	// Create and start MQTT manager for this store
	mqttManager := mqtt.NewManager(st, name)
	s.mqttManagers[name] = mqttManager
	go mqttManager.LoadAndStart()

	// Create and start alerts manager for this store
	alertsManager := alerts.NewManager(st, name)
	s.alertsManagers[name] = alertsManager
	go alertsManager.LoadAndStart()

	// Create and start rollups manager for this store (as a rollup SOURCE).
	rollupsManager := rollups.NewManager(st, name, s)
	s.rollupsManagers[name] = rollupsManager
	go rollupsManager.LoadAndStart()

	return st, nil
}

// ListOpen returns names of all currently open stores.
func (s *StoreService) ListOpen() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.stores))
	for name := range s.stores {
		names = append(names, name)
	}
	return names
}

// ListAll returns names of all stores on disk by scanning the data directory
// for subdirectories containing a meta.tsdb file.
func (s *StoreService) ListAll() []string {
	entries, err := os.ReadDir(s.cfg.Store.BasePath)
	if err != nil {
		return s.ListOpen()
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := filepath.Join(s.cfg.Store.BasePath, entry.Name(), "meta.tsdb")
		if _, err := os.Stat(metaPath); err == nil {
			names = append(names, entry.Name())
		}
	}
	return names
}

// StoreInfo is the per-store summary returned by the enriched store listing.
type StoreInfo struct {
	Name     string `json:"name"`
	DataType string `json:"data_type,omitempty"`
	Role     string `json:"role"`                 // "source" | "rollup" | "store"
	RollupOf string `json:"rollup_of,omitempty"`  // set when Role == "rollup"
	Window   string `json:"window,omitempty"`     // set when Role == "rollup"
}

// ListAllInfo returns enriched info for every store on disk: data type and
// rollup role/relationship. A store is "rollup" if it carries a rollup_meta
// sidecar, "source" if it has at least one rollup config, else "store".
func (s *StoreService) ListAllInfo() []StoreInfo {
	names := s.ListAll()
	out := make([]StoreInfo, 0, len(names))
	for _, name := range names {
		info := StoreInfo{Name: name, Role: "store"}

		dir := filepath.Join(s.cfg.Store.BasePath, name)

		// Rollup target? (cheap sidecar read, no store open)
		if meta, err := store.ReadRollupMetaAt(dir); err == nil && meta != nil {
			info.Role = "rollup"
			info.RollupOf = meta.RollupOf
			info.Window = meta.Window
		} else if _, statErr := os.Stat(filepath.Join(dir, "rollup_configs.json")); statErr == nil {
			// Has rollup configs -> it's a source. (Best-effort: presence of
			// the file; an empty configs file still reads as "source".)
			info.Role = "source"
		}

		// Data type (open lazily; stores stay cached open).
		if st, err := s.GetOrOpen(name); err == nil {
			info.DataType = st.DataType().String()
		}

		out = append(out, info)
	}
	return out
}

// Stats returns statistics for a store.
func (s *StoreService) Stats(name string) (*store.StoreStats, error) {
	st, err := s.GetOrOpen(name)
	if err != nil {
		return nil, err
	}

	stats := st.Stats()
	return &stats, nil
}

// Reset clears all data from a store but keeps configuration and API keys.
func (s *StoreService) Reset(name string) error {
	st, err := s.GetOrOpen(name)
	if err != nil {
		return err
	}

	return st.Reset()
}

// StoreActivity is the per-store activity snapshot returned by Metrics.
// Combines I/O counters from the store with rule/alert counters summed
// across every webhook/WS/MQTT alert worker for that store.
type StoreActivity struct {
	Store  store.StoreMetrics `json:"store"`
	Alerts []alerts.Metrics   `json:"alerts,omitempty"`
}

// Metrics returns activity counters for the store.
func (s *StoreService) Metrics(name string) (*StoreActivity, error) {
	st, err := s.GetOrOpen(name)
	if err != nil {
		return nil, err
	}
	out := &StoreActivity{Store: st.Metrics()}
	if mgr := s.GetAlertsManager(name); mgr != nil {
		out.Alerts = mgr.AllMetrics()
	}
	return out, nil
}

// ResetMetrics zeros the activity counters on the store and every alert
// worker for the store.
func (s *StoreService) ResetMetrics(name string) error {
	st, err := s.GetOrOpen(name)
	if err != nil {
		return err
	}
	st.ResetMetrics()
	if mgr := s.GetAlertsManager(name); mgr != nil {
		mgr.ResetMetrics()
	}
	return nil
}

// CloseAll closes all open stores.
func (s *StoreService) CloseAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop all rollups managers first
	for name, manager := range s.rollupsManagers {
		manager.Stop()
		delete(s.rollupsManagers, name)
	}

	// Stop all alerts managers
	for name, manager := range s.alertsManagers {
		manager.Stop()
		delete(s.alertsManagers, name)
	}

	// Stop all MQTT managers
	for name, manager := range s.mqttManagers {
		manager.Stop()
		delete(s.mqttManagers, name)
	}

	// Stop all WS managers
	for name, manager := range s.wsManagers {
		manager.Stop()
		delete(s.wsManagers, name)
	}

	var lastErr error
	for name, st := range s.stores {
		if err := st.Close(); err != nil {
			lastErr = err
		}
		delete(s.stores, name)
	}

	return lastErr
}

// GetWSManager returns the WebSocket manager for a store.
func (s *StoreService) GetWSManager(name string) *ws.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wsManagers[name]
}

// GetMQTTManager returns the MQTT manager for a store.
func (s *StoreService) GetMQTTManager(name string) *mqtt.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mqttManagers[name]
}

// GetAlertsManager returns the alerts manager for a store.
func (s *StoreService) GetAlertsManager(name string) *alerts.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alertsManagers[name]
}

// GetRollupsManager returns the rollups manager for a (source) store.
func (s *StoreService) GetRollupsManager(name string) *rollups.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rollupsManagers[name]
}

// --- rollups.Provider implementation ---

// GetOrOpenStore satisfies rollups.Provider.
func (s *StoreService) GetOrOpenStore(name string) (*store.Store, error) {
	return s.GetOrOpen(name)
}

// CreateRollupTarget creates a schema-store rollup target from cfg and links
// its API keys to sourceStore, then opens it. Satisfies rollups.Provider.
func (s *StoreService) CreateRollupTarget(cfg store.Config, sourceStore string) (*store.Store, error) {
	s.mu.Lock()
	// Fill in the base path the service owns.
	cfg.Path = s.cfg.Store.BasePath
	if _, ok := s.stores[cfg.Name]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("store %q already open", cfg.Name)
	}
	st, err := store.Create(cfg)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.stores[cfg.Name] = st
	s.mu.Unlock()

	// Link the target's API keys to the source (shared keys, no drift).
	if err := s.keyManager.CreateLinkedKeyFile(cfg.Name, sourceStore); err != nil {
		// Roll back the half-created store on link failure.
		_ = s.deleteStoreInternal(cfg.Name)
		return nil, fmt.Errorf("link target keys: %w", err)
	}

	// Start the auxiliary managers for the target like any opened store, but
	// NOT a rollups manager (a target is not itself a source unless configured).
	s.mu.Lock()
	wsManager := ws.NewManager(st, cfg.Name)
	s.wsManagers[cfg.Name] = wsManager
	mqttManager := mqtt.NewManager(st, cfg.Name)
	s.mqttManagers[cfg.Name] = mqttManager
	alertsManager := alerts.NewManager(st, cfg.Name)
	s.alertsManagers[cfg.Name] = alertsManager
	rollupsManager := rollups.NewManager(st, cfg.Name, s)
	s.rollupsManagers[cfg.Name] = rollupsManager
	s.mu.Unlock()
	go wsManager.LoadAndStart()
	go mqttManager.LoadAndStart()
	go alertsManager.LoadAndStart()
	go rollupsManager.LoadAndStart()

	return st, nil
}

// DeleteStore satisfies rollups.Provider (used on force_recreate). Bypasses the
// linked-dependents guard since the rollup manager owns this target.
func (s *StoreService) DeleteStore(name string) error {
	return s.deleteStoreInternal(name)
}
