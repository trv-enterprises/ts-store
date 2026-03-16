// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
)

// V2 partitioned storage magic number and version
const (
	magicNumberV2 uint64 = 0x545353544F524532 // "TSSTORE2"
	versionV2     uint32 = 2

	// Metadata sizes
	globalMetadataSize    = 128
	partitionMetadataSize = 64

	// Maximum number of partitions supported
	maxPartitions = 16

	// Default partition configuration
	defaultNumPartitions = 6
)

// GlobalMetadata is persisted to meta.tsdb for V2 partitioned stores.
// Total size: 128 bytes
type GlobalMetadata struct {
	Magic            uint64    // "TSSTORE2" (0x545353544F524532)
	Version          uint32    // 2
	NumPartitions    uint32    // Number of partitions (default: 6)
	BlocksPerPart    uint32    // Blocks per partition
	BlockSize        uint32    // Data block size
	DataType         DataType  // Type of data stored
	CurrentPartition uint32    // Partition being written to (0-based)
	OldestPartition  uint32    // Will be deleted first (0-based)
	PartitionOrder   [16]uint8 // Ordered list of partition IDs (oldest→newest)
	ActiveCount      uint8     // Number of active partitions (1-16)
	Reserved         [46]byte  // Reserved for future use
}

// PartitionMetadata is persisted to each partition's meta.tsdb file.
// Total size: 64 bytes
type PartitionMetadata struct {
	PartitionID  uint32 // Partition ID (matches directory name)
	NumBlocks    uint32 // Total blocks in this partition
	HeadBlock    uint32 // Current head block (newest)
	WriteOffset  uint32 // Current write position within head block
	MinTimestamp int64  // Oldest timestamp in partition
	MaxTimestamp int64  // Newest timestamp in partition
	ObjectCount  uint64 // Number of objects in partition
	BytesUsed    uint64 // Total bytes used (data + headers)
	Sealed       bool   // True if partition is full (read-only)
	Reserved     [15]byte
}

// EncodeGlobalMetadata serializes GlobalMetadata to bytes.
func (m *GlobalMetadata) Encode() []byte {
	buf := make([]byte, globalMetadataSize)
	binary.LittleEndian.PutUint64(buf[0:8], m.Magic)
	binary.LittleEndian.PutUint32(buf[8:12], m.Version)
	binary.LittleEndian.PutUint32(buf[12:16], m.NumPartitions)
	binary.LittleEndian.PutUint32(buf[16:20], m.BlocksPerPart)
	binary.LittleEndian.PutUint32(buf[20:24], m.BlockSize)
	buf[24] = byte(m.DataType)
	binary.LittleEndian.PutUint32(buf[25:29], m.CurrentPartition)
	binary.LittleEndian.PutUint32(buf[29:33], m.OldestPartition)
	copy(buf[33:49], m.PartitionOrder[:])
	buf[49] = m.ActiveCount
	// bytes 50-127 reserved
	return buf
}

// DecodeGlobalMetadata deserializes bytes into GlobalMetadata.
func DecodeGlobalMetadata(buf []byte) *GlobalMetadata {
	m := &GlobalMetadata{}
	m.Magic = binary.LittleEndian.Uint64(buf[0:8])
	m.Version = binary.LittleEndian.Uint32(buf[8:12])
	m.NumPartitions = binary.LittleEndian.Uint32(buf[12:16])
	m.BlocksPerPart = binary.LittleEndian.Uint32(buf[16:20])
	m.BlockSize = binary.LittleEndian.Uint32(buf[20:24])
	m.DataType = DataType(buf[24])
	m.CurrentPartition = binary.LittleEndian.Uint32(buf[25:29])
	m.OldestPartition = binary.LittleEndian.Uint32(buf[29:33])
	copy(m.PartitionOrder[:], buf[33:49])
	m.ActiveCount = buf[49]
	return m
}

// EncodePartitionMetadata serializes PartitionMetadata to bytes.
func (m *PartitionMetadata) Encode() []byte {
	buf := make([]byte, partitionMetadataSize)
	binary.LittleEndian.PutUint32(buf[0:4], m.PartitionID)
	binary.LittleEndian.PutUint32(buf[4:8], m.NumBlocks)
	binary.LittleEndian.PutUint32(buf[8:12], m.HeadBlock)
	binary.LittleEndian.PutUint32(buf[12:16], m.WriteOffset)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(m.MinTimestamp))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(m.MaxTimestamp))
	binary.LittleEndian.PutUint64(buf[32:40], m.ObjectCount)
	binary.LittleEndian.PutUint64(buf[40:48], m.BytesUsed)
	if m.Sealed {
		buf[48] = 1
	}
	// bytes 49-63 reserved
	return buf
}

// DecodePartitionMetadata deserializes bytes into PartitionMetadata.
func DecodePartitionMetadata(buf []byte) *PartitionMetadata {
	m := &PartitionMetadata{}
	m.PartitionID = binary.LittleEndian.Uint32(buf[0:4])
	m.NumBlocks = binary.LittleEndian.Uint32(buf[4:8])
	m.HeadBlock = binary.LittleEndian.Uint32(buf[8:12])
	m.WriteOffset = binary.LittleEndian.Uint32(buf[12:16])
	m.MinTimestamp = int64(binary.LittleEndian.Uint64(buf[16:24]))
	m.MaxTimestamp = int64(binary.LittleEndian.Uint64(buf[24:32]))
	m.ObjectCount = binary.LittleEndian.Uint64(buf[32:40])
	m.BytesUsed = binary.LittleEndian.Uint64(buf[40:48])
	m.Sealed = buf[48] == 1
	return m
}

// IsV2Store checks if a magic number indicates V2 partitioned storage.
func IsV2Store(magic uint64) bool {
	return magic == magicNumberV2
}

// IsV1Store checks if a magic number indicates V1 circular buffer storage.
func IsV1Store(magic uint64) bool {
	return magic == magicNumber
}

// PartitionStats contains runtime statistics for a single partition.
type PartitionStats struct {
	PartitionID  uint32 `json:"partition_id"`
	NumBlocks    uint32 `json:"num_blocks"`
	HeadBlock    uint32 `json:"head_block"`
	WriteOffset  uint32 `json:"write_offset"`
	MinTimestamp int64  `json:"min_timestamp,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp,omitempty"`
	ObjectCount  uint64 `json:"object_count"`
	BytesUsed    uint64 `json:"bytes_used"`
	Sealed       bool   `json:"sealed"`
}
