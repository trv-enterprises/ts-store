// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"sync"
	"time"

	"github.com/tviviano/ts-store/internal/notify"
)

// AlertRule combines a parsed rule with webhook configuration.
type AlertRule struct {
	Rule     *Rule
	Webhook  *notify.Webhook // nil if no webhook configured
	Cooldown time.Duration   // minimum time between alerts
}

// Evaluator evaluates rules against incoming data and fires alerts.
type Evaluator struct {
	storeName string
	rules     []AlertRule

	// Cooldown tracking: rule name -> last fired time
	lastFired map[string]time.Time
	mu        sync.RWMutex

	// Input channel for async evaluation
	dataCh chan dataRecord
	stopCh chan struct{}
	wg     sync.WaitGroup

	// Callback for alert firing (e.g., send over WebSocket)
	onAlert func(alert notify.Alert)
}

type dataRecord struct {
	timestamp int64
	data      map[string]interface{}
}

// NewEvaluator creates a new rule evaluator.
func NewEvaluator(storeName string, rules []AlertRule, onAlert func(notify.Alert)) *Evaluator {
	return &Evaluator{
		storeName: storeName,
		rules:     rules,
		lastFired: make(map[string]time.Time),
		dataCh:    make(chan dataRecord, 1000), // Buffer to avoid blocking data path
		stopCh:    make(chan struct{}),
		onAlert:   onAlert,
	}
}

// Start starts the evaluator goroutine.
func (e *Evaluator) Start() {
	// Start all webhooks
	for _, r := range e.rules {
		if r.Webhook != nil {
			r.Webhook.Start()
		}
	}

	e.wg.Add(1)
	go e.runLoop()
}

// Stop stops the evaluator and all webhooks.
func (e *Evaluator) Stop() {
	close(e.stopCh)
	e.wg.Wait()

	// Stop all webhooks
	for _, r := range e.rules {
		if r.Webhook != nil {
			r.Webhook.Stop()
		}
	}
}

// Evaluate queues data for async rule evaluation.
// Non-blocking: returns immediately.
func (e *Evaluator) Evaluate(timestamp int64, data map[string]interface{}) {
	select {
	case e.dataCh <- dataRecord{timestamp: timestamp, data: data}:
	default:
		// Queue full, drop (shouldn't happen with large buffer)
	}
}

// runLoop processes incoming data and evaluates rules.
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

// evaluateRecord evaluates all rules against a single record.
func (e *Evaluator) evaluateRecord(rec dataRecord) {
	now := time.Now()

	for _, ar := range e.rules {
		if !ar.Rule.Evaluate(rec.data) {
			continue
		}

		// Check cooldown
		if !e.checkCooldown(ar.Rule.Name, ar.Cooldown, now) {
			continue
		}

		// Build alert
		alert := notify.Alert{
			RuleName:  ar.Rule.Name,
			Condition: e.conditionString(ar.Rule),
			Timestamp: rec.timestamp,
			Data:      rec.data,
			StoreName: e.storeName,
		}

		// Fire webhook if configured
		if ar.Webhook != nil {
			ar.Webhook.Send(alert)
		}

		// Call alert callback (e.g., send over WS)
		if e.onAlert != nil {
			e.onAlert(alert)
		}

		// Update last fired
		e.mu.Lock()
		e.lastFired[ar.Rule.Name] = now
		e.mu.Unlock()
	}
}

// checkCooldown returns true if the rule can fire (cooldown elapsed).
func (e *Evaluator) checkCooldown(ruleName string, cooldown time.Duration, now time.Time) bool {
	if cooldown == 0 {
		return true
	}

	e.mu.RLock()
	lastFired, ok := e.lastFired[ruleName]
	e.mu.RUnlock()

	if !ok {
		return true
	}

	return now.Sub(lastFired) >= cooldown
}

// conditionString returns a human-readable condition string.
func (e *Evaluator) conditionString(r *Rule) string {
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
