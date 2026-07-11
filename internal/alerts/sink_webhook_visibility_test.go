// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tviviano/ts-store/internal/notify"
	"github.com/tviviano/ts-store/pkg/store"
)

// Regression tests for issue #18: webhook delivery failures were invisible —
// a queue-full drop returned nil (counted as fired), and async HTTP failures
// only reached the process log, never the worker's status.

func TestWebhookSinkQueueFullReturnsError(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(srv.URL, nil, "5s")
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}
	defer sink.Close()
	defer close(block) // runs first: unblock handlers so sink.Close drains fast

	// The endpoint never responds, so the queue (cap 100 + 1 in flight)
	// must fill and Send must start reporting drops.
	var gotErr error
	for i := 0; i < 205; i++ {
		if err := sink.Send(notify.Alert{RuleName: "flood"}); err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("205 sends against a blocked endpoint never returned a queue-full error")
	}
}

func TestWebhookDeliveryFailureSurfacesInStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(srv.URL, nil, "2s")
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}

	s := newTestStore(t, "webhook-vis")
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	sink.SetOnError(w.noteDeliveryFailure) // as the manager wires it
	startPolling(t, s, w)
	defer w.Stop()

	time.Sleep(150 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := w.Status()
		if st.DeliveryFailures >= 1 && st.LastError != "" {
			return // failure surfaced in status
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := w.Status()
	t.Fatalf("delivery failure never surfaced: delivery_failures=%d last_error=%q",
		st.DeliveryFailures, st.LastError)
}
