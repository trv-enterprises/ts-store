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
)

// AlertsHandler exposes webhook/WS/MQTT alert CRUD under /api/stores/:store/alerts.
type AlertsHandler struct {
	getManager func(storeName string) *alerts.Manager
}

// NewAlertsHandler creates a new alerts handler.
func NewAlertsHandler(getManager func(storeName string) *alerts.Manager) *AlertsHandler {
	return &AlertsHandler{getManager: getManager}
}

func (h *AlertsHandler) resolveManager(c *gin.Context) *alerts.Manager {
	storeName := middleware.GetStoreName(c)
	mgr := h.getManager(storeName)
	if mgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
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
// (records_evaluated, records_matched, alerts_dropped) so a caller sees rule
// health, not just configuration.
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
