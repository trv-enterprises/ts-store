// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"errors"

	"github.com/tviviano/ts-store/pkg/block"
)

var (
	ErrTimestampNotFound = errors.New("timestamp not found")
	ErrEmptyStore        = errors.New("store is empty")
)

// findBlockByTimeLocked finds the primary block number for a given timestamp using binary search.
// For packed blocks, finds the block whose first object timestamp <= target timestamp.
// The caller should scan the block for the exact object.
// Lock must be held.
func (s *Store) findBlockByTimeLocked(timestamp int64) (uint32, error) {
	if s.meta.HeadBlock == s.meta.TailBlock {
		// Check if there's at least one entry
		entry, err := s.readIndexEntry(s.meta.HeadBlock)
		if err != nil {
			return 0, err
		}
		if entry.Timestamp == 0 {
			return 0, ErrEmptyStore
		}
		return s.meta.HeadBlock, nil
	}

	// Binary search on the circular index
	// The index is sorted by time from tail (oldest) to head (newest)
	// For packed blocks, we find the last block whose first object <= timestamp
	count := s.activeBlockCount()

	left := uint32(0)
	right := count - 1
	result := left

	for left <= right {
		mid := (left + right) / 2
		midBlockNum := s.blockNumFromOffset(mid)

		entry, err := s.readIndexEntry(midBlockNum)
		if err != nil {
			return 0, err
		}

		// Skip continuation blocks (timestamp 0)
		if entry.Timestamp == 0 {
			left = mid + 1
			continue
		}

		if entry.Timestamp == timestamp {
			// Exact match on block's first object
			return midBlockNum, nil
		} else if entry.Timestamp < timestamp {
			// This block's first object is before target, might contain it
			result = mid
			left = mid + 1
		} else {
			// This block starts after target
			if mid == 0 {
				break
			}
			right = mid - 1
		}
	}

	// Return the block whose first object is <= timestamp
	// Caller will scan for exact match
	blockNum := s.blockNumFromOffset(result)
	return blockNum, nil
}

// FindBlockByTime finds the primary block number for a given timestamp.
// Returns the block with the exact timestamp, or the closest block if exact match not found.
func (s *Store) FindBlockByTime(timestamp int64) (uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	return s.findBlockByTimeLocked(timestamp)
}

// FindBlockByTimeExact finds the primary block with an exact timestamp match.
// Returns ErrTimestampNotFound if no exact match exists.
func (s *Store) FindBlockByTimeExact(timestamp int64) (uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	blockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return 0, err
	}

	entry, err := s.readIndexEntry(blockNum)
	if err != nil {
		return 0, err
	}

	if entry.Timestamp != timestamp {
		return 0, ErrTimestampNotFound
	}

	return blockNum, nil
}

// FindBlocksInRange finds all primary blocks with timestamps in the given range [startTime, endTime].
func (s *Store) FindBlocksInRange(startTime, endTime int64) ([]uint32, error) {
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

	// Collect all blocks in range
	blocks := make([]uint32, 0, endOffset-startOffset+1)
	for offset := startOffset; offset <= endOffset; offset++ {
		blockNum := s.blockNumFromOffset(offset)
		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			return nil, err
		}
		if entry.Timestamp >= startTime && entry.Timestamp <= endTime {
			blocks = append(blocks, blockNum)
		}
	}

	return blocks, nil
}

// findOffsetForTimeLocked performs binary search and returns the offset.
// If findFirst is true, finds the first entry >= timestamp.
// If findFirst is false, finds the last entry <= timestamp.
func (s *Store) findOffsetForTimeLocked(timestamp int64, left, right uint32, findFirst bool) uint32 {
	result := left

	for left <= right {
		mid := (left + right) / 2
		blockNum := s.blockNumFromOffset(mid)

		entry, err := s.readIndexEntry(blockNum)
		if err != nil {
			// On error, return current result
			return result
		}

		if findFirst {
			if entry.Timestamp >= timestamp {
				result = mid
				if mid == 0 {
					break
				}
				right = mid - 1
			} else {
				left = mid + 1
			}
		} else {
			if entry.Timestamp <= timestamp {
				result = mid
				left = mid + 1
			} else {
				if mid == 0 {
					break
				}
				right = mid - 1
			}
		}
	}

	return result
}

// activeBlockCount returns the number of active (non-free) primary blocks.
func (s *Store) activeBlockCount() uint32 {
	if s.meta.HeadBlock >= s.meta.TailBlock {
		return s.meta.HeadBlock - s.meta.TailBlock + 1
	}
	// Wraparound case
	return s.meta.NumBlocks - s.meta.TailBlock + s.meta.HeadBlock + 1
}

// blockNumFromOffset converts a logical offset (0 = tail, oldest) to a block number.
func (s *Store) blockNumFromOffset(offset uint32) uint32 {
	return (s.meta.TailBlock + offset) % s.meta.NumBlocks
}

// GetOldestTimestamp returns the timestamp of the oldest entry.
func (s *Store) GetOldestTimestamp() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	entry, err := s.readIndexEntry(s.meta.TailBlock)
	if err != nil {
		return 0, err
	}

	if entry.Timestamp == 0 {
		return 0, ErrEmptyStore
	}

	return entry.Timestamp, nil
}

// GetNewestTimestamp returns the timestamp of the newest entry.
func (s *Store) GetNewestTimestamp() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	entry, err := s.readIndexEntry(s.meta.HeadBlock)
	if err != nil {
		return 0, err
	}

	if entry.Timestamp == 0 {
		return 0, ErrEmptyStore
	}

	return entry.Timestamp, nil
}

// GetBlockHeader returns the header information for a block.
func (s *Store) GetBlockHeader(blockNum uint32) (*block.BlockHeader, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	return s.readBlockHeader(blockNum)
}

// GetIndexEntry returns the index entry for a primary block.
func (s *Store) GetIndexEntry(blockNum uint32) (*block.IndexEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if blockNum >= s.meta.NumBlocks {
		return nil, ErrBlockOutOfRange
	}

	return s.readIndexEntry(blockNum)
}
