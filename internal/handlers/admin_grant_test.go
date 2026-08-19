// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

const testAdminKey = "test-admin-key-at-least-20-chars"

// setupAdminGrantRouter wires POST /api/stores behind AdminAuth the way
// main.go does, so these tests exercise the real credential resolution
// (X-Admin-Key or an admin-granted X-API-Key) rather than a stub.
func setupAdminGrantRouter(t *testing.T) (*gin.Engine, *service.StoreService, *apikey.Manager) {
	t.Helper()
	router, storeService, keyManager, _ := setupTestRouter(t)

	storeHandler := NewStoreHandler(storeService, keyManager)
	g := router.Group("/api/v2/stores")
	g.Use(middleware.AdminAuth(testAdminKey, keyManager))
	g.POST("", storeHandler.Create)

	return router, storeService, keyManager
}

func createStoreAs(t *testing.T, router *gin.Engine, header, value, name string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name})
	req, _ := http.NewRequest("POST", "/api/v2/stores", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(header, value)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestAdminKeyStillCreatesStores: the server-tier bootstrap credential is
// unchanged by #157. A fresh server has an empty key registry, so some
// credential must exist before any grant can.
func TestAdminKeyStillCreatesStores(t *testing.T) {
	router, storeService, _ := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	if code, body := createStoreAs(t, router, "X-Admin-Key", testAdminKey, "via-admin-key"); code != http.StatusCreated {
		t.Errorf("admin key: got %d (%s), want 201", code, body)
	}
}

// TestAdminGrantCreatesMatchingStore is the point of the feature: a registry
// key with admin:sensors-* may create stores in that namespace — something the
// all-or-nothing server admin key cannot express.
func TestAdminGrantCreatesMatchingStore(t *testing.T) {
	router, storeService, keyManager := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	provisioner, _, err := keyManager.Create("provisioner", []apikey.Grant{{
		Stores: "sensors-*", Access: []apikey.Access{apikey.AccessAdmin},
	}})
	if err != nil {
		t.Fatalf("create provisioner key: %v", err)
	}

	if code, body := createStoreAs(t, router, "X-API-Key", provisioner, "sensors-garage"); code != http.StatusCreated {
		t.Fatalf("admin grant on matching name: got %d (%s), want 201", code, body)
	}

	// The pattern is the boundary: a name outside it is refused with 403 —
	// a valid credential lacking authority, not a bad credential.
	if code, body := createStoreAs(t, router, "X-API-Key", provisioner, "billing"); code != http.StatusForbidden {
		t.Errorf("admin grant on non-matching name: got %d (%s), want 403", code, body)
	}
}

// TestNonAdminGrantsCannotCreate: read/write/manage are store-scoped
// authority over an EXISTING store and confer no lifecycle rights. This also
// pins that AllAccess (what every store's initial key carries) does not
// include admin — otherwise every store key could create stores.
func TestNonAdminGrantsCannotCreate(t *testing.T) {
	router, storeService, keyManager := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	full, _, err := keyManager.Create("full-store-key", []apikey.Grant{{
		Stores: "*", Access: apikey.AllAccess,
	}})
	if err != nil {
		t.Fatalf("create full key: %v", err)
	}

	if code, body := createStoreAs(t, router, "X-API-Key", full, "should-not-exist"); code != http.StatusForbidden {
		t.Errorf("read,write,manage:* key creating a store: got %d (%s), want 403", code, body)
	}
}

// TestAdminAuthRejectsUnknownKey keeps the 401/403 split honest on this path:
// an unknown credential is 401, a known one lacking authority is 403.
func TestAdminAuthRejectsUnknownKey(t *testing.T) {
	router, storeService, _ := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	if code, _ := createStoreAs(t, router, "X-API-Key", "tsstore_00000000-0000-0000-0000-000000000000", "nope"); code != http.StatusUnauthorized {
		t.Errorf("unknown key: got %d, want 401", code)
	}
	if code, _ := createStoreAs(t, router, "X-Admin-Key", "wrong-admin-key-000000", "nope"); code != http.StatusUnauthorized {
		t.Errorf("wrong admin key: got %d, want 401", code)
	}
	if code, _ := createStoreAs(t, router, "", "", "nope"); code != http.StatusUnauthorized {
		t.Errorf("no credential: got %d, want 401", code)
	}
}

// TestAdminAuthBodyPeekLeavesBodyIntact is the subtle one. Resolving a
// pattern-scoped admin grant requires reading the store name from the JSON
// body, and Gin's body is a single-use stream — if the middleware consumes it
// without replacing it, the handler sees an empty body and reports "name is
// required" even though the request was well-formed. A 201 here proves the
// handler still received the full body.
func TestAdminAuthBodyPeekLeavesBodyIntact(t *testing.T) {
	router, storeService, keyManager := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	provisioner, _, err := keyManager.Create("provisioner", []apikey.Grant{{
		Stores: "*", Access: []apikey.Access{apikey.AccessAdmin},
	}})
	if err != nil {
		t.Fatalf("create provisioner key: %v", err)
	}

	// Send a body carrying more than the name, and assert the non-name
	// fields survived the peek by checking they took effect.
	body, _ := json.Marshal(map[string]any{
		"name": "peek-intact", "num_blocks": 64, "data_type": "text",
	})
	req, _ := http.NewRequest("POST", "/api/v2/stores", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", provisioner)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create with full body: got %d (%s), want 201", w.Code, w.Body.String())
	}

	st, err := storeService.GetOrOpen("peek-intact")
	if err != nil {
		t.Fatalf("GetOrOpen: %v", err)
	}
	if got := st.DataType().String(); got != "text" {
		t.Errorf("data_type from the peeked body = %q, want text — the handler saw a truncated body", got)
	}
}

// TestAdminGrantRejectsInvalidStoreName: the peeked name is caller-supplied
// and reaches a filepath join downstream, so it is validated before any grant
// check — a traversal-shaped name is a 400, never a 403 or a created store.
func TestAdminGrantRejectsInvalidStoreName(t *testing.T) {
	router, storeService, keyManager := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	provisioner, _, err := keyManager.Create("provisioner", []apikey.Grant{{
		Stores: "*", Access: []apikey.Access{apikey.AccessAdmin},
	}})
	if err != nil {
		t.Fatalf("create provisioner key: %v", err)
	}

	for _, name := range []string{"../etc/passwd", "a/b", "..", ".hidden"} {
		if code, body := createStoreAs(t, router, "X-API-Key", provisioner, name); code != http.StatusBadRequest {
			t.Errorf("name %q: got %d (%s), want 400", name, code, body)
		}
	}
}

// TestAdminOnlyKeyGetsNoStoreListing: an admin grant is lifecycle authority
// and says nothing about an existing store's contents, so it contributes no
// access classes and the store stays out of that key's listing (issue #157).
func TestAdminOnlyKeyGetsNoStoreListing(t *testing.T) {
	router, storeService, keyManager := setupAdminGrantRouter(t)
	defer storeService.CloseAll()

	provisioner, _, err := keyManager.Create("provisioner", []apikey.Grant{{
		Stores: "*", Access: []apikey.Access{apikey.AccessAdmin},
	}})
	if err != nil {
		t.Fatalf("create provisioner key: %v", err)
	}
	if code, body := createStoreAs(t, router, "X-API-Key", provisioner, "made-by-admin"); code != http.StatusCreated {
		t.Fatalf("create: got %d (%s)", code, body)
	}

	req, _ := http.NewRequest("GET", "/api/stores", nil)
	req.Header.Set("X-API-Key", provisioner)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d (%s), want 200", w.Code, w.Body.String())
	}

	var resp struct {
		Stores []struct {
			Name string `json:"name"`
		} `json:"stores"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	for _, s := range resp.Stores {
		if s.Name == "made-by-admin" {
			t.Errorf("admin-only key sees a store in its listing; admin conveys no read access")
		}
	}
}
