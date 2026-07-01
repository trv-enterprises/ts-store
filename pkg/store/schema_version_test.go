// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tviviano/ts-store/pkg/schema"
)

// schemaV1 and schemaV2 mirror the evolution used across these tests.
var (
	schemaV1Fields = []schema.Field{
		{Index: 1, Name: "temperature", Type: "float32"},
		{Index: 2, Name: "humidity", Type: "float32"},
	}
	schemaV2Fields = []schema.Field{
		{Index: 1, Name: "temperature", Type: "float32"},
		{Index: 2, Name: "humidity", Type: "float32"},
		{Index: 3, Name: "pressure", Type: "float32"},
	}
)

// putFull validates, compacts and writes a full-JSON record, returning its handle.
func putFull(t *testing.T, s *Store, ts int64, full string) *ObjectHandle {
	t.Helper()
	compact, err := s.ValidateAndCompact([]byte(full))
	if err != nil {
		t.Fatalf("ValidateAndCompact(%s) failed: %v", full, err)
	}
	h, err := s.PutObject(ts, compact)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	return h
}

// newSchemaStore creates a schema store and sets schema v1. The v2 parameter is
// retained for table-driven callers; V2 partitioned storage is the only mode.
func newSchemaStore(t *testing.T, v2 bool) *Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3
	cfg.Name = "schema-ver-test"
	cfg.Path = t.TempDir()
	cfg.DataType = DataTypeSchema

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.SetSchema(&schema.Schema{Fields: schemaV1Fields}); err != nil {
		t.Fatalf("SetSchema v1 failed: %v", err)
	}
	return s
}

func TestPutObject_StampsSchemaVersion(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		name := "V1"
		if v2 {
			name = "V2"
		}
		t.Run(name, func(t *testing.T) {
			s := newSchemaStore(t, v2)

			h := putFull(t, s, 1000, `{"temperature":72.5,"humidity":45}`)
			if h.SchemaVersion != 1 {
				t.Errorf("write handle SchemaVersion = %d, want 1", h.SchemaVersion)
			}

			// Read it back; the handle from the read path must also carry version 1.
			_, rh, err := s.GetObjectByTime(1000)
			if err != nil {
				t.Fatalf("GetObjectByTime failed: %v", err)
			}
			if rh.SchemaVersion != 1 {
				t.Errorf("read handle SchemaVersion = %d, want 1", rh.SchemaVersion)
			}
		})
	}
}

func TestPutObject_NonSchemaStoreReservedZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Name = "json-store"
	cfg.Path = t.TempDir()
	cfg.DataType = DataTypeJSON

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer s.Close()

	h, err := s.PutObject(1000, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	if h.SchemaVersion != 0 {
		t.Errorf("non-schema store SchemaVersion = %d, want 0", h.SchemaVersion)
	}
}

func TestEvolveThenStampPerRecord(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		name := "V1"
		if v2 {
			name = "V2"
		}
		t.Run(name, func(t *testing.T) {
			s := newSchemaStore(t, v2)

			// Record A under v1.
			putFull(t, s, 1000, `{"temperature":72.5,"humidity":45}`)

			// Evolve to v2, then write record B.
			if _, err := s.SetSchema(&schema.Schema{Fields: schemaV2Fields}); err != nil {
				t.Fatalf("SetSchema v2 failed: %v", err)
			}
			putFull(t, s, 2000, `{"temperature":70,"humidity":50,"pressure":1013}`)

			_, ha, err := s.GetObjectByTime(1000)
			if err != nil {
				t.Fatalf("GetObjectByTime(A) failed: %v", err)
			}
			if ha.SchemaVersion != 1 {
				t.Errorf("record A version = %d, want 1", ha.SchemaVersion)
			}

			_, hb, err := s.GetObjectByTime(2000)
			if err != nil {
				t.Fatalf("GetObjectByTime(B) failed: %v", err)
			}
			if hb.SchemaVersion != 2 {
				t.Errorf("record B version = %d, want 2", hb.SchemaVersion)
			}
		})
	}
}

func TestExpandDataWide_NullFillEndToEnd(t *testing.T) {
	s := newSchemaStore(t, false)

	hA := putFull(t, s, 1000, `{"temperature":72.5,"humidity":45}`)
	if _, err := s.SetSchema(&schema.Schema{Fields: schemaV2Fields}); err != nil {
		t.Fatalf("SetSchema v2 failed: %v", err)
	}

	dataA, _, err := s.GetObjectByTime(1000)
	if err != nil {
		t.Fatalf("GetObjectByTime failed: %v", err)
	}

	wide, err := s.ExpandDataWide(dataA, int(hA.SchemaVersion))
	if err != nil {
		t.Fatalf("ExpandDataWide failed: %v", err)
	}

	var m map[string]interface{}
	json.Unmarshal(wide, &m)
	if _, ok := m["pressure"]; !ok {
		t.Error("wide expansion missing pressure key")
	}
	if m["pressure"] != nil {
		t.Errorf("pressure = %v, want null", m["pressure"])
	}
	if m["temperature"] != 72.5 {
		t.Errorf("temperature = %v, want 72.5", m["temperature"])
	}
}

func TestSpanningObjectVersionStamp(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		name := "V1"
		if v2 {
			name = "V2"
		}
		t.Run(name, func(t *testing.T) {
			s := newSchemaStore(t, v2)
			// Evolve to v2, adding a string field large enough to span blocks.
			// The spanning record is then stamped version 2.
			v2WithNote := append(append([]schema.Field{}, schemaV1Fields...),
				schema.Field{Index: 3, Name: "note", Type: "string"})
			if _, err := s.SetSchema(&schema.Schema{Fields: v2WithNote}); err != nil {
				t.Fatalf("SetSchema v2 failed: %v", err)
			}

			// Build a string field big enough to span multiple 4KB blocks.
			big := strings.Repeat("x", 9000)
			h := putFull(t, s, 1000, `{"temperature":1,"humidity":2,"note":"`+big+`"}`)
			if h.SpanCount < 2 {
				t.Fatalf("expected spanning object (SpanCount>=2), got %d", h.SpanCount)
			}
			if h.SchemaVersion != 2 {
				t.Errorf("spanning record version = %d, want 2", h.SchemaVersion)
			}

			data, rh, err := s.GetObjectByTime(1000)
			if err != nil {
				t.Fatalf("GetObjectByTime failed: %v", err)
			}
			if rh.SchemaVersion != 2 {
				t.Errorf("read spanning record version = %d, want 2", rh.SchemaVersion)
			}
			// Sanity: data round-trips.
			full, err := s.ExpandData(data, int(rh.SchemaVersion))
			if err != nil {
				t.Fatalf("ExpandData failed: %v", err)
			}
			var m map[string]interface{}
			json.Unmarshal(full, &m)
			if m["note"] != big {
				t.Error("spanning record note field did not round-trip")
			}
		})
	}
}
