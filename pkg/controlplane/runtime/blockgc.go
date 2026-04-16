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
			BackupHold:  hold, // SAFETY-01 wiring (Plan 08 -> Plan 10)
		}
		logger.Info("RunBlockGC: starting",
			"configID", entry.ConfigID,
			"shares", entry.Shares,
			"dryRun", dryRun,
			"sharePrefix", sharePrefix)

		stats := collectGarbageFn(ctx, entry.Store, r, opts) // r satisfies MetadataReconciler
		if stats != nil {
			total.SharesScanned += stats.SharesScanned
			total.BlocksScanned += stats.BlocksScanned
			total.OrphanFiles += stats.OrphanFiles
			total.OrphanBlocks += stats.OrphanBlocks
			total.BytesReclaimed += stats.BytesReclaimed
			total.Errors += stats.Errors
		}
		logger.Info("RunBlockGC: complete",
			"configID", entry.ConfigID,
			"orphanFiles", statsOrZero(stats).OrphanFiles,
			"orphanBlocks", statsOrZero(stats).OrphanBlocks,
			"bytesReclaimed", statsOrZero(stats).BytesReclaimed,
			"errors", statsOrZero(stats).Errors)
	}
	return total, nil
}

// statsOrZero returns a zero-valued Stats when s is nil so log statements
// stay panic-safe without allocating a sentinel on every call.
func statsOrZero(s *gc.Stats) gc.Stats {
	if s == nil {
		return gc.Stats{}
	}
	return *s
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
