// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"testing"
	"time"
)

func TestMQTTAlertsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "mqa-rt"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()

	loaded, err := s.LoadMQTTAlerts()
	if err != nil {
		t.Fatalf("LoadMQTTAlerts (empty): %v", err)
	}
	if len(loaded.Alerts) != 0 {
		t.Errorf("Empty store: expected 0 alerts, got %d", len(loaded.Alerts))
	}

	a := MQTTAlert{
		ID:        "alert-1",
		BrokerURL: "tcp://broker.example:1883",
		Topic:     "alerts/heat",
		QoS:       1,
		Rules:     []AlertRuleConfig{{Name: "hot", Condition: "t > 80", Cooldown: "30s"}},
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := s.AddMQTTAlert(a); err != nil {
		t.Fatalf("AddMQTTAlert: %v", err)
	}

	got, err := s.GetMQTTAlert("alert-1")
	if err != nil {
		t.Fatalf("GetMQTTAlert: %v", err)
	}
	if got.BrokerURL != a.BrokerURL || got.Topic != "alerts/heat" || got.QoS != 1 {
		t.Errorf("Round-trip mismatch: %+v", got)
	}

	if err := s.RemoveMQTTAlert("alert-1"); err != nil {
		t.Fatalf("RemoveMQTTAlert: %v", err)
	}
	if _, err := s.GetMQTTAlert("alert-1"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Expected ErrObjectNotFound after remove, got: %v", err)
	}
}
