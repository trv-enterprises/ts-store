// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/ws"
)

// WSConnectionsHandler handles outbound WebSocket connection management.
type WSConnectionsHandler struct {
	getManager func(storeName string) *ws.Manager
}

// NewWSConnectionsHandler creates a new connections handler.
// getManager is a function that returns the WS manager for a given store.
func NewWSConnectionsHandler(getManager func(storeName string) *ws.Manager) *WSConnectionsHandler {
	return &WSConnectionsHandler{
		getManager: getManager,
	}
}

// List handles GET /api/stores/:store/ws/connections
func (h *WSConnectionsHandler) List(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
		return
	}

	connections := manager.ListConnections()

	c.JSON(http.StatusOK, gin.H{
		"connections": connections,
	})
}

// CreateRequest represents a request to create a new outbound connection.
type CreateRequest struct {
	Mode             string            `json:"mode" binding:"required"`
	URL              string            `json:"url" binding:"required"`
	From             int64             `json:"from,omitempty"`
	Format           string            `json:"format,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Filter           string            `json:"filter,omitempty"`
	FilterIgnoreCase bool              `json:"filter_ignore_case,omitempty"`
	AggWindow        string            `json:"agg_window,omitempty"`
	AggFields        string            `json:"agg_fields,omitempty"`
	AggDefault       string            `json:"agg_default,omitempty"`
}

// Create handles POST /api/stores/:store/ws/connections
func (h *WSConnectionsHandler) Create(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
		return
	}

	var req CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	status, err := manager.CreateConnection(ws.CreateConnectionRequest{
		Mode:             req.Mode,
		URL:              req.URL,
		From:             req.From,
		Format:           req.Format,
		Headers:          req.Headers,
		Filter:           req.Filter,
		FilterIgnoreCase: req.FilterIgnoreCase,
		AggWindow:        req.AggWindow,
		AggFields:        req.AggFields,
		AggDefault:       req.AggDefault,
	})
	if err != nil {
		if err == ws.ErrInvalidMode {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusCreated, status)
}

// Get handles GET /api/stores/:store/ws/connections/:id
func (h *WSConnectionsHandler) Get(c *gin.Context) {
	storeName := middleware.GetStoreName(c)
	connID := c.Param("id")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
		return
	}

	status, err := manager.GetConnection(connID)
	if err != nil {
		if err == ws.ErrConnectionNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, status)
}

// Delete handles DELETE /api/stores/:store/ws/connections/:id
func (h *WSConnectionsHandler) Delete(c *gin.Context) {
	storeName := middleware.GetStoreName(c)
	connID := c.Param("id")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
		return
	}

	if err := manager.DeleteConnection(connID); err != nil {
		if err == ws.ErrConnectionNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "connection deleted"})
}
