# Time-Windowed Aggregation for ts-store

## Overview

Add time-windowed aggregation (sum, avg, max, min) to all output paths: REST API, WebSocket push, and MQTT sink. Data collected at high frequency (e.g., every second) can be aggregated into windows (e.g., every minute) to reduce network traffic and noise.

## Phase 1: Shared Aggregation Engine

**Create `internal/aggregation/` package** — the core math engine used by all output paths.

### New Files
- `internal/aggregation/aggregation.go` — Types, config, numeric helpers
- `internal/aggregation/accumulator.go` — Streaming accumulator (Add/Flush)
- `internal/aggregation/batch.go` — Batch aggregation for REST (wraps accumulator)
- `internal/aggregation/aggregation_test.go` — Unit tests

### Key Types
```
Config {
    Window      time.Duration
    Fields      []FieldAgg       // e.g. [{Field: "cpu.pct", Function: "avg"}]
    Default     AggFunc          // default function for unspecified numeric fields
    NumericMap  map[string]bool  // pre-computed from schema (if available)
}

AggFunc: "sum", "avg", "max", "min", "count", "last"

AggResult {
    Timestamp int64                    // window end timestamp
    Count     int                      // records in this window
    Partial   bool                     // true if window was flushed before full
    Data      map[string]interface{}   // aggregated field values
}
```

### Accumulator (streaming — WS/MQTT)
- Created once per connection, lives for the connection duration
- `Add(timestamp, data) *AggResult` — feed a record; returns result when window closes
- `Flush() *AggResult` — force emit current window (partial). Sets `Partial=true` on result
- NumericMap pre-computed at creation from schema (if available), avoiding per-record type checks

### AggregateBatch (REST)
- Takes sorted `[]TimestampedRecord`, returns `[]AggResult` — one per window

### Numeric handling
- Schema stores: pre-compute `NumericMap` from schema field types at config creation time
- JSON stores: type-sniff from values (float64 → numeric, string/bool → non-numeric)
- Non-numeric fields: use `last` value, skip for sum/avg/max/min

### Partial window handling
- `avg`, `max`, `min`: valid on partial windows (output normally)
- `sum`: output `null` on partial windows (partial sum is misleading)
- `count`: valid on partial windows

## Phase 2: Move ParseDuration to shared package

**Move** `ParseDuration` from `internal/handlers/duration.go` → `internal/duration/duration.go`

Update `internal/handlers/duration.go` to import from new location. This lets the aggregation package parse window strings like "1m", "30s", "5m".

## Phase 3: REST API Integration

**Modify** `internal/handlers/unified_handler.go`

### New query parameters on `/data/range` (and `/data/newest`)
- `agg_window=1m` — aggregation window duration (required to activate aggregation)
- `agg_fields=cpu.pct:avg,memory.used:max` — per-field function (optional)
- `agg_default=avg` — default function for unlisted numeric fields (optional)

### Behavior when `agg_window` is present
1. Fetch all records in range (internal limit, not user limit; safety cap at 100k raw records)
2. Expand schema data, parse to `map[string]interface{}`
3. Call `AggregateBatch(records, config)`
4. Apply user `limit` to the aggregated windows
5. Return response

### Output format — same as normal records
Aggregated output uses the same structure as regular data responses. Each aggregated window looks like a normal record with the window-end timestamp and the aggregated data values. The user defined the window so they know the period.

```json
{
  "objects": [
    {
      "timestamp": 1706000060000000000,
      "data": {"cpu.pct": 45.2, "memory.used": 8192}
    },
    {
      "timestamp": 1706000120000000000,
      "data": {"cpu.pct": 38.7, "memory.used": 8201}
    }
  ],
  "count": 2
}
```

### Compact format with aggregation
- Default: expanded format (full field names) when aggregation is active
- If `format=compact` is requested: first record in response contains the schema mapping, subsequent records use compact indices

```json
{
  "objects": [
    {"_schema": {"1": "cpu.pct", "2": "memory.used"}},
    {"timestamp": 1706000060000000000, "data": {"1": 45.2, "2": 8192}},
    {"timestamp": 1706000120000000000, "data": {"1": 38.7, "2": 8201}}
  ],
  "count": 2
}
```

Only works for JSON and schema stores. Returns error for binary/text.

## Phase 4: WebSocket Push Integration

**Modify:**
- `pkg/store/ws_config.go` — Add `AggWindow`, `AggFields`, `AggDefault` to `WSConnection`
- `internal/ws/pusher.go` — Add `*aggregation.Accumulator` to `Pusher` struct
- `internal/ws/manager.go` — Pass agg config through `CreateConnectionRequest`
- `internal/handlers/ws_connections.go` — Accept agg params in create request

### Pusher changes
- Accumulator created once at connection start, persists for connection lifetime
- `sendNewData()`: feed records to accumulator instead of sending immediately; send when `Add()` returns a result
- `pushLoop()`: add flush ticker at window interval for time-based emission
- On stop: flush remaining accumulated data
- Filters applied before feeding to accumulator

### Output message format — same structure as non-aggregated
```json
{"type": "data", "timestamp": 1706000060000000000, "data": {"cpu.pct": 45.2, "memory.used": 8192}}
```

For compact format: first message after connect contains `{"type": "schema", "_schema": {"1": "cpu.pct", ...}}`, then subsequent messages use compact indices.

## Phase 5: MQTT Push Integration

**Modify:**
- `internal/mqtt/manager.go` — Add agg fields to `MQTTConnection` and `CreateConnectionRequest`
- `internal/mqtt/mqtt_pusher.go` — Same pattern as WS: add accumulator, modify `sendNewData()` and `pushLoop()`
- `internal/handlers/mqtt_handler.go` — Accept agg params in create request

Identical pattern to WebSocket. Cursor persistence still tracks last raw record timestamp (not aggregated window). Same compact format handling with schema header.

## Phase 6: Documentation & Swagger

**Modify:**
- `swagger.yaml` — Document new query parameters and response behavior
- `README.md` — Add aggregation examples for REST, WS, and MQTT

## Notes

- All changes are additive/backward-compatible. No aggregation params → existing behavior unchanged
- Existing persisted WS/MQTT connection configs load fine (new fields default to empty/zero)
- Safety limit on REST: cap raw records before aggregation (100,000) to prevent memory issues
- Filters applied before aggregation (only matching records are aggregated)
- `sum` on partial (flushed) windows outputs `null` — all other functions are valid on partial data

## Verification
1. Unit tests on aggregation engine (sum/avg/max/min across windows, partial flush, edge cases)
2. REST: `curl .../data/range?since=1h&agg_window=1m&agg_default=avg` against system-stats store
3. WS: create connection with `agg_window=1m` and verify aggregated messages arrive at window intervals
4. MQTT: same as WS via MQTT connection create
5. Run existing test suite to confirm no regressions
