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
	BlockNum      uint32 `json:"block_num"`
	Timestamp     int64  `json:"timestamp"`
	Data          string `json:"data"` // Base64 encoded
	AttachedCount uint32 `json:"attached_count"`
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
		BlockNum:      blockNum,
		Timestamp:     header.Timestamp,
		Data:          base64.StdEncoding.EncodeToString(data),
		AttachedCount: header.AttachedCount,
	})
}

// GetByBlock handles GET /api/stores/:store/data/block/:blocknum
func (h *DataHandler) GetByBlock(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	blockNumStr := c.Param("blocknum")
	blockNum64, err := strconv.ParseUint(blockNumStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid block number"})
		return
	}
	blockNum := uint32(blockNum64)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		BlockNum:      blockNum,
		Timestamp:     header.Timestamp,
		Data:          base64.StdEncoding.EncodeToString(data),
		AttachedCount: header.AttachedCount,
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
			BlockNum:      blockNum,
			Timestamp:     header.Timestamp,
			Data:          base64.StdEncoding.EncodeToString(data),
			AttachedCount: header.AttachedCount,
		})
	}

	c.JSON(http.StatusOK, RangeResponse{
		Blocks: blocks,
		Count:  len(blocks),
	})
}

// AttachRequest represents an attach block request.
type AttachRequest struct {
	Data string `json:"data"` // Base64 encoded data
}

// AttachResponse represents the attach response.
type AttachResponse struct {
	PrimaryBlockNum  uint32 `json:"primary_block_num"`
	AttachedBlockNum uint32 `json:"attached_block_num"`
}

// AttachByBlock handles POST /api/stores/:store/data/block/:blocknum/attach
func (h *DataHandler) AttachByBlock(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	blockNumStr := c.Param("blocknum")
	blockNum64, err := strconv.ParseUint(blockNumStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid block number"})
		return
	}
	primaryBlockNum := uint32(blockNum64)

	var req AttachRequest
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

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Attach new block
	attachedBlockNum, err := st.AttachBlock(primaryBlockNum)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Write data to attached block
	if len(data) > 0 {
		if err := st.WriteBlockData(attachedBlockNum, data); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusCreated, AttachResponse{
		PrimaryBlockNum:  primaryBlockNum,
		AttachedBlockNum: attachedBlockNum,
	})
}

// GetAttached handles GET /api/stores/:store/data/block/:blocknum/attached
func (h *DataHandler) GetAttached(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	blockNumStr := c.Param("blocknum")
	blockNum64, err := strconv.ParseUint(blockNumStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid block number"})
		return
	}
	primaryBlockNum := uint32(blockNum64)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Get attached block numbers
	attachedNums, err := st.GetAttachedBlocks(primaryBlockNum)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Read data from each attached block
	blocks := make([]BlockResponse, 0, len(attachedNums))
	for _, blockNum := range attachedNums {
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

	c.JSON(http.StatusOK, gin.H{
		"primary_block_num": primaryBlockNum,
		"attached_blocks":   blocks,
		"count":             len(blocks),
	})
}

// ReclaimRequest represents a reclaim request.
type ReclaimRequest struct {
	StartBlock *uint32 `json:"start_block,omitempty"`
	EndBlock   *uint32 `json:"end_block,omitempty"`
	StartTime  *int64  `json:"start_time,omitempty"`
	EndTime    *int64  `json:"end_time,omitempty"`
}

// Reclaim handles POST /api/stores/:store/data/reclaim
func (h *DataHandler) Reclaim(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	var req ReclaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Reclaim by block range
	if req.StartBlock != nil && req.EndBlock != nil {
		if err := st.AddRangeToFreeList(*req.StartBlock, *req.EndBlock); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "blocks reclaimed"})
		return
	}

	// Reclaim by time range
	if req.StartTime != nil && req.EndTime != nil {
		if err := st.AddRangeToFreeListByTime(*req.StartTime, *req.EndTime); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "blocks reclaimed"})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "must specify block range or time range"})
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
