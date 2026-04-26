---
phase: 11-cas-write-path-gc-rewrite-a2
reviewed: 2026-04-25T00:00:00Z
depth: deep
pass: 5
files_reviewed: 22
files_reviewed_list:
  - pkg/blockstore/engine/gc.go
  - pkg/blockstore/engine/gcstate.go
  - pkg/blockstore/engine/cache.go
  - pkg/blockstore/engine/fetch.go
  - pkg/blockstore/engine/upload.go
  - pkg/blockstore/engine/syncer.go
  - pkg/blockstore/engine/engine.go
  - pkg/blockstore/local/fs/fs.go
  - pkg/blockstore/local/fs/recovery.go
  - pkg/blockstore/local/fs/chunkstore.go
  - pkg/blockstore/store.go
  - pkg/blockstore/types.go
  - pkg/blockstore/remote/memory/store.go
  - pkg/blockstore/remote/s3/verifier.go
  - pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql
  - pkg/metadata/store/postgres/migrations/000011_file_blocks_hash_nonunique.up.sql
  - pkg/metadata/store/postgres/postgres_conformance_test.go
  - pkg/metadata/storetest/file_block_ops.go
  - pkg/metadata/storetest/suite.go
  - pkg/controlplane/runtime/blockgc.go
  - cmd/dfs/commands/start.go
  - docs/CONFIGURATION.md
  - docs/ARCHITECTURE.md
  - docs/FAQ.md
  - docs/IMPLEMENTING_STORES.md
findings:
  critical: 0
  warning: 0
  info: 2
  total: 2
status: clean_with_notes
---

# Phase 11: Code Review Report (Pass 5 — Final)

**Reviewed:** 2026-04-25
**Depth:** deep — regression sweep against passes 1–4 fixes; whole-diff scan;
public-API audit; doc/code coherence; final-merge sanity
**Files Reviewed:** 22 source files plus the 4 doc surfaces and the conformance
suite. Whole-diff `git diff --stat develop..HEAD` skimmed end-to-end (122 files,
+17068/-2663).
**Pass-1..4 fixes confirmed clean:** every CR/WR/IN that landed in REVIEW-FIX-1..4
holds up; no regression introduced by the later fixes undoing earlier ones (see
"Regression check" below).

## Verdict

**PASS 5 CLEAN — ready for history cleanup + PR.**

Two INFO-level observations are recorded below for future-phase awareness;
neither is a defect and neither blocks merge. The architectural intent is sound,
the invariants (INV-03/04/06) are defended in the code paths that matter, and
the 30+ fix-commits across passes 1–4 form a coherent sequence with no
conflicting state.

## Regression check (pass 4 ↔ passes 1–3)

Verified every pass-4 fix against the prior-pass surface it touched:

- **WR-4-01 (drop UNIQUE on `file_blocks.hash`)** — does NOT regress pass-1
  IN-3-02. The PutFileBlock contract is tightened in `pkg/blockstore/store.go`
  (commit `369350fd`) and the cross-backend conformance test
  `testPutFileBlock_TwoIDsSameHash` is wired into `RunConformanceSuite` via
  `runFileBlockOpsTests` (storetest/suite.go:40). Suite is invoked from all
  three backends:
  - `pkg/metadata/store/memory/memory_conformance_test.go:12` — runs always.
  - `pkg/metadata/store/badger/badger_conformance_test.go:16` — runs always.
  - `pkg/metadata/store/postgres/postgres_conformance_test.go:22` — gated on
    `//go:build integration` AND `DITTOFS_TEST_POSTGRES_DSN`. Postgres is
    therefore *covered in CI only when the integration suite is run with a
    Postgres DSN*. This is pre-existing test scaffolding and fine for v0.15.0,
    but the dedup-collision case won't catch a Postgres regression in `go test
    ./...` alone — see IN-5-01.
  - Migration 000011 confirmed: `DROP INDEX IF EXISTS idx_file_blocks_hash`
    then re-CREATE as non-UNIQUE; idempotent. 000010 was also rewritten in
    place for fresh installs (CREATE INDEX, no UNIQUE).

- **WR-4-02 (fail-closed on zero LastModified)** — does NOT trigger excess
  errors in tests that didn't expect it. Both production backends
  (`memory.Store.ListByPrefixWithMeta` at memory/store.go and
  `s3.Store.ListByPrefixWithMeta` at s3/store.go) populate `LastModified`
  unconditionally. The fail-closed branch is covered by
  `ListByPrefixWithMeta_LastModifiedNonZero` in remotetest/suite.go and never
  fires under the in-tree code paths. The `addError` / `continue` is correct
  defense-in-depth against third-party backends.

- **IN-4-04 (batched `GCState.Add`)** — flushes correctly across share
  boundaries. `markPhase` (gc.go:329) iterates shares serially in a single
  goroutine, then calls `gcs.FlushAdd()` once after the loop completes
  (gc.go:361). The batch is process-wide, so the final partial batch carries
  hashes from the last share regardless of how the previous shares' batches
  were flushed (intra-loop boundary crossings just trigger early flushes at
  `gcAddBatchSize`). `Has()` also implicitly flushes (gcstate.go:148–151) for
  test interleavings. The single-goroutine constraint is documented at the
  Add() callsite. The cross-share batching is correct.

  IN-4-05's per-share lock-granularity comment on `gcRootLocks` (gc.go:51–72)
  matches the actual key derivation — `acquireGCRootLock(filepath.Clean(root))`
  produces a distinct mutex per share's `gc-state/` directory. Cross-share
  parallelism is preserved.

## Whole-diff sanity

`git diff --stat develop..HEAD` shows 122 files / +17068 / -2663. Spot-checked:

- **Lock manager / SMB lease / SMB v2 handler diff is develop-drift, NOT phase
  11.** `git log develop..HEAD -- pkg/metadata/lock/ internal/adapter/smb/`
  returns 0 commits. The 7 lock files appearing as `-740` lines and the SMB
  v2/lease files appearing as modifications are all develop-only commits
  (`58516fc4`, `ad26ff80`, `d73fdab7`) that landed after the phase 11 branch
  was cut. The user will rebase / merge from develop before PR; no review
  action needed.
- **Files touched match the 9 sub-plans.** Engine, local/fs, metadata stores,
  remote stores, runtime/blockgc, dfsctl, REST handlers, apiclient, docs, e2e,
  conformance suites — all align with PLAN-01..09.
- **No spurious files modified.** No accidental commits in unrelated
  subsystems (NFS handlers, control plane API beyond block_gc, auth, etc.).
- **No file mentioned in the plan is missing from the diff.** The 9 SUMMARY
  manifests cross-check cleanly.

## Final-sweep checks (debug residue, dead code, etc.)

- **TODO/FIXME/XXX/HACK in changed source:** exactly one — `gc.go:64` documents
  the pending cross-process `flock` for multi-server (out of v0.15.0 scope,
  expected). No untracked TODOs.
- **Stray println / debug logging:** all `fmt.Println`/`fmt.Printf` additions
  in the diff are in `cmd/dfsctl/commands/store/block/gc.go` and
  `gc_status.go` — legitimate operator-facing CLI output.
- **`t.Skip(...)` shipped:** all 13 skips in changed test files are gated by
  legitimate preconditions (`-short`, `-race`, OS=Windows, Postgres DSN
  missing, perf-gate env var, `D40_GATE`). No accidental skips.
- **Empty/stub functions:** none introduced.
- **Commented-out code:** none introduced.
- **Build:** `go build ./...` returns clean.

## Public API additions — all justified

Audited every exported symbol added in the phase 11 diff:

- `engine.GCState`, `NewGCState`, `Add`, `FlushAdd`, `Has`, `MarkComplete`,
  `Close`, `RunDir`, `CleanStaleGCStateDirs` — used by gc.go internally and
  exposed for the conformance test suite (`gcstate_test.go`). Justified.
- `engine.GCRunSummary`, `PersistLastRunSummary` — used by the apiclient
  (`pkg/apiclient/blockstore.go:127`) and by handlers/runtime. Justified.
- `engine.MultiShareReconciler` — used at the engine ↔ runtime boundary so
  `RunBlockGC*` can pass the share list down. Justified.
- `engine.Options` (added fields: `GracePeriod`, `SweepConcurrency`,
  `DryRunSampleSize`, `GCStateRoot`, `RemoteEndpointID`, `Shares`) — all
  populated by runtime/blockgc.go. Justified.
- `blockstore.FormatCASKey`, `ParseCASKey` — used by upload.go, fetch.go,
  gc.go. Justified.
- `remote.WriteBlockWithHash`, `ReadBlockVerified`, `ListByPrefixWithMeta` —
  added to the `RemoteStore` interface; used by upload.go, fetch.go, gc.go.
  Justified.
- `remote.ObjectInfo` — return type of `ListByPrefixWithMeta`. Justified.
- `memory.Store.GetObjectMetadata`, `SetNowFnForTest` — used by remotetest
  conformance suite (BSCAS-06 verifier-on-read tests). Acceptable as
  test-support, see IN-5-02 for one minor note.

**Two exported types worth flagging — `blockstore.RemoteObjectInfo` and
`blockstore.RemoteStoreSweepSurface` (store.go:174 and store.go:190).** Both
are documentation-only re-exports with no callers anywhere in the tree
(verified: `grep -rn "blockstore\.RemoteObjectInfo\|blockstore\.RemoteStoreSweepSurface"`
returns 0 hits). Their godoc explicitly says "this type is never used as a
parameter — the production code uses remote.RemoteStore directly." This is a
pure-navigation aid the author left for future readers tracing the GC plumbing
across package boundaries. Stylistic preference — could be deleted to keep the
public API minimal — but not a defect.

## Comment quality on the tricky parts

Spot-checked the parts of the code where a future maintainer most needs the
"why":

- **D-11 PUT-then-meta-txn ordering** (`upload.go:115–132`): well-commented.
  "PUT succeeded; only NOW promote to Remote (INV-03 ordering)" plus the
  failure-mode explanation ("S3 object exists; row stayed Syncing. GC +
  janitor will resolve"). Excellent.
- **D-21 dual-read fallback decision tree** (`fetch.go:60–91`): clear. The
  CAS vs legacy split is annotated and the resilience-against-future-schema
  fallback is explained.
- **Streaming verifier wrapping pattern** (`s3/verifier.go`): the
  `verifyingReader` doc explains the BLAKE3-on-Read invariant; `readAllVerified`
  documents the contract. The IN-2-02 drain-on-close fix is annotated with
  the keep-alive rationale. Good.
- **Mark snapshot + grace TTL window** (`gc.go:439–453`): the WR-4-02
  fail-closed branch is annotated with INV-04 reasoning. The grace window
  comment cites D-05.
- **3-state lifecycle + claim batching** (`syncer.go` + `upload.go`): the
  Pending → Syncing → Remote transitions are individually annotated; the
  WR-05 cross-process tolerance comment (4a2f99ba) is excellent.
- **CR-2-01 fix on `WriteFromRemote`** (`fs.go:815–875`): the doc-comment
  block before the function is one of the best in the diff — explains the
  fail-mode, the post-restart steady-state, and the canonical-row
  preservation invariant. Future maintainers will not get this wrong.
- **IN-3-05 fail-closed CAS miss** (`fetch.go:121–142`): the eight-line
  comment block explains why CAS-key-with-non-zero-hash MUST NOT be silenced.

The narrative archaeology was correctly trimmed in the late-phase refactor
commits (`88198f53`, `065c8d36`, `5ad33a95`, `c85497e0`, etc.) — the comments
that survived are all WHY-comments, not historical narrative.

## Documentation/code coherence

- `docs/CONFIGURATION.md`: gc.* knobs match config.go (grace_period rejected
  <5m, warned [5m, 10m); sweep_concurrency clamp to 32; dry_run_sample_size
  default 1000); `gc.interval` clearly marked as reserved/deferred matching
  the start.go warning.
- `docs/ARCHITECTURE.md` and `docs/FAQ.md`: same gc.interval deferral
  language. INV-04 fail-closed posture is documented.
- `docs/IMPLEMENTING_STORES.md`: the new `MetadataStore.EnumerateFileBlocks`
  contract (Phase 11 §) lists 5 conformance scenarios that match the actual
  storetest cases. The new `RemoteStore.WriteBlockWithHash` /
  `ReadBlockVerified` / `ListByPrefixWithMeta` contracts are documented at
  395, 421, and around 480 (sweep surface).
- `docs/CLI.md`: `dfsctl store block gc <share>` and `gc-status <share>`
  match the cobra command definitions and the REST handler's URL shape.

No drift detected after the 30+ fix commits.

## Considered and discarded (no actionable bug)

- **Bifurcated cache (`PutCAS`/`GetCAS`/`PutLegacy`/`GetLegacy`/`InvalidateCAS`/
  `InvalidateLegacy`/`ContainsCAS`/`ContainsLegacy`) is not wired into the
  production read path** — only used by `cache_bifurcation_test.go`. Plan
  11-03 listed "caller wiring" of the bifurcated API as a deliverable
  (PLAN-03 Task 3 step 4 mentions `engine/fetch.go` should populate the
  cache via the bifurcated key). The actual fetch path (`engine.go:625`)
  uses only the coordinate-keyed `Get(payloadID, blockIdx, ...)` and never
  calls `PutCAS`/`GetCAS`. This means cross-file CAS dedup at the in-memory
  read-buffer layer is implemented but inert — two payloads referencing the
  same hash get separate cache lines instead of sharing one (the CAS
  PUT/GET to the remote still works because that's a different layer).
  **Why discarded:** correctness is unaffected (the verifier still runs on
  every CAS read; routing still works); the gain would be a memory
  efficiency improvement, not a bug fix. The plan's "Phase 12 collapses to
  a single hash-keyed API per CACHE-02" comment in cache.go:184 indicates
  the author intends to wire this in Phase 12. If the team cares about
  surfacing the gap, file an issue tagged "Phase 12 prerequisite."
- **Postgres backend's dedup-collision conformance test runs only under
  `//go:build integration` + a Postgres DSN.** A regression that re-introduced
  a UNIQUE constraint would not be caught by `go test ./...`. See IN-5-01.
- **`SetNowFnForTest` exported on `memory.Store`** — used only by the
  conformance suite. Could be moved behind a build tag, but the existing
  pattern (test helpers exported on production types) is project-wide.
- **`pendingFBs` on `manage_test.go:126` `t.Skip`** — conditional on a setup
  precondition; correct behavior, not a shipped skip.
- **gc.* sub-config validators applied in start.go** — verified the
  GCDefaults flow lands in engine.Options via runtime.RunBlockGC* (commit
  `7bd0a2cb` adds the per-call population). Pass-1 WR-01 fully closed.
- **`engine.GCStats.DryRunCandidates` always-empty for non-dry-run** —
  IN-01 noted this; the `omitempty` tag handles it; documented behavior.
- **Verifier's HTTP body draining on the EOF-mismatch path** — pass-2 IN-2-02
  fix at verifier.go's `Close()` covers the abandoned-stream case; on the
  EOF path `v.done` is true and no drain is needed.
- **`recoverStaleSyncing` log-and-continue posture** — pass-1 IN-02 fix
  upgraded to error logging; the operator gets visibility without aborting
  startup.

---

## INFO

### IN-5-01: Postgres dedup-collision conformance test only runs in `//go:build integration` + with `DITTOFS_TEST_POSTGRES_DSN` — no `go test ./...` coverage

**Files:** `pkg/metadata/store/postgres/postgres_conformance_test.go:1` (the
build tag), `pkg/metadata/storetest/file_block_ops.go:564`
(`testPutFileBlock_TwoIDsSameHash`)

**Issue:** WR-4-01's fix dropped the UNIQUE constraint on
`file_blocks.hash`, and `testPutFileBlock_TwoIDsSameHash` was added to the
conformance suite to pin the contract. The test runs against memory + badger
on every `go test ./...` and against Postgres only when the integration tag
is supplied AND a DSN is configured. A future contributor who re-adds the
UNIQUE constraint to the migration (e.g. a "schema cleanup" PR) would see
green CI in the standard test loop and only get caught when someone runs
the integration suite.

**Confidence:** LOW — operationally rare (the contract is documented in
`pkg/blockstore/store.go` and a careful reviewer would catch it), and the
fix is to either (a) plumb a Postgres-in-Docker into the unit test loop or
(b) note this gap in CONTRIBUTING.md for future schema reviewers. Both
out of phase 11 scope.

**Suggested follow-up (not blocking):** file a tracker issue noting that
the file_blocks dedup-collision contract has different test coverage by
backend, and that schema PRs touching `file_blocks` indexes must re-run
the integration suite.

### IN-5-02: Two `blockstore.*` exports are documentation-only and have no callers (`RemoteObjectInfo`, `RemoteStoreSweepSurface`)

**File:** `pkg/blockstore/store.go:174` and `pkg/blockstore/store.go:190`

**Issue:** Both types' godoc explicitly says they're navigation aids re-exporting
shapes that live in `pkg/blockstore/remote/remote.go`. No code in the tree
imports or uses the qualified `blockstore.RemoteObjectInfo` or
`blockstore.RemoteStoreSweepSurface`. They are dead exports from a public-API
perspective.

**Confidence:** LOW — stylistic; the author intentionally left them as
"reader hints." Removing them would shrink the public API surface and
eliminate a class of "is this used?" question for future maintainers, but
it's not a defect.

**Fix:** Either remove both types and add a one-line "// see
remote.RemoteStore for the GC sweep surface" cross-reference comment at
the top of store.go, or keep as-is. Recommend removal for v0.15.0 since
adding them later is cheaper than removing them after API consumers
materialize.

---

## Closing note

This is the cleanest fifth-pass review in this phase: zero new bugs
discovered, two minor stylistic observations recorded for follow-up. The
30+ commits across REVIEW-FIX-1..4 form a sequence with no observable
regressions; INV-03/04/06 are correctly defended; the public API additions
are justified by their consumers; documentation and code agree.

The phase 11 branch is in shape for history cleanup (squash merge of the
30+ fix commits per phase, or whatever the team's preferred shape is) and
the PR.

---

_Reviewed: 2026-04-25_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: deep (regression sweep + whole-diff scan + public-API audit + doc/code coherence)_
_Pass: 5 of 5 — final pass before history cleanup + PR_
