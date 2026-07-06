// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Regression test for issue #30: request bodies were read into memory with
// no size cap.
func TestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(1 << 10)) // 1KB for the test
	r.POST("/x", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.String(http.StatusRequestEntityTooLarge, "too large")
			return
		}
		c.String(http.StatusOK, "ok")
	})

	// Over the limit: reads must fail
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, 4<<10)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d, want 413", w.Code)
	}

	// Under the limit: unaffected
	req = httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, 512)))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("normal body: status %d, want 200", w.Code)
	}
}
