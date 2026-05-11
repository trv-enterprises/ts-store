// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/tviviano/ts-store/internal/notify"
)

const mqttSinkConnectTimeout = 10 * time.Second

// MQTTSink dispatches alerts as MQTT publishes at the configured QoS. The
// client is connected lazily on first Send so construction can't fail with
// a network error, and reconnects automatically (best-effort) on transient
// drops. Default QoS is 1 (at-least-once).
type MQTTSink struct {
	mu sync.Mutex

	brokerURL string
	topic     string
	username  string
	password  string
	qos       byte
	clientID  string

	client mqtt.Client // nil until first Send connects
}

// NewMQTTSink builds a sink. The client is not connected yet.
func NewMQTTSink(brokerURL, topic, username, password string, qos byte, clientID string) *MQTTSink {
	if qos > 2 {
		qos = 1
	}
	return &MQTTSink{
		brokerURL: brokerURL,
		topic:     topic,
		username:  username,
		password:  password,
		qos:       qos,
		clientID:  clientID,
	}
}

func (s *MQTTSink) ensureConnected() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil && s.client.IsConnected() {
		return nil
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(s.brokerURL)
	opts.SetClientID(s.clientID)
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(mqttSinkConnectTimeout)
	opts.SetWriteTimeout(mqttSinkConnectTimeout)
	if s.username != "" {
		opts.SetUsername(s.username)
	}
	if s.password != "" {
		opts.SetPassword(s.password)
	}

	c := mqtt.NewClient(opts)
	tok := c.Connect()
	tok.WaitTimeout(mqttSinkConnectTimeout)
	if err := tok.Error(); err != nil {
		return fmt.Errorf("mqtt connect %s: %w", s.brokerURL, err)
	}
	s.client = c
	return nil
}

func (s *MQTTSink) Send(alert notify.Alert) error {
	if err := s.ensureConnected(); err != nil {
		return err
	}

	body, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	tok := s.client.Publish(s.topic, s.qos, false, body)
	tok.WaitTimeout(mqttSinkConnectTimeout)
	if err := tok.Error(); err != nil {
		return fmt.Errorf("mqtt publish to %s: %w", s.topic, err)
	}
	return nil
}

func (s *MQTTSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && s.client.IsConnected() {
		s.client.Disconnect(250) // ms
	}
	return nil
}
