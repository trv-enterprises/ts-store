// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"fmt"
	"os"

	"github.com/tviviano/ts-store/pkg/block"
)

// recoverFromCrash checks for and fixes inconsistencies that may have resulted
// from a crash during a write operation.
//
// Crash Recovery Strategy:
//
// The circular buffer maintains two pointers: HeadBlock (newest) and TailBlock (oldest).
// Crashes can occur during two operations:
//
// 1. Writing a new block (advancing head):
//   - Block data is written first, then HeadBlock is updated in metadata
//   - If crash occurs after writing block but before updating metadata,
//     we have an "orphaned" block with valid data that isn't tracked
//   - Recovery: scan forward from HeadBlock to find orphaned blocks and advance head
//
// 2. Reclaiming old blocks (advancing tail):
//   - TailBlock is updated first, then old block is cleared
//   - If crash occurs after updating tail but before clearing,
//     old data remains but is correctly excluded from the active range
//   - Recovery: no action needed, old data will be overwritten
//
// The key invariant: we never lose committed data. Orphaned writes are recovered,
// and the worst case for reclaim is some garbage data that will be overwritten.
func (s *Store) recoverFromCrash() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	recovered := false

	// Phase 1: Detect orphaned writes after HeadBlock
	// If a crash occurred after writing a block but before updating HeadBlock,
	// the block will have valid data but won't be tracked.
	orphanedHead, err := s.findOrphanedHead()
	if err != nil {
		return err
	}
	if orphanedHead != s.meta.HeadBlock {
		s.meta.HeadBlock = orphanedHead
		recovered = true
	}

	// Phase 2: Ensure TailBlock points to a primary block, not continuation data
	// If tail points to a continuation block, advance it to the next primary block
	if err := s.fixTailPointer(); err != nil {
		return err
	}

	// Phase 3: Update WriteOffset if needed
	// If head block exists, calculate correct WriteOffset from its contents
	if err := s.fixWriteOffset(); err != nil {
		return err
	}

	// Persist any fixes
	if recovered {
		if err := s.writeMetaLocked(); err != nil {
			return err
		}
	}

	return nil
}

// findOrphanedHead scans forward from HeadBlock to find any orphaned blocks
// that were written but not tracked due to a crash.
// Returns the correct HeadBlock position.
func (s *Store) findOrphanedHead() (uint32, error) {
	currentHead := s.meta.HeadBlock

	// Check if store is empty
	headHeader, err := s.readBlockHeader(currentHead)
	if err != nil {
		return currentHead, nil // Can't read, assume empty
	}
	if headHeader.DataLen == 0 && headHeader.Flags == 0 {
		return currentHead, nil // Empty store
	}

	// Scan forward looking for orphaned blocks
	maxScans := s.meta.NumBlocks // Prevent infinite loop
	for i := uint32(0); i < maxScans; i++ {
		nextBlock := (currentHead + 1) % s.meta.NumBlocks

		// Don't wrap around to tail (that's free space)
		if nextBlock == s.meta.TailBlock {
			break
		}

		// Check if next block has valid data
		header, err := s.readBlockHeader(nextBlock)
		if err != nil {
			break
		}

		// Check for valid data: non-zero flags or data length indicates written block
		if header.Flags == 0 && header.DataLen == 0 {
			break // Empty block, no orphan
		}

		// Found an orphaned block - advance head
		currentHead = nextBlock
	}

	return currentHead, nil
}

// fixTailPointer ensures TailBlock points to a primary block, not a continuation.
// This can happen if a spanning object was partially reclaimed before crash.
func (s *Store) fixTailPointer() error {
	maxScans := s.meta.NumBlocks
	for i := uint32(0); i < maxScans; i++ {
		// Don't advance tail past head (empty store case)
		if s.meta.TailBlock == s.meta.HeadBlock {
			break
		}

		header, err := s.readBlockHeader(s.meta.TailBlock)
		if err != nil {
			return err
		}

		// If tail points to a continuation block, advance it
		if header.IsContinuation() {
			s.meta.TailBlock = (s.meta.TailBlock + 1) % s.meta.NumBlocks
			continue
		}

		// Found a primary block (or empty block), we're done
		break
	}

	return nil
}

// fixWriteOffset recalculates WriteOffset based on the actual contents of HeadBlock.
// This is needed if a crash occurred mid-write to a packed block.
func (s *Store) fixWriteOffset() error {
	header, err := s.readBlockHeader(s.meta.HeadBlock)
	if err != nil {
		return nil // Can't read header, leave offset as-is
	}

	// If block is empty, reset offset
	if header.DataLen == 0 && header.Flags == 0 {
		s.meta.WriteOffset = 0
		return nil
	}

	// For packed blocks, scan to find actual end of data
	if header.IsPacked() && !header.IsContinuation() {
		// DataLen includes object headers, so add block header offset
		s.meta.WriteOffset = block.BlockHeaderSize + header.DataLen
	}

	return nil
}

// isBlockEmpty returns true if a block appears to be unused/empty.
func (s *Store) isBlockEmpty(blockNum uint32) (bool, error) {
	header, err := s.readBlockHeader(blockNum)
	if err != nil {
		return false, err
	}
	return header.DataLen == 0 && header.Flags == 0 && header.Timestamp == 0, nil
}

// V2 Rollover Recovery

// recoverRolloverV2 recovers from a crash during partition rollover.
// Must be called BEFORE opening partitions, as it may create or delete
// partition directories.
//
// Phase 0 (idle): nothing to do.
// Phase 1 (deleting): delete the target directory (may already be gone),
//   then transition to Phase 2 to create the new partition.
// Phase 2 (creating): delete any partial directory at target, create fresh
//   partition, add to tracking, and finalize to Phase 0.
func (s *Store) recoverRolloverV2() error {
	phase := s.globalMeta.RolloverPhase
	target := s.globalMeta.RolloverTarget

	if phase == RolloverPhaseIdle {
		return nil
	}

	if phase == RolloverPhaseDeleting {
		// Complete the delete (idempotent - directory may already be gone)
		deletePartition(s.path, target)

		// Derive the next partition ID to create and fall through to Phase 2
		nextID := (s.globalMeta.CurrentPartition + 1) % s.globalMeta.NumPartitions
		s.globalMeta.RolloverPhase = RolloverPhaseCreating
		s.globalMeta.RolloverTarget = nextID
		if err := s.writeGlobalMeta(); err != nil {
			return err
		}
		target = nextID
	}

	// Phase 2: creating
	// Remove any partial directory at target
	deletePartition(s.path, target)

	// Create fresh partition
	newPart, err := createPartition(s.path, target, s.globalMeta.BlocksPerPart, s.globalMeta.BlockSize)
	if err != nil {
		return fmt.Errorf("rollover recovery: failed to create partition %d: %w", target, err)
	}

	// Add to tracking
	s.partitions[target] = newPart
	s.currentPartition = newPart
	s.globalMeta.CurrentPartition = target
	s.globalMeta.PartitionOrder[s.globalMeta.ActiveCount] = uint8(target)
	s.globalMeta.ActiveCount++
	if s.globalMeta.ActiveCount == 1 {
		s.globalMeta.OldestPartition = target
	}

	// Finalize to idle
	s.globalMeta.RolloverPhase = RolloverPhaseIdle
	s.globalMeta.RolloverTarget = 0
	return s.writeGlobalMeta()
}

// cleanOrphanedPartitions removes partition directories that are not in the
// active PartitionOrder. This handles leftover directories from incomplete
// operations that weren't covered by rollover recovery.
func (s *Store) cleanOrphanedPartitions() error {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return fmt.Errorf("failed to read store directory: %w", err)
	}

	// Build set of active partition IDs
	active := make(map[uint32]bool)
	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		active[uint32(s.globalMeta.PartitionOrder[i])] = true
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var id uint32
		if n, _ := fmt.Sscanf(entry.Name(), "partition-%d", &id); n == 1 {
			if !active[id] {
				deletePartition(s.path, id)
			}
		}
	}

	return nil
}

// V2 Partition Recovery

// recoverPartitionsV2 performs crash recovery for V2 partitioned stores.
// Discovers partition directories, validates each, and rebuilds partition order.
func (s *Store) recoverPartitionsV2() error {
	// For each active partition, perform per-partition recovery
	for i := uint8(0); i < s.globalMeta.ActiveCount; i++ {
		partID := uint32(s.globalMeta.PartitionOrder[i])
		p := s.partitions[partID]
		if p == nil {
			continue
		}

		if err := s.recoverPartition(p); err != nil {
			return err
		}
	}

	return nil
}

// recoverPartition performs crash recovery for a single partition.
func (s *Store) recoverPartition(p *Partition) error {
	// Phase 1: Find orphaned head blocks
	orphanedHead, err := s.findOrphanedHeadInPartition(p)
	if err != nil {
		return err
	}
	if orphanedHead != p.meta.HeadBlock {
		p.meta.HeadBlock = orphanedHead
	}

	// Phase 2: Fix write offset
	if err := s.fixWriteOffsetInPartition(p); err != nil {
		return err
	}

	// Phase 3: Update timestamp bounds
	if err := s.updatePartitionTimestampBounds(p); err != nil {
		return err
	}

	return p.writeMeta()
}

// findOrphanedHeadInPartition finds orphaned blocks in a partition.
func (s *Store) findOrphanedHeadInPartition(p *Partition) (uint32, error) {
	currentHead := p.meta.HeadBlock

	// Check if partition is empty
	headHeader, err := p.readBlockHeader(currentHead)
	if err != nil {
		return currentHead, nil
	}
	if headHeader.DataLen == 0 && headHeader.Flags == 0 {
		return currentHead, nil
	}

	// Scan forward looking for orphaned blocks
	maxScans := p.numBlocks
	for i := uint32(0); i < maxScans; i++ {
		nextBlock := currentHead + 1
		if nextBlock >= p.numBlocks {
			break // V2 partitions don't wrap
		}

		header, err := p.readBlockHeader(nextBlock)
		if err != nil {
			break
		}

		if header.Flags == 0 && header.DataLen == 0 {
			break // Empty block, no orphan
		}

		currentHead = nextBlock
	}

	return currentHead, nil
}

// fixWriteOffsetInPartition recalculates WriteOffset for a partition.
func (s *Store) fixWriteOffsetInPartition(p *Partition) error {
	header, err := p.readBlockHeader(p.meta.HeadBlock)
	if err != nil {
		return nil
	}

	if header.DataLen == 0 && header.Flags == 0 {
		p.meta.WriteOffset = 0
		return nil
	}

	if header.IsPacked() && !header.IsContinuation() {
		p.meta.WriteOffset = block.BlockHeaderSize + header.DataLen
	}

	return nil
}

// updatePartitionTimestampBounds updates min/max timestamps for a partition.
func (s *Store) updatePartitionTimestampBounds(p *Partition) error {
	if p.activeBlockCount() == 0 {
		p.meta.MinTimestamp = 0
		p.meta.MaxTimestamp = 0
		return nil
	}

	// Get min timestamp from first block
	firstEntry, err := p.readIndexEntry(0)
	if err == nil && firstEntry.Timestamp > 0 {
		p.meta.MinTimestamp = firstEntry.Timestamp
	}

	// Get max timestamp from head block's last object
	headEntry, err := p.readIndexEntry(p.meta.HeadBlock)
	if err == nil && headEntry.Timestamp > 0 {
		// Scan for last object timestamp
		lastTs := headEntry.Timestamp
		offset := uint32(block.BlockHeaderSize)
		for offset < p.meta.WriteOffset {
			objHeader, err := p.readObjectHeader(p.meta.HeadBlock, offset)
			if err != nil || objHeader.Timestamp == 0 {
				break
			}
			lastTs = objHeader.Timestamp
			if objHeader.NextOffset == 0 || objHeader.IsLastInBlock() {
				break
			}
			offset = objHeader.NextOffset
		}
		p.meta.MaxTimestamp = lastTs
	}

	return nil
}
