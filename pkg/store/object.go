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
	Size      uint32 `json:"size"`
}

// MaxObjectSize returns the maximum object size for this store.
func (s *Store) MaxObjectSize() uint32 {
	return s.config.DataBlockSize - block.BlockHeaderSize
}

// PutObject stores an object at the given timestamp.
// Returns ErrObjectTooLarge if data exceeds block size.
func (s *Store) PutObject(timestamp int64, data []byte) (*ObjectHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if timestamp <= 0 {
		return nil, ErrInvalidTimestamp
	}

	maxSize := s.config.DataBlockSize - block.BlockHeaderSize
	if uint32(len(data)) > maxSize {
		return nil, ErrObjectTooLarge
	}

	blockNum, err := s.insertLocked(timestamp, data)
	if err != nil {
		return nil, err
	}

	if err := s.writeMetaLocked(); err != nil {
		return nil, err
	}

	return &ObjectHandle{
		Timestamp: timestamp,
		BlockNum:  blockNum,
		Size:      uint32(len(data)),
	}, nil
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

	return s.readBlockDataLocked(handle.BlockNum)
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

// GetObjectByBlock retrieves an object by its block number.
func (s *Store) GetObjectByBlock(blockNum uint32) ([]byte, *ObjectHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, nil, ErrStoreClosed
	}

	if blockNum >= s.meta.NumBlocks {
		return nil, nil, ErrBlockOutOfRange
	}

	data, err := s.readBlockDataLocked(blockNum)
	if err != nil {
		return nil, nil, err
	}

	header, err := s.readBlockHeader(blockNum)
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
			Timestamp: entry.Timestamp,
			BlockNum:  blockNum,
			Size:      header.DataLen,
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
			Timestamp: entry.Timestamp,
			BlockNum:  blockNum,
			Size:      header.DataLen,
		})
	}

	return handles, nil
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
			Timestamp: entry.Timestamp,
			BlockNum:  blockNum,
			Size:      header.DataLen,
		})
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
