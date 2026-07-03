// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"fmt"

	"github.com/tviviano/ts-store/pkg/block"
)

// V2 write methods for partitioned storage.
// These operate within a single partition using append-only writes.

// putObjectV2 stores an object in the V2 partitioned store.
func (s *Store) putObjectV2(timestamp int64, data []byte, schemaVer uint32) (*ObjectHandle, error) {
	objSize := block.ObjectHeaderSize + uint32(len(data))
	usableSpace := s.globalMeta.BlockSize - block.BlockHeaderSize

	// An object can span blocks but never partitions. Reject anything that
	// couldn't fit even in an empty partition, before touching any state.
	if needed := s.currentPartition.blocksNeededFor(objSize); needed > s.currentPartition.numBlocks {
		return nil, fmt.Errorf("%w: object needs %d blocks, partition holds %d",
			ErrObjectTooLarge, needed, s.currentPartition.numBlocks)
	}

	// Check if we need to roll to a new partition
	if !s.currentPartition.hasSpaceFor(objSize) {
		if err := s.rollToNewPartition(); err != nil {
			return nil, err
		}
	}

	var handle *ObjectHandle
	var err error

	// Route to appropriate write strategy
	if s.canFitInCurrentBlockV2(objSize) {
		handle, err = s.appendToCurrentBlockV2(timestamp, data, schemaVer)
	} else if objSize <= usableSpace {
		handle, err = s.writeToNewBlockV2(timestamp, data, schemaVer)
	} else {
		handle, err = s.writeSpanningObjectV2(timestamp, data, schemaVer)
	}

	if err != nil {
		return nil, err
	}

	// Update partition metadata
	p := s.currentPartition
	p.meta.ObjectCount++
	p.meta.BytesUsed += uint64(objSize)
	if p.meta.MinTimestamp == 0 || timestamp < p.meta.MinTimestamp {
		p.meta.MinTimestamp = timestamp
	}
	if timestamp > p.meta.MaxTimestamp {
		p.meta.MaxTimestamp = timestamp
	}

	// Persist metadata
	if err := p.writeMeta(); err != nil {
		return nil, err
	}
	if err := s.writeGlobalMeta(); err != nil {
		return nil, err
	}

	return handle, nil
}

// canFitInCurrentBlockV2 checks if object fits in remaining space of current block.
func (s *Store) canFitInCurrentBlockV2(objSize uint32) bool {
	p := s.currentPartition
	if p.meta.WriteOffset == 0 {
		return false
	}
	remaining := p.blockSize - p.meta.WriteOffset
	return objSize <= remaining
}

// appendToCurrentBlockV2 appends an object to the current head block within the partition.
func (s *Store) appendToCurrentBlockV2(timestamp int64, data []byte, schemaVer uint32) (*ObjectHandle, error) {
	p := s.currentPartition
	blockNum := p.meta.HeadBlock
	writeOffset := p.meta.WriteOffset

	// Create object header. Reserved carries the per-record schema version.
	objHeader := &block.ObjectHeader{
		Timestamp:  timestamp,
		DataLen:    uint32(len(data)),
		Flags:      block.ObjFlagLastInBlock,
		NextOffset: 0,
		Reserved:   schemaVer,
	}

	// Update previous object's NextOffset to point to this one
	if err := s.updatePreviousObjectLinkV2(p, blockNum, writeOffset); err != nil {
		return nil, err
	}

	// Write object header
	if err := p.writeObjectHeader(blockNum, writeOffset, objHeader); err != nil {
		return nil, err
	}

	// Write object data
	dataOffset := writeOffset + block.ObjectHeaderSize
	if len(data) > 0 {
		fileOffset := p.blockOffset(blockNum) + int64(dataOffset)
		if _, err := p.dataFile.WriteAt(data, fileOffset); err != nil {
			return nil, err
		}
	}

	// Update block header DataLen
	blockHeader, err := p.readBlockHeader(blockNum)
	if err != nil {
		return nil, err
	}
	blockHeader.DataLen = dataOffset + uint32(len(data)) - block.BlockHeaderSize
	if err := p.writeBlockHeader(blockNum, blockHeader); err != nil {
		return nil, err
	}

	// Update partition metadata
	p.meta.WriteOffset = dataOffset + uint32(len(data))

	return &ObjectHandle{
		Timestamp:     timestamp,
		BlockNum:      blockNum,
		Offset:        writeOffset,
		Size:          uint32(len(data)),
		SpanCount:     1,
		PartitionID:   p.id,
		SchemaVersion: schemaVer,
	}, nil
}

// writeToNewBlockV2 writes an object to a fresh block within the partition.
func (s *Store) writeToNewBlockV2(timestamp int64, data []byte, schemaVer uint32) (*ObjectHandle, error) {
	p := s.currentPartition

	// Allocate a new block
	blockNum, err := s.allocateNextBlockV2(p)
	if err != nil {
		return nil, err
	}

	// Initialize block header
	blockHeader := &block.BlockHeader{
		Timestamp: timestamp,
		DataLen:   block.ObjectHeaderSize + uint32(len(data)),
		Flags:     block.FlagPrimary | block.FlagPacked,
	}
	if err := p.writeBlockHeader(blockNum, blockHeader); err != nil {
		return nil, err
	}

	// Write object header
	objOffset := uint32(block.BlockHeaderSize)
	objHeader := &block.ObjectHeader{
		Timestamp:  timestamp,
		DataLen:    uint32(len(data)),
		Flags:      block.ObjFlagLastInBlock,
		NextOffset: 0,
		Reserved:   schemaVer,
	}
	if err := p.writeObjectHeader(blockNum, objOffset, objHeader); err != nil {
		return nil, err
	}

	// Write object data
	dataOffset := objOffset + block.ObjectHeaderSize
	if len(data) > 0 {
		fileOffset := p.blockOffset(blockNum) + int64(dataOffset)
		if _, err := p.dataFile.WriteAt(data, fileOffset); err != nil {
			return nil, err
		}
	}

	// Write index entry
	indexEntry := &block.IndexEntry{
		Timestamp: timestamp,
		BlockNum:  blockNum,
	}
	if err := p.writeIndexEntry(blockNum, indexEntry); err != nil {
		return nil, err
	}

	// Update partition metadata
	p.meta.HeadBlock = blockNum
	p.meta.WriteOffset = dataOffset + uint32(len(data))

	return &ObjectHandle{
		Timestamp:     timestamp,
		BlockNum:      blockNum,
		Offset:        objOffset,
		Size:          uint32(len(data)),
		SpanCount:     1,
		PartitionID:   p.id,
		SchemaVersion: schemaVer,
	}, nil
}

// writeSpanningObjectV2 writes an object that spans multiple blocks within the partition.
func (s *Store) writeSpanningObjectV2(timestamp int64, data []byte, schemaVer uint32) (*ObjectHandle, error) {
	p := s.currentPartition
	usablePerBlock := p.blockSize - block.BlockHeaderSize
	firstBlockUsable := usablePerBlock - block.ObjectHeaderSize

	// Calculate number of blocks needed
	remaining := uint32(len(data))
	spanCount := uint32(1)
	if remaining > firstBlockUsable {
		remaining -= firstBlockUsable
		spanCount += (remaining + usablePerBlock - 1) / usablePerBlock
	}

	// Allocate first block
	firstBlock, err := s.allocateNextBlockV2(p)
	if err != nil {
		return nil, err
	}
	p.meta.HeadBlock = firstBlock

	currentBlock := firstBlock
	dataPos := uint32(0)

	// Write first block
	{
		chunkSize := firstBlockUsable
		if chunkSize > uint32(len(data)) {
			chunkSize = uint32(len(data))
		}

		blockHeader := &block.BlockHeader{
			Timestamp: timestamp,
			DataLen:   block.ObjectHeaderSize + chunkSize,
			Flags:     block.FlagPrimary | block.FlagPacked,
		}

		objFlags := uint32(block.ObjFlagLastInBlock)
		if chunkSize < uint32(len(data)) {
			objFlags |= block.ObjFlagContinues
		}

		objHeader := &block.ObjectHeader{
			Timestamp:  timestamp,
			DataLen:    uint32(len(data)), // Total size
			Flags:      objFlags,
			NextOffset: 0,
			Reserved:   schemaVer,
		}

		if err := p.writeBlockHeader(currentBlock, blockHeader); err != nil {
			return nil, err
		}

		if err := p.writeObjectHeader(currentBlock, block.BlockHeaderSize, objHeader); err != nil {
			return nil, err
		}

		// Write data chunk
		fileOffset := p.blockOffset(currentBlock) + int64(block.BlockHeaderSize+block.ObjectHeaderSize)
		if _, err := p.dataFile.WriteAt(data[0:chunkSize], fileOffset); err != nil {
			return nil, err
		}

		// Write index entry
		indexEntry := &block.IndexEntry{
			Timestamp: timestamp,
			BlockNum:  currentBlock,
		}
		if err := p.writeIndexEntry(currentBlock, indexEntry); err != nil {
			return nil, err
		}

		dataPos = chunkSize
	}

	// Write continuation blocks
	for dataPos < uint32(len(data)) {
		chunkSize := usablePerBlock
		if chunkSize > uint32(len(data))-dataPos {
			chunkSize = uint32(len(data)) - dataPos
		}

		// Allocate next block (sequential)
		nextBlock, err := s.allocateNextBlockV2(p)
		if err != nil {
			return nil, err
		}

		p.meta.HeadBlock = nextBlock
		currentBlock = nextBlock

		// Write continuation block header
		contHeader := &block.BlockHeader{
			Timestamp: 0, // Continuation blocks have timestamp 0
			DataLen:   chunkSize,
			Flags:     block.FlagPrimary | block.FlagPacked | block.FlagContinuation,
		}
		if err := p.writeBlockHeader(currentBlock, contHeader); err != nil {
			return nil, err
		}

		// Write data chunk
		fileOffset := p.blockOffset(currentBlock) + int64(block.BlockHeaderSize)
		if _, err := p.dataFile.WriteAt(data[dataPos:dataPos+chunkSize], fileOffset); err != nil {
			return nil, err
		}

		// Index entry for continuation block
		indexEntry := &block.IndexEntry{
			Timestamp: 0,
			BlockNum:  currentBlock,
		}
		if err := p.writeIndexEntry(currentBlock, indexEntry); err != nil {
			return nil, err
		}

		dataPos += chunkSize
	}

	// A continuation block holds raw spanned payload, not an object-header
	// chain, so nothing may ever be packed after it. Mark the head block
	// full so the next object starts a fresh block.
	p.meta.WriteOffset = p.blockSize

	return &ObjectHandle{
		Timestamp:     timestamp,
		BlockNum:      firstBlock,
		Offset:        block.BlockHeaderSize,
		Size:          uint32(len(data)),
		SpanCount:     spanCount,
		PartitionID:   p.id,
		SchemaVersion: schemaVer,
	}, nil
}

// allocateNextBlockV2 allocates the next block within a partition.
// Returns error if partition is full (caller should roll to new partition).
func (s *Store) allocateNextBlockV2(p *Partition) (uint32, error) {
	// Check if this is the first block ever
	if p.meta.WriteOffset == 0 && p.meta.HeadBlock == 0 {
		header, err := p.readBlockHeader(0)
		if err != nil || (header.DataLen == 0 && header.Flags == 0) {
			return 0, nil // First block
		}
	}

	// Check if partition is full
	nextBlock := p.meta.HeadBlock + 1
	if nextBlock >= p.numBlocks {
		return 0, ErrPartitionFull
	}

	return nextBlock, nil
}

// updatePreviousObjectLinkV2 updates the previous object's NextOffset.
func (s *Store) updatePreviousObjectLinkV2(p *Partition, blockNum uint32, newOffset uint32) error {
	offset := uint32(block.BlockHeaderSize)

	for offset < p.meta.WriteOffset {
		objHeader, err := p.readObjectHeader(blockNum, offset)
		if err != nil {
			return err
		}

		if objHeader.IsLastInBlock() {
			objHeader.NextOffset = newOffset
			objHeader.Flags &^= block.ObjFlagLastInBlock
			return p.writeObjectHeader(blockNum, offset, objHeader)
		}

		if objHeader.NextOffset == 0 {
			break
		}
		offset = objHeader.NextOffset
	}

	return nil
}

// rollToNewPartition seals the current partition and creates a new one.
// If all partition slots are used, deletes the oldest partition first.
//
// This uses a phase-based approach for crash safety. The global metadata
// write is the atomic commit point. By writing the phase before each
// non-atomic filesystem operation, recovery knows what to clean up.
func (s *Store) rollToNewPartition() error {
	// Seal current partition if it exists and has data (idempotent)
	if s.currentPartition != nil && !s.currentPartition.isEmpty() {
		if err := s.currentPartition.Seal(); err != nil {
			return err
		}
	}

	// Find next partition ID
	nextID := (s.globalMeta.CurrentPartition + 1) % s.globalMeta.NumPartitions

	// Phase 1: Delete oldest partition if needed
	if s.globalMeta.ActiveCount >= uint8(s.globalMeta.NumPartitions) {
		oldestID := uint32(s.globalMeta.PartitionOrder[0])

		// Remove from in-memory tracking and commit Phase 1 BEFORE deleting
		oldestPart := s.partitions[oldestID]
		if oldestPart != nil {
			if err := oldestPart.Close(); err != nil {
				return err
			}
		}
		s.partitions[oldestID] = nil
		copy(s.globalMeta.PartitionOrder[:], s.globalMeta.PartitionOrder[1:])
		s.globalMeta.PartitionOrder[s.globalMeta.ActiveCount-1] = 0
		s.globalMeta.ActiveCount--

		// Commit Phase 1: tells recovery to delete this partition directory
		s.globalMeta.RolloverPhase = RolloverPhaseDeleting
		s.globalMeta.RolloverTarget = oldestID
		if err := s.writeGlobalMeta(); err != nil {
			return err
		}

		// Now safe to delete the directory
		if err := deletePartition(s.path, oldestID); err != nil {
			return err
		}
	}

	// Commit Phase 2: tells recovery to clean up partial dir and create partition
	s.globalMeta.RolloverPhase = RolloverPhaseCreating
	s.globalMeta.RolloverTarget = nextID
	if err := s.writeGlobalMeta(); err != nil {
		return err
	}

	// Remove any orphaned directory at nextID before creating fresh
	deletePartition(s.path, nextID)

	// Create new partition
	newPart, err := createPartition(s.path, nextID, s.globalMeta.BlocksPerPart, s.globalMeta.BlockSize)
	if err != nil {
		return err
	}

	// Finalize: add to tracking and commit Phase 0 (idle)
	s.partitions[nextID] = newPart
	s.currentPartition = newPart
	s.globalMeta.CurrentPartition = nextID
	s.globalMeta.PartitionOrder[s.globalMeta.ActiveCount] = uint8(nextID)
	s.globalMeta.ActiveCount++
	if s.globalMeta.ActiveCount == 1 {
		s.globalMeta.OldestPartition = nextID
	}
	s.globalMeta.RolloverPhase = RolloverPhaseIdle
	s.globalMeta.RolloverTarget = 0
	return s.writeGlobalMeta()
}

// writeGlobalMeta persists the global metadata to disk.
func (s *Store) writeGlobalMeta() error {
	buf := s.globalMeta.Encode()
	if _, err := s.metaFile.WriteAt(buf, 0); err != nil {
		return err
	}
	return s.metaFile.Sync()
}
