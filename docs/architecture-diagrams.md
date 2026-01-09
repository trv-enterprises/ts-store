# Architecture Diagrams

## Block Structure

```
BlockHeader (32 bytes):
+----------------------------------------------------------+
| Timestamp (8 bytes)  | BlockNum (4 bytes)                |
+----------------------------------------------------------+
| DataLen (4 bytes)    | Flags (4 bytes)                   |
+----------------------------------------------------------+
| NextFree (4 bytes)   | Reserved (8 bytes)                |
+----------------------------------------------------------+

Flags:
  - FlagFree    = 0x01  (block is in free list)
  - FlagPrimary = 0x02  (block contains data)

NextFree: Only used when block is in free list, points to next free block
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
- When head+1 == tail, the buffer is full
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
    |                  | Check: head+1     |
    |                  | == tail?          |
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
+--------+         +-------+         +----------+         +-----------+
| Client |         | Store |         | FreeList |         | DataFile  |
+---+----+         +---+---+         +----+-----+         +-----+-----+
    |                  |                  |                     |
    | PutObject(ts,    |                  |                     |
    |   data)          |                  |                     |
    |----------------->|                  |                     |
    |                  |                  |                     |
    |                  | Check: head+1    |                     |
    |                  | == tail? YES     |                     |
    |                  |                  |                     |
    |                  | allocateBlock()  |                     |
    |                  |----------------->|                     |
    |                  |                  |                     |
    |                  |                  | FreeList empty?     |
    |                  |                  |                     |
    |        +---------+------------------+-------+             |
    |        | IF FreeList not empty:            |             |
    |        |   Pop block from free list        |             |
    |        |   Return that block               |             |
    |        +---------+------------------+-------+             |
    |        | ELSE (FreeList empty):            |             |
    |        |   Reclaim oldest (tail) block     |             |
    |        |   Clear index entry               |------------>|
    |        |   Advance tail pointer            |             |
    |        |   Return reclaimed block          |             |
    |        +---------+------------------+-------+             |
    |                  |                  |                     |
    |                  |<-----------------|                     |
    |                  |                  |                     |
    |                  | Write header     |                     |
    |                  | + data to block  |                     |
    |                  |-------------------------------------- >|
    |                  |                  |                     |
    |                  | Write index      |                     |
    |                  | entry            |                     |
    |                  |--------------------------------------- >|
    |                  |                  |                     |
    |                  | Update head      |                     |
    |                  |                  |                     |
    | ObjectHandle     |                  |                     |
    |<-----------------|                  |                     |
```

## Free List Structure

```
FreeList (singly-linked via NextFree field):

  meta.FreeListHead
        |
        v
   +---------+     +---------+     +---------+
   | Block A |---->| Block B |---->| Block C |----> 0 (end)
   | NextFree|     | NextFree|     | NextFree|
   | Flag=   |     | Flag=   |     | Flag=   |
   | Free    |     | Free    |     | Free    |
   +---------+     +---------+     +---------+

pushFreeList(X): Insert at HEAD - O(1)
   1. X.NextFree = FreeListHead
   2. X.Flags = FlagFree
   3. FreeListHead = X
   4. FreeListCount++

popFreeList(): Remove from HEAD - O(1)
   1. result = FreeListHead
   2. Read result header
   3. FreeListHead = result.NextFree
   4. FreeListCount--
   5. return result
```

## Reclaim Process

```
When buffer is full and no free blocks available:

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
  1. Save tail block number
  2. Clear index entry for tail block
  3. Advance tail: tail = (tail + 1) % NumBlocks
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

## Object Size Constraint

```
Single-Block Object Model:

+------------------+
|  BlockHeader     |  32 bytes
|  (fixed size)    |
+------------------+
|                  |
|  Object Data     |  up to (BlockSize - 32) bytes
|                  |
|                  |
+------------------+

Maximum Object Size = BlockSize - BlockHeaderSize
                    = BlockSize - 32 bytes

Examples:
  - 4KB block:  max object = 4064 bytes
  - 8KB block:  max object = 8160 bytes
  - 64KB block: max object = 65504 bytes

Objects larger than max size are rejected with ErrObjectTooLarge.
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
| (88 bytes)       |
+------------------+

Contains: Magic, Version, NumBlocks, BlockSize, Head, Tail, FreeListHead, etc.
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

## Summary

| Component | Size | Purpose |
|-----------|------|---------|
| BlockHeader | 32 bytes | Block metadata (timestamp, length, flags) |
| IndexEntry | 16 bytes | Fast timestamp-to-block lookup |
| Data Block | Configurable | Actual object data storage |
| Max Object | BlockSize - 32 | Largest storable object |
