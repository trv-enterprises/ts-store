// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package ws

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tviviano/ts-store/pkg/schema"
	"github.com/tviviano/ts-store/pkg/store"
)

// newSchemaStore creates a schema-type store with two numeric fields.
func newSchemaStore(t *testing.T, name string) *store.Store {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.Name = name
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 8
	cfg.DataType = store.DataTypeSchema
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	if _, err := s.SetSchema(&schema.Schema{Fields: []schema.Field{
		{Index: 1, Name: "temperature", Type: "float32"},
		{Index: 2, Name: "humidity", Type: "float32"},
	}}); err != nil {
		t.Fatalf("SetSchema: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// envelope builds the wire message a remote server sends to a pull connection.
func envelope(t *testing.T, ts int64, data []byte) []byte {
	t.Helper()
	msg, err := json.Marshal(PullMessage{Timestamp: ts, Data: data})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return msg
}

// storedBytes reads back the newest record's raw stored bytes.
func storedBytes(t *testing.T, s *store.Store) []byte {
	t.Helper()
	handles, err := s.GetNewestObjects(1)
	if err != nil || len(handles) != 1 {
		t.Fatalf("GetNewestObjects: handles=%d err=%v", len(handles), err)
	}
	data, err := s.GetObject(handles[0])
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	return data
}

// Regression test for issue #22: full-format (and default-format) pull data
// on a schema store must be validated and compacted before storage, exactly
// like the HTTP and WS-write input paths.
func TestPullerCompactsFullFormatOnSchemaStore(t *testing.T) {
	fullJSON := []byte(`{"temperature": 72.5, "humidity": 45}`)

	for _, format := range []string{"", "full"} {
		name := format
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			s := newSchemaStore(t, "puller-"+name)
			want, err := s.ValidateAndCompact(fullJSON)
			if err != nil {
				t.Fatalf("ValidateAndCompact: %v", err)
			}

			p := NewPuller(s, "puller-"+name, store.WSConnection{ID: "c1", Format: format})
			if err := p.processMessage(envelope(t, 1000, fullJSON)); err != nil {
				t.Fatalf("processMessage: %v", err)
			}

			got := storedBytes(t, s)
			if !bytes.Equal(got, want) {
				t.Fatalf("stored bytes are not compacted schema form:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

// Compact-format pull data is already in stored form and must pass through
// unchanged.
func TestPullerPassesThroughCompactFormat(t *testing.T) {
	s := newSchemaStore(t, "puller-compact")

	compact, err := s.ValidateAndCompact([]byte(`{"temperature": 72.5, "humidity": 45}`))
	if err != nil {
		t.Fatalf("ValidateAndCompact: %v", err)
	}

	p := NewPuller(s, "puller-compact", store.WSConnection{ID: "c1", Format: "compact"})
	if err := p.processMessage(envelope(t, 1000, compact)); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if got := storedBytes(t, s); !bytes.Equal(got, compact) {
		t.Fatalf("compact payload was modified:\ngot  %s\nwant %s", got, compact)
	}
}

// A message without a data field must be rejected, not stored as an empty
// record.
func TestPullerRejectsMissingData(t *testing.T) {
	s := newSchemaStore(t, "puller-nodata")

	p := NewPuller(s, "puller-nodata", store.WSConnection{ID: "c1"})
	if err := p.processMessage([]byte(`{"timestamp": 1000}`)); err == nil {
		t.Fatal("expected error for message without data, got nil")
	}

	if handles, err := s.GetNewestObjects(1); err == nil && len(handles) > 0 {
		t.Fatalf("empty record was stored: %+v", handles)
	}
}
