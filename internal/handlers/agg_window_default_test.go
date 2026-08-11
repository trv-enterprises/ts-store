// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/service"
)

// seedAgedStore creates a JSON store and writes one record per offset, each
// carrying its offset as a label so a test can tell which records an
// aggregation actually saw.
func seedAgedStore(t *testing.T, router *gin.Engine, name string, offsets []time.Duration) (apiKey string) {
	t.Helper()

	body := fmt.Sprintf(`{"name": %q}`, name)
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: %d: %s", name, w.Code, w.Body.String())
	}
	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	for _, off := range offsets {
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": time.Now().Add(-off).UnixNano(),
			"data":      map[string]any{"v": 1.0, "age": off.String()},
		})
		req, _ = http.NewRequest("POST", "/api/stores/"+name+"/data", bytes.NewBuffer(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("insert at -%s: %d: %s", off, w.Code, w.Body.String())
		}
	}
	return createResp.APIKey
}

// agesOf collects the "age" labels present in an aggregated response, telling
// the test which seeded records fell inside the scanned span.
func agesOf(t *testing.T, resp DataListResponse) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, o := range resp.Objects {
		raw, err := json.Marshal(o.Data)
		if err != nil {
			t.Fatalf("re-marshal object data: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal object data: %v", err)
		}
		if age, _ := m["age"].(string); age != "" {
			out[age] = true
		}
	}
	return out
}

// TestUnfilteredAggDefaultWindow: aggregation on /newest with no filter and no
// since previously fetched the whole store (issue #150); it now defaults to the
// 48h aggregation lookback, reported in the scan field. A record older than the
// window is not aggregated.
func TestUnfilteredAggDefaultWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// One record inside the 48h default window, one well outside it.
	apiKey := seedAgedStore(t, router, "agg-default", []time.Duration{100 * time.Hour, 30 * time.Minute})

	resp := queryNewest(t, router, "agg-default", apiKey, "agg_window=10m&limit=100")

	ages := agesOf(t, resp)
	if !ages["30m0s"] || ages["100h0m0s"] {
		t.Errorf("default 48h window: expected only the 30m record aggregated, got %v", ages)
	}
	if resp.Scan == nil {
		t.Fatal("expected scan info on unfiltered aggregation with no time bound")
	}
	if !resp.Scan.WindowApplied || resp.Scan.Window != (48*time.Hour).String() {
		t.Errorf("expected window_applied with 48h window, got %+v", resp.Scan)
	}
}

// TestUnfilteredAggWindowOverrides: an explicit window widens the scan, and
// window=0 restores the full-store aggregation with no scan field. The
// window=0 case also guards the ordering fix: the unbounded fetch is
// newest-first, and feeding that to the ascending-only accumulator used to
// merge records 100h apart into one window — both ages surviving proves the
// records landed in separate buckets.
func TestUnfilteredAggWindowOverrides(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedAgedStore(t, router, "agg-override", []time.Duration{100 * time.Hour, 30 * time.Minute})

	// window=200h: both records fall inside.
	wide := queryNewest(t, router, "agg-override", apiKey, "agg_window=10m&window=200h&limit=100")
	if ages := agesOf(t, wide); !ages["30m0s"] || !ages["100h0m0s"] {
		t.Errorf("window=200h: expected both records aggregated, got %v", ages)
	}
	if wide.Scan == nil || wide.Scan.Window != (200*time.Hour).String() {
		t.Errorf("window=200h: unexpected scan info %+v", wide.Scan)
	}

	// window=0: explicit full-store scan, no scan field.
	full := queryNewest(t, router, "agg-override", apiKey, "agg_window=10m&window=0&limit=100")
	if ages := agesOf(t, full); !ages["30m0s"] || !ages["100h0m0s"] {
		t.Errorf("window=0: expected both records aggregated, got %v", ages)
	}
	if full.Scan != nil {
		t.Errorf("window=0: expected no scan field, got %+v", full.Scan)
	}
}

// TestUnfilteredAggSinceUnaffected: an explicit since bounds the scan itself;
// the default window and scan field stay out of it.
func TestUnfilteredAggSinceUnaffected(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedAgedStore(t, router, "agg-since", []time.Duration{100 * time.Hour, 30 * time.Minute})

	resp := queryNewest(t, router, "agg-since", apiKey, "agg_window=10m&since=1h&limit=100")
	if ages := agesOf(t, resp); !ages["30m0s"] || ages["100h0m0s"] {
		t.Errorf("since=1h: expected only the 30m record aggregated, got %v", ages)
	}
	if resp.Scan != nil {
		t.Errorf("since path: expected no scan field, got %+v", resp.Scan)
	}
}

// TestPlainNewestIgnoresWindow: without filter or aggregation the window param
// stays inert — no scan field, records of any age returned.
func TestPlainNewestIgnoresWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedAgedStore(t, router, "agg-plain", []time.Duration{100 * time.Hour, 30 * time.Minute})

	resp := queryNewest(t, router, "agg-plain", apiKey, "limit=10")
	if resp.Count != 2 {
		t.Errorf("plain newest: expected both records, got %d", resp.Count)
	}
	if resp.Scan != nil {
		t.Errorf("plain newest: expected no scan field, got %+v", resp.Scan)
	}
}
