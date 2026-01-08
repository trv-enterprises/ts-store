// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package handlers

import (
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// ObjectHandler handles object-level operations (large data spanning multiple blocks).
type ObjectHandler struct {
	storeService *service.StoreService
}

// NewObjectHandler creates a new object handler.
func NewObjectHandler(storeService *service.StoreService) *ObjectHandler {
	return &ObjectHandler{
		storeService: storeService,
	}
}

// PutObjectRequest represents a request to store an object.
type PutObjectRequest struct {
	Timestamp int64  `json:"timestamp,omitempty"` // Unix nanoseconds, optional (defaults to now)
	Data      string `json:"data"`                // Base64 encoded data
}

// ObjectHandleResponse represents the response after storing an object.
type ObjectHandleResponse struct {
	Timestamp       int64  `json:"timestamp"`
	PrimaryBlockNum uint32 `json:"primary_block_num"`
	TotalSize       uint32 `json:"total_size"`
	BlockCount      uint32 `json:"block_count"`
}

// ObjectDataResponse represents a response containing object data.
type ObjectDataResponse struct {
	Timestamp       int64  `json:"timestamp"`
	PrimaryBlockNum uint32 `json:"primary_block_num"`
	TotalSize       uint32 `json:"total_size"`
	BlockCount      uint32 `json:"block_count"`
	Data            string `json:"data"` // Base64 encoded
}

// Put handles POST /api/stores/:store/objects
// Stores an object that may span multiple blocks.
func (h *ObjectHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	var req PutObjectRequest
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

	// Store object
	var handle *store.ObjectHandle
	if req.Timestamp > 0 {
		handle, err = st.PutObject(req.Timestamp, data)
	} else {
		handle, err = st.PutObjectNow(data)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, ObjectHandleResponse{
		Timestamp:       handle.Timestamp,
		PrimaryBlockNum: handle.PrimaryBlockNum,
		TotalSize:       handle.TotalSize,
		BlockCount:      handle.BlockCount,
	})
}

// GetByTime handles GET /api/stores/:store/objects/time/:timestamp
// Retrieves an object by its timestamp.
func (h *ObjectHandler) GetByTime(c *gin.Context) {
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

	data, handle, err := st.GetObjectByTime(timestamp)
	if err != nil {
		if err == store.ErrTimestampNotFound || err == store.ErrEmptyStore {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, ObjectDataResponse{
		Timestamp:       handle.Timestamp,
		PrimaryBlockNum: handle.PrimaryBlockNum,
		TotalSize:       handle.TotalSize,
		BlockCount:      handle.BlockCount,
		Data:            base64.StdEncoding.EncodeToString(data),
	})
}

// GetByBlock handles GET /api/stores/:store/objects/block/:blocknum
// Retrieves an object by its primary block number.
func (h *ObjectHandler) GetByBlock(c *gin.Context) {
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

	data, handle, err := st.GetObjectByBlock(blockNum)
	if err != nil {
		if err == store.ErrBlockOutOfRange {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, ObjectDataResponse{
		Timestamp:       handle.Timestamp,
		PrimaryBlockNum: handle.PrimaryBlockNum,
		TotalSize:       handle.TotalSize,
		BlockCount:      handle.BlockCount,
		Data:            base64.StdEncoding.EncodeToString(data),
	})
}

// DeleteByTime handles DELETE /api/stores/:store/objects/time/:timestamp
// Deletes an object by its timestamp.
func (h *ObjectHandler) DeleteByTime(c *gin.Context) {
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

	if err := st.DeleteObjectByTime(timestamp); err != nil {
		if err == store.ErrTimestampNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "object deleted"})
}

// DeleteByBlock handles DELETE /api/stores/:store/objects/block/:blocknum
// Deletes an object by its primary block number.
func (h *ObjectHandler) DeleteByBlock(c *gin.Context) {
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

	// Create handle for deletion
	handle := &store.ObjectHandle{
		PrimaryBlockNum: blockNum,
	}

	if err := st.DeleteObject(handle); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "object deleted"})
}

// ListResponse represents a list of object handles.
type ListResponse struct {
	Objects []*ObjectHandleResponse `json:"objects"`
	Count   int                     `json:"count"`
}

// ListOldest handles GET /api/stores/:store/objects/oldest
// Returns the N oldest objects (default 10).
func (h *ObjectHandler) ListOldest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limitStr := c.DefaultQuery("limit", "10")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 10
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	handles, err := st.GetOldestObjects(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	objects := make([]*ObjectHandleResponse, len(handles))
	for i, h := range handles {
		objects[i] = &ObjectHandleResponse{
			Timestamp:       h.Timestamp,
			PrimaryBlockNum: h.PrimaryBlockNum,
			TotalSize:       h.TotalSize,
			BlockCount:      h.BlockCount,
		}
	}

	c.JSON(http.StatusOK, ListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListNewest handles GET /api/stores/:store/objects/newest
// Returns the N newest objects (default 10).
func (h *ObjectHandler) ListNewest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limitStr := c.DefaultQuery("limit", "10")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 10
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	handles, err := st.GetNewestObjects(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	objects := make([]*ObjectHandleResponse, len(handles))
	for i, h := range handles {
		objects[i] = &ObjectHandleResponse{
			Timestamp:       h.Timestamp,
			PrimaryBlockNum: h.PrimaryBlockNum,
			TotalSize:       h.TotalSize,
			BlockCount:      h.BlockCount,
		}
	}

	c.JSON(http.StatusOK, ListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListRange handles GET /api/stores/:store/objects/range
// Returns objects in a time range.
func (h *ObjectHandler) ListRange(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	if startTimeStr == "" || endTimeStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "start_time and end_time required"})
		return
	}

	startTime, err := strconv.ParseInt(startTimeStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start_time"})
		return
	}

	endTime, err := strconv.ParseInt(endTimeStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end_time"})
		return
	}

	limitStr := c.DefaultQuery("limit", "100")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 100
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	handles, err := st.GetObjectsInRange(startTime, endTime, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	objects := make([]*ObjectHandleResponse, len(handles))
	for i, h := range handles {
		objects[i] = &ObjectHandleResponse{
			Timestamp:       h.Timestamp,
			PrimaryBlockNum: h.PrimaryBlockNum,
			TotalSize:       h.TotalSize,
			BlockCount:      h.BlockCount,
		}
	}

	c.JSON(http.StatusOK, ListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}
