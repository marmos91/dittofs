// Package fs implements a filesystem-based local block store for DittoFS.
//
// Writes are buffered in memory and flushed to disk atomically on NFS COMMIT
// or when memory budget is exceeded. This avoids per-4KB disk I/O and OS page
// bloat that caused OOM on servers with large local stores.
//
// Key design:
//   - Memory buffer tier: 4KB NFS writes go to in-memory []byte buffers (no disk I/O)
//   - Atomic flush: complete blocks written to .blk files with FADV_DONTNEED
//   - Backpressure: memory budget limits dirty buffers, oldest flushed first
//   - Flat addressing: blockIdx = fileOffset / 8MB
//
// # Append-log tier
//
// Package fs hosts the random-write absorber tier (per-payload append
// log + FastCDC rollup) that satisfies blockstore.BlockStoreAppend.
// Append is mandatory on the local tier post-v0.16 — the flag-gated
// opt-out was deleted alongside the legacy path-keyed writer in Phase
// 17.
//
// # Hybrid tier layout
//
//	<baseDir>/logs/<payloadID>.log        per-file append-only log
//	<baseDir>/blocks/<hh>/<hh>/<hex>      content-addressed chunks (CAS)
//
// # Log format (LSL-01)
//
// 64-byte header (D-09):
//
//	magic 'DFLG' | version | rollup_offset | flags | created_at | hdr_crc | reserved[32]
//
// Record framing (D-11):
//
//	payload_len (u32 LE) | file_offset (u64 LE) | crc32c (u32 LE) | payload
//
// CRC32C (Castagnoli) covers file_offset || payload. Hardware-accelerated
// on amd64 (SSE4.2) and arm64 (ARMv8 CRC32 extension).
//
// # CommitChunks atomicity (D-12, INV-03)
//
// Metadata is the source of truth for rollup_offset; the log header is
// idempotent derived state. CommitChunks sequence:
//
//  1. StoreChunk(h, data) -> blocks/<hh>/<hh>/<hex> (.tmp + rename + fsync)
//  2. metadata.SetRollupOffset(ctx, payloadID, target)   // atomic commit
//  3. advanceRollupOffset(log, target) + fsync header
//  4. tree.ConsumeUpTo(target) + logBytesTotal.Sub
//  5. non-blocking signal on pressureCh
//
// Crash between (2) and (3) is recovered on next boot: recovery reads the
// metadata offset and rewrites the header if metadata is ahead. Crash
// between (1) and (2) leaves an orphan chunk under blocks/; Phase 11's
// mark-sweep GC cleans it up (not in Phase 10).
//
// # Per-file logIndex (Direction 1)
//
// The append log stores records in arrival order (per-file mu serializes
// AppendWrite), NOT in file_offset order. Parallel clients (e.g. macOS
// NFSv3) routinely produce interleaved file_offsets across consecutive
// records. The interval tree owns the file-offset domain — "which file
// regions are dirty/stable" — and a per-payload logIndex owns the
// log-position domain — "where in the log does each record's frame
// start". The two are populated in lockstep:
//
//   - AppendWrite advances lf.eofPos under the per-file mu and appends
//     (logPos, fileOff, payloadLen) to the logIndex.
//   - Recovery's per-log scan does the same, then idx.SetFence(effectiveOff)
//     pins the compactionFence at the persisted rollup_offset.
//
// Rollup runs against this pair:
//
//   - tree.EarliestStable picks the next file-offset interval to chunk.
//   - idx.EntriesForInterval(off, length) returns the records whose
//     fileOffsets intersect that window, in arrival (logPos) order — the
//     order reconstructStream needs to honor D-35 "later-record-wins"
//     overwrites at the same offset.
//   - readRecordAt(rf, logPos, payloadLen) does a single pread per record
//     (header + payload), CRC-validates, and returns the payload.
//   - After StoreChunk succeeds for every emitted chunk, idx.MarkConsumed
//     is called for each surviving record's FILE-OFFSET extent (R-7,
//     #580); idx.AdvanceFence walks entries forward from the prior fence
//     and returns the new one — that value is what gets persisted as
//     rollup_offset.
//
// rollup_offset semantic shift: the on-disk format is unchanged, but
// the value now records the compactionFence (largest logPos s.t. every
// earlier entry's file extent has been chunked by SOME consumed record)
// rather than a sequential scan cursor. An entry whose file extent is
// fully covered by a LATER overlapping consumed record becomes dead even
// if the entry itself was never picked up by a rollup pass — the head-
// of-log overwrite stall is gone. An entry whose file extent has no
// overlapping consumed coverage stays alive and pins the fence until
// some record covering it is chunked. tree.ConsumeUpTo still runs every
// pass that emits chunks, so the dirty interval clears and the workload
// makes forward progress even when the fence cannot.
//
// Physical log compaction (#579): when the in-memory compactionFence
// has advanced past logHeaderSize by more than
// FSStoreOptions.CompactionThresholdBytes (default = maxLogBytes / 4),
// the post-rollup pass rewrites the log file dropping every record
// below the fence. The rewrite uses a temp file + fsync + parent-dir
// fsync (skipped on Windows) + atomic rename, then swaps the in-memory
// fd / groupCommit / logIndex under the per-file mu. The compacted
// header carries a LogFlagCompacted bit so recovery skips the
// metaOff > hdrOff reconcile (post-compaction metadata.rollup_offset
// legitimately sits above the compacted file's header offset and is no
// longer a physical position). Metadata.rollup_offset retains its
// monotonic high-water mark after compaction — SetRollupOffset calls
// from post-compaction rollup passes return ErrRollupOffsetRegression
// and are swallowed; the on-disk header is the source of truth from
// then on. See compaction.go for the full sequence.
//
// In-memory bookkeeping IS trimmed (R-2, #581): every AdvanceFence
// call drops the prefix of logIndex.entries (and the matching consumed
// map keys) that the fence walked past. The backing array is
// reallocated whenever its capacity exceeds 4× the surviving length so
// the old high-water allocation can be reclaimed by the GC. Steady-state
// RSS for a long-lived payload is therefore bounded at O(unconsumed-
// record set), not at the historical arrival count.
//
// R-7 (#580): consumption is now keyed by FILE-OFFSET interval (see
// logindex.go::coverageSet), so an overwritten head record's bytes are
// reclaimed as soon as the overwrite chunks — no longer pinned by the
// head record's own stabilization. The stalled-fence pathology from the
// initial Direction-1 design is gone.
//
// # Pressure channel (LSL-04, INV-05)
//
// logBytesTotal <= max_log_bytes per FSStore. When budget is exceeded,
// AppendWrite blocks on a select over pressureCh + ctx.Done(). Rollup
// signals pressureCh non-blockingly when bytes reclaim. Default budget is
// 1 GiB (config key `max_log_bytes`).
//
// # Crash recovery (LSL-06)
//
//	(boot) ---> scan logs/*.log
//	          |
//	          +-- read + validate header
//	          |    +-- bad magic / version / CRC ?
//	          |    |    -> truncate + re-init, count as hard-error
//	          |    +-- metadata.GetRollupOffset > header.rollup_offset ?
//	          |         -> advanceRollupOffset(header, metadata_offset)
//	          |
//	          +-- scan records from rollup_offset
//	          |    +-- readRecord ok=false (torn / CRC mismatch) ?
//	          |         -> truncate log at this record boundary
//	          |
//	          +-- rebuild per-file interval tree AND logIndex from
//	              surviving records (Direction 1); SetFence(effectiveOff)
//	              pins compactionFence at the boot-time rollup_offset
//	          |
//	          +-- orphan sweep:
//	               metadata.GetRollupOffset(payloadID) == 0
//	                 && no live FileBlock for payloadID
//	                 && mtime older than orphan_log_min_age_seconds
//	                 -> unlink logs/<payloadID>.log
//
// Orphan chunks under blocks/{hh}/{hh}/{hex} are NOT swept by Phase 10
// recovery; they are content-addressed and idempotent, reclaimed by the
// Phase 11 (A2) mark-sweep GC.
//
// # Concurrency (D-32 .. D-35)
//
// One sync.Mutex per payloadID guards log append + interval-tree insert.
// Fixed-size rollup pool (default 2 workers, config key `rollup_workers`)
// consumes stabilized dirty intervals from a shared queue. AppendWrite
// bypasses fdpool entirely -- each log is opened once per payload and held
// for the FSStore lifetime (D-34).
//
// # Flag-gated construction
//
// When use_append_log=false (default), FSStore never creates logs/, never
// starts the rollup pool, and AppendWrite returns an error. Production
// deployments on v0.15.0 Phase 10 see zero new runtime behavior. See
// docs/CONFIGURATION.md for the full key list.
package fs
