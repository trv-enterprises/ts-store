// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/mqtt"
)

// MQTTHandler handles MQTT connection management endpoints.
type MQTTHandler struct {
	getManager func(storeName string) *mqtt.Manager
}

// NewMQTTHandler creates a new MQTT handler.
func NewMQTTHandler(getManager func(storeName string) *mqtt.Manager) *MQTTHandler {
	return &MQTTHandler{
		getManager: getManager,
	}
}

// List returns all MQTT connections for a store.
func (h *MQTTHandler) List(c *gin.Context) {
	storeName := c.Param("store")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return
	}

	connections := manager.ListConnections()
	c.JSON(http.StatusOK, gin.H{"connections": connections})
}

// Create creates a new MQTT connection.
func (h *MQTTHandler) Create(c *gin.Context) {
	storeName := c.Param("store")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return
	}

	var req mqtt.CreateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.BrokerURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "broker_url is required"})
		return
	}

	if req.Topic == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "topic is required"})
		return
	}

	status, err := manager.CreateConnection(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, status)
}

// Get returns a specific MQTT connection.
func (h *MQTTHandler) Get(c *gin.Context) {
	storeName := c.Param("store")
	connID := c.Param("id")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return
	}

	status, err := manager.GetConnection(connID)
	if err != nil {
		if err == mqtt.ErrConnectionNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, status)
}

// Delete removes an MQTT connection.
func (h *MQTTHandler) Delete(c *gin.Context) {
	storeName := c.Param("store")
	connID := c.Param("id")

	manager := h.getManager(storeName)
	if manager == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return
	}

	if err := manager.DeleteConnection(connID); err != nil {
		if err == mqtt.ErrConnectionNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
