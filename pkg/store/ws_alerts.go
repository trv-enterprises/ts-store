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

const wsAlertsFileName = "ws_alerts.json"

// WSAlert defines an alert that dispatches matching records over an outbound
// WebSocket connection. Default dispatch is open-on-fire: the worker dials
// URL, sends one JSON frame per matching record, and closes the connection.
type WSAlert struct {
	ID           string            `json:"id"`
	URL          string            `json:"url"`               // ws:// or wss://
	Headers      map[string]string `json:"headers,omitempty"` // Sent on the upgrade request
	PollInterval string            `json:"poll_interval,omitempty"` // default "1s"
	CreatedAt    time.Time         `json:"created_at"`
	AlertCommon
}

// WSAlertsConfig holds all WS alert configs for a store.
type WSAlertsConfig struct {
	Alerts []WSAlert `json:"alerts"`
}

// LoadWSAlerts loads WS alert configs from disk.
func (s *Store) LoadWSAlerts() (*WSAlertsConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadWSAlertsLocked()
}

func (s *Store) loadWSAlertsLocked() (*WSAlertsConfig, error) {
	configPath := filepath.Join(s.path, wsAlertsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &WSAlertsConfig{Alerts: []WSAlert{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config WSAlertsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// SaveWSAlerts saves WS alert configs to disk.
func (s *Store) SaveWSAlerts(config *WSAlertsConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveWSAlertsLocked(config)
}

func (s *Store) saveWSAlertsLocked(config *WSAlertsConfig) error {
	configPath := filepath.Join(s.path, wsAlertsFileName)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath, data, 0644)
}

// AddWSAlert adds a new WS alert to the config.
func (s *Store) AddWSAlert(alert WSAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWSAlertsLocked()
	if err != nil {
		return err
	}

	config.Alerts = append(config.Alerts, alert)
	return s.saveWSAlertsLocked(config)
}

// RemoveWSAlert removes a WS alert by ID.
func (s *Store) RemoveWSAlert(alertID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWSAlertsLocked()
	if err != nil {
		return err
	}

	for i, a := range config.Alerts {
		if a.ID == alertID {
			config.Alerts = append(config.Alerts[:i], config.Alerts[i+1:]...)
			return s.saveWSAlertsLocked(config)
		}
	}
	return ErrObjectNotFound
}

// GetWSAlert returns a WS alert by ID.
func (s *Store) GetWSAlert(alertID string) (*WSAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, err := s.loadWSAlertsLocked()
	if err != nil {
		return nil, err
	}

	for _, a := range config.Alerts {
		if a.ID == alertID {
			return &a, nil
		}
	}
	return nil, ErrObjectNotFound
}
