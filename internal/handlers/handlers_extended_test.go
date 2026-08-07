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

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// helper: create a store and return its API key
func createStore(t *testing.T, router http.Handler, name string, extraJSON ...string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name": %q`, name)
	for _, extra := range extraJSON {
		body += ", " + extra
	}
	body += "}"

	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("createStore(%s): expected 201, got %d: %s", name, w.Code, w.Body.String())
	}

	var resp service.CreateStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("createStore: failed to parse response: %v", err)
	}
	return resp.APIKey
}

// helper: insert JSON data into a store
func insertJSON(t *testing.T, router http.Handler, storeName, apiKey string, timestamp int64, data any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"timestamp": timestamp,
		"data":      data,
	})
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/stores/%s/data", storeName), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("insertJSON: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Store management endpoints ───────────────────────────────────────────────

// TestListStores covers the #138 behavior change: GET /api/stores now
// requires a key and returns only the stores that key may read.
func TestListStores(t *testing.T) {
	router, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	keyA := createStore(t, router, "list-a")
	createStore(t, router, "list-b")

	listWith := func(key string) (int, map[string]bool) {
		req, _ := http.NewRequest("GET", "/api/stores", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		var resp struct {
			Stores []struct {
				Name string `json:"name"`
			} `json:"stores"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		found := map[string]bool{}
		for _, s := range resp.Stores {
			found[s.Name] = true
		}
		return w.Code, found
	}

	// No key: rejected. The store inventory is no longer public.
	if code, _ := listWith(""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated list: got %d, want 401", code)
	}

	// A store's own key sees only that store.
	code, found := listWith(keyA)
	if code != http.StatusOK {
		t.Fatalf("list with store key: got %d, want 200", code)
	}
	if !found["list-a"] {
		t.Error("store key cannot see its own store")
	}
	if found["list-b"] {
		t.Error("store key saw a store it has no grant for")
	}

	// A wildcard read key sees both.
	wildKey, _, err := keyManager.Create("dashboard", []apikey.Grant{{
		Stores: "*", Access: []apikey.Access{apikey.AccessRead},
	}})
	if err != nil {
		t.Fatalf("create wildcard key: %v", err)
	}
	code, found = listWith(wildKey)
	if code != http.StatusOK {
		t.Fatalf("list with wildcard key: got %d, want 200", code)
	}
	if !found["list-a"] || !found["list-b"] {
		t.Errorf("wildcard key should see both stores, got %v", found)
	}

	// An invalid key is rejected rather than silently unfiltered.
	if code, _ := listWith("tsstore_00000000-0000-0000-0000-000000000000"); code != http.StatusUnauthorized {
		t.Errorf("invalid key: got %d, want 401", code)
	}
}

func TestStoreStats(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "stats-test")
	insertJSON(t, router, "stats-test", apiKey, 1000000000, map[string]string{"msg": "hi"})

	req, _ := http.NewRequest("GET", "/api/stores/stats-test/stats", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var stats store.StoreStats
	json.Unmarshal(w.Body.Bytes(), &stats)

	if stats.DataType == "" {
		t.Error("Expected data_type to be set")
	}
	if stats.NewestTimestamp == 0 {
		t.Error("Expected newest_timestamp to be non-zero after insert")
	}
}

func TestResetStore(t *testing.T) {
	router, storeService, keyManager, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Add the reset route (not in setupTestRouter by default)
	storeHandler := NewStoreHandler(storeService, keyManager)
	router.POST("/api/stores/:store/reset", storeHandler.Reset)

	apiKey := createStore(t, router, "reset-test")
	insertJSON(t, router, "reset-test", apiKey, 1000000000, map[string]string{"msg": "gone"})

	// Verify data exists before reset
	req, _ := http.NewRequest("GET", "/api/stores/reset-test/stats", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statsBefore store.StoreStats
	json.Unmarshal(w.Body.Bytes(), &statsBefore)
	if statsBefore.NewestTimestamp == 0 {
		t.Fatal("Expected data before reset")
	}

	// Reset
	req, _ = http.NewRequest("POST", "/api/stores/reset-test/reset", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Reset expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify stats show reset state
	req, _ = http.NewRequest("GET", "/api/stores/reset-test/stats", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var statsAfter store.StoreStats
	json.Unmarshal(w.Body.Bytes(), &statsAfter)

	// After reset, active blocks should be 0 or timestamps cleared
	if statsAfter.ActiveBlocks > statsBefore.ActiveBlocks {
		t.Errorf("Expected active blocks to not increase after reset, before=%d after=%d",
			statsBefore.ActiveBlocks, statsAfter.ActiveBlocks)
	}
}

func TestDeleteNonexistentStore(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// We need a valid API key for auth middleware, so create a real store first
	apiKey := createStore(t, router, "real-store")

	// Try deleting a non-existent store - auth middleware will reject because key
	// doesn't match "ghost-store"
	req, _ := http.NewRequest("DELETE", "/api/stores/ghost-store", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should fail with 401 (key doesn't match store) or 500
	if w.Code == http.StatusOK {
		t.Error("Expected error when deleting non-existent store, got 200")
	}
}

// ─── Data retrieval endpoints ─────────────────────────────────────────────────

func TestListOldest(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "oldest-test")

	// Insert 5 records with increasing timestamps
	for i := 0; i < 5; i++ {
		insertJSON(t, router, "oldest-test", apiKey, int64(1000000000+i*1000000), map[string]int{"i": i})
	}

	req, _ := http.NewRequest("GET", "/api/stores/oldest-test/data/oldest?limit=2", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Count != 2 {
		t.Fatalf("Expected 2 objects, got %d", resp.Count)
	}

	// Oldest should be first
	if resp.Objects[0].Timestamp > resp.Objects[1].Timestamp {
		t.Errorf("Expected oldest first: %d > %d", resp.Objects[0].Timestamp, resp.Objects[1].Timestamp)
	}
}

func TestListNewestWithIncludeData(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "newest-data-test")
	insertJSON(t, router, "newest-data-test", apiKey, 1000000000, map[string]string{"msg": "hello"})
	insertJSON(t, router, "newest-data-test", apiKey, 1001000000, map[string]string{"msg": "world"})

	// With include_data=true (default for newest)
	req, _ := http.NewRequest("GET", "/api/stores/newest-data-test/data/newest?limit=2", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Count != 2 {
		t.Fatalf("Expected 2 objects, got %d", resp.Count)
	}

	// Data should be present
	if resp.Objects[0].Data == nil {
		t.Error("Expected data to be included by default")
	}

	// With include_data=false
	req, _ = http.NewRequest("GET", "/api/stores/newest-data-test/data/newest?limit=2&include_data=false", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Objects[0].Data != nil {
		t.Error("Expected data to be excluded with include_data=false")
	}
}

func TestRangeQueryWithFilter(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "filter-test")

	insertJSON(t, router, "filter-test", apiKey, 1000000000, map[string]string{"color": "red"})
	insertJSON(t, router, "filter-test", apiKey, 1001000000, map[string]string{"color": "blue"})
	insertJSON(t, router, "filter-test", apiKey, 1002000000, map[string]string{"color": "red"})

	// Range query with filter - MatchesFilter does bytes.Contains, so use a substring that appears in JSON
	req, _ := http.NewRequest("GET",
		"/api/stores/filter-test/data/range?start_time=1000000000&end_time=1002000000&include_data=true&filter=red",
		nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Count != 2 {
		t.Errorf("Expected 2 red objects, got %d", resp.Count)
	}
}

func TestGetByTimestampNotFound(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "notfound-test")
	insertJSON(t, router, "notfound-test", apiKey, 1000000000, map[string]string{"msg": "exists"})

	req, _ := http.NewRequest("GET", "/api/stores/notfound-test/data/time/9999999999", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for missing timestamp, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Error handling ───────────────────────────────────────────────────────────

func TestCreateStoreMalformedJSON(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for malformed JSON, got %d", w.Code)
	}
}

func TestCreateStoreDuplicate(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	createStore(t, router, "dup-test")

	// Try creating the same store again
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(`{"name": "dup-test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code == http.StatusCreated {
		t.Error("Expected error when creating duplicate store, got 201")
	}
}

func TestInsertToNonexistentStore(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store so we have a valid API key
	apiKey := createStore(t, router, "exists-store")

	// Try inserting into a non-existent store - auth middleware will reject
	body := `{"timestamp": 1000000000, "data": {"msg": "test"}}`
	req, _ := http.NewRequest("POST", "/api/stores/no-such-store/data", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code == http.StatusCreated {
		t.Error("Expected error when inserting to non-existent store, got 201")
	}
}

func TestInsertInvalidContentType(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "ct-test")

	req, _ := http.NewRequest("POST", "/api/stores/ct-test/data", bytes.NewBufferString("some data"))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// JSON store should reject non-JSON content type
	if w.Code == http.StatusCreated {
		t.Error("Expected error for unsupported content type, got 201")
	}
}

func TestRangeQueryInvalidTimestamps(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "bad-range-test")

	// No start_time or end_time
	req, _ := http.NewRequest("GET", "/api/stores/bad-range-test/data/range", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing range params, got %d: %s", w.Code, w.Body.String())
	}

	// Invalid start_time
	req, _ = http.NewRequest("GET", "/api/stores/bad-range-test/data/range?start_time=notanumber", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid start_time, got %d: %s", w.Code, w.Body.String())
	}

	// Invalid end_time
	req, _ = http.NewRequest("GET", "/api/stores/bad-range-test/data/range?end_time=abc", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid end_time, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Data type tests ──────────────────────────────────────────────────────────

func TestInsertAndRetrieveText(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "text-test", `"data_type": "text"`)

	// Insert text data
	textData := "Hello, this is plain text data."
	req, _ := http.NewRequest("POST", "/api/stores/text-test/data", bytes.NewBufferString(textData))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Text insert failed: %d: %s", w.Code, w.Body.String())
	}

	var insertResp ObjectHandleResponse
	json.Unmarshal(w.Body.Bytes(), &insertResp)

	// Retrieve it
	req, _ = http.NewRequest("GET",
		fmt.Sprintf("/api/stores/text-test/data/time/%d", insertResp.Timestamp), nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Text retrieve failed: %d: %s", w.Code, w.Body.String())
	}

	var dataResp DataResponse
	json.Unmarshal(w.Body.Bytes(), &dataResp)

	// Text data comes back as a string
	text, ok := dataResp.Data.(string)
	if !ok {
		t.Fatalf("Expected string data, got %T", dataResp.Data)
	}
	if text != textData {
		t.Errorf("Expected %q, got %q", textData, text)
	}
}

func TestInsertAndRetrieveBinary(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "bin-test", `"data_type": "binary"`)

	// Insert binary data
	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
	req, _ := http.NewRequest("POST", "/api/stores/bin-test/data", bytes.NewReader(binaryData))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Binary insert failed: %d: %s", w.Code, w.Body.String())
	}

	var insertResp ObjectHandleResponse
	json.Unmarshal(w.Body.Bytes(), &insertResp)

	// Retrieve it
	req, _ = http.NewRequest("GET",
		fmt.Sprintf("/api/stores/bin-test/data/time/%d", insertResp.Timestamp), nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Binary retrieve failed: %d: %s", w.Code, w.Body.String())
	}

	var dataResp DataResponse
	json.Unmarshal(w.Body.Bytes(), &dataResp)

	// Binary data comes back as base64-encoded string
	b64, ok := dataResp.Data.(string)
	if !ok {
		t.Fatalf("Expected string (base64) data, got %T", dataResp.Data)
	}
	if b64 == "" {
		t.Error("Expected non-empty base64 data")
	}
}

// ─── V2 store tests ───────────────────────────────────────────────────────────

func TestCreateV2StoreWithOptions(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	body := `{"name": "v2-opts", "num_partitions": 4, "total_size": 1048576}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Name != "v2-opts" {
		t.Errorf("Expected name 'v2-opts', got %q", resp.Name)
	}
	if resp.APIKey == "" {
		t.Error("Expected API key")
	}

	// Verify via stats that the store is V2
	req, _ = http.NewRequest("GET", "/api/stores/v2-opts/stats", nil)
	req.Header.Set("X-API-Key", resp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Stats failed: %d: %s", w.Code, w.Body.String())
	}

	var stats store.StoreStats
	json.Unmarshal(w.Body.Bytes(), &stats)

	if stats.StorageVersion != 2 {
		t.Errorf("Expected storage_version 2, got %d", stats.StorageVersion)
	}
	if stats.NumPartitions != 4 {
		t.Errorf("Expected 4 partitions, got %d", stats.NumPartitions)
	}
}

func TestV2InsertAndRetrieve(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Default store is V2
	apiKey := createStore(t, router, "v2-cycle")

	// Insert
	insertJSON(t, router, "v2-cycle", apiKey, 2000000000, map[string]string{"key": "value"})

	// Retrieve by timestamp
	req, _ := http.NewRequest("GET", "/api/stores/v2-cycle/data/time/2000000000", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("V2 get failed: %d: %s", w.Code, w.Body.String())
	}

	var dataResp DataResponse
	json.Unmarshal(w.Body.Bytes(), &dataResp)

	if dataResp.Timestamp != 2000000000 {
		t.Errorf("Expected timestamp 2000000000, got %d", dataResp.Timestamp)
	}

	if dataResp.Data == nil {
		t.Error("Expected data in response")
	}

	// Also verify via newest
	req, _ = http.NewRequest("GET", "/api/stores/v2-cycle/data/newest?limit=1", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("V2 newest failed: %d: %s", w.Code, w.Body.String())
	}

	var listResp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &listResp)

	if listResp.Count != 1 {
		t.Errorf("Expected 1 object, got %d", listResp.Count)
	}
}

// TestRangeIncludesDataByDefault confirms /range defaults include_data
// to true, matching /newest, /oldest, and the documented default
// (issue #40 — it previously defaulted to false, so a spec-following
// client got records with no data).
func TestRangeIncludesDataByDefault(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	apiKey := createStore(t, router, "range-default-test")
	insertJSON(t, router, "range-default-test", apiKey, 1000000000, map[string]string{"msg": "hello"})
	insertJSON(t, router, "range-default-test", apiKey, 1001000000, map[string]string{"msg": "world"})

	// No include_data param: data must be present.
	req, _ := http.NewRequest("GET",
		"/api/stores/range-default-test/data/range?start_time=1000000000&end_time=1001000000", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp DataListResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 2 {
		t.Fatalf("Expected 2 objects, got %d", resp.Count)
	}
	if resp.Objects[0].Data == nil {
		t.Error("Expected /range to include data by default")
	}

	// Opt-out still works.
	req, _ = http.NewRequest("GET",
		"/api/stores/range-default-test/data/range?start_time=1000000000&end_time=1001000000&include_data=false", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Objects[0].Data != nil {
		t.Error("Expected data to be excluded with include_data=false")
	}
}
