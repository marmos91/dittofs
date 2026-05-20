---
phase: 16-cache-mmap-removal
plan: 03
subsystem: blockstore
tags: [blockstore, engine, cleanup, deletion]

requires:
  - phase: 16-cache-mmap-removal
    plan: 02
    provides: engine.loadByHash already rewired to local.Get(ctx, hash); generic byte-correctness ported into cache_test.go
provides:
  - cache_mmap_unix.go / cache_mmap_windows.go / cache_mmap_test.go deleted
  - TestPerfGate_Phase12_MmapHotPath removed; perf_bench_unix_test.go folded entirely
  - perf_bench_phase12_test.go docstrings purged of mmap / readFromCAS references
  - cache_test.go TestCache_LargeChunkRoundTrip docstring purged of "deleted readFromCAS file" reference (full symbol purge)
  - no per-OS cache file remains in pkg/blockstore/engine/
affects: [16-04 perf re-baseline / phase wrap]

tech-stack:
  added: []
  patterns:
    - "Post-rewire dead-code deletion: removes per-OS Cache loader files and their unit tests as a pure subtraction; production build stays green across the Task 1 → Task 2 boundary (only test compile briefly references deleted symbols)"
    - "Cross-OS build matrix via CGO_ENABLED=0 on a Darwin host — host cgo cannot cross-compile Linux libc; CGO_ENABLED=0 is the developer-laptop verification path for GOOS=linux/windows"

key-files:
  created: []
  modified:
    - pkg/blockstore/engine/cache_test.go
    - pkg/blockstore/engine/perf_bench_phase12_test.go
  deleted:
    - pkg/blockstore/engine/cache_mmap_unix.go
    - pkg/blockstore/engine/cache_mmap_windows.go
    - pkg/blockstore/engine/cache_mmap_test.go
    - pkg/blockstore/engine/perf_bench_unix_test.go

key-decisions:
  - "perf_bench_unix_test.go folded entirely (not surgically pruned) per PATTERNS.md inventory and the plan's Claude's Discretion YES ruling — the file contained only TestPerfGate_Phase12_MmapHotPath plus its formatChunkName helper; nothing else needed preservation, and removing the //go:build linux || darwin tag collapses the per-OS test surface to zero"
  - "perf_bench_phase12_test.go docstrings updated (not deleted): two paragraphs at the top of the file and inside BenchmarkRandRead_Phase12 referenced the mmap-via-readFromCAS / loadByHash→readFromCAS seam. Updated to reflect Phase 16's RAM-cache-backed local.Get path so the doc stays accurate against the live code"
  - "cache_test.go TestCache_LargeChunkRoundTrip docstring rewritten — the prior block referenced 'deleted readFromCAS file primitive' and 'mmap-specific assertions… that no longer exists post-Phase-16' which kept the symbol alive in source as past-tense documentation; the plan's acceptance gate `grep -rn 'readFromCAS' pkg/ --include='*.go'` mandates ZERO matches, so the cleanup is a Task 1 sub-action"
  - "No cache_ram_test.go created (D-11 honored) — RAM semantics stay covered by the existing cache_test.go (now augmented with Plan 16-02's TestCache_LargeChunkRoundTrip)"
  - "Two-commit task structure preserved per plan, despite the transient intermediate state where Task 1's commit leaves perf_bench_unix_test.go pointing at the deleted readFromCAS symbol. Production code (`go build ./...`) stays green at every commit; only the engine test compile is briefly broken between commits 59ccdf26 and 704f2f34. Acceptable because (a) no CI runs against intra-plan commits and (b) the alternative (merging Tasks into one commit, or deleting perf_bench_unix_test.go in Task 1) would violate the explicit plan directive 'Do NOT delete `perf_bench_unix_test.go` in this task — Task 2 owns that'"

patterns-established:
  - "Pure-subtraction plans honor task atomicity even when the intermediate state has a transient test-compile failure, provided production build stays green and the final state passes all gates"

requirements-completed: []

duration: ~12min
completed: 2026-05-20
---

# Phase 16 Plan 03: Delete mmap dead code + D-33 perf gate Summary

**Deletes four files (`cache_mmap_unix.go`, `cache_mmap_windows.go`, `cache_mmap_test.go`, `perf_bench_unix_test.go`) made dead by Plan 16-02's `loadByHash → local.Get` rewire; strips the last two stale `readFromCAS` references from `perf_bench_phase12_test.go` docstrings and the `cache_test.go::TestCache_LargeChunkRoundTrip` godoc; cross-OS build matrix (linux/darwin/windows) stays green and the engine race suite passes.**

## Performance

- **Duration:** ~12 min
- **Started:** 2026-05-20T13:50:00Z (approx)
- **Completed:** 2026-05-20T14:02:00Z (approx)
- **Tasks:** 2 (Task 1 — three file deletes + 1 docstring cleanup; Task 2 — one file delete + 1 docstring cleanup)
- **Files modified:** 6 (2 modified, 4 deleted)
- **LoC delta:** −440 lines (cache_mmap_unix.go 88 + cache_mmap_windows.go 32 + cache_mmap_test.go 204 + perf_bench_unix_test.go 105 + minor docstring edits)

## Accomplishments

- `pkg/blockstore/engine/cache_mmap_unix.go` deleted — 88 lines containing `mmapThresholdBytes`, the unix `readFromCAS` (with `syscall.Mmap` + `MAP_SHARED`/`MAP_FILE` flags), and the below-threshold `os.ReadFile` fallback are all gone.
- `pkg/blockstore/engine/cache_mmap_windows.go` deleted — 32 lines containing the Windows `readFromCAS` `os.ReadFile`-only fallback (no mmap on Windows).
- `pkg/blockstore/engine/cache_mmap_test.go` deleted — 204 lines containing 8 `TestReadFromCAS_*` functions (RoundTrip, PartialOffset, DestSmallerThanFile, BelowMmapThreshold_UsesReadFile, OffsetAtEOF, EmptyDest, MissingFile, Windows_FallbackPath) — all targeted the deleted primitive. Generic byte-correctness was already ported by Plan 16-02 into `cache_test.go::TestCache_LargeChunkRoundTrip`.
- `pkg/blockstore/engine/perf_bench_unix_test.go` deleted entirely (105 lines: `TestPerfGate_Phase12_MmapHotPath` + `formatChunkName` helper). Per PATTERNS.md inventory the file held only those two symbols, so the file folds away — the `//go:build linux || darwin` tag is removed with it, collapsing the per-OS test surface to zero.
- `pkg/blockstore/engine/perf_bench_phase12_test.go` docstrings updated: top-of-file paragraph rewritten ("mmap-via-readFromCAS on local hits" → "RAM-cache-backed local.Get on miss; mmap path removed in Phase 16"); `BenchmarkRandRead_Phase12` paragraph rewritten ("Mmap is exercised on linux/darwin via the loadByHash → readFromCAS seam" → simplified to call out only the binary search + OnRead + buffer copy that still apply).
- `pkg/blockstore/engine/cache_test.go::TestCache_LargeChunkRoundTrip` docstring shortened to drop the prior "Ported from cache_mmap_test.go::TestReadFromCAS_RoundTrip, reshaped to exercise the Cache API rather than the deleted readFromCAS file primitive" block — the symbol was kept alive in source as past-tense documentation; the plan's acceptance gate mandates a zero-match grep, so the cleanup runs as part of Task 1.

## Task Commits

1. **Task 1** — `59ccdf26` `chore(16-03): delete cache_mmap_{unix,windows,test}.go` (signed). Three `D` entries + `cache_test.go` docstring cleanup. `go build ./...` exits 0 post-commit; engine test compile briefly broken (Task 2's deleted file still depends on `readFromCAS`).
2. **Task 2** — `704f2f34` `chore(16-03): delete TestPerfGate_Phase12_MmapHotPath; fold perf_bench_unix_test.go` (signed). Fourth `D` entry + `perf_bench_phase12_test.go` docstring cleanup. Cross-OS build matrix green, engine race suite green.

## Files Created/Modified

### Deleted

- `pkg/blockstore/engine/cache_mmap_unix.go` (88 lines)
- `pkg/blockstore/engine/cache_mmap_windows.go` (32 lines)
- `pkg/blockstore/engine/cache_mmap_test.go` (204 lines)
- `pkg/blockstore/engine/perf_bench_unix_test.go` (105 lines)

### Modified

- `pkg/blockstore/engine/cache_test.go` — `TestCache_LargeChunkRoundTrip` godoc compressed from a 13-line "Ported from cache_mmap_test.go… deleted readFromCAS file primitive… mmap-specific assertions… were dropped per D-10" block to a 7-line "byte-equality of a multi-hundred-KiB chunk round-trip" pin. Behavior unchanged.
- `pkg/blockstore/engine/perf_bench_phase12_test.go` — two godoc paragraphs (top-of-file + `BenchmarkRandRead_Phase12`) updated to reflect Phase 16's RAM-only cache load path. No code changes.

## Decisions Made

All Must-Haves from `<must_haves>` were honored:

- `cache_mmap_unix.go` deleted (D-09 + Phase 16 scope).
- `cache_mmap_windows.go` deleted.
- `cache_mmap_test.go` deleted (D-09).
- `TestPerfGate_Phase12_MmapHotPath` removed from `perf_bench_unix_test.go` (D-08); the whole file folded per the plan's Claude's Discretion YES ruling, since PATTERNS.md inventory confirmed the file contained only the gate test plus the `formatChunkName` helper.
- No remaining references to `syscall.Mmap`, `readFromCAS`, or mmap as a load path in `pkg/blockstore/engine/` (verified by repository-wide grep below).
- `TestPerfGate_Phase12_MmapHotPath` references in `perf_bench_phase12_test.go` cleaned (two godoc paragraphs touched).
- Cross-OS build passes on linux + darwin + windows via `CGO_ENABLED=0 GOOS=<os> go build ./...`. Host-cgo `GOOS=linux go build ./...` from a Darwin laptop fails with `setresuid` / `setresgid` clang errors — that is a Darwin-host cgo cross-compile limitation (Linux libc unavailable to host clang), not a Phase 16 regression; with CGO disabled all three platforms exit 0.
- No `cache_ram_test.go` file created (D-11 honored).

## Deviations from Plan

**[Rule 3 — Blocking] Task 1's `<verify>` block as literally written cannot pass without Task 2.**

The plan's Task 1 `<verify>` gate runs `go test ./pkg/blockstore/engine/... -count=1 -race`. After Task 1's three file deletions, `perf_bench_unix_test.go` still references the deleted `readFromCAS` symbol — test compile fails until Task 2 deletes that file. Production code (`go build ./...`) passes after Task 1. Two valid resolutions: (a) merge the four deletions into a single commit (violates the plan's explicit two-task structure and the directive "Do NOT delete `perf_bench_unix_test.go` in this task — Task 2 owns that"), or (b) accept the transient red test-compile state between commits `59ccdf26` and `704f2f34`. Chose (b) — final state is fully green and the plan's prescribed task boundary is preserved. Documented here so future agents know Plan 16-03 is **not** safely `git bisect`-able between its two commits.

**[Rule 1 — Bug] Stale `readFromCAS` references in source documentation.**

`pkg/blockstore/engine/cache_test.go` lines 357–358 (from Plan 16-02) and `pkg/blockstore/engine/perf_bench_phase12_test.go` lines 3 + 162 referenced `readFromCAS` in godoc comments — these are not code references, but the plan's acceptance gate `grep -rn 'readFromCAS' pkg/ --include='*.go'` mandates zero matches. The plan's task list doesn't explicitly enumerate "scrub past-tense docs of the deleted symbol", but the gate's intent is unambiguous (full symbol purge). Both files were cleaned: `cache_test.go` change went into Task 1's commit (sibling to the deletion); `perf_bench_phase12_test.go` change went into Task 2's commit (sibling to the unix-test deletion).

## Issues Encountered

**Host-cgo cross-compile noise on Darwin.** Running `GOOS=linux go build ./...` on a Darwin host without `CGO_ENABLED=0` surfaces clang errors against `runtime/cgo` / `linux_syscall.c` (`setresuid` / `setresgid` undeclared) — host clang lacks Linux libc headers. With `CGO_ENABLED=0` all three target platforms exit 0. Not a Phase 16 regression — this is a standard Go cross-compile-from-Mac limitation and the build-tag-correctness test (does any Go file reference an OS-specific symbol that doesn't compile on another OS) is satisfied at the CGO-off level.

## Verification Results

### Symbol purge audit (all repo-wide greps return zero matches)

```
$ grep -rn 'readFromCAS' pkg/ --include='*.go'
(empty)
$ grep -rn 'syscall\.Mmap' pkg/ --include='*.go'
(empty)
$ grep -rn 'formatChunkName' pkg/ --include='*.go'
(empty)
$ grep -rn 'TestPerfGate_Phase12_MmapHotPath' pkg/ --include='*.go'
(empty)
$ grep -rn 'cache_mmap_unix\|cache_mmap_windows\|cache_mmap_test' pkg/ --include='*.go'
(empty)
```

### D-11 negative check (cache_ram_test.go must NOT exist)

```
$ ls pkg/blockstore/engine/cache_ram_test.go
ls: cannot access 'pkg/blockstore/engine/cache_ram_test.go': No such file or directory
```

### Cross-OS build matrix

```
$ go build ./...                              # native darwin/arm64 — exit 0
$ CGO_ENABLED=0 GOOS=linux   go build ./...   # exit 0
$ CGO_ENABLED=0 GOOS=darwin  go build ./...   # exit 0
$ CGO_ENABLED=0 GOOS=windows go build ./...   # exit 0
$ go vet ./pkg/blockstore/engine/...          # exit 0
```

### Engine test suite (with -race)

```
$ go test ./pkg/blockstore/engine/... -count=1 -race -short
ok  github.com/marmos91/dittofs/pkg/blockstore/engine  8.597s

$ go test ./pkg/blockstore/engine/... -count=1 -race
ok  github.com/marmos91/dittofs/pkg/blockstore/engine  24.171s
```

### Git status (post-plan)

```
$ git log --oneline -3
704f2f34 chore(16-03): delete TestPerfGate_Phase12_MmapHotPath; fold perf_bench_unix_test.go
59ccdf26 chore(16-03): delete cache_mmap_{unix,windows,test}.go
7f58eb93 docs(16-02): complete loadByHash rewire plan
```

## TDD Gate Compliance

N/A — pure deletion plan, no `tdd="true"` tasks. Plan type is `execute` (not `tdd`). The test artifacts being deleted target a primitive that no longer exists; preservation criteria (per Plan 16-02 D-10) was ported one plan earlier.

## User Setup Required

None.

## Next Phase Readiness

- **Plan 16-04** (warm-cache perf re-baseline) is unblocked. The engine no longer has per-OS files; `BenchmarkRandReadVerified` is the single warm-cache regression anchor per D-06 / D-07. Cross-OS build matrix is collapsed to one Cache implementation.
- **Phase 17** (unified `BlockStore` interface + legacy `.blk` delete + `migrate-to-cas`) inherits a clean Cache layer with a single source-of-bytes for misses (`local.Get(ctx, hash)`) — the type narrows from `LocalStore` to `BlockStore` with zero rename churn at the loadByHash site (Plan 16-02 D-02).
- **Phase 18** (Syncer mirror loop + ObjectID relocation) inherits the now-pure-RAM Cache contract.

## Self-Check: PASSED

- Deleted files absent on disk:
  - `pkg/blockstore/engine/cache_mmap_unix.go` — MISSING (expected — deleted)
  - `pkg/blockstore/engine/cache_mmap_windows.go` — MISSING (expected — deleted)
  - `pkg/blockstore/engine/cache_mmap_test.go` — MISSING (expected — deleted)
  - `pkg/blockstore/engine/perf_bench_unix_test.go` — MISSING (expected — deleted)
- Modified files contain expected content:
  - `TestCache_LargeChunkRoundTrip — Phase 16 D-10 generic byte-correctness pin` in `cache_test.go` — FOUND
  - `RAM-cache-backed local.Get on miss; mmap path removed in Phase 16` in `perf_bench_phase12_test.go` — FOUND
- Commits exist on `gsd/phase-16-cache-mmap-removal`:
  - `59ccdf26` (Task 1) — FOUND
  - `704f2f34` (Task 2) — FOUND
- All verification gates green:
  - 5 symbol-purge greps — empty
  - D-11 negative — file absent
  - 4 build invocations — all exit 0
  - `go vet ./pkg/blockstore/engine/...` — exit 0
  - `go test ./pkg/blockstore/engine/... -count=1 -race` — PASS (24.171s)

---
*Phase: 16-cache-mmap-removal*
*Completed: 2026-05-20*
