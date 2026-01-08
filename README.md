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

## Installation

```bash
go get github.com/tviviano/ts-store
```

## Usage

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

Each store creates a directory with three files:

```
sensor-data/
├── data.tsdb    # Data blocks (primary + attached)
├── index.tsdb   # Time index entries
└── meta.tsdb    # Store metadata (64 bytes)
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
