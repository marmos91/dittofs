---
phase: 11-cas-write-path-gc-rewrite-a2
reviewed: 2026-04-25T00:00:00Z
depth: deep
pass: 3
files_reviewed: 18
files_reviewed_list:
  - pkg/blockstore/engine/gc.go
  - pkg/blockstore/engine/gcstate.go
  - pkg/blockstore/engine/upload.go
  - pkg/blockstore/engine/syncer.go
  - pkg/blockstore/engine/fetch.go
  - pkg/blockstore/local/fs/rollup.go
  - pkg/blockstore/local/fs/write.go
  - pkg/blockstore/remote/s3/store.go
  - pkg/blockstore/remote/s3/verifier.go
  - pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql
  - pkg/metadata/store/postgres/objects.go
  - pkg/metadata/store/memory/objects.go
  - pkg/metadata/store/badger/objects.go
  - pkg/controlplane/runtime/runtime.go
  - pkg/controlplane/runtime/blockgc.go
  - pkg/config/config.go
  - cmd/dfs/commands/start.go
  - internal/adapter/nfs/v3/handlers/commit.go
  - internal/adapter/common/write_payload.go
  - internal/adapter/smb/v2/handlers/durable_scavenger.go
  - docs/ARCHITECTURE.md
  - docs/CONFIGURATION.md
  - docs/FAQ.md
findings:
  critical: 0
  warning: 2
  info: 5
  total: 7
status: issues_found
---

# Phase 11: Code Review Report (Pass 3)

**Reviewed:** 2026-04-25
**Depth:** deep — cross-protocol (NFS COMMIT ↔ engine.Flush, SMB durable ↔ syncer); GC concurrency; backend skew; doc/code drift
**Files Reviewed:** ~23 source + 3 docs traced; cross-checked against passes 1–2 fix commits
**Pass-2 fixes confirmed:** every CR-2/WR-2/IN-2 item lands cleanly; pass-3 starts from a clean baseline.

## Summary

Pass 3 widened the lens to areas passes 1–2 didn't exhaustively cover: cross-protocol semantics (NFS3/4 COMMIT, SMB durable handles), the chunker's behavior under sparse writes, GC concurrency with parallel callers, observability gaps, and documentation drift after the 30-commit fix sequence on this branch.

**Headline findings:**

- **No new BLOCKERs.** The Phase 11 surface is in good shape after passes 1–2.
- **WR-3-01 (HIGH):** Two parallel `CollectGarbage` calls against the same `GCStateRoot` will race in `CleanStaleGCStateDirs` and one run can delete the other's open Badger directory mid-mark. There is no mutex serializing GC at any layer. Reachable today via two parallel `dfsctl store block gc` invocations, or via REST + CLI overlap.
- **WR-3-02 (MEDIUM-HIGH, doc-drift):** `gc.interval` is parsed, validated, and documented in three places (CONFIGURATION.md, ARCHITECTURE.md, FAQ.md) as the periodic-GC trigger — but **no code references the field after `ApplyDefaults`**. Operators who set `gc.interval: 24h` get exactly nothing. This is the un-implemented half of REVIEW-1 WR-01 ("wire `gc.interval` to a periodic ticker"); the doc was never reverted.

Five INFO items cover narrower drift / observability gaps:

- IN-3-01: `gc.grace_period < 5m` doc says "logged as a warning" but pass-2's IN-2-03 fix makes it a hard error — CONFIGURATION.md is now wrong.
- IN-3-02: Postgres backend's `idx_file_blocks_hash` UNIQUE partial index will reject the dedup short-circuit's `PutFileBlock` for any cross-file hash collision (e.g. all-zero blocks across two VM image files). Memory + Badger silently overwrite their hash index. Pre-existing bug not introduced by Phase 11, but the conformance suite still has no cross-backend test for "two distinct block IDs share a hash" — meaning Postgres dedup is unverified.
- IN-3-03: GC error sample (`stats.FirstErrors`, capped at 16) collects the FIRST 16 errors. A burst of 100 identical S3 errors hides any 17th distinct error. A "diverse sample" (first error per error-class) would surface multi-mode failures.
- IN-3-04: GC's `slog.Info("GC: mark phase starting", ...)` doesn't include share names or the remote endpoint identity. Per-share runs are correlatable from the runtime's pre-call log, but the engine's own log can't be cross-referenced with S3 access logs without it.
- IN-3-05: `dispatchRemoteFetch` returns `nil, nil` (silent zeros) when a CAS-format key returns `ErrBlockNotFound`. This is correct for sparse blocks, but identical behavior masks data loss if a CAS object ever gets reaped while still referenced (the very scenario INV-04 fail-closed is supposed to prevent). A "CAS key absent → ERROR" mode would surface the bug class rather than silently returning zeros.

**Eight items pass 3 considered and DISCARDED (no actionable bug):**

- **NFS COMMIT semantics:** `commit.go` → `common.CommitBlockStore` → `engine.Flush` → `syncer.Flush` → `local.Flush` only. No remote sync, no metadata mutation. Per CONTEXT D-24 ("COMMIT must be a strong durability point — to local store, not necessarily remote"), this is correct. The doc-comment on syncer.Flush (line 180-185) is explicit and well-aligned.
- **SMB durable handles + syncer:** Reads after `ProcessDurableReconnectContext` go through the engine and check `IsBlockLocal` first. Pending and Syncing blocks both keep `LocalPath` set, so the local read path serves correct bytes regardless of upload state. The `DurableHandleScavenger.cleanupAndDelete` Flush call (durable_scavenger.go:152) is bounded by the durable timeout and tolerates engine errors — no concurrency hole found.
- **Sparse writes / zero-block dedup:** Chunker is content-addressed by BLAKE3, so all-zero regions across files DO share CAS keys (rollup.go:325 + FIX-3 anchor-at-byte-zero comment). This is by design (D-21 dedup goal) and is the dominant source of dedup wins on VM workloads. Memory cost: 32 bytes per chunk, regardless of how many files reference it. Confirmed intentional; no bug.
- **NFS sparse writes (holes):** `WriteAt` builds per-block memBlocks; gaps are zero-padded only inside a rollup pass's reconstruct buffer, not on the wire (rollup.go:409-431 reconstructStream). Sparse files don't materialize zero-bytes per-write; they only hash-merge zeros that are touched by a rollup window. No infinite zero-block PUT.
- **Verifier truncation handling:** `io.ReadFull` on `verifyingReader` propagates `io.ErrUnexpectedEOF` for short bodies, the verifier's `done` flag never gets set, the post-read `peek` triggers verifier `Read` which sees the body's EOF and runs `checkHash()` on the partial hash → mismatch → `ErrCASContentMismatch`. **Fail-closed for truncated streams.**
- **runID collision:** `randSuffix(6)` = 48 bits of entropy + RFC3339 second-resolution timestamp. Two same-second runs collide with probability ~2^-48 per pair; not a concern.
- **NFS COMMIT vs syncer mid-claim:** Flush only touches local; the syncer's own Pending → Syncing transition is per-row and serialized by `m.uploading.CompareAndSwap`. Flush during a syncer tick cannot lose data — both paths converge on the local store as source of truth.
- **`gc.sweep_concurrency` upper bound:** Validated in config (≤32 hard error, default 16), and additionally clamped to 32 in engine `CollectGarbage` (gc.go:158-160). Defense-in-depth is correct.
- **`syncer.upload_concurrency` vs `claim_batch_size` invariant:** validated (config.go:137-140); no way for upload pool to outsize claim batch.
- **`syncer.tick` (30s) vs `claim_timeout` (10m):** 20× headroom; no tick-faster-than-timeout edge.
- **Cross-protocol interop tests:** `test/e2e/cross_protocol_test.go` exists (NFS↔SMB read/write/delete/dir). Phase 11 is engine-internal, so cross-protocol is structurally unchanged. A targeted "drain remote → invalidate local → other-protocol read forces CAS path" test would be nice-to-have but not a Phase 11 regression risk — same engine on both sides.

---

## Warnings

### WR-3-01: Concurrent `CollectGarbage` calls against the same `GCStateRoot` race in `CleanStaleGCStateDirs` → one run can delete the other's open Badger DB mid-mark

**File:** `pkg/blockstore/engine/gc.go:166-172` (CollectGarbage stale-dir cleanup) and `pkg/blockstore/engine/gcstate.go:135-158` (`CleanStaleGCStateDirs`)

**Issue:** The mark phase persists the live ContentHash set in a Badger DB at `<GCStateRoot>/<runID>/db/` and drops `<GCStateRoot>/<runID>/incomplete.flag` until `MarkComplete()` removes it on success. On entry, `CollectGarbage` calls `CleanStaleGCStateDirs(GCStateRoot)` which:

1. `os.ReadDir(rootDir)`
2. for every per-runID subdir, `os.Stat(<dir>/incomplete.flag)`
3. if the flag exists → `os.RemoveAll(<dir>)`

There is no mutex anywhere serializing `CollectGarbage` invocations. Two concurrent calls produce the following interleaving:

- T=0  Run A enters `CollectGarbage`. CleanStaleGCStateDirs finds no stale dirs.
- T=1  Run A `NewGCState`: creates `<root>/A-runID/`, drops `incomplete.flag`, opens Badger.
- T=2  Run B enters `CollectGarbage`. CleanStaleGCStateDirs sees `<root>/A-runID/incomplete.flag` and **deletes the entire `<root>/A-runID/` directory** — including the open Badger DB files Run A is actively writing to.
- T=3  Run A's next `gcs.Add(h)` either silently succeeds (Badger writes to deleted-but-still-fd-open files), errors with disk-write failure, or — most likely — Badger's compaction/flush pool surfaces a confusing error a few minutes later.

The downstream consequences are subtle but real:

1. **Run A's mark-phase live set is silently truncated** (writes after deletion may or may not survive). If Run A's sweep proceeds (CollectGarbage doesn't propagate an error from `gcs.Add` in the bug case where the FS doesn't error immediately), it sweeps with an incomplete live set → INV-04 violation by data path → **silent live-data deletion**.
2. **Run A's `last-run.json` may be persisted with a phantom-success summary** that doesn't reflect what it actually did.
3. **Run B's Badger DB is fine** but its mark/sweep is now shadowed by Run A racing it on the same remote.

**Reachability today:**

- **Two parallel `dfsctl store block gc <share>` invocations** (operator runs second one because the first looks stuck or to compare dry-run vs real-run output).
- **CLI + REST overlap** — the REST handler and CLI both call `Runtime.RunBlockGCForShare`, no mutex.
- **`gc.interval` periodic GC + manual on-demand trigger** — would be reachable if the periodic ticker existed (see WR-3-02), but absent today.
- **Cross-share with shared `GCStateRoot`** — actually NOT a concern: `RunBlockGCForShare` derives the root from the share's local store dir (`GetGCStateDirForShare`), so two distinct shares get distinct roots. But `RunBlockGC` (no share scope) writes to a TEMP root per call (gc.go:179, `os.MkdirTemp("", "dittofs-gc-")`) — which IS unique per call. The **vulnerable case** is two `RunBlockGCForShare` calls for the **same share name** running in parallel.

**Confidence:** HIGH for the race window (static-trace confirmed). MEDIUM for "Badger silently survives the directory deletion" — POSIX lets the open fd keep working until close, but Badger's value-log + LSM compaction may try to create new files under the (now-recreated-by-Run-B) directory and produce confusing errors. Either outcome is unsafe.

**Fix sketch:** Three options, in order of robustness:

**Option A (process-level mutex):** Serialize all CollectGarbage calls per `GCStateRoot` with a sync.Mutex held by the runtime:

```go
// pkg/controlplane/runtime/runtime.go
type Runtime struct {
    // ...
    gcMu sync.Mutex // serializes CollectGarbage per process; Phase 11 WR-3-01.
}

// in RunBlockGC / RunBlockGCForShare, before collectGarbageFn(...)
r.gcMu.Lock()
defer r.gcMu.Unlock()
```

This is the minimal correct fix and matches the operational reality (one operator running on-demand GC at a time). For multi-process deployments, a flock-on-`<GCStateRoot>/.gc.lock` is the cross-process variant.

**Option B (lock the GCStateRoot on entry):** `CollectGarbage` itself acquires a `flock()` on `<GCStateRoot>/.gc.lock` before `CleanStaleGCStateDirs`. Releases on defer. Cross-process safe; OS-level guarantee.

**Option C (skip-cleanup-when-active):** `CleanStaleGCStateDirs` learns to distinguish "stale because crashed" from "in-flight from another process" by the marker's mtime — only sweep dirs whose `incomplete.flag` is older than e.g. 2× `gracePeriod`. Cleaner than B but couples cleanup to grace_period, which is a config knob meant for a different concern (clock skew).

Recommended: **Option A** for v0.15.0 (single-process today) plus a `// TODO: cross-process lock for multi-server`. Add a regression test that fires N=10 goroutines all calling `CollectGarbage(ctx, sharedRoot, ...)` and asserts every per-run Badger DB is intact at completion.

---

### WR-3-02: `gc.interval` is parsed, validated, documented as the periodic-GC trigger — and never read by any code path

**File:** `pkg/config/config.go:148-167` (field definition + validate); referenced in docs/CONFIGURATION.md:307, docs/ARCHITECTURE.md:408, docs/FAQ.md:164

**Issue:** REVIEW-1 WR-01 fix landed `GracePeriod`, `SweepConcurrency`, and `DryRunSampleSize` into `runtime.GCDefaults` (runtime.go:455-464) and through to `engine.Options`. **`Interval` was not included.** A grep for `GC.Interval`, `gcDefaults.Interval`, or any periodic-GC ticker construction in `cmd/dfs/`, `pkg/controlplane/`, or `pkg/blockstore/engine/` returns zero hits.

The field is parsed by Viper, surfaces in `GCConfig.Interval`, and is then dropped on the floor. Operators who configure `gc.interval: 24h` get:

- Config validation passes (silent acceptance).
- Server starts without warning.
- No periodic GC ever runs.
- `dfsctl store block gc-status` shows last-run from the most recent **manual** run.

Three documentation locations actively claim this works:

- `docs/CONFIGURATION.md:307-310` — `interval: 0  # Periodic GC interval. Default 0 = disabled... Set to e.g. 6h or 24h once you are confident in your live-set size.`
- `docs/ARCHITECTURE.md:408-409` — `Periodic via gc.interval (default 0 = disabled; operator opt-in for the v0.15.0 first deploy, D-08).`
- `docs/FAQ.md:164` — `Periodic via gc.interval (default 0 = disabled; opt-in for the v0.15.0 first deploy)`

This is a **silent feature non-existence** — a worse failure mode than a build error or a startup warning. Operators who want periodic GC and follow the docs will get no GC at all and won't know.

**Confidence:** HIGH. Static-grep verified.

**Fix:** Two options:

**Option A (implement the periodic ticker):** Add a goroutine in `cmd/dfs/commands/start.go` (or in Runtime startup) that fires `time.NewTicker(cfg.GC.Interval)` when `Interval > 0`. Call `runtime.RunBlockGC(ctx, "", false)` per tick (or per-share if you prefer to surface per-share `last-run.json` summaries). Combine with WR-3-01's mutex so a long-running GC doesn't pile up tickets.

```go
// cmd/dfs/commands/start.go (after rt.SetGCDefaults)
if cfg.GC.Interval > 0 {
    go func() {
        ticker := time.NewTicker(cfg.GC.Interval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                if _, err := rt.RunBlockGC(ctx, "", false); err != nil {
                    logger.Error("periodic GC failed", "err", err)
                }
            case <-ctx.Done():
                return
            }
        }
    }()
    logger.Info("periodic GC enabled", "interval", cfg.GC.Interval)
}
```

**Option B (remove the field + revert docs):** If periodic GC is genuinely deferred to a future phase, delete `GCConfig.Interval`, the validation block, and the three doc references. Operators who want periodic GC use cron + `dfsctl store block gc`.

Recommended: **Option A**, since the operator-facing contract is already advertised. Defense-in-depth: the new ticker should respect `WR-3-01`'s mutex so a periodic + manual overlap is safe.

Option B is the honest alternative if the team would rather not ship a periodic scheduler without a metrics phase; in that case, immediately revert the docs and emit a startup WARN if `gc.interval > 0` is configured.

---

## Info

### IN-3-01: `gc.grace_period < 5m` documented as "logged as a warning" — pass-2 IN-2-03 fix made it a hard config-validation error

**File:** `docs/CONFIGURATION.md:317-320`

**Issue:** Pre-pass-2, sub-5m grace periods were a runtime warning. Pass-2's IN-2-03 commit (`32d88ef8`) hardened the validator to reject any positive `grace_period < 5m` with a fatal config-load error (config.go:203-205). The CONFIGURATION.md doc still reads:

> grace_period: 1h            # Objects whose LastModified is newer than
>                             # (snapshot - grace_period) are NEVER
>                             # deleted. Default 1h. Setting below 5m
>                             # is logged as a warning -- the cushion
>                             # protects in-flight uploads whose
>                             # metadata-txn lands after the snapshot.

**Fix:** Update the comment to reflect the new contract (rejection at config load, with `[5m, 10m)` as the warn band). One-line doc edit:

```yaml
grace_period: 1h            # ... Default 1h. Values in (0, 5m) are
                            # rejected at config load; values in
                            # [5m, 10m) emit a warning.
```

### IN-3-02: Postgres `idx_file_blocks_hash` UNIQUE partial index will fail the dedup short-circuit's `PutFileBlock` for any cross-file hash collision; conformance suite has no test for it

**File:** `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql:29-30` (the constraint), `pkg/blockstore/engine/upload.go:101-114` (the dedup short-circuit), `pkg/metadata/storetest/file_block_ops.go` (no test for cross-block hash collision)

**Issue:** The migration declares:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_file_blocks_hash
    ON file_blocks(hash) WHERE hash IS NOT NULL;
```

`PostgresMetadataStore.PutFileBlock` uses `INSERT ... ON CONFLICT (id) DO UPDATE` — only resolves PK conflict on `id`, NOT the unique-on-hash constraint.

When `uploadOne`'s pre-PUT dedup short-circuit fires (upload.go:101) for two distinct files with identical block content (e.g., file1 block 0 and file2 block 0 both contain identical 8MB of zeros — the dominant case for VM image dedup):

1. `existing` = file1's row (state=Remote, hash=H, id=file1/0).
2. `IncrementRefCount(existing.ID)` succeeds → file1's refcount bumps.
3. `fb.Hash = hash; fb.State = Remote; PutFileBlock(fb)` where `fb.ID=file2/0`.
4. INSERT collides with `idx_file_blocks_hash` (file1's row already has hash=H). PostgreSQL returns `duplicate key value violates unique constraint "idx_file_blocks_hash"`.
5. uploadOne returns the error → fb stays Syncing, donor's refcount is permanently leaked.
6. Janitor requeues fb after `claim_timeout` (10m). Cycle repeats indefinitely.

Memory and Badger backends both maintain a `hashIndex map[hash]→id`; their `PutFileBlock` calls `set(hashKey, id)` which **silently overwrites** — no collision. So the test suite (`TestSyncer_Deduplication_Memory`) passes against memory but the Postgres backend is broken for this exact path.

**Pre-existing bug:** Predates Phase 11 (the constraint was in the inline `fileBlocksTableMigration` const). But Phase 11:
- Tightened reliance on dedup with the PUT-first ordering and the explicit short-circuit at upload.go:101.
- Codified the schema in a migration runner (so a fresh deploy now reliably has the constraint).
- Did not add a cross-backend conformance test that would have caught this.

**Confidence:** HIGH for the constraint behavior (verified via grep + SQL semantics). MEDIUM for production impact — workloads with little block-level dedup never hit it; VM workloads (the primary use case per project memory) hit it constantly.

**Fix:** Three options:

**Option A (resolve conflict at the metadata layer):** Add `... ON CONFLICT (hash) WHERE hash IS NOT NULL DO NOTHING` to the INSERT, and have `PutFileBlock` return a sentinel `ErrFileBlockHashCollision` the engine handles by re-pointing fb to the donor's row (which is morally what dedup is doing anyway). Aligns Postgres with the silent-overwrite semantics of memory/badger.

**Option B (drop the unique constraint):** Use a non-unique index for hash lookups. The semantic invariant "every CAS object has exactly one metadata row" is enforced by the dedup logic itself, not by the database — the constraint provides no actual guarantee the application doesn't already have, and breaks the dedup path it was meant to help.

**Option C (drop the dedup short-circuit entirely, per WR-03 follow-up):** Already noted in REVIEW-1 WR-03's "narrowest fix" — let the duplicate CAS PUT happen (idempotent, byte-identical, cheap), and rely on mark-sweep GC for the cleanup. This is the simplest path and naturally aligns with the multi-process tolerance the WR-05 doc-comment now describes.

**Conformance gap (independent of which fix):** Add a cross-backend test in `pkg/metadata/storetest/file_block_ops.go`:

```go
t.Run("PutFileBlock_TwoIDsSameHash", func(t *testing.T) {
    testPutFileBlockTwoIDsSameHash(t, factory)  // assert the backend's documented behavior
})
```

Pick the contract (silent-overwrite, error-on-collision, or ON-CONFLICT-DO-NOTHING) and require every backend to honor it. Today the contract is undefined and the three backends disagree.

### IN-3-03: GC `FirstErrors` captures the first 16 errors verbatim — a burst of 100 identical S3 503s hides any 17th distinct error

**File:** `pkg/blockstore/engine/gc.go:317-319` (sweepPhase) and `gc.go:407-410` (recordGCError)

**Issue:** Both error capture paths gate on `len(stats.FirstErrors) < 16`. A run that produces 16 identical "list cas/aa: 503 SlowDown" errors followed by a single "delete cas/bb/...: AccessDenied" loses the AccessDenied error entirely — the operator sees only a homogeneous error list and infers "transient throttling, will retry" when a permission misconfiguration is the actual problem.

**Confidence:** MEDIUM. The 16-cap is reasonable for boundedness; the diversity-of-sample is the gap.

**Fix:** Replace the cap-on-count with a cap-per-error-class. Two-line change:

```go
addError := func(msg string) {
    statsMu.Lock()
    defer statsMu.Unlock()
    stats.ErrorCount++
    // Per-class diversity: capture up to N distinct error prefixes.
    cls := classifyErr(msg) // first 60 chars, or up to first ":" — keep it cheap
    if _, seen := stats.errorClasses[cls]; !seen && len(stats.errorClasses) < 16 {
        stats.errorClasses[cls] = struct{}{}
        stats.FirstErrors = append(stats.FirstErrors, msg)
    }
}
```

Or, simpler: keep the count cap but also retain the LAST error of each unique class. Either gives operators a heterogeneous sample.

### IN-3-04: GC engine logs lack share/endpoint context for cross-correlation with S3 access logs

**File:** `pkg/blockstore/engine/gc.go:198-204` (mark start) and `gc.go:228-236` (complete)

**Issue:** The engine's structured logs include `run_id`, `snapshot_time`, `dry_run`, `grace_period`, `sweep_concurrency`, `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`, `error_count`. The engine doesn't know the share name (only the runtime caller does — and `RunBlockGCForShare`'s log includes it at runtime.go:131-140), but it also doesn't know the remote endpoint identity (bucket, prefix). For an SRE correlating GC activity with S3 access logs (e.g., "did GC issue these DELETEs at 14:30 UTC?"), the engine log alone is not enough.

**Confidence:** LOW. Operationally annoying, not a correctness bug.

**Fix:** Either thread `remoteStoreID` (e.g., bucket name + prefix) into `engine.Options` and include it in the start/complete log, or have callers wrap the engine log with their own context tags. The minimum useful improvement is including the remote's `Identity()` (or its `configID`) in the engine's two log lines — one new field per line.

### IN-3-05: `dispatchRemoteFetch` returns silent zeros (`nil, nil`) when a CAS-format key returns `ErrBlockNotFound` — masks GC-deletes-live-data data corruption

**File:** `pkg/blockstore/engine/fetch.go:122-126` (fetchBlock error handling)

**Issue:** Pre-Phase-11, returning `nil, nil` on `ErrBlockNotFound` was correct: the only reason a key would be absent was sparseness (the syncer never PUT it). Post-Phase-11, the same path is taken when:

- The block is genuinely sparse (correct).
- The block exists in metadata as state=Remote with a CAS hash, but the CAS object is missing from the remote — a state that should NEVER occur under INV-04 (mark fail-closed) but WOULD occur if WR-3-01 (GC racing GC) corrupts the live set, or if a future bug allows GC to delete a live CAS object.

The fix is asymmetric: legacy-path (`!fb.Hash.IsZero() == false`) absent → sparse, return zeros (correct). CAS-path (`!fb.Hash.IsZero() == true`) absent → live-data-loss, should be an ERROR not silent zeros. INV-06's read-time verifier catches the *corrupt-bytes* case but not the *missing-bytes* case.

**Confidence:** LOW for impact under correct GC. HIGH for impact when WR-3-01 fires.

**Fix:** Split the routing:

```go
// fetch.go:121, after dispatchRemoteFetch
storeKey, data, err := m.dispatchRemoteFetch(ctx, fb)
if err != nil {
    if errors.Is(err, blockstore.ErrBlockNotFound) {
        if !fb.Hash.IsZero() {
            // CAS-keyed row whose object is missing — should never happen
            // under INV-04. Surface as a hard error so silent data loss
            // is visible. Correlates with GC.
            return nil, fmt.Errorf("CAS object missing for live row %s (key %s) — possible GC race or live-data-loss: %w",
                fb.ID, storeKey, err)
        }
        return nil, nil // legacy sparse — preserve old behavior
    }
    return nil, fmt.Errorf("download block %s: %w", storeKey, err)
}
```

This is a defense-in-depth measure that turns INV-04 violations into loud errors instead of silent zero-byte reads.

---

_Reviewed: 2026-04-25_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: deep (cross-protocol: NFS3/4 COMMIT, SMB durable; GC concurrency; backend-skew; doc/code drift)_
_Pass: 3 of N — pass-1 + pass-2 fixes confirmed clean; pass-3 surfaces a GC concurrency race + a multi-doc drift on `gc.interval`, plus polish_
