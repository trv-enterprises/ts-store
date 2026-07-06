// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/alerts"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/mqtt"
	"github.com/tviviano/ts-store/internal/ws"
)

// newConnectionsTestContext builds a gin context with the store name set in the
// same key the auth middleware uses, so the handler resolves it normally.
func newConnectionsTestContext(t *testing.T, query string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest("GET", "/api/stores/s1/connections"+query, nil)
	c.Request = req
	c.Set(middleware.StoreNameKey, "s1")
	return c, w
}

// TestConnectionsListNilManagersEmptyArrays confirms a store with no managers
// (no connections wired) returns empty arrays rather than erroring, and that
// alerts are omitted unless requested.
func TestConnectionsListNilManagersEmptyArrays(t *testing.T) {
	h := NewConnectionsHandler(
		func(string) *ws.Manager { return nil },
		func(string) *mqtt.Manager { return nil },
		func(string) *alerts.Manager { return nil },
	)

	c, w := newConnectionsTestContext(t, "")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(resp["ws"]) != "[]" {
		t.Errorf("expected ws=[], got %s", resp["ws"])
	}
	if string(resp["mqtt"]) != "[]" {
		t.Errorf("expected mqtt=[], got %s", resp["mqtt"])
	}
	if _, present := resp["alerts"]; present {
		t.Errorf("alerts key must be absent without include_alerts, got %s", resp["alerts"])
	}
}

// TestConnectionsListIncludeAlerts confirms ?include_alerts=true adds the
// alerts key (empty array when no alerts manager).
func TestConnectionsListIncludeAlerts(t *testing.T) {
	h := NewConnectionsHandler(
		func(string) *ws.Manager { return nil },
		func(string) *mqtt.Manager { return nil },
		func(string) *alerts.Manager { return nil },
	)

	c, w := newConnectionsTestContext(t, "?include_alerts=true")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	alertsRaw, present := resp["alerts"]
	if !present {
		t.Fatal("alerts key must be present with include_alerts=true")
	}
	if string(alertsRaw) != "[]" {
		t.Errorf("expected alerts=[], got %s", alertsRaw)
	}
}

// TestConnectionsListMetaEnvelope confirms every response carries the meta
// envelope: the store the data belongs to, the serving host, the server
// version, and a parseable RFC3339 as_of timestamp (issue #92 — lets
// consumers detect a misdirected connection or stale feed).
func TestConnectionsListMetaEnvelope(t *testing.T) {
	h := NewConnectionsHandler(
		func(string) *ws.Manager { return nil },
		func(string) *mqtt.Manager { return nil },
		func(string) *alerts.Manager { return nil },
	)

	c, w := newConnectionsTestContext(t, "")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Meta struct {
			Store         string `json:"store"`
			Host          string `json:"host"`
			AsOf          string `json:"as_of"`
			ServerVersion string `json:"server_version"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Meta.Store != "s1" {
		t.Errorf("meta.store: expected s1, got %q", resp.Meta.Store)
	}
	if _, err := time.Parse(time.RFC3339, resp.Meta.AsOf); err != nil {
		t.Errorf("meta.as_of %q is not RFC3339: %v", resp.Meta.AsOf, err)
	}
	if resp.Meta.ServerVersion == "" {
		t.Error("meta.server_version must be non-empty")
	}
	// Host comes from os.Hostname(); empty is a tolerated degradation, so we
	// only assert the key decodes (covered by the struct above).
}

// TestMergeAlertsJoinsByID confirms status and metrics are zipped on the alert
// ID, surfacing the runtime counters (records_evaluated, alerts_dropped, etc.)
// alongside the status.
func TestMergeAlertsJoinsByID(t *testing.T) {
	statuses := []alerts.Status{
		{ID: "a1", Type: "webhook", RuleName: "hot", State: "running", AlertsFired: 5},
		{ID: "a2", Type: "mqtt", RuleName: "cold", State: "running"},
	}
	metrics := []alerts.Metrics{
		{ID: "a1", RecordsEvaluated: 100, RecordsMatched: 5, AlertsFired: 5, AlertsDropped: 2},
		// a2 intentionally has no metrics entry — should yield zero counters.
	}

	entries := mergeAlerts(statuses, metrics)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	a1 := entries[0]
	if a1.ID != "a1" || a1.RecordsEvaluated != 100 || a1.AlertsDropped != 2 || a1.RecordsMatched != 5 {
		t.Errorf("a1 counters not merged: %+v", a1)
	}
	if a1.State != "running" || a1.AlertsFired != 5 {
		t.Errorf("a1 status not preserved: %+v", a1)
	}

	a2 := entries[1]
	if a2.ID != "a2" || a2.RecordsEvaluated != 0 || a2.AlertsDropped != 0 {
		t.Errorf("a2 (no metrics) should have zero counters: %+v", a2)
	}
}
