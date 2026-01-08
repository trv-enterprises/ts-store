# ts-store

A time series database using a circular file architecture, inspired by work from 1978. Designed for fixed-size storage with predictable memory and disk usage.

## Overview

ts-store implements a circular buffer-based storage system optimized for time series data. The design ensures:

- **Fixed storage footprint** - Total size is determined at creation time
- **Automatic reclamation** - Oldest data is automatically reclaimed when space is needed
- **O(log n) time lookups** - Binary search on sorted timestamps
- **Overflow support** - Attached blocks allow entries to exceed a single block

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Circular Primary Blocks                   │
│  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐      │
│  │  0  │──│  1  │──│  2  │──│  3  │──│  4  │──│  5  │──... │
│  │     │  │     │  │     │  │     │  │     │  │     │      │
│  └──┬──┘  └─────┘  └──┬──┘  └─────┘  └─────┘  └─────┘      │
│     │                 │                                      │
│     ▼                 ▼                                      │
│  ┌─────┐           ┌─────┐                                  │
│  │ A.1 │           │ A.1 │  Attached Blocks (overflow)      │
│  └──┬──┘           └──┬──┘                                  │
│     ▼                 ▼                                      │
│  ┌─────┐           ┌─────┐                                  │
│  │ A.2 │           │ A.2 │                                  │
│  └─────┘           └─────┘                                  │
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

- **Primary blocks** form a fixed-size circular buffer ordered by time
- **Attached blocks** provide overflow capacity, linked to their primary block
- **Index** mirrors the circular structure, enabling binary search by timestamp
- **Free list** tracks reclaimed blocks for reuse

When the circular buffer is full, the oldest primary block (and its attached blocks) is automatically reclaimed.

## Features

- **Configurable block sizes** - Separate power-of-2 sizes for data blocks and index blocks
- **Multiple stores per process** - Each store is fully independent
- **Attached overflow blocks** - Data larger than one block can span multiple attached blocks
- **Range queries** - Efficiently find all blocks within a time range
- **Time-based or block-based reclaim** - Free specific ranges of data
- **Crash recovery** - Metadata is persisted after each operation
- **REST API server** - HTTP API with per-store API key authentication
- **Edge-friendly** - Small footprint, no external database dependencies

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

#### Get Data by Block Number (requires auth)
```
GET /api/stores/:store/data/block/:blocknum
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

#### Attach Block (requires auth)
```
POST /api/stores/:store/data/block/:blocknum/attach
X-API-Key: <api-key>
Content-Type: application/json

{
  "data": "base64-encoded-data"
}
```

#### Get Attached Blocks (requires auth)
```
GET /api/stores/:store/data/block/:blocknum/attached
X-API-Key: <api-key>
```

#### Reclaim Blocks (requires auth)
```
POST /api/stores/:store/data/reclaim
X-API-Key: <api-key>
Content-Type: application/json

{
  "start_block": 0,
  "end_block": 10
}
```
Or by time range:
```json
{
  "start_time": 1704067200000000000,
  "end_time": 1704153600000000000
}
```

### Object API (High-Level)

The Object API provides a higher-level interface for storing data that may span multiple blocks. Objects are automatically split across blocks on write and reassembled on read.

#### Store Object (requires auth)
```
POST /api/stores/:store/objects
X-API-Key: <api-key>
Content-Type: application/json

{
  "timestamp": 1704067200000000000,
  "data": "base64-encoded-data-of-any-size"
}
```
Returns:
```json
{
  "timestamp": 1704067200000000000,
  "primary_block_num": 5,
  "total_size": 50000,
  "block_count": 13
}
```

#### Get Object by Timestamp (requires auth)
```
GET /api/stores/:store/objects/time/:timestamp
X-API-Key: <api-key>
```
Returns the full object data (reassembled from all blocks).

#### Get Object by Block Number (requires auth)
```
GET /api/stores/:store/objects/block/:blocknum
X-API-Key: <api-key>
```

#### Delete Object by Timestamp (requires auth)
```
DELETE /api/stores/:store/objects/time/:timestamp
X-API-Key: <api-key>
```
Deletes the object and all its associated blocks.

#### Delete Object by Block Number (requires auth)
```
DELETE /api/stores/:store/objects/block/:blocknum
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
X-API-Key: <api-key>
```
Returns handles for the N newest objects (default 10).

#### List Objects in Time Range (requires auth)
```
GET /api/stores/:store/objects/range?start_time=X&end_time=Y&limit=100
X-API-Key: <api-key>
```
Returns handles for objects within the time range.

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
  "primary_block_num": 5,
  "total_size": 64,
  "block_count": 1,
  "data": {"temperature": 72.5, "humidity": 45, "sensor": "living-room"}
}
```

#### Get JSON by Block Number (requires auth)
```
GET /api/stores/:store/json/block/:blocknum
X-API-Key: <api-key>
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
X-API-Key: <api-key>
```
Returns the N newest JSON objects with their data.

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
- `--blocks <n>` - Number of primary blocks (default: 1024)
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
    NumBlocks:      10000,    // 10K primary blocks
    DataBlockSize:  8192,     // 8KB data blocks
    IndexBlockSize: 4096,     // 4KB index blocks
}

s, err := store.Create(cfg)
if err != nil {
    log.Fatal(err)
}
defer s.Close()
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

### Attached Blocks (Overflow)

When data exceeds a single block, use attached blocks:

```go
// Insert primary block
primaryBlock, _ := s.Insert(timestamp, initialData)

// Attach overflow blocks
attached1, _ := s.AttachBlock(primaryBlock)
s.WriteBlockData(attached1, moreData)

attached2, _ := s.AttachBlock(primaryBlock)
s.WriteBlockData(attached2, evenMoreData)

// Read all attached blocks
attachedBlocks, _ := s.GetAttachedBlocks(primaryBlock)
for _, blk := range attachedBlocks {
    data, _ := s.ReadBlockData(blk)
    // process overflow data...
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

For data that may exceed a single block, use the Object API which automatically handles splitting and reassembly:

```go
// Store an object (any size, automatically split across blocks)
handle, err := s.PutObject(timestamp, largeData)
handle, err := s.PutObjectNow(largeData) // Use current time

// Retrieve an object (automatically reassembled)
data, err := s.GetObject(handle)
data, handle, err := s.GetObjectByTime(timestamp)
data, handle, err := s.GetObjectByBlock(blockNum)

// List objects (returns handles only, not data)
handles, err := s.GetOldestObjects(10)  // First 10 (from tail)
handles, err := s.GetNewestObjects(10)  // Last 10 (from head)
handles, err := s.GetObjectsInRange(startTime, endTime, limit)

// Delete an object and all its blocks
err := s.DeleteObject(handle)
err := s.DeleteObjectByTime(timestamp)
```

The ObjectHandle contains metadata about the stored object:
```go
type ObjectHandle struct {
    Timestamp       int64  // When the object was stored
    PrimaryBlockNum uint32 // First block of the object
    TotalSize       uint32 // Total size in bytes
    BlockCount      uint32 // Number of blocks used
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
handle, err := s.GetJSONByBlock(blockNum, &result)

// Get raw JSON (when structure is unknown)
raw, err := s.GetJSONRaw(handle)  // Returns json.RawMessage
raw, handle, err := s.GetJSONRawByTime(timestamp)
raw, handle, err := s.GetJSONRawByBlock(blockNum)

// List JSON objects (returns raw JSON with handles)
rawMsgs, handles, err := s.GetOldestJSON(10)
rawMsgs, handles, err := s.GetNewestJSON(10)
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
fmt.Printf("Blocks: %d, Head: %d, Tail: %d, Free: %d, Attached: %d\n",
    stats.NumBlocks,
    stats.HeadBlock,
    stats.TailBlock,
    stats.FreeListCount,
    stats.TotalAttached,
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
| Attach block | O(1) | Links to end of chain |
| Reclaim | O(m) | m = attached blocks to free |

**Disk I/O for 1 million entries:**
- Single lookup: ~7-10 block reads average (1.4ms on NVMe SSD)
- Sequential scan: Optimal due to circular layout

## File Format

Each store creates a directory with the following files:

```
sensor-data/
├── data.tsdb    # Data blocks (primary + attached)
├── index.tsdb   # Time index entries
├── meta.tsdb    # Store metadata (64 bytes)
└── keys.json    # API key hashes (only when using API server)
```

**Block sizes must be powers of 2** (64, 128, 256, 512, 1024, 2048, 4096, 8192, etc.)

## Thread Safety

All Store methods are thread-safe. The implementation uses read-write locks to allow concurrent reads while serializing writes.

## Limitations

- Timestamps must be positive (Unix nanoseconds)
- Block sizes must be powers of 2, minimum 64 bytes
- Data per block is limited to `BlockSize - 40` bytes (40-byte header)
- Attached block pool equals primary block count (configurable in future)

## License

Copyright (c) 2026 TRV Enterprises LLC

This software is licensed under the PolyForm Noncommercial License 1.0.0.
You may use this software for non-commercial purposes only. For commercial
licensing, please contact TRV Enterprises LLC.

See [LICENSE](LICENSE) for full details.
