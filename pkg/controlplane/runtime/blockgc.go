package runtime

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/gc"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// RunBlockGC is the Phase-5 production entrypoint for block-store garbage
// collection. It enumerates every share with a remote block store,
// deduplicates distinct underlying remote stores (ref-counted sharing
// across shares is possible — see docs/ARCHITECTURE.md "per-share
// isolation; remote stores ref-counted"), and invokes gc.CollectGarbage
// once per remote with gc.Options.BackupHold attached.
//
// SAFETY-01 invariant: every production GC run MUST consult the backup
// hold set. Without a hold, block-GC run between backup day and restore
// day can reclaim blocks the backup manifest will later need, silently
// destroying DR capability. RunBlockGC refuses to execute (returns an
// error, no GC invocation) when the hold wiring is unavailable — this
// is the machine-enforcement point for the invariant.
//
// Scope boundary (Phase 5 vs Phase 6): this method is the runtime
// callable path. The operator-facing CLI/REST trigger (e.g.
// POST /api/blockgc/run, `dfsctl blockgc run`) is deferred to Phase 6
// per CONTEXT.md "Out of scope". Phase 6 will add a thin wrapper that
// calls this method; any future scheduler will also go through here.
//
// Arguments:
//   - sharePrefix: restrict block enumeration to keys with this prefix
//     (empty = scan every block in the remote).
//   - dryRun: if true, report orphans without deleting.
//
// Returns the summed *gc.Stats across all per-remote invocations and any
// fatal error (e.g. backup-hold wiring unavailable).
func (r *Runtime) RunBlockGC(ctx context.Context, sharePrefix string, dryRun bool) (*gc.Stats, error) {
	// SAFETY-01 gate: BackupHold is NON-OPTIONAL for production GC. Refuse
	// rather than silently under-hold (which would risk reclaiming blocks a
	// retained backup manifest still references).
	backupStore := r.BackupStore()
	destFactory := r.DestFactoryFn()
	if backupStore == nil || destFactory == nil {
		return nil, fmt.Errorf(
			"RunBlockGC refused: backup-hold wiring unavailable " +
				"(runtime missing BackupStore or destFactory — SAFETY-01)")
	}
	hold := storebackups.NewBackupHold(backupStore, destFactory)

	// SAFETY-01 eager resolution: query the hold up front and fail hard on
	// error rather than letting gc.CollectGarbage fall through to a soft
	// under-hold. A transient DB/destination error here must not permit a
	// hold-less GC run that could reclaim blocks referenced by retained
	// backup manifests. The resolved set is injected via a small adapter
	// so CollectGarbage never re-queries the provider.
	held, err := hold.HeldPayloadIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"RunBlockGC refused: backup-hold query failed (SAFETY-01): %w", err)
	}
	resolvedHold := gc.StaticBackupHold(held)

	// Enumerate distinct underlying remote stores. Dedup is by configID (not
	// by the per-share nonClosingRemote wrapper pointer), so two shares that
	// reference the same remote-store config produce one GC invocation.
	entries := r.sharesSvc.DistinctRemoteStores()
	if len(entries) == 0 {
		logger.Info("RunBlockGC: no remote-backed shares registered; nothing to scan",
			"dryRun", dryRun, "sharePrefix", sharePrefix)
		return &gc.Stats{}, nil
	}

	total := &gc.Stats{}
	for _, entry := range entries {
		opts := &gc.Options{
			SharePrefix: sharePrefix,
			DryRun:      dryRun,
			BackupHold:  resolvedHold, // SAFETY-01: eager-resolved, static
		}
		logger.Info("RunBlockGC: starting",
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"sharePrefix", sharePrefix)

		// r satisfies MetadataReconciler. stats is nil-safe — CollectGarbage
		// always returns a non-nil *gc.Stats (see pkg/blockstore/gc/gc.go),
		// but we defensively read through a zero-valued copy so a future
		// nil-returning edge case never panics in the log statement.
		stats := collectGarbageFn(ctx, entry.Store, r, opts)
		var s gc.Stats
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
// the gc.CollectGarbage call and assert Options.BackupHold was attached on
// every production GC invocation. Production code always resolves to
// gc.CollectGarbage.
var collectGarbageFn = gc.CollectGarbage

// SetBackupHoldWiringForTest installs a BackupStore + destFactory on the
// runtime directly, bypassing the full storebackups.Service construction
// path. Test-only helper: lets runtime-package tests exercise RunBlockGC's
// SAFETY-01 gate without standing up a real backup scheduler, executor,
// or destination registry.
//
// Panics if called outside of test contexts is not enforced — the helper is
// unexported-style (documented test-only) and only reachable from the
// runtime package.
func (r *Runtime) SetBackupHoldWiringForTest(bs store.BackupStore, destFactory storebackups.DestinationFactoryFn) {
	// Construct a lightweight storebackups.Service shim with only the fields
	// BackupStore() / DestFactory() read. Using the real constructor would
	// pull in the scheduler, executor, and retention path, none of which the
	// test exercises.
	r.storeBackupsSvc = storebackups.New(
		bs, nil, 0,
		storebackups.WithDestinationFactory(destFactory),
	)
}

// setShareRemoteForTest injects a remote store for an already-registered
// share. Delegates to shares.Service.SetShareRemoteForTest. Test-only.
func (r *Runtime) setShareRemoteForTest(shareName string, rs remote.RemoteStore) {
	r.sharesSvc.SetShareRemoteForTest(shareName, rs)
}
