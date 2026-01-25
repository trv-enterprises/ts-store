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
// Total size: 24 bytes (fixed)
//
// The block number is not stored - it's calculated from file offset.
// The next block in a spanning object is always (current + 1) % numBlocks.
type BlockHeader struct {
	Timestamp int64  // Unix nanoseconds
	DataLen   uint32 // Actual data length in this block
	Flags     uint32 // Block flags (primary, packed, continuation)
	Reserved  uint64 // Padding/future use (maintains 8-byte alignment)
}

const (
	BlockHeaderSize = 24
	MinBlockSize    = 64 // Minimum block size (must fit header + some data)

	// Block flags
	FlagPrimary      uint32 = 1 << 0 // Block is a primary circular block (starts an object or packed block)
	FlagPacked       uint32 = 1 << 1 // Block contains packed objects (V2 format)
	FlagContinuation uint32 = 1 << 2 // Block is continuation of spanning object
)

// ObjectHeader is stored before each object's data within the block data area.
// Used for packed blocks (V2 format) where multiple objects can share a block.
// Total size: 24 bytes
type ObjectHeader struct {
	Timestamp  int64  // Unix nanoseconds for this object
	DataLen    uint32 // Length of this object's data (total size if spanning)
	Flags      uint32 // Object flags
	NextOffset uint32 // Offset to next object in block (0 = last or continuation)
	Reserved   uint32 // Alignment/future use
}

const (
	ObjectHeaderSize = 24

	// Object flags
	ObjFlagContinuation uint32 = 1 << 0 // Object data continues from previous block
	ObjFlagContinues    uint32 = 1 << 1 // Object data continues in next block
	ObjFlagLastInBlock  uint32 = 1 << 2 // Last object header in this block
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
	binary.LittleEndian.PutUint32(buf[8:12], h.DataLen)
	binary.LittleEndian.PutUint32(buf[12:16], h.Flags)
	binary.LittleEndian.PutUint64(buf[16:24], h.Reserved)
}

// Decode deserializes bytes into a BlockHeader
func (h *BlockHeader) Decode(buf []byte) {
	h.Timestamp = int64(binary.LittleEndian.Uint64(buf[0:8]))
	h.DataLen = binary.LittleEndian.Uint32(buf[8:12])
	h.Flags = binary.LittleEndian.Uint32(buf[12:16])
	h.Reserved = binary.LittleEndian.Uint64(buf[16:24])
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

// IsPrimary returns true if block is a primary circular block
func (h *BlockHeader) IsPrimary() bool {
	return h.Flags&FlagPrimary != 0
}

// IsPacked returns true if block uses V2 packed format
func (h *BlockHeader) IsPacked() bool {
	return h.Flags&FlagPacked != 0
}

// IsContinuation returns true if block is a continuation of a spanning object
func (h *BlockHeader) IsContinuation() bool {
	return h.Flags&FlagContinuation != 0
}

// Encode serializes an ObjectHeader to bytes
func (o *ObjectHeader) Encode(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(o.Timestamp))
	binary.LittleEndian.PutUint32(buf[8:12], o.DataLen)
	binary.LittleEndian.PutUint32(buf[12:16], o.Flags)
	binary.LittleEndian.PutUint32(buf[16:20], o.NextOffset)
	binary.LittleEndian.PutUint32(buf[20:24], o.Reserved)
}

// Decode deserializes bytes into an ObjectHeader
func (o *ObjectHeader) Decode(buf []byte) {
	o.Timestamp = int64(binary.LittleEndian.Uint64(buf[0:8]))
	o.DataLen = binary.LittleEndian.Uint32(buf[8:12])
	o.Flags = binary.LittleEndian.Uint32(buf[12:16])
	o.NextOffset = binary.LittleEndian.Uint32(buf[16:20])
	o.Reserved = binary.LittleEndian.Uint32(buf[20:24])
}

// Time returns the timestamp as a time.Time
func (o *ObjectHeader) Time() time.Time {
	return time.Unix(0, o.Timestamp)
}

// IsContinuation returns true if object data continues from previous block
func (o *ObjectHeader) IsContinuation() bool {
	return o.Flags&ObjFlagContinuation != 0
}

// Continues returns true if object data continues in next block
func (o *ObjectHeader) Continues() bool {
	return o.Flags&ObjFlagContinues != 0
}

// IsLastInBlock returns true if this is the last object header in the block
func (o *ObjectHeader) IsLastInBlock() bool {
	return o.Flags&ObjFlagLastInBlock != 0
}
