# ts-store CLI Reference

[Back to main README](README.md)

This document covers command-line operations for ts-store.

## Installation

```bash
go get github.com/tviviano/ts-store
```

### Build the Server

```bash
go build -o tsstore ./cmd/tsstore
```

## Docker

### Using Pre-built Image (recommended)

```bash
# Pull from GitHub Container Registry
docker pull ghcr.io/trv-enterprises/ts-store:latest

# Run the container
docker run -d \
  -v tsstore-data:/data \
  -p 21080:21080 \
  -e TSSTORE_ADMIN_KEY="your-secure-admin-key-here" \
  --name tsstore \
  ghcr.io/trv-enterprises/ts-store:latest
```

### Building Locally

```bash
# Build the image
docker build -t tsstore .

# Run the container
docker run -d -v tsstore-data:/data -p 21080:21080 --name tsstore tsstore
```

### Docker Compose

```bash
docker compose up -d
```

### Managing stores in Docker

The CLI commands run inside the container via `docker exec`:

```bash
# Create a new store
docker exec tsstore tsstore create my-store
# Output shows API key (save it!)

# List API keys for a store
docker exec tsstore tsstore key list my-store

# Regenerate API key (revokes existing keys)
docker exec tsstore tsstore key regenerate my-store
```

This design maintains security - key management requires container access, while all data operations use the REST API with authentication.

## Store Management

### Create a Store

```bash
# Create a store with defaults (1024 blocks, 4KB data/index, json type)
./tsstore create my-store

# Create with custom settings
./tsstore create sensors --blocks 10000 --data-size 8192

# Create a schema store for compact JSON
./tsstore create metrics --type schema

# Create in a specific directory
./tsstore create logs --path /var/tsstore
```

**Options:**
- `--blocks <n>` - Number of blocks (default: 1024)
- `--data-size <n>` - Data block size in bytes, must be power of 2 (default: 4096)
- `--index-size <n>` - Index block size in bytes, must be power of 2 (default: 4096)
- `--path <dir>` - Base directory for stores (default: ./data or TSSTORE_DATA_PATH)
- `--type <type>` - Data type: binary, text, json, schema (default: json)

## API Key Management

API keys can only be managed via CLI (requires device access):

```bash
# Regenerate key (revokes all existing keys)
./tsstore key regenerate my-store

# List keys (shows IDs only, not actual keys)
./tsstore key list my-store

# Revoke a specific key
./tsstore key revoke my-store a1b2c3d4
```

## Server Commands

### Start the Server

```bash
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
./tsstore serve
```

**Options:**
- `--socket /path/to/socket.sock` - Unix socket path
- `--no-socket` - Disable Unix socket

### Swagger UI

```bash
./tsstore swagger
```

Starts a local file server on port 21090, serves `swagger.yaml` with CORS headers, and opens https://editor.swagger.io in your browser with the spec pre-loaded.

## Alerts

`tsstore alerts` manages webhook, WS, and MQTT alert resources. Each `add` invocation creates one alert with one rule; create multiple alerts to attach multiple rules to the same target. All forms hit `POST /api/stores/:store/alerts` with a `type`-discriminated body.

```bash
# Webhook alert
./tsstore alerts webhook add my-store \
  --url https://hooks.slack.com/services/... \
  --name high-temp --condition "temperature > 80" --cooldown 5m

# WS alert
./tsstore alerts ws add my-store \
  --url wss://alerts.example.com/in \
  --name error-log --condition "level == \"error\""

# MQTT alert
./tsstore alerts mqtt add my-store \
  --broker tcp://mqtt:1883 --topic alerts/heat \
  --name high-temp --condition "temperature > 80" --qos 1

# List / delete
./tsstore alerts list my-store
./tsstore alerts rm   my-store <alert-id>
```

**Required for every create:** `--name <label>` and `--condition <expr>`.

**Common create flags** (any transport):
- `--cooldown <duration>` — minimum interval between fires (e.g., `5m`)
- `--external-ref <s>` — opaque pass-through string echoed on every alert payload
- `--restart now|resume` — restart policy (default `now`). `resume` reads the persisted cursor on Start and replays records since.
- `--max-replay <duration>` — when `--restart=resume`, cap replay window (e.g., `1h`). Default: unbounded.
- `--poll <duration>` — poll interval (default `1s`)
- `--header K:V` — additional HTTP header, repeatable (webhook/ws only)
- `--api-key <key>` — store API key (or set `TSSTORE_API_KEY`)

**Transport-specific:** webhook adds `--timeout <duration>`. MQTT adds `--qos 0|1|2`, `--username`, `--password`.

## Rollups

`tsstore rollups` manages rollup aggregations: a background worker aggregates a high-frequency **source** store into clock-aligned windows and writes one record per closed window into a second **target** store (auto-created and sized from `--retention`). Subcommands hit `/api/stores/:store/rollups` and are scoped to the source store. See [Rollups](README-API.md#rollups-two-tier-aggregation) for the full model.

```bash
# Hourly min/max/avg for EVERY numeric field in system-stats, kept 1 year:
./tsstore rollups add system-stats --window 1h --default "min,max,avg" --retention 1y

# Per-field aggregation, minute windows kept 90 days:
./tsstore rollups add sensors --window 1m \
  --fields "temp:avg+max,humidity:avg" --retention 90d

# List / inspect / delete
./tsstore rollups list system-stats
./tsstore rollups get  system-stats <rollup-id>
./tsstore rollups rm   system-stats <rollup-id>
```

**Required for create:** `--window <duration>` and at least one of `--fields`/`--default`.

**Create flags:**
- `--default <funcs>` — functions applied to every numeric field, e.g. `min,max,avg` (the easy way to roll up all numeric parameters)
- `--fields <spec>` — per-field functions, e.g. `cpu:avg+max,mem:avg`
- `--retention <duration>` — how long the target keeps rows; sizes the target (default `1y`)
- `--target <store>` — target store name (default `<source>-<window>`)
- `--poll <duration>` — worker scan interval (default `30s`)
- `--restart resume|now` — restart policy (default `resume`)
- `--edge-tolerance <f>` — max over-retention fraction; picks partition count (default `0.10`)
- `--force-recreate` — flush/recreate the target to apply changed window/agg/retention params
- `--api-key <key>` — store API key (or set `TSSTORE_API_KEY`)

Every rollup record carries a `window_count` field for correct count-weighted downstream re-aggregation; see [Reading Rollup Stores](README-DATA_RETRIEVAL.md#reading-rollup-stores).

---

[Back to main README](README.md) | [API Reference](README-API.md) | [Data Input](README-DATA-INPUT.md) | [Data Output](README-DATA-OUTPUT.md)
