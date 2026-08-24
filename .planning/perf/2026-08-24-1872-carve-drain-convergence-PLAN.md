# Carve drain convergence — implementation plan (#2070, closes #1872)

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development (recommended)
> or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make a randomly-written share's drain converge, by uploading a file's dirty runs
concurrently instead of one after another.

**Architecture:** `carveFile` currently calls `carveRun` serially, and each `carveRun` ends by
waiting for its own commits (`carve.go:448`), so a scattered dirty set costs one serial remote
round-trip per run. Runs become concurrent under a bounded errgroup. Each run keeps its own
dispatcher, its own `flipIdx` and its own slice, so **`flipUpTo` is not touched** — that is the
whole point of this shape, and the reason it is not the closed PR #1875. Number of remote objects
is unchanged; only their serialisation goes away.

**Tech stack:** Go, `golang.org/x/sync/errgroup` (already a dependency, `go.mod:40`), badger
metadata store, `pkg/block/journal` + `pkg/block/engine`.

**Spec:** GitHub issue #2070. Defect report: #1872. Failed prior attempt: #1875 (closed).

## Global constraints

- **`flipUpTo` (`carve.go:574`) must not change in this plan.** It sets the on-disk synced bit;
  getting it wrong is silent data loss on recovery (#1850). Any task that finds itself editing it
  has left this plan — stop and re-scope.
- Peak carve RAM must stay `cap(sem) x (CarveBlockSize + one overhang chunk)`. This bound is the
  reason `sem` is hoisted rather than duplicated per run.
- No new config field. Run-level concurrency reuses `CarveUploadConcurrency`.
- Every task ends with a signed commit (`git commit -S`; if the agent socket fails, use
  `git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S`).
- `go test -race ./pkg/block/...` green is part of "done" for every task, not just the last.

## What is already known and must not be re-derived

- **The two defects are separable.** Serial runs (`carve.go:448`) and one-block-per-run
  (`carve.go:424`) are independent; only the second forces the flip chain to span runs. This plan
  fixes the first one only.
- **There is no reproduction of the #1875 stall at any level.** `carve_scatter_test.go` on
  `perf/1872-carve-pack-across-runs` writes 200 non-contiguous 4 KiB runs and asserts
  `post-carve unsynced want 0` — verified passing on the branch that stalls on hardware
  (`ok github.com/marmos91/dittofs/pkg/block/journal 0.905s`). A unit-level scattered carve
  asserting convergence does **not** reproduce it. Building a reproduction is the entry price for
  the conditional block-packing work at the end of this plan, not a warm-up for Task 1.
- **`sh.carveMu` already serialises the whole per-shard carve pass** (`carve.go:177`, `shard.go:54`),
  and `Carve`'s shard and file loops are both sequential — so at most one `carveFile` is active
  system-wide. Every race below is between goroutines of a *single* `carveFile` call.

## File structure

| File | Change | Responsibility |
| --- | --- | --- |
| `pkg/block/journal/carve.go` | modify `carveFile` (230-258), `carveRun` (263-463) | run-level concurrency; per-run result |
| `pkg/block/journal/carve_dispatch.go` | modify `newCarveDispatcher` (48-63) | accept a caller-owned `sem` |
| `pkg/block/journal/store.go` | comment only (67-73) | `CarveUploadConcurrency` now spans a file's runs |
| `pkg/block/engine/blocksink.go` | modify both `ReapSupersededManifest` (154-161, 195-199) | take the striped commit lock |
| `pkg/block/engine/blocksink_ssi_test.go` | add one test | reap-vs-commit SSI conflict |
| `pkg/block/journal/carve_concurrent_test.go` | create | scattered carve converges under concurrent runs |

---

### Task 1: Give each run its own `CarveResult`

`res.BytesCarved += int64(boundary)` (`carve.go:416`) and `res.BlocksWritten++`
(`carve_dispatch.go:145`) run in each run's own goroutine against one shared pointer
(`carve.go:251`). Within a run the `prev`/`mine` chain orders the dispatcher's writes; across runs
there is no happens-before at all. **This is a confirmed race, not a hypothetical** — it fires the
moment Task 4 lands, so it is fixed first, while callers are still serial and nothing can regress.

**Files:**
- Modify: `pkg/block/journal/carve.go:263` (signature), `:416`, `:432-443` (early return), `:246-255` (caller)

**Interfaces:**
- Produces: `func (s *Store) carveRun(ctx context.Context, sh *shard, id FileID, run []interval) (CarveResult, error)`

- [ ] **Step 1: Change `carveRun` to own its result**

```go
func (s *Store) carveRun(ctx context.Context, sh *shard, id FileID, run []interval) (CarveResult, error) {
	var res CarveResult
	// ... unchanged body; pass &res into newCarveDispatcher
```

- [ ] **Step 2: Return the partial result on the failure path too**

The early return at `carve.go:432-443` must return `(res, err)`, never `(CarveResult{}, err)` —
blocks that committed before the failure are already durable and today's caller counts them.

```go
	if packErr != nil || disp.aborted() {
		disp.discard(arenap, arena)
		if err := disp.wait(); err != nil {
			return res, err
		}
		return res, packErr
	}
```

- [ ] **Step 3: Sum in `carveFile`**

```go
		r, err := s.carveRun(ctx, sh, id, snap[start:end])
		res.BlocksWritten += r.BlocksWritten
		res.BytesCarved += r.BytesCarved
		if err != nil {
			return err
		}
```

- [ ] **Step 4: Verify no behaviour changed**

Run: `go test -race -count=1 ./pkg/block/journal/`
Expected: PASS. Callers are still serial, and a lone run still gets the full `sem` capacity, so
`TestCarveRoundTripAndFlip`, `TestCarveCommitStrictlyBeforeFlip`, `TestCarveSinkErrorLeavesDirty`,
`TestCarveDedupReCarveIsNoOp` and the four `carve_dispatch_test.go` tests must pass unchanged. Any
failure here is a transcription error in the refactor, not a real finding.

- [ ] **Step 5: Commit**

```bash
git add pkg/block/journal/carve.go
git commit -S -m "refactor(journal): give each carve run its own CarveResult"
```

---

### Task 2: Hoist `sem` to the file

`sem` is built inside `newCarveDispatcher` (`carve_dispatch.go:60`), so N concurrent runs would
multiply peak carve RAM by N. `store.go:67` already documents `CarveUploadConcurrency` as bounding
**one file's** in-flight blocks — per-run ownership only satisfied that by accident, because runs
were serial. Hoisting restores the documented contract.

**Files:**
- Modify: `pkg/block/journal/carve_dispatch.go:48-63`, `pkg/block/journal/carve.go` (`carveFile`, `carveRun`), `pkg/block/journal/store.go:67-73` (comment)

**Interfaces:**
- Consumes: Task 1's `carveRun` signature.
- Produces: `func newCarveDispatcher(ctx context.Context, s *Store, sh *shard, id FileID, run []interval, res *CarveResult, flipIdx *int, sem chan struct{}) *carveDispatcher`
  and `func (s *Store) carveRun(ctx context.Context, sh *shard, id FileID, run []interval, sem chan struct{}) (CarveResult, error)`

- [ ] **Step 1: Take the semaphore as a parameter**

```go
	return &carveDispatcher{
		ctx: ctx, s: s, sh: sh, id: id, run: run, res: res, flipIdx: flipIdx,
		sem: sem, prev: prev,
	}
```

Nothing else in the dispatcher assumes exclusive ownership: `acquire` (`:71`), `discard` (`:163`),
the deferred release in `commitAndFlip` (`:113`) and the bare-watermark acquire in `submit` (`:96`)
are all plain sends/receives on a bounded channel. `prev`/`mine`, `res`, `flipIdx` and `run` stay
per-dispatcher.

- [ ] **Step 2: Build it once per file**

```go
	// One semaphore for the whole file: it bounds in-flight blocks (and so peak
	// carve RAM) across every run, not per run.
	sem := make(chan struct{}, s.cfg.CarveUploadConcurrency)
```

- [ ] **Step 3: Update the `CarveUploadConcurrency` comment**

`store.go:67` — say the bound spans a file's carve pass including all its runs. Record the
trade-off this creates, since it is a deliberate ceiling:

```go
	// ponytail: with runs concurrent, a file at the limit can leave each run
	// holding a single slot, so the intra-run block overlap degrades under
	// multi-run contention; split into a separate CarveRunConcurrency only if
	// profiling shows the two dimensions need to diverge.
```

- [ ] **Step 4: Verify**

Run: `go test -race -count=1 ./pkg/block/journal/`
Expected: PASS, unchanged. Pure plumbing — callers are still serial.

- [ ] **Step 5: Commit**

```bash
git add pkg/block/journal/carve.go pkg/block/journal/carve_dispatch.go pkg/block/journal/store.go
git commit -S -m "refactor(journal): bound in-flight carve blocks per file, not per run"
```

---

### Task 3: Take the commit lock in `ReapSupersededManifest`

**This is the hazard the first draft of this plan missed, and it is real.** `CommitBlock` routes
through `commitManifestRows(ctx, s.committer, s.commitLocks, ...)` (`blocksink.go:147`, `:238`),
which takes a striped per-file mutex. Both `ReapSupersededManifest` impls (`:158`, `:196`) call
`s.committer.WithTransaction` **directly, with no lock** — verified in current code. Reap ends in
`ProjectManifestToBlocks` (`pkg/metadata/block_record_store.go:128`), a read-modify-write of the
same `File.Blocks` row that `carveCommitLocks` exists to serialise; its own doc comment
(`blocksink.go:26`) names the failure — badger SSI conflict, retry budget exhausted, EDEADLK at the
client.

Serial runs made this unreachable: a run reaps only after its own `disp.wait()` drained, and no
other run had started. Concurrent runs break it two ways — reap vs sibling reap, and reap vs a
**still-in-flight sibling's `CommitBlock`**. Under exactly the workload this plan targets (many
small runs on one file) that turns "drains slowly" into "churns retries", which is the failure mode
the plan exists to remove. Land the lock **before** anything calls the path concurrently.

**Files:**
- Modify: `pkg/block/engine/blocksink.go:154-161` (localBlockSink), `:195-199` (engineBlockSink)
- Test: `pkg/block/engine/blocksink_ssi_test.go`

- [ ] **Step 1: Write the failing test**

Model it on `TestLocalBlockSink_ConcurrentSameFileCommit_NoSSIConflict` (`blocksink_ssi_test.go:69`),
which already provides `newBadgerCommitter(t)` and the `TransactionConflictsForTest()` assertion.
Drive `CommitBlock` and `ReapSupersededManifest` for the same `payloadID` concurrently from a
released-together goroutine set:

```go
func TestLocalBlockSink_ConcurrentReapAndCommit_NoSSIConflict(t *testing.T) {
	ctx := context.Background()
	store, pid := newBadgerCommitter(t)
	sink := localBlockSink{committer: store, commitLocks: &carveCommitLocks{}}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				data := []byte{byte(i), byte(i >> 8), 0xab, 0xcd}
				errs[i] = sink.CommitBlock(ctx, []journal.CarveChunk{{
					FileID:     journal.FileID(pid),
					FileOffset: int64(i) * 4096,
					Hash:       journal.ChunkHash(blake3.Sum256(data)),
					Data:       data,
				}})
				return
			}
			// A disjoint span from every commit above, so a correct
			// implementation serialises only the shared File-row write.
			off := int64(i) * 4096
			errs[i] = sink.ReapSupersededManifest(ctx, journal.FileID(pid), off, off+4096, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent carve metadata write %d surfaced an error", i)
	}
	require.Zero(t, store.TransactionConflictsForTest(),
		"a reap racing a sibling run's commit must not conflict on the File row")
}
```

- [ ] **Step 2: Run it and confirm it fails for the right reason**

Run: `go test -race -count=1 -run TestLocalBlockSink_ConcurrentReapAndCommit ./pkg/block/engine/`
Expected: FAIL on the conflict counter, not on an error return — badger retries absorb the
conflict, so the count is the signal. If it passes first time, do **not** proceed: either the
contention window is too narrow (raise `n`, tighten the offsets onto one row) or the premise is
wrong and Task 3 should be dropped. Record which.

- [ ] **Step 3: Take the lock in both impls**

Mirror `commitManifestRows` (`blocksink.go:123-141`) exactly — the nil-receiver `forKey` already
makes this a no-op for the fixtures that wire no stripes.

```go
	if mu := s.commitLocks.forKey(string(id)); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return s.committer.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ReapSupersededManifest(ctx, tx, string(id), runStart, runEnd, newOffsets)
	})
```

- [ ] **Step 4: Verify**

Run: `go test -race -count=1 ./pkg/block/engine/`
Expected: PASS, including the pre-existing `TestLocalBlockSink_ConcurrentSameFileCommit_NoSSIConflict`.

- [ ] **Step 5: Commit**

```bash
git add pkg/block/engine/blocksink.go pkg/block/engine/blocksink_ssi_test.go
git commit -S -m "fix(engine): hold the per-file commit lock while reaping superseded manifest rows"
```

---

### Task 4: Run a file's runs concurrently

**Files:**
- Modify: `pkg/block/journal/carve.go:230-258`
- Create: `pkg/block/journal/carve_concurrent_test.go`

**Interfaces:**
- Consumes: Task 1's `(CarveResult, error)` return, Task 2's `sem` parameter.

- [ ] **Step 1: Write the failing test**

The convergence assertion alone passes even on the stalling branch (see "What is already known"), so
it is a floor, not the point. What is new here is that it must hold with runs *interleaved*, under
`-race`, which is what catches the `CarveResult` race if Task 1 was botched and the reap conflict if
Task 3 was.

```go
// TestCarveScatteredRunsConvergeConcurrently pins that a scattered dirty set still
// drains to zero when its runs execute concurrently: every record flips synced, the
// carved bytes match, and -race sees no shared state across runs.
func TestCarveScatteredRunsConvergeConcurrently(t *testing.T) {
	const (
		runs      = 200
		runSize   = 4 << 10
		gap       = 8 << 10 // stride > runSize keeps every run non-contiguous
		totalSize = runs * runSize
	)
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 8})
	ctx := context.Background()

	want := map[int64][]byte{}
	for i := 0; i < runs; i++ {
		off := int64(i) * gap
		b := randBytes(runSize, int64(i))
		if err := s.WriteAt(ctx, "f", off, b); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		want[off] = b
	}
	if s.UnsyncedBytes() != int64(totalSize) {
		t.Fatalf("pre-carve unsynced=%d want %d", s.UnsyncedBytes(), totalSize)
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BytesCarved != int64(totalSize) {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, totalSize)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
	for off, b := range want {
		if got := sink.chunkAt(off); got == nil {
			t.Fatalf("no committed chunk at %d", off)
		} else if string(got) != string(b) {
			t.Fatalf("committed bytes at %d differ", off)
		}
	}
	for off := range want {
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}
```

`sink.chunkAt` does not exist yet — add it next to `fakeSink.carved` (`carve_test.go:99`) as a
locked read of `s.chunks[off]`. `recRawFlags` already exists (`carve_test.go:151`); the per-offset
on-disk check is what makes this stronger than counting bytes, because `s.unsynced` is an in-memory
counter and the durable bit is the thing recovery reads.

- [ ] **Step 2: Run it against the serial implementation**

Run: `go test -race -count=1 -run TestCarveScatteredRunsConvergeConcurrently ./pkg/block/journal/`
Expected: PASS (runs are still serial at this point). This is deliberate — the test is a
**regression guard for Step 3**, not a red-then-green driver. Confirming it green here is what makes
a failure after Step 3 attributable to concurrency.

- [ ] **Step 3: Make the runs concurrent**

Follow the existing precedent at `pkg/block/engine/fetch.go:29-37`.

```go
	// Runs are disjoint by construction (extendRunToRowEnd stops at any non-warm
	// interval), so they share no intervals, no records to pack and no result.
	// Each keeps its own dispatcher and flipIdx: the flip chain stays per-run.
	sem := make(chan struct{}, s.cfg.CarveUploadConcurrency)
	g, gctx := errgroup.WithContext(ctx)
	// ponytail: one knob bounds both goroutines and the per-run
	// ManifestRowEndAfter transactions; give runs their own limit only if
	// profiling shows upload and metadata fan-out need to diverge.
	g.SetLimit(s.cfg.CarveUploadConcurrency)

	results := make([]CarveResult, len(runs))
	for i, run := range runs {
		g.Go(func() error {
			r, err := s.carveRun(gctx, sh, id, run, sem)
			results[i] = r
			return err
		})
	}
	err := g.Wait()
	for _, r := range results {
		res.BlocksWritten += r.BlocksWritten
		res.BytesCarved += r.BytesCarved
	}
	if err != nil {
		return err
	}
	s.maybeResetDirtyClock(sh, id)
	return nil
```

Extract the existing `start`/`end` splitting loop (`carve.go:246-255`) into a `splitRuns(snap)
[][]interval` helper so the errgroup reads as a fan-out over runs.

Notes on why this is enough, so nobody adds more:
- Distinct `results[i]` per goroutine plus `g.Wait()`'s barrier is not a race.
- `errgroup.WithContext` gives first-error-aborts-the-rest for free: `gctx` cancellation trips each
  run's existing `ctx.Err()` checks (`carve.go:345`, `:351`), and each run drains **itself** through
  the unchanged `disp.discard` + `disp.wait()` path (`:432-443`). `g.Wait()` returns the first real
  error, not the `context.Canceled` siblings see — same error as today.
- `maybeResetDirtyClock` stays single-threaded after `Wait`.

- [ ] **Step 4: Verify under race**

Run: `go test -race -count=1 ./pkg/block/journal/ ./pkg/block/engine/`
Expected: PASS. Specifically re-check `TestCarveRecordSplitNoPrematureFlip` (`carve_test.go:344`),
`TestCarveFlipsInWatermarkOrder` and `TestCarveConcurrentCommitErrorStopsWatermark`
(`carve_dispatch_test.go:229`, `:272`) — the closest existing coverage of the flip chain.

- [ ] **Step 5: Settle the shared-record premise**

`flipUpTo` holds `sh.mu` across its whole body, so "mark fragment synced, check
`recordHasDirtyFragment`, maybe flip" is atomic no matter which run's goroutine calls it — two runs
that are fragments of one physical record are safe, and only the run observing zero remaining dirty
fragments flips. That argument is sound but **nothing currently constructs that shape across runs**.

Try to build it: one large write, then overwrites that leave two non-adjacent dirty fragments of the
original record with a warm (already-synced) gap between them. If it can be constructed, add it to
`carve_concurrent_test.go`. **If it cannot be reached through the public API, say so in the commit
message and on #2070** — an unreachable case is a real finding, and the memory rule is to test the
premise rather than build on it.

- [ ] **Step 6: Commit**

```bash
git add pkg/block/journal/carve.go pkg/block/journal/carve_concurrent_test.go pkg/block/journal/carve_test.go
git commit -S -m "perf(journal): carve a file's dirty runs concurrently"
```

---

### Task 5: Measure on the rig

Unit tests cannot answer whether this fixed #1872 — the whole defect is remote round-trip latency,
which every fake sink elides. **This task is part of "done", not a follow-up.**

- [ ] **Step 1: Fresh bench VM.** Verify `.bench-vm.json` `server_id` is not a coder VM before any
      teardown. Heartbeat-monitor the run; the `--remote` poller hangs on a stall.
- [ ] **Step 2: Run the probe that motivated #1872** — `dittofs-s3-writeback`, `large`,
      `rand-write-4k` + `seq-read`, sampling the store every 30s through the drain.
- [ ] **Step 3: Compare against develop's baseline** — 49 → 34 → 45 → 22 → 2 → 2 MiB per 30s.
- [ ] **Step 4: Post the numbers on #2070**, whatever they show. The #1875 comment is the standard:
      report what the rig did, not what the object-count reduction implies.

**This measurement decides the next question and nothing else does:** is the drain round-trip-bound
(this plan fixes it, close #1872) or bandwidth-bound on 4 KiB objects (it does not, and the packing
work below becomes necessary)? The 68 KiB/s at ~21 PUTs/s in the #1875 probe says round-trip, but
that is one sample on one VM.

---

## Gate: block packing across runs (only if Task 5 says so)

Out of scope here. It is #1875's diff, it forces the flip chain to span runs, and its entry price is
a **reproduction of the #1875 stall**, which does not exist at any level today. The gap between the
passing unit test and the stalling rig is where to look:

| | unit test | rig |
| --- | --- | --- |
| files / shards | one / one | many |
| record shape | one interval per record | records carrying several fragments |
| prior state | fresh store | interleaved synced + cold intervals |
| row straddling | none | `extendRunToRowEnd` in play |
| dedup oracle | empty fake | real manifest lookups |
| invocation | one `Carve(Force:true)` | `Force:true` in a drain retry loop |
| scale | 800 KiB | 644 MiB |

Do not rebase `perf/1872-carve-pack-across-runs` — 19 journal commits have landed on `carve.go`
since (#1979, #2049, #1988, #2015, #2064). Keep it as the reproduction target; a worktree is at
`~/dittofs-worktrees/1875-repro`.

Also parked: coalescing runs across gaps by feeding the chunker already-synced resident bytes, so a
scattered neighbourhood becomes one run. It fixes both defects with `flipUpTo` untouched, because
already-synced intervals flip to a no-op — but its coverage depends on how much of each gap is still
resident rather than reclaimed or cold, which is unknown and workload-dependent. Only worth costing
if the gate opens and the stall stays unexplained.
