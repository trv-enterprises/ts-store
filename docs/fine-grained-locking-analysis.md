<!--
Copyright (c) 2026 TRV Enterprises LLC
Licensed under the PolyForm Noncommercial License 1.0.0
See LICENSE file for details.
-->

# Fine-Grained Locking Analysis for ts-store

## Summary

**Recommendation: Do NOT implement fine-grained locking at this time.**

The complexity cost significantly outweighs the benefits for the target use case (edge devices with modest concurrency).

## Current Architecture

Single `sync.RWMutex` protects all state:
- Metadata: HeadBlock, TailBlock, WriteOffset
- Index entries (one per block)
- Block data
- Schema definitions

**Current behavior:**
- Writes block everything
- Reads can proceed concurrently with other reads

## Why Fine-Grained Locking Doesn't Buy Much

### 1. Target Use Case Mismatch
ts-store targets edge devices (Raspberry Pi, etc.) with 1-4 cores and modest concurrency. Lock contention is rarely the bottleneck.

### 2. RWMutex Already Allows Concurrent Reads
Most workloads are read-heavy. The current design already allows unlimited concurrent readers.

### 3. Spanning Objects Complicate Everything
Packed format with spanning objects requires coordinated multi-lock acquisition across sequential blocks - error-prone and complex.

### 4. File I/O Dominates
On edge devices with SD cards, I/O latency (5-50ms) far exceeds lock contention (~1μs).

## Performance Analysis

| Scenario | Current | Fine-Grained | Improvement |
|----------|---------|--------------|-------------|
| N readers, 0 writers | Parallel | Parallel | None |
| N readers, 1 writer (different blocks) | Serialized | Parallel | ~2x |
| N readers, 1 writer (same block) | Serialized | Serialized | None |

**Edge device (1-4 cores):** 20-60% improvement in rare contention scenarios
**High-core server (16+ cores):** 5x+ improvement

## Complexity Cost

- ~400 lines of new synchronization code
- 60-80% of existing lock sites modified
- Deadlock prevention via strict lock ordering
- Multi-lock coordination for spanning objects
- New edge cases: range queries crossing boundaries, reclaim during read

## Alternative Approaches (Lower Complexity)

1. **Batch writes** - Accept multiple objects in single lock acquisition
2. **Lock-free Stats()** - Use atomics for HeadBlock/TailBlock reads
3. **Read-through caching** - Cache recently read blocks (reduces I/O, not contention)
4. **Sharded stores** - Multiple independent stores instead of one fine-grained store

## When to Reconsider

Implement fine-grained locking if:
- Benchmarks show lock contention > 30% of operation time
- Target deployment includes high-core-count servers
- Concurrent write patterns become common

## If Implemented: Incremental Approach

### Phase 1: Two-Lock Design (Lowest Risk)
Separate metadata lock from data lock:
```go
type Store struct {
    metaMu sync.RWMutex  // HeadBlock, TailBlock, WriteOffset
    dataMu sync.RWMutex  // Block data and index
}
```
Benefit: Reads proceed while metadata updates happen.

### Phase 2: Striped Block Locks
```go
blockStripes [16]sync.RWMutex  // Block N uses stripe N % 16
```
Only if Phase 1 benchmarks show continued contention.

### Lock Ordering (Deadlock Prevention)
1. metaMu before any stripe lock
2. Block stripes in ascending block number order
3. Index stripe after corresponding block stripe
4. Release in reverse order

## Files That Would Change

| File | Changes |
|------|---------|
| `pkg/store/store.go` | Add lock arrays, modify all methods |
| `pkg/store/object.go` | Lock per-block operations |
| `pkg/store/packed.go` | Multi-block lock coordination |
| `pkg/store/freelist.go` | Reclaim with partial locks |
| `pkg/store/insert.go` | Lock coordination |

## Conclusion

The current single RWMutex design is appropriate for ts-store's target use case. Fine-grained locking adds significant complexity for marginal benefit on edge devices. If high-concurrency server deployment becomes a requirement, start with the two-lock approach (Phase 1) and benchmark before adding more complexity.
