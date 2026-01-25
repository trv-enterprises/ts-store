// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
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
type Store struct {
	mu        sync.RWMutex
	config    Config
	meta      StoreMetadata
	dataFile  *os.File
	indexFile *os.File
	metaFile  *os.File
	schemaSet *schema.SchemaSet // Only used for DataTypeSchema stores
	closed    bool
	path      string
}

// Create creates a new store with the given configuration.
func Create(cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

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
	}

	// Initialize all primary blocks as available (not yet used)
	// They're not on the free list - they're virgin blocks
	// The free list is for reclaimed blocks

	// Write initial metadata
	if err := s.writeMeta(); err != nil {
		s.Close()
		os.RemoveAll(storePath)
		return nil, fmt.Errorf("failed to write metadata: %w", err)
	}

	return s, nil
}

// Open opens an existing store.
func Open(path string, name string) (*Store, error) {
	storePath := filepath.Join(path, name)

	// Check if store exists
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		return nil, ErrStoreNotFound
	}

	dataPath := filepath.Join(storePath, dataFileName)
	indexPath := filepath.Join(storePath, indexFileName)
	metaPath := filepath.Join(storePath, metaFileName)

	// Open metadata file and read metadata
	metaFile, err := os.OpenFile(metaPath, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open meta file: %w", err)
	}

	var meta StoreMetadata
	if err := readMetadata(metaFile, &meta); err != nil {
		metaFile.Close()
		return nil, err
	}

	// Validate magic and version
	if meta.Magic != magicNumber {
		metaFile.Close()
		return nil, ErrInvalidMagic
	}
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
	}

	s := &Store{
		config:    cfg,
		meta:      meta,
		dataFile:  dataFile,
		indexFile: indexFile,
		metaFile:  metaFile,
		closed:    false,
		path:      storePath,
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

// Close closes the store and flushes all data to disk.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	var errs []error

	// Write final metadata
	if err := s.writeMetaLocked(); err != nil {
		errs = append(errs, err)
	}

	// Sync and close files
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

// Reset clears all data from the store and reinitializes it.
// This is useful for recovering from clock issues or starting fresh.
// All existing data will be lost.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	// Reset metadata to initial state
	s.meta.HeadBlock = 0
	s.meta.TailBlock = 0
	s.meta.WriteOffset = 0

	// Clear all index entries
	emptyEntry := make([]byte, block.IndexEntrySize)
	for i := uint32(0); i < s.meta.NumBlocks; i++ {
		offset := s.indexOffset(i)
		if _, err := s.indexFile.WriteAt(emptyEntry, offset); err != nil {
			return fmt.Errorf("failed to clear index entry %d: %w", i, err)
		}
	}

	// Clear all block headers
	emptyHeader := make([]byte, block.BlockHeaderSize)
	for i := uint32(0); i < s.meta.NumBlocks; i++ {
		offset := s.blockOffset(i)
		if _, err := s.dataFile.WriteAt(emptyHeader, offset); err != nil {
			return fmt.Errorf("failed to clear block header %d: %w", i, err)
		}
	}

	// Write updated metadata
	if err := s.writeMetaLocked(); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Sync files
	if err := s.dataFile.Sync(); err != nil {
		return err
	}
	if err := s.indexFile.Sync(); err != nil {
		return err
	}

	return nil
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
	return s.meta.DataType
}

// Stats returns current store statistics.
func (s *Store) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

// StoreStats contains runtime statistics about the store.
type StoreStats struct {
	// Block configuration
	NumBlocks      uint32 `json:"num_blocks"`
	DataBlockSize  uint32 `json:"data_block_size"`
	IndexBlockSize uint32 `json:"index_block_size"`

	// Data type
	DataType string `json:"data_type"`

	// Current state
	HeadBlock   uint32 `json:"head_block"`
	TailBlock   uint32 `json:"tail_block"`
	WriteOffset uint32 `json:"write_offset"`

	// Derived stats
	ActiveBlocks uint32 `json:"active_blocks"`

	// Timestamps
	OldestTimestamp int64  `json:"oldest_timestamp,omitempty"`
	OldestTime      string `json:"oldest_time,omitempty"`
	NewestTimestamp int64  `json:"newest_timestamp,omitempty"`
	NewestTime      string `json:"newest_time,omitempty"`
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
