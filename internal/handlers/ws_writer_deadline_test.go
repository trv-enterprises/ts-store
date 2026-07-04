// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/pkg/store"
)

// wsConnPair returns a connected server-side and client-side WebSocket pair.
func wsConnPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverCh := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverCh <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	server = <-serverCh
	t.Cleanup(func() { server.Close() })
	return server, client
}

// Regression test for issue #27: the HTTP server's WriteTimeout arms an
// absolute deadline on the underlying conn which the hijacked WS connection
// inherits — after it elapsed (~30s into a session), every ack write failed
// silently because sendAck/sendError never set their own deadlines.
func TestWSWriterAckSurvivesExpiredInheritedDeadline(t *testing.T) {
	serverConn, clientConn := wsConnPair(t)

	cfg := store.DefaultConfig()
	cfg.Name = "ws-ack-deadline"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	defer s.Close()

	handle, err := s.PutObject(1000, []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	w := newWSWriter(serverConn, s, "full")

	// Mimic the inherited, already-expired server WriteTimeout deadline.
	serverConn.SetWriteDeadline(time.Now().Add(-1 * time.Second))

	if err := w.sendAck(handle); err != nil {
		t.Fatalf("sendAck under expired inherited deadline: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp WSWriteResponse
	if err := clientConn.ReadJSON(&resp); err != nil {
		t.Fatalf("client never received the ack: %v", err)
	}
	if resp.Type != "ack" || resp.Timestamp != 1000 {
		t.Fatalf("unexpected ack payload: %+v", resp)
	}

	// Same for the error path.
	serverConn.SetWriteDeadline(time.Now().Add(-1 * time.Second))
	if err := w.sendError("boom"); err != nil {
		t.Fatalf("sendError under expired inherited deadline: %v", err)
	}
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := clientConn.ReadJSON(&resp); err != nil {
		t.Fatalf("client never received the error frame: %v", err)
	}
	if resp.Type != "error" || resp.Message != "boom" {
		t.Fatalf("unexpected error payload: %+v", resp)
	}
}
