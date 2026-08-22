// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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
	ID       string `json:"id"`
	Type     string `json:"type"` // "webhook" | "ws" | "mqtt"
	Target   string `json:"target"`
	RuleName string `json:"rule_name"`

	// RuleType is "condition" or "staleness". Always populated, so the
	// alerts table can render both kinds in one list without inferring
	// the type from which other fields happen to be set.
	RuleType string `json:"rule_type"`

	// MaxAge echoes the staleness threshold as configured (e.g. "5m").
	// Empty for condition rules.
	MaxAge string `json:"max_age,omitempty"`

	AlertsFired   int64 `json:"alerts_fired"`
	LastTimestamp int64 `json:"last_timestamp,omitempty"`

	// LagMs is how far the worker's cursor trails the newest record in
	// the store, in milliseconds — 0 when caught up or the store is
	// empty. Sustained growth means the drain rate (maxBatchSize per
	// poll) can't keep up with the store's write rate.
	LagMs int64 `json:"lag_ms"`

	// LastFiredAt is when the rule last fired (survives restarts — it is
	// persisted next to the cursor and re-seeded on Start). Omitted if
	// the rule has never fired.
	LastFiredAt time.Time `json:"last_fired_at,omitzero"`

	// State is "running", "stopped", or "error". "error" means the most
	// recent activity failed; the next clean poll pass restores
	// "running". LastError/LastErrorAt are kept across that recovery as
	// history — state answers "broken now?", last_error_at answers
	// "when did it last hiccup?".
	State       string    `json:"state"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	CreatedAt   time.Time `json:"created_at"`

	// DeliveryFailures counts async sink failures (e.g. webhook non-2xx)
	// reported after the alert was already queued for delivery.
	DeliveryFailures int64 `json:"delivery_failures,omitempty"`
}

// Worker owns one alert's rule evaluator and sink, and receives new
// records from the store's shared poller (issue #4: one scan per store
// per tick instead of one per alert). Workers are created and owned by
// the alerts Manager; record delivery arrives via deliverBatch.
type Worker struct {
	mu sync.RWMutex

	store     *store.Store
	storeName string

	id        string
	alertType string // "webhook" | "ws" | "mqtt"
	target    string // URL or broker/topic, for status display

	evaluator *rules.Evaluator
	ruleName  string
	sink      Sink

	// ruleType is store.RuleTypeCondition or store.RuleTypeStaleness.
	// A staleness worker never receives records: deliverBatch is a no-op
	// for it and checkStaleness drives it from the poll tick instead.
	ruleType string
	// maxAge is the staleness threshold. Only set when ruleType is
	// staleness; there is deliberately no default (see AlertCommon.MaxAge).
	maxAge time.Duration
	// maxAgeRaw is the threshold as the user wrote it ("5m"), echoed in
	// status so the display matches the config rather than Go's
	// normalized duration rendering ("5m0s").
	maxAgeRaw string
	// startedAt is when this worker last Start()ed. A staleness worker
	// only fires on a store that has data and stopped, so this is used
	// solely to avoid reporting a store as stale based on a gap that
	// predates the alert's own existence.
	startedAt time.Time
	// inheritedStartedAt is a non-zero startedAt handed over by the worker
	// this one replaces, so an edit does not re-arm the staleness grace
	// period (issue #179).
	inheritedStartedAt time.Time
	// pollInterval is a hint to the shared poller: the store's loop
	// ticks at the minimum across its registered workers.
	pollInterval time.Duration

	// Restart policy. When restartPolicy == "resume", Start() reads
	// cursorPath; otherwise lastTs is wall-clock now and we skip
	// cursor writes entirely (cursorless workers don't touch disk
	// every poll).
	restartPolicy string
	maxReplay     time.Duration

	// lastTs is this alert's own cursor: records at or before it are
	// never evaluated. It doubles as the replay floor — the shared
	// poller may re-scan ranges other workers still need, and this
	// filter is what keeps that idempotent per alert.
	lastTs           int64
	cursorPath       string
	lastFiredPath    string
	alertsFired      int64
	alertsDropped    int64 // sink.Send returned an error
	deliveryFailures int64 // sink reported an async delivery failure after Send

	state       string
	lastError   string
	lastErrorAt time.Time

	createdAt time.Time
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
	// LastFiredPath persists the evaluator's cooldown mark across
	// restarts (written on every fire regardless of restart policy —
	// fires are rare). Empty disables persistence.
	LastFiredPath string
	CreatedAt     time.Time

	// StartedAt, when non-zero, seeds the staleness grace floor instead of
	// letting Start() reset it to now. Set by an in-place UPDATE so the
	// replacement worker inherits its predecessor's clock (issue #179).
	//
	// The floor exists so an alert created against a store that went quiet
	// last week doesn't fire instantly on a gap that predates it (#134).
	// That reasoning is about a NEW alert; an edited one is the same alert,
	// and re-arming its grace period means a staleness rule goes silent for
	// a full max_age every time an operator tunes it.
	StartedAt time.Time
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
		// A non-positive interval would panic time.NewTicker in the worker
		// goroutine — after the config is already persisted.
		if d <= 0 {
			return nil, fmt.Errorf("invalid poll_interval %q: must be positive", opts.PollInterval)
		}
		poll = d
	}

	// A staleness rule has no condition to parse — it is a function of
	// the clock, not of a record's fields. It still gets an Evaluator,
	// for the cooldown tracking and dispatch path that every alert
	// shares; the Rule it carries is never Evaluate()d against data.
	ruleType := opts.Rule.EffectiveRuleType()
	var parsedRule *rules.Rule
	var maxAge time.Duration
	if ruleType == store.RuleTypeStaleness {
		d, err := duration.ParseDuration(opts.Rule.MaxAge)
		if err != nil {
			return nil, fmt.Errorf("rule %q max_age %q: %w", opts.Rule.Name, opts.Rule.MaxAge, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("rule %q max_age %q: must be positive", opts.Rule.Name, opts.Rule.MaxAge)
		}
		maxAge = d
		parsedRule = &rules.Rule{Name: opts.Rule.Name}
	} else {
		var err error
		parsedRule, err = rules.Parse(opts.Rule.Name, opts.Rule.Condition)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", opts.Rule.Name, err)
		}
	}

	var cooldown time.Duration
	if opts.Rule.Cooldown != "" {
		d, err := duration.ParseDuration(opts.Rule.Cooldown)
		if err != nil {
			return nil, fmt.Errorf("rule %q cooldown %q: %w", opts.Rule.Name, opts.Rule.Cooldown, err)
		}
		cooldown = d
	}

	var maxReplay time.Duration
	if opts.Rule.MaxReplay != "" {
		d, err := duration.ParseDuration(opts.Rule.MaxReplay)
		if err != nil {
			return nil, fmt.Errorf("rule %q max_replay %q: %w", opts.Rule.Name, opts.Rule.MaxReplay, err)
		}
		maxReplay = d
	}

	w := &Worker{
		store:         opts.Store,
		storeName:     opts.StoreName,
		id:            opts.ID,
		alertType:     opts.Type,
		target:        opts.Target,
		sink:          opts.Sink,
		pollInterval:  poll,
		cursorPath:    opts.CursorPath,
		lastFiredPath: opts.LastFiredPath,
		restartPolicy: opts.Rule.RestartPolicy,
		maxReplay:     maxReplay,
		state:         "stopped",
		createdAt:     opts.CreatedAt,
		ruleName:      opts.Rule.Name,
		ruleType:      ruleType,
		maxAge:        maxAge,
		maxAgeRaw:     opts.Rule.MaxAge,

		inheritedStartedAt: opts.StartedAt,
	}

	w.evaluator = rules.NewEvaluator(opts.StoreName, parsedRule, cooldown, opts.Rule.ExternalRef, w.dispatch)
	return w, nil
}

// Start readies the worker to receive records: it seeds lastTs, re-seeds
// the cooldown mark, and starts the evaluator goroutine. Record delivery
// begins when the Manager registers the worker with the store's shared
// poller. The starting lastTs depends on restart_policy:
//   - "" or "now" — wall-clock now; the cursor is neither read nor
//     written for this worker.
//   - "resume"    — read the cursor file (or 0 if missing). When
//     max_replay is set, floor lastTs at now - max_replay so a
//     long-paused worker doesn't replay a flood of historical
//     matches. If the cursor file is missing on first resume, we
//     start from now — there's nothing meaningful to resume from.
func (w *Worker) Start() {
	w.mu.Lock()
	now := time.Now().UnixNano()
	// A worker rebuilt by an in-place update inherits its predecessor's
	// start time (issue #179) — see Options.StartedAt.
	if w.inheritedStartedAt.IsZero() {
		w.startedAt = time.Unix(0, now)
	} else {
		w.startedAt = w.inheritedStartedAt
	}
	switch w.restartPolicy {
	case "resume":
		ts := readCursor(w.cursorPath)
		if ts == 0 {
			ts = now
		}
		if w.maxReplay > 0 {
			floor := time.Now().Add(-w.maxReplay).UnixNano()
			if ts < floor {
				ts = floor
			}
		}
		w.lastTs = ts
	default: // "" or "now"
		w.lastTs = now
	}
	w.state = "running"
	w.mu.Unlock()

	// Re-seed the cooldown mark from disk so a still-closed cooldown
	// window survives the restart (issue #37). Without this, a
	// still-firing condition re-alerts immediately after every restart.
	if ts := readCursor(w.lastFiredPath); ts > 0 {
		w.evaluator.SeedLastFired(time.Unix(0, ts))
	}

	w.evaluator.Start()
}

// Stop shuts down the evaluator and sink. The Manager unregisters the
// worker from the shared poller before calling this, so no deliveries
// race the teardown. Safe to call multiple times.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.state == "stopped" {
		w.mu.Unlock()
		return
	}
	w.state = "stopped"
	w.mu.Unlock()

	w.evaluator.Stop()
	if err := w.sink.Close(); err != nil {
		log.Printf("alerts %s/%s: sink close: %v", logSafe(w.alertType), logSafe(w.id), logSafeErr(err))
	}
}

// lastTimestamp returns this alert's cursor position. The shared poller
// uses it as the scan floor when the worker registers.
func (w *Worker) lastTimestamp() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastTs
}

// deliverBatch feeds one shared-scan batch to this alert's evaluator.
// Records at or before the alert's own cursor are skipped, which makes
// re-scanned ranges (a resume worker registering with an older cursor
// pulls the shared scan back) idempotent for everyone else. The cursor
// is written once per batch, as before.
func (w *Worker) deliverBatch(recs []pollRecord) {
	w.mu.RLock()
	lastTs := w.lastTs
	stopped := w.state == "stopped"
	staleness := w.isStaleness()
	w.mu.RUnlock()
	if stopped {
		return
	}
	// A staleness rule is driven by checkStaleness on the tick, not by
	// record contents. It keeps no cursor, so there is nothing to
	// advance here either.
	if staleness {
		return
	}

	advanced := false
	for _, r := range recs {
		if r.ts <= lastTs {
			continue
		}
		if r.parsed != nil {
			w.evaluator.Evaluate(r.ts, r.parsed)
		}
		lastTs = r.ts
		advanced = true
	}
	if !advanced {
		return
	}

	w.mu.Lock()
	if lastTs > w.lastTs {
		w.lastTs = lastTs
	} else {
		lastTs = w.lastTs
	}
	w.mu.Unlock()
	w.writeCursor(lastTs)
}

// isStaleness reports whether this worker fires on absent data.
func (w *Worker) isStaleness() bool { return w.ruleType == store.RuleTypeStaleness }

// checkStaleness is the staleness rule's whole evaluation, driven by the
// poll tick rather than by an arriving record. The poller calls it on
// EVERY tick — including the idle and empty-store early-outs, which is
// precisely when a staleness rule must still work (issue #134).
//
// newestTs is the store's newest record timestamp as the poller already
// computed it; empty is true when the store holds no records at all.
// Passing them in keeps this free: GetNewestTimestamp is O(partitions)
// of metadata and the poller reads it once per tick regardless.
//
// Semantics:
//   - An empty store never fires. A store that has never received data
//     is "not yet started", not stale — otherwise every newly created
//     store alerts before its collector's first write.
//   - Otherwise fire when now - newestTs > max_age, subject to the same
//     cooldown as any other alert.
//   - The age is floored at the worker's own start time. Without this,
//     an alert created against a store that went quiet last week fires
//     immediately on a gap that predates the alert.
//
// There is no resolve event: when data returns, the rule simply stops
// firing. Cooldown governs repeat fires while the store stays quiet.
func (w *Worker) checkStaleness(newestTs int64, empty bool) {
	w.mu.RLock()
	stopped := w.state == "stopped"
	maxAge := w.maxAge
	startedAt := w.startedAt
	w.mu.RUnlock()
	if stopped || empty || maxAge <= 0 {
		return
	}

	last := time.Unix(0, newestTs)
	if last.Before(startedAt) {
		last = startedAt
	}
	now := time.Now()
	age := now.Sub(last)
	if age <= maxAge {
		return
	}

	// Report the real newest-record timestamp, not the start-time floor:
	// the receiver wants to know when data actually stopped.
	w.evaluator.FireStaleness(newestTs, age, maxAge)
}

// dispatch is the evaluator's onAlert callback. It hands the alert to the
// sink and increments the fired counter. A sink error counts as a drop.
func (w *Worker) dispatch(alert notify.Alert) {
	// The evaluator stamps lastFired before invoking this callback;
	// persist it so cooldown survives restarts. A drop still counts for
	// cooldown (matching the evaluator), so persist before Send.
	w.writeLastFired(w.evaluator.LastFired())
	if err := w.sink.Send(alert); err != nil {
		atomic.AddInt64(&w.alertsDropped, 1)
		w.setError(fmt.Sprintf("sink send: %v", err))
		return
	}
	atomic.AddInt64(&w.alertsFired, 1)
}

// noteDeliveryFailure records an async delivery failure reported by the
// sink after Send already returned (webhook deliveries are queued). Wired
// by the manager so failures surface in status instead of only the log.
func (w *Worker) noteDeliveryFailure(alert notify.Alert, err error) {
	atomic.AddInt64(&w.deliveryFailures, 1)
	w.setError(fmt.Sprintf("delivery: %v", err))
}

// Metrics is the activity snapshot for a Worker, exposed via the per-store
// /metrics endpoint. RecordsEvaluated, RecordsMatched and RecordsDropped
// come from the evaluator (cheap atomic loads).
type Metrics struct {
	ID               string `json:"id"`
	Type             string `json:"type"` // "webhook" | "ws" | "mqtt"
	RecordsEvaluated int64  `json:"records_evaluated"`
	RecordsMatched   int64  `json:"records_matched"`
	RecordsDropped   int64  `json:"records_dropped"`
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
		RecordsDropped:   w.evaluator.RecordsDropped(),
		AlertsFired:      atomic.LoadInt64(&w.alertsFired),
		AlertsDropped:    atomic.LoadInt64(&w.alertsDropped),
	}
}

// ResetMetrics zeros the worker's and its evaluator's counters.
func (w *Worker) ResetMetrics() {
	atomic.StoreInt64(&w.alertsFired, 0)
	atomic.StoreInt64(&w.alertsDropped, 0)
	atomic.StoreInt64(&w.deliveryFailures, 0)
	w.evaluator.ResetCounters()
}

// setError records the failure and flips a running worker into the
// "error" state. Only a running worker escalates — async delivery
// callbacks can land mid-Stop and must not resurrect a stopped worker.
func (w *Worker) setError(msg string) {
	w.mu.Lock()
	w.lastError = msg
	w.lastErrorAt = time.Now().UTC()
	if w.state == "running" {
		w.state = "error"
	}
	w.mu.Unlock()
}

// noteSuccess restores an errored worker to "running" after a clean poll
// pass. lastError/lastErrorAt are deliberately kept as history so a past
// hiccup stays visible (and datable) after recovery.
func (w *Worker) noteSuccess() {
	w.mu.Lock()
	if w.state == "error" {
		w.state = "running"
	}
	w.mu.Unlock()
}

// writeCursor persists lastTs as a UnixNano integer to cursorPath, when
// configured. Errors are logged but do not interrupt the poll loop —
// losing a cursor write only means we replay a few seconds of work on
// the next restart for a resume-policy worker; nothing for a now-policy
// worker (which doesn't read the file).
//
// No-op for restart_policy="now" or "" (the common case): metric
// streams don't need durable resume and we avoid disk I/O every tick.
// logSafeErr is logSafe for an error. os.WriteFile/os.Rename return
// *os.PathError, whose Error() embeds the offending PATH — which is built
// from the alert ID, so the ID reaches the log through the error even when
// the explicit ID argument is already sanitized. CodeQL caught exactly that
// after the first pass of this fix sanitized only the direct arguments.
func logSafeErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return logSafeN(err.Error(), 256)
}

// logSafe strips CR/LF (and other control characters) from a value before it
// is written to a log line. The alert ID reaches these logs, and since PUT
// takes that ID from a request path (issue #166), an unsanitized value could
// forge log entries — which matters here because journald output is itself
// ingested into ts-store by the journal-logs collector. Truncated too: a log
// field is never a place for unbounded caller-supplied text.
func logSafe(s string) string {
	return logSafeN(s, 128)
}

// logSafeN is logSafe with an explicit length bound: error text carries more
// context than an identifier and is worth more room, but is still bounded —
// a log field is never a place for unbounded caller-influenced text.
func logSafeN(s string, max int) string {
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0 && r < 0x20) || r == 0x7f {
			return '_'
		}
		return r
	}, s)
}

func (w *Worker) writeCursor(ts int64) {
	if w.restartPolicy != "resume" {
		return
	}
	if w.cursorPath == "" {
		return
	}
	tmp := w.cursorPath + ".tmp"
	body := strconv.FormatInt(ts, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		log.Printf("alerts %s/%s: cursor write: %v", logSafe(w.alertType), logSafe(w.id), logSafeErr(err))
		return
	}
	if err := os.Rename(tmp, w.cursorPath); err != nil {
		log.Printf("alerts %s/%s: cursor rename: %v", logSafe(w.alertType), logSafe(w.id), logSafeErr(err))
	}
}

// writeLastFired persists the cooldown mark as a UnixNano integer,
// same format and atomic-rename dance as the cursor. Unlike the cursor
// it is written for every restart policy — cooldown correctness matters
// even for "now" workers, and fires are rare enough that the extra
// write is negligible.
func (w *Worker) writeLastFired(t time.Time) {
	if w.lastFiredPath == "" || t.IsZero() {
		return
	}
	tmp := w.lastFiredPath + ".tmp"
	body := strconv.FormatInt(t.UnixNano(), 10) + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0644); err != nil {
		log.Printf("alerts %s/%s: last-fired write: %v", logSafe(w.alertType), logSafe(w.id), logSafeErr(err))
		return
	}
	if err := os.Rename(tmp, w.lastFiredPath); err != nil {
		log.Printf("alerts %s/%s: last-fired rename: %v", logSafe(w.alertType), logSafe(w.id), logSafeErr(err))
	}
}

// readCursor reads a UnixNano timestamp from path. Returns 0 if the
// file is missing or unparseable; the caller treats 0 as "no resume
// point, fall back to start-from-now."
func readCursor(path string) int64 {
	if path == "" {
		return 0
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return 0
	}
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
	// Lag measures how far a worker's cursor trails the store. A
	// staleness worker keeps no cursor and consumes no records, so its
	// lag is always 0 rather than "the whole store" — reporting the
	// latter would make an idle staleness alert look permanently behind.
	var lagMs int64
	if !w.isStaleness() {
		if newest, err := w.store.GetNewestTimestamp(); err == nil && newest > w.lastTs {
			lagMs = (newest - w.lastTs) / int64(time.Millisecond)
		}
	}
	return Status{
		ID:               w.id,
		Type:             w.alertType,
		Target:           w.target,
		RuleName:         w.ruleName,
		RuleType:         w.ruleType,
		MaxAge:           w.maxAgeRaw,
		AlertsFired:      atomic.LoadInt64(&w.alertsFired),
		LastTimestamp:    w.lastTs,
		LagMs:            lagMs,
		LastFiredAt:      w.evaluator.LastFired(),
		State:            w.state,
		LastError:        w.lastError,
		LastErrorAt:      w.lastErrorAt,
		CreatedAt:        w.createdAt,
		DeliveryFailures: atomic.LoadInt64(&w.deliveryFailures),
	}
}
