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

# List API keys (IDs and grants, never the keys)
docker exec tsstore tsstore key list

# Mint a scoped key
docker exec tsstore tsstore key create --grant read:* --note "dashboard"

# Regenerate the key for one store
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

API keys can only be managed via CLI (requires device access) — there is no HTTP endpoint for creating, supplying, or rotating keys.

Keys live in a central registry (`<data-path>/keys.registry.json`, mode 0600, hashes only) and carry **grants**: what the key may do, and on which stores.

> **On a production host, run key commands as the service user.** Every key
> command rewrites the registry file (write-temp-then-rename), and the file
> ends up owned by whoever ran the command. The running server must be able
> to rewrite that same file later — store creation registers the new store's
> key — so a registry left root-owned by a bare `sudo` makes subsequent
> store creates fail. Two more things the service user needs: a readable
> working directory (the CLI probes `./config.json`, and `sudo -u tsstore`
> inherits your login's cwd — typically your home directory, which `tsstore`
> cannot enter, failing with `permission denied` before doing anything) and
> the production data path (the default is `./data`):
>
> ```bash
> sudo -u tsstore sh -c 'cd /var/lib/tsstore && \
>   TSSTORE_DATA_PATH=/var/lib/tsstore tsstore key create \
>     --grant "read:*" --note "dashboard"'
> ```

```bash
# Read-only across every store (e.g. a dashboard)
./tsstore key create --grant read:* --note "dashboard"

# Ingest into a namespace, full control of one store
./tsstore key create \
  --grant read,write:sensors-* \
  --grant read,write,manage:home-env \
  --note "collector"

# Provision-only: may CREATE sensors-* stores, cannot read or write them
./tsstore key create --grant admin:sensors-* --note "provisioner"

# List keys (IDs and grants — never the keys themselves)
./tsstore key list
./tsstore key list my-store       # only keys that can reach my-store

# Revoke by ID (IDs are globally unique)
./tsstore key revoke a1b2c3d4

# Replace the keys granting a store by name, minting a fresh one
./tsstore key regenerate my-store
```

### Access classes

`read`, `write`, `manage`, and `admin` are independent flags, **not a hierarchy** — `manage` does not imply `read`.

| Class | Covers |
|---|---|
| `read` | Query, range, oldest/newest, schema read, stream out — including WS/MQTT push-connection lifecycle and alert reads (`GET /alerts`, `GET /alerts/:id`) |
| `write` | Ingest (REST, WebSocket, Unix socket) |
| `manage` | Alert mutation (create/update/test/delete), rollups, schema writes, reset, delete |
| `admin` | Store lifecycle: creating stores matching the pattern. No data or config access — pair with read/write to also use what you create. |

Store patterns are an exact name, a prefix glob (`sensors-*`), or `*`.

### Bring your own key

Mint the key yourself — in 1Password, say — and have ts-store adopt it, so the vault is the source rather than a copy of ts-store's output:

```bash
op read op://HOMELAB/nas-disks/credential \
  | ./tsstore key create --key-file - --grant read,write:nas-syn-002-disks

# Also works at store-creation time
op read op://HOMELAB/nas-disks/credential \
  | ./tsstore create nas-syn-002-disks --type json --key-file -
```

`--key-file <path|->` is the documented path. `--api-key <key>` also works but puts the secret in the process table (`/proc/<pid>/cmdline` is world-readable) for as long as the command runs. Supplied keys must use the same format as generated ones: `tsstore_` prefix, ≥44 characters. An adopted key is never echoed back.

### Upgrading from per-store keys

Existing per-store keys are imported into the registry automatically on first boot, each granted `read,write,manage` on its own store — the exact authority it had before. **No reconfiguration, no rotation.** The old `<store>/keys.json` files are left in place so a downgrade still works.

Note `key regenerate` now replaces keys whose grants name the store **by name**; wildcard grants (`read:*`) describe a namespace and are deliberately left alone.

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

# Staleness alert — fire when the store STOPS receiving data
./tsstore alerts webhook add nas-syn-002-disks \
  --url https://hooks.slack.com/services/... \
  --name "collector went quiet" \
  --rule-type staleness --max-age 5m --cooldown 30m

# List / delete
./tsstore alerts list my-store
./tsstore alerts rm   my-store <alert-id>
```

**Required for every create:** `--name <label>`, plus the rule fields for the chosen `--rule-type`.

### Rule types

`--rule-type` selects what makes the alert fire. It defaults to `condition`, so existing commands are unchanged.

| Rule type | Fires on | Rule flag |
|---|---|---|
| `condition` (default) | A record arrived whose fields match the expression | `--condition <expr>` |
| `staleness` | Nothing has arrived for longer than the threshold | `--max-age <duration>` |

A condition rule only ever runs against records that arrived, so a collector that is OOM-killed or loses its network produces zero records and therefore zero alerts. A staleness rule is checked on every poll tick — including ticks where nothing arrived — so it fires precisely when the collector cannot report for itself.

Notes on `--max-age`:
- **No default, by design.** A collector polling every 60s should alert after a few missed polls; an event-driven source can be legitimately silent for days. Set it per alert.
- A store that has **never** received data never fires — it is treated as not-yet-started, not stale.
- When data returns the alert simply stops firing; there is no separate "resolved" event. Use `--cooldown` to bound repeats while a store stays quiet.
- `--condition`, `--restart resume`, and `--max-replay` are rejected for staleness rules (a staleness rule has no cursor and no triggering record).
- This is **per-store**: it catches a dead collector, not one series going quiet while others keep reporting. That case is issue #135.

**Common create flags** (any transport):
- `--cooldown <duration>` — minimum interval between fires (e.g., `5m`)
- `--external-ref <s>` — opaque pass-through string echoed on every alert payload
- `--restart now|resume` — restart policy (default `now`). `resume` reads the persisted cursor on Start and replays records since.
- `--max-replay <duration>` — when `--restart=resume`, cap replay window (e.g., `1h`). Default: unbounded.
- `--poll <duration>` — poll cadence hint (default `1s`). The store runs one shared poll loop for all its alerts, ticking at the minimum across them.
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

# List / inspect / delete. get/rm accept the rollup's ID, its target store
# name, or its window (when unambiguous) — no need to copy-paste the ID:
./tsstore rollups list system-stats
./tsstore rollups get  system-stats 1h
./tsstore rollups rm   system-stats system-stats-1h

# Delete a rollup AND its target store (removes the target's linked API keys
# too, so the source store can later be deleted without a dependents error):
./tsstore rollups rm   system-stats <rollup-id> --delete-target
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
