// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tviviano/ts-store/pkg/block"
)

// Regression test for issue #9: appending a small object after a
// block-spanning object must not corrupt the spanning object, and the
// appended record must be readable.
func TestV2AppendAfterSpanningObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-append-after-span"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 256
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Spanning object: 500 bytes across 256-byte blocks
	spanData := make([]byte, 500)
	for i := range spanData {
		spanData[i] = byte(i % 256)
	}
	spanHandle, err := s.PutObject(1000, spanData)
	if err != nil {
		t.Fatalf("PutObject spanning failed: %v", err)
	}
	if spanHandle.SpanCount < 2 {
		t.Fatalf("Expected spanning object, got spanCount=%d", spanHandle.SpanCount)
	}

	// Append two small objects after the span
	if _, err := s.PutObject(2000, []byte("small-record-1")); err != nil {
		t.Fatalf("PutObject after span failed: %v", err)
	}
	if _, err := s.PutObject(3000, []byte("small-record-2")); err != nil {
		t.Fatalf("Second PutObject after span failed: %v", err)
	}

	// The spanning object must be intact
	retrieved, err := s.GetObject(spanHandle)
	if err != nil {
		t.Fatalf("GetObject for spanning object failed: %v", err)
	}
	if !bytes.Equal(spanData, retrieved) {
		for i := range spanData {
			if spanData[i] != retrieved[i] {
				t.Fatalf("Spanning object corrupted at byte %d after append", i)
			}
		}
		t.Fatalf("Spanning object length mismatch: got %d, want %d", len(retrieved), len(spanData))
	}

	// All three records must be visible to a range scan
	handles, err := s.GetObjectsInRange(1, 4000, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange failed: %v", err)
	}
	if len(handles) != 3 {
		t.Fatalf("Expected 3 objects in range, got %d", len(handles))
	}
	wantTs := []int64{1000, 2000, 3000}
	for i, h := range handles {
		if h.Timestamp != wantTs[i] {
			t.Errorf("Handle %d: got timestamp %d, want %d", i, h.Timestamp, wantTs[i])
		}
	}

	// The newest record must be the last appended one
	newest, err := s.GetNewestObjects(1)
	if err != nil {
		t.Fatalf("GetNewestObjects failed: %v", err)
	}
	if len(newest) != 1 || newest[0].Timestamp != 3000 {
		t.Fatalf("Expected newest timestamp 3000, got %+v", newest)
	}
}

// Same as above, but with a close/reopen between the spanning write and the
// append, so the append path runs against recovery-derived partition state.
func TestV2AppendAfterSpanningObjectReopen(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-append-after-span-reopen"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 256
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	spanData := make([]byte, 500)
	for i := range spanData {
		spanData[i] = byte(i % 256)
	}
	spanHandle, err := s.PutObject(1000, spanData)
	if err != nil {
		t.Fatalf("PutObject spanning failed: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	s, err = Open(tmpDir, cfg.Name)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	if _, err := s.PutObject(2000, []byte("small-record")); err != nil {
		t.Fatalf("PutObject after reopen failed: %v", err)
	}

	retrieved, err := s.GetObject(spanHandle)
	if err != nil {
		t.Fatalf("GetObject for spanning object failed: %v", err)
	}
	if !bytes.Equal(spanData, retrieved) {
		t.Fatalf("Spanning object corrupted after reopen+append")
	}

	handles, err := s.GetObjectsInRange(1, 4000, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange failed: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("Expected 2 objects in range, got %d", len(handles))
	}
}

// Regression test for issue #21: an object too large for a whole partition
// must be rejected with ErrObjectTooLarge, and the store must remain
// writable afterwards.
func TestV2OversizeObjectRejected(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-oversize"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4 // 4 blocks per partition
	cfg.DataBlockSize = 512
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Maximum single-object payload for one partition:
	// first block holds usable-objectHeader, remaining blocks hold usable each.
	usable := cfg.DataBlockSize - block.BlockHeaderSize
	maxPayload := (usable - block.ObjectHeaderSize) + (cfg.NumBlocks-1)*usable

	// One byte over capacity must be rejected
	tooBig := make([]byte, maxPayload+1)
	if _, err := s.PutObject(1000, tooBig); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("Expected ErrObjectTooLarge, got %v", err)
	}

	// The store must not be wedged: small writes still succeed
	if _, err := s.PutObject(2000, []byte("still-works")); err != nil {
		t.Fatalf("PutObject after oversize rejection failed: %v", err)
	}

	// An object at exactly partition capacity must succeed
	exact := make([]byte, maxPayload)
	for i := range exact {
		exact[i] = byte(i % 256)
	}
	handle, err := s.PutObject(3000, exact)
	if err != nil {
		t.Fatalf("PutObject at exact capacity failed: %v", err)
	}
	if handle.SpanCount != cfg.NumBlocks {
		t.Errorf("Expected spanCount=%d for full-partition object, got %d", cfg.NumBlocks, handle.SpanCount)
	}
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject for full-partition object failed: %v", err)
	}
	if !bytes.Equal(exact, retrieved) {
		t.Fatalf("Full-partition object corrupted")
	}

	// And the store keeps working after the full-partition write
	if _, err := s.PutObject(4000, []byte("after-exact")); err != nil {
		t.Fatalf("PutObject after full-partition object failed: %v", err)
	}
}
