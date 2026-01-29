# ts-store

A lightweight, embedded time series database with a fixed storage footprint. Built for edge devices and IoT applications where you need file-based persistence without database infrastructure.

## Why ts-store

Most time series databases fall into two camps: lightweight tools that aggregate your data (losing the raw readings), or full database engines that grow unbounded and require significant infrastructure.

ts-store takes a different approach:

- **Fixed storage footprint** - Total size is determined at creation time. No unbounded growth, no retention policies to tune, no midnight cleanup jobs.
- **Raw data preservation** - No lossy downsampling or rollup aggregation. Every sensor reading is stored exactly as received. Critical for anomaly detection, ML training, and forensic analysis.
- **Circular buffer architecture** - When storage is full, the oldest data is automatically overwritten. The store is always the same size whether it holds one reading or a million.
- **Zero external dependencies** - No database server, no runtime, no configuration management. A single binary and flat files on disk.
- **O(log n) time lookups** - Binary search on sorted timestamps for fast range queries, not just sequential access.

ts-store is designed for environments where you care about the last N readings with exact values: edge sensors, embedded systems, Raspberry Pi deployments, or any application where predictable resource usage matters more than unbounded retention.

## Architecture

ts-store implements a circular buffer-based storage system optimized for time series data. Multiple small objects pack into a single block for efficiency, and objects larger than a block automatically span multiple blocks.

```
┌────────────────────────────────────────────────────────────┐
│                    Circular Data Blocks                    │
│  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐      │
│  │  0  │──│  1  │──│  2  │──│  3  │──│  4  │──│  5  │──... │
│  │     │  │     │  │     │  │     │  │     │  │     │      │
│  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘  └─────┘      │
│     ↑                                   ↑                  │
│    tail                               head                 │
│  (oldest)                           (newest)               │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│                    Circular Index                          │
│  ┌───────────────────────────────────────────────────────┐ │
│  │ [ts₀, blk₀] [ts₁, blk₁] [ts₂, blk₂] ... [tsₙ, blkₙ]    │ │
│  └───────────────────────────────────────────────────────┘ │
│                    Binary search for O(log n) lookups      │
└────────────────────────────────────────────────────────────┘
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
- **Crash recovery** - Metadata is persisted after each operation
- **REST API server** - HTTP API with per-store API key authentication
- **WebSocket streaming** - Real-time data streaming with inbound and outbound modes
- **Unix socket ingestion** - Low-latency local data ingestion for high-frequency sensors
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
docker run -d -v tsstore-data:/data -p 21080:21080 --name tsstore tsstore
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
| `TSSTORE_ADMIN_KEY` | (required) | Admin key for store creation (min 20 chars) |
| `TSSTORE_DATA_PATH` | `/data` | Base path for stores |
| `TSSTORE_HOST` | `0.0.0.0` | Server bind address |
| `TSSTORE_PORT` | `21080` | Server port |
| `TSSTORE_MODE` | `release` | Gin mode (debug/release) |
| `TSSTORE_SOCKET_PATH` | `/var/run/tsstore/tsstore.sock` | Unix socket path |
| `TSSTORE_TLS_CERT` | (optional) | Path to TLS certificate file |
| `TSSTORE_TLS_KEY` | (optional) | Path to TLS private key file |

## REST API Server

ts-store includes a lightweight REST API server designed for edge devices.

### Starting the Server

```bash
# Admin key is required (prevents unauthorized store creation)
export TSSTORE_ADMIN_KEY="your-secure-admin-key-here"
./tsstore serve
```

The server reads configuration from `config.json` (or `TSSTORE_CONFIG` env var).

### Configuration

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

Environment variables:
- `TSSTORE_ADMIN_KEY` - Admin key for store creation (required, min 20 chars)
- `TSSTORE_HOST` - Server host
- `TSSTORE_PORT` - Server port
- `TSSTORE_MODE` - "debug" or "release"
- `TSSTORE_DATA_PATH` - Base path for stores
- `TSSTORE_SOCKET_PATH` - Unix socket path (empty to disable)
- `TSSTORE_TLS_CERT` - TLS certificate file (enables HTTPS when set with TLS_KEY)
- `TSSTORE_TLS_KEY` - TLS private key file (enables HTTPS when set with TLS_CERT)
- `TSSTORE_CONFIG` - Config file path

### TLS/HTTPS

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

### Authentication

ts-store uses two types of authentication:

**Admin Key** (for store management):
- Required for creating new stores
- Configured at server startup via `TSSTORE_ADMIN_KEY` (min 20 characters)
- Pass via `X-Admin-Key` header or `admin_key` query parameter

**Store API Key** (for data operations):
- Each store has its own API key, generated when the store is created
- The key is shown only once - store it securely
- Pass via any of these methods (checked in order):
  - Header: `X-API-Key: tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
  - Header: `Authorization: Bearer tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
  - Query param: `?api_key=tsstore_xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`

### API Endpoints

#### Health Check
```
GET /health
```
Returns server health status. No authentication required.

#### Create Store (requires admin key)
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

#### List Open Stores
```
GET /api/stores
```

#### Delete Store (requires auth)
```
DELETE /api/stores/:store
X-API-Key: <api-key>
```

#### Reset Store (requires auth)
```
POST /api/stores/:store/reset
X-API-Key: <api-key>
```
Clears all data from the store but keeps configuration, schema, and API keys. Useful for starting fresh without recreating the store.

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

**Note:** This is a soft delete. The data is excluded from API responses and WebSocket streams, but remains on disk until the block is overwritten as the circular buffer wraps. This is sufficient for hiding accidental data entries from clients.

#### List Oldest Data (requires auth)
```
GET /api/stores/:store/data/oldest?limit=10
X-API-Key: <api-key>
```
Returns the N oldest objects with data (default 10). Add `include_data=false` to return metadata only.

#### List Newest Data (requires auth)
```
GET /api/stores/:store/data/newest?limit=10
GET /api/stores/:store/data/newest?since=2h&limit=100
X-API-Key: <api-key>
```
Returns the N newest objects with data (default 10). Use `since` parameter for relative time queries. Add `include_data=false` to return metadata only.

#### Query Time Range (requires auth)
```
GET /api/stores/:store/data/range?start_time=X&end_time=Y&limit=100
GET /api/stores/:store/data/range?since=24h&limit=100
X-API-Key: <api-key>
```
Returns objects within the time range with data. Use `since` as an alternative to `start_time`/`end_time`. Add `include_data=false` to return metadata only.

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

**Important:** Schema stores expect flat JSON with dot-notation field names. Nested JSON objects are not supported. Use field names like `"cpu.pct"` and `"memory.total"` instead of nested structures like `{"cpu": {"pct": 5}}`.

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

**Slow client behavior:** WebSocket readers poll the store every 100ms for new data. If a client falls behind and the circular buffer wraps (oldest data gets reclaimed), the client will silently skip to the current oldest available data. There is no gap notification - the client simply continues from wherever the store's tail currently is. For use cases requiring guaranteed delivery, size the store appropriately or use a persistent queue architecture.

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

**Note:** This API only manages **outbound** connections (where ts-store initiates the connection to a remote server). **Inbound** connections (where clients connect to `/ws/read` or `/ws/write`) are not tracked or listed here - they remain open as long as the client maintains them.

**List Connections:**
```
GET /api/stores/:store/ws/connections
X-API-Key: <api-key>
```
Returns only outbound connections created via POST.

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

### Unix Socket (Low-Latency Local Ingestion)

For high-frequency local data ingestion with minimal overhead, ts-store provides a Unix domain socket interface. This eliminates HTTP overhead and is ideal for sensor data collection on edge devices.

**Configuration:**

By default, the socket is created at `/var/run/tsstore/tsstore.sock`. Override with:
- Environment: `TSSTORE_SOCKET_PATH=/path/to/socket.sock`
- Config: `{"server": {"socket_path": "/path/to/socket.sock"}}`
- CLI: `tsstore serve --socket /path/to/socket.sock`
- Disable: `tsstore serve --no-socket`

**Protocol:**

1. Connect to the Unix socket
2. Send authentication: `AUTH <store-name> <api-key>\n`
3. Receive response: `OK\n` or `ERROR <message>\n`
4. Send JSON data lines: `{"field": "value"}\n`
5. Receive per-line response: `OK <timestamp>\n` or `ERROR <message>\n`
6. Send `QUIT\n` to disconnect

**Example (using netcat):**
```bash
(
echo "AUTH my-store tsstore_xxxx-xxxx-xxxx"
echo '{"temp": 22.5, "humidity": 45.2}'
echo '{"temp": 22.6, "humidity": 45.1}'
echo "QUIT"
) | nc -U /var/run/tsstore/tsstore.sock
```

**Example (Python):**
```python
import socket
import json

sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect('/var/run/tsstore/tsstore.sock')

# Authenticate
sock.send(b'AUTH my-store tsstore_xxxx-xxxx-xxxx\n')
response = sock.recv(1024)  # OK\n

# Send data
data = {"temp": 22.5, "humidity": 45.2}
sock.send((json.dumps(data) + '\n').encode())
response = sock.recv(1024)  # OK <timestamp>\n

sock.send(b'QUIT\n')
sock.close()
```

**Benefits over HTTP:**
- ~10x lower latency (microseconds vs milliseconds)
- No TCP/HTTP overhead
- Persistent connection for streaming data
- Ideal for high-frequency sensor sampling (100Hz+)

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

### Swagger UI

Explore the API interactively using Swagger Editor:

```bash
./tsstore swagger
```

This starts a local file server on port 21090, serves `swagger.yaml` with CORS headers, and opens https://editor.swagger.io in your browser with the spec pre-loaded. Press Ctrl+C to stop.

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
# Use ws:// for HTTP, wss:// for HTTPS
websocat "ws://localhost:21080/api/stores/my-store/ws/read?api_key=KEY&from=0"
websocat -k "wss://localhost:21080/api/stores/my-store/ws/read?api_key=KEY&from=0"  # -k to skip cert verification

# Test inbound write (send data to store)
websocat "ws://localhost:21080/api/stores/my-store/ws/write?api_key=KEY"
# Then type: {"data": {"temp": 72.5}}

# Test outbound push (start a test server first)
websocat -s 9000  # Start test server on port 9000

# Create outbound push connection
curl -X POST localhost:21080/api/stores/my-store/ws/connections \
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

## Reset Store (Soft Reset)

If you need to clear all data from a store (e.g., after clock issues or for testing), use the Reset function:

```go
// Reset performs a soft reset - clears pointers, old data remains until overwritten
err := s.Reset()
```

Or via the API:
```bash
curl -X POST "http://localhost:21080/api/stores/my-store/reset" \
  -H "X-API-Key: <api-key>"
```

**Note:** This is a **soft reset**. It resets metadata pointers and clears the first block's index entry, making old data inaccessible. However, old data remains on disk until overwritten by new data. This is an O(1) operation that completes instantly regardless of store size.

For a complete data wipe, delete and recreate the store.

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
