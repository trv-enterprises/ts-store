// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package ws provides WebSocket connection management for outbound connections.
package ws

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tviviano/ts-store/pkg/store"
)

var (
	ErrConnectionNotFound = errors.New("connection not found")
	ErrInvalidMode        = errors.New("invalid mode: must be 'push' or 'pull'")
	ErrManagerClosed      = errors.New("manager is closed")
)

// ConnectionStatus represents the state of an outbound connection.
type ConnectionStatus struct {
	ID            string    `json:"id"`
	Mode          string    `json:"mode"`
	URL           string    `json:"url"`
	From          int64     `json:"from,omitempty"`
	Format        string    `json:"format"`
	Status        string    `json:"status"` // connecting, connected, disconnected, error
	CreatedAt     time.Time `json:"created_at"`
	LastTimestamp int64     `json:"last_timestamp,omitempty"`
	MessagesSent  int64     `json:"messages_sent,omitempty"`
	MessagesRecv  int64     `json:"messages_received,omitempty"`
	Errors        int64     `json:"errors,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// Connection is the interface for outbound connections (push or pull).
type Connection interface {
	ID() string
	Status() ConnectionStatus
	Start() error
	Stop() error
}

// Manager manages outbound WebSocket connections for a store.
type Manager struct {
	mu          sync.RWMutex
	store       *store.Store
	storeName   string
	connections map[string]Connection // id -> connection
	closed      bool
}

// NewManager creates a new WebSocket connection manager for a store.
func NewManager(st *store.Store, storeName string) *Manager {
	return &Manager{
		store:       st,
		storeName:   storeName,
		connections: make(map[string]Connection),
	}
}

// LoadAndStart loads persisted connections from config and starts them.
func (m *Manager) LoadAndStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}

	config, err := m.store.LoadWSConnections()
	if err != nil {
		return err
	}

	for _, connConfig := range config.Connections {
		conn, err := m.createConnectionLocked(connConfig)
		if err != nil {
			continue // Skip invalid connections
		}
		m.connections[conn.ID()] = conn
		go conn.Start()
	}

	return nil
}

// CreateConnectionRequest holds parameters for creating a new connection.
type CreateConnectionRequest struct {
	Mode    string            `json:"mode"` // "push" or "pull"
	URL     string            `json:"url"`
	From    int64             `json:"from"`    // Start timestamp (push mode)
	Format  string            `json:"format"`  // "compact" or "full"
	Headers map[string]string `json:"headers"` // Custom headers
}

// CreateConnection creates and starts a new outbound connection.
func (m *Manager) CreateConnection(req CreateConnectionRequest) (*ConnectionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrManagerClosed
	}

	if req.Mode != "push" && req.Mode != "pull" {
		return nil, ErrInvalidMode
	}

	// Default format
	if req.Format == "" {
		req.Format = "full"
	}

	// Generate ID
	id := uuid.New().String()[:8]

	wsConn := store.WSConnection{
		ID:        id,
		Mode:      req.Mode,
		URL:       req.URL,
		From:      req.From,
		Format:    req.Format,
		Headers:   req.Headers,
		CreatedAt: time.Now().UTC(),
	}

	// Persist to config
	if err := m.store.AddWSConnection(wsConn); err != nil {
		return nil, err
	}

	// Create and start connection
	conn, err := m.createConnectionLocked(wsConn)
	if err != nil {
		m.store.RemoveWSConnection(id)
		return nil, err
	}

	m.connections[id] = conn
	go conn.Start()

	status := conn.Status()
	return &status, nil
}

// createConnectionLocked creates a connection from config (lock must be held).
func (m *Manager) createConnectionLocked(config store.WSConnection) (Connection, error) {
	switch config.Mode {
	case "push":
		return NewPusher(m.store, m.storeName, config), nil
	case "pull":
		return NewPuller(m.store, m.storeName, config), nil
	default:
		return nil, ErrInvalidMode
	}
}

// GetConnection returns the status of a specific connection.
func (m *Manager) GetConnection(id string) (*ConnectionStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	conn, ok := m.connections[id]
	if !ok {
		return nil, ErrConnectionNotFound
	}

	status := conn.Status()
	return &status, nil
}

// ListConnections returns the status of all connections.
func (m *Manager) ListConnections() []ConnectionStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]ConnectionStatus, 0, len(m.connections))
	for _, conn := range m.connections {
		statuses = append(statuses, conn.Status())
	}

	return statuses
}

// DeleteConnection stops and removes a connection.
func (m *Manager) DeleteConnection(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, ok := m.connections[id]
	if !ok {
		return ErrConnectionNotFound
	}

	// Stop the connection
	conn.Stop()

	// Remove from map
	delete(m.connections, id)

	// Remove from persistent config
	return m.store.RemoveWSConnection(id)
}

// Stop stops all connections and closes the manager.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}

	m.closed = true

	for _, conn := range m.connections {
		conn.Stop()
	}

	m.connections = make(map[string]Connection)

	return nil
}
