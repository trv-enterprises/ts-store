# ts-store

A time series database using a circular file architecture, inspired by work from 1978. Designed for fixed-size storage with predictable memory and disk usage.

## Overview

ts-store implements a circular buffer-based storage system optimized for time series data. The design ensures:

- **Fixed storage footprint** - Total size is determined at creation time
- **Automatic reclamation** - Oldest data is automatically reclaimed when space is needed
- **O(log n) time lookups** - Binary search on sorted timestamps
- **Multi-object packing** - Multiple small objects can share a single block for efficiency
- **Large object spanning** - Objects larger than a block automatically span multiple blocks

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Circular Data Blocks                      │
│  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐      │
│  │  0  │──│  1  │──│  2  │──│  3  │──│  4  │──│  5  │──... │
│  │     │  │     │  │     │  │     │  │     │  │     │      │
│  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘      │
│     ↑                                   ↑                    │
│    tail                               head                   │
│  (oldest)                           (newest)                 │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                    Circular Index                            │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ [ts₀, blk₀] [ts₁, blk₁] [ts₂, blk₂] ... [tsₙ, blkₙ]  │ │
│  └────────────────────────────────────────────────────────┘ │
│                    Binary search for O(log n) lookups        │
└─────────────────────────────────────────────────────────────┘
```

**Key concepts:**

- **Data blocks** form a fixed-size circular buffer ordered by time
- **Index** mirrors the circular structure, enabling binary search by timestamp
- **Head/Tail pointers** track newest and oldest data; free space is implicit

When the circular buffer is full, the oldest block (at tail) is automatically reclaimed to make room for new data.

## Features

- **Configurable block sizes** - Separate power-of-2 sizes for data blocks and index blocks
- **Multiple stores per process** - Each store is fully independent
- **Range queries** - Efficiently find all blocks within a time range
- **Time-based or block-based reclaim** - Free specific ranges of data
- **Crash recovery** - Metadata is persisted after each operation
- **REST API server** - HTTP API with per-store API key authentication
- **WebSocket streaming** - Real-time data streaming with inbound and outbound modes
- **Edge-friendly** - Small footprint, no external database dependencies
- **Flexible object sizes** - Small objects pack together, large objects span multiple blocks

## Installation

```bash
go get github.com/tviviano/ts-store
```

### Build the Server

```bash
go build -o tsstore ./cmd/tsstore
```

### Docker

Build and run with Docker:

```bash
# Build the image
docker build -t tsstore .

# Run the container
docker run -d -v tsstore-data:/data -p 8080:8080 --name tsstore tsstore
```

Or use Docker Compose:

```bash
docker compose up -d
```

**Managing stores in Docker:**

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

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `TSSTORE_DATA_PATH` | `/data` | Base path for stores |
| `TSSTORE_HOST` | `0.0.0.0` | Server bind address |
| `TSSTORE_PORT` | `8080` | Server port |
| `TSSTORE_MODE` | `release` | Gin mode (debug/release) |

## REST API Server

ts-store includes a lightweight REST API server designed for edge devices.

### Starting the Server

```bash
./tsstore serve
```

The server reads configuration from `config.json` (or `TSSTORE_CONFIG` env var).

### Configuration

Create `config.json`:

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 8080,
    "mode": "release"
  },
  "store": {
    "base_path": "./data",
    "data_block_size": 4096,
    "index_block_size": 4096,
    "num_blocks": 1024
  }
}
```

Environment variables:
- `TSSTORE_HOST` - Server host
- `TSSTORE_PORT` - Server port
- `TSSTORE_MODE` - "debug" or "release"
- `TSSTORE_DATA_PATH` - Base path for stores
- `TSSTORE_CONFIG` - Config file path

### Authentication

Each store has its own API key. The key is generated when the store is created and shown only once. Store it securely.

Pass the API key via:
- Header: `X-API-Key: tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
- Query param: `?api_key=tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`

### API Endpoints

#### Health Check
```
GET /health
```
Returns server health status. No authentication required.

#### Create Store
```
POST /api/stores
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

Returns the API key (shown only once):
```json
{
  "name": "my-store",
  "api_key": "tsstore_a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "key_id": "a1b2c3d4"
}
```

#### List Open Stores
```
GET /api/stores
```

#### Delete Store (requires auth)
```
DELETE /api/stores/:store
X-API-Key: <api-key>
```

#### Get Store Stats (requires auth)
```
GET /api/stores/:store/stats
X-API-Key: <api-key>
```

### Data Endpoint

The unified `/data` endpoint handles all data operations. Content-Type header must match the store's data type.

#### Insert Data (requires auth)
```
POST /api/stores/:store/data
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```
Timestamp is optional (defaults to current time).

Returns:
```json
{
  "timestamp": 1704067200000000000,
  "block_num": 5,
  "size": 64
}
```

#### Get Data by Timestamp (requires auth)
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

#### Delete Data by Timestamp (requires auth)
```
DELETE /api/stores/:store/data/time/:timestamp
X-API-Key: <api-key>
```

#### List Oldest Data (requires auth)
```
GET /api/stores/:store/data/oldest?limit=10&include_data=true
X-API-Key: <api-key>
```
Returns handles for the N oldest objects (default 10). Add `include_data=true` to include data in response.

#### List Newest Data (requires auth)
```
GET /api/stores/:store/data/newest?limit=10
GET /api/stores/:store/data/newest?since=2h&limit=100&include_data=true
X-API-Key: <api-key>
```
Returns handles for the N newest objects (default 10). Use `since` parameter for relative time queries.

#### Query Time Range (requires auth)
```
GET /api/stores/:store/data/range?start_time=X&end_time=Y&limit=100
GET /api/stores/:store/data/range?since=24h&limit=100&include_data=true
X-API-Key: <api-key>
```
Returns objects within the time range. Use `since` as an alternative to `start_time`/`end_time`.

**Supported duration formats:**
- `30s` - 30 seconds
- `15m` - 15 minutes
- `2h` - 2 hours
- `7d` - 7 days
- `1w` - 1 week

#### Filtering Results

All list endpoints (`/data/oldest`, `/data/newest`, `/data/range`) support substring filtering:

```
GET /api/stores/:store/data/newest?filter=sensor:01&include_data=true
GET /api/stores/:store/data/range?since=1h&filter=BUILDING+A&filter_ignore_case=true
```

**Filter parameters:**
- `filter` - Substring to match in the object data
- `filter_ignore_case` - Set to `true` for case-insensitive matching (default: `false`)

Only objects containing the filter substring are returned. When filtering is active, all objects are scanned to find matches up to the specified limit.

### Schema Endpoint (for schema-type stores)

Schema stores use a compact JSON format where field names are replaced with numeric indices. This reduces storage space significantly for structured data with known schemas.

#### Get Current Schema
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

#### Set/Update Schema
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

When retrieving data, the compact format is automatically expanded to full field names (default) or returned in compact format with `?format=compact`.

### WebSocket Endpoints

ts-store supports real-time data streaming via WebSocket connections.

#### Inbound Read Stream
```
GET /api/stores/:store/ws/read?api_key=<key>&from=now&format=full
GET /api/stores/:store/ws/read?api_key=<key>&from=0&filter=sensor:01
```

Query parameters:
- `api_key` - Required for authentication
- `from` - Start point: Unix nanosecond timestamp or `now` (default: `now`)
- `format` - For schema stores: `compact` or `full` (default: `full`)
- `filter` - Substring to match in data (optional)
- `filter_ignore_case` - `true` for case-insensitive matching (default: `false`)

Server sends messages:
```json
{"type": "data", "timestamp": 1234567890, "block_num": 5, "size": 64, "data": {...}}
{"type": "caught_up"}
{"type": "error", "message": "..."}
```

#### Inbound Write Stream
```
GET /api/stores/:store/ws/write?api_key=<key>&format=full
```

Query parameters:
- `api_key` - Required for authentication
- `format` - For schema stores: `compact` or `full` (default: `full`)

Client sends:
```json
{"timestamp": 1234567890, "data": {...}}
```

Server responds:
```json
{"type": "ack", "timestamp": 1234567890, "block_num": 5, "size": 64}
{"type": "error", "message": "..."}
```

#### Outbound Connection Management

Create outbound connections where ts-store connects to remote servers.

**List Connections:**
```
GET /api/stores/:store/ws/connections
X-API-Key: <api-key>
```

**Create Connection:**
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
- `mode` - `push` (ts-store sends to remote) or `pull` (ts-store receives from remote)
- `url` - WebSocket URL to connect to
- `from` - Start timestamp for push mode (0 = from beginning)
- `format` - `compact` or `full` for schema stores
- `headers` - Custom HTTP headers for connection
- `filter` - Substring to match in data (push mode only)
- `filter_ignore_case` - `true` for case-insensitive matching

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
  "errors": 0
}
```

**Delete Connection:**
```
DELETE /api/stores/:store/ws/connections/:id
X-API-Key: <api-key>
```

Outbound connections automatically reconnect with exponential backoff (1s to 60s max) and resume from the last sent timestamp.

### CLI Store Management

Create stores from the command line:

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

Options:
- `--blocks <n>` - Number of blocks (default: 1024)
- `--data-size <n>` - Data block size in bytes, must be power of 2 (default: 4096)
- `--index-size <n>` - Index block size in bytes, must be power of 2 (default: 4096)
- `--path <dir>` - Base directory for stores (default: ./data or TSSTORE_DATA_PATH)
- `--type <type>` - Data type: binary, text, json, schema (default: json)

### API Key Management

API keys can only be managed via CLI (requires device access):

```bash
# Regenerate key (revokes all existing keys)
./tsstore key regenerate my-store

# List keys (shows IDs only, not actual keys)
./tsstore key list my-store

# Revoke a specific key
./tsstore key revoke my-store a1b2c3d4
```

## Go Library

The store can also be used directly as a Go library without the API server.

### Creating a Store

```go
import "github.com/tviviano/ts-store/pkg/store"

// Use defaults: 1024 blocks, 4KB data blocks, 4KB index blocks
cfg := store.DefaultConfig()
cfg.Name = "sensor-data"
cfg.Path = "/var/data"

// Or customize
cfg := store.Config{
    Name:           "sensor-data",
    Path:           "/var/data",
    NumBlocks:      10000,    // 10K blocks
    DataBlockSize:  8192,     // 8KB data blocks
    IndexBlockSize: 4096,     // 4KB index blocks
}

s, err := store.Create(cfg)
if err != nil {
    log.Fatal(err)
}
defer s.Close()

// Small objects are packed together, large objects span multiple blocks
// No practical size limit (within available blocks)
```

### Inserting Data

```go
// Insert with specific timestamp (Unix nanoseconds)
timestamp := time.Now().UnixNano()
data := []byte(`{"temp": 72.5, "humidity": 45}`)

blockNum, err := s.Insert(timestamp, data)

// Or use current time automatically
blockNum, err := s.InsertNow(data)
```

### Finding Data

```go
// Find by exact timestamp
blockNum, err := s.FindBlockByTimeExact(timestamp)
if err == store.ErrTimestampNotFound {
    // No exact match
}

// Find closest block to a timestamp
blockNum, err := s.FindBlockByTime(timestamp)

// Find all blocks in a time range
startTime := time.Now().Add(-1 * time.Hour).UnixNano()
endTime := time.Now().UnixNano()

blocks, err := s.FindBlocksInRange(startTime, endTime)
for _, blockNum := range blocks {
    data, _ := s.ReadBlockData(blockNum)
    // process data...
}
```

### Reclaiming Space

```go
// Reclaim by block number (clears index entry, tail advances when it reaches this block)
s.Reclaim(blockNum)

// Reclaim blocks up to a specific block number
s.ReclaimUpTo(targetBlock)

// Reclaim by time range (finds closest matches)
s.ReclaimByTimeRange(startTime, endTime)
```

### Object API (High-Level)

The Object API provides a convenient interface for storing objects:

```go
// Store an object (any size - small objects pack, large objects span)
handle, err := s.PutObject(timestamp, data)
handle, err := s.PutObjectNow(data) // Use current time

// Retrieve an object
data, err := s.GetObject(handle)
data, handle, err := s.GetObjectByTime(timestamp)

// List objects (returns handles only, not data)
handles, err := s.GetOldestObjects(10)  // First 10 (from tail)
handles, err := s.GetNewestObjects(10)  // Last 10 (from head)
handles, err := s.GetObjectsInRange(startTime, endTime, limit)
handles, err := s.GetObjectsSince(2*time.Hour, limit)  // Last 2 hours

// Delete an object
err := s.DeleteObject(handle)
err := s.DeleteObjectByTime(timestamp)
```

The ObjectHandle contains metadata about the stored object:
```go
type ObjectHandle struct {
    Timestamp int64  // When the object was stored
    BlockNum  uint32 // Starting block number
    Offset    uint32 // Position within block (for packed objects)
    Size      uint32 // Size in bytes
    SpanCount uint32 // Number of blocks (1 for single, >1 for spanning)
}
```

### JSON API (Go Library)

For convenient JSON storage without manual marshaling:

```go
// Store JSON objects directly
type SensorReading struct {
    Temperature float64 `json:"temperature"`
    Humidity    float64 `json:"humidity"`
    Sensor      string  `json:"sensor"`
}

reading := SensorReading{Temperature: 72.5, Humidity: 45, Sensor: "living-room"}
handle, err := s.PutJSON(timestamp, reading)
handle, err := s.PutJSONNow(reading)  // Use current time

// Retrieve and unmarshal
var result SensorReading
handle, err := s.GetJSONByTime(timestamp, &result)

// Get raw JSON (when structure is unknown)
raw, err := s.GetJSONRaw(handle)  // Returns json.RawMessage
raw, handle, err := s.GetJSONRawByTime(timestamp)

// List JSON objects (returns raw JSON with handles)
rawMsgs, handles, err := s.GetOldestJSON(10)
rawMsgs, handles, err := s.GetNewestJSON(10)
rawMsgs, handles, err := s.GetJSONSince(time.Hour, limit)  // Last hour
rawMsgs, handles, err := s.GetJSONInRange(startTime, endTime, limit)
```

### Opening an Existing Store

```go
s, err := store.Open("/var/data", "sensor-data")
if err != nil {
    log.Fatal(err)
}
defer s.Close()
```

### Deleting a Store

```go
// Delete an open store
s.Delete()

// Or delete by path without opening
store.DeleteStore("/var/data", "sensor-data")
```

### Store Statistics

```go
stats := s.Stats()
fmt.Printf("Blocks: %d, Head: %d, Tail: %d, Active: %d\n",
    stats.NumBlocks,
    stats.HeadBlock,
    stats.TailBlock,
    stats.ActiveBlocks,
)

oldest, _ := s.GetOldestTimestamp()
newest, _ := s.GetNewestTimestamp()
```

## Testing WebSocket Connections

Use `websocat` or similar tools to test WebSocket endpoints:

```bash
# Install websocat
brew install websocat  # macOS
# or: cargo install websocat

# Test inbound read (stream data from store)
websocat "ws://localhost:8080/api/stores/my-store/ws/read?api_key=KEY&from=0"

# Test inbound write (send data to store)
websocat "ws://localhost:8080/api/stores/my-store/ws/write?api_key=KEY"
# Then type: {"data": {"temp": 72.5}}

# Test outbound push (start a test server first)
websocat -s 9000  # Start test server on port 9000

# Create outbound push connection
curl -X POST localhost:8080/api/stores/my-store/ws/connections \
  -H "X-API-Key: KEY" \
  -H "Content-Type: application/json" \
  -d '{"mode": "push", "url": "ws://localhost:9000", "from": 0}'
```

## Performance Characteristics

| Operation | Complexity | Notes |
|-----------|------------|-------|
| Insert | O(1) | Appends to head of circle |
| Find by time | O(log n) | Binary search, ~20 reads max for 1M entries |
| Range query | O(log n + k) | k = number of results |
| Reclaim | O(1) | Just updates metadata |

**Disk I/O for 1 million entries:**
- Single lookup: ~7-10 block reads average (1.4ms on NVMe SSD)
- Sequential scan: Optimal due to circular layout

## File Format

Each store creates a directory with the following files:

```
sensor-data/
├── data.tsdb    # Data blocks
├── index.tsdb   # Time index entries
├── meta.tsdb    # Store metadata (64 bytes)
└── keys.json    # API key hashes (only when using API server)
```

**Block sizes must be powers of 2** (64, 128, 256, 512, 1024, 2048, 4096, 8192, etc.)

## Thread Safety

All Store methods are thread-safe. The implementation uses read-write locks to allow concurrent reads while serializing writes.

## Important: Timestamp Ordering

**ts-store requires strictly increasing timestamps.** The data store depends on monotonically increasing timestamps for its binary search index and efficient range queries. Out-of-order timestamps will be rejected with `ErrTimestampOutOfOrder`.

This constraint has important implications:

1. **User-provided timestamps** must always be greater than the most recent entry
2. **System clock resets** can cause problems - if the system time is reset to an earlier time (e.g., NTP correction, daylight saving time issues, or manual adjustment) and you rely on `PutObjectNow()`, new inserts will be rejected until the clock advances past the last stored timestamp
3. **Distributed systems** must coordinate timestamps if multiple writers are possible

**Recommendations:**
- Use logical timestamps or sequence numbers if clock stability is a concern
- Monitor for `ErrTimestampOutOfOrder` errors in production
- Consider using `Reset()` to clear the store if clock issues corrupt the timeline

## Reset Store

If you need to clear all data from a store (e.g., after clock issues or for testing), use the Reset function:

```go
// Reset clears all data and reinitializes the store
err := s.Reset()
```

This clears all blocks and resets head/tail pointers, allowing inserts to start fresh.

## Limitations

- **Timestamps must be strictly increasing** (see above)
- Timestamps must be positive (Unix nanoseconds)
- Block sizes must be powers of 2, minimum 64 bytes

## License

Copyright (c) 2026 TRV Enterprises LLC

This software is licensed under the [Business Source License 1.1](LICENSE).

**What you CAN do:**
- Use ts-store internally in your business (manufacturing, monitoring, etc.)
- Modify and create derivative works
- Use for development, testing, and non-production purposes

**What you CANNOT do:**
- Offer ts-store to third parties on a hosted, managed, or embedded basis
- Include ts-store in a product or SaaS offering you sell

**Open Source Conversion:**
On January 25, 2028 (or 4 years after any specific version's release), the code becomes available under the Apache License 2.0.

For commercial licensing inquiries, please contact TRV Enterprises LLC.
