# ts-store

A lightweight, embedded time series database with a fixed storage footprint. Built for edge devices and IoT applications where you need file-based persistence without database infrastructure.

## Why ts-store

Most time series databases fall into two camps: lightweight tools that aggregate your data (losing the raw readings), or full database engines that grow unbounded and require significant infrastructure.

ts-store takes a different approach:

- **Fixed storage footprint** - Total size is determined at creation time. No unbounded growth, no retention policies to tune.
- **Raw data preservation** - No lossy downsampling. Every sensor reading is stored exactly as received.
- **Circular buffer architecture** - When storage is full, the oldest data is automatically overwritten.
- **Zero external dependencies** - A single binary and flat files on disk.
- **O(log n) time lookups** - Binary search on sorted timestamps for fast range queries.

## Quick Start

```bash
# Build
go build -o tsstore ./cmd/tsstore

# Create a store
./tsstore create my-sensors

# Start the server
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
./tsstore serve
```

Insert data:
```bash
curl -X POST "http://localhost:21080/api/stores/my-sensors/data" \
  -H "X-API-Key: <your-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"data": {"temperature": 72.5, "humidity": 45}}'
```

Query data:
```bash
curl "http://localhost:21080/api/stores/my-sensors/data/newest?limit=10" \
  -H "X-API-Key: <your-api-key>"
```

## Architecture

ts-store implements a circular buffer-based storage system optimized for time series data.

```
┌────────────────────────────────────────────────────────────┐
│                    Circular Data Blocks                    │
│  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐      │
│  │  0  │──│  1  │──│  2  │──│  3  │──│  4  │──│  5  │──... │
│  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘      │
│     ↑                                   ↑                  │
│    tail                               head                 │
│  (oldest)                           (newest)               │
└────────────────────────────────────────────────────────────┘
```

- **Data blocks** form a fixed-size circular buffer ordered by time
- **Index** enables binary search by timestamp for O(log n) lookups
- **Head/Tail pointers** track newest and oldest data

When the buffer is full, the oldest block is automatically reclaimed.

## Features

| Feature | Description |
|---------|-------------|
| **Data Input** | [REST API](README-DATA-INPUT.md#rest-api-insert-data), [WebSocket streaming](README-DATA-INPUT.md#websocket-streaming-write), [Unix socket](README-DATA-INPUT.md#unix-socket-low-latency-local-ingestion), [Outbound pull](README-DATA-INPUT.md#outbound-pull-receive-from-remote-server) |
| **Data Output** | [REST queries](README-DATA-OUTPUT.md#rest-api-query-endpoints), [Outbound push](README-DATA-OUTPUT.md#outbound-push-websocket-to-remote-server), [MQTT sink](README-DATA-OUTPUT.md#mqtt-sink-publish-to-broker) |
| **Alerting** | [Rule-based alerts](README-DATA-OUTPUT.md#alerting) with webhook notifications and cooldown |
| **Aggregation** | [Time-windowed aggregation](README-DATA-OUTPUT.md#aggregation) with multi-function support |
| **Schema stores** | [Compact JSON](README-API.md#schema-configuration-for-schema-type-stores) with field name compression |
| **Go library** | [Direct embedding](README-LIBRARY.md) without the API server |

## Data Types

- `binary` - Raw binary data
- `text` - UTF-8 text
- `json` - Arbitrary JSON objects (default)
- `schema` - [Schema-defined compact JSON](README-API.md#schema-configuration-for-schema-type-stores)

## Performance

| Operation | Complexity | Notes |
|-----------|------------|-------|
| Insert | O(1) | Appends to head of circle |
| Find by time | O(log n) | Binary search, ~20 reads max for 1M entries |
| Range query | O(log n + k) | k = number of results |
| Reclaim | O(1) | Just updates metadata |

## Important: Timestamp Ordering

**ts-store requires strictly increasing timestamps.** Out-of-order timestamps will be rejected with `ErrTimestampOutOfOrder`.

Recommendations:
- Use logical timestamps or sequence numbers if clock stability is a concern
- Monitor for `ErrTimestampOutOfOrder` errors in production
- Use `Reset()` to clear the store if clock issues corrupt the timeline

## File Format

Each store creates a directory:

```
sensor-data/
├── data.tsdb    # Data blocks
├── index.tsdb   # Time index entries
├── meta.tsdb    # Store metadata (64 bytes)
└── keys.json    # API key hashes
```

Block sizes must be powers of 2 (64, 128, 256, 512, 1024, 2048, 4096, 8192, etc.)

## Documentation

| Document | Description |
|----------|-------------|
| [API Reference](README-API.md) | REST API, authentication, configuration, schema stores |
| [Data Input](README-DATA-INPUT.md) | REST, WebSocket, Unix socket, outbound pull |
| [Data Output](README-DATA-OUTPUT.md) | Queries, outbound push, MQTT, alerting, aggregation |
| [CLI Reference](README-CLI.md) | Command-line tools, Docker, key management |
| [Go Library](README-LIBRARY.md) | Embedding ts-store as a library |
| [Outbound WebSocket](docs/outbound-data-ws.md) | Detailed outbound push documentation |
| [Alerting Architecture](docs/alerting-architecture.md) | Alerting system design |

Additional diagrams and architecture notes are in `./docs`.

## License

Copyright (c) 2026 TRV Enterprises LLC

Licensed under the [Apache License, Version 2.0](LICENSE).
