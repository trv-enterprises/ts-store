// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/internal/notify"
	"github.com/tviviano/ts-store/internal/rules"
	"github.com/tviviano/ts-store/pkg/store"
)

const (
	defaultPollInterval = 1 * time.Second
	maxBatchSize        = 100
)

// Status is the runtime status of an alert worker. Exposed via the manager
// for HTTP status responses and CLI status output.
type Status struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"` // "webhook" | "ws" | "mqtt"
	Target        string    `json:"target"`
	RuleName      string    `json:"rule_name"`
	AlertsFired   int64     `json:"alerts_fired"`
	LastTimestamp int64     `json:"last_timestamp,omitempty"`
	State         string    `json:"state"` // running | stopped | error
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Worker polls a store for new records, evaluates a single rule against
// each, and dispatches matches through the configured Sink. Workers are
// created and owned by the alerts Manager.
type Worker struct {
	mu sync.RWMutex

	store     *store.Store
	storeName string

	id        string
	alertType string // "webhook" | "ws" | "mqtt"
	target    string // URL or broker/topic, for status display

	evaluator    *rules.Evaluator
	ruleName     string
	sink         Sink
	pollInterval time.Duration

	lastTs        int64
	cursorPath    string
	alertsFired   int64
	alertsDropped int64 // sink.Send returned an error

	state     string
	lastError string

	createdAt time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options collects everything a Worker needs at construction time. Each
// transport-specific Create* in the manager builds this from its config.
type Options struct {
	Store        *store.Store
	StoreName    string
	ID           string
	Type         string
	Target       string
	Rule         store.AlertCommon
	Sink         Sink
	PollInterval string // parsed via duration.ParseDuration; empty -> default
	CursorPath   string // for restart-resume in a future change; ignored on first start
	CreatedAt    time.Time
}

// NewWorker builds a Worker. The rule is parsed here so creation fails
// fast on bad config rather than at first tick. The Sink is owned by the
// worker after construction and will be Close()d on Stop().
func NewWorker(opts Options) (*Worker, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("alerts.NewWorker: Store is required")
	}
	if opts.Sink == nil {
		return nil, fmt.Errorf("alerts.NewWorker: Sink is required")
	}
	if err := opts.Rule.Validate(); err != nil {
		return nil, err
	}

	poll := defaultPollInterval
	if opts.PollInterval != "" {
		d, err := duration.ParseDuration(opts.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", opts.PollInterval, err)
		}
		poll = d
	}

	parsedRule, err := rules.Parse(opts.Rule.Name, opts.Rule.Condition)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", opts.Rule.Name, err)
	}

	var cooldown time.Duration
	if opts.Rule.Cooldown != "" {
		d, err := duration.ParseDuration(opts.Rule.Cooldown)
		if err != nil {
			return nil, fmt.Errorf("rule %q cooldown %q: %w", opts.Rule.Name, opts.Rule.Cooldown, err)
		}
		cooldown = d
	}

	w := &Worker{
		store:        opts.Store,
		storeName:    opts.StoreName,
		id:           opts.ID,
		alertType:    opts.Type,
		target:       opts.Target,
		sink:         opts.Sink,
		pollInterval: poll,
		cursorPath:   opts.CursorPath,
		state:        "stopped",
		createdAt:    opts.CreatedAt,
		ruleName:     opts.Rule.Name,
		stopCh:       make(chan struct{}),
	}

	w.evaluator = rules.NewEvaluator(opts.StoreName, parsedRule, cooldown, opts.Rule.ExternalRef, w.dispatch)
	return w, nil
}

// Start runs the worker until Stop. Per the plan's "start from now" policy,
// lastTs is initialized to wall-clock now; the persisted cursor file is
// updated but not consulted on this start.
func (w *Worker) Start() {
	w.mu.Lock()
	w.lastTs = time.Now().UnixNano()
	w.state = "running"
	w.mu.Unlock()

	w.evaluator.Start()
	w.wg.Add(1)
	go w.runLoop()
}

// Stop signals the worker to exit and waits for it. Sink and evaluator are
// shut down here. Safe to call multiple times — subsequent calls are no-ops.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.state == "stopped" {
		w.mu.Unlock()
		return
	}
	close(w.stopCh)
	w.mu.Unlock()

	w.wg.Wait()
	w.evaluator.Stop()
	if err := w.sink.Close(); err != nil {
		log.Printf("alerts %s/%s: sink close: %v", w.alertType, w.id, err)
	}

	w.mu.Lock()
	w.state = "stopped"
	w.mu.Unlock()
}

func (w *Worker) runLoop() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			if err := w.pollOnce(); err != nil {
				w.setError(err.Error())
			}
		}
	}
}

// pollOnce reads up to maxBatchSize records since lastTs and feeds each
// to the evaluator. Cursor is advanced as records are seen, then written
// once at the end of the batch.
func (w *Worker) pollOnce() error {
	w.mu.RLock()
	lastTs := w.lastTs
	w.mu.RUnlock()

	endTime := time.Now().UnixNano()
	handles, err := w.store.GetObjectsInRange(lastTs+1, endTime, maxBatchSize)
	if err != nil {
		return fmt.Errorf("range read: %w", err)
	}
	if len(handles) == 0 {
		return nil
	}

	for _, h := range handles {
		data, err := w.store.GetObject(h)
		if err != nil {
			// Skip bad records but keep advancing — a single corrupt
			// record must not stall alert evaluation forever.
			lastTs = h.Timestamp
			continue
		}

		// Expand schema data so condition fields match the schema-defined
		// names rather than positional indices.
		var jsonData []byte
		if w.store.DataType() == store.DataTypeSchema {
			expanded, expErr := w.store.ExpandData(data, 0)
			if expErr == nil {
				jsonData = expanded
			} else {
				jsonData = data
			}
		} else {
			jsonData = data
		}

		var parsed map[string]interface{}
		if json.Unmarshal(jsonData, &parsed) == nil {
			w.evaluator.Evaluate(h.Timestamp, parsed)
		}

		lastTs = h.Timestamp
	}

	w.mu.Lock()
	w.lastTs = lastTs
	w.mu.Unlock()
	w.writeCursor(lastTs)
	return nil
}

// dispatch is the evaluator's onAlert callback. It hands the alert to the
// sink and increments the fired counter. A sink error counts as a drop.
func (w *Worker) dispatch(alert notify.Alert) {
	if err := w.sink.Send(alert); err != nil {
		atomic.AddInt64(&w.alertsDropped, 1)
		w.setError(fmt.Sprintf("sink send: %v", err))
		return
	}
	atomic.AddInt64(&w.alertsFired, 1)
}

// Metrics is the activity snapshot for a Worker, exposed via the per-store
// /metrics endpoint. RecordsEvaluated and RecordsMatched come from the
// evaluator (cheap atomic loads).
type Metrics struct {
	ID               string `json:"id"`
	Type             string `json:"type"` // "webhook" | "ws" | "mqtt"
	RecordsEvaluated int64  `json:"records_evaluated"`
	RecordsMatched   int64  `json:"records_matched"`
	AlertsFired      int64  `json:"alerts_fired"`
	AlertsDropped    int64  `json:"alerts_dropped"`
}

// Metrics returns a snapshot of the worker's activity counters.
func (w *Worker) Metrics() Metrics {
	return Metrics{
		ID:               w.id,
		Type:             w.alertType,
		RecordsEvaluated: w.evaluator.RecordsEvaluated(),
		RecordsMatched:   w.evaluator.RecordsMatched(),
		AlertsFired:      atomic.LoadInt64(&w.alertsFired),
		AlertsDropped:    atomic.LoadInt64(&w.alertsDropped),
	}
}

// ResetMetrics zeros the worker's and its evaluator's counters.
func (w *Worker) ResetMetrics() {
	atomic.StoreInt64(&w.alertsFired, 0)
	atomic.StoreInt64(&w.alertsDropped, 0)
	w.evaluator.ResetCounters()
}

func (w *Worker) setError(msg string) {
	w.mu.Lock()
	w.lastError = msg
	w.mu.Unlock()
}

// writeCursor persists lastTs as a UnixNano integer to cursorPath, when
// configured. Errors are logged but do not interrupt the poll loop —
// losing a cursor write only means we replay a few seconds of work, and
// today we don't even resume from it on restart.
func (w *Worker) writeCursor(ts int64) {
	if w.cursorPath == "" {
		return
	}
	tmp := w.cursorPath + ".tmp"
	body := strconv.FormatInt(ts, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		log.Printf("alerts %s/%s: cursor write: %v", w.alertType, w.id, err)
		return
	}
	if err := os.Rename(tmp, w.cursorPath); err != nil {
		log.Printf("alerts %s/%s: cursor rename: %v", w.alertType, w.id, err)
	}
}

// Status returns a snapshot for HTTP/CLI display.
func (w *Worker) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Status{
		ID:            w.id,
		Type:          w.alertType,
		Target:        w.target,
		RuleName:      w.ruleName,
		AlertsFired:   atomic.LoadInt64(&w.alertsFired),
		LastTimestamp: w.lastTs,
		State:         w.state,
		LastError:     w.lastError,
		CreatedAt:     w.createdAt,
	}
}

