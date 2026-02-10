# Outbound WebSocket Data Push

tsstore can push data to downstream systems over WebSocket connections. This document explains how the outbound WebSocket feature works.

## Overview

The downstream system initiates the data flow by calling the tsstore API to create a connection configuration. Once configured, **tsstore acts as the WebSocket client** — it dials out to the downstream's WebSocket server and pushes data.

```
┌─────────────────┐                           ┌─────────────────┐
│                 │                           │                 │
│    TSSTORE      │  1. POST /ws/connections  │   DOWNSTREAM    │
│                 │ <─────────────────────────│                 │
│                 │   (create connection)     │  (initiates)    │
│                 │                           │                 │
│                 │  2. WebSocket Dial()      │                 │
│  (WS CLIENT)    │ ─────────────────────────>│  (WS SERVER)    │
│                 │                           │                 │
│                 │  3. JSON data messages    │                 │
│  Pushes data    │ ─────────────────────────>│  Receives data  │
│                 │                           │                 │
└─────────────────┘                           └─────────────────┘
```

**Connection flow:**
1. Downstream calls `POST /api/stores/{store}/ws/connections` to register its WebSocket server URL
2. tsstore dials out to that URL (tsstore is the WebSocket **client**)
3. tsstore pushes JSON data messages as new records arrive

The downstream system must:
- Call the tsstore API to create the connection (providing its WS server URL)
- Run a WebSocket server (listen on a port)
- Accept the incoming connection from tsstore
- Read JSON messages as they arrive
- Process/store/display the data

The downstream does **not** need to send anything back over the WebSocket. It's a one-way data push with automatic reconnection on failure.

## Creating a Connection

```bash
curl -X POST "http://localhost:21080/api/stores/my-store/ws/connections" \
  -H "X-API-Key: <store-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "push",
    "url": "ws://downstream-server:8080/ingest",
    "from": 0,
    "format": "full"
  }'
```

### Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `mode` | string | Must be `"push"` for outbound data streaming |
| `url` | string | WebSocket URL of the downstream server (ws:// or wss://) |
| `from` | int64 | Starting timestamp (nanoseconds). Use `0` to start from oldest data |
| `format` | string | `"full"` (default) or `"compact"` for schema stores |
| `headers` | object | Optional HTTP headers for the WebSocket handshake (e.g., auth tokens) |
| `filter` | string | Optional substring filter — only send matching records |
| `filter_ignore_case` | bool | Case-insensitive filter matching |
| `agg_window` | string | Optional aggregation window (e.g., `"1m"`, `"5m"`, `"1h"`) |
| `agg_fields` | string | Per-field aggregation functions. Single: `"temp:avg,count:sum"`. Multi: `"temp:avg+min+max"` |
| `agg_default` | string | Default aggregation function(s). Single: `"avg"`. Multi: `"avg,sum,min,max"` |
| `rules` | array | Optional alert rules (see [Alerting](#alerting) section) |

### Response

```json
{
  "id": "a1b2c3d4",
  "mode": "push",
  "url": "ws://downstream-server:8080/ingest",
  "status": "connecting",
  "created_at": "2026-02-04T12:00:00Z"
}
```

## Message Format

tsstore sends JSON messages to the downstream server:

```json
{
  "type": "data",
  "timestamp": 1707012345678901234,
  "data": {
    "temperature": 72.5,
    "humidity": 45.2
  }
}
```

| Field | Description |
|-------|-------------|
| `type` | Always `"data"` |
| `timestamp` | Record timestamp in nanoseconds since Unix epoch |
| `data` | The record payload (JSON object or raw value depending on store type) |

## Internal Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ TSSTORE SERVER                                                              │
│                                                                             │
│  ┌─────────────┐      ┌─────────────┐      ┌──────────────────────────┐    │
│  │             │      │             │      │                          │    │
│  │  Data Store │─────>│   Pusher    │─────>│  gorilla/websocket.Dial  │────────> To downstream
│  │  (circular  │ poll │  (100ms     │ send │                          │    │
│  │   buffer)   │      │   ticker)   │      │  Outbound WS Connection  │    │
│  │             │      │             │      │                          │    │
│  └─────────────┘      └─────────────┘      └──────────────────────────┘    │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

1. You create a connection via `POST /api/stores/{store}/ws/connections`
2. tsstore dials the specified URL (tsstore is the WebSocket **client**)
3. A background goroutine polls the data store every 100ms for new records
4. For each new record (after `from` timestamp), tsstore sends a JSON message
5. tsstore tracks the last sent timestamp and resumes from there on reconnect
6. On connection failure, tsstore automatically reconnects with exponential backoff (1s → 60s max)

## Managing Connections

### List connections

```bash
curl "http://localhost:21080/api/stores/my-store/ws/connections" \
  -H "X-API-Key: <store-api-key>"
```

### Get connection status

```bash
curl "http://localhost:21080/api/stores/my-store/ws/connections/a1b2c3d4" \
  -H "X-API-Key: <store-api-key>"
```

Response includes:
```json
{
  "id": "a1b2c3d4",
  "mode": "push",
  "url": "ws://downstream-server:8080/ingest",
  "status": "connected",
  "last_timestamp": 1707012345678901234,
  "messages_sent": 1523,
  "errors": 0
}
```

### Delete connection

```bash
curl -X DELETE "http://localhost:21080/api/stores/my-store/ws/connections/a1b2c3d4" \
  -H "X-API-Key: <store-api-key>"
```

## Aggregation

For high-frequency data, you can configure time-windowed aggregation to reduce downstream message volume:

```bash
curl -X POST "http://localhost:21080/api/stores/sensor-data/ws/connections" \
  -H "X-API-Key: <store-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "push",
    "url": "ws://dashboard:8080/metrics",
    "agg_window": "1m",
    "agg_fields": "temperature:avg,humidity:avg,events:sum",
    "agg_default": "last"
  }'
```

With aggregation enabled:
- Records are accumulated over the window period
- At window boundaries, a single aggregated message is sent
- Numeric fields use the specified aggregation function
- Non-numeric fields use `first` or `last`

### Multi-function aggregation

You can apply multiple aggregation functions to get statistics in a single output:

```bash
curl -X POST "http://localhost:21080/api/stores/sensor-data/ws/connections" \
  -H "X-API-Key: <store-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "push",
    "url": "ws://dashboard:8080/metrics",
    "agg_window": "1m",
    "agg_default": "avg,sum,min,max"
  }'
```

With `agg_default: "avg,sum,min,max"`, numeric fields produce multiple output values:

```json
{
  "temperature_avg": 72.5,
  "temperature_sum": 7250,
  "temperature_min": 68.0,
  "temperature_max": 78.0,
  "humidity_avg": 45.2,
  "humidity_sum": 4520,
  "humidity_min": 40.0,
  "humidity_max": 50.0
}
```

For per-field multi-function, use `+` to separate functions:

```bash
"agg_fields": "temperature:avg+min+max,events:sum"
```

## Example: Simple WebSocket Server (Python)

```python
import asyncio
import websockets
import json

async def handler(websocket):
    print("tsstore connected")
    async for message in websocket:
        data = json.loads(message)
        print(f"Received: ts={data['timestamp']} data={data['data']}")

async def main():
    async with websockets.serve(handler, "0.0.0.0", 8080):
        print("WebSocket server listening on ws://0.0.0.0:8080")
        await asyncio.Future()  # run forever

asyncio.run(main())
```

## Example: Simple WebSocket Server (Node.js)

```javascript
const WebSocket = require('ws');

const wss = new WebSocket.Server({ port: 8080 });

wss.on('connection', (ws) => {
  console.log('tsstore connected');

  ws.on('message', (message) => {
    const data = JSON.parse(message);
    console.log(`Received: ts=${data.timestamp} data=${JSON.stringify(data.data)}`);
  });
});

console.log('WebSocket server listening on ws://0.0.0.0:8080');
```

## Persistence

Connection configurations are persisted to `ws_connections.json` in the store's data directory. Connections are automatically restored and restarted when tsstore starts.

## Alerting

You can configure rules to trigger alerts when data matches specific conditions. Alerts can be sent over the WebSocket connection and/or to a webhook endpoint.

### Creating a Connection with Alerts

```bash
curl -X POST "http://localhost:21080/api/stores/sensor-data/ws/connections" \
  -H "X-API-Key: <store-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "push",
    "url": "ws://dashboard:8080/metrics",
    "rules": [
      {
        "name": "high_temp",
        "condition": "temperature > 80",
        "webhook": "https://alerts.example.com/notify",
        "webhook_headers": {"Authorization": "Bearer xxx"},
        "cooldown": "5m"
      },
      {
        "name": "critical_status",
        "condition": "status == \"error\" OR status == \"critical\"",
        "cooldown": "1m"
      }
    ]
  }'
```

### Rule Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Rule name/identifier (appears in alert messages) |
| `condition` | string | Condition expression (see syntax below) |
| `webhook` | string | Optional webhook URL to call when rule fires |
| `webhook_headers` | object | Optional HTTP headers for webhook (e.g., auth tokens) |
| `cooldown` | string | Minimum time between alerts for this rule (e.g., `"5m"`, `"1h"`) |

### Condition Syntax

Simple conditions:
- `temperature > 80` - field greater than value
- `temperature >= 80` - greater than or equal
- `temperature < 80` - less than
- `temperature <= 80` - less than or equal
- `status == "error"` - string equality (use quotes)
- `count != 0` - not equal
- `message contains "ERROR"` - substring match (case-sensitive)

Compound conditions:
- `temperature > 80 AND humidity < 30` - both must be true
- `status == "error" OR status == "critical"` - either can be true
- `message contains "ERROR" OR message contains "FATAL"` - text matching

### Alert Message Format

When a rule fires, an alert message is sent over the WebSocket:

```json
{
  "type": "alert",
  "timestamp": 1707012345678901234,
  "alert": {
    "rule_name": "high_temp",
    "condition": "temperature > 80",
    "timestamp": 1707012345678901234,
    "data": {
      "temperature": 85.5,
      "humidity": 45.2
    },
    "store_name": "sensor-data"
  }
}
```

### Webhook Payload

If a webhook URL is configured, an HTTP POST is sent with the same alert payload:

```json
{
  "rule_name": "high_temp",
  "condition": "temperature > 80",
  "timestamp": 1707012345678901234,
  "data": {
    "temperature": 85.5,
    "humidity": 45.2
  },
  "store_name": "sensor-data"
}
```

### Cooldown

The `cooldown` parameter prevents alert storms. After a rule fires, it won't fire again until the cooldown period elapses. For example, with `"cooldown": "5m"`, if temperature exceeds 80 at 10:00, the next alert for that rule won't fire until after 10:05 (even if temperature stays above 80).

### Performance

Rules are evaluated asynchronously in a separate goroutine, outside the data path's lock window. This ensures:
- Data streaming is not blocked by rule evaluation
- Webhook calls don't slow down the main data flow
- High-frequency data can be processed efficiently

## Connection Status Values

| Status | Description |
|--------|-------------|
| `connecting` | Attempting to establish WebSocket connection |
| `connected` | Connected and streaming data |
| `disconnected` | Not connected (will auto-reconnect) |
| `error` | Last operation failed (see `last_error` field) |
