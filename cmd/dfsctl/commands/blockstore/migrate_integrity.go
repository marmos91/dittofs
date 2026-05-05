package blockstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrIntegrityCheckFailed is returned by verifyIntegrity when one or
// more unique CAS keys referenced by the post-migration FileAttr.Blocks
// either could not be HEADed (404 / network error coerced to mismatch)
// or returned a content-hash header that did not match the expected
// blake3:{hex} derived from the key. D-A12 + MIG-04.
//
// Callers MUST treat this as fail-loud (D-A8): leave BlockLayout=legacy,
// leave the journal in place, leave any uploaded CAS chunks in S3
// (orphaned hashes — GC reclaims). No automatic legacy-key restore.
var ErrIntegrityCheckFailed = errors.New("post-migration integrity check failed (MIG-04)")

// integrityResult is the verifyIntegrity per-share summary. UniqueHashes
// counts the cardinality of the union FileAttr.Blocks[*].Hash across all
// migrated files; HEADCalls is the actual S3 HEAD count (== UniqueHashes
// on a clean run, may be less on early ctx cancel). Failures captures
// per-key error strings for the operator to inspect.
type integrityResult struct {
	UniqueHashes int
	HEADCalls    int
	Failures     []string
}

// verifyIntegrity walks the share's metadata, collects the union of
// FileAttr.Blocks[*].Hash across every regular file, then HEADs each
// unique CAS key in parallel. Asserts:
//
//  1. Every key returns 200 (no ErrBlockNotFound).
//  2. Every response's "content-hash" metadata equals
//     "blake3:" + hex(hash) derived from the key path (D-A12 parity check).
//
// Concurrency: errgroup-bounded by opts.parallel (default 4). The HEAD
// fleet does NOT charge against the upload bandwidth limiter (HEADs
// are verification, not uploads — D-A9 governs uploads only).
//
// Returns ErrIntegrityCheckFailed wrapped with diagnostic context if
// any key fails. Network / transport errors that are not
// ErrBlockNotFound bubble up unwrapped so the caller can distinguish
// "data missing" (fail-loud, no retry) from "transient error" (operator
// retries).
func verifyIntegrity(ctx context.Context, svc *offlineRuntime, opts migrateOptions) (integrityResult, error) {
	if svc == nil {
		return integrityResult{}, errors.New("verifyIntegrity: nil offlineRuntime")
	}
	if svc.RemoteStore() == nil {
		return integrityResult{}, errors.New("verifyIntegrity: nil remote store")
	}

	// Step 1: aggregate the unique-hash set by re-walking the share's
	// post-migration metadata. Re-using migrate.WalkShareFiles (Plan
	// 14-03) keeps the integrity check self-contained — it does not
	// rely on the journal being intact.
	uniq := make(map[blockstore.ContentHash]struct{})
	walkErr := migrate.WalkShareFiles(ctx, svc.MetadataStore(), opts.share,
		func(_ metadata.FileHandle, file *metadata.File) error {
			for _, br := range file.Blocks {
				uniq[br.Hash] = struct{}{}
			}
			return nil
		})
	if walkErr != nil {
		return integrityResult{}, fmt.Errorf("verifyIntegrity: walk share: %w", walkErr)
	}

	hashes := make([]blockstore.ContentHash, 0, len(uniq))
	for h := range uniq {
		hashes = append(hashes, h)
	}

	// Step 2: HEAD each unique key, parallel-bounded by --parallel.
	// errgroup gives us first-error cancellation + Wait semantics.
	parallel := opts.parallel
	if parallel < 1 {
		parallel = 4
	}

	var (
		failuresMu sync.Mutex
		failures   []string
		calls      atomic.Int64
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)

	rs := svc.RemoteStore()
	for _, h := range hashes {
		h := h
		g.Go(func() error {
			key := blockstore.FormatCASKey(h)
			res, err := rs.HeadObject(gctx, key)
			calls.Add(1)
			if err != nil {
				if errors.Is(err, blockstore.ErrBlockNotFound) {
					// Missing-object failures are aggregated and surface
					// as ErrIntegrityCheckFailed at the end; they do NOT
					// abort other in-flight HEADs (operator wants the
					// full failure list, not the first one).
					failuresMu.Lock()
					failures = append(failures, fmt.Sprintf("%s: missing", key))
					failuresMu.Unlock()
					return nil
				}
				// Network / transport error: bubble up unwrapped so the
				// caller can distinguish transient from permanent.
				return fmt.Errorf("HEAD %s: %w", key, err)
			}

			want := h.CASKey() // "blake3:{hex}"
			got := res.Metadata["content-hash"]
			if got != want {
				failuresMu.Lock()
				failures = append(failures, fmt.Sprintf(
					"%s: header mismatch want=%q got=%q", key, want, got))
				failuresMu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return integrityResult{
			UniqueHashes: len(hashes),
			HEADCalls:    int(calls.Load()),
			Failures:     failures,
		}, err
	}

	result := integrityResult{
		UniqueHashes: len(hashes),
		HEADCalls:    int(calls.Load()),
		Failures:     failures,
	}
	if len(failures) > 0 {
		// Sort-stable order is sufficient — operator triages by reading
		// the first entry. Detailed list logged at info level for
		// machine-parseable consumers.
		logger.Error("blockstore migrate: integrity check failed",
			"share", opts.share,
			"unique_hashes", len(hashes),
			"failures", len(failures),
			"first_failure", failures[0],
		)
		return result, fmt.Errorf("%w: %d/%d unique hashes failed; first: %s",
			ErrIntegrityCheckFailed, len(failures), len(hashes), failures[0])
	}

	logger.Info("blockstore migrate: integrity check passed",
		"share", opts.share,
		"unique_hashes", len(hashes),
		"head_calls", int(calls.Load()),
	)
	return result, nil
}
