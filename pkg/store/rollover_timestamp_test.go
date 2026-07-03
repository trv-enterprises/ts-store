// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"testing"
)

// Regression test for issue #16: after rollover recovery creates a fresh,
// empty partition as current, the newest-timestamp lookup returned
// ErrEmptyStore even though sealed partitions held data — so PutObject
// skipped the out-of-order check and accepted timestamps older than
// existing data, breaking the global ordering invariant that range scans
// and partition binary search depend on.
func TestMonotonicityEnforcedAfterRolloverRecovery(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Name = "rollover-monotonic"
	cfg.Path = tmpDir
	cfg.NumBlocks = 8
	cfg.NumPartitions = 3

	s, err := Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Seed data in partition 0
	for ts := int64(1000); ts <= 5000; ts += 1000 {
		if _, err := s.PutObject(ts, []byte("record")); err != nil {
			t.Fatalf("PutObject %d: %v", ts, err)
		}
	}

	// Simulate a crash mid-rollover: phase persisted as "creating", daemon
	// dies before the new partition exists. On reopen, recovery creates a
	// fresh empty partition and makes it current.
	s.globalMeta.RolloverPhase = RolloverPhaseCreating
	s.globalMeta.RolloverTarget = 1
	if err := s.writeGlobalMeta(); err != nil {
		t.Fatalf("writeGlobalMeta: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(tmpDir, cfg.Name)
	if err != nil {
		t.Fatalf("Open after simulated crash: %v", err)
	}
	defer s.Close()

	// Sanity: recovery must have made the empty partition current
	if s.currentPartition == nil || !s.currentPartition.isEmpty() {
		t.Fatal("expected an empty current partition after rollover recovery")
	}

	// A timestamp older than data in the sealed partition must be rejected
	if _, err := s.PutObject(3000, []byte("stale")); !errors.Is(err, ErrTimestampOutOfOrder) {
		t.Fatalf("PutObject with old timestamp: got %v, want ErrTimestampOutOfOrder", err)
	}

	// A genuinely newer timestamp must still be accepted
	if _, err := s.PutObject(6000, []byte("fresh")); err != nil {
		t.Fatalf("PutObject with new timestamp: %v", err)
	}

	// And the full timeline must remain ordered and complete
	handles, err := s.GetObjectsInRange(1, 10000, 0)
	if err != nil {
		t.Fatalf("GetObjectsInRange: %v", err)
	}
	want := []int64{1000, 2000, 3000, 4000, 5000, 6000}
	if len(handles) != len(want) {
		t.Fatalf("expected %d records, got %d", len(want), len(handles))
	}
	for i, h := range handles {
		if h.Timestamp != want[i] {
			t.Errorf("record %d: timestamp %d, want %d", i, h.Timestamp, want[i])
		}
	}
}
