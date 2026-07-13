// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tviviano/ts-store/pkg/store"
)

// pollRecord is one scanned record, read and parsed once, fanned out to
// every registered worker. The parsed map is shared read-only: rule
// evaluation and sink payload marshalling never mutate it. parsed is nil
// for records that failed to unmarshal — they still advance cursors.
type pollRecord struct {
	ts     int64
	parsed map[string]interface{}
}

// poller runs ONE poll loop for all alert workers on a store (issue #4).
// Previously each worker ran its own ticker and range scan, so N alerts
// cost N scans (and N reads + N unmarshals of the same records) per tick.
// The poller scans once from the minimum of the workers' cursors and
// fans the batch out; each worker's own cursor filter keeps overlapping
// ranges idempotent.
//
// The loop starts lazily on the first register and then runs until stop.
// A tick with no registered workers returns before touching the store,
// so an idle store with no alerts costs one timer wake-up per second.
type poller struct {
	store     *store.Store
	storeName string

	mu       sync.Mutex
	workers  map[string]*Worker
	lastTs   int64 // shared scan cursor
	interval time.Duration
	running  bool
	stopped  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

func newPoller(st *store.Store, storeName string) *poller {
	return &poller{
		store:     st,
		storeName: storeName,
		workers:   make(map[string]*Worker),
		interval:  defaultPollInterval,
		stopCh:    make(chan struct{}),
	}
}

// register adds a started worker to the fan-out set. If the worker's
// cursor is behind the shared scan position (a resume-policy alert with
// a persisted cursor), the scan is pulled back so its backlog gets
// replayed — workers already past that range skip the re-delivered
// records via their own cursors.
func (p *poller) register(w *Worker) {
	floor := w.lastTimestamp()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if len(p.workers) == 0 || floor < p.lastTs {
		p.lastTs = floor
	}
	p.workers[w.id] = w
	p.recomputeIntervalLocked()
	if !p.running {
		p.running = true
		p.wg.Add(1)
		go p.run()
	}
}

// unregister removes a worker from the fan-out set. The loop keeps
// ticking (cheaply) so register/unregister churn never races a loop
// restart; it only exits on stop.
func (p *poller) unregister(alertID string) {
	p.mu.Lock()
	delete(p.workers, alertID)
	p.recomputeIntervalLocked()
	p.mu.Unlock()
}

// stop terminates the loop and waits for it. Idempotent.
func (p *poller) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	p.mu.Unlock()
	p.wg.Wait()
}

// recomputeIntervalLocked sets the tick to the minimum poll interval
// across registered workers (each alert's poll_interval is a hint; the
// fastest one wins for the whole store). With no workers the loop falls
// back to the default so empty ticks stay rare and cheap. Caller holds
// p.mu.
func (p *poller) recomputeIntervalLocked() {
	interval := defaultPollInterval
	first := true
	for _, w := range p.workers {
		if first || w.pollInterval < interval {
			interval = w.pollInterval
			first = false
		}
	}
	p.interval = interval
}

func (p *poller) run() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		d := p.interval
		p.mu.Unlock()
		select {
		case <-p.stopCh:
			return
		case <-time.After(d):
			p.pollOnce()
		}
	}
}

// pollOnce performs the single shared scan and fan-out for one tick.
func (p *poller) pollOnce() {
	p.mu.Lock()
	lastTs := p.lastTs
	workers := make([]*Worker, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()
	if len(workers) == 0 {
		return
	}

	// Idle early-out (issue #57): the newest timestamp is O(partitions)
	// of metadata, while the range read block-scans under RLock. Skip
	// the scan when nothing was written since the shared cursor. Any
	// error falls through to the range read to be surfaced.
	if newestTs, err := p.store.GetNewestTimestamp(); err == store.ErrEmptyStore {
		p.noteSuccessAll(workers)
		return
	} else if err == nil && newestTs <= lastTs {
		p.noteSuccessAll(workers)
		return
	}

	endTime := time.Now().UnixNano()
	handles, err := p.store.GetObjectsInRange(lastTs+1, endTime, maxBatchSize)
	if err != nil {
		msg := fmt.Sprintf("range read: %v", err)
		for _, w := range workers {
			w.setError(msg)
		}
		return
	}

	recs := make([]pollRecord, 0, len(handles))
	for _, h := range handles {
		lastTs = h.Timestamp
		data, err := p.store.GetObject(h)
		if err != nil {
			// Keep advancing past bad records — a single corrupt record
			// must not stall alert evaluation forever. A nil parsed map
			// still moves every worker's cursor.
			recs = append(recs, pollRecord{ts: h.Timestamp})
			continue
		}

		// Expand schema data once so condition fields match the
		// schema-defined names rather than positional indices.
		jsonData := data
		if p.store.DataType() == store.DataTypeSchema {
			if expanded, expErr := p.store.ExpandData(data, 0); expErr == nil {
				jsonData = expanded
			}
		}

		rec := pollRecord{ts: h.Timestamp}
		var parsed map[string]interface{}
		if json.Unmarshal(jsonData, &parsed) == nil {
			rec.parsed = parsed
		}
		recs = append(recs, rec)
	}

	for _, w := range workers {
		w.deliverBatch(recs)
	}

	p.mu.Lock()
	if lastTs > p.lastTs {
		p.lastTs = lastTs
	}
	p.mu.Unlock()
	p.noteSuccessAll(workers)
}

func (p *poller) noteSuccessAll(workers []*Worker) {
	for _, w := range workers {
		w.noteSuccess()
	}
}
