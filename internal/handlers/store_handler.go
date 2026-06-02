// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package handlers contains HTTP request handlers.
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/service"
)

// StoreHandler handles store management endpoints.
type StoreHandler struct {
	storeService *service.StoreService
}

// NewStoreHandler creates a new store handler.
func NewStoreHandler(storeService *service.StoreService) *StoreHandler {
	return &StoreHandler{
		storeService: storeService,
	}
}

// Create handles POST /api/stores
// Creates a new store and returns the API key (shown only once).
func (h *StoreHandler) Create(c *gin.Context) {
	var req service.CreateStoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.storeService.Create(&req)
	if err != nil {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "store deleted"})
}

// Stats handles GET /api/stores/:store/stats
func (h *StoreHandler) Stats(c *gin.Context) {
	storeName := c.Param("store")

	stats, err := h.storeService.Stats(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "metrics reset"})
}

// List handles GET /api/stores
// Returns all stores on disk as objects with name, data_type, and rollup role.
// NOTE: the response shape is an array of objects (each {name, data_type, role,
// ...}); earlier versions returned a flat array of name strings.
func (h *StoreHandler) List(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"stores": h.storeService.ListAllInfo()})
}

// Reset handles POST /api/stores/:store/reset
// Clears all data but keeps store configuration and API keys.
func (h *StoreHandler) Reset(c *gin.Context) {
	storeName := c.Param("store")

	if err := h.storeService.Reset(storeName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "store reset"})
}
