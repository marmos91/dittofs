---
phase: 14-migration-tool-a5
plan: 05
subsystem: blockstore
tags: [migration, integrity_check, head_per_ref, cutover, legacy_gc, fail_loud, headobject, cas]

# Dependency graph
requires:
  - phase: 14-migration-tool-a5
    provides: Plan 14-03 re-chunk loop + journal + WalkShareFiles helper; Plan 14-04 worker pool / errgroup pattern; Plan 14-01 BlockLayout enum + ShareOptions field
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: blockstore.FormatCASKey + ParseCASKey + ContentHash.CASKey()
provides:
  - "remote.HeadObject(ctx, key) -> (HeadResult, error) on the public RemoteStore interface; HeadResult{ContentLength int64, Metadata map[string]string} with lowercased header keys"
  - "s3 backend HeadObject (uses AWS SDK HeadObject, maps NoSuchKey to ErrBlockNotFound, defensively lowercases SDK metadata keys)"
  - "memory backend HeadObject (returns the in-process metadata map populated by WriteBlockWithHash)"
  - "remotetest.TestHeadObjectRoundTrip conformance scenario covering 200+content-hash header and ErrBlockNotFound on missing keys; wired into the existing TestRemoteStoreSuite entry point so memory + s3 backends both exercise it"
  - "cmd/dfsctl/commands/blockstore.verifyIntegrity — walks the share's post-migration FileAttr.Blocks, aggregates the unique-hash set, HEADs each unique CAS key in parallel (errgroup, opts.parallel default 4), asserts (1) 200 + (2) Metadata['content-hash'] == blake3:{hex}; ErrIntegrityCheckFailed sentinel for fail-loud"
  - "cmd/dfsctl/commands/blockstore.performCutover — idempotent UpdateShareOptions txn flipping BlockLayout from legacy to cas-only"
  - "cmd/dfsctl/commands/blockstore.deleteLegacyKeys — best-effort errgroup-bounded sweep of {payloadID}/block-{idx} keys (cas/* skipped via ParseStoreKey filter); per-key DeleteBlock errors aggregated, do not abort the sweep"
  - "End-to-end runMigrateLoopWithRuntime pipeline: walk → re-chunk → integrity → cutover → legacy GC, with strict fail-loud ordering (D-A8) and dry-run skip-everything semantics"
affects: [14-06-rest-status, 14-07-docs-runbook]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "HEAD-per-unique-hash integrity check: union map over post-migration FileAttr.Blocks[*].Hash, errgroup-bounded parallel HEAD fleet, fails-on-first only for transient/network errors (which bubble unwrapped) — missing-data + header-mismatch failures are aggregated and surface as ErrIntegrityCheckFailed wrapping the full failure list"
    - "Best-effort sweep with aggregated failure return: per-key DeleteBlock errors collected into a slice, never abort the errgroup; final return is non-nil if any failure aggregated, but the count of successful deletions is also returned so the caller can report partial progress to the operator"
    - "Strict fail-loud ordering in runMigrateLoopWithRuntime: integrity → performCutover → deleteLegacyKeys, with early-return on integrity failure preserving BlockLayout=legacy + journal + legacy keys; cutover failure short-circuits before legacy delete (live data path still reads through the legacy keys)"
    - "Lowercased Metadata map convention on HeadResult: matches AWS SDK normalization on the wire (S3 lowercases x-amz-meta-* on response decode), and is enforced defensively via strings.ToLower in the s3 backend so non-AWS-SDK S3-compatible servers (Localstack/MinIO) can never break the contract"

key-files:
  created:
    - cmd/dfsctl/commands/blockstore/migrate_integrity.go
    - cmd/dfsctl/commands/blockstore/migrate_integrity_test.go
    - cmd/dfsctl/commands/blockstore/migrate_cutover.go
    - cmd/dfsctl/commands/blockstore/migrate_cutover_test.go
    - cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go
    - cmd/dfsctl/commands/blockstore/migrate_legacy_gc_test.go
  modified:
    - pkg/blockstore/remote/remote.go
    - pkg/blockstore/remote/s3/store.go
    - pkg/blockstore/remote/memory/store.go
    - pkg/blockstore/remote/remotetest/suite.go
    - cmd/dfsctl/commands/blockstore/migrate_loop.go
    - cmd/dfsctl/commands/blockstore/migrate_loop_test.go
    - pkg/blockstore/engine/gc_test.go
    - pkg/blockstore/engine/syncer_flush_test.go
    - pkg/controlplane/runtime/blockgc_test.go

key-decisions:
  - "verifyIntegrity treats missing-key + header-mismatch as aggregated failures (full list collected) but lets transient/network errors bubble up unwrapped via errgroup. Rationale: D-A8 fail-loud requires the operator to see the full picture before triaging — first-error abort would mask other broken keys and delay diagnosis. Transient errors are operationally distinct (operator retries) and should not be wrapped as ErrIntegrityCheckFailed."
  - "deleteLegacyKeys sweeps via ListByPrefix(\"\") + ParseStoreKey filter (cas/* skipped). For TB-scale shares with millions of keys this is an O(N) list. Plan 14-05's threat register (T-14-05-04) called out a per-payload-id streaming variant; deferred as a follow-up for Plan 14-07's runbook to surface if real workloads exhibit pain. The current shape matches Plan 14-03's WalkShareFiles cost profile."
  - "performCutover is idempotent (returns nil on already-cas-only). This unlocks the recovery path: an operator who hits an integrity-check pass + post-cutover legacy-delete failure can re-run the tool; the cutover is a no-op, the integrity check re-passes (CAS chunks are still there), and only the legacy delete sweep re-runs against the leftover keys."
  - "Legacy-GC partial failures log a warning + populate result.LegacyKeysDeleted but do NOT fail the command. Rationale: by the time the sweep runs, the cutover txn has already succeeded — the share is authoritative cas-only. Failing the command would force the operator to chase orphaned legacy keys via a different code path (`dfsctl store block gc` or external tooling), and there's no harm in leaving them: the dual-read shim is gone, the production read path can never reach them, they only consume S3 storage. Treating it as a hard failure would muddy the success story."
  - "Test fakes that explicitly spelled out every RemoteStore method (no embedding) needed manual HeadObject additions: prefixDeleteFailerRemote, deleteCountingRemote, concurrencyTrackingRemote, failingRemote, countingRemote, fakeRemoteStore. Embedding-based fakes (engine_health_test.go, sync_health_integration_test.go, etc.) auto-forwarded HeadObject and required no changes. Rule 3 (blocking) auto-fix — the alternative was to break the cross-package compile."

patterns-established:
  - "stubRemoteStore composition for test injection: embed memory.Store + headFn callback + concurrency counter (mu / inFlight / maxInFlight). Used across migrate_integrity_test.go and migrate_legacy_gc_test.go for HEAD/DELETE behavior swapping. Reusable by Plan 14-06 (REST status handler) when it needs a stand-in remote store."
  - "Integrity-fixture extends loopFixture: rebuild the offlineRuntime so its remote store is the stub; expose a runFullMigration helper that runs the loop and returns the unique-hash count for downstream HEAD-call assertions"

requirements-completed: [MIG-01, MIG-04]

# Metrics
duration: ~25min
completed: 2026-05-05
---

# Phase 14 Plan 05: Integrity + Cutover Summary

**`dfsctl blockstore migrate --share NAME` is now end-to-end: walk → re-chunk → HEAD-per-ref integrity check (200 + content-hash header parity) → BlockLayout flip txn → best-effort legacy-key sweep, with strict fail-loud preservation of legacy state on any integrity failure (D-A8). HeadObject lands on the public RemoteStore interface with conformance coverage.**

## Performance

- **Duration:** ~25 min single executor session
- **Started:** 2026-05-05
- **Completed:** 2026-05-05
- **Tasks:** 2 (Task 1: HeadObject + verifyIntegrity; Task 2: cutover + legacy GC + loop wiring)
- **Files modified/created:** 15 (6 new + 9 modified)

## Accomplishments

- **HeadObject on the public RemoteStore interface** (BLOCKER 1 from review). New `HeadResult{ContentLength, Metadata}` type with lowercased header keys; `HeadObject(ctx, key) (HeadResult, error)` semantically mirrors `ReadBlock` for not-found (`ErrBlockNotFound`); s3 backend uses AWS SDK `HeadObject` and defensively lowercases the metadata map; memory backend exposes the in-process map populated by `WriteBlockWithHash`.
- **`TestHeadObjectRoundTrip` conformance scenario** wired into the existing `RunSuite` entry point so every backend that runs the conformance suite (memory via unit tests, s3 via integration) automatically exercises both happy-path (200 + content-hash header) and missing-key (ErrBlockNotFound) cases.
- **`verifyIntegrity`** — walks the share's post-migration `FileAttr.Blocks`, aggregates the union-of-hashes set via the existing `migrate.WalkShareFiles` helper from Plan 14-03, then HEADs each unique CAS key through an `errgroup`-bounded parallel fleet (default `parallel=4`). Asserts both presence and `Metadata["content-hash"] == "blake3:" + hex(h)` (D-A12 parity). `ErrIntegrityCheckFailed` sentinel wraps aggregated missing-key + header-mismatch failures; transient/network errors bubble up unwrapped so the caller can distinguish "data missing" from "operator-retryable transient".
- **`performCutover`** — single-line `UpdateShareOptions` txn flipping `BlockLayout` from `legacy` to `cas-only`. Idempotent: returns nil on an already-cas-only share. Caller is responsible for ordering (integrity must pass first); the function does not re-check.
- **`deleteLegacyKeys`** — `ListByPrefix("")` + `ParseStoreKey` filter (skips `cas/*`); errgroup-bounded parallel `DeleteBlock` sweep; per-key failures aggregated into the returned error and a count of successful deletions, never abort the sweep. Best-effort by design (D-A13).
- **End-to-end loop wiring** in `runMigrateLoopWithRuntime`: integrity → cutover → legacy GC, all gated on `!opts.dryRun`. Integrity failure short-circuits via early return so `BlockLayout` stays `legacy`, the journal stays in place, legacy keys stay intact, and any uploaded CAS chunks become GC-reclaimable orphans (D-A8). Cutover failure short-circuits before legacy delete (the live read path still needs the legacy keys). Legacy-GC partial failures log a warning + populate `result.LegacyKeysDeleted` but do not fail the command (cutover txn already committed).
- **Test-fake catch-up commit** for the 6 explicit (non-embedding) RemoteStore fakes that needed HeadObject additions.

## Task Commits

1. **Task 1: HeadObject + verifyIntegrity** — `c5cf7ff5`. 6 files (4 modified + 2 created), 638 insertions. Adds the interface method, both backend implementations, the conformance scenario, the integrity helper, and 6 unit tests covering empty-share / happy-path / dedup-union / missing-key / header-mismatch / transient-error / concurrency-honors-parallel.
2. **Task 2: cutover + legacy GC + loop wiring** — `4a0bfa53`. 6 files (4 created + 2 modified), 652 insertions. Adds performCutover (4 unit tests), deleteLegacyKeys (5 unit tests), wires the post-loop pipeline into runMigrateLoopWithRuntime, and adds 3 end-to-end loop tests (happy / integrity-fail-aborts-cutover / dry-run-skips-everything).
3. **Test-fake catch-up** — `1ac7b881`. 3 files modified, 18 insertions. Adds HeadObject method to the 6 test-only RemoteStore fakes that didn't use embedding (Rule 3 — blocking).

## Verification Results

| Check | Result |
| ----- | ------ |
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `go test ./pkg/blockstore/remote/... -count=1` | PASS (memory + s3 conformance, including new TestHeadObjectRoundTrip Existing/Missing) |
| `go test ./cmd/dfsctl/commands/blockstore/ -run 'TestVerifyIntegrity' -count=1` | PASS (6 tests) |
| `go test ./cmd/dfsctl/commands/blockstore/ -run 'TestPerformCutover\|TestDeleteLegacyKeys\|TestMigrateLoop_EndToEnd\|TestMigrateLoop_IntegrityFail\|TestMigrateLoop_DryRunSkipsCutover' -count=1` | PASS (12 tests) |
| `go test ./cmd/dfsctl/commands/blockstore/ -count=1` | PASS (full migrate package, 7.7s) |
| `go test ./pkg/blockstore/migrate/ ./pkg/blockstore/remote/... -count=1` | PASS |
| `go test ./pkg/blockstore/... ./pkg/controlplane/... ./cmd/...` | PASS (full broad regression, ~110s) |
| `grep -c 'HeadObject' pkg/blockstore/remote/remote.go` | 3 (≥2 ✓) |
| `grep -c 'HeadResult' pkg/blockstore/remote/remote.go` | 4 (≥1 ✓) |
| `grep -c 'func.*HeadObject' pkg/blockstore/remote/s3/store.go` | 1 (≥1 ✓) |
| `grep -c 'func.*HeadObject' pkg/blockstore/remote/memory/store.go` | 1 (≥1 ✓) |
| `grep -c 'HeadObjectRoundTrip' pkg/blockstore/remote/remotetest/suite.go` | 3 (≥1 ✓) |
| `grep -c 'ErrIntegrityCheckFailed' cmd/dfsctl/commands/blockstore/migrate_integrity.go` | 5 (≥1 ✓) |
| `grep -c 'FormatCASKey' cmd/dfsctl/commands/blockstore/migrate_integrity.go` | 1 (≥1 ✓) |
| `grep -c 'content-hash' cmd/dfsctl/commands/blockstore/migrate_integrity.go` | 3 (≥1 ✓) |
| `grep -c 'g.SetLimit' cmd/dfsctl/commands/blockstore/migrate_integrity.go` | 1 (≥1 ✓) |
| `grep -c 'migrate\.WalkShareFiles' cmd/dfsctl/commands/blockstore/migrate_integrity.go` | 2 (≥1 ✓) |
| `grep -c 'verifyIntegrity' cmd/dfsctl/commands/blockstore/migrate_loop.go` | 1 (≥1 ✓) |
| `grep -c 'performCutover' cmd/dfsctl/commands/blockstore/migrate_loop.go` | 1 (≥1 ✓) |
| `grep -c 'deleteLegacyKeys' cmd/dfsctl/commands/blockstore/migrate_loop.go` | 1 (≥1 ✓) |
| `grep -c 'BlockLayoutCASOnly' cmd/dfsctl/commands/blockstore/migrate_cutover.go` | 1 (≥1 ✓) |
| `grep -c 'opts.dryRun' cmd/dfsctl/commands/blockstore/migrate_loop.go` | 6 (≥1 ✓) |

All grep-based acceptance criteria pass; the post-loop pipeline ordering (integrity → cutover → legacy delete with early-return on failure) is preserved as the canonical wiring.

## Files Created/Modified

### Created

- **`cmd/dfsctl/commands/blockstore/migrate_integrity.go`** — `ErrIntegrityCheckFailed` sentinel, `integrityResult` struct, `verifyIntegrity` helper.
- **`cmd/dfsctl/commands/blockstore/migrate_integrity_test.go`** — 6 unit tests + `stubRemoteStore` fixture (embeds memory.Store, exposes injectable headFn, tracks max-in-flight for concurrency assertions).
- **`cmd/dfsctl/commands/blockstore/migrate_cutover.go`** — `performCutover` helper.
- **`cmd/dfsctl/commands/blockstore/migrate_cutover_test.go`** — 4 unit tests (happy / idempotent / non-existent-share / nil-runtime).
- **`cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go`** — `deleteLegacyKeys` helper.
- **`cmd/dfsctl/commands/blockstore/migrate_legacy_gc_test.go`** — 5 unit tests + `failingDeleteRemoteStore` wrapper for partial-failure injection.

### Modified

- **`pkg/blockstore/remote/remote.go`** — Added `HeadResult` type and `HeadObject` method to `RemoteStore` interface.
- **`pkg/blockstore/remote/s3/store.go`** — `HeadObject` implementation (AWS SDK `HeadObject`, lowercase metadata map, NoSuchKey → ErrBlockNotFound).
- **`pkg/blockstore/remote/memory/store.go`** — `HeadObject` implementation (returns ContentLength + defensive copy of the in-process metadata map).
- **`pkg/blockstore/remote/remotetest/suite.go`** — Added `TestHeadObjectRoundTrip(t, factory)` exported function + sub-tests `Existing` / `Missing`; wired into `RunSuite`.
- **`cmd/dfsctl/commands/blockstore/migrate_loop.go`** — Post-loop pipeline: `verifyIntegrity` → `performCutover` → `deleteLegacyKeys` with strict fail-loud ordering. Dry-run logs "would have run" + skips all three.
- **`cmd/dfsctl/commands/blockstore/migrate_loop_test.go`** — Added `errors`, `strings`, `remote` imports; 3 end-to-end tests covering happy, integrity-fail, and dry-run-skip.
- **`pkg/blockstore/engine/gc_test.go`** — Added `HeadObject` to `prefixDeleteFailerRemote`, `deleteCountingRemote`, `concurrencyTrackingRemote` (Rule 3 fix).
- **`pkg/blockstore/engine/syncer_flush_test.go`** — Added `HeadObject` to `failingRemote`, `countingRemote` (Rule 3 fix).
- **`pkg/controlplane/runtime/blockgc_test.go`** — Added `HeadObject` to `fakeRemoteStore` (Rule 3 fix).

## Decisions Made

- **verifyIntegrity error taxonomy.** `ErrBlockNotFound` and content-hash header mismatches are *aggregated* into a `Failures []string` slice — the operator wants to see the full failure picture before triaging, not just the first one. Transient/network errors are *not* wrapped as `ErrIntegrityCheckFailed` — they bubble unwrapped via `errgroup` first-error, because they're a different failure class (operator retries; data is fine). This split mirrors the runbook's failure-mode distinction in D-A19.

- **deleteLegacyKeys uses ListByPrefix("")** rather than per-payload-id streaming. For most shares this is fine; for TB-scale shares with millions of keys it could be expensive (S3 LIST is paginated at 1000 keys/page). T-14-05-04 in the threat model documented the trade-off and authorized deferring the streaming variant to Plan 14-07's runbook if it surfaces as a real-world pain point. Deferring keeps Plan 14-05 focused on correctness over optimization.

- **performCutover does not re-check integrity.** The strict ordering is enforced by `runMigrateLoopWithRuntime`'s wiring (early return on `verifyIntegrity` error). Adding a defensive re-check inside `performCutover` would duplicate work and create a race window — the integrity check is point-in-time, and re-running it inside the cutover would not catch a hypothetical between-checks corruption. T-14-05-02 explicitly mitigates "operator deletes legacy keys before integrity check passes" via the loop-level early-return rather than per-helper re-checking.

- **Legacy-GC partial failures log a warning, do not fail the command.** Discussed in `key-decisions` above: by the time the sweep runs, the cutover txn has already succeeded; the operator's primary success criterion (share is cas-only and serves CAS reads) is met. Treating partial sweep failure as a hard exit would punish operators for a non-fatal cleanup issue.

- **Test-fake updates as a separate commit (`1ac7b881`).** The fake updates are mechanical Rule-3 plumbing — splitting them out keeps the Task 1 / Task 2 commits focused on the actual feature work and makes the diff easier to review (and to revert if the interface ever changes again). Aligned with Plan 14-04's approach of keeping mechanical plumbing distinct from semantic changes.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 — Blocking] Test-only RemoteStore fakes outside this plan's `<files>` list needed HeadObject method additions.**

- **Found during:** End-of-Task-2 `go vet ./...` cross-package compile check.
- **Issue:** Adding `HeadObject` to the public `RemoteStore` interface (Task 1) is the textbook Liskov-substitution change. Six test fakes across three packages spell out every method explicitly (no embedding) and therefore broke at compile time. The plan's `<files_modified>` listed only the interface + s3 + memory + remotetest paths; the test-fake catch-up was implicit in "go build ./... succeeds" but not enumerated.
- **Fix:** Added `HeadObject` to `prefixDeleteFailerRemote`, `deleteCountingRemote`, `concurrencyTrackingRemote` (gc_test.go); `failingRemote`, `countingRemote` (syncer_flush_test.go); `fakeRemoteStore` (blockgc_test.go). Each implementation forwards to the inner store's HeadObject — these fakes are pass-throughs for the methods they don't intercept.
- **Files modified:** `pkg/blockstore/engine/gc_test.go`, `pkg/blockstore/engine/syncer_flush_test.go`, `pkg/controlplane/runtime/blockgc_test.go`.
- **Verification:** `go vet ./...` clean; `go test ./pkg/blockstore/... ./pkg/controlplane/...` clean.
- **Committed in:** `1ac7b881`.

---

**Total deviations:** 1 (Rule 3 mechanical plumbing for cross-package compile)
**Impact on plan:** Necessary for `go build ./...` and `go vet ./...` to pass. No scope creep — same change every existing fake already absorbed for prior interface evolutions (e.g., `Healthcheck` lowercase added in Phase 11, `WriteBlockWithHash` added in BSCAS-06).

## Issues Encountered

- **None blocking.** The s3 backend `HeadObject` was a straight AWS SDK port; the memory backend was a 12-line wrapper around the existing metadata map; the conformance scenario fit the established `RunSuite` pattern. The post-loop pipeline integrated cleanly into Plan 14-04's two-phase walk-then-dispatch shape.

- **Pre-existing arm64 BLAKE3-vs-SHA256 perf flake (`TestBLAKE3FasterThanSHA256` in `pkg/blockstore`).** Unchanged from Plan 14-03 / 14-04. Not tracked under this plan; the broad regression run (`go test ./pkg/blockstore/... ./pkg/controlplane/... ./cmd/...`) was clean on the second run after the gc_test fakes were patched.

## Threat Surface Notes

The plan's `<threat_model>` covered five threats. Status:

- **T-14-05-01 (Tampering — integrity passes for tampered blocks):** mitigated. The header parity check (x-amz-meta-content-hash vs blake3:{hex} derived from the key path) is verified in every HEAD response. The hash-of-key invariant means tampering with bytes is caught on the next ReadBlockVerified call (Phase 11 BSCAS-06 contract).
- **T-14-05-02 (Tampering — operator deletes legacy keys before integrity):** mitigated. The sequencing is enforced by `runMigrateLoopWithRuntime`'s early-return on integrity failure; `TestMigrateLoop_IntegrityFail` asserts that no legacy keys are deleted when integrity fails (the test sets up legacy data, injects a 404 HEAD, runs the loop, then asserts both the BlockLayout stays legacy and the legacy keys are still listable).
- **T-14-05-03 (Information disclosure — logging full ContentHash):** accepted. Hashes are blake3 of public file content, not secrets. The error messages include the full CAS key (e.g., `cas/93/83/938390e9...`) for triage; the audit trail is operator-visible by design.
- **T-14-05-04 (DoS — legacy delete enumerates a giant share):** mitigated as documented. Per-payload-id streaming variant deferred to Plan 14-07's runbook to surface if a real workload exhibits the pain.
- **T-14-05-05 (Repudiation — uncaught panic between integrity-pass and cutover):** mitigated. All three steps run in the same process invocation; the dfsctl root catches panics and logs partial-state. Operator re-runs; cutover is idempotent (`TestPerformCutover_Idempotent`), integrity check is read-only.

## Threat Flags

None — no new security-relevant surface beyond what the plan's threat register already covered. `HeadObject` is a read-only metadata fetch with the same auth + transport plumbing as `ReadBlockVerified`.

## Known Stubs

- **`openOfflineRuntime` continues to return `ErrOfflineRuntimeNotWired`** (carried forward from Plan 14-03 / 14-04). The unit-test path (`newTestOfflineRuntime`) fully exercises the integrity → cutover → legacy GC pipeline against memory fixtures; production controlplane wire-up is the remaining piece before the runbook in Plan 14-07 can run end-to-end. The plan-level prompt explicitly authorized leaving this as-is for Plan 14-05 and noted the runbook will pick it up.

## Next Phase Readiness

- **Plan 14-06 (REST status):** unaffected by this plan; consumes `Journal.OpenJournalReadOnly` + `Journal.Aggregate()` from Plan 14-03. Could optionally surface `verifyIntegrity` results in the status response, but that's a follow-up — the plan's scope is read-only journal aggregation.
- **Plan 14-07 (docs runbook):** picks up the full `dfsctl blockstore migrate --share NAME` story end-to-end now that the integrity → cutover → legacy GC pipeline is wired. The four worked transcripts in D-A19 will exercise the full path. Two prerequisites remain:
  1. **`openOfflineRuntime` production wiring** — controlplane DB read of `BlockStoreConfigProvider`, per-share metadata + remote store factory dispatch, remote ref-counting. The interfaces are stable; the work is a focused composition pass against `pkg/controlplane/runtime/shares` + `pkg/metadata/store/{memory,badger,postgres}`.
  2. **Per-payload-id streaming variant of `deleteLegacyKeys`** — only if Plan 14-07's transcripts surface S3 LIST cost as an issue at TB scale (T-14-05-04).

## Self-Check: PASSED

- [x] `pkg/blockstore/remote/remote.go` contains `HeadResult` + `HeadObject` declarations — verified.
- [x] `pkg/blockstore/remote/s3/store.go` and `pkg/blockstore/remote/memory/store.go` contain `func (.*) HeadObject(...)` implementations — verified.
- [x] `pkg/blockstore/remote/remotetest/suite.go` contains `TestHeadObjectRoundTrip` (3 references) — verified.
- [x] `cmd/dfsctl/commands/blockstore/migrate_integrity.go` contains `ErrIntegrityCheckFailed`, `verifyIntegrity`, `migrate.WalkShareFiles`, `g.SetLimit`, `FormatCASKey`, `content-hash` — verified by grep.
- [x] `cmd/dfsctl/commands/blockstore/migrate_cutover.go` contains `BlockLayoutCASOnly` — verified.
- [x] `cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go` contains `FormatStoreKey`/`ParseStoreKey` filter and errgroup — verified by source.
- [x] `cmd/dfsctl/commands/blockstore/migrate_loop.go` calls `verifyIntegrity` → `performCutover` → `deleteLegacyKeys` in that order, with early-return on integrity failure — verified by source review.
- [x] Commit `c5cf7ff5` (Task 1) reachable via `git log` — verified.
- [x] Commit `4a0bfa53` (Task 2) reachable via `git log` — verified.
- [x] Commit `1ac7b881` (test-fake catch-up) reachable via `git log` — verified.
- [x] All 21 new tests + 2 conformance scenarios + 19 prior tests in cmd/dfsctl/commands/blockstore green; `go vet ./...` clean; `go build ./...` clean; full broad regression (~110s) green.

---
*Phase: 14-migration-tool-a5*
*Completed: 2026-05-05*
