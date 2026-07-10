// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tviviano/ts-store/pkg/schema"
)

var (
	ErrStoreExists         = errors.New("store already exists")
	ErrStoreNotFound       = errors.New("store not found")
	ErrStoreClosed         = errors.New("store is closed")
	ErrInvalidMagic        = errors.New("invalid store file (bad magic number)")
	ErrVersionMismatch     = errors.New("store version mismatch")
	ErrV1NotSupported      = errors.New("V1 circular-buffer stores are no longer supported")
	ErrBlockOutOfRange     = errors.New("block number out of range")
	ErrCorruptBlock        = errors.New("corrupt block (checksum mismatch)")
	ErrInvalidTimestamp    = errors.New("invalid timestamp")
	ErrTimestampOutOfOrder = errors.New("timestamp must be greater than newest entry")
)

const (
	magicNumber uint64 = 0x545353544F524531 // "TSSTORE1" (V1 detection only)

	// File names
	dataFileName   = "data.tsdb"
	indexFileName  = "index.tsdb"
	metaFileName   = "meta.tsdb"
	schemaFileName = "schema.json"
)

// Store represents an open partitioned (V2) time series store.
type Store struct {
	mu        sync.RWMutex
	config    Config
	metaFile  *os.File       // Global metadata file
	schemaSet *schema.SchemaSet // Only used for DataTypeSchema stores
	closed    bool
	path      string

	// V2 partitioned storage fields
	isV2             bool                   // Always true (V2 is the only storage mode)
	globalMeta       *GlobalMetadata        // V2 global metadata
	partitions       map[uint32]*Partition  // V2 partition map (id -> partition)
	currentPartition *Partition             // V2 current write partition

	// Activity counters surfaced via /metrics. Process-lifetime by
	// default; resettable via ResetMetrics.
	metrics storeMetrics
}

// Create creates a new store with the given configuration.
// All stores use V2 partitioned storage.
func Create(cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return createV2(cfg)
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
	s.initMetrics()

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
	// Names are joined onto the data path; refuse anything that could
	// escape it (e.g. "../..") before touching the filesystem.
	if err := ValidateStoreName(name); err != nil {
		return nil, err
	}

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

	// Route based on magic. Only V2 is supported; V1 stores are rejected.
	if IsV2Store(magic) {
		return openV2(path, name, storePath, metaFile)
	}
	if IsV1Store(magic) {
		metaFile.Close()
		return nil, ErrV1NotSupported
	}

	metaFile.Close()
	return nil, ErrInvalidMagic
}

// ReadDataTypeAt reads a store's data type straight from its meta.tsdb
// without opening the store. Opening is expensive at the service layer —
// it spawns WS/MQTT/alerts/rollups managers that stay alive until
// shutdown — and callers like the store listing only need this one byte.
func ReadDataTypeAt(storeDir string) (DataType, error) {
	f, err := os.Open(filepath.Join(storeDir, metaFileName))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// DataType is byte 24 of the V2 global metadata block (see
	// GlobalMetadata.Encode); the first 8 bytes are the magic.
	buf := make([]byte, 25)
	if _, err := io.ReadFull(f, buf); err != nil {
		return 0, fmt.Errorf("failed to read metadata header: %w", err)
	}
	magic := binary.LittleEndian.Uint64(buf[0:8])
	if IsV1Store(magic) {
		return 0, ErrV1NotSupported
	}
	if !IsV2Store(magic) {
		return 0, ErrInvalidMagic
	}
	return DataType(buf[24]), nil
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
	s.initMetrics()

	// Recover from any interrupted rollover BEFORE opening partitions.
	// This may create/delete partition directories and update globalMeta.
	if err := s.recoverRolloverV2(); err != nil {
		s.Close()
		return nil, fmt.Errorf("rollover recovery failed: %w", err)
	}

	// Open all active partitions, skipping any already opened by recovery
	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		if s.partitions[partID] != nil {
			continue // Already opened by rollover recovery
		}
		part, err := openPartition(storePath, partID, s.globalMeta.BlockSize)
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
	if s.globalMeta.ActiveCount > 0 {
		s.currentPartition = s.partitions[s.globalMeta.CurrentPartition]
	}

	// Perform per-partition crash recovery
	if err := s.recoverPartitionsV2(); err != nil {
		s.Close()
		return nil, fmt.Errorf("partition recovery failed: %w", err)
	}

	// Clean up any orphaned partition directories
	if err := s.cleanOrphanedPartitions(); err != nil {
		s.Close()
		return nil, fmt.Errorf("orphan cleanup failed: %w", err)
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

	// Close all partitions
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

	// Close metadata file
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

	return s.resetV2()
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
	// RemoveAll on a joined path — never allow a name that could point
	// outside the data directory.
	if err := ValidateStoreName(name); err != nil {
		return err
	}
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
	return s.globalMeta.DataType
}

// IsV2 returns true if this is a V2 partitioned store.
func (s *Store) IsV2() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isV2
}

// DiskUsage returns the total bytes of storage files on disk for this store.
//
// For V1 stores this is meta.tsdb + data.tsdb + index.tsdb at the store root.
// For V2 stores this is the root meta.tsdb plus every .tsdb file under each
// partition-N/ directory. Non-storage files (keys.json, schema.json, MQTT
// cursors, etc.) are excluded.
func (s *Store) DiskUsage() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total uint64
	err := filepath.WalkDir(s.path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(d.Name()) != ".tsdb" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += uint64(fi.Size())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// Stats returns current store statistics.
func (s *Store) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.statsV2()
}

// statsV2 returns statistics for V2 partitioned stores.
//
// NumBlocks and ActiveBlocks are both reported across the active partition
// ring (BlocksPerPart * ActiveCount). This keeps the usage percentage
// meaningful: as partitions rotate in and out, ActiveCount stays at its
// configured maximum and the ratio reflects how full the live ring is.
func (s *Store) statsV2() StoreStats {
	stats := StoreStats{
		DataBlockSize: s.globalMeta.BlockSize,
		DataType:      s.globalMeta.DataType.String(),

		StorageVersion:   2,
		NumPartitions:    s.globalMeta.NumPartitions,
		ActivePartitions: uint32(s.globalMeta.ActiveCount),
		CurrentPartition: s.globalMeta.CurrentPartition,
	}

	stats.NumBlocks = s.globalMeta.BlocksPerPart * uint32(s.globalMeta.ActiveCount)

	partStats := make([]PartitionStats, 0, s.globalMeta.ActiveCount)
	var totalBlocks uint32
	var oldestTs, newestTs int64

	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		p, ok := s.partitions[partID]
		if !ok {
			continue
		}
		ps := p.Stats()
		partStats = append(partStats, ps)

		// Only count blocks for partitions that have been written to.
		// An empty partition has HeadBlock=0 and ObjectCount=0, and must
		// not contribute 1 block to the total.
		if ps.ObjectCount > 0 {
			totalBlocks += ps.HeadBlock + 1
		}

		if ps.MinTimestamp > 0 && (oldestTs == 0 || ps.MinTimestamp < oldestTs) {
			oldestTs = ps.MinTimestamp
		}
		if ps.MaxTimestamp > newestTs {
			newestTs = ps.MaxTimestamp
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

