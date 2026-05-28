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
			if err := bc.rollupFile(ctx, pid); err != nil {
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
		if err := bc.rollupFile(ctx, pid); err != nil {
			slog.Error("rollupFile failed",
				"payloadID", pid, "error", err, "source", "ticker")
		}
	}
}

// rollupFile consumes the earliest stable interval for payloadID, emits
// chunks, and commits atomically.
//
// Concurrency: the per-file mutex (`mu`) is held through the ENTIRE
// reconstructStream -> chunker -> StoreChunk -> CommitChunks sequence.
// Releasing the mutex before StoreChunk/CommitChunks would break
// DeleteAppendLog's ability to wait for in-flight rollup by acquiring
// the same mutex.
// Holding the mutex serializes same-file rollup against same-file
// AppendWrite; this is acceptable because (a) rollup runs in a background
// worker pool, (b) AppendWrite's mutex window is ~5 µs under
// ordinary load, (c) different files have independent mutexes so
// one file's rollup never stalls another, and (d) pressure-channel
// blocking only trips when the log budget is exceeded.
//
// CommitChunks atomic ordering:
//  1. For each emitted chunk: StoreChunk(hash, data) — idempotent, fsynced.
//  2. ObjectIDPersister(payloadID, blocks, objectID) — writes per-chunk
//     FileBlock manifest rows + FileAttr.Blocks. MUST land before
//     SetRollupOffset so a manifest-persist failure leaves rollup_offset
//     UNCHANGED and the next pass retries; the alternative (persister
//     after SetRollupOffset) silently lost the manifest while
//     rollup_offset already advanced past the records.
//  3. rollupStore.SetRollupOffset(payloadID, targetPos) — atomic-monotone
//     on ErrRollupOffsetRegression, log at Debug and return nil (benign).
//  4. advanceRollupOffset(logFile, targetPos) — idempotent derived-state.
//     If it fails, metadata is already the source of truth and recovery
//
// will reconcile the header on next boot.
//  5. tree.ConsumeUpTo + logBytesTotal.Add(-reclaimed) + pressureCh signal.
func (bc *FSStore) rollupFile(ctx context.Context, payloadID string) error {
	if bc.isClosed() {
		return nil
	}

	bc.logsMu.RLock()
	lf := bc.logFDs[payloadID]
	tree := bc.dirtyIntervals[payloadID]
	mu := bc.logLocks[payloadID]
	idx := bc.logIndices[payloadID]
	bc.logsMu.RUnlock()
	if lf == nil || tree == nil || mu == nil || idx == nil {
		// Nothing to do — payload never had an AppendWrite (or was
		// cleared by Close/DeleteAppendLog).
		return nil
	}

	stabilization := time.Duration(bc.stabilizationMS) * time.Millisecond

	mu.Lock()
	defer mu.Unlock()

	// FIX-21: re-validate lf under the per-file mutex. Between the
	// snapshot above (RUnlock'd before we took mu) and now, a concurrent
	// AppendWrite that hit a writeRecord error could have run the FIX-2
	// recovery path: removed the lf from bc.logFDs and closed lf.f. If we
	// proceed with the stale lf we'd read from a closed fd. The
	// double-checked re-read under the per-file mutex closes that window
	// any FIX-2 cleanup must serialize behind us via mu, and any
	// already-completed cleanup will have left bc.logFDs[payloadID] nil
	// (or replaced with a fresh lf on a subsequent getOrCreateLog). A
	// stale rollup pass is benign — chunks will be retried on the next
	// pass once the new lf is established.
	bc.logsMu.RLock()
	curLf := bc.logFDs[payloadID]
	bc.logsMu.RUnlock()
	if curLf == nil || curLf != lf {
		return nil
	}

	// Tombstone re-check AFTER mutex acquire. DeleteAppendLog
	// sets the tombstone BEFORE acquiring this mutex; if we
	// it here, delete is in flight (or completed) and we must bail out
	// without persisting rollup state for a dead payload. The tombstones
	// map may be nil through plans up to, which is fine
	// ranging over a nil map is legal and the check becomes a no-op.
	if bc.isTombstoned(payloadID) {
		return nil
	}

	stable, ok := tree.EarliestStable(time.Now(), stabilization)
	if !ok {
		return nil
	}

	// Read + validate the log header. Direction-1 no longer uses
	// hdr.RollupOffset for any computation (the in-memory idx.Fence is
	// the truth), but the read still serves as a corruption sanity
	// check before we issue preads against the same fd.
	if _, err := readLogHeader(lf.f); err != nil {
		slog.Warn("rollup: read header", "payloadID", payloadID, "error", err)
		return err
	}

	// Direction-1 rollup redesign: consult the per-payload logIndex to
	// translate the stable file-offset interval into a set of records
	// whose frames sit somewhere in the on-disk log (not necessarily
	// contiguous at the head). Each entry carries logPos + payloadLen
	// so we pread each frame directly instead of scanning the log
	// sequentially. A separate read-only fd is used so we don't disturb
	// lf.f's append position.
	stableEnd := stable.Offset + uint64(stable.Length)
	entries := idx.EntriesForInterval(stable.Offset, uint64(stable.Length))
	if len(entries) == 0 {
		// The tree reported a stable interval the logIndex cannot back —
		// only possible under a tree/logIndex divergence (e.g. recovery
		// rebuilt the tree from records the index missed, or a residual
		// dirty interval was left behind by an interrupted write before
		// AppendWrite's atomic tree+logIndex update landed).
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
		return nil
	}

	rf, err := os.Open(lf.path)
	if err != nil {
		return fmt.Errorf("rollup: open log for read: %w", err)
	}
	// rfClosed lets the error-path defer skip the close once the
	// explicit close below runs. We must close rf before maybeCompactLog
	// fires at function tail — Windows refuses to rename over a path
	// that has ANY open handle, including this read-only one.
	rfClosed := false
	defer func() {
		if !rfClosed {
			_ = rf.Close()
		}
	}()

	// consumedExtents tracks the FILE-OFFSET extent of every record that
	// will be marked consumed in the logIndex below. R-7 (#580): consumption
	// is keyed by file-offset interval, not logPos — so the rollup records
	// (fileOff, payloadLen) per record instead of the logPos.
	type consumedExt struct {
		fileOff    uint64
		payloadLen uint32
	}
	recs := make([]rec, 0, len(entries))
	consumedExtents := make([]consumedExt, 0, len(entries))
	for _, e := range entries {
		off, payload, rerr := readRecordAt(rf, e.logPos, e.payloadLen)
		if rerr != nil {
			// A CRC mismatch or pread failure at a record the logIndex
			// claims is valid implies log-fd corruption or a logIndex/
			// log divergence bug. Returning nil here would re-queue the
			// interval forever — surface a hard error instead so the
			// worker pool's error log captures the divergence and
			// operators can inspect the log fd.
			return fmt.Errorf("rollup: readRecordAt(logPos=%d): %w", e.logPos, rerr)
		}
		if off != e.fileOff {
			return fmt.Errorf("rollup: logIndex fileOff divergence at logPos=%d (indexed=%d frame=%d)",
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
	trunc, hasTrunc := bc.truncations[payloadID]
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
		return nil
	}

	// Reconstruct contiguous byte stream ("later record wins") + chunk
	// + store. Chunker is stateless across calls; we feed it the whole
	// reconstructed buffer and slice out chunks by the returned boundaries.
	//
	// FIX-3: the buffer is indexed by FILE OFFSET (zero-padded for untouched
	// gaps below minOff), so we start the chunker at minOff — that is the
	// first byte that actually belongs to a record this pass. Indexing the
	// buffer by file offset is what makes chunk boundaries stable across
	// rollup passes (FastCDC gear-hash masks are buffer-position-keyed).
	//
	// FIX-5: reconstructStream may refuse to allocate when maxEnd exceeds
	// the 16 GiB ceiling. Treat that as benign at the rollup-pass level
	// log + return nil without persisting derived state. The dirty
	// intervals stay in the tree so a later pass (possibly after a
	// TruncateAppendLog) has another chance.
	stream, rerr := reconstructStream(recs)
	if rerr != nil {
		slog.Warn("rollup: reconstruct refused", "payloadID", payloadID, "error", rerr)
		return nil
	}
	minOff := minRecOffset(recs)
	ck := chunker.NewChunker()
	pos := minOff
	// Accumulate the BlockRef manifest as chunks are emitted. Sorted-by-
	// Offset is automatic because pos advances monotonically — that
	// matches the canonical FileAttr.Blocks invariant the downstream
	// ObjectID compute relies on.
	var blocks []blockstore.BlockRef
	// #669: collect hashes that StoreChunk wrote this pass, plus
	// hashes the LRU hit path successfully bumped. Both are pushed into
	// the LRU AFTER the ObjectIDPersister callback below confirms the
	// FileBlock rows are durable. Populating the LRU before persister
	// success let a concurrent rollup on the same payload observe the
	// hash via the LRU and call AddRef before the row existed →
	// ErrUnknownHash storm under load (and a silent retry loop).
	var pendingLRUPuts []blockstore.ContentHash
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
			Offset: pos,
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
		if bc.dedupLRU != nil && bc.dedupLRU.Get(h, payloadID) {
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

		// Queue the hash for LRU population after persister success.
		// Either path is valid: a successful AddRef proves the row
		// already exists for this (hash, payloadID); StoreChunk plus the
		// persister write below is what makes a fresh row durable.
		pendingLRUPuts = append(pendingLRUPuts, h)
		blocks = append(blocks, blockRef)
		pos += uint64(b)
	}

	// Tombstone re-check IMMEDIATELY before metadata commit.
	// Even if a delete raced between the first check and now, we must not
	// persist rollup_offset for a deleted payload. Content-addressed chunks
	// in blocks/ are swept by GC.
	if bc.isTombstoned(payloadID) {
		return nil
	}

	// R-7 (#580): mark every surviving record's FILE-OFFSET extent consumed
	// in the logIndex and ask the index for the new compaction fence. This
	// is the value we persist as rollup_offset — semantically the same on-
	// disk format, but computed from the consumed file-offset coverage
	// instead of a logPos-keyed consumption set. An entry whose file extent
	// is fully covered by some consumed record (itself OR a later
	// overlapping one) is dead — the fence walks past it on the next
	// AdvanceFence call even if the entry itself was never picked up by a
	// rollup pass.
	//
	// priorFence is captured BEFORE MarkConsumed/AdvanceFence so the
	// budget-release math at the bottom uses the in-memory fence delta
	// (truth) rather than (targetPos - hdr.RollupOffset). The log header
	// is derived state that can lag if a prior advanceRollupOffset
	// failed — using it to compute reclaimed would double-decrement
	// logBytesTotal on the recovery-from-stale-header path.
	priorFence := idx.Fence()
	for _, ext := range consumedExtents {
		idx.MarkConsumed(ext.fileOff, ext.payloadLen)
	}
	targetPos := idx.AdvanceFence()

	// CommitChunks atomic sequence. The on-disk CAS chunks are already
	// durable from StoreChunk above. The remaining commit steps must
	// land in this order so a partial failure never advances
	// rollup_offset past records whose FileBlock manifest rows haven't
	// been persisted:
	//
	//  1. ObjectIDPersister(payloadID, blocks, objectID) — writes the
	//     per-chunk FileBlock rows AND the FileAttr.Blocks manifest +
	//     ObjectID. If this fails, rollup_offset stays UNCHANGED and the
	//     next rollup pass retries — chunks are content-addressed and
	//     idempotent on re-store. (Persister-after-SetRollupOffset is
	//     the wrong ordering: a persister failure leaves rollup_offset
	//     advanced past records whose manifest never landed, and the
	//     engine read path falls into the sparse-zero branch.)
	//  2. rollupStore.SetRollupOffset(payloadID, targetPos) — atomic-
	//     monotone metadata-authoritative fence advance.
	//  3. advanceRollupOffset(logFile, targetPos) — idempotent derived
	//     state.
	objectID := blockstore.ComputeObjectID(blocks)
	bc.persisterMu.RLock()
	persister := bc.objectIDPersister
	bc.persisterMu.RUnlock()
	if persister != nil {
		if err := persister(ctx, payloadID, blocks, objectID); err != nil {
			return fmt.Errorf("rollup: ObjectIDPersister: %w", err)
		}
	}

	// #669: populate the dedup LRU only AFTER persister confirms
	// the FileBlock rows are durable. A concurrent rollup on the same
	// payload that observes a hash here is guaranteed to find a
	// committed row when it calls AddRef. PutMany is keyed by
	// (hash, payloadID) so cross-payload short-circuit cannot occur.
	if bc.dedupLRU != nil && len(pendingLRUPuts) > 0 {
		bc.dedupLRU.PutMany(pendingLRUPuts, payloadID)
	}

	// SetRollupOffset is atomic-monotone at the RollupStore layer: on
	// attempted regression it returns ErrRollupOffsetRegression and the
	// stored value is unchanged. With Direction-1 the fence is monotonic
	// by construction (newLogIndex / recovery seed + AdvanceFence
	// forward-walk), and the per-file mu serializes rollup workers on the
	// same payload, so regression should be unreachable in steady state.
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
	_, err = bc.rollupStore.SetRollupOffset(ctx, payloadID, targetPos)
	if errors.Is(err, metadata.ErrRollupOffsetRegression) {
		slog.Debug("rollup: SetRollupOffset regression rejected (benign)",
			"payloadID", payloadID, "target", targetPos)
		// Fall through — see comment block above. The header advance
		// below MUST run so the on-disk position stays aligned with
		// the in-memory fence even when metadata refuses to.
	} else if err != nil {
		return fmt.Errorf("rollup: SetRollupOffset: %w", err)
	}

	// Derived-state: advance the log header. If this fails, metadata is
	// already the source of truth and recovery will reconcile
	// — except when LogFlagCompacted is set (post-compaction state)
	// in which case the header IS the truth and a failure here means
	// the next boot may re-replay records that were already chunked.
	// CAS is idempotent so that is benign correctness-wise (only a
	// throughput hit).
	if err := advanceRollupOffset(lf.f, targetPos); err != nil {
		slog.Warn("rollup: advanceRollupOffset failed; recovery will reconcile",
			"payloadID", payloadID, "error", err)
	}

	// Consume the dirty interval(s) this rollup just covered and release
	// the corresponding log budget. Reclaimed bytes are measured against
	// the in-memory priorFence, NOT hdr.RollupOffset — the log header is
	// derived state that may lag the metadata-authoritative fence after
	// a prior advanceRollupOffset failure, and basing reclaimed on the
	// stale header would double-decrement budget on the next pass.
	tree.ConsumeUpTo(stableEnd)
	if targetPos > priorFence {
		bc.logBytesTotal.Add(-int64(targetPos - priorFence))
	}

	// Non-blocking signal to unblock any AppendWrite waiting on pressure.
	select {
	case bc.pressureCh <- struct{}{}:
	default:
	}

	// #579: physical log compaction. Runs under the per-file mu we
	// still hold (deferred Unlock above). The threshold check is
	// internal to maybeCompactLog, so this call is cheap when the
	// fence has not advanced enough to warrant a rewrite. Errors are
	// logged at Warn and otherwise swallowed — a failed compaction
	// pass leaves the original log untouched; the next rollup pass
	// retries automatically.
	//
	// Close rf first: Windows refuses to rename over a file with an
	// open handle, and compaction's atomic rename would otherwise fail
	// with ERROR_SHARING_VIOLATION. On POSIX the close is harmless
	// rf is otherwise unused past this point.
	_ = rf.Close()
	rfClosed = true
	if cerr := bc.maybeCompactLog(ctx, payloadID, lf, idx); cerr != nil {
		slog.Warn("rollup: compaction failed; will retry next pass",
			"payloadID", payloadID, "error", cerr)
	}

	return nil
}

// maxReconstructBytes caps the in-memory reconstruction buffer at 16 GiB
// (FIX-5). A pathological set of records could otherwise force an
// arbitrarily large allocation. Above the cap reconstructStream returns an
// error and the caller skips the rollup pass without persisting any state.
const maxReconstructBytes = uint64(1) << 34

// reconstructStream flattens records by absolute file_offset, later writes
// overwriting earlier ones at the same offset. Produces a contiguous
// byte slice starting at FILE BYTE 0 (NOT minOff), extending to the maximum
// record end.
//
// FIX-3 — chunker boundary stability: FastCDC gear-hash masks are
// buffer-position-keyed. If two sequential rollup passes reconstructed
// overlapping windows starting at different minOff values, identical bytes
// would land at different buffer positions across passes and the chunker
// would emit different content boundaries → no dedup across rollups (breaks
// ). Anchoring the buffer at file byte 0 (with zero-padded gaps for
// untouched regions) guarantees the chunker sees the same prefix bytes for
// the same file region every pass, so chunk boundaries are stable.
//
// FIX-5 — DoS guard: returns an error when maxEnd exceeds
// maxReconstructBytes (16 GiB). The caller (rollupFile) treats the error
// as benign at the rollup-pass level: log a warning and return without
// persisting derived state. The dirty intervals stay in the tree so a
// later pass — possibly after a TruncateAppendLog or a partial drain —
// has another chance.
//
// The caller is responsible for starting the chunker at minOff (not 0) so
// only the relevant suffix is processed. Callers that ALSO want the minOff
// can derive it from recs themselves; reconstructStream only returns the
// file-offset-indexed buffer.
func reconstructStream(recs []rec) ([]byte, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	var maxEnd uint64
	for _, r := range recs {
		end := r.off + uint64(len(r.payload))
		if end > maxEnd {
			maxEnd = end
		}
	}
	if maxEnd > maxReconstructBytes {
		return nil, fmt.Errorf("rollup: reconstruct would require %d bytes, exceeds %d ceiling", maxEnd, maxReconstructBytes)
	}
	buf := make([]byte, maxEnd)
	// Apply records in input (log) order so that "last record wins"
	// at the same offset holds — the mutex-serialized log order is the
	// authoritative ordering for same-offset overwrites.
	for _, r := range recs {
		copy(buf[r.off:], r.payload)
	}
	return buf, nil
}

// minRecOffset returns the smallest file offset across recs. Caller-side
// helper used by rollupFile to position the chunker after the buffer
// has been built file-offset-indexed (FIX-3).
func minRecOffset(recs []rec) uint64 {
	min := recs[0].off
	for _, r := range recs[1:] {
		if r.off < min {
			min = r.off
		}
	}
	return min
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
