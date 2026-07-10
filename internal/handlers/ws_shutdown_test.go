// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// TestWSWriterStopUnblocksRun verifies stop() makes a running writer exit
// promptly (issue #56: closeCh used to be dead code — nothing ever closed
// it, and a writer blocked in ReadMessage survived shutdown).
func TestWSWriterStopUnblocksRun(t *testing.T) {
	serverConn, clientConn := wsConnPair(t)

	cfg := store.DefaultConfig()
	cfg.Name = "ws-stop"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	defer s.Close()

	w := newWSWriter(serverConn, s, "full")
	done := make(chan struct{})
	go func() {
		w.run()
		close(done)
	}()

	// Give run() a moment to enter ReadMessage, then stop it.
	time.Sleep(50 * time.Millisecond)
	w.stop()
	w.stop() // idempotent

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not exit within 2s of stop()")
	}

	// The client saw a going-away close frame (or at least a closed conn).
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Error("client read succeeded after stop; expected close")
	}
}

// TestWSHandlerShutdownTerminatesSessions runs a real handler with an active
// client session and verifies Shutdown ends the session and rejects new ones.
func TestWSHandlerShutdownTerminatesSessions(t *testing.T) {
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
	storeService := service.NewStoreService(cfg, apikey.NewManager(tmpDir))
	t.Cleanup(func() { storeService.CloseAll() })
	if _, err := storeService.Create(&service.CreateStoreRequest{Name: "wsstore"}); err != nil {
		t.Fatalf("create store: %v", err)
	}

	h := NewWSHandler(storeService)
	router := gin.New()
	// Stand-in for Auth: inject the store name the handler reads.
	router.GET("/ws/write", func(c *gin.Context) {
		c.Set(middleware.StoreNameKey, "wsstore")
		h.Write(c)
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/write"

	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// Prove the session is live: one write round-trips an ack.
	if err := client.WriteJSON(WSWriteRequest{Data: []byte(`{"v":1}`)}); err != nil {
		t.Fatalf("client write: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp WSWriteResponse
	if err := client.ReadJSON(&resp); err != nil || resp.Type != "ack" {
		t.Fatalf("expected ack, got %+v err=%v", resp, err)
	}

	// Shutdown must return promptly with the session still connected.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	h.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v with an idle session", elapsed)
	}

	// The client's connection is terminated.
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Error("client still connected after Shutdown")
	}

	// New sessions after shutdown are closed immediately.
	client2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		client2.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := client2.ReadMessage(); err == nil {
			t.Error("post-shutdown session accepted writes")
		}
		client2.Close()
	}

	// Stores can now be closed safely; no writer is mid-PutObject.
	if err := storeService.CloseAll(); err != nil {
		t.Errorf("CloseAll after WS shutdown: %v", err)
	}
}
