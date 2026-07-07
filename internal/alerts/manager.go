// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tviviano/ts-store/pkg/store"
)

var (
	ErrManagerClosed = errors.New("alerts manager is closed")
	ErrNotFound      = errors.New("alert not found")
)

// Manager owns all webhook/WS/MQTT alert workers for a single store.
type Manager struct {
	mu sync.RWMutex

	store     *store.Store
	storeName string

	workers map[string]*Worker // alertID -> worker (IDs unique across types)
	closed  bool
}

// NewManager constructs a manager. Call LoadAndStart to spin up persisted alerts.
func NewManager(st *store.Store, storeName string) *Manager {
	return &Manager{
		store:     st,
		storeName: storeName,
		workers:   make(map[string]*Worker),
	}
}

// LoadAndStart reads persisted webhook/WS/MQTT alert configs and starts a
// Worker for each. Errors on individual alerts (or on loading a config
// list) are logged and do not block the rest — the config stays on disk,
// but no worker runs until the underlying problem is fixed.
func (m *Manager) LoadAndStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrManagerClosed
	}

	if wh, err := m.store.LoadWebhookAlerts(); err != nil {
		log.Printf("alerts %s: load webhook alerts: %v", m.storeName, err)
	} else {
		for _, a := range wh.Alerts {
			w, err := m.buildWebhookWorker(a)
			if err != nil {
				log.Printf("alerts %s: failed to start persisted webhook alert %s (%q): %v", m.storeName, a.ID, a.Name, err)
				continue
			}
			m.workers[a.ID] = w
			w.Start()
		}
	}

	if wsa, err := m.store.LoadWSAlerts(); err != nil {
		log.Printf("alerts %s: load ws alerts: %v", m.storeName, err)
	} else {
		for _, a := range wsa.Alerts {
			w, err := m.buildWSWorker(a)
			if err != nil {
				log.Printf("alerts %s: failed to start persisted ws alert %s (%q): %v", m.storeName, a.ID, a.Name, err)
				continue
			}
			m.workers[a.ID] = w
			w.Start()
		}
	}

	if mq, err := m.store.LoadMQTTAlerts(); err != nil {
		log.Printf("alerts %s: load mqtt alerts: %v", m.storeName, err)
	} else {
		for _, a := range mq.Alerts {
			w, err := m.buildMQTTWorker(a)
			if err != nil {
				log.Printf("alerts %s: failed to start persisted mqtt alert %s (%q): %v", m.storeName, a.ID, a.Name, err)
				continue
			}
			m.workers[a.ID] = w
			w.Start()
		}
	}

	return nil
}

// WebhookOptions carries the webhook-only fields in a unified create
// request.
type WebhookOptions struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// WSOptions carries the WS-only fields in a unified create request.
type WSOptions struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MQTTOptions carries the MQTT-only fields in a unified create request.
type MQTTOptions struct {
	BrokerURL string `json:"broker_url"`
	Topic     string `json:"topic"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	QoS       byte   `json:"qos,omitempty"`
}

// CreateAlertRequest is the unified wire shape for `POST
// /api/stores/:store/alerts`. Exactly one of Webhook/WS/MQTT must be
// non-nil and must agree with Type.
type CreateAlertRequest struct {
	Type         string `json:"type"` // "webhook" | "ws" | "mqtt"
	PollInterval string `json:"poll_interval,omitempty"`

	store.AlertCommon

	Webhook *WebhookOptions `json:"webhook,omitempty"`
	WS      *WSOptions      `json:"ws,omitempty"`
	MQTT    *MQTTOptions    `json:"mqtt,omitempty"`
}

// CreateAlert validates the unified request, dispatches to the
// transport-specific persistence + worker construction, and returns the
// worker status.
func (m *Manager) CreateAlert(req CreateAlertRequest) (Status, error) {
	if err := req.AlertCommon.Validate(); err != nil {
		return Status{}, err
	}

	switch req.Type {
	case "webhook":
		if req.Webhook == nil {
			return Status{}, fmt.Errorf("type=webhook requires \"webhook\" options")
		}
		if req.WS != nil || req.MQTT != nil {
			return Status{}, fmt.Errorf("type=webhook must not include \"ws\" or \"mqtt\" options")
		}
		if req.Webhook.URL == "" {
			return Status{}, fmt.Errorf("webhook.url is required")
		}
		return m.createWebhook(req)
	case "ws":
		if req.WS == nil {
			return Status{}, fmt.Errorf("type=ws requires \"ws\" options")
		}
		if req.Webhook != nil || req.MQTT != nil {
			return Status{}, fmt.Errorf("type=ws must not include \"webhook\" or \"mqtt\" options")
		}
		if req.WS.URL == "" {
			return Status{}, fmt.Errorf("ws.url is required")
		}
		return m.createWS(req)
	case "mqtt":
		if req.MQTT == nil {
			return Status{}, fmt.Errorf("type=mqtt requires \"mqtt\" options")
		}
		if req.Webhook != nil || req.WS != nil {
			return Status{}, fmt.Errorf("type=mqtt must not include \"webhook\" or \"ws\" options")
		}
		if req.MQTT.BrokerURL == "" || req.MQTT.Topic == "" {
			return Status{}, fmt.Errorf("mqtt.broker_url and mqtt.topic are required")
		}
		return m.createMQTT(req)
	case "":
		return Status{}, fmt.Errorf("type is required (webhook|ws|mqtt)")
	default:
		return Status{}, fmt.Errorf("unknown alert type %q (expected webhook|ws|mqtt)", req.Type)
	}
}

func (m *Manager) createWebhook(req CreateAlertRequest) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, ErrManagerClosed
	}
	alert := store.WebhookAlert{
		ID:           uuid.New().String()[:8],
		URL:          req.Webhook.URL,
		Headers:      req.Webhook.Headers,
		PollInterval: req.PollInterval,
		Timeout:      req.Webhook.Timeout,
		CreatedAt:    time.Now().UTC(),
		AlertCommon:  req.AlertCommon,
	}
	if err := m.store.AddWebhookAlert(alert); err != nil {
		return Status{}, err
	}
	w, err := m.buildWebhookWorker(alert)
	if err != nil {
		_ = m.store.RemoveWebhookAlert(alert.ID)
		return Status{}, err
	}
	m.workers[alert.ID] = w
	w.Start()
	return w.Status(), nil
}

func (m *Manager) createWS(req CreateAlertRequest) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, ErrManagerClosed
	}
	alert := store.WSAlert{
		ID:           uuid.New().String()[:8],
		URL:          req.WS.URL,
		Headers:      req.WS.Headers,
		PollInterval: req.PollInterval,
		CreatedAt:    time.Now().UTC(),
		AlertCommon:  req.AlertCommon,
	}
	if err := m.store.AddWSAlert(alert); err != nil {
		return Status{}, err
	}
	w, err := m.buildWSWorker(alert)
	if err != nil {
		_ = m.store.RemoveWSAlert(alert.ID)
		return Status{}, err
	}
	m.workers[alert.ID] = w
	w.Start()
	return w.Status(), nil
}

func (m *Manager) createMQTT(req CreateAlertRequest) (Status, error) {
	qos := req.MQTT.QoS
	if qos == 0 {
		qos = 1 // default at-least-once
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, ErrManagerClosed
	}
	alert := store.MQTTAlert{
		ID:           uuid.New().String()[:8],
		BrokerURL:    req.MQTT.BrokerURL,
		Topic:        req.MQTT.Topic,
		Username:     req.MQTT.Username,
		Password:     req.MQTT.Password,
		QoS:          qos,
		PollInterval: req.PollInterval,
		CreatedAt:    time.Now().UTC(),
		AlertCommon:  req.AlertCommon,
	}
	if err := m.store.AddMQTTAlert(alert); err != nil {
		return Status{}, err
	}
	w, err := m.buildMQTTWorker(alert)
	if err != nil {
		_ = m.store.RemoveMQTTAlert(alert.ID)
		return Status{}, err
	}
	m.workers[alert.ID] = w
	w.Start()
	return w.Status(), nil
}

// AllMetrics returns a per-worker snapshot of activity counters for every
// alert managed here. Used by the /metrics endpoint.
func (m *Manager) AllMetrics() []Metrics {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Metrics, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, w.Metrics())
	}
	return out
}

// ResetMetrics zeros activity counters on every worker.
func (m *Manager) ResetMetrics() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.workers {
		w.ResetMetrics()
	}
}

// ListAlerts returns a snapshot of all worker statuses, across all types.
func (m *Manager) ListAlerts() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Status, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, w.Status())
	}
	return out
}

// GetAlert returns the status of a single alert by ID.
func (m *Manager) GetAlert(alertID string) (Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.workers[alertID]
	if !ok {
		return Status{}, ErrNotFound
	}
	return w.Status(), nil
}

// AlertDetail combines a Worker's runtime Status with the persisted
// configuration for the alert. Used by GET /alerts/{id} so callers can
// see both "what is this alert configured to do?" and "what is it
// currently doing?" in one response.
//
// Exactly one of WebhookConfig, WSConfig, or MQTTConfig is non-nil,
// matching Status.Type.
type AlertDetail struct {
	Status        Status              `json:",inline"`
	WebhookConfig *store.WebhookAlert `json:"webhook,omitempty"`
	WSConfig      *store.WSAlert      `json:"ws,omitempty"`
	MQTTConfig    *store.MQTTAlert    `json:"mqtt,omitempty"`
}

// MarshalJSON flattens Status into the top-level object so the response
// reads naturally instead of nesting status under a key. Go's struct-tag
// `json:",inline"` doesn't actually flatten — that's a yaml tag — so we
// do it by hand.
func (d AlertDetail) MarshalJSON() ([]byte, error) {
	// Marshal Status as the base object, then merge the config field.
	base, err := json.Marshal(d.Status)
	if err != nil {
		return nil, err
	}
	// Strip the trailing "}" and append our config field if any.
	if len(base) == 0 || base[len(base)-1] != '}' {
		return base, nil // shouldn't happen with a struct
	}
	var configKey string
	var configVal interface{}
	switch {
	case d.WebhookConfig != nil:
		configKey = "webhook"
		configVal = d.WebhookConfig
	case d.WSConfig != nil:
		configKey = "ws"
		configVal = d.WSConfig
	case d.MQTTConfig != nil:
		configKey = "mqtt"
		configVal = d.MQTTConfig
	default:
		return base, nil
	}
	configBytes, err := json.Marshal(configVal)
	if err != nil {
		return nil, err
	}
	// base is `{...}`, configBytes is `{...}`. We want `{...,"key":{...}}`.
	out := make([]byte, 0, len(base)+len(configBytes)+len(configKey)+5)
	out = append(out, base[:len(base)-1]...)
	if len(base) > 2 { // not just "{}"
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, configKey...)
	out = append(out, '"', ':')
	out = append(out, configBytes...)
	out = append(out, '}')
	return out, nil
}

// sensitiveHeaderNames are HTTP header names whose values are masked when
// returned via the read API. Matched case-insensitively. The values are
// still persisted as-is on disk; this redaction is purely about not
// echoing secrets back over the network on read.
var sensitiveHeaderNames = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"x-api-key":           {},
	"x-auth-token":        {},
	"x-access-token":      {},
}

// redactURL strips userinfo and query values from a sink URL for display.
// Webhook URLs can carry bearer secrets in userinfo or query params; the
// scheme/host/path is what identifies the target. On-disk values are
// unchanged — this only shapes what read APIs and status echo back.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable-url]"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "[redacted]"
	}
	return u.String()
}

func redactHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if _, isSensitive := sensitiveHeaderNames[strings.ToLower(k)]; isSensitive && v != "" {
			out[k] = "[redacted]"
		} else {
			out[k] = v
		}
	}
	return out
}

// GetAlertDetail returns the worker status plus the persisted config for
// an alert. Secrets (MQTT password, common auth-style HTTP headers) are
// redacted; the on-disk values are unchanged.
func (m *Manager) GetAlertDetail(alertID string) (AlertDetail, error) {
	m.mu.RLock()
	w, ok := m.workers[alertID]
	m.mu.RUnlock()
	if !ok {
		return AlertDetail{}, ErrNotFound
	}
	status := w.Status()
	detail := AlertDetail{Status: status}

	// Read the persisted config matching the worker's type. Reading
	// outside the manager lock is fine: the store's own mutex protects
	// the file load, and the worker can't change type at runtime.
	switch status.Type {
	case "webhook":
		cfg, err := m.store.GetWebhookAlert(alertID)
		if err != nil {
			return AlertDetail{}, err
		}
		redacted := *cfg
		redacted.Headers = redactHeaders(cfg.Headers)
		redacted.URL = redactURL(cfg.URL)
		detail.WebhookConfig = &redacted
	case "ws":
		cfg, err := m.store.GetWSAlert(alertID)
		if err != nil {
			return AlertDetail{}, err
		}
		redacted := *cfg
		redacted.Headers = redactHeaders(cfg.Headers)
		redacted.URL = redactURL(cfg.URL)
		detail.WSConfig = &redacted
	case "mqtt":
		cfg, err := m.store.GetMQTTAlert(alertID)
		if err != nil {
			return AlertDetail{}, err
		}
		redacted := *cfg
		if redacted.Password != "" {
			redacted.Password = "[redacted]"
		}
		redacted.BrokerURL = redactURL(cfg.BrokerURL)
		detail.MQTTConfig = &redacted
	}

	return detail, nil
}

// DeleteAlert stops the worker, removes the persisted config, and deletes
// the cursor file. The alert is located by scanning each of the three
// persisted alert lists.
func (m *Manager) DeleteAlert(alertID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.workers[alertID]
	if !ok {
		return ErrNotFound
	}
	w.Stop()
	delete(m.workers, alertID)

	// Try each store remover; one will succeed, the others return
	// ErrObjectNotFound which we ignore.
	if err := m.store.RemoveWebhookAlert(alertID); err == nil {
		removeCursor(m.store, "webhook", alertID)
		return nil
	}
	if err := m.store.RemoveWSAlert(alertID); err == nil {
		removeCursor(m.store, "ws", alertID)
		return nil
	}
	if err := m.store.RemoveMQTTAlert(alertID); err == nil {
		removeCursor(m.store, "mqtt", alertID)
		return nil
	}
	// Worker existed in memory but no persisted config — clean up anyway.
	return nil
}

// Stop stops all workers. Safe to call multiple times.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	for _, w := range m.workers {
		w.Stop()
	}
	m.workers = make(map[string]*Worker)
	return nil
}

// --- worker builders ---

func (m *Manager) buildWebhookWorker(a store.WebhookAlert) (*Worker, error) {
	sink, err := NewWebhookSink(a.URL, a.Headers, a.Timeout)
	if err != nil {
		return nil, err
	}
	w, err := NewWorker(Options{
		Store:        m.store,
		StoreName:    m.storeName,
		ID:           a.ID,
		Type:         "webhook",
		Target:       redactURL(a.URL),
		Rule:         a.AlertCommon,
		Sink:         sink,
		PollInterval: a.PollInterval,
		CursorPath:   cursorPathFor(m.store, "webhook", a.ID),
		CreatedAt:    a.CreatedAt,
	})
	if err != nil {
		sink.Close() // sink owns a running goroutine; don't leak it
		return nil, err
	}
	// Webhook deliveries are async — surface their failures in the
	// worker's status/metrics rather than only the process log.
	sink.SetOnError(w.noteDeliveryFailure)
	return w, nil
}

func (m *Manager) buildWSWorker(a store.WSAlert) (*Worker, error) {
	sink := NewWSSink(a.URL, a.Headers)
	return NewWorker(Options{
		Store:        m.store,
		StoreName:    m.storeName,
		ID:           a.ID,
		Type:         "ws",
		Target:       redactURL(a.URL),
		Rule:         a.AlertCommon,
		Sink:         sink,
		PollInterval: a.PollInterval,
		CursorPath:   cursorPathFor(m.store, "ws", a.ID),
		CreatedAt:    a.CreatedAt,
	})
}

func (m *Manager) buildMQTTWorker(a store.MQTTAlert) (*Worker, error) {
	clientID := fmt.Sprintf("tsstore-%s-alert-%s", m.storeName, a.ID)
	sink := NewMQTTSink(a.BrokerURL, a.Topic, a.Username, a.Password, a.QoS, clientID)
	return NewWorker(Options{
		Store:        m.store,
		StoreName:    m.storeName,
		ID:           a.ID,
		Type:         "mqtt",
		Target:       fmt.Sprintf("%s -> %s", redactURL(a.BrokerURL), a.Topic),
		Rule:         a.AlertCommon,
		Sink:         sink,
		PollInterval: a.PollInterval,
		CursorPath:   cursorPathFor(m.store, "mqtt", a.ID),
		CreatedAt:    a.CreatedAt,
	})
}

// cursorPathFor returns a per-alert cursor file under the store directory.
func cursorPathFor(st *store.Store, alertType, alertID string) string {
	return filepath.Join(st.StorePath(), alertType+"_alert_"+alertID+".cursor")
}

// removeCursor deletes the cursor file. Errors are silently ignored — a
// missing file is fine, and we don't want to fail Delete on cleanup quirks.
func removeCursor(st *store.Store, alertType, alertID string) {
	_ = os.Remove(cursorPathFor(st, alertType, alertID))
}
