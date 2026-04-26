package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// RunBlockGC is the runtime callable entrypoint for block-store garbage
// collection. It enumerates every share with a remote block store,
// deduplicates distinct underlying remote stores (ref-counted sharing
// across shares is possible — see docs/ARCHITECTURE.md "per-share
// isolation; remote stores ref-counted"), and invokes engine.CollectGarbage
// once per remote.
//
// Phase 11 Plan 06 cross-share aggregation (D-03): each per-remote
// invocation receives a MultiShareReconciler scoped to the shares
// pointing at that remote, so the mark phase enumerates the union of
// every share's FileBlocks. Without this, two shares sharing one remote
// would each delete the other's CAS objects.
//
// Arguments:
//   - sharePrefix: HISTORICAL — preserved in the signature for callers
//     that still pass it, but Phase 11 WR-04 removed engine.Options.SharePrefix
//     because the mark-sweep design has a global live set and per-share
//     scoping no longer makes sense. A non-empty value is logged at WARN
//     so operators see that the flag is no longer honored, then ignored.
//   - dryRun: if true, report orphans without deleting.
//
// Returns the summed *engine.GCStats across all per-remote invocations and any
// fatal error.
func (r *Runtime) RunBlockGC(ctx context.Context, sharePrefix string, dryRun bool) (*engine.GCStats, error) {
	if sharePrefix != "" {
		logger.Warn("RunBlockGC: sharePrefix is no longer honored — mark-sweep uses a global live set across every cas/XX prefix",
			"ignored_sharePrefix", sharePrefix)
	}
	// Enumerate distinct underlying remote stores. Dedup is by configID (not
	// by the per-share nonClosingRemote wrapper pointer), so two shares that
	// reference the same remote-store config produce one GC invocation.
	entries := r.sharesSvc.DistinctRemoteStores()
	if len(entries) == 0 {
		logger.Info("RunBlockGC: no remote-backed shares registered; nothing to scan",
			"dryRun", dryRun)
		return &engine.GCStats{}, nil
	}

	gcDefaults := r.gcDefaultsSnapshot()
	total := &engine.GCStats{}
	for _, entry := range entries {
		opts := &engine.Options{
			DryRun: dryRun,
			// Phase 11 IN-3-04: thread the remote-store config UUID
			// and the per-remote share scope into engine.Options so the
			// engine's own start/complete log lines carry the
			// correlation keys SREs need for cross-checking against
			// S3 access logs.
			RemoteEndpointID: entry.ConfigID,
			Shares:           append([]string(nil), entry.Shares...),
		}
		applyGCDefaults(opts, gcDefaults)
		logger.Info("RunBlockGC: starting",
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"gracePeriod", opts.GracePeriod,
			"sweepConcurrency", opts.SweepConcurrency,
			"dryRunSampleSize", opts.DryRunSampleSize)

		// Per-remote MultiShareReconciler: the mark phase enumerates
		// EnumerateFileBlocks across every share pointing at this
		// remote so the live set is the union (D-03). Without this,
		// each share's GC would treat the other's CAS objects as
		// orphans.
		rec := &perRemoteReconciler{rt: r, shares: entry.Shares}

		stats := collectGarbageFn(ctx, entry.Store, rec, opts)
		s := accumulateGCStats(total, stats, false)
		logger.Info("RunBlockGC: complete",
			"configID", entry.ConfigID,
			"hashesMarked", s.HashesMarked,
			"objectsSwept", s.ObjectsSwept,
			"bytesFreed", s.BytesFreed,
			"errors", s.ErrorCount,
		)
	}
	return total, nil
}

// perRemoteReconciler scopes the GC mark phase to the shares pointing at
// a single remote store. Implements engine.MultiShareReconciler.
type perRemoteReconciler struct {
	rt     *Runtime
	shares []string
}

// SharesForGC implements engine.MultiShareReconciler.
func (p *perRemoteReconciler) SharesForGC() []string { return p.shares }

// GetMetadataStoreForShare delegates to the wrapped Runtime so the engine
// receives the per-share metadata store.
func (p *perRemoteReconciler) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	return p.rt.GetMetadataStoreForShare(shareName)
}

// RunBlockGCForShare looks up the named share, derives its persistent
// gc-state directory (Phase 11 D-10), and dispatches a GC run that scopes
// last-run.json persistence to that share. Behavior beyond persistence
// matches RunBlockGC: every distinct remote referenced by any registered
// share is scanned, and the cross-share live set (D-03) is enforced —
// the {name} parameter only narrows where the run summary is written, not
// what is marked or swept.
//
// Returns an ErrShareNotFound-wrapped error if name is unknown.
func (r *Runtime) RunBlockGCForShare(ctx context.Context, name string, dryRun bool) (*engine.GCStats, error) {
	gcRoot, err := r.sharesSvc.GetGCStateDirForShare(name)
	if err != nil {
		return nil, err
	}

	entries := r.sharesSvc.DistinctRemoteStores()
	if len(entries) == 0 {
		logger.Info("RunBlockGCForShare: no remote-backed shares registered; nothing to scan",
			"share", name, "dryRun", dryRun)
		return &engine.GCStats{}, nil
	}

	gcDefaults := r.gcDefaultsSnapshot()
	total := &engine.GCStats{}
	for _, entry := range entries {
		opts := &engine.Options{
			DryRun:      dryRun,
			GCStateRoot: gcRoot,
			// Phase 11 IN-3-04: surface per-remote correlation in the
			// engine logs even on the per-share path.
			RemoteEndpointID: entry.ConfigID,
			Shares:           append([]string(nil), entry.Shares...),
		}
		applyGCDefaults(opts, gcDefaults)
		logger.Info("RunBlockGCForShare: starting",
			"share", name,
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"gcStateRoot", gcRoot,
			"gracePeriod", opts.GracePeriod,
			"sweepConcurrency", opts.SweepConcurrency,
			"dryRunSampleSize", opts.DryRunSampleSize,
		)

		rec := &perRemoteReconciler{rt: r, shares: entry.Shares}
		stats := collectGarbageFn(ctx, entry.Store, rec, opts)
		s := accumulateGCStats(total, stats, true)
		logger.Info("RunBlockGCForShare: complete",
			"share", name,
			"configID", entry.ConfigID,
			"hashesMarked", s.HashesMarked,
			"objectsSwept", s.ObjectsSwept,
			"bytesFreed", s.BytesFreed,
			"errors", s.ErrorCount,
		)
	}
	return total, nil
}

// GCStateDirForShare returns the per-share gc-state directory the GC
// engine writes `last-run.json` into. Empty when the share's local store
// has no persistent root (in-memory backend). Returns ErrShareNotFound
// when the share is unknown.
func (r *Runtime) GCStateDirForShare(name string) (string, error) {
	return r.sharesSvc.GetGCStateDirForShare(name)
}

// applyGCDefaults overlays operator-configured GC defaults onto opts. Phase
// 11 WR-01: without this, the gc.grace_period / gc.sweep_concurrency /
// gc.dry_run_sample_size knobs surfaced in pkg/config and validated at
// startup were silently ignored — every CollectGarbage invocation
// fell back to the engine's hardcoded defaults (1h, 16, 1000).
//
// Per-call opts fields take precedence over defaults so a future caller
// can still override on a single run.
func applyGCDefaults(opts *engine.Options, defaults *GCDefaults) {
	if defaults == nil || opts == nil {
		return
	}
	if opts.GracePeriod == 0 && defaults.GracePeriod > 0 {
		opts.GracePeriod = defaults.GracePeriod
	}
	if opts.SweepConcurrency == 0 && defaults.SweepConcurrency > 0 {
		opts.SweepConcurrency = defaults.SweepConcurrency
	}
	if opts.DryRunSampleSize == 0 && defaults.DryRunSampleSize > 0 {
		opts.DryRunSampleSize = defaults.DryRunSampleSize
	}
}

// collectGarbageFn is a package-level indirection that lets tests intercept
// the engine.CollectGarbage call. Production code always resolves to
// engine.CollectGarbage.
var collectGarbageFn = engine.CollectGarbage

// accumulateGCStats folds a per-remote stats result into total and returns
// the per-remote snapshot for logging. Returns a zero value when stats is
// nil — CollectGarbage always returns non-nil but the defensive copy keeps
// callers panic-free if that ever changes. When includeDryRunMeta is true,
// also propagates DryRun, DryRunCandidates, and FirstErrors.
func accumulateGCStats(total, stats *engine.GCStats, includeDryRunMeta bool) engine.GCStats {
	if stats == nil {
		return engine.GCStats{}
	}
	s := *stats
	total.HashesMarked += s.HashesMarked
	total.ObjectsSwept += s.ObjectsSwept
	total.BytesFreed += s.BytesFreed
	total.ErrorCount += s.ErrorCount
	total.SharesScanned += s.SharesScanned
	total.BlocksScanned += s.BlocksScanned
	total.OrphanFiles += s.OrphanFiles
	total.OrphanBlocks += s.OrphanBlocks
	total.BytesReclaimed += s.BytesReclaimed
	total.Errors += s.Errors
	if includeDryRunMeta {
		if s.DryRun {
			total.DryRun = true
		}
		total.DryRunCandidates = append(total.DryRunCandidates, s.DryRunCandidates...)
		total.FirstErrors = append(total.FirstErrors, s.FirstErrors...)
	}
	return s
}

// setShareRemoteForTest injects a remote store for an already-registered
// share. Delegates to shares.Service.SetShareRemoteForTest. Test-only.
func (r *Runtime) setShareRemoteForTest(shareName string, rs remote.RemoteStore) {
	r.sharesSvc.SetShareRemoteForTest(shareName, rs)
}
