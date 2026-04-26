---
phase: 11-cas-write-path-gc-rewrite-a2
reviewed: 2026-04-25T00:00:00Z
depth: deep
files_reviewed: 30
files_reviewed_list:
  - pkg/blockstore/types.go
  - pkg/blockstore/errors.go
  - pkg/blockstore/store.go
  - pkg/blockstore/engine/syncer.go
  - pkg/blockstore/engine/upload.go
  - pkg/blockstore/engine/fetch.go
  - pkg/blockstore/engine/gc.go
  - pkg/blockstore/engine/gcstate.go
  - pkg/blockstore/engine/cache.go
  - pkg/blockstore/engine/engine.go
  - pkg/blockstore/local/fs/flush.go
  - pkg/blockstore/local/fs/fs.go
  - pkg/blockstore/local/fs/eviction.go
  - pkg/blockstore/local/local.go
  - pkg/blockstore/remote/s3/store.go
  - pkg/blockstore/remote/s3/verifier.go
  - pkg/blockstore/remote/memory/store.go
  - pkg/blockstore/remote/remote.go
  - pkg/metadata/store/badger/objects.go
  - pkg/metadata/store/memory/objects.go
  - pkg/metadata/store/postgres/objects.go
  - pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql
  - pkg/controlplane/runtime/blockgc.go
  - pkg/controlplane/runtime/shares/service.go
  - pkg/config/config.go
  - internal/adapter/common/content_errmap.go
  - internal/controlplane/api/handlers/block_gc.go
  - cmd/dfsctl/commands/store/block/gc.go
  - cmd/dfsctl/commands/store/block/gc_status.go
  - pkg/apiclient/blockstore.go
findings:
  critical: 1
  warning: 5
  info: 4
  total: 10
status: issues_found
---

# Phase 11: Code Review Report

**Reviewed:** 2026-04-25
**Depth:** deep
**Files Reviewed:** 30 source files in the Phase 11 diff (`develop..HEAD`, 68 commits)
**Status:** issues_found

## Summary

Phase 11 (v0.15.0 A2) lands a comprehensive CAS-rewrite of the write path, a fail-closed mark-sweep GC, three-state lifecycle, dual-read shim, and BLAKE3-streaming verifier on read. The code is well-structured, the docstrings are excellent, and the invariants are visibly defended in most places. Most of the load-bearing decisions (D-11 PUT-first ordering, D-13 batched-claim serialization, D-18/19 two-stage verification, D-23 stage-and-release in flushBlock, INV-04 mark fail-closed in `markPhase`) are implemented correctly.

The review found one BLOCKER and five HIGH-impact correctness issues that should be addressed before the phase merges. The BLOCKER is a silent-error / data-loss path in the Postgres `EnumerateFileBlocks` (and `scanFileBlock`) that violates INV-04's fail-closed posture in a way that can lead to deletion of live CAS objects on a corrupt/short hash row. The HIGH issues cluster around (a) GC config knobs being defined but never wired into engine.Options, (b) a fail-OPEN edge in `markPhase` when `MultiShareReconciler.SharesForGC()` returns an empty slice while the remote has objects, (c) a refcount leak in the dedup short-circuit path, (d) the `SharePrefix` option being collected from callers and silently ignored by the sweep, and (e) a comment/contract drift on the documented "PutFileBlock per-row serializes claim" claim.

The remaining items are LOW-severity polish.

---

## Critical Issues

### CR-01: Postgres `scanFileBlock` / `EnumerateFileBlocks` silently swallow hash parse errors → data-loss in GC

**File:** `pkg/metadata/store/postgres/objects.go:285` and `pkg/metadata/store/postgres/objects.go:326`
**Issue:** Both code paths use `h, _ = metadata.ParseContentHash(hashStr.String)`, discarding the error. If the `hash` column ever holds a malformed value (truncated by a partial write, downgraded encoding, application bug, schema drift), `ParseContentHash` returns the zero `ContentHash` and the row is treated identically to a legacy pre-CAS row.

The downstream consequences are severe:

1. **GC mark phase data-loss path (INV-04 violation).** `engine.markPhase` (gc.go:267-271) explicitly skips `h.IsZero()` rows. A corrupt hash row therefore never enters the live set; the sweep then reaps the corresponding `cas/...` object as an orphan once the grace TTL lapses. **Live data is silently deleted.** This directly violates INV-04 (mark fail-closed: "any error during EnumerateFileBlocks aborts the sweep entirely — orphan-not-deleted is always preferred over live-data-deleted").

2. **Dual-read INV-06 violation.** `dispatchRemoteFetch` (fetch.go:72) routes by `fb.Hash.IsZero()`. A row whose hash failed to parse but whose `BlockStoreKey` is a CAS key gets routed to the *legacy* `m.remoteStore.ReadBlock` path with **no BLAKE3 verification**, contradicting INV-06 ("Every chunk downloaded from S3 is BLAKE3-verified before bytes reach the caller").

Compare with the badger backend (`pkg/metadata/store/badger/objects.go:455-462`) which `json.Unmarshal`s the row and properly returns the error from the iterator — fail-closed.

**Fix:** Surface the parse error. Two minimal changes are sufficient:

```go
// pkg/metadata/store/postgres/objects.go EnumerateFileBlocks (line ~285)
if hashStr.Valid {
    h, err = metadata.ParseContentHash(hashStr.String)
    if err != nil {
        return fmt.Errorf("enumerate file blocks: parse hash %q: %w",
            hashStr.String, err)
    }
}
```

```go
// pkg/metadata/store/postgres/objects.go scanFileBlock (line ~325)
if hashStr.Valid {
    h, err := metadata.ParseContentHash(hashStr.String)
    if err != nil {
        return nil, fmt.Errorf("scan file block %s: parse hash %q: %w",
            block.ID, hashStr.String, err)
    }
    block.Hash = h
}
```

The conformance suite under `pkg/metadata/storetest/file_block_ops.go` should also gain a deliberate "corrupt hash → enumeration returns error" scenario so future backends cannot regress this. (See also CR-01-followup in the conformance test list — INV-04 is currently not exercised against the Postgres backend's hash-corruption path.)

---

## Warnings

### WR-01: GC config knobs (`gc.grace_period`, `gc.sweep_concurrency`, `gc.dry_run_sample_size`) are defined and validated but never reach `engine.Options`

**File:** `pkg/controlplane/runtime/blockgc.go:43-48` and `pkg/controlplane/runtime/blockgc.go:113-118`
**Issue:** `pkg/config/config.go:148-200` defines `GCConfig` with `Interval`, `SweepConcurrency`, `GracePeriod`, `DryRunSampleSize`, applies defaults, and validates ranges. The handler (`internal/controlplane/api/handlers/block_gc.go`) and CLI (`cmd/dfsctl/commands/store/block/gc.go`) both pass `dryRun` through the runtime. But `RunBlockGC` and `RunBlockGCForShare` only ever populate `engine.Options{SharePrefix, DryRun}` (and `GCStateRoot` for the per-share variant). The grace period, sweep concurrency, and dry-run sample size that the operator configured are **silently ignored** — the engine always falls back to its hardcoded defaults (1h, 16, 1000).

Documentation (docs/CONFIGURATION.md, docs/CLI.md) advertises these knobs as functional. Operators who set `gc.grace_period: 24h` to align with their backup window today get the engine default of 1h.

**Fix:** Pass GCConfig from runtime through to `engine.Options`. The Runtime already has a config handle; thread it (or pass the relevant `GCConfig` snapshot) into both entry points:

```go
// pkg/controlplane/runtime/blockgc.go
opts := &engine.Options{
    SharePrefix:      sharePrefix,
    DryRun:           dryRun,
    GracePeriod:      r.cfg.GC.GracePeriod,
    SweepConcurrency: r.cfg.GC.SweepConcurrency,
    DryRunSampleSize: r.cfg.GC.DryRunSampleSize,
    GCStateRoot:      gcRoot, // already set in RunBlockGCForShare
}
```

Also wire `gc.interval` to a periodic ticker in the runtime startup path — the doc claims it triggers periodic runs but no ticker exists in the diff.

### WR-02: `markPhase` fail-OPEN when `SharesForGC()` returns an empty slice while the remote has live objects

**File:** `pkg/blockstore/engine/gc.go:249-257`
**Issue:** When `sharesForReconciler(reconciler)` returns an empty slice, `markPhase` returns `nil` (i.e., success with an empty live set). The sweep then proceeds and treats every `cas/...` object as an orphan candidate — the only thing standing between live data and deletion is the grace TTL.

Today `Runtime.RunBlockGC` defends against this at a higher level (it returns early when `entries` is empty), but the engine API itself is fail-OPEN. Any future caller (a unit test fixture, a third-party reconciler, a maintenance script that constructs the reconciler manually, a caller that builds a `MultiShareReconciler` with an empty list during a transient edge case) will silently nuke every CAS object outside the grace window.

The current `gc.go` comment ("No shares means the live set is empty — every CAS object on the remote is a sweep candidate") describes this as documented behavior, but it directly contradicts INV-04's posture ("orphan-not-deleted is always preferred over live-data-deleted"). The fail-CLOSED interpretation is "if I have no shares to enumerate, I cannot prove what is live, therefore I do not sweep."

**Fix:** Treat `len(shares) == 0` as a hard error in `markPhase`:

```go
func markPhase(ctx context.Context, reconciler MetadataReconciler, gcs *GCState, stats *GCStats) error {
    shares := sharesForReconciler(reconciler)
    if len(shares) == 0 {
        return fmt.Errorf("mark phase: reconciler reports zero shares — refusing to sweep CAS objects without a live set (INV-04 fail-closed)")
    }
    // ... existing loop
}
```

The existing Runtime guard becomes redundant but remains a useful early-return optimization.

### WR-03: Dedup short-circuit in `uploadOne` permanently increments RefCount on the donor block, leaking a refcount that no decrement path ever reverses

**File:** `pkg/blockstore/engine/upload.go:93-104`
**Issue:** When a Pending block hashes to an already-Remote `existing` block, the syncer:

1. Calls `IncrementRefCount(existing.ID)` to bump the donor's refcount.
2. Persists `fb` with the donor's `BlockStoreKey` and `State=Remote`.

When the file owning `fb` is later deleted, `DeleteWithRefCount` decrements `fb.ID`'s refcount — not `existing.ID`'s. The donor's incremented refcount is **never reversed**. Over a long-running deployment with heavy dedup hit rates, `existing.RefCount` monotonically grows, masking the GC-reclaim signal that flows through `ListUnreferenced`.

The mark-sweep GC saves the day in practice (it walks `FileAttr.Blocks[*].Hash`, not refcounts), so the CAS object is still reapable. But:

- `ListUnreferenced` now returns false negatives, breaking any operator-facing introspection that relies on it.
- The dedup hot-path becomes a slow leak in metadata size if the project ever wires refcount-driven deletes back in.
- The error from `IncrementRefCount` is silently dropped (`_ =`), so a transient metadata error has no observable effect.

**Fix:** Either record the refcount-increment against the new file's block (so it gets balanced on delete), or — better, since two block IDs now point at one CAS key — point `fb` at `existing.ID` so all future operations target the single canonical row, and skip the `PutFileBlock` for `fb` entirely. The latter requires the engine to also record the ID-mapping somewhere the metadata-side dual lookup honors. The narrowest fix here is to drop the dedup short-circuit entirely (the CAS PUT is idempotent — re-uploading the same bytes to the same key is cheap), and let the rare collision pay the duplicate-PUT cost; the mark-sweep GC already handles the cleanup.

If you keep the short-circuit, at minimum log the IncrementRefCount error rather than discarding it.

### WR-04: `engine.Options.SharePrefix` is collected by every caller and silently ignored by the sweep

**File:** `pkg/blockstore/engine/gc.go:77-82` and `pkg/controlplane/runtime/blockgc.go:46`
**Issue:** `Options.SharePrefix` is documented as a forward filter on `remote.ListByPrefixWithMeta`. The runtime collects it, the doc/CLI surface it. But `sweepPhase` (gc.go:301-395) hardcodes `listPrefix := casPrefix + j.xx + "/"` for all 256 prefixes — `SharePrefix` is never consulted. Operators who pass `--share-prefix=foo/` expecting the GC to scope to that subset get a full-bucket scan.

**Fix:** Either honor `SharePrefix` (compose it with the cas/XX/ prefix during the sweep, skipping prefixes that cannot match), or remove the field from the public Options and from the runtime/CLI surface so the contract matches the implementation. The latter is simpler given the mark-sweep design — per-share scoping no longer makes sense once the live set is global.

### WR-05: Single-process intra-tick double-claim is impossible by `m.uploading`, but the doc-comment overstates cross-process serialization

**File:** `pkg/blockstore/engine/syncer.go:489-519`
**Issue:** The doc-comment on `claimBatch` reads: "the metadata writes are the serialization point against duplicate uploads — two concurrent claimBatch callers cannot both observe + claim the same row because PutFileBlock is applied per row before the next iteration sees it." This is true within one process (the per-row write happens between the list and the next iteration), but it does NOT serialize across processes: two syncer instances on two nodes can each `ListLocalBlocks` the same Pending row before either calls PutFileBlock, both flip it to Syncing, both upload (idempotent because CAS-keyed but wasteful), and the second `PutFileBlock` simply overwrites the first.

The syncer design is correct (CAS makes the duplicate work harmless), but the comment misleads future readers into believing the metadata-store layer provides a stronger guarantee than it does. This matters because META-01 in Phase 12 may consume this contract.

**Fix:** Reword the doc to make explicit that the serialization is single-process only and that cross-process duplicate claims are tolerated by CAS idempotency:

```go
// Within one syncer instance, PutFileBlock applied per row prevents the
// next iteration from re-claiming the same row. Across syncer instances
// (multi-process / multi-node), two concurrent claimBatch callers MAY
// both observe a Pending row and both flip it to Syncing — this is
// tolerated because CAS keys are content-defined and the resulting
// duplicate PUT is byte-identical to the same key (D-11 / INV-03).
```

Optional follow-up: add `WHERE state = 0` to the Postgres update and use `RETURNING` to make the per-row claim atomic — would close the cross-process window without coordination.

---

## Info

### IN-01: `gcRunSummaryFromStats` does not zero-out `DryRunCandidates` when `DryRun=false`

**File:** `pkg/blockstore/engine/gc.go:418-432`
**Issue:** The summary projection always includes `DryRunCandidates` — but those slices are only populated when `DryRun=true` (gc.go:354-360). Today they're always empty for non-dry-run runs so `omitempty` strips them in JSON. Defensive note: if a future change populates the field on real runs (e.g., for "blocks deleted" tracing), the API contract for `GCRunSummary` would silently change.

**Fix:** Either nil-out `DryRunCandidates` explicitly in the non-dry-run case, or document that the field's meaning depends on `DryRun`.

### IN-02: `recoverStaleSyncing` swallows individual `PutFileBlock` errors at WARN

**File:** `pkg/blockstore/engine/syncer.go:553-559`
**Issue:** When the janitor fails to requeue a single stale Syncing row, it logs WARN and `continue`s, returning `nil` overall even if every row failed. The caller (`Start`) treats this as success and proceeds to start the periodic uploader. A fully-broken metadata path therefore looks like a healthy syncer — the only signal is the WARN log.

**Fix:** Track failures and return a joined error (or at least a count) so the operator can detect a broken janitor at startup. Alternatively, keep the current behavior but elevate the per-row log to ERROR so it stands out in monitoring.

### IN-03: `cleanupTempGCStateRoot` is fire-and-forget — a failure leaks the temp directory silently

**File:** `pkg/blockstore/engine/gc.go:451-454`
**Issue:** The defer runs `os.RemoveAll(dir)` and discards the error. On a long-running server, repeated GC runs that fail to clean up (permissions issue, mounted FS quirk) will silently accumulate temp dirs under `os.TempDir()`.

**Fix:** Log a WARN if RemoveAll fails. Cheap and informative.

### IN-04: `SharePrefix` field on `engine.Options` is unused but still in the public Options struct

**File:** `pkg/blockstore/engine/gc.go:77-82`
**Issue:** Related to WR-04. If the field is going to stay (documented as "forward filter") it should actually be applied; if not, removing it now (Phase 11 is the introducing phase) keeps the public API honest and avoids future confusion.

**Fix:** Remove the field, or honor it in sweepPhase. See WR-04.

---

_Reviewed: 2026-04-25_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: deep (cross-file: engine ↔ runtime ↔ metadata ↔ adapter)_
