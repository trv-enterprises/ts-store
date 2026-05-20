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

const webhookAlertsFileName = "webhook_alerts.json"

// WebhookAlert defines an alert that dispatches matching records to an HTTP
// webhook. Rules are evaluated by a background worker that polls the store
// at PollInterval; when a rule matches, the worker POSTs the alert payload
// to URL with the configured Headers.
type WebhookAlert struct {
	ID           string            `json:"id"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	PollInterval string            `json:"poll_interval,omitempty"` // default "1s"
	Timeout      string            `json:"timeout,omitempty"`       // default "10s"
	CreatedAt    time.Time         `json:"created_at"`
	AlertCommon
}

// WebhookAlertsConfig holds all webhook alert configs for a store.
type WebhookAlertsConfig struct {
	Alerts []WebhookAlert `json:"alerts"`
}

// LoadWebhookAlerts loads webhook alert configs from disk.
func (s *Store) LoadWebhookAlerts() (*WebhookAlertsConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadWebhookAlertsLocked()
}

func (s *Store) loadWebhookAlertsLocked() (*WebhookAlertsConfig, error) {
	configPath := filepath.Join(s.path, webhookAlertsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &WebhookAlertsConfig{Alerts: []WebhookAlert{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config WebhookAlertsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// SaveWebhookAlerts saves webhook alert configs to disk.
func (s *Store) SaveWebhookAlerts(config *WebhookAlertsConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveWebhookAlertsLocked(config)
}

func (s *Store) saveWebhookAlertsLocked(config *WebhookAlertsConfig) error {
	configPath := filepath.Join(s.path, webhookAlertsFileName)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// AddWebhookAlert adds a new webhook alert to the config.
func (s *Store) AddWebhookAlert(alert WebhookAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWebhookAlertsLocked()
	if err != nil {
		return err
	}

	config.Alerts = append(config.Alerts, alert)
	return s.saveWebhookAlertsLocked(config)
}

// RemoveWebhookAlert removes a webhook alert by ID.
func (s *Store) RemoveWebhookAlert(alertID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadWebhookAlertsLocked()
	if err != nil {
		return err
	}

	for i, a := range config.Alerts {
		if a.ID == alertID {
			config.Alerts = append(config.Alerts[:i], config.Alerts[i+1:]...)
			return s.saveWebhookAlertsLocked(config)
		}
	}
	return ErrObjectNotFound
}

// GetWebhookAlert returns a webhook alert by ID.
func (s *Store) GetWebhookAlert(alertID string) (*WebhookAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, err := s.loadWebhookAlertsLocked()
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
