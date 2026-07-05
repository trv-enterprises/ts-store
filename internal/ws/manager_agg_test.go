// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package ws

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

// Regression test for issue #29: a connection with an invalid aggregation
// config returned 201 Created and then silently streamed raw records —
// initAggregation only logged "continuing without aggregation".
func TestWSCreateConnectionRejectsBadAggConfig(t *testing.T) {
	s := newJSONStore(t, "ws-agg-validate")
	m := NewManager(s, "ws-agg-validate")
	defer m.Stop()

	cases := []CreateConnectionRequest{
		{Mode: "push", URL: "ws://127.0.0.1:9/x", AggWindow: "bogus", AggDefault: "avg"},
		{Mode: "push", URL: "ws://127.0.0.1:9/x", AggWindow: "1m", AggDefault: "notafunc"},
		{Mode: "push", URL: "ws://127.0.0.1:9/x", AggWindow: "1m", AggFields: "temp:notafunc"},
		{Mode: "push", URL: "ws://127.0.0.1:9/x", AggFields: "temp:avg"}, // fields without window: silently ignored before
	}
	for _, req := range cases {
		if _, err := m.CreateConnection(req); err == nil {
			t.Errorf("CreateConnection(%+v) accepted an invalid aggregation config", req)
		} else if !strings.Contains(err.Error(), "agg") {
			t.Errorf("error should point at the aggregation config, got: %v", err)
		}
	}

	// Nothing may have been persisted
	conns, err := s.LoadWSConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(conns.Connections) != 0 {
		t.Errorf("invalid configs were persisted: %d connections", len(conns.Connections))
	}

	// A valid aggregation spec must still create fine
	st, err := m.CreateConnection(CreateConnectionRequest{
		Mode: "push", URL: "ws://127.0.0.1:9/x", AggWindow: "1m", AggDefault: "avg",
	})
	if err != nil {
		t.Fatalf("valid aggregation config rejected: %v", err)
	}
	if st.ID == "" {
		t.Fatal("no status returned for valid create")
	}
}
