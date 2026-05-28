package runtime

import (
	"context"
	"path/filepath"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
)

// AuditRefcounts runs the refcount reconciliation audit for the named
// share. Resolves the share's metadata store and audit-state root, then
// delegates to engine.AuditRefcounts which walks the metadata and
// computes:
//
//	∑ FileBlock.RefCount   vs   ∑ len(FileAttr.Blocks)
//
// A non-zero delta indicates refcount drift (leak or missed decrement).
// Last-run summary is persisted under
// <localStoreRoot>/audit-state/last-inv02.json — analogous to GC's
// last-run.json under <gcStateRoot>/.
//
// Returns ErrShareNotFound (wrapped) when the share is unknown. The
// audit runs to completion and is operator-invoked (no periodic
// schedule in v0.15.0).
func (r *Runtime) AuditRefcounts(ctx context.Context, shareName string) (*engine.AuditRefcountsResult, error) {
	mds, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	// Audit-state root sits at the same depth as gc-state under the
	// share's local store directory: <basePath>/shares/<sanitized>/.
	// Reuse the share-resolver path that already returns
	// <basePath>/shares/<sanitized>/gc-state and trim the trailing
	// "gc-state" so engine.AuditRefcounts can append "audit-state".
	// Empty gcStateRoot (in-memory backend) yields empty
	// localStoreRoot, which engine.AuditRefcounts treats as
	// "do not persist" (mirrors gcstate's empty-root contract).
	gcRoot, err := r.sharesSvc.GetGCStateDirForShare(shareName)
	if err != nil {
		return nil, err
	}
	var localStoreRoot string
	if gcRoot != "" {
		localStoreRoot = filepath.Dir(gcRoot)
	}

	logger.Info("RunAuditRefcountsForShare: starting",
		"share", shareName,
		"localStoreRoot", localStoreRoot,
	)
	res, err := engine.AuditRefcounts(ctx, shareName, mds, localStoreRoot)
	if err != nil {
		return nil, err
	}
	logger.Info("RunAuditRefcountsForShare: complete",
		"share", shareName,
		"totalFiles", res.TotalFiles,
		"totalRefs", res.TotalRefs,
		"totalRefCount", res.TotalRefCount,
		"delta", res.Delta,
		"durationMs", res.DurationMS,
	)
	return res, nil
}
