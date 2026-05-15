// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"testing"
	"time"
)

// newJSONStore creates a fresh V2 JSON store under a temp directory.
// Returned store is registered for cleanup. Helpful for the metrics
// tests that need real read/write paths.
func newJSONStore(t *testing.T, name string) *Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Name = name
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 4
	cfg.DataType = DataTypeJSON
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMetricsZeroAfterCreate(t *testing.T) {
	s := newJSONStore(t, "zero")
	m := s.Metrics()
	if m.Writes != 0 || m.Reads != 0 || m.BytesWritten != 0 || m.BytesRead != 0 {
		t.Errorf("expected zero counters at create, got %+v", m)
	}
	if m.Since.IsZero() {
		t.Errorf("Since should be initialized at create, got zero time")
	}
	if time.Since(m.Since) > time.Second {
		t.Errorf("Since should be ~now, got %v ago", time.Since(m.Since))
	}
}

func TestMetricsCountsWrites(t *testing.T) {
	s := newJSONStore(t, "writes")
	payload := []byte(`{"k":1}`)
	ts := time.Now().UnixNano()
	for i := 0; i < 3; i++ {
		if _, err := s.PutObject(ts+int64(i+1), payload); err != nil {
			t.Fatalf("PutObject #%d: %v", i, err)
		}
	}
	m := s.Metrics()
	if m.Writes != 3 {
		t.Errorf("Writes: got %d, want 3", m.Writes)
	}
	want := int64(len(payload) * 3)
	if m.BytesWritten != want {
		t.Errorf("BytesWritten: got %d, want %d", m.BytesWritten, want)
	}
}

func TestMetricsCountsReads(t *testing.T) {
	s := newJSONStore(t, "reads")
	payload := []byte(`{"k":1}`)
	ts := time.Now().UnixNano()
	h, err := s.PutObject(ts+1, payload)
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// GetObject — counts as a read, with bytes.
	got, err := s.GetObject(h)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("GetObject payload mismatch")
	}

	// GetObjectsInRange — counts as a read (range), no bytes (handles only).
	if _, err := s.GetObjectsInRange(0, time.Now().UnixNano(), 10); err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}

	m := s.Metrics()
	if m.Reads != 2 {
		t.Errorf("Reads: got %d, want 2 (1 GetObject + 1 range scan)", m.Reads)
	}
	if m.BytesRead != int64(len(payload)) {
		t.Errorf("BytesRead: got %d, want %d", m.BytesRead, len(payload))
	}
}

func TestMetricsResetZerosAndAdvancesSince(t *testing.T) {
	s := newJSONStore(t, "reset")
	ts := time.Now().UnixNano()
	if _, err := s.PutObject(ts+1, []byte("x")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	before := s.Metrics()
	if before.Writes == 0 {
		t.Fatalf("test setup: expected non-zero writes before reset")
	}

	// Sleep so the new Since timestamp is strictly later (sub-microsecond
	// resolution makes this safe in practice).
	time.Sleep(2 * time.Millisecond)
	s.ResetMetrics()

	after := s.Metrics()
	if after.Writes != 0 || after.BytesWritten != 0 || after.Reads != 0 || after.BytesRead != 0 {
		t.Errorf("counters not zero after reset: %+v", after)
	}
	if !after.Since.After(before.Since) {
		t.Errorf("Since should advance on reset: before=%v after=%v", before.Since, after.Since)
	}
}
