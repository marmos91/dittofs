package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// gcResult maps accumulated GC stats to a bounded result label for the
// gc_runs_total counter: "error" if the pass captured any per-object or
// fatal error, "ok" otherwise.
func gcResult(total *engine.GCStats) string {
	if total == nil || total.ErrorCount > 0 {
		return "error"
	}
	return "ok"
}

// RunBlockGC is the runtime callable entrypoint for block-store garbage
// collection. It enumerates every share with a remote block store,
// deduplicates distinct underlying remote stores (ref-counted sharing
// across shares is possible — see docs/ARCHITECTURE.md "per-share
// isolation; remote stores ref-counted"), and invokes engine.CollectGarbage
// once per remote.
//
// Cross-share aggregation: each per-remote invocation receives a
// MultiShareReconciler scoped to the shares pointing at that remote,
// so the mark phase enumerates the union of every share's FileChunks.
// Without this, two shares sharing one remote would each delete the
// other's CAS objects.
//
// Arguments:
//   - sharePrefix: HISTORICAL — preserved in the signature for callers
//     that still pass it, but engine.Options.SharePrefix was removed
//     because the mark-sweep design has a global live set and per-share
//     scoping no longer makes sense. A non-empty value is logged at WARN
//     so operators see that the flag is no longer honored, then ignored.
//   - dryRun: if true, report orphans without deleting.
//
// Returns the summed *engine.GCStats across all per-remote invocations and any
// fatal error.
func (r *Runtime) RunBlockGC(ctx context.Context, sharePrefix string, dryRun bool) (*engine.GCStats, error) {
	// Steady-state GC: index-based remote sweep (synced − live), no S3 LIST.
	return r.runBlockGCSweep(ctx, sharePrefix, dryRun, false, nil)
}

// applyGCProgress wires an optional progress sink into engine.Options so a
// long-running run (mark phase on a snapshot-heavy deployment) reports liveness
// to an async caller. The mark phase reports the running hash count;
// ProgressCallback reports post-sweep totals. Both are best-effort liveness —
// the authoritative result is the returned (accumulated) GCStats. No-op when
// progress is nil (the scheduler / synchronous callers).
func applyGCProgress(opts *engine.Options, progress func(engine.GCStats)) {
	if progress == nil {
		return
	}
	opts.MarkProgress = func(hashesMarked int64) {
		progress(engine.GCStats{HashesMarked: hashesMarked})
	}
	opts.ProgressCallback = progress
}

// runBlockGCSweep is the shared remote+local GC pass. fullScan selects the
// full RemoteStore.Walk sweep (the reconcile drift + upgrade-migration
// backstop, which scans the real remote to catch objects the synced-hash index
// does not know about) instead of the steady-state index sweep (synced − live,
// no S3 LIST). Both paths clear synced markers on delete, preserving the
// synced ⊆ remote invariant (#1433).
func (r *Runtime) runBlockGCSweep(ctx context.Context, sharePrefix string, dryRun, fullScan bool, progress func(engine.GCStats)) (*engine.GCStats, error) {
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
	// Inline GC metrics: treat the whole multi-remote invocation as one pass.
	// `running` flips 1→0 across the loop (also surfaces a stuck/incomplete
	// pass); GCFinished records the accumulated totals + result + duration.
	gcStart := time.Now()
	r.metrics.GCStarted()
	defer func() {
		r.metrics.GCFinished(gcResult(total), total.ObjectsSwept, total.BytesFreed, time.Since(gcStart))
	}()
	for _, entry := range entries {
		opts := &engine.Options{
			DryRun: dryRun,
			// Thread the remote-store config UUID and the per-remote
			// share scope into engine.Options so the engine's own
			// start/complete log lines carry the correlation keys SREs
			// need for cross-checking against S3 access logs.
			RemoteEndpointID: entry.ConfigID,
			Shares:           append([]string(nil), entry.Shares...),
		}
		applyGCDefaults(opts, gcDefaults)
		applyGCProgress(opts, progress)
		// Inject held hashes from ready snapshots into the GC mark phase.
		opts.HoldProvider = r.snapshotHoldForRemote(entry.Shares)
		// The per-remote synced-hash index: the index-sweep candidate source AND
		// the marker-clear for swept hashes (so they re-upload if they reappear
		// in the live set) (#1433). Remote sweep only. A steady-state run sweeps
		// from it; the reconcile path (fullScan) keeps it only for the
		// marker-clear and Walks the namespace instead.
		opts.SyncedHashIndex = r.syncedHashStoreForShares(entry.Shares)
		opts.FullScan = fullScan
		logger.Info("RunBlockGC: starting",
			"fullScan", opts.FullScan,
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"gracePeriod", opts.GracePeriod,
			"dryRunSampleSize", opts.DryRunSampleSize)

		// Per-remote MultiShareReconciler: the mark phase enumerates
		// EnumerateFileChunks across every share pointing at this
		// remote so the live set is the union. Without this, each
		// share's GC would treat the other's CAS objects as orphans.
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
	// Sweep the local tier too, so one `gc` invocation reclaims orphaned
	// chunks on both remote and local stores (#1433).
	r.runLocalGC(ctx, "", dryRun, total, progress, nil)
	return total, nil
}

// collectGarbageLocalFn is a package-level indirection that lets tests
// intercept the engine.CollectGarbageLocal call. Production code always
// resolves to engine.CollectGarbageLocal.
var collectGarbageLocalFn = engine.CollectGarbageLocal

// RunBlockGCLocal sweeps orphaned chunks off every registered share's LOCAL
// block store. Local stores are isolated per share (architecture invariant
// #4), so each share is swept against its OWN live set: that share's
// EnumerateFileChunks plus its snapshot holds. Shares with an in-memory
// backend (no persistent gc-state root) are skipped — their chunks evaporate
// on restart, so on-disk reclamation is moot.
//
// This is the local-tier counterpart to RunBlockGC (which sweeps the shared
// remote tier). Together they reclaim deleted-file blocks on both tiers
// (#1433).
func (r *Runtime) RunBlockGCLocal(ctx context.Context, dryRun bool) (*engine.GCStats, error) {
	total := &engine.GCStats{}
	r.runLocalGC(ctx, "", dryRun, total, nil, nil)
	return total, nil
}

// runLocalGC sweeps each share's local block store, accumulating per-share
// stats into total. When shareFilter is non-empty only that share is swept;
// otherwise every share with a local store is swept. Shares with an in-memory
// backend (empty gc-state root) are skipped. Shared by RunBlockGC,
// RunBlockGCForShare, and RunBlockGCLocal so a single `gc` invocation reclaims
// orphans on BOTH tiers (#1433).
// gracePeriod, when non-nil, overrides the configured local-tier sweep grace
// for this run (including zero). nil leaves the configured/default grace, used
// by the server-wide and reconcile sweeps.
func (r *Runtime) runLocalGC(ctx context.Context, shareFilter string, dryRun bool, total *engine.GCStats, progress func(engine.GCStats), gracePeriod *time.Duration) {
	gcDefaults := r.gcDefaultsSnapshot()
	for _, entry := range r.sharesSvc.ShareLocalStores() {
		if shareFilter != "" && entry.ShareName != shareFilter {
			continue
		}
		if entry.GCStateRoot == "" {
			logger.Debug("local GC: skipping in-memory share (no persistent gc-state)",
				"share", entry.ShareName)
			continue
		}
		opts := &engine.Options{
			DryRun:      dryRun,
			GCStateRoot: entry.GCStateRoot,
			Shares:      []string{entry.ShareName},
		}
		applyGCDefaults(opts, gcDefaults)
		if gracePeriod != nil {
			// Operator override for this run only: authoritative grace,
			// including zero (no age guard).
			opts.GracePeriod = *gracePeriod
			opts.GracePeriodSet = true
		}
		applyGCProgress(opts, progress)
		opts.HoldProvider = r.snapshotHoldForShare(entry.ShareName)
		logger.Info("local GC: starting",
			"tier", "local",
			"share", entry.ShareName,
			"dryRun", dryRun,
			"gcStateRoot", entry.GCStateRoot,
			"gracePeriod", opts.GracePeriod,
		)

		rec := &singleShareReconciler{rt: r, shareName: entry.ShareName}
		stats := collectGarbageLocalFn(ctx, entry.Store, rec, opts)
		s := accumulateGCStats(total, stats, true)
		logger.Info("local GC: complete",
			"tier", "local",
			"share", entry.ShareName,
			"hashesMarked", s.HashesMarked,
			"objectsSwept", s.ObjectsSwept,
			"bytesFreed", s.BytesFreed,
			"errors", s.ErrorCount,
		)
	}
}

// singleShareReconciler scopes the GC mark phase to exactly one share, for the
// per-share local sweep. Implements engine.MultiShareReconciler.
type singleShareReconciler struct {
	rt        *Runtime
	shareName string
}

// SharesForGC implements engine.MultiShareReconciler.
func (s *singleShareReconciler) SharesForGC() []string { return []string{s.shareName} }

// GetMetadataStoreForShare implements engine.MultiShareReconciler.
func (s *singleShareReconciler) GetMetadataStoreForShare(shareName string) (metadata.Store, error) {
	return s.rt.GetMetadataStoreForShare(shareName)
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
func (p *perRemoteReconciler) GetMetadataStoreForShare(shareName string) (metadata.Store, error) {
	return p.rt.GetMetadataStoreForShare(shareName)
}

// RunBlockGCForShare looks up the named share, derives its persistent
// gc-state directory, and dispatches a GC run that scopes last-run.json
// persistence to that share. Behavior beyond persistence matches
// RunBlockGC: every distinct remote referenced by any registered share
// is scanned, and the cross-share live set is enforced — the {name}
// parameter only narrows where the run summary is written, not what is
// marked or swept.
//
// Returns an ErrShareNotFound-wrapped error if name is unknown.
func (r *Runtime) RunBlockGCForShare(ctx context.Context, name string, dryRun bool) (*engine.GCStats, error) {
	return r.runBlockGCForShare(ctx, name, dryRun, nil, nil)
}

// runBlockGCForShare is RunBlockGCForShare with an optional progress sink wired
// into the engine for async callers (StartBlockGC). progress is nil for the
// synchronous public entrypoint. gracePeriod, when non-nil, overrides the
// server-configured sweep grace for this run only — including zero, which reaps
// every eligible orphan with no age guard (dfsctl store block gc
// --grace-period).
func (r *Runtime) runBlockGCForShare(ctx context.Context, name string, dryRun bool, progress func(engine.GCStats), gracePeriod *time.Duration) (*engine.GCStats, error) {
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
	gcStart := time.Now()
	r.metrics.GCStarted()
	defer func() {
		r.metrics.GCFinished(gcResult(total), total.ObjectsSwept, total.BytesFreed, time.Since(gcStart))
	}()
	for _, entry := range entries {
		opts := &engine.Options{
			DryRun:      dryRun,
			GCStateRoot: gcRoot,
			// Surface per-remote correlation in the engine logs even on
			// the per-share path.
			RemoteEndpointID: entry.ConfigID,
			Shares:           append([]string(nil), entry.Shares...),
		}
		applyGCDefaults(opts, gcDefaults)
		if gracePeriod != nil {
			// Operator override for this run only: authoritative grace,
			// including zero (no age guard).
			opts.GracePeriod = *gracePeriod
			opts.GracePeriodSet = true
		}
		applyGCProgress(opts, progress)
		// Inject held hashes from ready snapshots into the GC mark phase.
		opts.HoldProvider = r.snapshotHoldForRemote(entry.Shares)
		// Per-remote synced-hash index: LIST-free candidate source + marker-clear
		// for swept hashes (#1433). The live set unions every share on this
		// remote (perRemoteReconciler below), so a single-share invocation never
		// treats a sibling share's live block as an orphan.
		opts.SyncedHashIndex = r.syncedHashStoreForShares(entry.Shares)
		// Per-share GC is steady-state (index sweep, no S3 LIST); FullScan stays
		// false (its zero value).
		logger.Info("RunBlockGCForShare: starting",
			"share", name,
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"gcStateRoot", gcRoot,
			"gracePeriod", opts.GracePeriod,
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
	// Sweep this share's local tier too (#1433).
	r.runLocalGC(ctx, name, dryRun, total, progress, gracePeriod)
	return total, nil
}

// GCStateDirForShare returns the per-share gc-state directory the GC
// engine writes `last-run.json` into. Empty when the share's local store
// has no persistent root (in-memory backend). Returns ErrShareNotFound
// when the share is unknown.
func (r *Runtime) GCStateDirForShare(name string) (string, error) {
	return r.sharesSvc.GetGCStateDirForShare(name)
}

// applyGCDefaults overlays operator-configured GC defaults onto opts.
// Without this, the gc.grace_period / gc.dry_run_sample_size knobs
// surfaced in pkg/config and validated at startup would be silently
// ignored — every CollectGarbage invocation would fall back to the
// engine's hardcoded defaults (1h, 1000).
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
	total.ObjectsScanned += s.ObjectsScanned
	total.ObjectsSwept += s.ObjectsSwept
	total.BytesFreed += s.BytesFreed
	total.ErrorCount += s.ErrorCount
	total.StrandedRowsReaped += s.StrandedRowsReaped
	// SharesScanned / BlocksScanned are deprecated REST wire fields that
	// are never populated by the mark-sweep engine — accumulation would
	// always be zero, so it is skipped here. See engine.GCStats godoc.
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

// multiSyncedHashStore unions the per-share synced-hash indexes on a shared
// remote into the single engine.SyncedHashIndex the GC sweep consumes:
// EnumerateSynced for the LIST-free candidate set and DeleteSynced to clear a
// swept hash everywhere it was recorded. It implements ONLY what GC needs — the
// per-hash IsSynced/MarkSynced live on the underlying metadata stores for the
// syncer and eviction paths and are deliberately not re-exposed here.
type multiSyncedHashStore []engine.SyncedHashIndex

var _ engine.SyncedHashIndex = multiSyncedHashStore(nil)

// EnumerateSynced fans enumeration across every share's index and unions the
// results. The same hash can be synced in several shares (CAS dedup) at
// different first-mirror times; it keeps the LATEST so the grace window covers
// the most recent upload — the most conservative choice (never delete a hash
// still within any share's grace). A real timestamp supersedes a legacy zero.
func (m multiSyncedHashStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	latest := make(map[block.ContentHash]time.Time)
	for _, s := range m {
		if err := s.EnumerateSynced(ctx, func(h block.ContentHash, syncedAt time.Time) error {
			if prev, ok := latest[h]; !ok || syncedAt.After(prev) {
				latest[h] = syncedAt
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for h, t := range latest {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(h, t); err != nil {
			return err
		}
	}
	return nil
}

func (m multiSyncedHashStore) DeleteSynced(ctx context.Context, h block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Attempt every store even if one fails, so a hash is un-synced everywhere it
	// can be, then surface the failures: the GC sweep records them as non-fatal
	// errors (a stale marker only costs a missed re-upload, recovered next pass).
	var errs []error
	for _, s := range m {
		if err := s.DeleteSynced(ctx, h); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncedHashStoreForShares unions the synced-hash indexes of the given shares
// into the engine.SyncedHashIndex a remote-tier GC sweep consumes: the LIST-free
// candidate source (EnumerateSynced) and the marker-clear for every hash it
// deletes (DeleteSynced) (#1433). Each share's metadata store provides both via
// its concrete EnumerateSynced/DeleteSynced. Returns nil when none are
// available, which forces the sweep onto the full-Walk path.
func (r *Runtime) syncedHashStoreForShares(shares []string) engine.SyncedHashIndex {
	var stores multiSyncedHashStore
	for _, shareName := range shares {
		mds, err := r.GetMetadataStoreForShare(shareName)
		if err != nil {
			// A share whose metadata store cannot be resolved drops out of the
			// index union: its hashes will not be sweep candidates and its
			// markers will not be cleared. Surface it so operators can explain a
			// degraded sweep rather than seeing a silent fall-through to Walk.
			logger.Warn("GC: synced index unavailable for share — excluded from sweep candidate set",
				"share", shareName, "err", err)
			continue
		}
		if shs, ok := mds.(engine.SyncedHashIndex); ok {
			stores = append(stores, shs)
		} else {
			logger.Warn("GC: metadata store does not implement EnumerateSynced — share excluded from index sweep, marker-clear disabled",
				"share", shareName)
		}
	}
	if len(stores) == 0 {
		return nil
	}
	return stores
}
