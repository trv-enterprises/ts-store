// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/store"
)

func TestWebhookSinkEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", got)
		}
		if got := r.Header.Get("X-Test-Header"); got != "abc" {
			t.Errorf("custom header missing/wrong: got %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("payload not JSON: %v", err)
		}
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(srv.URL, map[string]string{"X-Test-Header": "abc"}, "")
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	s := newTestStore(t, "wh-sink")
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	startPolling(t, s, w)
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})

	// Wait for the webhook server to receive at least one POST.
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
		t.Fatalf("webhook server received no POST")
	}
	if received[0]["rule_name"] != "hot" {
		t.Errorf("rule_name: got %v, want \"hot\"", received[0]["rule_name"])
	}
	data, ok := received[0]["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload.data not a map: %T", received[0]["data"])
	}
	if data["temperature"].(float64) != 95.0 {
		t.Errorf("payload.data.temperature: got %v", data["temperature"])
	}
}
