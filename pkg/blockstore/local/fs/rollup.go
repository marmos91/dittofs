package fs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/chunker"
	"github.com/marmos91/dittofs/pkg/metadata"
	"lukechampine.com/blake3"
)

// rec is a single record replayed from the append log during rollup.
// Declared at file scope (NOT redeclared inside rollupFile) so
// reconstructStream can take []rec without shadowing.
//
// endPos is the on-disk file position immediately AFTER the record's
// framed bytes (i.e., where the next record would start). FIX-19
// rollupFile uses this to recompute targetPos AFTER the truncation
// filter has dropped records past the truncation boundary, so
// SetRollupOffset never persists past bytes that did not contribute
// chunks. Zero is acceptable for callers (tests, reconstructStream)
// that do not care about file position.
type rec struct {
	off     uint64
	payload []byte
	endPos  uint64
}

// consumedExt is the FILE-OFFSET extent of a record the rollup will mark
// consumed in the logIndex. R-7 (#580): consumption is keyed by file-offset
// interval, not logPos. Captured in Phase A (under the append mutex) and
// applied in Phase C, so it must be an owned value type, not a view into the
// logIndex scratch buffer.
type consumedExt struct {
	fileOff    uint64
	payloadLen uint32
}

// StartRollup launches the chunkRollup worker pool. Idempotent
// subsequent calls after the first are no-ops. Requires a non-nil
// RollupStore.
//
// Workers join on Close() via bc.rollupWg; see Close in fs.go.
func (bc *FSStore) StartRollup(ctx context.Context) error {
	if bc.rollupStore == nil {
		return fmt.Errorf("rollup: nil RollupStore (set opts.RollupStore in NewWithOptions)")
	}
	if !bc.rollupStarted.CompareAndSwap(false, true) {
		return nil
	}
	workers := bc.rollupWorkers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		bc.rollupWg.Add(1)
		go bc.chunkRollupWorker(ctx, i)
	}
	return nil
}

// chunkRollupWorker is the per-worker goroutine body. It consumes payload
// IDs from bc.rollupCh and also periodically scans bc.dirtyIntervals on a
// ticker so payloads whose rollupCh signal was dropped (buffer full) still
// get processed. three-arm select guarantees no leaks on Close or ctx
// cancellation.
func (bc *FSStore) chunkRollupWorker(ctx context.Context, _ int) {
	defer bc.rollupWg.Done()

	// Tick at the stabilization window so a freshly-touched interval becomes
	// eligible on the next pass. Conservative floor of 50ms prevents
	// pathologically tight spin when stabilizationMS is set very low in
	// tests.
	tickInterval := time.Duration(bc.stabilizationMS) * time.Millisecond
	if tickInterval < 50*time.Millisecond {
		tickInterval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case pid := <-bc.rollupCh:
			if err := bc.rollupFile(ctx, pid, false); err != nil {
				// Log at Error so misconfigured or corrupted state is
				// observable; the dirty interval stays in the tree and
				// a future pass (or restart + recovery) retries. A
				// swallowed error here would hide FileBlock manifest
				// persistence failures and logIndex/tree divergence
				// reports from operator logs.
				slog.Error("rollupFile failed",
					"payloadID", pid, "error", err)
			}
		case <-ticker.C:
			bc.scanAllFiles(ctx)
		case <-bc.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// scanAllFiles snapshots every payloadID with a non-empty interval tree and
// dispatches rollupFile on each. Used by the ticker arm so payloads that
// missed a rollupCh signal (buffer full) still get rolled up.
func (bc *FSStore) scanAllFiles(ctx context.Context) {
	bc.logsMu.RLock()
	ids := make([]string, 0, len(bc.dirtyIntervals))
	for pid := range bc.dirtyIntervals {
		ids = append(ids, pid)
	}
	bc.logsMu.RUnlock()
	for _, pid := range ids {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := bc.rollupFile(ctx, pid, false); err != nil {
			slog.Error("rollupFile failed",
				"payloadID", pid, "error", err, "source", "ticker")
		}
	}
}

// rollupFile consumes the earliest stable interval for payloadID, emits
// chunks, and commits atomically.
//
// Concurrency (C1): the pass runs in three phases under TWO per-payload locks.
//
//   - rmu (the rollup mutex) is held across ALL three phases. It serializes
//     concurrent rollups on the same payload and is the barrier
//     DeleteAppendLog drains, so the ObjectIDPersister never writes manifest
//     rows for a payload that is being deleted.
//   - mu (the append mutex) is held only in Phase A and Phase C. It is
//     RELEASED for Phase B so a concurrent AppendWrite to this same payload is
//     not blocked on rollup I/O. Lock order: rmu (outer) -> mu (inner).
//
// Phase A (mu held): resolve the earliest stable interval to its backing log
// records, pread them into owned buffers, and capture the file-offset extents
// to consume. Only Phase A reads/mutates per-file log state (tree, idx, lf, rf).
//
// Phase B (no mu): reconstruct -> chunk -> StoreChunk -> ObjectIDPersister.
// These touch only CAS and the metadata manifest, never per-file log state.
//
// Phase C (mu re-held): re-validate lf/tombstone, then the CommitChunks
// sequence on log-side state:
//  1. (done in Phase B) StoreChunk — idempotent, fsynced.
//  2. (done in Phase B) ObjectIDPersister — writes the FileBlock manifest +
//     FileAttr.Blocks. MUST land before SetRollupOffset so a manifest-persist
//     failure leaves rollup_offset UNCHANGED and the next pass retries.
//  3. rollupStore.SetRollupOffset(payloadID, targetPos) — atomic-monotone;
//     on ErrRollupOffsetRegression, log at Debug and fall through (benign).
//  4. advanceRollupOffset(logFile, targetPos) — idempotent derived-state.
//  5. tree.ConsumeUpToStable + logBytesTotal.Add(-reclaimed) + pressureCh +
//     maybeCompactLog.
//
// Because mu is released between A and C, Phase C consumes the dirty tree with
// ConsumeUpToStable(stableEnd, phaseStart) — NOT plain ConsumeUpTo — so a write
// that raced Phase B (Touched > phaseStart) keeps its dirty marker and is
// rolled up by a later pass instead of being silently dropped. The logIndex
// fence only advances over consumedExtents (the Phase-A records), so a
// racing write's record stays unconsumed too.
//
// force=true bypasses the stabilization-window gate: the earliest dirty
// interval is consumed regardless of how recently it was touched. This is
// the snapshot-drain path (DrainRollups) — at snapshot time we want every
// written byte flushed to CAS + the FileBlock manifest NOW, not just the
// bytes that have aged past the stabilization window. Steady-state rollup
// (worker pool, ticker) always passes force=false to preserve the
// stabilization invariant.
func (bc *FSStore) rollupFile(ctx context.Context, payloadID string, force bool) error {
	if bc.isClosed() {
		return nil
	}

	bc.logsMu.RLock()
	lf := bc.logFDs[payloadID]
	tree := bc.dirtyIntervals[payloadID]
	mu := bc.logLocks[payloadID]
	rmu := bc.rollupLocks[payloadID]
	idx := bc.logIndices[payloadID]
	bc.logsMu.RUnlock()
	if lf == nil || tree == nil || mu == nil || rmu == nil || idx == nil {
		// Nothing to do — payload never had an AppendWrite (or was
		// cleared by Close/DeleteAppendLog).
		return nil
	}

	stabilization := time.Duration(bc.stabilizationMS) * time.Millisecond

	// C1: hold the rollup mutex across the entire pass (Phase A -> B -> C).
	// See the function doc comment for the locking contract. The append mutex
	// (mu) is acquired only inside Phase A and Phase C below.
	rmu.Lock()
	defer rmu.Unlock()

	// ---- Phase A: read snapshot under the append mutex ----
	var (
		stable          interval
		stableEnd       uint64
		recs            []rec
		consumedExtents []consumedExt
		trunc           uint64
		hasTrunc        bool
		// maxReadLogPos is the highest logPos among the records read in
		// Phase A. Phase C passes it to AdvanceFenceUpTo so the fence never
		// walks past a racing same-extent write that landed during Phase B
		// (its logPos is strictly greater — log positions are monotonic).
		maxReadLogPos uint64
		// phaseStart anchors the Phase C dirty-interval consume. It is captured
		// UNDER mu (below) so every interval in this pass's snapshot has
		// Touched <= phaseStart and is consumed, while only a write that lands
		// AFTER mu is released for Phase B has Touched > phaseStart and is
		// preserved for a later pass. Capturing it before acquiring mu would
		// spuriously preserve a write that completed between the timestamp and
		// the snapshot — already chunked this pass — causing redundant rollups.
		phaseStart time.Time
	)
	proceed, err := func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		phaseStart = time.Now()

		// FIX-21: re-validate lf under the per-file mutex. Between the
		// snapshot above (RUnlock'd before we took mu) and now, a concurrent
		// AppendWrite that hit a writeRecord error could have run the FIX-2
		// recovery path: removed the lf from bc.logFDs and closed lf.f. If we
		// proceed with the stale lf we'd read from a closed fd. The
		// double-checked re-read under the per-file mutex closes that window:
		// any FIX-2 cleanup must serialize behind us via mu, and any
		// already-completed cleanup will have left bc.logFDs[payloadID] nil
		// (or replaced with a fresh lf on a subsequent getOrCreateLog). A
		// stale rollup pass is benign — chunks will be retried on the next
		// pass once the new lf is established.
		bc.logsMu.RLock()
		curLf := bc.logFDs[payloadID]
		bc.logsMu.RUnlock()
		if curLf == nil || curLf != lf {
			return false, nil
		}

		// Tombstone re-check AFTER mutex acquire. DeleteAppendLog sets the
		// tombstone BEFORE its drain barrier; if we see it here, delete is in
		// flight (or completed) and we must bail out without persisting
		// rollup state for a dead payload. Ranging over a nil tombstones map
		// is legal so the check is a no-op before the first delete.
		if bc.isTombstoned(payloadID) {
			return false, nil
		}

		var ok bool
		if force {
			// Snapshot drain: take the earliest dirty interval regardless of
			// the stabilization window. Every written byte must reach CAS.
			stable, ok = tree.Earliest()
		} else {
			stable, ok = tree.EarliestStable(phaseStart, stabilization)
		}
		if !ok {
			return false, nil
		}

		// Read + validate the log header. Direction-1 no longer uses
		// hdr.RollupOffset for any computation (the in-memory idx.Fence is
		// the truth), but the read still serves as a corruption sanity
		// check before we issue preads against the same fd.
		if _, herr := readLogHeader(lf.f); herr != nil {
			slog.Warn("rollup: read header", "payloadID", payloadID, "error", herr)
			return false, herr
		}

		// Direction-1 rollup redesign: consult the per-payload logIndex to
		// translate the stable file-offset interval into a set of records
		// whose frames sit somewhere in the on-disk log (not necessarily
		// contiguous at the head). Each entry carries logPos + payloadLen
		// so we pread each frame directly instead of scanning the log
		// sequentially. A separate read-only fd is used so we don't disturb
		// lf.f's append position.
		stableEnd = stable.Offset + uint64(stable.Length)
		// A stable interval can match thousands of log entries under
		// parallel-write workloads; lookupInterval reuses a per-index scratch
		// buffer so this lookup does not reallocate every rollup pass. The
		// result is valid until the next lookupInterval call on this index —
		// safe here because we copy every entry into owned recs/consumedExtents
		// before releasing mu, and rmu serializes any other rollup on this
		// payload that could call lookupInterval again.
		entries := idx.lookupInterval(stable.Offset, uint64(stable.Length))
		if len(entries) == 0 {
			// The tree reported a stable interval the logIndex cannot back —
			// a tree/logIndex divergence. Causes: recovery rebuilt the tree
			// from records the index missed; a residual dirty interval left
			// behind by an interrupted write before AppendWrite's atomic
			// tree+logIndex update landed; or (C1) a benign stale tree marker.
			//
			// The C1 stale-marker case: because the append mutex is released
			// during Phase B, ConsumeUpToStable may preserve a dirty interval
			// at an offset whose backing index entries were all fenced+trimmed
			// by a concurrent same-payload pass. That is harmless — an entry is
			// fenced only once consumedCoverage covers its extent, which only
			// happens after StoreChunk persisted that offset's latest bytes to
			// CAS. So the offset's data is already durable; the leftover tree
			// marker has no bytes to roll up. Dropping it is correct, not a
			// loss. (It cannot occur on the pre-C1 whole-pass-lock design,
			// which is why this path is exercised only under concurrency.)
			//
			// #668: returning an error here re-queued the same dirty
			// interval on every ticker pass, wedging the payload's rollup
			// permanently in a tight Error-log loop until process restart.
			// Drop the divergent interval from the tree so the next pass
			// either picks up a later stable interval or finds the tree
			// empty; log once at Warn so operators still see the divergence
			// without the loop.
			slog.Warn("rollup: dropping divergent stable interval (no logIndex entries)",
				"payloadID", payloadID,
				"offset", stable.Offset,
				"length", stable.Length)
			tree.DropExact(stable.Offset, stable.Length)
			return false, nil
		}

		rf, oerr := os.Open(lf.path)
		if oerr != nil {
			return false, fmt.Errorf("rollup: open log for read: %w", oerr)
		}
		// rf is fully consumed within Phase A (the pread loop below), so close
		// it before releasing mu: Phase B then holds no fd, and the Phase C
		// maybeCompactLog rename never races an open read handle (Windows
		// refuses to rename over a path with ANY open handle).
		defer func() { _ = rf.Close() }()

		recs = make([]rec, 0, len(entries))
		consumedExtents = make([]consumedExt, 0, len(entries))
		for _, e := range entries {
			if e.logPos > maxReadLogPos {
				maxReadLogPos = e.logPos
			}
			off, payload, rerr := readRecordAt(rf, e.logPos, e.payloadLen)
			if rerr != nil {
				// A CRC mismatch or pread failure at a record the logIndex
				// claims is valid implies log-fd corruption or a logIndex/
				// log divergence bug. Returning nil here would re-queue the
				// interval forever — surface a hard error instead so the
				// worker pool's error log captures the divergence and
				// operators can inspect the log fd.
				return false, fmt.Errorf("rollup: readRecordAt(logPos=%d): %w", e.logPos, rerr)
			}
			if off != e.fileOff {
				return false, fmt.Errorf("rollup: logIndex fileOff divergence at logPos=%d (indexed=%d frame=%d)",
					e.logPos, e.fileOff, off)
			}
			recs = append(recs, rec{
				off:     off,
				payload: payload,
				// endPos preserved for downstream consumers that still rely
				// on it; with the logIndex-driven path it is no longer used
				// to advance targetPos, but reconstructStream / dedup code
				// reads it via the rec struct.
				endPos: e.logPos + uint64(recordFrameOverhead) + uint64(e.payloadLen),
			})
			consumedExtents = append(consumedExtents, consumedExt{fileOff: e.fileOff, payloadLen: e.payloadLen})
		}

		// truncation filter: drop records entirely past the truncation
		// boundary and clip straddling records. Recorded by TruncateAppendLog
		// consulted here so emitted chunks never contain bytes
		// beyond the client-observed size at truncate time.
		//
		// Direction-1 redesign: consumedExtents is filtered in lockstep with
		// recs so we only mark logIndex coverage for records that actually
		// contributed payload bytes to a stored chunk. Entries that truncation
		// dropped entirely stay unconsumed; the on-disk bytes belong to a
		// truncated tail the operator already invalidated. Clipped records
		// have their consumedExtent.payloadLen shortened to match the bytes
		// that actually made it into a chunk — coverage MUST match the bytes
		// chunked, not the original record size.
		bc.logsMu.RLock()
		trunc, hasTrunc = bc.truncations[payloadID]
		bc.logsMu.RUnlock()
		if hasTrunc {
			filtered := recs[:0]
			filteredExt := consumedExtents[:0]
			for i, r := range recs {
				if r.off >= trunc {
					continue
				}
				end := r.off + uint64(len(r.payload))
				if end > trunc {
					r.payload = r.payload[:trunc-r.off]
				}
				filtered = append(filtered, r)
				// Reflect any clipping into the consumed extent so coverage
				// doesn't over-claim file bytes that never reached CAS.
				ext := consumedExtents[i]
				ext.payloadLen = uint32(len(r.payload))
				filteredExt = append(filteredExt, ext)
			}
			recs = filtered
			consumedExtents = filteredExt
		}

		if len(recs) == 0 {
			return false, nil
		}
		return true, nil
	}()
	if err != nil || !proceed {
		return err
	}

	// ---- Phase B: reconstruct + chunk + store to CAS WITHOUT the append mutex ----
	// recs/consumedExtents are owned copies; alignRecsToChunkBoundaries,
	// reconstructStream, StoreChunk, and the ObjectIDPersister touch only CAS
	// and the metadata manifest, never per-file log state, so a concurrent
	// AppendWrite to this payload proceeds in parallel. rmu (still held) keeps
	// any second rollup on this payload serialized behind us.

	// rollupPhaseBHook is a test-only seam (nil in production) that fires once
	// the append mutex has been released for Phase B. Tests use it to inject a
	// concurrent AppendWrite into the window the rollup is committing,
	// deterministically exercising the ConsumeUpToStable lost-write guard.
	if rollupPhaseBHook != nil {
		rollupPhaseBHook(payloadID)
	}

	// #953: align the reconstruction window to existing CAS chunk
	// boundaries. An in-place overwrite whose dirty interval starts/ends
	// INSIDE a previously-rolled-up chunk would otherwise re-chunk only
	// [baseOff, maxEnd) and emit new FileBlock rows that OVERLAP the old
	// straddling chunks. The CAS read path (fillFromCASManifest /
	// readLocalByHash) then mixes generations: a stale row sorted after a
	// new row clobbers the freshly-written bytes (silent corruption on the
	// cold read), and naively reaping the straddling chunk instead would
	// leave its non-overwritten head/tail uncovered (zero-fill data loss).
	//
	// Expand the window to whole straddling-chunk boundaries by prepending
	// synthetic records carrying the straddling neighbors' CAS bytes, so
	// the re-chunk produces boundary-aligned chunks and every superseded
	// row is FULLY contained in the new region (cleanly reapable by the
	// ObjectIDPersister). The straddling bytes that the overwrite did not
	// touch are preserved because the overwrite records, applied later in
	// reconstructStream's "last record wins" order, overlay only the bytes
	// the client actually rewrote.
	//
	// Best-effort: a straddling neighbor whose chunk is not locally
	// readable (post-eviction) cannot be spliced; we skip expansion on
	// that side and the persister's reap stays conservative (it only
	// reaps rows fully inside the unexpanded region), leaving the existing
	// behaviour on that edge rather than risking a gap.
	//
	// trunc/hasTrunc are threaded through so a straddling neighbor is
	// never spliced past the truncation boundary — re-emitting bytes the
	// truncate just removed would resurrect truncated tail data.
	recs = bc.alignRecsToChunkBoundaries(ctx, payloadID, recs, trunc, hasTrunc)

	// Reconstruct contiguous byte stream ("later record wins") + chunk
	// + store. Chunker is stateless across calls; we feed it the whole
	// reconstructed buffer and slice out chunks by the returned boundaries.
	//
	// reconstructStream returns a buffer covering [baseOff, maxEnd) where
	// baseOff is the smallest record offset this pass; buf[i] holds file
	// byte baseOff+i, with untouched gaps inside the window zero-filled.
	// FastCDC is content-defined, so feeding the chunker stream[i:] yields
	// the same boundaries regardless of baseOff — anchoring at baseOff
	// avoids materializing the dead [0,baseOff) prefix.
	//
	// FIX-5: reconstructStream may refuse to allocate when the span exceeds
	// the ceiling. Treat that as benign at the rollup-pass level: log +
	// return nil without persisting derived state. The dirty intervals stay
	// in the tree so a later pass (possibly after a TruncateAppendLog) has
	// another chance.
	stream, baseOff, rerr := reconstructStream(recs)
	if rerr != nil {
		slog.Warn("rollup: reconstruct refused", "payloadID", payloadID, "error", rerr)
		return nil
	}
	// Return the pooled buffer when this pass ends. Safe at the function
	// tail: stream is consumed entirely by the chunker loop below and is
	// never captured by a closure, goroutine, or persisted beyond it.
	defer putReconstructBuf(stream)
	ck := chunker.NewChunker()
	// pos indexes the buffer; the absolute file offset of buf[pos] is
	// baseOff+pos (used for the BlockRef manifest below).
	pos := uint64(0)
	// Accumulate the BlockRef manifest as chunks are emitted. Sorted-by-
	// Offset is automatic because pos advances monotonically — that
	// matches the canonical FileAttr.Blocks invariant the downstream
	// ObjectID compute relies on.
	// #669: the LRU is populated AFTER the ObjectIDPersister callback
	// below confirms the FileBlock rows are durable. Populating before
	// persister success let a concurrent rollup on the same payload
	// observe the hash via the LRU and call AddRef before the row
	// existed → ErrUnknownHash storm under load (and a silent retry
	// loop). The hashes pushed into the LRU are derived from `blocks`
	// at the call site below; both the LRU-hit and StoreChunk paths
	// append to `blocks`, so no separate buffer is needed.
	var blocks []blockstore.BlockRef
	for pos < uint64(len(stream)) {
		b, _ := ck.Next(stream[pos:], true)
		if b <= 0 {
			// Defensive: chunker should always emit ≥1 byte when final=true
			// and data is non-empty; break to avoid an infinite loop.
			break
		}
		chunkBytes := stream[pos : pos+uint64(b)]
		h := blake3ContentHash(chunkBytes)
		blockRef := blockstore.BlockRef{
			Hash:   h,
			Offset: baseOff + pos,
			Size:   uint32(b),
		}

		// Consult the per-FSStore dedup LRU between FastCDC.Next() and
		// StoreChunk. The LRU is keyed by (hash, payloadID): on hit we
		// know THIS payload's prior rollup pass stored hash h and the
		// FileBlock row is committed, so AddRef bumps RefCount on the
		// correct row. Cross-payload LRU short-circuit is intentionally
		// not supported (#669 — the "wrong-row-owner" subcase);
		// cross-payload dedup still happens via the regular CAS path.
		// BlockRef append below still happens — manifest invariant:
		// ComputeObjectID later in this function sees the same BlockRef
		// list with or without the LRU. State preservation: AddRef
		// leaves BlockState unchanged; this hit path neither creates a
		// row nor transitions one.
		skipStoreChunk := false
		if bc.dedupLRU != nil && bc.dedupLRU.Hit(h, payloadID) {
			addRefErr := bc.blockStore.AddRef(ctx, h, payloadID, blockRef)
			switch {
			case addRefErr == nil:
				skipStoreChunk = true
				slog.Debug("rollup: LRU dedup hit",
					"hash", h, "payloadID", payloadID, "size", b)
			case errors.Is(addRefErr, metadata.ErrUnknownHash):
				// TOCTOU: hash was in LRU but the row got swept by a
				// concurrent engine.Delete cascade. Fall through to
				// StoreChunk + LRU re-populate below.
				slog.Debug("rollup: LRU stale (ErrUnknownHash); falling back to StoreChunk",
					"hash", h, "payloadID", payloadID)
			default:
				return fmt.Errorf("rollup: AddRef: %w", addRefErr)
			}
		}

		if !skipStoreChunk {
			if err := bc.StoreChunk(ctx, h, chunkBytes); err != nil {
				return fmt.Errorf("rollup: StoreChunk: %w", err)
			}
		}

		blocks = append(blocks, blockRef)
		pos += uint64(b)
	}

	// Tombstone re-check IMMEDIATELY before the metadata commit — and before
	// the persister, matching the pre-C1 ordering. A delete that raced Phase B
	// set the tombstone in DeleteAppendLog step 1 (before its drain barrier);
	// we must not write FileBlock manifest rows or advance rollup_offset for a
	// deleted payload. Content-addressed chunks already in blocks/ are swept
	// by GC, and the DeleteAppendLog drain on rmu waits for this pass to
	// finish before clearing the metadata row.
	if bc.isTombstoned(payloadID) {
		return nil
	}

	// CommitChunks atomic sequence. The on-disk CAS chunks are already durable
	// from StoreChunk above. The remaining steps must land in this order so a
	// partial failure never advances rollup_offset past records whose
	// FileBlock manifest rows haven't been persisted:
	//
	//  1. ObjectIDPersister(payloadID, blocks, objectID) — writes the per-chunk
	//     FileBlock rows AND the FileAttr.Blocks manifest + ObjectID. Runs here
	//     in Phase B (still WITHOUT the append mutex) because it touches only
	//     metadata. If it fails, rollup_offset stays UNCHANGED (Phase C never
	//     runs) and the next pass retries — chunks are content-addressed and
	//     idempotent on re-store. (Persister-after-SetRollupOffset is the wrong
	//     ordering: a persister failure would leave rollup_offset advanced past
	//     records whose manifest never landed, and the engine read path falls
	//     into the sparse-zero branch.)
	//  2. (Phase C) rollupStore.SetRollupOffset — atomic-monotone fence advance.
	//  3. (Phase C) advanceRollupOffset — idempotent derived state.
	objectID := blockstore.ComputeObjectID(blocks)
	bc.persisterMu.RLock()
	persister := bc.objectIDPersister
	bc.persisterMu.RUnlock()
	if persister != nil {
		if perr := persister(ctx, payloadID, blocks, objectID); perr != nil {
			return fmt.Errorf("rollup: ObjectIDPersister: %w", perr)
		}
	}

	// #669: populate the dedup LRU only AFTER persister confirms
	// the FileBlock rows are durable. A concurrent rollup on the same
	// payload that observes a hash here is guaranteed to find a
	// committed row when it calls AddRef. rmu serializes same-payload
	// rollups, and PutMany is keyed by (hash, payloadID) so a cross-payload
	// short-circuit cannot occur.
	if bc.dedupLRU != nil && len(blocks) > 0 {
		hashes := make([]blockstore.ContentHash, len(blocks))
		for i, b := range blocks {
			hashes[i] = b.Hash
		}
		bc.dedupLRU.PutMany(hashes, payloadID)
	}

	// ---- Phase C: commit log-side state under the re-acquired append mutex ----
	return func() error {
		mu.Lock()
		defer mu.Unlock()

		// Re-validate lf (FIX-21) and the tombstone under the re-acquired
		// mutex: a DeleteAppendLog or an AppendWrite FIX-2 recovery during
		// Phase B may have closed/replaced lf or tombstoned the payload. If
		// so, skip the log-side commit entirely — the CAS chunks and manifest
		// rows we wrote are reaped by GC / DeleteAppendLog cleanup. (The
		// DeleteAppendLog rmu drain blocks delete's metadata clear until this
		// pass returns, so it observes a consistent state afterwards.)
		bc.logsMu.RLock()
		curLf := bc.logFDs[payloadID]
		bc.logsMu.RUnlock()
		if curLf == nil || curLf != lf {
			return nil
		}
		if bc.isTombstoned(payloadID) {
			return nil
		}

		// R-7 (#580): mark every surviving record's FILE-OFFSET extent consumed
		// in the logIndex and ask the index for the new compaction fence. This
		// is the value we persist as rollup_offset — computed from the consumed
		// file-offset coverage. An entry whose file extent is fully covered by
		// some consumed record (itself OR a later overlapping one) is dead —
		// the fence walks past it on the next AdvanceFence call even if the
		// entry itself was never picked up by a rollup pass.
		//
		// Only the Phase-A records (consumedExtents) are marked; a write that
		// landed during Phase B has its own logIndex entry, NOT in
		// consumedExtents, so the fence does not advance past it and it is
		// rolled up by a later pass.
		//
		// priorFence is captured BEFORE MarkConsumed/AdvanceFence so the
		// budget-release math below uses the in-memory fence delta (truth)
		// rather than (targetPos - hdr.RollupOffset). The log header is
		// derived state that can lag if a prior advanceRollupOffset failed —
		// using it to compute reclaimed would double-decrement logBytesTotal
		// on the recovery-from-stale-header path.
		priorFence := idx.Fence()
		for _, ext := range consumedExtents {
			idx.MarkConsumed(ext.fileOff, ext.payloadLen)
		}
		// AdvanceFenceUpTo(maxReadLogPos) — NOT plain AdvanceFence — so the
		// fence never walks past a record that arrived during Phase B at a
		// file extent we just consumed (its logPos exceeds everything we read
		// in Phase A). Such a record's bytes were not chunked this pass; it
		// stays in the index + dirty tree and is rolled up by a later pass.
		targetPos := idx.AdvanceFenceUpTo(maxReadLogPos)

		// SetRollupOffset is atomic-monotone at the RollupStore layer: on
		// attempted regression it returns ErrRollupOffsetRegression and the
		// stored value is unchanged. With Direction-1 the fence is monotonic
		// by construction (newLogIndex / recovery seed + AdvanceFence
		// forward-walk), and rmu serializes rollup workers on the same
		// payload, so regression should be unreachable in steady state.
		//
		// Post-#579: after a physical compaction pass the in-memory fence
		// resets to logHeaderSize while metadata.rollup_offset retains its
		// pre-compaction high-water mark (monotonicity at the metadata
		// layer cannot be regressed). Subsequent rollup passes therefore
		// compute targetPos values BELOW the persisted metaOff and the
		// SetRollupOffset call legitimately returns ErrRollupOffsetRegression.
		// That is benign — we must NOT return early here, because the
		// derived-state header advance below is the on-disk source of
		// truth that recovery's seek() uses to position the readRecord
		// loop. Falling through to advanceRollupOffset on the regression
		// branch keeps the header in sync with the in-memory fence so a
		// post-compaction reboot replays from the correct position. The
		// LogFlagCompacted bit (set during compactLogLocked) tells recovery
		// to trust the header over metadata.
		_, serr := bc.rollupStore.SetRollupOffset(ctx, payloadID, targetPos)
		if errors.Is(serr, metadata.ErrRollupOffsetRegression) {
			slog.Debug("rollup: SetRollupOffset regression rejected (benign)",
				"payloadID", payloadID, "target", targetPos)
			// Fall through — see comment block above. The header advance
			// below MUST run so the on-disk position stays aligned with
			// the in-memory fence even when metadata refuses to.
		} else if serr != nil {
			return fmt.Errorf("rollup: SetRollupOffset: %w", serr)
		}

		// Derived-state: advance the log header. If this fails, metadata is
		// already the source of truth and recovery will reconcile
		// — except when LogFlagCompacted is set (post-compaction state)
		// in which case the header IS the truth and a failure here means
		// the next boot may re-replay records that were already chunked.
		// CAS is idempotent so that is benign correctness-wise (only a
		// throughput hit).
		if aerr := advanceRollupOffset(lf.f, targetPos); aerr != nil {
			slog.Warn("rollup: advanceRollupOffset failed; recovery will reconcile",
				"payloadID", payloadID, "error", aerr)
		}

		// Consume the dirty interval(s) this rollup just covered and release
		// the corresponding log budget. C1: ConsumeUpToStable (not plain
		// ConsumeUpTo) preserves any interval a write created during Phase B
		// (Touched > phaseStart), so its bytes are rolled up by a later pass
		// rather than silently dropped. Reclaimed bytes are measured against
		// the in-memory priorFence, NOT hdr.RollupOffset — the log header is
		// derived state that may lag the metadata-authoritative fence after a
		// prior advanceRollupOffset failure, and basing reclaimed on the stale
		// header would double-decrement budget on the next pass.
		tree.ConsumeUpToStable(stableEnd, phaseStart)
		if targetPos > priorFence {
			bc.logBytesTotal.Add(-int64(targetPos - priorFence))
		}

		// Non-blocking signal to unblock any AppendWrite waiting on pressure.
		select {
		case bc.pressureCh <- struct{}{}:
		default:
		}

		// #579: physical log compaction. Runs under the append mutex (mu) we
		// hold here in Phase C. The threshold check is internal to
		// maybeCompactLog, so this call is cheap when the fence has not
		// advanced enough to warrant a rewrite. Errors are logged at Warn and
		// otherwise swallowed — a failed compaction pass leaves the original
		// log untouched; the next rollup pass retries automatically. rf was
		// already closed at the end of Phase A, so no open read handle blocks
		// the atomic rename on Windows.
		if cerr := bc.maybeCompactLog(ctx, payloadID, lf, idx); cerr != nil {
			slog.Warn("rollup: compaction failed; will retry next pass",
				"payloadID", payloadID, "error", cerr)
		}

		return nil
	}()
}

// maxReconstructBytes caps the in-memory reconstruction buffer at 16 GiB
// (FIX-5). A pathological set of sparse records spread across a huge file-
// offset span could otherwise force an arbitrarily large allocation. Above
// the cap reconstructStream returns an error and the caller skips the rollup
// pass without persisting any state. A var (not const) so tests can lower
// the ceiling to exercise the refusal path without allocating gigabytes.
// Tests that mutate this MUST defer-restore it and MUST NOT run with
// t.Parallel() or alongside a started rollup worker pool — rollup workers
// read it concurrently, so a mutation under either condition races.
var maxReconstructBytes = uint64(1) << 34

// rollupPhaseBHook, when non-nil, is invoked at the start of rollupFile's
// Phase B — after the append mutex has been released and before any CAS store.
// Production leaves it nil; C1 concurrency tests set it (and MUST defer-restore
// it to nil) to deterministically interleave a racing AppendWrite. It must not
// be set while a rollup worker pool is running on an unrelated test.
var rollupPhaseBHook func(payloadID string)

// reconstructStream flattens records by file offset, later writes overwriting
// earlier ones at the same offset. Produces a contiguous byte slice anchored
// at baseOff (the smallest record offset): buf[i] holds file byte baseOff+i,
// with untouched gaps inside the window zero-filled. Returns (buf, baseOff).
//
// Why baseOff-anchored (not file byte 0): the only consumer is the FastCDC
// chunker, which is content-defined — Chunker.Next computes its gear hash
// from data[0] of the slice it is handed, independent of the bytes' absolute
// file position. The rollup feeds it stream[i:] starting at the first real
// byte, so anchoring the backing array at baseOff yields byte-identical
// chunker input as a file-0-anchored buffer would, while not allocating the
// dead [0,baseOff) prefix (the dominant rollup allocation on large/append-
// heavy files). Chunk boundaries — and therefore dedup — are unchanged.
//
// FIX-5 — DoS guard: returns an error when the span (maxEnd-baseOff) exceeds
// maxReconstructBytes. The caller (rollupFile) treats the error as benign at
// the rollup-pass level: log a warning and return without persisting derived
// state. The dirty intervals stay in the tree so a later pass — possibly
// after a TruncateAppendLog or a partial drain — has another chance.
func reconstructStream(recs []rec) ([]byte, uint64, error) {
	if len(recs) == 0 {
		return nil, 0, nil
	}
	baseOff := recs[0].off
	var maxEnd uint64
	for _, r := range recs {
		if r.off < baseOff {
			baseOff = r.off
		}
		end := r.off + uint64(len(r.payload))
		if end > maxEnd {
			maxEnd = end
		}
	}
	span := maxEnd - baseOff
	if span > maxReconstructBytes {
		return nil, 0, fmt.Errorf("rollup: reconstruct would require %d bytes, exceeds %d ceiling", span, maxReconstructBytes)
	}
	// Pooled, pre-zeroed buffer covering [baseOff, maxEnd) — the caller MUST
	// release it via putReconstructBuf once the rollup pass completes. Zero-
	// fill is load-bearing: untouched gaps inside the window stay zero.
	buf := getReconstructBuf(span)
	// Apply records in input (log) order so that "last record wins"
	// at the same offset holds — the mutex-serialized log order is the
	// authoritative ordering for same-offset overwrites.
	for _, r := range recs {
		copy(buf[r.off-baseOff:], r.payload)
	}
	return buf, baseOff, nil
}

// alignRecsToChunkBoundaries expands the record set so the reconstructed
// window covers WHOLE existing CAS chunks at its edges (#953). It finds
// the existing FileBlock rows that straddle the dirty window's start or
// end — i.e. a chunk [off, off+size) with off < windowStart < off+size or
// off < windowEnd < off+size — reads those chunks from the local CAS, and
// prepends them as synthetic records at their original offset. Prepending
// (rather than appending) keeps the real overwrite records LAST so
// reconstructStream's "last record wins" rule still lets the overwrite
// overlay the bytes the client actually rewrote, while the straddling
// chunk supplies its non-overwritten head/tail.
//
// Returns recs unchanged when there is no FileBlock store, no rows, or no
// straddling chunk is locally readable. The function never returns an
// error: an unreadable straddling chunk is a benign best-effort miss (the
// caller's reap stays conservative on that edge).
func (bc *FSStore) alignRecsToChunkBoundaries(ctx context.Context, payloadID string, recs []rec, trunc uint64, hasTrunc bool) []rec {
	if bc.blockStore == nil || len(recs) == 0 {
		return recs
	}
	var windowStart, windowEnd uint64
	windowStart = recs[0].off
	for _, r := range recs {
		if r.off < windowStart {
			windowStart = r.off
		}
		end := r.off + uint64(len(r.payload))
		if end > windowEnd {
			windowEnd = end
		}
	}

	rows, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil || len(rows) == 0 {
		return recs
	}

	var prepend []rec
	for _, fb := range rows {
		if fb == nil || fb.Hash.IsZero() || fb.DataSize == 0 {
			continue
		}
		off, ok := blockstore.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		// A pending truncation already removed bytes at/after trunc this
		// pass. A neighbor chunk starting at/after trunc is gone; one that
		// straddles trunc must only contribute its surviving head [off,
		// trunc). Splicing past trunc would re-emit truncated tail data.
		dataSize := uint64(fb.DataSize)
		if hasTrunc {
			if off >= trunc {
				continue
			}
			if off+dataSize > trunc {
				dataSize = trunc - off
			}
		}
		end := off + dataSize
		// Only chunks that STRADDLE an edge of the window need splicing.
		// A chunk fully inside the window is superseded (reaped later); a
		// chunk fully outside is untouched and must NOT be rewritten.
		straddlesStart := off < windowStart && windowStart < end
		straddlesEnd := off < windowEnd && windowEnd < end
		if !straddlesStart && !straddlesEnd {
			continue
		}
		data, gerr := bc.Get(ctx, fb.Hash)
		if gerr != nil {
			// Not locally readable (evicted) — skip; reap stays
			// conservative on this edge.
			continue
		}
		// Clamp to the surviving byte count so a padded on-disk surface
		// (or a truncation boundary) never injects bytes past it.
		if dataSize < uint64(len(data)) {
			data = data[:dataSize]
		}
		// Copy: bc.Get may return an LRU-owned/shared buffer; the
		// reconstruction buffer must not alias it.
		buf := make([]byte, len(data))
		copy(buf, data)
		prepend = append(prepend, rec{off: off, payload: buf})
	}
	if len(prepend) == 0 {
		return recs
	}
	// Synthetic straddling-chunk records FIRST, real overwrite records
	// LAST — preserves "last record wins" so the overwrite overlays only
	// the bytes it actually rewrote.
	return append(prepend, recs...)
}

// blake3ContentHash returns the 32-byte BLAKE3 hash of data as a
// blockstore.ContentHash. Matches the rollup's content-address contract
// chunks in blocks/{hh}/{hh}/{hex} are keyed by BLAKE3(data).
func blake3ContentHash(data []byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	sum := blake3.Sum256(data)
	copy(h[:], sum[:])
	return h
}

// isTombstoned reports whether payloadID has been marked for deletion by
// DeleteAppendLog. Through plans up to 09, bc.tombstones
// is nil and this always returns false — which is correct, because no
// deletion path exists yet.
func (bc *FSStore) isTombstoned(payloadID string) bool {
	bc.logsMu.RLock()
	defer bc.logsMu.RUnlock()
	if bc.tombstones == nil {
		return false
	}
	_, ok := bc.tombstones[payloadID]
	return ok
}
