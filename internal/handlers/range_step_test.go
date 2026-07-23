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

// seedNumericStore creates a JSON store and inserts one record per sample at
// consecutive nanosecond timestamps anchored at base, each carrying a numeric
// "v" field and a non-numeric "tag". Returns the store's API key and the base
// timestamp so tests can bound queries.
func seedNumericStore(t *testing.T, router http.Handler, name string, base int64, values []float64) (apiKey string) {
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

	for i, v := range values {
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": base + int64(i),
			"data":      map[string]any{"v": v, "tag": fmt.Sprintf("t%d", i)},
		})
		req, _ = http.NewRequest("POST", "/api/stores/"+name+"/data", bytes.NewBuffer(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("insert %d: %d: %s", i, w.Code, w.Body.String())
		}
	}
	return createResp.APIKey
}

func queryRange(t *testing.T, router http.Handler, name, apiKey, query string) (DataListResponse, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/stores/"+name+"/data/range?"+query, nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var resp DataListResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal range?%s: %v", query, err)
		}
	}
	return resp, w.Code
}

// windowBounds returns start/end query params spanning a fixed hour so all
// samples anchored inside it fall into a single step/agg window.
func windowBounds(base int64) string {
	// Align base to the start of its hour so a 1h window contains every
	// sample seeded at base+i.
	hour := int64(time.Hour)
	start := (base / hour) * hour
	return fmt.Sprintf("start_time=%d&end_time=%d", start, start+hour)
}

// TestRangeStepAveragesNumericFields (issue #7): step downsamples a range to a
// resolution, averaging numeric fields like Prometheus — unlike a bare
// agg_window, which takes the last value.
func TestRangeStepAveragesNumericFields(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	base := time.Now().Add(-30 * time.Minute).UnixNano()
	apiKey := seedNumericStore(t, router, "step-avg", base, []float64{10, 20, 30})

	// step=1h → one window, v averaged to 20.
	resp, code := queryRange(t, router, "step-avg", apiKey, windowBounds(base)+"&step=1h")
	if code != http.StatusOK {
		t.Fatalf("step query: %d", code)
	}
	if resp.Count != 1 {
		t.Fatalf("step=1h: expected 1 window, got %d", resp.Count)
	}
	got, _ := resp.Objects[0].Data.(map[string]any)
	if v, _ := got["v"].(float64); v != 20 {
		t.Errorf("step avg v = %v, want 20", v)
	}
	// Field name is preserved (single function → no _avg suffix).
	if _, suffixed := got["v_avg"]; suffixed {
		t.Error("step avg should keep field name v, not v_avg")
	}
	// Non-numeric field falls back to last.
	if tag, _ := got["tag"].(string); tag != "t2" {
		t.Errorf("non-numeric tag = %q, want last (t2)", tag)
	}
}

// TestRangeAggWindowStillTakesLast pins the pre-existing agg_window default so
// the step change didn't alter it: bare agg_window is decimation, not
// averaging.
func TestRangeAggWindowStillTakesLast(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	base := time.Now().Add(-30 * time.Minute).UnixNano()
	apiKey := seedNumericStore(t, router, "aggwin-last", base, []float64{10, 20, 30})

	resp, code := queryRange(t, router, "aggwin-last", apiKey, windowBounds(base)+"&agg_window=1h")
	if code != http.StatusOK {
		t.Fatalf("agg_window query: %d", code)
	}
	if resp.Count != 1 {
		t.Fatalf("agg_window=1h: expected 1 window, got %d", resp.Count)
	}
	got, _ := resp.Objects[0].Data.(map[string]any)
	if v, _ := got["v"].(float64); v != 30 {
		t.Errorf("agg_window default v = %v, want last (30)", v)
	}
}

// TestRangeStepExplicitAggWins: step supplies the window, but an explicit
// agg_default/agg_fields overrides step's implied avg.
func TestRangeStepExplicitAggWins(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	base := time.Now().Add(-30 * time.Minute).UnixNano()
	apiKey := seedNumericStore(t, router, "step-override", base, []float64{10, 20, 30})

	// step=1h with agg_default=max → 30, not the avg 20.
	resp, code := queryRange(t, router, "step-override", apiKey, windowBounds(base)+"&step=1h&agg_default=max")
	if code != http.StatusOK {
		t.Fatalf("step+agg_default query: %d", code)
	}
	got, _ := resp.Objects[0].Data.(map[string]any)
	if v, _ := got["v"].(float64); v != 30 {
		t.Errorf("step + agg_default=max v = %v, want 30", v)
	}
}

// TestRangeStepAndAggWindowConflict: setting both step and agg_window is a 400.
func TestRangeStepAndAggWindowConflict(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	base := time.Now().Add(-30 * time.Minute).UnixNano()
	apiKey := seedNumericStore(t, router, "step-conflict", base, []float64{10})

	_, code := queryRange(t, router, "step-conflict", apiKey, windowBounds(base)+"&step=1h&agg_window=1m")
	if code != http.StatusBadRequest {
		t.Errorf("step+agg_window: expected 400, got %d", code)
	}
}

// TestRangeStepInvalidDuration surfaces the step label in the error.
func TestRangeStepInvalidDuration(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	base := time.Now().Add(-30 * time.Minute).UnixNano()
	apiKey := seedNumericStore(t, router, "step-bad", base, []float64{10})

	req, _ := http.NewRequest("GET", "/api/stores/step-bad/data/range?"+windowBounds(base)+"&step=notaduration", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad step: expected 400, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("step")) {
		t.Errorf("error should name step, got: %s", w.Body.String())
	}
}

// seedNumericStoreSpaced is like seedNumericStore but spaces samples by a real
// duration ending "now", so relative (since=) queries and multi-bucket step
// downsampling are meaningful. Returns the API key.
func seedNumericStoreSpaced(t *testing.T, router http.Handler, name string, spacing time.Duration, values []float64) (apiKey string) {
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

	n := int64(len(values))
	start := time.Now().Add(-time.Duration(n-1) * spacing).UnixNano()
	for i, v := range values {
		ts := start + int64(i)*spacing.Nanoseconds()
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": ts,
			"data":      map[string]any{"v": v, "tag": fmt.Sprintf("t%d", i)},
		})
		req, _ = http.NewRequest("POST", "/api/stores/"+name+"/data", bytes.NewBuffer(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("insert %d: %d: %s", i, w.Code, w.Body.String())
		}
	}
	return createResp.APIKey
}

// TestRangeSinceStepDownsamples covers the exact real-world query shape a
// dashboard sends — a relative window plus step — which previously had no test
// (all step coverage used absolute start_time/end_time). A stale deploy that
// dropped the param would return raw rows here; this pins that since+step
// actually downsamples and averages.
func TestRangeSinceStepDownsamples(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// 12 samples 1min apart span ~11min; step=5m yields ~3 buckets, each far
	// fewer rows than the 12 raw samples. v alternates 0/10 → bucket avg ~5,
	// distinguishable from any single (last) value.
	vals := []float64{0, 10, 0, 10, 0, 10, 0, 10, 0, 10, 0, 10}
	apiKey := seedNumericStoreSpaced(t, router, "since-step", time.Minute, vals)

	raw, code := queryRange(t, router, "since-step", apiKey, "since=1h&limit=1000")
	if code != http.StatusOK {
		t.Fatalf("raw since query: %d", code)
	}
	if raw.Count != len(vals) {
		t.Fatalf("raw since=1h: expected %d rows, got %d", len(vals), raw.Count)
	}

	agg, code := queryRange(t, router, "since-step", apiKey, "since=1h&step=5m&limit=1000")
	if code != http.StatusOK {
		t.Fatalf("since+step query: %d", code)
	}
	if agg.Count >= raw.Count {
		t.Fatalf("since+step should downsample: got %d rows, raw was %d (step ignored?)", agg.Count, raw.Count)
	}
	// At least one bucket must show an averaged value (not a raw 0 or 10).
	averaged := false
	for _, o := range agg.Objects {
		m, _ := o.Data.(map[string]any)
		if v, ok := m["v"].(float64); ok && v != 0 && v != 10 {
			averaged = true
			break
		}
	}
	if !averaged {
		t.Errorf("since+step: no bucket shows an averaged v (step defaulted to last, not avg)")
	}
}

// TestNewestStepDownsamples pins that step works on /data/newest too — the
// other endpoint that shares the aggregation path but had zero step coverage.
func TestNewestStepDownsamples(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	vals := []float64{0, 10, 0, 10, 0, 10, 0, 10, 0, 10, 0, 10}
	apiKey := seedNumericStoreSpaced(t, router, "newest-step", time.Minute, vals)

	agg := queryNewest(t, router, "newest-step", apiKey, "since=1h&step=5m&limit=1000")
	if agg.Count == 0 || agg.Count >= len(vals) {
		t.Fatalf("newest since+step should downsample to <%d buckets, got %d", len(vals), agg.Count)
	}
	averaged := false
	for _, o := range agg.Objects {
		m, _ := o.Data.(map[string]any)
		if v, ok := m["v"].(float64); ok && v != 0 && v != 10 {
			averaged = true
			break
		}
	}
	if !averaged {
		t.Errorf("newest since+step: no averaged bucket (step avg not applied)")
	}
}

// TestNewestStepAndAggWindowConflict pins the mutual-exclusion 400 on /newest,
// mirroring the /range coverage — resolveAggWindow is shared, but the guard
// was only ever asserted through /range.
func TestNewestStepAndAggWindowConflict(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedNumericStoreSpaced(t, router, "newest-conflict", time.Minute, []float64{1, 2})
	req, _ := http.NewRequest("GET", "/api/stores/newest-conflict/data/newest?since=1h&step=5m&agg_window=1m", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("newest step+agg_window: expected 400, got %d", w.Code)
	}
}

// TestDataResponseIncludesDataType pins that read responses carry the store's
// data_type so consumers needn't a separate store-info call. Regression guard
// for the empty-string data_type a downstream consumer observed.
func TestDataResponseIncludesDataType(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedNumericStoreSpaced(t, router, "dtype", time.Minute, []float64{1, 2, 3})

	// Raw path.
	raw, code := queryRange(t, router, "dtype", apiKey, "since=1h")
	if code != http.StatusOK {
		t.Fatalf("raw range: %d", code)
	}
	if raw.DataType != "json" {
		t.Errorf("raw range data_type = %q, want json", raw.DataType)
	}
	// Aggregation path.
	agg, code := queryRange(t, router, "dtype", apiKey, "since=1h&step=5m")
	if code != http.StatusOK {
		t.Fatalf("agg range: %d", code)
	}
	if agg.DataType != "json" {
		t.Errorf("agg range data_type = %q, want json", agg.DataType)
	}
	// Newest.
	nw := queryNewest(t, router, "dtype", apiKey, "limit=1")
	if nw.DataType != "json" {
		t.Errorf("newest data_type = %q, want json", nw.DataType)
	}
}
