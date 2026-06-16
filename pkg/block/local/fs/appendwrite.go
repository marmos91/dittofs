package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// logFile wraps an open per-payload append-log file descriptor. The path
// is held alongside so recovery (06) can reopen after a forced
// close on write error (mirrors fdPool.Evict posture from the legacy
// path).
type logFile struct {
	f    *os.File
	path string
	// groupCommit coalesces concurrent fsyncs against this log's fd into
	// a single underlying f.Sync() call (Opt 2).
	// Per-file scope — different logFiles fsync independently. Synchronous
	// durability preserved: Sync(ctx) blocks until fsync completes; NFS
	// COMMIT / SMB Flush callers see no async ack.
	//
	// The coordinator is bound to lf.f.Sync at construction (bound method
	// value). The fd is owned for the lifetime of the FSStore (not
	// pooled, not rotated); there is no file-rotation path that would
	// stale-capture lf.f. On error-recovery close (the writeRecord-error
	// branch in AppendWrite), the entire *logFile is dropped from
	// the shard's logFDs and a fresh one is constructed on next touch — the
	// coordinator goes with it.
	//
	// Teardown: no explicit Stop is required. The design has no
	// goroutines outside the in-flight piggyback path; any in-flight
	// fsync against a Close()d fd surfaces as an EBADF to its caller
	// which is the correct error posture (caller observes the
	// fsync result).
	groupCommit *groupCommit

	// eofPos tracks the on-disk EOF byte offset of this log. Initialized
	// at construction (either logHeaderSize for a freshly initialized log
	// or the seek-end offset for a reopened one) and advanced under the
	// per-file `mu` after each successful writeRecord + groupCommit.Sync
	// pair in AppendWrite. It is the canonical "log position" cursor used
	// by the per-file logIndex (Direction 1 redesign) to record where in
	// the log each AppendWrite's record landed without re-statting the fd.
	eofPos uint64
}

// logPath returns the on-disk path for a payload's append-log file
// {baseDir}/logs/{payloadID}.log (flat layout; one log per payload).
func (bc *FSStore) logPath(payloadID string) string {
	return filepath.Join(bc.baseDir, "logs", payloadID+".log")
}

// getOrCreateLog returns the open log file, per-file append mutex,
// interval tree, and logIndex for payloadID, creating them on first
// touch. Uses the standard double-checked-locking idiom against the
// payload's shard lock (mirrors getOrCreateMemBlock in fs.go).
//
// On first touch it initializes a fresh log via initLogFile; if the log
// already exists on disk it is reopened O_RDWR and seeked to EOF for
// append. The per-file mutex, interval tree, and logIndex are allocated
// per payload on first call and reused thereafter.
//
// the log fd is owned here for the lifetime of the FSStore and is
// NOT routed through fdPool — AppendWrite is append-only single-file-per-
// payload, so the pool (optimized for random-access .blk files) would be
// a pessimization.
//
// Returning the logIndex alongside the tree lets AppendWrite use the
// snapshot that getOrCreateLog produced under the shard lock rather than
// re-reading the shard's logIndices on a second RLock cycle. That second lookup
// could observe a nil idx if the map had been cleared between AppendWrite's
// tree.Insert and the logIndex re-read, producing a tree/logIndex
// divergence that wedged rollup (#668).
func (bc *FSStore) getOrCreateLog(payloadID string) (*logFile, *sync.Mutex, *intervalTree, *logIndex, error) {
	// Defense-in-depth (REVIEW.md §3b S-4): reject malformed payloadIDs at
	// the FIRST line, before any mutex acquisition or fd open. logPath
	// joins payloadID into filepath.Join, so a '../'-bearing id would
	// otherwise place a log file outside <baseDir>/logs. Recovery already
	// validates filenames read from disk (recovery.go ~line 254); this
	// guard closes the symmetric write-path gap. The check is intentionally
	// upstream of the shard lock so a malicious id can't take a shard RLock
	// either.
	if !isValidPayloadID(payloadID) {
		return nil, nil, nil, nil, fmt.Errorf("%w: %q", ErrInvalidPayloadID, payloadID)
	}

	// C2: per-payload state lives in the shard for this payloadID, so the
	// create-path write lock below contends only with other payloads in the
	// same shard, not the whole store.
	sh := bc.shardFor(payloadID)

	// Fast path: all four already present.
	sh.mu.RLock()
	lf, lfOk := sh.logFDs[payloadID]
	mu, muOk := sh.logLocks[payloadID]
	tree, treeOk := sh.dirtyIntervals[payloadID]
	idx, idxOk := sh.logIndices[payloadID]
	sh.mu.RUnlock()
	if lfOk && muOk && treeOk && idxOk {
		return lf, mu, tree, idx, nil
	}

	// Slow path: upgrade to write lock, double-check, create missing entries.
	sh.mu.Lock()
	defer sh.mu.Unlock()
	lf, lfOk = sh.logFDs[payloadID]
	mu, muOk = sh.logLocks[payloadID]
	tree, treeOk = sh.dirtyIntervals[payloadID]
	idx, idxOk = sh.logIndices[payloadID]
	if !lfOk {
		path := bc.logPath(payloadID)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("append log: mkdir: %w", err)
		}
		// Declare f at outer scope and use `=` (not `:=`) in the branches
		// below to avoid the shadowing bug called out in.
		var f *os.File
		var eof int64
		_, statErr := os.Stat(path)
		if statErr == nil {
			// Reopen existing log, seek to EOF for append.
			var err error
			f, err = os.OpenFile(path, os.O_RDWR, 0644)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("append log: reopen: %w", err)
			}
			if eof, err = f.Seek(0, io.SeekEnd); err != nil {
				_ = f.Close()
				return nil, nil, nil, nil, fmt.Errorf("append log: seek end: %w", err)
			}
		} else if os.IsNotExist(statErr) {
			var err error
			f, err = initLogFile(path, time.Now().Unix())
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if eof, err = f.Seek(0, io.SeekEnd); err != nil {
				_ = f.Close()
				return nil, nil, nil, nil, fmt.Errorf("append log: seek end: %w", err)
			}
		} else {
			return nil, nil, nil, nil, fmt.Errorf("append log: stat: %w", statErr)
		}
		lf = &logFile{f: f, path: path, eofPos: uint64(eof)}
		// Opt 2: per-file fsync coordinator.
		// Bound method value captures lf.f at construction; lf.f is not
		// rotated for the FSStore lifetime, so the binding stays valid.
		lf.groupCommit = newGroupCommit(lf.f.Sync)
		sh.logFDs[payloadID] = lf
	}
	if !muOk {
		mu = &sync.Mutex{}
		sh.logLocks[payloadID] = mu
	}
	// C1: pair every payload's append mutex with a rollup mutex, allocated
	// under the same shard write lock so rollupFile always observes a
	// non-nil rollup lock whenever the append mutex exists.
	if _, rmuOk := sh.rollupLocks[payloadID]; !rmuOk {
		sh.rollupLocks[payloadID] = &sync.Mutex{}
	}
	if !treeOk {
		tree = newIntervalTree()
		sh.dirtyIntervals[payloadID] = tree
	}
	// Direction-1 rollup redesign: pair every payload's interval tree
	// with a logIndex. The logIndex is allocated on first touch (here)
	// and from recovery's install path. Both sites run under the shard's
	// write lock.
	//
	// #668: the tree and logIndex MUST be allocated under the same
	// shard write lock so any caller that observes a non-nil tree on
	// the fast path also observes a non-nil logIndex. A prior version
	// re-read the shard's logIndices on a second RLock cycle inside AppendWrite,
	// which could see nil and skip Append while tree.Insert had already
	// landed — wedging the rollup on the next stabilization tick.
	if !idxOk {
		idx = newLogIndex()
		sh.logIndices[payloadID] = idx
	}
	return lf, mu, tree, idx, nil
}

// signalPressure wakes any AppendWrite blocked in the pressure loop after log
// budget has been freed (rollup retirement or payload delete). pressureCh is a
// size-1 buffered channel, so the send is non-blocking and a missed pulse is
// harmless — a blocked writer also re-checks the budget on the rollup ticker.
func (bc *FSStore) signalPressure() {
	select {
	case bc.pressureCh <- struct{}{}:
	default:
	}
}

// releaseLogBytes subtracts n from the pressure budget and pulses pressureCh,
// flooring the running total at zero. The exact callers (the rollup fence
// retirement and the DeleteAppendLog mid-write reclaim) never over-release in
// steady state, but recovery seeds logBytesTotal from the on-disk resident
// estimate (file size - rollup_offset) which can drift by a torn-tail record
// from the per-record frame sum; the floor keeps a benign drift from turning
// the budget negative and defeating backpressure. n <= 0 is a no-op.
func (bc *FSStore) releaseLogBytes(n int64) {
	if n <= 0 {
		return
	}
	if bc.logBytesTotal.Add(-n) < 0 {
		bc.logBytesTotal.Store(0)
	}
	bc.signalPressure()
}

// AppendWrite writes exactly one framed record to the payload's append
// log. The append is serialized by a per-file mutex to
// guarantee crash-safe log ordering — a lock-free log would risk torn
// pwrite + successful fsync overwriting acknowledged data.
//
// Returns
//   - ErrStoreClosed if the store is closed.
//   - nil if len(data) == 0 (matches WriteAt short-circuit).
//   - ctx.Err() if the context is canceled before or during the pressure
//     wait.
//
// Pressure semantics: if logBytesTotal exceeds maxLogBytes
// AppendWrite blocks on bc.pressureCh (pulsed by the rollup) or
// ctx.Done / bc.done. This is the only blocking arm; the
// mutex window itself is bounded to pwrite + fsync + tree.Insert (~5µs
// on NVMe).
//
// Deadline (#670 defense-in-depth): the pressure wait is bounded by
// bc.pressureMaxWait (FSStoreOptions.PressureMaxWait; default 30s).
// On expiry AppendWrite returns ErrPressureTimeout — distinguishable
// from ctx.Err() (caller-side deadline) and ErrStoreClosed (store
// shutdown). This bound prevents a wedged rollup pool from translating
// into NFS-client D-state when the caller (NFS COMMIT / SMB Flush)
// arrived with no usable deadline. Set PressureMaxWait < 0 to disable.
//
// On successful append the interval tree gains a single entry covering
// [offset, offset+len(data)) with Touched=now; the rollup later consumes
// it after the stabilization window.
//
// AppendWrite never touches bc.fdPool — the log fd is owned for
// the lifetime of the FSStore, not pooled.
func (bc *FSStore) AppendWrite(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if bc.isClosed() {
		return ErrStoreClosed
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

	// short-circuit writes for tombstoned payloads. This
	// check sits BEFORE the pressure loop so a writer stuck on pressure
	// for a to-be-deleted payload still surfaces ErrDeleted promptly once
	// DeleteAppendLog tombstones the id (DeleteAppendLog acquires the
	// per-file mutex after tombstoning, and any pressure-blocked writer
	// re-checks this flag each loop iteration below).
	tsh := bc.shardFor(payloadID)
	tsh.mu.RLock()
	_, isDeleted := tsh.tombstones[payloadID]
	tsh.mu.RUnlock()
	if isDeleted {
		return ErrDeleted
	}

	// Pressure loop. Re-check under the loop in case the
	// rollup pulsed pressureCh but another writer consumed the freed
	// budget before this goroutine could proceed.
	//
	// #670: bound the wait with bc.pressureMaxWait so a wedged rollup
	// cannot translate into NFS-client D-state. The timer is allocated
	// lazily — only when we actually need to block — and is shared across
	// pressureCh wake-ups so a sequence of false-wakes (rollup pulses +
	// another writer immediately consumes the freed budget) still
	// respects the cumulative deadline. Using time.NewTimer + defer Stop()
	// instead of time.After lets the runtime release the timer slot as
	// soon as pressureCh or bc.done fires first; time.After otherwise
	// keeps the underlying runtime timer armed until the original deadline
	// expires.
	var pressureTimer *time.Timer
	var pressureC <-chan time.Time
	defer func() {
		if pressureTimer != nil {
			pressureTimer.Stop()
		}
	}()
	for bc.logBytesTotal.Load() > bc.maxLogBytes {
		if pressureTimer == nil && bc.pressureMaxWait > 0 {
			pressureTimer = time.NewTimer(bc.pressureMaxWait)
			pressureC = pressureTimer.C
		}
		select {
		case <-bc.pressureCh:
			// rollup or test freed space; re-check budget. Timer keeps
			// running so a writer that loses the budget race repeatedly
			// still surfaces ErrPressureTimeout at the cumulative deadline.
		case <-ctx.Done():
			return ctx.Err()
		case <-bc.done:
			return ErrStoreClosed
		case <-pressureC:
			return ErrPressureTimeout
		}
	}

	lf, mu, tree, idx, err := bc.getOrCreateLog(payloadID)
	if err != nil {
		return err
	}

	mu.Lock()
	// Guarded unlock so the error path can release `mu` BEFORE acquiring
	// the shard lock (FIX-2 lock-order discipline) without risking a
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

	// Tombstone re-check under the per-file mutex. A
	// DeleteAppendLog call that tombstoned the payload while we were
	// waiting on the mutex above must be surfaced here rather than after
	// a wasted log append; DeleteAppendLog itself acquires the same mutex
	// immediately after tombstoning so this check is race-free.
	//
	// Also re-validate `lf` against the current shard's logFDs entry: if
	// DeleteAppendLog ran fully (including step 5's logFDs+tombstones
	// clear) BETWEEN the writer's pre-mutex tombstone check and the
	// mutex hand-off, our snapshotted `lf` references a closed/unlinked
	// fd while the tombstone has already been cleared (post-Phase-18
	// recreate semantics). Mirrors the rollup-side curLf != lf bail in
	// rollup.go. We surface ErrDeleted for the original writer because
	// its target log was unambiguously deleted; the caller (typically
	// the NFS WRITE handler) will return EIO/STATUS_DELETE_PENDING to
	// the client. The recreate path is intentionally NOT followed by
	// the same in-flight writer — it belongs to a different file
	// lifecycle.
	tsh.mu.RLock()
	_, isDeleted = tsh.tombstones[payloadID]
	curLf, curLfOk := tsh.logFDs[payloadID]
	tsh.mu.RUnlock()
	if isDeleted {
		return ErrDeleted
	}
	if !curLfOk || curLf != lf {
		return ErrDeleted
	}

	n, err := writeRecord(lf.f, offset, data)
	if err != nil {
		// Recovery posture mirrors fdPool.Evict in the legacy write path
		// close + drop the fd so the next call reopens fresh and an
		// already-written prefix does not poison subsequent appends.
		//
		// LOCK ORDERING (FIX-2 / FIX-20 extension)
		//   per-file `mu` (held by caller across this region)
		//     → groupCommit.mu (acquired inside lf.groupCommit.Sync below)
		// → the shard lock (NEVER held while waiting on groupCommit.mu
		//                    the coordinator NEVER references a shard lock
		//                    directly — enforced by the
		//                    TestGroupCommit_NoLogsMuTouch grep gate).
		// The pre-Phase-19 rule still holds: release per-file mu BEFORE
		// acquiring the shard lock (any path that holds the shard lock and
		// waits on mu would otherwise deadlock against us; the global
		// rule is "always acquire mu before the shard lock" — here we
		// guarantee it by releasing mu first).
		//
		// FIX-20: remove the lf entry from the shard's logFDs BEFORE closing the
		// fd. The previous order (close, then unlock-and-delete) opened
		// a use-after-close window: any concurrent reader (another
		// AppendWrite or rollupFile) that snapshotted `lf` from
		// the shard's logFDs while we held the per-file mutex could still hold
		// the same `*logFile` pointer, and once we Close()d the fd they
		// would read/write a closed descriptor. Removing the map entry
		// first guarantees no NEW caller can retrieve this lf, and the
		// per-file mutex we still hold serializes us against any
		// in-flight caller that already snapshotted it (those callers
		// also take mu before touching lf.f). Once both invariants
		// hold, Close() is safe.
		mu.Unlock()
		muUnlocked = true
		tsh.mu.Lock()
		delete(tsh.logFDs, payloadID)
		tsh.mu.Unlock()
		_ = lf.f.Close()
		return fmt.Errorf("append log: %w", err)
	}
	// Opt 2: fsync coalesced through the
	// per-logFile groupCommit coordinator. Synchronous durability
	// preserved — caller blocks until fsync completes; NFS COMMIT /
	// SMB Flush callers see no async ack. The per-file `mu` is still
	// held across this call; coordinator's internal mu is a different
	// lock (lock-order rule in the godoc block above).
	//
	// TRANSITIONAL-NEXT-MILESTONE: O_DIRECT for log writes (see #519
	// "Deferred to v0.17+"). When O_DIRECT lands, the groupCommit
	// coordinator may need to switch from fsync to fdatasync or
	// fsync(O_SYNC) — revisit the fsyncFn closure at construction
	// (logFile creation in getOrCreateLog / recovery.go).
	if err := lf.groupCommit.Sync(ctx); err != nil {
		// writeRecord already advanced the OS fd position by n bytes, but we
		// have NOT advanced lf.eofPos yet (that happens below). If we simply
		// returned here, the next AppendWrite to this payload would write its
		// record at fd_pos = old_eofPos + n while capturing logPos = eofPos
		// (the pre-write value), recording a logIndex entry that points at the
		// orphaned, un-fsync'd frame instead of the new one — permanently
		// wedging rollup with a wrong file_offset / CRC mismatch. Evict the fd
		// exactly as the writeRecord-error path does so the next call reopens
		// fresh from the on-disk eofPos and the orphaned tail is ignored.
		mu.Unlock()
		muUnlocked = true
		tsh.mu.Lock()
		delete(tsh.logFDs, payloadID)
		tsh.mu.Unlock()
		_ = lf.f.Close()
		return fmt.Errorf("log fsync: %w", err)
	}
	// Advance the log-position cursor only after the writeRecord +
	// groupCommit.Sync pair has succeeded. We are still under the per-
	// file `mu`, so the increment is serialized against any other writer
	// or rollup pread that consults lf.eofPos. Direction-1 redesign: the
	// pre-advance value is the logPos at which this record's frame
	// starts; capture it before the advance so the logIndex entry
	// points at the correct frame boundary.
	logPos := lf.eofPos
	lf.eofPos += uint64(n)
	bc.logBytesTotal.Add(int64(n))
	// Direction-1 redesign: record this AppendWrite in the per-payload
	// logIndex. The interval tree answers "which file regions are
	// dirty"; logIndex answers "where in the log are the records for
	// those regions" — decoupling the two domains so the rollup can
	// translate a stable file-offset interval into a pread set even
	// when records arrived out of file-offset order.
	//
	// #668: idx is the snapshot getOrCreateLog returned under
	// the shard lock, paired with tree under the same lock. Append BEFORE
	// tree.Insert so a future rollup pass that observes the dirty
	// interval is guaranteed to also see the matching logIndex entry,
	// closing the divergence window that wedged rollupFile permanently.
	idx.Append(logPos, offset, uint32(len(data)))
	tree.Insert(offset, uint32(len(data)), time.Now())

	// Non-blocking nudge to the rollup pool. If the channel is
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

// DeleteAppendLog runs the delete sequence for payloadID. Idempotent
// calling on a payload with no log is a no-op (returns nil).
//
// FIX-17: step 4 (`os.Remove(lf.path)`) may legitimately fail in the wild
// (EBUSY on Windows-mounted volumes, EROFS on a degraded backing store
// EACCES if file ownership shifted). The error is now SURFACED to the
// caller — wrapped and logged — instead of silently swallowed. Step 5
// state cleanup still runs so the in-memory FSStore stays consistent
// even when the on-disk unlink failed; the next boot's recovery
// orphan-sweep will eventually clean the residual file. Callers MUST be
// prepared for DeleteAppendLog to return a non-nil error from step 4.
// ENOENT is treated as benign (idempotent re-delete) and does NOT
// surface as an error.
//
// Tombstone lifecycle: the tombstone set in step 1 is CLEARED in step 5
// after the in-flight drain (step 2) has serialized us against all
// pre-delete AppendWrites and rollups. Clearing on success is safe
// because
//
//   - Step 2's Lock/Unlock barrier waits for any goroutine that
//     snapshotted the shard's logFDs / logLocks BEFORE step 1's
//     tombstones[payloadID] = struct{}{} to complete. By the time
//     step 5 runs, no in-flight AppendWrite or rollup can still be
//     racing against the tombstone clear.
//   - Steps 4 and 5 happen AFTER the drain — when we clear the
//     tombstone, on-disk state and in-memory state for this payloadID
//     are fully reset. Any AppendWrite that arrives AFTER step 5
//     fresh-creates state via getOrCreateLog as if the payload never
//     existed. Files created after #1166 PR-3 get a UUID-based
//     PayloadID (metadata/file_helpers.go:buildPayloadID), so a
//     recreate-at-same-path uses a fresh content_id; this reset path
//     is still exercised on delete to reclaim the deleted file's own
//     append-log state and must leave the store ready for a
//     subsequent fresh log.
//
// This reverses the FIX-8 invariant (which kept the tombstone for the
// FSStore lifetime to eliminate a clear-on-success resurrection race).
// FIX-8 assumed callers would allocate a fresh PayloadID on recreate;
// the older path-based PayloadID scheme violated that assumption,
// breaking POSIX recreate-at-same-name workflows over NFSv3 (which has
// no silly-rename masking — see pjdfstest chmod/12.t, unlink/14.t,
// open/00.t). With UUID-based PayloadIDs recreates get a fresh id by
// construction, but the FSStore reset on delete must still leave state
// ready for the next fresh log.
//
// Ordering (Blocker 3 fix)
//  1. Set tombstone under the shard lock so new AppendWrites and new rollupFile
//     entries short-circuit immediately.
//  2. Acquire the per-file mutex. Because rollupFile holds the
//     same mutex through reconstructStream → chunker → StoreChunk →
//     CommitChunks, blocking on Lock() here genuinely waits for any
//     in-flight rollup to finish. Rollup's pre-commit tombstone re-check
//     (rollup.go ~line 247) guarantees it will NOT persist rollup_offset
//     for a tombstoned payload even if it acquired the mutex first — so
//     once we hold the mutex, metadata is guaranteed consistent with the
//     tombstone we just set.
//  3. Clear the rollup_offset row (best-effort; rejection is
//     expected when a prior rollup persisted a positive offset and is
//     treated as benign — the tombstone blocks any future rollup so a
//     residual positive offset is harmless once the log and dirty state
//     are gone).
//  4. Close + unlink the log file.
//  5. Clear per-file in-memory state (fd, lock, interval tree
//     truncation boundary) AND clear the tombstone — the drain in
//     step 2 has serialized us against all pre-delete writers/rollups
//     so it is safe to allow fresh recreates here.
//
// Orphan content-addressed chunks under blocks/{hh}/{hh}/{hex} are NOT
// removed here; they are swept by 's mark-sweep GC. This is a
// known and documented limitation (10-CONTEXT.md).
func (bc *FSStore) DeleteAppendLog(ctx context.Context, payloadID string) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	// Step 1: tombstone + snapshot mutex/fd under the shard lock. We
	// release the shard lock before acquiring the per-file mutex so a
	// concurrent rollup that already holds the per-file mutex can still
	// RLock the shard (e.g., inside isTombstoned) without deadlocking.
	sh := bc.shardFor(payloadID)
	sh.mu.Lock()
	sh.tombstones[payloadID] = struct{}{}
	mu := sh.logLocks[payloadID]
	rmu := sh.rollupLocks[payloadID]
	lf := sh.logFDs[payloadID]
	sh.mu.Unlock()

	// Step 2: wait for any in-flight AppendWrite / rollupFile to drain.
	// When mu is nil the payload never had a log — no race to wait for.
	//
	// C1: rollupFile now releases the append mutex (mu) during its CAS-store
	// phase, so draining mu alone no longer guarantees a rollup has finished.
	// Drain the rollup mutex (rmu) FIRST — rollupFile holds it across its
	// entire pass, so this blocks until any in-flight rollup completes its
	// metadata commit, preventing the ObjectIDPersister from writing manifest
	// rows for a payload we are about to delete. Lock order is rmu (outer) ->
	// mu (inner), matching rollupFile's acquisition order; the two barriers
	// are sequential here (not nested) so there is no AB/BA hazard either way.
	if rmu != nil {
		//nolint:staticcheck // SA2001: intentional in-flight rollup drain barrier
		rmu.Lock()
		rmu.Unlock() //nolint:staticcheck // SA2001: see above
	}
	if mu != nil {
		// Lock+Unlock sequence is the in-flight barrier — staticcheck SA2001
		// would flag this as an "empty critical section" if naively written
		// as `mu.Lock(); mu.Unlock()`. The pattern is intentional: we wait
		// for any goroutine currently holding `mu` (an AppendWrite or
		// rollupFile pass) to complete before clearing per-payload state.
		// Subsequent state mutation is guarded by the shard lock.
		//nolint:staticcheck // SA2001: intentional in-flight drain barrier
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // SA2001: see above
	}

	// Step 3: clear metadata row (source of truth advance).
	// monotone enforcement means SetRollupOffset(payloadID, 0) is
	// rejected with ErrRollupOffsetRegression when a prior rollup
	// persisted a positive offset. That rejection is BENIGN: the
	// tombstone set in step 1 prevents any future rollup from touching
	// this payload, and mark-sweep GC will collect the
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
			if rerr := removeLogFile(lf.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
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

	// Step 5: remove per-file FSStore state for payloadID AND clear the
	// tombstone. The drain in step 2 has already serialized us against
	// any in-flight AppendWrite or rollupFile that snapshotted state
	// before step 1, so it is safe to allow fresh recreates from this
	// point on. This reverses the original FIX-8 invariant (which kept
	// the tombstone for the FSStore lifetime to eliminate a
	// clear-on-success resurrection race). Files created after #1166 PR-3
	// get a UUID-based PayloadID (metadata/file_helpers.go buildPayloadID),
	// so a recreate-at-same-path gets a fresh content_id; this reset still
	// runs on delete to reclaim the deleted file's own state and must
	// leave the store ready for a subsequent fresh log.
	sh.mu.Lock()
	// C3: release the pressure budget for any record still live in this
	// payload's logIndex. AppendWrite reserved recordFrameOverhead+payloadLen
	// per record in logBytesTotal; a rollup pass releases it via the fence
	// retirement, but a payload deleted mid-write (storm create/delete churn)
	// discards its logIndex here before those records ever roll up. Without
	// this reclaim their reserved bytes leak permanently and the global budget
	// ratchets to maxLogBytes, eventually wedging every writer on
	// ErrPressureTimeout. LiveBytes counts exactly the un-retired records, so
	// the reclaim never double-counts against the rollup's own release.
	var reclaim int64
	if idx := sh.logIndices[payloadID]; idx != nil {
		reclaim = idx.LiveBytes()
	}
	delete(sh.logFDs, payloadID)
	delete(sh.logLocks, payloadID)
	delete(sh.rollupLocks, payloadID)
	delete(sh.dirtyIntervals, payloadID)
	delete(sh.logIndices, payloadID)
	delete(sh.truncations, payloadID)
	delete(sh.tombstones, payloadID)
	sh.mu.Unlock()
	// Release the discarded payload's reserved budget and pulse pressure so any
	// writer blocked on the budget re-checks immediately rather than waiting
	// for the next rollup tick.
	bc.releaseLogBytes(reclaim)

	// FIX-17: surface any step-4 unlink error after step 5 cleanup so the
	// in-memory FSStore is left consistent regardless of whether the
	// on-disk file was successfully removed.
	return stepFourErr
}

// removeLogFile unlinks path, tolerating Windows' delayed handle release.
//
// On POSIX a file can be unlinked while a handle is still open and Close()
// synchronously releases the descriptor, so a single os.Remove always
// succeeds (or fails for a real reason). Windows is different on both counts:
// it refuses to delete a file that has ANY open handle, and the kernel may
// not have fully released a handle by the time Close() returns. The result
// is a transient ERROR_SHARING_VIOLATION ("The process cannot access the
// file because it is being used by another process") on the os.Remove that
// immediately follows the log fd's Close() — even though no goroutine is
// logically using the file anymore. This surfaced as a Windows-CI-only flake
// in the AppendWrite/DeleteAppendLog delete race (#714) and would equally
// break real unlink-after-write workflows on a Windows host.
//
// On Windows we retry with a short backoff so the unlink lands once the
// kernel drains the closed handle; on POSIX the first os.Remove is
// authoritative and the loop never iterates. The retry is gated on a
// transient ERROR_SHARING_VIOLATION (32) / ERROR_LOCK_VIOLATION (33) — a
// genuine non-transient error (EACCES, EROFS, invalid path) is surfaced
// immediately rather than stalling the delete path for ~500ms first. After
// the bounded retry budget a still-held handle is returned for the caller to
// log and for recovery's boot-time orphan sweep to reconcile.
func removeLogFile(path string) error {
	err := os.Remove(path)
	if runtime.GOOS != "windows" || err == nil || errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !isTransientUnlinkError(err) {
		return err
	}
	// Windows-only: back off and retry the unlink while the closed handle
	// drains. Total worst-case wait ~500ms (10 × 50ms) — long enough for the
	// kernel's lazy release, short enough not to stall a delete-heavy caller.
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		err = os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !isTransientUnlinkError(err) {
			return err
		}
	}
	return err
}

// isTransientUnlinkError reports whether err is a Windows
// ERROR_SHARING_VIOLATION (32) or ERROR_LOCK_VIOLATION (33) — the transient
// "file in use" condition a just-closed handle produces before the kernel
// drains it. On non-Windows it is always false so the same numeric errnos on
// Unix are never misread.
func isTransientUnlinkError(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 32 || errno == 33
	}
	return false
}

// TruncateAppendLog records a truncation boundary for payloadID.
// Subsequent behavior
//   - The per-file interval tree drops entries whose Offset >= newSize and
//     clips entries that straddle (intervalTree.DropAbove).
//   - rollupFile consults the shard's truncations and filters / clips records
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
// explicitly accepts that the truncation boundary is in-memory only
// this phase. Persistent truncate across crash is
// when `[]BlockRef` carries per-chunk size.
func (bc *FSStore) TruncateAppendLog(_ context.Context, payloadID string, newSize uint64) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	mu := sh.logLocks[payloadID]
	tree := sh.dirtyIntervals[payloadID]
	sh.mu.RUnlock()

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
	sh.mu.Lock()
	sh.truncations[payloadID] = newSize
	sh.mu.Unlock()

	return nil
}
