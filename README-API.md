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

`base_path` is the data directory every store lives under. The other three
(`data_block_size`, `index_block_size`, `num_blocks`) are **defaults applied
when a store is created without them** — see [Create Store](#create-store).
They do not affect stores that already exist: a store's geometry is fixed in
its own metadata at creation, so changing these only shapes stores created
afterwards.

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
- Pass via the `X-Admin-Key` header

### Scoped API Keys (for data operations)

Keys live in a central registry and carry **grants** — what a key may do, and on which stores. Creating a store still mints a key with full access to that store, so the common case is unchanged.

- Pass via either header (checked in order):
  - Header: `X-API-Key: tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
  - Header: `Authorization: Bearer tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
- A generated key is shown only once — store it securely.

Credentials are header-only — query-string keys would land in proxy and access logs. The one exception is the inbound WebSocket handshake (`/ws/write?api_key=...`), where the browser WebSocket API cannot set headers.

#### Access classes

Every endpoint requires one of three classes. They are **independent flags, not a hierarchy** — `manage` does not imply `read`, so a key lists exactly the classes it needs.

| Class | Covers |
|---|---|
| `read` | Query, range, oldest/newest, schema read, stream out — including WS/MQTT push-connection lifecycle and alert **reads** |
| `write` | Ingest (`POST /data`, `GET /ws/write`, the Unix socket) |
| `manage` | Store-scoped administration: alert **mutation** (create/update/test/delete), rollups, schema writes, reset, store delete |
| `admin` | Store **lifecycle**: creating stores whose name matches the pattern. Grants **no** data or configuration access — an admin-only key can bring a store into existence and never read, write, or configure it. |

Alerts are store-scoped rather than server-scoped, so alert management is a `manage` grant on the store — it does not require the admin key.

Alert **reads** (`GET /alerts`, `GET /alerts/:id`) are `read`, not `manage`: seeing which rules exist and whether they are keeping up (fired counts, lag, drop counters) is observability over the store's data, so a dashboard doesn't need the authority to reconfigure or delete alerts. This is safe because alert read payloads redact every credential surface — sink URLs lose userinfo and query strings, MQTT passwords are masked, and header values are masked by allowlist. Creating, updating, testing, and deleting alerts remain `manage`.

Push connections (WS and MQTT sinks, and the consolidated `GET /connections` view) are `read`, not `manage`: a push connection only ever delivers data the key could already poll, so consumers create and remove their own subscriptions without holding `manage`'s reset/schema powers. This also means a manage-only key cannot gain data access by pointing a push connection at itself.

The exception is a **pull-mode** WS connection (`"mode": "pull"`), which reverses the data direction — ts-store ingests what the remote sends — so creating one additionally requires `write` on the store (403 otherwise).

#### Grants

A grant pairs access classes with a store pattern: an exact name, a prefix glob (`sensors-*`), or `*` for every store.

```
read:*                        read-only across every store
read,write:sensors-*          ingest into a namespace
read,write,manage:home-env    full control of one store
admin:sensors-*               may CREATE sensors-* stores; no access to them
```

An `admin` grant contributes no access classes to `GET /api/stores`, so an
admin-only key sees an empty listing — it can create stores, not enumerate
them. Note also that a store's own initial key carries `read,write,manage` and
deliberately **not** `admin`: lifecycle authority is always granted explicitly.

Wildcards matter because an enumerated store list goes stale the moment a collector auto-creates a store.

#### 401 vs 403

- **401** — no key, a malformed key, or a key not in the registry.
- **403** — the key is valid but lacks the required class for that store. Retrying with the same credential will never succeed.

#### Bring your own key

A key can be minted externally (e.g. in a secrets vault) and adopted, so the vault is the origin rather than a downstream copy of ts-store's output. Supplied keys must use the same format as generated ones: the `tsstore_` prefix, at least 44 characters total.

```bash
op read op://HOMELAB/nas-disks/credential \
  | tsstore key create --key-file - --grant read,write:nas-syn-002-disks
```

When you supply a key, no response or CLI output echoes it back.

#### Upgrading from per-store keys

Pre-existing per-store keys are imported into the registry automatically on first boot, each granted `read,write,manage` on its own store — exactly the authority it had before. **No client reconfiguration and no key rotation is required.** The old `<store>/keys.json` files are left in place (unread) so a downgrade still works.

### Failed-auth rate limiting

Repeated authentication failures are throttled per client IP: after 10 consecutive `401`s the IP receives `429 Too Many Requests` (with a `Retry-After` header) for 30 seconds, doubling on each further block up to 15 minutes. Any successful authentication from that IP clears its counter. This applies to both store API keys and the admin key; the state is in-memory and resets on server restart.

## Core Endpoints

### Health Check
```
GET /health
```
Returns server health status. No authentication required.

### Create Store
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

**Authentication — either credential works:**

- `X-Admin-Key` — the server-tier bootstrap credential. A fresh server has an
  empty key registry, so this always works and is the only option before any
  key exists.
- `X-API-Key` — a registry key holding an **`admin` grant whose pattern covers
  the requested name** (see [Access classes](#access-classes)). A key granted
  `admin:sensors-*` may create `sensors-garage` but not `billing` — scoping the
  all-or-nothing admin key cannot express. Since this route has no `:store` in
  the path, the name is read from the request body to evaluate the pattern.

A name outside the grant's pattern is `403`; an unrecognized key is `401`.

**Fields:** only `name` is required. `num_blocks`, `data_block_size`, and
`index_block_size` are **optional** — omit them and the server's
[configured defaults](#configuration) (`store.num_blocks` etc.) apply.
`data_type` defaults to `json`.

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
X-API-Key: <api-key>
```

**Requires an API key**, and returns only the stores that key holds a grant on, each annotated with the caller's effective access classes:

```json
{
  "stores": [
    { "name": "sensor-data", "data_type": "schema", "role": "store", "access": ["read", "write", "manage"] },
    { "name": "docker-containers", "data_type": "schema", "role": "source", "access": ["read"] }
  ]
}
```

`access` is the subset of `read`/`write`/`manage` (always in that order) the key holds on that store — so a client learns "read here, read+write there" without probing. Access classes are independent flags (`manage` does not imply `read`), and *any* grant makes a store visible: a write-only collector key sees its store with `"access": ["write"]`.

This doubles as a connection test that needs no store name — endpoint plus key is enough — and as store discovery for clients building a picker. A read-only dashboard key sees exactly the stores it can chart; a store's own key sees just that store.

> **Breaking change.** This endpoint was previously unauthenticated and returned every store. It now returns `401` without a valid key. An unauthenticated listing would disclose the full store inventory of a deployment, which is precisely what scoped keys exist to prevent.
>
> The `access` field is additive. Visibility widened with it: keys holding only `write` and/or `manage` on a store previously got an empty listing; they now see those stores.

The admin key is deliberately not accepted here: it is the server tier (store lifecycle, key management) and holds no grants, so "which stores can I access?" has no meaningful answer for it.

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

## Rollups (two-tier aggregation)

A **rollup** periodically aggregates a high-frequency **source** store into clock-aligned time windows and writes one record per closed window into a second **target** store. This gives you cheap long-range queries (e.g. a year of hourly averages) while keeping the raw high-frequency data for drill-down over short ranges.

Rollup endpoints are scoped to the **source** store.

### Create a rollup
```
POST /api/stores/:store/rollups
X-API-Key: <api-key>
Content-Type: application/json

{
  "window": "1h",                 // aggregation window (1m, 1h, 1d, ...)
  "agg_fields": "cpu:avg+max,mem:avg",
  "agg_default": "avg",           // optional: applied to numeric fields not listed
  "retention": "1y",              // how long the target keeps rows (sizes the target)
  "poll_interval": "30s",         // optional: how often the worker scans (default 30s)
  "target_store": "",             // optional: defaults to "<source>-<window>"
  "edge_tolerance": 0.10,         // optional: max over-retention fraction (picks partitions)
  "force_recreate": false         // see "Changing a rollup" below
}
```

On success the target store is **auto-created** if it doesn't exist:
- It is a **schema store**; its schema is derived from the agg spec (e.g. `cpu_avg`, `cpu_max`, `mem`, plus a `window_count` field) and **auto-evolves** (append-only) if you later add aggregates.
- It is **sized from `retention`**: capacity = `retention / window` records, with the partition count chosen so worst-case over-retention stays within `edge_tolerance`. Capacity is always rounded **up** (you get at least the requested retention).
- Its **API keys are linked to the source** — the *same* API key authenticates both stores, and rotating the source's key applies to the target automatically.

### Target store naming
If `target_store` is omitted, the name is `<source>-<canonical-window>` (e.g. source `sensors`, window `60m` → `sensors-1h`; the window is normalized to its largest clean unit). Re-creating an identical rollup is idempotent (reuses the target). If a store of that name already exists but isn't a compatible rollup of this source, the request is rejected — pass an explicit `target_store` or `force_recreate`.

### Record format & consuming rollups
Each rollup record covers the **half-open window `[label − window, label)`** and is **timestamped at the window END** (UTC-epoch-aligned). So a `1h` record stamped `10:00:00Z` covers `09:00–10:00`; a raw sample at exactly `10:00:00.000Z` belongs to the *next* window.

Every record carries **`window_count`** — the number of source samples in the window:
```json
{ "cpu_avg": 22.5, "cpu_max": 80.0, "mem": 41.2, "window_count": 3600 }
```
To roll minute/hourly data up further (e.g. to daily) **client-side**, `sum`/`min`/`max`/`count` compose directly, but **`avg` must be count-weighted**:
```
day_avg = Σ(window_avg × window_count) / Σ(window_count)
```
A naive average of the per-window averages is wrong when windows have different sample counts. (ts-store produces a single rollup tier; coarser tiers are the consumer's job, which is why `window_count` is always present.)

Windows with **no source data are skipped** (no record written), so the series has gaps rather than zero rows.

When you query a rollup target's data, the response envelope echoes a `rollup` descriptor so you know the window in the same call:
```json
{ "rollup": {"role": "rollup", "window": "1h", "rollup_of": "sensors"},
  "objects": [ ... ], "count": 24 }
```

### List / get / delete
```
GET    /api/stores/:store/rollups          # list rollups on this source
GET    /api/stores/:store/rollups/:id      # one rollup's status
DELETE /api/stores/:store/rollups/:id      # remove rollup (target store left intact)
DELETE /api/stores/:store/rollups/:id?delete_target=true   # remove rollup AND its target store
```

`delete_target=true` also removes the target's linked API keys, so the source store can afterwards be deleted without a dependents conflict. It is refused if another rollup still writes to the same target.

### Changing a rollup
Rollup parameters can't be changed in place (a new window or agg spec invalidates already-written rows; a new retention changes the target's fixed capacity). To apply new parameters, POST with **`"force_recreate": true`** — the target is flushed/recreated, the cursor is reset, and the rollup is rebuilt from all source history still available. A parameter-changing POST **without** `force_recreate` is rejected.

> Deleting a source store that still has linked rollup targets is refused (the targets share its API keys) — the error names the surviving targets. Delete those target stores first, either directly or by deleting their rollups with `delete_target=true`.

## Swagger UI

Explore the API interactively using Swagger Editor:

```bash
./tsstore swagger
```

This starts a local file server on port 21090, serves `swagger.yaml` with CORS headers, and opens https://editor.swagger.io in your browser with the spec pre-loaded.

---

[Back to main README](README.md) | [Data Input](README-DATA-INPUT.md) | [Data Output](README-DATA-OUTPUT.md) | [CLI Reference](README-CLI.md)
