// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/service"
)

// TestMissingStoreReturns404 confirms operations on a nonexistent store
// come back 404, not 500 (issue #39). Uses /stats — it routes through
// the same GetOrOpen → respondStoreError path as the other read
// endpoints.
func TestMissingStoreReturns404(t *testing.T) {
	_, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	router := gin.New()
	sh := NewStoreHandler(storeService, keyManager)
	router.GET("/api/stores/:store/stats", sh.Stats)

	req, _ := http.NewRequest("GET", "/api/stores/does-not-exist/stats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("stats on missing store: got %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestDeleteSourceLeavesTargetKeysWorking replaces the issue #39
// dependent-guard test. That guard existed because a rollup target held
// no keys of its own and deferred auth to its source, so deleting the
// source stranded the target's authentication — hence the 409.
//
// Under #138 the target carries its own grant on the source's key, so
// deleting the source cannot strand it and there is nothing to refuse.
// What must hold instead: the key keeps working on the surviving target.
func TestDeleteSourceLeavesTargetKeysWorking(t *testing.T) {
	_, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	resp, err := storeService.Create(&service.CreateStoreRequest{Name: "dep-source"})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if _, err := storeService.Create(&service.CreateStoreRequest{Name: "dep-target"}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	// The source's key is extended to the target, as CreateRollupTarget does.
	if err := keyManager.ExtendGrants("dep-source", "dep-target"); err != nil {
		t.Fatalf("extend grants: %v", err)
	}

	router := gin.New()
	sh := NewStoreHandler(storeService, keyManager)
	router.DELETE("/api/stores/:store", sh.Delete)

	req, _ := http.NewRequest("DELETE", "/api/stores/dep-source", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete source: got %d, want 200: %s", w.Code, w.Body.String())
	}

	// The key survives (it still grants the target) and still authorizes there.
	if _, err := keyManager.Authorize("dep-target", resp.APIKey, apikey.AccessRead); err != nil {
		t.Errorf("target access broke when its source store was deleted: %v", err)
	}
	// But its grant on the deleted store is gone.
	if _, err := keyManager.Authorize("dep-source", resp.APIKey, apikey.AccessRead); err == nil {
		t.Error("key still authorizes on the deleted store")
	}
}
