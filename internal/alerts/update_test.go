// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/store"
)

// TestUpdateAlertPreservesIdentityAndHistory: an update edits in place —
// same alert ID, same CreatedAt — rather than becoming a new resource.
// That identity is what delete+recreate destroys (issue #166).
func TestUpdateAlertPreservesIdentityAndHistory(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80", Cooldown: "5m"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	before, err := mgr.store.GetWebhookAlert(created.ID)
	if err != nil {
		t.Fatalf("GetWebhookAlert: %v", err)
	}

	updated, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "very hot", Condition: "t > 95", Cooldown: "5m"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook2"},
	})
	if err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	if updated.ID != created.ID {
		t.Errorf("alert ID changed: %q -> %q", created.ID, updated.ID)
	}
	if updated.RuleName != "very hot" {
		t.Errorf("rule name not updated: %q", updated.RuleName)
	}

	after, err := mgr.store.GetWebhookAlert(created.ID)
	if err != nil {
		t.Fatalf("GetWebhookAlert after update: %v", err)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("CreatedAt changed: %v -> %v", before.CreatedAt, after.CreatedAt)
	}
	if after.URL != "https://example.com/hook2" {
		t.Errorf("URL not updated: %q", after.URL)
	}
	if after.Condition != "t > 95" {
		t.Errorf("condition not updated: %q", after.Condition)
	}

	// Exactly one alert persisted — an update must not append a second.
	cfg, err := mgr.store.LoadWebhookAlerts()
	if err != nil {
		t.Fatalf("LoadWebhookAlerts: %v", err)
	}
	if len(cfg.Alerts) != 1 {
		t.Errorf("expected 1 persisted alert after update, got %d", len(cfg.Alerts))
	}
}

// TestUpdateAlertPreservesRedactedSecrets is the load-bearing case: a
// client that read the alert only ever saw "[redacted]" for auth headers
// and the MQTT password, so echoing the marker back must keep the stored
// secret rather than overwrite it with the literal marker.
func TestUpdateAlertPreservesRedactedSecrets(t *testing.T) {
	mgr := newManagerFixture(t)

	const realSecret = "Bearer real-secret-value"

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook: &WebhookOptions{
			URL: "https://example.com/hook",
			Headers: map[string]string{
				"Authorization": realSecret,
				"Content-Type":  "application/json",
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	// What a client sees on read.
	detail, err := mgr.GetAlertDetail(created.ID)
	if err != nil {
		t.Fatalf("GetAlertDetail: %v", err)
	}
	if got := detail.WebhookConfig.Headers["Authorization"]; got != redactedMarker {
		t.Fatalf("precondition: read should redact Authorization, got %q", got)
	}

	// Submit exactly what was read back, with an unrelated edit.
	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot renamed", Condition: "t > 80"},
		Webhook: &WebhookOptions{
			URL:     "https://example.com/hook",
			Headers: detail.WebhookConfig.Headers,
		},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	after, err := mgr.store.GetWebhookAlert(created.ID)
	if err != nil {
		t.Fatalf("GetWebhookAlert: %v", err)
	}
	if after.Headers["Authorization"] != realSecret {
		t.Errorf("redacted header not preserved: got %q, want the stored secret", after.Headers["Authorization"])
	}
	if after.Headers["Content-Type"] != "application/json" {
		t.Errorf("non-secret header lost: %q", after.Headers["Content-Type"])
	}
	if after.Name != "hot renamed" {
		t.Errorf("rename did not apply: %q", after.Name)
	}
}

// TestUpdateAlertAcceptsNewSecret: the marker means "keep", but a real
// value means "replace" — otherwise credentials could never be rotated.
func TestUpdateAlertAcceptsNewSecret(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook: &WebhookOptions{
			URL:     "https://example.com/hook",
			Headers: map[string]string{"Authorization": "Bearer old"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook: &WebhookOptions{
			URL:     "https://example.com/hook",
			Headers: map[string]string{"Authorization": "Bearer rotated"},
		},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	after, _ := mgr.store.GetWebhookAlert(created.ID)
	if after.Headers["Authorization"] != "Bearer rotated" {
		t.Errorf("new secret not applied: %q", after.Headers["Authorization"])
	}
}

// TestUpdateMQTTPasswordPreserveAndRotate covers the other redacted
// surface: the MQTT password preserves on marker/omission and replaces on
// a real value.
func TestUpdateMQTTPasswordPreserveAndRotate(t *testing.T) {
	mgr := newManagerFixture(t)

	base := func(password string) CreateAlertRequest {
		return CreateAlertRequest{
			Type:        "mqtt",
			AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
			MQTT: &MQTTOptions{
				BrokerURL: "tcp://broker.example.com:1883",
				Topic:     "alerts/heat",
				Username:  "user",
				Password:  password,
			},
		}
	}

	created, err := mgr.CreateAlert(base("original-password"))
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	// Marker → preserved.
	if _, err := mgr.UpdateAlert(created.ID, base(redactedMarker)); err != nil {
		t.Fatalf("UpdateAlert (marker): %v", err)
	}
	after, _ := mgr.store.GetMQTTAlert(created.ID)
	if after.Password != "original-password" {
		t.Errorf("marker did not preserve password: %q", after.Password)
	}

	// Omitted → preserved.
	if _, err := mgr.UpdateAlert(created.ID, base("")); err != nil {
		t.Fatalf("UpdateAlert (omitted): %v", err)
	}
	after, _ = mgr.store.GetMQTTAlert(created.ID)
	if after.Password != "original-password" {
		t.Errorf("omission did not preserve password: %q", after.Password)
	}

	// Real value → rotated.
	if _, err := mgr.UpdateAlert(created.ID, base("new-password")); err != nil {
		t.Fatalf("UpdateAlert (rotate): %v", err)
	}
	after, _ = mgr.store.GetMQTTAlert(created.ID)
	if after.Password != "new-password" {
		t.Errorf("password not rotated: %q", after.Password)
	}
}

// TestUpdateAlertFullReplaceSemantics: non-secret fields are NOT merged —
// omitting one reverts it to the default, matching create. Only redacted
// secrets preserve on omission.
func TestUpdateAlertFullReplaceSemantics(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80", Cooldown: "30m"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook", Timeout: "30s"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	// Omit cooldown and timeout entirely.
	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	after, _ := mgr.store.GetWebhookAlert(created.ID)
	if after.Cooldown != "" {
		t.Errorf("omitted cooldown was merged, want cleared: %q", after.Cooldown)
	}
	if after.Timeout != "" {
		t.Errorf("omitted timeout was merged, want cleared: %q", after.Timeout)
	}
}

// TestUpdateAlertRejectsTypeChange: the persisted lists are per-type, so a
// transport swap is a delete+recreate, not an update.
func TestUpdateAlertRejectsTypeChange(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	_, err = mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "mqtt",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		MQTT:        &MQTTOptions{BrokerURL: "tcp://b:1883", Topic: "t"},
	})
	if err == nil {
		t.Fatal("expected type change to be rejected")
	}
}

// TestUpdateAlertUnknownID returns ErrNotFound so the handler can 404
// rather than reporting a validation problem.
func TestUpdateAlertUnknownID(t *testing.T) {
	mgr := newManagerFixture(t)

	_, err := mgr.UpdateAlert("deadbeef", CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestUpdateAlertRejectsInvalidConfig: validation matches create, and a
// rejected update must leave the running alert untouched.
func TestUpdateAlertRejectsInvalidConfig(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	// Bad scheme — same check create applies.
	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "ftp://example.com/hook"},
	}); err == nil {
		t.Fatal("expected invalid URL scheme to be rejected")
	}

	after, err := mgr.store.GetWebhookAlert(created.ID)
	if err != nil {
		t.Fatalf("GetWebhookAlert: %v", err)
	}
	if after.URL != "https://example.com/hook" {
		t.Errorf("rejected update mutated the stored alert: %q", after.URL)
	}
	if _, ok := mgr.workers[created.ID]; !ok {
		t.Error("rejected update tore down the running worker")
	}
}

// TestUpdateAlertRejectsTraversalID pins the guard CodeQL's path-injection
// warning depends on. Unlike create, where the alert ID is a server-minted
// UUID, update takes the ID from the request path — and that ID reaches
// filepath.Join in cursorPathFor. Reaching it is gated on the worker-map
// lookup, which only matches IDs the server itself generated, so a
// traversal-shaped ID is refused before any path is built. If that lookup
// ever moves after path construction, this test fails.
func TestUpdateAlertRejectsTraversalID(t *testing.T) {
	mgr := newManagerFixture(t)

	// A real alert exists, so the failure below is the ID check rather
	// than an empty manager.
	if _, err := mgr.CreateAlert(CreateAlertRequest{
		Type:        "webhook",
		AlertCommon: store.AlertCommon{Name: "hot", Condition: "t > 80"},
		Webhook:     &WebhookOptions{URL: "https://example.com/hook"},
	}); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	for _, id := range []string{
		"../../etc/passwd",
		"../escape",
		"a/b",
		"..",
	} {
		if _, err := mgr.UpdateAlert(id, CreateAlertRequest{
			Type:        "webhook",
			AlertCommon: store.AlertCommon{Name: "x", Condition: "t > 1"},
			Webhook:     &WebhookOptions{URL: "https://example.com/h"},
		}); err != ErrNotFound {
			t.Errorf("traversal-shaped id %q: got %v, want ErrNotFound", id, err)
		}
	}
}

// TestLogSafeNeutralizesForgedLogLines: the alert ID reaches log lines, and
// PUT takes that ID from a request path — so a value containing newlines
// could forge journal entries, which ts-store's own journal-logs collector
// then ingests. logSafe strips control characters and bounds the length.
func TestLogSafeNeutralizesForgedLogLines(t *testing.T) {
	forged := "abc\nAug 16 00:00:00 host tsstore[1]: alerts: FAKE ENTRY"
	got := logSafe(forged)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("logSafe left a line break in %q", got)
	}
	if strings.Contains(got, "FAKE") == false {
		t.Errorf("logSafe should neutralize, not silently drop content: %q", got)
	}

	if got := logSafe("tab\there"); strings.Contains(got, "\t") {
		t.Errorf("logSafe left a tab: %q", got)
	}
	if got := logSafe("clean-id-123"); got != "clean-id-123" {
		t.Errorf("logSafe mangled a benign value: %q", got)
	}

	long := strings.Repeat("x", 500)
	if got := logSafe(long); len([]rune(got)) > 129 {
		t.Errorf("logSafe did not bound length: %d runes", len([]rune(got)))
	}
}

// TestLogSafeErrSanitizesEmbeddedPaths: os.WriteFile/os.Rename return
// *os.PathError, whose Error() embeds the path — which is built from the
// alert ID. Sanitizing only the explicit ID argument left that route open,
// which is what CodeQL flagged on PR #169 after the first pass.
func TestLogSafeErrSanitizesEmbeddedPaths(t *testing.T) {
	err := &os.PathError{
		Op:   "open",
		Path: "/var/lib/tsstore/x/webhook_alert_abc\nAug 16 00:00:00 host tsstore[1]: FAKE.cursor",
		Err:  os.ErrPermission,
	}
	got := logSafeErr(err)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("logSafeErr left a line break: %q", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("logSafeErr dropped the useful part of the error: %q", got)
	}
	if logSafeErr(nil) != "<nil>" {
		t.Errorf("logSafeErr(nil) = %q, want <nil>", logSafeErr(nil))
	}
}

// writeLastFiredMark plants a cooldown mark as though the alert had fired at
// the given time, which is the state a staleness rule is in whenever an
// operator is tuning it — the store is quiet, so it is mid repeat-fire loop.
func writeLastFiredMark(t *testing.T, mgr *Manager, alertType, alertID string, at time.Time) string {
	t.Helper()
	path := lastFiredPathFor(mgr.store, alertType, alertID)
	if err := os.WriteFile(path, []byte(strconv.FormatInt(at.UnixNano(), 10)+"\n"), 0644); err != nil {
		t.Fatalf("seed lastfired: %v", err)
	}
	return path
}

// TestUpdateClearsCooldownMarkWhenCooldownChanges pins the first half of
// issue #179. The mark persists across process restarts by design (#37), but
// an operator shortening a cooldown is explicitly asking for the old window
// not to apply — and on a staleness rule the mark is always recent, so
// keeping it made the edit look like it did nothing.
func TestUpdateClearsCooldownMarkWhenCooldownChanges(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "quiet", RuleType: "staleness", MaxAge: "1m", Cooldown: "30m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	path := writeLastFiredMark(t, mgr, "webhook", created.ID, time.Now().Add(-90*time.Second))

	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "quiet", RuleType: "staleness", MaxAge: "1m", Cooldown: "1m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cooldown mark survived a cooldown change; the old window still gates firing")
	}
}

// TestUpdateKeepsCooldownMarkWhenCooldownUnchanged is the other side of it:
// an unrelated edit must NOT reopen a legitimately closed window, or renaming
// a rule would fire it immediately.
func TestUpdateKeepsCooldownMarkWhenCooldownUnchanged(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "quiet", RuleType: "staleness", MaxAge: "1m", Cooldown: "30m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	path := writeLastFiredMark(t, mgr, "webhook", created.ID, time.Now().Add(-90*time.Second))

	// Rename and move the sink; cooldown untouched.
	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "renamed", RuleType: "staleness", MaxAge: "1m", Cooldown: "30m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook2"},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("cooldown mark was cleared by an unrelated edit: %v", err)
	}
}

// TestUpdatePreservesStalenessGraceFloor pins the second half of #179. Start()
// resets startedAt, which floors the staleness age — so before this fix every
// edit bought the store a fresh max_age of silence. The floor's purpose (#134)
// is that a NEWLY created alert shouldn't fire on a gap predating it; an
// edited alert is not new.
func TestUpdatePreservesStalenessGraceFloor(t *testing.T) {
	mgr := newManagerFixture(t)

	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "quiet", RuleType: "staleness", MaxAge: "5m", Cooldown: "1m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	mgr.mu.RLock()
	original := mgr.workers[created.ID]
	mgr.mu.RUnlock()
	original.mu.RLock()
	originalStart := original.startedAt
	original.mu.RUnlock()
	if originalStart.IsZero() {
		t.Fatal("precondition: worker has no startedAt")
	}

	time.Sleep(20 * time.Millisecond) // ensure a later Start() would differ

	if _, err := mgr.UpdateAlert(created.ID, CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "quiet", RuleType: "staleness", MaxAge: "5m", Cooldown: "2m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	}); err != nil {
		t.Fatalf("UpdateAlert: %v", err)
	}

	mgr.mu.RLock()
	replacement := mgr.workers[created.ID]
	mgr.mu.RUnlock()
	replacement.mu.RLock()
	newStart := replacement.startedAt
	replacement.mu.RUnlock()

	if !newStart.Equal(originalStart) {
		t.Errorf("startedAt moved on update: %s -> %s; the edit re-armed the max_age grace period",
			originalStart.Format(time.RFC3339Nano), newStart.Format(time.RFC3339Nano))
	}
}

// TestCreateStillArmsStalenessGraceFloor: the inheritance must not leak into
// creation. A brand-new alert still gets its grace period, which is what
// stops it firing on a gap that predates it (#134).
func TestCreateStillArmsStalenessGraceFloor(t *testing.T) {
	mgr := newManagerFixture(t)

	before := time.Now()
	created, err := mgr.CreateAlert(CreateAlertRequest{
		Type: "webhook",
		AlertCommon: store.AlertCommon{
			Name: "fresh", RuleType: "staleness", MaxAge: "1m", Cooldown: "1m",
		},
		Webhook: &WebhookOptions{URL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	mgr.mu.RLock()
	w := mgr.workers[created.ID]
	mgr.mu.RUnlock()
	w.mu.RLock()
	startedAt := w.startedAt
	w.mu.RUnlock()

	if startedAt.Before(before) {
		t.Errorf("a new alert inherited a startedAt from the past (%s); the #134 grace floor is gone",
			startedAt.Format(time.RFC3339Nano))
	}
}
