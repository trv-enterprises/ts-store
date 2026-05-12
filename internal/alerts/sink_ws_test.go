// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/pkg/store"
)

// startStubWSServer accepts WS upgrades and records the first JSON message
// from each connection. Returns the ws:// URL to dial.
func startStubWSServer(t *testing.T, recv *[]map[string]interface{}, mu *sync.Mutex) string {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		// Read one message; ignore subsequent ones (the client sends a
		// close frame after the data frame).
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Errorf("not JSON: %v", err)
			return
		}
		mu.Lock()
		*recv = append(*recv, payload)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	return strings.Replace(srv.URL, "http://", "ws://", 1)
}

func TestWSSinkEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]interface{}
	wsURL := startStubWSServer(t, &received, &mu)

	sink := NewWSSink(wsURL, nil)

	s := newTestStore(t, "ws-sink")
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
	}, "")
	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatalf("ws stub server received no frame")
	}
	if received[0]["type"] != "alert" {
		t.Errorf("frame type: got %v, want \"alert\"", received[0]["type"])
	}
	a, ok := received[0]["alert"].(map[string]interface{})
	if !ok {
		t.Fatalf("frame.alert not a map: %T", received[0]["alert"])
	}
	if a["rule_name"] != "hot" {
		t.Errorf("rule_name: got %v", a["rule_name"])
	}
	data, _ := a["data"].(map[string]interface{})
	if data["temperature"].(float64) != 95.0 {
		t.Errorf("alert.data.temperature: got %v", data["temperature"])
	}
}
