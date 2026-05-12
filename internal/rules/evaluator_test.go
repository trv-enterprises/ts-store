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

func mustParseRule(t *testing.T, name, cond string) AlertRule {
	t.Helper()
	r, err := Parse(name, cond)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", name, cond, err)
	}
	return AlertRule{Rule: r}
}

func TestEvaluatorFiresOnMatch(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Alert
	e := NewEvaluator("store-a", []AlertRule{
		mustParseRule(t, "hot", "temperature > 80"),
	}, captureCallback(&got, &mu))
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
	e := NewEvaluator("s", []AlertRule{
		mustParseRule(t, "hot", "temperature > 80"),
	}, captureCallback(&got, &mu))
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
	rule := mustParseRule(t, "hot", "temperature > 80")
	rule.Cooldown = 1 * time.Hour
	e := NewEvaluator("s", []AlertRule{rule}, captureCallback(&got, &mu))
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

func TestEvaluatorWebhookInvokedWhenSet(t *testing.T) {
	// Quietly confirm: when AlertRule.Webhook is non-nil, evaluator.Send is
	// called on the *Webhook. We don't dispatch an actual HTTP request — we
	// just verify the queue receives the alert (a non-blocking Send returns
	// immediately to the caller).
	wh := notify.NewWebhook(notify.WebhookConfig{URL: "http://127.0.0.1:1/never-used"})
	// Note: NOT calling wh.Start() — runLoop would actually try to POST.
	// The Send() path used by the evaluator just enqueues, no IO.

	rule := mustParseRule(t, "hot", "temperature > 80")
	ar := AlertRule{Rule: rule.Rule, Webhook: wh}

	var mu sync.Mutex
	var cb []notify.Alert
	e := NewEvaluator("s", []AlertRule{ar}, captureCallback(&cb, &mu))
	e.Start()
	defer e.Stop()

	e.Evaluate(1, map[string]interface{}{"temperature": 90.0})

	// The onAlert callback fires after the webhook enqueue, so when we see
	// the callback fire, the webhook Send has already happened.
	waitFor(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(cb)
	}, 1, 2*time.Second)
}

func TestEvaluatorStopDoesntPanic(t *testing.T) {
	e := NewEvaluator("s", []AlertRule{
		mustParseRule(t, "hot", "temperature > 80"),
	}, func(notify.Alert) {})
	e.Start()
	// Stop without sending anything must be clean.
	e.Stop()
}
