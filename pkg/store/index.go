// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
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

// GetOldestTimestamp returns the timestamp of the oldest entry.
func (s *Store) GetOldestTimestamp() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	return s.getOldestTimestampV2()
}

// GetNewestTimestamp returns the timestamp of the newest entry.
func (s *Store) GetNewestTimestamp() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	return s.getNewestTimestampV2()
}

// GetBlockHeader returns the header information for a block in the current partition.
func (s *Store) GetBlockHeader(blockNum uint32) (*block.BlockHeader, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if s.currentPartition == nil {
		return nil, ErrEmptyStore
	}
	return s.currentPartition.readBlockHeader(blockNum)
}

// V2 Index Methods

// findPartitionByTime finds the partition that contains the given timestamp.
// Uses binary search on partition min/max timestamps.
// Returns the partition and its position in the order array.
func (s *Store) findPartitionByTime(timestamp int64) (*Partition, int, error) {
	if s.globalMeta.ActiveCount == 0 {
		return nil, -1, ErrEmptyStore
	}

	// Binary search on partitions (ordered oldest to newest)
	left := 0
	right := int(s.globalMeta.ActiveCount) - 1
	result := -1

	for left <= right {
		mid := (left + right) / 2
		partID := uint32(s.globalMeta.PartitionOrder[mid])
		p := s.partitions[partID]
		if p == nil {
			left = mid + 1
			continue
		}

		if timestamp < p.meta.MinTimestamp {
			right = mid - 1
		} else if timestamp > p.meta.MaxTimestamp {
			result = mid // This partition is before target, remember it
			left = mid + 1
		} else {
			// Timestamp is within this partition's range
			return p, mid, nil
		}
	}

	// Return the last partition that's before the timestamp
	// (caller will scan forward if needed)
	if result >= 0 {
		partID := uint32(s.globalMeta.PartitionOrder[result])
		return s.partitions[partID], result, nil
	}

	// Timestamp is before all partitions, return the oldest
	partID := uint32(s.globalMeta.PartitionOrder[0])
	return s.partitions[partID], 0, nil
}

// findBlockByTimeV2 finds the block for a timestamp in V2 store.
// Returns partition, block number, and error.
func (s *Store) findBlockByTimeV2(timestamp int64) (*Partition, uint32, error) {
	p, _, err := s.findPartitionByTime(timestamp)
	if err != nil {
		return nil, 0, err
	}

	// Binary search within the partition
	blockNum, err := s.findBlockInPartition(p, timestamp)
	if err != nil {
		return nil, 0, err
	}

	return p, blockNum, nil
}

// findBlockInPartition finds the block for a timestamp within a partition.
func (s *Store) findBlockInPartition(p *Partition, timestamp int64) (uint32, error) {
	count := p.activeBlockCount()
	if count == 0 {
		return 0, ErrEmptyStore
	}

	if count == 1 {
		return 0, nil
	}

	left := uint32(0)
	right := count - 1
	result := left

	for left <= right {
		mid := (left + right) / 2

		entry, err := p.readIndexEntry(mid)
		if err != nil {
			return 0, err
		}

		// Skip continuation blocks
		if entry.Timestamp == 0 {
			left = mid + 1
			continue
		}

		if entry.Timestamp == timestamp {
			return mid, nil
		} else if entry.Timestamp < timestamp {
			result = mid
			left = mid + 1
		} else {
			if mid == 0 {
				break
			}
			right = mid - 1
		}
	}

	return result, nil
}

// getNewestTimestampV2 returns the newest timestamp for V2 stores.
func (s *Store) getNewestTimestampV2() (int64, error) {
	if s.globalMeta.ActiveCount == 0 {
		return 0, ErrEmptyStore
	}

	// Get the newest partition (last in order)
	newestIdx := s.globalMeta.ActiveCount - 1
	partID := uint32(s.globalMeta.PartitionOrder[newestIdx])
	p := s.partitions[partID]
	if p == nil || p.meta.MaxTimestamp == 0 {
		return 0, ErrEmptyStore
	}

	return p.meta.MaxTimestamp, nil
}

// getOldestTimestampV2 returns the oldest timestamp for V2 stores.
func (s *Store) getOldestTimestampV2() (int64, error) {
	if s.globalMeta.ActiveCount == 0 {
		return 0, ErrEmptyStore
	}

	// Get the oldest partition (first in order)
	partID := uint32(s.globalMeta.PartitionOrder[0])
	p := s.partitions[partID]
	if p == nil || p.meta.MinTimestamp == 0 {
		return 0, ErrEmptyStore
	}

	return p.meta.MinTimestamp, nil
}

// getPartitionsInRange returns partitions that may contain data in the time range.
func (s *Store) getPartitionsInRange(startTime, endTime int64) []*Partition {
	var result []*Partition

	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		p := s.partitions[partID]
		if p == nil {
			continue
		}

		// Check if partition's range overlaps with query range
		if p.meta.MaxTimestamp >= startTime && p.meta.MinTimestamp <= endTime {
			result = append(result, p)
		}
	}

	return result
}
