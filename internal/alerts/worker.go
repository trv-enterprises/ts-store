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
	RulesCount    int       `json:"rules_count"`
	RuleNames     []string  `json:"rule_names,omitempty"`
	AlertsFired   int64     `json:"alerts_fired"`
	LastTimestamp int64     `json:"last_timestamp,omitempty"`
	State         string    `json:"state"` // running | stopped | error
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Worker polls a store for new records, evaluates rules against each, and
// dispatches matching records through the configured Sink. Workers are
// created and owned by the alerts Manager.
type Worker struct {
	mu sync.RWMutex

	store     *store.Store
	storeName string

	id        string
	alertType string // "webhook" | "ws" | "mqtt"
	target    string // URL or broker/topic, for status display

	evaluator    *rules.Evaluator
	rulesCount   int
	ruleNames    []string
	sink         Sink
	pollInterval time.Duration

	lastTs      int64
	cursorPath  string
	alertsFired int64

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
	Rules        []store.AlertRuleConfig
	Sink         Sink
	PollInterval string // parsed via duration.ParseDuration; empty -> default
	CursorPath   string // for restart-resume in a future change; ignored on first start
	CreatedAt    time.Time
}

// NewWorker builds a Worker. The rules are parsed here so creation fails
// fast on bad config rather than at first tick. The Sink is owned by the
// worker after construction and will be Close()d on Stop().
func NewWorker(opts Options) (*Worker, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("alerts.NewWorker: Store is required")
	}
	if opts.Sink == nil {
		return nil, fmt.Errorf("alerts.NewWorker: Sink is required")
	}
	if len(opts.Rules) == 0 {
		return nil, fmt.Errorf("alerts.NewWorker: at least one rule is required")
	}

	poll := defaultPollInterval
	if opts.PollInterval != "" {
		d, err := duration.ParseDuration(opts.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", opts.PollInterval, err)
		}
		poll = d
	}

	alertRules, err := buildAlertRules(opts.Rules)
	if err != nil {
		return nil, err
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
		stopCh:       make(chan struct{}),
	}

	// Evaluator dispatches every match through our sink. Per-rule webhooks
	// are NOT used — dispatch is unified through Sink.Send.
	w.evaluator = rules.NewEvaluator(opts.StoreName, alertRules, w.dispatch)
	w.rulesCount = len(alertRules)
	w.ruleNames = make([]string, 0, len(alertRules))
	for _, ar := range alertRules {
		w.ruleNames = append(w.ruleNames, ar.Rule.Name)
	}
	return w, nil
}

// buildAlertRules parses the store-level rule configs into rules.AlertRule
// values. Per-rule webhooks are not constructed: dispatch flows through the
// worker's sink, so the AlertRule.Webhook field is left nil.
func buildAlertRules(configs []store.AlertRuleConfig) ([]rules.AlertRule, error) {
	out := make([]rules.AlertRule, 0, len(configs))
	for _, rc := range configs {
		rule, err := rules.Parse(rc.Name, rc.Condition)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", rc.Name, err)
		}
		// Pass-through reference attached to every alert this rule fires.
		// Not interpreted by ts-store.
		rule.ExternalRef = rc.ExternalRef
		ar := rules.AlertRule{Rule: rule}
		if rc.Cooldown != "" {
			d, err := duration.ParseDuration(rc.Cooldown)
			if err != nil {
				return nil, fmt.Errorf("rule %q cooldown %q: %w", rc.Name, rc.Cooldown, err)
			}
			ar.Cooldown = d
		}
		out = append(out, ar)
	}
	return out, nil
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
// sink and increments the fired counter.
func (w *Worker) dispatch(alert notify.Alert) {
	if err := w.sink.Send(alert); err != nil {
		w.setError(fmt.Sprintf("sink send: %v", err))
		return
	}
	atomic.AddInt64(&w.alertsFired, 1)
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
	// ruleNames is set at construction and never mutated, so the slice
	// header copy is safe — readers can't see it grow or shrink.
	return Status{
		ID:            w.id,
		Type:          w.alertType,
		Target:        w.target,
		RulesCount:    w.rulesCount,
		RuleNames:     w.ruleNames,
		AlertsFired:   atomic.LoadInt64(&w.alertsFired),
		LastTimestamp: w.lastTs,
		State:         w.state,
		LastError:     w.lastError,
		CreatedAt:     w.createdAt,
	}
}

