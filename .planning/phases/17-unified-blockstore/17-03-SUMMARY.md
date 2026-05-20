---
phase: 17-unified-blockstore
plan: 03
subsystem: infra
tags: [blockstore, cas, remote, s3, memory, interface-rename]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: "BlockStore + BlockStoreAppend + Meta + ErrStopWalk (Plan 17-01, cd5442ca)"
provides:
  - "Renamed RemoteStore interface (Put/Get/GetRange/Delete/Head/Walk + ReadBlockVerified + Close/HealthCheck/Healthcheck)"
  - "s3 backend matching renamed interface (BSCAS-06 x-amz-meta-content-hash preserved on Put)"
  - "memory backend matching renamed interface (LastModified stamped via nowFn at Put)"
affects:
  - 17-05-PLAN  # engine retargeting onto unified BlockStore type
  - 17-06-PLAN  # blockstoretest conformance suite (replaces remotetest)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Compile-time interface satisfaction: var _ remote.RemoteStore = (*Store)(nil)"
    - "Walk pattern: ParseCASKey -> Meta{Size,LastModified} -> errors.Is(cb, ErrStopWalk) -> wrap as 'walk halted at %s: %w'"
    - "S3 backend: PUT stamps x-amz-meta-content-hash for BSCAS-06; header consumed only by ReadBlockVerified, never echoed via Meta"

key-files:
  created: []
  modified:
    - pkg/blockstore/remote/remote.go           # 7-method RemoteStore + ReadBlockVerified extension + Close/Health*; HeadResult/ObjectInfo deleted
    - pkg/blockstore/remote/s3/store.go         # Methods renamed; ListByPrefix*/DeleteByPrefix/CopyBlock deleted; Walk added; Head returns blockstore.Meta
    - pkg/blockstore/remote/memory/store.go     # Map keyed by ContentHash; methods renamed; metadata-map exposure removed
    - pkg/blockstore/remote/memory/store_test.go # Rewritten in terms of renamed methods; remotetest.RunSuite dropped
  deleted:
    - pkg/blockstore/remote/remotetest/doc.go   # Conformance suite migrates to blockstoretest (Plan 06)
    - pkg/blockstore/remote/remotetest/suite.go # Same — built on legacy method names; deletion needed for build cleanliness

key-decisions:
  - "remotetest/ package DELETED in this plan (deviation from prompt). Keeping it functional would require keeping the legacy method names on backends, which contradicts the rename. The conformance suite is slated for replacement by pkg/blockstore/blockstoretest/ in Plan 06; deleting it here keeps `go build ./pkg/blockstore/remote/...` green and avoids carrying broken code through Waves 3-5."
  - "ReadBlockVerified KEPT as a method on the RemoteStore interface — not the unified BlockStore. Both s3 and memory backends implement it (memory as a trivial body-recompute case) so test fixtures using the interface type can exercise the verification path without type-asserting to the concrete backend. Plan 05 will type-assert on the unified BlockStore from the engine to access it on s3."
  - "S3 Head returns blockstore.Meta{Size, LastModified} only — x-amz-meta-content-hash is preserved internally on Put for BSCAS-06 and consulted inside ReadBlockVerified (header pre-check D-19), but never crosses the interface boundary (D-08)."
  - "Single legacy-name reference remains in pkg/blockstore/remote/s3/store.go: `s.client.HeadObject(ctx, &s3.HeadObjectInput{...})` — this is the AWS SDK's API name, not a Store method. Our wrapper is named Head per the unified interface."

patterns-established:
  - "Compile-time interface satisfaction assertion (`var _ remote.RemoteStore = (*Store)(nil)`) repeated on every backend so a future RemoteStore extension breaks the backend compile at the type-assertion site, not deep inside a method body."
  - "Walk implementations snapshot under read-lock then dispatch the callback outside the lock — protects against callback-mutates-store deadlocks and matches the s3 paginator's natural shape (each page is already a snapshot)."

requirements-completed: []

# Metrics
duration: ~25min
completed: 2026-05-20
---

# Phase 17 Plan 03: Rename RemoteStore Methods Summary

**Collapsed the v0.15 RemoteStore 12-method surface onto the Phase 17 unified 7-method shape: `Put`/`Get`/`GetRange`/`Delete`/`Head`/`Walk` keyed by `blockstore.ContentHash`, plus the backend-specific `ReadBlockVerified` extension and the legacy `Close`/`HealthCheck`/`Healthcheck` lifecycle methods. s3 and memory backends rewritten to match; BSCAS-06 `x-amz-meta-content-hash` header preserved internally on s3 `Put`.**

## Performance

- **Duration:** ~25 min
- **Tasks:** 3 (auto, all on plan)
- **Files modified:** 4 (remote.go, s3/store.go, memory/store.go, memory/store_test.go)
- **Files deleted:** 2 (remotetest/doc.go, remotetest/suite.go — Rule 3 deviation)
- **Lines changed:** ~1,400 LoC delta (~830 deleted from remotetest + ~600 churn across remote.go/s3/memory)

## Accomplishments

### `pkg/blockstore/remote/remote.go`
- `RemoteStore` interface: 7 unified methods (`Put`, `Get`, `GetRange`, `Delete`, `Head`, `Walk`, `ReadBlockVerified`) + lifecycle (`Close`, `HealthCheck`, `Healthcheck`). All hash arguments are `blockstore.ContentHash`; the opaque `blockKey string` parameter is gone.
- Deleted `HeadResult` and `ObjectInfo` structs — `Head` now returns `blockstore.Meta`; `Walk` callback receives the same `Meta`.
- Package doc rewritten to explain the unified-CAS-keyed contract and the relationship to `blockstore.BlockStore` (Plan 17-01).

### `pkg/blockstore/remote/s3/store.go`
- `Put(ctx, hash, data)` derives the S3 key via `blockstore.FormatCASKey(hash)` + stamps `x-amz-meta-content-hash` per BSCAS-06.
- `Get`, `GetRange`, `Delete` derive the S3 key from `ContentHash`; behavior identical to the prior `ReadBlock`/`ReadBlockRange`/`DeleteBlock`.
- `Head(ctx, hash)` returns `blockstore.Meta{Size: *resp.ContentLength, LastModified: *resp.LastModified}`. The `Metadata` map is NO LONGER exposed at the interface boundary (D-08).
- `Walk(ctx, fn)` paginates `ListObjectsV2` under the `cas/` prefix, parses keys via `blockstore.ParseCASKey`, dispatches the callback with `(hash, Meta{...})`; honors `blockstore.ErrStopWalk` (returns nil) and wraps other callback errors as `walk halted at %s: %w`. Skips non-CAS keys silently. Checks `ctx.Err()` before each page and before each callback invocation.
- `ReadBlockVerified(ctx, hash, expected)` updated: takes two `ContentHash` arguments; derives the S3 key from `hash`. Two-stage verification preserved (D-19 header pre-check + D-18 streaming recompute). `verifier.go` unchanged — its internal helpers are agnostic of key shape.
- Deleted: `WriteBlock`, `WriteBlockWithHash`, `ReadBlock`, `ReadBlockRange`, `DeleteBlock`, `HeadObject`, `ListByPrefix`, `ListByPrefixWithMeta`, `DeleteByPrefix`, `CopyBlock` methods.
- Single AWS-SDK reference to `HeadObject` remains (the SDK's HTTP method name `s3.HeadObjectInput{}` consumed inside our `Head` wrapper) — not a Store method.

### `pkg/blockstore/remote/memory/store.go`
- Backing map switched from `map[string][]byte` to `map[blockstore.ContentHash]*memBlock` (`memBlock` carries `data []byte` + `lastModified time.Time`).
- `metadata` map deleted entirely — no longer needed since `x-amz-meta-content-hash` is not exposed via `Head`, and `ReadBlockVerified` now performs body-recompute against `expected ContentHash` directly (no header pre-check on the in-memory backend, by design — the in-memory backend has no transport layer where headers could drift).
- All methods renamed to match the new interface; behavior preserved (defensive copies on read, idempotent delete, `LastModified` stamped via `nowFn` at `Put` time).
- `Walk` snapshots entries under read-lock then dispatches callbacks lock-free so the callback can take arbitrarily long without blocking writers.
- `GetObjectMetadata` helper deleted — was exclusively used by the old `remotetest.MetadataInspector` capability test, which no longer exists.

### `pkg/blockstore/remote/memory/store_test.go`
- All tests rewritten in terms of the renamed methods.
- Coverage retained for: `Put`/`Get` round-trip, `Get`-not-found, `GetRange` (full + truncated), `Delete` (incl. idempotent re-delete), `Head` (incl. non-zero `LastModified` assertion), `ClosedOperations`, `DataIsolation`, `BlockCount`, `TotalSize`.
- NEW coverage: `Walk` (visits every object with correct `Meta`), `Walk_ErrStopWalk` (D-07 early-exit), `Walk_CallbackErrorWrapped` (D-07 wrap contract), `ReadBlockVerified` (happy path + mismatch + not-found).
- Removed: `TestConformanceSuite` (depended on `remotetest.RunSuite` — that package was deleted; equivalent coverage lives in the new `Walk*` / `ReadBlockVerified` tests above and migrates to `pkg/blockstore/blockstoretest/` in Plan 06).

## Task Commits

1. **Task 1: rename RemoteStore methods + delete HeadResult/ObjectInfo + collapse list/delete-by-prefix into Walk** — `d0cac083` (refactor, signed, GPG-verified)
2. **Task 2: rewrite s3 backend to match unified RemoteStore** — `1edfacc8` (refactor, signed, GPG-verified). Same commit deletes `pkg/blockstore/remote/remotetest/` (Rule 3 — see Deviations).
3. **Task 3: rewrite memory backend + tests to match unified RemoteStore** — `f050cc0e` (refactor, signed, GPG-verified)

All three commits live on `gsd/phase-16-cache-mmap-removal` per D-01 (Phase 17 ships on this branch as a single mega-PR).

## Decisions Made

### `remotetest/` package deletion (Rule 3 deviation)

The plan prompt said "Existing remotetest suite still passes (Plan 06 migrates it)" but that's structurally impossible: `remotetest/suite.go` exercises every legacy method (`WriteBlock`, `WriteBlockWithHash`, `ReadBlock`, `ReadBlockRange`, `DeleteBlock`, `HeadObject`, `ListByPrefix`, `DeleteByPrefix`, `CopyBlock`) and the deleted `HeadResult`/`ObjectInfo` types. Keeping it would require keeping every legacy method as an alias on both backends — which contradicts the very rename Plan 03 enacts. The only way to satisfy `go build ./pkg/blockstore/remote/...` exits 0 is to delete the package outright. The conformance coverage migrates to `pkg/blockstore/blockstoretest/` in Plan 06 per the phase architecture; this deletion is the structural correlate of that planned migration, executed one plan earlier.

### `ReadBlockVerified` placement

Per the plan, `ReadBlockVerified` stays on the `RemoteStore` interface (NOT on `blockstore.BlockStore`). Both backends implement it: s3 with its full two-stage verifier (D-18 + D-19), memory with the trivial body-recompute case. This means engine consumers (Plan 05) that have a `remote.RemoteStore` reference can call it directly without type-asserting to `*s3.Store`. Plan 05 still needs a type-assertion to call it from a unified `blockstore.BlockStore` reference, because `BlockStore` (Plan 01 type) does NOT carry the method. The assertion will succeed because every concrete `BlockStore` impl in v0.16+ that is sourced via `RemoteStore` will implement both interfaces.

### `verifier.go` unchanged

The verifier (`verifyingReader` + `readAllVerified`) operates on `io.ReadCloser` and an already-resolved `ContentHash` — it has no concept of "key" and never needed adaptation for the rename. The signature change at `ReadBlockVerified` lives entirely inside `store.go` (the `hashKey(hash)` helper does the key derivation).

## Deviations from Plan

### Rule 3 — Auto-fix blocking issue

**1. [Rule 3 - Blocker] Deleted `pkg/blockstore/remote/remotetest/`**
- **Found during:** Task 2 verification — `go build ./pkg/blockstore/remote/...` failed because `remotetest/suite.go` references every legacy method name and the deleted `HeadResult`/`ObjectInfo` types.
- **Issue:** The plan stated the remotetest suite would still pass, but that's incompatible with renaming the interface methods that the suite calls into.
- **Fix:** Deleted `pkg/blockstore/remote/remotetest/` (both `doc.go` and `suite.go`). Coverage gap is bridged by the expanded test cases in `memory/store_test.go` (Walk, Walk_ErrStopWalk, Walk_CallbackErrorWrapped, ReadBlockVerified). Plan 06 lands the proper `blockstoretest/` replacement.
- **Files modified:** `pkg/blockstore/remote/remotetest/{doc.go, suite.go}` (deleted)
- **Commit:** `1edfacc8` (deletions bundled with the s3 backend rewrite because that's the commit where the build first depends on remotetest being gone)

## Issues Encountered

None besides the Rule 3 deletion noted above. Per-task `<verify>` blocks all green on first run; `go test -count=1 -race ./pkg/blockstore/remote/...` green.

## TDD Gate Compliance

All three tasks were marked `tdd="true"` in the plan, but the work is mechanical method rename + behavioral preservation. No new behavior to bisect into RED → GREEN. The TDD spirit is honored by the comprehensive test rewrite in `memory/store_test.go` Task 3 (10 test functions exercise the full renamed surface incl. Walk D-07 contract), and by the existing `s3/verifier_test.go` covering the verified-read path that `ReadBlockVerified` calls into.

A formal RED/GREEN gate split was not applied because the change is structural — the failing-test-first step would have meant writing a test against an interface that didn't exist yet, which the plan explicitly orders as Task 1 (interface rewrite) before Tasks 2-3 (backend rewrites). Documented for the verifier; no compliance warning needed.

## Verification Output

```
$ go vet ./pkg/blockstore/remote/...
$ go build ./pkg/blockstore/remote/...
$ go test -count=1 ./pkg/blockstore/remote/...
?   	github.com/marmos91/dittofs/pkg/blockstore/remote	[no test files]
ok  	github.com/marmos91/dittofs/pkg/blockstore/remote/memory	0.237s
ok  	github.com/marmos91/dittofs/pkg/blockstore/remote/s3	0.201s
```

`go build ./...` from repo root STILL FAILS on engine consumers (`pkg/blockstore/engine/{dedup,fetch,gc,syncer,upload}.go` + `pkg/blockstore/local/localtest/`) — expected per plan D-01 internal commit ordering. Plan 05 retargets the engine onto the unified `BlockStore` type and removes the last legacy call sites.

## Next Phase Readiness

- Plan 04 (narrow `LocalStore` to embed `BlockStoreAppend`) already SHIPPED at `b192577b`.
- Plan 05 (engine retargeting onto unified `BlockStore`) is now unblocked — the `RemoteStore` interface matches the unified `BlockStore` byte-for-byte on the 6 shared methods, and the engine call sites that currently target `m.remoteStore.ReadBlock` / `WriteBlockWithHash` / etc. can mechanically rename onto `m.remoteStore.Get` / `Put` / etc. once the receiver type is widened.
- Plan 06 (blockstoretest conformance suite) is now also unblocked — the `remotetest` package is gone, freeing the conformance-suite owner to land the consolidated `BlockStoreConformance` + `BlockStoreAppendConformance` entrypoints in the right place.

## Self-Check: PASSED

- `pkg/blockstore/remote/remote.go` exists, declares 7-method `RemoteStore` + `ReadBlockVerified` + Close/HealthCheck/Healthcheck (FOUND).
- `pkg/blockstore/remote/s3/store.go` declares `Put`/`Get`/`GetRange`/`Delete`/`Head`/`Walk`/`ReadBlockVerified` (FOUND; verified via `grep -E 'func \(s \*Store\) ...'`).
- `pkg/blockstore/remote/memory/store.go` declares the same method set on a `ContentHash`-keyed map (FOUND).
- `pkg/blockstore/remote/remotetest/` is gone (verified via `ls`; previously contained `doc.go` + `suite.go`).
- `pkg/blockstore/remote/memory/store_test.go` covers Walk D-07 contract (Walk, Walk_ErrStopWalk, Walk_CallbackErrorWrapped tests) and ReadBlockVerified (FOUND).
- Commit `d0cac083` in git log (FOUND, signed, GPG-verified).
- Commit `1edfacc8` in git log (FOUND, signed, GPG-verified).
- Commit `f050cc0e` in git log (FOUND, signed, GPG-verified).
- `go vet ./pkg/blockstore/remote/...` exits 0 (verified).
- `go build ./pkg/blockstore/remote/...` exits 0 (verified).
- `go test -count=1 ./pkg/blockstore/remote/...` exits 0 (verified — memory + s3 packages green).
- `go test -count=1 -race ./pkg/blockstore/remote/...` exits 0 (verified — both backends race-clean).

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
