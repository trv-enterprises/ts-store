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

// seedSeriesSequence creates a JSON store and writes one record per label in
// order, with strictly-increasing timestamps ending near now. Repeating a label
// writes multiple records for that series, so a test can control exactly where
// in the newest-first order each series last appears.
func seedSeriesSequence(t *testing.T, router *gin.Engine, name string, labels []string) (apiKey string) {
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

	start := time.Now().Add(-time.Duration(len(labels)) * time.Second).UnixNano()
	for i, label := range labels {
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": start + int64(i)*time.Second.Nanoseconds(),
			"data":      map[string]any{"c": label, "seq": i},
		})
		req, _ = http.NewRequest("POST", "/api/stores/"+name+"/data", bytes.NewBuffer(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("insert %d/%s: %d: %s", i, label, w.Code, w.Body.String())
		}
	}
	return createResp.APIKey
}

// labelsOf extracts the series label ("c") from each returned object.
func labelsOf(t *testing.T, resp DataListResponse) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, o := range resp.Objects {
		m, ok := o.Data.(map[string]any)
		if !ok {
			t.Fatalf("object data is not a map: %T", o.Data)
		}
		label, _ := m["c"].(string)
		out[label] = true
	}
	return out
}

// TestLatestByDefaultScanBudget: latest_by with no time bound reports the
// default scan budget in the scan field. On a store smaller than the budget the
// result is complete and the budget is not marked reached.
func TestLatestByDefaultScanBudget(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedSeriesSequence(t, router, "sb-default", []string{"web", "db", "web", "db"})

	resp := queryNewest(t, router, "sb-default", apiKey, "latest_by=c")
	if resp.Count != 2 {
		t.Fatalf("expected 2 series, got %d", resp.Count)
	}
	if resp.Scan == nil {
		t.Fatal("expected scan info on latest_by with no time bound")
	}
	if resp.Scan.ScanLimit != defaultScanLimit {
		t.Errorf("scan_limit = %d, want default %d", resp.Scan.ScanLimit, defaultScanLimit)
	}
	if resp.Scan.ScanLimitReached {
		t.Error("scan_limit_reached should be false when the store is smaller than the budget")
	}
}

// TestLatestByScanLimitBoundsFetch: a series whose newest record lies beyond
// the scan budget drops out (and the response says so via scan_limit_reached);
// scan_limit=0 restores the full-store scan and finds it again.
func TestLatestByScanLimitBoundsFetch(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// "old" appears once, then 20 records of "new": with scan_limit=10 the
	// newest-first fetch never reaches "old".
	labels := []string{"old"}
	for i := 0; i < 20; i++ {
		labels = append(labels, "new")
	}
	apiKey := seedSeriesSequence(t, router, "sb-bound", labels)

	resp := queryNewest(t, router, "sb-bound", apiKey, "latest_by=c&scan_limit=10")
	if got := labelsOf(t, resp); !got["new"] || got["old"] {
		t.Errorf("scan_limit=10: expected only series \"new\", got %v", got)
	}
	if resp.Scan == nil || !resp.Scan.ScanLimitReached {
		t.Errorf("expected scan_limit_reached=true, got %+v", resp.Scan)
	}
	if resp.Scan != nil && resp.Scan.ScanLimit != 10 {
		t.Errorf("scan_limit = %d, want 10", resp.Scan.ScanLimit)
	}

	// scan_limit=0: explicit full scan, both series, no scan field.
	full := queryNewest(t, router, "sb-bound", apiKey, "latest_by=c&scan_limit=0")
	if got := labelsOf(t, full); !got["new"] || !got["old"] {
		t.Errorf("scan_limit=0: expected both series, got %v", got)
	}
	if full.Scan != nil {
		t.Errorf("scan_limit=0: expected no scan field, got %+v", full.Scan)
	}
}

// TestLatestByWithSinceSkipsScanBudget: an explicit since bounds the scan
// already, so the budget (and the scan field) does not apply.
func TestLatestByWithSinceSkipsScanBudget(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedSeriesSequence(t, router, "sb-since", []string{"web", "db"})

	resp := queryNewest(t, router, "sb-since", apiKey, "latest_by=c&since=1h&scan_limit=10")
	if resp.Count != 2 {
		t.Fatalf("expected 2 series, got %d", resp.Count)
	}
	if resp.Scan != nil {
		t.Errorf("since path: expected no scan field, got %+v", resp.Scan)
	}
}

// TestScanLimitValidation: malformed or negative scan_limit is a 400; a value
// above the cap is clamped (not rejected) and the clamped budget is reported.
func TestScanLimitValidation(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedSeriesSequence(t, router, "sb-valid", []string{"web"})

	for _, bad := range []string{"abc", "-5", "1.5"} {
		req, _ := http.NewRequest("GET", "/api/stores/sb-valid/data/newest?latest_by=c&scan_limit="+bad, nil)
		req.Header.Set("X-API-Key", apiKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("scan_limit=%s: expected 400, got %d", bad, w.Code)
		}
	}

	resp := queryNewest(t, router, "sb-valid", apiKey, fmt.Sprintf("latest_by=c&scan_limit=%d", maxScanLimit*2))
	if resp.Scan == nil || resp.Scan.ScanLimit != maxScanLimit {
		t.Errorf("expected scan_limit clamped to %d, got %+v", maxScanLimit, resp.Scan)
	}
}

// TestOldestFilteredScanBudget: a filtered /oldest bounds its fetch with the
// same budget instead of decoding the whole store when the filter matches
// little or nothing (the issue #140 sibling).
func TestOldestFilteredScanBudget(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// 20 non-matching records first (oldest), then the single match — so the
	// match lies beyond a 10-record oldest-first budget.
	labels := make([]string, 0, 21)
	for i := 0; i < 20; i++ {
		labels = append(labels, "filler")
	}
	labels = append(labels, "target")
	apiKey := seedSeriesSequence(t, router, "sb-oldest", labels)

	queryOldest := func(query string) DataListResponse {
		req, _ := http.NewRequest("GET", "/api/stores/sb-oldest/data/oldest?"+query, nil)
		req.Header.Set("X-API-Key", apiKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("oldest?%s: %d: %s", query, w.Code, w.Body.String())
		}
		var resp DataListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal oldest?%s: %v", query, err)
		}
		return resp
	}

	// Budget of 10 oldest records: the match at position 21 is never examined.
	resp := queryOldest("filter=target&scan_limit=10")
	if resp.Count != 0 {
		t.Errorf("scan_limit=10: expected 0 matches within budget, got %d", resp.Count)
	}
	if resp.Scan == nil || !resp.Scan.ScanLimitReached {
		t.Errorf("expected scan_limit_reached=true, got %+v", resp.Scan)
	}

	// scan_limit=0: full scan finds it; no scan field.
	full := queryOldest("filter=target&scan_limit=0")
	if full.Count != 1 {
		t.Errorf("scan_limit=0: expected 1 match, got %d", full.Count)
	}
	if full.Scan != nil {
		t.Errorf("scan_limit=0: expected no scan field, got %+v", full.Scan)
	}

	// Default budget covers the whole store here: match found, budget reported,
	// not marked reached.
	def := queryOldest("filter=target")
	if def.Count != 1 {
		t.Errorf("default budget: expected 1 match, got %d", def.Count)
	}
	if def.Scan == nil || def.Scan.ScanLimit != defaultScanLimit || def.Scan.ScanLimitReached {
		t.Errorf("default budget: unexpected scan info %+v", def.Scan)
	}

	// Unfiltered oldest is unchanged: no scan field.
	plain := queryOldest("limit=5")
	if plain.Scan != nil {
		t.Errorf("unfiltered oldest: expected no scan field, got %+v", plain.Scan)
	}
}
