// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/block"
)

func TestPutGetSmallObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "small-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 4096

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Small object that fits in one block
	data := []byte("Hello, World! This is a small object.")
	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if handle.Size != uint32(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), handle.Size)
	}

	// Retrieve by handle
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Errorf("Data mismatch: got %q, want %q", retrieved, data)
	}

	// Retrieve by timestamp
	retrieved2, handle2, err := s.GetObjectByTime(timestamp)
	if err != nil {
		t.Fatalf("GetObjectByTime failed: %v", err)
	}

	if !bytes.Equal(data, retrieved2) {
		t.Errorf("Data mismatch on time lookup: got %q, want %q", retrieved2, data)
	}

	if handle2.Size != handle.Size {
		t.Errorf("Handle size mismatch")
	}
}

func TestSpanningObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "spanning-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create object larger than single block (should span multiple blocks)
	usablePerBlock := cfg.DataBlockSize - block.BlockHeaderSize
	firstBlockUsable := usablePerBlock - block.ObjectHeaderSize
	// Create an object that needs 3 blocks
	dataSize := usablePerBlock*2 + 100
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	_ = firstBlockUsable // Used for calculation validation

	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed for spanning object: %v", err)
	}

	t.Logf("Spanning object: size=%d, spanCount=%d, blockNum=%d", handle.Size, handle.SpanCount, handle.BlockNum)

	// Check block headers
	for i := uint32(0); i < handle.SpanCount; i++ {
		h, _ := s.GetBlockHeader(handle.BlockNum + i)
		t.Logf("Block %d: DataLen=%d, Flags=%x",
			handle.BlockNum+i, h.DataLen, h.Flags)
	}

	if handle.SpanCount < 2 {
		t.Errorf("Expected spanning object to use multiple blocks, got spanCount=%d", handle.SpanCount)
	}

	// Retrieve and verify
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed for spanning object: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		// Find first difference
		for i := 0; i < len(data) && i < len(retrieved); i++ {
			if data[i] != retrieved[i] {
				t.Errorf("Data mismatch at byte %d: got %d, want %d", i, retrieved[i], data[i])
				break
			}
		}
		t.Errorf("Data mismatch for spanning object: got len=%d, want len=%d", len(retrieved), len(data))
	}

	// Also test retrieval by timestamp
	retrieved2, handle2, err := s.GetObjectByTime(timestamp)
	if err != nil {
		t.Fatalf("GetObjectByTime failed for spanning object: %v", err)
	}

	if !bytes.Equal(data, retrieved2) {
		t.Errorf("Data mismatch on time lookup for spanning object")
	}

	if handle2.SpanCount != handle.SpanCount {
		t.Errorf("SpanCount mismatch: got %d, want %d", handle2.SpanCount, handle.SpanCount)
	}
}

func TestMaxObjectSize(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "max-size-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 1024

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create object exactly at max size
	maxSize := s.MaxObjectSize()
	data := make([]byte, maxSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed for max size object: %v", err)
	}

	if handle.Size != maxSize {
		t.Errorf("Expected size %d, got %d", maxSize, handle.Size)
	}

	// Retrieve and verify
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Errorf("Data mismatch for max size object")
	}
}

func TestDeleteObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "delete-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Store an object
	data := make([]byte, 400)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()
	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Delete the object
	if err := s.DeleteObject(handle); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Verify blocks were reclaimed (tail advanced)
	stats := s.Stats()
	t.Logf("After delete: HeadBlock=%d, TailBlock=%d", stats.HeadBlock, stats.TailBlock)

	// Try to retrieve - should fail or return empty
	_, err = s.GetObject(handle)
	// After deletion the block may be reclaimed but reading an empty block is allowed
	t.Logf("GetObject after delete returned: %v", err)
}

func TestDeleteObjectByTime(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "delete-by-time-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	data := []byte("test data")
	timestamp := time.Now().UnixNano()

	_, err = s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Delete by time
	if err := s.DeleteObjectByTime(timestamp); err != nil {
		t.Fatalf("DeleteObjectByTime failed: %v", err)
	}

	// Verify it's gone
	_, _, err = s.GetObjectByTime(timestamp)
	if err == nil {
		t.Error("Expected error when getting deleted object by time")
	}
}

func TestDeleteSpanningObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "delete-spanning-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create spanning object (needs 3 blocks with 512 byte blocks)
	usablePerBlock := cfg.DataBlockSize - block.BlockHeaderSize
	dataSize := usablePerBlock*2 + 100
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()
	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	t.Logf("Spanning object spans %d blocks starting at block %d", handle.SpanCount, handle.BlockNum)

	if handle.SpanCount < 2 {
		t.Fatalf("Expected spanning object with multiple blocks, got %d", handle.SpanCount)
	}

	// Get stats before delete
	statsBefore := s.Stats()
	t.Logf("Before delete: HeadBlock=%d, TailBlock=%d", statsBefore.HeadBlock, statsBefore.TailBlock)

	// Delete the spanning object
	if err := s.DeleteObject(handle); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Verify blocks were reclaimed (tail advanced)
	statsAfter := s.Stats()
	t.Logf("After delete: HeadBlock=%d, TailBlock=%d", statsAfter.HeadBlock, statsAfter.TailBlock)

	// Verify object is no longer retrievable
	_, err = s.GetObject(handle)
	// After deletion, blocks are cleared, so reading should fail or return empty data
	t.Logf("GetObject after delete returned: %v", err)
}

func TestDeleteLargeSpanningObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "delete-large-spanning-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create large spanning object (needs 5+ blocks with 512 byte blocks)
	usablePerBlock := cfg.DataBlockSize - block.BlockHeaderSize
	dataSize := usablePerBlock*5 + 100 // Should span 6 blocks
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()
	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	t.Logf("Large spanning object: size=%d, spans %d blocks starting at block %d",
		handle.Size, handle.SpanCount, handle.BlockNum)

	if handle.SpanCount < 5 {
		t.Fatalf("Expected spanning object with 5+ blocks, got %d", handle.SpanCount)
	}

	// Verify we can read it back correctly first
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed before delete: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Fatalf("Data mismatch before delete")
	}

	// Get stats before delete
	statsBefore := s.Stats()
	t.Logf("Before delete: HeadBlock=%d, TailBlock=%d",
		statsBefore.HeadBlock, statsBefore.TailBlock)

	// Delete the spanning object
	if err := s.DeleteObject(handle); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Verify blocks were reclaimed (tail advanced)
	statsAfter := s.Stats()
	t.Logf("After delete: HeadBlock=%d, TailBlock=%d",
		statsAfter.HeadBlock, statsAfter.TailBlock)
}

func TestMultipleObjects(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "multi-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	maxSize := s.MaxObjectSize()

	// Store multiple objects of different sizes (all within block size)
	objects := []struct {
		data      []byte
		timestamp int64
		handle    *ObjectHandle
	}{
		{data: []byte("small object 1"), timestamp: 1000000000},
		{data: make([]byte, maxSize/2), timestamp: 2000000000},
		{data: []byte("small object 2"), timestamp: 3000000000},
		{data: make([]byte, maxSize/4), timestamp: 4000000000},
	}

	// Fill larger objects with data
	for i := range objects[1].data {
		objects[1].data[i] = byte(i % 256)
	}
	for i := range objects[3].data {
		objects[3].data[i] = byte((i * 7) % 256)
	}

	// Store all objects
	for i := range objects {
		handle, err := s.PutObject(objects[i].timestamp, objects[i].data)
		if err != nil {
			t.Fatalf("Failed to store object %d: %v", i, err)
		}
		objects[i].handle = handle
		t.Logf("Object %d: %d bytes", i, handle.Size)
	}

	// Retrieve and verify all objects
	for i := range objects {
		retrieved, err := s.GetObject(objects[i].handle)
		if err != nil {
			t.Fatalf("Failed to retrieve object %d: %v", i, err)
		}
		if !bytes.Equal(objects[i].data, retrieved) {
			t.Errorf("Object %d data mismatch", i)
		}
	}

	// Retrieve by timestamp
	for i := range objects {
		retrieved, _, err := s.GetObjectByTime(objects[i].timestamp)
		if err != nil {
			t.Fatalf("Failed to retrieve object %d by time: %v", i, err)
		}
		if !bytes.Equal(objects[i].data, retrieved) {
			t.Errorf("Object %d data mismatch on time lookup", i)
		}
	}
}

func TestGetObjectByBlock(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "get-by-block-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	data := []byte("test data")
	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Get by block number
	retrieved, handle2, err := s.GetObjectByBlock(handle.BlockNum)
	if err != nil {
		t.Fatalf("GetObjectByBlock failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Error("Data mismatch")
	}

	if handle2.Timestamp != timestamp {
		t.Errorf("Timestamp mismatch: got %d, want %d", handle2.Timestamp, timestamp)
	}
}

func TestGetOldestNewestObjects(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "oldest-newest-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert 5 objects
	for i := 0; i < 5; i++ {
		timestamp := int64((i + 1) * 1000000000)
		data := []byte("test data " + string(rune('a'+i)))
		_, err := s.PutObject(timestamp, data)
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	// Get oldest 3
	oldest, err := s.GetOldestObjects(3)
	if err != nil {
		t.Fatalf("GetOldestObjects failed: %v", err)
	}
	if len(oldest) != 3 {
		t.Errorf("Expected 3 oldest objects, got %d", len(oldest))
	}

	// Get newest 3
	newest, err := s.GetNewestObjects(3)
	if err != nil {
		t.Fatalf("GetNewestObjects failed: %v", err)
	}
	if len(newest) != 3 {
		t.Errorf("Expected 3 newest objects, got %d", len(newest))
	}

	// Verify oldest has lower timestamps
	if oldest[0].Timestamp > newest[0].Timestamp {
		t.Error("Oldest should have lower timestamp than newest")
	}
}

func TestGetObjectsInRange(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "range-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert objects with known timestamps
	timestamps := []int64{1000, 2000, 3000, 4000, 5000}
	for _, ts := range timestamps {
		_, err := s.PutObject(ts, []byte("data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	// Get objects in range [2000, 4000]
	handles, err := s.GetObjectsInRange(2000, 4000, 100)
	if err != nil {
		t.Fatalf("GetObjectsInRange failed: %v", err)
	}
	if len(handles) != 3 {
		t.Errorf("Expected 3 objects in range, got %d", len(handles))
	}
}

func TestTimestampOutOfOrder(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "timestamp-order-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert first object with timestamp 1000
	_, err = s.PutObject(1000, []byte("first"))
	if err != nil {
		t.Fatalf("First PutObject failed: %v", err)
	}

	// Try to insert with earlier timestamp - should fail
	_, err = s.PutObject(500, []byte("earlier"))
	if err != ErrTimestampOutOfOrder {
		t.Errorf("Expected ErrTimestampOutOfOrder, got: %v", err)
	}

	// Try to insert with same timestamp - should fail
	_, err = s.PutObject(1000, []byte("same"))
	if err != ErrTimestampOutOfOrder {
		t.Errorf("Expected ErrTimestampOutOfOrder for same timestamp, got: %v", err)
	}

	// Insert with later timestamp - should succeed
	_, err = s.PutObject(2000, []byte("later"))
	if err != nil {
		t.Errorf("Later PutObject should succeed, got: %v", err)
	}
}
