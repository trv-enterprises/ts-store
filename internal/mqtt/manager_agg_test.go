// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package mqtt

import (
	"strings"
	"testing"

	"github.com/tviviano/ts-store/pkg/store"
)

func newJSONStore(t *testing.T, name string) *store.Store {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.Name = name
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Regression test for issue #29 (MQTT side): invalid aggregation configs
// were persisted and 201'd, then streamed raw records.
func TestMQTTCreateConnectionRejectsBadAggConfig(t *testing.T) {
	s := newJSONStore(t, "mqtt-agg-validate")
	m := NewManager(s, "mqtt-agg-validate")
	defer m.Stop()

	cases := []CreateConnectionRequest{
		{BrokerURL: "tcp://127.0.0.1:9", Topic: "t", AggWindow: "bogus", AggDefault: "avg"},
		{BrokerURL: "tcp://127.0.0.1:9", Topic: "t", AggWindow: "1m", AggDefault: "notafunc"},
		{BrokerURL: "tcp://127.0.0.1:9", Topic: "t", AggFields: "temp:avg"}, // fields without window
	}
	for _, req := range cases {
		if _, err := m.CreateConnection(req); err == nil {
			t.Errorf("CreateConnection(%+v) accepted an invalid aggregation config", req)
		} else if !strings.Contains(err.Error(), "agg") {
			t.Errorf("error should point at the aggregation config, got: %v", err)
		}
	}

	// Nothing persisted
	cfgList, err := m.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgList.Connections) != 0 {
		t.Errorf("invalid configs were persisted: %d connections", len(cfgList.Connections))
	}

	// Valid spec still creates
	st, err := m.CreateConnection(CreateConnectionRequest{
		BrokerURL: "tcp://127.0.0.1:9", Topic: "t", AggWindow: "1m", AggDefault: "avg",
	})
	if err != nil {
		t.Fatalf("valid aggregation config rejected: %v", err)
	}
	if st.ID == "" {
		t.Fatal("no status returned for valid create")
	}
}
