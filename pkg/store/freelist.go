// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"github.com/tviviano/ts-store/pkg/block"
)

// allocateBlock reclaims the oldest block (at tail) for reuse.
// In a pure circular buffer, free space is implicit - the gap between head and tail.
// Returns the block number. Lock must be held.
func (s *Store) allocateBlock() (uint32, error) {
	return s.reclaimOldestBlock()
}

// reclaimOldestBlock reclaims the oldest block (at tail).
// For spanning objects, advances tail past all continuation blocks.
// Lock must be held.
func (s *Store) reclaimOldestBlock() (uint32, error) {
	tailBlock := s.meta.TailBlock

	// Clear the index entry for this block
	if err := s.clearIndexEntry(tailBlock); err != nil {
		return 0, err
	}

	// Advance the tail
	nextTail := (tailBlock + 1) % s.meta.NumBlocks
	s.meta.TailBlock = nextTail

	// Skip over any continuation blocks (they're sequential in circular order)
	for s.meta.TailBlock != s.meta.HeadBlock {
		nextHeader, err := s.readBlockHeader(s.meta.TailBlock)
		if err != nil {
			break
		}
		if !nextHeader.IsContinuation() {
			break
		}
		// Clear this continuation block's index entry
		if err := s.clearIndexEntry(s.meta.TailBlock); err != nil {
			return 0, err
		}
		s.meta.TailBlock = (s.meta.TailBlock + 1) % s.meta.NumBlocks
	}

	// Clear the reclaimed block header
	clearHeader := &block.BlockHeader{
		Flags: 0,
	}
	if err := s.writeBlockHeader(tailBlock, clearHeader); err != nil {
		return 0, err
	}

	return tailBlock, nil
}

// clearIndexEntry clears the index entry for a given block.
// Lock must be held.
func (s *Store) clearIndexEntry(blockNum uint32) error {
	entry := &block.IndexEntry{
		Timestamp: 0,
		BlockNum:  blockNum,
	}
	return s.writeIndexEntry(blockNum, entry)
}

// ReclaimUpTo reclaims blocks from tail up to (but not including) the specified block.
// This is useful for bulk deletion of old data.
// Returns the number of blocks reclaimed.
func (s *Store) ReclaimUpTo(targetBlock uint32) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	if targetBlock >= s.meta.NumBlocks {
		return 0, ErrBlockOutOfRange
	}

	count := uint32(0)
	for s.meta.TailBlock != targetBlock && s.meta.TailBlock != s.meta.HeadBlock {
		// Clear index entry
		if err := s.clearIndexEntry(s.meta.TailBlock); err != nil {
			return count, err
		}

		// Clear block header
		clearHeader := &block.BlockHeader{Flags: 0}
		if err := s.writeBlockHeader(s.meta.TailBlock, clearHeader); err != nil {
			return count, err
		}

		s.meta.TailBlock = (s.meta.TailBlock + 1) % s.meta.NumBlocks
		count++
	}

	if err := s.writeMetaLocked(); err != nil {
		return count, err
	}

	return count, nil
}
