package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// VerifyRemoteDurability probes the remote store for every hash in the
// manifest and reports the first miss. It dispatches up to concurrency
// parallel Head() probes; the first probe to return ErrChunkNotFound
// cancels in-flight siblings and the call returns a wrapped
// block.ErrChunkNotFound naming the missing hash. Non-NotFound
// I/O errors propagate unchanged (not wrapped as ErrChunkNotFound) so
// callers can distinguish "block is genuinely absent on remote" from
// "remote was unreachable mid-verify."
//
// Iteration order matches manifest.Sorted, but with concurrency > 1 the
// first miss observed depends on remote latency — different remotes can
// surface a later sorted hash before an earlier one. This helper only
// guarantees that *some* missing hash is reported when any are absent.
//
// concurrency <= 0 is clamped to 1. A nil or empty manifest returns nil
// without any remote I/O — verifying nothing is vacuously durable. This
// short-circuit is intentional and applies to GENUINELY-empty manifests
// (a truly empty share). It is NOT a license to report durability over a
// spuriously-empty manifest: detecting "empty manifest on a non-empty
// share" (the hollow-durability case) is the caller's responsibility.
// Runtime.runSnapshotOrchestration cross-checks an empty manifest against
// the live FileBlock enumeration before this call and fails the snapshot
// if the share still references hashes — see the empty-manifest guard there.
//
// Caller wraps the returned error with models.ErrSnapshotVerifyFailed
// at the Runtime orchestration layer; this helper stays purely
// blockstore-package-oriented.
func VerifyRemoteDurability(
	ctx context.Context,
	rs remote.RemoteStore,
	manifest *block.HashSet,
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

loop:
	for _, h := range manifest.Sorted() {
		select {
		case sem <- struct{}{}:
		case <-errCtx.Done():
			// Parent ctx cancelled or a sibling probe failed.
			break loop
		}

		wg.Add(1)
		go func(hash block.ContentHash) {
			defer wg.Done()
			defer func() { <-sem }()

			_, err := rs.Head(errCtx, hash)
			switch {
			case err == nil:
				return
			case errors.Is(err, block.ErrChunkNotFound):
				logger.Debug("snapshot verify: missing hash on remote", "hash", hash.String())
				recordErr(fmt.Errorf(
					"snapshot: remote durability verify: missing hash %s: %w",
					hash, block.ErrChunkNotFound,
				))
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// Distinguish "sibling probe cancelled us" (safe to drop)
				// from "the remote itself returned a ctx-class error
				// before our cancel fired" (real failure). If errCtx is
				// still live, the remote-side ctx error must be recorded
				// or the verifier would silently skip a chunk and report
				// success.
				if errCtx.Err() == nil {
					logger.Error("snapshot verify: head probe failed with ctx error pre-cancel",
						"hash", hash.String(), "error", err)
					recordErr(fmt.Errorf(
						"snapshot: remote durability verify: head probe %s: %w",
						hash, err,
					))
				}
			default:
				logger.Error("snapshot verify: head probe failed",
					"hash", hash.String(), "error", err)
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
