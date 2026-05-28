package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
)

// VerifyRemoteDurability probes the remote store for every hash in the
// manifest and reports the first miss. It dispatches up to concurrency
// parallel Head() probes; the first probe to return ErrChunkNotFound
// cancels in-flight siblings and the call returns a wrapped
// blockstore.ErrChunkNotFound naming the missing hash. Non-NotFound
// I/O errors propagate unchanged (not wrapped as ErrChunkNotFound) so
// callers can distinguish "block is genuinely absent on remote" from
// "remote was unreachable mid-verify."
//
// Iteration order matches manifest.Sorted (deterministic; mirrors
// WriteManifest), so given the same inputs every run reports the same
// missing hash first.
//
// concurrency values <= 0 are clamped to 1 (safe lower bound; never
// deadlocks, never panics). A nil or empty manifest returns nil
// without any remote I/O. The caller's ctx is the only deadline source;
// there is no internal timeout.
//
// Caller wraps the returned error with models.ErrSnapshotVerifyFailed
// at the Runtime orchestration layer; this helper stays purely
// blockstore-package-oriented.
func VerifyRemoteDurability(
	ctx context.Context,
	rs remote.RemoteStore,
	manifest *blockstore.HashSet,
	concurrency int,
) error {
	if manifest == nil || manifest.Len() == 0 {
		return nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}

	// Derive a cancellable child ctx so the first miss / I-O error
	// can short-circuit sibling probes.
	errCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once

	recordErr := func(err error) {
		firstErrOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	hashes := manifest.Sorted()
loop:
	for _, h := range hashes {
		select {
		case sem <- struct{}{}:
			// Acquired a worker slot, dispatch the probe.
		case <-errCtx.Done():
			// Either parent ctx cancelled or a sibling probe failed —
			// stop dispatching new work.
			break loop
		}

		wg.Add(1)
		go func(hash blockstore.ContentHash) {
			defer wg.Done()
			defer func() { <-sem }()

			_, err := rs.Head(errCtx, hash)
			switch {
			case err == nil:
				return
			case errors.Is(err, blockstore.ErrChunkNotFound):
				logger.Debug("snapshot sync gate: missing hash on remote",
					"hash", hash.String(),
				)
				recordErr(fmt.Errorf(
					"snapshot: remote durability verify: missing hash %s: %w",
					hash, blockstore.ErrChunkNotFound,
				))
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// Sibling probe cancelled us; do NOT overwrite firstErr
				// — let the sibling's error win.
				return
			default:
				logger.Error("snapshot sync gate: head probe failed",
					"hash", hash.String(),
					"error", err,
				)
				recordErr(fmt.Errorf(
					"snapshot: remote durability verify: head hash %s: %w",
					hash, err,
				))
			}
		}(h)
	}

	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	// No probe-derived error; surface parent ctx cancel if it happened
	// (we may have broken out of the dispatch loop without dispatching
	// all hashes).
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
