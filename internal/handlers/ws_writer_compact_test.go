// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"testing"

	"github.com/tviviano/ts-store/pkg/schema"
	"github.com/tviviano/ts-store/pkg/store"
)

// newCompactTestStore creates a schema store with one float field at
// index 1 ("temperature").
func newCompactTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.Name = "ws-compact"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	cfg.DataType = store.DataTypeSchema
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.SetSchema(&schema.Schema{Fields: []schema.Field{
		{Index: 1, Name: "temperature", Type: schema.FieldTypeFloat32},
	}}); err != nil {
		t.Fatalf("SetSchema: %v", err)
	}
	return s
}

// TestWSWriterCompactValidatesPayload is the issue #41 scenario: the
// format=compact WS write path previously stored payload bytes as-is,
// letting a client insert data into a schema store that HTTP would
// reject (HTTP has no compact write path and always validates). Valid
// index-keyed payloads still store; garbage, full-form (named-key),
// and unknown-index payloads are rejected and nothing is written.
func TestWSWriterCompactValidatesPayload(t *testing.T) {
	s := newCompactTestStore(t)
	serverConn, clientConn := wsConnPair(t)
	_ = clientConn
	w := newWSWriter(serverConn, s, "compact")

	// Valid compact payload stores fine.
	if err := w.processMessage([]byte(`{"timestamp": 1000, "data": {"1": 25.5}}`)); err != nil {
		t.Fatalf("valid compact payload rejected: %v", err)
	}

	rejected := []struct {
		name    string
		message string
	}{
		{"non-object garbage", `{"timestamp": 2000, "data": "not-a-record"}`},
		{"full-form named keys", `{"timestamp": 3000, "data": {"temperature": 26.0}}`},
		{"unknown schema index", `{"timestamp": 4000, "data": {"99": 1}}`},
	}
	for _, tc := range rejected {
		if err := w.processMessage([]byte(tc.message)); err == nil {
			t.Errorf("%s: expected rejection, got nil error", tc.name)
		}
	}

	// Only the valid record landed.
	handles, err := s.GetOldestObjects(0)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(handles) != 1 || handles[0].Timestamp != 1000 {
		t.Fatalf("expected exactly the one valid record @1000, got %d handles: %+v", len(handles), handles)
	}
	// And it round-trips through expand (the read path that previously
	// hit the expand-failure fallback on garbage records).
	data, err := s.GetObject(handles[0])
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if _, err := s.ExpandData(data, 0); err != nil {
		t.Fatalf("stored compact record failed to expand: %v", err)
	}
}
