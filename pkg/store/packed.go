// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"github.com/tviviano/ts-store/pkg/block"
)

// canFitInCurrentBlock checks if the object can fit in remaining space of head block.
func (s *Store) canFitInCurrentBlock(objSize uint32) bool {
	// If WriteOffset is 0, no packed objects have been written yet
	if s.meta.WriteOffset == 0 {
		return false
	}

	remaining := s.config.DataBlockSize - s.meta.WriteOffset
	return objSize <= remaining
}

// appendToCurrentBlock appends an object to the current head block.
// Called when the object fits in remaining space.
func (s *Store) appendToCurrentBlock(timestamp int64, data []byte) (*ObjectHandle, error) {
	blockNum := s.meta.HeadBlock
	writeOffset := s.meta.WriteOffset

	// Create object header
	objHeader := &block.ObjectHeader{
		Timestamp:  timestamp,
		DataLen:    uint32(len(data)),
		Flags:      block.ObjFlagLastInBlock,
		NextOffset: 0,
	}

	// Update previous object's NextOffset to point to this one
	if err := s.updatePreviousObjectLink(blockNum, writeOffset); err != nil {
		return nil, err
	}

	// Write object header
	if err := s.writeObjectHeader(blockNum, writeOffset, objHeader); err != nil {
		return nil, err
	}

	// Write object data
	dataOffset := writeOffset + block.ObjectHeaderSize
	if len(data) > 0 {
		fileOffset := s.blockOffset(blockNum) + int64(dataOffset)
		if _, err := s.dataFile.WriteAt(data, fileOffset); err != nil {
			return nil, err
		}
	}

	// Update block header DataLen to reflect total used space
	blockHeader, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, err
	}
	blockHeader.DataLen = dataOffset + uint32(len(data)) - block.BlockHeaderSize
	if err := s.writeBlockHeader(blockNum, blockHeader); err != nil {
		return nil, err
	}

	// Update metadata
	s.meta.WriteOffset = dataOffset + uint32(len(data))

	return &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  blockNum,
		Offset:    writeOffset,
		Size:      uint32(len(data)),
		SpanCount: 1,
	}, nil
}

// writeToNewBlock writes an object to a fresh block (single block, not spanning).
func (s *Store) writeToNewBlock(timestamp int64, data []byte) (*ObjectHandle, error) {
	// Allocate a new block
	blockNum, err := s.allocateNextBlock()
	if err != nil {
		return nil, err
	}

	// Initialize block header with packed flag
	blockHeader := &block.BlockHeader{
		Timestamp: timestamp, // First object's timestamp
		BlockNum:  blockNum,
		DataLen:   block.ObjectHeaderSize + uint32(len(data)),
		Flags:     block.FlagPrimary | block.FlagPacked,
	}
	if err := s.writeBlockHeader(blockNum, blockHeader); err != nil {
		return nil, err
	}

	// Write object header at start of data area
	objOffset := uint32(block.BlockHeaderSize)
	objHeader := &block.ObjectHeader{
		Timestamp:  timestamp,
		DataLen:    uint32(len(data)),
		Flags:      block.ObjFlagLastInBlock,
		NextOffset: 0,
	}
	if err := s.writeObjectHeader(blockNum, objOffset, objHeader); err != nil {
		return nil, err
	}

	// Write object data
	dataOffset := objOffset + block.ObjectHeaderSize
	if len(data) > 0 {
		fileOffset := s.blockOffset(blockNum) + int64(dataOffset)
		if _, err := s.dataFile.WriteAt(data, fileOffset); err != nil {
			return nil, err
		}
	}

	// Write index entry pointing to first object
	indexEntry := &block.IndexEntry{
		Timestamp: timestamp,
		BlockNum:  blockNum,
	}
	if err := s.writeIndexEntry(blockNum, indexEntry); err != nil {
		return nil, err
	}

	// Update metadata
	s.meta.HeadBlock = blockNum
	s.meta.WriteOffset = dataOffset + uint32(len(data))

	return &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  blockNum,
		Offset:    objOffset,
		Size:      uint32(len(data)),
		SpanCount: 1,
	}, nil
}

// writeSpanningObject writes an object that spans multiple blocks.
func (s *Store) writeSpanningObject(timestamp int64, data []byte) (*ObjectHandle, error) {
	usablePerBlock := s.config.DataBlockSize - block.BlockHeaderSize
	firstBlockUsable := usablePerBlock - block.ObjectHeaderSize // First block has object header

	// Calculate number of blocks needed
	remaining := uint32(len(data))
	spanCount := uint32(1)
	if remaining > firstBlockUsable {
		remaining -= firstBlockUsable
		spanCount += (remaining + usablePerBlock - 1) / usablePerBlock
	}

	// Debug
	// fmt.Printf("writeSpanningObject: dataLen=%d, firstBlockUsable=%d, usablePerBlock=%d, spanCount=%d\n",
	//     len(data), firstBlockUsable, usablePerBlock, spanCount)

	// Allocate first block
	firstBlock, err := s.allocateNextBlock()
	if err != nil {
		return nil, err
	}
	s.meta.HeadBlock = firstBlock

	prevBlock := firstBlock
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
			BlockNum:  currentBlock,
			DataLen:   block.ObjectHeaderSize + chunkSize,
			Flags:     block.FlagPrimary | block.FlagPacked,
			NextFree:  0, // Will be updated if there are continuation blocks
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
		}

		if err := s.writeBlockHeader(currentBlock, blockHeader); err != nil {
			return nil, err
		}

		if err := s.writeObjectHeader(currentBlock, block.BlockHeaderSize, objHeader); err != nil {
			return nil, err
		}

		// Write data chunk
		fileOffset := s.blockOffset(currentBlock) + int64(block.BlockHeaderSize+block.ObjectHeaderSize)
		if _, err := s.dataFile.WriteAt(data[0:chunkSize], fileOffset); err != nil {
			return nil, err
		}

		// Write index entry
		indexEntry := &block.IndexEntry{
			Timestamp: timestamp,
			BlockNum:  currentBlock,
		}
		if err := s.writeIndexEntry(currentBlock, indexEntry); err != nil {
			return nil, err
		}

		dataPos = chunkSize
		prevBlock = currentBlock
	}

	// Write continuation blocks
	for dataPos < uint32(len(data)) {
		chunkSize := usablePerBlock
		if chunkSize > uint32(len(data))-dataPos {
			chunkSize = uint32(len(data)) - dataPos
		}

		// Allocate next block
		nextBlock, err := s.allocateNextBlock()
		if err != nil {
			return nil, err
		}


		s.meta.HeadBlock = nextBlock

		// Link previous block to this continuation
		prevHeader, err := s.readBlockHeader(prevBlock)
		if err != nil {
			return nil, err
		}
		// Debug: fmt.Printf("DEBUG: before link: prevBlock=%d has DataLen=%d, NextFree=%d\n",
		//     prevBlock, prevHeader.DataLen, prevHeader.NextFree)
		prevHeader.NextFree = nextBlock // Continuation pointer
		if err := s.writeBlockHeader(prevBlock, prevHeader); err != nil {
			return nil, err
		}

		currentBlock = nextBlock

		// Write continuation block header
		contHeader := &block.BlockHeader{
			Timestamp: 0, // Continuation blocks have timestamp 0
			BlockNum:  currentBlock,
			DataLen:   chunkSize,
			Flags:     block.FlagPrimary | block.FlagPacked | block.FlagContinuation,
			NextFree:  0, // Will be updated if more continuations
		}
		// Debug: fmt.Printf("DEBUG: writing contHeader to block %d with DataLen=%d\n", currentBlock, chunkSize)
		if err := s.writeBlockHeader(currentBlock, contHeader); err != nil {
			return nil, err
		}

		// Write data chunk (no object header in continuation)
		fileOffset := s.blockOffset(currentBlock) + int64(block.BlockHeaderSize)
		if _, err := s.dataFile.WriteAt(data[dataPos:dataPos+chunkSize], fileOffset); err != nil {
			return nil, err
		}

		// Index entry for continuation block has timestamp 0
		indexEntry := &block.IndexEntry{
			Timestamp: 0,
			BlockNum:  currentBlock,
		}
		if err := s.writeIndexEntry(currentBlock, indexEntry); err != nil {
			return nil, err
		}

		dataPos += chunkSize
		prevBlock = currentBlock
	}

	// Calculate final write offset
	lastChunkSize := uint32(len(data)) % usablePerBlock
	if lastChunkSize == 0 && len(data) > 0 {
		lastChunkSize = usablePerBlock
	}
	s.meta.WriteOffset = block.BlockHeaderSize + lastChunkSize

	return &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  firstBlock,
		Offset:    block.BlockHeaderSize,
		Size:      uint32(len(data)),
		SpanCount: spanCount,
	}, nil
}

// allocateNextBlock allocates the next block for writing.
// Uses free list if available, otherwise advances head or reclaims oldest.
func (s *Store) allocateNextBlock() (uint32, error) {
	// Check if this is the first insert ever
	// We check both the index entry timestamp AND the block header
	// Continuation blocks have timestamp=0 in index but are not empty
	firstEntry, _ := s.readIndexEntry(s.meta.HeadBlock)
	if firstEntry.Timestamp == 0 {
		// Could be truly empty, or could be a continuation block
		header, _ := s.readBlockHeader(s.meta.HeadBlock)
		if header.DataLen == 0 && header.Flags == 0 {
			// Truly empty block - first insert
			return s.meta.HeadBlock, nil
		}
	}

	// Check if circle is full
	nextHead := (s.meta.HeadBlock + 1) % s.meta.NumBlocks
	if nextHead == s.meta.TailBlock {
		// Circle is full - need to reclaim oldest or use free list
		return s.allocateBlock()
	}

	return nextHead, nil
}

// updatePreviousObjectLink updates the previous object's NextOffset to point to newOffset.
func (s *Store) updatePreviousObjectLink(blockNum uint32, newOffset uint32) error {
	// Find the last object in the block and update its NextOffset
	offset := uint32(block.BlockHeaderSize)

	for offset < s.meta.WriteOffset {
		objHeader, err := s.readObjectHeader(blockNum, offset)
		if err != nil {
			return err
		}

		if objHeader.IsLastInBlock() {
			// This is the last object - update it to point to new object
			objHeader.NextOffset = newOffset
			objHeader.Flags &^= block.ObjFlagLastInBlock // Clear last flag
			return s.writeObjectHeader(blockNum, offset, objHeader)
		}

		if objHeader.NextOffset == 0 {
			break
		}
		offset = objHeader.NextOffset
	}

	return nil
}

// writeObjectHeader writes an object header at the specified offset.
func (s *Store) writeObjectHeader(blockNum uint32, offset uint32, header *block.ObjectHeader) error {
	buf := make([]byte, block.ObjectHeaderSize)
	header.Encode(buf)
	fileOffset := s.blockOffset(blockNum) + int64(offset)
	_, err := s.dataFile.WriteAt(buf, fileOffset)
	return err
}

// readObjectHeader reads an object header from the specified offset.
func (s *Store) readObjectHeader(blockNum uint32, offset uint32) (*block.ObjectHeader, error) {
	buf := make([]byte, block.ObjectHeaderSize)
	fileOffset := s.blockOffset(blockNum) + int64(offset)
	if _, err := s.dataFile.ReadAt(buf, fileOffset); err != nil {
		return nil, err
	}
	header := &block.ObjectHeader{}
	header.Decode(buf)
	return header, nil
}

// readPackedObjectData reads object data from a packed block.
// Handles both single-block and spanning objects.
func (s *Store) readPackedObjectData(blockNum uint32, offset uint32, size uint32, spanCount uint32) ([]byte, error) {
	// Read object header to get flags
	objHeader, err := s.readObjectHeader(blockNum, offset)
	if err != nil {
		return nil, err
	}

	// If not spanning, read directly
	if !objHeader.Continues() {
		data := make([]byte, objHeader.DataLen)
		dataOffset := offset + block.ObjectHeaderSize
		fileOffset := s.blockOffset(blockNum) + int64(dataOffset)
		if _, err := s.dataFile.ReadAt(data, fileOffset); err != nil {
			return nil, err
		}
		return data, nil
	}

	// Spanning object - read from multiple blocks
	data := make([]byte, 0, objHeader.DataLen)
	currentBlock := blockNum
	remaining := objHeader.DataLen
	isFirst := true

	for remaining > 0 {
		blockHeader, err := s.readBlockHeader(currentBlock)
		if err != nil {
			return nil, err
		}

		var readStart uint32
		var chunkSize uint32

		if isFirst {
			// First block - data starts after object header
			readStart = offset + block.ObjectHeaderSize
			// DataLen in first block includes ObjectHeader, so subtract it
			chunkSize = blockHeader.DataLen - block.ObjectHeaderSize
			if chunkSize > remaining {
				chunkSize = remaining
			}
		} else {
			// Continuation block - data starts after block header
			readStart = block.BlockHeaderSize
			chunkSize = blockHeader.DataLen
			if chunkSize > remaining {
				chunkSize = remaining
			}
		}

		// Read chunk
		chunk := make([]byte, chunkSize)
		fileOffset := s.blockOffset(currentBlock) + int64(readStart)
		if _, err := s.dataFile.ReadAt(chunk, fileOffset); err != nil {
			return nil, err
		}
		data = append(data, chunk...)
		remaining -= chunkSize

		// Move to next block if more data
		if remaining > 0 {
			currentBlock = blockHeader.NextFree // Continuation pointer
		}

		isFirst = false
	}

	return data, nil
}

// scanBlockForTimestamp scans a packed block for an object with the given timestamp.
func (s *Store) scanBlockForTimestamp(blockNum uint32, timestamp int64) ([]byte, *ObjectHandle, error) {
	offset := uint32(block.BlockHeaderSize)

	for offset < s.config.DataBlockSize {
		objHeader, err := s.readObjectHeader(blockNum, offset)
		if err != nil {
			return nil, nil, err
		}

		// Check for zero timestamp (end of objects or uninitialized)
		if objHeader.Timestamp == 0 {
			break
		}

		if objHeader.Timestamp == timestamp {
			// Found it - read the data
			data, err := s.readPackedObjectData(blockNum, offset, objHeader.DataLen, 1)
			if err != nil {
				return nil, nil, err
			}

			spanCount := uint32(1)
			if objHeader.Continues() {
				// Calculate span count
				usablePerBlock := s.config.DataBlockSize - block.BlockHeaderSize
				firstBlockUsable := usablePerBlock - block.ObjectHeaderSize
				remaining := objHeader.DataLen
				if remaining > firstBlockUsable {
					remaining -= firstBlockUsable
					spanCount += (remaining + usablePerBlock - 1) / usablePerBlock
				}
			}

			return data, &ObjectHandle{
				Timestamp: timestamp,
				BlockNum:  blockNum,
				Offset:    offset,
				Size:      objHeader.DataLen,
				SpanCount: spanCount,
			}, nil
		}

		if objHeader.Timestamp > timestamp {
			// Passed it (timestamps are ordered)
			return nil, nil, ErrTimestampNotFound
		}

		// Move to next object
		if objHeader.NextOffset == 0 || objHeader.IsLastInBlock() {
			break
		}
		offset = objHeader.NextOffset
	}

	return nil, nil, ErrTimestampNotFound
}

// scanBlockObjects returns all object handles in a packed block.
func (s *Store) scanBlockObjects(blockNum uint32) ([]*ObjectHandle, error) {
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, err
	}

	// Skip continuation blocks
	if header.IsContinuation() {
		return nil, nil
	}

	// V1 format - single object per block
	if !header.IsPacked() {
		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			return nil, err
		}
		if entry.Timestamp == 0 {
			return nil, nil
		}
		return []*ObjectHandle{{
			Timestamp: entry.Timestamp,
			BlockNum:  blockNum,
			Offset:    0,
			Size:      header.DataLen,
			SpanCount: 0,
		}}, nil
	}

	// V2 packed format - scan all objects
	var handles []*ObjectHandle
	offset := uint32(block.BlockHeaderSize)

	for offset < s.config.DataBlockSize {
		objHeader, err := s.readObjectHeader(blockNum, offset)
		if err != nil {
			break
		}

		// Check for end of objects
		if objHeader.Timestamp == 0 {
			break
		}

		spanCount := uint32(1)
		if objHeader.Continues() {
			usablePerBlock := s.config.DataBlockSize - block.BlockHeaderSize
			firstBlockUsable := usablePerBlock - block.ObjectHeaderSize
			remaining := objHeader.DataLen
			if remaining > firstBlockUsable {
				remaining -= firstBlockUsable
				spanCount += (remaining + usablePerBlock - 1) / usablePerBlock
			}
		}

		handles = append(handles, &ObjectHandle{
			Timestamp: objHeader.Timestamp,
			BlockNum:  blockNum,
			Offset:    offset,
			Size:      objHeader.DataLen,
			SpanCount: spanCount,
		})

		// Move to next object
		if objHeader.NextOffset == 0 || objHeader.IsLastInBlock() {
			break
		}
		offset = objHeader.NextOffset
	}

	return handles, nil
}
