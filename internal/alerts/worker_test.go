// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

func newWorker(t *testing.T, s *store.Store, sink Sink, rule store.AlertCommon, cursorPath string) *Worker {
	t.Helper()
	w, err := NewWorker(Options{
		Store:        s,
		StoreName:    "test",
		ID:           "w1",
		Type:         "webhook",
		Target:       "stub",
		Rule:         rule,
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
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")

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
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")

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
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80", Cooldown: "1h"}, "")

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

func TestWorkerWritesCursorOnResume(t *testing.T) {
	s := newTestStore(t, "cursor")
	sink := &stubSink{}
	cursorPath := filepath.Join(t.TempDir(), "alert.cursor")
	w := newWorker(t, s, sink,
		store.AlertCommon{Name: "any", Condition: "temperature > 0", RestartPolicy: "resume"},
		cursorPath)

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

func TestWorkerSkipsCursorWhenPolicyIsNow(t *testing.T) {
	// Default policy ("") and explicit "now" must skip cursor writes so
	// metric-stream workers don't churn disk every poll tick.
	for _, policy := range []string{"", "now"} {
		t.Run("policy="+policy, func(t *testing.T) {
			s := newTestStore(t, "no-cursor")
			sink := &stubSink{}
			cursorPath := filepath.Join(t.TempDir(), "alert.cursor")
			w := newWorker(t, s, sink,
				store.AlertCommon{Name: "any", Condition: "temperature > 0", RestartPolicy: policy},
				cursorPath)
			w.Start()
			defer w.Stop()

			time.Sleep(100 * time.Millisecond)
			writeRecord(t, s, map[string]interface{}{"temperature": 42.0})
			waitForAlerts(t, sink, 1, 2*time.Second)
			time.Sleep(150 * time.Millisecond)

			if _, err := os.Stat(cursorPath); err == nil {
				t.Errorf("cursor file must not be written when restart_policy=%q", policy)
			}
		})
	}
}

func TestWorkerResumeReadsCursor(t *testing.T) {
	// Write a cursor pointing at a time well before any records exist,
	// then write a matching record. A resume-policy worker must read
	// the cursor and pick up the record (whereas a "now" worker would
	// have started past it).
	s := newTestStore(t, "resume")
	sink := &stubSink{}
	cursorPath := filepath.Join(t.TempDir(), "alert.cursor")

	// Pre-write the matching record FIRST (it goes in the past),
	// then write the cursor pointing before it.
	preTs := writeRecord(t, s, map[string]interface{}{"temperature": 88.0})
	cursorTs := preTs - int64(time.Second) // 1s before the record
	if err := os.WriteFile(cursorPath, []byte(strconv.FormatInt(cursorTs, 10)+"\n"), 0644); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	w := newWorker(t, s, sink,
		store.AlertCommon{Name: "hot", Condition: "temperature > 80", RestartPolicy: "resume"},
		cursorPath)
	w.Start()
	defer w.Stop()

	// Worker should replay the pre-existing record because the cursor
	// points before it.
	waitForAlerts(t, sink, 1, 2*time.Second)
}

func TestWorkerResumeBoundedByMaxReplay(t *testing.T) {
	// Old matching record + cursor pointing before it. max_replay
	// should floor the resume window past the old record so it is
	// NOT replayed. A fresh record after Start must still fire.
	s := newTestStore(t, "bounded")
	sink := &stubSink{}
	cursorPath := filepath.Join(t.TempDir(), "alert.cursor")

	// Old matching record.
	writeRecord(t, s, map[string]interface{}{"temperature": 90.0})

	// Cursor pointing way back so unbounded resume would replay it.
	oldTs := time.Now().Add(-1 * time.Hour).UnixNano()
	if err := os.WriteFile(cursorPath, []byte(strconv.FormatInt(oldTs, 10)+"\n"), 0644); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	// Sleep so the old record is comfortably older than max_replay (100ms).
	time.Sleep(200 * time.Millisecond)

	w := newWorker(t, s, sink,
		store.AlertCommon{
			Name: "hot", Condition: "temperature > 80",
			RestartPolicy: "resume", MaxReplay: "100ms",
		},
		cursorPath)
	w.Start()
	defer w.Stop()

	// Give the worker a couple of poll cycles. The pre-Start record
	// is outside the 100ms replay window, so no alert.
	time.Sleep(300 * time.Millisecond)
	if got := sink.Received(); len(got) != 0 {
		t.Errorf("max_replay should have excluded the old record, got %d alerts", len(got))
	}

	// Now a fresh record — that one should fire normally.
	writeRecord(t, s, map[string]interface{}{"temperature": 91.0})
	waitForAlerts(t, sink, 1, 2*time.Second)
}

func TestWorkerStartFromNowIgnoresHistory(t *testing.T) {
	s := newTestStore(t, "history")
	sink := &stubSink{}

	// Write a matching record BEFORE the worker starts. With start-from-now
	// policy, the worker must not fire on it.
	writeRecord(t, s, map[string]interface{}{"temperature": 99.0})

	// Give the store a clean separation between historical write and worker start.
	time.Sleep(50 * time.Millisecond)

	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
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

func TestWorkerStatusReportsRuleName(t *testing.T) {
	s := newTestStore(t, "rule-name")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")
	defer w.Stop()

	got := w.Status()
	if got.RuleName != "hot" {
		t.Errorf("RuleName: got %q, want %q", got.RuleName, "hot")
	}
}

func TestWorkerMetricsCountsRecordsAndDispatches(t *testing.T) {
	s := newTestStore(t, "metrics")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "hot", Condition: "temperature > 80"}, "")

	w.Start()
	defer w.Stop()

	// Wait one poll cycle so lastTs is set before we write.
	time.Sleep(100 * time.Millisecond)

	// Two records: one matches, one doesn't. 2 records evaluated, 1 matched.
	writeRecord(t, s, map[string]interface{}{"temperature": 95.0})
	writeRecord(t, s, map[string]interface{}{"temperature": 50.0})

	waitForAlerts(t, sink, 1, 2*time.Second)
	time.Sleep(150 * time.Millisecond) // let counters settle

	m := w.Metrics()
	if m.RecordsEvaluated != 2 {
		t.Errorf("RecordsEvaluated: got %d, want 2", m.RecordsEvaluated)
	}
	if m.RecordsMatched != 1 {
		t.Errorf("RecordsMatched: got %d, want 1", m.RecordsMatched)
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
	w := newWorker(t, s, failingSink{}, store.AlertCommon{Name: "always", Condition: "temperature > 0"}, "")
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
	w := newWorker(t, s, sink, store.AlertCommon{Name: "x", Condition: "temperature > 0"}, "")
	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 1.0})
	waitForAlerts(t, sink, 1, 2*time.Second)

	before := w.Metrics()
	if before.RecordsEvaluated == 0 || before.AlertsFired == 0 {
		t.Fatalf("test setup: counters should be non-zero before reset, got %+v", before)
	}

	w.ResetMetrics()
	after := w.Metrics()
	if after.RecordsEvaluated != 0 || after.RecordsMatched != 0 || after.AlertsFired != 0 || after.AlertsDropped != 0 {
		t.Errorf("expected zero counters after reset, got %+v", after)
	}
}

func TestWorkerStopClosesSink(t *testing.T) {
	s := newTestStore(t, "stop")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "any", Condition: "x > 0"}, "")
	w.Start()
	w.Stop()

	if !sink.closed {
		t.Errorf("Stop must close the sink")
	}
}

// Regression test for issue #10: a non-positive poll_interval must fail at
// construction. Before this check, "0s" passed validation, was persisted,
// and then panicked time.NewTicker in the worker goroutine — crash-looping
// the daemon on every restart.
func TestWorkerRejectsNonPositivePollInterval(t *testing.T) {
	s := newTestStore(t, "nonpositive-poll")
	rule := store.AlertCommon{Name: "r", Condition: "temperature > 80"}

	for _, poll := range []string{"0s", "-1s"} {
		_, err := NewWorker(Options{
			Store:        s,
			StoreName:    "test",
			ID:           "w1",
			Type:         "webhook",
			Target:       "stub",
			Rule:         rule,
			Sink:         &stubSink{},
			PollInterval: poll,
			CreatedAt:    time.Now(),
		})
		if err == nil {
			t.Errorf("NewWorker with poll_interval %q: expected error, got nil", poll)
		}
	}
}

// Companion to the above: a non-positive webhook timeout must fail at sink
// construction rather than producing an HTTP client that never times out.
func TestWebhookSinkRejectsNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-5s"} {
		if _, err := NewWebhookSink("http://localhost:1/hook", nil, timeout); err == nil {
			t.Errorf("NewWebhookSink with timeout %q: expected error, got nil", timeout)
		}
	}
}

// TestWorkerErrorStateSetsAndRecovers confirms the documented "error"
// state is actually reachable and self-healing (issue #35): a failure
// flips a running worker to "error", the next clean poll pass restores
// "running", and the last error text/timestamp survive as history.
func TestWorkerErrorStateSetsAndRecovers(t *testing.T) {
	s := newTestStore(t, "errstate")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "r", Condition: "t > 0"}, "")

	w.Start()
	defer w.Stop()
	if st := w.Status().State; st != "running" {
		t.Fatalf("state after Start: %q, want running", st)
	}

	w.setError("boom")
	st := w.Status()
	if st.State != "error" {
		t.Fatalf("state after setError: %q, want error", st.State)
	}
	if st.LastError != "boom" || st.LastErrorAt.IsZero() {
		t.Fatalf("error history not recorded: %+v", st)
	}

	// The 50ms poll loop's next clean pass should restore "running".
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && w.Status().State != "running" {
		time.Sleep(20 * time.Millisecond)
	}
	st = w.Status()
	if st.State != "running" {
		t.Fatalf("state never recovered from error: %+v", st)
	}
	if st.LastError != "boom" || st.LastErrorAt.IsZero() {
		t.Errorf("recovery must keep error history, got: %+v", st)
	}
}

// TestWorkerSetErrorDoesNotResurrectStopped confirms a late async
// failure (e.g. a webhook delivery callback landing after Stop) records
// the error but cannot flip a stopped worker back to "error".
func TestWorkerSetErrorDoesNotResurrectStopped(t *testing.T) {
	s := newTestStore(t, "errstopped")
	sink := &stubSink{}
	w := newWorker(t, s, sink, store.AlertCommon{Name: "r", Condition: "t > 0"}, "")

	w.Start()
	w.Stop()

	w.setError("late delivery failure")
	st := w.Status()
	if st.State != "stopped" {
		t.Errorf("state: got %q, want stopped", st.State)
	}
	if st.LastError != "late delivery failure" {
		t.Errorf("lastError should still record: %+v", st)
	}
}

// TestWorkerCooldownSurvivesRestart is the issue #37 scenario: a rule
// with a long cooldown fires, the daemon "restarts" (new Worker, same
// last-fired path), and a still-matching record must NOT re-alert —
// the persisted cooldown mark is re-seeded on Start. Also checks the
// mark is exposed as Status.LastFiredAt on both sides of the restart.
func TestWorkerCooldownSurvivesRestart(t *testing.T) {
	s := newTestStore(t, "cooldownrestart")
	lastFiredPath := filepath.Join(t.TempDir(), "webhook_alert_x.lastfired")
	rule := store.AlertCommon{Name: "hot", Condition: "temperature > 80", Cooldown: "1h"}

	build := func(sink Sink) *Worker {
		t.Helper()
		w, err := NewWorker(Options{
			Store:         s,
			StoreName:     "test",
			ID:            "w1",
			Type:          "webhook",
			Target:        "stub",
			Rule:          rule,
			Sink:          sink,
			PollInterval:  "50ms",
			LastFiredPath: lastFiredPath,
			CreatedAt:     time.Now(),
		})
		if err != nil {
			t.Fatalf("NewWorker: %v", err)
		}
		return w
	}

	// First life: fire once.
	sink1 := &stubSink{}
	w1 := build(sink1)
	w1.Start()
	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 90.0})
	waitForAlerts(t, sink1, 1, 2*time.Second)
	if got := w1.Status().LastFiredAt; got.IsZero() {
		t.Error("Status.LastFiredAt should be set after a fire")
	}
	w1.Stop()

	if ts := readCursor(lastFiredPath); ts == 0 {
		t.Fatal("last-fired mark was not persisted on fire")
	}

	// Second life: same path, fresh worker. The 1h cooldown must still
	// be closed, so a matching record is suppressed.
	sink2 := &stubSink{}
	w2 := build(sink2)
	w2.Start()
	defer w2.Stop()
	if got := w2.Status().LastFiredAt; got.IsZero() {
		t.Error("Status.LastFiredAt should be seeded from disk after restart")
	}
	time.Sleep(100 * time.Millisecond)
	writeRecord(t, s, map[string]interface{}{"temperature": 91.0})
	time.Sleep(300 * time.Millisecond)
	if got := sink2.Received(); len(got) != 0 {
		t.Errorf("cooldown must survive restart; got %d alerts: %+v", len(got), got)
	}
}
