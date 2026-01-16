// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// JSONHandler handles JSON object operations.
type JSONHandler struct {
	storeService *service.StoreService
}

// NewJSONHandler creates a new JSON handler.
func NewJSONHandler(storeService *service.StoreService) *JSONHandler {
	return &JSONHandler{
		storeService: storeService,
	}
}

// PutJSONRequest represents a request to store a JSON object.
type PutJSONRequest struct {
	Timestamp int64           `json:"timestamp,omitempty"` // Unix nanoseconds, optional
	Data      json.RawMessage `json:"data"`                // JSON data (not base64)
}

// JSONResponse represents a JSON object response.
type JSONResponse struct {
	Timestamp int64           `json:"timestamp"`
	BlockNum  uint32          `json:"block_num"`
	Size      uint32          `json:"size"`
	Data      json.RawMessage `json:"data"`
}

// Put handles POST /api/stores/:store/json
// Stores a JSON object directly (no base64 encoding needed).
func (h *JSONHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	var req PutJSONRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "data is required"})
		return
	}

	// Validate JSON
	var js json.RawMessage
	if err := json.Unmarshal(req.Data, &js); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON data"})
		return
	}

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

	// Store the JSON data directly (it's already []byte from RawMessage)
	handle, err := st.PutObject(timestamp, req.Data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, ObjectHandleResponse{
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
	})
}

// GetByTime handles GET /api/stores/:store/json/time/:timestamp
func (h *JSONHandler) GetByTime(c *gin.Context) {
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

	raw, handle, err := st.GetJSONRawByTime(timestamp)
	if err != nil {
		if err == store.ErrTimestampNotFound || err == store.ErrEmptyStore {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else if err == store.ErrInvalidJSON {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "stored data is not valid JSON"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, JSONResponse{
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
		Data:      raw,
	})
}

// JSONListResponse represents a list of JSON objects.
type JSONListResponse struct {
	Objects []JSONResponse `json:"objects"`
	Count   int            `json:"count"`
}

// ListOldest handles GET /api/stores/:store/json/oldest
func (h *JSONHandler) ListOldest(c *gin.Context) {
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

	rawMsgs, handles, err := st.GetOldestJSON(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	objects := make([]JSONResponse, len(rawMsgs))
	for i, raw := range rawMsgs {
		objects[i] = JSONResponse{
			Timestamp: handles[i].Timestamp,
			BlockNum:  handles[i].BlockNum,
			Size:      handles[i].Size,
			Data:      raw,
		}
	}

	c.JSON(http.StatusOK, JSONListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListNewest handles GET /api/stores/:store/json/newest
// Supports optional ?since=<duration> parameter (e.g., since=2h, since=30m, since=7d)
func (h *JSONHandler) ListNewest(c *gin.Context) {
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

	var rawMsgs []json.RawMessage
	var handles []*store.ObjectHandle

	// Check for since parameter
	sinceStr := c.Query("since")
	if sinceStr != "" {
		duration, err := ParseDuration(sinceStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since duration: " + err.Error()})
			return
		}
		rawMsgs, handles, err = st.GetJSONSince(duration, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		rawMsgs, handles, err = st.GetNewestJSON(limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	objects := make([]JSONResponse, len(rawMsgs))
	for i, raw := range rawMsgs {
		objects[i] = JSONResponse{
			Timestamp: handles[i].Timestamp,
			BlockNum:  handles[i].BlockNum,
			Size:      handles[i].Size,
			Data:      raw,
		}
	}

	c.JSON(http.StatusOK, JSONListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListRange handles GET /api/stores/:store/json/range
// Supports ?start_time=X&end_time=Y or ?since=<duration>
func (h *JSONHandler) ListRange(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limitStr := c.DefaultQuery("limit", "100")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 100
	}

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var rawMsgs []json.RawMessage
	var handles []*store.ObjectHandle

	// Check for since parameter first
	sinceStr := c.Query("since")
	if sinceStr != "" {
		duration, err := ParseDuration(sinceStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since duration: " + err.Error()})
			return
		}
		rawMsgs, handles, err = st.GetJSONSince(duration, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		// Use start_time/end_time
		startTimeStr := c.Query("start_time")
		endTimeStr := c.Query("end_time")

		if startTimeStr == "" || endTimeStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "start_time and end_time required (or use since parameter)"})
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

		rawMsgs, handles, err = st.GetJSONInRange(startTime, endTime, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	objects := make([]JSONResponse, len(rawMsgs))
	for i, raw := range rawMsgs {
		objects[i] = JSONResponse{
			Timestamp: handles[i].Timestamp,
			BlockNum:  handles[i].BlockNum,
			Size:      handles[i].Size,
			Data:      raw,
		}
	}

	c.JSON(http.StatusOK, JSONListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}
