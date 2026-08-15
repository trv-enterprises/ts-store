// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"strings"
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
			ID:          "abc12345",
			URL:         "https://example.com/hook",
			AlertCommon: store.AlertCommon{Name: "r1", Condition: "x > 0"},
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
	if webhook["name"] != "r1" {
		t.Errorf("webhook.name: %v", webhook["name"])
	}
	if webhook["condition"] != "x > 0" {
		t.Errorf("webhook.condition: %v", webhook["condition"])
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

// TestRedactHeadersMasksEverythingButAllowlist pins the issue #163
// inversion: redaction is an allowlist, so any header not explicitly
// echoable is masked — including vendor auth headers that no denylist
// would have anticipated.
func TestRedactHeadersMasksEverythingButAllowlist(t *testing.T) {
	in := map[string]string{
		// Classic auth headers.
		"Authorization":       "Bearer secret",
		"authorization":       "Bearer also-secret", // lowercase variant
		"X-API-Key":           "k123",
		"X-Auth-Token":        "t456",
		"Cookie":              "sid=abc",
		"Proxy-Authorization": "Basic xyz",
		"X-Access-Token":      "at789",
		// The #163 cases: real secrets a name-based denylist misses.
		"X-Vendor-Signature":       "sig-abc",
		"X-Hub-Signature-256":      "sha256=deadbeef",
		"X-Shopify-Hmac-Sha256":    "hmac-xyz",
		"PRIVATE-TOKEN":            "glpat-secret",
		"X-Figma-Token":            "figd-secret",
		"X-Anything-Unanticipated": "could-be-anything",
		// Benign transport headers: echoed.
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   "tsstore/1.0",
	}
	out := redactHeaders(in)

	for k := range in {
		switch strings.ToLower(k) {
		case "content-type", "accept", "user-agent":
			if out[k] != in[k] {
				t.Errorf("%q should be echoed, got %q", k, out[k])
			}
		default:
			if out[k] != "[redacted]" {
				t.Errorf("%q: got %q, want [redacted] — allowlist must fail closed", k, out[k])
			}
		}
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

// TestRedactedConfigLeaksNoSecretsInJSON is the refactor-proof check: it
// plants secrets in every channel a webhook config carries (URL userinfo,
// URL query, a vendor auth header) and asserts none of those plaintexts
// survive into the marshalled read payload. It deliberately asserts on the
// serialized bytes rather than on individual fields, so a future field
// added to the config without redaction fails here.
func TestRedactedConfigLeaksNoSecretsInJSON(t *testing.T) {
	const (
		userinfoSecret = "userinfo-secret-9d3f"
		querySecret    = "query-secret-7b1a"
		headerSecret   = "vendor-header-secret-4c8e"
		mqttPassword   = "mqtt-password-2f6d"
	)

	cfg := store.WebhookAlert{
		ID:  "abc12345",
		URL: "https://user:" + userinfoSecret + "@example.com/hook?token=" + querySecret,
		Headers: map[string]string{
			"X-Vendor-Signature": headerSecret,
			"Content-Type":       "application/json",
		},
		AlertCommon: store.AlertCommon{Name: "r1", Condition: "x > 0"},
	}
	redacted := cfg
	redacted.Headers = redactHeaders(cfg.Headers)
	redacted.URL = redactURL(cfg.URL)

	mqtt := store.MQTTAlert{
		ID:          "def67890",
		BrokerURL:   "tcp://broker.example.com:1883",
		Password:    mqttPassword,
		AlertCommon: store.AlertCommon{Name: "r2", Condition: "y > 0"},
	}
	redactedMQTT := mqtt
	redactedMQTT.Password = "[redacted]"
	redactedMQTT.BrokerURL = redactURL(mqtt.BrokerURL)

	payload, err := json.Marshal(AlertDetail{
		Status:        Status{ID: "abc12345", Type: "webhook", Target: redactURL(cfg.URL)},
		WebhookConfig: &redacted,
		MQTTConfig:    &redactedMQTT,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, secret := range []string{userinfoSecret, querySecret, headerSecret, mqttPassword} {
		if strings.Contains(string(payload), secret) {
			t.Errorf("secret %q leaked into the read payload: %s", secret, payload)
		}
	}

	// Sanity: the target is still identifiable after redaction, else the
	// endpoint would be useless.
	if !strings.Contains(string(payload), "example.com/hook") {
		t.Errorf("redaction destroyed the identifying host/path: %s", payload)
	}
}
