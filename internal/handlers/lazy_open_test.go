// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

// setupLazyOpenRouter wires the store-scoped handlers the way main.go does —
// through the *OrOpen accessors — so these tests exercise the real
// open-on-demand path rather than a fixture that pre-opens everything.
func setupLazyOpenRouter(t *testing.T) (*gin.Engine, *service.StoreService) {
	t.Helper()
	router, storeService, keyManager, _ := setupTestRouter(t)

	alertsHandler := NewAlertsHandler(storeService.GetAlertsManagerOrOpen)
	rollupsHandler := NewRollupsHandler(storeService.GetRollupsManagerOrOpen)
	wsConnHandler := NewWSConnectionsHandler(storeService.GetWSManagerOrOpen)
	mqttHandler := NewMQTTHandler(storeService.GetMQTTManagerOrOpen)
	connectionsHandler := NewConnectionsHandler(
		storeService.GetWSManagerOrOpen, storeService.GetMQTTManagerOrOpen, storeService.GetAlertsManagerOrOpen)

	g := router.Group("/api/stores/:store")
	g.Use(middleware.Auth(keyManager, apikey.AccessWrite))
	g.GET("/alerts", alertsHandler.List)
	g.GET("/rollups", rollupsHandler.List)
	g.GET("/ws/connections", wsConnHandler.List)
	g.GET("/mqtt/connections", mqttHandler.List)
	g.GET("/connections", connectionsHandler.List)

	return router, storeService
}

// TestStoreScopedEndpointsOpenOnDemand: a store that exists on disk but is
// closed (the state after a restart, or for a store fed only through the Unix
// socket collector) must serve its alerts/rollups/connections endpoints
// without some data request having opened it first. Previously these handlers
// consulted only the open-store maps and returned 404 "store not found or not
// open", which reads to a dashboard like a bad key rather than a cold store.
func TestStoreScopedEndpointsOpenOnDemand(t *testing.T) {
	router, storeService := setupLazyOpenRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "cold-store")

	// Close it: back to "exists on disk, not open", exactly as after a
	// restart. Close also drops the alerts/rollups/ws/mqtt managers.
	if err := storeService.Close("cold-store"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, name := range storeService.ListOpen() {
		if name == "cold-store" {
			t.Fatal("precondition: store should be closed")
		}
	}

	get := func(path string) (int, []byte) {
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("X-API-Key", apiKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	// The reported case: /alerts first, with no prior data request.
	code, body := get("/api/stores/cold-store/alerts")
	if code != http.StatusOK {
		t.Fatalf("GET /alerts on a closed store: got %d (%s), want 200", code, body)
	}
	var alertsResp struct {
		Alerts []map[string]any `json:"alerts"`
	}
	if err := json.Unmarshal(body, &alertsResp); err != nil {
		t.Fatalf("unmarshal alerts: %v", err)
	}
	if len(alertsResp.Alerts) != 0 {
		t.Errorf("expected no alerts on a fresh store, got %d", len(alertsResp.Alerts))
	}

	// The sibling endpoints that shared the pattern. Each is checked from a
	// closed state so one endpoint's open doesn't mask another's bug.
	for _, path := range []string{
		"/api/stores/cold-store/rollups",
		"/api/stores/cold-store/ws/connections",
		"/api/stores/cold-store/mqtt/connections",
		"/api/stores/cold-store/connections",
	} {
		if err := storeService.Close("cold-store"); err != nil && err != service.ErrStoreNotOpen {
			t.Fatalf("Close before %s: %v", path, err)
		}
		if code, body := get(path); code != http.StatusOK {
			t.Errorf("GET %s on a closed store: got %d (%s), want 200", path, code, body)
		}
	}
}

// TestStoreScopedEndpointsStill404OnMissingStore: open-on-demand must not
// paper over a genuinely absent store. The key carries a wildcard grant so
// the request reaches the handler rather than being refused at auth.
func TestStoreScopedEndpointsStill404OnMissingStore(t *testing.T) {
	router, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	alertsHandler := NewAlertsHandler(storeService.GetAlertsManagerOrOpen)
	rollupsHandler := NewRollupsHandler(storeService.GetRollupsManagerOrOpen)
	wsConnHandler := NewWSConnectionsHandler(storeService.GetWSManagerOrOpen)

	g := router.Group("/api/stores/:store")
	g.Use(middleware.Auth(keyManager, apikey.AccessWrite))
	g.GET("/alerts", alertsHandler.List)
	g.GET("/rollups", rollupsHandler.List)
	g.GET("/ws/connections", wsConnHandler.List)

	wildKey, _, err := keyManager.Create("admin-ish", []apikey.Grant{{
		Stores: "*", Access: apikey.AllAccess,
	}})
	if err != nil {
		t.Fatalf("create wildcard key: %v", err)
	}

	for _, path := range []string{
		"/api/stores/no-such-store/alerts",
		"/api/stores/no-such-store/rollups",
		"/api/stores/no-such-store/ws/connections",
	} {
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("X-API-Key", wildKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d (%s), want 404", path, w.Code, w.Body.String())
		}
	}
}
