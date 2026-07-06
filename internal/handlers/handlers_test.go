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
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

func setupTestRouter(t *testing.T) (*gin.Engine, *service.StoreService, *apikey.Manager, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()

	cfg := &config.Config{
		Store: config.StoreConfig{
			BasePath:       tmpDir,
			DataBlockSize:  4096,
			IndexBlockSize: 4096,
			NumBlocks:      100,
		},
	}

	keyManager := apikey.NewManager(tmpDir)
	storeService := service.NewStoreService(cfg, keyManager)

	router := gin.New()
	router.Use(gin.Recovery())

	storeHandler := NewStoreHandler(storeService)
	unifiedHandler := NewUnifiedHandler(storeService)

	api := router.Group("/api")
	stores := api.Group("/stores")
	stores.POST("", storeHandler.Create)
	stores.GET("", storeHandler.List)

	storeRoutes := stores.Group("/:store")
	storeRoutes.Use(middleware.Auth(keyManager))
	storeRoutes.DELETE("", storeHandler.Delete)
	storeRoutes.GET("/stats", storeHandler.Stats)

	data := storeRoutes.Group("/data")
	data.POST("", unifiedHandler.Put)
	data.GET("/time/:timestamp", unifiedHandler.GetByTime)
	data.GET("/oldest", unifiedHandler.ListOldest)
	data.GET("/newest", unifiedHandler.ListNewest)
	data.GET("/range", unifiedHandler.ListRange)

	return router, storeService, keyManager, tmpDir
}

func TestCreateStore(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "test-store"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp service.CreateStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if resp.Name != "test-store" {
		t.Errorf("Expected name 'test-store', got '%s'", resp.Name)
	}

	if resp.APIKey == "" {
		t.Error("Expected API key in response")
	}

	if !apikey.ValidateKeyFormat(resp.APIKey) {
		t.Error("API key has invalid format")
	}
}

// TestCreateStoreRejectsUnknownField guards the `type` vs `data_type` footgun:
// posting an unknown field (e.g. `type` instead of `data_type`) must 400 rather
// than silently dropping it and creating a JSON store.
func TestCreateStoreRejectsUnknownField(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "typo-store", "type": "schema"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for unknown field, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateDuplicateStoreReturns409 confirms that re-creating an existing
// store yields 409 Conflict (not 500). Idempotent callers — e.g. the
// simulators deploy playbook — rely on the distinct status to treat
// "already exists" as a no-op instead of a server fault.
func TestCreateDuplicateStoreReturns409(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "dup-store"}`

	first, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	first.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, first)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", w1.Code, w1.Body.String())
	}

	second, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	second.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, second)
	if w2.Code != http.StatusConflict {
		t.Errorf("duplicate create: expected 409, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestCreateSchemaStore confirms the correct field name (`data_type`) actually
// produces a schema store via the API.
func TestCreateSchemaStore(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "schema-store", "data_type": "schema"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d: %s", w.Code, w.Body.String())
	}

	st, err := storeService.Get("schema-store")
	if err != nil {
		t.Fatalf("Get schema-store: %v", err)
	}
	if got := st.DataType().String(); got != "schema" {
		t.Errorf("Expected data type 'schema', got '%s'", got)
	}
}

func TestAuthRequired(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store first
	body := `{"name": "auth-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Try to access without API key
	req, _ = http.NewRequest("GET", "/api/stores/auth-test/stats", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 without API key, got %d", w.Code)
	}
}

func TestAuthWithValidKey(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "key-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Access with valid API key
	req, _ = http.NewRequest("GET", "/api/stores/key-test/stats", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 with valid key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInsertAndRetrieveJSON(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store (default is JSON type)
	body := `{"name": "data-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Insert JSON data
	insertBody := `{"timestamp": 1000000000, "data": {"message": "hello world"}}`

	req, _ = http.NewRequest("POST", "/api/stores/data-test/data", bytes.NewBufferString(insertBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Insert failed: %d: %s", w.Code, w.Body.String())
	}

	var insertResp ObjectHandleResponse
	json.Unmarshal(w.Body.Bytes(), &insertResp)

	if insertResp.Timestamp != 1000000000 {
		t.Errorf("Expected timestamp 1000000000, got %d", insertResp.Timestamp)
	}

	// Retrieve by timestamp
	req, _ = http.NewRequest("GET", "/api/stores/data-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Get by time failed: %d: %s", w.Code, w.Body.String())
	}

	var dataResp DataResponse
	json.Unmarshal(w.Body.Bytes(), &dataResp)

	// Check the data is returned as JSON
	dataBytes, _ := json.Marshal(dataResp.Data)
	var msg map[string]string
	json.Unmarshal(dataBytes, &msg)
	if msg["message"] != "hello world" {
		t.Errorf("Data mismatch: got %v", dataResp.Data)
	}
}

func TestListNewest(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "list-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Insert multiple entries
	for i := 0; i < 5; i++ {
		insertBody := `{"timestamp": ` + string(rune('0'+i)) + `000000000, "data": {"index": ` + string(rune('0'+i)) + `}}`
		insertBody = `{"timestamp": ` + json.Number([]byte{byte('1'), byte('0' + i), '0', '0', '0', '0', '0', '0', '0', '0'}).String() + `, "data": {"index": ` + json.Number([]byte{byte('0' + i)}).String() + `}}`

		req, _ = http.NewRequest("POST", "/api/stores/list-test/data", bytes.NewBufferString(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
	}

	// Get newest
	req, _ = http.NewRequest("GET", "/api/stores/list-test/data/newest?limit=3", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List newest failed: %d: %s", w.Code, w.Body.String())
	}

	var listResp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &listResp)

	if listResp.Count != 3 {
		t.Errorf("Expected 3 objects, got %d", listResp.Count)
	}
}

func TestRangeQuery(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "range-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Insert multiple entries with known timestamps
	timestamps := []int64{1000000000, 1001000000, 1002000000, 1003000000, 1004000000,
		1005000000, 1006000000, 1007000000, 1008000000, 1009000000}

	for _, ts := range timestamps {
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": ts,
			"data":      map[string]any{"ts": ts},
		})

		req, _ = http.NewRequest("POST", "/api/stores/range-test/data", bytes.NewBuffer(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
	}

	// Query range
	req, _ = http.NewRequest("GET", "/api/stores/range-test/data/range?start_time=1003000000&end_time=1007000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Range query failed: %d: %s", w.Code, w.Body.String())
	}

	var rangeResp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &rangeResp)

	if rangeResp.Count != 5 {
		t.Errorf("Expected 5 objects in range, got %d", rangeResp.Count)
	}
}

func TestInvalidAPIKey(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "invalid-key-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Try with invalid key
	req, _ = http.NewRequest("GET", "/api/stores/invalid-key-test/stats", nil)
	req.Header.Set("X-API-Key", "tsstore_00000000-0000-0000-0000-000000000000")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 with invalid key, got %d", w.Code)
	}
}

func TestDeleteStore(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "delete-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Delete the store
	req, _ = http.NewRequest("DELETE", "/api/stores/delete-test", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Delete failed: %d: %s", w.Code, w.Body.String())
	}

	// Verify store is gone - key should no longer work
	req, _ = http.NewRequest("GET", "/api/stores/delete-test/stats", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 after delete, got %d", w.Code)
	}
}

// TestDeleteByTimestampRemoved verifies that the delete-by-timestamp endpoint
// no longer exists (individual object deletion was a V1-only capability) and
// that the object remains retrievable.
func TestDeleteByTimestampRemoved(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "del-time-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Insert data
	insertBody := `{"timestamp": 1000000000, "data": {"message": "to delete"}}`
	req, _ = http.NewRequest("POST", "/api/stores/del-time-test/data", bytes.NewBufferString(insertBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// The DELETE route is gone, so the router has no handler for it.
	req, _ = http.NewRequest("DELETE", "/api/stores/del-time-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Delete-by-timestamp route should be removed (404), got %d", w.Code)
	}

	// Verify the object is still present.
	req, _ = http.NewRequest("GET", "/api/stores/del-time-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected object to remain retrievable, got %d", w.Code)
	}
}

// seedFilterWindowStore creates a JSON store and inserts records at fixed
// offsets before now, each carrying {"tag": "match", "ts": <offset label>}.
// Returns the store name and API key. Used by the filtered-window tests.
func seedFilterWindowStore(t *testing.T, router *gin.Engine, name string, offsets []time.Duration) string {
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

	// The store requires monotonically increasing timestamps, so insert
	// oldest-first: sort offsets descending (largest offset = oldest).
	sorted := append([]time.Duration(nil), offsets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })

	now := time.Now()
	for _, off := range sorted {
		ts := now.Add(-off).UnixNano()
		insertBody, _ := json.Marshal(map[string]any{
			"timestamp": ts,
			"data":      map[string]any{"tag": "match", "off": off.String()},
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

func queryNewest(t *testing.T, router *gin.Engine, name, apiKey, query string) DataListResponse {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/stores/"+name+"/data/newest?"+query, nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("newest?%s: %d: %s", query, w.Code, w.Body.String())
	}
	var resp DataListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal newest?%s: %v", query, err)
	}
	return resp
}

// TestFilteredNewestDefaultWindow confirms a filtered /newest with no explicit
// time bound only scans the last 1h: a matching record older than 1h is not
// returned, and the response reports window_applied with the 1h window.
func TestFilteredNewestDefaultWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// One match within 1h, one well outside it.
	apiKey := seedFilterWindowStore(t, router, "fw-default", []time.Duration{30 * time.Minute, 6 * time.Hour})

	resp := queryNewest(t, router, "fw-default", apiKey, "filter=match")

	if resp.Count != 1 {
		t.Errorf("expected 1 match within default 1h window, got %d", resp.Count)
	}
	if resp.Scan == nil {
		t.Fatal("expected scan info on filtered windowed query, got nil")
	}
	if !resp.Scan.WindowApplied {
		t.Error("expected window_applied=true")
	}
	if resp.Scan.Window != time.Hour.String() {
		t.Errorf("expected window %q, got %q", time.Hour.String(), resp.Scan.Window)
	}
}

// TestFilteredNewestWindowZeroUnbounded confirms window=0 restores the full
// store scan: the >1h-old match is returned and window_applied is false.
func TestFilteredNewestWindowZeroUnbounded(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-zero", []time.Duration{30 * time.Minute, 6 * time.Hour})

	resp := queryNewest(t, router, "fw-zero", apiKey, "filter=match&window=0")

	if resp.Count != 2 {
		t.Errorf("expected 2 matches with window=0 (full scan), got %d", resp.Count)
	}
	if resp.Scan != nil && resp.Scan.WindowApplied {
		t.Errorf("expected window_applied=false for window=0, got scan=%+v", resp.Scan)
	}
}

// TestFilteredNewestExplicitWindow confirms an explicit window widens the scan.
func TestFilteredNewestExplicitWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-explicit", []time.Duration{30 * time.Minute, 6 * time.Hour, 50 * time.Hour})

	resp := queryNewest(t, router, "fw-explicit", apiKey, "filter=match&window=12h")

	if resp.Count != 2 {
		t.Errorf("expected 2 matches within 12h window, got %d", resp.Count)
	}
	if resp.Scan == nil || resp.Scan.Window != (12*time.Hour).String() {
		t.Errorf("expected window 12h, got scan=%+v", resp.Scan)
	}
}

// TestFilteredNewestSinceOverridesWindow confirms an explicit since bound wins
// over the aggressive default (and no scan window signal is emitted).
func TestFilteredNewestSinceOverridesWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-since", []time.Duration{30 * time.Minute, 6 * time.Hour})

	resp := queryNewest(t, router, "fw-since", apiKey, "filter=match&since=24h")

	if resp.Count != 2 {
		t.Errorf("expected 2 matches with since=24h, got %d", resp.Count)
	}
	if resp.Scan != nil {
		t.Errorf("expected no scan signal when since overrides window, got %+v", resp.Scan)
	}
}

// TestFilteredNewestAggDefaultWindow confirms the aggregation default is 48h:
// a match at ~40h is included, one at ~50h is not.
func TestFilteredNewestAggDefaultWindow(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-agg", []time.Duration{40 * time.Hour, 50 * time.Hour})

	resp := queryNewest(t, router, "fw-agg", apiKey, "filter=match&agg_window=1m")

	if resp.Scan == nil {
		t.Fatal("expected scan info on filtered agg query, got nil")
	}
	if resp.Scan.Window != (48 * time.Hour).String() {
		t.Errorf("expected agg default window 48h, got %q", resp.Scan.Window)
	}
	// The 40h match aggregates into a window; the 50h match is outside 48h.
	// Each surviving record becomes its own 1m aggregation window, so exactly
	// one aggregated object is expected.
	if resp.Count != 1 {
		t.Errorf("expected 1 aggregated window (40h match only), got %d", resp.Count)
	}
}

// TestFilteredNewestLimitReached confirms limit_reached is set when the scan
// stops at the limit with matching records still unexamined within the window.
func TestFilteredNewestLimitReached(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Five matches all within the default 1h window.
	apiKey := seedFilterWindowStore(t, router, "fw-limit", []time.Duration{
		1 * time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute,
	})

	resp := queryNewest(t, router, "fw-limit", apiKey, "filter=match&limit=2")

	if resp.Count != 2 {
		t.Errorf("expected 2 returned (limit), got %d", resp.Count)
	}
	if resp.Scan == nil || !resp.Scan.LimitReached {
		t.Errorf("expected limit_reached=true, got scan=%+v", resp.Scan)
	}
}

// TestFilteredNewestExhaustedNotLimitReached confirms limit_reached is false
// when the window is exhausted below the limit.
func TestFilteredNewestExhaustedNotLimitReached(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-exhaust", []time.Duration{1 * time.Minute, 2 * time.Minute})

	resp := queryNewest(t, router, "fw-exhaust", apiKey, "filter=match&limit=10")

	if resp.Count != 2 {
		t.Errorf("expected 2 matches, got %d", resp.Count)
	}
	if resp.Scan == nil {
		t.Fatal("expected scan info, got nil")
	}
	if resp.Scan.LimitReached {
		t.Error("expected limit_reached=false when window exhausted below limit")
	}
}

// TestUnfilteredNewestNoScanSignal confirms an unfiltered /newest is unchanged:
// the window param is ignored and no scan signal is emitted.
func TestUnfilteredNewestNoScanSignal(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := seedFilterWindowStore(t, router, "fw-unfiltered", []time.Duration{30 * time.Minute, 6 * time.Hour})

	// No filter, even with a window param — must behave as before (full newest).
	resp := queryNewest(t, router, "fw-unfiltered", apiKey, "limit=10&window=1m")

	if resp.Count != 2 {
		t.Errorf("expected 2 records (window ignored without filter), got %d", resp.Count)
	}
	if resp.Scan != nil {
		t.Errorf("expected no scan signal on unfiltered query, got %+v", resp.Scan)
	}
}

// Regression test for issue #30: ?limit= was uncapped and the response slice
// is pre-allocated at that capacity — limit=2000000000 pre-allocated ~96GB.
// The value must clamp, not error and not allocate.
func TestQueryLimitClamped(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "limit-clamp"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var createResp service.CreateStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("create store: %v", err)
	}

	for i := 1; i <= 3; i++ {
		insertBody := fmt.Sprintf(`{"timestamp": %d, "data": {"i": %d}}`, int64(i)*1000000000, i)
		req, _ = http.NewRequest("POST", "/api/stores/limit-clamp/data", bytes.NewBufferString(insertBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", createResp.APIKey)
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("insert %d: %d: %s", i, w.Code, w.Body.String())
		}
	}

	req, _ = http.NewRequest("GET", "/api/stores/limit-clamp/data/newest?limit=2000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("clamped limit: status %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Objects []json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(out.Objects) != 3 {
		t.Errorf("expected all 3 records, got %d", len(out.Objects))
	}
}

// Regression test for issue #30 (WS side): a single frame could buffer
// unbounded bytes; gorilla must now terminate the connection at the limit.
func TestWSWriterRejectsOversizedFrame(t *testing.T) {
	serverConn, clientConn := wsConnPair(t)

	old := wsMaxMessageBytes
	wsMaxMessageBytes = 1 << 10 // 1KB for the test
	defer func() { wsMaxMessageBytes = old }()

	cfg := store.DefaultConfig()
	cfg.Name = "ws-frame-limit"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	defer s.Close()

	w := newWSWriter(serverConn, s, "full")
	done := make(chan struct{})
	go func() { defer close(done); w.run() }()

	// Oversized frame: the server must drop the connection, not buffer it.
	big := append([]byte(`{"data": {"blob": "`), bytes.Repeat([]byte("x"), 4<<10)...)
	big = append(big, []byte(`"}}`)...)
	if err := clientConn.WriteMessage(websocket.TextMessage, big); err != nil {
		t.Fatalf("client write: %v", err)
	}

	select {
	case <-done: // run() exited: connection terminated at the read limit
	case <-time.After(3 * time.Second):
		t.Fatal("server kept the connection after an oversized frame")
	}
}

// Tests for issue #34: POST /alerts/test dry-runs a condition without
// creating an alert.
func TestAlertConditionDryRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewAlertsHandler(nil) // Test uses no manager: rules are pure
	r.POST("/test", h.Test)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Matching record
	w := post(`{"condition": "temperature > 80", "data": {"temperature": 95}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("match case: %d: %s", w.Code, w.Body.String())
	}
	var resp TestAlertResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Matched {
		t.Error("expected matched=true for temperature 95 > 80")
	}
	if len(resp.Conditions) != 1 || resp.Conditions[0].Field != "temperature" {
		t.Errorf("parsed conditions: %+v", resp.Conditions)
	}

	// Non-matching record — the misspelled-field case the endpoint exists for
	w = post(`{"condition": "temperature > 80", "data": {"temprature": 95}}`)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Matched {
		t.Errorf("misspelled field must be matched=false, got %d %s", w.Code, w.Body.String())
	}

	// Compound condition
	w = post(`{"condition": "temperature > 80 AND humidity < 30", "data": {"temperature": 95, "humidity": 20}}`)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Matched || resp.LogicalOp != "AND" || len(resp.Conditions) != 2 {
		t.Errorf("compound: %+v", resp)
	}

	// Invalid condition -> 400 with the parse error
	w = post(`{"condition": "temperature > 80 extra", "data": {}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid condition: status %d, want 400", w.Code)
	}

	// Missing condition -> 400
	w = post(`{"data": {"temperature": 95}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing condition: status %d, want 400", w.Code)
	}
}
