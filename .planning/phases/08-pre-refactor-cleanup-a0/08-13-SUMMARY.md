---
phase: 08-pre-refactor-cleanup-a0
plan: 13
subsystem: blockstore
tags: [blockstore, engine, refactor, git-mv, TD-01, PR-C, D-17, D-18, D-31, W9]

# Dependency graph
requires:
  - phase: 08-pre-refactor-cleanup-a0
    provides: "Plan 08-12 closed PR-B (go mod tidy after v0.13.0 backup removal), leaving the block-store layer stable and ready for structural reshaping."
provides:
  - "Single flat `pkg/blockstore/engine/` package containing the former readbuffer / sync / gc subtrees. No behavioral changes — imports, types, and function signatures re-routed only."
  - "Blame-preserving `git mv` for all 22 source files (rename detection ≥ 92% on every file, 97% on syncer/gc/cache)."
  - "Collision-resolving renames captured in git history with the merge commit, so `git log --follow` traces back to the originals."
  - "PR-C block 1 complete — PR-C block 2 (TD-03 remnant deletions) and block 3 (TD-04 parser collapse) now execute against a single package instead of four."
affects: [v0.15.0-pr-c]

# Tech tracking
tech-stack:
  added: []
  removed: []
  patterns:
    - "`git mv` across package boundaries preserves blame (Git auto-detects rename-with-modify at 50% similarity; our files landed at 92-99%)."
    - "Inline alias strip after `sed`-based import rewrite (W9) — the existing test files pre-aliased `blocksync` ended up self-importing after the merge; Step 7 enumerated every `blocksync.Identifier` / `readbuffer.Identifier` pair and rewrote via `gofmt -r` per identifier."
    - "Package-qualified identifier strip (e.g., `blocksync.Syncer` → `*Syncer`, `readbuffer.ReadBuffer` → `*ReadBuffer`) with post-pass double-pointer sanity fix (`\\*\\*X` → `\\*X`)."

key-files:
  created:
    - "pkg/blockstore/engine/doc.go (consolidated package doc for readbuffer + sync + gc sections)"
  renamed:
    - "pkg/blockstore/readbuffer/readbuffer.go → pkg/blockstore/engine/cache.go (97% similarity; per D-17 rename-at-move)"
    - "pkg/blockstore/readbuffer/readbuffer_test.go → pkg/blockstore/engine/cache_test.go"
    - "pkg/blockstore/readbuffer/prefetch.go → pkg/blockstore/engine/prefetch.go"
    - "pkg/blockstore/readbuffer/prefetch_test.go → pkg/blockstore/engine/prefetch_test.go"
    - "pkg/blockstore/sync/syncer.go → pkg/blockstore/engine/syncer.go"
    - "pkg/blockstore/sync/syncer_test.go → pkg/blockstore/engine/syncer_test.go"
    - "pkg/blockstore/sync/syncer_put_error_test.go → pkg/blockstore/engine/syncer_put_error_test.go (not in plan D-17 mapping — Rule 3 carry-over; file would have orphaned)"
    - "pkg/blockstore/sync/upload.go → pkg/blockstore/engine/upload.go"
    - "pkg/blockstore/sync/queue.go → pkg/blockstore/engine/sync_queue.go (prefixed per D-17)"
    - "pkg/blockstore/sync/queue_test.go → pkg/blockstore/engine/sync_queue_test.go"
    - "pkg/blockstore/sync/health.go → pkg/blockstore/engine/sync_health.go (prefixed)"
    - "pkg/blockstore/sync/health_test.go → pkg/blockstore/engine/sync_health_test.go"
    - "pkg/blockstore/sync/health_integration_test.go → pkg/blockstore/engine/sync_health_integration_test.go"
    - "pkg/blockstore/sync/fetch.go → pkg/blockstore/engine/fetch.go"
    - "pkg/blockstore/sync/dedup.go → pkg/blockstore/engine/dedup.go"
    - "pkg/blockstore/sync/entry.go → pkg/blockstore/engine/sync_entry.go (prefixed)"
    - "pkg/blockstore/sync/entry_test.go → pkg/blockstore/engine/sync_entry_test.go"
    - "pkg/blockstore/sync/types.go → pkg/blockstore/engine/types.go (D-17 planner choice: new types.go, not engine.go — clearer scope)"
    - "pkg/blockstore/sync/nil_remotestore_test.go → pkg/blockstore/engine/nil_remotestore_test.go"
    - "pkg/blockstore/gc/gc.go → pkg/blockstore/engine/gc.go"
    - "pkg/blockstore/gc/gc_test.go → pkg/blockstore/engine/gc_test.go"
    - "pkg/blockstore/gc/gc_integration_test.go → pkg/blockstore/engine/gc_integration_test.go"
  modified:
    - "pkg/blockstore/engine/engine.go (strip self-import of engine, inline `blocksync.Syncer` → `*Syncer`, `readbuffer.ReadBuffer` → `*ReadBuffer`, package doc comment removed → moved to doc.go)"
    - "pkg/blockstore/engine/engine_test.go (strip `blocksync` alias, rewrite `blocksync.New` → `NewSyncer`, `blocksync.DefaultConfig` → `DefaultConfig`)"
    - "pkg/blockstore/engine/engine_offline_test.go (same alias strip; `blocksync.Config` → `SyncerConfig`)"
    - "pkg/blockstore/engine/engine_health_test.go (same alias strip)"
    - "pkg/controlplane/runtime/blockgc.go (rewrite `gc.Stats` → `engine.GCStats`, `gc.Options` → `engine.Options`, `gc.CollectGarbage` → `engine.CollectGarbage`)"
    - "pkg/controlplane/runtime/blockgc_test.go (same)"
    - "pkg/controlplane/runtime/runtime_test.go (remove duplicate engine import introduced by alias cleanup)"
    - "pkg/controlplane/runtime/shares/service.go (drop `blocksync` alias — use plain `engine`; rewrite 4 call sites)"
    - "internal/adapter/nfs/v3/handlers/testing/fixtures.go (drop `blocksync` alias)"
    - "internal/adapter/nfs/v4/handlers/io_test.go (drop `blocksync` alias)"
  deleted:
    - "pkg/blockstore/readbuffer/doc.go (content merged into engine/doc.go)"
    - "pkg/blockstore/sync/doc.go (content merged into engine/doc.go)"
    - "pkg/blockstore/gc/doc.go (content merged into engine/doc.go)"
    - "pkg/blockstore/readbuffer/ (directory removed)"
    - "pkg/blockstore/sync/ (directory removed)"
    - "pkg/blockstore/gc/ (directory removed)"

key-decisions:
  - "**D-17 types.go landing spot — chose new engine/types.go (not engine.go).** Engine.go already hosts the BlockStore orchestrator + its `Config` type; adding syncer constants (`DefaultParallelUploads` etc.), `SyncerConfig`, `SyncQueueConfig`, `TransferType`, `ErrClosed` there would mix concerns. Separate types.go keeps engine.go focused on orchestration."
  - "**Symbol rename — `sync.Config` → `SyncerConfig`.** `Config` collided with the existing `engine.Config` (the BlockStore orchestrator config). Since engine.Config has a more central role and is the public constructor input, renamed the syncer one."
  - "**Symbol rename — `gc.Stats` → `GCStats`.** Collided with `readbuffer.Stats` (now in cache.go). Both renamed: gc's → `GCStats`, readbuffer's → `CacheStats`. External callers in `pkg/controlplane/runtime/blockgc{,_test}.go` updated to `engine.GCStats`."
  - "**Symbol rename — `readbuffer.Stats` → `CacheStats`.** As above. The method `ReadBuffer.Stats()` (method, not type) kept its name — scoped to the ReadBuffer receiver, no collision."
  - "**Symbol rename — two `func New` constructors.** Three `New` existed post-merge: `cache.New` (ReadBuffer), `syncer.New` (Syncer), and `engine.New` (BlockStore). Renamed former two to `NewReadBuffer` and `NewSyncer` respectively. Kept `engine.New` as the public BlockStore constructor since it is the most-called external entry point."
  - "**Duplicate const `BlockSize`.** Both `sync/types.go` and `gc/gc.go` re-exported `blockstore.BlockSize`. Kept the one in gc.go (originally a re-export for byte estimation); deleted the one in types.go. Replaced with a comment explaining the single declaration lives in gc.go."
  - "**Duplicate test infra consolidation.** `TestMain`, `startSharedLocalstack`, `localstackHelper`, `sharedHelper`, `initClient`, `createBucket` were duplicated across `syncer_test.go` (sync pkg) and `gc_integration_test.go` (gc pkg) — identical bodies. Kept the syncer_test.go copy as canonical; gc_integration_test.go gutted to just its tests + a comment pointing at syncer_test.go for Localstack setup."
  - "**External `blocksync` alias cleanup.** Files outside `pkg/blockstore/engine/` that aliased the old sync package as `blocksync` (`internal/adapter/nfs/v4/handlers/io_test.go`, `internal/adapter/nfs/v3/handlers/testing/fixtures.go`, `pkg/controlplane/runtime/runtime_test.go`, `pkg/controlplane/runtime/shares/service.go`) had the alias dropped — they now use plain `engine.` qualifiers. Not strictly self-imports, but simplifies the mental model now that the sibling packages are gone."
  - "**Package doc comment migration.** Removed the inline package doc from engine.go; the consolidated doc now lives in engine/doc.go, organized into three sections (read buffer + prefetch, syncer, gc) that preserve the editorial content of the three deleted doc.go files."

patterns-established:
  - "**Collision-resolving rename at merge.** When two Go packages merge and both export `Config` / `Stats` / `New` / `TestMain`, rename the less-central one with a package-prefix suffix (`Syncer*`, `GC*`, `Cache*`). Update all external call sites in the same commit. Rule of thumb: prefer renaming the smaller external surface."
  - "**`gofmt -r` caveat for selector expressions.** `gofmt -r 'Stats -> GCStats'` rewrites ALL `Stats` identifiers, including selector exprs like `blockstore.Stats` → `blockstore.GCStats`. Always grep for unintended selector rewrites after a rewrite pass and restore them with targeted sed."
  - "**GNU vs BSD sed -i.** This repo's macOS dev workflow has historically used `sed -i ''`, but Nix-installed dev envs ship GNU sed where `-i ''` passes empty string as a filename. For portability, prefer `-i` without argument on GNU sed. Switched to `-i` form after confirming `sed --version` at start of session."

requirements-completed: [TD-01]

# Metrics
duration: ~25min
started: 2026-04-23T20:29:00Z
completed: 2026-04-23T20:44:21Z

commits:
  - hash: ca12df6a
    title: "blockstore: merge readbuffer/sync/gc into engine (TD-01)"
    signed: true
    file-count: 36
    insertions: 230
    deletions: 352
---

# Phase 08 Plan 13: TD-01 Merge — readbuffer + sync + gc → engine Summary

**One-liner:** Atomic `git mv` merge of `pkg/blockstore/{readbuffer,sync,gc}` into `pkg/blockstore/engine/` with D-17 rename-at-move, W9 self-referential alias strip, and consolidation of three `doc.go` files into a single `engine/doc.go` — blame preserved on all 22 moved files.

## Outcome

- Three sibling packages (`readbuffer`, `sync`, `gc`) deleted from `pkg/blockstore/`.
- All 22 `.go` files moved via `git mv` (rename similarity 92%–99% — full blame history preserved).
- All imports of the three old paths rewritten to `pkg/blockstore/engine` across the entire repository.
- Self-referential aliases (`blocksync`, `readbuffer`) in existing engine test files stripped per W9.
- One atomic signed commit: `ca12df6a`.

## Symbol Renames Applied (Collision Resolution)

The merge surfaced four type collisions and two test-infra collisions. Resolved in the same commit:

| Original                               | Renamed to                 | Reason                                                             |
| -------------------------------------- | -------------------------- | ------------------------------------------------------------------ |
| `sync.Config` (struct)                 | `SyncerConfig`             | Collided with existing `engine.Config` (BlockStore constructor).   |
| `gc.Stats` (struct)                    | `GCStats`                  | Collided with `readbuffer.Stats` post-merge.                       |
| `readbuffer.Stats` (struct)            | `CacheStats`               | Collided with `gc.Stats` post-merge. Method `ReadBuffer.Stats()` kept — receiver-scoped, no collision. |
| `sync.New` (syncer constructor)        | `NewSyncer`                | Three `New` in same package post-merge.                            |
| `readbuffer.New` (ReadBuffer ctor)     | `NewReadBuffer`            | Three `New` in same package post-merge.                            |
| `sync.BlockSize` (const)               | deleted (kept gc's)        | Both re-exported `blockstore.BlockSize` — deduplicated.            |
| `sync.TestMain` + `gc.TestMain`        | kept syncer_test.go copy   | Identical bodies; gc_integration_test.go gutted to just tests.     |
| `sync.startSharedLocalstack` + gc's    | kept syncer_test.go copy   | Identical bodies; same treatment as TestMain.                      |
| `localstackHelper` (type, 2 copies)    | kept syncer_test.go copy   | Identical bodies.                                                  |

External callers of renamed symbols were updated in the same commit:

- `pkg/controlplane/runtime/blockgc.go` + `blockgc_test.go`: `gc.Stats` → `engine.GCStats`, `gc.Options` → `engine.Options`, `gc.CollectGarbage` → `engine.CollectGarbage`, `gc.MetadataReconciler` → `engine.MetadataReconciler`.
- `pkg/controlplane/runtime/shares/service.go`: dropped `blocksync` alias, updated `blocksync.{New,Config,DefaultConfig}` → `engine.{NewSyncer,SyncerConfig,DefaultConfig}`.
- `pkg/controlplane/runtime/runtime_test.go`: same.
- `internal/adapter/nfs/v4/handlers/io_test.go`: same.
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go`: same.

## W9 Self-Alias Strip

After the import rewrite, four files in `pkg/blockstore/engine/` self-imported engine via the `blocksync` or unaliased alias:

| File                                       | Line | Alias removed                                              |
| ------------------------------------------ | ---- | ---------------------------------------------------------- |
| `pkg/blockstore/engine/engine.go`          | 19   | `"github.com/marmos91/dittofs/pkg/blockstore/engine"`      |
| `pkg/blockstore/engine/engine.go`          | 21   | `blocksync "github.com/marmos91/dittofs/pkg/blockstore/engine"` |
| `pkg/blockstore/engine/engine_test.go`     | 13   | `blocksync "github.com/marmos91/dittofs/pkg/blockstore/engine"` |
| `pkg/blockstore/engine/engine_offline_test.go` | 14 | `blocksync "github.com/marmos91/dittofs/pkg/blockstore/engine"` |
| `pkg/blockstore/engine/engine_health_test.go`  | 13 | `blocksync "github.com/marmos91/dittofs/pkg/blockstore/engine"` |

All five lines deleted. Qualified call sites (`blocksync.Syncer`, `blocksync.New`, `blocksync.Config`, `blocksync.DefaultConfig`, `readbuffer.ReadBuffer`, `readbuffer.Prefetcher`, `readbuffer.New`, `readbuffer.NewPrefetcher`) stripped to bare identifiers with renames applied (`blocksync.New` → `NewSyncer`, etc.). Double-pointer artifact from pointer-qualifier rewrites (e.g., `*blocksync.Syncer` → `**Syncer`) fixed in a follow-up sed pass.

Final grep verifies zero `blocksync.` or `readbuffer.` qualifiers remain in `pkg/blockstore/engine/` (excluding comments and URLs), and zero `blocksync` alias imports exist anywhere in the repository.

## D-17 `types.go` Landing Decision

The planner deferred to the executor on whether to fold `sync/types.go` contents into `engine/engine.go` or create a new `engine/types.go`. Chose **new `engine/types.go`** because:

1. `engine/engine.go` already owns the BlockStore orchestrator and its `Config` type — conceptually a different layer from the syncer's constants / queue config / transfer-type enum.
2. The moved file contains standalone types (`SyncerConfig`, `SyncQueueConfig`, `TransferType`, `ErrClosed`, default constants) with no dependency on `engine.BlockStore` and no reason to share a file.
3. Git mv preserves history better on a 1:1 file rename than on a content-splitting merge.

`engine/types.go` retains its original sync-package contents minus the duplicated `BlockSize` const (declared in gc.go post-merge) and the unused `blockstore` package import.

## Doc.go Consolidation

`pkg/blockstore/engine/doc.go` (new file, 89 lines) merges the editorial content of the three deleted `doc.go` files into a unified package doc comment with three sections:

1. **Read buffer and prefetch** — taken from `readbuffer/doc.go`; updated `New` → `NewReadBuffer`.
2. **Syncer** — taken from `sync/doc.go`; dropped the now-obsolete "package sync shadows stdlib sync" note since we're no longer named `sync`. Dropped the "Finalization callback" bullet since TD-03 (next PR-C commit) will delete it.
3. **Block garbage collection** — taken from `gc/doc.go`; updated `gc.CollectGarbage` / `gc.Options` → `engine.CollectGarbage` / `engine.Options`.

The inline package doc at the top of `engine.go` was removed to avoid duplicate package comments.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Moved `syncer_put_error_test.go` not in plan mapping**

- **Found during:** Step 2 (`git mv`)
- **Issue:** `pkg/blockstore/sync/syncer_put_error_test.go` existed on disk but was not in the D-17 rename mapping in the plan's `<interfaces>` block. Leaving it would either orphan a test file in a soon-to-be-deleted directory or cause `rmdir pkg/blockstore/sync` to fail.
- **Fix:** Added to the `git mv` batch: `pkg/blockstore/sync/syncer_put_error_test.go` → `pkg/blockstore/engine/syncer_put_error_test.go`. Package declaration rewritten `package sync` → `package engine`. Bare `New(` calls rewritten to `NewSyncer(` per the rest of the batch.
- **Commit:** `ca12df6a` (same atomic commit as the rest of the merge)

**2. [Rule 3 - Blocking] GNU sed `-i ''` argument parsing**

- **Found during:** Step 3 (package rename)
- **Issue:** The plan used BSD-style `sed -i ''` (macOS). This repo's Nix dev env ships GNU sed, which interprets the empty string as a filename, causing `sed: can't read : No such file or directory`.
- **Fix:** Switched to `sed -i` without any argument (GNU form). All subsequent sed passes use the GNU form. Documented in patterns-established for future phases.
- **Commit:** `ca12df6a`

**3. [Rule 3 - Blocking] `gofmt -r 'Config -> SyncerConfig'` also rewrote selector `remotes3.Config`**

- **Found during:** Step 4 (collision rename)
- **Issue:** `gofmt -r` rewrites every identifier named `Config`, including selector expressions like `remotes3.Config{Bucket: ...}`, turning them into `remotes3.SyncerConfig{...}` which does not exist.
- **Fix:** Post-pass `sed` restored `remotes3.SyncerConfig` → `remotes3.Config` in the two affected call sites (syncer_test.go lines 342 and 363). Added to patterns-established.
- **Commit:** `ca12df6a`

**4. [Rule 3 - Blocking] `*blocksync.Syncer` → `**Syncer` pointer artifact**

- **Found during:** Step 7 (W9 alias strip)
- **Issue:** The sed pass `blocksync.Syncer -> *Syncer` turned pre-existing `*blocksync.Syncer` declarations into `**Syncer` (double pointer). Same for `*readbuffer.ReadBuffer` and `*readbuffer.Prefetcher`.
- **Fix:** Follow-up `sed -i 's|\*\*Syncer|*Syncer|g'` etc. cleanup in engine.go. Four declarations fixed.
- **Commit:** `ca12df6a`

**5. [Rule 3 - Blocking] Duplicate `engine` import after alias cleanup**

- **Found during:** Final sweep of external blocksync alias removal.
- **Issue:** Four files (`shares/service.go`, `runtime_test.go`, `fixtures.go`, `io_test.go`) already had an `"github.com/marmos91/dittofs/pkg/blockstore/engine"` import in addition to the `blocksync "..../engine"` alias (introduced earlier during plan 08-10 or similar). Dropping the alias left duplicate imports → `engine redeclared in this block` compile error.
- **Fix:** Deleted the redundant import line in each of the four files. Build clean after fix.
- **Commit:** `ca12df6a`

**6. [Rule 3 - Blocking] cache.go `func Stats()` method name captured by rename**

- **Found during:** Step 4 (collision rename)
- **Issue:** `gofmt -r 'Stats -> CacheStats'` rewrote not only the `type Stats` declaration but also the method name `func (c *ReadBuffer) Stats()` → `func (c *ReadBuffer) CacheStats()`. Callers of `readBuffer.Stats()` in `engine.go` line 478 and 542 would break.
- **Fix:** Restored method name to `Stats()` (it's receiver-scoped, no collision); kept the type rename to `CacheStats`. Edited `cache.go` lines 248-259 manually.
- **Commit:** `ca12df6a`

### Auth Gates

None.

### CLAUDE.md Adjustments

None — CLAUDE.md invariants (Runtime entrypoint, AuthContext threading, opaque file handles, per-share block stores, WRITE coordination order, ExportError codes, conformance suites) are unaffected by this structural merge. No signatures of the Runtime surface changed.

## Verification

- `test ! -d pkg/blockstore/readbuffer && test ! -d pkg/blockstore/sync && test ! -d pkg/blockstore/gc` — PASS.
- `test -f pkg/blockstore/engine/{cache,prefetch,syncer,upload,sync_queue,sync_health,fetch,dedup,sync_entry,types,gc,doc}.go` — PASS (all 12 files).
- `grep -rnE '"github\.com/marmos91/dittofs/pkg/blockstore/(readbuffer|sync|gc)"' . --include='*.go' | grep -v vendor` — 0 matches.
- `grep -rn blocksync --include='*.go' . | grep -v vendor` — 0 matches.
- `grep -rnE 'blocksync\.|readbuffer\.' pkg/blockstore/engine/ --include='*.go' | grep -v "^[^:]*:[0-9]*:[[:space:]]*//"` — 0 matches (W9).
- `go build ./...` — exit 0.
- `go vet ./...` — exit 0.
- `go test -count=1 -short -race ./pkg/blockstore/... ./pkg/controlplane/... ./internal/adapter/...` — all packages PASS.
- `go test -count=1 -short -race ./...` — all packages PASS (no FAIL in output).
- `git log --follow --oneline pkg/blockstore/engine/cache.go | wc -l` > 1 — PASS (history back to Phase 47 block store refactor).
- `git log --follow --oneline pkg/blockstore/engine/syncer.go | wc -l` > 1 — PASS.
- `git log --follow --oneline pkg/blockstore/engine/gc.go | wc -l` > 1 — PASS (history back to 2023 payload-layer restructuring).
- `git log -1 --show-signature` — `Good "git" signature`.
- `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by"` — exit 1 (no match, as expected).

## Self-Check: PASSED

All claimed files and commits exist:

- `pkg/blockstore/engine/doc.go` — FOUND.
- `pkg/blockstore/engine/cache.go` — FOUND (renamed from `readbuffer/readbuffer.go`).
- `pkg/blockstore/engine/syncer.go` — FOUND.
- `pkg/blockstore/engine/gc.go` — FOUND.
- Commit `ca12df6a` — FOUND in `git log`.
- Signed-by key SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8 — FOUND.
- No STATE.md / ROADMAP.md modifications — confirmed via `git show --stat ca12df6a` (list contains only pkg/, internal/, controlplane/ files + engine/doc.go).
