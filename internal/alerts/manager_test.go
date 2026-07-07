// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/tviviano/ts-store/pkg/store"
)

func newManagerFixture(t *testing.T) *Manager {
	t.Helper()
	s := newTestStore(t, "mgr")
	mgr := NewManager(s, "mgr")
	t.Cleanup(func() { mgr.Stop() })
	return mgr
}

func TestCreateAlertWebhook(t *testing.T) {
	mgr := newManagerFixture(t)
	st, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	if st.Type != "webhook" {
		t.Errorf("Status.Type: got %q, want webhook", st.Type)
	}
	if st.RuleName != "hot" {
		t.Errorf("Status.RuleName: got %q, want hot", st.RuleName)
	}
}

func TestCreateAlertWS(t *testing.T) {
	mgr := newManagerFixture(t)
	st, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "ws",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		WS:          &WSOptions{URL: "wss://example.com/stream"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	if st.Type != "ws" {
		t.Errorf("Status.Type: got %q, want ws", st.Type)
	}
}

func TestCreateAlertMQTT(t *testing.T) {
	mgr := newManagerFixture(t)
	st, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "mqtt",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		MQTT:        &MQTTOptions{BrokerURL: "tcp://broker.example:1883", Topic: "alerts/x"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	if st.Type != "mqtt" {
		t.Errorf("Status.Type: got %q, want mqtt", st.Type)
	}
}

// TestLoadAndStartLogsAndSkipsBrokenAlerts persists one valid and one
// unbuildable webhook alert directly (bypassing CreateAlert validation —
// simulating a config that stopped building after an upgrade), then
// confirms LoadAndStart starts the good one, skips the broken one, and
// logs the skip instead of silently dropping it (issue #36).
func TestLoadAndStartLogsAndSkipsBrokenAlerts(t *testing.T) {
	s := newTestStore(t, "loadstart")
	good := store.WebhookAlert{
		ID: "good1", URL: "https://example.com/hook",
		AlertCommon: store.AlertCommon{Name: "ok", Condition: "t > 0"},
	}
	broken := store.WebhookAlert{
		ID: "bad1", URL: "https://example.com/hook", PollInterval: "-5s",
		AlertCommon: store.AlertCommon{Name: "nope", Condition: "t > 0"},
	}
	if err := s.AddWebhookAlert(good); err != nil {
		t.Fatalf("AddWebhookAlert(good): %v", err)
	}
	if err := s.AddWebhookAlert(broken); err != nil {
		t.Fatalf("AddWebhookAlert(broken): %v", err)
	}

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prev)

	mgr := NewManager(s, "loadstart")
	t.Cleanup(func() { mgr.Stop() })
	if err := mgr.LoadAndStart(); err != nil {
		t.Fatalf("LoadAndStart: %v", err)
	}

	if _, err := mgr.GetAlert("good1"); err != nil {
		t.Errorf("good alert should have a running worker: %v", err)
	}
	if _, err := mgr.GetAlert("bad1"); err == nil {
		t.Error("broken alert must not have a worker")
	}
	if got := logBuf.String(); !strings.Contains(got, "failed to start persisted webhook alert bad1") {
		t.Errorf("expected a log line naming bad1, got: %q", got)
	}
}

func TestCreateAlertRejectsMismatchedOptions(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateAlertRequest
		wantSub string
	}{
		{
			name: "missing type",
			req: CreateAlertRequest{
				AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 0"},
				Webhook:     &WebhookOptions{URL: "https://e/"},
			},
			wantSub: "type is required",
		},
		{
			name: "unknown type",
			req: CreateAlertRequest{
				Type:        "smoke-signal",
				AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 0"},
			},
			wantSub: "unknown alert type",
		},
		{
			name: "webhook type without webhook options",
			req: CreateAlertRequest{
				Type:        "webhook",
				AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 0"},
			},
			wantSub: "requires \"webhook\" options",
		},
		{
			name: "webhook type with ws options",
			req: CreateAlertRequest{
				Type:        "webhook",
				AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 0"},
				Webhook:     &WebhookOptions{URL: "https://e/"},
				WS:          &WSOptions{URL: "wss://e/"},
			},
			wantSub: "must not include",
		},
		{
			name: "missing rule name",
			req: CreateAlertRequest{
				Type:    "webhook",
				Webhook: &WebhookOptions{URL: "https://e/"},
				AlertCommon: store.AlertCommon{Condition: "t > 0"}, // no Name
			},
			wantSub: "name is required",
		},
		{
			name: "mqtt missing topic",
			req: CreateAlertRequest{
				Type:        "mqtt",
				AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 0"},
				MQTT:        &MQTTOptions{BrokerURL: "tcp://b:1883"},
			},
			wantSub: "broker_url and mqtt.topic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newManagerFixture(t)
			_, err := mgr.CreateAlert(tt.req)
			if err == nil {
				t.Fatalf("CreateAlert: error = nil, want substring %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("CreateAlert: error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}
