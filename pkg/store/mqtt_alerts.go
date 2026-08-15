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

const mqttAlertsFileName = "mqtt_alerts.json"

// MQTTAlert defines an alert that publishes matching records to an MQTT topic.
// The worker holds a persistent MQTT client and publishes at the configured QoS
// (default 1) when rules match.
type MQTTAlert struct {
	ID           string            `json:"id"`
	BrokerURL    string            `json:"broker_url"`
	Topic        string            `json:"topic"`
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
	QoS          byte              `json:"qos"` // 0, 1, or 2 — default 1
	PollInterval string            `json:"poll_interval,omitempty"` // default "1s"
	CreatedAt    time.Time         `json:"created_at"`
	AlertCommon
}

// MQTTAlertsConfig holds all MQTT alert configs for a store.
type MQTTAlertsConfig struct {
	Alerts []MQTTAlert `json:"alerts"`
}

// LoadMQTTAlerts loads MQTT alert configs from disk.
func (s *Store) LoadMQTTAlerts() (*MQTTAlertsConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadMQTTAlertsLocked()
}

func (s *Store) loadMQTTAlertsLocked() (*MQTTAlertsConfig, error) {
	configPath := filepath.Join(s.path, mqttAlertsFileName)

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &MQTTAlertsConfig{Alerts: []MQTTAlert{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var config MQTTAlertsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// SaveMQTTAlerts saves MQTT alert configs to disk.
func (s *Store) SaveMQTTAlerts(config *MQTTAlertsConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveMQTTAlertsLocked(config)
}

func (s *Store) saveMQTTAlertsLocked(config *MQTTAlertsConfig) error {
	configPath := filepath.Join(s.path, mqttAlertsFileName)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath, data, 0644)
}

// AddMQTTAlert adds a new MQTT alert to the config.
func (s *Store) AddMQTTAlert(alert MQTTAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadMQTTAlertsLocked()
	if err != nil {
		return err
	}

	config.Alerts = append(config.Alerts, alert)
	return s.saveMQTTAlertsLocked(config)
}

// UpdateMQTTAlert replaces the stored alert carrying alert.ID, keeping its
// position in the list. Returns ErrObjectNotFound if no alert has that ID.
// See UpdateWebhookAlert for the caller's field-preservation contract.
func (s *Store) UpdateMQTTAlert(alert MQTTAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadMQTTAlertsLocked()
	if err != nil {
		return err
	}

	for i, a := range config.Alerts {
		if a.ID == alert.ID {
			config.Alerts[i] = alert
			return s.saveMQTTAlertsLocked(config)
		}
	}
	return ErrObjectNotFound
}

// RemoveMQTTAlert removes an MQTT alert by ID.
func (s *Store) RemoveMQTTAlert(alertID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, err := s.loadMQTTAlertsLocked()
	if err != nil {
		return err
	}

	for i, a := range config.Alerts {
		if a.ID == alertID {
			config.Alerts = append(config.Alerts[:i], config.Alerts[i+1:]...)
			return s.saveMQTTAlertsLocked(config)
		}
	}
	return ErrObjectNotFound
}

// GetMQTTAlert returns an MQTT alert by ID.
func (s *Store) GetMQTTAlert(alertID string) (*MQTTAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	config, err := s.loadMQTTAlertsLocked()
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
