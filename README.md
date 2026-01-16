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
- **Free list** tracks reclaimed blocks for reuse

When the circular buffer is full, the oldest block is automatically reclaimed.

## Features

- **Configurable block sizes** - Separate power-of-2 sizes for data blocks and index blocks
- **Multiple stores per process** - Each store is fully independent
- **Range queries** - Efficiently find all blocks within a time range
- **Time-based or block-based reclaim** - Free specific ranges of data
- **Crash recovery** - Metadata is persisted after each operation
- **REST API server** - HTTP API with per-store API key authentication
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
  "index_block_size": 4096
}
```
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

#### Insert Data (requires auth)
```
POST /api/stores/:store/data
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": "base64-encoded-data"
}
```
Timestamp is optional (defaults to current time). Data must be base64 encoded.

#### Get Data by Timestamp (requires auth)
```
GET /api/stores/:store/data/time/:timestamp
X-API-Key: <api-key>
```

#### Query Time Range (requires auth)
```
GET /api/stores/:store/data/range?start_time=X&end_time=Y
X-API-Key: <api-key>
```

#### Get Oldest/Newest Timestamps (requires auth)
```
GET /api/stores/:store/data/oldest
GET /api/stores/:store/data/newest
X-API-Key: <api-key>
```

### Object API (High-Level)

The Object API provides a higher-level interface for storing objects. Small objects are packed together efficiently, and large objects automatically span multiple blocks.

#### Store Object (requires auth)
```
POST /api/stores/:store/objects
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": "base64-encoded-data"
}
```
Returns:
```json
{
  "timestamp": 1704067200000000000,
  "block_num": 5,
  "size": 1024
}
```

#### Get Object by Timestamp (requires auth)
```
GET /api/stores/:store/objects/time/:timestamp
X-API-Key: <api-key>
```

#### Delete Object by Timestamp (requires auth)
```
DELETE /api/stores/:store/objects/time/:timestamp
X-API-Key: <api-key>
```

#### List Oldest Objects (requires auth)
```
GET /api/stores/:store/objects/oldest?limit=10
X-API-Key: <api-key>
```
Returns handles for the N oldest objects (default 10). Does not include data.

#### List Newest Objects (requires auth)
```
GET /api/stores/:store/objects/newest?limit=10
GET /api/stores/:store/objects/newest?since=2h&limit=100
X-API-Key: <api-key>
```
Returns handles for the N newest objects (default 10). Use `since` parameter for relative time queries.

#### List Objects in Time Range (requires auth)
```
GET /api/stores/:store/objects/range?start_time=X&end_time=Y&limit=100
GET /api/stores/:store/objects/range?since=24h&limit=100
X-API-Key: <api-key>
```
Returns handles for objects within the time range. Use `since` as an alternative to `start_time`/`end_time`.

**Supported duration formats:**
- `30s` - 30 seconds
- `15m` - 15 minutes
- `2h` - 2 hours
- `7d` - 7 days
- `1w` - 1 week

### JSON API (No Base64 Encoding)

The JSON API provides a convenient interface for storing and retrieving JSON objects without base64 encoding.

#### Store JSON Object (requires auth)
```
POST /api/stores/:store/json
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```
Timestamp is optional (defaults to current time). Data is stored as-is (no base64).

#### Get JSON by Timestamp (requires auth)
```
GET /api/stores/:store/json/time/:timestamp
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

#### List Oldest JSON Objects (requires auth)
```
GET /api/stores/:store/json/oldest?limit=10
X-API-Key: <api-key>
```
Returns the N oldest JSON objects with their data.

#### List Newest JSON Objects (requires auth)
```
GET /api/stores/:store/json/newest?limit=10
GET /api/stores/:store/json/newest?since=30m&limit=50
X-API-Key: <api-key>
```
Returns the N newest JSON objects with their data. Use `since` for relative time queries.

#### List JSON Objects in Time Range (requires auth)
```
GET /api/stores/:store/json/range?start_time=X&end_time=Y&limit=100
GET /api/stores/:store/json/range?since=1h&limit=100
X-API-Key: <api-key>
```
Returns JSON objects within the time range. Use `since` as an alternative to `start_time`/`end_time`.

### CLI Store Management

Create stores from the command line:

```bash
# Create a store with defaults (1024 blocks, 4KB data/index)
./tsstore create my-store

# Create with custom settings
./tsstore create sensors --blocks 10000 --data-size 8192

# Create in a specific directory
./tsstore create logs --path /var/tsstore
```

Options:
- `--blocks <n>` - Number of blocks (default: 1024)
- `--data-size <n>` - Data block size in bytes, must be power of 2 (default: 4096)
- `--index-size <n>` - Index block size in bytes, must be power of 2 (default: 4096)
- `--path <dir>` - Base directory for stores (default: ./data or TSSTORE_DATA_PATH)

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
// Reclaim by block number
s.Reclaim(blockNum)

// Reclaim a range of blocks
s.AddRangeToFreeList(startBlock, endBlock)

// Reclaim by time range (finds closest matches)
s.AddRangeToFreeListByTime(startTime, endTime)
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
fmt.Printf("Blocks: %d, Head: %d, Tail: %d, Free: %d\n",
    stats.NumBlocks,
    stats.HeadBlock,
    stats.TailBlock,
    stats.FreeListCount,
)

oldest, _ := s.GetOldestTimestamp()
newest, _ := s.GetNewestTimestamp()
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

This software is licensed under the PolyForm Noncommercial License 1.0.0.
You may use this software for non-commercial purposes only. For commercial
licensing, please contact TRV Enterprises LLC.

See [LICENSE](LICENSE) for full details.
