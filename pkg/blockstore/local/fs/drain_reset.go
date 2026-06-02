package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
		bc.logsMu.RLock()
		pending := make([]string, 0, len(bc.dirtyIntervals))
		for pid := range bc.dirtyIntervals {
			pending = append(pending, pid)
		}
		bc.logsMu.RUnlock()

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

// dirtyLen returns the current dirty-interval count for payloadID, or 0
// when no tree exists. The btree is not safe for concurrent use, so the
// Len() read is serialized against AppendWrite / rollupFile via the
// per-file mutex.
func (bc *FSStore) dirtyLen(payloadID string) int {
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if tree == nil {
		return 0
	}
	if mu == nil {
		// No per-file mutex established yet (payload registered but never
		// written through getOrCreateLog). Guard the btree read against a
		// racing tree mutation under the shared logsMu instead.
		bc.logsMu.RLock()
		defer bc.logsMu.RUnlock()
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
		bc.logsMu.RLock()
		tree := bc.dirtyIntervals[pid]
		mu := bc.logLocks[pid]
		idx := bc.logIndices[pid]
		bc.logsMu.RUnlock()
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
			bc.logsMu.RLock()
			walk()
			bc.logsMu.RUnlock()
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
// during restore). ResetLocalState takes bc.logsMu for the whole teardown
// so it does not race getOrCreateLog; it does not, however, defend against
// a concurrent in-flight AppendWrite mid-record — that is the caller's
// share-disabled barrier to provide.
func (bc *FSStore) ResetLocalState(_ context.Context) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	bc.logsMu.Lock()
	defer bc.logsMu.Unlock()

	var firstErr error
	for pid, lf := range bc.logFDs {
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

	// Wipe every per-payload map so a subsequent AppendWrite starts from a
	// fresh log and a subsequent ReadPayloadAt finds no log to replay.
	clear(bc.logFDs)
	clear(bc.logLocks)
	clear(bc.rollupLocks) // C1: keep rollupLocks 1:1 with logLocks across reset
	clear(bc.dirtyIntervals)
	clear(bc.logIndices)
	clear(bc.truncations)
	clear(bc.tombstones)
	bc.logBytesTotal.Store(0)

	// Best-effort: remove any residual .log files left under the logs dir
	// that had no live fd (e.g. orphaned by an interrupted DeleteAppendLog).
	// A missing logs dir is fine — nothing was ever written.
	logsDir := filepath.Join(bc.baseDir, "logs")
	if entries, derr := os.ReadDir(logsDir); derr == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			full := filepath.Join(logsDir, e.Name())
			if rerr := os.Remove(full); rerr != nil && !os.IsNotExist(rerr) {
				logger.Warn("ResetLocalState: residual log unlink failed",
					"path", full, "error", rerr)
			}
		}
	}

	return firstErr
}
