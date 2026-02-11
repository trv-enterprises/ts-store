# ts-store Data Output Methods

[Back to main README](README.md)

This document covers all methods for reading and streaming data from a ts-store.

## REST API: Query Endpoints

### Get Data by Timestamp
```
GET /api/stores/:store/data/time/:timestamp
X-API-Key: <api-key>
```
Returns:
```json
{
  "timestamp": 1704067200000000000,
  "block_num": 5,
  "size": 64,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```

### List Oldest Data
```
GET /api/stores/:store/data/oldest?limit=10
X-API-Key: <api-key>
```
Returns the N oldest objects with data (default 10). Add `include_data=false` to return metadata only.

### List Newest Data
```
GET /api/stores/:store/data/newest?limit=10
GET /api/stores/:store/data/newest?since=2h&limit=100
X-API-Key: <api-key>
```
Returns the N newest objects with data (default 10). Use `since` parameter for relative time queries. Add `include_data=false` to return metadata only.

### Query Time Range
```
GET /api/stores/:store/data/range?start_time=X&end_time=Y&limit=100
GET /api/stores/:store/data/range?since=24h&limit=100
GET /api/stores/:store/data/range?after=<timestamp>&limit=100
X-API-Key: <api-key>
```
Returns objects within the time range. Add `include_data=true` to include object data.

**Query parameters (use one of these approaches):**
- `since` - Relative duration from now (e.g., `24h`, `7d`)
- `after` - Cursor-based: all records after this timestamp (exclusive), useful for polling
- `start_time` / `end_time` - Explicit bounds (either or both optional; 0 or omitted = unbounded)

**Supported duration formats for `since`:**
- `30s` - 30 seconds
- `15m` - 15 minutes
- `2h` - 2 hours
- `7d` - 7 days
- `1w` - 1 week

### Filtering Results

All list endpoints (`/data/oldest`, `/data/newest`, `/data/range`) support substring filtering:

```
GET /api/stores/:store/data/newest?filter=sensor:01&include_data=true
GET /api/stores/:store/data/range?since=1h&filter=BUILDING+A&filter_ignore_case=true
```

- `filter` - Substring to match in the object data
- `filter_ignore_case` - Set to `true` for case-insensitive matching (default: `false`)

Only objects containing the filter substring are returned.

### Delete Data by Timestamp
```
DELETE /api/stores/:store/data/time/:timestamp
X-API-Key: <api-key>
```

**Note:** This is a soft delete. The data is excluded from API responses and output streams, but remains on disk until the block is overwritten as the circular buffer wraps.

## Outbound Push: WebSocket to Remote Server

ts-store can connect to a remote WebSocket server and push data to it. See [docs/outbound-data-ws.md](docs/outbound-data-ws.md) for complete details.

```
POST /api/stores/:store/ws/connections
X-API-Key: <api-key>
Content-Type: application/json

{
  "mode": "push",
  "url": "wss://remote.example.com/data",
  "from": 0,
  "format": "compact",
  "headers": {"Authorization": "Bearer token"},
  "filter": "building:north",
  "filter_ignore_case": true
}
```

**Connection parameters:**
- `mode` - `push` (ts-store sends data to remote server)
- `url` - WebSocket URL to connect to
- `from` - Start timestamp (0 = from beginning)
- `format` - `compact` or `full` for schema stores
- `headers` - Custom HTTP headers for connection
- `filter` - Substring to match in data
- `filter_ignore_case` - `true` for case-insensitive matching

Outbound connections automatically reconnect with exponential backoff (1s to 60s max) and resume from the last sent timestamp.

### Aggregation

For high-frequency data, configure time-windowed aggregation to reduce downstream message volume:

```json
{
  "mode": "push",
  "url": "ws://dashboard:8080/metrics",
  "agg_window": "1m",
  "agg_fields": "temperature:avg,humidity:avg,events:sum",
  "agg_default": "last"
}
```

Multi-function aggregation outputs multiple values per field:

```json
{
  "agg_window": "1m",
  "agg_default": "avg,sum,min,max"
}
```

Output:
```json
{
  "temperature_avg": 72.5,
  "temperature_sum": 7250,
  "temperature_min": 68.0,
  "temperature_max": 78.0
}
```

### Alerting

Push connections can include alert rules that trigger when data matches conditions. See [docs/alerting-architecture.md](docs/alerting-architecture.md) for design details.

```json
{
  "mode": "push",
  "url": "wss://dashboard.example.com/data",
  "rules": [
    {
      "name": "high_temp",
      "condition": "temperature > 80",
      "webhook": "https://alerts.example.com/notify",
      "webhook_headers": {"Authorization": "Bearer xxx"},
      "cooldown": "5m"
    },
    {
      "name": "error_log",
      "condition": "message contains \"ERROR\"",
      "cooldown": "1m"
    }
  ]
}
```

**Condition operators:** `>`, `>=`, `<`, `<=`, `==`, `!=`, `contains`

**Compound conditions:** `AND`, `OR`

When a rule fires:
- An alert message is sent over the WebSocket (`{"type": "alert", ...}`)
- If `webhook` is configured, an HTTP POST is sent to the URL
- `cooldown` prevents alert storms (minimum time between alerts per rule)

## MQTT Sink: Publish to Broker

ts-store can publish data directly to an MQTT broker, maintaining its own cursor for crash recovery and respecting broker backpressure.

```
POST /api/stores/:store/mqtt/connections
X-API-Key: <api-key>
Content-Type: application/json

{
  "broker_url": "tcp://mqtt-broker:1883",
  "topic": "sensors/temperature",
  "from": 0,
  "include_timestamp": true,
  "cursor_persist_interval": 30,
  "username": "optional",
  "password": "optional"
}
```

**Connection parameters:**
- `broker_url` - MQTT broker URL (tcp:// or ssl://)
- `topic` - MQTT topic to publish to
- `from` - Start timestamp: `0`=oldest, `-1`=now, or specific nanosecond timestamp
- `include_timestamp` - Wrap data with `{"timestamp": ..., "data": ...}`
- `cursor_persist_interval` - Cursor persistence in seconds (see below)
- `client_id` - Custom MQTT client ID (default: tsstore-<store>-<id>)
- `username` / `password` - MQTT authentication

**Cursor persistence options (`cursor_persist_interval`):**
- `> 0` - Persist cursor every N seconds (resume from cursor on restart)
- `0` - In-memory only, auto-reconnect on failure (default)
- `-1` - No persistence, no auto-reconnect (one-shot mode, stays dead on failure)

**Behavior:**
- Uses QoS 1 (at least once delivery)
- Blocks on each publish until broker ACKs
- Auto-reconnects with exponential backoff (1s to 60s) unless `cursor_persist_interval: -1`
- Schema stores are automatically expanded to JSON

## Connection Management

### WebSocket Connections

**List Connections:**
```
GET /api/stores/:store/ws/connections
X-API-Key: <api-key>
```

**Get Connection Status:**
```
GET /api/stores/:store/ws/connections/:id
X-API-Key: <api-key>
```

Returns:
```json
{
  "id": "abc123",
  "mode": "push",
  "url": "wss://remote.example.com/data",
  "status": "connected",
  "last_timestamp": 1234567890,
  "messages_sent": 1000,
  "rules_count": 2,
  "alerts_fired": 47,
  "errors": 0
}
```

**Delete Connection:**
```
DELETE /api/stores/:store/ws/connections/:id
X-API-Key: <api-key>
```

### MQTT Connections

**List Connections:**
```
GET /api/stores/:store/mqtt/connections
X-API-Key: <api-key>
```

**Get Connection Status:**
```
GET /api/stores/:store/mqtt/connections/:id
X-API-Key: <api-key>
```

Returns:
```json
{
  "id": "abc123",
  "broker_url": "tcp://mqtt-broker:1883",
  "topic": "sensors/temperature",
  "status": "connected",
  "last_timestamp": 1234567890,
  "messages_sent": 5000,
  "errors": 0
}
```

**Delete Connection:**
```
DELETE /api/stores/:store/mqtt/connections/:id
X-API-Key: <api-key>
```

---

[Back to main README](README.md) | [API Reference](README-API.md) | [Data Input](README-DATA-INPUT.md) | [CLI Reference](README-CLI.md)
