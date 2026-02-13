// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package mqtt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tviviano/ts-store/pkg/store"
)

const mqttConnectionsFileName = "mqtt_connections.json"

var (
	ErrConnectionNotFound = errors.New("connection not found")
	ErrManagerClosed      = errors.New("manager is closed")
)

// MQTTConnection represents an MQTT sink configuration.
type MQTTConnection struct {
	ID                    string    `json:"id"`
	BrokerURL             string    `json:"broker_url"`
	Topic                 string    `json:"topic"`
	ClientID              string    `json:"client_id,omitempty"`
	Username              string    `json:"username,omitempty"`
	Password              string    `json:"password,omitempty"`
	From                  int64     `json:"from"`                    // Start timestamp (0=oldest, -1=now)
	IncludeTimestamp      bool      `json:"include_timestamp"`       // Wrap data with timestamp
	CursorPersistInterval int       `json:"cursor_persist_interval"` // Seconds: >0=persist, 0=memory only, -1=no auto-reconnect
	AggWindow             string    `json:"agg_window,omitempty"`    // Aggregation window (e.g., "1m", "30s")
	AggFields             string    `json:"agg_fields,omitempty"`    // Per-field functions (e.g., "cpu:avg,mem:max")
	AggDefault            string    `json:"agg_default,omitempty"`   // Default aggregation function
	CreatedAt             time.Time `json:"created_at"`
}

// MQTTConnectionsConfig holds all MQTT connection configurations for a store.
type MQTTConnectionsConfig struct {
	Connections []MQTTConnection `json:"connections"`
}

// ConnectionStatus represents the state of an MQTT connection.
type ConnectionStatus struct {
	ID            string    `json:"id"`
	BrokerURL     string    `json:"broker_url"`
	Topic         string    `json:"topic"`
	From          int64     `json:"from,omitempty"`
	Status        string    `json:"status"` // connecting, connected, disconnected, error
	CreatedAt     time.Time `json:"created_at"`
	LastTimestamp int64     `json:"last_timestamp,omitempty"`
	MessagesSent  int64     `json:"messages_sent,omitempty"`
	Errors        int64     `json:"errors,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// Connection is the interface for MQTT connections.
type Connection interface {
	ID() string
	Status() ConnectionStatus
	Start() error
	Stop() error
}

// Manager manages MQTT connections for a store.
type Manager struct {
	mu          sync.RWMutex
	store       *store.Store
	storeName   string
	connections map[string]Connection
	closed      bool
}

// NewManager creates a new MQTT connection manager for a store.
func NewManager(st *store.Store, storeName string) *Manager {
	return &Manager{
		store:       st,
		storeName:   storeName,
		connections: make(map[string]Connection),
	}
}

// LoadAndStart loads persisted MQTT connections from config and starts them.
func (m *Manager) LoadAndStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}

	config, err := m.loadConfig()
	if err != nil {
		return err
	}

	for _, connConfig := range config.Connections {
		conn := NewPusher(m.store, m.storeName, connConfig)
		m.connections[conn.ID()] = conn
		go conn.Start()
	}

	return nil
}

// loadConfig loads the MQTT connections config from disk.
func (m *Manager) loadConfig() (*MQTTConnectionsConfig, error) {
	configPath := filepath.Join(m.store.StorePath(), mqttConnectionsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &MQTTConnectionsConfig{Connections: []MQTTConnection{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config MQTTConnectionsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// saveConfig saves the MQTT connections config to disk.
func (m *Manager) saveConfig(config *MQTTConnectionsConfig) error {
	configPath := filepath.Join(m.store.StorePath(), mqttConnectionsFileName)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// CreateConnectionRequest holds parameters for creating a new MQTT connection.
type CreateConnectionRequest struct {
	BrokerURL             string `json:"broker_url"`
	Topic                 string `json:"topic"`
	ClientID              string `json:"client_id,omitempty"`
	Username              string `json:"username,omitempty"`
	Password              string `json:"password,omitempty"`
	From                  int64  `json:"from"`                    // Start timestamp (0=oldest, -1=now)
	IncludeTimestamp      bool   `json:"include_timestamp"`       // Wrap data with timestamp
	CursorPersistInterval *int   `json:"cursor_persist_interval"` // Seconds: >0=persist, 0=memory only, -1=no auto-reconnect (default: 0)
	AggWindow             string `json:"agg_window,omitempty"`    // Aggregation window (e.g., "1m")
	AggFields             string `json:"agg_fields,omitempty"`    // Per-field functions
	AggDefault            string `json:"agg_default,omitempty"`   // Default aggregation function
}

// CreateConnection creates and starts a new MQTT connection.
func (m *Manager) CreateConnection(req CreateConnectionRequest) (*ConnectionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrManagerClosed
	}

	// Generate ID
	id := uuid.New().String()[:8]

	// Default cursor_persist_interval to 0 (memory only)
	cursorInterval := 0
	if req.CursorPersistInterval != nil {
		cursorInterval = *req.CursorPersistInterval
	}

	mqttConn := MQTTConnection{
		ID:                    id,
		BrokerURL:             req.BrokerURL,
		Topic:                 req.Topic,
		ClientID:              req.ClientID,
		Username:              req.Username,
		Password:              req.Password,
		From:                  req.From,
		IncludeTimestamp:      req.IncludeTimestamp,
		CursorPersistInterval: cursorInterval,
		AggWindow:             req.AggWindow,
		AggFields:             req.AggFields,
		AggDefault:            req.AggDefault,
		CreatedAt:             time.Now().UTC(),
	}

	// Persist to config
	config, err := m.loadConfig()
	if err != nil {
		return nil, err
	}
	config.Connections = append(config.Connections, mqttConn)
	if err := m.saveConfig(config); err != nil {
		return nil, err
	}

	// Create and start connection
	conn := NewPusher(m.store, m.storeName, mqttConn)
	m.connections[id] = conn
	go conn.Start()

	status := conn.Status()
	return &status, nil
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
	config, err := m.loadConfig()
	if err != nil {
		return err
	}

	for i, c := range config.Connections {
		if c.ID == id {
			config.Connections = append(config.Connections[:i], config.Connections[i+1:]...)
			break
		}
	}

	// Remove cursor file if it exists
	cursorPath := filepath.Join(m.store.StorePath(), "mqtt_"+id+".cursor")
	os.Remove(cursorPath) // Ignore error - file may not exist

	return m.saveConfig(config)
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
