// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"strings"
	"testing"

	"github.com/tviviano/ts-store/pkg/store"
)

// Regression tests for issue #33 (alerts side): webhook URLs are bearer
// secrets when tokens ride in userinfo or query params, and they were echoed
// verbatim in list Target and detail config.

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://hooks.example.com/incoming":            "https://hooks.example.com/incoming",
		"https://hooks.example.com/hook?token=s3cret":   "https://hooks.example.com/hook?[redacted]",
		"https://user:pass@hooks.example.com/hook":      "https://hooks.example.com/hook",
		"tcp://u:p@broker.example.com:1883":             "tcp://broker.example.com:1883",
		"wss://alerts.example.com/in?key=abc&sig=def":   "wss://alerts.example.com/in?[redacted]",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlertTargetRedactsQuerySecrets(t *testing.T) {
	s := newTestStore(t, "redact-target")
	m := NewManager(s, "redact-target")
	defer m.Stop()

	st, err := m.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "r", Condition: "temperature > 80"},
		Webhook:     &WebhookOptions{URL: "https://hooks.example.com/hook?token=supersecret"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	if strings.Contains(st.Target, "supersecret") {
		t.Errorf("status target leaks the query secret: %q", st.Target)
	}
	if !strings.HasPrefix(st.Target, "https://hooks.example.com/hook") {
		t.Errorf("target should still identify the endpoint: %q", st.Target)
	}

	detail, err := m.GetAlertDetail(st.ID)
	if err != nil {
		t.Fatalf("GetAlertDetail: %v", err)
	}
	if detail.WebhookConfig == nil || strings.Contains(detail.WebhookConfig.URL, "supersecret") {
		t.Errorf("detail config leaks the query secret: %+v", detail.WebhookConfig)
	}
}
