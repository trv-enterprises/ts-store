// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"net/http"
	"strconv"

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

// SchemaVersionsResponse lists all schema versions for a store.
type SchemaVersionsResponse struct {
	CurrentVersion int              `json:"current_version"`
	Versions       []SchemaResponse `json:"versions"`
}

// Get handles GET /api/stores/:store/schema
// Returns the current schema for schema-type stores, or a specific version when
// the ?version=<N> query parameter is supplied.
func (h *SchemaHandler) Get(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}

	if st.DataType() != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "schema endpoint only available for schema-type stores"})
		return
	}

	// Specific version requested via ?version=N.
	if v := c.Query("version"); v != "" {
		version, err := strconv.Atoi(v)
		if err != nil || version <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
			return
		}
		ss := st.GetSchemaSet()
		if ss == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no schema defined"})
			return
		}
		sch, err := ss.GetSchema(version)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, SchemaResponse{
			Version: sch.Version,
			Fields:  sch.Fields,
		})
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

// ListVersions handles GET /api/stores/:store/schema/versions
// Returns every schema version for a schema-type store, in ascending order.
func (h *SchemaHandler) ListVersions(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}

	if st.DataType() != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "schema endpoint only available for schema-type stores"})
		return
	}

	ss := st.GetSchemaSet()
	if ss == nil || ss.CurrentVersion == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no schema defined"})
		return
	}

	versions := make([]SchemaResponse, 0, ss.CurrentVersion)
	for v := 1; v <= ss.CurrentVersion; v++ {
		sch, err := ss.GetSchema(v)
		if err != nil {
			continue
		}
		versions = append(versions, SchemaResponse{
			Version: sch.Version,
			Fields:  sch.Fields,
		})
	}

	c.JSON(http.StatusOK, SchemaVersionsResponse{
		CurrentVersion: ss.CurrentVersion,
		Versions:       versions,
	})
}

// Put handles PUT /api/stores/:store/schema
// Sets or updates the schema for schema-type stores.
// For updates, the new schema must be append-only compatible.
func (h *SchemaHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
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
