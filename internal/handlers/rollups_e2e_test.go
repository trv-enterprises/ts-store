// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

func setupRollupsRouter(t *testing.T) (*gin.Engine, *service.StoreService, string) {
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
	rollupsHandler := NewRollupsHandler(storeService.GetRollupsManager)
	storeHandler := NewStoreHandler(storeService, keyManager)

	router.GET("/api/stores", storeHandler.List)
	sr := router.Group("/api/stores/:store")
	sr.Use(middleware.Auth(keyManager, apikey.AccessWrite))
	data := sr.Group("/data")
	data.POST("", unifiedHandler.Put)
	data.GET("/newest", unifiedHandler.ListNewest)
	sr.GET("/schema", schemaHandler.Get)
	sr.PUT("/schema", schemaHandler.Put)
	rg := sr.Group("/rollups")
	rg.GET("", rollupsHandler.List)
	rg.POST("", rollupsHandler.Create)
	rg.GET("/:id", rollupsHandler.Get)
	rg.DELETE("/:id", rollupsHandler.Delete)

	resp, err := storeService.Create(&service.CreateStoreRequest{Name: "sensors", DataType: "schema"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	t.Cleanup(func() { storeService.CloseAll() })
	return router, storeService, resp.APIKey
}

func TestRollupsEndToEnd(t *testing.T) {
	router, _, apiKey := setupRollupsRouter(t)

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

	// Source schema.
	if w := do("PUT", "/api/stores/sensors/schema",
		`{"fields":[{"index":1,"name":"temp","type":"float64"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT schema: %d %s", w.Code, w.Body.String())
	}

	// Create a rollup (auto-creates target sensors-1m).
	w := do("POST", "/api/stores/sensors/rollups",
		`{"window":"1m","agg_fields":"temp:avg","retention":"1d"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST rollup: %d %s", w.Code, w.Body.String())
	}
	var st struct {
		ID          string `json:"id"`
		TargetStore string `json:"target_store"`
		Window      string `json:"window"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.TargetStore != "sensors-1m" || st.Window != "1m" {
		t.Errorf("rollup status = %+v, want target sensors-1m window 1m", st)
	}

	// Listing rollups returns it.
	if w := do("GET", "/api/stores/sensors/rollups", ""); w.Code != http.StatusOK {
		t.Errorf("GET rollups: %d", w.Code)
	}

	// The target shows up in the store listing as a rollup of sensors.
	w = do("GET", "/api/stores", "")
	var list struct {
		Stores []struct {
			Name     string `json:"name"`
			Role     string `json:"role"`
			RollupOf string `json:"rollup_of"`
			Window   string `json:"window"`
		} `json:"stores"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	var foundRollup bool
	for _, s := range list.Stores {
		if s.Name == "sensors-1m" {
			foundRollup = true
			if s.Role != "rollup" || s.RollupOf != "sensors" || s.Window != "1m" {
				t.Errorf("listing for sensors-1m = %+v", s)
			}
		}
	}
	if !foundRollup {
		t.Errorf("sensors-1m not in store listing: %+v", list.Stores)
	}

	// The SAME api key works on the rollup target (linked keys): read its data.
	w = do("GET", "/api/stores/sensors-1m/data/newest", "")
	if w.Code != http.StatusOK {
		t.Fatalf("read target with source key: %d %s", w.Code, w.Body.String())
	}
	// Response echoes the rollup descriptor.
	var dataResp struct {
		Rollup *struct {
			Role     string `json:"role"`
			Window   string `json:"window"`
			RollupOf string `json:"rollup_of"`
		} `json:"rollup"`
	}
	json.Unmarshal(w.Body.Bytes(), &dataResp)
	if dataResp.Rollup == nil || dataResp.Rollup.Window != "1m" || dataResp.Rollup.RollupOf != "sensors" {
		t.Errorf("data response rollup descriptor = %+v", dataResp.Rollup)
	}

	// Delete the rollup.
	if w := do("DELETE", "/api/stores/sensors/rollups/"+st.ID, ""); w.Code != http.StatusOK {
		t.Errorf("DELETE rollup: %d %s", w.Code, w.Body.String())
	}
}

func TestRollupDeleteLifecycle(t *testing.T) {
	router, storeService, apiKey := setupRollupsRouter(t)

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

	if w := do("PUT", "/api/stores/sensors/schema",
		`{"fields":[{"index":1,"name":"temp","type":"float64"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT schema: %d %s", w.Code, w.Body.String())
	}

	createRollup := func() string {
		w := do("POST", "/api/stores/sensors/rollups",
			`{"window":"1m","agg_fields":"temp:avg","retention":"1d"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST rollup: %d %s", w.Code, w.Body.String())
		}
		var st struct {
			ID string `json:"id"`
		}
		json.Unmarshal(w.Body.Bytes(), &st)
		return st.ID
	}

	// Plain delete keeps the target; the source delete is then refused with
	// an error that names the surviving target.
	id := createRollup()
	if w := do("DELETE", "/api/stores/sensors/rollups/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE rollup: %d %s", w.Code, w.Body.String())
	}
	err := storeService.Delete("sensors")
	if !errors.Is(err, service.ErrHasDependents) {
		t.Fatalf("Delete(source) after plain rollup delete = %v, want ErrHasDependents", err)
	}
	if !strings.Contains(err.Error(), "sensors-1m") {
		t.Errorf("dependents error should name sensors-1m: %v", err)
	}

	// A malformed delete_target is rejected.
	id = createRollup()
	if w := do("DELETE", "/api/stores/sensors/rollups/"+id+"?delete_target=yesplease", ""); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE with bad delete_target: %d, want 400", w.Code)
	}

	// delete_target=true removes the target and its key link, so the source
	// can now be deleted.
	w := do("DELETE", "/api/stores/sensors/rollups/"+id+"?delete_target=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE rollup with delete_target: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/api/stores/sensors-1m/data/newest", ""); w.Code == http.StatusOK {
		t.Errorf("target sensors-1m still readable after delete_target: %d", w.Code)
	}
	if err := storeService.Delete("sensors"); err != nil {
		t.Errorf("Delete(source) after delete_target = %v, want nil", err)
	}
}
