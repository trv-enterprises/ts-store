// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/alerts"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/mqtt"
	"github.com/tviviano/ts-store/internal/version"
	"github.com/tviviano/ts-store/internal/ws"
)

// ConnectionsHandler serves the consolidated, read-only view of everything
// wired to a store: outbound/inbound WebSocket connections, MQTT sink
// connections, and (opt-in) the configured alert rules with their runtime
// status. It exists so a caller learns the full I/O picture in one request
// instead of three.
type ConnectionsHandler struct {
	getWS     func(storeName string) *ws.Manager
	getMQTT   func(storeName string) *mqtt.Manager
	getAlerts func(storeName string) *alerts.Manager
	hostname  string
}

// NewConnectionsHandler wires the three per-store managers.
func NewConnectionsHandler(
	getWS func(string) *ws.Manager,
	getMQTT func(string) *mqtt.Manager,
	getAlerts func(string) *alerts.Manager,
) *ConnectionsHandler {
	// Hostname is part of the response envelope (see List). It can't change
	// while the process runs, so resolve it once; an empty string on error is
	// an acceptable degradation.
	hostname, _ := os.Hostname()
	return &ConnectionsHandler{getWS: getWS, getMQTT: getMQTT, getAlerts: getAlerts, hostname: hostname}
}

// AlertListEntry is an alert's persisted status joined with its runtime
// activity counters. The counters already exist via alerts.Manager.AllMetrics;
// this merges them onto the status so a single list carries both "what is it"
// and "how is it doing" — including alerts_dropped, the alert-side equivalent
// of a connection's error count.
type AlertListEntry struct {
	alerts.Status
	RecordsEvaluated int64 `json:"records_evaluated"`
	RecordsMatched   int64 `json:"records_matched"`
	AlertsDropped    int64 `json:"alerts_dropped"`
}

// mergeAlerts zips per-alert Status with per-alert Metrics by ID. Metrics
// without a matching status (or vice versa) are tolerated — the join is
// best-effort and keyed on the stable alert ID.
func mergeAlerts(statuses []alerts.Status, metrics []alerts.Metrics) []AlertListEntry {
	byID := make(map[string]alerts.Metrics, len(metrics))
	for _, m := range metrics {
		byID[m.ID] = m
	}
	out := make([]AlertListEntry, 0, len(statuses))
	for _, s := range statuses {
		m := byID[s.ID] // zero value if absent
		out = append(out, AlertListEntry{
			Status:           s,
			RecordsEvaluated: m.RecordsEvaluated,
			RecordsMatched:   m.RecordsMatched,
			AlertsDropped:    m.AlertsDropped,
		})
	}
	return out
}

// List handles GET /api/stores/:store/connections.
//
// Returns WebSocket and MQTT connections always; includes the alert rules
// (with merged runtime counters) only when ?include_alerts=true. A store with
// no connections of a given kind yields an empty array for that key rather than
// an error — having nothing wired is a valid state.
//
// The response carries a "meta" envelope identifying where the data came from
// (store, host, server_version) and when (as_of), so consumers rendering it
// can detect a misdirected connection or a stale feed instead of trusting the
// request URL.
func (h *ConnectionsHandler) List(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	wsConns := []ws.ConnectionStatus{}
	if mgr := h.getWS(storeName); mgr != nil {
		wsConns = mgr.ListConnections()
	}

	mqttConns := []mqtt.ConnectionStatus{}
	if mgr := h.getMQTT(storeName); mgr != nil {
		mqttConns = mgr.ListConnections()
	}

	resp := gin.H{
		"meta": gin.H{
			"store":          storeName,
			"host":           h.hostname,
			"as_of":          time.Now().UTC().Format(time.RFC3339),
			"server_version": version.Version,
		},
		"ws":   wsConns,
		"mqtt": mqttConns,
	}

	if c.Query("include_alerts") == "true" {
		entries := []AlertListEntry{}
		if mgr := h.getAlerts(storeName); mgr != nil {
			entries = mergeAlerts(mgr.ListAlerts(), mgr.AllMetrics())
		}
		resp["alerts"] = entries
	}

	c.JSON(http.StatusOK, resp)
}
