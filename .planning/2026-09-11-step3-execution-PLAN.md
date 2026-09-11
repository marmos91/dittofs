# Step 3 execution plan — journal seam inversion (block data-flow master plan §4)

Status: **EXECUTING.** Fan-out model: pi-subagents, one lane per managed worktree,
orchestrator opens/merges PRs serially. Authority: master plan §3b/§3c + design doc
(`2026-09-01-journal-library-design-PLAN.md`) §4–§6. Baseline: develop @ `3d598ac73`,
build+vet+`go test ./pkg/block/...` green (journal 154s, engine 150s).

## Already landed (verified 2026-09-11 — do NOT redo)

- Step 0 HIGH fixes #2227–#2231/#2238 → #2246/#2248/#2249/#2252/#2253; provenance
  validation (`ErrColdProvenanceAmbiguous`) lives in `RestoreToVersion`.
- Design doc §9/§10 gaps already closed: all 21 journal benchmarks `ReportAllocs`;
  badgerstore test import gone (in-package `slowDeduper`); per-transition benchmark set
  (`transition_bench_test.go`) exists.
- `pkg/block/syncer` exists (dynsem + upload_controller) from Step 1.

## Settled decisions (write into every lane prompt — do not re-litigate)

1. **Names stay.** journal / carver / syncer / engine (design doc §11.1). Tree surface
   names (`Commit`=`Sync`, `Hydrate`=`Fill`, `RestoreToVersion`=`Restore`,
   `SeedCold*`=`Seed`, `ListFiles`=`Files`) are the decided names.
2. **Queries:** `Extents(ctx, id) []Extent` becomes the state-truth primitive (Lane S);
   `FileSize`/`DurableExtent`/`ColdExtents`/`DataExtents` stay as derived methods —
   master plan §3c mitigation: "Size and DurableExtent stay separate; benchmark and
   keep receipts." Receipts = `transition_bench_test.go`, already recorded.
3. **`GC` is unexported, not deleted** — `gcLoop` (store.go:399) is the *only* trigger
   of dead-byte repack; eviction is independent (`ensureSpace`). Exported surface dies,
   body survives.
4. **`ErrFutureFormat` becomes journal-local; the shared `block.ErrFutureFormat`
   sentinel remains for the metadata tiers.** Boot-guard matching sites must be updated:
   `controlplane/runtime/init.go:265`, `cmd/dfs/commands/start.go:882,954` (+ tests),
   `local/fs/format_test.go`.
5. **§5 item 5 splits into two PRs** (deliberate, for reviewability): Lane C moves
   cutting/hashing/skip/accumulation into `pkg/block/carver` with byte-identical
   behaviour; Lane F is the `Flush`+`Run`+`AfterFile` seam on top. The four pinned seam
   tests port in Lane F, per design doc §4.4 ("port before rewriting").
6. **`FlushOptions` carries `Force bool`** (the tree's `CarveOptions.Force` capability —
   `RemoteSync.Flush` drain path and `DrainRollups` need it).
7. **`AfterFile` is mandatory** (§4.2): manifest reap + `maybeResetDirtyClock` fold, run
   under the shard flush lock, on success AND on partial-credit error (C5) AND on
   cancellation (C7).
8. **`Open` collapses to `Open(dir string, cfg Config)`** — `remote` param deleted
   (field assigned, never read — verified), `clock` moves to `Config.Clock` (nil →
   internal system clock), `CheckFormat` folds inside `Open`.
9. FSStore embed→named field is **Lane L** (§5 item 6), not earlier — engine's
   structural interfaces keep resolving via explicit forwards.

## Lanes

| # | Lane | Scope | Files touched | Depends on | PR |
| --- | --- | --- | --- | --- | --- |
| 1 | **H — hygiene** | Dead API (remote/`BlockID`/`PinVersion()` getter/`SegmentLocation` unexport), `Open`/`Config` collapse, `CheckFormat` fold, `GC` unexport, logger severance (`Config.Logger *slog.Logger`, 11 `Warn` sites in cold/reclaim/recovery/store), `ErrFutureFormat` local sentinel + boot-guard sites | `pkg/block/journal/{store,format,cold,reclaim,recovery}.go` + their tests, `pkg/block/local/fs/{fs.go,format_test.go}`, `controlplane/runtime/init.go`, `cmd/dfs/commands/{start.go,start_test.go}`, `pkg/block/engine/blocksink_test.go` | — | 1 |
| 2 | **C — carver library** | New `pkg/block/carver` (`Carver`/`Options`/`New`/`Box` per design doc §7.2, wrapping `pkg/block/chunker`; `Skip` = dedup hook); journal `packRuns` delegates cutting+hashing+skip+accumulation; `blake3` leaves journal. NOT the seam inversion: Carve flow, flip planning, ClobberGuard, reap, guards stay. Accumulation-level boundary-stability test added | `pkg/block/carver/**` (new), `pkg/block/journal/carve.go` (+ carve tests as needed) | — (disjoint from H) | 2 |
| 3 | **S — state model** | `State` enum + `StateLost` + `Extent{Off,Len,State,Durable}`; `Extents()` primitive; transition table as tests (WriteAt/Commit/Hydrate/Invalidate/Seed/evict/compact); `demote()` fsync-before-residency-loss chokepoint; `Invalidate` = `Resident→Remote` definition (dirty refusal already at store.go:1506); randomised model test vs naive oracle (never Lost, never silent zeros); `DurableExtent` over the new predicate | `pkg/block/journal/{index,store,cold,reclaim,evict paths}.go` + tests | H (store.go) | 3 |
| 4 | **F — the seam** | `Flush(ctx, id, FlushOptions, fn)` + `Run{ID,Extent,DurableTail,Final,ReaderAt}` + fragment-validated flip (C4, mirrors `flipUpTo`); C1–C10 contract; `AfterFile`; port the 4 pinned tests; engine `fn` closure (carver + dedup Skip + uploads + futures in submission order + manifest reap in AfterFile); `Carve`/`CarveOptions`/`CarveResult`/`SetCarveTargets`/journal `carve_dispatch.go` die; `carveMu`→`flushMu`; engine `carvePass`→`flushPass`; `Deduper`/`BlockSink`/`SupersededReaper`/`ManifestRowEnder`/`ClobberGuard` leave journal | journal `carve*.go`, `store.go`, `shard.go`, `flush.go` (new) + tests; `pkg/block/engine/{carve_dispatch.go,flush.go,syncer.go,blocksink.go}` | H, C, S | 4 |
| 5 | **L — layout + tests** | §6.2 file reorg (journal.go/open.go/write.go/read.go/flush.go/evict.go/compact.go/retire.go/recover.go/internal/wire//internal/fsutil/); FSStore embed→named field (+ explicit forwards); `TestNoForeignImports` (§8.1); `journaltest/` conformance suite; `Example_` tests; package README | journal tree-wide moves, `pkg/block/local/fs/fs.go`, new test pkgs | F | 5 |
| 6 | **D — docs** | §14 rewrite: architecture.md four-part model + data-flow, durability.md exposure accounting, configuration.md vestigial knobs, implementing-stores.md repoint, snapshots.md, faq §12 gaps, BENCHMARKS.md, contributing.md, CLAUDE.md invariants 4/5/6, README; stale RFC status line fix (`docs/internals/rfc-block-dataflow.md` says "nothing built" — steps 0–2 landed) | `docs/**`, `CLAUDE.md`, `README.md` — **never `.planning/**`** | C (carver shape); merges last | 6 |

Parallelism: **H ∥ C** (disjoint files) → **S ∥ D** → F → L → D merge.
All merges serial, order H → C → S → F → L → D, rebase each branch before its merge.

## Per-PR protocol

1. Lane agent implements in its worktree; gates: `go build ./...`, `go vet ./...`,
   `gofmt -l`, `go test ./pkg/block/... -count=1`; `-race` on journal+engine for S/F.
   Every regression test must fail on a deliberately broken build before it is trusted.
2. Orchestrator dispatches THREE fresh-context reviews on the diff before opening:
   correctness reviewer, simplification reviewer (ponytail charter — clean & simple is
   the mantra; delete over add), adversarial panel (refute-root-cause charter — concrete
   counterexample or no objection; seam invariants on trial: commit-before-flip,
   watermark order, AfterFile-under-lock, fragment-validated credit, no-shared-completion-
   stream).
3. Verdict fixes resume the lane agent. Then PR opens: signed commits (`git commit -S`),
   assign marmos91, lane-scoped scratchpad body re-read immediately before publish, no
   `Closes #N` (no issue maps to a lane).
4. Babysit: `gh pr checks` via background watcher; Copilot comments fixed on arrival;
   CI failures diagnosed from logs and fixed. Pre-existing known-failures tally (SMB 86
   - NFS 92, row regex `^\| *[A-Z]+[0-9]+[a-z]? *\|`) is not a regression.
5. Merge serially via `gh api PUT pulls/{n}/merge -f sha=<re-read head>` + squash +
   delete branch; rebase next lane; `graphify update .` after the last merge of the day.

## Out of scope (adjacent, not Step 3)

- Master plan §12.5 items 2–4 (dedup scaffolding decision, retired local-GC machinery,
  117 LOW triage) — separate workstreams.
- Step 4 legacy cleanup (`local/fs` deletion, vestigial knobs) — after Step 3.
- SCW VM: not needed by any lane (no Linux/root gates; dm-flakey rigs stay shell-side).
