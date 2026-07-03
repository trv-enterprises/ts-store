// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"testing"

	"github.com/tviviano/ts-store/pkg/block"
)

// craftPartialSpan writes the on-disk state a crash mid-spanning-write leaves
// behind: the span's first block and some (but not all) continuation blocks
// are on disk, and partition metadata was never updated. totalDataLen is what
// the span's object header promises; contBlocks is how many continuation
// blocks were "written" before the crash.
func craftPartialSpan(t *testing.T, s *Store, startBlock, contBlocks, totalDataLen uint32) {
	t.Helper()
	p := s.currentPartition
	usable := p.blockSize - block.BlockHeaderSize
	firstChunk := usable - block.ObjectHeaderSize

	if err := p.writeBlockHeader(startBlock, &block.BlockHeader{
		Timestamp: 2000,
		DataLen:   block.ObjectHeaderSize + firstChunk,
		Flags:     block.FlagPrimary | block.FlagPacked,
	}); err != nil {
		t.Fatalf("write span head block header: %v", err)
	}
	if err := p.writeObjectHeader(startBlock, block.BlockHeaderSize, &block.ObjectHeader{
		Timestamp: 2000,
		DataLen:   totalDataLen,
		Flags:     block.ObjFlagLastInBlock | block.ObjFlagContinues,
	}); err != nil {
		t.Fatalf("write span object header: %v", err)
	}
	if err := p.writeIndexEntry(startBlock, &block.IndexEntry{Timestamp: 2000, BlockNum: startBlock}); err != nil {
		t.Fatalf("write span index entry: %v", err)
	}

	for i := uint32(1); i <= contBlocks; i++ {
		b := startBlock + i
		if err := p.writeBlockHeader(b, &block.BlockHeader{
			Timestamp: 0,
			DataLen:   usable,
			Flags:     block.FlagPrimary | block.FlagPacked | block.FlagContinuation,
		}); err != nil {
			t.Fatalf("write continuation header %d: %v", b, err)
		}
		if err := p.writeIndexEntry(b, &block.IndexEntry{Timestamp: 0, BlockNum: b}); err != nil {
			t.Fatalf("write continuation index entry %d: %v", b, err)
		}
	}
}

// Regression test for issue #24: a spanning write that crashed partway left
// a first-block header promising bytes that were never written. Reads of
// that object returned zero-padded garbage and the stale head state
// shadowed subsequent writes. Recovery must roll the partition back to
// before the span.
func TestRecoveryRepairsIncompleteSpan(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "mid-span-crash"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.DataBlockSize = 256
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One good record in block 0
	if _, err := s.PutObject(1000, []byte("good-record")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Crash mid-span: span head at block 1 promising ~4 blocks of data,
	// only one continuation block made it to disk. Metadata not updated.
	usable := cfg.DataBlockSize - block.BlockHeaderSize
	craftPartialSpan(t, s, 1, 1, 4*usable)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(tmpDir, cfg.Name)
	if err != nil {
		t.Fatalf("Open after simulated crash: %v", err)
	}
	defer s.Close()

	// The truncated span must be gone and writes must work
	if _, err := s.PutObject(3000, []byte("after-crash")); err != nil {
		t.Fatalf("PutObject after recovery: %v", err)
	}

	handles, err := s.GetObjectsInRange(1, 10000, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}
	want := []int64{1000, 3000}
	if len(handles) != len(want) {
		t.Fatalf("expected %d records after recovery, got %d (truncated span leaked into reads)", len(want), len(handles))
	}
	for i, h := range handles {
		if h.Timestamp != want[i] {
			t.Errorf("record %d: timestamp %d, want %d", i, h.Timestamp, want[i])
		}
	}
}

// Same crash, but the partial span is the only thing in the partition —
// recovery must land on a clean, writable, empty partition.
func TestRecoveryRepairsIncompleteSpanAtBlockZero(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "mid-span-crash-empty"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.DataBlockSize = 256
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	usable := cfg.DataBlockSize - block.BlockHeaderSize
	craftPartialSpan(t, s, 0, 2, 5*usable)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(tmpDir, cfg.Name)
	if err != nil {
		t.Fatalf("Open after simulated crash: %v", err)
	}
	defer s.Close()

	if _, err := s.PutObject(500, []byte("fresh-start")); err != nil {
		t.Fatalf("PutObject after recovery: %v", err)
	}

	handles, err := s.GetObjectsInRange(1, 10000, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}
	if len(handles) != 1 || handles[0].Timestamp != 500 {
		t.Fatalf("expected exactly the fresh record, got %+v", handles)
	}
}

// A complete span must survive recovery untouched — the repair must not
// mistake a finished spanning write for a truncated one.
func TestRecoveryKeepsCompleteSpan(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "complete-span-survives"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.DataBlockSize = 256
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	spanData := make([]byte, 500)
	for i := range spanData {
		spanData[i] = byte(i % 256)
	}
	handle, err := s.PutObject(1000, spanData)
	if err != nil {
		t.Fatalf("PutObject spanning: %v", err)
	}
	if handle.SpanCount < 2 {
		t.Fatalf("expected a spanning object, got spanCount=%d", handle.SpanCount)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = Open(tmpDir, cfg.Name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	got, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject after reopen: %v", err)
	}
	if len(got) != len(spanData) {
		t.Fatalf("span data length changed after recovery: got %d, want %d", len(got), len(spanData))
	}
	for i := range spanData {
		if got[i] != spanData[i] {
			t.Fatalf("span data corrupted at byte %d after recovery", i)
		}
	}
}
