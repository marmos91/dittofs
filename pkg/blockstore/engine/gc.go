// Package engine — Mark-Sweep CollectGarbage.
//
// Two phases:
//
//  1. MARK: stream every live FileBlock's ContentHash into a disk-backed
//     live set (see GCState in gcstate.go). Memory is bounded regardless
//     of metadata size.
//  2. SWEEP: enumerate the 256 cas/XX/* prefixes via a bounded worker
//     pool. For each object: parse the CAS key, skip foreign keys, apply
//     the snapshot - GracePeriod TTL filter, and DELETE iff absent from
//     the live set.
//
// Invariants:
//   - INV-04 (mark fail-closed): any error during EnumerateFileBlocks
//     aborts the sweep entirely — orphan-not-deleted is always preferred
//     over live-data-deleted.
//   - D-07 (sweep continue+capture): a Delete or list error in one prefix
//     worker is recorded in GCStats but does not abort the run.
//   - GC is opt-in: the operator enables it via gc.interval.
//
// Cross-share aggregation lives in Runtime.RunBlockGC: it enumerates
// distinct remote stores and invokes CollectGarbage once per remote,
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

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockSize is the size of a single block (8MB), used for byte estimation.
const BlockSize = blockstore.BlockSize

// casPrefix is the top-level CAS prefix on the remote store. Objects
// outside this prefix are not eligible for sweep.
const casPrefix = "cas/"

// gcRootLocks serializes CollectGarbage invocations that share a
// GCStateRoot. Phase 11 WR-3-01: without this, two concurrent calls
// against the same root race in CleanStaleGCStateDirs — Run B can sweep
// Run A's open Badger directory while Run A is still writing to it,
// silently truncating the live set and risking INV-04 (mark fail-closed)
// violation by data path.
//
// Scope is per-process: a sync.Mutex keyed by absolute GCStateRoot path.
// IN-4-05: lock granularity is therefore PER-SHARE in practice — each
// share owns its own gc-state directory under <localStore>/gc-state/, so
// concurrent runs against DIFFERENT shares acquire DIFFERENT mutexes and
// proceed in parallel; only same-share GC calls serialize. For the
// Phase-11 single-server deployment this is sufficient; cross-process
// safety (multi-server) requires an OS-level flock and is left as a TODO
// for the multi-process phase.
//
// The empty key ("") receives its own mutex so callers that pass no
// GCStateRoot (RunBlockGC's temp-root variant) still serialize against
// each other. This is conservative: each temp root is unique per call so
// races on the temp dir itself are impossible, but sharing one mutex
// across all temp-root runs prevents accidental concurrent CollectGarbage
// pile-ups against a single remote endpoint (see WR-3-01 sketch).
var (
	gcRootLocksMu sync.Mutex
	gcRootLocks   = make(map[string]*sync.Mutex)
)

// acquireGCRootLock returns the per-root mutex (creating it on first use)
// already locked. Callers MUST defer mu.Unlock(). The lock key is
// filepath.Clean'd so cosmetic differences ("/a/b" vs "/a/b/") map to the
// same mutex.
func acquireGCRootLock(root string) *sync.Mutex {
	key := root
	if key != "" {
		key = filepath.Clean(key)
	}
	gcRootLocksMu.Lock()
	mu, ok := gcRootLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		gcRootLocks[key] = mu
	}
	gcRootLocksMu.Unlock()
	mu.Lock()
	return mu
}

// GCStats holds statistics about the garbage collection run.
//
// The mark-sweep fields are authoritative; the legacy aggregator fields
// (SharesScanned, BlocksScanned, OrphanFiles, OrphanBlocks, BytesReclaimed,
// Errors) are preserved for Runtime.RunBlockGC and the dfsctl gc-status
// surface and are populated by finalizeStats as aliases of the new ones.
type GCStats struct {
	RunID            string
	HashesMarked     int64
	ObjectsSwept     int64
	BytesFreed       int64
	DurationMs       int64
	ErrorCount       int
	FirstErrors      []string
	DryRun           bool
	DryRunCandidates []string

	// Legacy aggregator fields (compat aliases — see finalizeStats).
	SharesScanned  int   // Always 0 in mark-sweep.
	BlocksScanned  int   // Always 0 in mark-sweep.
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
	// May be nil. Phase 11 Plan 06: invoked at most once per cas/XX/
	// prefix completion.
	ProgressCallback func(stats GCStats)

	// GracePeriod is the TTL applied during sweep: an object whose
	// LastModified is within snapshot - GracePeriod is preserved (D-05).
	// Zero defaults to one hour.
	GracePeriod time.Duration

	// SweepConcurrency bounds the worker pool walking the 256 cas/XX/*
	// prefixes (D-04). Zero defaults to 16; values above 32 are clamped.
	SweepConcurrency int

	// DryRunSampleSize bounds the count of candidate keys captured in
	// GCStats.DryRunCandidates. Zero defaults to 1000.
	DryRunSampleSize int

	// GCStateRoot is the directory in which the per-run gc-state dir
	// (and last-run.json) are persisted (D-01 / D-10). Empty means
	// "do not persist": GCState falls back to a temp dir under os.TempDir
	// and last-run.json is skipped.
	GCStateRoot string

	// RemoteEndpointID identifies the remote store this run targets
	// (typically the remote-store config UUID — for S3 a bucket/prefix
	// would also work). Phase 11 IN-3-04: included in the engine's
	// start/complete log lines so SREs can correlate engine GC activity
	// with S3 access logs without round-tripping through the runtime
	// caller. Empty when the caller does not provide one.
	RemoteEndpointID string

	// Shares is the list of share names this run scoped its mark phase
	// to (the MultiShareReconciler.SharesForGC return). Phase 11 IN-3-04:
	// surfaces the share scope in the engine's log lines for cross-
	// correlation. Optional — engine logic does not depend on this
	// field; SharesForGC remains the source of truth for marking.
	Shares []string
}

// MetadataReconciler resolves per-share metadata stores. The mark phase
// calls EnumerateFileBlocks on each.
type MetadataReconciler interface {
	GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error)
}

// MultiShareReconciler enumerates every share pointing at a single remote
// store so the mark phase can union live FileBlocks across all of them
// (D-03). A reconciler that does not implement this is treated as having
// no shares — every CAS object becomes a sweep candidate.
type MultiShareReconciler interface {
	MetadataReconciler
	SharesForGC() []string
}

// CollectGarbage scans the remote store and removes orphan blocks via the
// mark-sweep algorithm. Mark errors abort the sweep entirely (INV-04
// fail-closed); sweep errors are captured and the sweep continues
// (D-07). Returns a non-nil *GCStats with aggregate counts.
//
// reconciler MUST satisfy MultiShareReconciler when more than one share
// points at the remote. A reconciler that satisfies only the legacy
// MetadataReconciler is treated as "no shares" — the live set is empty
// and every CAS object is a sweep candidate. Callers in production
// (Runtime.RunBlockGC) supply a MultiShareReconciler.
func CollectGarbage(
	ctx context.Context,
	remoteStore remote.RemoteStore,
	reconciler MetadataReconciler,
	options *Options,
) *GCStats {
	if options == nil {
		options = &Options{}
	}
	started := time.Now()
	runID := started.UTC().Format("20060102T150405Z") + "-" + randSuffix(6)
	stats := &GCStats{RunID: runID, DryRun: options.DryRun}

	// Phase 11 WR-3-01: serialize concurrent CollectGarbage calls that
	// share a GCStateRoot. Without this, two parallel runs race in
	// CleanStaleGCStateDirs — one run can delete the other's open Badger
	// directory mid-mark, silently truncating the live set and risking
	// INV-04 (mark fail-closed) violation by data path. Lock is acquired
	// before any disk-state work and released on return.
	rootLock := acquireGCRootLock(options.GCStateRoot)
	defer rootLock.Unlock()

	// Apply defaults that the caller did not specify.
	gracePeriod := options.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = time.Hour
	}
	sweepConcurrency := options.SweepConcurrency
	if sweepConcurrency <= 0 {
		sweepConcurrency = 16
	}
	if sweepConcurrency > 32 {
		sweepConcurrency = 32
	}
	dryRunSample := options.DryRunSampleSize
	if dryRunSample <= 0 {
		dryRunSample = 1000
	}

	// Stale-dir cleanup before opening this run's GCState (D-01).
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
		"sweep_concurrency", sweepConcurrency,
		"remote_endpoint_id", options.RemoteEndpointID,
		"shares", options.Shares,
	)

	// MARK: stream every FileBlock's ContentHash into gcs across every share
	// the reconciler reports. Mark fail-closed per INV-04.
	if err := markPhase(ctx, reconciler, gcs, stats); err != nil {
		recordGCError(stats, "mark: "+err.Error())
		slog.Error("GC: mark failed — aborting sweep (fail-closed per INV-04)",
			"run_id", runID, "err", err,
			"remote_endpoint_id", options.RemoteEndpointID,
			"shares", options.Shares)
		finalizeStats(stats, started)
		_ = PersistLastRunSummary(options.GCStateRoot, gcRunSummaryFromStats(stats, started, time.Now()))
		return stats
	}

	// SWEEP: bounded worker pool over 256 cas/XX/ prefixes (D-04).
	sweepPhase(ctx, remoteStore, gcs, stats, snapshotTime, gracePeriod, sweepConcurrency, dryRunSample, options)

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
// store aborts the entire mark phase (INV-04 fail-closed).
//
// Phase 11 WR-02: an empty share list is treated as a HARD ERROR. With no
// shares to enumerate, the engine cannot prove what is live and therefore
// MUST NOT sweep — orphan-not-deleted is always preferred over
// live-data-deleted (INV-04). Callers that genuinely have no shares must
// short-circuit at a higher level (Runtime.RunBlockGC already does so
// when DistinctRemoteStores returns an empty slice).
func markPhase(ctx context.Context, reconciler MetadataReconciler, gcs *GCState, stats *GCStats) error {
	shares := sharesForReconciler(reconciler)
	if len(shares) == 0 {
		return fmt.Errorf("mark phase: reconciler reports zero shares — refusing to sweep CAS objects without a live set (INV-04 fail-closed)")
	}

	for _, shareName := range shares {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("mark phase ctx: %w", err)
		}
		store, err := reconciler.GetMetadataStoreForShare(shareName)
		if err != nil {
			return fmt.Errorf("get metadata store for share %q: %w", shareName, err)
		}
		if err := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
			if h.IsZero() {
				// Legacy rows pre-CAS — not in the CAS keyspace.
				return nil
			}
			if err := gcs.Add(h); err != nil {
				return fmt.Errorf("gcstate add: %w", err)
			}
			stats.HashesMarked++
			return nil
		}); err != nil {
			return fmt.Errorf("enumerate share %q: %w", shareName, err)
		}
	}
	// Phase 11 IN-4-04: flush the batched Add()s so the sweep's Has()
	// queries observe every marked hash. Without this the final partial
	// batch (< gcAddBatchSize hashes from the last share) sits in memory
	// and Has() returns false for them — INV-04 violation by data path.
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

// sweepPhase walks the 256 cas/XX/ prefixes via a bounded worker pool.
// Per-prefix errors are captured in stats but do not abort the sweep
// (D-07). Foreign keys (non-CAS) are silently skipped (T-11-C-07).
// Objects within snapshot - GracePeriod are preserved (D-05).
func sweepPhase(
	ctx context.Context,
	remoteStore remote.RemoteStore,
	gcs *GCState,
	stats *GCStats,
	snapshotTime time.Time,
	gracePeriod time.Duration,
	sweepConcurrency int,
	dryRunSample int,
	options *Options,
) {
	type prefixJob struct{ xx string }
	jobs := make(chan prefixJob, 256)
	var sweepWG sync.WaitGroup
	var statsMu sync.Mutex

	// Phase 11 IN-3-03: keep FirstErrors heterogeneous. Without
	// diversification a burst of identical "list cas/aa: 503 SlowDown"
	// errors fills the 16-slot cap and silently hides any 17th distinct
	// error (e.g. an AccessDenied on DeleteBlock). We capture the FIRST
	// occurrence of up to 16 distinct error classes so multi-mode
	// failures are visible at a glance.
	seenClasses := make(map[string]struct{}, 16)

	addError := func(msg string) {
		statsMu.Lock()
		defer statsMu.Unlock()
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

	sweepOne := func(j prefixJob) {
		listPrefix := casPrefix + j.xx + "/"
		objects, err := remoteStore.ListByPrefixWithMeta(ctx, listPrefix)
		if err != nil {
			addError("list " + j.xx + ": " + err.Error())
			return
		}
		for _, obj := range objects {
			if err := ctx.Err(); err != nil {
				return
			}
			h, err := blockstore.ParseCASKey(obj.Key)
			if err != nil {
				// Non-CAS key — silently skip (T-11-C-07).
				continue
			}
			// Phase 11 WR-4-02 — fail-closed on missing LastModified.
			// A zero LastModified means the backend did not report
			// per-object age; we cannot evaluate the snapshot - grace
			// TTL filter (D-05) and we MUST NOT proceed to delete on
			// the live-set check alone. Preserve the object and capture
			// a diagnostic; the operator must fix the backend's
			// ListByPrefixWithMeta to populate LastModified (see the
			// remote.ObjectInfo contract).
			if obj.LastModified.IsZero() {
				addError("zero LastModified " + obj.Key + ": backend ListByPrefixWithMeta must populate LastModified for grace TTL evaluation")
				continue
			}
			if obj.LastModified.After(snapshotTime.Add(-gracePeriod)) {
				continue // within grace window (D-05)
			}
			present, err := gcs.Has(h)
			if err != nil {
				addError("gcstate has " + obj.Key + ": " + err.Error())
				continue
			}
			if present {
				continue // live — keep
			}
			if options.DryRun {
				statsMu.Lock()
				if int64(len(stats.DryRunCandidates)) < int64(dryRunSample) {
					stats.DryRunCandidates = append(stats.DryRunCandidates, obj.Key)
				}
				stats.ObjectsSwept++ // count what would be deleted
				statsMu.Unlock()
				continue
			}
			if err := remoteStore.DeleteBlock(ctx, obj.Key); err != nil {
				// D-07: continue + capture
				addError("delete " + obj.Key + ": " + err.Error())
				continue
			}
			statsMu.Lock()
			stats.ObjectsSwept++
			stats.BytesFreed += obj.Size
			statsMu.Unlock()
		}
		if options.ProgressCallback != nil {
			statsMu.Lock()
			snap := *stats
			statsMu.Unlock()
			options.ProgressCallback(snap)
		}
	}

	for w := 0; w < sweepConcurrency; w++ {
		sweepWG.Add(1)
		go func() {
			defer sweepWG.Done()
			for j := range jobs {
				sweepOne(j)
			}
		}()
	}
	for xx := 0; xx < 256; xx++ {
		jobs <- prefixJob{xx: fmt.Sprintf("%02x", xx)}
	}
	close(jobs)
	sweepWG.Wait()
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
// diversity-aware capture lives in sweepPhase.addError.
func recordGCError(stats *GCStats, msg string) {
	stats.ErrorCount++
	if len(stats.FirstErrors) < 16 {
		stats.FirstErrors = append(stats.FirstErrors, msg)
	}
}

// classifyGCError extracts a short class key from a sweep-phase error
// string so the FirstErrors capture stays heterogeneous (Phase 11
// IN-3-03). The class is derived from the leading "<verb> <args>:"
// prefix the addError callers consistently produce — e.g.
// "list aa: 503 SlowDown"           -> "list:503 SlowDown"
// "delete cas/bb/cc/...: AccessDenied" -> "delete:AccessDenied"
// "gcstate has cas/bb/...: io error"  -> "gcstate has:io error"
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
// Phase 11 IN-01: DryRunCandidates is forced to nil when DryRun=false.
// The current sweepPhase only populates the slice on DryRun runs, but
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

// cleanupTempGCStateRoot removes a temp gc-state root. Phase 11 IN-03:
// log a WARN on failure so repeated cleanup leaks (permissions, mounted
// FS quirks) surface in monitoring instead of silently accumulating
// under os.TempDir().
func cleanupTempGCStateRoot(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("GC: temp gc-state cleanup failed", "dir", dir, "err", err)
	}
}

