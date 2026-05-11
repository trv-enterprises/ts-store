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

// CreateWebhook handles POST /api/stores/:store/alerts/webhook
func (h *AlertsHandler) CreateWebhook(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	var req alerts.CreateWebhookAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, err := mgr.CreateWebhookAlert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, status)
}

// CreateWS handles POST /api/stores/:store/alerts/ws
func (h *AlertsHandler) CreateWS(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	var req alerts.CreateWSAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, err := mgr.CreateWSAlert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, status)
}

// CreateMQTT handles POST /api/stores/:store/alerts/mqtt
func (h *AlertsHandler) CreateMQTT(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	var req alerts.CreateMQTTAlertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, err := mgr.CreateMQTTAlert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, status)
}

// List handles GET /api/stores/:store/alerts — returns all three types tagged.
func (h *AlertsHandler) List(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"alerts": mgr.ListAlerts()})
}

// Get handles GET /api/stores/:store/alerts/:id
func (h *AlertsHandler) Get(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	status, err := mgr.GetAlert(c.Param("id"))
	if err != nil {
		if errors.Is(err, alerts.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
