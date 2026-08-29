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

// The unit tests cover rendering; these cover the wiring — that a
// configured template actually reaches a fired Alert, on BOTH fire
// paths. A renderer that works while nothing calls it is the failure
// mode worth guarding.

func fireOnce(t *testing.T, tmpl string, data map[string]interface{}) notify.Alert {
	t.Helper()

	var (
		mu   sync.Mutex
		got  notify.Alert
		done = make(chan struct{})
	)
	e := NewEvaluator("sensors", mustParse(t, "high temp", "temperature > 80"), 0, "ref-9",
		func(a notify.Alert) {
			mu.Lock()
			got = a
			mu.Unlock()
			close(done)
		})
	e.SetMessageTemplate(tmpl)
	e.Start()
	defer e.Stop()

	e.Evaluate(1747000000000000000, data)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("alert did not fire")
	}
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestConditionAlertCarriesRenderedMessage(t *testing.T) {
	got := fireOnce(t, "Server Room 5's temperature {temperature} exceeded the max",
		map[string]interface{}{"temperature": 95.0})

	want := "Server Room 5's temperature 95 exceeded the max"
	if got.Message != want {
		t.Errorf("Message = %q, want %q", got.Message, want)
	}
	// Additive: the existing fields must be untouched.
	if got.RuleName != "high temp" || got.Condition != "temperature > 80" {
		t.Errorf("existing fields changed: %+v", got)
	}
	if got.Data["temperature"] != 95.0 {
		t.Errorf("Data lost the triggering value: %+v", got.Data)
	}
}

// No template configured must leave Message empty so `omitempty` drops
// it from the payload entirely — existing receivers see no new field.
func TestNoTemplateLeavesMessageEmpty(t *testing.T) {
	got := fireOnce(t, "", map[string]interface{}{"temperature": 95.0})
	if got.Message != "" {
		t.Errorf("Message = %q, want empty", got.Message)
	}
}

func TestConditionAlertBuiltinsAndSpecs(t *testing.T) {
	got := fireOnce(t,
		"{store}/{rule_name} [{external_ref}] {condition} at {timestamp:time}: {temperature:.1f}",
		map[string]interface{}{"temperature": 95.04})

	want := "sensors/high temp [ref-9] temperature > 80 at 2025-05-11T21:46:40Z: 95.0"
	if got.Message != want {
		t.Errorf("Message = %q\n         want %q", got.Message, want)
	}
}

// A misspelled field must not cost the operator the alert.
func TestBadTemplateStillFiresAlert(t *testing.T) {
	got := fireOnce(t, "temp is {temprature}", map[string]interface{}{"temperature": 95.0})

	if got.RuleName != "high temp" {
		t.Fatal("alert did not fire with a malformed template")
	}
	if got.Message != "temp is " {
		t.Errorf("Message = %q, want %q", got.Message, "temp is ")
	}
}

func TestStalenessAlertCarriesRenderedMessage(t *testing.T) {
	var (
		mu   sync.Mutex
		got  notify.Alert
		done = make(chan struct{})
	)
	e := NewEvaluator("nas-syn-002-disks", mustParse(t, "disks quiet", "temperature > 80"), 0, "",
		func(a notify.Alert) {
			mu.Lock()
			got = a
			mu.Unlock()
			close(done)
		})
	e.SetMessageTemplate("{store} went quiet — no data for {age_seconds}s (limit {max_age_seconds}s)")

	e.FireStaleness(1747000000000000000, 612*time.Second, 300*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("staleness alert did not fire")
	}

	mu.Lock()
	defer mu.Unlock()
	want := "nas-syn-002-disks went quiet — no data for 612s (limit 300s)"
	if got.Message != want {
		t.Errorf("Message = %q\n         want %q", got.Message, want)
	}
	// The synthetic data the template rendered against must still be
	// the data the payload carries.
	if got.Data["age_seconds"] != 612.0 {
		t.Errorf("Data mismatch: %+v", got.Data)
	}
}

// {timestamp} on a staleness alert is the moment data STOPPED, not now —
// the same anchor the payload's Timestamp uses.
func TestStalenessMessageTimestampAnchorsAtLastData(t *testing.T) {
	var (
		mu   sync.Mutex
		got  notify.Alert
		done = make(chan struct{})
	)
	e := NewEvaluator("s", mustParse(t, "r", "temperature > 80"), 0, "",
		func(a notify.Alert) {
			mu.Lock()
			got = a
			mu.Unlock()
			close(done)
		})
	e.SetMessageTemplate("last data at {timestamp:time}")

	e.FireStaleness(1747000000000000000, 612*time.Second, 300*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("staleness alert did not fire")
	}

	mu.Lock()
	defer mu.Unlock()
	if got.Message != "last data at 2025-05-11T21:46:40Z" {
		t.Errorf("Message = %q", got.Message)
	}
}
