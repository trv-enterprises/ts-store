// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tviviano/ts-store/pkg/block"
)

// Partition represents a single partition in a V2 partitioned store.
// Each partition has its own data file, index file, and metadata file.
type Partition struct {
	id         uint32
	path       string
	meta       PartitionMetadata
	dataFile   *os.File
	indexFile  *os.File
	metaFile   *os.File
	blockSize  uint32
	numBlocks  uint32
	closed     bool
}

// partitionDirName returns the directory name for a partition.
func partitionDirName(id uint32) string {
	return fmt.Sprintf("partition-%d", id)
}

// createPartition creates a new partition directory and files.
func createPartition(basePath string, id uint32, numBlocks uint32, blockSize uint32) (*Partition, error) {
	partPath := filepath.Join(basePath, partitionDirName(id))

	// Create partition directory
	if err := os.MkdirAll(partPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create partition directory: %w", err)
	}

	dataPath := filepath.Join(partPath, dataFileName)
	indexPath := filepath.Join(partPath, indexFileName)
	metaPath := filepath.Join(partPath, metaFileName)

	// Create data file
	dataFile, err := os.Create(dataPath)
	if err != nil {
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to create partition data file: %w", err)
	}

	// Pre-allocate data file
	dataSize := int64(numBlocks) * int64(blockSize)
	if err := dataFile.Truncate(dataSize); err != nil {
		dataFile.Close()
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to allocate partition data file: %w", err)
	}

	// Create index file
	indexFile, err := os.Create(indexPath)
	if err != nil {
		dataFile.Close()
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to create partition index file: %w", err)
	}

	// Pre-allocate index file
	indexSize := int64(numBlocks) * int64(block.IndexEntrySize)
	if err := indexFile.Truncate(indexSize); err != nil {
		dataFile.Close()
		indexFile.Close()
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to allocate partition index file: %w", err)
	}

	// Create metadata file
	metaFile, err := os.Create(metaPath)
	if err != nil {
		dataFile.Close()
		indexFile.Close()
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to create partition meta file: %w", err)
	}

	// Initialize partition metadata
	meta := PartitionMetadata{
		PartitionID:  id,
		NumBlocks:    numBlocks,
		HeadBlock:    0,
		WriteOffset:  0,
		MinTimestamp: 0,
		MaxTimestamp: 0,
		ObjectCount:  0,
		BytesUsed:    0,
		Sealed:       false,
	}

	p := &Partition{
		id:        id,
		path:      partPath,
		meta:      meta,
		dataFile:  dataFile,
		indexFile: indexFile,
		metaFile:  metaFile,
		blockSize: blockSize,
		numBlocks: numBlocks,
		closed:    false,
	}

	// Write initial metadata
	if err := p.writeMeta(); err != nil {
		p.Close()
		os.RemoveAll(partPath)
		return nil, fmt.Errorf("failed to write partition metadata: %w", err)
	}

	return p, nil
}

// openPartition opens an existing partition.
func openPartition(basePath string, id uint32, blockSize uint32) (*Partition, error) {
	partPath := filepath.Join(basePath, partitionDirName(id))

	// Check if partition exists
	if _, err := os.Stat(partPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("partition %d not found", id)
	}

	dataPath := filepath.Join(partPath, dataFileName)
	indexPath := filepath.Join(partPath, indexFileName)
	metaPath := filepath.Join(partPath, metaFileName)

	// Open metadata file and read metadata
	metaFile, err := os.OpenFile(metaPath, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open partition meta file: %w", err)
	}

	buf := make([]byte, partitionMetadataSize)
	if _, err := metaFile.ReadAt(buf, 0); err != nil {
		metaFile.Close()
		return nil, fmt.Errorf("failed to read partition metadata: %w", err)
	}

	meta := DecodePartitionMetadata(buf)

	// Open data file
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR, 0644)
	if err != nil {
		metaFile.Close()
		return nil, fmt.Errorf("failed to open partition data file: %w", err)
	}

	// Open index file
	indexFile, err := os.OpenFile(indexPath, os.O_RDWR, 0644)
	if err != nil {
		metaFile.Close()
		dataFile.Close()
		return nil, fmt.Errorf("failed to open partition index file: %w", err)
	}

	return &Partition{
		id:        id,
		path:      partPath,
		meta:      *meta,
		dataFile:  dataFile,
		indexFile: indexFile,
		metaFile:  metaFile,
		blockSize: blockSize,
		numBlocks: meta.NumBlocks,
		closed:    false,
	}, nil
}

// Close closes the partition files.
func (p *Partition) Close() error {
	if p.closed {
		return nil
	}

	var errs []error

	// Write final metadata
	if err := p.writeMeta(); err != nil {
		errs = append(errs, err)
	}

	// Sync and close files
	if p.dataFile != nil {
		if err := p.dataFile.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := p.dataFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if p.indexFile != nil {
		if err := p.indexFile.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := p.indexFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if p.metaFile != nil {
		if err := p.metaFile.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := p.metaFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	p.closed = true

	if len(errs) > 0 {
		return fmt.Errorf("errors closing partition: %v", errs)
	}
	return nil
}

// deletePartition removes a partition directory and all its files.
// This is the O(1) space reclaim operation - just unlink the directory.
func deletePartition(basePath string, id uint32) error {
	partPath := filepath.Join(basePath, partitionDirName(id))
	return os.RemoveAll(partPath)
}

// writeMeta persists the partition metadata to disk.
func (p *Partition) writeMeta() error {
	buf := p.meta.Encode()
	if _, err := p.metaFile.WriteAt(buf, 0); err != nil {
		return err
	}
	return p.metaFile.Sync()
}

// blockOffset calculates the file offset for a given block number.
func (p *Partition) blockOffset(blockNum uint32) int64 {
	return int64(blockNum) * int64(p.blockSize)
}

// indexOffset calculates the file offset for a given index entry.
func (p *Partition) indexOffset(entryNum uint32) int64 {
	return int64(entryNum) * int64(block.IndexEntrySize)
}

// readBlockHeader reads the header of a block.
func (p *Partition) readBlockHeader(blockNum uint32) (*block.BlockHeader, error) {
	buf := make([]byte, block.BlockHeaderSize)
	offset := p.blockOffset(blockNum)

	if _, err := p.dataFile.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	header := &block.BlockHeader{}
	header.Decode(buf)
	return header, nil
}

// writeBlockHeader writes the header of a block.
func (p *Partition) writeBlockHeader(blockNum uint32, header *block.BlockHeader) error {
	buf := make([]byte, block.BlockHeaderSize)
	header.Encode(buf)
	offset := p.blockOffset(blockNum)

	_, err := p.dataFile.WriteAt(buf, offset)
	return err
}

// readIndexEntry reads an index entry.
func (p *Partition) readIndexEntry(entryNum uint32) (*block.IndexEntry, error) {
	buf := make([]byte, block.IndexEntrySize)
	offset := p.indexOffset(entryNum)

	if _, err := p.indexFile.ReadAt(buf, offset); err != nil {
		return nil, err
	}

	entry := &block.IndexEntry{}
	entry.Decode(buf)
	return entry, nil
}

// writeIndexEntry writes an index entry.
func (p *Partition) writeIndexEntry(entryNum uint32, entry *block.IndexEntry) error {
	buf := make([]byte, block.IndexEntrySize)
	entry.Encode(buf)
	offset := p.indexOffset(entryNum)

	_, err := p.indexFile.WriteAt(buf, offset)
	return err
}

// ID returns the partition ID.
func (p *Partition) ID() uint32 {
	return p.id
}

// IsSealed returns whether the partition is sealed (read-only).
func (p *Partition) IsSealed() bool {
	return p.meta.Sealed
}

// Seal marks the partition as sealed (full, read-only).
func (p *Partition) Seal() error {
	p.meta.Sealed = true
	return p.writeMeta()
}

// Stats returns statistics for this partition.
func (p *Partition) Stats() PartitionStats {
	return PartitionStats{
		PartitionID:  p.meta.PartitionID,
		NumBlocks:    p.meta.NumBlocks,
		HeadBlock:    p.meta.HeadBlock,
		WriteOffset:  p.meta.WriteOffset,
		MinTimestamp: p.meta.MinTimestamp,
		MaxTimestamp: p.meta.MaxTimestamp,
		ObjectCount:  p.meta.ObjectCount,
		BytesUsed:    p.meta.BytesUsed,
		Sealed:       p.meta.Sealed,
	}
}

// isEmpty returns true if the partition has no data.
func (p *Partition) isEmpty() bool {
	return p.meta.ObjectCount == 0
}

// isFull returns true if the partition has no more space for a new block.
func (p *Partition) isFull() bool {
	// If head block is at the last block and we've written to it
	return p.meta.HeadBlock >= p.numBlocks-1 && p.meta.WriteOffset > 0
}

// hasSpaceFor returns true if there's space for an object of the given size.
// For spanning objects, verifies enough contiguous blocks remain in the partition.
func (p *Partition) hasSpaceFor(objSize uint32) bool {
	if p.meta.Sealed {
		return false
	}

	// If we haven't started writing to any block yet
	if p.meta.WriteOffset == 0 && p.meta.HeadBlock == 0 {
		// Check first block header
		header, err := p.readBlockHeader(0)
		if err != nil || (header.DataLen == 0 && header.Flags == 0) {
			// Empty partition - check if object fits
			return p.blocksNeededFor(objSize) <= p.numBlocks
		}
	}

	// Check if object fits in current block's remaining space
	if p.meta.WriteOffset > 0 {
		remaining := p.blockSize - p.meta.WriteOffset
		if objSize <= remaining {
			return true
		}
	}

	// Object needs a new block (or spans multiple blocks)
	// Calculate how many blocks we need
	blocksNeeded := p.blocksNeededFor(objSize)

	// Calculate how many blocks are available
	nextBlock := p.meta.HeadBlock + 1
	if p.meta.WriteOffset == 0 {
		nextBlock = p.meta.HeadBlock
	}
	blocksAvailable := p.numBlocks - nextBlock

	return blocksNeeded <= blocksAvailable
}

// blocksNeededFor calculates how many blocks an object of given size requires.
func (p *Partition) blocksNeededFor(objSize uint32) uint32 {
	usablePerBlock := p.blockSize - block.BlockHeaderSize
	firstBlockUsable := usablePerBlock - block.ObjectHeaderSize

	// Small object fits in one block
	if objSize <= firstBlockUsable {
		return 1
	}

	// Spanning object
	remaining := objSize - firstBlockUsable
	return 1 + (remaining+usablePerBlock-1)/usablePerBlock
}

// readObjectHeader reads an object header from the specified offset.
func (p *Partition) readObjectHeader(blockNum uint32, offset uint32) (*block.ObjectHeader, error) {
	buf := make([]byte, block.ObjectHeaderSize)
	fileOffset := p.blockOffset(blockNum) + int64(offset)
	if _, err := p.dataFile.ReadAt(buf, fileOffset); err != nil {
		return nil, err
	}
	header := &block.ObjectHeader{}
	header.Decode(buf)
	return header, nil
}

// writeObjectHeader writes an object header at the specified offset.
func (p *Partition) writeObjectHeader(blockNum uint32, offset uint32, header *block.ObjectHeader) error {
	buf := make([]byte, block.ObjectHeaderSize)
	header.Encode(buf)
	fileOffset := p.blockOffset(blockNum) + int64(offset)
	_, err := p.dataFile.WriteAt(buf, fileOffset)
	return err
}

// activeBlockCount returns the number of blocks with data.
func (p *Partition) activeBlockCount() uint32 {
	if p.meta.ObjectCount == 0 {
		return 0
	}
	// HeadBlock is 0-indexed, so if HeadBlock is N, we have N+1 active blocks
	return p.meta.HeadBlock + 1
}
