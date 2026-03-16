// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper: create a V2 store and fill it to the point of needing a rollover.
func createFilledV2Store(t *testing.T, numPartitions uint32) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Name = "test-store"
	cfg.Path = dir
	cfg.NumBlocks = 4
	cfg.NumPartitions = numPartitions
	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s, dir
}

// helper: write enough data to fill the current partition and trigger a rollover.
func fillPartition(t *testing.T, s *Store) {
	t.Helper()
	data := make([]byte, 100)
	for i := 0; i < 100; i++ {
		ts := time.Now().UnixNano() + int64(i)
		_, err := s.PutObject(ts, data)
		if err != nil {
			return
		}
	}
}

// helper: directly manipulate meta.tsdb to simulate a crash at a given phase.
func setRolloverPhase(t *testing.T, storePath string, phase uint8, target uint32) {
	t.Helper()
	metaPath := filepath.Join(storePath, metaFileName)
	f, err := os.OpenFile(metaPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	defer f.Close()

	buf := make([]byte, globalMetadataSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	buf[50] = phase
	binary.LittleEndian.PutUint32(buf[51:55], target)
	if _, err := f.WriteAt(buf, 0); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync meta: %v", err)
	}
}

// helper: remove a partition from PartitionOrder/ActiveCount in the on-disk metadata.
func removePartitionFromMeta(t *testing.T, storePath string, partID uint32) {
	t.Helper()
	metaPath := filepath.Join(storePath, metaFileName)
	f, err := os.OpenFile(metaPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	defer f.Close()

	buf := make([]byte, globalMetadataSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("read meta: %v", err)
	}

	gm := DecodeGlobalMetadata(buf)

	found := false
	for i := uint8(0); i < gm.ActiveCount; i++ {
		if uint32(gm.PartitionOrder[i]) == partID {
			found = true
			copy(gm.PartitionOrder[i:], gm.PartitionOrder[i+1:])
			gm.PartitionOrder[gm.ActiveCount-1] = 0
			gm.ActiveCount--
			break
		}
	}
	if !found {
		t.Fatalf("partition %d not found in PartitionOrder", partID)
	}

	encoded := gm.Encode()
	if _, err := f.WriteAt(encoded, 0); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync meta: %v", err)
	}
}

func TestRolloverPhase0_BackwardCompat(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)
	ts := time.Now().UnixNano()
	if _, err := s.PutObject(ts, []byte("hello")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	s.Close()

	storePath := filepath.Join(dir, "test-store")

	// Verify rollover phase is 0
	metaPath := filepath.Join(storePath, metaFileName)
	f, err := os.Open(metaPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, globalMetadataSize)
	f.ReadAt(buf, 0)
	f.Close()
	if buf[50] != 0 {
		t.Fatalf("expected phase 0, got %d", buf[50])
	}

	// Reopen
	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()

	handles, err := s2.GetObjectsInRange(ts-1, ts+1, 10)
	if err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 result, got %d", len(handles))
	}
}

func TestRolloverPhase1_CrashAfterDeleteBeforeCreate(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)
	storePath := filepath.Join(dir, "test-store")

	for i := 0; i < 3; i++ {
		fillPartition(t, s)
	}

	stats := s.Stats()
	if stats.ActivePartitions < 3 {
		t.Fatalf("expected 3 active partitions, got %d", stats.ActivePartitions)
	}

	oldestID := uint32(s.globalMeta.PartitionOrder[0])
	currentPartID := s.globalMeta.CurrentPartition
	s.Close()

	// Simulate crash: oldest removed from metadata and deleted, Phase 1 set
	removePartitionFromMeta(t, storePath, oldestID)
	deletePartition(storePath, oldestID)
	setRolloverPhase(t, storePath, RolloverPhaseDeleting, oldestID)

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open after Phase 1 crash: %v", err)
	}
	defer s2.Close()

	if s2.globalMeta.RolloverPhase != RolloverPhaseIdle {
		t.Fatalf("expected Phase 0 after recovery, got %d", s2.globalMeta.RolloverPhase)
	}

	nextID := (currentPartID + 1) % 3
	if s2.partitions[nextID] == nil {
		t.Fatalf("expected partition %d to be created by recovery", nextID)
	}

	ts := time.Now().UnixNano()
	if _, err := s2.PutObject(ts, []byte("after-recovery")); err != nil {
		t.Fatalf("PutObject after recovery: %v", err)
	}
}

func TestRolloverPhase1_DirectoryStillExists(t *testing.T) {
	// Phase 1 committed but directory deletion hasn't happened yet.
	// Recovery should delete it, then complete the rollover.
	s, dir := createFilledV2Store(t, 3)
	storePath := filepath.Join(dir, "test-store")

	for i := 0; i < 3; i++ {
		fillPartition(t, s)
	}

	oldestID := uint32(s.globalMeta.PartitionOrder[0])
	currentPartID := s.globalMeta.CurrentPartition
	s.Close()

	// Simulate: Phase 1 committed, oldest removed from metadata, directory NOT deleted
	removePartitionFromMeta(t, storePath, oldestID)
	setRolloverPhase(t, storePath, RolloverPhaseDeleting, oldestID)

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()

	if s2.globalMeta.RolloverPhase != RolloverPhaseIdle {
		t.Fatalf("expected Phase 0, got %d", s2.globalMeta.RolloverPhase)
	}

	// Recovery deletes the target then creates a new partition.
	// The new partition's ID may reuse oldestID (e.g. with 3 partitions: 0,1,2 → next is 0).
	nextID := (currentPartID + 1) % 3
	if s2.partitions[nextID] == nil {
		t.Fatalf("expected new partition %d to exist after recovery", nextID)
	}

	// If nextID != oldestID, the old directory should be cleaned up
	if nextID != oldestID {
		oldPath := filepath.Join(storePath, partitionDirName(oldestID))
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatalf("expected old partition directory to be removed")
		}
	}

	// Should be able to write
	ts := time.Now().UnixNano()
	if _, err := s2.PutObject(ts, []byte("after-recovery")); err != nil {
		t.Fatalf("PutObject after recovery: %v", err)
	}
}

func TestRolloverPhase2_CrashWithPartialDir(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)
	storePath := filepath.Join(dir, "test-store")

	for i := 0; i < 3; i++ {
		fillPartition(t, s)
	}
	currentPartID := s.globalMeta.CurrentPartition
	oldestID := uint32(s.globalMeta.PartitionOrder[0])
	s.Close()

	nextID := (currentPartID + 1) % 3

	// Simulate Phase 1 done (oldest removed + deleted), now in Phase 2 with partial dir
	removePartitionFromMeta(t, storePath, oldestID)
	deletePartition(storePath, oldestID)

	partialPath := filepath.Join(storePath, partitionDirName(nextID))
	os.MkdirAll(partialPath, 0755)

	setRolloverPhase(t, storePath, RolloverPhaseCreating, nextID)

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open after Phase 2 crash with partial dir: %v", err)
	}
	defer s2.Close()

	if s2.globalMeta.RolloverPhase != RolloverPhaseIdle {
		t.Fatalf("expected Phase 0, got %d", s2.globalMeta.RolloverPhase)
	}

	ts := time.Now().UnixNano()
	if _, err := s2.PutObject(ts, []byte("recovered")); err != nil {
		t.Fatalf("PutObject after recovery: %v", err)
	}
}

func TestRolloverPhase2_CrashWithFullDir(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)
	storePath := filepath.Join(dir, "test-store")

	for i := 0; i < 3; i++ {
		fillPartition(t, s)
	}
	currentPartID := s.globalMeta.CurrentPartition
	oldestID := uint32(s.globalMeta.PartitionOrder[0])
	s.Close()

	nextID := (currentPartID + 1) % 3

	// Simulate Phase 1 done, Phase 2 with fully created partition
	removePartitionFromMeta(t, storePath, oldestID)
	deletePartition(storePath, oldestID)

	p, err := createPartition(storePath, nextID, 4, 4096)
	if err != nil {
		t.Fatalf("createPartition: %v", err)
	}
	p.Close()

	setRolloverPhase(t, storePath, RolloverPhaseCreating, nextID)

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open after Phase 2 crash with full dir: %v", err)
	}
	defer s2.Close()

	if s2.globalMeta.RolloverPhase != RolloverPhaseIdle {
		t.Fatalf("expected Phase 0, got %d", s2.globalMeta.RolloverPhase)
	}
	if s2.currentPartition == nil {
		t.Fatal("expected currentPartition to be set")
	}
	if s2.currentPartition.id != nextID {
		t.Fatalf("expected current partition %d, got %d", nextID, s2.currentPartition.id)
	}
}

func TestRolloverOrphanedPartitionCleanup(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)
	storePath := filepath.Join(dir, "test-store")

	ts := time.Now().UnixNano()
	s.PutObject(ts, []byte("data"))
	s.Close()

	// Create an orphaned partition directory
	orphanID := uint32(15)
	orphanPath := filepath.Join(storePath, partitionDirName(orphanID))
	os.MkdirAll(orphanPath, 0755)
	os.WriteFile(filepath.Join(orphanPath, "junk"), []byte("orphan"), 0644)

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("expected orphaned partition directory to be removed")
	}
}

func TestRolloverEndToEnd_MultipleRollovers(t *testing.T) {
	s, dir := createFilledV2Store(t, 3)

	var writtenTimestamps []int64
	data := make([]byte, 100)
	baseTs := time.Now().UnixNano()

	for i := 0; i < 200; i++ {
		ts := baseTs + int64(i)*1000
		_, err := s.PutObject(ts, data)
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
		writtenTimestamps = append(writtenTimestamps, ts)
	}

	s.Close()

	s2, err := Open(dir, "test-store")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s2.Close()

	handles, err := s2.GetObjectsInRange(baseTs-1, baseTs+int64(200)*1000+1, 1000)
	if err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}

	if len(handles) == 0 {
		t.Fatal("expected some results after multiple rollovers")
	}

	writtenSet := make(map[int64]bool)
	for _, ts := range writtenTimestamps {
		writtenSet[ts] = true
	}
	for _, h := range handles {
		if !writtenSet[h.Timestamp] {
			t.Fatalf("unexpected timestamp %d in results", h.Timestamp)
		}
	}

	// Confirm store is functional after reopen
	newTs := baseTs + 999999
	if _, err := s2.PutObject(newTs, []byte("post-reopen")); err != nil {
		t.Fatalf("PutObject after reopen: %v", err)
	}
}

func TestRolloverPhaseEncodeDecode(t *testing.T) {
	gm := &GlobalMetadata{
		Magic:          magicNumberV2,
		Version:        versionV2,
		NumPartitions:  6,
		BlocksPerPart:  10,
		BlockSize:      4096,
		ActiveCount:    3,
		RolloverPhase:  RolloverPhaseCreating,
		RolloverTarget: 5,
	}

	buf := gm.Encode()
	decoded := DecodeGlobalMetadata(buf)

	if decoded.RolloverPhase != RolloverPhaseCreating {
		t.Fatalf("expected phase %d, got %d", RolloverPhaseCreating, decoded.RolloverPhase)
	}
	if decoded.RolloverTarget != 5 {
		t.Fatalf("expected target 5, got %d", decoded.RolloverTarget)
	}

	// Backward compat: all zeros = Phase 0
	gm2 := &GlobalMetadata{
		Magic:         magicNumberV2,
		Version:       versionV2,
		NumPartitions: 6,
	}
	buf2 := gm2.Encode()
	decoded2 := DecodeGlobalMetadata(buf2)
	if decoded2.RolloverPhase != RolloverPhaseIdle {
		t.Fatalf("expected idle phase for zero bytes, got %d", decoded2.RolloverPhase)
	}
	if decoded2.RolloverTarget != 0 {
		t.Fatalf("expected target 0 for zero bytes, got %d", decoded2.RolloverTarget)
	}
}
