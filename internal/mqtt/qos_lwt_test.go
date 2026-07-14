// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package mqtt

import (
	"strings"
	"testing"
)

// TestCreateConnectionQoS (issue #63): the data sink's QoS is configurable
// (1 or 2), 0/omitted defaults to 1 — the same convention MQTT alerts use —
// and out-of-range values are rejected at create.
func TestCreateConnectionQoS(t *testing.T) {
	s := newJSONStore(t, "mqtt-qos")
	m := NewManager(s, "mqtt-qos")
	defer m.Stop()

	// Omitted → 1.
	st, err := m.CreateConnection(CreateConnectionRequest{BrokerURL: "tcp://127.0.0.1:9", Topic: "t"})
	if err != nil {
		t.Fatalf("CreateConnection default qos: %v", err)
	}
	if st.QoS != 1 {
		t.Errorf("default qos = %d, want 1", st.QoS)
	}

	// Explicit 2 sticks.
	st, err = m.CreateConnection(CreateConnectionRequest{BrokerURL: "tcp://127.0.0.1:9", Topic: "t2", QoS: 2})
	if err != nil {
		t.Fatalf("CreateConnection qos=2: %v", err)
	}
	if st.QoS != 2 {
		t.Errorf("qos = %d, want 2", st.QoS)
	}

	// Out of range → 400-shaped error.
	if _, err := m.CreateConnection(CreateConnectionRequest{BrokerURL: "tcp://127.0.0.1:9", Topic: "t3", QoS: 3}); err == nil {
		t.Error("qos=3 accepted")
	} else if !strings.Contains(err.Error(), "qos") {
		t.Errorf("qos error should name the field: %v", err)
	}
}

// TestPusherNormalizesLegacyQoS: configs persisted before the qos field
// decode as 0, whose behavior was always QoS 1.
func TestPusherNormalizesLegacyQoS(t *testing.T) {
	s := newJSONStore(t, "mqtt-legacy-qos")
	p := NewPusher(s, "mqtt-legacy-qos", MQTTConnection{ID: "x", BrokerURL: "tcp://127.0.0.1:9", Topic: "t"})
	if p.config.QoS != 1 {
		t.Errorf("legacy qos normalized to %d, want 1", p.config.QoS)
	}
	if got := p.Status().QoS; got != 1 {
		t.Errorf("status qos = %d, want 1", got)
	}
}

// TestStatusTopic pins the liveness topic shape the LWT and explicit
// status publishes share.
func TestStatusTopic(t *testing.T) {
	s := newJSONStore(t, "mqtt-status-topic")
	p := NewPusher(s, "mqtt-status-topic", MQTTConnection{ID: "x", BrokerURL: "tcp://127.0.0.1:9", Topic: "sensors/raw"})
	if got := p.statusTopic(); got != "sensors/raw/status" {
		t.Errorf("statusTopic = %q, want sensors/raw/status", got)
	}
}
