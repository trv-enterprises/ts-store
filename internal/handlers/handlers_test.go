// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
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
	data.DELETE("/time/:timestamp", unifiedHandler.DeleteByTime)
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
		insertBody = `{"timestamp": ` + json.Number([]byte{byte('1'), byte('0'+i), '0', '0', '0', '0', '0', '0', '0', '0'}).String() + `, "data": {"index": ` + json.Number([]byte{byte('0' + i)}).String() + `}}`

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

func TestDeleteByTimestamp(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a V1 store (delete is only supported in V1)
	body := `{"name": "del-time-test", "storage_type": "v1"}`
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

	// Delete by timestamp
	req, _ = http.NewRequest("DELETE", "/api/stores/del-time-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Delete by timestamp failed: %d: %s", w.Code, w.Body.String())
	}

	// Verify it's gone
	req, _ = http.NewRequest("GET", "/api/stores/del-time-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected 404 after delete, got %d", w.Code)
	}
}
