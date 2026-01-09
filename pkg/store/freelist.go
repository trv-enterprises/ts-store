// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"github.com/tviviano/ts-store/pkg/block"
)

// allocateBlock gets a block from the free list or reclaims the oldest block.
// Returns the block number. Lock must be held.
func (s *Store) allocateBlock() (uint32, error) {
	// First, try to get from free list
	if s.meta.FreeListCount > 0 {
		return s.popFreeList()
	}

	// No free blocks - must reclaim oldest primary block
	return s.reclaimOldestBlock()
}

// popFreeList removes and returns the first block from the free list.
// Lock must be held.
func (s *Store) popFreeList() (uint32, error) {
	if s.meta.FreeListCount == 0 {
		return 0, ErrNoFreeBlocks
	}

	blockNum := s.meta.FreeListHead

	// Read the block header to get the next free block
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return 0, err
	}

	// Update free list head
	s.meta.FreeListHead = header.NextFree
	s.meta.FreeListCount--

	// Clear the free flag
	header.Flags &^= block.FlagFree
	header.NextFree = 0
	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return 0, err
	}

	return blockNum, nil
}

// pushFreeList adds a block to the front of the free list.
// Lock must be held.
func (s *Store) pushFreeList(blockNum uint32) error {
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		// Block might be uninitialized, create new header
		header = &block.BlockHeader{}
	}

	// Set up as free block
	header.Flags = block.FlagFree
	header.NextFree = s.meta.FreeListHead
	header.BlockNum = blockNum
	header.Timestamp = 0
	header.DataLen = 0

	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return err
	}

	// Update free list metadata
	s.meta.FreeListHead = blockNum
	s.meta.FreeListCount++

	return nil
}

// reclaimOldestBlock reclaims the oldest primary block (at tail).
// Lock must be held.
func (s *Store) reclaimOldestBlock() (uint32, error) {
	tailBlock := s.meta.TailBlock

	// Clear the index entry for this block
	if err := s.clearIndexEntry(tailBlock); err != nil {
		return 0, err
	}

	// Advance the tail
	s.meta.TailBlock = (tailBlock + 1) % s.meta.NumBlocks

	// The reclaimed primary block is now available for use
	// We return it directly instead of putting it on the free list
	header := &block.BlockHeader{
		BlockNum: tailBlock,
		Flags:    0,
	}

	if err := s.writeBlockHeader(tailBlock, header); err != nil {
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

// AddRangeToFreeList adds a range of blocks (by block number) to the free list.
func (s *Store) AddRangeToFreeList(startBlock, endBlock uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if startBlock >= s.meta.NumBlocks || endBlock >= s.meta.NumBlocks {
		return ErrBlockOutOfRange
	}

	// Handle wraparound
	if startBlock <= endBlock {
		for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
			if err := s.reclaimBlock(blockNum); err != nil {
				return err
			}
		}
	} else {
		// Wraparound case: startBlock to end, then 0 to endBlock
		for blockNum := startBlock; blockNum < s.meta.NumBlocks; blockNum++ {
			if err := s.reclaimBlock(blockNum); err != nil {
				return err
			}
		}
		for blockNum := uint32(0); blockNum <= endBlock; blockNum++ {
			if err := s.reclaimBlock(blockNum); err != nil {
				return err
			}
		}
	}

	return s.writeMetaLocked()
}

// reclaimBlock reclaims a single block.
// Lock must be held.
func (s *Store) reclaimBlock(blockNum uint32) error {
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return err
	}

	// Already free? Skip
	if header.IsFree() {
		return nil
	}

	// Clear index entry
	if err := s.clearIndexEntry(blockNum); err != nil {
		return err
	}

	// Add block to free list
	return s.pushFreeList(blockNum)
}

// FreeListCount returns the number of blocks on the free list.
func (s *Store) FreeListCount() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta.FreeListCount
}
