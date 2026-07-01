// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/block"
	"github.com/tviviano/ts-store/pkg/schema"
)

// --- Metadata encode/decode round-trip tests ---

func TestGlobalMetadataEncodeDecode(t *testing.T) {
	original := &GlobalMetadata{
		Magic:            magicNumberV2,
		Version:          versionV2,
		NumPartitions:    6,
		BlocksPerPart:    1024,
		BlockSize:        4096,
		DataType:         DataTypeJSON,
		CurrentPartition: 2,
		OldestPartition:  0,
		ActiveCount:      3,
	}
	original.PartitionOrder[0] = 0
	original.PartitionOrder[1] = 1
	original.PartitionOrder[2] = 2

	buf := original.Encode()
	if len(buf) != globalMetadataSize {
		t.Fatalf("Expected encoded size %d, got %d", globalMetadataSize, len(buf))
	}

	decoded := DecodeGlobalMetadata(buf)

	if decoded.Magic != original.Magic {
		t.Errorf("Magic mismatch: got %x, want %x", decoded.Magic, original.Magic)
	}
	if decoded.Version != original.Version {
		t.Errorf("Version mismatch: got %d, want %d", decoded.Version, original.Version)
	}
	if decoded.NumPartitions != original.NumPartitions {
		t.Errorf("NumPartitions mismatch: got %d, want %d", decoded.NumPartitions, original.NumPartitions)
	}
	if decoded.BlocksPerPart != original.BlocksPerPart {
		t.Errorf("BlocksPerPart mismatch: got %d, want %d", decoded.BlocksPerPart, original.BlocksPerPart)
	}
	if decoded.BlockSize != original.BlockSize {
		t.Errorf("BlockSize mismatch: got %d, want %d", decoded.BlockSize, original.BlockSize)
	}
	if decoded.DataType != original.DataType {
		t.Errorf("DataType mismatch: got %d, want %d", decoded.DataType, original.DataType)
	}
	if decoded.CurrentPartition != original.CurrentPartition {
		t.Errorf("CurrentPartition mismatch: got %d, want %d", decoded.CurrentPartition, original.CurrentPartition)
	}
	if decoded.OldestPartition != original.OldestPartition {
		t.Errorf("OldestPartition mismatch: got %d, want %d", decoded.OldestPartition, original.OldestPartition)
	}
	if decoded.ActiveCount != original.ActiveCount {
		t.Errorf("ActiveCount mismatch: got %d, want %d", decoded.ActiveCount, original.ActiveCount)
	}
	for i := 0; i < 16; i++ {
		if decoded.PartitionOrder[i] != original.PartitionOrder[i] {
			t.Errorf("PartitionOrder[%d] mismatch: got %d, want %d", i, decoded.PartitionOrder[i], original.PartitionOrder[i])
		}
	}
}

func TestPartitionMetadataEncodeDecode(t *testing.T) {
	original := &PartitionMetadata{
		PartitionID:  3,
		NumBlocks:    512,
		HeadBlock:    100,
		WriteOffset:  2048,
		MinTimestamp: 1000000000,
		MaxTimestamp: 9000000000,
		ObjectCount:  42,
		BytesUsed:    102400,
		Sealed:       true,
	}

	buf := original.Encode()
	if len(buf) != partitionMetadataSize {
		t.Fatalf("Expected encoded size %d, got %d", partitionMetadataSize, len(buf))
	}

	decoded := DecodePartitionMetadata(buf)

	if decoded.PartitionID != original.PartitionID {
		t.Errorf("PartitionID mismatch: got %d, want %d", decoded.PartitionID, original.PartitionID)
	}
	if decoded.NumBlocks != original.NumBlocks {
		t.Errorf("NumBlocks mismatch: got %d, want %d", decoded.NumBlocks, original.NumBlocks)
	}
	if decoded.HeadBlock != original.HeadBlock {
		t.Errorf("HeadBlock mismatch: got %d, want %d", decoded.HeadBlock, original.HeadBlock)
	}
	if decoded.WriteOffset != original.WriteOffset {
		t.Errorf("WriteOffset mismatch: got %d, want %d", decoded.WriteOffset, original.WriteOffset)
	}
	if decoded.MinTimestamp != original.MinTimestamp {
		t.Errorf("MinTimestamp mismatch: got %d, want %d", decoded.MinTimestamp, original.MinTimestamp)
	}
	if decoded.MaxTimestamp != original.MaxTimestamp {
		t.Errorf("MaxTimestamp mismatch: got %d, want %d", decoded.MaxTimestamp, original.MaxTimestamp)
	}
	if decoded.ObjectCount != original.ObjectCount {
		t.Errorf("ObjectCount mismatch: got %d, want %d", decoded.ObjectCount, original.ObjectCount)
	}
	if decoded.BytesUsed != original.BytesUsed {
		t.Errorf("BytesUsed mismatch: got %d, want %d", decoded.BytesUsed, original.BytesUsed)
	}
	if decoded.Sealed != original.Sealed {
		t.Errorf("Sealed mismatch: got %v, want %v", decoded.Sealed, original.Sealed)
	}
}

func TestIsV2Store(t *testing.T) {
	if !IsV2Store(magicNumberV2) {
		t.Error("Expected IsV2Store to return true for V2 magic number")
	}
	if IsV2Store(0x1234567890ABCDEF) {
		t.Error("Expected IsV2Store to return false for arbitrary number")
	}
	if IsV2Store(0) {
		t.Error("Expected IsV2Store to return false for zero")
	}
}

func TestIsV1Store(t *testing.T) {
	if !IsV1Store(magicNumber) {
		t.Error("Expected IsV1Store to return true for V1 magic number")
	}
	if IsV1Store(magicNumberV2) {
		t.Error("Expected IsV1Store to return false for V2 magic number")
	}
	if IsV1Store(0) {
		t.Error("Expected IsV1Store to return false for zero")
	}
}

// --- Config helper tests ---

func TestParseDataType(t *testing.T) {
	tests := []struct {
		input    string
		expected DataType
		wantErr  bool
	}{
		{"json", DataTypeJSON, false},
		{"text", DataTypeText, false},
		{"binary", DataTypeBinary, false},
		{"schema", DataTypeSchema, false},
		{"invalid", 0, true},
		{"", 0, true},
		{"JSON", 0, true}, // case-sensitive
	}

	for _, tc := range tests {
		dt, err := ParseDataType(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDataType(%q) expected error, got nil", tc.input)
			}
		} else {
			if err != nil {
				t.Errorf("ParseDataType(%q) unexpected error: %v", tc.input, err)
			}
			if dt != tc.expected {
				t.Errorf("ParseDataType(%q) = %d, want %d", tc.input, dt, tc.expected)
			}
		}
	}
}

func TestConfigBlocksPerPartition(t *testing.T) {
	// When TotalSize is 0, BlocksPerPartition returns NumBlocks
	cfg := DefaultConfig()
	cfg.NumBlocks = 256
	cfg.NumPartitions = 4
	cfg.TotalSize = 0

	if got := cfg.BlocksPerPartition(); got != 256 {
		t.Errorf("BlocksPerPartition() with TotalSize=0: got %d, want 256", got)
	}

	// When TotalSize is set, it overrides
	cfg.TotalSize = 4096 * 4 * 100 // 100 blocks per partition with 4096 block size and 4 partitions
	cfg.DataBlockSize = 4096
	if got := cfg.BlocksPerPartition(); got != 100 {
		t.Errorf("BlocksPerPartition() with TotalSize: got %d, want 100", got)
	}
}

func TestConfigDataFileSize(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NumBlocks = 100
	cfg.DataBlockSize = 4096

	expected := int64(100) * int64(4096)
	if got := cfg.DataFileSize(); got != expected {
		t.Errorf("DataFileSize() = %d, want %d", got, expected)
	}
}

func TestConfigIndexFileSize(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NumBlocks = 100
	cfg.IndexBlockSize = 4096

	// Calculate expected: entries per block * index block size
	entriesPerBlock := block.IndexEntriesPerBlock(4096)
	numIndexBlocks := (int64(100) + int64(entriesPerBlock) - 1) / int64(entriesPerBlock)
	expected := numIndexBlocks * int64(4096)

	if got := cfg.IndexFileSize(); got != expected {
		t.Errorf("IndexFileSize() = %d, want %d", got, expected)
	}
}

func TestConfigUsableDataPerBlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataBlockSize = 4096

	expected := block.UsableDataSize(4096)
	if got := cfg.UsableDataPerBlock(); got != expected {
		t.Errorf("UsableDataPerBlock() = %d, want %d", got, expected)
	}

	// Verify it's blockSize minus header
	if expected != 4096-block.BlockHeaderSize {
		t.Errorf("UsableDataSize unexpected: got %d, want %d", expected, 4096-block.BlockHeaderSize)
	}
}

func TestConfigIndexEntriesPerBlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IndexBlockSize = 4096

	expected := block.IndexEntriesPerBlock(4096)
	if got := cfg.IndexEntriesPerBlock(); got != expected {
		t.Errorf("IndexEntriesPerBlock() = %d, want %d", got, expected)
	}

	// Verify it's blockSize / entrySize
	if expected != 4096/block.IndexEntrySize {
		t.Errorf("IndexEntriesPerBlock unexpected: got %d, want %d", expected, 4096/block.IndexEntrySize)
	}
}

func TestConfigValidation(t *testing.T) {
	// Missing name
	cfg := DefaultConfig()
	cfg.Path = "/tmp"
	if err := cfg.Validate(); err != ErrNameRequired {
		t.Errorf("Expected ErrNameRequired, got %v", err)
	}

	// Missing path
	cfg = DefaultConfig()
	cfg.Name = "test"
	if err := cfg.Validate(); err != ErrPathRequired {
		t.Errorf("Expected ErrPathRequired, got %v", err)
	}

	// Invalid block size (not power of 2)
	cfg = DefaultConfig()
	cfg.Name = "test"
	cfg.Path = "/tmp"
	cfg.DataBlockSize = 1000
	if err := cfg.Validate(); err == nil {
		t.Error("Expected error for non-power-of-2 block size")
	}

	// Zero NumBlocks with no TotalSize is invalid (V2 cannot size partitions)
	cfg = DefaultConfig()
	cfg.Name = "test"
	cfg.Path = "/tmp"
	cfg.NumBlocks = 0
	cfg.TotalSize = 0
	if err := cfg.Validate(); err != ErrInvalidNumBlocks {
		t.Errorf("Expected ErrInvalidNumBlocks, got %v", err)
	}

	// Too many partitions for V2
	cfg = DefaultConfig()
	cfg.Name = "test"
	cfg.Path = "/tmp"
	cfg.NumPartitions = 20
	if err := cfg.Validate(); err != ErrTooManyPartitions {
		t.Errorf("Expected ErrTooManyPartitions, got %v", err)
	}

	// Valid V2 config
	cfg = DefaultConfig()
	cfg.Name = "test"
	cfg.Path = "/tmp"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Expected valid config, got %v", err)
	}

	// Unset StorageType (0) is treated as V2 and validates.
	cfg = DefaultConfig()
	cfg.Name = "test"
	cfg.Path = "/tmp"
	cfg.StorageType = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Expected unset StorageType to validate as V2, got %v", err)
	}
}

// --- Index/timestamp tests ---

func TestGetNewestTimestamp(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "newest-ts-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Empty V2 store should return ErrEmptyStore
	_, err = s.GetNewestTimestamp()
	if err != ErrEmptyStore {
		t.Errorf("Expected ErrEmptyStore for empty V2 store, got %v", err)
	}

	// Insert objects with known timestamps
	timestamps := []int64{1000000, 2000000, 3000000, 4000000, 5000000}
	for _, ts := range timestamps {
		_, err := s.PutObject(ts, []byte("data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	newest, err := s.GetNewestTimestamp()
	if err != nil {
		t.Fatalf("GetNewestTimestamp failed: %v", err)
	}
	if newest != 5000000 {
		t.Errorf("GetNewestTimestamp() = %d, want 5000000", newest)
	}
}

func TestGetOldestTimestamp(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "oldest-ts-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Empty V2 store should return ErrEmptyStore
	_, err = s.GetOldestTimestamp()
	if err != ErrEmptyStore {
		t.Errorf("Expected ErrEmptyStore for empty V2 store, got %v", err)
	}

	// Insert objects
	timestamps := []int64{1000000, 2000000, 3000000}
	for _, ts := range timestamps {
		_, err := s.PutObject(ts, []byte("data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	oldest, err := s.GetOldestTimestamp()
	if err != nil {
		t.Fatalf("GetOldestTimestamp failed: %v", err)
	}
	if oldest != 1000000 {
		t.Errorf("GetOldestTimestamp() = %d, want 1000000", oldest)
	}
}

// --- Partition lifecycle tests ---

func TestPartitionSeal(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "seal-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 10
	cfg.DataBlockSize = 512
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Write some data to ensure partition exists
	for i := 0; i < 3; i++ {
		ts := int64((i + 1) * 1000000)
		_, err := s.PutObject(ts, []byte("seal test data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	// Get the current partition and seal it
	p := s.currentPartition
	if p == nil {
		t.Fatal("currentPartition is nil")
	}

	if p.IsSealed() {
		t.Error("Partition should not be sealed initially")
	}

	if err := p.Seal(); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	if !p.IsSealed() {
		t.Error("Partition should be sealed after Seal()")
	}
}

func TestPartitionID(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "id-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Write something so partition is initialized
	_, err = s.PutObject(1000000, []byte("test"))
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	p := s.currentPartition
	if p == nil {
		t.Fatal("currentPartition is nil")
	}

	// The first partition should have ID 0
	if p.ID() != 0 {
		t.Errorf("Expected partition ID 0, got %d", p.ID())
	}
}

// --- Convenience wrapper tests ---

func TestPutObjectNow(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "put-now-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	before := time.Now().UnixNano()
	handle, err := s.PutObjectNow([]byte("now data"))
	if err != nil {
		t.Fatalf("PutObjectNow failed: %v", err)
	}
	after := time.Now().UnixNano()

	if handle.Timestamp < before || handle.Timestamp > after {
		t.Errorf("PutObjectNow timestamp %d not in range [%d, %d]", handle.Timestamp, before, after)
	}

	// Verify we can read it back
	data, err := s.GetObject(handle)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	if string(data) != "now data" {
		t.Errorf("Data mismatch: got %q, want %q", data, "now data")
	}
}

func TestGetObjectsSince(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "since-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Insert objects with timestamps relative to now
	now := time.Now().UnixNano()
	// Insert 5 objects: 2 hours ago, 90 min ago, 30 min ago, 10 min ago, 1 min ago
	offsets := []time.Duration{
		-2 * time.Hour,
		-90 * time.Minute,
		-30 * time.Minute,
		-10 * time.Minute,
		-1 * time.Minute,
	}

	for _, offset := range offsets {
		ts := now + int64(offset)
		_, err := s.PutObject(ts, []byte("since data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}

	// Query for objects in the last hour - should get the last 3
	handles, err := s.GetObjectsSince(time.Hour, 0)
	if err != nil {
		t.Fatalf("GetObjectsSince failed: %v", err)
	}

	if len(handles) != 3 {
		t.Errorf("GetObjectsSince(1h) returned %d handles, want 3", len(handles))
	}

	// Query for objects in the last 15 minutes - should get the last 2
	handles, err = s.GetObjectsSince(15*time.Minute, 0)
	if err != nil {
		t.Fatalf("GetObjectsSince failed: %v", err)
	}

	if len(handles) != 2 {
		t.Errorf("GetObjectsSince(15m) returned %d handles, want 2", len(handles))
	}
}

func TestDeleteStore(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "delete-store-test"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	// Insert some data
	_, err = s.PutObject(1000000, []byte("test"))
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	s.Close()

	storePath := filepath.Join(tmpDir, "delete-store-test")

	// Verify store directory exists
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		t.Fatal("Store directory should exist before delete")
	}

	// Delete using static function
	if err := DeleteStore(tmpDir, "delete-store-test"); err != nil {
		t.Fatalf("DeleteStore failed: %v", err)
	}

	// Verify directory is gone
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Error("Store directory should not exist after DeleteStore")
	}
}

// --- Recovery tests ---

func TestRecoverV2CleanStore(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "recover-v2-clean"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create V2 store: %v", err)
	}

	// Insert some data
	for i := 0; i < 5; i++ {
		ts := int64((i + 1) * 1000000)
		_, err := s.PutObject(ts, []byte("v2 recovery data"))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}
	}
	s.Close()

	// Re-open the store - this triggers V2 recovery
	s2, err := Open(tmpDir, "recover-v2-clean")
	if err != nil {
		t.Fatalf("Failed to re-open V2 store: %v", err)
	}
	defer s2.Close()

	if !s2.IsV2() {
		t.Error("Re-opened store should be V2")
	}

	// Verify data is still accessible via Stats
	stats := s2.Stats()
	if stats.NewestTimestamp != 5000000 {
		t.Errorf("Newest timestamp after V2 recovery: got %d, want 5000000", stats.NewestTimestamp)
	}
	if stats.OldestTimestamp != 1000000 {
		t.Errorf("Oldest timestamp after V2 recovery: got %d, want 1000000", stats.OldestTimestamp)
	}

	// Verify we can still write
	_, err = s2.PutObject(6000000, []byte("after recovery"))
	if err != nil {
		t.Fatalf("PutObject after V2 recovery failed: %v", err)
	}
}

// Regression test for v0.5.0 bug: schema operations on V2 partitioned stores
// returned ErrSchemaNotSupported because they read s.meta.DataType directly
// instead of using s.dataTypeLocked() which handles V2's globalMeta.
func TestV2SchemaStoreOperations(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "v2-schema-store"
	cfg.Path = tmpDir
	cfg.NumBlocks = 100
	cfg.NumPartitions = 3
	cfg.DataType = DataTypeSchema

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Failed to create V2 schema store: %v", err)
	}
	defer s.Close()

	if !s.IsV2() {
		t.Fatal("Expected V2 store")
	}
	if s.DataType() != DataTypeSchema {
		t.Fatalf("DataType() = %v, want DataTypeSchema", s.DataType())
	}

	// SetSchema must succeed (would have failed with ErrSchemaNotSupported in v0.5.0)
	sch := &schema.Schema{
		Fields: []schema.Field{
			{Index: 1, Name: "temperature", Type: "float32"},
			{Index: 2, Name: "humidity", Type: "float32"},
		},
	}
	version, err := s.SetSchema(sch)
	if err != nil {
		t.Fatalf("SetSchema on V2 schema store failed: %v", err)
	}
	if version != 1 {
		t.Errorf("First schema version: got %d, want 1", version)
	}

	// GetSchema must return the schema we just set
	got, err := s.GetSchema()
	if err != nil {
		t.Fatalf("GetSchema on V2 schema store failed: %v", err)
	}
	if len(got.Fields) != 2 {
		t.Errorf("Schema fields: got %d, want 2", len(got.Fields))
	}

	// ValidateAndCompact must work
	full := []byte(`{"temperature":72.5,"humidity":45}`)
	compact, err := s.ValidateAndCompact(full)
	if err != nil {
		t.Fatalf("ValidateAndCompact on V2 schema store failed: %v", err)
	}

	// ExpandData must work
	expanded, err := s.ExpandData(compact, 0)
	if err != nil {
		t.Fatalf("ExpandData on V2 schema store failed: %v", err)
	}
	if len(expanded) == 0 {
		t.Error("ExpandData returned empty result")
	}
}
