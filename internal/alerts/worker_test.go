// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tviviano/ts-store/internal/notify"
	"github.com/tviviano/ts-store/pkg/store"
)

// stubSink collects all alerts in memory for assertions.
type stubSink struct {
	mu     sync.Mutex
	alerts []notify.Alert
	closed bool
}

func (s *stubSink) Send(a notify.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, a)
	return nil
}

func (s *stubSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stubSink) Received() []notify.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notify.Alert, len(s.alerts))
	copy(out, s.alerts)
	return out
}

// newTestStore creates a V2 JSON store for worker tests.
func newTestStore(t *testing.T, name string) *store.Store {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.Name = name
	cfg.Path = t.TempDir()
	cfg.NumBlocks = 4
	cfg.DataType = store.DataTypeJSON
	s, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// writeRecord inserts a JSON record at a fresh timestamp and returns the
// timestamp used. Sleeps 1ms so consecutive writes get distinct nanos.
func writeRecord(t *testing.T, s *store.Store, payload map[string]interface{}) int64 {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ts := time.Now().UnixNano()
	if _, err := s.PutObject(ts, body); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	time.Sleep(1 * time.Millisecond)
	return ts
}

// waitForAlerts polls the sink until n alerts arrive or timeout elapses.
func waitForAlerts(t *testing.T, sink *stubSink, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(sink.Received()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d alerts, got %d", n, len(sink.Received()))
}

func newWorker(t *testing.T, s *store.Store, sink Sink, rules []store.AlertRuleConfig, cursorPath string) *Worker {
	t.Helper()
	w, err := NewWorker(Options{
		Store:        s,
		StoreName:    "test",
		ID:           "w1",
		Type:         "webhook",
		Target:       "stub",
		Rules:        rules,
		Sink:         sink,
		PollInterval: "50ms",
		CursorPath:   cursorPath,
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

func TestWorkerMatchingRecordFires(t *testing.T) {
	s := newTestStore(t, "match")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
	}, "")

	w.Start()
	defer w.Stop()

	// Wait one poll tick so the worker's lastTs is set, then write a record.
	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 90.0})

	waitForAlerts(t, sink, 1, 2*time.Second)

	got := sink.Received()
	if got[0].RuleName != "hot" {
		t.Errorf("Alert RuleName: got %q, want %q", got[0].RuleName, "hot")
	}
	if got[0].Data["temperature"].(float64) != 90.0 {
		t.Errorf("Alert.Data.temperature: got %v", got[0].Data["temperature"])
	}
}

func TestWorkerNonMatchingRecordIgnored(t *testing.T) {
	s := newTestStore(t, "nomatch")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
	}, "")

	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 50.0})

	// Give the worker a fair chance to see the record and decide.
	time.Sleep(300 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Errorf("Expected 0 alerts, got %d: %+v", len(got), got)
	}
}

func TestWorkerCooldownSuppresses(t *testing.T) {
	s := newTestStore(t, "cooldown")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80", Cooldown: "1h"},
	}, "")

	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 90.0})
	writeRecord(t, s, map[string]interface{}{"temperature": 91.0})
	writeRecord(t, s, map[string]interface{}{"temperature": 92.0})

	waitForAlerts(t, sink, 1, 2*time.Second)
	time.Sleep(300 * time.Millisecond)

	if got := sink.Received(); len(got) != 1 {
		t.Errorf("Expected exactly 1 alert under 1h cooldown, got %d", len(got))
	}
}

func TestWorkerWritesCursor(t *testing.T) {
	s := newTestStore(t, "cursor")
	sink := &stubSink{}
	cursorPath := filepath.Join(t.TempDir(), "alert.cursor")
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "any", Condition: "temperature > 0"},
	}, cursorPath)

	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 42.0})
	waitForAlerts(t, sink, 1, 2*time.Second)

	// Cursor file should exist and contain a recent UnixNano.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cursorPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("cursor file not written: %v", err)
	}
	if len(body) == 0 {
		t.Errorf("cursor file empty")
	}
}

func TestWorkerStartFromNowIgnoresHistory(t *testing.T) {
	s := newTestStore(t, "history")
	sink := &stubSink{}

	// Write a matching record BEFORE the worker starts. With start-from-now
	// policy, the worker must not fire on it.
	writeRecord(t, s, map[string]interface{}{"temperature": 99.0})

	// Give the store a clean separation between historical write and worker start.
	time.Sleep(50 * time.Millisecond)

	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
	}, "")
	w.Start()
	defer w.Stop()

	// Give the worker a couple of poll cycles. No new writes, so no alerts.
	time.Sleep(300 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Errorf("Expected start-from-now to skip history, got %d alerts", len(got))
	}

	// Now a fresh write — that one should fire.
	writeRecord(t, s, map[string]interface{}{"temperature": 91.0})
	waitForAlerts(t, sink, 1, 2*time.Second)
}

func TestWorkerStatusReportsRuleNames(t *testing.T) {
	s := newTestStore(t, "rule-names")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
		{Name: "cold", Condition: "temperature < 0"},
	}, "")
	defer w.Stop()

	got := w.Status()
	if got.RulesCount != 2 {
		t.Errorf("RulesCount: got %d, want 2", got.RulesCount)
	}
	if len(got.RuleNames) != 2 || got.RuleNames[0] != "hot" || got.RuleNames[1] != "cold" {
		t.Errorf("RuleNames: got %v, want [hot cold]", got.RuleNames)
	}
}

func TestWorkerMetricsCountsRulesAndDispatches(t *testing.T) {
	s := newTestStore(t, "metrics")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "hot", Condition: "temperature > 80"},
		{Name: "cold", Condition: "temperature < 0"},
	}, "")

	w.Start()
	defer w.Stop()

	// Wait one poll cycle so lastTs is set before we write.
	time.Sleep(100 * time.Millisecond)

	// Two records: one matches "hot" (one of two rules), the other matches
	// nothing. Both records test both rules → 4 rules tested. One match.
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})
	writeRecord(t, s, map[string]interface{}{"temperature": 50.0})

	waitForAlerts(t, sink, 1, 2*time.Second)
	time.Sleep(150 * time.Millisecond) // let counters settle

	m := w.Metrics()
	if m.RulesTested != 4 {
		t.Errorf("RulesTested: got %d, want 4 (2 records × 2 rules)", m.RulesTested)
	}
	if m.RulesMatched != 1 {
		t.Errorf("RulesMatched: got %d, want 1", m.RulesMatched)
	}
	if m.AlertsFired != 1 {
		t.Errorf("AlertsFired: got %d, want 1", m.AlertsFired)
	}
	if m.AlertsDropped != 0 {
		t.Errorf("AlertsDropped: got %d, want 0", m.AlertsDropped)
	}
}

// failingSink always returns an error from Send — used to exercise the
// AlertsDropped counter path.
type failingSink struct{}

func (failingSink) Send(notify.Alert) error { return errStubFail }
func (failingSink) Close() error            { return nil }

var errStubFail = errSink("stub")

type errSink string

func (e errSink) Error() string { return string(e) }

func TestWorkerMetricsCountsDrops(t *testing.T) {
	s := newTestStore(t, "drops")
	w := newWorker(t, s, failingSink{}, []store.AlertRuleConfig{
		{Name: "always", Condition: "temperature > 0"},
	}, "")
	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 1.0})

	// Wait for the worker to evaluate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Metrics().AlertsDropped >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := w.Metrics()
	if m.AlertsDropped != 1 {
		t.Errorf("AlertsDropped: got %d, want 1", m.AlertsDropped)
	}
	if m.AlertsFired != 0 {
		t.Errorf("AlertsFired: got %d, want 0 (sink errored)", m.AlertsFired)
	}
}

func TestWorkerResetMetricsZeros(t *testing.T) {
	s := newTestStore(t, "reset")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "x", Condition: "temperature > 0"},
	}, "")
	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 1.0})
	waitForAlerts(t, sink, 1, 2*time.Second)

	before := w.Metrics()
	if before.RulesTested == 0 || before.AlertsFired == 0 {
		t.Fatalf("test setup: counters should be non-zero before reset, got %+v", before)
	}

	w.ResetMetrics()
	after := w.Metrics()
	if after.RulesTested != 0 || after.RulesMatched != 0 || after.AlertsFired != 0 || after.AlertsDropped != 0 {
		t.Errorf("expected zero counters after reset, got %+v", after)
	}
}

func TestWorkerStopClosesSink(t *testing.T) {
	s := newTestStore(t, "stop")
	sink := &stubSink{}
	w := newWorker(t, s, sink, []store.AlertRuleConfig{
		{Name: "any", Condition: "x > 0"},
	}, "")
	w.Start()
	w.Stop()

	if !sink.closed {
		t.Errorf("Stop must close the sink")
	}
}
