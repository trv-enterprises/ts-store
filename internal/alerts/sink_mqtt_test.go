// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/tviviano/ts-store/pkg/store"
)

// brokerAddrOrSkip returns a usable MQTT broker URL or skips the test.
// Honors TSSTORE_TEST_MQTT_URL, otherwise tries tcp://127.0.0.1:1883.
func brokerAddrOrSkip(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("TSSTORE_TEST_MQTT_URL"); url != "" {
		return url
	}
	// Probe localhost:1883 with a short TCP dial.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:1883", 300*time.Millisecond)
	if err != nil {
		t.Skipf("no MQTT broker reachable on tcp://127.0.0.1:1883 (set TSSTORE_TEST_MQTT_URL to override): %v", err)
	}
	conn.Close()
	return "tcp://127.0.0.1:1883"
}

func TestMQTTSinkEndToEnd(t *testing.T) {
	brokerURL := brokerAddrOrSkip(t)

	// Subscriber side: subscribe to a unique topic and collect messages.
	topic := "tsstore-test/alerts/" + t.Name()
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID("tsstore-test-sub-" + t.Name()).
		SetConnectTimeout(2 * time.Second)
	subClient := mqtt.NewClient(opts)
	if tok := subClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("subscriber connect: %v", tok.Error())
	}
	defer subClient.Disconnect(250)

	var mu sync.Mutex
	var received []map[string]interface{}
	tok := subClient.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		var p map[string]interface{}
		if err := json.Unmarshal(msg.Payload(), &p); err == nil {
			mu.Lock()
			received = append(received, p)
			mu.Unlock()
		}
	})
	if tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("subscribe: %v", tok.Error())
	}

	// Publisher side: worker + MQTTSink.
	sink := NewMQTTSink(brokerURL, topic, "", "", 1, "tsstore-test-pub-"+t.Name())
	defer sink.Close()

	s := newTestStore(t, "mqtt-sink")
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
	}, "")
	w.Start()
	defer w.Stop()

	time.Sleep(150 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatalf("subscriber received no MQTT message")
	}
	if received[0]["rule_name"] != "hot" {
		t.Errorf("rule_name: got %v", received[0]["rule_name"])
	}
}
