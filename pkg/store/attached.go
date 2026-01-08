// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"github.com/tviviano/ts-store/pkg/block"
)

// AttachBlock attaches a new overflow block to a primary block.
// Returns the attached block number.
func (s *Store) AttachBlock(primaryBlockNum uint32) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	if primaryBlockNum >= s.meta.NumBlocks {
		return 0, ErrBlockOutOfRange
	}

	// Read primary block header
	primaryHeader, err := s.readBlockHeader(primaryBlockNum)
	if err != nil {
		return 0, err
	}

	// Allocate a new block for attachment
	attachedBlockNum, err := s.allocateAttachedBlock()
	if err != nil {
		return 0, err
	}

	// Initialize attached block header
	attachedHeader := &block.BlockHeader{
		Timestamp:     primaryHeader.Timestamp,
		BlockNum:      attachedBlockNum,
		AttachedCount: 0,
		FirstAttached: 0,
		LastAttached:  0,
		DataLen:       0,
		Flags:         block.FlagAttached,
		NextFree:      0, // Used as next-attached link
	}

	// Link into the chain
	if primaryHeader.AttachedCount == 0 {
		// First attached block
		primaryHeader.FirstAttached = attachedBlockNum
		primaryHeader.LastAttached = attachedBlockNum
	} else {
		// Append to existing chain
		lastAttached, err := s.readBlockHeader(primaryHeader.LastAttached)
		if err != nil {
			return 0, err
		}
		lastAttached.NextFree = attachedBlockNum // Link to new block
		if err := s.writeBlockHeader(primaryHeader.LastAttached, lastAttached); err != nil {
			return 0, err
		}
		primaryHeader.LastAttached = attachedBlockNum
	}

	primaryHeader.AttachedCount++

	// Write headers
	if err := s.writeBlockHeader(attachedBlockNum, attachedHeader); err != nil {
		return 0, err
	}
	if err := s.writeBlockHeader(primaryBlockNum, primaryHeader); err != nil {
		return 0, err
	}

	// Update index entry
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
	if err := s.writeMetaLocked(); err != nil {
		return 0, err
	}

	return attachedBlockNum, nil
}

// AttachBlockByTime attaches a new overflow block to the primary block containing the given timestamp.
// Returns the attached block number.
func (s *Store) AttachBlockByTime(timestamp int64) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrStoreClosed
	}

	// Find the primary block by timestamp
	primaryBlockNum, err := s.findBlockByTimeLocked(timestamp)
	if err != nil {
		return 0, err
	}

	// Unlock and call the regular attach (which will reacquire lock)
	s.mu.Unlock()
	result, err := s.AttachBlock(primaryBlockNum)
	s.mu.Lock() // Re-lock for deferred unlock
	return result, err
}

// allocateAttachedBlock gets a block for use as an attached block.
// Attached blocks come from a reserved pool beyond the primary circular blocks.
// Lock must be held.
func (s *Store) allocateAttachedBlock() (uint32, error) {
	// First try the free list
	if s.meta.FreeListCount > 0 {
		return s.popFreeList()
	}

	// Calculate attached block pool start (after primary blocks)
	attachedPoolStart := s.meta.NumBlocks
	maxAttached := s.meta.NumBlocks // Equal number of attached blocks available

	// Find next available attached block
	// For simplicity, we track TotalAttached and allocate sequentially
	// until pool is full, then we must reclaim
	nextAttached := attachedPoolStart + s.meta.TotalAttached

	if s.meta.TotalAttached >= maxAttached {
		// Attached pool full - must reclaim oldest primary to free up attached blocks
		// This is a design decision: when attached pool is full, reclaim oldest
		_, err := s.reclaimOldestBlock()
		if err != nil {
			return 0, err
		}
		// Now free list should have blocks from the reclaimed primary's attachments
		if s.meta.FreeListCount > 0 {
			return s.popFreeList()
		}
		// If still no free blocks, allocate from the now-available pool
		nextAttached = attachedPoolStart + s.meta.TotalAttached
	}

	// Initialize the new attached block
	header := &block.BlockHeader{
		BlockNum: nextAttached,
		Flags:    0, // Will be set by caller
	}
	if err := s.writeBlockHeader(nextAttached, header); err != nil {
		return 0, err
	}

	return nextAttached, nil
}

// GetAttachedBlocks returns the block numbers of all blocks attached to a primary block.
func (s *Store) GetAttachedBlocks(primaryBlockNum uint32) ([]uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if primaryBlockNum >= s.meta.NumBlocks {
		return nil, ErrBlockOutOfRange
	}

	header, err := s.readBlockHeader(primaryBlockNum)
	if err != nil {
		return nil, err
	}

	if header.AttachedCount == 0 {
		return nil, nil
	}

	blocks := make([]uint32, 0, header.AttachedCount)
	current := header.FirstAttached

	for i := uint32(0); i < header.AttachedCount; i++ {
		blocks = append(blocks, current)

		attachedHeader, err := s.readBlockHeader(current)
		if err != nil {
			return nil, err
		}
		current = attachedHeader.NextFree // NextFree used as next-attached link
	}

	return blocks, nil
}

// ReadBlockData reads the data portion of a block (primary or attached).
func (s *Store) ReadBlockData(blockNum uint32) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

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

// WriteBlockData writes data to a block (primary or attached).
func (s *Store) WriteBlockData(blockNum uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	maxData := s.config.DataBlockSize - block.BlockHeaderSize
	if uint32(len(data)) > maxData {
		return ErrBlockOutOfRange // Data too large for block
	}

	// Update header with data length
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return err
	}
	header.DataLen = uint32(len(data))
	if err := s.writeBlockHeader(blockNum, header); err != nil {
		return err
	}

	// Write data
	offset := s.blockOffset(blockNum) + block.BlockHeaderSize
	_, err = s.dataFile.WriteAt(data, offset)
	return err
}
