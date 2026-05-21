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
//	          +-- rebuild per-file interval tree from surviving records
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
