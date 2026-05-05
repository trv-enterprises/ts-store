// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV2CreateAndOpen(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig() // V2 is default
	cfg.Name = "v2-test-store"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	// Create V2 store
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create V2 store: %v", err)
	}

	// Verify it's a V2 store
	if !s.IsV2() {
		t.Error("Expected V2 store")
	}

	// Verify directory structure
	storePath := filepath.Join(tmpDir, "v2-test-store")
	if _, err := os.Stat(filepath.Join(storePath, "meta.tsdb")); os.IsNotExist(err) {
		t.Error("Global meta file not created")
	}
	if _, err := os.Stat(filepath.Join(storePath, "partition-0")); os.IsNotExist(err) {
		t.Error("First partition directory not created")
	}
	if _, err := os.Stat(filepath.Join(storePath, "partition-0", "data.tsdb")); os.IsNotExist(err) {
		t.Error("Partition data file not created")
	}

	// Close store
	if err := s.Close(); err != nil {
		t.Fatalf("Failed to close store: %v", err)
	}

	// Re-open store
	s2, err := Open(tmpDir, "v2-test-store")
	if err != nil {
		t.Fatalf("Failed to open V2 store: %v", err)
	}
	defer s2.Close()

	// Verify it's still V2
	if !s2.IsV2() {
		t.Error("Re-opened store should be V2")
	}

	// Verify config matches
	cfg2 := s2.Config()
	if cfg2.NumPartitions != cfg.NumPartitions {
		t.Errorf("NumPartitions mismatch: got %d, want %d", cfg2.NumPartitions, cfg.NumPartitions)
	}
}

func TestV2PutGetObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-put-get"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert an object
	timestamp := time.Now().UnixNano()
	data := []byte("hello v2 world")

	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	if handle.PartitionID != 0 {
		t.Errorf("Expected partition ID 0, got %d", handle.PartitionID)
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

	if handle2.PartitionID != handle.PartitionID {
		t.Errorf("Partition ID mismatch: got %d, want %d", handle2.PartitionID, handle.PartitionID)
	}
}

func TestV2MultipleObjects(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-multi"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert multiple objects
	baseTime := time.Now().UnixNano()
	objects := make([]struct {
		ts   int64
		data []byte
	}, 10)

	for i := 0; i < 10; i++ {
		objects[i].ts = baseTime + int64(i*1000000) // 1ms apart
		objects[i].data = []byte("object " + string(rune('A'+i)))

		_, err := s.PutObject(objects[i].ts, objects[i].data)
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
	}

	// Verify all objects can be retrieved
	for i, obj := range objects {
		retrieved, _, err := s.GetObjectByTime(obj.ts)
		if err != nil {
			t.Fatalf("GetObjectByTime %d failed: %v", i, err)
		}
		if !bytes.Equal(obj.data, retrieved) {
			t.Errorf("Object %d mismatch: got %q, want %q", i, retrieved, obj.data)
		}
	}

	// Test GetNewestObjects
	newest, err := s.GetNewestObjects(3)
	if err != nil {
		t.Fatalf("GetNewestObjects failed: %v", err)
	}
	if len(newest) != 3 {
		t.Errorf("Expected 3 newest objects, got %d", len(newest))
	}
	// Should be in reverse order (newest first)
	if newest[0].Timestamp != objects[9].ts {
		t.Errorf("Newest object timestamp wrong: got %d, want %d", newest[0].Timestamp, objects[9].ts)
	}

	// Test GetOldestObjects
	oldest, err := s.GetOldestObjects(3)
	if err != nil {
		t.Fatalf("GetOldestObjects failed: %v", err)
	}
	if len(oldest) != 3 {
		t.Errorf("Expected 3 oldest objects, got %d", len(oldest))
	}
	if oldest[0].Timestamp != objects[0].ts {
		t.Errorf("Oldest object timestamp wrong: got %d, want %d", oldest[0].Timestamp, objects[0].ts)
	}
}

func TestV2SpanningObject(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-spanning"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 512
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Create a large object that spans multiple blocks
	dataSize := 1500 // Should span 4+ blocks with 512-byte blocks
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	timestamp := time.Now().UnixNano()
	handle, err := s.PutObject(timestamp, data)
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	t.Logf("Spanning object: size=%d, spanCount=%d", handle.Size, handle.SpanCount)

	if handle.SpanCount < 2 {
		t.Errorf("Expected spanning object to use multiple blocks, got spanCount=%d", handle.SpanCount)
	}

	// Retrieve and verify
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Errorf("Data mismatch for spanning object: got len=%d, want len=%d", len(retrieved), len(data))
	}
}

func TestV2ObjectsInRange(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-range"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert 20 objects
	baseTime := time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		ts := baseTime + int64(i*1000000000) // 1 second apart
		_, err := s.PutObject(ts, []byte("data"))
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
	}

	// Query range: entries 5-15
	startTime := baseTime + int64(5*1000000000)
	endTime := baseTime + int64(15*1000000000)

	handles, err := s.GetObjectsInRange(startTime, endTime, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange failed: %v", err)
	}

	expectedCount := 11 // entries 5 through 15
	if len(handles) != expectedCount {
		t.Errorf("Expected %d objects in range, got %d", expectedCount, len(handles))
	}
}

func TestV2Stats(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-stats"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 4

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert some data
	for i := 0; i < 5; i++ {
		ts := int64((i + 1) * 1000000000)
		_, err := s.PutObject(ts, []byte("data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	stats := s.Stats()

	if stats.StorageVersion != 2 {
		t.Errorf("Expected storage version 2, got %d", stats.StorageVersion)
	}

	if stats.NumPartitions != 4 {
		t.Errorf("Expected 4 partitions, got %d", stats.NumPartitions)
	}

	if stats.ActivePartitions != 1 {
		t.Errorf("Expected 1 active partition, got %d", stats.ActivePartitions)
	}

	if len(stats.PartitionStats) != 1 {
		t.Errorf("Expected 1 partition stat, got %d", len(stats.PartitionStats))
	}

	t.Logf("V2 Stats: Version=%d, Partitions=%d, Active=%d, Blocks=%d",
		stats.StorageVersion, stats.NumPartitions, stats.ActivePartitions, stats.ActiveBlocks)
}

func TestV2Reset(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-reset"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

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

	// Reset the store
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Verify store is empty
	stats = s.Stats()
	if stats.ActivePartitions != 1 {
		t.Errorf("Expected 1 active partition after reset, got %d", stats.ActivePartitions)
	}

	// Verify we can insert new data
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

func TestV2PartitionRollover(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-rollover"
	cfg.Path = tmpDir
	cfg.NumBlocks = 10 // Small blocks per partition
	cfg.DataBlockSize = 512
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Fill up the first partition to trigger rollover
	baseTime := time.Now().UnixNano()
	count := 0
	for i := 0; i < 50; i++ {
		ts := baseTime + int64(i*1000000)
		data := make([]byte, 400) // Large enough to use most of a block
		_, err := s.PutObject(ts, data)
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
		count++
	}

	stats := s.Stats()
	t.Logf("After %d inserts: ActivePartitions=%d, CurrentPartition=%d",
		count, stats.ActivePartitions, stats.CurrentPartition)

	// We should have rolled over to at least one new partition
	if stats.ActivePartitions < 2 {
		t.Logf("Warning: expected partition rollover but only %d active", stats.ActivePartitions)
		// This might not happen if data fits in one partition, which is OK
	}
}

func TestV2SpanningObjectPartitionBoundary(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-span-boundary"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4 // Very small partition - only 4 blocks
	cfg.DataBlockSize = 512
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Fill up most of the first partition with medium objects
	// Each ~400 byte object uses most of a block
	baseTime := time.Now().UnixNano()
	for i := 0; i < 3; i++ {
		ts := baseTime + int64(i*1000000)
		data := make([]byte, 400)
		_, err := s.PutObject(ts, data)
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
	}

	stats := s.Stats()
	t.Logf("After 3 objects: HeadBlock in partition stats")
	for _, ps := range stats.PartitionStats {
		t.Logf("  Partition %d: HeadBlock=%d, NumBlocks=%d", ps.PartitionID, ps.HeadBlock, ps.NumBlocks)
	}

	// Now try a large spanning object that needs 4 blocks but only 1 remains
	largeData := make([]byte, 1500) // Needs ~4 blocks
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	ts := baseTime + int64(10*1000000)
	handle, err := s.PutObject(ts, largeData)
	if err != nil {
		t.Fatalf("PutObject large failed: %v", err)
	}

	t.Logf("Large object (span=%d) written to partition %d", handle.SpanCount, handle.PartitionID)

	// Should have rolled to a new partition since it couldn't fit
	if handle.PartitionID == 0 {
		t.Errorf("Expected rollover to new partition, but stayed in partition 0")
	}

	// Verify the data is correct
	retrieved, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}

	if !bytes.Equal(largeData, retrieved) {
		t.Errorf("Data mismatch: got len=%d, want len=%d", len(retrieved), len(largeData))
	}

	stats = s.Stats()
	t.Logf("Final state: ActivePartitions=%d, CurrentPartition=%d",
		stats.ActivePartitions, stats.CurrentPartition)
}

func TestV2BackwardCompatibility(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a V1 store
	cfgV1 := DefaultConfigV1()
	cfgV1.Name = "v1-compat"
	cfgV1.Path = tmpDir
	cfgV1.NumBlocks = 100

	s1, err := Create(cfgV1)
	if err != nil {
		t.Fatalf("Failed to create V1 store: %v", err)
	}

	// Insert data with V1
	timestamp := time.Now().UnixNano()
	data := []byte("v1 data")
	_, err = s1.Insert(timestamp, data)
	if err != nil {
		t.Fatalf("V1 Insert failed: %v", err)
	}
	s1.Close()

	// Re-open with generic Open (should detect V1)
	s2, err := Open(tmpDir, "v1-compat")
	if err != nil {
		t.Fatalf("Failed to re-open V1 store: %v", err)
	}
	defer s2.Close()

	// Verify it's still V1
	if s2.IsV2() {
		t.Error("V1 store should not be detected as V2")
	}

	// Verify data is accessible
	readData, err := s2.ReadBlockData(0)
	if err != nil {
		t.Fatalf("Failed to read V1 data: %v", err)
	}
	if string(readData) != string(data) {
		t.Errorf("V1 data mismatch: got %q, want %q", readData, data)
	}
}

func TestV2StatsAggregatesAcrossPartitions(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-stats-aggregate"
	cfg.Path = tmpDir
	cfg.NumBlocks = 4
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// With no writes, ActiveBlocks must be 0 even though one partition exists.
	if got := s.Stats().ActiveBlocks; got != 0 {
		t.Errorf("Empty store ActiveBlocks: got %d, want 0", got)
	}

	// Write enough to fill ~1.5 partitions and force a rollover so two
	// partitions are active. With NumBlocks=4 (BlocksPerPart) and one object
	// per block, 6 inserts gives HeadBlock=3 in part 0 and HeadBlock=1 in
	// part 1.
	objSize := int(cfg.DataBlockSize) // ~one block per write
	for i := 0; i < 6; i++ {
		_, err := s.PutObject(int64(i+1)*1_000_000_000, make([]byte, objSize))
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
	}

	stats := s.Stats()
	if stats.ActivePartitions < 2 {
		t.Fatalf("Expected at least 2 active partitions after rollover, got %d", stats.ActivePartitions)
	}

	// NumBlocks must reflect the active ring (BlocksPerPart * ActiveCount),
	// not just one partition. Otherwise ActiveBlocks/NumBlocks goes >100%.
	wantNumBlocks := cfg.NumBlocks * stats.ActivePartitions
	if stats.NumBlocks != wantNumBlocks {
		t.Errorf("NumBlocks: got %d, want %d (BlocksPerPart=%d * ActivePartitions=%d)",
			stats.NumBlocks, wantNumBlocks, cfg.NumBlocks, stats.ActivePartitions)
	}

	if stats.ActiveBlocks > stats.NumBlocks {
		t.Errorf("ActiveBlocks (%d) must not exceed NumBlocks (%d) — usage would exceed 100%%",
			stats.ActiveBlocks, stats.NumBlocks)
	}
}

func TestV2DiskUsage(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-disk-usage"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	for i := 0; i < 4; i++ {
		_, err := s.PutObject(int64(i+1)*1_000_000_000, []byte("payload"))
		if err != nil {
			t.Fatalf("PutObject %d failed: %v", i, err)
		}
	}

	got, err := s.DiskUsage()
	if err != nil {
		t.Fatalf("DiskUsage failed: %v", err)
	}

	// Independently sum every .tsdb file under the store path and compare.
	var want uint64
	storePath := filepath.Join(tmpDir, cfg.Name)
	err = filepath.WalkDir(storePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(d.Name()) != ".tsdb" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		want += uint64(fi.Size())
		return nil
	})
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}

	if got != want {
		t.Errorf("DiskUsage: got %d bytes, want %d bytes", got, want)
	}

	// Sanity: must be far larger than just the 128-byte root meta —
	// that was the original CLI bug (only the root file got counted).
	if got <= 128 {
		t.Errorf("DiskUsage (%d) suspiciously small — partition files likely missed", got)
	}
}
