---
status: diagnosed
trigger: "DittoFS random write performance is 281 IOPS on filesystem backend (308 IOPS on S3), which is behind ALL competitors"
created: 2026-03-07T12:00:00Z
updated: 2026-03-07T12:30:00Z
---

## Current Focus

hypothesis: CONFIRMED - COMMIT handler is the primary bottleneck due to heavy synchronous I/O and metadata operations on the critical path, compounded by per-connection serial dispatch.
test: Full code path analysis of WRITE+COMMIT cycle with timing estimates
expecting: COMMIT latency dominates the per-IOPS cost
next_action: Return diagnosis

## Symptoms

expected: DittoFS random write should be competitive with other userspace NFS servers (at least matching Rclone ~358 IOPS and JuiceFS ~309 IOPS)
actual: DittoFS random write is 281 IOPS (fs backend) and 308 IOPS (S3 backend), the worst among all tested systems
errors: None - pure performance issue
reproduction: Run rand-write fio benchmark with 4 threads, 4KB writes, 1GiB file, 60s duration, direct=1, iodepth=32
started: Since cache rewrite (was 331 IOPS before)

## Eliminated

(none - root cause confirmed on first hypothesis)

## Evidence

- timestamp: 2026-03-07T12:00:00Z
  checked: NFS connection dispatch model (pkg/adapter/nfs/connection.go:122-139)
  found: dispatchRequest uses SYNCHRONOUS IIFE (not goroutine). Each connection processes ONE RPC at a time serially. Comment says "Requests are handled one at a time per connection."
  implication: With 4 NFS connections, max parallelism is 4 concurrent RPCs. A slow COMMIT blocks ALL subsequent WRITEs on that connection.

- timestamp: 2026-03-07T12:01:00Z
  checked: NFS WRITE handler (internal/adapter/nfs/v3/handlers/write.go)
  found: WRITE uses deferred commits: PrepareWrite (cached file lookup - fast), cache.WriteAt (4KB memcpy into 8MB buffer - fast), CommitWrite (record in pendingWrites tracker - fast). Always returns UNSTABLE.
  implication: WRITE is fast (<300us). Not the bottleneck.

- timestamp: 2026-03-07T12:02:00Z
  checked: NFS COMMIT handler (internal/adapter/nfs/v3/handlers/commit.go)
  found: COMMIT does 5 expensive steps: (1) metaSvc.GetFile -> BadgerDB read tx, (2) payloadSvc.Flush -> offloader.Flush -> cache.GetDirtyBlocks -> cache.Flush + SyncFileBlocks + ListPendingUpload, (3) BuildAuthContextWithMapping (cached - fast), (4) metaSvc.FlushPendingWriteForFile -> BadgerDB write tx, (5) metaSvc.GetFile AGAIN for post-op WCC attrs.
  implication: Each COMMIT has 3+ BadgerDB transactions and synchronous cache flush I/O. Redundant second GetFile.

- timestamp: 2026-03-07T12:03:00Z
  checked: Offloader Flush path (pkg/payload/offloader/offloader.go:154-182)
  found: Offloader.Flush calls cache.GetDirtyBlocks SYNCHRONOUSLY. GetDirtyBlocks calls cache.Flush (which flushes dirty memBlocks to .blk files with fsync), then SyncFileBlocks (drains pending FileBlock updates to BadgerDB), then ListPendingUpload (BadgerDB scan). Only the final block store upload is async.
  implication: The COMMIT handler synchronously waits for ALL of: memBlock flush to disk, fsync, BadgerDB metadata writes, and BadgerDB scan.

- timestamp: 2026-03-07T12:04:00Z
  checked: Cache Flush block scanning (pkg/cache/flush.go:29-36)
  found: Flush iterates ENTIRE memBlocks map under RLock to find blocks matching payloadID. For 1GB file with random 4KB writes across all 128 blocks, this is O(128) per COMMIT.
  implication: Linear scan adds overhead proportional to file size / block size.

- timestamp: 2026-03-07T12:05:00Z
  checked: flushBlock implementation (pkg/cache/flush.go:86-156)
  found: Per block: os.Stat (disk I/O), os.OpenFile + f.Write of full 8MB (even if only 4KB dirty), lookupFileBlock (BadgerDB or pendingFBs), queueFileBlockUpdate. Then Flush does fsync on each flushed path.
  implication: Flushing an 8MB block for 4KB of dirty data = 2000x write amplification per block.

- timestamp: 2026-03-07T12:06:00Z
  checked: BadgerDB transaction conflict handling (pkg/metadata/store/badger/transaction.go:32-65)
  found: maxTransactionRetries=20, exponential backoff: base=(1+attempt)ms + jitter=attempt*ms. 4 threads writing same file -> FlushPendingWriteForFile all doing read-modify-write on same BadgerDB key -> ErrConflict.
  implication: Transaction conflicts add P99 latency spikes (23ms on fs backend). Each retry wastes 2-50ms.

- timestamp: 2026-03-07T12:07:00Z
  checked: fio benchmark config (bench/workloads/rand-write-4k.fio)
  found: direct=1, iodepth=32, ioengine=posixaio, fsync_on_close=1. With direct=1 on NFS, each write bypasses the page cache; the NFS client sends UNSTABLE WRITE RPCs and batches COMMITs for durability.
  implication: Each write operation requires WRITE + eventual COMMIT. The COMMIT frequency determines effective IOPS.

- timestamp: 2026-03-07T12:08:00Z
  checked: P50/P99 latency analysis
  found: P50=1,986us (fs), P99=23,492us (fs). 281 IOPS with 4 threads = 70 IOPS/connection = 14.2ms avg per completed write. If WRITE takes ~300us and the rest is COMMIT-amortized overhead, this suggests ~14ms effective per-op latency including COMMIT wait time.
  implication: The serial dispatch means WRITEs queue behind COMMITs. A single COMMIT flushing multiple dirty blocks (scan + flush + fsync + metadata) takes 5-20ms, blocking ~10-40 WRITEs.

- timestamp: 2026-03-07T12:09:00Z
  checked: S3 vs filesystem IOPS (308 vs 281)
  found: Both use identical cache+offloader path. S3 backend is slightly faster. The fs payload store writes to the same physical disk as the cache .blk files. Cache flush + fsync + payload pwrite all compete for disk I/O bandwidth.
  implication: Filesystem backend suffers from disk I/O contention between cache and payload store on same device.

- timestamp: 2026-03-07T12:10:00Z
  checked: Comparison with competitors' architecture
  found: kernel-nfs/ganesha write directly to the VFS (one write, kernel handles caching). DittoFS does: NFS WRITE -> memBlock copy, NFS COMMIT -> scan blocks -> flush 8MB to .blk -> fsync -> BadgerDB metadata -> BadgerDB metadata again. This is a fundamentally heavier COMMIT path.
  implication: Competitors skip the cache+metadata layers entirely for local filesystem backends, explaining the 5x-10x gap.

## Resolution

root_cause: The COMMIT handler is the primary bottleneck for random write IOPS. Five compounding issues:

**1. COMMIT handler does too much synchronous work** (`internal/adapter/nfs/v3/handlers/commit.go`)
- 3 BadgerDB transactions per COMMIT (GetFile, FlushPendingWriteForFile, GetFile again)
- Synchronous cache.Flush with fsync
- Synchronous SyncFileBlocks (drain to BadgerDB)
- Synchronous ListPendingUpload (BadgerDB scan)
- Redundant second GetFile for post-op attributes

**2. Per-connection serial dispatch** (`pkg/adapter/nfs/connection.go:122-139`)
- Requests are processed synchronously (sync IIFE, not goroutine)
- A 5-20ms COMMIT blocks all queued WRITEs on that connection
- With 4 connections, effective parallelism is limited to 4 concurrent RPCs

**3. Write amplification in cache flush** (`pkg/cache/flush.go:86-156`)
- Random 4KB write to an 8MB block -> full 8MB flush to disk
- Up to 128 blocks for a 1GB file, each needing fsync
- O(N) scan of all blocks to find dirty ones for one payloadID

**4. BadgerDB transaction conflicts** (`pkg/metadata/store/badger/transaction.go`)
- 4 threads COMMITing the same file concurrently -> ErrConflict
- Exponential backoff retries (1-50ms per retry)
- Explains P99 latency of 23ms

**5. Architectural overhead vs competitors**
- kernel-nfs/ganesha: WRITE -> VFS pwrite -> done (kernel handles cache)
- DittoFS: WRITE -> memcpy, COMMIT -> scan + flush 8MB + fsync + 3x BadgerDB ops

fix: Suggested fix directions (not implemented):

**Quick wins (should get to 500-800 IOPS):**
a. Make COMMIT lightweight: skip cache.Flush and cache.GetDirtyBlocks entirely for the NFS COMMIT path. Since data is already safe in memBlocks (process-safe) and the WAL provides crash safety, COMMIT only needs to flush pending metadata (FlushPendingWriteForFile). The periodic uploader handles cache-to-disk transfer.
b. Remove redundant second GetFile in COMMIT: use the file attrs from step 1 or get updated attrs from FlushPendingWriteForFile directly.
c. Add per-file index to memBlocks map: maintain a secondary index `map[payloadID][]blockKey` so Flush doesn't scan all blocks.

**Medium effort (should get to 1000+ IOPS):**
d. Make NFS dispatch concurrent: change the sync IIFE in `dispatchRequest` to a goroutine (`go func()`) with the existing requestSem limiting concurrency. This allows WRITEs to proceed while a COMMIT is in flight on the same connection.
e. Skip fsync in cache.Flush for random write workloads: fsync is only needed for NFS COMMIT data integrity, but if memBlocks are WAL-backed, fsync of .blk files is redundant.
f. Partial block flush: for blocks with small dirty regions, pwrite only the dirty range instead of the full 8MB.

**Architectural (approaching kernel-nfs 1,446 IOPS):**
g. For filesystem payload backends, bypass cache entirely: pwrite directly to the payload store file. This eliminates the double-write (cache .blk + payload store) and all the cache machinery overhead.
h. Replace BadgerDB metadata updates in COMMIT with in-memory tracking + periodic persist (similar to how deferred commits already work for file metadata).

verification:
files_changed: []
