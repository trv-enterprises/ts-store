// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package block defines the core block structures for the circular time series store.
package block

import (
	"encoding/binary"
	"errors"
	"time"
)

var (
	ErrInvalidBlockSize = errors.New("block size must be a power of 2 and at least 64 bytes")
)

// BlockHeader is stored at the beginning of each data block.
// Total size: 32 bytes (fixed)
type BlockHeader struct {
	Timestamp int64  // Unix nanoseconds
	BlockNum  uint32 // Block number in circle (0 to NumBlocks-1)
	DataLen   uint32 // Actual data length in this block
	Flags     uint32 // Block flags (free, primary)
	NextFree  uint32 // Next block in free list (only valid if on free list)
	Reserved  uint32 // Padding/future use
}

const (
	BlockHeaderSize = 32
	MinBlockSize    = 64 // Minimum block size (must fit header + some data)

	// Block flags
	FlagFree    uint32 = 1 << 0 // Block is on free list
	FlagPrimary uint32 = 1 << 1 // Block is a primary circular block
)

// IndexEntry represents one entry in the circular index.
// Total size: 16 bytes
type IndexEntry struct {
	Timestamp int64  // Unix nanoseconds (0 if slot is empty/reclaimed)
	BlockNum  uint32 // Block number
	Reserved  uint32 // For alignment and future use
}

const IndexEntrySize = 16

// ValidateBlockSize checks if size is a power of 2 and >= MinBlockSize
func ValidateBlockSize(size uint32) error {
	if size < MinBlockSize {
		return ErrInvalidBlockSize
	}
	// Check power of 2: only one bit set
	if size&(size-1) != 0 {
		return ErrInvalidBlockSize
	}
	return nil
}

// UsableDataSize returns how much user data fits in a data block
func UsableDataSize(blockSize uint32) uint32 {
	return blockSize - BlockHeaderSize
}

// IndexEntriesPerBlock returns how many index entries fit in one index block
func IndexEntriesPerBlock(indexBlockSize uint32) uint32 {
	return indexBlockSize / IndexEntrySize
}

// Encode serializes a BlockHeader to bytes
func (h *BlockHeader) Encode(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(h.Timestamp))
	binary.LittleEndian.PutUint32(buf[8:12], h.BlockNum)
	binary.LittleEndian.PutUint32(buf[12:16], h.DataLen)
	binary.LittleEndian.PutUint32(buf[16:20], h.Flags)
	binary.LittleEndian.PutUint32(buf[20:24], h.NextFree)
	binary.LittleEndian.PutUint32(buf[24:28], h.Reserved)
	// bytes 28-31 padding
}

// Decode deserializes bytes into a BlockHeader
func (h *BlockHeader) Decode(buf []byte) {
	h.Timestamp = int64(binary.LittleEndian.Uint64(buf[0:8]))
	h.BlockNum = binary.LittleEndian.Uint32(buf[8:12])
	h.DataLen = binary.LittleEndian.Uint32(buf[12:16])
	h.Flags = binary.LittleEndian.Uint32(buf[16:20])
	h.NextFree = binary.LittleEndian.Uint32(buf[20:24])
	h.Reserved = binary.LittleEndian.Uint32(buf[24:28])
}

// Encode serializes an IndexEntry to bytes
func (e *IndexEntry) Encode(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(e.Timestamp))
	binary.LittleEndian.PutUint32(buf[8:12], e.BlockNum)
	binary.LittleEndian.PutUint32(buf[12:16], e.Reserved)
}

// Decode deserializes bytes into an IndexEntry
func (e *IndexEntry) Decode(buf []byte) {
	e.Timestamp = int64(binary.LittleEndian.Uint64(buf[0:8]))
	e.BlockNum = binary.LittleEndian.Uint32(buf[8:12])
	e.Reserved = binary.LittleEndian.Uint32(buf[12:16])
}

// Time returns the timestamp as a time.Time
func (h *BlockHeader) Time() time.Time {
	return time.Unix(0, h.Timestamp)
}

// Time returns the timestamp as a time.Time
func (e *IndexEntry) Time() time.Time {
	return time.Unix(0, e.Timestamp)
}

// IsFree returns true if block is on free list
func (h *BlockHeader) IsFree() bool {
	return h.Flags&FlagFree != 0
}

// IsPrimary returns true if block is a primary circular block
func (h *BlockHeader) IsPrimary() bool {
	return h.Flags&FlagPrimary != 0
}
