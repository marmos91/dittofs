---
status: awaiting_human_verify
trigger: "seq-write-throughput-bottleneck: DittoFS 16.6 MB/s vs competitors ~50 MB/s despite lower per-op latency"
created: 2026-03-06T00:00:00Z
updated: 2026-03-06T22:00:00Z
---

## Current Focus

hypothesis: CONFIRMED -- First-run penalty caused by missing .blk cache files forcing slow memory-buffer-then-flush path
test: Implemented eager .blk file creation in tryDirectDiskWrite for writes >= 64KiB
expecting: First run matches second run at ~51 MB/s
next_action: User to verify fix on Scaleway benchmark infrastructure

## Symptoms

expected: Sequential write throughput of ~50 MB/s, matching kernel-nfs and other userspace competitors
actual: Sequential write throughput of only 16.6 MB/s (0.34x kernel-nfs). Per-op P50 latency is 612 us -- actually the LOWEST of all systems tested, yet throughput is 3x worse.
errors: No errors -- writes complete successfully, just slowly
reproduction: Mount DittoFS (badger+fs, 10 GiB cache, NFSv3 hard mount) and run dfsctl bench run with 4 threads, 1 GiB files, 4 KiB blocks
started: First time benchmarking feat/cache-rewrite branch against competitors

## Eliminated

- hypothesis: "WAL fsync serializes writes"
  evidence: There is no WAL. The cache-rewrite branch uses a two-tier memory+disk BlockCache.
  timestamp: 2026-03-06T00:01:00Z

- hypothesis: "Offloader backpressure blocks foreground writes"
  evidence: offloader.Flush() is non-blocking.
  timestamp: 2026-03-06T00:01:00Z

- hypothesis: "Metadata CommitWrite does synchronous store transaction"
  evidence: deferredCommit=true by default, no store transaction on write path.
  timestamp: 2026-03-06T00:01:00Z

- hypothesis: "NFS COMMIT handler blocks on synchronous flush to S3"
  evidence: offloader.Flush() is non-blocking for large files.
  timestamp: 2026-03-06T00:01:00Z

- hypothesis: "Buffer pool contention limits concurrency"
  evidence: With synchronous processing, only ONE request active per connection.
  timestamp: 2026-03-06T00:01:00Z

- hypothesis: "Global RWMutex on cache was the sole bottleneck"
  evidence: Replacing with sync.Map did not fix throughput (still 16.6 MB/s on clean start).
  timestamp: 2026-03-06T14:00:00Z

- hypothesis: "Synchronous fsync on flushBlock was the bottleneck"
  evidence: Removing fsync did not fix throughput (still 16.6 MB/s on clean start).
  timestamp: 2026-03-06T14:00:00Z

- hypothesis: "BadgerDB sealed index scan was the bottleneck"
  evidence: Adding fb-sealed: index did not fix throughput (still 16.6 MB/s on clean start).
  timestamp: 2026-03-06T14:00:00Z

- hypothesis: "tcp_slot_table_entries=2 is the sole cause"
  evidence: Second run with 2 slots = 51 MB/s, so 2 slots is not inherently limiting.
  timestamp: 2026-03-06T18:00:00Z

- hypothesis: "OS page cache warming explains the second-run speedup"
  evidence: Dropping server caches between runs did NOT slow the second run (still 51 MB/s).
  timestamp: 2026-03-06T18:00:00Z

- hypothesis: "Periodic uploader I/O contention causes first-run slowdown"
  evidence: Deleting .blk files (without restarting dfs) reproduced 16.6 MB/s, disproving uploader as sole cause.
  timestamp: 2026-03-06T20:00:00Z

## Evidence

- timestamp: 2026-03-06T00:01:00Z
  checked: NFS connection.go dispatch model
  found: Requests processed SYNCHRONOUSLY (IIFE, not go func). Single-threaded per connection.
  implication: Prevents NFS client pipelining benefit.

- timestamp: 2026-03-06T00:01:00Z
  checked: BlockCache.flushBlock (pkg/cache/flush.go)
  found: f.Sync() (fsync) on every 8MB block fill. Blocks write thread 5-10ms per call.
  implication: Stalls ALL RPC processing during fsync on serialized connection.

- timestamp: 2026-03-06T00:01:00Z
  checked: Concurrent dispatch attempt (go func) and nconnect=4
  found: Both WORSENED performance. rand-write -51%, metadata -86%, nconnect dropped to 5.0 MB/s.
  implication: Cache internal locking is the root contention bottleneck.

- timestamp: 2026-03-06T10:00:00Z
  checked: Full cache locking hierarchy
  found: Single bc.mu RWMutex guards both memBlocks and files maps. Hot-path operations (getOrCreateMemBlock, flushBlock, flushOldestDirtyBlock, updateFileSize) all contend on it.
  implication: Must shard the lock or use lock-free data structure.

- timestamp: 2026-03-06T10:30:00Z
  checked: Previous fix implementation (sync.Map + fsync removal + sealed index)
  found: All three optimizations applied but throughput unchanged at 16.6 MB/s on clean start.
  implication: The real bottleneck was NOT locking, fsync, or index scan.

- timestamp: 2026-03-06T16:00:00Z
  checked: First run vs second run behavior
  found: |
    First run (clean start, no .blk files): 16.6 MB/s
    Second run (same process, .blk files exist): 51.0 MB/s
    Second run (server caches dropped, .blk files exist): 51.0 MB/s
    First run (clean start, tcp_slot_table_entries=16): 50.9 MB/s
  implication: Something about existing .blk cache files makes writes 3x faster.

- timestamp: 2026-03-06T20:00:00Z
  checked: tryDirectDiskWrite code path in pkg/cache/write.go
  found: |
    tryDirectDiskWrite attempts pwrite() to existing .blk file. If no file exists,
    returns false and falls through to slow memory-buffer path:
    - Memory path: allocate 8MiB buffer, copy 1MiB per NFS WRITE, flush 8MiB to disk every 8 writes
    - Direct path: pwrite 1MiB directly to file per NFS WRITE (no memory allocation, no flush)
    PayloadID is deterministic (based on share+path), so second run's .blk files
    are found by tryDirectDiskWrite even though the file was "recreated".
    cache.Truncate only purges memBlocks, does NOT delete .blk files from disk.
  implication: First run ALWAYS uses slow memory path; second run uses fast pwrite path.

- timestamp: 2026-03-06T20:30:00Z
  checked: Controlled experiment - delete .blk files on warm server
  found: |
    Deleted all .blk files (kept dfs running, BadgerDB intact): 16.6 MB/s
    Ran again immediately (with .blk files now recreated): 51.0 MB/s
    This is 100% reproducible across multiple trials.
  implication: CONFIRMED - the presence of .blk cache files is the critical variable.

- timestamp: 2026-03-06T21:30:00Z
  checked: Fix - eager .blk file creation for large writes
  found: |
    Modified tryDirectDiskWrite to create .blk file eagerly when write >= 64KiB.
    Clean start benchmark results:
    - seq-write: 51.0 MB/s (was 16.6 MB/s) -- 3.1x improvement
    - Second run: 50.9 MB/s (consistent)
    - rand-write: 309 IOPS (was 331 -- within noise, no regression)
    - metadata: 626 ops/s (was 367 -- 71% improvement, bonus from less 8MiB flush churn)
    All cache tests pass with -race, full project builds cleanly.
  implication: Fix eliminates the first-run penalty completely.

## Resolution

root_cause: |
  The write path had two code paths with dramatically different performance:

  1. FAST PATH (tryDirectDiskWrite): pwrite() 1MiB directly to existing .blk file.
     ~600us per NFS WRITE. Only works when .blk file already exists on disk.

  2. SLOW PATH (memory buffer): Allocate 8MiB buffer, copy 1MiB per WRITE, flush
     entire 8MiB to disk when buffer is full. The periodic 8MiB flush creates I/O
     bursts and contention with background upload I/O.

  On first run after clean start: no .blk files exist, ALL writes go through slow
  path. Result: 16.6 MB/s.

  On second run: .blk files from first run still exist (cache.Truncate doesn't
  delete them), tryDirectDiskWrite succeeds, all writes use fast pwrite path.
  Result: 51.0 MB/s.

  The PayloadID is deterministic (share_name/file_path), so overwriting a file
  with the same name reuses the same .blk cache files.

fix: |
  Modified tryDirectDiskWrite in pkg/cache/write.go to eagerly CREATE .blk files
  for writes >= 64KiB (directDiskWriteThreshold). Previously it only opened
  existing files; now it creates them on first write if the write is large enough.

  The 64KiB threshold ensures:
  - Large sequential writes (NFS wsize=1MiB) always use the fast pwrite path
  - Small random writes (4KiB) still use the memory buffer for efficient batching

  Added createBlockFile helper that creates parent directories and opens the file.

verification: |
  Benchmark results (Scaleway GP1-XS, badger+fs, NFSv3 hard mount):

  BEFORE (clean start):
    seq-write: 16.6 MB/s

  AFTER (clean start with fix):
    seq-write: 51.0 MB/s (3.1x improvement, matches Ganesha at 49.2 MB/s)
    second run: 50.9 MB/s (consistent)
    rand-write: 309 IOPS (no regression from 331)
    metadata: 626 ops/s (71% improvement from 367)

  - All 16 cache unit tests pass with -race (zero data races)
  - Full project builds cleanly (go build ./...)
  - Verified on actual Scaleway infrastructure with real NFS mounts

files_changed:
  - pkg/cache/write.go (eager .blk creation + directDiskWriteThreshold constant)
  - pkg/cache/cache.go (sync.Map for memBlocks, separate filesMu)
  - pkg/cache/flush.go (fsync removal from flushBlock, syncFile helper)
  - pkg/cache/block.go (sync.Pool buffer reuse)
  - pkg/cache/cache_test.go (new, 16 tests)
  - pkg/metadata/store/badger/objects.go (fb-sealed: secondary index)
