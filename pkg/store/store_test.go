// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
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

	// Verify the global metadata file and the first partition exist.
	storePath := filepath.Join(tmpDir, "test-store")
	if _, err := os.Stat(filepath.Join(storePath, "meta.tsdb")); os.IsNotExist(err) {
		t.Error("Meta file not created")
	}
	if _, err := os.Stat(filepath.Join(storePath, "partition-0")); os.IsNotExist(err) {
		t.Error("First partition not created")
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
		ts := time.Now().UnixNano() + int64(i)
		data := []byte("store " + string(rune('a'+i)))
		if _, err := s.PutObject(ts, data); err != nil {
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
	if stats.OldestTimestamp != 0 || stats.NewestTimestamp != 0 {
		t.Errorf("Expected no timestamps after reset")
	}

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
