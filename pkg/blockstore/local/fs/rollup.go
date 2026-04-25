package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// framed bytes (i.e., where the next record would start). FIX-19:
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

// StartRollup launches the chunkRollup worker pool (D-13). Idempotent;
// subsequent calls after the first are no-ops. Requires useAppendLog=true
// and a non-nil RollupStore.
//
// Workers join on Close() via bc.rollupWg; see Close in fs.go.
func (bc *FSStore) StartRollup(ctx context.Context) error {
	if !bc.useAppendLog {
		return ErrAppendLogDisabled
	}
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
// get processed. D-15 three-arm select guarantees no leaks on Close or ctx
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
			_ = bc.rollupFile(ctx, pid)
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
		_ = bc.rollupFile(ctx, pid)
	}
}

// rollupFile consumes the earliest stable interval for payloadID, emits
// chunks, and commits atomically per D-12.
//
// Concurrency: the per-file mutex (`mu`) is held through the ENTIRE
// reconstructStream -> chunker -> StoreChunk -> CommitChunks sequence.
// This is the fix for the plan-checker's Blocker 3: releasing the mutex
// before StoreChunk/CommitChunks would break DeleteAppendLog's ability
// (plan 09) to wait for in-flight rollup by acquiring the same mutex.
// Holding the mutex serializes same-file rollup against same-file
// AppendWrite; this is acceptable because (a) rollup runs in a background
// worker pool (D-13), (b) AppendWrite's mutex window is ~5 µs under
// ordinary load (D-32), (c) different files have independent mutexes so
// one file's rollup never stalls another, and (d) pressure-channel
// blocking only trips when the log budget is exceeded.
//
// CommitChunks atomic ordering (D-12):
//  1. For each emitted chunk: StoreChunk(hash, data) — idempotent, fsynced.
//  2. rollupStore.SetRollupOffset(payloadID, targetPos) — atomic-monotone;
//     on ErrRollupOffsetRegression, log at Debug and return nil (benign).
//  3. advanceRollupOffset(logFile, targetPos) — idempotent derived-state.
//     If it fails, metadata is already the source of truth and recovery
//     (plan 07) will reconcile the header on next boot.
//  4. tree.ConsumeUpTo + logBytesTotal.Add(-reclaimed) + pressureCh signal.
func (bc *FSStore) rollupFile(ctx context.Context, payloadID string) error {
	if bc.isClosed() {
		return nil
	}

	bc.logsMu.RLock()
	lf := bc.logFDs[payloadID]
	tree := bc.dirtyIntervals[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if lf == nil || tree == nil || mu == nil {
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
		return nil
	}

	// Tombstone re-check AFTER mutex acquire (Blocker 3). DeleteAppendLog
	// (plan 09) sets the tombstone BEFORE acquiring this mutex; if we see
	// it here, delete is in flight (or completed) and we must bail out
	// without persisting rollup state for a dead payload. The tombstones
	// map may be nil through Phase 10 plans up to plan 09, which is fine:
	// ranging over a nil map is legal and the check becomes a no-op.
	if bc.isTombstoned(payloadID) {
		return nil
	}

	stable, ok := tree.EarliestStable(time.Now(), stabilization)
	if !ok {
		return nil
	}

	// Read the log header to learn the current rollup_offset.
	hdr, err := readLogHeader(lf.f)
	if err != nil {
		slog.Warn("rollup: read header", "payloadID", payloadID, "error", err)
		return err
	}

	// Scan records from rollup_offset onward using a SEPARATE read-only fd
	// so we don't disturb lf.f's append position (getOrCreateLog seeks it
	// to EOF). The reader is closed before the method returns.
	rf, err := os.Open(lf.path)
	if err != nil {
		return fmt.Errorf("rollup: open log for read: %w", err)
	}
	defer func() { _ = rf.Close() }()
	if _, err := rf.Seek(int64(hdr.RollupOffset), io.SeekStart); err != nil {
		return fmt.Errorf("rollup: seek: %w", err)
	}

	stableEnd := stable.Offset + uint64(stable.Length)

	var recs []rec
	currentPos := hdr.RollupOffset
	targetPos := currentPos
	for {
		off, payload, rok, rerr := readRecord(rf)
		if rerr != nil {
			return fmt.Errorf("rollup: readRecord: %w", rerr)
		}
		if !rok {
			break // clean EOF / torn tail — recovery handles truncation
		}
		recSize := uint64(recordFrameOverhead) + uint64(len(payload))
		nextPos := currentPos + recSize

		// Record entirely before the stable interval — already covered by
		// an earlier rollup (shouldn't normally happen since we started at
		// hdr.RollupOffset, but be defensive). Advance targetPos past it.
		if off+uint64(len(payload)) <= stable.Offset {
			targetPos = nextPos
			currentPos = nextPos
			continue
		}
		// Record entirely past the stable interval — stop; later records
		// belong to a future pass.
		if off >= stableEnd {
			break
		}
		// Record intersects the stable interval — include it.
		recs = append(recs, rec{off: off, payload: payload, endPos: nextPos})
		targetPos = nextPos
		currentPos = nextPos
	}

	// D-29 truncation filter: drop records entirely past the truncation
	// boundary and clip straddling records. Recorded by TruncateAppendLog
	// (plan 09); consulted here so emitted chunks never contain bytes
	// beyond the client-observed size at truncate time.
	//
	// FIX-19: when truncation drops records, targetPos was already advanced
	// past their on-disk frames during the scan above. Recompute targetPos
	// from the filtered set so SetRollupOffset never claims to have
	// committed bytes for records that produced no chunks. The cap is the
	// pre-filter targetPos so we don't accidentally walk BACKWARDS past a
	// previously-scanned "skipped" record (the early-pre-stable arm above
	// also bumps targetPos but never appends to recs).
	bc.logsMu.RLock()
	trunc, hasTrunc := bc.truncations[payloadID]
	bc.logsMu.RUnlock()
	if hasTrunc {
		preFilterTarget := targetPos
		filtered := recs[:0]
		for _, r := range recs {
			if r.off >= trunc {
				continue
			}
			end := r.off + uint64(len(r.payload))
			if end > trunc {
				r.payload = r.payload[:trunc-r.off]
			}
			filtered = append(filtered, r)
		}
		recs = filtered
		// Recompute targetPos based on the LAST RECORD ACTUALLY in recs.
		// If everything was filtered out, fall through to the len(recs)==0
		// short-circuit below; targetPos stays unchanged so we don't
		// regress the offset (irrelevant since we won't call
		// SetRollupOffset).
		if len(recs) > 0 {
			lastEnd := recs[0].endPos
			for _, r := range recs[1:] {
				if r.endPos > lastEnd {
					lastEnd = r.endPos
				}
			}
			if lastEnd < preFilterTarget {
				targetPos = lastEnd
			}
		}
	}

	if len(recs) == 0 {
		return nil
	}

	// Reconstruct contiguous byte stream (D-35 "later record wins") + chunk
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
	// the 16 GiB ceiling. Treat that as benign at the rollup-pass level:
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
	for pos < uint64(len(stream)) {
		b, _ := ck.Next(stream[pos:], true)
		if b <= 0 {
			// Defensive: chunker should always emit ≥1 byte when final=true
			// and data is non-empty; break to avoid an infinite loop.
			break
		}
		chunkBytes := stream[pos : pos+uint64(b)]
		h := blake3ContentHash(chunkBytes)
		if err := bc.StoreChunk(ctx, h, chunkBytes); err != nil {
			return fmt.Errorf("rollup: StoreChunk: %w", err)
		}
		pos += uint64(b)
	}

	// Tombstone re-check IMMEDIATELY before metadata commit (Blocker 3).
	// Even if a delete raced between the first check and now, we must not
	// persist rollup_offset for a deleted payload. Content-addressed chunks
	// in blocks/ are swept by Phase 11 GC.
	if bc.isTombstoned(payloadID) {
		return nil
	}

	// CommitChunks atomic sequence (D-12). SetRollupOffset is atomic-monotone
	// at the RollupStore layer: on attempted regression it returns
	// ErrRollupOffsetRegression and the stored value is unchanged. We treat
	// that as benign (another worker raced ahead) and return nil.
	_, err = bc.rollupStore.SetRollupOffset(ctx, payloadID, targetPos)
	if errors.Is(err, metadata.ErrRollupOffsetRegression) {
		slog.Debug("rollup: SetRollupOffset regression rejected (benign — another worker advanced past us)",
			"payloadID", payloadID, "target", targetPos)
		return nil
	}
	if err != nil {
		return fmt.Errorf("rollup: SetRollupOffset: %w", err)
	}

	// Derived-state: advance the log header. If this fails, metadata is
	// already the source of truth and recovery (plan 07) will reconcile.
	if err := advanceRollupOffset(lf.f, targetPos); err != nil {
		slog.Warn("rollup: advanceRollupOffset failed; recovery will reconcile",
			"payloadID", payloadID, "error", err)
	}

	// Consume the dirty interval(s) this rollup just covered and release
	// the corresponding log budget.
	reclaimed := int64(targetPos - hdr.RollupOffset)
	tree.ConsumeUpTo(stableEnd)
	if reclaimed > 0 {
		bc.logBytesTotal.Add(-reclaimed)
	}

	// Non-blocking signal to unblock any AppendWrite waiting on pressure.
	select {
	case bc.pressureCh <- struct{}{}:
	default:
	}

	return nil
}

// maxReconstructBytes caps the in-memory reconstruction buffer at 16 GiB
// (FIX-5). A pathological set of records could otherwise force an
// arbitrarily large allocation. Above the cap reconstructStream returns an
// error and the caller skips the rollup pass without persisting any state.
const maxReconstructBytes = uint64(1) << 34

// reconstructStream flattens records by absolute file_offset, later writes
// overwriting earlier ones at the same offset (D-35). Produces a contiguous
// byte slice starting at FILE BYTE 0 (NOT minOff), extending to the maximum
// record end.
//
// FIX-3 — chunker boundary stability: FastCDC gear-hash masks are
// buffer-position-keyed. If two sequential rollup passes reconstructed
// overlapping windows starting at different minOff values, identical bytes
// would land at different buffer positions across passes and the chunker
// would emit different content boundaries → no dedup across rollups (breaks
// D-21). Anchoring the buffer at file byte 0 (with zero-padded gaps for
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
// can derive it from recs themselves; this function only returns the
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
	// Apply records in input (log) order so that D-35 "last record wins"
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
// (D-05/D-06): chunks in blocks/{hh}/{hh}/{hex} are keyed by BLAKE3(data).
func blake3ContentHash(data []byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	sum := blake3.Sum256(data)
	copy(h[:], sum[:])
	return h
}

// isTombstoned reports whether payloadID has been marked for deletion by
// DeleteAppendLog (plan 09). Through Phase 10 plans up to 09, bc.tombstones
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
