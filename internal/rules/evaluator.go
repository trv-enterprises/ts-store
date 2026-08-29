// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/tviviano/ts-store/internal/notify"
)

// Evaluator evaluates a single rule against incoming data and fires
// alerts. Each alert subscription (webhook/WS/MQTT) owns one Evaluator;
// there is no longer any "list of rules" concept here.
type Evaluator struct {
	storeName   string
	rule        *Rule
	cooldown    time.Duration
	externalRef string

	// messageTmpl is the rule's message template, rendered on each fire
	// (issue #144). Empty means the alert carries no Message field.
	messageTmpl string

	// Cooldown tracking. Protected by mu.
	mu        sync.RWMutex
	lastFired time.Time

	// Input channel for async evaluation.
	dataCh chan dataRecord
	stopCh chan struct{}
	wg     sync.WaitGroup

	// Callback for alert firing (e.g., send over WebSocket / webhook /
	// MQTT — wired by the worker).
	onAlert func(alert notify.Alert)

	// Activity counters. Atomic int64 — one increment per incoming
	// record (RecordsEvaluated), per match (RecordsMatched), and per
	// record dropped because the queue was full (RecordsDropped).
	// Process-lifetime by default; the owning Worker exposes a reset
	// path.
	recordsEvaluated atomic.Int64
	recordsMatched   atomic.Int64
	recordsDropped   atomic.Int64
}

type dataRecord struct {
	timestamp int64
	data      map[string]interface{}
}

// NewEvaluator constructs an Evaluator for a single rule on a store.
// externalRef is echoed on every Alert this evaluator fires.
func NewEvaluator(storeName string, rule *Rule, cooldown time.Duration, externalRef string, onAlert func(notify.Alert)) *Evaluator {
	return &Evaluator{
		storeName:   storeName,
		rule:        rule,
		cooldown:    cooldown,
		externalRef: externalRef,
		dataCh:      make(chan dataRecord, 1000), // Buffer to avoid blocking data path
		stopCh:      make(chan struct{}),
		onAlert:     onAlert,
	}
}

// SetMessageTemplate sets the template rendered onto each Alert's
// Message field (issue #144). Empty leaves Message unset.
//
// A setter rather than a sixth constructor parameter: it would sit
// beside externalRef as a second adjacent string that is legitimately
// empty most of the time, and transposing the two at a call site would
// compile cleanly and mis-populate both fields.
//
// Call before Start; not safe to change on a running evaluator.
func (e *Evaluator) SetMessageTemplate(tmpl string) {
	e.messageTmpl = tmpl
}

// renderMessage renders the rule's template for one fire. Returns ""
// when no template is set, which leaves Alert.Message omitted from the
// payload.
func (e *Evaluator) renderMessage(data map[string]interface{}, condition string, timestamp int64) string {
	if e.messageTmpl == "" {
		return ""
	}
	return RenderMessage(e.messageTmpl, TemplateVars(
		data, e.storeName, e.rule.Name, condition, e.externalRef, timestamp,
	))
}

// Start starts the evaluator goroutine.
func (e *Evaluator) Start() {
	e.wg.Add(1)
	go e.runLoop()
}

// Stop stops the evaluator goroutine.
func (e *Evaluator) Stop() {
	close(e.stopCh)
	e.wg.Wait()
}

// Evaluate queues data for async rule evaluation.
// Non-blocking: returns immediately.
func (e *Evaluator) Evaluate(timestamp int64, data map[string]interface{}) {
	select {
	case e.dataCh <- dataRecord{timestamp: timestamp, data: data}:
	default:
		// Queue full — likely a slow sink stalling the evaluator
		// goroutine while writes keep arriving. Count the drop so the
		// stall is observable in metrics.
		e.recordsDropped.Add(1)
	}
}

// runLoop processes incoming data and evaluates the rule.
func (e *Evaluator) runLoop() {
	defer e.wg.Done()

	for {
		select {
		case <-e.stopCh:
			return
		case rec := <-e.dataCh:
			e.evaluateRecord(rec)
		}
	}
}

// evaluateRecord evaluates the single rule against one record.
func (e *Evaluator) evaluateRecord(rec dataRecord) {
	e.recordsEvaluated.Add(1)
	if !e.rule.Evaluate(rec.data) {
		return
	}
	e.recordsMatched.Add(1)

	now := time.Now()
	if !e.checkCooldown(now) {
		return
	}

	condition := e.conditionString()
	alert := notify.Alert{
		RuleName:    e.rule.Name,
		Condition:   condition,
		Timestamp:   rec.timestamp,
		Data:        rec.data,
		StoreName:   e.storeName,
		ExternalRef: e.externalRef,
		Message:     e.renderMessage(rec.data, condition, rec.timestamp),
	}

	// Stamp lastFired BEFORE the callback: the worker's onAlert persists
	// LastFired() to disk, so the mark must be visible when it runs.
	e.mu.Lock()
	e.lastFired = now
	e.mu.Unlock()

	if e.onAlert != nil {
		e.onAlert(alert)
	}
}

// FireStaleness fires a staleness alert: the store has gone longer than
// maxAge without a new record (issue #134). Unlike Evaluate, this does
// not queue onto dataCh — there is no record to evaluate, and a
// staleness check is already running on the poll goroutine at tick
// cadence, so the buffering that protects the shared scan from a slow
// sink is neither needed nor meaningful here.
//
// Cooldown is enforced exactly as it is for a condition match, and
// lastFired is stamped before the callback so the worker can persist it.
// newestTs is the store's newest record timestamp; age is how long ago
// that was.
func (e *Evaluator) FireStaleness(newestTs int64, age, maxAge time.Duration) {
	e.recordsMatched.Add(1)

	now := time.Now()
	if !e.checkCooldown(now) {
		return
	}

	// Bound before building the Alert so the message template can render
	// against the same synthetic data the payload carries.
	data := map[string]interface{}{
		"last_timestamp":  newestTs,
		"age_seconds":     age.Seconds(),
		"max_age_seconds": maxAge.Seconds(),
		"store":           e.storeName,
		"rule_type":       "staleness",
	}
	condition := StalenessCondition(maxAge)

	alert := notify.Alert{
		RuleName:  e.rule.Name,
		Condition: condition,
		// The alert is about the moment data stopped, so anchor the
		// payload timestamp there rather than at "now".
		Timestamp:   newestTs,
		Data:        data,
		StoreName:   e.storeName,
		ExternalRef: e.externalRef,
		Message:     e.renderMessage(data, condition, newestTs),
	}

	e.mu.Lock()
	e.lastFired = now
	e.mu.Unlock()

	if e.onAlert != nil {
		e.onAlert(alert)
	}
}

// StalenessCondition renders a staleness threshold in the same
// human-readable shape as a parsed condition, so a receiver (and the
// dashboard alerts table) can display one `condition` field for every
// alert regardless of rule type.
func StalenessCondition(maxAge time.Duration) string {
	return "no data for " + maxAge.String()
}

// LastFired returns when the rule last fired, or the zero time if it
// has not fired since the evaluator was created (or seeded).
func (e *Evaluator) LastFired() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastFired
}

// SeedLastFired primes cooldown tracking from persisted state so a
// process restart doesn't reopen a cooldown window that was still
// closed. The mark only moves forward — a stale seed can't erase a
// fire that already happened this run.
func (e *Evaluator) SeedLastFired(t time.Time) {
	e.mu.Lock()
	if t.After(e.lastFired) {
		e.lastFired = t
	}
	e.mu.Unlock()
}

// checkCooldown returns true if the rule can fire (cooldown elapsed).
func (e *Evaluator) checkCooldown(now time.Time) bool {
	if e.cooldown == 0 {
		return true
	}
	e.mu.RLock()
	last := e.lastFired
	e.mu.RUnlock()
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= e.cooldown
}

// conditionString returns a human-readable condition string for the rule.
func (e *Evaluator) conditionString() string {
	r := e.rule
	if len(r.Conditions) == 0 {
		return ""
	}

	var parts []string
	for _, c := range r.Conditions {
		parts = append(parts, c.Field+" "+string(c.Operator)+" "+formatValue(c.Value))
	}

	if len(parts) == 1 {
		return parts[0]
	}

	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += " " + r.LogicalOp + " " + parts[i]
	}
	return result
}

// formatValue formats a value for display.
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return "\"" + val + "\""
	default:
		return toString(v)
	}
}

// RecordsEvaluated returns the number of records evaluated since the
// evaluator started or was last reset.
func (e *Evaluator) RecordsEvaluated() int64 { return e.recordsEvaluated.Load() }

// RecordsMatched returns the number of records that matched the rule
// since the evaluator started or was last reset. (Subset of
// RecordsEvaluated.)
func (e *Evaluator) RecordsMatched() int64 { return e.recordsMatched.Load() }

// RecordsDropped returns the number of records dropped because the
// evaluation queue was full, since the evaluator started or was last
// reset. Dropped records are never evaluated, so RecordsEvaluated
// undercounts by exactly this amount.
func (e *Evaluator) RecordsDropped() int64 { return e.recordsDropped.Load() }

// ResetCounters zeros the activity counters.
func (e *Evaluator) ResetCounters() {
	e.recordsEvaluated.Store(0)
	e.recordsMatched.Store(0)
	e.recordsDropped.Store(0)
}
