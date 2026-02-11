# ts-store Go Library

[Back to main README](README.md)

This document covers using ts-store directly as a Go library without the API server.

## Installation

```go
import "github.com/tviviano/ts-store/pkg/store"
```

## Creating a Store

```go
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

## Opening an Existing Store

```go
s, err := store.Open("/var/data", "sensor-data")
if err != nil {
    log.Fatal(err)
}
defer s.Close()
```

## Inserting Data

```go
// Insert with specific timestamp (Unix nanoseconds)
timestamp := time.Now().UnixNano()
data := []byte(`{"temp": 72.5, "humidity": 45}`)

blockNum, err := s.Insert(timestamp, data)

// Or use current time automatically
blockNum, err := s.InsertNow(data)
```

## Finding Data

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

## Object API (High-Level)

The Object API provides a convenient interface for storing objects of any size:

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

### ObjectHandle

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

## JSON API

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

## Store Statistics

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

## Deleting a Store

```go
// Delete an open store
s.Delete()

// Or delete by path without opening
store.DeleteStore("/var/data", "sensor-data")
```

## Reset Store

```go
// Reset performs a soft reset - clears pointers, old data remains until overwritten
err := s.Reset()
```

**Note:** This is a **soft reset**. It resets metadata pointers and clears the first block's index entry, making old data inaccessible. However, old data remains on disk until overwritten by new data. This is an O(1) operation that completes instantly regardless of store size.

## Thread Safety

All Store methods are thread-safe. The implementation uses read-write locks to allow concurrent reads while serializing writes.

---

[Back to main README](README.md) | [API Reference](README-API.md) | [CLI Reference](README-CLI.md)
