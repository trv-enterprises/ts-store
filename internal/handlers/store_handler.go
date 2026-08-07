// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package handlers contains HTTP request handlers.
package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// StoreHandler handles store management endpoints.
type StoreHandler struct {
	storeService *service.StoreService
	// keyManager is optional. When set, GET /api/stores filters the
	// listing to what a supplied key may read; when nil, the listing is
	// unfiltered (the pre-#138 behavior).
	keyManager *apikey.Manager
}

// NewStoreHandler creates a new store handler.
func NewStoreHandler(storeService *service.StoreService, keyManager *apikey.Manager) *StoreHandler {
	return &StoreHandler{
		storeService: storeService,
		keyManager:   keyManager,
	}
}

// Create handles POST /api/stores
// Creates a new store and returns the API key (shown only once).
//
// Binding is strict: unknown JSON fields are rejected with 400. This guards a
// known footgun — posting `type` instead of `data_type` would otherwise be
// silently dropped and yield a JSON store regardless of the intended type.
func (h *StoreHandler) Create(c *gin.Context) {
	var req service.CreateStoreRequest
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	resp, err := h.storeService.Create(&req)
	if err != nil {
		// A store that already exists is a client-side conflict, not a server
		// fault — return 409 so idempotent callers (e.g. the deploy playbook)
		// can treat "already exists" as a no-op rather than a 500.
		if errors.Is(err, store.ErrStoreExists) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// Delete handles DELETE /api/stores/:store
// Requires valid API key for the store.
func (h *StoreHandler) Delete(c *gin.Context) {
	storeName := c.Param("store")

	if err := h.storeService.Delete(storeName); err != nil {
		respondStoreError(c, err) // 409 when rollup dependents link here
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "store deleted"})
}

// Stats handles GET /api/stores/:store/stats
func (h *StoreHandler) Stats(c *gin.Context) {
	storeName := c.Param("store")

	stats, err := h.storeService.Stats(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}

	c.JSON(http.StatusOK, stats)
}

// Metrics handles GET /api/stores/:store/metrics
// Returns activity counters (writes, reads, rule evaluations, alert
// dispatches) since process start or the last reset. Unauthenticated —
// operational metadata, no record data exposed.
func (h *StoreHandler) Metrics(c *gin.Context) {
	storeName := c.Param("store")
	activity, err := h.storeService.Metrics(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, activity)
}

// ResetMetrics handles POST /api/stores/:store/metrics/reset
// Zeros all activity counters and advances the "since" timestamp.
// Authenticated — changes server-visible state.
func (h *StoreHandler) ResetMetrics(c *gin.Context) {
	storeName := c.Param("store")
	if err := h.storeService.ResetMetrics(storeName); err != nil {
		respondStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "metrics reset"})
}

// List handles GET /api/stores
// Returns all stores on disk as objects with name, data_type, and rollup role.
// NOTE: the response shape is an array of objects (each {name, data_type, role,
// ...}); earlier versions returned a flat array of name strings.
//
// BREAKING (issue #138): this endpoint now REQUIRES an API key and
// returns only the stores that key may read. It was previously public
// and unfiltered.
//
// The filtering is the point: it makes this a connection test that needs
// no store name (endpoint + key is enough), gives clients store
// discovery, and scopes that discovery to the caller's own authority —
// a read-only dashboard key sees exactly the stores it can chart. An
// unauthenticated listing would leak the full store inventory of the
// deployment, which is the one thing this endpoint should not do once
// keys are scoped.
//
// The admin key is deliberately NOT accepted here: it is the server
// tier (store lifecycle, key management) and holds no read grants, so
// "which stores can I read?" has no meaningful answer for it.
func (h *StoreHandler) List(c *gin.Context) {
	provided := c.GetHeader("X-API-Key")
	if provided == "" {
		if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			provided = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}
	if provided == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
		return
	}
	if h.keyManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "key manager unavailable"})
		return
	}

	key, err := h.keyManager.Resolve(provided)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
		return
	}
	// Reset this IP's auth-failure counter, same as the Auth middleware
	// does — without it a client that mistypes a key once keeps that
	// strike against it forever.
	c.Set(middleware.AuthPassedKey, true)

	all := h.storeService.ListAllInfo()
	filtered := make([]service.StoreInfo, 0, len(all))
	for _, info := range all {
		if key.Permits(info.Name, apikey.AccessRead) {
			filtered = append(filtered, info)
		}
	}
	c.JSON(http.StatusOK, gin.H{"stores": filtered})
}

// Reset handles POST /api/stores/:store/reset
// Clears all data but keeps store configuration and API keys.
func (h *StoreHandler) Reset(c *gin.Context) {
	storeName := c.Param("store")

	if err := h.storeService.Reset(storeName); err != nil {
		respondStoreError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "store reset"})
}
