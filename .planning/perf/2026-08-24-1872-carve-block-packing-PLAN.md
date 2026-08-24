# Carve block packing across runs — implementation plan (#1872)

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development (recommended)
> or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make a randomly-written share's writeback drain converge, by letting one packed remote
block span several dirty runs instead of flushing at every run's end.

**Architecture:** One sequential packer per file. `carveRun` splits into three phases that live in
`carveFile`: **A** resolves every run's extents with the existing concurrent `ManifestRowEndAfter`
fan-out, **B** streams all runs in offset order through one block accumulator that flushes only at
`CarveBlockSize`, **C** reaps per run once that run is fully flipped. Chunks stay inside runs (a
chunker is created per run, so a chunk never spans a gap); only *blocks* span runs. `flipUpTo` is
byte-identical — its loop condition is purely `run[*flipIdx].end() <= watermark` over an
offset-ordered slice, so it works unchanged once each run keeps its own `flipIdx`.

**Tech stack:** Go, `pkg/block/journal` (carve path), `pkg/block/engine` (sink), badger metadata
store. No new dependency. No change to `pkg/block/engine`, `pkg/block/blockcodec`,
`pkg/block/remote` or `pkg/metadata` for the packing itself.

**Spec:** GitHub issue #1872. Predecessor plan (shipped, closed):
`.planning/perf/2026-08-24-1872-carve-drain-convergence-PLAN.md` → #2070, PRs #2071 + #2072.
Failed prior attempt: #1875 (closed). Long-horizon design: #1414 (block-level sync state).

---

## Global constraints

- **`flipUpTo` (`carve.go:~600`) must not change.** It sets the on-disk synced bit; getting it wrong
  is silent data loss on recovery. #1850, #1879 and #1956 are three separate field incidents of
  exactly this class. Any task that finds itself editing its body has left this plan — stop and
  re-scope. Changing *which slice and which `flipIdx`* are passed to it is in scope; changing what it
  does with them is not.
- **The four reap-safety tests must pass unmodified**: `TestCarveRunExtendsToStraddledRowEnd`,
  `TestCarveRunDoesNotExtendIntoDirtyRange` (`carve_runextend_test.go`),
  `TestCarveRunDoesNotExtendPastNextRun`, `TestCarveReapSurvivesSiblingFailure`
  (`carve_concurrent_test.go`). If any of them needs editing, the design has drifted and the change
  is wrong. Stop and report rather than adjusting the test.
- **`extendRunToRowEnd` keeps its body and its refusal semantics.** It moves call site only.
- Peak carve RAM must not grow. It is `cap(sem) x (CarveBlockSize + one overhang chunk)` for arenas
  plus chunker scratch; this plan *reduces* the scratch term from one buffer per concurrent run to
  one buffer total.
- No new config field in Tasks 1-5. `CarveUploadConcurrency` keeps its meaning.
- Every task ends with a signed commit (`git commit -S`; if the agent socket fails, use
  `git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S`).
- `go test -race ./pkg/block/...` green is part of "done" for every task, not just the last.
  Use a timeout above the default: `go test -race -timeout 10m`.

---

## What is already established — do not re-derive

Each of these was verified against the code on `develop` at `4f2fe25b`.

1. **Object size is the binding constraint; request concurrency is not.** Measured on one SCW VM
   against a real S3 endpoint: PUT latency is flat at ~0.4-0.5 s *regardless of object size*, the
   link runs at ~1.7% utilisation, and drain objects are quantized on the wire at 4168 / 6216 /
   8264 B. 935 MiB at that granularity is ~240,000 PUTs (~68 min); the same bytes as 4 MiB objects
   is 234 PUTs (~70 s). **Do not propose or accept a concurrency-shaped fix.**
2. **The sink is already a packer.** `blocksink.go:255-300` frames N chunks into one
   `blockcodec.Builder` and issues exactly one `PutBlock`; `blockcodec` has no chunk-count cap, and
   `fetch.go:336` already reads back per-chunk via ranged `ReadChunk(BlockID, WireOffset,
   WireLength)`. **The packing change is confined to `pkg/block/journal`.**
3. **The root cause is two lines.** `carve.go:518` flushes unconditionally at the run's end, and the
   `eof` path (`carve.go:445-451`) force-cuts a sub-minimum remainder into its own chunk. With
   `defaultCarveBlockSize` at 4 MiB and runs a few KiB wide, the run-end flush always wins.
4. **Tiny runs are correct, not a defect.** `index.go` case (b) — exact-range overwrite — replaces
   one interval in place and returns before `remarkFragmentsDirty` (only reachable at `:189`/`:200`).
   A 4K-aligned rewrite over a 4K-laid-out file therefore dirties exactly one 4 KiB interval and no
   neighbour, which is right: the neighbours' remote bytes are still valid. This explains the
   4/6/8 KiB objects exactly, and it means **the fix belongs in the packer, not in the index.**
5. **Gap residency is already queryable in memory.** On an `interval`, `synced && !cold` = warm and
   locally resident, `synced && cold` = remote-only, and *no interval at all* = a true hole. The
   query is implemented: `warmTail` (`carve.go:616-641`) returns exactly the bridgeable set. This
   matters only for the parked Phase 3 below.
6. **`sh.carveMu` serialises the whole per-shard carve pass**, and `Carve`'s shard and file loops are
   sequential, so at most one `carveFile` is active system-wide.

---

## Ruling: `TestCarveRunsCommitConcurrently` is replaced, not preserved

`carve_concurrent_test.go:422` asserts `maxRuns >= 2` — "the runs did not overlap". It shipped in
#2072 and it pins run-goroutine overlap, which this plan deliberately removes.

**It must not be left in place and it must not be quietly deleted.** Under this design its geometry
(`runSize` 32 KiB > `CarveBlockSize` 8 KiB) means blocks never span runs, so adjacent runs' commits
still overlap *incidentally* through the in-flight window — the assertion would pass on timing rather
than on structure, which is worse than failing, because it would keep reporting green while measuring
nothing. Replace it with a floor on **blocks** in flight (`max >= 2`), which is the property that
survives, and keep its existing `cur > window` ceiling assertion unchanged. Task 3 does this, and the
PR body must say so explicitly.

## Ruling: #2072's run fan-out is given back, on purpose

Say this plainly in the PR, do not bury it. #2072 bought ~7x on a harness by overlapping round trips.
This plan makes each round trip carry ~1000x more payload, so that axis stops being the binding one.
Of #2072, precisely:

- **Removed** — run-level concurrency of chunking, BLAKE3 hashing, `IsChunkDurable` and local record
  reads. These return to one goroutine.
- **Survives** — the concurrent `ManifestRowEndAfter` fan-out (the per-run metadata round trip, the
  expensive part) becomes Phase A with the same errgroup and the same limit.
- **Survives and becomes reachable for the first time** — the file-wide `sem` from #2072 Task 2. With
  serial packing it degenerated to "one run at a time gets the whole window"; with one packer it does
  what its doc says and bounds blocks in flight across the pass.
- **Still required** — #2071's striped reap lock. `CommitBlock` and the reap still share the `File`
  row.

## Accepted ceiling: reclaim granularity (R6)

A block is reclaimed only when its last member chunk dies (`DecrLiveChunkCount` reaching 0). Today's
~4-chunk blocks die readily. A ~1000-chunk block on a random-overwrite workload effectively never
does: its members are the chunks that happened to be dirty in the same carve window, i.e. a random
scatter across the file, so their death times are maximally decorrelated. **Remote space will track
bytes written rather than bytes live until a low-liveness repack exists (#1414 / #1715).**

This is not a regression this plan introduces — it is the same effect at a magnitude that starts to
matter. It ships as a named `ponytail:` ceiling with an observable trigger (Task 1), not as a
pretended bound. It is also the honest reason #1414 is real work: it is just not work that closes
#1872.

---

## File structure

| File | Change | Responsibility |
| --- | --- | --- |
| `pkg/block/journal/carve_pack.go` | create | `runState` + `flipPlan` types and their arithmetic |
| `pkg/block/journal/carve.go` | modify `carveFile`, split `carveRun` into `resolveRunExtents` + `packRuns` | phase A/B/C sequencing |
| `pkg/block/journal/carve_dispatch.go` | modify struct, `newCarveDispatcher`, `submit`, `commitAndFlip` flip branch | flip across a multi-run block |
| `pkg/block/journal/store.go` | comment only | `CarveUploadConcurrency` meaning; drop the stale `ponytail:` |
| `pkg/block/journal/carve_pack_test.go` | create | packing, cross-run flip, seam failure, flipPlan arithmetic, scatter benchmark |
| `pkg/block/journal/carve_concurrent_test.go` | modify one test | replace the run-overlap floor with a block-overlap floor |
| `pkg/controlplane/runtime/blockgc_reconcile_reclaim.go` | modify | space-amplification gauge |

---

### Task 1: Observe the space-amplification ceiling before creating it

Ship the R6 observation **first**, so the ceiling this plan creates is measured from day one rather
than discovered in the field. `BlockRecord` (`pkg/block/block_record.go:4-10`) carries `Length` and
`LiveChunkCount` but no initial count, so a per-block liveness *ratio* would need a schema change.
It does not need one: the number that matters is total remote bytes against live logical bytes, and
`Length` alone gives that.

**Files:**
- Modify: `pkg/controlplane/runtime/blockgc_reconcile_reclaim.go`

**Interfaces:**
- Produces: a log line at each block-GC sweep reporting `remoteBytes`, `liveLogicalBytes`, and their
  ratio. No exported symbol; later tasks do not consume it.

- [ ] **Step 1: Find the sweep's existing block-record walk**

Read `pkg/controlplane/runtime/blockgc_reconcile_reclaim.go` in full and locate where it already
enumerates block records. Do **not** add a second full walk — accumulate into two `int64`s inside the
walk that is already there. If no such walk exists in that file, grep for `WalkBlockRecords` and put
the accumulator in the sweep that owns it. Record in the commit message which walk you used.

- [ ] **Step 2: Accumulate and log**

```go
	// ponytail: a scattered carve now packs ~1000 chunks into one block, and a
	// block is reclaimed only when its last member dies — under random overwrite
	// that is effectively never, so remote space tracks bytes written rather than
	// bytes live. This ratio is the trigger: upgrade to a low-liveness repack
	// (block-level sync state) when it stays above ~2.0 in the field.
	if liveLogical > 0 {
		logger.Info("block gc sweep: remote space amplification",
			"remote_bytes", remoteBytes,
			"live_logical_bytes", liveLogical,
			"ratio", float64(remoteBytes)/float64(liveLogical))
	}
```

The `2.0` threshold is a calibration knob to be replaced by the first real reading, not a derived
constant. Say so in the commit message.

- [ ] **Step 3: Verify**

Run: `go test -race -count=1 -timeout 10m ./pkg/controlplane/runtime/`
Expected: PASS. This task adds observation only; no test should change behaviour.

- [ ] **Step 4: Commit**

```bash
git add pkg/controlplane/runtime/blockgc_reconcile_reclaim.go
git commit -S -m "feat(gc): report remote space amplification at each block sweep"
```

---

### Task 2: Phase A — lift extent resolution out of the run (pure move)

No behaviour change. This isolates the mechanical restructuring so that when Task 3 changes
behaviour, a bisect lands on the right commit.

**Files:**
- Create: `pkg/block/journal/carve_pack.go`
- Modify: `pkg/block/journal/carve.go` (`carveFile` run loop; `carveRun`'s first statements)

**Interfaces:**
- Produces:
  ```go
  type runState struct {
      ivs        []interval
      flipIdx    int
      newOffsets map[int64]struct{}
  }
  func (r *runState) start() int64   // r.ivs[0].fileOff
  func (r *runState) end() int64     // r.ivs[len(r.ivs)-1].end()
  func (r *runState) complete() bool // r.flipIdx == len(r.ivs)

  type flipPlan struct {
      first, last int   // inclusive run indices this block contributed to
      lastOff     int64 // file offset packed into run `last`
  }

  func (s *Store) resolveRunExtents(ctx context.Context, sh *shard, id FileID, rs []*runState) error
  ```

- [ ] **Step 1: Create the types**

```go
package journal

// runState carries one dirty run through a carve pass: its live intervals (as
// widened by extendRunToRowEnd), how far its records have been flipped synced,
// and the file offsets the fresh tiling produced. flipIdx is advanced only by
// the dispatcher's flipping worker, one at a time via the prev/mine chain;
// newOffsets is written only by the single packer goroutine.
type runState struct {
	ivs        []interval
	flipIdx    int
	newOffsets map[int64]struct{}
}

func (r *runState) start() int64   { return r.ivs[0].fileOff }
func (r *runState) end() int64     { return r.ivs[len(r.ivs)-1].end() }

// complete reports whether every one of this run's records flipped synced, which
// is the precondition for reaping the rows the run superseded.
func (r *runState) complete() bool { return r.flipIdx == len(r.ivs) }

// flipPlan names the runs one packed block contributed to. A block flushes at
// CarveBlockSize, so it may cover the tail of one run, several whole runs, and a
// prefix of the last: runs first..last-1 flip to their own end, run last flips
// only up to lastOff.
type flipPlan struct {
	first, last int
	lastOff     int64
}
```

- [ ] **Step 2: Move the `extendRunToRowEnd` call up into a phase**

In `carve.go`, after `runs := splitRuns(snap)`, build `rs` and replace the existing `g.Go` body so it
calls only the extent resolution. The `limit` computation stays exactly as it is.

```go
	rs := make([]*runState, len(runs))
	for i, run := range runs {
		rs[i] = &runState{ivs: run, newOffsets: map[int64]struct{}{}}
	}
	if err := s.resolveRunExtents(ctx, sh, id, rs); err != nil {
		return err
	}
```

```go
// resolveRunExtents widens each run to end on a manifest row boundary, so the
// reap that follows never deletes a row straddling the run's tail. The
// ManifestRowEndAfter lookups are a per-run metadata round trip, so they fan out
// under the same bound as block commits.
func (s *Store) resolveRunExtents(ctx context.Context, sh *shard, id FileID, rs []*runState) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.CarveUploadConcurrency)
	for i := range rs {
		limit := int64(math.MaxInt64)
		if i+1 < len(rs) {
			limit = rs[i+1].start()
		}
		g.Go(func() error {
			ivs, err := s.extendRunToRowEnd(gctx, sh, id, rs[i].ivs, limit)
			if err != nil {
				return err
			}
			rs[i].ivs = ivs
			return nil
		})
	}
	return g.Wait()
}
```

Then delete the `extendRunToRowEnd` call from the top of `carveRun` (currently `carve.go:335-338`)
and have `carveFile` pass `rs[i].ivs` where it passed `run`. `carveRun` still runs per-run and still
returns `(CarveResult, error)` at this point — Task 3 is what merges it.

- [ ] **Step 3: Verify nothing moved but code**

Run: `go test -race -count=1 -timeout 10m ./pkg/block/journal/ ./pkg/block/engine/`
Expected: PASS, every test unchanged — including all four reap-safety tests and
`TestCarveRunsCommitConcurrently`, which is still valid at this point because runs are still
concurrent. Any failure here is a transcription error, not a finding.

- [ ] **Step 4: Commit**

```bash
git add pkg/block/journal/carve_pack.go pkg/block/journal/carve.go
git commit -S -m "refactor(journal): resolve every dirty run's extents before packing"
```

---

### Task 3: Phase B — one packer whose blocks span runs

This is the behaviour change and the point of the plan.

**Files:**
- Modify: `pkg/block/journal/carve.go` (`carveRun` → `packRuns`, `carveFile` call site, RAM comment)
- Modify: `pkg/block/journal/carve_dispatch.go` (struct fields, constructor, `submit`, flip branch)
- Modify: `pkg/block/journal/store.go` (`CarveUploadConcurrency` doc)
- Create: `pkg/block/journal/carve_pack_test.go`
- Modify: `pkg/block/journal/carve_concurrent_test.go` (the one replaced test)

**Interfaces:**
- Consumes: Task 2's `runState`, `flipPlan`, `resolveRunExtents`.
- Produces:
  ```go
  func (s *Store) packRuns(ctx context.Context, sh *shard, id FileID, rs []*runState) (CarveResult, error)
  func newCarveDispatcher(ctx context.Context, s *Store, sh *shard, id FileID, rs []*runState, res *CarveResult, sem chan struct{}) *carveDispatcher
  func (d *carveDispatcher) submit(chunks []CarveChunk, arenap *[]byte, arena []byte, plan flipPlan)
  ```

- [ ] **Step 1: Write the failing test**

```go
// TestCarvePackReachesBlockSizeOnScatteredRuns pins the defect this plan fixes:
// a scattered dirty set must coalesce into one remote block, not one per run.
// Each run is far below CarveBlockSize, so a packer that flushes at the end of
// every run emits `runs` blocks; one that flushes only at CarveBlockSize emits 1.
func TestCarvePackReachesBlockSizeOnScatteredRuns(t *testing.T) {
	const (
		runs    = 300
		runSize = 4 << 10
		gap     = 64 << 10 // a hole between runs keeps them separate
	)
	s, _, _, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BytesCarved != int64(runs*runSize) {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, runs*runSize)
	}
	// 300 x 4 KiB = 1.2 MiB, comfortably inside one 4 MiB block.
	if res.BlocksWritten != 1 {
		t.Fatalf("BlocksWritten=%d want 1: blocks did not span runs", res.BlocksWritten)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}
```

- [ ] **Step 2: Run it and confirm it fails for the right reason**

Run: `go test -race -count=1 -timeout 10m -run TestCarvePackReachesBlockSizeOnScatteredRuns ./pkg/block/journal/`
Expected: FAIL with `BlocksWritten=300 want 1`. If it fails with any other number, stop and report —
the geometry is wrong and the test is not measuring what it claims.

- [ ] **Step 3: Teach the dispatcher to flip across a multi-run block**

In `carve_dispatch.go`, replace the `run []interval` and `flipIdx *int` fields with `rs []*runState`,
update `newCarveDispatcher` and `submit` to the signatures above, and replace the `ok` branch of
`commitAndFlip` (currently the `case ok:` arm) with a loop over the plan's runs:

```go
	case ok:
		flipped := false
		for i := plan.first; i <= plan.last; i++ {
			wm := d.rs[i].end()
			if i == plan.last {
				wm = plan.lastOff
			}
			if err := d.s.flipUpTo(d.sh, d.id, d.rs[i].ivs, &d.rs[i].flipIdx, wm); err != nil {
				d.setErr(err)
				ok = false
				break
			}
			flipped = true
		}
		if ok && flipped && arenap != nil {
			d.res.BlocksWritten++
		}
```

`acquire`, `discard`, `wait`, `aborted`, `setErr`, the arena pool and the `prev`/`mine` ordering chain
are **unchanged**. `flipUpTo` itself is unchanged: it is called once per contributing run with that
run's own slice and own `flipIdx`, which is exactly its existing contract.

- [ ] **Step 4: Turn `carveRun` into `packRuns`**

Rename, change the signature to take `rs []*runState`, and wrap the existing pack loop in an outer
`for ri := range rs`. Inside the outer loop create a fresh `chunker` and run reader and reset `eof`,
so a chunk never spans a gap. Three edits inside the body:

- `newOffsets[fileOff]` becomes `rs[ri].newOffsets[fileOff]`
- track `blockFirstRun int`, initialised to 0 and reset to `ri` immediately after each `flush`
- `flush(watermark)` becomes `flush(flipPlan{first: blockFirstRun, last: ri, lastOff: fileOff})`

**Delete the unconditional per-run flush.** The only remaining flushes are the `batchBytes >=
s.cfg.CarveBlockSize` one inside the loop and a single tail flush after the outer loop:

```go
	last := len(rs) - 1
	flush(flipPlan{first: blockFirstRun, last: last, lastOff: rs[last].end()})
	if err := disp.wait(); err != nil {
		return res, err
	}
	return res, nil
```

The reap block currently at the end of `carveRun` moves to Task 4 — delete it here, and delete the
`ponytail:` comment above `carveRun` that describes the reap window (it moves with the code).

In `carveFile`, replace the `g.Go`/`results[i]` fan-out with a single call:

```go
	res2, err := s.packRuns(ctx, sh, id, rs)
	res.BlocksWritten += res2.BlocksWritten
	res.BytesCarved += res2.BytesCarved
	if err != nil {
		return err
	}
```

- [ ] **Step 5: Update the two stale comments**

`carve.go` — rewrite the peak-RAM paragraph: there is now **one** chunker scratch buffer for the
whole file rather than one per concurrent run, so peak carve RAM *drops*. Delete the
`ponytail:` about sizing scratch from `ChunkParams.Max` per run — it no longer describes the code.

`store.go:66-82` — `CarveUploadConcurrency` now bounds the Phase A `ManifestRowEndAfter` fan-out and
the blocks in flight; packing is sequential across the file. Delete the `ponytail:` about
`CarveRunConcurrency` — with one packer there is no second dimension to split.

- [ ] **Step 6: Replace the run-overlap test (see the Ruling above)**

In `carve_concurrent_test.go`, `TestCarveRunsCommitConcurrently` asserts `maxRuns >= 2`. Delete the
`inFlight`/`maxRuns` bookkeeping and the assertion at `:422-424`, rename the test to
`TestCarveBlocksCommitConcurrently`, and assert the property that survives:

```go
	if max < 2 {
		t.Fatalf("max blocks committing at once = %d, want > 1: commits did not overlap", max)
	}
```

Keep the existing `cur > window` ceiling assertion exactly as it is — that is what pins `sem`.
Update the doc comment to say it pins block overlap, not run overlap.

- [ ] **Step 7: Add the three remaining new tests**

```go
// TestCarvePackSpansRunsFlipsEveryContributingRun pins that when one block covers
// many runs, every record in every contributing run has its durable synced bit
// set — not merely that the in-memory unsynced counter reached zero. The on-disk
// flag is what recovery reads, so it is the only assertion that rules out the
// silent-zeros class.
func TestCarvePackSpansRunsFlipsEveryContributingRun(t *testing.T) {
	const (
		runs    = 64
		runSize = 4 << 10
		gap     = 32 << 10
	)
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
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
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	for off, b := range want {
		if got := sink.chunkAt(off); got == nil {
			t.Fatalf("no committed chunk at %d", off)
		} else if string(got) != string(b) {
			t.Fatalf("committed bytes at %d differ", off)
		}
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}

// TestCarvePackFlipPlanArithmetic pins the per-run watermark derivation on
// hand-built runState arrays, with no store and no sink: runs before the last
// flip to their own end, the last flips only to the offset actually packed.
func TestCarvePackFlipPlanArithmetic(t *testing.T) {
	mk := func(off, length int64) *runState {
		return &runState{ivs: []interval{{fileOff: off, length: length}}}
	}
	rs := []*runState{mk(0, 4096), mk(8192, 4096), mk(16384, 4096)}
	plan := flipPlan{first: 0, last: 2, lastOff: 18000}

	var got []int64
	for i := plan.first; i <= plan.last; i++ {
		wm := rs[i].end()
		if i == plan.last {
			wm = plan.lastOff
		}
		got = append(got, wm)
	}
	want := []int64{4096, 12288, 18000}
	if len(got) != len(want) {
		t.Fatalf("watermarks=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("watermark[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

// TestCarvePackSeamRunFailureLeavesSuffixDirty pins the abort semantics at a
// block seam: when a run is split across blocks k and k+1 and k+1's commit
// fails, the run's prefix is durable and flipped while its suffix stays dirty
// for the next pass. This is the same mid-run semantics carve has always had;
// spanning runs must not widen it into a half-flipped run reported as complete.
func TestCarvePackSeamRunFailureLeavesSuffixDirty(t *testing.T) {
	const runSize = 512 << 10
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         64 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(runSize, 1)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Let the first block commit, then fail every one after it. fakeSink already
	// has both hooks (carve_test.go:56-63) — do not add a new failure field.
	sink.okCommits = 1
	sink.failErr = errors.New("seam commit failed")

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the seam failure to surface, got nil")
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatal("post-carve unsynced=0: the failed suffix must stay dirty")
	}
	if s.UnsyncedBytes() == int64(runSize) {
		t.Fatal("post-carve unsynced=runSize: the committed prefix must have flipped")
	}
}
```

`sink.chunkAt` and `recRawFlags` already exist (`carve_test.go:99`, `:151`), and `fakeSink` already
carries `failErr` + `okCommits` (`carve_test.go:56-63`), which is why the seam test uses them rather
than a new hook. Do not add fields to the fake and do not build a second one.

- [ ] **Step 8: Verify**

Run: `go test -race -count=1 -timeout 10m ./pkg/block/journal/ ./pkg/block/engine/`
Expected: PASS. The four reap-safety tests, `TestCarveSharedRecordAcrossRuns`,
`TestCarveScatteredRunsConvergeConcurrently`, `TestCarveRecordSplitNoPrematureFlip`,
`TestCarveCommitStrictlyBeforeFlip`, `TestCarveSinkErrorLeavesDirty` and `TestCarveDedupReCarveIsNoOp`
must all pass **unmodified**. `carve_dispatch_test.go`'s tests change only where the constructor and
`submit` signatures changed — if any of them needs a changed *assertion*, stop and report.

- [ ] **Step 9: Commit**

```bash
git add pkg/block/journal/ 
git commit -S -m "perf(journal): pack one remote block across several dirty runs"
```

---

### Task 4: Phase C — reap per run, once that run is fully flipped

**Files:**
- Modify: `pkg/block/journal/carve.go` (`carveFile`, after `packRuns`)

**Interfaces:**
- Consumes: Task 2's `runState.complete()`, `start()`, `end()`, `newOffsets`.

- [ ] **Step 1: Move the reap into `carveFile`**

```go
	// Reap each run that fully flipped: with every row it produced committed, the
	// rows it superseded (stale straddlers, interior chunks the fresh tiling
	// replaced) are safe to delete. A run that did not complete is skipped — its
	// records stay dirty and a later pass re-carves and re-reaps them.
	//
	// Serial, not concurrent: carveCommitLocks stripes on the payload ID, so every
	// reap for one file contends on the same mutex anyway.
	//
	// ponytail: a caller cancelling between the flip and the reap still strands
	// those rows, since nothing retries a reap and the records are no longer
	// dirty; persist a pending-reap intent, or defer the flip until the reap
	// lands, if that window ever shows up in the field.
	if r, ok := s.sink.(supersededReaper); ok {
		for _, st := range rs {
			if !st.complete() {
				continue
			}
			if err := r.ReapSupersededManifest(reapCtx, id, st.start(), st.end(), st.newOffsets); err != nil {
				return err
			}
		}
	}
```

`reapCtx` is `carveFile`'s own `ctx`, never the errgroup's — that is what
`TestCarveReapSurvivesSiblingFailure` pins, and it must keep passing untouched. The interface is
`supersededReaper` (`carve.go:106-108`) and the call signature is unchanged from the one `carveRun`
used at `carve.go:529`; only `runStart`/`runEnd`/`newOffsets` now come off the `runState`.

Note for Task 7: a run that did **not** complete is skipped here, which preserves today's behaviour
and today's leak (#2073). Task 7 fills that branch in — it is deliberately not folded in here,
because this task must be reviewable as a pure move.

- [ ] **Step 2: Verify**

Run: `go test -race -count=1 -timeout 10m ./pkg/block/journal/ ./pkg/block/engine/`
Expected: PASS, all four reap-safety tests unmodified.

- [ ] **Step 3: Commit**

```bash
git add pkg/block/journal/carve.go
git commit -S -m "refactor(journal): reap superseded rows per run after packing"
```

---

### Task 5: Guard the serialised dedup path (R3)

Packing 240,000 chunks through one goroutine means 240,000 serialised `IsChunkDurable` lookups where
there used to be `CarveUploadConcurrency` of them in flight. Against a real badger oracle that could
eat the win this plan exists to deliver. Measure it before shipping, not after.

**Files:**
- Modify: `pkg/block/journal/carve_pack_test.go`

- [ ] **Step 1: Add the benchmark**

```go
// BenchmarkCarveScatteredPass measures a full scattered carve pass against a
// real dedup oracle, so the cost of serialising IsChunkDurable through one
// packer goroutine is visible rather than elided by a map-backed fake.
func BenchmarkCarveScatteredPass(b *testing.B) {
	const (
		runs    = 5000
		runSize = 4 << 10
		gap     = 16 << 10
	)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, _, _, _ := carveStore(b, Config{
			CarveBlockSize:         4 << 20,
			CarveUploadConcurrency: 8,
			ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
		})
		ctx := context.Background()
		for r := 0; r < runs; r++ {
			if err := s.WriteAt(ctx, "f", int64(r)*gap, randBytes(runSize, int64(r))); err != nil {
				b.Fatalf("WriteAt %d: %v", r, err)
			}
		}
		b.StartTimer()
		if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
			b.Fatalf("Carve: %v", err)
		}
	}
}
```

`carveStore` is declared `func carveStore(t *testing.T, cfg Config) (*Store, *fakeDeduper,
*fakeSink, *fakeClock)` (`carve_test.go:139`). Widen its parameter to `testing.TB` and use it
directly — every existing caller passes a `*testing.T`, which satisfies `testing.TB`, so no call site
changes and no `carveStoreB` twin is needed. If its body calls a `*testing.T`-only method, replace
that one call rather than duplicating the helper.

- [ ] **Step 2: Record the baseline and the delta**

Run on `develop` and on this branch:
`go test -run '^$' -bench BenchmarkCarveScatteredPass -benchtime 3x -timeout 20m ./pkg/block/journal/`

Put both numbers in the commit message. **If the packed pass is more than 2x slower in CPU time than
the serial-run baseline, stop and report** — the fix would be trading a remote-latency win for a
local-CPU wall, and the upgrade path (a bounded hash-ahead pipeline that computes BLAKE3 and
`IsChunkDurable` for the next chunks while the current block packs) needs to land in this plan rather
than after it.

- [ ] **Step 3: Commit**

```bash
git add pkg/block/journal/carve_pack_test.go
git commit -S -m "test(journal): benchmark a scattered carve pass against a real dedup oracle"
```

---

### Task 6: Measure on the rig — this is part of done

Unit tests cannot answer whether this closed #1872: the whole defect is remote round-trip count,
which every fake sink elides.

- [ ] **Step 1: Fresh bench VM.** Verify `.bench-vm.json` `server_id` is **not** a coder VM
      (`d9f39027-487e-4523-8033-1488eb3c3639`, `fb430ccc-8c94-4892-ad60-9b689f0b4732`) before any
      teardown. Heartbeat-monitor the run; the `--remote` poller hangs on a stall.
- [ ] **Step 2: Run the probe that motivated #1872** — `dittofs-s3-writeback`, `large`,
      `rand-write-4k` + `seq-read`, sampling the store every 30 s through the drain.
- [ ] **Step 3: Capture object size on the wire**, not just throughput. The pre-fix distribution is
      4168 / 6216 / 8264 B; the target is ~4 MiB. Object-size distribution is the primary result and
      drain wall-clock is the secondary one — report both.
- [ ] **Step 4: Compare against develop's baseline** — 49 → 34 → 45 → 22 → 2 → 2 MiB per 30 s, and
      the cold barrier failing with 935 MiB unsynced at the 15 m timeout.
- [ ] **Step 5: Post the numbers on #1872**, whatever they show. Report what the rig did, not what
      the object-count reduction implies — that is the standard the #1875 comment set and the reason
      #2070 did not close #1872.
- [ ] **Step 6: Close #1872 only if the barrier passes.** If the drain converges but the barrier
      still fails, say so and keep it open; a second binding constraint would then exist and this
      plan has not found it.

---

---

### Task 7: Close #2073 — reap the committed prefix of an aborted run

Deliberately last, and deliberately **after** the packing work rather than before it. #2073 is a
pre-existing correctness bug on `develop` (a run that aborts never reaps the rows its already-flipped
prefix superseded, and overlap resolution is greatest-start, so a stale row starting later wins and
serves old bytes on a cold read). It does not gate #1872, and Task 2 makes it materially cheaper:
its validated design needs a persistent committed frontier, and `runState.flipIdx` is exactly that,
surviving in `carveFile` after `packRuns` returns. Doing it first would mean writing the plumbing
twice.

**Files:**
- Modify: `pkg/block/journal/carve.go` (`carveFile`, the Task 4 reap loop)
- Test: `pkg/block/journal/carve_pack_test.go`

**Interfaces:**
- Consumes: Task 4's reap loop, Task 2's `runState.flipIdx`, the existing `manifestRowEnder`
  (`carve.go:110-113`).

- [ ] **Step 1: Write the failing test**

```go
// TestCarveAbortedRunReapsCommittedPrefix pins that a run which aborts partway
// still reaps the rows its flipped prefix superseded. Those records are clean,
// so no later pass revisits them; a skipped reap leaves the superseded rows
// alive forever, and greatest-start overlap resolution then serves the stale
// row's bytes on a cold read.
func TestCarveAbortedRunReapsCommittedPrefix(t *testing.T) {
	const runSize = 512 << 10
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         64 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(runSize, 7)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	sink.okCommits = 1
	sink.failErr = errors.New("abort after the first block")

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the abort to surface, got nil")
	}
	reaps := sink.reapCalls()
	if len(reaps) != 1 {
		t.Fatalf("reap calls=%d want 1: the committed prefix was not reaped", len(reaps))
	}
	if reaps[0].start != 0 {
		t.Fatalf("reap start=%d want 0", reaps[0].start)
	}
	if reaps[0].end == 0 || reaps[0].end >= int64(runSize) {
		t.Fatalf("reap end=%d want a strict prefix of %d", reaps[0].end, runSize)
	}
}
```

`fakeSink` does not implement `supersededReaper` at all today (that is why the reap is skipped in
most journal tests). Add the method to `fakeSink` recording `{start, end int64}` per call under its
existing mutex, plus a `reapCalls()` accessor returning a copy — and make it implement
`manifestRowEnder` too, returning the argument unchanged so no row ever straddles. Adding the
capability changes which branch existing tests take, so run the whole package after this step, not
just the new test.

- [ ] **Step 2: Run it and confirm it fails for the right reason**

Run: `go test -race -count=1 -timeout 10m -run TestCarveAbortedRunReapsCommittedPrefix ./pkg/block/journal/`
Expected: FAIL with `reap calls=0`. If it fails with a non-zero count, the fake is reaping through
some other path — stop and report.

- [ ] **Step 3: Fill in the incomplete-run branch**

The naive fix — reap `[start, watermark)` unconditionally — is **unsafe and must not be written**.
`ReapSupersededManifest` deletes a row *whole* when `runStart <= rowStart < runEnd`; the narrowing
branch fires only for `rowStart < runStart` and refuses when `rowEnd > runEnd`. The only thing that
normally stops a straddler delete is `runEnd` being a manifest row boundary, which
`extendRunToRowEnd` guarantees for a completed run and a **prefix watermark does not**. Deleting a
straddler whose `rowEnd > watermark` over warm, durable, non-dirty bytes leaves a permanent
uncovered range that cold-reads as zeros — strictly worse than the overlap it fixes.

Guard on the row boundary instead:

```go
		if !st.complete() {
			// The run aborted. Its flipped prefix is clean, so nothing revisits the
			// rows that prefix superseded — but the prefix watermark is not a
			// manifest row boundary, and the reap deletes a straddling row whole.
			// Reap only when no row straddles the watermark; otherwise leave the
			// rows, since a stale overlap is recoverable and a hole is not.
			if st.flipIdx == 0 {
				continue
			}
			wm := st.ivs[st.flipIdx-1].end()
			ender, ok := s.sink.(manifestRowEnder)
			if !ok {
				continue
			}
			rowEnd, err := ender.ManifestRowEndAfter(reapCtx, id, wm)
			if err != nil {
				return err
			}
			if rowEnd > wm {
				continue // a row straddles the watermark: deleting it would open a gap
			}
			if err := r.ReapSupersededManifest(reapCtx, id, st.start(), wm, st.newOffsets); err != nil {
				return err
			}
			continue
		}
```

`st.newOffsets` may contain offsets that were packed but never committed (from the discarded
half-block). That is benign in one direction only: an uncommitted offset in `newOffsets` can only
cause the reap to *keep* a stale row, never to delete a live one. Say so in the commit message.

- [ ] **Step 4: Verify**

Run: `go test -race -count=1 -timeout 10m ./pkg/block/journal/ ./pkg/block/engine/`
Expected: PASS. `TestCarveReapSurvivesSiblingFailure` and both `carve_runextend_test.go` tests must
pass unmodified. Because Step 1 gave `fakeSink` two capabilities it did not have, watch specifically
for `TestCarveDedupReCarveIsNoOp` and `TestCarveScatteredRunsConvergeConcurrently` — if either now
fails, the fake's `ManifestRowEndAfter` is returning something other than its argument.

- [ ] **Step 5: Commit and close the issue**

```bash
git add pkg/block/journal/carve.go pkg/block/journal/carve_pack_test.go pkg/block/journal/carve_test.go
git commit -S -m "fix(journal): reap the rows an aborted run's committed prefix superseded"
```

Close #2073 manually after the PR merges, quoting the guard that makes it safe.

## Gate: Phase 3 — warm-gap bridging (only if Task 6 leaves a gap)

Out of scope here. Feed the chunker the already-synced, still-locally-resident bytes in the gaps
between dirty intervals so a scattered neighbourhood becomes one run, reducing manifest rows per MiB
as well as objects. `warmTail` (`carve.go:616-641`) already returns exactly the bridgeable set.

Three things must hold before it is worth costing:

- **A hole is not a gap.** No interval at all means a genuine hole; bridging one fabricates coverage
  and breaks `DataExtents` / `SEEK_HOLE`. `synced && cold` needs a remote GET and defeats itself.
  Only `synced && !cold` is bridgeable.
- It needs a `CarveBridgeBytes` ceiling shipping at **0** (off), or a scattered file bridges its
  whole length and the pass never ends.
- Arithmetic on the rig aggregates (~235 KiB/s drained, 5 s `CarveMaxAge`) suggests ~0.1% dirty
  density and ~3 MiB mean gaps, where bridging yields either nothing or ~750x write amplification.
  **Measure gap composition first** — Task 1's sweep is the natural place to add it.

## Gate: Phase 4 — block-level sync state (#1414)

Reframed by this plan. It is **not** an alternative route to #1872 — Task 3 closes that — it is the
fix for the reclaim-granularity ceiling this plan accepts (R6). Its trigger is Task 1's amplification
ratio staying above ~2.0 in the field. PR3a already landed (#1469): `PutBlock`, `GetLocator` and
`ChunkLocator` are present tree-wide. PR3b did not: `MarkSyncedBatch`, `TransformChunk`,
`sealAndUploadPack` and `PackBuffer` are absent.

## Do not restart PR #1875

Its branch `perf/1872-carve-pack-across-runs` (worktree `~/dittofs-worktrees/1875-repro` at
`66fa8a36`) spans the **watermark** across runs with N concurrent producers. This plan has exactly
one producer, so the shape that stalled does not exist here. Do **not** gate Task 3 on reproducing
that stall, and do **not** rebase the branch — 19 journal commits have landed on `carve.go` since.
