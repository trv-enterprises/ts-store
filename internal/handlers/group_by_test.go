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

	"github.com/tviviano/ts-store/internal/service"
)

// seedMultiSeriesStore creates a JSON store and inserts one record per (tick,
// series) pair. seriesVals maps a series label (carried in field "c") to the
// constant numeric "v" that series reports, so a correct group_by keeps the two
// values separate while a time-only aggregation blends them. Records are written
// with strictly-increasing timestamps ending "now".
func seedMultiSeriesStore(t *testing.T, router http.Handler, name string, ticks int, seriesVals map[string]float64) (apiKey string) {
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

	// Deterministic label order so timestamps are reproducible.
	labels := make([]string, 0, len(seriesVals))
	for l := range seriesVals {
		labels = append(labels, l)
	}

	tick := time.Minute.Nanoseconds()
	start := time.Now().Add(-time.Duration(ticks) * time.Minute).UnixNano()
	var seq int64
	for i := 0; i < ticks; i++ {
		for _, label := range labels {
			ts := start + int64(i)*tick + seq
			seq++
			insertBody, _ := json.Marshal(map[string]any{
				"timestamp": ts,
				"data":      map[string]any{"v": seriesVals[label], "c": label},
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
	}
	return createResp.APIKey
}

// TestGroupByKeepsSeriesSeparate is the core fix: without group_by, step blends
// the two series into one meaningless value per bucket; with group_by, each
// series downsamples independently and carries its own label.
func TestGroupByKeepsSeriesSeparate(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// web always 10, db always 90.
	apiKey := seedMultiSeriesStore(t, router, "gb-core", 8, map[string]float64{"web": 10, "db": 90})

	// Without group_by: every bucket blends web+db → 50.
	blended, code := queryRange(t, router, "gb-core", apiKey, "since=1h&step=5m&limit=100")
	if code != http.StatusOK {
		t.Fatalf("blended query: %d", code)
	}
	for _, o := range blended.Objects {
		m, _ := o.Data.(map[string]any)
		if v, _ := m["v"].(float64); v != 50 {
			t.Fatalf("without group_by expected blended 50, got %v", v)
		}
	}

	// With group_by=c: two series, web=10 and db=90, each labeled.
	grouped, code := queryRange(t, router, "gb-core", apiKey, "since=1h&step=5m&group_by=c&limit=100")
	if code != http.StatusOK {
		t.Fatalf("grouped query: %d", code)
	}
	byLabel := map[string][]float64{}
	for _, o := range grouped.Objects {
		m, _ := o.Data.(map[string]any)
		label, _ := m["c"].(string)
		if label == "" {
			t.Errorf("grouped row missing its series label c: %v", m)
		}
		v, _ := m["v"].(float64)
		byLabel[label] = append(byLabel[label], v)
	}
	if len(byLabel) != 2 {
		t.Fatalf("expected 2 series, got %d (%v)", len(byLabel), byLabel)
	}
	for _, v := range byLabel["web"] {
		if v != 10 {
			t.Errorf("web series v = %v, want 10 (not blended)", v)
		}
	}
	for _, v := range byLabel["db"] {
		if v != 90 {
			t.Errorf("db series v = %v, want 90 (not blended)", v)
		}
	}
}

// TestGroupByOnNewest confirms group_by works on /data/newest too (shared
// aggregation path).
func TestGroupByOnNewest(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedMultiSeriesStore(t, router, "gb-newest", 8, map[string]float64{"web": 10, "db": 90})

	req, _ := http.NewRequest("GET", "/api/stores/gb-newest/data/newest?since=1h&step=5m&group_by=c&limit=100", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("newest grouped: %d: %s", w.Code, w.Body.String())
	}
	var resp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	labels := map[string]bool{}
	for _, o := range resp.Objects {
		m, _ := o.Data.(map[string]any)
		if l, _ := m["c"].(string); l != "" {
			labels[l] = true
		}
	}
	if !labels["web"] || !labels["db"] {
		t.Errorf("newest group_by: expected both web and db series, got %v", labels)
	}
}

// TestGroupByPerSeriesLimit: limit applies per series, so a small limit does not
// drop whole series — each series is truncated to the same depth.
func TestGroupByPerSeriesLimit(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// 20 ticks 1min apart → step=1m yields ~20 buckets per series.
	apiKey := seedMultiSeriesStore(t, router, "gb-limit", 20, map[string]float64{"web": 10, "db": 90})

	resp, code := queryRange(t, router, "gb-limit", apiKey, "since=2h&step=1m&group_by=c&limit=3")
	if code != http.StatusOK {
		t.Fatalf("limited grouped: %d", code)
	}
	counts := map[string]int{}
	for _, o := range resp.Objects {
		m, _ := o.Data.(map[string]any)
		if l, _ := m["c"].(string); l != "" {
			counts[l]++
		}
	}
	// Both series present, each capped at the per-series limit (not one dropped).
	if counts["web"] == 0 || counts["db"] == 0 {
		t.Fatalf("per-series limit dropped a series: %v", counts)
	}
	if counts["web"] > 3 || counts["db"] > 3 {
		t.Errorf("per-series limit=3 exceeded: %v", counts)
	}
}

// TestGroupByMissingFieldRemainder: grouping by a field absent from all records
// yields a single remainder series (no phantom label), equivalent to ungrouped.
func TestGroupByMissingFieldRemainder(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedMultiSeriesStore(t, router, "gb-missing", 8, map[string]float64{"web": 10, "db": 90})

	resp, code := queryRange(t, router, "gb-missing", apiKey, "since=1h&step=5m&group_by=nosuchfield&limit=100")
	if code != http.StatusOK {
		t.Fatalf("missing-field grouped: %d", code)
	}
	// All records fall into one remainder series → blended 50, and no
	// "nosuchfield" key is emitted.
	for _, o := range resp.Objects {
		m, _ := o.Data.(map[string]any)
		if _, ok := m["nosuchfield"]; ok {
			t.Errorf("remainder series should not emit the missing group key: %v", m)
		}
		if v, _ := m["v"].(float64); v != 50 {
			t.Errorf("remainder blended v = %v, want 50", v)
		}
	}
}

// seedIncrementingSeriesStore writes `ticks` rows per series where "v" starts
// at seriesStart[label] and increments by 1 each tick — so the newest row of
// each series has a distinct, known value. Strictly-increasing timestamps.
func seedIncrementingSeriesStore(t *testing.T, router http.Handler, name string, ticks int, seriesStart map[string]int) (apiKey string) {
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

	labels := make([]string, 0, len(seriesStart))
	for l := range seriesStart {
		labels = append(labels, l)
	}
	tick := time.Minute.Nanoseconds()
	start := time.Now().Add(-time.Duration(ticks) * time.Minute).UnixNano()
	var seq int64
	for i := 0; i < ticks; i++ {
		for _, label := range labels {
			ts := start + int64(i)*tick + seq
			seq++
			insertBody, _ := json.Marshal(map[string]any{
				"timestamp": ts,
				"data":      map[string]any{"v": float64(seriesStart[label] + i), "c": label},
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
	}
	return createResp.APIKey
}

// TestLatestByReturnsNewestPerGroup: latest_by=c returns exactly one row per
// distinct value — the newest — with no time window or aggregation.
func TestLatestByReturnsNewestPerGroup(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// web v: 1..5 (latest 5), db v: 101..105 (latest 105).
	apiKey := seedIncrementingSeriesStore(t, router, "lb-basic", 5, map[string]int{"web": 1, "db": 101})

	resp := queryNewest(t, router, "lb-basic", apiKey, "latest_by=c")
	if resp.Count != 2 {
		t.Fatalf("latest_by: expected 2 rows (one per series), got %d", resp.Count)
	}
	got := map[string]float64{}
	for _, o := range resp.Objects {
		m, _ := o.Data.(map[string]any)
		label, _ := m["c"].(string)
		v, _ := m["v"].(float64)
		got[label] = v
	}
	if got["web"] != 5 {
		t.Errorf("web latest v = %v, want 5", got["web"])
	}
	if got["db"] != 105 {
		t.Errorf("db latest v = %v, want 105", got["db"])
	}
}

// TestLatestByConflictsWithAggregation: latest_by + step is a 400 (they answer
// different questions — newest-per-group vs time-windowed rollup).
func TestLatestByConflictsWithAggregation(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedIncrementingSeriesStore(t, router, "lb-conflict", 3, map[string]int{"web": 1})
	req, _ := http.NewRequest("GET", "/api/stores/lb-conflict/data/newest?latest_by=c&step=5m", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("latest_by+step: expected 400, got %d", w.Code)
	}
}

// TestLatestByComposesWithFilter: latest_by narrows to the filtered subset,
// still one newest row per surviving group.
func TestLatestByComposesWithFilter(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedIncrementingSeriesStore(t, router, "lb-filter", 5, map[string]int{"web": 1, "db": 101})

	// Filter to web only; still latest-per-group → one row.
	resp := queryNewest(t, router, "lb-filter", apiKey, `latest_by=c&filter=c":"web`)
	if resp.Count != 1 {
		t.Fatalf("latest_by+filter: expected 1 row, got %d", resp.Count)
	}
	m, _ := resp.Objects[0].Data.(map[string]any)
	if label, _ := m["c"].(string); label != "web" {
		t.Errorf("filtered series = %q, want web", label)
	}
	if v, _ := m["v"].(float64); v != 5 {
		t.Errorf("web latest v = %v, want 5", v)
	}
}

// TestLatestByWithSinceIsOrderIndependent guards the ordering bug: the since=
// path returns handles ascending (oldest-first), so a naive first-seen dedup
// would return the OLDEST per group. latest_by must still return the newest.
func TestLatestByWithSinceIsOrderIndependent(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedIncrementingSeriesStore(t, router, "lb-since", 5, map[string]int{"web": 1, "db": 101})

	resp := queryNewest(t, router, "lb-since", apiKey, "latest_by=c&since=1h")
	if resp.Count != 2 {
		t.Fatalf("latest_by+since: expected 2 rows, got %d", resp.Count)
	}
	got := map[string]float64{}
	for _, o := range resp.Objects {
		m, _ := o.Data.(map[string]any)
		got[m["c"].(string)] = m["v"].(float64)
	}
	if got["web"] != 5 || got["db"] != 105 {
		t.Errorf("latest_by+since returned non-newest values: %v (want web=5, db=105)", got)
	}
}
