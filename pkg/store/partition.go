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

	// putsSinceMetaSync counts hot-path meta writes since the last fsync
	// (see writeMetaAsync). Guarded by the store mutex like meta itself.
	putsSinceMetaSync int
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

// openPartition opens an existing partition. expectedBlocks is the
// per-partition block count from the store's global metadata; the
// partition's own metadata must agree with it and with the directory's
// partition ID, otherwise a corrupt meta file would be trusted as-is and
// garbage HeadBlock/NumBlocks values surface later as confusing EOF
// errors deep in the read/write paths instead of a clear diagnosis here.
func openPartition(basePath string, id uint32, blockSize uint32, expectedBlocks uint32) (*Partition, error) {
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

	// Validate the decoded metadata before trusting any of its fields.
	if err := validatePartitionMeta(meta, id, expectedBlocks); err != nil {
		metaFile.Close()
		return nil, err
	}

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

// validatePartitionMeta rejects partition metadata that contradicts the
// directory it was read from or the store's global configuration.
func validatePartitionMeta(meta *PartitionMetadata, id uint32, expectedBlocks uint32) error {
	if meta.PartitionID != id {
		return fmt.Errorf("partition %d metadata corrupt: partition_id says %d (directory and metadata disagree)", id, meta.PartitionID)
	}
	if meta.NumBlocks != expectedBlocks {
		return fmt.Errorf("partition %d metadata corrupt: num_blocks is %d, global metadata says %d blocks per partition", id, meta.NumBlocks, expectedBlocks)
	}
	if meta.HeadBlock >= meta.NumBlocks {
		return fmt.Errorf("partition %d metadata corrupt: head_block %d out of range (partition has %d blocks)", id, meta.HeadBlock, meta.NumBlocks)
	}
	return nil
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

// metaSyncEvery is how many hot-path meta writes may elapse between fsyncs.
const metaSyncEvery = 64

// writeMetaAsync persists the partition metadata without forcing it to disk
// on every call, fsyncing every metaSyncEvery writes. Used on the per-put
// hot path: recovery re-derives HeadBlock/WriteOffset/timestamp bounds from
// block contents on open, so stale-on-crash meta costs a recovery pass, not
// correctness — while an fsync per put capped throughput at fsync latency
// and wore SD cards. Clean shutdown still syncs (Close/Seal call writeMeta).
func (p *Partition) writeMetaAsync() error {
	buf := p.meta.Encode()
	if _, err := p.metaFile.WriteAt(buf, 0); err != nil {
		return err
	}
	p.putsSinceMetaSync++
	if p.putsSinceMetaSync >= metaSyncEvery {
		p.putsSinceMetaSync = 0
		return p.metaFile.Sync()
	}
	return nil
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

// writeBlockHeaderChecksummed stamps a CRC32C of the block's used data
// area into the header, then writes the header. The payload is read
// back from disk because packed blocks mutate in place (appends rewrite
// the previous object's link), so the caller's in-memory bytes never
// cover the whole area. Called on the put paths AFTER the object bytes
// land, so the read-back sees the final payload (issue #58).
func (p *Partition) writeBlockHeaderChecksummed(blockNum uint32, header *block.BlockHeader) error {
	payload, err := p.readBlockPayload(blockNum, header)
	if err != nil {
		return err
	}
	header.SetChecksum(block.PayloadChecksum(payload))
	return p.writeBlockHeader(blockNum, header)
}

// readBlockPayload reads the block's used data area
// [BlockHeaderSize, BlockHeaderSize+DataLen), bounds-checking DataLen so
// a corrupted length can't drive a huge allocation or out-of-block read.
func (p *Partition) readBlockPayload(blockNum uint32, header *block.BlockHeader) ([]byte, error) {
	if header.DataLen > p.blockSize-block.BlockHeaderSize {
		return nil, fmt.Errorf("%w: block %d data length %d exceeds block size", ErrCorruptBlock, blockNum, header.DataLen)
	}
	payload := make([]byte, header.DataLen)
	if header.DataLen == 0 {
		return payload, nil
	}
	if _, err := p.dataFile.ReadAt(payload, p.blockOffset(blockNum)+block.BlockHeaderSize); err != nil {
		return nil, err
	}
	return payload, nil
}

// verifiedBlockPayload returns the block's used data area, verified
// against the header's checksum when one is present. Legacy blocks
// (no checksum stamped) are returned unverified — pre-#58 data keeps
// reading without migration.
func (p *Partition) verifiedBlockPayload(blockNum uint32, header *block.BlockHeader) ([]byte, error) {
	payload, err := p.readBlockPayload(blockNum, header)
	if err != nil {
		return nil, err
	}
	if header.HasChecksum() {
		if got := block.PayloadChecksum(payload); got != header.Checksum() {
			return nil, fmt.Errorf("%w: block %d payload crc %08x, header says %08x", ErrCorruptBlock, blockNum, got, header.Checksum())
		}
	}
	return payload, nil
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

	// Calculate how many blocks are available. This must mirror
	// allocateNextBlockV2: once anything has been written to the head block,
	// the next allocation is HeadBlock+1 (the empty-partition case is
	// handled above).
	nextBlock := p.meta.HeadBlock + 1
	if nextBlock > p.numBlocks {
		return false
	}
	blocksAvailable := p.numBlocks - nextBlock

	return blocksNeeded <= blocksAvailable
}

// blocksNeededFor calculates how many blocks an object of given size requires.
// objSize includes the object header. The arithmetic mirrors
// writeSpanningObjectV2: the first block holds usable-minus-object-header
// payload bytes, each continuation block holds a full usable payload.
func (p *Partition) blocksNeededFor(objSize uint32) uint32 {
	usablePerBlock := p.blockSize - block.BlockHeaderSize

	// Header plus data fit in one block
	if objSize <= usablePerBlock {
		return 1
	}

	// Spanning object: objSize - usablePerBlock is exactly the payload that
	// overflows the first block (header accounted for once).
	remaining := objSize - usablePerBlock
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
