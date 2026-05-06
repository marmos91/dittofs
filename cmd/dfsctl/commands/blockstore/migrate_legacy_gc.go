package blockstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// deleteLegacyKeys enumerates every legacy {payloadID}/block-{idx} key
// the share's remote store sees and deletes them. Best-effort sweep:
// per-key DeleteBlock errors are aggregated and reported via the
// returned error (so the operator can chase orphaned keys with `dfsctl
// store block gc` or external tooling), but they do NOT abort the
// sweep — D-A13.
//
// Strict ordering: callers MUST have run performCutover to success
// first. Once BlockLayout=cas-only, the daemon's read path can no
// longer reach the legacy keys; deleting them is then safe.
//
// Filtering: ListByPrefix("") returns every key the store sees. We
// skip cas/* keys (the freshly-uploaded migration target) and let
// blockstore.ParseStoreKey discriminate true legacy keys from any
// future-format keys this code does not know about. Keys that don't
// parse as either are left alone.
//
// Returns the count of deletions that succeeded and a non-nil error
// summarizing any per-key failures. A nil error means every legacy
// key was deleted; the count + nil-error pair == "share fully cut
// over to CAS, no orphans".
//
// Concurrency: opts.parallel-bounded errgroup. The HEAD/list IS NOT
// metered against the upload bandwidth limiter — the limiter governs
// uploads only (D-A9).
//
// Note on TB-scale shares: ListByPrefix("") with millions of keys can
// be costly on S3-compatible backends without efficient cursor pagination.
// The current implementation accepts the cost in the migration window;
// a per-payload-id streaming variant is a deferred follow-up if the
// runbook (Plan 14-07) surfaces it as a real problem at scale (T-14-05-04).
func deleteLegacyKeys(ctx context.Context, svc *offlineRuntime, opts migrateOptions) (int, error) {
	if svc == nil {
		return 0, errors.New("deleteLegacyKeys: nil offlineRuntime")
	}
	rs := svc.RemoteStore()
	if rs == nil {
		return 0, errors.New("deleteLegacyKeys: nil remote store")
	}

	keys, err := rs.ListByPrefix(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("deleteLegacyKeys: list remote keys: %w", err)
	}

	legacy := make([]string, 0, len(keys))
	for _, k := range keys {
		// CAS keys are the post-migration data; never sweep them.
		if strings.HasPrefix(k, "cas/") {
			continue
		}
		// Only delete keys that parse as legacy {payloadID}/block-{idx};
		// anything else (other future schemes, debug objects, etc.) is
		// left for the operator to triage.
		if _, _, ok := blockstore.ParseStoreKey(k); ok {
			legacy = append(legacy, k)
		}
	}
	logger.Info("blockstore migrate: legacy keys identified for deletion",
		"share", opts.share,
		"count", len(legacy),
	)

	if len(legacy) == 0 {
		return 0, nil
	}

	parallel := opts.parallel
	if parallel < 1 {
		parallel = 4
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	var (
		failuresMu sync.Mutex
		failures   []string
		deleted    atomic.Int64
	)

	for _, k := range legacy {
		g.Go(func() error {
			if err := rs.DeleteBlock(gctx, k); err != nil {
				failuresMu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", k, err))
				failuresMu.Unlock()
				return nil // best-effort; do NOT abort the sweep
			}
			deleted.Add(1)
			return nil
		})
	}
	// Best-effort sweep — per-key errors are captured into `failures`.
	// g.Wait should not return non-nil since worker goroutines never
	// return errors directly, but log if it does so future regressions
	// don't go silent.
	if werr := g.Wait(); werr != nil {
		logger.Error("blockstore migrate: legacy GC errgroup wait returned",
			"share", opts.share, "error", werr)
	}

	count := int(deleted.Load())
	if len(failures) > 0 {
		logger.Warn("blockstore migrate: legacy delete had partial failures",
			"share", opts.share,
			"deleted", count,
			"failed", len(failures),
			"first_failure", failures[0],
		)
		return count, fmt.Errorf("deleteLegacyKeys: %d of %d keys failed; first: %s",
			len(failures), len(legacy), failures[0])
	}
	logger.Info("blockstore migrate: legacy keys deleted",
		"share", opts.share,
		"deleted", count,
	)
	return count, nil
}
