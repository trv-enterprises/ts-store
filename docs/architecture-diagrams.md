<!--
Copyright (c) 2026 TRV Enterprises LLC
Licensed under the Business Source License 1.1
See LICENSE file for details.
-->

# Architecture Diagrams

## Block Structure

```
BlockHeader (24 bytes):
+----------------------------------------------------------+
| Timestamp (8 bytes)                                       |
+----------------------------------------------------------+
| DataLen (4 bytes)    | Flags (4 bytes)                   |
+----------------------------------------------------------+
| Reserved (8 bytes)                                        |
+----------------------------------------------------------+

Flags:
  - FlagPrimary      = 0x01  (block is a primary block)
  - FlagPacked       = 0x02  (block contains packed objects)
  - FlagContinuation = 0x04  (block is continuation of spanning object)

Note: Block number is NOT stored - it's calculated from file offset:
      block_number = file_offset / block_size
```

## Index Entry Structure

```
IndexEntry (16 bytes):
+----------------------------------------------------------+
| Timestamp (8 bytes)                                       |
+----------------------------------------------------------+
| BlockNum (4 bytes)   | Reserved (4 bytes)                |
+----------------------------------------------------------+
```

## Circular Buffer Layout

```
Store with NumBlocks = 10:

     Tail                              Head
       |                                 |
       v                                 v
   +---+---+---+---+---+---+---+---+---+---+
   | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 |
   +---+---+---+---+---+---+---+---+---+---+
     ^                                   ^
     |                                   |
   Oldest                             Newest
   Data                               Data

- Tail points to oldest data (next to be reclaimed)
- Head points to newest data (last inserted)
- Free space is implicit: the gap between (Head+1) and Tail
- When (head+1) % numBlocks == tail, the buffer is full
- On full, oldest block (at tail) is reclaimed
```

## Write Flow (space available)

```
+--------+         +-------+         +----------+
| Client |         | Store |         | DataFile |
+---+----+         +---+---+         +-----+----+
    |                  |                   |
    | PutObject(ts,    |                   |
    |   data)          |                   |
    |----------------->|                   |
    |                  |                   |
    |                  | Check: (head+1)   |
    |                  | % n == tail?      |
    |                  |                   |
    |                  | No, space exists  |
    |                  |                   |
    |                  | Calculate next    |
    |                  | head position     |
    |                  |                   |
    |                  | Write header      |
    |                  | + data            |
    |                  |------------------>|
    |                  |                   |
    |                  | Write index       |
    |                  | entry             |
    |                  |------------------>|
    |                  |                   |
    |                  | Advance head      |
    |                  |                   |
    | ObjectHandle     |                   |
    |<-----------------|                   |
    |                  |                   |
```

## Write Flow (buffer full, needs reclaim)

```
+--------+         +-------+         +----------+
| Client |         | Store |         | DataFile |
+---+----+         +---+---+         +-----+----+
    |                  |                   |
    | PutObject(ts,    |                   |
    |   data)          |                   |
    |----------------->|                   |
    |                  |                   |
    |                  | Check: (head+1)   |
    |                  | % n == tail? YES  |
    |                  |                   |
    |                  | Reclaim oldest:   |
    |                  | 1. Clear index    |------------>|
    |                  | 2. Advance tail   |             |
    |                  | 3. Skip any       |             |
    |                  |    continuations  |             |
    |                  |                   |             |
    |                  | Write new data    |             |
    |                  | to reclaimed      |             |
    |                  | block             |------------>|
    |                  |                   |             |
    |                  | Write index       |             |
    |                  | entry             |------------>|
    |                  |                   |             |
    |                  | Advance head      |             |
    |                  |                   |             |
    | ObjectHandle     |                   |             |
    |<-----------------|                   |             |
```

## Spanning Objects (Large Object Storage)

```
Large objects that exceed a single block span multiple sequential blocks:

Block N (Primary):
+------------------+
| BlockHeader      |  Flags: Primary | Packed
| - Timestamp      |
| - DataLen        |  (includes ObjectHeader + first chunk)
+------------------+
| ObjectHeader     |  Flags: Continues
| - Timestamp      |
| - DataLen        |  (total object size)
+------------------+
| Data chunk 1     |
+------------------+

Block N+1 (Continuation):      Block N+2 (Continuation):
+------------------+           +------------------+
| BlockHeader      |           | BlockHeader      |
| - Timestamp = 0  |           | - Timestamp = 0  |
| - DataLen        |           | - DataLen        |
| Flags: Cont.     |           | Flags: Cont.     |
+------------------+           +------------------+
| Data chunk 2     |           | Data chunk 3     |
+------------------+           +------------------+

Key points:
- Continuation blocks are ALWAYS sequential: (N+1) % numBlocks
- No pointers needed - next block is calculated
- Continuation blocks have Timestamp = 0 in index
- When reading, follow circular order until all data read
- When reclaiming, advance tail past all continuations
```

## Reclaim Process

```
When buffer is full:

BEFORE:
   Tail                              Head
     |                                 |
     v                                 v
 +---+---+---+---+---+---+---+---+---+---+
 | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 |
 +---+---+---+---+---+---+---+---+---+---+
   ^
   |
 Oldest - will be reclaimed

RECLAIM PROCESS:
  1. Clear index entry for tail block
  2. Advance tail: tail = (tail + 1) % NumBlocks
  3. If new tail is a continuation block, keep advancing
  4. Return reclaimed block for reuse

AFTER (new data written to reclaimed block):
       Tail                          Head
         |                             |
         v                             v
 +---+---+---+---+---+---+---+---+---+---+
 | 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 |
 +---+---+---+---+---+---+---+---+---+---+
   ^
   |
 New data written here (block reused directly)
```

## Crash Recovery

The store is designed to recover from crashes without data loss.

### Write Ordering Guarantees

**Writing a new block (advancing head):**
1. Write block data and header to disk
2. Write index entry to disk
3. Update HeadBlock in metadata (fsync)

If crash occurs:
- After step 1 or 2 but before 3: orphaned block exists
- Recovery detects and includes orphaned blocks

**Reclaiming old blocks (advancing tail):**
1. Update TailBlock in metadata first (fsync)
2. Clear old block data

If crash occurs:
- After step 1 but before 2: stale data remains but is excluded
- Old data will be overwritten on next use

### Recovery Algorithm (on Open)

```
recoverFromCrash():

Phase 1: Find orphaned writes
  - Scan forward from HeadBlock
  - If block at (head+1) has valid data, advance head
  - Repeat until no more orphaned blocks found

Phase 2: Fix tail pointer
  - If TailBlock points to a continuation block, advance it
  - Continue until tail points to a primary block

Phase 3: Fix WriteOffset
  - Recalculate from actual block contents
  - Ensures packed block writes resume correctly

Persist fixes to metadata
```

### Recovery Scenarios

```
Scenario 1: Crash during write
  Before crash: HeadBlock = 5, wrote data to block 6
  After crash:  HeadBlock = 5 (metadata not updated)
  Recovery:     Scan finds block 6 has data, advances head to 6

Scenario 2: Crash during reclaim
  Before crash: TailBlock = 3, cleared block 2
  After crash:  TailBlock = 3 (already updated)
  Recovery:     No action needed, old data excluded

Scenario 3: Spanning object partial reclaim
  Before crash: Tail at continuation block
  After crash:  TailBlock points to continuation
  Recovery:     Advance tail to next primary block
```

## Object Size Constraint

```
Single-Block Object Model:

+------------------+
|  BlockHeader     |  24 bytes
|  (fixed size)    |
+------------------+
|  ObjectHeader    |  24 bytes (for packed format)
+------------------+
|                  |
|  Object Data     |  up to (BlockSize - 48) bytes
|                  |
+------------------+

For packed blocks:
  Max single object = BlockSize - BlockHeaderSize - ObjectHeaderSize
                    = BlockSize - 48 bytes

For spanning objects:
  First block data  = BlockSize - 48 bytes
  Continuation data = BlockSize - 24 bytes per block
  Total capacity    = unlimited (uses multiple blocks)

Examples (4KB block):
  - Max single object: 4048 bytes
  - Spanning 2 blocks: ~8120 bytes
  - Spanning 3 blocks: ~12192 bytes
```

## File Layout

```
Data File (data.tsdb):
+------------------+------------------+------------------+-----+
|     Block 0      |     Block 1      |     Block 2      | ... |
|  (BlockSize)     |  (BlockSize)     |  (BlockSize)     |     |
+------------------+------------------+------------------+-----+

Total size = NumBlocks * BlockSize

Index File (index.tsdb):
+------------------+------------------+------------------+-----+
|   IndexEntry 0   |   IndexEntry 1   |   IndexEntry 2   | ... |
|   (16 bytes)     |   (16 bytes)     |   (16 bytes)     |     |
+------------------+------------------+------------------+-----+

Total size = NumBlocks * 16 bytes

Meta File (meta.tsdb):
+------------------+
| StoreMetadata    |
| (64 bytes)       |
+------------------+

Contains: Magic, Version, NumBlocks, BlockSize, Head, Tail, WriteOffset
```

## Time-Based Lookup

```
Binary Search on Index File:

Looking for timestamp T:

   0     1     2     3     4     5     6     7     8     9
+-----+-----+-----+-----+-----+-----+-----+-----+-----+-----+
|T100 |T200 |T300 |T400 |T500 |T600 |T700 |T800 |T900 |T1000|
+-----+-----+-----+-----+-----+-----+-----+-----+-----+-----+
  ^                       ^                             ^
  |                       |                             |
 Tail                   Found!                        Head

Search for T=500:
  1. Binary search index entries between tail and head
  2. Handle wraparound (if head < tail)
  3. Return block number for exact match
  4. Return ErrTimestampNotFound if not found
```

## WebSocket Architecture

ts-store supports real-time data streaming via WebSocket connections in both inbound and outbound modes.

### Connection Modes

```
INBOUND MODE:                         OUTBOUND MODE:

Client ──WS──► ts-store               ts-store ──WS──► Remote Server
  │              │                       │                  │
  │◄── data ─────┤ (read)               │─── data ────────►│ (push)
  │─── data ────►│ (write)              │◄── data ─────────│ (pull)
```

**Inbound connections** - Clients connect to ts-store:
- **Read**: Client receives real-time data stream from store
- **Write**: Client sends data to store

**Outbound connections** - ts-store connects to external servers:
- **Push**: ts-store sends store data to remote server
- **Pull**: ts-store receives data from remote server into store

### WebSocket Message Flow (Inbound Read)

```
+--------+         +---------+         +-------+
| Client |         | Handler |         | Store |
+---+----+         +----+----+         +---+---+
    |                   |                  |
    | WS Connect        |                  |
    | /ws/read?from=0   |                  |
    |------------------>|                  |
    |                   |                  |
    |                   | GetObjectsInRange|
    |                   |----------------->|
    |                   |                  |
    |                   |<-- handles ------|
    |                   |                  |
    | {"type":"data",   |                  |
    |  "timestamp":..., |                  |
    |  "data":...}      |                  |
    |<------------------|                  |
    |     (repeat for   |                  |
    |      each object) |                  |
    |                   |                  |
    | {"type":          |                  |
    |  "caught_up"}     |                  |
    |<------------------|                  |
    |                   |                  |
    |    ... live       |                  |
    |    streaming ...  | Poll for new    |
    |                   |----------------->|
    |                   |                  |
```

### WebSocket Message Flow (Inbound Write)

```
+--------+         +---------+         +-------+
| Client |         | Handler |         | Store |
+---+----+         +----+----+         +---+---+
    |                   |                  |
    | WS Connect        |                  |
    | /ws/write         |                  |
    |------------------>|                  |
    |                   |                  |
    | {"timestamp":..., |                  |
    |  "data":{...}}    |                  |
    |------------------>|                  |
    |                   |                  |
    |                   | PutObject        |
    |                   |----------------->|
    |                   |                  |
    |                   |<-- handle -------|
    |                   |                  |
    | {"type":"ack",    |                  |
    |  "timestamp":..., |                  |
    |  "block_num":...} |                  |
    |<------------------|                  |
    |                   |                  |
```

### Outbound Connection Lifecycle

```
+----------+         +---------+         +--------+
|  Manager |         |  Pusher |         | Remote |
+----+-----+         +----+----+         +----+---+
     |                    |                   |
     | Start()            |                   |
     |------------------->|                   |
     |                    |                   |
     |                    | Dial              |
     |                    |------------------>|
     |                    |                   |
     |                    |<-- connected -----|
     |                    |                   |
     |                    | Send data         |
     |                    |------------------>|
     |                    |     (loop)        |
     |                    |                   |
     |                    |<-- disconnect ----|
     |                    |                   |
     |                    | Backoff wait      |
     |                    |                   |
     |                    | Reconnect         |
     |                    |------------------>|
     |                    |                   |
     |                    | Resume from       |
     |                    | last_timestamp    |
     |                    |------------------>|
```

### Connection Config Persistence

Outbound connection configurations are persisted to enable automatic reconnection after restart:

```
Store Directory:
sensor-data/
├── data.tsdb
├── index.tsdb
├── meta.tsdb
├── keys.json
└── ws_connections.json    <-- WebSocket configs

ws_connections.json:
{
  "connections": [
    {
      "id": "abc123",
      "mode": "push",
      "url": "wss://remote.example.com/data",
      "from": 1234567890,
      "format": "compact",
      "headers": {"Authorization": "Bearer ..."},
      "created_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

### Reconnection Strategy

```
Outbound connection retry with exponential backoff:

Attempt   Delay
   1      1 second
   2      2 seconds
   3      4 seconds
   4      8 seconds
   5      16 seconds
   6      32 seconds
   7+     60 seconds (max)

On successful reconnect:
  - Resume from last_timestamp
  - No duplicate data sent
```

## Summary

| Component | Size | Purpose |
|-----------|------|---------|
| BlockHeader | 24 bytes | Block metadata (timestamp, length, flags) |
| ObjectHeader | 24 bytes | Per-object metadata in packed blocks |
| IndexEntry | 16 bytes | Fast timestamp-to-block lookup |
| Data Block | Configurable | Actual object data storage |
| StoreMetadata | 64 bytes | Store configuration and pointers |
| WSConnection | Variable | Outbound WebSocket connection config |
