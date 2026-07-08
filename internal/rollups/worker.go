// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rollups

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/pkg/store"
)

const (
	defaultPollInterval = 30 * time.Second
	// maxWindowsPerTick caps how many closed windows one tick processes so a
	// cold start over a long backlog doesn't block the loop; the rest resume
	// on the next tick.
	maxWindowsPerTick = 500
)

// Status is the runtime status of a rollup worker.
type Status struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	SourceStore   string `json:"source_store"`
	TargetStore   string `json:"target_store"`
	Window        string `json:"window"`
	LastWindowEnd int64  `json:"last_window_end,omitempty"`
	WindowsWritten int64 `json:"windows_written"`

	// State is "running", "stopped", or "error". "error" means the most
	// recent rollup pass failed; the next clean pass restores "running".
	// LastError/LastErrorAt are kept across that recovery as history —
	// state answers "broken now?", last_error_at answers "when did it
	// last hiccup?".
	State       string    `json:"state"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
}

// Worker rolls one source store's data into one target store, one closed
// window at a time. Owned by the rollups Manager.
type Worker struct {
	mu sync.RWMutex

	id          string
	name        string
	sourceName  string
	targetName  string

	source *store.Store
	target *store.Store

	windowNanos   int64
	windowStr     string // canonical window, e.g. "1h"
	aggConfig     *aggregation.Config
	aggFields     string // raw spec, kept so the config can be re-derived on schema drift
	aggDefault    string
	pollInterval  time.Duration
	restartPolicy string
	cursorPath    string

	lastWindowEnd  int64
	windowsWritten int64
	state          string
	lastError      string
	lastErrorAt    time.Time
	createdAt      time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options collects everything a Worker needs at construction.
type Options struct {
	ID         string
	Name       string
	SourceName string
	TargetName string
	Source     *store.Store
	Target     *store.Store

	WindowDuration string
	AggFields      string
	AggDefault     string
	PollInterval   string
	RestartPolicy  string // "resume" (default) | "now"
	CursorPath     string
	CreatedAt      time.Time
}

// NewWorker builds a Worker, parsing the window and aggregation spec up front
// so creation fails fast on bad config.
func NewWorker(opts Options) (*Worker, error) {
	if opts.Source == nil || opts.Target == nil {
		return nil, fmt.Errorf("rollups.NewWorker: Source and Target are required")
	}

	cw, window, err := canonicalWindow(opts.WindowDuration)
	if err != nil {
		return nil, err
	}

	fields, err := aggregation.ParseFieldAggs(opts.AggFields)
	if err != nil {
		return nil, err
	}
	numericMap := aggregation.BuildNumericMap(opts.Source.GetSchemaSet())
	aggConfig, err := aggregation.NewConfig(window, fields, aggregation.AggFunc(opts.AggDefault), numericMap)
	if err != nil {
		return nil, err
	}

	poll := defaultPollInterval
	if opts.PollInterval != "" {
		d, err := duration.ParseDuration(opts.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", opts.PollInterval, err)
		}
		// A non-positive interval would panic time.NewTicker in the worker
		// goroutine — after the config is already persisted.
		if d <= 0 {
			return nil, fmt.Errorf("invalid poll_interval %q: must be positive", opts.PollInterval)
		}
		poll = d
	}

	restart := opts.RestartPolicy
	if restart == "" {
		restart = "resume"
	}

	return &Worker{
		id:            opts.ID,
		name:          opts.Name,
		sourceName:    opts.SourceName,
		targetName:    opts.TargetName,
		source:        opts.Source,
		target:        opts.Target,
		windowNanos:   window.Nanoseconds(),
		windowStr:     cw,
		aggConfig:     aggConfig,
		aggFields:     opts.AggFields,
		aggDefault:    opts.AggDefault,
		pollInterval:  poll,
		restartPolicy: restart,
		cursorPath:    opts.CursorPath,
		state:         "stopped",
		createdAt:     opts.CreatedAt,
		stopCh:        make(chan struct{}),
	}, nil
}

// Start launches the background loop. The starting cursor depends on
// restart_policy:
//   - "resume" — the max of the persisted cursor and the target store's
//     newest record (backstop against a lost/stale cursor). If neither
//     exists, the first tick backfills from the source's oldest record
//     aligned to a window boundary.
//   - "now" — the current aligned window boundary: no backfill, no
//     cursor. The first window written is the one open right now, once
//     it closes.
func (w *Worker) Start() {
	w.mu.Lock()
	var resume int64
	switch w.restartPolicy {
	case "now":
		// Documented "start from now" semantics: skip every already-
		// closed window regardless of cursor or existing data.
		// Previously the targetNewest backstop (and, on an empty
		// target, rollupOnce's source-oldest fallback) applied
		// unconditionally, making "now" behave like resume/rebuild
		// whenever any data existed (issue #38).
		resume = (time.Now().UnixNano() / w.windowNanos) * w.windowNanos
	default: // "resume"
		resume = readCursor(w.cursorPath)
		if targetNewest := w.targetNewest(); targetNewest > resume {
			resume = targetNewest
		}
	}
	w.lastWindowEnd = resume
	w.state = "running"
	w.mu.Unlock()

	w.wg.Add(1)
	go w.runLoop()
}

// Stop signals the loop to exit and waits. Safe to call multiple times.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.state == "stopped" {
		w.mu.Unlock()
		return
	}
	close(w.stopCh)
	w.mu.Unlock()

	w.wg.Wait()

	w.mu.Lock()
	w.state = "stopped"
	w.mu.Unlock()
}

func (w *Worker) runLoop() {
	defer w.wg.Done()

	// Run once promptly, then on the ticker.
	if err := w.rollupOnce(); err != nil {
		w.setError(err.Error())
	} else {
		w.noteSuccess()
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			if err := w.rollupOnce(); err != nil {
				w.setError(err.Error())
			} else {
				w.noteSuccess()
			}
		}
	}
}

// rollupOnce processes all newly-closed windows since the cursor: for each
// window [wStart, wStart+window) it reads the source records, aggregates them,
// and writes one record to the target stamped at the window END. Windows are
// processed in ascending order so target writes stay strictly increasing.
func (w *Worker) rollupOnce() error {
	w.mu.RLock()
	cursor := w.lastWindowEnd
	w.mu.RUnlock()

	// Persist the cursor once per batch, whatever the exit path (done,
	// error, stop, window cap) — advanceCursor is memory-only. Skipped
	// when nothing advanced, so idle ticks still cost zero file ops.
	startCursor := cursor
	defer func() {
		w.mu.RLock()
		end := w.lastWindowEnd
		w.mu.RUnlock()
		if end != startCursor {
			w.writeCursor(end)
		}
	}()

	now := time.Now().UnixNano()
	// Last fully-closed window boundary (exclusive upper bound on window ends).
	lastClosedEnd := (now / w.windowNanos) * w.windowNanos

	nextStart := cursor
	if nextStart == 0 {
		oldest := w.sourceOldest()
		if oldest <= 0 {
			return nil // source empty; nothing to roll up yet
		}
		nextStart = (oldest / w.windowNanos) * w.windowNanos
	}

	processed := 0
	for wStart := nextStart; wStart < lastClosedEnd; wStart += w.windowNanos {
		if processed >= maxWindowsPerTick {
			break // resume on next tick
		}
		select {
		case <-w.stopCh:
			return nil
		default:
		}

		windowEnd := wStart + w.windowNanos
		// Inclusive-both-ends range read; windowEnd-1 makes the upper bound
		// EXCLUSIVE of windowEnd, realizing the half-open window [wStart, windowEnd).
		handles, err := w.source.GetObjectsInRange(wStart, windowEnd-1, 0)
		if err != nil {
			return fmt.Errorf("range read [%d,%d): %w", wStart, windowEnd, err)
		}

		if len(handles) == 0 {
			// Empty window: skip (no record). Still advance the cursor so we
			// don't rescan this window forever.
			w.advanceCursor(windowEnd)
			processed++
			continue
		}

		records := make([]aggregation.TimestampedRecord, 0, len(handles))
		for _, h := range handles {
			data, err := w.source.GetObject(h)
			if err != nil {
				continue
			}
			jsonData := data
			if w.source.DataType() == store.DataTypeSchema {
				if expanded, expErr := w.source.ExpandData(data, 0); expErr == nil {
					jsonData = expanded
				}
			}
			var parsed map[string]interface{}
			if json.Unmarshal(jsonData, &parsed) == nil {
				records = append(records, aggregation.TimestampedRecord{Timestamp: h.Timestamp, Data: parsed})
			}
		}

		results := aggregation.AggregateBatch(records, w.aggConfig)
		if len(results) == 0 {
			w.advanceCursor(windowEnd)
			processed++
			continue
		}
		// One window's records yield one result; use the first (defensive).
		res := results[0]
		out := make(map[string]interface{}, len(res.Data)+1)
		for k, v := range res.Data {
			out[k] = v
		}
		out[windowCountField] = res.Count

		payload, err := json.Marshal(out)
		if err != nil {
			return fmt.Errorf("marshal rollup record: %w", err)
		}
		compact, err := w.target.ValidateAndCompact(payload)
		if err != nil {
			// The usual cause: the source schema gained a field after this
			// worker derived its config, so the row now carries a field the
			// target doesn't know. Refresh (append-only) and retry once —
			// otherwise this window errors forever and the rollup is dead
			// until a manual force_recreate.
			if rerr := w.refreshSchemaAndConfig(); rerr != nil {
				return fmt.Errorf("compact rollup record: %w (schema refresh failed: %v)", err, rerr)
			}
			compact, err = w.target.ValidateAndCompact(payload)
			if err != nil {
				return fmt.Errorf("compact rollup record after schema refresh: %w", err)
			}
		}
		if _, err := w.target.PutObject(windowEnd, compact); err != nil {
			return fmt.Errorf("write rollup record @%d: %w", windowEnd, err)
		}

		atomic.AddInt64(&w.windowsWritten, 1)
		w.advanceCursor(windowEnd)
		processed++
	}

	return nil
}

// refreshSchemaAndConfig re-derives the target schema from the source's
// CURRENT schema, extends the target (by name, append-only) with any new
// fields, and rebuilds the aggregation config with a fresh numeric map.
func (w *Worker) refreshSchemaAndConfig() error {
	derived, err := deriveTargetSchema(w.aggFields, w.aggDefault, sourceNumeric(w.source))
	if err != nil {
		return fmt.Errorf("re-derive target schema: %w", err)
	}
	if merged := mergeTargetSchema(w.target, derived); merged != nil {
		if _, err := w.target.SetSchema(merged); err != nil {
			return fmt.Errorf("extend target schema: %w", err)
		}
	}

	fields, err := aggregation.ParseFieldAggs(w.aggFields)
	if err != nil {
		return err
	}
	cfg, err := aggregation.NewConfig(time.Duration(w.windowNanos), fields,
		aggregation.AggFunc(w.aggDefault), aggregation.BuildNumericMap(w.source.GetSchemaSet()))
	if err != nil {
		return err
	}
	w.aggConfig = cfg
	return nil
}

// advanceCursor moves the in-memory cursor only. Persistence happens
// once per rollupOnce batch (see the flush there) — a per-window
// write+rename during a cold backfill was up to 1000 file ops per tick,
// needless flash wear on SD-card devices (issue #44). Crash-safety is
// unchanged in kind: losing an unflushed batch only means re-processing
// those windows on the next start, which rollups already tolerate.
func (w *Worker) advanceCursor(windowEnd int64) {
	w.mu.Lock()
	w.lastWindowEnd = windowEnd
	w.mu.Unlock()
}

func (w *Worker) sourceOldest() int64 {
	return w.source.Stats().OldestTimestamp
}

func (w *Worker) targetNewest() int64 {
	return w.target.Stats().NewestTimestamp
}

// setError records the failure and flips a running worker into the
// "error" state. Only a running worker escalates, so a failure landing
// mid-Stop can't resurrect a stopped worker.
func (w *Worker) setError(msg string) {
	w.mu.Lock()
	w.lastError = msg
	w.lastErrorAt = time.Now().UTC()
	if w.state == "running" {
		w.state = "error"
	}
	w.mu.Unlock()
}

// noteSuccess restores an errored worker to "running" after a clean
// rollup pass. lastError/lastErrorAt are deliberately kept as history so
// a past hiccup stays visible (and datable) after recovery.
func (w *Worker) noteSuccess() {
	w.mu.Lock()
	if w.state == "error" {
		w.state = "running"
	}
	w.mu.Unlock()
}

// writeCursor persists the last written window-end. No-op for "now" policy.
func (w *Worker) writeCursor(ts int64) {
	if w.restartPolicy != "resume" || w.cursorPath == "" {
		return
	}
	tmp := w.cursorPath + ".tmp"
	body := strconv.FormatInt(ts, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		log.Printf("rollup %s: cursor write: %v", w.id, err)
		return
	}
	if err := os.Rename(tmp, w.cursorPath); err != nil {
		log.Printf("rollup %s: cursor rename: %v", w.id, err)
	}
}

func readCursor(path string) int64 {
	if path == "" {
		return 0
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(body))
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

// Status returns a snapshot for HTTP/CLI display.
func (w *Worker) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Status{
		ID:             w.id,
		Name:           w.name,
		SourceStore:    w.sourceName,
		TargetStore:    w.targetName,
		Window:         w.windowStr,
		State:          w.state,
		LastWindowEnd:  w.lastWindowEnd,
		WindowsWritten: atomic.LoadInt64(&w.windowsWritten),
		LastError:      w.lastError,
		LastErrorAt:    w.lastErrorAt,
		CreatedAt:      w.createdAt,
	}
}
