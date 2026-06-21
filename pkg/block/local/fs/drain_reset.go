package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// DrainRollups forces rollup of ALL currently-dirty payloads to
// completion, bypassing the stabilization-window gate, and waits for any
// in-flight rollup-worker passes to finish first.
//
// This is the snapshot-create primitive: at snapshot time every written
// byte must be flushed from the per-payload append log into CAS AND into
// the FileBlock manifest (via the ObjectIDPersister) so the metadata
// Backup() observes a fully-populated FileAttr.Blocks. Steady-state
// rollup only consumes intervals that have aged past the stabilization
// window; a snapshot taken before that window elapses would otherwise
// capture an empty or partial manifest.
//
// Contract:
//   - Returns nil with no work when the store has no dirty payloads.
//   - Each dirty payload is force-rolled until its dirty-interval tree
//     drains (or stops making progress — a divergent interval that the
//     logIndex cannot back is dropped by rollupFile, and a truncation-
//     filtered empty batch is skipped; both are bounded by the
//     no-progress guard so the loop always terminates).
//   - ctx cancellation aborts the drain and returns ctx.Err().
//
// DrainRollups does NOT require StartRollup to have been called — it
// drives rollupFile synchronously on the caller's goroutine. When the
// rollup worker pool IS running, the per-file mutex inside rollupFile
// serializes this forced pass against concurrent worker passes.
func (bc *FSStore) DrainRollups(ctx context.Context) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Snapshot every payload that currently has a tree. The set can
		// grow under a concurrent AppendWrite, but each outer iteration
		// re-reads it so newly-dirtied payloads are picked up. Per-payload
		// emptiness is rechecked under the per-file mutex by dirtyLen
		// below — the btree is not safe to read here without that mutex.
		var pending []string
		for _, sh := range bc.logShards {
			sh.mu.RLock()
			for pid := range sh.dirtyIntervals {
				pending = append(pending, pid)
			}
			sh.mu.RUnlock()
		}

		progressed := false
		anyDirty := false
		for _, pid := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			before := bc.dirtyLen(pid)
			if before == 0 {
				continue
			}
			anyDirty = true
			if err := bc.rollupFile(ctx, pid, true); err != nil {
				return fmt.Errorf("DrainRollups: rollup payload %q: %w", pid, err)
			}
			if bc.dirtyLen(pid) < before {
				progressed = true
			}
		}

		if !anyDirty {
			// Nothing dirty remains — drain complete.
			return nil
		}
		if !progressed {
			// Dirty intervals remain but none shrank this pass. Distinguish
			// two cases:
			//
			//   - Benign skip: tombstoned payloads (a deleted file whose
			//     rollup is short-circuited) and tree/logIndex-divergent
			//     intervals (the rollupFile DropExact path — bytes that
			//     never reached a chunk). These have no backing logIndex
			//     entries (or a tombstone), so there is nothing to capture
			//     in the manifest and returning nil is correct.
			//
			//   - Real residual: a payload that is NOT tombstoned and whose
			//     logIndex CAN back its residual dirty intervals — genuine
			//     unflushed data that should have reached CAS. Returning nil
			//     here would let snapshot-create proceed to Backup with a
			//     partial manifest. Surface ErrDrainIncomplete so the
			//     orchestration fails the snapshot visibly instead.
			realResidual := bc.payloadsWithRealResidual(pending)
			if len(realResidual) > 0 {
				logger.Error("DrainRollups: residual dirty intervals with backing log data",
					"payload_count", len(realResidual))
				return fmt.Errorf("%w (%d payloads)", ErrDrainIncomplete, len(realResidual))
			}
			logger.Warn("DrainRollups: residual dirty intervals not drainable (tombstoned/divergent)",
				"payload_count", len(pending))
			return nil
		}
	}
}

// GracefulStopRollup implements the crash-safe shutdown drain for the rollup
// worker pool (#1245 Bug C).
//
// The bug it fixes: on shutdown the long-lived runtime context is cancelled,
// so any rollup pass that is in flight on that context hits
// context.Canceled in StoreChunk / the ObjectIDPersister and (pre-fix)
// propagated that as a fatal error to os.Exit (status=1/FAILURE). Rollups are
// already idempotent + resumable by design — CAS chunks are content-addressed
// and rollup_offset only advances after the FileBlock manifest lands — so an
// interrupted rollup is safe to finish on a fresh context.
//
// GracefulStopRollup performs that finish:
//
//  1. Stop accepting NEW rollups: signal the worker pool (started by
//     StartRollup on the cancellable runtime ctx) to exit and join it. New
//     AppendWrites no longer dispatch into a running pool.
//  2. DRAIN the remaining + in-flight dirty payloads to completion using a
//     SEPARATE, non-cancelled context bounded by `grace`. The per-file rollup
//     mutex inside rollupFile serializes this forced pass against any pass that
//     a worker had in flight, so we never double-roll or race the worker.
//
// Returns nil once every drainable payload has been flushed (or only
// benign tombstoned/divergent residual remains). A drain that exceeds the
// grace deadline returns a non-fatal context.DeadlineExceeded — the caller
// (Close) logs it and proceeds; the leftover dirty intervals are durable and
// resume on the next boot. Idempotent: safe to call more than once and safe
// to call when StartRollup was never invoked (it then drives the drain
// directly on the caller's goroutine).
//
// grace <= 0 defers to a 30s default.
func (bc *FSStore) GracefulStopRollup(grace time.Duration) error {
	if bc.isClosed() {
		return nil
	}
	if grace <= 0 {
		grace = 30 * time.Second
	}

	// Step 1: stop accepting new rollups and join the cancellable-ctx
	// worker pool. closeOnce-style guard so concurrent / repeated calls are
	// safe. Only signal when a pool was actually started.
	if bc.rollupStarted.Load() {
		bc.stopRollupOnce.Do(func() {
			bc.rollupStopped.Store(true)
			close(bc.stopRollup)
		})
		// Join the workers so no pass is in flight on the cancelled runtime
		// ctx while we drain on the fresh ctx below. rollupWg is the same
		// WaitGroup Close() joins; draining it here is safe because the
		// workers exit on stopRollup.
		bc.rollupWg.Wait()
	}

	// Step 2: drain whatever remains on a FRESH, non-cancelled context with a
	// bounded grace deadline. This is the crux of the fix: the drain context
	// is decoupled from the cancelled runtime context, so StoreChunk and the
	// ObjectIDPersister see a live context and the in-flight rollup completes
	// instead of dying with context.Canceled.
	drainCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := bc.DrainRollups(drainCtx); err != nil {
		// A grace-deadline timeout is benign: the remaining dirty intervals
		// are durable (the append log is fsynced) and resume on restart.
		// ErrDrainIncomplete is likewise non-fatal at shutdown — we did the
		// best-effort drain and any genuine residual recovers on next boot.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDrainIncomplete) {
			logger.Warn("GracefulStopRollup: drain did not fully complete within grace; remaining rollups resume on restart",
				"grace", grace, "error", err)
			return err
		}
		return fmt.Errorf("GracefulStopRollup: %w", err)
	}
	return nil
}

// dirtyLen returns the current dirty-interval count for payloadID, or 0
// when no tree exists. The btree is not safe for concurrent use, so the
// Len() read is serialized against AppendWrite / rollupFile via the
// per-file mutex.
func (bc *FSStore) dirtyLen(payloadID string) int {
	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	tree := sh.dirtyIntervals[payloadID]
	mu := sh.logLocks[payloadID]
	sh.mu.RUnlock()
	if tree == nil {
		return 0
	}
	if mu == nil {
		// No per-file mutex established yet (payload registered but never
		// written through getOrCreateLog). Guard the btree read against a
		// racing tree mutation under the shard lock instead.
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return tree.Len()
	}
	mu.Lock()
	defer mu.Unlock()
	return tree.Len()
}

// payloadsWithRealResidual filters the candidate payloads down to those
// that still carry GENUINE unflushed dirty data after a no-progress drain
// pass: the payload is NOT tombstoned AND at least one of its dirty
// intervals is backed by logIndex entries (so its bytes could have reached
// a chunk but did not). Tombstoned payloads and tree/logIndex-divergent
// intervals (no backing entries — the rollupFile DropExact case) are
// excluded; they are legitimately skipped by the drain.
//
// Per-payload state is read under the per-file mutex (and the logIndex's
// own mutex via EntriesForInterval) to match the dirtyLen / rollupFile
// serialization contract.
func (bc *FSStore) payloadsWithRealResidual(candidates []string) []string {
	var out []string
	for _, pid := range candidates {
		sh := bc.shardFor(pid)
		sh.mu.RLock()
		tree := sh.dirtyIntervals[pid]
		mu := sh.logLocks[pid]
		idx := sh.logIndices[pid]
		sh.mu.RUnlock()
		if tree == nil || idx == nil {
			continue
		}
		if bc.isTombstoned(pid) {
			continue
		}

		hasReal := false
		walk := func() {
			tree.t.Ascend(func(iv *interval) bool {
				if iv.Length == 0 {
					return true
				}
				if len(idx.EntriesForInterval(iv.Offset, uint64(iv.Length), nil)) > 0 {
					hasReal = true
					return false
				}
				return true
			})
		}
		if mu != nil {
			mu.Lock()
			walk()
			mu.Unlock()
		} else {
			sh.mu.RLock()
			walk()
			sh.mu.RUnlock()
		}
		if hasReal {
			out = append(out, pid)
		}
	}
	return out
}

// ResetLocalState clears ALL per-payload append-log state for the store —
// in-memory logIndices, dirty-interval trees, rollup offsets, truncation
// boundaries — closes every open log fd, and removes the on-disk `.log`
// files under <baseDir>/logs/. After ResetLocalState, ReadPayloadAt no
// longer replays any append-log records, so reads resolve purely through
// the (restored) CAS manifest.
//
// This is the snapshot-restore primitive: RestoreSnapshot resets the
// metadata store to a prior dump, but the block store's per-payload append
// log still holds post-snapshot write records. Without this reset,
// ReadPayloadAt's replayLogIntoDest would overlay those stale records on
// top of the restored CAS content ("last record wins"), returning the
// post-snapshot mutated bytes — silent corruption of in-place-modified
// files. Dropping the log makes the restored CAS manifest the sole source
// of truth.
//
// Safety precondition (enforced by the caller, RestoreSnapshot): both the
// snapshot being restored AND the pre-restore safety snapshot drained
// rollups, so every byte that must survive the restore is already durable
// in CAS. Any bytes that lived ONLY in the append log are post-snapshot
// writes the operator is deliberately discarding.
//
// Concurrency: the caller MUST have quiesced writes (the share is disabled
// during restore). ResetLocalState takes each shard lock (one at a time)
// for that shard's teardown so it does not race getOrCreateLog; it does
// not, however, defend against a concurrent in-flight AppendWrite
// mid-record — that is the caller's share-disabled barrier to provide.
func (bc *FSStore) ResetLocalState(_ context.Context) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	// Tear down every shard under its own lock (the caller has quiesced
	// writes, so a one-shard-at-a-time sweep is safe and avoids holding all
	// shard locks at once). Each shard's close+unlink+clear is atomic under
	// its lock so no getOrCreateLog can resurrect a half-cleared payload.
	var firstErr error
	for _, sh := range bc.logShards {
		sh.mu.Lock()
		for pid, lf := range sh.logFDs {
			if lf == nil {
				continue
			}
			if lf.f != nil {
				if cerr := lf.f.Close(); cerr != nil {
					logger.Warn("ResetLocalState: log file close failed",
						"payloadID", pid, "path", lf.path, "error", cerr)
				}
			}
			if lf.path != "" {
				if rerr := os.Remove(lf.path); rerr != nil && !os.IsNotExist(rerr) {
					logger.Error("ResetLocalState: log file unlink failed",
						"payloadID", pid, "path", lf.path, "error", rerr)
					if firstErr == nil {
						firstErr = fmt.Errorf("ResetLocalState: unlink log for payload %q: %w", pid, rerr)
					}
				}
			}
		}
		// Wipe this shard's per-payload maps so a subsequent AppendWrite
		// starts from a fresh log and ReadPayloadAt finds no log to replay.
		clear(sh.logFDs)
		clear(sh.logLocks)
		clear(sh.rollupLocks) // C1: keep rollupLocks 1:1 with logLocks across reset
		clear(sh.dirtyIntervals)
		clear(sh.logIndices)
		clear(sh.truncations)
		clear(sh.tombstones)
		sh.mu.Unlock()
	}
	bc.logBytesTotal.Store(0)

	// Best-effort: remove any residual .log files (including nested subdirs
	// created by slash-bearing payloadIDs) that had no live fd — e.g.,
	// orphaned by an interrupted DeleteAppendLog. Use RemoveAll + MkdirAll
	// to clear the entire tree atomically rather than a flat ReadDir that
	// misses nested layouts. A missing logs dir is fine — MkdirAll is
	// skipped so we don't create an empty directory that never existed.
	logsDir := filepath.Join(bc.baseDir, "logs")
	if _, serr := os.Stat(logsDir); serr == nil {
		if rerr := os.RemoveAll(logsDir); rerr != nil {
			logger.Warn("ResetLocalState: failed to remove logs dir",
				"path", logsDir, "error", rerr)
		} else if merr := os.MkdirAll(logsDir, 0755); merr != nil {
			logger.Warn("ResetLocalState: failed to recreate logs dir after removal",
				"path", logsDir, "error", merr)
		}
	}

	return firstErr
}
