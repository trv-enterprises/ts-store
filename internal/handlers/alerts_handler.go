// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/alerts"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/rules"
)

// AlertsHandler exposes webhook/WS/MQTT alert CRUD under /api/stores/:store/alerts.
type AlertsHandler struct {
	getManager func(storeName string) *alerts.Manager
}

// NewAlertsHandler creates a new alerts handler.
func NewAlertsHandler(getManager func(storeName string) *alerts.Manager) *AlertsHandler {
	return &AlertsHandler{getManager: getManager}
}

// resolveManager returns the store's alerts manager, or writes a 404 and
// returns nil. getManager is expected to OPEN a store that exists on disk but
// isn't open yet (see StoreService.GetAlertsManagerOrOpen) — so a nil here
// means the store genuinely does not exist, not merely that nothing has
// touched it since the last restart.
func (h *AlertsHandler) resolveManager(c *gin.Context) *alerts.Manager {
	storeName := middleware.GetStoreName(c)
	mgr := h.getManager(storeName)
	if mgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return nil
	}
	return mgr
}

// Create handles POST /api/stores/:store/alerts. The "type" field on
// the request body discriminates webhook / ws / mqtt; the matching
// nested options object supplies the transport-specific fields.
func (h *AlertsHandler) Create(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	var req alerts.CreateAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, err := mgr.CreateAlert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, status)
}

// List handles GET /api/stores/:store/alerts — returns all three types tagged,
// each entry joining the alert's status with its runtime activity counters
// (records_evaluated, records_matched, records_dropped, alerts_dropped) so a
// caller sees rule health, not just configuration.
func (h *AlertsHandler) List(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"alerts": mergeAlerts(mgr.ListAlerts(), mgr.AllMetrics())})
}

// Get handles GET /api/stores/:store/alerts/:id — returns the worker
// status plus the persisted config (with auth-style headers and MQTT
// passwords redacted).
func (h *AlertsHandler) Get(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	detail, err := mgr.GetAlertDetail(c.Param("id"))
	if err != nil {
		if errors.Is(err, alerts.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

// Update handles PUT /api/stores/:store/alerts/:id — in-place edit of an
// existing alert (issue #166). The body is the same shape as create; the
// id in the path is authoritative and the type may not change.
//
// Semantics are full-replace, matching create: an omitted field reverts to
// its default. The exception is secrets the read API redacts (auth-style
// headers, the MQTT password) — a client that only ever saw "[redacted]"
// cannot round-trip them, so the marker (or omission) keeps the stored
// value. Without that, editing a rule whose credentials you don't hold
// would be impossible.
func (h *AlertsHandler) Update(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}

	var req alerts.CreateAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	status, err := mgr.UpdateAlert(c.Param("id"), req)
	if err != nil {
		switch {
		case errors.Is(err, alerts.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		case errors.Is(err, alerts.ErrManagerClosed):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			// Validation failures (bad condition, bad URL, type change)
			// are client errors, same as on create.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, status)
}

// Delete handles DELETE /api/stores/:store/alerts/:id
func (h *AlertsHandler) Delete(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	if err := mgr.DeleteAlert(c.Param("id")); err != nil {
		if errors.Is(err, alerts.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "alert deleted"})
}

// TestAlertRequest is the wire shape for POST /api/stores/:store/alerts/test.
type TestAlertRequest struct {
	Condition string                 `json:"condition"`
	Data      map[string]interface{} `json:"data"`
}

// TestedCondition echoes one parsed comparison back to the caller.
type TestedCondition struct {
	Field    string      `json:"field"`
	Operator string      `json:"operator"`
	Value    interface{} `json:"value"`
}

// TestAlertResponse reports how the condition parsed and whether it matched
// the sample record.
type TestAlertResponse struct {
	Matched    bool              `json:"matched"`
	LogicalOp  string            `json:"logical_op,omitempty"`
	Conditions []TestedCondition `json:"conditions"`
}

// Test handles POST /api/stores/:store/alerts/test: dry-runs a condition
// against a sample record without creating an alert. A condition on a
// misspelled field otherwise validates, persists, and silently never fires —
// this is the only way to see what a rule actually does before going live.
func (h *AlertsHandler) Test(c *gin.Context) {
	var req TestAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Condition == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "condition is required"})
		return
	}

	rule, err := rules.Parse("test", req.Condition)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp := TestAlertResponse{
		Matched:    rule.Evaluate(req.Data),
		LogicalOp:  rule.LogicalOp,
		Conditions: make([]TestedCondition, 0, len(rule.Conditions)),
	}
	for _, cond := range rule.Conditions {
		resp.Conditions = append(resp.Conditions, TestedCondition{
			Field:    cond.Field,
			Operator: string(cond.Operator),
			Value:    cond.Value,
		})
	}
	c.JSON(http.StatusOK, resp)
}
