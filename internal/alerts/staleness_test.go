// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/store"
)

// writeRecordAt inserts a record at an explicit timestamp so a test can
// backdate data and control its apparent age. writeRecord always stamps
// time.Now(), which cannot produce a stale store.
func writeRecordAt(t *testing.T, s *store.Store, ts int64, payload map[string]interface{}) int64 {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := s.PutObject(ts, body); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	return ts
}

// newStalenessWorker builds a staleness worker wired to sink. backdate
// shifts the worker's startedAt into the past, standing in for an alert
// that has been running a while — the staleness check floors record age
// at the worker's start time, so a freshly built worker is never stale.
func newStalenessWorker(t *testing.T, s *store.Store, sink Sink, id, maxAge string, backdate time.Duration) *Worker {
	t.Helper()
	w, err := NewWorker(Options{
		Store:        s,
		StoreName:    "test",
		ID:           id,
		Type:         "webhook",
		Target:       "stub",
		Rule:         store.AlertCommon{Name: "went quiet", RuleType: store.RuleTypeStaleness, MaxAge: maxAge},
		Sink:         sink,
		PollInterval: "50ms",
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.Start()
	if backdate > 0 {
		w.mu.Lock()
		w.startedAt = w.startedAt.Add(-backdate)
		w.mu.Unlock()
	}
	return w
}

// pollerFor wires a worker to a poller by hand, without starting the run
// loop, so tests can drive pollOnce deterministically.
func pollerFor(t *testing.T, s *store.Store, workers ...*Worker) *poller {
	t.Helper()
	p := newPoller(s, "test")
	for _, w := range workers {
		p.workers[w.id] = w
	}
	return p
}

// TestStalenessFiresOnIdleTick is the core of issue #134: the alert must
// fire on a tick where NOTHING arrived. Every pre-existing alert type
// fires from a record; this one fires from the absence of one, on the
// idle early-out path that used to return before touching workers.
func TestStalenessFiresOnIdleTick(t *testing.T) {
	s := newTestStore(t, "stale-idle")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "5m", time.Hour)
	defer w.Stop()

	// Data exists but stopped 10 minutes ago — well past max_age.
	stopped := time.Now().Add(-10 * time.Minute).UnixNano()
	writeRecordAt(t, s, stopped, map[string]interface{}{"temperature": 70.0})

	p := pollerFor(t, s, w)
	// Cursor is already past the record, so this is the idle early-out:
	// no scan, no records delivered, and yet the alert must fire.
	p.lastTs = stopped + 1
	p.pollOnce()

	waitForAlerts(t, sink, 1, 2*time.Second)

	got := sink.Received()[0]
	if got.RuleName != "went quiet" {
		t.Errorf("rule_name = %q, want %q", got.RuleName, "went quiet")
	}
	if got.Timestamp != stopped {
		t.Errorf("timestamp = %d, want the newest record's ts %d", got.Timestamp, stopped)
	}
	if got.StoreName != "test" {
		t.Errorf("store_name = %q, want %q", got.StoreName, "test")
	}
	if rt, _ := got.Data["rule_type"].(string); rt != "staleness" {
		t.Errorf("data.rule_type = %v, want staleness", got.Data["rule_type"])
	}
	if lt, _ := got.Data["last_timestamp"].(int64); lt != stopped {
		t.Errorf("data.last_timestamp = %v, want %d", got.Data["last_timestamp"], stopped)
	}
	age, _ := got.Data["age_seconds"].(float64)
	if age < 500 {
		t.Errorf("data.age_seconds = %v, want roughly 600", got.Data["age_seconds"])
	}
	if maxAge, _ := got.Data["max_age_seconds"].(float64); maxAge != 300 {
		t.Errorf("data.max_age_seconds = %v, want 300", got.Data["max_age_seconds"])
	}
}

// TestStalenessSilentWhenFresh: a store still receiving data must not
// fire, on either the idle path or the scan path.
func TestStalenessSilentWhenFresh(t *testing.T) {
	s := newTestStore(t, "stale-fresh")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "5m", time.Hour)
	defer w.Stop()

	ts := writeRecordAt(t, s, time.Now().UnixNano(), map[string]interface{}{"temperature": 70.0})
	p := pollerFor(t, s, w)

	// Scan path: the record is new to the poller.
	p.lastTs = 0
	p.pollOnce()
	// Idle path: cursor now past the record.
	p.lastTs = ts + 1
	p.pollOnce()

	time.Sleep(200 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Fatalf("fresh store fired %d staleness alerts, want 0", len(got))
	}
}

// TestStalenessNeverFiresOnEmptyStore pins the decision from issue #134
// open question 1: a store that has NEVER received data is "not yet
// started", not stale. Firing here would alert on every newly created
// store before its collector's first write.
func TestStalenessNeverFiresOnEmptyStore(t *testing.T) {
	s := newTestStore(t, "stale-empty")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "1ms", time.Hour)
	defer w.Stop()

	p := pollerFor(t, s, w)
	for i := 0; i < 3; i++ {
		p.pollOnce()
	}

	time.Sleep(200 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Fatalf("empty store fired %d staleness alerts, want 0", len(got))
	}
}

// TestStalenessDoesNotFireOnGapPredatingWorker: an alert created against
// a store that went quiet long ago must not fire immediately on a gap it
// was never running for. The age is floored at the worker's start time.
func TestStalenessDoesNotFireOnGapPredatingWorker(t *testing.T) {
	s := newTestStore(t, "stale-predates")
	sink := &stubSink{}
	// No backdate: the worker just started.
	w := newStalenessWorker(t, s, sink, "w1", "5m", 0)
	defer w.Stop()

	// The store's newest record is an hour old, but this alert has only
	// existed for microseconds.
	old := time.Now().Add(-time.Hour).UnixNano()
	writeRecordAt(t, s, old, map[string]interface{}{"temperature": 70.0})

	p := pollerFor(t, s, w)
	p.lastTs = old + 1
	p.pollOnce()

	time.Sleep(200 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Fatalf("fired %d alerts on a gap predating the worker, want 0", len(got))
	}
}

// TestStalenessRespectsCooldown: a store that stays quiet must not fire
// on every tick. Cooldown is the only thing bounding repeat fires, since
// there is deliberately no resolve/re-arm concept (open question 3).
func TestStalenessRespectsCooldown(t *testing.T) {
	s := newTestStore(t, "stale-cooldown")
	sink := &stubSink{}
	w, err := NewWorker(Options{
		Store:     s,
		StoreName: "test",
		ID:        "w1",
		Type:      "webhook",
		Target:    "stub",
		Rule: store.AlertCommon{
			Name: "went quiet", RuleType: store.RuleTypeStaleness,
			MaxAge: "1m", Cooldown: "30m",
		},
		Sink:         sink,
		PollInterval: "50ms",
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.Start()
	defer w.Stop()
	w.mu.Lock()
	w.startedAt = w.startedAt.Add(-time.Hour)
	w.mu.Unlock()

	stopped := time.Now().Add(-10 * time.Minute).UnixNano()
	writeRecordAt(t, s, stopped, map[string]interface{}{"temperature": 70.0})

	p := pollerFor(t, s, w)
	p.lastTs = stopped + 1
	for i := 0; i < 5; i++ {
		p.pollOnce()
	}

	waitForAlerts(t, sink, 1, 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	if got := sink.Received(); len(got) != 1 {
		t.Fatalf("cooldown did not suppress repeats: %d alerts, want 1", len(got))
	}
}

// TestStalenessStopsFiringWhenDataReturns pins open question 3: there is
// no resolve event. Once data arrives the rule simply goes quiet.
func TestStalenessStopsFiringWhenDataReturns(t *testing.T) {
	s := newTestStore(t, "stale-recovery")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "1m", time.Hour)
	defer w.Stop()

	stopped := time.Now().Add(-10 * time.Minute).UnixNano()
	writeRecordAt(t, s, stopped, map[string]interface{}{"temperature": 70.0})

	p := pollerFor(t, s, w)
	p.lastTs = stopped + 1
	p.pollOnce()
	waitForAlerts(t, sink, 1, 2*time.Second)
	firedWhileDown := len(sink.Received())

	// The collector comes back.
	writeRecordAt(t, s, time.Now().UnixNano(), map[string]interface{}{"temperature": 71.0})
	for i := 0; i < 3; i++ {
		p.pollOnce()
	}
	time.Sleep(200 * time.Millisecond)

	if got := sink.Received(); len(got) != firedWhileDown {
		t.Fatalf("recovery emitted %d extra alerts, want 0 (no resolve event)",
			len(got)-firedWhileDown)
	}
}

// TestStalenessWorkerIgnoresRecords: a staleness worker must never
// evaluate record contents, even while records are streaming past it.
func TestStalenessWorkerIgnoresRecords(t *testing.T) {
	s := newTestStore(t, "stale-ignores")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "1h", 0)
	defer w.Stop()

	p := pollerFor(t, s, w)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})
	p.pollOnce()
	time.Sleep(200 * time.Millisecond)

	if n := w.evaluator.RecordsEvaluated(); n != 0 {
		t.Errorf("staleness worker evaluated %d records, want 0", n)
	}
	if w.lastTimestamp() != 0 && w.lastTs != w.startedAt.UnixNano() {
		// A staleness worker keeps no cursor; deliverBatch must not move it.
		t.Errorf("staleness worker advanced its cursor to %d", w.lastTs)
	}
}

// TestStalenessCoexistsWithConditionAlert: both rule types on one store
// share the poller. The condition alert still sees records; the
// staleness alert still fires on the quiet store. This is the mixed case
// the dashboard renders in a single table.
func TestStalenessCoexistsWithConditionAlert(t *testing.T) {
	s := newTestStore(t, "stale-mixed")

	condSink := &stubSink{}
	cond := newWorker(t, s, condSink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	cond.Start()
	defer cond.Stop()

	staleSink := &stubSink{}
	stale := newStalenessWorker(t, s, staleSink, "w2", "5m", time.Hour)
	defer stale.Stop()

	p := pollerFor(t, s, cond, stale)

	// A hot record arrives now: the condition alert fires, and the store
	// is fresh so the staleness alert stays silent.
	cond.mu.Lock()
	cond.lastTs = 0
	cond.mu.Unlock()
	p.lastTs = 0
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})
	p.pollOnce()

	waitForAlerts(t, condSink, 1, 2*time.Second)
	if got := staleSink.Received(); len(got) != 0 {
		t.Errorf("staleness fired on a fresh store: %d alerts", len(got))
	}
}

// TestStalenessOnlyStoreSkipsRangeScan guards a performance regression:
// a staleness worker never advances the shared cursor, so without an
// explicit check the idle early-out (newestTs <= lastTs) never triggers
// and every tick block-scans the whole store.
func TestStalenessOnlyStoreSkipsRangeScan(t *testing.T) {
	s := newTestStore(t, "stale-noscan")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "1h", 0)
	defer w.Stop()

	writeRecord(t, s, map[string]interface{}{"temperature": 70.0})

	p := pollerFor(t, s, w)
	p.lastTs = 0 // never advances: no record-consuming workers
	p.pollOnce()

	// The scan path sets p.lastTs from scanned handles. Staying at 0
	// proves the early-out fired instead.
	if p.lastTs != 0 {
		t.Errorf("staleness-only store ran a range scan: poller lastTs = %d, want 0", p.lastTs)
	}
}

// TestStalenessWorkerDoesNotPullBackSharedCursor: registering a
// staleness worker must not move the shared scan position, or every
// other alert on the store replays a range it already handled.
func TestStalenessWorkerDoesNotPullBackSharedCursor(t *testing.T) {
	s := newTestStore(t, "stale-cursor")

	condSink := &stubSink{}
	cond := newWorker(t, s, condSink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	p := newPoller(s, "test")
	t.Cleanup(p.stop)
	cond.Start()
	defer cond.Stop()
	p.register(cond)

	afterCond := p.lastTs
	if afterCond == 0 {
		t.Fatal("condition worker did not seed the shared cursor")
	}

	staleSink := &stubSink{}
	stale := newStalenessWorker(t, s, staleSink, "w2", "5m", time.Hour)
	defer stale.Stop()
	// Its lastTs is start-time-ish; back it up to look like an old cursor.
	stale.mu.Lock()
	stale.lastTs = 1
	stale.mu.Unlock()
	p.register(stale)

	if p.lastTs != afterCond {
		t.Errorf("staleness worker pulled the shared cursor back to %d (was %d)", p.lastTs, afterCond)
	}
}

// TestStalenessStatusReportsRuleType: the alerts table renders both rule
// types in one list, so status must say which this is without the caller
// inferring it from whichever other fields happen to be set.
func TestStalenessStatusReportsRuleType(t *testing.T) {
	s := newTestStore(t, "stale-status")
	sink := &stubSink{}
	w := newStalenessWorker(t, s, sink, "w1", "5m", 0)
	defer w.Stop()

	st := w.Status()
	if st.RuleType != store.RuleTypeStaleness {
		t.Errorf("rule_type = %q, want %q", st.RuleType, store.RuleTypeStaleness)
	}
	if st.MaxAge != "5m" {
		t.Errorf("max_age = %q, want %q", st.MaxAge, "5m")
	}

	// Lag is cursor-based and meaningless here: a staleness worker
	// consumes no records, so it must not look permanently behind.
	writeRecordAt(t, s, time.Now().Add(-time.Hour).UnixNano(), map[string]interface{}{"t": 1.0})
	if lag := w.Status().LagMs; lag != 0 {
		t.Errorf("staleness worker reports lag_ms = %d, want 0", lag)
	}
}

// TestConditionAlertUnaffectedByRuleTypeDefault: an alert persisted
// before #134 has no rule_type field at all. It must keep behaving
// exactly as a condition rule.
func TestConditionAlertUnaffectedByRuleTypeDefault(t *testing.T) {
	s := newTestStore(t, "stale-default")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	defer w.Stop()

	if w.isStaleness() {
		t.Fatal("empty rule_type produced a staleness worker")
	}
	if st := w.Status(); st.RuleType != store.RuleTypeCondition {
		t.Errorf("status rule_type = %q, want %q", st.RuleType, store.RuleTypeCondition)
	}

	startPolling(t, s, w)
	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})
	waitForAlerts(t, sink, 1, 2*time.Second)
}
