// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package store implements the circular time series store.
package store

import (
	"errors"

	"github.com/tviviano/ts-store/pkg/block"
)

var (
	ErrInvalidNumBlocks = errors.New("number of blocks must be greater than 0")
	ErrNameRequired     = errors.New("store name is required")
	ErrPathRequired     = errors.New("store path is required")
)

// Config defines the configuration for creating a new store.
type Config struct {
	Name           string // Unique name for this store
	Path           string // Directory path where store files will be created
	NumBlocks      uint32 // Number of primary blocks in the circular buffer
	DataBlockSize  uint32 // Size of each data block (must be power of 2, >= 64)
	IndexBlockSize uint32 // Size of each index block (must be power of 2, >= 64)
}

// DefaultConfig returns a Config with sensible defaults.
// Name and Path must still be set.
func DefaultConfig() Config {
	return Config{
		NumBlocks:      1024,  // 1K primary blocks
		DataBlockSize:  4096,  // 4KB data blocks
		IndexBlockSize: 4096,  // 4KB index blocks
	}
}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	if c.Name == "" {
		return ErrNameRequired
	}
	if c.Path == "" {
		return ErrPathRequired
	}
	if c.NumBlocks == 0 {
		return ErrInvalidNumBlocks
	}
	if err := block.ValidateBlockSize(c.DataBlockSize); err != nil {
		return err
	}
	if err := block.ValidateBlockSize(c.IndexBlockSize); err != nil {
		return err
	}
	return nil
}

// DataFileSize returns the total size of the data file in bytes.
// This includes space for primary blocks plus reserved space for attached blocks.
// Attached blocks are allocated from a pool that can grow up to 2x the primary blocks.
func (c *Config) DataFileSize() int64 {
	// Primary blocks + equal number of potential attached blocks
	totalBlocks := int64(c.NumBlocks) * 2
	return totalBlocks * int64(c.DataBlockSize)
}

// IndexFileSize returns the total size of the index file in bytes.
func (c *Config) IndexFileSize() int64 {
	entriesPerBlock := block.IndexEntriesPerBlock(c.IndexBlockSize)
	// Number of index blocks needed to hold all entries
	numIndexBlocks := (int64(c.NumBlocks) + int64(entriesPerBlock) - 1) / int64(entriesPerBlock)
	return numIndexBlocks * int64(c.IndexBlockSize)
}

// UsableDataPerBlock returns bytes available for user data in each data block.
func (c *Config) UsableDataPerBlock() uint32 {
	return block.UsableDataSize(c.DataBlockSize)
}

// IndexEntriesPerBlock returns how many index entries fit in one index block.
func (c *Config) IndexEntriesPerBlock() uint32 {
	return block.IndexEntriesPerBlock(c.IndexBlockSize)
}
