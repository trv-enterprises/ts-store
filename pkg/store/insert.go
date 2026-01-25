// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package store

import (
	"time"

	"github.com/tviviano/ts-store/pkg/block"
)

// Insert inserts a new time entry with data into the store.
// Returns the block number where the data was written.
// If the store is full, the oldest entry is automatically reclaimed.
func (s *Store) Insert(timestamp int64, data []byte) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	if timestamp <= 0 {
		return 0, ErrInvalidTimestamp
	}

	// Check data size
	maxData := s.config.DataBlockSize - block.BlockHeaderSize
	if uint32(len(data)) > maxData {
		return 0, ErrBlockOutOfRange
	}

	// Determine the block to use
	var blockNum uint32
	var err error

	// Check if this is the first insert
	firstEntry, _ := s.readIndexEntry(s.meta.HeadBlock)
	isFirstInsert := firstEntry.Timestamp == 0

	if isFirstInsert {
		// First insert - use head position
		blockNum = s.meta.HeadBlock
	} else {
		// Check if circle is full
		nextHead := (s.meta.HeadBlock + 1) % s.meta.NumBlocks
		if nextHead == s.meta.TailBlock {
			// Circle is full - need to reclaim oldest or use free list
			blockNum, err = s.allocateBlock()
			if err != nil {
				return 0, err
			}
		} else {
			// Use next position in circle
			blockNum = nextHead
		}
		s.meta.HeadBlock = blockNum
	}

	// Initialize block header
	header := &block.BlockHeader{
		Timestamp: timestamp,
		DataLen:   uint32(len(data)),
		Flags:     block.FlagPrimary,
	}

	// Write block header
	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return 0, err
	}

	// Write data if present
	if len(data) > 0 {
		offset := s.blockOffset(blockNum) + block.BlockHeaderSize
		if _, err := s.dataFile.WriteAt(data, offset); err != nil {
			return 0, err
		}
	}

	// Update index entry
	entry := &block.IndexEntry{
		Timestamp: timestamp,
		BlockNum:  blockNum,
	}
	if err := s.writeIndexEntry(blockNum, entry); err != nil {
		return 0, err
	}

	// Persist metadata
	if err := s.writeMetaLocked(); err != nil {
		return 0, err
	}

	return blockNum, nil
}

// InsertNow inserts a new entry with the current time.
func (s *Store) InsertNow(data []byte) (uint32, error) {
	return s.Insert(time.Now().UnixNano(), data)
}

// ReclaimByTimeRange clears blocks within a time range.
// In a circular buffer, this clears index entries. The blocks will be
// reused when the tail advances to their positions.
func (s *Store) ReclaimByTimeRange(startTime, endTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if startTime > endTime {
		return ErrInvalidTimestamp
	}

	count := s.activeBlockCount()
	if count == 0 {
		return nil
	}

	// Find start offset (first block >= startTime)
	startOffset := s.findOffsetForTimeLocked(startTime, 0, count-1, true)

	// Find end offset (last block <= endTime)
	endOffset := s.findOffsetForTimeLocked(endTime, 0, count-1, false)

	if startOffset > endOffset {
		return nil
	}

	// Clear index entries for blocks from startOffset to endOffset
	for offset := startOffset; offset <= endOffset; offset++ {
		blockNum := s.blockNumFromOffset(offset)

		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			continue
		}

		// Verify the block is within time range
		if entry.Timestamp >= startTime && entry.Timestamp <= endTime {
			if err := s.clearIndexEntry(blockNum); err != nil {
				return err
			}
		}
	}

	// Adjust tail if we cleared the tail blocks
	s.adjustTailAfterReclaim()

	return s.writeMetaLocked()
}

// adjustTailAfterReclaim moves the tail forward past any free/empty blocks.
func (s *Store) adjustTailAfterReclaim() {
	for {
		entry, err := s.readIndexEntry(s.meta.TailBlock)
		if err != nil {
			break
		}

		// If this entry is still valid, stop
		if entry.Timestamp != 0 {
			break
		}

		// Move tail forward
		nextTail := (s.meta.TailBlock + 1) % s.meta.NumBlocks
		if nextTail == s.meta.HeadBlock {
			// Don't move past head
			break
		}
		s.meta.TailBlock = nextTail
	}
}

// Reclaim explicitly reclaims a block by block number.
// In a circular buffer, this clears the index entry. The block will be
// reused when the tail advances to this position.
func (s *Store) Reclaim(blockNum uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if blockNum >= s.meta.NumBlocks {
		return ErrBlockOutOfRange
	}

	// Clear the index entry
	if err := s.clearIndexEntry(blockNum); err != nil {
		return err
	}

	// If this was the tail block, advance the tail
	s.adjustTailAfterReclaim()

	return s.writeMetaLocked()
}

// ReclaimByTime reclaims the block closest to the given timestamp.
// In a circular buffer, this clears the index entry. The block will be
// reused when the tail advances to this position.
func (s *Store) ReclaimByTime(timestamp int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	blockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return err
	}

	// Clear the index entry
	if err := s.clearIndexEntry(blockNum); err != nil {
		return err
	}

	// If this was the tail block, advance the tail
	s.adjustTailAfterReclaim()

	return s.writeMetaLocked()
}
