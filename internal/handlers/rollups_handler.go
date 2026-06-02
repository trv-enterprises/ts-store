// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/rollups"
)

// RollupsHandler exposes rollup CRUD under /api/stores/:store/rollups, scoped
// to the SOURCE store named in the path.
type RollupsHandler struct {
	getManager func(storeName string) *rollups.Manager
}

// NewRollupsHandler creates a new rollups handler.
func NewRollupsHandler(getManager func(storeName string) *rollups.Manager) *RollupsHandler {
	return &RollupsHandler{getManager: getManager}
}

func (h *RollupsHandler) resolveManager(c *gin.Context) *rollups.Manager {
	storeName := middleware.GetStoreName(c)
	mgr := h.getManager(storeName)
	if mgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found or not open"})
		return nil
	}
	return mgr
}

// Create handles POST /api/stores/:store/rollups. Auto-creates and sizes the
// target store if it doesn't exist.
func (h *RollupsHandler) Create(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	var req rollups.CreateRollupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, err := mgr.CreateRollup(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, status)
}

// List handles GET /api/stores/:store/rollups
func (h *RollupsHandler) List(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"rollups": mgr.ListRollups()})
}

// Get handles GET /api/stores/:store/rollups/:id
func (h *RollupsHandler) Get(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	status, err := mgr.GetRollup(c.Param("id"))
	if err != nil {
		if errors.Is(err, rollups.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "rollup not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

// Delete handles DELETE /api/stores/:store/rollups/:id. Removes the rollup
// config and stops its worker; the target store is left intact.
func (h *RollupsHandler) Delete(c *gin.Context) {
	mgr := h.resolveManager(c)
	if mgr == nil {
		return
	}
	if err := mgr.DeleteRollup(c.Param("id")); err != nil {
		if errors.Is(err, rollups.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "rollup not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "rollup deleted"})
}
