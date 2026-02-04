// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package handlers

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

const maxRawRecordsForAgg = 100000

// UnifiedHandler handles the unified /data endpoint.
// Content-Type header determines encoding:
//   - application/octet-stream: binary data
//   - text/plain: UTF-8 text
//   - application/json: JSON data (or schema-encoded JSON)
type UnifiedHandler struct {
	storeService *service.StoreService
}

// NewUnifiedHandler creates a new unified data handler.
func NewUnifiedHandler(storeService *service.StoreService) *UnifiedHandler {
	return &UnifiedHandler{
		storeService: storeService,
	}
}

// ObjectHandleResponse represents the response after storing an object.
type ObjectHandleResponse struct {
	Timestamp int64  `json:"timestamp"`
	BlockNum  uint32 `json:"block_num"`
	Size      uint32 `json:"size"`
}

// DataResponse represents a single data object in responses.
type DataResponse struct {
	Timestamp int64  `json:"timestamp"`
	BlockNum  uint32 `json:"block_num"`
	Size      uint32 `json:"size"`
	Data      any    `json:"data"` // string (base64 or text) or json.RawMessage
}

// DataListResponse represents a list of data objects.
type DataListResponse struct {
	Objects []DataResponse `json:"objects"`
	Count   int            `json:"count"`
}

// Put handles POST /api/stores/:store/data
// Content-Type determines format:
//   - application/octet-stream: raw binary body
//   - text/plain: raw text body
//   - application/json: JSON body with optional timestamp wrapper
func (h *UnifiedHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	contentType := c.ContentType()
	storeDataType := st.DataType()

	// Validate content type matches store data type
	if err := validateContentType(contentType, storeDataType); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var data []byte
	var timestamp int64

	switch {
	case strings.HasPrefix(contentType, "application/octet-stream"):
		// Binary: read raw body
		data, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		timestamp = time.Now().UnixNano()

	case strings.HasPrefix(contentType, "text/plain"):
		// Text: read raw body as text
		data, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		timestamp = time.Now().UnixNano()

	case strings.HasPrefix(contentType, "application/json"):
		// JSON: parse wrapper with optional timestamp
		var req struct {
			Timestamp int64           `json:"timestamp,omitempty"`
			Data      json.RawMessage `json:"data"`
		}
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
		data = req.Data
		timestamp = req.Timestamp
		if timestamp == 0 {
			timestamp = time.Now().UnixNano()
		}

		// For schema stores, validate and compact the data
		if storeDataType == store.DataTypeSchema {
			compactData, err := st.ValidateAndCompact(data)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "schema validation failed: " + err.Error()})
				return
			}
			data = compactData
		}

	default:
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error": "unsupported content type, use application/octet-stream, text/plain, or application/json",
		})
		return
	}

	// Store the object
	handle, err := st.PutObject(timestamp, data)
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

// GetByTime handles GET /api/stores/:store/data/time/:timestamp
func (h *UnifiedHandler) GetByTime(c *gin.Context) {
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

	// Check if client wants expanded format (default for schema stores)
	expand := c.Query("format") != "compact"

	c.JSON(http.StatusOK, h.formatDataResponse(data, handle, st.DataType(), st, expand))
}

// DeleteByTime handles DELETE /api/stores/:store/data/time/:timestamp
func (h *UnifiedHandler) DeleteByTime(c *gin.Context) {
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

// ListOldest handles GET /api/stores/:store/data/oldest
func (h *UnifiedHandler) ListOldest(c *gin.Context) {
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

	// Get filter parameters
	filter := c.Query("filter")
	filterIgnoreCase := c.Query("filter_ignore_case") == "true"

	// When filtering, we need to fetch more than limit since some may be filtered out
	fetchLimit := limit
	if filter != "" {
		fetchLimit = 0 // Fetch all, filter in loop
	}

	handles, err := st.GetOldestObjects(fetchLimit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// For list operations, include data by default (set include_data=false to exclude)
	includeData := c.Query("include_data") != "false"
	expand := c.Query("format") != "compact"

	objects := make([]DataResponse, 0, limit)
	for _, handle := range handles {
		if len(objects) >= limit {
			break
		}

		data, err := st.GetObject(handle)
		if err != nil {
			continue
		}

		// Apply filter
		if !store.MatchesFilter(data, filter, filterIgnoreCase) {
			continue
		}

		obj := DataResponse{
			Timestamp: handle.Timestamp,
			BlockNum:  handle.BlockNum,
			Size:      handle.Size,
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, expand)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListNewest handles GET /api/stores/:store/data/newest
// Supports optional ?since=<duration> parameter (e.g., since=2h, since=30m, since=7d)
// Supports aggregation with ?agg_window=<duration> (e.g., agg_window=1m)
func (h *UnifiedHandler) ListNewest(c *gin.Context) {
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

	// Get filter parameters
	filter := c.Query("filter")
	filterIgnoreCase := c.Query("filter_ignore_case") == "true"

	// Check for aggregation
	aggWindowStr := c.Query("agg_window")
	hasAgg := aggWindowStr != ""

	// When filtering or aggregating, fetch all records
	fetchLimit := limit
	if filter != "" || hasAgg {
		fetchLimit = 0
	}

	var handles []*store.ObjectHandle

	// Check for since parameter
	sinceStr := c.Query("since")
	if sinceStr != "" {
		dur, err := ParseDuration(sinceStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since duration: " + err.Error()})
			return
		}
		handles, err = st.GetObjectsSince(dur, fetchLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		handles, err = st.GetNewestObjects(fetchLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	// Aggregation path
	if hasAgg {
		h.aggregateAndRespond(c, st, handles, filter, filterIgnoreCase, aggWindowStr, limit)
		return
	}

	includeData := c.Query("include_data") != "false"
	expand := c.Query("format") != "compact"

	objects := make([]DataResponse, 0, limit)
	for _, handle := range handles {
		if len(objects) >= limit {
			break
		}

		data, err := st.GetObject(handle)
		if err != nil {
			continue
		}

		// Apply filter
		if !store.MatchesFilter(data, filter, filterIgnoreCase) {
			continue
		}

		obj := DataResponse{
			Timestamp: handle.Timestamp,
			BlockNum:  handle.BlockNum,
			Size:      handle.Size,
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, expand)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// ListRange handles GET /api/stores/:store/data/range
// Supports ?start_time=X&end_time=Y, ?since=<duration>, or ?after=<timestamp>
// Supports aggregation with ?agg_window=<duration> (e.g., agg_window=1m)
func (h *UnifiedHandler) ListRange(c *gin.Context) {
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

	// Get filter parameters
	filter := c.Query("filter")
	filterIgnoreCase := c.Query("filter_ignore_case") == "true"

	// Check for aggregation
	aggWindowStr := c.Query("agg_window")
	hasAgg := aggWindowStr != ""

	// When filtering or aggregating, fetch all records in range
	fetchLimit := limit
	if filter != "" || hasAgg {
		fetchLimit = 0
	}

	var handles []*store.ObjectHandle

	// Check for since parameter first (relative duration)
	sinceStr := c.Query("since")
	afterStr := c.Query("after")

	if sinceStr != "" {
		dur, err := ParseDuration(sinceStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since duration: " + err.Error()})
			return
		}
		handles, err = st.GetObjectsSince(dur, fetchLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else if afterStr != "" {
		// Cursor-based: get all records after the given timestamp
		after, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid after timestamp"})
			return
		}
		// Use after+1 as start_time to exclude the cursor itself, 0 for unbounded end
		handles, err = st.GetObjectsInRange(after+1, 0, fetchLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		// Use start_time/end_time (both now optional, 0 means unbounded)
		startTimeStr := c.Query("start_time")
		endTimeStr := c.Query("end_time")

		var startTime, endTime int64

		if startTimeStr != "" {
			startTime, err = strconv.ParseInt(startTimeStr, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start_time"})
				return
			}
		}

		if endTimeStr != "" {
			endTime, err = strconv.ParseInt(endTimeStr, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end_time"})
				return
			}
		}

		// At least one parameter required
		if startTimeStr == "" && endTimeStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "start_time, end_time, after, or since parameter required"})
			return
		}

		handles, err = st.GetObjectsInRange(startTime, endTime, fetchLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	// Aggregation path
	if hasAgg {
		h.aggregateAndRespond(c, st, handles, filter, filterIgnoreCase, aggWindowStr, limit)
		return
	}

	includeData := c.Query("include_data") == "true"
	expand := c.Query("format") != "compact"

	objects := make([]DataResponse, 0, limit)
	for _, handle := range handles {
		if len(objects) >= limit {
			break
		}

		data, err := st.GetObject(handle)
		if err != nil {
			continue
		}

		// Apply filter
		if !store.MatchesFilter(data, filter, filterIgnoreCase) {
			continue
		}

		obj := DataResponse{
			Timestamp: handle.Timestamp,
			BlockNum:  handle.BlockNum,
			Size:      handle.Size,
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, expand)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// formatDataResponse formats a single data response based on store type.
func (h *UnifiedHandler) formatDataResponse(data []byte, handle *store.ObjectHandle, dataType store.DataType, st *store.Store, expand bool) DataResponse {
	return DataResponse{
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
		Data:      h.formatData(data, dataType, st, expand),
	}
}

// formatData formats data based on store type.
// For schema stores, if expand is true, converts compact format to full field names.
func (h *UnifiedHandler) formatData(data []byte, dataType store.DataType, st *store.Store, expand bool) any {
	switch dataType {
	case store.DataTypeBinary:
		return base64.StdEncoding.EncodeToString(data)
	case store.DataTypeText:
		return string(data)
	case store.DataTypeJSON:
		return json.RawMessage(data)
	case store.DataTypeSchema:
		if expand {
			// Expand compact format to full field names
			expanded, err := st.ExpandData(data, 0) // 0 = current version
			if err == nil {
				return json.RawMessage(expanded)
			}
		}
		// Return compact format if not expanding or expansion failed
		return json.RawMessage(data)
	default:
		return base64.StdEncoding.EncodeToString(data)
	}
}

// aggregateAndRespond reads raw records, applies filtering, runs batch aggregation,
// and writes the aggregated response. Only valid for JSON and schema stores.
func (h *UnifiedHandler) aggregateAndRespond(c *gin.Context, st *store.Store, handles []*store.ObjectHandle, filter string, filterIgnoreCase bool, aggWindowStr string, limit int) {
	dataType := st.DataType()
	if dataType != store.DataTypeJSON && dataType != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "aggregation is only supported for json and schema stores"})
		return
	}

	// Parse aggregation config
	aggWindow, err := duration.ParseDuration(aggWindowStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agg_window: " + err.Error()})
		return
	}

	aggFieldsStr := c.Query("agg_fields")
	aggFields, err := aggregation.ParseFieldAggs(aggFieldsStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agg_fields: " + err.Error()})
		return
	}

	aggDefault := aggregation.AggFunc(c.Query("agg_default"))

	numericMap := aggregation.BuildNumericMap(st.GetSchemaSet())

	aggConfig, err := aggregation.NewConfig(aggWindow, aggFields, aggDefault, numericMap)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Safety cap on raw records
	if len(handles) > maxRawRecordsForAgg {
		handles = handles[:maxRawRecordsForAgg]
	}

	// Build timestamped records, applying filter and expanding data
	records := make([]aggregation.TimestampedRecord, 0, len(handles))
	for _, handle := range handles {
		rawData, err := st.GetObject(handle)
		if err != nil {
			continue
		}

		if !store.MatchesFilter(rawData, filter, filterIgnoreCase) {
			continue
		}

		// Expand to full field names for schema stores
		var jsonData []byte
		if dataType == store.DataTypeSchema {
			expanded, err := st.ExpandData(rawData, 0)
			if err != nil {
				continue
			}
			jsonData = expanded
		} else {
			jsonData = rawData
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(jsonData, &parsed); err != nil {
			continue
		}

		records = append(records, aggregation.TimestampedRecord{
			Timestamp: handle.Timestamp,
			Data:      parsed,
		})
	}

	// Run batch aggregation
	results := aggregation.AggregateBatch(records, aggConfig)

	// Apply user limit to aggregated windows
	if len(results) > limit {
		results = results[:limit]
	}

	// Build response — check for compact format
	compact := c.Query("format") == "compact"

	objects := make([]DataResponse, 0, len(results))

	if compact && dataType == store.DataTypeSchema {
		// Build schema mapping for compact response
		schemaMap := make(map[string]string)
		ss := st.GetSchemaSet()
		if ss != nil && ss.CurrentVersion > 0 {
			if s, err := ss.GetCurrentSchema(); err == nil {
				for _, f := range s.Fields {
					schemaMap[strconv.Itoa(f.Index)] = f.Name
				}
			}
		}
		// First object is the schema header
		schemaBytes, _ := json.Marshal(schemaMap)
		objects = append(objects, DataResponse{
			Data: json.RawMessage(`{"_schema":` + string(schemaBytes) + `}`),
		})

		// Subsequent objects use compact indices (reversed)
		nameToIndex := make(map[string]string)
		for idx, name := range schemaMap {
			nameToIndex[name] = idx
		}
		for _, res := range results {
			compactData := make(map[string]interface{})
			for field, val := range res.Data {
				if idx, ok := nameToIndex[field]; ok {
					compactData[idx] = val
				} else {
					compactData[field] = val
				}
			}
			dataBytes, _ := json.Marshal(compactData)
			objects = append(objects, DataResponse{
				Timestamp: res.Timestamp,
				Data:      json.RawMessage(dataBytes),
			})
		}
	} else {
		// Expanded format (default)
		for _, res := range results {
			dataBytes, _ := json.Marshal(res.Data)
			objects = append(objects, DataResponse{
				Timestamp: res.Timestamp,
				Data:      json.RawMessage(dataBytes),
			})
		}
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects: objects,
		Count:   len(objects),
	})
}

// validateContentType checks if content type is compatible with store data type.
func validateContentType(contentType string, dataType store.DataType) error {
	switch dataType {
	case store.DataTypeBinary:
		if !strings.HasPrefix(contentType, "application/octet-stream") {
			return store.ErrDataTypeMismatch
		}
	case store.DataTypeText:
		if !strings.HasPrefix(contentType, "text/plain") {
			return store.ErrDataTypeMismatch
		}
	case store.DataTypeJSON, store.DataTypeSchema:
		if !strings.HasPrefix(contentType, "application/json") {
			return store.ErrDataTypeMismatch
		}
	}
	return nil
}
