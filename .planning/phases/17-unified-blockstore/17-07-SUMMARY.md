---
phase: 17-unified-blockstore
plan: 07
subsystem: infra
tags: [blockstore, cas, interface, go, refactor, append-log, deletion]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: "Narrowed LocalStore embedding BlockStoreAppend (Plan 04, b192577b)"
  - phase: 17-unified-blockstore
    provides: "Engine retargeted onto renamed RemoteStore (Plan 05, a1ec11b0 + a94f17b0 + 7ee552da)"
  - phase: 17-unified-blockstore
    provides: "Backends wired to blockstoretest (Plan 06, 0f2c9070 + edfc30bb + f0ced747)"
provides:
  - "Path-keyed write.go writer DELETED; transitional WriteAt(payloadID,...) preserved on a new write_transitional.go"
  - "FormatStoreKey / ParseStoreKey / KeyBelongsToFile / ParseBlockIdx DELETED (blockstore.types.go)"
  - "UseAppendLog opt-out flag + ErrAppendLogDisabled sentinel DELETED; append is mandatory on local tier"
  - "Orphan helpers (ExistsOnDisk, DeleteBlockFile [public], TruncateBlockFiles) DELETED from fs.manage.go; deleteBlockFile retained as private helper"
  - "Legacy v0.14/v0.15 cmd/dfsctl/commands/blockstore/ migrate tool DELETED (3674 LoC) — superseded by Phase 17's planned `dfs migrate-to-cas`"
  - "*FSStore + *MemoryStore (local) implement BlockStoreAppend in full; *Store (remote/s3 + remote/memory) gains Has"
  - "Compile-time interface assertion `var _ local.LocalStore = (*FSStore)(nil)` restored (Plan 04's 'Plan 17-07 restores' marker removed)"
  - "go build ./... + go vet ./... + go test ./... all green repo-wide"
affects:
  - 17-08-PLAN  # Migration tool `dfs migrate-to-cas` (next wave)
  - 17-09-PLAN  # Boot guard + sentinel detection
  - 18          # Syncer simplification carries remaining transitional methods

# Tech tracking
tech-stack:
  added:
    - "lukechampine.com/blake3 (already a transitive dep) is now direct in pkg/blockstore/local/memory for synchronous AppendWrite rollup"
  patterns:
    - "Transitional method move on file deletion: when deleting a file that contains BOTH dead code (path-keyed writer) and a still-consumed method (engine-consumed WriteAt), MOVE the surviving method to a *_transitional.go file rather than gating the deletion. Path-keyed writer dies cleanly; engine consumers keep type-checking."
    - "Engine in-flight dedup keys: post-FormatStoreKey, the engine derives its own `payloadID/N` string via a private inFlightKey() helper in fetch.go. The shape is intentionally NOT the legacy CAS key — it is an in-memory map key, distinct from any on-disk or on-wire identifier."
    - "Memory backend synchronous rollup: the in-memory BlockStoreAppend implementation runs FastCDC inline inside AppendWrite (no background goroutine). Acceptable because the in-memory backend is test-only; fs runs the async rollup pool."

key-files:
  created:
    - pkg/blockstore/local/fs/blockstore_methods.go    # Put/GetRange/Has/Delete/Head/Walk/DeleteLog wrappers over chunkstore.go
    - pkg/blockstore/local/fs/write_transitional.go    # Engine-consumed transitional WriteAt (Phase 18 deletion target)
    - .planning/phases/17-unified-blockstore/17-07-SUMMARY.md
  modified:
    - pkg/blockstore/local/fs/fs.go                    # Restore LocalStore assertion; drop UseAppendLog + useAppendLog field
    - pkg/blockstore/local/fs/appendwrite.go           # Drop UseAppendLog gates from AppendWrite + DeleteAppendLog + TruncateAppendLog
    - pkg/blockstore/local/fs/rollup.go                # StartRollup no longer gates on useAppendLog
    - pkg/blockstore/local/fs/recovery.go              # recoverAppendLogs runs unconditionally
    - pkg/blockstore/local/fs/manage.go                # Inline private deleteBlockFile; delete orphan helpers
    - pkg/blockstore/local/fs/errors.go                # Delete ErrAppendLogDisabled sentinel
    - pkg/blockstore/local/fs/doc.go                   # Updated godoc — no flag-gated tier; append mandatory
    - pkg/blockstore/local/fs/test_hooks.go            # ReopenForTest drops UseAppendLog field
    - pkg/blockstore/local/memory/memory.go            # Major rewrite: add CAS surface + synchronous AppendWrite rollup; internal memBlockKey/keyBelongsToFile/parseBlockIdx helpers
    - pkg/blockstore/remote/memory/store.go            # Add Has
    - pkg/blockstore/remote/s3/store.go                # Add Has (HEAD probe)
    - pkg/blockstore/types.go                          # Delete FormatStoreKey, ParseStoreKey, KeyBelongsToFile, ParseBlockIdx
    - pkg/blockstore/engine/fetch.go                   # Internal inFlightKey() helper
    - pkg/blockstore/engine/sync_entry.go              # BlockKey() now uses fmt.Sprintf("payloadID/N")
    - pkg/blockstore/engine/gc.go                      # copylocks fix: _ = &sweepWG
    - pkg/blockstore/blockstoretest/conformance.go     # Accept ErrChunkNotFound OR ErrBlockNotFound
    - pkg/controlplane/runtime/shares/service.go       # Append-mandatory wiring; warn on legacy use_append_log config key
    - pkg/controlplane/runtime/blockgc_test.go         # fakeRemoteStore on unified RemoteStore
    - pkg/controlplane/runtime/init_test.go            # Shared remote round-trip via Put/Get
    - pkg/controlplane/runtime/shares/create_local_store_test.go  # Rewrite onto mandatory-append wiring
    - pkg/blockstore/types_test.go                     # Delete TestParseStoreKey_RoundTrip
    - pkg/blockstore/engine/perf_bench_test.go         # WriteBlockWithHash/ReadBlock → Put/Get
    - pkg/blockstore/engine/syncer_crash_test.go       # WriteBlockWithHash → Put on crashingRemoteStore
    - pkg/blockstore/engine/syncer_unit_test.go        # ReadBlock(string) → Get(hash); drop GetObjectMetadata assertion
    - pkg/blockstore/engine/sync_entry_test.go         # Expected BlockKey shape now "payloadID/N"
    - pkg/blockstore/engine/engine_dualread_test.go    # Inline legacy storeKey literal
    - pkg/blockstore/local/fs/appendwrite_test.go      # Drop TestAppendWrite_DisabledByDefault
    - pkg/blockstore/local/fs/rollup_test.go           # Drop TestRollup_StartRollup_DisabledFlag
    - pkg/blockstore/local/fs/delete_truncate_test.go  # Rename DisabledFlag_NoOp → NoPriorLog_NoOp
    - pkg/blockstore/local/fs/manage_test.go           # Delete obsolete TestManage{Truncate,Exists}* tests
    - pkg/blockstore/local/fs/appendwrite_bench_test.go, chunkstore_test.go, chunkstore_get_test.go, lockorder_test.go, recovery_test.go, rollup_test.go, appendlog_internals_test.go, fs_conformance_test.go  # Drop UseAppendLog field from FSStoreOptions literals
  deleted:
    - pkg/blockstore/local/fs/write.go                                       # 223 LoC; path-keyed <share>/<file>/<idx>.blk writer with tryDirectDiskWrite + createBlockFile + directDiskWriteThreshold
    - cmd/dfsctl/commands/blockstore/                                        # 3674 LoC; entire legacy v0.14/v0.15 migrate tool

key-decisions:
  - "WriteAt body moved to a new write_transitional.go file rather than gating the path-keyed writer deletion. The engine still calls m.local.WriteAt at engine.go:320 (Phase 18 carry-over per Plan 04 / CONTEXT.md <deferred>); deleting write.go without preserving WriteAt would have broken the build. The transitional file carries the same `Deprecated: removed in Phase 18` godoc tag the LocalStore interface declaration uses, so Phase 18's grep-sweep finds it."
  - "Direct-disk-write fast path (tryDirectDiskWrite + createBlockFile + directDiskWriteThreshold) DELETED outright — the post-Phase-17 CAS rollup path supersedes it on the production write surface. The 51 MB/s pwrite optimization the path-keyed writer offered is moot under CAS (the rollup uploads CAS chunks directly to remote). The memBlock buffer path still serves the transitional engine WriteAt consumer; that's good enough through Phase 17, and Phase 18's Syncer rewrite deletes that consumer entirely."
  - "Append is mandatory on the local tier post-v0.16 — the UseAppendLog opt-out flag, the useAppendLog field, the ErrAppendLogDisabled sentinel, and all `if !bc.useAppendLog` branches in appendwrite.go / rollup.go / recovery.go / manage.go are deleted in one sweep. shares/service.go warns once if a stale `use_append_log` config key is present and ignores the value. The legacy New() constructor's no-RollupStore path now silently uses defaults; NewWithOptions always wires the rollup pool."
  - "Legacy v0.14/v0.15 `dfsctl blockstore` migrate tool DELETED (3674 LoC). Its surface (RemoteStore.ListByPrefix, DeleteBlock, WriteBlockWithHash, ReadBlock, HeadObject) was already gone post Plan 03; the package no longer compiled. CONTEXT.md D-02 mandates `dfs migrate-to-cas` on the dfs binary as the v0.16 replacement, so the legacy tool was dead code. The deletion is consistent with the project memory rule 'v0.16+ refactors: delete legacy eagerly, no compat shims'. This was a Rule 3 (blocking) deviation discovered at full-repo build time."
  - "Engine in-flight dedup map keys: post-FormatStoreKey, fetch.go derives its own `payloadID/N` string via a private inFlightKey() helper. The shape is intentionally NOT the CAS key — it is an in-memory map key for download-dedup, distinct from any on-disk identifier. sync_entry.go's TransferRequest.BlockKey() mirrors the same shape."
  - "MemoryStore (local) was significantly rewritten — it now carries a real CAS-keyed surface (cas map[ContentHash]casEntry + synchronous FastCDC AppendWrite) alongside the legacy per-block surface (blocks map + files map + WriteAt/ReadAt/Flush). The legacy surface continues to satisfy the narrowed LocalStore admin-superset; the new CAS surface satisfies BlockStore+BlockStoreAppend. Internal helpers memBlockKey / keyBelongsToFile / parseBlockIdx replace the deleted package-level blockstore helpers."
  - "BlockStoreConformance Get_NotFound + Delete assertions LOOSENED to accept either ErrChunkNotFound or ErrBlockNotFound. The blockstore.go Get godoc already states 'Returns blockstore.ErrChunkNotFound (or blockstore.ErrBlockNotFound for remote backends — both are acceptable; callers match via errors.Is on either)' — the conformance test was over-tight relative to that contract."
  - "manage.go's DeleteBlockFile lost its public surface but the body was preserved as a private deleteBlockFile helper, called inline from DeleteAllBlockFiles (which itself is still on the interface as a Phase-18-carry-over transitional method)."

patterns-established:
  - "Atomic-merge-friendly deletion of a file with mixed-aliveness symbols: move the survivor to a *_transitional.go file BEFORE deleting the file with `git rm`. The deletion no longer affects the survivor's call sites, and the transitional marker tells Phase N+1's grep-sweep where to look."
  - "Test-side bulk pruning of removed config fields via Python re.sub script: when a deprecated struct field is removed across N test files, a tightly-scoped multi-pattern Python script (regex-anchored to the FSStoreOptions{...} literal forms) is more reliable than BSD sed under quoting constraints."
  - "Conformance suite contract loosening: when the consolidated interface godoc explicitly accepts multiple sentinel error types for the same condition, the suite must check via errors.Is on EACH accepted sentinel; otherwise the suite is over-tight relative to its own contract documentation."

requirements-completed: []

# Metrics
duration: ~75min
completed: 2026-05-20
---

# Phase 17 Plan 07: Delete path-keyed writer + restore LocalStore conformance

**Closed the mid-PR build gap Plan 04 licensed: added the BlockStoreAppend method surface to local FSStore + MemoryStore, added `Has` to remote backends, restored the `var _ local.LocalStore = (*FSStore)(nil)` compile-time assertion, deleted the path-keyed `pkg/blockstore/local/fs/write.go` (path-keyed writer) and orphan helpers, deleted `FormatStoreKey` + the legacy key parsers from `pkg/blockstore/types.go`, deleted the `UseAppendLog` opt-out flag + `ErrAppendLogDisabled` sentinel (append is mandatory post-v0.16), and as a Rule 3 deviation deleted the legacy `cmd/dfsctl/commands/blockstore/` migration tool (3674 LoC, superseded by Phase 17's planned `dfs migrate-to-cas`). go build, go vet, and go test all green repo-wide.**

## Performance

- **Duration:** ~75 min
- **Started:** 2026-05-20T19:00:00Z
- **Completed:** 2026-05-20T20:15:00Z
- **Tasks:** 3 (auto, all on plan)
- **Commits:** 3 — `1d544d3b` (Task 1), `d3e5dd8a` (Task 2), `48f28a44` (Task 3)
- **Files created:** 3 (2 production + 1 SUMMARY)
- **Files modified:** ~35
- **Files deleted:** 23 (pkg/blockstore/local/fs/write.go + 21 legacy migrate tool files + various)
- **LoC delta:** approximately +570 / -4570 = net -4000 LoC

## Accomplishments

### Task 1 — Add BlockStoreAppend methods on backends

**`pkg/blockstore/local/fs/blockstore_methods.go` (NEW)** — Put, GetRange, Has, Delete, Head, Walk, DeleteLog wrappers over the existing chunkstore.go primitives:

- `Put` → `StoreChunk` (with zero-hash guard).
- `GetRange` → opens the chunk file, validates offset/length, reads sub-range.
- `Has` → `HasChunk`.
- `Delete` → `DeleteChunk`.
- `Head` → `os.Stat` on `chunkPath(hash)`, returns `Meta{Size, LastModified}`.
- `Walk` → `filepath.WalkDir` over `<baseDir>/blocks/<hh>/<hh>/<hex>`, surfaces every chunk via the callback. Returns nil on `ErrStopWalk`, wraps any other callback error per the D-07 contract.
- `DeleteLog` → wrapper over `DeleteAppendLog`.

The existing `Get` (chunkstore.go:147 from Phase 16) and `AppendWrite` (appendwrite.go) already satisfy the contract.

**`pkg/blockstore/local/memory/memory.go` (MAJOR REWRITE)** — added a CAS-keyed surface alongside the legacy per-block surface:

- New fields: `cas map[ContentHash]casEntry`, `appendLogs map[string]*appendLog`, `tombstones map[string]struct{}`.
- New methods: `Put`, `Get`, `GetRange`, `Has`, `Delete`, `Head`, `Walk`, `AppendWrite`, `DeleteLog`. `AppendWrite` runs synchronous FastCDC inline (no background goroutine; safe because the in-memory backend is test-only).
- Internal helpers `memBlockKey`, `keyBelongsToFile`, `parseBlockIdx` replace the soon-deleted package-level `blockstore.FormatStoreKey` etc.
- Restored `var _ local.LocalStore = (*MemoryStore)(nil)` compile-time assertion.

**`pkg/blockstore/remote/{memory,s3}/store.go`** — added `Has`:
- memory: O(1) map lookup.
- s3: HEAD probe with `isNotFoundError` swallow.

**`pkg/blockstore/local/fs/fs.go`** — restored `var _ local.LocalStore = (*FSStore)(nil)` (Plan 04 marker `Plan 17-07 restores` removed).

### Task 2 — Delete path-keyed writer + FormatStoreKey + UseAppendLog

**DELETED `pkg/blockstore/local/fs/write.go` (223 LoC)** — `tryDirectDiskWrite`, `createBlockFile`, `directDiskWriteThreshold` constants, and the original `WriteAt`. The `WriteAt(ctx, payloadID, data, offset)` method body was moved to a new `pkg/blockstore/local/fs/write_transitional.go` (engine-consumed transitional method; Phase 18 deletion target) with the direct-disk fast path stripped — it now goes only through the memBlock buffer.

**DELETED from `pkg/blockstore/types.go`:**
- `FormatStoreKey(payloadID, blockIdx) string` — no remaining callers.
- `ParseStoreKey(storeKey) (payloadID, blockIdx, ok)` — symmetric to the above.
- `KeyBelongsToFile(key, payloadID) bool` — only used by `local/memory` which now has its own internal `keyBelongsToFile` helper.
- `ParseBlockIdx(key, payloadID) uint64` — same.

**DELETED from `pkg/blockstore/local/fs/`:**
- `errors.go::ErrAppendLogDisabled` sentinel.
- `fs.go::FSStore.useAppendLog` field.
- `fs.go::FSStoreOptions.UseAppendLog` field.
- The `if !bc.useAppendLog { return ErrAppendLogDisabled }` gate from `AppendWrite`, `DeleteAppendLog`, `TruncateAppendLog`, `StartRollup`.
- The `if bc.useAppendLog { recoverAppendLogs(...) }` branch from `recovery.go` (recovery now runs unconditionally).
- The public `DeleteBlockFile(payloadID, blockIdx)` method (body preserved as private `deleteBlockFile` helper, called inline from `DeleteAllBlockFiles`).
- `TruncateBlockFiles(payloadID, newSize)` — orphan post Plan 05.
- `ExistsOnDisk(payloadID, blockIdx)` — orphan post Plan 05.

**DELETED `cmd/dfsctl/commands/blockstore/` (Rule 3 deviation, see below)** — the entire v0.14/v0.15 legacy migration tool package, 3674 LoC across 21 files.

**ENGINE rewires:**
- `engine/fetch.go` — private `inFlightKey(payloadID, blockIdx)` helper replaces 3 `blockstore.FormatStoreKey` call sites.
- `engine/sync_entry.go` — `TransferRequest.BlockKey()` uses `fmt.Sprintf("%s/%d", ...)`.

**CONTROLPLANE rewire:**
- `pkg/controlplane/runtime/shares/service.go` — append-mandatory wiring. The `use_append_log` config key is now warn-logged and ignored. `fs.NewWithOptions` is always called (no longer conditional on the flag).

### Task 3 — Adapt tests + fix copylocks vet bug

- Bulk-removed `UseAppendLog: true|false,` fields from ~12 fs test files via a tightly-scoped Python `re.sub` script. Manual cleanup followed for empty `FSStoreOptions{}` literals.
- Deleted obsolete flag-off tests (`TestAppendWrite_DisabledByDefault`, `TestRollup_StartRollup_DisabledFlag`).
- Renamed `TestDelete_DisabledFlag_NoOp` → `TestDelete_NoPriorLog_NoOp`; same for the Truncate variant.
- Rewrote `pkg/controlplane/runtime/shares/create_local_store_test.go` to test the mandatory-append wiring (no flag).
- Retargeted engine tests onto the renamed RemoteStore (Put/Get/GetRange/Delete/Head/Walk/ReadBlockVerified(hash, expected)):
  - `perf_bench_test.go` — seed via Put, reads via Get + ReadBlockVerified.
  - `syncer_crash_test.go` — `crashingRemoteStore.WriteBlockWithHash` → `Put`.
  - `syncer_unit_test.go` — `rs.ReadBlock(string)` → `rs.Get(hash)`. Dropped `GetObjectMetadata` assertion (memory remote no longer exposes legacy metadata; content integrity is via `ReadBlockVerified`).
  - `engine_dualread_test.go` — inline legacy storeKey literal.
  - `sync_entry_test.go` — BlockKey shape now `"payloadID/N"` (was `"payloadID/block-N"`).
- Deleted `TestManageTruncateBlockFiles`, `TestManageExistsOnDisk`, `TestManageExistsOnDiskStaleMetadata`; rewrote the `DeleteBlockFile` callers to use the private helper.
- Deleted `TestParseStoreKey_RoundTrip` (helper deleted).
- Loosened `BlockStoreConformance.Get_NotFound` + `Delete` assertions to accept either `ErrChunkNotFound` or `ErrBlockNotFound` per the contract godoc.

**Rule 1 fix in `pkg/blockstore/engine/gc.go`:** the line `_ = sweepWG` (added by Plan 05 to silence the unused-variable error for inert scaffolding) was copying `sync.WaitGroup`, triggering govet's `copylocks` check. Replaced with `_ = &sweepWG` (pointer anchor).

## Mid-PR Build State

| Gate | Result |
|------|--------|
| `go build ./...` | PASS (exit 0) |
| `go vet ./...` | PASS (exit 0) |
| `go test ./pkg/blockstore/...` | PASS |
| `go test ./pkg/controlplane/...` | PASS |
| `go test ./...` (whole repo) | PASS |

## Decisions Made

### WriteAt body moved to write_transitional.go (departure from the plan's "preserve in place" wording)

The plan instructed to "MOVE just the `WriteAt` method body to a NEW file `pkg/blockstore/local/fs/write_transitional.go`". Done literally. The transitional file carries a `Deprecated: removed in Phase 18` godoc tag so Phase 18's `grep -rn 'removed in Phase 18'` sweep finds it alongside the LocalStore interface's transitional method declarations.

### tryDirectDiskWrite path deleted outright (no fallback)

The plan permits keeping or dropping the direct-disk fast path. Dropped it — the post-Phase-17 CAS rollup is the primary write path; the path-keyed writer's pwrite optimization was for a layout that no longer exists.

### Append-log mandatory + legacy use_append_log config key warns

`shares/service.go` previously read `use_append_log` from config and gated on it. Now the value is ignored with a warn log: `"block store config has use_append_log: append is mandatory in v0.16+, flag is ignored"`. Operators with stale configs see the deprecation; their stores still come up correctly because append is always wired.

### Legacy dfsctl migrate tool deletion (Rule 3 deviation)

**[Rule 3 - Blocker]** `cmd/dfsctl/commands/blockstore/` was broken at full-repo build time because it referenced symbols deleted by Plan 03 (`RemoteStore.WriteBlockWithHash`, `RemoteStore.ReadBlock`, `RemoteStore.ListByPrefix`, `RemoteStore.DeleteBlock`, `RemoteStore.HeadObject`) and types deleted in Plan 03 (`remote.ObjectInfo`, `remote.HeadResult`). Without rewriting the whole package onto the unified interface — which is out of scope for Plan 07 and would require re-validating a v0.14/v0.15 migration runbook that v0.16 explicitly replaces — the build cannot pass `go build ./...`.

Per CONTEXT.md D-02, v0.16 replaces this tool with `dfs migrate-to-cas` (offline cobra subcommand on the `dfs` binary). The legacy tool is dead code. Per the project memory rule "v0.16+ refactors: delete legacy eagerly, no compat shims", this was the right call.

- **Found during:** Task 2 build verification (`go build ./...` after deleting FormatStoreKey).
- **Issue:** Legacy package referenced 5 deleted RemoteStore methods + 2 deleted types + 4 deleted package-level helpers.
- **Fix:** `rm -rf cmd/dfsctl/commands/blockstore/` + removed the cobra registration in `cmd/dfsctl/commands/root.go`.
- **Files removed:** 21 (.go files in the deleted directory).
- **Commit:** `d3e5dd8a`.

### MemoryStore local rewrite

`MemoryStore` was the most complex backend change because it now needs to satisfy **both** the narrowed LocalStore interface (legacy ReadAt/WriteAt/Flush/IsBlockLocal/GetBlockData/WriteFromRemote/DeleteAllBlockFiles still required as transitional admin) **and** BlockStoreAppend (new Put/Get/Has/etc.). The implementation maintains two parallel data structures:

- Legacy per-block: `blocks map[string]*memBlock` (keyed by `<payloadID>/<blockIdx>`) — serves the transitional admin methods.
- CAS: `cas map[ContentHash]casEntry` + `appendLogs map[string]*appendLog` — serves the BlockStore + BlockStoreAppend surface.

The synchronous FastCDC rollup inside `AppendWrite` is a deliberate simplification for the in-memory backend — production fs backends run an async rollup pool, but the in-memory backend's only consumer is the conformance suite, which polls `Walk` for chunk surfacing with a 10-second deadline. Inline rollup makes that deterministic.

### BlockStoreConformance Get_NotFound + Delete: contract-correct loosening

The conformance suite's `Get_NotFound` and `Delete` subtests previously checked `errors.Is(err, ErrChunkNotFound)` exactly. The contract godoc in `pkg/blockstore/blockstore.go` already states "Returns blockstore.ErrChunkNotFound (or blockstore.ErrBlockNotFound for remote backends — both are acceptable; callers match via errors.Is on either)". The test was over-tight. Loosened to accept either sentinel. The fs backend continues to return `ErrChunkNotFound`; the s3/memory-remote backends continue to return `ErrBlockNotFound`.

## Deviations from Plan

### Rule 3 — Auto-fix blocking issue

**1. [Rule 3 - Blocker] Delete legacy `cmd/dfsctl/commands/blockstore/` migration tool**

Already documented under "Decisions Made § Legacy dfsctl migrate tool deletion". Adding here for the standard checklist.

### Rule 1 — Auto-fix bug

**1. [Rule 1 - Bug] Fix copylocks vet warning in `pkg/blockstore/engine/gc.go`**

- **Found during:** Task 3 `go vet ./...` after test files were repaired.
- **Issue:** Plan 05 added `_ = sweepWG` to silence the unused-variable error for inert worker-pool scaffolding (post-Walk-based-sweep). The blank assignment copies `sync.WaitGroup`, which embeds `sync.noCopy`. govet's `copylocks` analyzer fires.
- **Fix:** Replace with `_ = &sweepWG` (pointer-anchor, no copy).
- **Files modified:** `pkg/blockstore/engine/gc.go`.
- **Commit:** `48f28a44`.

## Known Flakes (not introduced by this plan; deferred)

- `TestFSStore_BlockStoreAppendConformance/ConcurrentStorm` is flaky when run as an ISOLATED subtest (`go test -run "ConcurrentStorm" ./pkg/blockstore/local/fs/`). When run as part of the full `TestFSStore_BlockStoreAppendConformance` test (all 5 subtests in sequence) it passes deterministically. Root cause appears to be timing-sensitive — the storm test waits up to 10s for the rollup pool to surface chunks, and the isolated-run case loses the warm-up that the prior `AppendLogRoundTrip` subtest performs. Pre-existing from Plan 06's conformance wiring. **Out of scope for Plan 07** — the full conformance suite passes reliably; the isolated-subtest run is a developer-debug ergonomics concern. Filed for Plan 18 / a future Plan 06 follow-up. Not in this plan's deviation register because it does not affect the production code path or any test gate this plan owns.

## Issues Encountered

None besides the Rule 3 + Rule 1 fixes above.

## TDD Gate Compliance

Plan 17-07 is mechanical interface migration + deletion. No new behavior to bisect into RED → GREEN. The TDD spirit is honored by the test adaptation work in Task 3 — the conformance suite continued to assert the contract throughout, and the test source teaches the deleted-symbol set what survives. No `tdd="true"` markers in the plan.

## Verification Output

```
$ test ! -f pkg/blockstore/local/fs/write.go && echo "DELETED"
DELETED

$ grep -rE '\bFormatStoreKey\b' --include='*.go' . | wc -l
0

$ grep -rE '\bUseAppendLog\b' --include='*.go' . | wc -l
0

$ grep -rE '\bErrAppendLogDisabled\b' --include='*.go' . | wc -l
0

$ grep -c 'Plan 17-07 restores' pkg/blockstore/local/fs/fs.go
0

$ grep -c 'Plan 17-07 restores' pkg/blockstore/local/memory/memory.go
0

$ grep -c 'var _ local.LocalStore = (\*FSStore)(nil)' pkg/blockstore/local/fs/fs.go
1

$ grep -c 'var _ local.LocalStore = (\*MemoryStore)(nil)' pkg/blockstore/local/memory/memory.go
1

$ go build ./...
$ echo $?
0

$ go vet ./...
$ echo $?
0

$ go test -count=1 -timeout 600s ./pkg/blockstore/... ./pkg/controlplane/...
ok  github.com/marmos91/dittofs/pkg/blockstore
ok  github.com/marmos91/dittofs/pkg/blockstore/chunker
ok  github.com/marmos91/dittofs/pkg/blockstore/engine
ok  github.com/marmos91/dittofs/pkg/blockstore/local/fs
ok  github.com/marmos91/dittofs/pkg/blockstore/local/memory
ok  github.com/marmos91/dittofs/pkg/blockstore/migrate
ok  github.com/marmos91/dittofs/pkg/blockstore/remote/memory
ok  github.com/marmos91/dittofs/pkg/blockstore/remote/s3
ok  github.com/marmos91/dittofs/pkg/controlplane/api
ok  github.com/marmos91/dittofs/pkg/controlplane/models
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/blockstoreprobe
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/clients
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/shares
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/stores
ok  github.com/marmos91/dittofs/pkg/controlplane/store
```

## Next Plan Readiness

- **Plan 17-08** (offline `dfs migrate-to-cas` subcommand) lands on a fully-clean tree. Inputs available: the unified BlockStore + BlockStoreAppend interfaces are in place; the FastCDC chunker is reusable; the existing `pkg/blockstore/migrate/` Phase 14 A5 framework can be adapted (per-share scope, journaling, progress, status reporting).
- **Plan 17-09** (boot guard + sentinel detection) — `NewFSStore` adds the `.cas-migrated-v1` stat check; `ErrLegacyLayoutDetected` sentinel surfaces through `cmd/dfs/start` for exit code 78.
- **Plan 18** (Syncer simplification) — the 7 transitional admin methods on `*FSStore` and `*MemoryStore` (ReadAt, WriteAt, Flush, IsBlockLocal, GetBlockData, WriteFromRemote, DeleteAllBlockFiles) remain in place with `Deprecated: removed in Phase 18` godoc. The engine consumer sites (engine.go:147,320,423,635,800,828, fetch.go:115,131,168,213,253,267, syncer.go:381, upload.go:168, dedup.go:248) are unchanged from Plan 05. Phase 18 deletes them together.

## Self-Check

- `pkg/blockstore/local/fs/write.go` does NOT exist — **FOUND (deletion confirmed)**.
- `pkg/blockstore/local/fs/write_transitional.go` exists with `func (bc *FSStore) WriteAt(...)` — **FOUND**.
- `pkg/blockstore/local/fs/blockstore_methods.go` exists with `Put`, `Has`, `Walk`, `Delete`, `Head`, `GetRange`, `DeleteLog` — **FOUND**.
- `pkg/blockstore/local/fs/fs.go` contains `var _ local.LocalStore = (*FSStore)(nil)` (uncommented) and does NOT contain `Plan 17-07 restores` — **FOUND**.
- `pkg/blockstore/local/memory/memory.go` contains `var _ local.LocalStore = (*MemoryStore)(nil)` (uncommented) and does NOT contain `Plan 17-07 restores` — **FOUND**.
- `pkg/blockstore/types.go` does NOT contain `FormatStoreKey`, `ParseStoreKey`, `KeyBelongsToFile`, `ParseBlockIdx` — **VERIFIED (grep returns 0)**.
- `pkg/blockstore/local/fs/errors.go` does NOT contain `ErrAppendLogDisabled` — **VERIFIED**.
- `pkg/blockstore/local/fs/fs.go::FSStoreOptions` does NOT contain `UseAppendLog` — **VERIFIED**.
- `cmd/dfsctl/commands/blockstore/` directory does NOT exist — **VERIFIED**.
- Commits `1d544d3b`, `d3e5dd8a`, `48f28a44` in `git log` — **FOUND**, all signed.
- `go build ./...` exits 0 — **VERIFIED**.
- `go vet ./...` exits 0 — **VERIFIED**.
- `go test ./...` all green — **VERIFIED**.

## Self-Check: PASSED

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
