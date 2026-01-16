// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package handlers

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

// DataHandler handles data operation endpoints.
type DataHandler struct {
	storeService *service.StoreService
}

// NewDataHandler creates a new data handler.
func NewDataHandler(storeService *service.StoreService) *DataHandler {
	return &DataHandler{
		storeService: storeService,
	}
}

// InsertRequest represents a data insert request.
type InsertRequest struct {
	Timestamp int64  `json:"timestamp,omitempty"` // Unix nanoseconds, optional (defaults to now)
	Data      string `json:"data"`                // Base64 encoded data
}

// InsertResponse represents the insert response.
type InsertResponse struct {
	BlockNum  uint32 `json:"block_num"`
	Timestamp int64  `json:"timestamp"`
}

// Insert handles POST /api/stores/:store/data
func (h *DataHandler) Insert(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	var req InsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Decode base64 data
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base64 data"})
		return
	}

	// Get store
	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Use provided timestamp or current time
	timestamp := req.Timestamp
	if timestamp == 0 {
		timestamp = time.Now().UnixNano()
	}

	// Insert data
	blockNum, err := st.Insert(timestamp, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, InsertResponse{
		BlockNum:  blockNum,
		Timestamp: timestamp,
	})
}

// BlockResponse represents a block data response.
type BlockResponse struct {
	BlockNum  uint32 `json:"block_num"`
	Timestamp int64  `json:"timestamp"`
	Data      string `json:"data"` // Base64 encoded
}

// GetByTime handles GET /api/stores/:store/data/time/:timestamp
func (h *DataHandler) GetByTime(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	timestampStr := c.Param("timestamp")
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timestamp"})
		return
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Find block by time
	blockNum, err := st.FindBlockByTime(timestamp)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Read block data
	data, err := st.ReadBlockData(blockNum)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Get header for metadata
	header, err := st.GetBlockHeader(blockNum)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, BlockResponse{
		BlockNum:  blockNum,
		Timestamp: header.Timestamp,
		Data:      base64.StdEncoding.EncodeToString(data),
	})
}

// RangeRequest represents a time range query.
type RangeRequest struct {
	StartTime int64 `form:"start_time" binding:"required"`
	EndTime   int64 `form:"end_time" binding:"required"`
}

// RangeResponse represents the range query response.
type RangeResponse struct {
	Blocks []BlockResponse `json:"blocks"`
	Count  int             `json:"count"`
}

// GetRange handles GET /api/stores/:store/data/range
func (h *DataHandler) GetRange(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	var req RangeRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Find blocks in range
	blockNums, err := st.FindBlocksInRange(req.StartTime, req.EndTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Build response
	blocks := make([]BlockResponse, 0, len(blockNums))
	for _, blockNum := range blockNums {
		data, err := st.ReadBlockData(blockNum)
		if err != nil {
			continue
		}

		header, err := st.GetBlockHeader(blockNum)
		if err != nil {
			continue
		}

		blocks = append(blocks, BlockResponse{
			BlockNum:  blockNum,
			Timestamp: header.Timestamp,
			Data:      base64.StdEncoding.EncodeToString(data),
		})
	}

	c.JSON(http.StatusOK, RangeResponse{
		Blocks: blocks,
		Count:  len(blocks),
	})
}

// GetOldest handles GET /api/stores/:store/data/oldest
func (h *DataHandler) GetOldest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	timestamp, err := st.GetOldestTimestamp()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"oldest_timestamp": timestamp})
}

// GetNewest handles GET /api/stores/:store/data/newest
func (h *DataHandler) GetNewest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	timestamp, err := st.GetNewestTimestamp()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"newest_timestamp": timestamp})
}
