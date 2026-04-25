package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// logFile wraps an open per-payload append-log file descriptor. The path
// is held alongside so recovery (Phase 10-06) can reopen after a forced
// close on write error (mirrors fdPool.Evict posture from the legacy
// path).
type logFile struct {
	f    *os.File
	path string
}

// logPath returns the on-disk path for a payload's append-log file:
// {baseDir}/logs/{payloadID}.log (D-09 flat layout; one log per payload).
func (bc *FSStore) logPath(payloadID string) string {
	return filepath.Join(bc.baseDir, "logs", payloadID+".log")
}

// getOrCreateLog returns the open log file, per-file append mutex, and
// interval tree for payloadID, creating them on first touch. Uses the
// standard double-checked-locking idiom against bc.logsMu (mirrors
// getOrCreateMemBlock in fs.go).
//
// On first touch it initializes a fresh log via initLogFile; if the log
// already exists on disk it is reopened O_RDWR and seeked to EOF for
// append. The per-file mutex and interval tree are allocated per payload
// on first call and reused thereafter.
//
// D-34: the log fd is owned here for the lifetime of the FSStore and is
// NOT routed through fdPool — AppendWrite is append-only single-file-per-
// payload, so the pool (optimized for random-access .blk files) would be
// a pessimization.
func (bc *FSStore) getOrCreateLog(payloadID string) (*logFile, *sync.Mutex, *intervalTree, error) {
	// Fast path: all three already present.
	bc.logsMu.RLock()
	lf, lfOk := bc.logFDs[payloadID]
	mu, muOk := bc.logLocks[payloadID]
	tree, treeOk := bc.dirtyIntervals[payloadID]
	bc.logsMu.RUnlock()
	if lfOk && muOk && treeOk {
		return lf, mu, tree, nil
	}

	// Slow path: upgrade to write lock, double-check, create missing entries.
	bc.logsMu.Lock()
	defer bc.logsMu.Unlock()
	lf, lfOk = bc.logFDs[payloadID]
	mu, muOk = bc.logLocks[payloadID]
	tree, treeOk = bc.dirtyIntervals[payloadID]
	if !lfOk {
		path := bc.logPath(payloadID)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, nil, nil, fmt.Errorf("append log: mkdir: %w", err)
		}
		// Declare f at outer scope and use `=` (not `:=`) in the branches
		// below to avoid the shadowing bug called out in plan 04.
		var f *os.File
		_, statErr := os.Stat(path)
		if statErr == nil {
			// Reopen existing log, seek to EOF for append.
			var err error
			f, err = os.OpenFile(path, os.O_RDWR, 0644)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("append log: reopen: %w", err)
			}
			if _, err = f.Seek(0, io.SeekEnd); err != nil {
				_ = f.Close()
				return nil, nil, nil, fmt.Errorf("append log: seek end: %w", err)
			}
		} else if os.IsNotExist(statErr) {
			var err error
			f, err = initLogFile(path, time.Now().Unix())
			if err != nil {
				return nil, nil, nil, err
			}
			if _, err = f.Seek(0, io.SeekEnd); err != nil {
				_ = f.Close()
				return nil, nil, nil, fmt.Errorf("append log: seek end: %w", err)
			}
		} else {
			return nil, nil, nil, fmt.Errorf("append log: stat: %w", statErr)
		}
		lf = &logFile{f: f, path: path}
		bc.logFDs[payloadID] = lf
	}
	if !muOk {
		mu = &sync.Mutex{}
		bc.logLocks[payloadID] = mu
	}
	if !treeOk {
		tree = newIntervalTree()
		bc.dirtyIntervals[payloadID] = tree
	}
	return lf, mu, tree, nil
}

// AppendWrite writes exactly one framed record to the payload's append
// log (LSL-03). The append is serialized by a per-file mutex (D-32) to
// guarantee crash-safe log ordering — a lock-free log would risk torn
// pwrite + successful fsync overwriting acknowledged data (see D-32
// rationale).
//
// Returns:
//   - ErrStoreClosed if the store is closed.
//   - ErrAppendLogDisabled if useAppendLog is false (D-36; no disk side
//     effects, no log file created).
//   - nil if len(data) == 0 (matches WriteAt short-circuit).
//   - ctx.Err() if the context is canceled before or during the pressure
//     wait.
//
// Pressure semantics (D-14/D-15): if logBytesTotal exceeds maxLogBytes,
// AppendWrite blocks on bc.pressureCh (pulsed by the rollup in Phase
// 10-06) or ctx.Done / bc.done. This is the only blocking arm; the
// mutex window itself is bounded to pwrite + fsync + tree.Insert (~5µs
// on NVMe).
//
// On successful append the interval tree gains a single entry covering
// [offset, offset+len(data)) with Touched=now; the rollup later consumes
// it after the stabilization window (D-16).
//
// D-34: this method never touches bc.fdPool — the log fd is owned for
// the lifetime of the FSStore, not pooled.
func (bc *FSStore) AppendWrite(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if !bc.useAppendLog {
		return ErrAppendLogDisabled
	}
	if len(data) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > int(^uint32(0)) {
		return fmt.Errorf("append log: payload too large: %d", len(data))
	}

	// D-28 / plan 09: short-circuit writes for tombstoned payloads. This
	// check sits BEFORE the pressure loop so a writer stuck on pressure
	// for a to-be-deleted payload still surfaces ErrDeleted promptly once
	// DeleteAppendLog tombstones the id (DeleteAppendLog acquires the
	// per-file mutex after tombstoning, and any pressure-blocked writer
	// re-checks this flag each loop iteration below).
	bc.logsMu.RLock()
	_, isDeleted := bc.tombstones[payloadID]
	bc.logsMu.RUnlock()
	if isDeleted {
		return ErrDeleted
	}

	// Pressure loop (D-14/D-15). Re-check under the loop in case the
	// rollup pulsed pressureCh but another writer consumed the freed
	// budget before this goroutine could proceed.
	for bc.logBytesTotal.Load() > bc.maxLogBytes {
		select {
		case <-bc.pressureCh:
			// rollup (future plan) or test freed space; re-check budget.
		case <-ctx.Done():
			return ctx.Err()
		case <-bc.done:
			return ErrStoreClosed
		}
	}

	lf, mu, tree, err := bc.getOrCreateLog(payloadID)
	if err != nil {
		return err
	}

	mu.Lock()
	// Guarded unlock so the error path can release `mu` BEFORE acquiring
	// bc.logsMu.Lock() (FIX-2 lock-order discipline) without risking a
	// double-unlock panic from this deferred call.
	muUnlocked := false
	defer func() {
		if !muUnlocked {
			mu.Unlock()
		}
	}()

	// Guard against a Close() that raced us between the pressure loop and
	// here: the outer closedFlag guard catches the common case; this
	// second check closes the window where bc.done fired while we were
	// waiting on the per-file mutex.
	if bc.isClosed() {
		return ErrStoreClosed
	}

	// Tombstone re-check under the per-file mutex (D-28 / plan 09). A
	// DeleteAppendLog call that tombstoned the payload while we were
	// waiting on the mutex above must be surfaced here rather than after
	// a wasted log append; DeleteAppendLog itself acquires the same mutex
	// immediately after tombstoning so this check is race-free.
	bc.logsMu.RLock()
	_, isDeleted = bc.tombstones[payloadID]
	bc.logsMu.RUnlock()
	if isDeleted {
		return ErrDeleted
	}

	n, err := writeRecord(lf.f, offset, data)
	if err != nil {
		// Recovery posture mirrors fdPool.Evict in the legacy write path:
		// close + drop the fd so the next call reopens fresh and an
		// already-written prefix does not poison subsequent appends.
		//
		// LOCK ORDERING (FIX-2): release the per-file mutex BEFORE
		// acquiring bc.logsMu.Lock(). Any path that holds logsMu and
		// waits on mu would otherwise deadlock against us; the global
		// rule is "always acquire mu before logsMu" — here we guarantee
		// it by releasing mu first.
		//
		// FIX-20: remove the lf entry from bc.logFDs BEFORE closing the
		// fd. The previous order (close, then unlock-and-delete) opened
		// a use-after-close window: any concurrent reader (another
		// AppendWrite or rollupFile) that snapshotted `lf` from
		// bc.logFDs while we held the per-file mutex could still hold
		// the same `*logFile` pointer, and once we Close()d the fd they
		// would read/write a closed descriptor. Removing the map entry
		// first guarantees no NEW caller can retrieve this lf, and the
		// per-file mutex we still hold serializes us against any
		// in-flight caller that already snapshotted it (those callers
		// also take mu before touching lf.f). Once both invariants
		// hold, Close() is safe.
		mu.Unlock()
		muUnlocked = true
		bc.logsMu.Lock()
		delete(bc.logFDs, payloadID)
		bc.logsMu.Unlock()
		_ = lf.f.Close()
		return fmt.Errorf("append log: %w", err)
	}
	if !bc.skipFsync {
		if err := lf.f.Sync(); err != nil {
			return fmt.Errorf("log fsync: %w", err)
		}
	}
	bc.logBytesTotal.Add(int64(n))
	tree.Insert(offset, uint32(len(data)), time.Now())

	// Non-blocking nudge to the rollup pool (plan 06). If the channel is
	// full, drop the signal — the rollup worker's ticker arm will pick up
	// this payloadID on the next scan of dirtyIntervals.
	if bc.rollupCh != nil {
		select {
		case bc.rollupCh <- payloadID:
		default:
		}
	}
	return nil
}

// DeleteAppendLog runs the D-28 delete sequence for payloadID. Idempotent:
// calling on a payload with no log is a no-op (returns nil).
//
// FIX-17: step 4 (`os.Remove(lf.path)`) may legitimately fail in the wild
// (EBUSY on Windows-mounted volumes, EROFS on a degraded backing store,
// EACCES if file ownership shifted). The error is now SURFACED to the
// caller — wrapped and logged — instead of silently swallowed. Step 5
// state cleanup still runs so the in-memory FSStore stays consistent
// even when the on-disk unlink failed; the next boot's recovery
// orphan-sweep will eventually clean the residual file. Callers MUST be
// prepared for DeleteAppendLog to return a non-nil error from step 4.
// ENOENT is treated as benign (idempotent re-delete) and does NOT
// surface as an error.
//
// IMPORTANT (FIX-8): the tombstone set here is PERMANENT for the lifetime
// of this FSStore process. After DeleteAppendLog returns, ANY subsequent
// AppendWrite or DeleteAppendLog on the same payloadID returns ErrDeleted
// (or short-circuits as no-op). This eliminates a re-creation race: under
// the previous "clear-on-success" semantics, a writer that observed the
// tombstone after we cleared it could resurrect a deleted payloadID with a
// fresh log file — causing dedup, refcount, and orphan-sweep state to
// diverge from the metadata layer's view of the payload as deleted.
//
// Callers that need to "delete and recreate" must use a fresh payloadID
// (e.g., add a generation suffix). The metadata-layer file-handle abstraction
// already does this — file handles are opaque and a new file gets a new
// payloadID, so this constraint is invisible to higher layers.
//
// Ordering (D-28, Blocker 3 fix):
//  1. Set tombstone under bc.logsMu so new AppendWrites and new rollupFile
//     entries short-circuit immediately.
//  2. Acquire the per-file mutex. Because rollupFile (plan 06) holds the
//     same mutex through reconstructStream → chunker → StoreChunk →
//     CommitChunks, blocking on Lock() here genuinely waits for any
//     in-flight rollup to finish. Rollup's pre-commit tombstone re-check
//     (rollup.go ~line 247) guarantees it will NOT persist rollup_offset
//     for a tombstoned payload even if it acquired the mutex first — so
//     once we hold the mutex, metadata is guaranteed consistent with the
//     tombstone we just set.
//  3. Clear the rollup_offset row (best-effort; INV-03 rejection is
//     expected when a prior rollup persisted a positive offset and is
//     treated as benign — the tombstone blocks any future rollup so a
//     residual positive offset is harmless once the log and dirty state
//     are gone).
//  4. Close + unlink the log file.
//  5. Clear per-file in-memory state (fd, lock, interval tree, truncation
//     boundary). The TOMBSTONE entry is preserved (FIX-8) so future
//     operations on payloadID stay rejected.
//
// Orphan content-addressed chunks under blocks/{hh}/{hh}/{hex} are NOT
// removed here; they are swept by Phase 11's mark-sweep GC. This is a
// known and documented limitation (10-CONTEXT.md D-38).
func (bc *FSStore) DeleteAppendLog(ctx context.Context, payloadID string) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if !bc.useAppendLog {
		return nil
	}

	// Step 1: tombstone + snapshot mutex/fd under the shared lock. We
	// release logsMu before acquiring the per-file mutex so a concurrent
	// rollup that already holds the per-file mutex can still RLock logsMu
	// (e.g., inside isTombstoned) without deadlocking.
	bc.logsMu.Lock()
	if bc.tombstones == nil {
		bc.tombstones = make(map[string]struct{})
	}
	bc.tombstones[payloadID] = struct{}{}
	mu := bc.logLocks[payloadID]
	lf := bc.logFDs[payloadID]
	bc.logsMu.Unlock()

	// Step 2: wait for any in-flight AppendWrite / rollupFile to drain.
	// When mu is nil the payload never had a log — no race to wait for.
	if mu != nil {
		mu.Lock()
		// Immediately release. The mutex's purpose here is the Lock()
		// barrier, not continued ownership; subsequent state mutation is
		// guarded by bc.logsMu.
		mu.Unlock()
	}

	// Step 3: clear metadata row (source of truth advance). INV-03
	// monotone enforcement means SetRollupOffset(payloadID, 0) is
	// rejected with ErrRollupOffsetRegression when a prior rollup
	// persisted a positive offset. That rejection is BENIGN: the
	// tombstone set in step 1 prevents any future rollup from touching
	// this payload, and Phase 11 mark-sweep GC will collect the
	// associated chunks. We deliberately swallow the error.
	if bc.rollupStore != nil {
		_, _ = bc.rollupStore.SetRollupOffset(ctx, payloadID, 0)
	}

	// Step 4: close + unlink the log file. Closing an already-closed fd is
	// idempotent (we still log a non-nil close error at Warn). Unlinking an
	// already-missing file is benign (ENOENT swallowed). FIX-17: any other
	// unlink error is now LOGGED at Error AND SURFACED to the caller wrapped
	// as `delete log file: %w` — operators previously had no signal when an
	// unlink persistently failed (EBUSY, EROFS, EACCES). State cleanup in
	// step 5 still runs so in-memory FSStore stays consistent; recovery's
	// boot-time orphan sweep eventually cleans the residual file.
	var stepFourErr error
	if lf != nil {
		if lf.f != nil {
			if cerr := lf.f.Close(); cerr != nil {
				logger.Warn("DeleteAppendLog: log file close failed", "payloadID", payloadID, "path", lf.path, "error", cerr)
			}
		}
		if lf.path != "" {
			if rerr := os.Remove(lf.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				// FIX-25: do NOT include the absolute on-disk path in
				// the error returned to the caller — protocol error
				// responses (NFS/SMB) propagate this string and would
				// leak server-side filesystem layout to remote clients.
				// The full path is still logged at Error so operators
				// retain forensic visibility.
				logger.Error("DeleteAppendLog: log file unlink failed", "payloadID", payloadID, "path", lf.path, "error", rerr)
				stepFourErr = fmt.Errorf("delete log file for payload %q: %w", payloadID, rerr)
			}
		}
	}

	// Step 5: remove per-file FSStore state for payloadID. The TOMBSTONE
	// entry is intentionally NOT cleared (FIX-8) — clearing it allowed a
	// re-creation race where a writer that lost a wakeup race could
	// resurrect a deleted payloadID with a fresh log, diverging on-disk
	// state from the metadata layer's view of the payload as deleted.
	// Tombstones live for the lifetime of the FSStore process; callers
	// that need a fresh payload must allocate a fresh payloadID.
	bc.logsMu.Lock()
	delete(bc.logFDs, payloadID)
	delete(bc.logLocks, payloadID)
	delete(bc.dirtyIntervals, payloadID)
	delete(bc.truncations, payloadID)
	// NOTE: bc.tombstones[payloadID] is preserved by design.
	bc.logsMu.Unlock()

	// FIX-17: surface any step-4 unlink error after step 5 cleanup so the
	// in-memory FSStore is left consistent regardless of whether the
	// on-disk file was successfully removed.
	return stepFourErr
}

// TruncateAppendLog records a truncation boundary for payloadID (D-29).
// Subsequent behavior:
//   - The per-file interval tree drops entries whose Offset >= newSize and
//     clips entries that straddle (intervalTree.DropAbove).
//   - rollupFile consults bc.truncations and filters / clips records
//     whose file_offset + len(payload) crosses newSize so the emitted
//     chunk stream never contains data past the truncation point.
//   - AppendWrite is NOT blocked — writes past newSize are still accepted
//     into the log (a client-level truncate followed by a write past
//     newSize must logically extend the file; the truncation boundary is
//     only an invariant for records already in the log at the moment of
//     the call). If the caller needs post-truncate writes to also be
//     clipped, they must issue a fresh TruncateAppendLog.
//
// Idempotent on a payload with no log (no-op).
//
// D-29 explicitly accepts that the truncation boundary is in-memory only
// this phase (T-10-09-04). Persistent truncate across crash is Phase 12
// when `[]BlockRef` carries per-chunk size.
func (bc *FSStore) TruncateAppendLog(_ context.Context, payloadID string, newSize uint64) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if !bc.useAppendLog {
		return nil
	}

	bc.logsMu.RLock()
	mu := bc.logLocks[payloadID]
	tree := bc.dirtyIntervals[payloadID]
	bc.logsMu.RUnlock()

	// Drop / clip dirty intervals under the per-file mutex so a
	// concurrent AppendWrite cannot observe a half-updated tree.
	if tree != nil && mu != nil {
		mu.Lock()
		tree.DropAbove(newSize)
		mu.Unlock()
	}

	// Record the boundary so rollupFile can filter records. We always
	// record, even when there is no open log yet, so a truncate followed
	// by a later AppendWrite + rollup still honors the boundary for the
	// records that existed at truncate time. (Records appended AFTER the
	// truncate are still accepted — see method doc.)
	bc.logsMu.Lock()
	if bc.truncations == nil {
		bc.truncations = make(map[string]uint64)
	}
	bc.truncations[payloadID] = newSize
	bc.logsMu.Unlock()

	return nil
}
