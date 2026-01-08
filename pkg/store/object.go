// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/tviviano/ts-store/pkg/block"
)

var (
	ErrObjectNotFound   = errors.New("object not found")
	ErrObjectCorrupted  = errors.New("object data corrupted")
	ErrObjectTooLarge   = errors.New("object exceeds maximum size")
)

// ObjectHandle identifies a stored object.
type ObjectHandle struct {
	Timestamp       int64  `json:"timestamp"`
	PrimaryBlockNum uint32 `json:"primary_block_num"`
	TotalSize       uint32 `json:"total_size"`
	BlockCount      uint32 `json:"block_count"` // Primary + attached
}

// objectHeader is stored at the beginning of the primary block's data.
// It describes the full object spanning multiple blocks.
// Size: 16 bytes
type objectHeader struct {
	Magic      uint32 // 0x4F424A31 = "OBJ1"
	TotalSize  uint32 // Total size of object data (excluding headers)
	BlockCount uint32 // Total number of blocks (primary + attached)
	Checksum   uint32 // Simple checksum of object data
}

const (
	objectMagic      = 0x4F424A31 // "OBJ1"
	objectHeaderSize = 16
)

// PutObject stores an arbitrary-sized object at the given timestamp.
// The object is automatically split across multiple blocks if needed.
// Returns a handle that can be used to retrieve or delete the object.
func (s *Store) PutObject(timestamp int64, data []byte) (*ObjectHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if timestamp <= 0 {
		return nil, ErrInvalidTimestamp
	}

	// Calculate usable space per block
	usablePerBlock := s.config.DataBlockSize - block.BlockHeaderSize
	usableInPrimary := usablePerBlock - objectHeaderSize // Primary has object header

	// Calculate blocks needed
	dataLen := uint32(len(data))
	blocksNeeded := uint32(1) // At least primary

	if dataLen > usableInPrimary {
		remaining := dataLen - usableInPrimary
		additionalBlocks := (remaining + usablePerBlock - 1) / usablePerBlock
		blocksNeeded += additionalBlocks
	}

	// Calculate checksum
	checksum := calculateChecksum(data)

	// Create object header
	header := objectHeader{
		Magic:      objectMagic,
		TotalSize:  dataLen,
		BlockCount: blocksNeeded,
		Checksum:   checksum,
	}

	// Prepare primary block data (header + first chunk)
	primaryData := make([]byte, objectHeaderSize)
	encodeObjectHeader(&header, primaryData)

	firstChunkSize := usableInPrimary
	if dataLen < usableInPrimary {
		firstChunkSize = dataLen
	}
	primaryData = append(primaryData, data[:firstChunkSize]...)

	// Insert primary block
	primaryBlockNum, err := s.insertLocked(timestamp, primaryData)
	if err != nil {
		return nil, err
	}

	// Store remaining data in attached blocks
	offset := firstChunkSize
	for i := uint32(1); i < blocksNeeded; i++ {
		// Calculate chunk size for this block
		remaining := dataLen - offset
		chunkSize := usablePerBlock
		if remaining < usablePerBlock {
			chunkSize = remaining
		}

		// Get chunk data
		chunk := data[offset : offset+chunkSize]

		// Attach and write block
		attachedBlockNum, err := s.attachBlockLocked(primaryBlockNum)
		if err != nil {
			// Rollback: try to reclaim what we've allocated
			// In a production system, this would need better handling
			return nil, err
		}

		if err := s.writeBlockDataLocked(attachedBlockNum, chunk); err != nil {
			return nil, err
		}

		offset += chunkSize
	}

	// Persist metadata
	if err := s.writeMetaLocked(); err != nil {
		return nil, err
	}

	return &ObjectHandle{
		Timestamp:       timestamp,
		PrimaryBlockNum: primaryBlockNum,
		TotalSize:       dataLen,
		BlockCount:      blocksNeeded,
	}, nil
}

// PutObjectNow stores an object with the current timestamp.
func (s *Store) PutObjectNow(data []byte) (*ObjectHandle, error) {
	return s.PutObject(time.Now().UnixNano(), data)
}

// GetObject retrieves a complete object by its handle.
// The data is reassembled from all blocks.
func (s *Store) GetObject(handle *ObjectHandle) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	return s.getObjectLocked(handle.PrimaryBlockNum)
}

// GetObjectByTime retrieves an object by its timestamp.
func (s *Store) GetObjectByTime(timestamp int64) ([]byte, *ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	// Find block by timestamp
	blockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return nil, nil, err
	}

	// Verify exact match
	entry, err := s.readIndexEntry(blockNum)
	if err != nil {
		return nil, nil, err
	}
	if entry.Timestamp != timestamp {
		return nil, nil, ErrTimestampNotFound
	}

	data, err := s.getObjectLocked(blockNum)
	if err != nil {
		return nil, nil, err
	}

	// Build handle from block data
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, nil, err
	}

	handle := &ObjectHandle{
		Timestamp:       timestamp,
		PrimaryBlockNum: blockNum,
		TotalSize:       uint32(len(data)),
		BlockCount:      1 + header.AttachedCount,
	}

	return data, handle, nil
}

// GetObjectByBlock retrieves an object by its primary block number.
func (s *Store) GetObjectByBlock(primaryBlockNum uint32) ([]byte, *ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	if primaryBlockNum >= s.meta.NumBlocks {
		return nil, nil, ErrBlockOutOfRange
	}

	data, err := s.getObjectLocked(primaryBlockNum)
	if err != nil {
		return nil, nil, err
	}

	// Build handle
	header, err := s.readBlockHeader(primaryBlockNum)
	if err != nil {
		return nil, nil, err
	}

	handle := &ObjectHandle{
		Timestamp:       header.Timestamp,
		PrimaryBlockNum: primaryBlockNum,
		TotalSize:       uint32(len(data)),
		BlockCount:      1 + header.AttachedCount,
	}

	return data, handle, nil
}

// ListObjectResult contains an object handle without the data.
type ListObjectResult struct {
	Handle *ObjectHandle
}

// GetOldestObjects returns the N oldest objects (from tail).
// Returns handles only, not data. Use GetObject to retrieve data.
func (s *Store) GetOldestObjects(limit int) ([]*ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	if limit <= 0 || uint32(limit) > count {
		limit = int(count)
	}

	handles := make([]*ObjectHandle, 0, limit)

	// Start from tail (oldest) and walk forward
	for i := 0; i < limit; i++ {
		blockNum := s.blockNumFromOffset(uint32(i))

		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			return nil, err
		}

		// Skip empty entries
		if entry.Timestamp == 0 {
			continue
		}

		header, err := s.readBlockHeader(blockNum)
		if err != nil {
			return nil, err
		}

		handles = append(handles, &ObjectHandle{
			Timestamp:       entry.Timestamp,
			PrimaryBlockNum: blockNum,
			TotalSize:       header.DataLen, // Note: this is block data len, not total object size
			BlockCount:      1 + header.AttachedCount,
		})
	}

	return handles, nil
}

// GetNewestObjects returns the N newest objects (from head).
// Returns handles only, not data. Use GetObject to retrieve data.
func (s *Store) GetNewestObjects(limit int) ([]*ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	if limit <= 0 || uint32(limit) > count {
		limit = int(count)
	}

	handles := make([]*ObjectHandle, 0, limit)

	// Start from head (newest) and walk backward
	for i := 0; i < limit; i++ {
		offset := int(count) - 1 - i
		if offset < 0 {
			break
		}

		blockNum := s.blockNumFromOffset(uint32(offset))

		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			return nil, err
		}

		// Skip empty entries
		if entry.Timestamp == 0 {
			continue
		}

		header, err := s.readBlockHeader(blockNum)
		if err != nil {
			return nil, err
		}

		handles = append(handles, &ObjectHandle{
			Timestamp:       entry.Timestamp,
			PrimaryBlockNum: blockNum,
			TotalSize:       header.DataLen,
			BlockCount:      1 + header.AttachedCount,
		})
	}

	return handles, nil
}

// GetObjectsInRange returns objects with timestamps in [startTime, endTime].
// Returns handles only, not data.
func (s *Store) GetObjectsInRange(startTime, endTime int64, limit int) ([]*ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if startTime > endTime {
		return nil, ErrInvalidTimestamp
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	// Find start position
	startOffset := s.findOffsetForTimeLocked(startTime, 0, count-1, true)

	// Find end position
	endOffset := s.findOffsetForTimeLocked(endTime, 0, count-1, false)

	if startOffset > endOffset {
		return nil, nil
	}

	// Limit results
	resultCount := int(endOffset - startOffset + 1)
	if limit > 0 && resultCount > limit {
		resultCount = limit
	}

	handles := make([]*ObjectHandle, 0, resultCount)

	for offset := startOffset; offset <= endOffset && len(handles) < resultCount; offset++ {
		blockNum := s.blockNumFromOffset(offset)

		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			return nil, err
		}

		// Verify within time range
		if entry.Timestamp < startTime || entry.Timestamp > endTime {
			continue
		}

		header, err := s.readBlockHeader(blockNum)
		if err != nil {
			return nil, err
		}

		handles = append(handles, &ObjectHandle{
			Timestamp:       entry.Timestamp,
			PrimaryBlockNum: blockNum,
			TotalSize:       header.DataLen,
			BlockCount:      1 + header.AttachedCount,
		})
	}

	return handles, nil
}

// DeleteObject removes an object and all its blocks.
func (s *Store) DeleteObject(handle *ObjectHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	return s.deleteObjectLocked(handle.PrimaryBlockNum)
}

// DeleteObjectByTime removes an object by its timestamp.
func (s *Store) DeleteObjectByTime(timestamp int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	// Find block by timestamp
	blockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return err
	}

	// Verify exact match
	entry, err := s.readIndexEntry(blockNum)
	if err != nil {
		return err
	}
	if entry.Timestamp != timestamp {
		return ErrTimestampNotFound
	}

	return s.deleteObjectLocked(blockNum)
}

// getObjectLocked reads and reassembles an object from blocks.
// Lock must be held.
func (s *Store) getObjectLocked(primaryBlockNum uint32) ([]byte, error) {
	// Read primary block
	primaryData, err := s.readBlockDataLocked(primaryBlockNum)
	if err != nil {
		return nil, err
	}

	if len(primaryData) < objectHeaderSize {
		return nil, ErrObjectCorrupted
	}

	// Parse object header
	var header objectHeader
	decodeObjectHeader(primaryData[:objectHeaderSize], &header)

	if header.Magic != objectMagic {
		// Not an object-format block, return raw data
		return primaryData, nil
	}

	// Allocate buffer for full object
	result := make([]byte, 0, header.TotalSize)

	// Add data from primary block (after header)
	result = append(result, primaryData[objectHeaderSize:]...)

	// Read attached blocks if needed
	if header.BlockCount > 1 {
		blockHeader, err := s.readBlockHeader(primaryBlockNum)
		if err != nil {
			return nil, err
		}

		// Walk the attached block chain
		currentBlock := blockHeader.FirstAttached
		for i := uint32(1); i < header.BlockCount; i++ {
			attachedData, err := s.readBlockDataLocked(currentBlock)
			if err != nil {
				return nil, err
			}
			result = append(result, attachedData...)

			// Get next attached block
			attachedHeader, err := s.readBlockHeader(currentBlock)
			if err != nil {
				return nil, err
			}
			currentBlock = attachedHeader.NextFree // NextFree used as next-attached link
		}
	}

	// Trim to actual size (in case blocks had padding)
	if uint32(len(result)) > header.TotalSize {
		result = result[:header.TotalSize]
	}

	// Verify checksum
	if calculateChecksum(result) != header.Checksum {
		return nil, ErrObjectCorrupted
	}

	return result, nil
}

// deleteObjectLocked removes an object and reclaims its blocks.
// Lock must be held.
func (s *Store) deleteObjectLocked(primaryBlockNum uint32) error {
	// Use the existing reclaim mechanism which handles attached blocks
	if err := s.reclaimBlock(primaryBlockNum); err != nil {
		return err
	}

	s.adjustTailAfterReclaim()
	return s.writeMetaLocked()
}

// insertLocked is the internal insert without lock acquisition.
func (s *Store) insertLocked(timestamp int64, data []byte) (uint32, error) {
	if timestamp <= 0 {
		return 0, ErrInvalidTimestamp
	}

	maxData := s.config.DataBlockSize - block.BlockHeaderSize
	if uint32(len(data)) > maxData {
		return 0, ErrBlockOutOfRange
	}

	var blockNum uint32
	var err error

	firstEntry, _ := s.readIndexEntry(s.meta.HeadBlock)
	isFirstInsert := firstEntry.Timestamp == 0

	if isFirstInsert {
		blockNum = s.meta.HeadBlock
	} else {
		nextHead := (s.meta.HeadBlock + 1) % s.meta.NumBlocks
		if nextHead == s.meta.TailBlock {
			blockNum, err = s.allocateBlock()
			if err != nil {
				return 0, err
			}
		} else {
			blockNum = nextHead
		}
		s.meta.HeadBlock = blockNum
	}

	header := &block.BlockHeader{
		Timestamp:     timestamp,
		BlockNum:      blockNum,
		AttachedCount: 0,
		FirstAttached: 0,
		LastAttached:  0,
		DataLen:       uint32(len(data)),
		Flags:         block.FlagPrimary,
		NextFree:      0,
	}

	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return 0, err
	}

	if len(data) > 0 {
		offset := s.blockOffset(blockNum) + block.BlockHeaderSize
		if _, err := s.dataFile.WriteAt(data, offset); err != nil {
			return 0, err
		}
	}

	entry := &block.IndexEntry{
		Timestamp:     timestamp,
		BlockNum:      blockNum,
		AttachedCount: 0,
		FirstAttached: 0,
	}
	if err := s.writeIndexEntry(blockNum, entry); err != nil {
		return 0, err
	}

	return blockNum, nil
}

// attachBlockLocked attaches a block without lock acquisition.
func (s *Store) attachBlockLocked(primaryBlockNum uint32) (uint32, error) {
	if primaryBlockNum >= s.meta.NumBlocks {
		return 0, ErrBlockOutOfRange
	}

	primaryHeader, err := s.readBlockHeader(primaryBlockNum)
	if err != nil {
		return 0, err
	}

	attachedBlockNum, err := s.allocateAttachedBlock()
	if err != nil {
		return 0, err
	}

	attachedHeader := &block.BlockHeader{
		Timestamp:     primaryHeader.Timestamp,
		BlockNum:      attachedBlockNum,
		AttachedCount: 0,
		FirstAttached: 0,
		LastAttached:  0,
		DataLen:       0,
		Flags:         block.FlagAttached,
		NextFree:      0,
	}

	if primaryHeader.AttachedCount == 0 {
		primaryHeader.FirstAttached = attachedBlockNum
		primaryHeader.LastAttached = attachedBlockNum
	} else {
		lastAttached, err := s.readBlockHeader(primaryHeader.LastAttached)
		if err != nil {
			return 0, err
		}
		lastAttached.NextFree = attachedBlockNum
		if err := s.writeBlockHeader(primaryHeader.LastAttached, lastAttached); err != nil {
			return 0, err
		}
		primaryHeader.LastAttached = attachedBlockNum
	}

	primaryHeader.AttachedCount++

	if err := s.writeBlockHeader(attachedBlockNum, attachedHeader); err != nil {
		return 0, err
	}
	if err := s.writeBlockHeader(primaryBlockNum, primaryHeader); err != nil {
		return 0, err
	}

	entry, err := s.readIndexEntry(primaryBlockNum)
	if err != nil {
		return 0, err
	}
	entry.AttachedCount = primaryHeader.AttachedCount
	if entry.FirstAttached == 0 {
		entry.FirstAttached = attachedBlockNum
	}
	if err := s.writeIndexEntry(primaryBlockNum, entry); err != nil {
		return 0, err
	}

	s.meta.TotalAttached++

	return attachedBlockNum, nil
}

// writeBlockDataLocked writes data to a block without lock acquisition.
func (s *Store) writeBlockDataLocked(blockNum uint32, data []byte) error {
	maxData := s.config.DataBlockSize - block.BlockHeaderSize
	if uint32(len(data)) > maxData {
		return ErrBlockOutOfRange
	}

	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return err
	}
	header.DataLen = uint32(len(data))
	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return err
	}

	offset := s.blockOffset(blockNum) + block.BlockHeaderSize
	_, err = s.dataFile.WriteAt(data, offset)
	return err
}

// readBlockDataLocked reads block data without lock acquisition.
func (s *Store) readBlockDataLocked(blockNum uint32) ([]byte, error) {
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, err
	}

	if header.DataLen == 0 {
		return nil, nil
	}

	data := make([]byte, header.DataLen)
	offset := s.blockOffset(blockNum) + block.BlockHeaderSize

	if _, err := s.dataFile.ReadAt(data, offset); err != nil {
		return nil, err
	}

	return data, nil
}

// encodeObjectHeader serializes an object header.
func encodeObjectHeader(h *objectHeader, buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:4], h.Magic)
	binary.LittleEndian.PutUint32(buf[4:8], h.TotalSize)
	binary.LittleEndian.PutUint32(buf[8:12], h.BlockCount)
	binary.LittleEndian.PutUint32(buf[12:16], h.Checksum)
}

// decodeObjectHeader deserializes an object header.
func decodeObjectHeader(buf []byte, h *objectHeader) {
	h.Magic = binary.LittleEndian.Uint32(buf[0:4])
	h.TotalSize = binary.LittleEndian.Uint32(buf[4:8])
	h.BlockCount = binary.LittleEndian.Uint32(buf[8:12])
	h.Checksum = binary.LittleEndian.Uint32(buf[12:16])
}

// calculateChecksum computes a simple checksum of data.
func calculateChecksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		sum = sum*31 + uint32(b)
	}
	return sum
}
