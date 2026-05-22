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
//     is called for each surviving logPos; idx.AdvanceFence walks the
//     consumed set forward from the prior fence and returns the new one
//     — that value is what gets persisted as rollup_offset.
//
// rollup_offset semantic shift: the on-disk format is unchanged, but
// the value now records the compactionFence (largest logPos s.t. every
// earlier entry has been consumed) rather than a sequential scan cursor.
// A consumed entry preceded by an unconsumed predecessor does NOT advance
// the fence — its chunks are durable, but the log byte prefix stays
// anchored until the predecessor is consumed (the "stalled fence"
// scenario). tree.ConsumeUpTo still runs every pass that emits chunks,
// so the dirty interval clears and the workload makes forward progress
// even when the fence cannot.
//
// TRANSITIONAL-V0.17+: physical log compaction (rewriting the log file
// dropping consumed records up to compactionFence) is not implemented
// here. The log grows until the payload is deleted; pressure machinery
// caps the worst case via maxLogBytes. A future milestone will
// add a compaction pass that truncates the on-disk log down to its
// post-fence suffix.
//
// TRANSITIONAL-V0.17+: tracking consumption by file-offset interval
// instead of log position would eliminate the stalled-fence pathology
// entirely. Direction 1 keeps consumption keyed by logPos for minimal
// surface change; the file-offset-keyed variant is a v0.17 follow-up
// per R-7 in .planning/proposals/2026-05-22-logindex-rollup-redesign.md.
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
