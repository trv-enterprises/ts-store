// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"sync"
	"sync/atomic"
	"time"
)

// storeMetrics holds the per-store I/O counters surfaced via the /metrics
// endpoint. All counters are accessed via atomic operations so the hot
// path stays lock-free. The since timestamp is protected by mu because
// it's read+rewritten as a unit on Reset.
//
// The zero value is ready to use: ZeroSince() must be called once at
// construction (via Open/Create) to set since to the daemon-start time.
type storeMetrics struct {
	writes       atomic.Int64 // PutObject calls
	bytesWritten atomic.Int64 // total bytes of object payloads written
	reads        atomic.Int64 // GetObject / GetObjectByTime / GetObjectsInRange / GetOldestObjects / GetNewestObjects
	bytesRead    atomic.Int64 // total bytes of object payloads returned to callers via GetObject

	mu    sync.RWMutex
	since time.Time
}

// recordWrite is called by PutObject after a successful write.
func (m *storeMetrics) recordWrite(payloadBytes int) {
	m.writes.Add(1)
	if payloadBytes > 0 {
		m.bytesWritten.Add(int64(payloadBytes))
	}
}

// recordRead is called by GetObject after a successful read.
func (m *storeMetrics) recordRead(payloadBytes int) {
	m.reads.Add(1)
	if payloadBytes > 0 {
		m.bytesRead.Add(int64(payloadBytes))
	}
}

// recordRangeRead is called by the range/oldest/newest readers — these
// return handles, not payloads, so byte accounting only credits when the
// caller subsequently calls GetObject. We still bump the read count once
// per range call so users see range-scan pressure.
func (m *storeMetrics) recordRangeRead() {
	m.reads.Add(1)
}

// reset zeros the counters and updates the since timestamp to now.
func (m *storeMetrics) reset() {
	m.writes.Store(0)
	m.bytesWritten.Store(0)
	m.reads.Store(0)
	m.bytesRead.Store(0)
	m.mu.Lock()
	m.since = time.Now().UTC()
	m.mu.Unlock()
}

// snapshot returns a point-in-time copy. Each counter is sampled
// independently, so the snapshot is only approximately consistent — this
// is fine for a monitoring endpoint where the values are advancing
// constantly anyway.
func (m *storeMetrics) snapshot() StoreMetrics {
	m.mu.RLock()
	since := m.since
	m.mu.RUnlock()
	return StoreMetrics{
		Writes:       m.writes.Load(),
		BytesWritten: m.bytesWritten.Load(),
		Reads:        m.reads.Load(),
		BytesRead:    m.bytesRead.Load(),
		Since:        since,
	}
}

// StoreMetrics is the JSON-exposed shape of the I/O counters.
type StoreMetrics struct {
	Writes       int64     `json:"writes"`
	BytesWritten int64     `json:"bytes_written"`
	Reads        int64     `json:"reads"`
	BytesRead    int64     `json:"bytes_read"`
	Since        time.Time `json:"since"`
}

// initMetrics seeds the counters' since timestamp to now. Called by
// every Store constructor (createV2, openV2) so the /metrics endpoint
// reports a sensible "since" value from process start.
func (s *Store) initMetrics() {
	s.metrics.mu.Lock()
	s.metrics.since = time.Now().UTC()
	s.metrics.mu.Unlock()
}

// Metrics returns a snapshot of the store's I/O counters.
func (s *Store) Metrics() StoreMetrics {
	return s.metrics.snapshot()
}

// ResetMetrics zeros the I/O counters and advances "since" to now.
func (s *Store) ResetMetrics() {
	s.metrics.reset()
}
