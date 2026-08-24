# #1872 carve drain convergence — staged plan (tracking: #2070)

A randomly-written share's drain does not converge: `dfsctl system drain-uploads` times out and
eviction fails with it. Measured tail on the bench VM was ~2 MiB/30s (~68 KiB/s), network TX
matching, about 21 serial PUTs/s.

#1875 fixed both contributing defects in one diff and **regressed on hardware** — blocks committed
continuously while `UnsyncedBytes` stayed flat at 644 MiB, meaning records were never flipped
synced and the carve re-carved the same ranges forever. That root cause is still unknown. The
plan below exists to avoid paying that price again.

## The two defects, and why they are separable

`pkg/block/journal/carve.go`, current develop:

| # | Defect | Site | Touches the flip chain? |
| --- | --- | --- | --- |
| 1 | Runs upload serially — `carveRun` ends by waiting for its own commits | `carve.go:448` | **no** |
| 2 | A block is cut at every run boundary, so a 4 KiB run commits its own remote object | `carve.go:424`, tail flush at `:447` | **yes** |

#1875 assumed they were one change, because it fixed both by hoisting the block builder *and* the
dispatcher to file scope — and a block spanning runs forces `flipUpTo` (`carve.go:574`) to walk the
whole snapshot with one shared `flipIdx`. That is the part that broke.

Defect 1 alone can be fixed with `flipUpTo` untouched, because each run already carries its own
dispatcher and its own `flipIdx` over its own slice (`carve.go:273`, `:281`). Defect 1 is also the
one that makes the drain *converge*, which is the user-visible bug. Defect 2 only makes it faster.

## Step 0 — the regression test that was missing

`pkg/block/journal/carve_scatter_test.go` (new; the same file name exists on the closed
`perf/1872-carve-pack-across-runs` branch and its two tests are worth salvaging as-is).

Assert that after carving a scattered dirty set, `UnsyncedBytes() == 0`, under a sink that mimics
the real commit path rather than a fake that always succeeds. #1875's tests asserted block count
and peak commit concurrency; both passed while the drain looped forever, which is exactly the
failure this must catch.

Bar: this test fails on `perf/1872-carve-pack-across-runs` and passes on develop. Verify both before
moving on — a test that passes on the known-bad branch is worth nothing here.

## Step 1 — parallelise runs (no flip changes)

**`carve.go:246`** — the run-splitting loop calls `carveRun` serially. Replace with a bounded
errgroup over the run slices. Runs are disjoint by construction, and each already owns its
`flipIdx`, its dispatcher and its `newOffsets`.

**`carve_dispatch.go:60`** — `sem` is built per dispatcher, so N concurrent runs would multiply
peak carve RAM by N. Hoist it: build one `chan struct{}` in `carveFile` and pass it to
`newCarveDispatcher`. This *restores* the documented contract — `store.go:67` already says
`CarveUploadConcurrency` bounds how many of **one file's** blocks are in flight, which per-run
semantics only accidentally satisfied. Peak RAM stays `cap(sem) x (CarveBlockSize + overhang)`.

Sharp edges, all bounded and none on the flip path:

- **Shared records across adjacent runs.** `extendRunToRowEnd` (`carve.go:264`) can pull two
  adjacent runs into the same manifest row, and `flipUpTo`'s `recordHasDirtyFragment` check reads
  fragments of a record another run may be flipping. Both take `sh.mu`; confirm the read-modify-write
  of the flags byte stays atomic under that lock with two runs in flight, and that
  `recordHasDirtyFragment` observing a *partially* flipped record is safe (it should be — it skips,
  and the losing run re-checks on its own flip).
- **`res *CarveResult`.** `BlocksWritten` / `BytesCarved` are incremented from run goroutines
  (`carve.go:263` signature). Give each run its own result and merge after the group, rather than
  adding a mutex to a struct on a hot-ish path.
- **`ReapSupersededManifest`** (`carve.go:452`) now runs concurrently per run. Runs are disjoint
  spans and each passes its own `newOffsets`, so a reap cannot delete a sibling's fresh rows — but
  #1979 and #2049 both landed in this area since #1875 was written, so re-read the row-straddling
  argument against current code rather than against the PR description.
- **Error semantics.** Today the first failing run aborts the file (`carve.go:251`). Keep that:
  collect the first error, let in-flight runs drain, leave the rest dirty.

Verification: full `pkg/block/journal` suite (it covers flip ordering, crash-mid-commit re-carve and
dedup), `go test -race ./pkg/block/...`, and step 0's test.

## Step 2 — measure on the rig

The probe that motivated #1872: `dittofs-s3-writeback`, `large`, `rand-write-4k` + `seq-read`,
sampling the store every 30s through the drain. Baseline is develop's progression
(49 -> 34 -> 45 -> 22 -> 2 -> 2 MiB per 30s).

This settles the question that decides step 3: is the drain **round-trip-bound** (step 1 fixes it)
or **bandwidth-bound on 4 KiB objects** (only step 3 helps)? 68 KiB/s at ~21 PUTs/s says round-trip,
but that is one sample on one VM.

Rig discipline: fresh SCW VM, verify `.bench-vm.json` `server_id` is not a coder VM before any
teardown, heartbeat-monitor the run.

## Step 3 — block packing across runs (conditional)

Only if step 2 shows small-object count still dominating. This is #1875's diff, and its entry price
is root-causing that stall on hardware first — not re-deriving the watermark-ordering argument,
which read as correct and was not. Note the obvious theory is already dead: `fileOff` *is* reset per
run on that branch (line 324), so the watermarks did ascend.

Do not rebase the branch. 19 journal commits have landed on `carve.go` since (#1979, #2049, #1988,
#2015, #2064); rewrite from the diagnosis.

## What is deliberately not done

No coalescing of runs across gaps (feeding the chunker already-synced resident bytes so a scattered
neighbourhood becomes one run). It is appealing — it fixes both defects with `flipUpTo` untouched,
because already-synced intervals flip to a no-op — but its coverage depends on how much of the gap
is still resident rather than reclaimed or cold, which is unknown and workload-dependent. Revisit
only if step 2 says step 3 is needed and the flip stall stays unexplained.
