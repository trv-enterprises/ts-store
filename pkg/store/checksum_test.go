// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tviviano/ts-store/pkg/block"
)

func newChecksumTestStore(t *testing.T, blockSize uint32) *Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Name = "crc"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 32
	cfg.DataBlockSize = blockSize
	cfg.DataType = DataTypeJSON
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// corruptDataFile flips one byte in partition-0's data file at offset.
func corruptDataFile(t *testing.T, s *Store, offset int64) {
	t.Helper()
	path := filepath.Join(s.StorePath(), "partition-0", "data.tsdb")
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open data file: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, offset); err != nil {
		t.Fatalf("read byte: %v", err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, offset); err != nil {
		t.Fatalf("write byte: %v", err)
	}
}

// TestChecksumRoundTrip: packed appends and a spanning object all read
// back cleanly with checksums stamped and verified (issue #58).
func TestChecksumRoundTrip(t *testing.T) {
	s := newChecksumTestStore(t, 256)

	// Several small objects (packed appends into shared blocks)…
	var handles []*ObjectHandle
	for i := 0; i < 10; i++ {
		h, err := s.PutObject(int64(1000+i), []byte(`{"v":`+strings.Repeat("1", i+1)+`}`))
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
		handles = append(handles, h)
	}
	// …and one spanning object (larger than a 256-byte block).
	big := []byte(`{"big":"` + strings.Repeat("x", 700) + `"}`)
	hBig, err := s.PutObject(5000, big)
	if err != nil {
		t.Fatalf("PutObject big: %v", err)
	}

	for i, h := range handles {
		if _, err := s.GetObject(h); err != nil {
			t.Errorf("GetObject %d: %v", i, err)
		}
	}
	got, err := s.GetObject(hBig)
	if err != nil {
		t.Fatalf("GetObject big: %v", err)
	}
	if string(got) != string(big) {
		t.Error("spanning object did not round-trip")
	}

	// Every non-empty block header carries a checksum.
	p := s.partitions[hBig.PartitionID]
	for b := uint32(0); b <= p.meta.HeadBlock; b++ {
		hdr, err := p.readBlockHeader(b)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.DataLen > 0 && !hdr.HasChecksum() {
			t.Errorf("block %d has data but no checksum", b)
		}
	}
}

// TestChecksumDetectsPayloadCorruption: a flipped payload byte turns
// reads into ErrCorruptBlock instead of returning garbage.
func TestChecksumDetectsPayloadCorruption(t *testing.T) {
	s := newChecksumTestStore(t, 256)
	h, err := s.PutObject(1000, []byte(`{"v":12345}`))
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Flip a byte inside the object's data area.
	off := int64(h.BlockNum)*256 + int64(h.Offset) + block.ObjectHeaderSize + 5
	corruptDataFile(t, s, off)

	if _, err := s.GetObject(h); !errors.Is(err, ErrCorruptBlock) {
		t.Errorf("GetObject on corrupt block: err = %v, want ErrCorruptBlock", err)
	}
}

// TestLegacyBlockWithoutChecksumStillReads: a block whose header has
// Reserved == 0 (written before checksumming existed) reads without
// verification — old stores need no migration.
func TestLegacyBlockWithoutChecksumStillReads(t *testing.T) {
	s := newChecksumTestStore(t, 256)
	h, err := s.PutObject(1000, []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Zero the header's Reserved field to simulate a pre-#58 block.
	path := filepath.Join(s.StorePath(), "partition-0", "data.tsdb")
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	zero := make([]byte, 8)
	binary.LittleEndian.PutUint64(zero, 0)
	if _, err := f.WriteAt(zero, int64(h.BlockNum)*256+16); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := s.GetObject(h); err != nil {
		t.Errorf("legacy block should read without verification: %v", err)
	}
}

// TestRecoveryWipesCorruptHead: on reopen, a checksummed head block that
// fails verification is discarded and the head rolled back, instead of
// recovery following its header blindly.
func TestRecoveryWipesCorruptHead(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Name = "crcrec"
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 32
	cfg.DataBlockSize = 256
	cfg.DataType = DataTypeJSON
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Two objects in two separate blocks (fill block 0 enough that the
	// second write opens block 1).
	pad := strings.Repeat("a", 150)
	h1, err := s.PutObject(1000, []byte(`{"one":"`+pad+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.PutObject(2000, []byte(`{"two":"`+pad+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if h1.BlockNum == h2.BlockNum {
		t.Fatalf("test setup: objects share block %d; need separate blocks", h1.BlockNum)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the head block's payload on disk.
	path := filepath.Join(cfg.Path, "crcrec", "partition-0", "data.tsdb")
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte{0xFF}
	if _, err := f.WriteAt(buf, int64(h2.BlockNum)*256+block.BlockHeaderSize+block.ObjectHeaderSize+3); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Reopen: recovery must discard the corrupt head and keep the
	// earlier block readable.
	s2, err := Open(cfg.Path, "crcrec")
	if err != nil {
		t.Fatalf("Open after corruption: %v", err)
	}
	defer s2.Close()

	if _, err := s2.GetObject(h1); err != nil {
		t.Errorf("intact block should still read after recovery: %v", err)
	}
	stats := s2.Stats()
	if stats.NewestTimestamp >= 2000 {
		t.Errorf("corrupt head should have been discarded; newest = %d", stats.NewestTimestamp)
	}
}
