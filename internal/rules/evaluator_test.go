// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"sync"
	"testing"
	"time"

	"github.com/tviviano/ts-store/internal/notify"
)

// captureCallback returns an onAlert that appends every alert into the
// provided slice (under mu) so tests can assert on it.
func captureCallback(received *[]notify.Alert, mu *sync.Mutex) func(notify.Alert) {
	return func(a notify.Alert) {
		mu.Lock()
		*received = append(*received, a)
		mu.Unlock()
	}
}

// waitFor polls a getter until it returns >=n, or the deadline elapses.
func waitFor(t *testing.T, get func() int, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if get() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d items, got %d", n, get())
}

func mustParse(t *testing.T, name, cond string) *Rule {
	t.Helper()
	r, err := Parse(name, cond)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", name, cond, err)
	}
	return r
}

func TestEvaluatorFiresOnMatch(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("store-a", mustParse(t, "hot", "temperature > 80"), 0, "", captureCallback(&got, &mu))
	e.Start()
	defer e.Stop()

	e.Evaluate(1, map[string]interface{}{"temperature": 90.0})

	waitFor(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if got[0].RuleName != "hot" {
		t.Errorf("RuleName: got %q", got[0].RuleName)
	}
	if got[0].StoreName != "store-a" {
		t.Errorf("StoreName: got %q", got[0].StoreName)
	}
	if got[0].Data["temperature"].(float64) != 90.0 {
		t.Errorf("Data.temperature: got %v", got[0].Data["temperature"])
	}
}

func TestEvaluatorIgnoresNonMatch(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 0, "", captureCallback(&got, &mu))
	e.Start()
	defer e.Stop()

	e.Evaluate(1, map[string]interface{}{"temperature": 50.0})

	// Give the worker time to consume and decide.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Errorf("Expected 0 alerts, got %d", len(got))
	}
}

func TestEvaluatorCooldownSuppressesRapidMatches(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 1*time.Hour, "", captureCallback(&got, &mu))
	e.Start()
	defer e.Stop()

	for i := 0; i < 5; i++ {
		e.Evaluate(int64(i+1), map[string]interface{}{"temperature": 90.0 + float64(i)})
	}

	waitFor(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}, 1, 2*time.Second)
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Errorf("Expected exactly 1 alert under 1h cooldown, got %d", len(got))
	}
}

func TestEvaluatorPassesExternalRefThrough(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 0, "dashboards/x#component-42", captureCallback(&got, &mu))
	e.Start()
	defer e.Stop()

	e.Evaluate(1, map[string]interface{}{"temperature": 90.0})

	waitFor(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if got[0].ExternalRef != "dashboards/x#component-42" {
		t.Errorf("ExternalRef: got %q", got[0].ExternalRef)
	}
}

func TestEvaluatorOmitsEmptyExternalRef(t *testing.T) {
	// When the evaluator has no external_ref, Alert.ExternalRef should be
	// empty and (with omitempty on the JSON tag) absent from the wire.
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 0, "", captureCallback(&got, &mu))
	e.Start()
	defer e.Stop()

	e.Evaluate(1, map[string]interface{}{"temperature": 90.0})

	waitFor(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if got[0].ExternalRef != "" {
		t.Errorf("ExternalRef should be empty, got %q", got[0].ExternalRef)
	}
}

func TestEvaluatorStopDoesntPanic(t *testing.T) {
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 0, "", func(notify.Alert) {})
	e.Start()
	// Stop without sending anything must be clean.
	e.Stop()
}

func TestEvaluatorCountsDropsWhenQueueFull(t *testing.T) {
	// Never started: nothing drains dataCh, so pushes past the buffer
	// capacity (1000) must land in the default branch and be counted.
	e := NewEvaluator("s", mustParse(t, "hot", "temperature > 80"), 0, "", func(notify.Alert) {})

	for i := 0; i < 1005; i++ {
		e.Evaluate(int64(i), map[string]interface{}{"temperature": 90.0})
	}

	if got := e.RecordsDropped(); got != 5 {
		t.Errorf("RecordsDropped: got %d, want 5", got)
	}

	// ResetCounters must zero it along with the other counters.
	e.ResetCounters()
	if got := e.RecordsDropped(); got != 0 {
		t.Errorf("RecordsDropped after reset: got %d, want 0", got)
	}
}
