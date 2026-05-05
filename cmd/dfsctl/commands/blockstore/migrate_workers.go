package blockstore

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// walkedFile is the per-file unit dispatched to the worker pool. It
// pairs the encoded file handle with a snapshot of the metadata
// FileAttr returned by the share walk.
type walkedFile struct {
	Handle metadata.FileHandle
	Attr   *metadata.File
}

// maxWorkerSoftCap clamps the operator-supplied --parallel value to a
// sane ceiling. Above this the marginal goroutine-scheduling cost +
// S3 endpoint pressure outweighs throughput gains; the threat model
// (T-14-04-02) calls this out as "accept + warn-log + clamp".
const maxWorkerSoftCap = 64

// workerPoolMigrateOneFile is the indirection point tests use to
// substitute migrateOneFile. The default value is the real loop's
// per-file orchestrator. Tests swap it for a stub to observe
// concurrency / cancellation / dispatch decisions without exercising
// the chunker + remote store machinery.
var workerPoolMigrateOneFile = migrateOneFile

// workerPool dispatches per-file migration over a fixed worker count
// using errgroup for lifecycle + cancellation propagation. Mirrors the
// pattern in pkg/blockstore/engine/syncer.go (errgroup + SetLimit).
//
// Per D-A1 + D-A10, the atomic unit dispatched to a worker is one
// file. Bandwidth metering happens inside migrateOneFile via the
// shared *rate.Limiter — workers do NOT shard bandwidth across
// themselves, all upload bytes pass through one limiter so the
// configured ceiling is honored across the fleet.
type workerPool struct {
	parallel int
	rt       *offlineRuntime
	journal  *migrate.Journal
	opts     migrateOptions
	limiter  *rate.Limiter
	progress *progressReporter

	mu     sync.Mutex
	result migrateResult
}

// newWorkerPool clamps parallel into [1, maxWorkerSoftCap] and warns
// (slog) when the input is outside the band so the operator can see
// the effective concurrency in the structured log.
func newWorkerPool(
	parallel int,
	rt *offlineRuntime,
	journal *migrate.Journal,
	opts migrateOptions,
	limiter *rate.Limiter,
	progress *progressReporter,
) *workerPool {
	if parallel <= 0 {
		logger.Warn("blockstore migrate: --parallel <= 0, defaulting to 1",
			"requested", parallel,
		)
		parallel = 1
	} else if parallel > maxWorkerSoftCap {
		logger.Warn("blockstore migrate: --parallel above soft cap, clamped",
			"requested", parallel,
			"clamped", maxWorkerSoftCap,
		)
		parallel = maxWorkerSoftCap
	}
	return &workerPool{
		parallel: parallel,
		rt:       rt,
		journal:  journal,
		opts:     opts,
		limiter:  limiter,
		progress: progress,
	}
}

// Run dispatches each file in files to a worker. Files already
// recorded as done in the journal are skipped synchronously (no
// goroutine spawn). The first non-nil per-file error cancels the
// errgroup ctx; remaining files observe the cancellation and short-
// circuit. Returns the aggregated migrateResult and the first error
// (or nil if every file completed successfully).
func (wp *workerPool) Run(ctx context.Context, files []walkedFile) (migrateResult, error) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(wp.parallel)

	for _, f := range files {
		f := f

		// Stop submitting once the errgroup ctx is cancelled (first
		// worker error or external cancellation). errgroup.Go itself
		// has no early-exit on g context cancel — without this check
		// the loop would queue every remaining file even after the
		// first failure, defeating the cancellation semantics tested
		// by TestWorkerPool_FirstErrorCancels. Break out of the loop
		// rather than returning directly so g.Wait() can surface the
		// canonical first error (not gctx.Err(), which would mask it).
		select {
		case <-gctx.Done():
			goto wait
		default:
		}

		// Resume short-circuit: skip BEFORE spawning a goroutine.
		// Avoids paying the goroutine-spawn cost on resume runs that
		// are mostly already-done. journal may be nil in unit tests
		// that exercise the pool standalone.
		if wp.journal != nil && wp.journal.IsFileDone(string(f.Handle)) {
			wp.mu.Lock()
			wp.result.FilesSkipped++
			wp.mu.Unlock()
			continue
		}

		g.Go(func() error {
			r, err := workerPoolMigrateOneFile(
				gctx, wp.rt, wp.journal, wp.opts, wp.limiter,
				f.Handle, f.Attr,
			)
			if err != nil {
				return err
			}
			wp.mu.Lock()
			if r.Skipped {
				wp.result.FilesSkipped++
			} else {
				wp.result.FilesDone++
				wp.result.BytesUploaded += r.BytesUploaded
				wp.result.BytesDeduped += r.BytesDeduped
			}
			wp.mu.Unlock()
			if wp.progress != nil && !r.Skipped {
				wp.progress.OnFileCommit(r)
			}
			return nil
		})
	}

wait:
	if err := g.Wait(); err != nil {
		// Take a snapshot of the partial result so the caller can
		// report bytes-already-uploaded even on early termination.
		return wp.snapshotResult(), err
	}
	return wp.snapshotResult(), nil
}

// snapshotResult returns a value-copy of the accumulated result under
// the pool mutex. Used by the dispatch-cancel + g.Wait error paths.
func (wp *workerPool) snapshotResult() migrateResult {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.result
}
