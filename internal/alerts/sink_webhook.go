// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"time"

	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/internal/notify"
)

const defaultWebhookTimeout = 10 * time.Second

// WebhookSink dispatches alerts as HTTP POSTs. Wraps notify.Webhook to get
// the existing async queue, retry-free semantics.
type WebhookSink struct {
	wh *notify.Webhook
}

// NewWebhookSink builds a webhook sink. timeoutStr is parsed as a duration;
// empty defaults to 10s.
func NewWebhookSink(url string, headers map[string]string, timeoutStr string) (*WebhookSink, error) {
	timeout := defaultWebhookTimeout
	if timeoutStr != "" {
		d, err := duration.ParseDuration(timeoutStr)
		if err != nil {
			return nil, err
		}
		timeout = d
	}

	wh := notify.NewWebhook(notify.WebhookConfig{
		URL:     url,
		Headers: headers,
		Timeout: timeout,
	})
	wh.Start()
	return &WebhookSink{wh: wh}, nil
}

func (s *WebhookSink) Send(alert notify.Alert) error {
	s.wh.Send(alert)
	return nil
}

func (s *WebhookSink) Close() error {
	s.wh.Stop()
	return nil
}
