// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Regression test for issue #23: the middleware reflected any request
// Origin verbatim while also sending Access-Control-Allow-Credentials:
// true — the canonical CORS misconfiguration. Auth is header-based, so the
// policy is a plain wildcard with no credentials.
func TestCORSNoCredentialedOriginReflection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(method, "/x", nil)
		req.Header.Set("Origin", "http://evil.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Allow-Origin = %q, want * (must not reflect the request origin)", method, got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("%s: Allow-Credentials = %q, want unset", method, got)
		}
	}

	// Preflight still short-circuits
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "http://app.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("OPTIONS preflight status = %d, want 204", w.Code)
	}
}

// TestSecurityHeadersOnEveryResponse (issue #62): API responses carry
// nosniff and no-store so bodies are never content-sniffed or cached.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(method, "/x", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", method, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", method, got)
		}
	}
}
