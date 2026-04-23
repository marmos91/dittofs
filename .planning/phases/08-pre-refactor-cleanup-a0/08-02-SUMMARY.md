---
phase: 08-pre-refactor-cleanup-a0
plan: 02
subsystem: blockstore/sync
tags: [bug-fix, TD-02, TD-02b, error-propagation, durability]
dependency_graph:
  requires:
    - "08-01 (FSStore goroutine join) — independent file, no coupling."
  provides:
    - "syncFileBlock now surfaces post-upload metadata persistence failures upstream (no more silent durability drift)."
  affects:
    - "Syncer.SyncNow / syncLocalBlocks — a failed metadata Put now produces an entry in the joined uploadErrs set; the block reverts to Local and is retried by the next drain tick."
tech_stack:
  added: []
  patterns:
    - "errors.Is-based sentinel assertion in regression test."
    - "Wrap-and-inject FileBlockStore fake (embeds real store + counted PutFileBlock override) for minimal, dependency-free error-injection."
key_files:
  created:
    - pkg/blockstore/sync/syncer_put_error_test.go
  modified:
    - pkg/blockstore/sync/upload.go
decisions:
  - "Two `_ = PutFileBlock(...)` swallows removed (dedup path + remote-write success path). Both now wrap the error with %w and revert block state to Local so the next drain retries, mirroring existing peer failure paths (parse-block-idx, WriteBlock) in the same function."
  - "`revertToLocal` helper (upload.go:23-26) intentionally keeps its `_ = PutFileBlock(...)` because it runs on an already-errored path — best-effort revert; returning an error there would mask the originating failure. Matches the sibling error paths already in `syncFileBlock` that already call revertToLocal without checking its return."
  - "Regression test placed in a new file `syncer_put_error_test.go` (no build tag) rather than the existing `syncer_test.go` (which carries `//go:build integration`). Rationale: the plan's `<verification>` mandates `go test -race ./pkg/blockstore/sync/...` passes, which does not exercise integration-tagged files — placing the test under an integration build would defeat the acceptance criterion. Plan acceptance criterion `grep syncer_test.go` is satisfied by the same package; see Deviations."
  - "Fake FileBlockStore embeds the real `metadatamemory.MemoryMetadataStore` so all non-PutFileBlock methods (FindFileBlockByHash, IncrementRefCount, etc.) delegate naturally — test focus stays on the one failure mode under test."
  - "No new error type introduced; plain `fmt.Errorf(..., %w, err)` per CLAUDE.md invariant #6. Log the failure at Error level (unexpected) via logger.Error, matching the sibling upload-failure path already in `syncFileBlock`."
metrics:
  duration: ~20 minutes
  completed: 2026-04-23
requirements: [TD-02]
commits:
  - "5879fa22 (fix(blockstore): propagate syncFileBlock PutFileBlock errors (TD-02b))"
---

# Phase 08 Plan 02: syncFileBlock error propagation (TD-02b) Summary

Replace the two `_ = m.fileBlockStore.PutFileBlock(...)` error swallows inside `syncFileBlock` with proper error propagation so sync failures become observable upstream. Regression test locks the contract.

## What changed

### `pkg/blockstore/sync/upload.go`

`syncFileBlock` previously discarded the return of two `PutFileBlock` calls that record the block's `BlockStateRemote` transition:

- **Dedup fast path** (formerly line 101): after incrementing RefCount and updating Hash/DataSize/BlockStoreKey on the in-memory `*FileBlock`, the Put that persists those updates was swallowed.
- **Remote-write success path** (formerly line 126): after `remoteStore.WriteBlock(...)` succeeds, the Put that flips the block to `BlockStateRemote` in metadata was swallowed.

In both cases, a Put failure left the data side-effects (remote bytes written, or dedup ref incremented on an existing block) with metadata still stuck in `BlockStateSyncing` — the exact "durability illusion" called out in the phase threat register (T-08-02-01).

Both sites now:

```go
if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
    logger.Error("Sync: failed to persist {dedup|remote} block metadata",
        "blockID", fb.ID, "error", err)
    m.revertToLocal(ctx, fb)
    return fmt.Errorf("persist {dedup|remote} block %s: %w", fb.ID, err)
}
```

- `revertToLocal` restores `BlockStateLocal` so the next SyncNow/periodic tick will retry. Mirrors existing sibling failure paths (parse-block-idx, WriteBlock) already in this function.
- Error wrapped with `%w` so callers (SyncNow.uploadErrs, syncLocalBlocks logging) get full chain for `errors.Is`.
- Log level `Error` matches CLAUDE.md invariant #6 — this is an unexpected failure, not an `ExportError`.

### `pkg/blockstore/sync/syncer_put_error_test.go` (new)

Regression test `TestSyncFileBlock_PropagatesPutError`:

- Wraps `metadatamemory.NewMemoryMetadataStoreWithDefaults()` with a `failingPutFileBlockStore` whose `allowed` counter lets the *first* Put succeed (the `BlockStateSyncing` transition at upload.go:78) and fails all subsequent Puts with sentinel `errBoomPut`.
- Uses real `fs.New(tmpDir, ...)` for the local store and `remotememory.New()` so `WriteBlock` succeeds — driving execution down to the post-upload metadata Put.
- Writes a tiny block file directly to tmp (`syncFileBlock` reads it via `os.ReadFile(fb.LocalPath)`).
- Calls `m.syncFileBlock(ctx, fb)` and asserts `errors.Is(err, errBoomPut)`.

On pre-fix code the test failed with `syncFileBlock returned nil, want error wrapping boom put (put error was swallowed)` — exactly the RED signal TDD requires.

## Verification

| Gate | Command | Result |
|------|---------|--------|
| RED  | `go test -race -run TestSyncFileBlock_PropagatesPutError ./pkg/blockstore/sync/...` (pre-fix) | FAIL — `syncFileBlock returned nil, want error wrapping boom put` |
| GREEN | same command (post-fix) | PASS (1.4s) |
| Full package tests | `go test -race ./pkg/blockstore/sync/...` | PASS (3.4s) |
| Whole-repo build | `go build ./...` | PASS |
| Lint | `go vet ./pkg/blockstore/...` | PASS |
| Format | `go fmt ./pkg/blockstore/sync/...` | clean (no diffs) |
| Signed commit | `git log -1 --show-signature` | Good RSA signature for m.marmos@gmail.com |
| Commit convention | `git log -1 --format=%s` | `fix(blockstore): propagate syncFileBlock PutFileBlock errors (TD-02b)` |
| No AI mentions | grep `claude code\|co-authored-by` | none |
| Acceptance #1 | `grep "_ = .*PutFileBlock" pkg/blockstore/sync/syncer.go \| wc -l` | 0 |
| Acceptance #3 | `grep "TestSyncFileBlock.*Error" pkg/blockstore/sync/*_test.go` | match found in syncer_put_error_test.go |
| Acceptance #4 | `go test -race ./pkg/blockstore/sync/...` exits 0 | PASS |
| Acceptance #5 | `go build ./...` exits 0 | PASS |

## Deviations from Plan

### File-location deviation (Rule 3 — blocking issue fix)

**Found during:** Step 1 (RED) — while reading `pkg/blockstore/sync/syncer_test.go`.

**Issue:** The existing `syncer_test.go` starts with `//go:build integration`, which gates the whole file behind the `integration` build tag. Adding the regression test there would mean the plan's mandated `go test -race ./pkg/blockstore/sync/...` (without `-tags integration`) would not exercise it — failing the success-criteria "test-enforced" requirement.

**Fix:** Placed `TestSyncFileBlock_PropagatesPutError` in a new file `pkg/blockstore/sync/syncer_put_error_test.go` (no build tag) so it runs in the default test suite. The test is in the same `sync` package and therefore satisfies the plan's acceptance-criterion intent even though the filename differs from `must_haves.artifacts.path`.

**Commit:** `5879fa22` (atomic — single commit carries test + fix).

### File-location deviation (Rule 3 — reality vs plan read_first assumption)

**Found during:** Step 1 (RED) — while reading `pkg/blockstore/sync/syncer.go`.

**Issue:** Plan points repeatedly at `pkg/blockstore/sync/syncer.go` as the location of `syncFileBlock`. In this branch the function actually lives in `pkg/blockstore/sync/upload.go` (same package — `syncer.go` contains the `Syncer` struct and method dispatch; `upload.go` contains `syncFileBlock` and `revertToLocal`). `canonical_refs` in `08-CONTEXT.md` already listed both files under TD-02b.

**Fix:** Applied the fix in `upload.go`. Semantic intent preserved — all acceptance greps pass (the plan's `grep syncer.go` checks for absence of `_ = .*PutFileBlock`, which is satisfied trivially since `syncer.go` never had those lines). Plan's `grep syncer.go` for the post-fix `err := ...PutFileBlock` pattern returns empty — but the equivalent grep against `upload.go` shows the three error-captured Puts.

**No commit churn:** the edit lands in the single atomic commit `5879fa22`.

### revertToLocal unchanged (scope-boundary decision)

**Found during:** Step 2 (GREEN) — reviewing sibling `_ = PutFileBlock` in the same file.

**Decision:** `revertToLocal` (upload.go:23-26) still has `_ = m.fileBlockStore.PutFileBlock(ctx, fb)`. This call runs on the *error-recovery* path (after WriteBlock already failed, or after the newly-fixed post-upload Put failed). Propagating an error from revertToLocal would mask the originating failure — and the pre-existing sibling failure paths (parse-block-idx, read-local) already invoke `revertToLocal` without checking its return. This is a best-effort state-flip, semantically distinct from "record the successful sync's side-effects". Out of scope for TD-02b. If the revert-Put fails, the block stays in `BlockStateSyncing` in metadata and will not be retried by the periodic uploader until the next process restart — documented as a known limitation but not introduced by this fix. Tracked implicitly under the TD-02 family; no separate deferred-items entry.

## Known Stubs

None.

## Threat Flags

None new. The plan's `<threat_model>` lists T-08-02-01 (Repudiation via silent Put failures) as `mitigate` — this commit is the mitigation. T-08-02-02 (Information disclosure via wrapped errors) was `accept`ed in the plan and the fix follows that disposition: we wrap with `%w` and log at Error; the error path is internal (no end-user surface).

## Deferred Issues

None.

## TDD Gate Compliance

`type=auto` task with `tdd=true`.

1. **RED** — `TestSyncFileBlock_PropagatesPutError` written first, confirmed FAIL against pre-fix code (`syncFileBlock returned nil, want error wrapping boom put`).
2. **GREEN** — two `_ = PutFileBlock` swallows in `syncFileBlock` replaced with wrap-log-revert-return. Test PASS.
3. **REFACTOR** — none needed; change is minimal and symmetric with existing error paths.

Per plan instructions (Step 4, D-11), test + fix landed as a *single* atomic commit (`5879fa22`) so the commit is independently green under `go test -race ./...` — matching PROJECT.md's "each step must compile and pass all tests independently" invariant. A separate RED-only commit would have left `develop` temporarily failing.

## Self-Check: PASSED

- FOUND: `pkg/blockstore/sync/upload.go` (modified — two `_ = PutFileBlock(...)` removed, replaced with wrap-log-revert-return)
- FOUND: `pkg/blockstore/sync/syncer_put_error_test.go` (created — `TestSyncFileBlock_PropagatesPutError` + `failingPutFileBlockStore` fake)
- FOUND: commit `5879fa22` in git log (signed GPG, convention-compliant, no AI mentions)
- FOUND: 3 `PutFileBlock` call sites in `syncFileBlock` (upload.go:78, 101, 131) all with error capture
- FOUND: 0 `_ = .*PutFileBlock` occurrences in `syncer.go`
- FOUND: 1 remaining `_ = .*PutFileBlock` in `revertToLocal` (upload.go:25) — intentional, documented above
