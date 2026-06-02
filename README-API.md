# ts-store REST API Reference

[Back to main README](README.md)

This document covers the REST API server configuration, authentication, and core endpoints.

## Starting the Server

```bash
# Admin key is required (prevents unauthorized store creation)
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
./tsstore serve
```

The server reads configuration from `config.json` (or `TSSTORE_CONFIG` env var).

## Configuration

Create `config.json`:

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 21080,
    "mode": "release",
    "socket_path": "/var/run/tsstore/tsstore.sock",
    "admin_key": "your-secure-admin-key-here",
    "tls": {
      "cert_file": "/path/to/cert.pem",
      "key_file": "/path/to/key.pem"
    }
  },
  "store": {
    "base_path": "./data",
    "data_block_size": 4096,
    "index_block_size": 4096,
    "num_blocks": 1024
  }
}
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TSSTORE_ADMIN_KEY` | (required) | Admin key for store creation (min 20 chars) |
| `TSSTORE_DATA_PATH` | `/data` | Base path for stores |
| `TSSTORE_HOST` | `0.0.0.0` | Server bind address |
| `TSSTORE_PORT` | `21080` | Server port |
| `TSSTORE_MODE` | `release` | Gin mode (debug/release) |
| `TSSTORE_SOCKET_PATH` | `/var/run/tsstore/tsstore.sock` | Unix socket path |
| `TSSTORE_TLS_CERT` | (optional) | Path to TLS certificate file |
| `TSSTORE_TLS_KEY` | (optional) | Path to TLS private key file |
| `TSSTORE_CONFIG` | (optional) | Config file path |

## TLS/HTTPS

To enable HTTPS, provide both certificate and key files:

```bash
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
export TSSTORE_TLS_CERT="/path/to/cert.pem"
export TSSTORE_TLS_KEY="/path/to/key.pem"
./tsstore serve
```

When TLS is enabled:
- HTTP API uses HTTPS
- WebSocket connections use WSS (secure WebSocket)
- Server logs will show "(HTTPS)" instead of "(HTTP)"

If TLS is not configured, the server falls back to HTTP/WS.

## Authentication

ts-store uses two types of authentication:

### Admin Key (for store management)
- Required for creating new stores
- Configured at server startup via `TSSTORE_ADMIN_KEY` (min 20 characters)
- Pass via `X-Admin-Key` header or `admin_key` query parameter

### Store API Key (for data operations)
- Each store has its own API key, generated when the store is created
- The key is shown only once - store it securely
- Pass via any of these methods (checked in order):
  - Header: `X-API-Key: tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
  - Header: `Authorization: Bearer tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
  - Query param: `?api_key=tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`

## Core Endpoints

### Health Check
```
GET /health
```
Returns server health status. No authentication required.

### Create Store (requires admin key)
```
POST /api/stores
X-Admin-Key: <admin-key>
Content-Type: application/json

{
  "name": "my-store",
  "num_blocks": 1000,
  "data_block_size": 4096,
  "index_block_size": 4096,
  "data_type": "json"
}
```

**Data Types:**
- `binary` - Raw binary data (Content-Type: application/octet-stream)
- `text` - UTF-8 text (Content-Type: text/plain)
- `json` - Arbitrary JSON objects (Content-Type: application/json) - default
- `schema` - Schema-defined compact JSON (Content-Type: application/json)

Returns the store API key (shown only once):
```json
{
  "name": "my-store",
  "api_key": "tsstore_a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "key_id": "a1b2c3d4"
}
```

### List Stores
```
GET /api/stores
```
No authentication required. Lists open stores by name.

### Delete Store (requires auth)
```
DELETE /api/stores/:store
X-API-Key: <api-key>
```

### Reset Store (requires auth)
```
POST /api/stores/:store/reset
X-API-Key: <api-key>
```
Clears all data from the store but keeps configuration, schema, and API keys. Useful for starting fresh without recreating the store.

### Get Store Stats
```
GET /api/stores/:store/stats
```
No authentication required. Returns operational metadata — block counts, time range, partition layout, disk usage. Deliberately public so dashboards and monitors don't need a per-store API key just to poll capacity/health. No stored data is exposed here.

### Get Store Activity Metrics
```
GET /api/stores/:store/metrics
```
No authentication required. Returns per-store activity counters: writes, reads, records evaluated, rule matches, alerts fired, and a `since` timestamp marking when the counters last started. Same public posture as `/stats` — no data exposed.

### Reset Store Activity Metrics (requires auth)
```
POST /api/stores/:store/metrics/reset
X-API-Key: <api-key>
```
Zeros the activity counters and advances the `since` timestamp to now. Useful for snapshotting throughput between two points in time.

## Schema Configuration (for schema-type stores)

Schema stores use a compact JSON format where field names are replaced with numeric indices. This reduces storage space significantly for structured data with known schemas.

**Important:** Schema stores expect flat JSON with dot-notation field names. Nested JSON objects are not supported. Use field names like `"cpu.pct"` and `"memory.total"` instead of nested structures like `{"cpu": {"pct": 5}}`.

### Get Current Schema
```
GET /api/stores/:store/schema
X-API-Key: <api-key>
```
Returns:
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

### Get a Specific Schema Version
```
GET /api/stores/:store/schema?version=<N>
X-API-Key: <api-key>
```
Returns the requested historical schema version (same shape as above), or `404` if that version does not exist.

### List All Schema Versions
```
GET /api/stores/:store/schema/versions
X-API-Key: <api-key>
```
Returns every schema version in ascending order:
```json
{
  "current_version": 2,
  "versions": [
    {"version": 1, "fields": [ ... ]},
    {"version": 2, "fields": [ ... ]}
  ]
}
```

### Set/Update Schema
```
PUT /api/stores/:store/schema
X-API-Key: <api-key>
Content-Type: application/json

{
  "fields": [
    {"index": 1, "name": "temperature", "type": "float32"},
    {"index": 2, "name": "humidity", "type": "float32"},
    {"index": 3, "name": "sensor_id", "type": "string"}
  ]
}
```

**Field types:** `int8`, `int16`, `int32`, `int64`, `uint8`, `uint16`, `uint32`, `uint64`, `float32`, `float64`, `bool`, `string`

**Schema evolution:** New schemas must only add fields (append-only). Existing fields cannot be modified or removed. This ensures backward compatibility with stored data.

**Compact storage:** When data is stored, field names are replaced with indices:
- Input: `{"temperature": 72.5, "humidity": 45, "sensor_id": "room-1"}`
- Stored: `{"1": 72.5, "2": 45, "3": "room-1"}`

**Per-record versioning:** Every record is tagged with the schema version it was written under. Read responses include that version as a `schema_version` field on each object, and it controls how the record is expanded.

**Read-time expansion views:** When retrieving data from a schema store, the expansion view is selected with the `?schema_version=` query parameter (the legacy `?format=compact` is still honored):

| Query | Result |
|-------|--------|
| (none) or `?schema_version=wide` | **Default.** Wide union schema: every field across all schema versions is present; fields a record lacks are returned as JSON `null`. (Parquet/Avro-style stable column set.) |
| `?schema_version=record` | Each record is expanded against the exact version it was written under; fields added in later versions are simply absent (no `null`). |
| `?schema_version=<N>` | Every record is expanded through schema version `N`; fields not in version `N` are dropped. |
| `?format=compact` | Raw compact form with numeric indices, e.g. `{"1": 72.5, "2": 45}`. |

Example — a record written under v1 `{temperature, humidity}` after the schema evolves to v2 adding `pressure`:
- `wide` (default): `{"temperature": 72.5, "humidity": 45, "pressure": null}`
- `record`: `{"temperature": 72.5, "humidity": 45}`

> Aggregation queries (`?agg_window=`) always use record-version expansion (absent, not `null`) so missing fields don't skew counts and averages; the `?schema_version=` view does not affect aggregation.

## Swagger UI

Explore the API interactively using Swagger Editor:

```bash
./tsstore swagger
```

This starts a local file server on port 21090, serves `swagger.yaml` with CORS headers, and opens https://editor.swagger.io in your browser with the spec pre-loaded.

---

[Back to main README](README.md) | [Data Input](README-DATA-INPUT.md) | [Data Output](README-DATA-OUTPUT.md) | [CLI Reference](README-CLI.md)
