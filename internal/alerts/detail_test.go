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

func TestAlertDetailMarshalFlattensStatus(t *testing.T) {
	d := AlertDetail{
		Status: Status{
			ID:        "abc12345",
			Type:      "webhook",
			Target:    "https://example.com/hook",
			State:     "running",
			CreatedAt: time.Unix(0, 0).UTC(),
		},
		WebhookConfig: &store.WebhookAlert{
			ID:    "abc12345",
			URL:   "https://example.com/hook",
			Rules: []store.AlertRuleConfig{{Name: "r1", Condition: "x > 0"}},
		},
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Status fields must be at the top level.
	if got["id"] != "abc12345" {
		t.Errorf("status.id not at top level: %v", got["id"])
	}
	if got["type"] != "webhook" {
		t.Errorf("status.type not at top level: %v", got["type"])
	}
	if got["state"] != "running" {
		t.Errorf("status.state not at top level: %v", got["state"])
	}

	// Config must be under "webhook".
	webhook, ok := got["webhook"].(map[string]interface{})
	if !ok {
		t.Fatalf("webhook config missing or wrong type: %T", got["webhook"])
	}
	if webhook["url"] != "https://example.com/hook" {
		t.Errorf("webhook.url: %v", webhook["url"])
	}
	rules, ok := webhook["rules"].([]interface{})
	if !ok || len(rules) != 1 {
		t.Fatalf("webhook.rules: %v", webhook["rules"])
	}
}

func TestAlertDetailMarshalNoConfig(t *testing.T) {
	// No config attached should still produce valid JSON (the worker
	// disappeared between status read and config read — unlikely but
	// not a crash).
	d := AlertDetail{Status: Status{ID: "x", Type: "webhook", State: "running"}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := got["webhook"]; has {
		t.Errorf("expected no webhook key, got: %v", got["webhook"])
	}
}

func TestRedactHeadersMasksKnownAuthHeaders(t *testing.T) {
	in := map[string]string{
		"Authorization":       "Bearer secret",
		"authorization":       "Bearer also-secret", // lowercase variant
		"X-API-Key":           "k123",
		"X-Auth-Token":        "t456",
		"Cookie":              "sid=abc",
		"Proxy-Authorization": "Basic xyz",
		"X-Access-Token":      "at789",
		// These must NOT be redacted.
		"Content-Type": "application/json",
		"X-Custom":     "ok-to-show",
		"X-Public-Key": "rsa-pub", // contains "key" but isn't on the list
	}
	out := redactHeaders(in)

	for _, k := range []string{"Authorization", "authorization", "X-API-Key", "X-Auth-Token", "Cookie", "Proxy-Authorization", "X-Access-Token"} {
		if out[k] != "[redacted]" {
			t.Errorf("%q: got %q, want [redacted]", k, out[k])
		}
	}
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type was redacted: %q", out["Content-Type"])
	}
	if out["X-Custom"] != "ok-to-show" {
		t.Errorf("X-Custom was redacted: %q", out["X-Custom"])
	}
	if out["X-Public-Key"] != "rsa-pub" {
		t.Errorf("X-Public-Key was redacted (substring match too aggressive): %q", out["X-Public-Key"])
	}
}

func TestRedactHeadersEmptyValuePreserved(t *testing.T) {
	// An empty Authorization is not a leak; don't replace it with "[redacted]"
	// because that would imply a value existed.
	in := map[string]string{"Authorization": ""}
	out := redactHeaders(in)
	if out["Authorization"] != "" {
		t.Errorf("empty value was redacted: %q", out["Authorization"])
	}
}

func TestRedactHeadersNilSafe(t *testing.T) {
	if got := redactHeaders(nil); got != nil {
		t.Errorf("nil input should pass through: %v", got)
	}
	if got := redactHeaders(map[string]string{}); len(got) != 0 {
		t.Errorf("empty input should pass through: %v", got)
	}
}
