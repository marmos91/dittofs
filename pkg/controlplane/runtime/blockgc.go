package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
)

// RunBlockGC is the runtime callable entrypoint for block-store garbage
// collection. It enumerates every share with a remote block store,
// deduplicates distinct underlying remote stores (ref-counted sharing
// across shares is possible — see docs/ARCHITECTURE.md "per-share
// isolation; remote stores ref-counted"), and invokes engine.CollectGarbage
// once per remote.
//
// Arguments:
//   - sharePrefix: restrict block enumeration to keys with this prefix
//     (empty = scan every block in the remote).
//   - dryRun: if true, report orphans without deleting.
//
// Returns the summed *engine.GCStats across all per-remote invocations and any
// fatal error.
func (r *Runtime) RunBlockGC(ctx context.Context, sharePrefix string, dryRun bool) (*engine.GCStats, error) {
	// Enumerate distinct underlying remote stores. Dedup is by configID (not
	// by the per-share nonClosingRemote wrapper pointer), so two shares that
	// reference the same remote-store config produce one GC invocation.
	entries := r.sharesSvc.DistinctRemoteStores()
	if len(entries) == 0 {
		logger.Info("RunBlockGC: no remote-backed shares registered; nothing to scan",
			"dryRun", dryRun, "sharePrefix", sharePrefix)
		return &engine.GCStats{}, nil
	}

	total := &engine.GCStats{}
	for _, entry := range entries {
		opts := &engine.Options{
			SharePrefix: sharePrefix,
			DryRun:      dryRun,
		}
		logger.Info("RunBlockGC: starting",
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"sharePrefix", sharePrefix)

		// r satisfies MetadataReconciler. stats is nil-safe — CollectGarbage
		// always returns a non-nil *engine.GCStats, but we defensively read
		// through a zero-valued copy so a future nil-returning edge case
		// never panics in the log statement.
		stats := collectGarbageFn(ctx, entry.Store, r, opts)
		var s engine.GCStats
		if stats != nil {
			s = *stats
			total.SharesScanned += s.SharesScanned
			total.BlocksScanned += s.BlocksScanned
			total.OrphanFiles += s.OrphanFiles
			total.OrphanBlocks += s.OrphanBlocks
			total.BytesReclaimed += s.BytesReclaimed
			total.Errors += s.Errors
		}
		logger.Info("RunBlockGC: complete",
			"configID", entry.ConfigID,
			"orphanFiles", s.OrphanFiles,
			"orphanBlocks", s.OrphanBlocks,
			"bytesReclaimed", s.BytesReclaimed,
			"errors", s.Errors)
	}
	return total, nil
}

// collectGarbageFn is a package-level indirection that lets tests intercept
// the engine.CollectGarbage call. Production code always resolves to
// engine.CollectGarbage.
var collectGarbageFn = engine.CollectGarbage

// setShareRemoteForTest injects a remote store for an already-registered
// share. Delegates to shares.Service.SetShareRemoteForTest. Test-only.
func (r *Runtime) setShareRemoteForTest(shareName string, rs remote.RemoteStore) {
	r.sharesSvc.SetShareRemoteForTest(shareName, rs)
}
