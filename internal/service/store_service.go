// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package service contains business logic for the API server.
package service

import (
	"errors"
	"sync"

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/ws"
	"github.com/tviviano/ts-store/pkg/store"
)

var (
	ErrStoreAlreadyOpen = errors.New("store is already open")
	ErrStoreNotOpen     = errors.New("store is not open")
)

// StoreService manages store lifecycle and operations.
type StoreService struct {
	mu         sync.RWMutex
	cfg        *config.Config
	keyManager *apikey.Manager
	stores     map[string]*store.Store  // storeName -> Store
	wsManagers map[string]*ws.Manager   // storeName -> WS Manager
}

// NewStoreService creates a new store service.
func NewStoreService(cfg *config.Config, keyManager *apikey.Manager) *StoreService {
	return &StoreService{
		cfg:        cfg,
		keyManager: keyManager,
		stores:     make(map[string]*store.Store),
		wsManagers: make(map[string]*ws.Manager),
	}
}

// CreateStoreRequest contains parameters for creating a new store.
type CreateStoreRequest struct {
	Name           string `json:"name" binding:"required"`
	NumBlocks      uint32 `json:"num_blocks,omitempty"`
	DataBlockSize  uint32 `json:"data_block_size,omitempty"`
	IndexBlockSize uint32 `json:"index_block_size,omitempty"`
	DataType       string `json:"data_type,omitempty"` // binary, text, json, schema (default: json)
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
		DataType:       store.DataTypeJSON, // default
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

	// Stop WS manager first
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

// Delete deletes a store and its API keys.
func (s *StoreService) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop WS manager first
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

	return st, nil
}

// List returns names of all open stores.
func (s *StoreService) ListOpen() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.stores))
	for name := range s.stores {
		names = append(names, name)
	}
	return names
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

// CloseAll closes all open stores.
func (s *StoreService) CloseAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop all WS managers first
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
