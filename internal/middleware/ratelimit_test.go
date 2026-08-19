// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
)

// newLimitedRouter mirrors the production shape: AuthFailureLimiter in front
// of Auth on the store routes and in front of AdminAuth on store creation.
func newLimitedRouter(t *testing.T, l *AuthLimiter) (*gin.Engine, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	km := apikey.NewManager(t.TempDir())
	key, _, err := km.CreateForStore("teststore", "test")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// Store creation authorizes against the registry now (issue #176), so
	// the admin route needs a real bootstrap key rather than a config string.
	boot, err := km.EnsureBootstrap("")
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	r := gin.New()
	ok := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	r.POST("/api/stores", AuthFailureLimiter(l), AdminAuth(km), ok)
	sr := r.Group("/api/stores/:store")
	sr.Use(AuthFailureLimiter(l))
	sr.Use(Auth(km, apikey.AccessWrite))
	sr.GET("/data/newest", ok)
	return r, key, boot.Plaintext
}

func TestAuthFailureLimiterBlocksAfterThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewAuthLimiter(3, 30*time.Second, 15*time.Minute)
	l.now = func() time.Time { return now }
	r, key, _ := newLimitedRouter(t, l)

	do := func(headerKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/stores/teststore/data/newest", nil)
		if headerKey != "" {
			req.Header.Set("X-API-Key", headerKey)
		}
		req.RemoteAddr = "10.0.0.9:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Failures below the threshold still get plain 401s.
	badKey := "tsstore_00000000-0000-0000-0000-000000000000"
	for i := 0; i < 2; i++ {
		if w := do(badKey); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, w.Code)
		}
	}

	// The threshold-hitting failure is a 401; the NEXT request is blocked.
	if w := do(badKey); w.Code != http.StatusUnauthorized {
		t.Fatalf("threshold failure: status %d, want 401", w.Code)
	}
	w := do(key)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request while blocked: status %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("blocked response missing Retry-After header")
	}

	// After the block expires a valid key works and clears the state.
	now = now.Add(31 * time.Second)
	if w := do(key); w.Code != http.StatusOK {
		t.Fatalf("valid key after block expiry: status %d, want 200", w.Code)
	}

	// State was cleared: three fresh failures are needed to block again.
	for i := 0; i < 2; i++ {
		if w := do(badKey); w.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status %d, want 401", i+1, w.Code)
		}
	}
	if w := do(key); w.Code != http.StatusOK {
		t.Errorf("valid key below threshold: status %d, want 200", w.Code)
	}
}

func TestAuthFailureLimiterBackoffDoubles(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewAuthLimiter(2, 30*time.Second, 15*time.Minute)
	l.now = func() time.Time { return now }

	if d := l.RecordFailure("ip1"); d != 0 {
		t.Fatalf("first failure blocked: %v", d)
	}
	if d := l.RecordFailure("ip1"); d != 30*time.Second {
		t.Fatalf("first block = %v, want 30s", d)
	}
	now = now.Add(time.Minute) // block expired
	l.RecordFailure("ip1")
	if d := l.RecordFailure("ip1"); d != 60*time.Second {
		t.Fatalf("second block = %v, want 60s", d)
	}

	// Backoff is capped at maxBlock.
	for i := 0; i < 10; i++ {
		now = now.Add(time.Hour)
		l.RecordFailure("ip1")
		if d := l.RecordFailure("ip1"); d > 15*time.Minute {
			t.Fatalf("block %d = %v, exceeds 15m cap", i+3, d)
		}
	}
}

func TestAuthFailureLimiterAdminRoute(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewAuthLimiter(2, 30*time.Second, 15*time.Minute)
	l.now = func() time.Time { return now }
	r, _, bootstrapKey := newLimitedRouter(t, l)

	do := func(adminKey string) int {
		// A valid body: store creation validates the requested name before
		// authorizing (the name reaches a filepath join), so a bodyless
		// request would 400 before the credential is ever examined — and
		// this test is about auth failures reaching the limiter.
		req := httptest.NewRequest(http.MethodPost, "/api/stores", strings.NewReader(`{"name":"limiter-store"}`))
		if adminKey != "" {
			req.Header.Set("X-Admin-Key", adminKey)
		}
		req.RemoteAddr = "10.0.0.7:999"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := do(apikey.KeyPrefix + "00000000-0000-0000-0000-000000000000"); code != http.StatusUnauthorized {
		t.Fatalf("bad admin key: %d, want 401", code)
	}
	if code := do(apikey.KeyPrefix + "00000000-0000-0000-0000-000000000000"); code != http.StatusUnauthorized {
		t.Fatalf("bad admin key #2: %d, want 401", code)
	}
	// IP is now blocked — even the correct key gets 429 until the block ends.
	if code := do(bootstrapKey); code != http.StatusTooManyRequests {
		t.Fatalf("blocked admin request: %d, want 429", code)
	}
	now = now.Add(31 * time.Second)
	if code := do(bootstrapKey); code != http.StatusOK {
		t.Fatalf("valid admin key after block: %d, want 200", code)
	}
}

func TestAuthLimiterSweepDropsIdleEntries(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewAuthLimiter(5, 30*time.Second, 15*time.Minute)
	l.now = func() time.Time { return now }

	l.RecordFailure("stale-ip")
	now = now.Add(entryIdleTTL + sweepEvery + time.Second)
	l.RecordFailure("fresh-ip")

	l.mu.Lock()
	_, stale := l.entries["stale-ip"]
	_, fresh := l.entries["fresh-ip"]
	l.mu.Unlock()
	if stale {
		t.Error("idle entry survived sweep")
	}
	if !fresh {
		t.Error("fresh entry was swept")
	}
}
