// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"testing"
	"time"
)

func TestWebhookAlertsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "wha-rt"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer s.Close()

	// Empty load works.
	loaded, err := s.LoadWebhookAlerts()
	if err != nil {
		t.Fatalf("LoadWebhookAlerts (empty): %v", err)
	}
	if len(loaded.Alerts) != 0 {
		t.Errorf("Empty store: expected 0 alerts, got %d", len(loaded.Alerts))
	}

	// Add one.
	a := WebhookAlert{
		ID:        "alert-1",
		URL:       "https://hook.example/incoming",
		Headers:   map[string]string{"X-Token": "secret"},
		Rules:     []AlertRuleConfig{{Name: "hot", Condition: "t > 80", Cooldown: "1m"}},
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := s.AddWebhookAlert(a); err != nil {
		t.Fatalf("AddWebhookAlert: %v", err)
	}

	got, err := s.GetWebhookAlert("alert-1")
	if err != nil {
		t.Fatalf("GetWebhookAlert: %v", err)
	}
	if got.URL != a.URL || got.Headers["X-Token"] != "secret" {
		t.Errorf("Round-trip mismatch: got %+v", got)
	}
	if len(got.Rules) != 1 || got.Rules[0].Name != "hot" {
		t.Errorf("Rules not persisted: %+v", got.Rules)
	}

	// Remove.
	if err := s.RemoveWebhookAlert("alert-1"); err != nil {
		t.Fatalf("RemoveWebhookAlert: %v", err)
	}
	if _, err := s.GetWebhookAlert("alert-1"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Expected ErrObjectNotFound after remove, got: %v", err)
	}

	// Removing again is also not-found.
	if err := s.RemoveWebhookAlert("alert-1"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Expected ErrObjectNotFound on double remove, got: %v", err)
	}
}
