// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const wsConnectionsFileName = "ws_connections.json"

// WSConnection represents an outbound WebSocket connection configuration.
type WSConnection struct {
	ID        string            `json:"id"`
	Mode      string            `json:"mode"` // "push" or "pull"
	URL       string            `json:"url"`
	From      int64             `json:"from"`    // Start timestamp (push mode)
	Format    string            `json:"format"`  // "compact" or "full"
	Headers   map[string]string `json:"headers"` // Custom headers
	CreatedAt time.Time         `json:"created_at"`
}

// WSConnectionsConfig holds all outbound connection configurations for a store.
type WSConnectionsConfig struct {
	Connections []WSConnection `json:"connections"`
}

// LoadWSConnections loads the WebSocket connections config from disk.
func (s *Store) LoadWSConnections() (*WSConnectionsConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.loadWSConnectionsLocked()
}

// loadWSConnectionsLocked loads the config without holding the lock.
func (s *Store) loadWSConnectionsLocked() (*WSConnectionsConfig, error) {
	configPath := filepath.Join(s.path, wsConnectionsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		// No connections yet
		return &WSConnectionsConfig{Connections: []WSConnection{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config WSConnectionsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveWSConnections saves the WebSocket connections config to disk.
func (s *Store) SaveWSConnections(config *WSConnectionsConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveWSConnectionsLocked(config)
}

// saveWSConnectionsLocked saves the config without holding the lock.
func (s *Store) saveWSConnectionsLocked(config *WSConnectionsConfig) error {
	configPath := filepath.Join(s.path, wsConnectionsFileName)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// AddWSConnection adds a new WebSocket connection to the config.
func (s *Store) AddWSConnection(conn WSConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWSConnectionsLocked()
	if err != nil {
		return err
	}

	config.Connections = append(config.Connections, conn)

	return s.saveWSConnectionsLocked(config)
}

// RemoveWSConnection removes a WebSocket connection from the config.
func (s *Store) RemoveWSConnection(connID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWSConnectionsLocked()
	if err != nil {
		return err
	}

	for i, conn := range config.Connections {
		if conn.ID == connID {
			config.Connections = append(config.Connections[:i], config.Connections[i+1:]...)
			return s.saveWSConnectionsLocked(config)
		}
	}

	return ErrObjectNotFound
}

// GetWSConnection returns a specific WebSocket connection by ID.
func (s *Store) GetWSConnection(connID string) (*WSConnection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, err := s.loadWSConnectionsLocked()
	if err != nil {
		return nil, err
	}

	for _, conn := range config.Connections {
		if conn.ID == connID {
			return &conn, nil
		}
	}

	return nil, ErrObjectNotFound
}

// StorePath returns the path to the store directory.
func (s *Store) StorePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}
