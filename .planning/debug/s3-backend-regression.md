---
status: verifying
trigger: "S3 backend shows severe performance regressions after S3 data flow optimization changes"
created: 2026-03-07T00:00:00Z
updated: 2026-03-07T00:04:00Z
---

## Current Focus

hypothesis: The combination of multiple S3 optimizations (skipFsync, ReleaseMemBlock goroutines, GetDirtyBlocks skipping Flush, FromMemory bypass) creates systemic contention that degrades ALL operations including metadata ops. FS backend unaffected because directWritePath skips all offloader code paths.
test: Revert all S3-specific changes (cache.go, flush.go, types.go, upload.go, download.go, init.go) to baseline 02c1311e behavior, keeping only FS-specific optimizations. Then benchmark.
expecting: S3 performance returns to Round 20 levels. FS performance unchanged.
next_action: Deploy reverted code to server, run S3 and FS benchmarks to verify Round 20 performance restored.

## Symptoms

expected: S3 backend should perform at Round 20 levels (rand-write 820 IOPS, rand-read 1826 IOPS, metadata 626 ops/s, seq-write 50.9 MB/s, seq-read 63.9 MB/s)
actual: Round 22 S3 warm shows rand-write 498 IOPS (39% DROP), rand-read 692 IOPS (62% DROP), metadata 95 ops/s (85% DROP, P99=205ms!), small-files 86 ops/s. FS backend shows NO regression (rand-write 749, rand-read 1921, metadata 639). Server crashed during S3 cold read.
errors: Server became unresponsive (SSH timed out) during S3 cold read benchmark. No OOM in dmesg. Metadata P99=205ms on S3.
reproduction: Deploy current code to server, configure S3 backend, run benchmarks
started: After S3 data flow optimization changes (uncommitted on feat/cache-rewrite)

## Eliminated

- hypothesis: Three targeted fixes (FromMemory flag, remove inlineRangeDownload, download semaphore) would restore S3 performance
  evidence: Round 22 after fixes still shows severe regression. rand-write 498 (was 820), rand-read 692 (was 1826), metadata 95 (was 626). Targeted patches did not address the systemic contention.
  timestamp: 2026-03-07T00:03:00Z

## Evidence

- timestamp: 2026-03-07T00:00:30Z
  checked: GetDirtyBlocks Phase 1 captures dirty memBlocks, uploadRemainingBlocks processes them
  found: MarkBlockUploading(ctx, payloadID, blockIdx) requires BlockStateSealed, but dirty memBlocks captured from memory have NO FileBlock entry in BadgerDB (never flushed to disk). lookupFileBlock returns error, transitionBlockState returns false, MarkBlockUploading returns false, block is SKIPPED.
  implication: Dirty memBlocks captured from memory are NEVER uploaded to S3. Data leaks in memory. This is the PRIMARY root cause for rand-write, metadata, and small-files regressions, plus the server crash.

- timestamp: 2026-03-07T00:00:45Z
  checked: Old GetDirtyBlocks vs new - old called bc.Flush() first which created FileBlock entries with BlockStateSealed
  found: New GetDirtyBlocks removed bc.Flush() call, so FileBlock entries are never created for dirty memBlocks. The entire Sealed->Uploading->Uploaded state machine is bypassed. Neither the COMMIT path nor the periodic uploader can upload these blocks.
  implication: Complete failure of S3 upload path for data that has never been flushed to disk.

- timestamp: 2026-03-07T00:01:00Z
  checked: inlineRangeDownload for random reads (< 64KB)
  found: Each 4KB random read does a separate S3 range request (~5ms RTT). Old code downloaded full 8MB block and cached it, serving ~2048 subsequent reads from cache. New code never caches partial range data.
  implication: rand-read IOPS limited to ~800 (4 threads * 1000ms / 5ms RTT) which matches observed 762 IOPS. Old code got 1826 IOPS because block caching amortized S3 latency.

- timestamp: 2026-03-07T00:01:15Z
  checked: Multi-block parallel download path (line 113-156 in download.go)
  found: Unbounded goroutine creation for parallel block downloads. For a 1GB file cold read (128 blocks), creates 128 goroutines each downloading 8MB simultaneously, plus 64 prefetch goroutines. Combined with memory leak from dirty memBlocks, this likely caused the server crash.
  implication: Server crash during cold read was from combined memory pressure: leaked dirty memBlocks + unbounded parallel downloads.

- timestamp: 2026-03-07T00:03:30Z
  checked: Round 22 results after 3 targeted fixes (FromMemory, download full-block, semaphore)
  found: Fixes did NOT restore performance. metadata 95 ops/s (was 626), rand-read 692 (was 1826), rand-write 498 (was 820). FS backend unaffected (metadata 639, rand-read 1921, rand-write 749).
  implication: The 3 fixes addressed specific bugs but not the systemic contention. The code diff from baseline is 453 lines across 6 files with multiple interacting changes. Strategy: revert ALL S3 changes to baseline, keep FS optimizations.

- timestamp: 2026-03-07T00:04:00Z
  checked: Full analysis of diff from 02c1311e to identify all S3-specific changes
  found: Six categories of changes from baseline:
    1. skipFsync field + SetSkipFsync (new in cache.go, flush.go, init.go)
    2. ReleaseMemBlock function (new in cache.go) - spawns fire-and-forget goroutines
    3. GetDirtyBlocks rewritten with Phase1/Phase2 (cache.go) - skips Flush(), reads from memory
    4. FromMemory field in PendingBlock (types.go)
    5. uploadRemainingBlocks rewritten for FromMemory handling (upload.go) - skips MarkBlockUploading, calls ReleaseMemBlock
    6. EnsureAvailableAndRead rewritten with multi-block parallelism (download.go) - new semaphore, removed inlineRangeDownload
    7. Removed runtime.GC() from uploadPendingBlocks (upload.go)
  implication: These changes are deeply intertwined. Reverting to baseline for S3 while keeping FS direct-write path is the safest fix.

## Resolution

root_cause: The S3 data flow optimization rewrite introduced multiple interacting changes that collectively cause systemic contention degrading all operations. The optimizations (skip Flush, skip fsync, direct memory uploads, ReleaseMemBlock goroutines) bypass the well-tested Sealed->Uploading->Uploaded state machine. The targeted fixes (FromMemory flag etc.) addressed specific bugs but not the systemic issue. The safest approach is to revert S3 paths to baseline behavior.

fix: Reverted all 6 files to their baseline 02c1311e versions:
- pkg/cache/cache.go (removed skipFsync, ReleaseMemBlock, Phase1/Phase2 GetDirtyBlocks)
- pkg/cache/flush.go (removed skipFsync conditional)
- pkg/cache/types.go (removed FromMemory field)
- pkg/payload/offloader/upload.go (restored original uploadRemainingBlocks + uploadPendingBlocks with runtime.GC)
- pkg/payload/offloader/download.go (restored original EnsureAvailableAndRead without multi-block parallelism)
- pkg/controlplane/runtime/init.go (removed else branch with SetSkipFsync)

The FS direct-write optimization (directWritePath/SetDirectWritePath/IsDirectWrite) was already in the baseline, so it is preserved.

verification: Build succeeds. Cache tests pass (15/15). Offloader tests pass (25/25). Pre-existing GetStorageStats failure unchanged. git diff 02c1311e shows only bench code and planning config changes. Need production benchmark to confirm S3 performance restored to Round 20 levels.

files_changed:
- pkg/cache/cache.go (reverted to baseline)
- pkg/cache/flush.go (reverted to baseline)
- pkg/cache/types.go (reverted to baseline)
- pkg/payload/offloader/upload.go (reverted to baseline)
- pkg/payload/offloader/download.go (reverted to baseline)
- pkg/controlplane/runtime/init.go (reverted to baseline)
