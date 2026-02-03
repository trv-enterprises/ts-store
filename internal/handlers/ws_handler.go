// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for WebSocket
	},
}

// WSHandler handles inbound WebSocket connections.
type WSHandler struct {
	storeService *service.StoreService
}

// NewWSHandler creates a new WebSocket handler.
func NewWSHandler(storeService *service.StoreService) *WSHandler {
	return &WSHandler{
		storeService: storeService,
	}
}

// Write handles GET /api/stores/:store/ws/write
// Query params:
//   - api_key: Required for authentication
//   - format: For schema stores - "compact" or "full" (default: "full")
func (h *WSHandler) Write(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Get query parameters
	format := c.DefaultQuery("format", "full")

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already sends an error response
		return
	}

	// Create and run writer
	writer := newWSWriter(conn, st, format)

	// Run in the current goroutine (blocking)
	writer.run()
}
