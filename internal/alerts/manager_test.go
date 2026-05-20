// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
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
