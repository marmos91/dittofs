// Package engine — Mark-Sweep CollectGarbage.
//
// Two phases
//
//  1. MARK: stream every live FileBlock's ContentHash into a disk-backed
//     live set (see GCState in gcstate.go). Memory is bounded regardless
//     of metadata size.
//  2. SWEEP: enumerate the unified CAS namespace via a single
//     RemoteStore.Walk call (the backend paginates internally). For each
//     object: parse the CAS key, skip foreign keys, apply the
//     snapshot - GracePeriod TTL filter, and DELETE iff absent from the
//     live set.
//
// Invariants
//   - (mark fail-closed): any error during EnumerateFileBlocks
//     aborts the sweep entirely — orphan-not-deleted is always preferred
//     over live-data-deleted.
//   - (sweep continue+capture): a Delete or list error in one prefix
//     worker is recorded in GCStats but does not abort the run.
//   - GC is opt-in: the operator triggers it on demand via dfsctl/REST.
//
// Cross-share aggregation lives in Runtime.RunBlockGC: it enumerates
// distinct remote stores and invokes CollectGarbage once per remote
// with a MultiShareReconciler that fans EnumerateFileBlocks across every
// share pointing at that remote.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"

	// justification: GC is the cross-share metadata-mark
	// entrypoint — it MUST bind metadata.Store /
	// MetadataReconciler to enumerate live FileBlocks.
	// Lifting these to blockstore would create a circular import.
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockSize is the size of a single block (8MB), used for byte estimation.
const BlockSize = block.BlockSize

// gcRootLocks serializes CollectGarbage invocations that share a
// GCStateRoot.: without this, two concurrent calls
// against the same root race in CleanStaleGCStateDirs — Run B can sweep
// Run A's open Badger directory while Run A is still writing to it
// silently truncating the live set and risking (mark fail-closed)
// violation by data path.
//
// Scope is per-process: a refcounted *gcRootLock keyed by absolute
// GCStateRoot path (the empty key "" maps to its own shared lock so
// temp-root callers still serialize against each other). Lock
// granularity is therefore PER-SHARE in practice — each share owns its
// own gc-state directory under <localStore>/gc-state/, so concurrent
// runs against DIFFERENT shares acquire DIFFERENT locks and proceed in
// parallel; only same-share GC calls serialize. For the Phase-11
// single-server deployment this is sufficient; cross-process safety
// (multi-server) requires an OS-level flock and is left as a TODO for
// the multi-process phase.
//
// Entries are reference-counted and deleted from the map when their
// refcount drops to zero, so the map shrinks back to size 0 whenever no
// GC run is in flight. Without this, a long-running server that creates
// and destroys shares with distinct gc-state paths would accumulate
// stale map entries indefinitely.
var (
	gcRootLocksMu sync.Mutex
	gcRootLocks   = make(map[string]*gcRootLock)
)

// gcRootLock is the map value: a serializing mutex plus a refcount
// guarded by gcRootLocksMu. The refcount tracks live acquireGCRootLock
// callers so releaseGCRootLock can drop the map entry when nobody else
// is holding or waiting on it.
type gcRootLock struct {
	mu       sync.Mutex
	refCount int // guarded by gcRootLocksMu
}

// acquireGCRootLock returns the per-root lock (creating it on first
// use) already locked. Callers MUST pair this with releaseGCRootLock so
// the refcount drops and the entry can be reclaimed when idle. The lock
// key is filepath.Clean'd so cosmetic differences ("/a/b" vs "/a/b/")
// map to the same lock.
func acquireGCRootLock(root string) *gcRootLock {
	key := root
	if key != "" {
		key = filepath.Clean(key)
	}
	gcRootLocksMu.Lock()
	entry, ok := gcRootLocks[key]
	if !ok {
		entry = &gcRootLock{}
		gcRootLocks[key] = entry
	}
	entry.refCount++
	gcRootLocksMu.Unlock()
	entry.mu.Lock()
	return entry
}

// releaseGCRootLock unlocks the per-root lock and drops the map entry
// when no other caller is holding or waiting on it. Pairs with
// acquireGCRootLock.
func releaseGCRootLock(root string, entry *gcRootLock) {
	entry.mu.Unlock()
	key := root
	if key != "" {
		key = filepath.Clean(key)
	}
	gcRootLocksMu.Lock()
	entry.refCount--
	if entry.refCount <= 0 {
		delete(gcRootLocks, key)
	}
	gcRootLocksMu.Unlock()
}

// GCStats holds statistics about the garbage collection run.
//
// The mark-sweep fields are authoritative; the legacy aggregator fields
// (OrphanFiles, OrphanBlocks, BytesReclaimed, Errors) are preserved for
// Runtime.RunBlockGC and the dfsctl gc-status surface and are populated
// by finalizeStats as aliases of the new ones.
type GCStats struct {
	RunID            string
	HashesMarked     int64
	ObjectsScanned   int64
	ObjectsSwept     int64
	BytesFreed       int64
	DurationMs       int64
	ErrorCount       int
	FirstErrors      []string
	DryRun           bool
	DryRunCandidates []string

	// StrandedRowsReaped counts file_blocks rows reaped by the reconcile pass
	// (rows whose owning inode was already gone — the pre-fix leak). Zero for a
	// plain GC run; only the reconcile sets it (#1433).
	StrandedRowsReaped int64 `json:"stranded_rows_reaped,omitempty"`

	// IsLocalTier is true when these stats come from a local-store pass
	// (CollectGarbageLocal), false for the default remote pass. Per-invocation
	// metadata only — accumulateGCStats does not fold it into a total; the
	// runtime uses it to tag metrics/logs with the tier.
	IsLocalTier bool

	// Legacy aggregator fields (compat aliases — see finalizeStats).

	// Deprecated: SharesScanned is retained on the REST/dfsctl wire for
	// compatibility with older clients but is never populated by the
	// mark-sweep engine and will always be zero. New consumers should
	// rely on HashesMarked / the per-share log lines instead.
	SharesScanned int
	// Deprecated: BlocksScanned is retained on the REST/dfsctl wire for
	// compatibility with older clients but is never populated by the
	// mark-sweep engine and will always be zero. New consumers should
	// rely on ObjectsSwept / HashesMarked instead.
	BlocksScanned  int
	OrphanFiles    int   // = ObjectsSwept.
	OrphanBlocks   int   // = ObjectsSwept.
	BytesReclaimed int64 // = BytesFreed.
	Errors         int   // = ErrorCount.
}

// Options configures the garbage collection behavior.
type Options struct {
	// DryRun if true, only reports orphans without deleting.
	DryRun bool

	// MaxOrphansPerShare is preserved for API compatibility but has no
	// effect in the mark-sweep algorithm — the sweep walks every prefix
	// to completion. A future plan can repurpose this if a per-prefix
	// cap becomes useful.
	MaxOrphansPerShare int

	// ProgressCallback is called periodically with progress updates.
	// May be nil.: invoked at most once per cas/XX/
	// prefix completion.
	ProgressCallback func(stats GCStats)

	// GracePeriod is the TTL applied during sweep: an object whose
	// LastModified is within snapshot - GracePeriod is preserved.
	// Zero defaults to one hour.
	GracePeriod time.Duration

	// DryRunSampleSize bounds the count of candidate keys captured in
	// GCStats.DryRunCandidates. Zero defaults to 1000.
	DryRunSampleSize int

	// GCStateRoot is the directory in which the per-run gc-state dir
	// (and last-run.json) are persisted. Empty means
	// "do not persist": GCState falls back to a temp dir under os.TempDir
	// and last-run.json is skipped.
	GCStateRoot string

	// RemoteEndpointID identifies the remote store this run targets
	// (typically the remote-store config UUID — for S3 a bucket/prefix
	// would also work).: included in the engine's
	// start/complete log lines so SREs can correlate engine GC activity
	// with S3 access logs without round-tripping through the runtime
	// caller. Empty when the caller does not provide one.
	RemoteEndpointID string

	// Shares is the list of share names this run scoped its mark phase
	// to (the MultiShareReconciler.SharesForGC return).
	// surfaces the share scope in the engine's log lines for cross-
	// correlation. Optional — engine logic does not depend on this
	// field; SharesForGC remains the source of truth for marking.
	Shares []string

	// HoldProvider injects held hashes into the GC mark phase. Optional —
	// nil means "no holds" and the live set is determined solely by
	// EnumerateFileBlocks. Errors from HeldHashes abort the entire run
	// (orphan-not-deleted is always preferred over live-data-deleted, per
	// the mark fail-closed invariant).
	HoldProvider HoldProvider

	// SyncedHashIndex, when non-nil, is the per-hash "is-on-remote" marker
	// index for the swept remote store. The sweep has DeleteSynced called for
	// every hash it deletes, keeping the index a strict subset of remote
	// contents: a hash swept off the remote is no longer synced and must be
	// re-uploaded if it reappears in the live set; without this, ListUnsynced
	// skips it forever and a later snapshot's durability verify fails (#1433).
	// Set ONLY for remote-tier sweeps — local eviction does not change remote
	// synced state, so local sweeps leave it nil.
	//
	// For a remote steady-state run (FullScan false) it is ALSO the candidate
	// source: the sweep enumerates (synced − live) from this index instead of
	// LISTing the remote (see sweepFromSyncedIndex).
	SyncedHashIndex SyncedHashIndex

	// FullScan forces the namespace Walk sweep (sweepByWalk) over the remote
	// store instead of the index-based sweep. It is the drift + upgrade-
	// migration backstop: only a full Walk finds remote objects the synced
	// index does not know about (markers written before timestamps existed, or
	// a Put-then-Mark crash-gap). Reconcile sets it; steady-state remote GC
	// leaves it false and sweeps from the index (no S3 LIST). Local-tier sweeps
	// always Walk their own (local-disk) namespace regardless of this flag.
	FullScan bool
}

// SyncedHashIndex is the narrow slice of the synced-hash marker store the GC
// sweep depends on: enumerate the synced set (the LIST-free orphan-candidate
// source) and clear a marker once its object is swept (preserving the
// synced ⊆ remote invariant). The full marker store (metadata.SyncedHashStore)
// and the runtime's per-remote union both satisfy it structurally; GC declares
// only what it uses so the broader IsSynced/MarkSynced surface stays out of
// this dependency.
type SyncedHashIndex interface {
	// EnumerateSynced calls fn once per synced marker with its content hash
	// and first-mirror timestamp. A zero syncedAt means the backend has no
	// recorded time (legacy marker) — the sweep treats it as fail-closed.
	EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, syncedAt time.Time) error) error

	// DeleteSynced removes the synced marker for a swept hash. Idempotent.
	DeleteSynced(ctx context.Context, hash block.ContentHash) error
}

// MetadataReconciler resolves per-share metadata stores. The mark phase
// calls EnumerateFileBlocks on each.
type MetadataReconciler interface {
	GetMetadataStoreForShare(shareName string) (metadata.Store, error)
}

// MultiShareReconciler enumerates every share pointing at a single remote
// store so the mark phase can union live FileBlocks across all of them
// . A reconciler that does not implement this is treated as having
// no shares — every CAS object becomes a sweep candidate.
type MultiShareReconciler interface {
	MetadataReconciler
	SharesForGC() []string
}

// HoldProvider injects "held" hashes into the GC mark phase so referenced
// blocks are never collected. The mark phase invokes HeldHashes AFTER
// per-share EnumerateFileBlocks and BEFORE FlushAdd, so held hashes land
// in the SAME live set used by the sweep's presence check. Any error
// from HeldHashes aborts the run via the mark fail-closed path.
//
// Scope is per-remote: remoteEndpointID and shares carry the run's
// remote-store identity and share scope so implementations can filter
// their held set accordingly.
type HoldProvider interface {
	HeldHashes(ctx context.Context, remoteEndpointID string, shares []string, fn func(block.ContentHash) error) error
}

// CollectGarbage scans the remote store and removes orphan blocks via the
// mark-sweep algorithm. Mark errors abort the sweep entirely
// (fail-closed); sweep errors are captured and the sweep continues.
// Returns a non-nil *GCStats with aggregate counts.
//
// reconciler MUST satisfy MultiShareReconciler when more than one share
// points at the remote. A reconciler that satisfies only the legacy
// MetadataReconciler is treated as "no shares" — the live set is empty
// and every CAS object is a sweep candidate. Callers in production
// (Runtime.RunBlockGC) supply a MultiShareReconciler.
// CollectGarbage reclaims orphaned objects on a remote (S3) store via the
// shared mark-sweep kernel (collectGarbage).
func CollectGarbage(
	ctx context.Context,
	remoteStore remote.RemoteStore,
	reconciler MetadataReconciler,
	options *Options,
) *GCStats {
	return collectGarbage(ctx, remoteStore, reconciler, options, false)
}

// CollectGarbageLocal reclaims orphaned chunks on a per-share LOCAL block
// store. Local stores are isolated per share (architecture invariant #4), so
// the caller scopes the reconciler and snapshot holds to that single share.
// The grace period protects freshly-written chunks whose FileBlock rows have
// not yet committed (#1433).
func CollectGarbageLocal(
	ctx context.Context,
	localStore block.Store,
	reconciler MetadataReconciler,
	options *Options,
) *GCStats {
	return collectGarbage(ctx, localStore, reconciler, options, true)
}

// collectGarbage is the shared mark-sweep kernel for both tiers. isLocal only
// tags the resulting stats; the live-set, grace, and fail-closed semantics are
// identical regardless of tier.
func collectGarbage(
	ctx context.Context,
	store sweepable,
	reconciler MetadataReconciler,
	options *Options,
	isLocal bool,
) *GCStats {
	if options == nil {
		options = &Options{}
	}
	started := time.Now()
	runID := started.UTC().Format("20060102T150405Z") + "-" + randSuffix(6)
	stats := &GCStats{RunID: runID, DryRun: options.DryRun, IsLocalTier: isLocal}

	// serialize concurrent CollectGarbage calls that
	// share a GCStateRoot. Without this, two parallel runs race in
	// CleanStaleGCStateDirs — one run can delete the other's open Badger
	// directory mid-mark, silently truncating the live set and risking
	// (mark fail-closed) violation by data path. Lock is acquired
	// before any disk-state work and released on return.
	rootLock := acquireGCRootLock(options.GCStateRoot)
	defer releaseGCRootLock(options.GCStateRoot, rootLock)

	// Apply defaults that the caller did not specify.
	gracePeriod := options.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = time.Hour
	}
	dryRunSample := options.DryRunSampleSize
	if dryRunSample <= 0 {
		dryRunSample = 1000
	}

	// Stale-dir cleanup before opening this run's GCState.
	if options.GCStateRoot != "" {
		if err := CleanStaleGCStateDirs(options.GCStateRoot); err != nil {
			slog.Warn("GC: stale dir cleanup failed", "err", err)
			// Not fatal; proceed.
		}
	}

	gcStateRoot := options.GCStateRoot
	if gcStateRoot == "" {
		// Caller did not configure persistent gc-state. Use a temp dir
		// scoped to this run; it is removed on completion.
		var err error
		gcStateRoot, err = makeTempGCStateRoot()
		if err != nil {
			recordGCError(stats, "gcstate temp root: "+err.Error())
			finalizeStats(stats, started)
			return stats
		}
		defer cleanupTempGCStateRoot(gcStateRoot)
	}

	gcs, err := NewGCState(gcStateRoot, runID)
	if err != nil {
		recordGCError(stats, "gcstate open: "+err.Error())
		finalizeStats(stats, started)
		_ = PersistLastRunSummary(options.GCStateRoot, gcRunSummaryFromStats(stats, started, time.Now()))
		return stats
	}
	defer func() { _ = gcs.Close() }()

	snapshotTime := time.Now()
	slog.Info("GC: mark phase starting",
		"run_id", runID,
		"snapshot_time", snapshotTime,
		"dry_run", options.DryRun,
		"grace_period", gracePeriod,
		"remote_endpoint_id", options.RemoteEndpointID,
		"shares", options.Shares,
		"hold_provider", options.HoldProvider != nil,
	)

	// MARK: stream every FileBlock's ContentHash into gcs across every share
	// the reconciler reports. Mark fail-closed.
	if err := markPhase(ctx, reconciler, gcs, stats, options.HoldProvider, options.RemoteEndpointID, options.Shares); err != nil {
		recordGCError(stats, "mark: "+err.Error())
		slog.Error("GC: mark failed — aborting sweep (fail-closed)",
			"run_id", runID, "err", err,
			"remote_endpoint_id", options.RemoteEndpointID,
			"shares", options.Shares)
		finalizeStats(stats, started)
		_ = PersistLastRunSummary(options.GCStateRoot, gcRunSummaryFromStats(stats, started, time.Now()))
		return stats
	}

	// SWEEP: a remote steady-state run derives orphan candidates from the
	// synced-hash index (synced − live), avoiding a full S3 LIST. Local-tier
	// sweeps and reconcile (FullScan) Walk the CAS namespace instead — for the
	// local tier that is a cheap local-disk walk; for reconcile it is the
	// drift + upgrade-migration backstop that finds remote objects the index
	// cannot (pre-timestamp markers, Put-then-Mark crash-gap).
	if !isLocal && !options.FullScan && options.SyncedHashIndex != nil {
		sweepFromSyncedIndex(ctx, store, gcs, stats, snapshotTime, gracePeriod, dryRunSample, options)
	} else {
		sweepByWalk(ctx, store, gcs, stats, snapshotTime, gracePeriod, dryRunSample, options)
	}

	// Mark complete + persist summary.
	if err := gcs.MarkComplete(); err != nil {
		slog.Warn("GC: mark complete failed", "err", err)
	}
	finalizeStats(stats, started)
	if err := PersistLastRunSummary(options.GCStateRoot, gcRunSummaryFromStats(stats, started, time.Now())); err != nil {
		slog.Warn("GC: persist last-run.json failed", "err", err)
	}
	slog.Info("GC: complete",
		"run_id", runID,
		"hashes_marked", stats.HashesMarked,
		"objects_scanned", stats.ObjectsScanned,
		"objects_swept", stats.ObjectsSwept,
		"bytes_freed", stats.BytesFreed,
		"duration_ms", stats.DurationMs,
		"error_count", stats.ErrorCount,
		"dry_run", options.DryRun,
		"remote_endpoint_id", options.RemoteEndpointID,
		"shares", options.Shares,
	)
	return stats
}

// markPhase iterates every share's FileBlockStore and calls
// EnumerateFileBlocks to populate the live set. The first error from any
// store aborts the entire mark phase (fail-closed).
//
// an empty share list is treated as a HARD ERROR. With no
// shares to enumerate, the engine cannot prove what is live and therefore
// MUST NOT sweep — orphan-not-deleted is always preferred over
// live-data-deleted. Callers that genuinely have no shares must
// short-circuit at a higher level (Runtime.RunBlockGC already does so
// when DistinctRemoteStores returns an empty slice).
func markPhase(ctx context.Context, reconciler MetadataReconciler, gcs *GCState, stats *GCStats, hold HoldProvider, remoteEndpointID string, shares []string) error {
	reconcilerShares := sharesForReconciler(reconciler)
	if len(reconcilerShares) == 0 {
		return fmt.Errorf("mark phase: reconciler reports zero shares — refusing to sweep CAS objects without a live set (fail-closed)")
	}

	addHash := func(h block.ContentHash) error {
		if h.IsZero() {
			// Legacy rows pre-CAS — not in the CAS keyspace.
			return nil
		}
		if err := gcs.Add(h); err != nil {
			return fmt.Errorf("gcstate add: %w", err)
		}
		stats.HashesMarked++
		return nil
	}

	for _, shareName := range reconcilerShares {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("mark phase ctx: %w", err)
		}
		store, err := reconciler.GetMetadataStoreForShare(shareName)
		if err != nil {
			return fmt.Errorf("get metadata store for share %q: %w", shareName, err)
		}
		if err := store.EnumerateFileBlocks(ctx, addHash); err != nil {
			return fmt.Errorf("enumerate share %q: %w", shareName, err)
		}
	}

	if hold != nil {
		if err := hold.HeldHashes(ctx, remoteEndpointID, shares, addHash); err != nil {
			return fmt.Errorf("hold provider: %w", err)
		}
	}

	// flush the batched Add()s so the sweep's Has() queries observe every
	// marked hash. Without this the final partial batch (< gcAddBatchSize
	// hashes from the last share) sits in memory and Has() returns false
	// for them — fail-closed violation by data path.
	if err := gcs.FlushAdd(); err != nil {
		return fmt.Errorf("flush gcstate batch: %w", err)
	}
	return nil
}

// sharesForReconciler extracts the share names to scan from the
// reconciler. A MultiShareReconciler returns its SharesForGC list; a
// legacy MetadataReconciler returns an empty slice (documented behavior).
func sharesForReconciler(r MetadataReconciler) []string {
	if r == nil {
		return nil
	}
	if multi, ok := r.(MultiShareReconciler); ok {
		return multi.SharesForGC()
	}
	return nil
}

// sweepByWalk walks the unified CAS namespace via a single
// remoteStore.Walk call. Per-object errors are captured in stats but
// do not abort the sweep. Foreign keys (non-CAS) are silently
// skipped (T-11-C-07). Objects within snapshot - GracePeriod are
// preserved.
//
// There is no caller-side concurrency knob: the 256-way prefix sharding
// has been collapsed onto RemoteStore.Walk, which paginates internally
// at the backend layer. A future re-sharding extension (per-prefix Walk
// fan-out) is expected to re-wire concurrency at the backend, not
// here. Callers wanting per-prefix parallelism should file against
// the backend.
// sweepable is the minimal store surface the sweep phase needs: enumerate the
// CAS namespace and delete one object by hash. Both remote.RemoteStore and the
// local block.Store satisfy it, so the same sweep kernel reclaims orphans on
// either tier (#1433).
//
// Walk MUST invoke its callback sequentially: sweepByWalk mutates unsynchronized
// per-run counters (scanned/swept/bytes) from inside the callback. Every
// current backend (local filepath.WalkDir, remote paginated list) honors this;
// a backend that parallelizes Walk must serialize the callback before
// satisfying this interface.
type sweepable interface {
	Walk(ctx context.Context, fn func(block.ContentHash, block.Meta) error) error
	Delete(ctx context.Context, hash block.ContentHash) error
}

// sweepByWalk reclaims orphans by enumerating the ENTIRE store namespace via
// store.Walk and deleting objects absent from the live set (past grace). For
// the remote tier this is an S3 LIST — used only by local-tier GC (a cheap
// local-disk walk) and by reconcile (FullScan), which needs it as the drift +
// upgrade-migration backstop. Steady-state remote GC uses sweepFromSyncedIndex.
func sweepByWalk(
	ctx context.Context,
	store sweepable,
	gcs *GCState,
	stats *GCStats,
	snapshotTime time.Time,
	gracePeriod time.Duration,
	dryRunSample int,
	options *Options,
) {
	var statsMu sync.Mutex
	addError := newSweepErrorRecorder(stats, &statsMu)

	// Post-Phase-17: the renamed RemoteStore.Walk replaces the per-XX
	// ListByPrefixWithMeta scan. Walk enumerates every CAS object in the
	// store in one call. The s3 backend Walk paginates internally; a
	// future re-sharding extension belongs at the backend Walk layer
	// (per-prefix fan-out), not here.
	runSweep := func() {
		// Count every object the backend reports — before any grace /
		// zero-LastModified / live-set filtering — so ObjectsScanned is the
		// total CAS objects present in the store ("blocks found"). Walk
		// invokes the callback sequentially, so a local counter folded into
		// stats once after the walk avoids a mutex round-trip per object.
		var scanned int64
		walkErr := store.Walk(ctx, func(h block.ContentHash, meta block.Meta) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			casKey := block.FormatCASKey(h)
			scanned++
			// — fail-closed on missing LastModified.
			// A zero LastModified means the backend did not report
			// per-object age; we cannot evaluate the snapshot - grace
			// TTL filter and we MUST NOT proceed to delete on
			// the live-set check alone. Preserve the object and capture
			// a diagnostic.
			if meta.LastModified.IsZero() {
				addError("zero LastModified " + casKey + ": backend Walk must populate LastModified for grace TTL evaluation")
				return nil
			}
			if meta.LastModified.After(snapshotTime.Add(-gracePeriod)) {
				return nil // within grace window
			}
			present, err := gcs.Has(h)
			if err != nil {
				addError("gcstate has " + casKey + ": " + err.Error())
				return nil
			}
			if present {
				return nil // live — keep
			}
			if options.DryRun {
				recordDryRunCandidate(stats, &statsMu, casKey, dryRunSample)
				return nil
			}
			if err := store.Delete(ctx, h); err != nil {
				// continue + capture
				addError("delete " + casKey + ": " + err.Error())
				return nil
			}
			// Keep the synced-hash index a strict subset of remote contents: the
			// hash is gone from this store, so it is no longer synced. Without
			// this, ListUnsynced skips it forever and a later snapshot's
			// durability verify fails on a block that will never re-upload
			// (#1433). Idempotent + non-fatal: a stale marker only costs a missed
			// re-upload, recoverable on the next pass. Set only for remote sweeps.
			if options.SyncedHashIndex != nil {
				if serr := options.SyncedHashIndex.DeleteSynced(ctx, h); serr != nil {
					addError("delete-synced " + casKey + ": " + serr.Error())
				}
			}
			statsMu.Lock()
			stats.ObjectsSwept++
			stats.BytesFreed += meta.Size
			statsMu.Unlock()
			return nil
		})
		if walkErr != nil {
			addError("walk: " + walkErr.Error())
		}
		statsMu.Lock()
		stats.ObjectsScanned += scanned
		statsMu.Unlock()
		if options.ProgressCallback != nil {
			statsMu.Lock()
			snap := *stats
			statsMu.Unlock()
			options.ProgressCallback(snap)
		}
	}

	runSweep()
}

// finalizeStats fills the legacy aggregator fields from the mark-sweep
// fields and stamps the run duration.
func finalizeStats(stats *GCStats, started time.Time) {
	stats.DurationMs = time.Since(started).Milliseconds()
	// Mirror new fields onto the legacy aggregator surface.
	stats.OrphanFiles = int(stats.ObjectsSwept)
	stats.OrphanBlocks = int(stats.ObjectsSwept)
	stats.BytesReclaimed = stats.BytesFreed
	stats.Errors = stats.ErrorCount
}

// recordGCError appends an error message to stats with the standard cap.
// Used by the engine-level pre-sweep paths (gcstate temp root, gcstate
// open, mark phase) where errors are bounded to a handful per run; the
// diversity-aware capture lives in newSweepErrorRecorder.
func recordGCError(stats *GCStats, msg string) {
	stats.ErrorCount++
	if len(stats.FirstErrors) < 16 {
		stats.FirstErrors = append(stats.FirstErrors, msg)
	}
}

// newSweepErrorRecorder returns the addError closure both sweep kernels
// (sweepByWalk, sweepFromSyncedIndex) use to record per-object errors under mu.
// It keeps FirstErrors heterogeneous: without diversification a burst of
// identical "list cas/aa: 503 SlowDown" errors fills the 16-slot cap and
// silently hides any 17th distinct error (e.g. an AccessDenied on Delete). We
// capture the FIRST occurrence of up to 16 distinct error classes so multi-mode
// failures are visible at a glance. ErrorCount still counts every error.
func newSweepErrorRecorder(stats *GCStats, mu *sync.Mutex) func(msg string) {
	seenClasses := make(map[string]struct{}, 16)
	return func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		stats.ErrorCount++
		cls := classifyGCError(msg)
		if _, ok := seenClasses[cls]; ok {
			return
		}
		if len(seenClasses) >= 16 {
			return
		}
		seenClasses[cls] = struct{}{}
		stats.FirstErrors = append(stats.FirstErrors, msg)
	}
}

// recordDryRunCandidate captures up to dryRunSample would-be-deleted CAS keys
// and counts the would-be deletion in ObjectsSwept, under mu. Shared by both
// sweep kernels so a DryRun pass reports identically regardless of sweep path.
func recordDryRunCandidate(stats *GCStats, mu *sync.Mutex, casKey string, dryRunSample int) {
	mu.Lock()
	defer mu.Unlock()
	if int64(len(stats.DryRunCandidates)) < int64(dryRunSample) {
		stats.DryRunCandidates = append(stats.DryRunCandidates, casKey)
	}
	stats.ObjectsSwept++
}

// classifyGCError extracts a short class key from a sweep-phase error
// string so the FirstErrors capture stays heterogeneous. The class is
// derived from the leading "<verb> <args>:"
// prefix the addError callers consistently produce — e.g.
// "list aa: 503 SlowDown" -> "list:503 SlowDown"
// "delete cas/bb/cc/...: AccessDenied" -> "delete:AccessDenied"
// "gcstate has cas/bb/...: io error" -> "gcstate has:io error"
//
// We strip the per-prefix/per-key tail (which is high-cardinality) and
// truncate the underlying error to its first ":" segment (S3 errors are
// typically "<status code> <code>: <message>"). 60 chars is enough to
// distinguish error families without keying on long stack traces.
func classifyGCError(msg string) string {
	verb, rest, ok := strings.Cut(msg, ": ")
	if !ok {
		if len(msg) > 60 {
			return msg[:60]
		}
		return msg
	}
	// Drop the high-cardinality argument from the verb prefix
	// ("delete cas/bb/cc/dd" -> "delete"). Keep multi-word verbs like
	// "gcstate has" intact by stripping only the trailing space-token.
	if i := strings.LastIndex(verb, " "); i > 0 {
		head := verb[:i]
		// Heuristic: if the trailing token starts with "cas/" or contains
		// "/", it is a path/prefix argument and gets dropped.
		tail := verb[i+1:]
		if strings.HasPrefix(tail, "cas/") || strings.Contains(tail, "/") {
			verb = head
		}
	}
	// Truncate the error body to its first ":" group.
	if i := strings.IndexByte(rest, ':'); i > 0 {
		rest = rest[:i]
	}
	cls := verb + ":" + rest
	if len(cls) > 60 {
		cls = cls[:60]
	}
	return cls
}

// gcRunSummaryFromStats projects a *GCStats into the GCRunSummary shape
// for last-run.json persistence.
//
// DryRunCandidates is forced to nil when DryRun=false.
// The current sweepByWalk only populates the slice on DryRun runs, but
// pinning the contract here means a future change that populates the
// slice on real runs (e.g. for "blocks deleted" tracing) cannot leak
// across the API boundary without an explicit decision.
func gcRunSummaryFromStats(stats *GCStats, started, completed time.Time) GCRunSummary {
	candidates := stats.DryRunCandidates
	if !stats.DryRun {
		candidates = nil
	}
	return GCRunSummary{
		RunID:            stats.RunID,
		StartedAt:        started,
		CompletedAt:      completed,
		HashesMarked:     stats.HashesMarked,
		ObjectsScanned:   stats.ObjectsScanned,
		ObjectsSwept:     stats.ObjectsSwept,
		BytesFreed:       stats.BytesFreed,
		DurationMs:       stats.DurationMs,
		ErrorCount:       stats.ErrorCount,
		FirstErrors:      stats.FirstErrors,
		DryRun:           stats.DryRun,
		DryRunCandidates: candidates,
	}
}

// randSuffix returns a hex-encoded random suffix of n bytes.
func randSuffix(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// Fall back to a timestamp-derived suffix; the runID need only
		// be unique per directory.
		return fmt.Sprintf("%x", time.Now().UnixNano())[:2*n]
	}
	return hex.EncodeToString(buf)
}

// makeTempGCStateRoot creates a temp directory for ephemeral gc-state
// when the operator did not configure a persistent root.
func makeTempGCStateRoot() (string, error) {
	return os.MkdirTemp("", "dittofs-gc-")
}

// cleanupTempGCStateRoot removes a temp gc-state root.
// log a WARN on failure so repeated cleanup leaks (permissions, mounted
// FS quirks) surface in monitoring instead of silently accumulating
// under os.TempDir().
func cleanupTempGCStateRoot(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("GC: temp gc-state cleanup failed", "dir", dir, "err", err)
	}
}
