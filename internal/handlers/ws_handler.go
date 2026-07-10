// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"context"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Any origin may connect, consistent with the wildcard CORS policy in
	// middleware.CORS: auth is per-request (api_key on the handshake),
	// never ambient, so origin checks add no protection here.
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WSHandler handles inbound WebSocket connections. It tracks the active
// writer sessions so Shutdown can terminate them: hijacked connections are
// invisible to http.Server.Shutdown, and the stores they write to are closed
// right after — an untracked session would be mid-PutObject on a closed store.
type WSHandler struct {
	storeService *service.StoreService

	mu      sync.Mutex
	writers map[*wsWriter]struct{}
	wg      sync.WaitGroup
	closed  bool
}

// NewWSHandler creates a new WebSocket handler.
func NewWSHandler(storeService *service.StoreService) *WSHandler {
	return &WSHandler{
		storeService: storeService,
		writers:      make(map[*wsWriter]struct{}),
	}
}

// Shutdown stops all active writer sessions and waits for them to finish,
// or until ctx expires — then surviving connections are force-closed. Call
// after http.Server.Shutdown and before closing the stores.
func (h *WSHandler) Shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	for w := range h.writers {
		w.stop()
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		h.mu.Lock()
		for w := range h.writers {
			_ = w.conn.Close()
		}
		h.mu.Unlock()
		<-done
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
		respondStoreError(c, err)
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

	// Create and register the writer so Shutdown can reach this session.
	writer := newWSWriter(conn, st, format)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		conn.Close()
		return
	}
	h.writers[writer] = struct{}{}
	h.wg.Add(1)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.writers, writer)
		h.mu.Unlock()
		h.wg.Done()
	}()

	// Run in the current goroutine (blocking)
	writer.run()
}
