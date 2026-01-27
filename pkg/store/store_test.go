// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAndOpen(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir

	// Create store
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	// Verify files exist
	storePath := filepath.Join(tmpDir, "test-store")
	if _, err := os.Stat(filepath.Join(storePath, "data.tsdb")); os.IsNotExist(err) {
		t.Error("Data file not created")
	}
	if _, err := os.Stat(filepath.Join(storePath, "index.tsdb")); os.IsNotExist(err) {
		t.Error("Index file not created")
	}
	if _, err := os.Stat(filepath.Join(storePath, "meta.tsdb")); os.IsNotExist(err) {
		t.Error("Meta file not created")
	}

	// Close store
	if err := s.Close(); err != nil {
		t.Fatalf("Failed to close store: %v", err)
	}

	// Re-open store
	s2, err := Open(tmpDir, "test-store")
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	defer s2.Close()

	// Verify config matches
	cfg2 := s2.Config()
	if cfg2.NumBlocks != cfg.NumBlocks {
		t.Errorf("NumBlocks mismatch: got %d, want %d", cfg2.NumBlocks, cfg.NumBlocks)
	}
	if cfg2.DataBlockSize != cfg.DataBlockSize {
		t.Errorf("DataBlockSize mismatch: got %d, want %d", cfg2.DataBlockSize, cfg.DataBlockSize)
	}
}

func TestCreateDuplicate(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	s.Close()

	// Try to create again
	_, err = Create(cfg)
	if err != ErrStoreExists {
		t.Errorf("Expected ErrStoreExists, got %v", err)
	}
}

func TestInsertAndFind(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert some data
	baseTime := time.Now().UnixNano()
	data := []byte("test data")

	blockNum, err := s.Insert(baseTime, data)
	if err != nil {
		t.Fatalf("Failed to insert: %v", err)
	}
	if blockNum != 0 {
		t.Errorf("Expected first block to be 0, got %d", blockNum)
	}

	// Find by time
	found, err := s.FindBlockByTimeExact(baseTime)
	if err != nil {
		t.Fatalf("Failed to find by time: %v", err)
	}
	if found != blockNum {
		t.Errorf("Found wrong block: got %d, want %d", found, blockNum)
	}

	// Read data back
	readData, err := s.ReadBlockData(blockNum)
	if err != nil {
		t.Fatalf("Failed to read data: %v", err)
	}
	if string(readData) != string(data) {
		t.Errorf("Data mismatch: got %s, want %s", readData, data)
	}
}

func TestCircularWrap(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir
	cfg.NumBlocks = 10 // Small circle for testing

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	baseTime := time.Now().UnixNano()

	// Insert more than NumBlocks entries
	for i := 0; i < 15; i++ {
		ts := baseTime + int64(i*1000000) // 1ms apart
		_, err := s.Insert(ts, []byte("data"))
		if err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
	}

	// Verify oldest entries were reclaimed
	stats := s.Stats()
	t.Logf("Stats: Head=%d, Tail=%d", stats.HeadBlock, stats.TailBlock)

	// The oldest 5 entries should have been reclaimed
	// Try to find the first entry - it should not exist
	_, err = s.FindBlockByTimeExact(baseTime)
	if err != ErrTimestampNotFound {
		t.Errorf("Expected oldest entry to be reclaimed, got err=%v", err)
	}

	// Newest entry should exist
	newestTime := baseTime + int64(14*1000000)
	_, err = s.FindBlockByTimeExact(newestTime)
	if err != nil {
		t.Errorf("Expected newest entry to exist: %v", err)
	}
}

func TestRangeQuery(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	baseTime := time.Now().UnixNano()

	// Insert 20 entries
	for i := 0; i < 20; i++ {
		ts := baseTime + int64(i*1000000000) // 1 second apart
		_, err := s.Insert(ts, []byte("data"))
		if err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
	}

	// Query range: entries 5-15
	startTime := baseTime + int64(5*1000000000)
	endTime := baseTime + int64(15*1000000000)

	blocks, err := s.FindBlocksInRange(startTime, endTime)
	if err != nil {
		t.Fatalf("Range query failed: %v", err)
	}

	expectedCount := 11 // entries 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15
	if len(blocks) != expectedCount {
		t.Errorf("Expected %d blocks in range, got %d", expectedCount, len(blocks))
	}
}

func TestDelete(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	storePath := filepath.Join(tmpDir, "test-store")

	// Delete store
	if err := s.Delete(); err != nil {
		t.Fatalf("Failed to delete store: %v", err)
	}

	// Verify directory is gone
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Error("Store directory still exists after delete")
	}
}

func TestBlockSizeValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// Test invalid block size (not power of 2)
	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = tmpDir
	cfg.DataBlockSize = 1000 // Not power of 2

	_, err := Create(cfg)
	if err == nil {
		t.Error("Expected error for invalid block size")
	}

	// Test valid power of 2
	cfg.DataBlockSize = 1024
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed with valid block size: %v", err)
	}
	s.Close()
}

func TestMultipleStores(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple stores
	stores := make([]*Store, 3)
	for i := 0; i < 3; i++ {
		cfg := DefaultConfig()
		cfg.Name = "store-" + string(rune('a'+i))
		cfg.Path = tmpDir

		s, err := Create(cfg)
		if err != nil {
			t.Fatalf("Failed to create store %d: %v", i, err)
		}
		stores[i] = s
	}

	// Insert into each store
	for i, s := range stores {
		ts := time.Now().UnixNano()
		data := []byte("store " + string(rune('a'+i)))
		if _, err := s.Insert(ts, data); err != nil {
			t.Fatalf("Failed to insert into store %d: %v", i, err)
		}
	}

	// Verify isolation
	for i, s := range stores {
		oldest, err := s.GetOldestTimestamp()
		if err != nil {
			t.Fatalf("Failed to get oldest from store %d: %v", i, err)
		}
		if oldest == 0 {
			t.Errorf("Store %d has no data", i)
		}
	}

	// Close all
	for _, s := range stores {
		s.Close()
	}
}

func TestReset(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "reset-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert some data
	for i := 0; i < 10; i++ {
		ts := int64((i + 1) * 1000000000)
		_, err := s.PutObject(ts, []byte("test data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	// Verify data exists
	stats := s.Stats()
	if stats.ActiveBlocks == 0 {
		t.Fatal("Expected data before reset")
	}
	t.Logf("Before reset: ActiveBlocks=%d, OldestTime=%s", stats.ActiveBlocks, stats.OldestTime)

	// Reset the store
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Verify store is empty
	stats = s.Stats()
	if stats.HeadBlock != 0 || stats.TailBlock != 0 {
		t.Errorf("Expected head/tail to be 0 after reset, got head=%d tail=%d",
			stats.HeadBlock, stats.TailBlock)
	}
	if stats.OldestTimestamp != 0 || stats.NewestTimestamp != 0 {
		t.Errorf("Expected no timestamps after reset")
	}
	t.Logf("After reset: HeadBlock=%d, TailBlock=%d", stats.HeadBlock, stats.TailBlock)

	// Verify we can insert new data starting from any timestamp
	_, err = s.PutObject(500, []byte("new data after reset"))
	if err != nil {
		t.Fatalf("Insert after reset failed: %v", err)
	}

	// Verify data is there
	_, _, err = s.GetObjectByTime(500)
	if err != nil {
		t.Fatalf("GetObjectByTime after reset failed: %v", err)
	}
}
