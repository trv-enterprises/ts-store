# ts-store Data Retrieval Guide

[Back to main README](README.md)

This document is a comprehensive reference for every mechanism available to retrieve data from a ts-store datastore. It covers REST API queries, outbound WebSocket push connections, and MQTT sink publishing -- including protocol details, message formats, and configuration options for each.

## Table of Contents

- [1. REST API Query Endpoints](#1-rest-api-query-endpoints)
  - [Get Data by Timestamp](#get-data-by-timestamp)
  - [List Oldest Data](#list-oldest-data)
  - [List Newest Data](#list-newest-data)
  - [Query Time Range](#query-time-range)
  - [Get Store Stats](#get-store-stats)
  - [Get Schema](#get-schema)
- [2. Outbound WebSocket Push](#2-outbound-websocket-push)
  - [Creating a Push Connection](#creating-a-push-connection)
  - [Push Message Format](#push-message-format)
  - [Aggregation](#aggregation)
  - [Alerting](#alerting)
  - [Remote Control: Seek](#remote-control-seek)
  - [Managing Push Connections](#managing-push-connections)
- [3. MQTT Sink](#3-mqtt-sink)
  - [Creating an MQTT Connection](#creating-an-mqtt-connection)
  - [MQTT Message Format](#mqtt-message-format)
  - [Cursor Persistence](#cursor-persistence)
  - [Managing MQTT Connections](#managing-mqtt-connections)
- [Authentication](#authentication)
- [Common Parameters](#common-parameters)
- [Data Format Reference](#data-format-reference)

---

## 1. REST API Query Endpoints

Pull-based retrieval over HTTP. The client sends a request and receives data in the response. All query endpoints require store-level authentication (see [Authentication](#authentication)).

### Get Data by Timestamp

Retrieve a single record by its exact nanosecond timestamp.

```
GET /api/stores/:store/data/time/:timestamp
```

| Parameter   | In   | Type  | Description |
|-------------|------|-------|-------------|
| `store`     | path | string | Store name |
| `timestamp` | path | int64 | Nanosecond Unix timestamp |
| `format`    | query | string | `"full"` (default) or `"compact"` -- schema stores only |

**Response** (200):
```json
{
  "timestamp": 1704067200000000000,
  "block_num": 5,
  "size": 64,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```

**Errors:** 400 (invalid timestamp), 404 (not found), 500 (server error)

---

### List Oldest Data

Retrieve the N oldest records in the store.

```
GET /api/stores/:store/data/oldest
```

| Parameter            | In    | Type   | Default | Description |
|----------------------|-------|--------|---------|-------------|
| `limit`              | query | int    | 10      | Max records to return |
| `include_data`       | query | bool   | true    | Set `false` to return metadata only (timestamps, sizes) |
| `filter`             | query | string | --      | Substring match against serialized data |
| `filter_ignore_case` | query | bool   | false   | Case-insensitive substring matching |
| `format`             | query | string | `"full"` | `"full"` or `"compact"` -- schema stores only |

**Response** (200):
```json
{
  "objects": [
    {
      "timestamp": 1704067200000000000,
      "block_num": 5,
      "size": 64,
      "data": {"temperature": 72.5, "humidity": 45}
    }
  ],
  "count": 1
}
```

When `include_data=false`, the `data` field is omitted from each object.

---

### List Newest Data

Retrieve the N newest records, optionally scoped to a relative time window. This endpoint also supports time-windowed aggregation.

```
GET /api/stores/:store/data/newest
```

| Parameter            | In    | Type   | Default | Description |
|----------------------|-------|--------|---------|-------------|
| `limit`              | query | int    | 10      | Max records to return |
| `since`              | query | string | --      | Relative duration from now (e.g., `2h`, `24h`, `7d`) |
| `window`             | query | string | *1h / 48h* | **Only applies when `filter` is set.** Aggressive default lookback so a filtered scan doesn't read the whole store: `1h` plain, `48h` when `agg_window` is set. Override with a duration, or `window=0` for an unbounded full scan. An explicit `since` overrides it. Ignored without a filter. |
| `include_data`       | query | bool   | true    | Set `false` to return metadata only |
| `filter`             | query | string | --      | Substring match against serialized data |
| `filter_ignore_case` | query | bool   | false   | Case-insensitive substring matching |
| `format`             | query | string | `"full"` | `"full"` or `"compact"` -- schema stores only |
| `agg_window`         | query | string | --      | Aggregation window (e.g., `1m`, `5m`, `1h`) |
| `agg_fields`         | query | string | --      | Per-field aggregation (e.g., `temperature:avg,humidity:avg`) |
| `agg_default`        | query | string | --      | Default aggregation function (e.g., `avg` or `avg,sum,min,max`) |

**Response** (200): Same `DataListResponse` format as [List Oldest Data](#list-oldest-data). When aggregation parameters are provided, each object in the response contains aggregated values instead of raw data (see [Aggregation](#aggregation)).

**Filtered scans and the `scan` field.** Because `filter` is applied after fetch, a filtered query with no time bound would read the whole store to return a few matches. So a filtered `/newest` with no explicit `since` defaults to an aggressive lookback window (`1h` plain, `48h` for aggregation) and includes a `scan` object describing it:

```json
{
  "objects": [ ... ],
  "count": 50,
  "scan": { "window": "1h", "window_applied": true, "limit_reached": true }
}
```

- `window` -- the effective lookback applied.
- `window_applied` -- `true` when a window bounded the scan; `false` for `window=0` (full scan).
- `limit_reached` -- `true` when the scan stopped at `limit` with matching records still unexamined *within the window*. It does **not** assert matches exist beyond the window (the scan never looked there). To search the full history, pass `window=0` or an explicit `since`.

> **Behavior change.** Filtered `/newest` queries previously scanned the entire store. They now default to the last `1h` (`48h` with `agg_window`). Pass `window=0` or a time bound to restore a full scan. The `scan` field is additive; unfiltered queries are unchanged and omit it.

---

### Query Time Range

Retrieve records within a time window. Supports absolute timestamps, relative durations, and cursor-based pagination.

```
GET /api/stores/:store/data/range
```

**Time selection** -- use one of these approaches:

| Approach | Parameters | Description |
|----------|-----------|-------------|
| Absolute range | `start_time`, `end_time` | Nanosecond timestamps. Either or both optional; 0 or omitted = unbounded. |
| Relative range | `since` | Duration from now (e.g., `24h`, `7d`, `1w`) |
| Cursor-based | `after` | Nanosecond timestamp. Returns records strictly after this value. Use the `timestamp` of the last result as the next `after` value to paginate. |

| Parameter            | In    | Type   | Default | Description |
|----------------------|-------|--------|---------|-------------|
| `limit`              | query | int    | 100     | Max records to return |
| `start_time`         | query | int64  | --      | Start of range (inclusive), nanoseconds |
| `end_time`           | query | int64  | --      | End of range (inclusive), nanoseconds |
| `since`              | query | string | --      | Relative duration from now |
| `after`              | query | int64  | --      | Cursor: return records after this timestamp (exclusive) |
| `include_data`       | query | bool   | true    | Set `false` to return metadata only |
| `filter`             | query | string | --      | Substring match against serialized data |
| `filter_ignore_case` | query | bool   | false   | Case-insensitive substring matching |
| `format`             | query | string | `"full"` | `"full"` or `"compact"` -- schema stores only |
| `agg_window`         | query | string | --      | Aggregation window |
| `agg_fields`         | query | string | --      | Per-field aggregation |
| `agg_default`        | query | string | --      | Default aggregation function |

**Response** (200): Same `DataListResponse` format.

**Cursor-based pagination example:**

```bash
# First page
curl "http://localhost:21080/api/stores/sensors/data/range?since=1h&limit=50&include_data=true" \
  -H "X-API-Key: $KEY"

# Next page -- use the last timestamp from the previous response
curl "http://localhost:21080/api/stores/sensors/data/range?after=1704067200000000000&limit=50&include_data=true" \
  -H "X-API-Key: $KEY"
```

---

### Get Store Stats

Retrieve metadata about the store: capacity, block usage, and data boundaries.

```
GET /api/stores/:store/stats
```

No query parameters. Returns store statistics including block count, data size, and timestamp boundaries.

---

### Get Schema

Retrieve the current schema definition for a schema-type store.

```
GET /api/stores/:store/schema
```

**Response** (200):
```json
{
  "version": 1,
  "fields": [
    {"index": 1, "name": "temperature", "type": "float32"},
    {"index": 2, "name": "humidity", "type": "float32"},
    {"index": 3, "name": "sensor_id", "type": "string"}
  ]
}
```

**Supported field types:** `int8`, `int16`, `int32`, `int64`, `uint8`, `uint16`, `uint32`, `uint64`, `float32`, `float64`, `bool`, `string`

---

## 2. Outbound WebSocket Push

ts-store can act as a WebSocket **client**, connecting to a remote WebSocket server and streaming data to it in real time. This is a push-based mechanism: ts-store initiates the connection, walks through the data from a given starting point, and continuously sends new records as they arrive.

Push connections support filtering, time-windowed aggregation, and rule-based alerting.

### Creating a Push Connection

```
POST /api/stores/:store/ws/connections
X-API-Key: <api-key>
Content-Type: application/json
```

**Request body:**
```json
{
  "mode": "push",
  "url": "wss://remote.example.com/data",
  "from": 0,
  "format": "full",
  "headers": {"Authorization": "Bearer token"},
  "filter": "building:north",
  "filter_ignore_case": true,
  "agg_window": "1m",
  "agg_fields": "temperature:avg,humidity:avg,events:sum",
  "agg_default": "last"
}
```

| Field                | Type   | Required | Default | Description |
|----------------------|--------|----------|---------|-------------|
| `mode`               | string | yes      | --      | Must be `"push"` |
| `url`                | string | yes      | --      | Remote WebSocket URL (`ws://` or `wss://`) |
| `from`               | int64  | no       | 0       | Start timestamp: `0` = oldest data, `-1` = now (live tail), or a specific nanosecond timestamp |
| `format`             | string | no       | `"full"` | `"compact"` or `"full"` -- schema stores only |
| `headers`            | object | no       | --      | Custom HTTP headers sent during WebSocket handshake |
| `filter`             | string | no       | --      | Substring match -- only send records containing this string |
| `filter_ignore_case` | bool   | no       | false   | Case-insensitive filtering |
| `agg_window`         | string | no       | --      | Aggregation time window (e.g., `1m`, `5m`, `1h`) |
| `agg_fields`         | string | no       | --      | Per-field aggregation functions (see [Aggregation](#aggregation)) |
| `agg_default`        | string | no       | --      | Default aggregation function |

> Alerts are a separate resource, not configured on push connections — see [Alerting](#alerting) below.

**Response** (201):
```json
{
  "id": "a1b2c3d4",
  "mode": "push",
  "url": "wss://remote.example.com/data",
  "status": "connecting",
  "created_at": "2026-02-04T12:00:00Z"
}
```

**Connection behavior:**
- Polls for new data every 100ms
- Auto-reconnects with exponential backoff (1s to 60s max)
- Resumes from `last_timestamp` on reconnect -- no data is skipped
- Connection configuration is persisted to `ws_connections.json` in the store directory and survives server restarts

---

### Push Message Format

Each data record is sent to the remote server as a JSON message:

```json
{
  "type": "data",
  "timestamp": 1704067200000000000,
  "data": {
    "temperature": 72.5,
    "humidity": 45.2,
    "sensor": "room-1"
  }
}
```

| Field       | Type   | Description |
|-------------|--------|-------------|
| `type`      | string | Always `"data"` for normal records |
| `timestamp` | int64  | Nanosecond Unix timestamp of the record |
| `data`      | object | The record payload (expanded to full field names for schema stores by default) |

---

### Aggregation

When `agg_window` is set, ts-store batches records within each time window and sends a single aggregated message per window instead of individual records. This reduces message volume for high-frequency data.

**Single-function aggregation** applies one function per field:

```json
{
  "agg_window": "1m",
  "agg_fields": "temperature:avg,humidity:avg,events:sum",
  "agg_default": "last"
}
```

- `agg_fields` assigns specific functions to named fields (format: `field:function`)
- `agg_default` applies to any numeric field not listed in `agg_fields`
- Available functions: `avg`, `sum`, `min`, `max`, `last`, `first`, `count`, `stddev` (population standard deviation)

Output retains the original field names:
```json
{"temperature": 72.5, "humidity": 45.2, "events": 312}
```

**Multi-function aggregation** outputs multiple derived values per field:

```json
{
  "agg_window": "1m",
  "agg_default": "avg,sum,min,max"
}
```

Output appends the function name as a suffix:
```json
{
  "temperature_avg": 72.5,
  "temperature_sum": 7250.0,
  "temperature_min": 68.0,
  "temperature_max": 78.0,
  "humidity_avg": 45.2,
  "humidity_sum": 4520.0,
  "humidity_min": 40.0,
  "humidity_max": 50.0
}
```

Per-field multi-function syntax uses `+` to combine:
```json
{
  "agg_fields": "temperature:avg+min+max,events:sum"
}
```

Aggregation is also available on REST query endpoints (`/data/newest` and `/data/range`) using the same `agg_window`, `agg_fields`, and `agg_default` query parameters.

---

### Alerting

Alerts are an independent resource: each alert polls the store, evaluates one rule against every new record, and dispatches a payload through its configured sink (webhook, WebSocket, or MQTT). Alerts are **not** attached to push connections — they are managed under `/api/stores/:store/alerts` with a unified `type`-discriminated create endpoint.

**Create an alert** (webhook example — replace `type` and the nested options block for `ws` or `mqtt`):

```bash
curl -X POST "http://localhost:21080/api/stores/sensor-data/alerts" \
  -H "X-API-Key: <api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "webhook",
    "name": "high_temp",
    "condition": "temperature > 80",
    "cooldown": "5m",
    "restart_policy": "now",
    "webhook": {
      "url": "https://alerts.example.com/notify",
      "headers": {"Authorization": "Bearer xxx"}
    }
  }'
```

**Common alert fields** (apply to every transport):

| Field             | Type   | Required | Default | Description |
|-------------------|--------|----------|---------|-------------|
| `type`            | string | yes      | --      | `"webhook"`, `"ws"`, or `"mqtt"` |
| `name`            | string | yes      | --      | Human label, echoed on every fire |
| `condition`       | string | yes      | --      | Expression to evaluate (see syntax below) |
| `cooldown`        | string | no       | --      | Minimum interval between firings (e.g., `30s`, `5m`, `1h`) |
| `external_ref`    | string | no       | --      | Opaque pass-through (≤512 bytes); echoed verbatim on each fire |
| `restart_policy`  | string | no       | `"now"` | `"now"` = start from wall-clock now on Start (no cursor). `"resume"` = read cursor on Start and replay since `lastTs`. |
| `max_replay`      | string | no       | --      | Only valid when `restart_policy="resume"`. Cap how far back to replay (e.g., `1h`). Default: unbounded. |
| `poll_interval`   | string | no       | `1s`    | How often the worker polls the store |

**Transport-specific options** go in a nested object matching `type`:

| `type` | Nested key | Required fields | Optional fields |
|---|---|---|---|
| `webhook` | `webhook` | `url` | `headers`, `timeout` |
| `ws` | `ws` | `url` | `headers` |
| `mqtt` | `mqtt` | `broker_url`, `topic` | `username`, `password`, `qos` (default 1) |

**Condition syntax:**

| Operator   | Example | Description |
|------------|---------|-------------|
| `>`        | `temperature > 80` | Greater than |
| `>=`       | `temperature >= 80` | Greater than or equal |
| `<`        | `temperature < 10` | Less than |
| `<=`       | `temperature <= 10` | Less than or equal |
| `==`       | `status == "error"` | Equals (numbers or quoted strings) |
| `!=`       | `count != 0` | Not equal |
| `contains` | `message contains "ERROR"` | Substring match |

Field names may include dots and hyphens (`cpu.percent`, `temp.cpu_c`). Compound conditions use `AND` / `OR`:

```
temperature > 80 AND humidity < 30
status == "error" OR status == "critical"
```

**When the rule fires**, the alert payload is identical across all three transports:

```json
{
  "rule_name": "high_temp",
  "condition": "temperature > 80",
  "timestamp": 1704067200000000000,
  "data": {
    "temperature": 85.5,
    "humidity": 45.2
  },
  "store_name": "sensor-data",
  "external_ref": "dashboards/warehouse#component-42"
}
```

For WebSocket alerts, the payload is wrapped in a `{"type":"alert","alert":{...}}` frame on a fresh outbound connection. The `cooldown` timer prevents alert storms — once a rule fires, it won't fire again until the cooldown elapses.

**Manage alerts:**
```
GET    /api/stores/:store/alerts           # list (all types, tagged)
GET    /api/stores/:store/alerts/:id       # detail (config + status, secrets redacted)
DELETE /api/stores/:store/alerts/:id       # stop worker, remove persisted config
```

See [docs/alerting-architecture.md](docs/alerting-architecture.md) for the full reference.

---

### Remote Control: Seek

Push connections listen for control messages from the remote server, enabling the remote end to control the data stream over the existing outbound WebSocket.

This is essential in **outbound-only firewall environments** where the edge device can only dial out (e.g., HTTPS/WSS allowed, inbound blocked). The push connection is the only communication channel available. Since the push cursor is memory-only, a power failure or restart causes it to be lost. The connection restarts with `from: -1` (now), creating a data gap. The remote server -- which knows the last timestamp it received -- can send a `seek` message to request backfill over the same outbound connection.

**Seek message format** (sent by remote server to ts-store):
```json
{"type": "seek", "timestamp": 1704067200000000000}
```

| Field       | Type   | Description |
|-------------|--------|-------------|
| `type`      | string | Must be `"seek"` |
| `timestamp` | int64  | Nanosecond Unix timestamp to rewind to |

**Behavior:**
- ts-store rewinds its push cursor to the given timestamp
- The next poll cycle (100ms) begins sending data from that point forward
- If aggregation is active, the current partial window is discarded and a fresh window starts from the new position
- Unknown message types are logged and ignored (forward-compatible)

**Typical recovery flow:**
1. Edge device loses power, restarts, reconnects with `from: -1` (now)
2. Remote server receives the new connection, checks its last-received timestamp `T`
3. Remote sends `{"type": "seek", "timestamp": T}`
4. ts-store rewinds to `T` and re-sends all data from that point, filling the gap

---

### Managing Push Connections

**List all connections:**
```
GET /api/stores/:store/ws/connections
```

**Get connection status:**
```
GET /api/stores/:store/ws/connections/:id
```

Response:
```json
{
  "id": "a1b2c3d4",
  "mode": "push",
  "url": "wss://remote.example.com/data",
  "status": "connected",
  "last_timestamp": 1704067200000000000,
  "messages_sent": 1000,
  "messages_received": 0,
  "errors": 0
}
```

**Delete a connection:**
```
DELETE /api/stores/:store/ws/connections/:id
```

---

## 3. MQTT Sink

ts-store can publish data directly to an MQTT broker. Like WebSocket push, the MQTT sink walks through the store from a starting point and continuously publishes new records as they arrive.

### Creating an MQTT Connection

```
POST /api/stores/:store/mqtt/connections
X-API-Key: <api-key>
Content-Type: application/json
```

**Request body:**
```json
{
  "broker_url": "tcp://mqtt-broker:1883",
  "topic": "sensors/temperature",
  "from": 0,
  "include_timestamp": true,
  "cursor_persist_interval": 30,
  "client_id": "edge-device-01",
  "username": "optional",
  "password": "optional",
  "agg_window": "1m",
  "agg_fields": "temperature:avg,humidity:avg",
  "agg_default": "last"
}
```

| Field                     | Type   | Required | Default | Description |
|---------------------------|--------|----------|---------|-------------|
| `broker_url`              | string | yes      | --      | MQTT broker URL (`tcp://` or `ssl://`) |
| `topic`                   | string | yes      | --      | MQTT topic to publish to |
| `from`                    | int64  | no       | 0       | Start timestamp: `0` = oldest, `-1` = now, or specific nanosecond timestamp |
| `include_timestamp`       | bool   | no       | false   | Wrap payload with `{"timestamp": ..., "data": ...}` |
| `cursor_persist_interval` | int    | no       | 0       | Cursor persistence behavior (see [Cursor Persistence](#cursor-persistence)) |
| `client_id`               | string | no       | auto    | Custom MQTT client ID (default: `tsstore-<store>-<id>`) |
| `username`                | string | no       | --      | MQTT authentication username |
| `password`                | string | no       | --      | MQTT authentication password |
| `agg_window`              | string | no       | --      | Aggregation time window |
| `agg_fields`              | string | no       | --      | Per-field aggregation functions |
| `agg_default`             | string | no       | --      | Default aggregation function |

---

### MQTT Message Format

Each record is published as a JSON payload to the configured topic.

**Default** (`include_timestamp: false`): the raw data object is published directly.

```json
{"temperature": 72.5, "humidity": 45.2, "sensor": "room-1"}
```

**With timestamp wrapper** (`include_timestamp: true`):

```json
{
  "timestamp": 1704067200000000000,
  "data": {"temperature": 72.5, "humidity": 45.2, "sensor": "room-1"}
}
```

Schema stores are automatically expanded to full field names.

**MQTT behavior:**
- QoS 1 (at least once delivery)
- Blocks on each publish until the broker acknowledges
- Auto-reconnects with exponential backoff (1s to 60s) unless in one-shot mode

---

### Cursor Persistence

The `cursor_persist_interval` field controls how the MQTT sink tracks its position and handles failures:

| Value  | Behavior |
|--------|----------|
| `> 0`  | Persist cursor to disk every N seconds. On server restart, resume from the persisted position. Cursor file: `mqtt_<id>.cursor` in the store directory. |
| `0`    | In-memory cursor only. Auto-reconnects on broker failure but resets to `from` on server restart. (default) |
| `-1`   | One-shot mode. No persistence, no auto-reconnect. Connection stays dead on failure. |

---

### Managing MQTT Connections

**List all MQTT connections:**
```
GET /api/stores/:store/mqtt/connections
```

**Get connection status:**
```
GET /api/stores/:store/mqtt/connections/:id
```

Response:
```json
{
  "id": "abc123",
  "broker_url": "tcp://mqtt-broker:1883",
  "topic": "sensors/temperature",
  "status": "connected",
  "last_timestamp": 1704067200000000000,
  "messages_sent": 5000,
  "errors": 0
}
```

**Delete a connection:**
```
DELETE /api/stores/:store/mqtt/connections/:id
```

---

## Authentication

All data retrieval endpoints require store-level authentication. Provide the store API key via one of these methods (checked in this order):

| Method | Example |
|--------|---------|
| `X-API-Key` header | `X-API-Key: tsstore_a1b2c3d4-e5f6-7890-abcd-ef1234567890` |
| `Authorization` header | `Authorization: Bearer tsstore_a1b2c3d4-e5f6-7890-abcd-ef1234567890` |
| `api_key` query parameter | `?api_key=tsstore_a1b2c3d4-e5f6-7890-abcd-ef1234567890` |

The store API key is generated when the store is created and shown only once.

---

## Common Parameters

### Duration Format

Used in `since`, `agg_window`, and `cooldown` parameters:

| Format | Example | Description |
|--------|---------|-------------|
| `Ns`   | `30s`   | Seconds |
| `Nm`   | `15m`   | Minutes |
| `Nh`   | `2h`    | Hours |
| `Nd`   | `7d`    | Days |
| `Nw`   | `1w`    | Weeks |

### Filtering

All list endpoints and outbound connections support substring filtering:

- `filter` -- only return/send records where the serialized data contains this substring
- `filter_ignore_case` -- set `true` for case-insensitive matching

### Schema Format

Schema stores support a `format` parameter on all retrieval paths:

- `"full"` (default) -- field names are expanded: `{"temperature": 72.5, "humidity": 45}`
- `"compact"` -- numeric indices as stored: `{"1": 72.5, "2": 45}`

---

## Data Format Reference

How data appears in retrieval responses depends on the store's data type:

| Store Type | `data` Field Contents |
|------------|----------------------|
| `json`     | JSON object or value as stored |
| `schema`   | JSON object with field names expanded (or compact indices with `?format=compact`) |
| `text`     | UTF-8 string |
| `binary`   | Base64-encoded string |

---

## Reading Rollup Stores

A **rollup** store holds one aggregated record per closed time window of a source
store (see [Rollups](README-API.md#rollups-two-tier-aggregation)). Reading one is
the same as any schema store, with two things to know:

**Timestamps are at the window END (half-open window).** A record covers
`[label − window, label)` and is stamped at the window's end, aligned to UTC
epoch boundaries. A `1h` record stamped `10:00:00Z` covers `09:00–10:00`; a raw
sample at exactly `10:00:00.000Z` falls in the *next* window. Empty windows are
skipped (gaps, not zero rows).

**Every record has a `window_count` field** — the number of source samples in
that window. It is what lets you re-aggregate to coarser tiers correctly:

| To combine windows into a coarser one | Formula |
|---|---|
| `sum`, `min`, `max`, `count` | compose directly (`sum`→sum, `min`→min, …) |
| `avg` | **count-weighted**: `Σ(window_avg × window_count) / Σ(window_count)` |

A naive average of per-window averages is wrong when windows have different
sample counts — always weight `avg` by `window_count`.

Query responses for a rollup store echo a `rollup` descriptor so you know the
window without a second call:

```json
{ "rollup": {"role": "rollup", "window": "1h", "rollup_of": "sensors"},
  "objects": [ {"timestamp": ..., "data": {"cpu_avg": 22.5, "window_count": 3600}} ],
  "count": 24 }
```

---

[Back to main README](README.md) | [API Reference](README-API.md) | [Data Input](README-DATA-INPUT.md) | [Data Output](README-DATA-OUTPUT.md) | [CLI Reference](README-CLI.md)
