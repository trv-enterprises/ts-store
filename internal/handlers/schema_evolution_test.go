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

// setupSchemaRouter builds a router that also wires the schema endpoints, and
// returns a helper for authenticated requests against a freshly created schema store.
func setupSchemaRouter(t *testing.T) (*gin.Engine, *service.StoreService, string, string) {
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

	unifiedHandler := NewUnifiedHandler(storeService)
	schemaHandler := NewSchemaHandler(storeService)

	storeRoutes := router.Group("/api/stores/:store")
	storeRoutes.Use(middleware.Auth(keyManager, apikey.AccessWrite))
	data := storeRoutes.Group("/data")
	data.POST("", unifiedHandler.Put)
	data.GET("/time/:timestamp", unifiedHandler.GetByTime)
	data.GET("/newest", unifiedHandler.ListNewest)
	storeRoutes.GET("/schema", schemaHandler.Get)
	storeRoutes.GET("/schema/versions", schemaHandler.ListVersions)
	storeRoutes.PUT("/schema", schemaHandler.Put)

	// Create a schema-type store.
	resp, err := storeService.Create(&service.CreateStoreRequest{
		Name:     "metrics",
		DataType: "schema",
	})
	if err != nil {
		t.Fatalf("create schema store failed: %v", err)
	}
	t.Cleanup(func() { storeService.CloseAll() })

	return router, storeService, resp.APIKey, tmpDir
}

func TestSchemaEvolutionEndToEnd(t *testing.T) {
	router, _, apiKey, _ := setupSchemaRouter(t)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r, _ = http.NewRequest(method, path, nil)
		} else {
			r, _ = http.NewRequest(method, path, bytes.NewBufferString(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("X-API-Key", apiKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	// Set schema v1.
	if w := do("PUT", "/api/stores/metrics/schema",
		`{"fields":[{"index":1,"name":"temperature","type":"float32"},{"index":2,"name":"humidity","type":"float32"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT schema v1: status %d: %s", w.Code, w.Body.String())
	}

	// Write a v1 record.
	if w := do("POST", "/api/stores/metrics/data",
		`{"timestamp":1000,"data":{"temperature":72.5,"humidity":45}}`); w.Code != http.StatusCreated {
		t.Fatalf("POST v1 record: status %d: %s", w.Code, w.Body.String())
	}

	// Evolve to v2 (append pressure).
	if w := do("PUT", "/api/stores/metrics/schema",
		`{"fields":[{"index":1,"name":"temperature","type":"float32"},{"index":2,"name":"humidity","type":"float32"},{"index":3,"name":"pressure","type":"float32"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT schema v2: status %d: %s", w.Code, w.Body.String())
	}

	// Write a v2 record.
	if w := do("POST", "/api/stores/metrics/data",
		`{"timestamp":2000,"data":{"temperature":70,"humidity":50,"pressure":1013}}`); w.Code != http.StatusCreated {
		t.Fatalf("POST v2 record: status %d: %s", w.Code, w.Body.String())
	}

	type dataResp struct {
		Timestamp     int64                  `json:"timestamp"`
		SchemaVersion uint32                 `json:"schema_version"`
		Data          map[string]interface{} `json:"data"`
	}
	type listResp struct {
		Objects []dataResp `json:"objects"`
	}

	// Helper: fetch newest (limit 10) with a given query, return records keyed by ts.
	fetch := func(query string) map[int64]dataResp {
		w := do("GET", "/api/stores/metrics/data/newest?limit=10"+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET newest%s: status %d: %s", query, w.Code, w.Body.String())
		}
		var lr listResp
		if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
			t.Fatalf("decode newest%s: %v (%s)", query, err, w.Body.String())
		}
		out := map[int64]dataResp{}
		for _, o := range lr.Objects {
			out[o.Timestamp] = o
		}
		return out
	}

	// Default (wide) view: v1 record has pressure:null; v2 record full.
	wide := fetch("")
	rec1 := wide[1000]
	if rec1.SchemaVersion != 1 {
		t.Errorf("wide rec1 schema_version = %d, want 1", rec1.SchemaVersion)
	}
	if _, ok := rec1.Data["pressure"]; !ok {
		t.Error("wide rec1 missing pressure key (expected null)")
	}
	if rec1.Data["pressure"] != nil {
		t.Errorf("wide rec1 pressure = %v, want null", rec1.Data["pressure"])
	}
	if rec1.Data["temperature"] != 72.5 {
		t.Errorf("wide rec1 temperature = %v, want 72.5", rec1.Data["temperature"])
	}
	rec2 := wide[2000]
	if rec2.SchemaVersion != 2 {
		t.Errorf("wide rec2 schema_version = %d, want 2", rec2.SchemaVersion)
	}
	if rec2.Data["pressure"] != float64(1013) {
		t.Errorf("wide rec2 pressure = %v, want 1013", rec2.Data["pressure"])
	}

	// Record view: v1 record has NO pressure key at all.
	rv := fetch("&schema_version=record")
	if _, ok := rv[1000].Data["pressure"]; ok {
		t.Error("record-view rec1 should not contain pressure key")
	}
	if rv[2000].Data["pressure"] != float64(1013) {
		t.Errorf("record-view rec2 pressure = %v, want 1013", rv[2000].Data["pressure"])
	}

	// Force version 1: v2 record's pressure must be dropped.
	v1view := fetch("&schema_version=1")
	if _, ok := v1view[2000].Data["pressure"]; ok {
		t.Error("schema_version=1 should drop rec2's pressure field")
	}

	// Compact view: numeric-keyed compact JSON.
	compact := fetch("&format=compact")
	if _, ok := compact[1000].Data["1"]; !ok {
		t.Errorf("compact view should have numeric keys, got %v", compact[1000].Data)
	}

	// Schema history endpoints.
	if w := do("GET", "/api/stores/metrics/schema?version=1", ""); w.Code != http.StatusOK {
		t.Errorf("GET schema?version=1: status %d", w.Code)
	} else {
		var sr SchemaResponse
		json.Unmarshal(w.Body.Bytes(), &sr)
		if len(sr.Fields) != 2 {
			t.Errorf("schema v1 fields = %d, want 2", len(sr.Fields))
		}
	}
	if w := do("GET", "/api/stores/metrics/schema?version=99", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET schema?version=99: status %d, want 404", w.Code)
	}
	if w := do("GET", "/api/stores/metrics/schema/versions", ""); w.Code != http.StatusOK {
		t.Errorf("GET schema/versions: status %d", w.Code)
	} else {
		var vr SchemaVersionsResponse
		json.Unmarshal(w.Body.Bytes(), &vr)
		if vr.CurrentVersion != 2 || len(vr.Versions) != 2 {
			t.Errorf("versions: current=%d count=%d, want 2/2", vr.CurrentVersion, len(vr.Versions))
		}
	}
}
