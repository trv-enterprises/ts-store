// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/service"
)

// TestMissingStoreReturns404 confirms operations on a nonexistent store
// come back 404, not 500 (issue #39). Uses /stats — it routes through
// the same GetOrOpen → respondStoreError path as the other read
// endpoints.
func TestMissingStoreReturns404(t *testing.T) {
	_, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	router := gin.New()
	sh := NewStoreHandler(storeService)
	router.GET("/api/stores/:store/stats", sh.Stats)

	req, _ := http.NewRequest("GET", "/api/stores/does-not-exist/stats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("stats on missing store: got %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestDeleteWithDependentsReturns409 confirms deleting a store that
// rollup targets link to for auth is refused with 409, not 500
// (issue #39).
func TestDeleteWithDependentsReturns409(t *testing.T) {
	_, storeService, keyManager, tmpDir := setupTestRouter(t)
	defer storeService.CloseAll()

	if _, err := storeService.Create(&service.CreateStoreRequest{Name: "dep-source"}); err != nil {
		t.Fatalf("create store: %v", err)
	}
	// Simulate a rollup target linking to the source's keys.
	if err := os.MkdirAll(filepath.Join(tmpDir, "dep-target"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := keyManager.CreateLinkedKeyFile("dep-target", "dep-source"); err != nil {
		t.Fatalf("link key file: %v", err)
	}

	router := gin.New()
	sh := NewStoreHandler(storeService)
	router.DELETE("/api/stores/:store", sh.Delete)

	req, _ := http.NewRequest("DELETE", "/api/stores/dep-source", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("delete with dependents: got %d, want 409: %s", w.Code, w.Body.String())
	}
}
