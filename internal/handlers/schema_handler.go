// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/schema"
	"github.com/tviviano/ts-store/pkg/store"
)

// SchemaHandler handles schema management endpoints.
type SchemaHandler struct {
	storeService *service.StoreService
}

// NewSchemaHandler creates a new schema handler.
func NewSchemaHandler(storeService *service.StoreService) *SchemaHandler {
	return &SchemaHandler{
		storeService: storeService,
	}
}

// SchemaRequest represents a request to set or update a schema.
type SchemaRequest struct {
	Fields []schema.Field `json:"fields" binding:"required"`
}

// SchemaResponse represents a schema response.
type SchemaResponse struct {
	Version int            `json:"version"`
	Fields  []schema.Field `json:"fields"`
}

// Get handles GET /api/stores/:store/schema
// Returns the current schema for schema-type stores.
func (h *SchemaHandler) Get(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if st.DataType() != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "schema endpoint only available for schema-type stores"})
		return
	}

	sch, err := st.GetSchema()
	if err != nil {
		if err == store.ErrSchemaRequired {
			c.JSON(http.StatusNotFound, gin.H{"error": "no schema defined"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, SchemaResponse{
		Version: sch.Version,
		Fields:  sch.Fields,
	})
}

// Put handles PUT /api/stores/:store/schema
// Sets or updates the schema for schema-type stores.
// For updates, the new schema must be append-only compatible.
func (h *SchemaHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if st.DataType() != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "schema endpoint only available for schema-type stores"})
		return
	}

	var req SchemaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sch := &schema.Schema{
		Fields: req.Fields,
	}

	version, err := st.SetSchema(sch)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "schema updated",
		"version": version,
	})
}
