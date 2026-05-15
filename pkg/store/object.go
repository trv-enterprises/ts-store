// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
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
	Timestamp   int64  `json:"timestamp"`
	BlockNum    uint32 `json:"block_num"`
	Offset      uint32 `json:"offset,omitempty"`       // Position within block (0 for V1 format)
	Size        uint32 `json:"size"`
	SpanCount   uint32 `json:"span_count,omitempty"`   // Number of blocks (1 = single block, 0 = legacy)
	PartitionID uint32 `json:"partition_id,omitempty"` // V2: partition containing this object
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
	var newestTs int64
	var tsErr error
	if s.isV2 {
		newestTs, tsErr = s.getNewestTimestampV2()
	} else {
		newestTs, tsErr = s.getNewestTimestampLocked()
	}
	if tsErr == nil && timestamp <= newestTs {
		return nil, ErrTimestampOutOfOrder
	}
	// ErrEmptyStore is OK - first insert

	// Route to V2 if partitioned store
	if s.isV2 {
		h, err := s.putObjectV2(timestamp, data)
		if err == nil {
			s.metrics.recordWrite(len(data))
		}
		return h, err
	}

	// V1 circular buffer path
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

	s.metrics.recordWrite(len(data))
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

	var (
		data []byte
		err  error
	)
	switch {
	case s.isV2:
		// V2 partitioned store - use partition from handle.
		data, err = s.getObjectV2(handle)
	case handle.Offset > 0:
		// V1 packed: V2-style handle with Offset set.
		data, err = s.readPackedObjectData(handle.BlockNum, handle.Offset, handle.Size, handle.SpanCount)
	default:
		// V1 legacy: single object per block.
		data, err = s.readBlockDataLocked(handle.BlockNum)
	}
	if err == nil {
		s.metrics.recordRead(len(data))
	}
	return data, err
}

// getObjectV2 retrieves an object from a V2 partitioned store.
func (s *Store) getObjectV2(handle *ObjectHandle) ([]byte, error) {
	p := s.partitions[handle.PartitionID]
	if p == nil {
		return nil, ErrObjectNotFound
	}

	return s.readPackedObjectDataFromPartition(p, handle.BlockNum, handle.Offset, handle.Size, handle.SpanCount)
}

// readPackedObjectDataFromPartition reads object data from a partition.
func (s *Store) readPackedObjectDataFromPartition(p *Partition, blockNum uint32, offset uint32, size uint32, spanCount uint32) ([]byte, error) {
	// Read object header to get flags
	objHeader, err := p.readObjectHeader(blockNum, offset)
	if err != nil {
		return nil, err
	}

	// If not spanning, read directly
	if !objHeader.Continues() {
		data := make([]byte, objHeader.DataLen)
		dataOffset := offset + block.ObjectHeaderSize
		fileOffset := p.blockOffset(blockNum) + int64(dataOffset)
		if _, err := p.dataFile.ReadAt(data, fileOffset); err != nil {
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
		blockHeader, err := p.readBlockHeader(currentBlock)
		if err != nil {
			return nil, err
		}

		var readStart uint32
		var chunkSize uint32

		if isFirst {
			readStart = offset + block.ObjectHeaderSize
			chunkSize = blockHeader.DataLen - block.ObjectHeaderSize
			if chunkSize > remaining {
				chunkSize = remaining
			}
		} else {
			readStart = block.BlockHeaderSize
			chunkSize = blockHeader.DataLen
			if chunkSize > remaining {
				chunkSize = remaining
			}
		}

		chunk := make([]byte, chunkSize)
		fileOffset := p.blockOffset(currentBlock) + int64(readStart)
		if _, err := p.dataFile.ReadAt(chunk, fileOffset); err != nil {
			return nil, err
		}
		data = append(data, chunk...)
		remaining -= chunkSize

		if remaining > 0 {
			currentBlock++
		}

		isFirst = false
	}

	return data, nil
}

// GetObjectByTime retrieves an object by its timestamp.
func (s *Store) GetObjectByTime(timestamp int64) (data []byte, handle *ObjectHandle, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Record on any successful return path. The named-return + defer
	// pattern keeps the early-return branches free of counter calls.
	defer func() {
		if err == nil {
			s.metrics.recordRead(len(data))
		}
	}()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	// V2 partitioned store
	if s.isV2 {
		return s.getObjectByTimeV2(timestamp)
	}

	// V1: Find block by timestamp (binary search finds block with first object <= timestamp)
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

	data, err = s.readBlockDataLocked(blockNum)
	if err != nil {
		return nil, nil, err
	}

	return data, &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  blockNum,
		Size:      uint32(len(data)),
	}, nil
}

// getObjectByTimeV2 retrieves an object by timestamp from V2 store.
func (s *Store) getObjectByTimeV2(timestamp int64) ([]byte, *ObjectHandle, error) {
	p, blockNum, err := s.findBlockByTimeV2(timestamp)
	if err != nil {
		return nil, nil, err
	}

	// Scan the block for the exact timestamp
	return s.scanBlockForTimestampInPartition(p, blockNum, timestamp)
}

// scanBlockForTimestampInPartition scans a block for an object with the given timestamp.
func (s *Store) scanBlockForTimestampInPartition(p *Partition, blockNum uint32, timestamp int64) ([]byte, *ObjectHandle, error) {
	offset := uint32(block.BlockHeaderSize)

	for offset < p.blockSize {
		objHeader, err := p.readObjectHeader(blockNum, offset)
		if err != nil {
			return nil, nil, err
		}

		if objHeader.Timestamp == 0 {
			break
		}

		if objHeader.Timestamp == timestamp {
			data, err := s.readPackedObjectDataFromPartition(p, blockNum, offset, objHeader.DataLen, 1)
			if err != nil {
				return nil, nil, err
			}

			spanCount := uint32(1)
			if objHeader.Continues() {
				spanCount = s.calculateSpanCountV2(p, objHeader.DataLen)
			}

			return data, &ObjectHandle{
				Timestamp:   timestamp,
				BlockNum:    blockNum,
				Offset:      offset,
				Size:        objHeader.DataLen,
				SpanCount:   spanCount,
				PartitionID: p.id,
			}, nil
		}

		if objHeader.Timestamp > timestamp {
			return nil, nil, ErrTimestampNotFound
		}

		if objHeader.NextOffset == 0 || objHeader.IsLastInBlock() {
			break
		}
		offset = objHeader.NextOffset
	}

	return nil, nil, ErrTimestampNotFound
}

// calculateSpanCountV2 calculates span count for V2 stores.
func (s *Store) calculateSpanCountV2(p *Partition, dataLen uint32) uint32 {
	usablePerBlock := p.blockSize - block.BlockHeaderSize
	firstBlockUsable := usablePerBlock - block.ObjectHeaderSize

	if dataLen <= firstBlockUsable {
		return 1
	}

	remaining := dataLen - firstBlockUsable
	return 1 + (remaining+usablePerBlock-1)/usablePerBlock
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
func (s *Store) GetOldestObjects(limit int) (handles []*ObjectHandle, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defer func() {
		if err == nil {
			s.metrics.recordRangeRead()
		}
	}()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if s.isV2 {
		return s.getOldestObjectsV2(limit)
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	handles = make([]*ObjectHandle, 0)

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

// getOldestObjectsV2 returns oldest objects from V2 store.
func (s *Store) getOldestObjectsV2(limit int) ([]*ObjectHandle, error) {
	handles := make([]*ObjectHandle, 0)

	// Iterate through partitions from oldest to newest
	for i := uint8(0); i < s.globalMeta.ActiveCount && (limit <= 0 || len(handles) < limit); i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		p := s.partitions[partID]
		if p == nil {
			continue
		}

		// Scan blocks in this partition from oldest to newest
		count := p.activeBlockCount()
		for blockNum := uint32(0); blockNum < count && (limit <= 0 || len(handles) < limit); blockNum++ {
			blockHandles, err := s.scanBlockObjectsInPartition(p, blockNum)
			if err != nil {
				continue
			}

			for _, h := range blockHandles {
				handles = append(handles, h)
				if limit > 0 && len(handles) >= limit {
					return handles, nil
				}
			}
		}
	}

	return handles, nil
}

// GetNewestObjects returns the N newest objects (from head).
// Returns handles only, not data. Use GetObject to retrieve data.
// Results are returned newest first.
func (s *Store) GetNewestObjects(limit int) (handles []*ObjectHandle, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defer func() {
		if err == nil {
			s.metrics.recordRangeRead()
		}
	}()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if s.isV2 {
		return s.getNewestObjectsV2(limit)
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	// Iterate from head (newest) backwards to collect only needed objects
	result := make([]*ObjectHandle, 0, limit)

	for i := int(count) - 1; i >= 0 && (limit <= 0 || len(result) < limit); i-- {
		blockNum := s.blockNumFromOffset(uint32(i))
		blockHandles, err := s.scanBlockObjects(blockNum)
		if err != nil {
			continue
		}

		// Add handles from this block in reverse order (newest first)
		for j := len(blockHandles) - 1; j >= 0 && (limit <= 0 || len(result) < limit); j-- {
			result = append(result, blockHandles[j])
		}
	}

	return result, nil
}

// getNewestObjectsV2 returns newest objects from V2 store.
func (s *Store) getNewestObjectsV2(limit int) ([]*ObjectHandle, error) {
	result := make([]*ObjectHandle, 0, limit)

	// Iterate through partitions from newest to oldest
	for i := int(s.globalMeta.ActiveCount) - 1; i >= 0 && (limit <= 0 || len(result) < limit); i-- {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		p := s.partitions[partID]
		if p == nil {
			continue
		}

		// Scan blocks in this partition from newest to oldest
		count := p.activeBlockCount()
		for blockIdx := int(count) - 1; blockIdx >= 0 && (limit <= 0 || len(result) < limit); blockIdx-- {
			blockHandles, err := s.scanBlockObjectsInPartition(p, uint32(blockIdx))
			if err != nil {
				continue
			}

			// Add handles in reverse order (newest first)
			for j := len(blockHandles) - 1; j >= 0 && (limit <= 0 || len(result) < limit); j-- {
				result = append(result, blockHandles[j])
			}
		}
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
// Pass 0 for startTime to start from oldest, 0 for endTime to go until now.
// Returns handles only, not data.
func (s *Store) GetObjectsInRange(startTime, endTime int64, limit int) (handles []*ObjectHandle, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defer func() {
		if err == nil {
			s.metrics.recordRangeRead()
		}
	}()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if s.isV2 {
		return s.getObjectsInRangeV2(startTime, endTime, limit)
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil, nil
	}

	// Handle unbounded start time
	if startTime == 0 {
		oldest, err := s.getOldestTimestampLocked()
		if err != nil {
			return nil, nil // Empty store
		}
		startTime = oldest
	}

	// Handle unbounded end time
	if endTime == 0 {
		endTime = time.Now().UnixNano()
	}

	if startTime > endTime {
		return nil, ErrInvalidTimestamp
	}

	// Find start block position
	startOffset := s.findOffsetForTimeLocked(startTime, 0, count-1, true)

	// Find end block position - we may need to scan one block past this
	endOffset := s.findOffsetForTimeLocked(endTime, 0, count-1, false)

	handles = make([]*ObjectHandle, 0)

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

// getObjectsInRangeV2 returns objects in time range from V2 store.
func (s *Store) getObjectsInRangeV2(startTime, endTime int64, limit int) ([]*ObjectHandle, error) {
	// Handle unbounded times
	if startTime == 0 {
		oldest, err := s.getOldestTimestampV2()
		if err != nil {
			return nil, nil
		}
		startTime = oldest
	}
	if endTime == 0 {
		endTime = time.Now().UnixNano()
	}

	if startTime > endTime {
		return nil, ErrInvalidTimestamp
	}

	handles := make([]*ObjectHandle, 0)

	// Get partitions that may contain data in range
	partitions := s.getPartitionsInRange(startTime, endTime)

	for _, p := range partitions {
		if limit > 0 && len(handles) >= limit {
			break
		}

		// Scan blocks in this partition
		count := p.activeBlockCount()
		for blockNum := uint32(0); blockNum < count && (limit <= 0 || len(handles) < limit); blockNum++ {
			blockHandles, err := s.scanBlockObjectsInPartition(p, blockNum)
			if err != nil {
				continue
			}

			for _, h := range blockHandles {
				if h.Timestamp > endTime {
					return handles, nil
				}
				if h.Timestamp >= startTime {
					handles = append(handles, h)
					if limit > 0 && len(handles) >= limit {
						return handles, nil
					}
				}
			}
		}
	}

	return handles, nil
}

// scanBlockObjectsInPartition scans all objects in a block within a partition.
func (s *Store) scanBlockObjectsInPartition(p *Partition, blockNum uint32) ([]*ObjectHandle, error) {
	header, err := p.readBlockHeader(blockNum)
	if err != nil {
		return nil, err
	}

	// Skip continuation blocks
	if header.IsContinuation() {
		return nil, nil
	}

	// V2 packed format - scan all objects
	var handles []*ObjectHandle
	offset := uint32(block.BlockHeaderSize)

	for offset < p.blockSize {
		objHeader, err := p.readObjectHeader(blockNum, offset)
		if err != nil {
			break
		}

		if objHeader.Timestamp == 0 {
			break
		}

		spanCount := uint32(1)
		if objHeader.Continues() {
			spanCount = s.calculateSpanCountV2(p, objHeader.DataLen)
		}

		handles = append(handles, &ObjectHandle{
			Timestamp:   objHeader.Timestamp,
			BlockNum:    blockNum,
			Offset:      offset,
			Size:        objHeader.DataLen,
			SpanCount:   spanCount,
			PartitionID: p.id,
		})

		if objHeader.NextOffset == 0 || objHeader.IsLastInBlock() {
			break
		}
		offset = objHeader.NextOffset
	}

	return handles, nil
}

// DeleteObject removes an object by clearing its index entry.
// In a circular buffer, the block space is not immediately reusable -
// it will be reclaimed when the tail advances to this position.
// Note: This operation is not supported for V2 partitioned stores.
func (s *Store) DeleteObject(handle *ObjectHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	// V2 stores don't support individual object deletion
	if s.isV2 {
		return ErrDeleteNotSupportedV2
	}

	// Clear the index entry to mark the object as deleted
	if err := s.clearIndexEntry(handle.BlockNum); err != nil {
		return err
	}

	// If this was the tail block, advance the tail
	s.adjustTailAfterReclaim()
	return s.writeMetaLocked()
}

// DeleteObjectByTime removes an object by its timestamp.
// In a circular buffer, the block space is not immediately reusable -
// it will be reclaimed when the tail advances to this position.
// Note: This operation is not supported for V2 partitioned stores.
func (s *Store) DeleteObjectByTime(timestamp int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	// V2 stores don't support individual object deletion
	if s.isV2 {
		return ErrDeleteNotSupportedV2
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

	// Clear the index entry to mark the object as deleted
	if err := s.clearIndexEntry(blockNum); err != nil {
		return err
	}

	// If this was the tail block, advance the tail
	s.adjustTailAfterReclaim()
	return s.writeMetaLocked()
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
