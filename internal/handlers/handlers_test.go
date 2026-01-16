// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/base64"
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
	dataHandler := NewDataHandler(storeService)

	api := router.Group("/api")
	stores := api.Group("/stores")
	stores.POST("", storeHandler.Create)
	stores.GET("", storeHandler.List)

	storeRoutes := stores.Group("/:store")
	storeRoutes.Use(middleware.Auth(keyManager))
	storeRoutes.DELETE("", storeHandler.Delete)
	storeRoutes.GET("/stats", storeHandler.Stats)

	data := storeRoutes.Group("/data")
	data.POST("", dataHandler.Insert)
	data.GET("/time/:timestamp", dataHandler.GetByTime)
	data.GET("/range", dataHandler.GetRange)
	data.GET("/oldest", dataHandler.GetOldest)
	data.GET("/newest", dataHandler.GetNewest)

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

func TestInsertAndRetrieve(t *testing.T) {
	router, storeService, _, _ := setupTestRouter(t)
	defer storeService.CloseAll()

	// Create a store
	body := `{"name": "data-test"}`
	req, _ := http.NewRequest("POST", "/api/stores", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var createResp service.CreateStoreResponse
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Insert data
	testData := "hello world"
	insertBody := InsertRequest{
		Timestamp: 1000000000,
		Data:      base64.StdEncoding.EncodeToString([]byte(testData)),
	}
	bodyBytes, _ := json.Marshal(insertBody)

	req, _ = http.NewRequest("POST", "/api/stores/data-test/data", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Insert failed: %d: %s", w.Code, w.Body.String())
	}

	var insertResp InsertResponse
	json.Unmarshal(w.Body.Bytes(), &insertResp)

	// Retrieve by timestamp
	req, _ = http.NewRequest("GET", "/api/stores/data-test/data/time/1000000000", nil)
	req.Header.Set("X-API-Key", createResp.APIKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Get by time failed: %d: %s", w.Code, w.Body.String())
	}

	var blockResp BlockResponse
	json.Unmarshal(w.Body.Bytes(), &blockResp)

	decodedData, _ := base64.StdEncoding.DecodeString(blockResp.Data)
	if string(decodedData) != testData {
		t.Errorf("Data mismatch: got '%s', want '%s'", decodedData, testData)
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

	// Insert multiple entries
	for i := 0; i < 10; i++ {
		insertBody := InsertRequest{
			Timestamp: int64(1000000000 + i*1000000),
			Data:      base64.StdEncoding.EncodeToString([]byte("data")),
		}
		bodyBytes, _ := json.Marshal(insertBody)

		req, _ = http.NewRequest("POST", "/api/stores/range-test/data", bytes.NewBuffer(bodyBytes))
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

	var rangeResp RangeResponse
	json.Unmarshal(w.Body.Bytes(), &rangeResp)

	if rangeResp.Count != 5 {
		t.Errorf("Expected 5 blocks in range, got %d", rangeResp.Count)
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
