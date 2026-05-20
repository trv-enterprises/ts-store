// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"testing"
	"time"
)

func TestWSAlertsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "wsa-rt"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()

	loaded, err := s.LoadWSAlerts()
	if err != nil {
		t.Fatalf("LoadWSAlerts (empty): %v", err)
	}
	if len(loaded.Alerts) != 0 {
		t.Errorf("Empty store: expected 0 alerts, got %d", len(loaded.Alerts))
	}

	a := WSAlert{
		ID:          "alert-1",
		URL:         "wss://relay.example/alerts",
		Headers:     map[string]string{"X-Token": "secret"},
		AlertCommon: AlertCommon{Name: "hot", Condition: "t > 80"},
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	}
	if err := s.AddWSAlert(a); err != nil {
		t.Fatalf("AddWSAlert: %v", err)
	}

	got, err := s.GetWSAlert("alert-1")
	if err != nil {
		t.Fatalf("GetWSAlert: %v", err)
	}
	if got.URL != a.URL || got.Headers["X-Token"] != "secret" {
		t.Errorf("Round-trip mismatch: %+v", got)
	}

	if err := s.RemoveWSAlert("alert-1"); err != nil {
		t.Fatalf("RemoveWSAlert: %v", err)
	}
	if _, err := s.GetWSAlert("alert-1"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Expected ErrObjectNotFound after remove, got: %v", err)
	}
}
