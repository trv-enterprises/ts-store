# docker-stats

Collects Docker container and daemon statistics and writes them to ts-store.
Talks to the local Docker daemon over its Unix socket using raw HTTP — no
Docker SDK, so the result is a single static binary with zero dependencies.

## What it collects

Two independent streams, each into its own store:

| Store | Cadence | Shape |
|---|---|---|
| `docker-containers` | every 20s | one row **per running container** per tick, distinguished by a `container` field |
| `docker-daemon` | every 120s | one row per tick with host-wide Docker totals |

The daemon store ticks slower on purpose: those numbers barely move, and
`/system/df` is comparatively expensive (it walks the filesystem).

**Counters, not rates.** `net.*` and `blkio.*` are stored as raw cumulative
counters (exactly as Docker reports them), not per-second rates. Compute the
rate at query time as `Δbytes / Δt` — that way a missed tick never corrupts a
rate, and you keep the raw signal.

## Features

- Reads from `/var/run/docker.sock` via raw HTTP — no Docker SDK, no deps
- Per-container CPU %, memory (working set + limit + %), network, block I/O, PIDs
- Daemon-wide container/image counts and disk usage (`/info` + `/system/df`)
- Supports Unix socket (low latency) or HTTP API output
- Two stores tick independently (20s / 120s), each with its own API key
- Single static binary

## Deployment Topology

One ts-store instance is a multi-tenant server: it holds **many** stores, each
with its own name, schema, and API key, and it doesn't care who writes to them.
So the model is **one central ts-store, many collectors** — run ts-store once
on whatever host you like (a Pi, a VM, an existing homelab service), and point
every collector at it over HTTP.

There are **two sockets in play, with opposite locality rules** — this is the
key thing to get right:

- **The ts-store side is remote-capable.** With `-http` the collector reaches
  ts-store over the network, so the store can live anywhere reachable. (The
  `-socket` path is a local-only fast lane for when they share a host.)
- **The Docker side must be local.** The collector reads
  `/var/run/docker.sock`, which is local to the Docker LXC/host — so **the
  collector runs in (or on) the Docker host**. Don't try to attach to a remote
  Docker over TCP (`tcp://`): an exposed daemon socket is root-equivalent access
  to that host and a well-known security footgun. Keep it local socket, remote
  store.

Only the collector needs to sit next to Docker; the store lives elsewhere.
Keeping the store off the Docker host also means its history survives the LXC
being rebuilt or OOM'd — you don't lose the metrics right when you'd want them.

**Separate hosts by store name.** Each collector deployment writes to its own
pair of uniquely-named stores; the store name *is* the separator. For a Docker
host called `hostA`:

```bash
docker-stats -http http://tsstore:21080 \
  -container-store hostA-docker-containers \
  -daemon-store    hostA-docker-daemon \
  -container-key   <hostA-container-key> \
  -daemon-key      <hostA-daemon-key>
```

```
┌─ Docker host: hostA ───────┐        ┌─ ts-store (central) ─────────────┐
│  dockerd + docker.sock     │        │  hostA-docker-containers         │
│  docker-stats ──────HTTP───┼───────▶│  hostA-docker-daemon             │
└────────────────────────────┘        │  hostB-docker-containers         │
┌─ Docker host: hostB ───────┐        │  hostB-docker-daemon             │
│  dockerd + docker.sock     │        │  pi-system-stats  (system-stats) │
│  docker-stats ──────HTTP───┼───────▶│  ...                             │
└────────────────────────────┘        └──────────────────────────────────┘
```

Create a distinct store pair (with the schemas below) per Docker host, and give
each its own API keys. Only when the collector and ts-store share a filesystem —
e.g. ts-store runs *in* the same host — should you prefer the Unix socket
(`-socket`) for its lower latency; across hosts, use `-http`.

## Building

```bash
# From ts-store root
cd examples/docker-stats

# For Linux AMD64
GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o docker-stats-linux-amd64 .

# For ARM64 (Raspberry Pi 4, etc.)
GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o docker-stats-linux-arm64 .
```

## Usage

```bash
# Print both streams to stdout (for testing — no ts-store needed)
./docker-stats -stdout

# Write to ts-store via Unix socket (recommended for local ts-store)
./docker-stats -socket /var/run/tsstore/tsstore.sock \
               -container-key tsstore_xxxx-xxxx-xxxx \
               -daemon-key    tsstore_yyyy-yyyy-yyyy

# Write to ts-store via HTTP (for remote or Docker deployments)
./docker-stats -http http://localhost:21080 \
               -container-key tsstore_xxxx-xxxx-xxxx \
               -daemon-key    tsstore_yyyy-yyyy-yyyy
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-socket` | `/var/run/tsstore/tsstore.sock` | ts-store Unix socket path |
| `-http` | (none) | ts-store HTTP URL (use instead of socket) |
| `-docker-socket` | `/var/run/docker.sock` | Docker daemon Unix socket |
| `-container-store` | `docker-containers` | Store name for per-container stats |
| `-daemon-store` | `docker-daemon` | Store name for daemon-wide stats |
| `-container-key` | (required) | API key for the container store |
| `-daemon-key` | (required) | API key for the daemon store |
| `-container-interval` | `20` | Container collection interval (seconds) |
| `-daemon-interval` | `120` | Daemon collection interval (seconds) |
| `-stdout` | `false` | Print to stdout instead of writing to ts-store |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `TSSTORE_CONTAINER_KEY` | Container-store API key (alternative to `-container-key`) |
| `TSSTORE_DAEMON_KEY` | Daemon-store API key (alternative to `-daemon-key`) |
| `TSSTORE_URL` | HTTP URL (alternative to `-http`) |

## Permissions

The collector must be able to read the Docker socket. Either run it as a user
in the `docker` group, or point `-docker-socket` at a socket you can reach.
Read access to `/var/run/docker.sock` is equivalent to root on the host —
run the collector as a trusted local service, not something exposed.

## Output Format

**docker-containers** — one line per container (dot-notation, schema-friendly):

```json
{
  "container": "web",
  "image": "nginx:latest",
  "state": "running",
  "cpu.pct": 12.4,
  "mem.used": 134217728,
  "mem.limit": 2147483648,
  "mem.pct": 6.25,
  "net.rx_bytes": 10240,
  "net.tx_bytes": 5120,
  "blkio.read_bytes": 0,
  "blkio.write_bytes": 4096,
  "pids": 8
}
```

**docker-daemon** — one line per tick:

```json
{
  "containers.running": 7,
  "containers.paused": 0,
  "containers.stopped": 2,
  "containers.total": 9,
  "images.total": 24,
  "df.layers_size": 3221225472,
  "df.images_size": 4294967296,
  "df.containers_size": 104857600,
  "df.volumes_size": 2147483648
}
```

`image` and `state` are tagged `omitempty`.

## Creating the Stores

Both are best created as **schema** stores (compact, ~50% smaller than JSON,
columnar querying). The container schema describes *a single container's*
metrics — the `container` field is what lets one store hold every container as
a separate series.

### docker-containers

```bash
# Create the store
curl -X POST http://localhost:21080/api/stores \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{"name": "docker-containers", "num_blocks": 20000, "data_type": "schema"}'

# Save the returned API key -> this is your -container-key

# Set the schema
curl -X PUT http://localhost:21080/api/stores/docker-containers/schema \
  -H "X-API-Key: your-container-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "fields": [
      {"index": 1,  "name": "container",         "type": "string"},
      {"index": 2,  "name": "image",             "type": "string"},
      {"index": 3,  "name": "state",             "type": "string"},
      {"index": 4,  "name": "cpu.pct",           "type": "float64"},
      {"index": 5,  "name": "mem.used",          "type": "int64"},
      {"index": 6,  "name": "mem.limit",         "type": "int64"},
      {"index": 7,  "name": "mem.pct",           "type": "float64"},
      {"index": 8,  "name": "net.rx_bytes",      "type": "int64"},
      {"index": 9,  "name": "net.tx_bytes",      "type": "int64"},
      {"index": 10, "name": "blkio.read_bytes",  "type": "int64"},
      {"index": 11, "name": "blkio.write_bytes", "type": "int64"},
      {"index": 12, "name": "pids",              "type": "int32"}
    ]
  }'
```

### docker-daemon

```bash
curl -X POST http://localhost:21080/api/stores \
  -H "X-Admin-Key: your-admin-key" \
  -H "Content-Type: application/json" \
  -d '{"name": "docker-daemon", "num_blocks": 2000, "data_type": "schema"}'

# Save the returned API key -> this is your -daemon-key

curl -X PUT http://localhost:21080/api/stores/docker-daemon/schema \
  -H "X-API-Key: your-daemon-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "fields": [
      {"index": 1, "name": "containers.running",  "type": "int32"},
      {"index": 2, "name": "containers.paused",   "type": "int32"},
      {"index": 3, "name": "containers.stopped",  "type": "int32"},
      {"index": 4, "name": "containers.total",    "type": "int32"},
      {"index": 5, "name": "images.total",        "type": "int32"},
      {"index": 6, "name": "df.layers_size",      "type": "int64"},
      {"index": 7, "name": "df.images_size",      "type": "int64"},
      {"index": 8, "name": "df.containers_size",  "type": "int64"},
      {"index": 9, "name": "df.volumes_size",     "type": "int64"}
    ]
  }'
```

> **Note on schemas and new containers:** adding or removing a container needs
> **no schema change** — a new container just shows up as new rows with a new
> `container` value. The schema only fixes the set of *metrics*, not the set of
> containers.

## Systemd Service

`/etc/systemd/system/docker-stats.service`:

```ini
[Unit]
Description=Docker Stats Collector for ts-store
After=tsstore.service docker.service
Requires=tsstore.service
Wants=docker.service

[Service]
Type=simple
User=youruser
SupplementaryGroups=docker
ExecStartPre=/bin/sleep 5
ExecStart=/home/youruser/bin/docker-stats -socket /var/run/tsstore/tsstore.sock
Restart=always
RestartSec=10
Environment=TSSTORE_CONTAINER_KEY=tsstore_xxxx-xxxx-xxxx
Environment=TSSTORE_DAEMON_KEY=tsstore_yyyy-yyyy-yyyy

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp docker-stats.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now docker-stats
```

## Overhead

Deliberately light. There are two costs — what the collector burns at runtime,
and what it stores.

### Runtime (CPU / daemon load)

- **Container tick.** N calls to `/containers/{id}/stats?stream=false`. The
  daemon takes two ~1s-apart CPU samples per call, so the collector goroutine
  **waits ~1s per container** — that's wall-clock, not CPU. Actual CPU is a few
  ms of JSON decode per container. Equivalent to running
  `docker stats --no-stream` once per interval.
- **Daemon tick.** One trivial `/info` plus one `/system/df`. `/system/df` is
  the only non-cheap call — it walks image layers, container writable layers,
  and volumes on disk (a few hundred ms to ~2s on image/volume-heavy hosts).
  It runs on the slow 120s tick precisely to keep this rare (30× less often
  than the container tick).
- **Footprint.** One small static binary, a few goroutines, RSS in single-digit
  MB. Connections are opened and closed per tick — nothing is held open. No
  background threads, no persistent daemon connections.

The one thing to size around: keep `N × ~1s` comfortably under the container
interval. At 20s that's fine up to ~13+ containers; past that, raise
`-container-interval`. (For **sub-second** monitoring you'd want a different,
streaming-based design — `stats` without `stream=false` pushes ~1 frame/sec on
a held-open connection. This collector samples instead, which is the right
shape for 20s+ intervals and avoids one held connection per container.)

On ts-store itself the write load — N rows per 20s — is a rounding error for a
store built for 100Hz+ ingestion.

### Storage

Schema-encoded container rows are **~100 bytes each** (~12 fields, two short
strings). Storage is dominated entirely by **container count × sample rate**:

| Containers | Rows/day (20s) | ~Bytes/day | ~Per month |
|---|---|---|---|
| 3  | ~13,000  | ~1.3 MB | ~39 MB  |
| 10 | ~43,000  | ~4.3 MB | ~130 MB |
| 25 | ~108,000 | ~11 MB  | ~325 MB |

The daemon store is trivial: 1 row / 120s = 720 rows/day ≈ ~90 KB/day
(~2.7 MB/month).

## Storage Calculations

`num_blocks` is a **fixed pre-allocation**, not growth — it caps how much
history a store holds, and the store overwrites oldest-first once full.

- Container store: N containers × (86400 / 20) = **N × 4,320 rows/day**.
  At ~100 bytes/row and ~30 rows per 4KB data block, `num_blocks: 20000`
  ≈ ~80 MB on disk, holding ~600,000 rows ≈ **~5–6 days for 10 containers**.
  Scale `num_blocks` linearly with your retention target and container count.
- Daemon store: one row per 120s = 720 rows/day; `num_blocks: 2000` holds
  weeks.

Bump `num_blocks` to match your retention needs.

## Querying

`filter` is a **substring match against the serialized record** (not a
key:value field selector). Because every container row carries
`"container":"web"`, filtering on `container":"web` isolates that series;
compute rates client-side from the raw counters.

```bash
# Latest readings for the "web" container
curl 'http://localhost:21080/api/stores/docker-containers/data/newest?filter=container%22%3A%22web%22' \
  -H "X-API-Key: your-container-api-key"

# Range for one container (compute net.rx rate as Δrx_bytes / Δt across rows)
curl 'http://localhost:21080/api/stores/docker-containers/data/range?since=6h&filter=container%22%3A%22web%22' \
  -H "X-API-Key: your-container-api-key"
```

> The filter is matched after fetch, so a filtered `/data/newest` defaults to
> a `1h` lookback window — pass `since=`/`window=0` to widen it. See
> [README-DATA_RETRIEVAL.md](../../README-DATA_RETRIEVAL.md) for the full
> filter, windowing, and aggregation syntax.

## Design Decisions

The choices behind how this collector is built, and the reasoning:

- **Two stores, not one.** Per-container metrics and daemon-wide totals have
  genuinely different shapes (one has a `container` field and many rows per
  tick; the other is a single host-wide row). Splitting them keeps each schema
  clean and lets them tick at different rates. Separating multiple Docker
  *hosts* is done by store **name** (`<host>-docker-containers`), not a field —
  clean blast-radius isolation and independent keys/retention per host.

- **One store per stream + a `container` field, not one column per container.**
  A ts-store schema is a fixed, positional field list. Encoding containers as
  columns (`web.cpu.pct`, `db.cpu.pct`, …) would force a schema change every
  time a container is added or removed. Instead each container is a **row**
  tagged with `container`, so new containers just appear as new rows — no schema
  change ever. This is the standard "one series per label value" time-series
  shape.

- **Server-assigned timestamps.** ts-store enforces strictly-increasing
  timestamps per store. A container tick writes N rows "at the same time"; by
  letting the server stamp them, the N rows get monotonic timestamps in write
  order and never trip the ordering check. (The collector therefore does **not**
  send a timestamp envelope.)

- **Raw cumulative counters for `net.*` / `blkio.*`, not per-second rates.**
  Storing the raw counters means a missed or delayed tick never corrupts a
  rate — you compute `Δbytes / Δt` at query time from whatever rows exist, and
  the underlying signal is never lost. (Contrast `system-stats`, which stores
  pre-computed rates; for per-container I/O the raw-counter approach is more
  robust.)

- **Polling `stats?stream=false`, not the streaming endpoint.** Docker's
  `stats` can hold a connection open and push ~1 frame/sec per container. That
  suits *sub-second* monitoring but is the wrong shape for periodic sampling:
  it means one held-open connection per container plus lifecycle bookkeeping as
  containers come and go, to then discard ~19 of every 20 frames at a 20s
  interval. Polling re-lists containers each tick (new ones appear, gone ones
  vanish, no bookkeeping) and matches the sampling model. The tradeoff is the
  ~1s-per-container wait; fine at 20s+ intervals (see [Overhead](#overhead)).

- **Daemon store on a slow 120s tick.** Host-wide counts and disk usage barely
  move, and `/system/df` is the one comparatively expensive call (it walks the
  filesystem). Running it 30× less often than the container tick keeps that cost
  rare.

- **Raw HTTP over the Docker socket, no Docker SDK.** Keeps the collector a
  single static binary with zero dependencies, matching `system-stats` and
  `journal-logs`. Only a handful of endpoints are needed
  (`/containers/json`, `/containers/{id}/stats`, `/info`, `/system/df`).

- **Local Docker socket, remote ts-store.** The collector must sit next to the
  Docker daemon (it reads the local `/var/run/docker.sock`), but writes to
  ts-store over HTTP so the store can be central and remote. Attaching to a
  remote Docker over TCP is deliberately **not** supported — an exposed daemon
  socket is root-equivalent access to the host. See
  [Deployment Topology](#deployment-topology).
