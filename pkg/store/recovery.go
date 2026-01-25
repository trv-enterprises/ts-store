// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package store

import (
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
