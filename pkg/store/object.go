// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"errors"
	"time"

	"github.com/tviviano/ts-store/pkg/block"
)

var (
	ErrObjectNotFound  = errors.New("object not found")
	ErrObjectTooLarge  = errors.New("object exceeds maximum block size")
)

// ObjectHandle identifies a stored object.
type ObjectHandle struct {
	Timestamp int64  `json:"timestamp"`
	BlockNum  uint32 `json:"block_num"`
	Offset    uint32 `json:"offset,omitempty"`     // Position within block (0 for V1 format)
	Size      uint32 `json:"size"`
	SpanCount uint32 `json:"span_count,omitempty"` // Number of blocks (1 = single block, 0 = legacy)
}

// MaxObjectSize returns the maximum object size for this store.
func (s *Store) MaxObjectSize() uint32 {
	return s.config.DataBlockSize - block.BlockHeaderSize
}

// PutObject stores an object at the given timestamp.
// Objects are packed into blocks for efficiency. Large objects span multiple blocks.
func (s *Store) PutObject(timestamp int64, data []byte) (*ObjectHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if timestamp <= 0 {
		return nil, ErrInvalidTimestamp
	}

	// Validate timestamp is monotonically increasing
	if newestTs, tsErr := s.getNewestTimestampLocked(); tsErr == nil && timestamp <= newestTs {
		return nil, ErrTimestampOutOfOrder
	}
	// ErrEmptyStore is OK - first insert

	objSize := block.ObjectHeaderSize + uint32(len(data))
	usableSpace := s.config.DataBlockSize - block.BlockHeaderSize

	var handle *ObjectHandle
	var err error

	// Case 1: Fits in remaining space of current block
	if s.canFitInCurrentBlock(objSize) {
		handle, err = s.appendToCurrentBlock(timestamp, data)
	} else if objSize <= usableSpace {
		// Case 2: Fits in a single new block
		handle, err = s.writeToNewBlock(timestamp, data)
	} else {
		// Case 3: Spans multiple blocks
		handle, err = s.writeSpanningObject(timestamp, data)
	}

	if err != nil {
		return nil, err
	}

	if err := s.writeMetaLocked(); err != nil {
		return nil, err
	}

	return handle, nil
}

// PutObjectNow stores an object with the current timestamp.
func (s *Store) PutObjectNow(data []byte) (*ObjectHandle, error) {
	return s.PutObject(time.Now().UnixNano(), data)
}

// GetObject retrieves an object by its handle.
func (s *Store) GetObject(handle *ObjectHandle) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	// Check if this is a V2 packed handle (has Offset set)
	if handle.Offset > 0 {
		return s.readPackedObjectData(handle.BlockNum, handle.Offset, handle.Size, handle.SpanCount)
	}

	// V1 legacy format - single object per block
	return s.readBlockDataLocked(handle.BlockNum)
}

// GetObjectByTime retrieves an object by its timestamp.
func (s *Store) GetObjectByTime(timestamp int64) ([]byte, *ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	// Find block by timestamp (binary search finds block with first object <= timestamp)
	blockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return nil, nil, err
	}

	// Check if this is a packed block
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, nil, err
	}

	if header.IsPacked() {
		// V2 packed format - scan for exact timestamp
		return s.scanBlockForTimestamp(blockNum, timestamp)
	}

	// V1 legacy format - verify exact match
	entry, err := s.readIndexEntry(blockNum)
	if err != nil {
		return nil, nil, err
	}
	if entry.Timestamp != timestamp {
		return nil, nil, ErrTimestampNotFound
	}

	data, err := s.readBlockDataLocked(blockNum)
	if err != nil {
		return nil, nil, err
	}

	return data, &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  blockNum,
		Size:      uint32(len(data)),
	}, nil
}

// GetObjectByBlock retrieves the first object in a block by block number.
// For packed blocks with multiple objects, use GetObjectsByBlock.
func (s *Store) GetObjectByBlock(blockNum uint32) ([]byte, *ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	if blockNum >= s.meta.NumBlocks {
		return nil, nil, ErrBlockOutOfRange
	}

	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return nil, nil, err
	}

	if header.IsPacked() {
		// V2 packed format - return first object
		objHeader, err := s.readObjectHeader(blockNum, block.BlockHeaderSize)
		if err != nil {
			return nil, nil, err
		}

		data, err := s.readPackedObjectData(blockNum, block.BlockHeaderSize, objHeader.DataLen, 1)
		if err != nil {
			return nil, nil, err
		}

		return data, &ObjectHandle{
			Timestamp: objHeader.Timestamp,
			BlockNum:  blockNum,
			Offset:    block.BlockHeaderSize,
			Size:      objHeader.DataLen,
			SpanCount: 1,
		}, nil
	}

	// V1 legacy format
	data, err := s.readBlockDataLocked(blockNum)
	if err != nil {
		return nil, nil, err
	}

	return data, &ObjectHandle{
		Timestamp: header.Timestamp,
		BlockNum:  blockNum,
		Size:      uint32(len(data)),
	}, nil
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

	handles := make([]*ObjectHandle, 0)

	// Start from tail (oldest) and walk forward through blocks
	for i := uint32(0); i < count && (limit <= 0 || len(handles) < limit); i++ {
		blockNum := s.blockNumFromOffset(i)

		// Get all objects in this block
		blockHandles, err := s.scanBlockObjects(blockNum)
		if err != nil {
			continue
		}

		for _, h := range blockHandles {
			handles = append(handles, h)
			if limit > 0 && len(handles) >= limit {
				break
			}
		}
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

	// First collect all objects from all blocks (in order)
	var allHandles []*ObjectHandle
	for i := uint32(0); i < count; i++ {
		blockNum := s.blockNumFromOffset(i)
		blockHandles, err := s.scanBlockObjects(blockNum)
		if err != nil {
			continue
		}
		allHandles = append(allHandles, blockHandles...)
	}

	// Return the last N objects
	if limit <= 0 || limit > len(allHandles) {
		limit = len(allHandles)
	}

	// Return in reverse order (newest first)
	result := make([]*ObjectHandle, 0, limit)
	for i := len(allHandles) - 1; i >= 0 && len(result) < limit; i-- {
		result = append(result, allHandles[i])
	}

	return result, nil
}

// GetObjectsSince returns objects from the last duration.
// For example, GetObjectsSince(time.Hour) returns objects from the last hour.
// Returns handles only, not data.
func (s *Store) GetObjectsSince(d time.Duration, limit int) ([]*ObjectHandle, error) {
	endTime := time.Now().UnixNano()
	startTime := endTime - d.Nanoseconds()
	return s.GetObjectsInRange(startTime, endTime, limit)
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

	// Find start block position
	startOffset := s.findOffsetForTimeLocked(startTime, 0, count-1, true)

	// Find end block position - we may need to scan one block past this
	endOffset := s.findOffsetForTimeLocked(endTime, 0, count-1, false)

	handles := make([]*ObjectHandle, 0)

	// Scan blocks from startOffset to endOffset+1 (may need extra block for packed objects)
	for offset := startOffset; offset <= endOffset+1 && offset < count; offset++ {
		blockNum := s.blockNumFromOffset(offset)

		// Get all objects in this block
		blockHandles, err := s.scanBlockObjects(blockNum)
		if err != nil {
			continue
		}

		for _, h := range blockHandles {
			// Check if past end of range - stop scanning
			if h.Timestamp > endTime {
				return handles, nil
			}

			// Check if within range
			if h.Timestamp >= startTime {
				handles = append(handles, h)
				if limit > 0 && len(handles) >= limit {
					return handles, nil
				}
			}
		}
	}

	return handles, nil
}

// DeleteObject removes an object.
func (s *Store) DeleteObject(handle *ObjectHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if err := s.reclaimBlock(handle.BlockNum); err != nil {
		return err
	}

	s.adjustTailAfterReclaim()
	return s.writeMetaLocked()
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

	if err := s.reclaimBlock(blockNum); err != nil {
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
		return 0, ErrObjectTooLarge
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
		Timestamp: timestamp,
		BlockNum:  blockNum,
		DataLen:   uint32(len(data)),
		Flags:     block.FlagPrimary,
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
		Timestamp: timestamp,
		BlockNum:  blockNum,
	}
	if err := s.writeIndexEntry(blockNum, entry); err != nil {
		return 0, err
	}

	return blockNum, nil
}

// ReadBlockData reads the data from a block.
func (s *Store) ReadBlockData(blockNum uint32) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if blockNum >= s.meta.NumBlocks {
		return nil, ErrBlockOutOfRange
	}

	return s.readBlockDataLocked(blockNum)
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
