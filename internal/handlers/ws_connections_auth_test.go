// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/middleware"
)

// TestPullModeRequiresWrite pins the issue #160 gate: the connection route's
// class is read (#154, decided by push mode — deliver data the key could
// already poll), but a pull-mode connection INGESTS what the remote sends,
// so creating one additionally requires write. A read-only key may create
// push connections but must be refused pull ones.
func TestPullModeRequiresWrite(t *testing.T) {
	router, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// setupTestRouter doesn't register the connection routes; wire them the
	// way main.go does, behind the same Auth middleware.
	wsConnHandler := NewWSConnectionsHandler(storeService.GetWSManager)
	g := router.Group("/api/stores/:store")
	g.Use(middleware.Auth(keyManager, apikey.AccessWrite))
	g.POST("/ws/connections", wsConnHandler.Create)

	fullKey := createStore(t, router, "pull-auth")

	roKey, _, err := keyManager.Create("consumer", []apikey.Grant{{
		Stores: "pull-auth", Access: []apikey.Access{apikey.AccessRead},
	}})
	if err != nil {
		t.Fatalf("create read-only key: %v", err)
	}

	create := func(key, mode string) int {
		body := fmt.Sprintf(`{"mode": %q, "url": "ws://127.0.0.1:9/dead"}`, mode)
		req, _ := http.NewRequest("POST", "/api/stores/pull-auth/ws/connections", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	// Read-only key: push is stream-out and allowed; pull is ingest and
	// refused with 403 (valid credential, insufficient authority).
	if code := create(roKey, "push"); code != http.StatusCreated {
		t.Errorf("read key creating push connection: got %d, want 201", code)
	}
	if code := create(roKey, "pull"); code != http.StatusForbidden {
		t.Errorf("read key creating pull connection: got %d, want 403", code)
	}

	// A key holding write may create pull connections.
	if code := create(fullKey, "pull"); code != http.StatusCreated {
		t.Errorf("full key creating pull connection: got %d, want 201", code)
	}
}
