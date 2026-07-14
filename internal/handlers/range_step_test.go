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
