// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
	Timestamp     int64  `json:"timestamp"`
	BlockNum      uint32 `json:"block_num"`
	Size          uint32 `json:"size"`
	SchemaVersion uint32 `json:"schema_version,omitempty"` // Schema version the record was written under (schema stores only)
	Data          any    `json:"data"`                     // string (base64 or text) or json.RawMessage
}

// RollupInfo describes the rollup nature of a store, echoed on data responses
// for rollup target stores so a consumer learns the window (needed to interpret
// the half-open window-end timestamps and to do count-weighted re-aggregation)
// in the same call as the data.
type RollupInfo struct {
	Role     string `json:"role"`      // always "rollup" here
	Window   string `json:"window"`    // canonical window, e.g. "1h"
	RollupOf string `json:"rollup_of"` // source store name
}

// ScanInfo describes how a filtered /newest scan was bounded, so a caller can
// tell a short result set ("only 50 of your limit=100") apart from genuine
// exhaustion, and learn it was a windowed (not full-store) scan. It is present
// only on filtered /newest responses where a window was applied; the objects
// array is untouched by its presence.
type ScanInfo struct {
	Window        string `json:"window"`         // effective lookback window, e.g. "1h" / "48h"
	WindowApplied bool   `json:"window_applied"` // true when the aggressive/explicit window bounded the scan
	LimitReached  bool   `json:"limit_reached"`  // true when the scan stopped at limit with records still unexamined
}

// DataListResponse represents a list of data objects.
type DataListResponse struct {
	Objects  []DataResponse `json:"objects"`
	Count    int            `json:"count"`
	DataType string         `json:"data_type"`        // store data type ("schema", "json", "text", "binary") so consumers needn't a second store-info call
	Rollup   *RollupInfo    `json:"rollup,omitempty"` // present only for rollup target stores
	Scan     *ScanInfo      `json:"scan,omitempty"`   // present only for filtered /newest scans bounded by a window
}

// rollupInfoFor returns a RollupInfo for a store if it is a rollup target, else
// nil (so the field is omitted for non-rollup stores).
func rollupInfoFor(st *store.Store) *RollupInfo {
	meta, err := st.ReadRollupMeta()
	if err != nil || meta == nil {
		return nil
	}
	return &RollupInfo{Role: "rollup", Window: meta.Window, RollupOf: meta.RollupOf}
}

// Put handles POST /api/stores/:store/data
// Content-Type determines format:
//   - application/octet-stream: raw binary body
//   - text/plain: raw text body
//   - application/json: JSON body with optional timestamp wrapper
// maxQueryLimit caps the limit query param. The response slice is
// pre-allocated at this capacity, so an unbounded value is an OOM lever on
// small devices (issue #30).
const maxQueryLimit = 10000

// parseLimit reads the limit query param, falling back to def and clamping
// to maxQueryLimit.
func parseLimit(c *gin.Context, def int) int {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(def)))
	if err != nil || limit <= 0 {
		return def
	}
	if limit > maxQueryLimit {
		return maxQueryLimit
	}
	return limit
}

func (h *UnifiedHandler) Put(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
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
		respondStoreError(c, err)
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

	// Determine the read-time expansion view (compact/wide/record/version).
	spec := parseExpandSpec(c)

	c.JSON(http.StatusOK, h.formatDataResponse(data, handle, st.DataType(), st, spec))
}

// ListOldest handles GET /api/stores/:store/data/oldest
func (h *UnifiedHandler) ListOldest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limit := parseLimit(c, 10)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
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
	spec := parseExpandSpec(c)

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
		if st.DataType() == store.DataTypeSchema {
			obj.SchemaVersion = recordSchemaVersion(handle)
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, handle, spec)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects:  objects,
		Count:    len(objects),
		DataType: st.DataType().String(),
		Rollup:   rollupInfoFor(st),
	})
}

// Aggressive default lookback windows for a filtered /newest scan with no
// explicit time bound. A filtered scan reads the whole store otherwise; these
// keep "fetch a few recent matches" cheap. The aggregation default is wider so
// minute/hourly/daily rollups aren't silently truncated.
const (
	defaultFilterWindow    = time.Hour      // plain filtered fetch
	defaultFilterWindowAgg = 48 * time.Hour // filtered aggregation
)

// resolveFilterWindow determines the lookback window for a filtered /newest
// query that has no explicit time bound. It reads the optional `window` param:
// absent → the aggressive default (1h plain, 48h aggregation); `window=0` →
// unbounded (full-store scan); otherwise the given duration. Returns the window
// and whether a window should be applied (false only for window=0).
func resolveFilterWindow(c *gin.Context, hasAgg bool) (time.Duration, bool, error) {
	windowStr := c.Query("window")
	if windowStr == "" {
		if hasAgg {
			return defaultFilterWindowAgg, true, nil
		}
		return defaultFilterWindow, true, nil
	}
	dur, err := ParseDuration(windowStr)
	if err != nil {
		return 0, false, err
	}
	if dur <= 0 {
		// window=0: explicit unbounded full scan.
		return 0, false, nil
	}
	return dur, true, nil
}

// ListNewest handles GET /api/stores/:store/data/newest
// Supports optional ?since=<duration> parameter (e.g., since=2h, since=30m, since=7d)
// Supports aggregation with ?agg_window=<duration> (e.g., agg_window=1m)
// When ?filter= is set with no explicit time bound, an aggressive default
// lookback window is applied (1h plain, 48h aggregation) so the filtered scan
// does not read the whole store; override with ?window=<dur> or window=0 for
// an unbounded scan. The window param is ignored without a filter.
func (h *UnifiedHandler) ListNewest(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limit := parseLimit(c, 10)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}

	// Get filter parameters
	filter := c.Query("filter")
	filterIgnoreCase := c.Query("filter_ignore_case") == "true"

	// Check for aggregation (agg_window, or the step shorthand)
	aggWindowStr, hasAgg, fromStep, err := resolveAggWindow(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// When filtering or aggregating, fetch all records
	fetchLimit := limit
	if filter != "" || hasAgg {
		fetchLimit = 0
	}

	var handles []*store.ObjectHandle

	// A filtered scan is applied post-fetch, so without a time bound it would read
	// the whole store to return a handful of matches. When the caller gives no
	// explicit time bound, apply an aggressive default lookback window: 1h for a
	// plain fetch, 48h for aggregation (so minute/hourly/daily rollups aren't
	// silently truncated). The window param overrides the default; window=0 means
	// unbounded (the old full-store scan). The window only matters when filtering.
	var scan *ScanInfo

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
	} else if filter != "" {
		// Filtered, no explicit time bound: resolve the effective window.
		effectiveWindow, windowApplied, err := resolveFilterWindow(c, hasAgg)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window duration: " + err.Error()})
			return
		}
		if windowApplied {
			handles, err = st.GetObjectsSince(effectiveWindow, 0)
			scan = &ScanInfo{Window: effectiveWindow.String(), WindowApplied: true}
		} else {
			// window=0: explicit unbounded full scan.
			handles, err = st.GetNewestObjects(0)
		}
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
		h.aggregateAndRespond(c, st, handles, filter, filterIgnoreCase, aggWindowStr, fromStep, limit, scan)
		return
	}

	includeData := c.Query("include_data") != "false"
	spec := parseExpandSpec(c)

	objects := make([]DataResponse, 0, limit)
	for _, handle := range handles {
		if len(objects) >= limit {
			// Stopped at the limit with records still unexamined: report it so a
			// caller can tell this apart from genuine exhaustion of the window.
			if scan != nil {
				scan.LimitReached = true
			}
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
		if st.DataType() == store.DataTypeSchema {
			obj.SchemaVersion = recordSchemaVersion(handle)
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, handle, spec)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects:  objects,
		Count:    len(objects),
		DataType: st.DataType().String(),
		Rollup:   rollupInfoFor(st),
		Scan:     scan,
	})
}

// ListRange handles GET /api/stores/:store/data/range
// Supports ?start_time=X&end_time=Y, ?since=<duration>, or ?after=<timestamp>
// Supports aggregation with ?agg_window=<duration> (e.g., agg_window=1m)
func (h *UnifiedHandler) ListRange(c *gin.Context) {
	storeName := middleware.GetStoreName(c)

	limit := parseLimit(c, 100)

	st, err := h.storeService.GetOrOpen(storeName)
	if err != nil {
		respondStoreError(c, err)
		return
	}

	// Get filter parameters
	filter := c.Query("filter")
	filterIgnoreCase := c.Query("filter_ignore_case") == "true"

	// Check for aggregation (agg_window, or the step shorthand)
	aggWindowStr, hasAgg, fromStep, err := resolveAggWindow(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

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

	// Aggregation path. /range takes explicit time bounds, so no scan window
	// signal applies here.
	if hasAgg {
		h.aggregateAndRespond(c, st, handles, filter, filterIgnoreCase, aggWindowStr, fromStep, limit, nil)
		return
	}

	// Data included by default (include_data=false to exclude) — same
	// default as /newest and /oldest and as documented in swagger. /range
	// previously defaulted to false, stranding spec-following clients
	// with data-less records (issue #40).
	includeData := c.Query("include_data") != "false"
	spec := parseExpandSpec(c)

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
		if st.DataType() == store.DataTypeSchema {
			obj.SchemaVersion = recordSchemaVersion(handle)
		}
		if includeData {
			obj.Data = h.formatData(data, st.DataType(), st, handle, spec)
		}
		objects = append(objects, obj)
	}

	c.JSON(http.StatusOK, DataListResponse{
		Objects:  objects,
		Count:    len(objects),
		DataType: st.DataType().String(),
		Rollup:   rollupInfoFor(st),
	})
}

// schemaView selects how a schema-store record is expanded on read.
type schemaView int

const (
	viewCompact schemaView = iota // raw compact {"1":..,"2":..} (format=compact)
	viewWide                      // wide union, null-filled (default)
	viewRecord                    // expand against the record's own write version
	viewVersion                   // expand against a fixed version (forceVersion)
)

// expandSpec captures the read-time expansion choice for a request, derived from
// the ?format= and ?schema_version= query parameters.
type expandSpec struct {
	view         schemaView
	forceVersion int // used only when view == viewVersion
}

// parseExpandSpec reads ?format= and ?schema_version= and returns the expansion spec.
//
//	?format=compact            -> raw compact bytes
//	?schema_version=wide|""    -> wide null-filled union (default)
//	?schema_version=record     -> each record's own write version (absent-not-null)
//	?schema_version=<N>         -> force version N for every record
func parseExpandSpec(c *gin.Context) expandSpec {
	if c.Query("format") == "compact" {
		return expandSpec{view: viewCompact}
	}
	switch sv := c.Query("schema_version"); sv {
	case "", "wide":
		return expandSpec{view: viewWide}
	case "record":
		return expandSpec{view: viewRecord}
	default:
		if n, err := strconv.Atoi(sv); err == nil && n > 0 {
			return expandSpec{view: viewVersion, forceVersion: n}
		}
		// Unrecognized value falls back to the wide default.
		return expandSpec{view: viewWide}
	}
}

// formatDataResponse formats a single data response based on store type.
func (h *UnifiedHandler) formatDataResponse(data []byte, handle *store.ObjectHandle, dataType store.DataType, st *store.Store, spec expandSpec) DataResponse {
	resp := DataResponse{
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
		Data:      h.formatData(data, dataType, st, handle, spec),
	}
	if dataType == store.DataTypeSchema {
		resp.SchemaVersion = recordSchemaVersion(handle)
	}
	return resp
}

// recordSchemaVersion returns the schema version a record was written under,
// mapping untagged records (0) to version 1 (they predate per-record tagging).
func recordSchemaVersion(handle *store.ObjectHandle) uint32 {
	if handle.SchemaVersion == 0 {
		return 1
	}
	return handle.SchemaVersion
}

// formatData formats data based on store type. For schema stores the expansion
// view is chosen by spec (compact/wide/record/version).
func (h *UnifiedHandler) formatData(data []byte, dataType store.DataType, st *store.Store, handle *store.ObjectHandle, spec expandSpec) any {
	switch dataType {
	case store.DataTypeBinary:
		return base64.StdEncoding.EncodeToString(data)
	case store.DataTypeText:
		return string(data)
	case store.DataTypeJSON:
		return json.RawMessage(data)
	case store.DataTypeSchema:
		var (
			expanded []byte
			err      error
		)
		switch spec.view {
		case viewCompact:
			return json.RawMessage(data)
		case viewRecord:
			expanded, err = st.ExpandData(data, int(recordSchemaVersion(handle)))
		case viewVersion:
			expanded, err = st.ExpandData(data, spec.forceVersion)
		default: // viewWide
			expanded, err = st.ExpandDataWide(data, int(recordSchemaVersion(handle)))
		}
		if err == nil {
			return json.RawMessage(expanded)
		}
		// Fall back to compact format if expansion failed.
		return json.RawMessage(data)
	default:
		return base64.StdEncoding.EncodeToString(data)
	}
}

// resolveAggWindow determines the aggregation window for a request and how it
// was requested. A caller may set the window either via agg_window (the raw
// downsampling knob, defaulting per-field to "last") or via step (a
// Prometheus-flavored shorthand that additionally implies agg_default=avg).
// Setting both is rejected — they'd fight over the window. Returns the window
// string, whether aggregation is active, and whether step was the source.
func resolveAggWindow(c *gin.Context) (windowStr string, hasAgg bool, fromStep bool, err error) {
	aggWindowStr := c.Query("agg_window")
	stepStr := c.Query("step")
	if aggWindowStr != "" && stepStr != "" {
		return "", false, false, errors.New("set either step or agg_window, not both")
	}
	if stepStr != "" {
		return stepStr, true, true, nil
	}
	return aggWindowStr, aggWindowStr != "", false, nil
}

// aggregateAndRespond reads raw records, applies filtering, runs batch aggregation,
// and writes the aggregated response. Only valid for JSON and schema stores.
//
// fromStep marks that the window came from the step shorthand rather than
// agg_window: when the caller supplied no explicit agg_fields/agg_default,
// step implies averaging numeric fields (Prometheus-style downsampling) rather
// than agg_window's plain "last" default.
func (h *UnifiedHandler) aggregateAndRespond(c *gin.Context, st *store.Store, handles []*store.ObjectHandle, filter string, filterIgnoreCase bool, aggWindowStr string, fromStep bool, limit int, scan *ScanInfo) {
	dataType := st.DataType()
	if dataType != store.DataTypeJSON && dataType != store.DataTypeSchema {
		c.JSON(http.StatusBadRequest, gin.H{"error": "aggregation is only supported for json and schema stores"})
		return
	}

	// Parse aggregation config
	windowLabel := "agg_window"
	if fromStep {
		windowLabel = "step"
	}
	aggWindow, err := duration.ParseDuration(aggWindowStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + windowLabel + ": " + err.Error()})
		return
	}

	aggFieldsStr := c.Query("agg_fields")
	aggFields, err := aggregation.ParseFieldAggs(aggFieldsStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agg_fields: " + err.Error()})
		return
	}

	aggDefault := aggregation.AggFunc(c.Query("agg_default"))
	// step means "downsample to this resolution", which implies averaging
	// numeric fields — unlike a bare agg_window, whose per-field default is
	// "last". Only fill this in when the caller gave no explicit spec, so
	// step=1h&agg_default=max or step=1h&agg_fields=cpu:max still win.
	if fromStep && aggFieldsStr == "" && aggDefault == "" {
		aggDefault = aggregation.AggAvg
	}

	numericMap := aggregation.BuildNumericMap(st.GetSchemaSet())

	aggConfig, err := aggregation.NewConfig(aggWindow, aggFields, aggDefault, numericMap)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Safety cap on raw records. If the cap truncates the input, the aggregation
	// did not see every record in the window — surface that via the scan signal.
	if len(handles) > maxRawRecordsForAgg {
		handles = handles[:maxRawRecordsForAgg]
		if scan != nil {
			scan.LimitReached = true
		}
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

	// Aggregated output: the response window is agg_window, not the store's
	// rollup window, so we don't echo the store's rollup descriptor here.
	c.JSON(http.StatusOK, DataListResponse{
		Objects:  objects,
		Count:    len(objects),
		DataType: st.DataType().String(),
		Scan:     scan,
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
