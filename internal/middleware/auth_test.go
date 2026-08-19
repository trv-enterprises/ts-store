// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
)

// newAuthRouter builds a router mirroring the production route shape: a
// header-auth REST route and the WS handshake route, both behind Auth.
func newAuthRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	km := apikey.NewManager(t.TempDir())
	key, _, err := km.CreateForStore("teststore", "test")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	r := gin.New()
	sr := r.Group("/api/stores/:store")
	sr.Use(Auth(km, apikey.AccessWrite))
	ok := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	sr.GET("/data/newest", ok)
	sr.GET("/ws/write", ok)
	return r, key
}

func TestAuthQueryKeyOnlyOnWSHandshake(t *testing.T) {
	r, key := newAuthRouter(t)

	do := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// Query credential on a REST route: rejected.
	if code := do("/api/stores/teststore/data/newest?api_key=" + key); code != http.StatusUnauthorized {
		t.Errorf("REST route with query api_key: status %d, want 401", code)
	}

	// Query credential on the WS handshake: accepted (browser WebSocket
	// API can't set headers).
	if code := do("/api/stores/teststore/ws/write?api_key=" + key); code != http.StatusOK {
		t.Errorf("ws/write with query api_key: status %d, want 200", code)
	}
}

func TestAuthHeaderKeysStillWork(t *testing.T) {
	r, key := newAuthRouter(t)

	for _, h := range []struct{ name, value string }{
		{"X-API-Key", key},
		{"Authorization", "Bearer " + key},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/stores/teststore/data/newest", nil)
		req.Header.Set(h.name, h.value)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s auth: status %d, want 200", h.name, w.Code)
		}
	}
}

func TestAdminAuthRejectsQueryKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// The admin key is now an ordinary registry entry carrying every grant
	// (issue #176), so this test needs a real manager rather than a string.
	km := apikey.NewManager(t.TempDir())
	admin := apikey.KeyPrefix + "12345678-1234-1234-1234-123456789abc"
	if _, err := km.EnsureBootstrap(admin); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	r := gin.New()
	r.POST("/api/stores", AdminAuth(km), func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	body := func() *strings.Reader { return strings.NewReader(`{"name":"a-store"}`) }

	// admin_key query parameter: no longer accepted.
	req := httptest.NewRequest(http.MethodPost, "/api/stores?admin_key="+admin, body())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("query admin_key: status %d, want 401", w.Code)
	}

	// X-Admin-Key still works — deprecated alias, resolved via the registry.
	req = httptest.NewRequest(http.MethodPost, "/api/stores", body())
	req.Header.Set("X-Admin-Key", admin)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("deprecated X-Admin-Key: status %d, want 200", w.Code)
	}

	// ...and so does the same value sent as X-API-Key, which is the
	// replacement callers should migrate to.
	req = httptest.NewRequest(http.MethodPost, "/api/stores", body())
	req.Header.Set("X-API-Key", admin)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("bootstrap key via X-API-Key: status %d, want 200", w.Code)
	}
}
