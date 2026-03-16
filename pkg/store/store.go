// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tviviano/ts-store/pkg/block"
	"github.com/tviviano/ts-store/pkg/schema"
)

var (
	ErrStoreExists         = errors.New("store already exists")
	ErrStoreNotFound       = errors.New("store not found")
	ErrStoreClosed         = errors.New("store is closed")
	ErrInvalidMagic        = errors.New("invalid store file (bad magic number)")
	ErrVersionMismatch     = errors.New("store version mismatch")
	ErrBlockOutOfRange     = errors.New("block number out of range")
	ErrInvalidTimestamp    = errors.New("invalid timestamp")
	ErrTimestampOutOfOrder = errors.New("timestamp must be greater than newest entry")
)

const (
	magicNumber uint64 = 0x545353544F524531 // "TSSTORE1"
	version     uint32 = 1

	// File names
	dataFileName   = "data.tsdb"
	indexFileName  = "index.tsdb"
	metaFileName   = "meta.tsdb"
	schemaFileName = "schema.json"
)

// StoreMetadata is persisted to disk and contains store configuration.
// Total size: 64 bytes
//
// The circular buffer uses HeadBlock and TailBlock to track used space:
// - HeadBlock: newest data (where writes happen)
// - TailBlock: oldest data (reclaimed when space needed)
// - Free space is implicit: the gap from (HeadBlock+1) to TailBlock
type StoreMetadata struct {
	Magic          uint64   // Magic number for file identification
	Version        uint32   // Store format version
	NumBlocks      uint32   // Number of circular blocks
	DataBlockSize  uint32   // Size of each data block
	IndexBlockSize uint32   // Size of each index block
	HeadBlock      uint32   // Current head of circle (newest)
	TailBlock      uint32   // Current tail of circle (oldest)
	WriteOffset    uint32   // Current write position within head block (V2 packed format)
	DataType       DataType // Type of data stored (binary, text, json, schema)
	Reserved       [19]byte
}

const metadataSize = 64

// Store represents an open circular time series store.
// Supports both V1 (single circular buffer) and V2 (partitioned) storage.
type Store struct {
	mu        sync.RWMutex
	config    Config
	meta      StoreMetadata  // V1 metadata
	dataFile  *os.File       // V1 data file
	indexFile *os.File       // V1 index file
	metaFile  *os.File       // Shared metadata file (both V1 and V2)
	schemaSet *schema.SchemaSet // Only used for DataTypeSchema stores
	closed    bool
	path      string

	// V2 partitioned storage fields
	isV2             bool                   // True if V2 partitioned store
	globalMeta       *GlobalMetadata        // V2 global metadata
	partitions       map[uint32]*Partition  // V2 partition map (id -> partition)
	currentPartition *Partition             // V2 current write partition
}

// Create creates a new store with the given configuration.
func Create(cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Route to V2 creation if configured for partitioned storage
	if cfg.StorageType == StorageTypeV2Partitioned {
		return createV2(cfg)
	}

	return createV1(cfg)
}

// createV1 creates a new V1 circular buffer store.
func createV1(cfg Config) (*Store, error) {
	storePath := filepath.Join(cfg.Path, cfg.Name)

	// Check if store already exists
	if _, err := os.Stat(storePath); err == nil {
		return nil, ErrStoreExists
	}

	// Create store directory
	if err := os.MkdirAll(storePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %w", err)
	}

	// Create and initialize files
	dataPath := filepath.Join(storePath, dataFileName)
	indexPath := filepath.Join(storePath, indexFileName)
	metaPath := filepath.Join(storePath, metaFileName)

	// Create data file
	dataFile, err := os.Create(dataPath)
	if err != nil {
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to create data file: %w", err)
	}

	// Pre-allocate data file
	if err := dataFile.Truncate(cfg.DataFileSize()); err != nil {
		dataFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to allocate data file: %w", err)
	}

	// Create index file
	indexFile, err := os.Create(indexPath)
	if err != nil {
		dataFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to create index file: %w", err)
	}

	// Pre-allocate index file
	if err := indexFile.Truncate(cfg.IndexFileSize()); err != nil {
		dataFile.Close()
		indexFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to allocate index file: %w", err)
	}

	// Create metadata file
	metaFile, err := os.Create(metaPath)
	if err != nil {
		dataFile.Close()
		indexFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to create meta file: %w", err)
	}

	// Initialize metadata
	meta := StoreMetadata{
		Magic:          magicNumber,
		Version:        version,
		NumBlocks:      cfg.NumBlocks,
		DataBlockSize:  cfg.DataBlockSize,
		IndexBlockSize: cfg.IndexBlockSize,
		HeadBlock:      0,
		TailBlock:      0,
		WriteOffset:    0,
		DataType:       cfg.DataType,
	}

	s := &Store{
		config:    cfg,
		meta:      meta,
		dataFile:  dataFile,
		indexFile: indexFile,
		metaFile:  metaFile,
		closed:    false,
		path:      storePath,
		isV2:      false,
	}

	// Write initial metadata
	if err := s.writeMeta(); err != nil {
		s.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to write metadata: %w", err)
	}

	return s, nil
}

// createV2 creates a new V2 partitioned store.
func createV2(cfg Config) (*Store, error) {
	storePath := filepath.Join(cfg.Path, cfg.Name)

	// Check if store already exists
	if _, err := os.Stat(storePath); err == nil {
		return nil, ErrStoreExists
	}

	// Create store directory
	if err := os.MkdirAll(storePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %w", err)
	}

	metaPath := filepath.Join(storePath, metaFileName)

	// Create global metadata file
	metaFile, err := os.Create(metaPath)
	if err != nil {
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to create meta file: %w", err)
	}

	blocksPerPart := cfg.BlocksPerPartition()

	// Initialize global metadata
	globalMeta := &GlobalMetadata{
		Magic:            magicNumberV2,
		Version:          versionV2,
		NumPartitions:    cfg.NumPartitions,
		BlocksPerPart:    blocksPerPart,
		BlockSize:        cfg.DataBlockSize,
		DataType:         cfg.DataType,
		CurrentPartition: 0,
		OldestPartition:  0,
		ActiveCount:      0,
	}

	s := &Store{
		config:     cfg,
		metaFile:   metaFile,
		closed:     false,
		path:       storePath,
		isV2:       true,
		globalMeta: globalMeta,
		partitions: make(map[uint32]*Partition),
	}

	// Create the first partition
	firstPart, err := createPartition(storePath, 0, blocksPerPart, cfg.DataBlockSize)
	if err != nil {
		metaFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to create first partition: %w", err)
	}

	s.partitions[0] = firstPart
	s.currentPartition = firstPart
	s.globalMeta.CurrentPartition = 0
	s.globalMeta.OldestPartition = 0
	s.globalMeta.PartitionOrder[0] = 0
	s.globalMeta.ActiveCount = 1

	// Write global metadata
	if err := s.writeGlobalMeta(); err != nil {
		firstPart.Close()
		metaFile.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to write global metadata: %w", err)
	}

	return s, nil
}

// Open opens an existing store.
// Automatically detects V1 vs V2 format based on the magic number.
func Open(path string, name string) (*Store, error) {
	storePath := filepath.Join(path, name)

	// Check if store exists
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		return nil, ErrStoreNotFound
	}

	metaPath := filepath.Join(storePath, metaFileName)

	// Open metadata file and read magic to detect version
	metaFile, err := os.OpenFile(metaPath, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open meta file: %w", err)
	}

	// Read first 8 bytes to get magic number
	magicBuf := make([]byte, 8)
	if _, err := metaFile.ReadAt(magicBuf, 0); err != nil {
		metaFile.Close()
		return nil, fmt.Errorf("failed to read magic number: %w", err)
	}
	magic := binary.LittleEndian.Uint64(magicBuf)

	// Route to appropriate open function based on magic
	if IsV2Store(magic) {
		return openV2(path, name, storePath, metaFile)
	}
	if IsV1Store(magic) {
		return openV1(path, name, storePath, metaFile)
	}

	metaFile.Close()
	return nil, ErrInvalidMagic
}

// openV1 opens a V1 circular buffer store.
func openV1(path string, name string, storePath string, metaFile *os.File) (*Store, error) {
	dataPath := filepath.Join(storePath, dataFileName)
	indexPath := filepath.Join(storePath, indexFileName)

	var meta StoreMetadata
	if err := readMetadata(metaFile, &meta); err != nil {
		metaFile.Close()
		return nil, err
	}

	// Validate version
	if meta.Version != version {
		metaFile.Close()
		return nil, ErrVersionMismatch
	}

	// Open data file
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0644)
	if err != nil {
		metaFile.Close()
		return nil, fmt.Errorf("failed to open data file: %w", err)
	}

	// Open index file
	indexFile, err := os.OpenFile(indexPath, os.O_RDWR, 0644)
	if err != nil {
		metaFile.Close()
		dataFile.Close()
		return nil, fmt.Errorf("failed to open index file: %w", err)
	}

	cfg := Config{
		Name:           name,
		Path:           path,
		NumBlocks:      meta.NumBlocks,
		DataBlockSize:  meta.DataBlockSize,
		IndexBlockSize: meta.IndexBlockSize,
		DataType:       meta.DataType,
		StorageType:    StorageTypeV1Circular,
	}

	s := &Store{
		config:    cfg,
		meta:      meta,
		dataFile:  dataFile,
		indexFile: indexFile,
		metaFile:  metaFile,
		closed:    false,
		path:      storePath,
		isV2:      false,
	}

	// Perform crash recovery to fix any inconsistencies
	if err := s.recoverFromCrash(); err != nil {
		s.Close()
		return nil, fmt.Errorf("crash recovery failed: %w", err)
	}

	// Load schema for schema stores
	if err := s.loadSchema(); err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to load schema: %w", err)
	}

	return s, nil
}

// openV2 opens a V2 partitioned store.
func openV2(path string, name string, storePath string, metaFile *os.File) (*Store, error) {
	// Read global metadata
	buf := make([]byte, globalMetadataSize)
	if _, err := metaFile.ReadAt(buf, 0); err != nil {
		metaFile.Close()
		return nil, fmt.Errorf("failed to read global metadata: %w", err)
	}

	globalMeta := DecodeGlobalMetadata(buf)

	// Validate version
	if globalMeta.Version != versionV2 {
		metaFile.Close()
		return nil, ErrVersionMismatch
	}

	cfg := Config{
		Name:           name,
		Path:           path,
		NumBlocks:      globalMeta.BlocksPerPart,
		DataBlockSize:  globalMeta.BlockSize,
		IndexBlockSize: 4096, // Default, not stored in V2 global meta
		DataType:       globalMeta.DataType,
		StorageType:    StorageTypeV2Partitioned,
		NumPartitions:  globalMeta.NumPartitions,
	}

	s := &Store{
		config:     cfg,
		metaFile:   metaFile,
		closed:     false,
		path:       storePath,
		isV2:       true,
		globalMeta: globalMeta,
		partitions: make(map[uint32]*Partition),
	}

	// Open all active partitions
	for i := uint8(0); i < globalMeta.ActiveCount; i++ {
		partID := uint32(globalMeta.PartitionOrder[i])
		part, err := openPartition(storePath, partID, globalMeta.BlockSize)
		if err != nil {
			// Clean up already opened partitions
			for _, p := range s.partitions {
				p.Close()
			}
			metaFile.Close()
			return nil, fmt.Errorf("failed to open partition %d: %w", partID, err)
		}
		s.partitions[partID] = part
	}

	// Set current partition
	if globalMeta.ActiveCount > 0 {
		s.currentPartition = s.partitions[globalMeta.CurrentPartition]
	}

	// Perform V2 crash recovery
	if err := s.recoverPartitionsV2(); err != nil {
		s.Close()
		return nil, fmt.Errorf("partition recovery failed: %w", err)
	}

	// Load schema for schema stores
	if err := s.loadSchema(); err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to load schema: %w", err)
	}

	return s, nil
}

// Close closes the store and flushes all data to disk.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	var errs []error

	if s.isV2 {
		// V2: Close all partitions
		for _, p := range s.partitions {
			if p != nil {
				if err := p.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}

		// Write final global metadata
		if err := s.writeGlobalMeta(); err != nil {
			errs = append(errs, err)
		}
	} else {
		// V1: Write final metadata
		if err := s.writeMetaLocked(); err != nil {
			errs = append(errs, err)
		}

		// Sync and close V1 files
		if s.dataFile != nil {
			if err := s.dataFile.Sync(); err != nil {
				errs = append(errs, err)
			}
			if err := s.dataFile.Close(); err != nil {
				errs = append(errs, err)
			}
		}

		if s.indexFile != nil {
			if err := s.indexFile.Sync(); err != nil {
				errs = append(errs, err)
			}
			if err := s.indexFile.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Close metadata file (shared by V1 and V2)
	if s.metaFile != nil {
		if err := s.metaFile.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := s.metaFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	s.closed = true

	if len(errs) > 0 {
		return fmt.Errorf("errors closing store: %v", errs)
	}
	return nil
}

// Delete closes and removes the store and all its files.
func (s *Store) Delete() error {
	path := s.path

	if err := s.Close(); err != nil {
		// Continue with deletion even if close fails
	}

	return os.RemoveAll(path)
}

// Reset performs a soft reset of the store by resetting metadata pointers.
// This is useful for recovering from clock issues or starting fresh.
// Old data becomes inaccessible and will be overwritten as new data arrives.
// Note: This is a SOFT reset - old data remains on disk until overwritten.
// This is an O(1) operation - it only writes the metadata and clears block 0's index.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if s.isV2 {
		return s.resetV2()
	}

	// V1: Reset metadata to initial state - this makes all existing data inaccessible
	// The circular buffer will overwrite old blocks naturally as new data arrives
	s.meta.HeadBlock = 0
	s.meta.TailBlock = 0
	s.meta.WriteOffset = 0

	// Clear block 0's index entry so readers don't see stale data
	emptyEntry := make([]byte, block.IndexEntrySize)
	if _, err := s.indexFile.WriteAt(emptyEntry, s.indexOffset(0)); err != nil {
		return fmt.Errorf("failed to clear index entry: %w", err)
	}

	// Write updated metadata
	if err := s.writeMetaLocked(); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Sync files
	if err := s.indexFile.Sync(); err != nil {
		return err
	}
	if err := s.metaFile.Sync(); err != nil {
		return err
	}

	return nil
}

// resetV2 performs a reset for V2 partitioned stores.
// Deletes all partitions except the first one, which is cleared.
func (s *Store) resetV2() error {
	// Close and delete all partitions except first
	for id, p := range s.partitions {
		if p != nil {
			p.Close()
		}
		if id != 0 {
			deletePartition(s.path, id)
		}
		delete(s.partitions, id)
	}

	// Delete partition 0 and recreate it
	deletePartition(s.path, 0)

	newPart, err := createPartition(s.path, 0, s.globalMeta.BlocksPerPart, s.globalMeta.BlockSize)
	if err != nil {
		return fmt.Errorf("failed to create partition 0: %w", err)
	}

	s.partitions[0] = newPart
	s.currentPartition = newPart
	s.globalMeta.CurrentPartition = 0
	s.globalMeta.OldestPartition = 0
	s.globalMeta.ActiveCount = 1
	s.globalMeta.PartitionOrder = [16]uint8{}
	s.globalMeta.PartitionOrder[0] = 0

	return s.writeGlobalMeta()
}

// DeleteStore removes a store by path and name without opening it.
func DeleteStore(path string, name string) error {
	storePath := filepath.Join(path, name)
	return os.RemoveAll(storePath)
}

// Config returns the store configuration.
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// DataType returns the store's data type.
func (s *Store) DataType() DataType {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.isV2 {
		return s.globalMeta.DataType
	}
	return s.meta.DataType
}

// IsV2 returns true if this is a V2 partitioned store.
func (s *Store) IsV2() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isV2
}

// Stats returns current store statistics.
func (s *Store) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.isV2 {
		return s.statsV2()
	}

	stats := StoreStats{
		// Block configuration
		NumBlocks:      s.meta.NumBlocks,
		DataBlockSize:  s.meta.DataBlockSize,
		IndexBlockSize: s.meta.IndexBlockSize,

		// Data type
		DataType: s.meta.DataType.String(),

		// Current state
		HeadBlock:   s.meta.HeadBlock,
		TailBlock:   s.meta.TailBlock,
		WriteOffset: s.meta.WriteOffset,

		// Derived stats
		ActiveBlocks: s.activeBlockCount(),

		// Version
		StorageVersion: 1,
	}

	// Get oldest timestamp (from tail)
	if tailEntry, err := s.readIndexEntry(s.meta.TailBlock); err == nil && tailEntry.Timestamp != 0 {
		stats.OldestTimestamp = tailEntry.Timestamp
		stats.OldestTime = time.Unix(0, tailEntry.Timestamp).UTC().Format(time.RFC3339)
	}

	// Get newest timestamp (from head)
	if headEntry, err := s.readIndexEntry(s.meta.HeadBlock); err == nil && headEntry.Timestamp != 0 {
		stats.NewestTimestamp = headEntry.Timestamp
		stats.NewestTime = time.Unix(0, headEntry.Timestamp).UTC().Format(time.RFC3339)
	}

	return stats
}

// statsV2 returns statistics for V2 partitioned stores.
func (s *Store) statsV2() StoreStats {
	stats := StoreStats{
		// Block configuration (per partition)
		NumBlocks:     s.globalMeta.BlocksPerPart,
		DataBlockSize: s.globalMeta.BlockSize,

		// Data type
		DataType: s.globalMeta.DataType.String(),

		// V2-specific
		StorageVersion:   2,
		NumPartitions:    s.globalMeta.NumPartitions,
		ActivePartitions: uint32(s.globalMeta.ActiveCount),
		CurrentPartition: s.globalMeta.CurrentPartition,
	}

	// Collect partition stats
	partStats := make([]PartitionStats, 0, s.globalMeta.ActiveCount)
	var totalBlocks uint32
	var oldestTs, newestTs int64

	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		if p, ok := s.partitions[partID]; ok {
			ps := p.Stats()
			partStats = append(partStats, ps)
			totalBlocks += ps.HeadBlock + 1

			if ps.MinTimestamp > 0 && (oldestTs == 0 || ps.MinTimestamp < oldestTs) {
				oldestTs = ps.MinTimestamp
			}
			if ps.MaxTimestamp > newestTs {
				newestTs = ps.MaxTimestamp
			}
		}
	}

	stats.PartitionStats = partStats
	stats.ActiveBlocks = totalBlocks

	if oldestTs > 0 {
		stats.OldestTimestamp = oldestTs
		stats.OldestTime = time.Unix(0, oldestTs).UTC().Format(time.RFC3339)
	}
	if newestTs > 0 {
		stats.NewestTimestamp = newestTs
		stats.NewestTime = time.Unix(0, newestTs).UTC().Format(time.RFC3339)
	}

	return stats
}

// StoreStats contains runtime statistics about the store.
type StoreStats struct {
	// Block configuration
	NumBlocks      uint32 `json:"num_blocks"`
	DataBlockSize  uint32 `json:"data_block_size"`
	IndexBlockSize uint32 `json:"index_block_size,omitempty"`

	// Data type
	DataType string `json:"data_type"`

	// Current state (V1)
	HeadBlock   uint32 `json:"head_block,omitempty"`
	TailBlock   uint32 `json:"tail_block,omitempty"`
	WriteOffset uint32 `json:"write_offset,omitempty"`

	// Derived stats
	ActiveBlocks uint32 `json:"active_blocks"`

	// Timestamps
	OldestTimestamp int64  `json:"oldest_timestamp,omitempty"`
	OldestTime      string `json:"oldest_time,omitempty"`
	NewestTimestamp int64  `json:"newest_timestamp,omitempty"`
	NewestTime      string `json:"newest_time,omitempty"`

	// V2 Partition fields
	StorageVersion   uint32           `json:"storage_version"`
	NumPartitions    uint32           `json:"num_partitions,omitempty"`
	ActivePartitions uint32           `json:"active_partitions,omitempty"`
	CurrentPartition uint32           `json:"current_partition,omitempty"`
	PartitionStats   []PartitionStats `json:"partition_stats,omitempty"`
}

// writeMeta writes metadata to disk (acquires lock).
func (s *Store) writeMeta() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeMetaLocked()
}

// writeMetaLocked writes metadata to disk (lock must be held).
func (s *Store) writeMetaLocked() error {
	buf := make([]byte, metadataSize)
	binary.LittleEndian.PutUint64(buf[0:8], s.meta.Magic)
	binary.LittleEndian.PutUint32(buf[8:12], s.meta.Version)
	binary.LittleEndian.PutUint32(buf[12:16], s.meta.NumBlocks)
	binary.LittleEndian.PutUint32(buf[16:20], s.meta.DataBlockSize)
	binary.LittleEndian.PutUint32(buf[20:24], s.meta.IndexBlockSize)
	binary.LittleEndian.PutUint32(buf[24:28], s.meta.HeadBlock)
	binary.LittleEndian.PutUint32(buf[28:32], s.meta.TailBlock)
	binary.LittleEndian.PutUint32(buf[32:36], s.meta.WriteOffset)
	buf[36] = byte(s.meta.DataType)
	// bytes 37-63 reserved

	if _, err := s.metaFile.WriteAt(buf, 0); err != nil {
		return err
	}
	return s.metaFile.Sync()
}

// readMetadata reads metadata from a file.
func readMetadata(f *os.File, meta *StoreMetadata) error {
	buf := make([]byte, metadataSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return err
	}

	meta.Magic = binary.LittleEndian.Uint64(buf[0:8])
	meta.Version = binary.LittleEndian.Uint32(buf[8:12])
	meta.NumBlocks = binary.LittleEndian.Uint32(buf[12:16])
	meta.DataBlockSize = binary.LittleEndian.Uint32(buf[16:20])
	meta.IndexBlockSize = binary.LittleEndian.Uint32(buf[20:24])
	meta.HeadBlock = binary.LittleEndian.Uint32(buf[24:28])
	meta.TailBlock = binary.LittleEndian.Uint32(buf[28:32])
	meta.WriteOffset = binary.LittleEndian.Uint32(buf[32:36])
	meta.DataType = DataType(buf[36])

	return nil
}

// blockOffset calculates the file offset for a given block number.
func (s *Store) blockOffset(blockNum uint32) int64 {
	return int64(blockNum) * int64(s.config.DataBlockSize)
}

// indexOffset calculates the file offset for a given index entry.
func (s *Store) indexOffset(entryNum uint32) int64 {
	return int64(entryNum) * int64(block.IndexEntrySize)
}

// readBlockHeader reads the header of a block.
func (s *Store) readBlockHeader(blockNum uint32) (*block.BlockHeader, error) {
	buf := make([]byte, block.BlockHeaderSize)
	offset := s.blockOffset(blockNum)

	if _, err := s.dataFile.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	header := &block.BlockHeader{}
	header.Decode(buf)
	return header, nil
}

// writeBlockHeader writes the header of a block.
func (s *Store) writeBlockHeader(blockNum uint32, header *block.BlockHeader) error {
	buf := make([]byte, block.BlockHeaderSize)
	header.Encode(buf)
	offset := s.blockOffset(blockNum)

	_, err := s.dataFile.WriteAt(buf, offset)
	return err
}

// readIndexEntry reads an index entry.
func (s *Store) readIndexEntry(entryNum uint32) (*block.IndexEntry, error) {
	buf := make([]byte, block.IndexEntrySize)
	offset := s.indexOffset(entryNum)

	if _, err := s.indexFile.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	entry := &block.IndexEntry{}
	entry.Decode(buf)
	return entry, nil
}

// writeIndexEntry writes an index entry.
func (s *Store) writeIndexEntry(entryNum uint32, entry *block.IndexEntry) error {
	buf := make([]byte, block.IndexEntrySize)
	entry.Encode(buf)
	offset := s.indexOffset(entryNum)

	_, err := s.indexFile.WriteAt(buf, offset)
	return err
}
