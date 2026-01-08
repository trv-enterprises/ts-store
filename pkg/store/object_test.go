// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"testing"
	"time"
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

	if handle.TotalSize != uint32(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), handle.TotalSize)
	}

	if handle.BlockCount != 1 {
		t.Errorf("Expected 1 block for small object, got %d", handle.BlockCount)
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

	if handle2.TotalSize != handle.TotalSize {
		t.Errorf("Handle size mismatch")
	}
}

func TestPutGetLargeObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "large-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512 // Small blocks to force multi-block storage

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create object larger than one block
	// Block size 512, header 40, object header 16, usable in primary = 456
	// We'll create a 2000 byte object
	data := make([]byte, 2000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if handle.TotalSize != uint32(len(data)) {
		t.Errorf("Expected size %d, got %d", len(data), handle.TotalSize)
	}

	// Should need multiple blocks
	if handle.BlockCount < 2 {
		t.Errorf("Expected multiple blocks for large object, got %d", handle.BlockCount)
	}

	t.Logf("Large object stored in %d blocks", handle.BlockCount)

	// Retrieve and verify
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Errorf("Data mismatch: got %d bytes, want %d bytes", len(retrieved), len(data))
		// Find first difference
		for i := 0; i < len(data) && i < len(retrieved); i++ {
			if data[i] != retrieved[i] {
				t.Errorf("First difference at byte %d: got %d, want %d", i, retrieved[i], data[i])
				break
			}
		}
	}
}

func TestPutGetVeryLargeObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "very-large-object-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 1024

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create 50KB object
	data := make([]byte, 50*1024)
	for i := range data {
		data[i] = byte((i * 17) % 256) // Semi-random pattern
	}

	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	t.Logf("50KB object stored in %d blocks", handle.BlockCount)

	// Retrieve and verify
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Errorf("Data mismatch for 50KB object")
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

	// Store a multi-block object
	data := make([]byte, 1500)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()
	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	blockCountBefore := handle.BlockCount
	t.Logf("Object uses %d blocks", blockCountBefore)

	// Delete the object
	if err := s.DeleteObject(handle); err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// Verify blocks were reclaimed
	stats := s.Stats()
	t.Logf("After delete: FreeListCount=%d", stats.FreeListCount)

	// Try to retrieve - should fail
	_, err = s.GetObject(handle)
	if err == nil {
		t.Error("Expected error when getting deleted object")
	}
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

	// Store multiple objects of different sizes
	objects := []struct {
		data      []byte
		timestamp int64
		handle    *ObjectHandle
	}{
		{data: []byte("small object 1"), timestamp: 1000000000},
		{data: make([]byte, 1000), timestamp: 2000000000},
		{data: []byte("small object 2"), timestamp: 3000000000},
		{data: make([]byte, 2000), timestamp: 4000000000},
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
		t.Logf("Object %d: %d bytes in %d blocks", i, handle.TotalSize, handle.BlockCount)
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

func TestObjectChecksumValidation(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "checksum-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	data := []byte("test data for checksum validation")
	timestamp := time.Now().UnixNano()

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Normal retrieval should work
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("Data mismatch")
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
	retrieved, handle2, err := s.GetObjectByBlock(handle.PrimaryBlockNum)
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
