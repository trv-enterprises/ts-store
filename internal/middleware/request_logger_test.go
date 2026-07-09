// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// captureLog redirects the standard logger into a buffer for the duration
// of the test and restores it afterward.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func newLoggedRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestLogger())
	r.GET("/ok", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/denied", func(c *gin.Context) { c.String(http.StatusUnauthorized, "no") })
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusInternalServerError, "sick") })
	return r
}

func TestRequestLoggerLogsErrors(t *testing.T) {
	buf := captureLog(t)
	r := newLoggedRouter()

	req := httptest.NewRequest(http.MethodGet, "/denied", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := buf.String()
	if !strings.Contains(got, "GET") || !strings.Contains(got, "/denied") || !strings.Contains(got, "401") {
		t.Errorf("401 log line missing method/path/status: %q", got)
	}
}

func TestRequestLoggerSilentOnSuccess(t *testing.T) {
	buf := captureLog(t)
	r := newLoggedRouter()

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := buf.String(); got != "" {
		t.Errorf("2xx request logged: %q", got)
	}
}

func TestRequestLoggerSkipsHealth(t *testing.T) {
	buf := captureLog(t)
	r := newLoggedRouter()

	// Even an erroring health check stays out of the log.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := buf.String(); got != "" {
		t.Errorf("health check logged: %q", got)
	}
}

func TestRequestLoggerRedactsCredentialQueryParams(t *testing.T) {
	buf := captureLog(t)
	r := newLoggedRouter()

	req := httptest.NewRequest(http.MethodGet,
		"/denied?api_key=ts_secret123&admin_key=ts_admin456&limit=5", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := buf.String()
	if strings.Contains(got, "ts_secret123") || strings.Contains(got, "ts_admin456") {
		t.Fatalf("credential leaked into log: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expected redaction marker in log line: %q", got)
	}
	if !strings.Contains(got, "limit=5") {
		t.Errorf("non-credential query params should survive: %q", got)
	}
}
