// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/aggregation"
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

// Regression test for issue #20: pusher writes had no deadline while holding
// p.mu, so one stalled consumer (never reading; TCP buffers full) blocked the
// write forever — hanging Status(), the connections API, and
// DeleteConnection for the whole store. The write must now fail within the
// configured bound instead of blocking indefinitely.
func TestPusherWriteToStalledConsumerIsBounded(t *testing.T) {
	serverConn, _ := wsConnPair(t) // client never reads

	old := pusherWriteTimeout
	pusherWriteTimeout = 300 * time.Millisecond
	defer func() { pusherWriteTimeout = old }()

	p := &Pusher{conn: serverConn}
	bigResult := &aggregation.AggResult{
		Timestamp: 1000,
		Data:      map[string]interface{}{"blob": strings.Repeat("x", 1<<20)},
	}

	start := time.Now()
	var gotErr error
	for i := 0; i < 64; i++ { // fill the kernel buffers, then block
		if err := p.sendAggResult(bigResult); err != nil {
			gotErr = err
			break
		}
		if time.Since(start) > 10*time.Second {
			break
		}
	}
	elapsed := time.Since(start)

	if gotErr == nil {
		t.Fatal("writes to a stalled consumer never errored (unbounded block)")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("write block was not bounded by the deadline: took %v", elapsed)
	}
	if netErr, ok := gotErr.(interface{ Timeout() bool }); !ok || !netErr.Timeout() {
		t.Logf("note: error is not a timeout type (still bounded): %v", gotErr)
	}
}

// A stale (expired) deadline left on the conn must not poison later writes —
// each write arms a fresh deadline.
func TestPusherWriteRefreshesDeadline(t *testing.T) {
	serverConn, clientConn := wsConnPair(t)

	p := &Pusher{conn: serverConn}
	serverConn.SetWriteDeadline(time.Now().Add(-1 * time.Second)) // expired

	result := &aggregation.AggResult{
		Timestamp: 2000,
		Data:      map[string]interface{}{"v": 1.0},
	}
	if err := p.sendAggResult(result); err != nil {
		t.Fatalf("sendAggResult under expired stale deadline: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg map[string]interface{}
	if err := clientConn.ReadJSON(&msg); err != nil {
		t.Fatalf("client never received the message: %v", err)
	}
	if msg["type"] != "data" {
		t.Fatalf("unexpected message: %v", msg)
	}
}
