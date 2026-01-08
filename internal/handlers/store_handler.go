// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
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

// List handles GET /api/stores
// Returns list of open stores.
func (h *StoreHandler) List(c *gin.Context) {
	stores := h.storeService.ListOpen()
	c.JSON(http.StatusOK, gin.H{"stores": stores})
}
