// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package notify provides webhook notification capabilities.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Alert represents an alert notification to be sent.
type Alert struct {
	RuleName  string                 `json:"rule_name"`
	Condition string                 `json:"condition"`
	Timestamp int64                  `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
	StoreName string                 `json:"store_name,omitempty"`
}

// WebhookConfig holds configuration for a webhook endpoint.
type WebhookConfig struct {
	URL     string
	Headers map[string]string
	Timeout time.Duration
}

// Webhook handles sending notifications to a webhook endpoint.
type Webhook struct {
	config WebhookConfig
	client *http.Client

	// Queue for async sends
	queue chan Alert
	wg    sync.WaitGroup

	stopCh chan struct{}
}

// NewWebhook creates a new webhook notifier.
func NewWebhook(config WebhookConfig) *Webhook {
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}

	w := &Webhook{
		config: config,
		client: &http.Client{
			Timeout: config.Timeout,
		},
		queue:  make(chan Alert, 100), // Buffer up to 100 alerts
		stopCh: make(chan struct{}),
	}

	return w
}

// Start starts the async webhook sender goroutine.
func (w *Webhook) Start() {
	w.wg.Add(1)
	go w.runLoop()
}

// Stop stops the webhook sender and waits for pending sends.
func (w *Webhook) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

// Send queues an alert for async delivery.
// Non-blocking: drops if queue is full.
func (w *Webhook) Send(alert Alert) bool {
	select {
	case w.queue <- alert:
		return true
	default:
		// Queue full, drop alert
		log.Printf("webhook queue full, dropping alert: %s", alert.RuleName)
		return false
	}
}

// SendSync sends an alert synchronously.
func (w *Webhook) SendSync(ctx context.Context, alert Alert) error {
	body, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.config.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}

// runLoop processes the alert queue.
func (w *Webhook) runLoop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.stopCh:
			// Drain remaining alerts with timeout
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			w.drainQueue(ctx)
			cancel()
			return
		case alert := <-w.queue:
			ctx, cancel := context.WithTimeout(context.Background(), w.config.Timeout)
			if err := w.SendSync(ctx, alert); err != nil {
				log.Printf("webhook send failed for %s: %v", alert.RuleName, err)
			}
			cancel()
		}
	}
}

// drainQueue sends remaining queued alerts.
func (w *Webhook) drainQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case alert, ok := <-w.queue:
			if !ok {
				return
			}
			if err := w.SendSync(ctx, alert); err != nil {
				log.Printf("webhook drain failed for %s: %v", alert.RuleName, err)
			}
		default:
			return
		}
	}
}
