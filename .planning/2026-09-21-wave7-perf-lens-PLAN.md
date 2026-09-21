# Wave 7 — the perf lens: plan

**Status:** plan, not started · **Revised:** 2026-09-21 after ponytail + adversarial + general
review · **Parent:** `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` §Wave 7

Wave 7 does not start with code. Implementation issues open only against a component that a
profile put cost on, at or above the bar in §Exit.

Refuted axes and their revival conditions live in the perf-attempts ledger, which is **session
memory, not in this repo** — anyone picking this up cold must be handed it, or they will re-walk
something closed. Its overarching rule governs everything below: the rig lies about IOPS, so it is
a load generator here, never a meter.

## Verified premises (2026-09-21, on `develop@8c8c7546a`)

- `/debug/pprof/{profile,trace}` exist and serve CPU, block, mutex and execution traces
  (`pkg/controlplane/api/router.go:84-94`), gated on `controlplane.pprof` **and** admin JWT.
  Defaults: off; when on, `PprofMutexRate=100`, `PprofBlockRateNs=1_000_000`.
- The harness starts `dfs` from a fixed command line in `internal/dfsbench/backend/dittofs.go`
  (~:340) that does **not** set pprof. Enabling it is a harness change.
- `transactorSpy` (`pkg/metadata/writeback_tier_test.go`) already counts durable commits per
  operation, locally, with no VM.
- `metadata` is the create+4k-write workload and **must run first**
  (`internal/dfsbench/fio/workload.go:41-56`); `--evict-cache` defaults **true** and inserts a cold
  barrier before each read cell.

## Scope: one axis

**Axis B — the per-op write path.** #1752 put the create ceiling above the metadata engine. The
live question is whether the `WriteFile` size/mtime commit, the block-journal fsync and the NFS
WRITE/COMMIT round-trip serialize.

Two corrections to how that was framed in the first draft, both from review:

- #1752 ruled out the **metadata-commit** fsync for postgres on the create workload
  (`synchronous_commit=off`, 452 vs 496). It says nothing about DittoFS's block-journal fsync,
  which is one of the three suspects. "Never fsync-bound" is not licensed.
- Aggregate CPU was low (~1/8 cores on 8 vCPU), which is *consistent with* a serialized path and
  does not rule CPU out.
- Non-overlap is a **hypothesis** from a parenthetical in #1752, not a recorded finding. One leg is
  already settled: #1747 measured that badger's `db.Update` commits are serialized and never
  overlap. So the open legs are journal-fsync vs NFS round-trip.

**Axis A — warm per-read NFS/metadata — is closed, not un-walked.** The first draft of this plan
called its attribution an unmeasured residual. That was wrong: six rounds of VM server-side pprof
split it (`GetFileForRead` 47%, `GetShareOptions→decode` 17.4%, goroutine stack growth ~25%), three
fixes shipped (#1654, #1657, worker pool), and Round 5/6 reached a negative verdict — both the
metadata caches and the worker pool moved server CPU 25%+ each and **did not move IOPS**. The wall
is per-RPC round-trip latency, and competitors win warm random read by serving 4 KiB from the
kernel page cache with no userspace round-trip. That is structural. Any further work here is
client/protocol-level and is **not** this wave.

Note the shipped-and-inert trap this closes: `GetShareOptions` decode was 17.4% of server CPU
pre-fix, the `sharecache` fixed it across all backends, and it moved no IOPS. `ShareOptions` is now
two bools, so what is left to decode there is near-zero. It is not a place to look.

**Out of scope, named so nobody assumes otherwise:** seq-write and upload throughput (#1717,
within-file concurrent PutBlocks) — the largest open ledger item, and not this axis. **#2423**
(upload concurrency windows) is scheduled as Wave 7 work by
`.planning/2026-09-11-wave8-plan.md` §Step 3; it stays there and is not authorized by this plan.

**Protocol coverage:** NFSv3 only, deliberately. v4 compounds change per-op RPC accounting so a v3
split does not transfer, and SMB's per-op suspects (the double allocation in `framing.go`'s
`readNetBIOSPayload`) are unpriced. Both are real and both are a second pass — this one does not
claim to have measured them.

## Phase 0

Step 0 gates the rest: if the #1752 controls no longer hold on the current tree, steps 1-3 profile
the wrong thing at full VM cost.

0. **Locally, no VM:** take the commit inventory for create+write+close with `transactorSpy`, and
   attempt the overlap question with `go tool trace` over an integration test of that sequence.
   Ordering is not a magnitude, so the macOS caveat below does not forbid this. If this settles it,
   stop — the rest of Phase 0 is unnecessary.
**Phase 0 step 0 ran 2026-09-21 on develop `4dcbd89bc`. It does not settle the axis, but it
moves the suspect.** Two artifacts, both local, no VM:

`TestWave7Phase0_CommitInventory` (`pkg/metadata`) counts what `transactorSpy` sees for
create + UNSTABLE write + FILE_SYNC close:

| phase | default share | writeback share |
| --- | --- | --- |
| CREATE | 1 relaxed | 1 relaxed |
| WRITE (UNSTABLE) | 0 | 0 |
| CLOSE (FILE_SYNC) | 1 durable | 1 relaxed |

Two metadata commits for the sequence, exactly one of them durable, none on a writeback share.
CREATE is relaxed by design, not by accident -- it is a pure-namespace write and routes through
`withRelaxedTransaction`. **So the metadata tier is not where a per-op write path spends itself,
and the first draft's framing of the `WriteFile` size/mtime commit as one of three co-equal
suspects overstated it.** The counts are asserted, not logged, so the next reader quotes a number
a test is holding.

`BenchmarkWave7WritePath_CreateWrite` (`internal/adapter/nfs/v3/handlers`) drives CREATE plus a
4 KiB write through the v3 handlers over a real on-disk journal, at each stability level:

| stability | ns/op (macOS, APFS, 300x, 3 reps) |
| --- | --- |
| UNSTABLE | ~36,000 |
| DATA_SYNC | ~4,900,000 |
| FILE_SYNC | ~5,000,000 |

A 134x step at the sync boundary is the probe that the durability path is genuinely reached; a
profile taken without it would have been measuring the wrong thing, which is the failure mode the
note below this section warns about.

**The 5 ms is almost entirely not this goroutine's own syscall time.** `go tool trace -pprof=syscall`
attributes only ~74 us/op of blocked syscall to `Handler.Write`, all of it `pwrite`/`write` under
`journal.(*Store).appendRecord` -- no `fsync` on the append path at all. The rest is spent waiting
on the journal shard's group-commit leader to fsync on this operation's behalf
(`shard.groupCommit`, `store.go:789`).

**So the open leg is not "does the journal fsync serialise with the metadata commit" -- it is
whether the group commit amortises.** Measured with `BenchmarkWave7WritePath_FileSyncParallel`
across `-cpu 1,2,4,8`: 4.99 / 4.57 / 3.83 / 3.34 ms per op. **Eight times the writers buys 1.5x
the throughput.** Perfect batching would have approached 8x; flat would have meant no batching at
all. It is neither.

**The mechanism is shard fan-out, and it is measured, not inferred.** Group commit is per shard
(`shardFor` hashes FNV-1a over the FileID) and the journal defaults to `ShardCount = 16`
(`store.go:104`), so writers touching distinct files land on distinct leaders and each pays its own
fsync. `BenchmarkWave7GroupCommit_ShardFanOut` sweeps the shard count directly at the journal
layer, everything else held fixed, 8 writers, each committing its own FileID:

| configuration (8 writers) | ns/op |
| --- | --- |
| `ShardCount = 1` | 1,284,096 |
| `ShardCount = 2` | 2,183,253 |
| `ShardCount = 4` | 3,217,541 |
| `ShardCount = 16` (default) | 2,947,807 |
| **one shared FileID, default shards** | **1,284,620** |

The control is what makes it conclusive: every writer committing the *same* FileID lands on one
shard and costs 1,284,620 ns/op -- the same as collapsing the store to a single shard, to within
0.05%. So the batching machinery works exactly as designed when writers share a leader, and the
2.3x penalty at the default comes entirely from spreading them across leaders. A create-heavy
workload, which is the `metadata` cell, is the worst case by construction: every operation is a new
FileID.

**This clears the Exit bar for opening an implementation issue** -- a named component
(`shard.groupCommit` / `ShardCount`) with a profile and a controlled sweep behind it, not a
suspicion. It is a real trade rather than a bug: shards exist to keep writers off each other's
locks, and collapsing them trades fsync amortisation against lock contention. The issue should ask
for the two to be measured against each other, not for the shard count to be lowered.

**What Phase 0 could not do, stated so nobody reads more into it.** The NFS WRITE/COMMIT
round-trip is not in this process: there is no client and no network, so the third suspect is
untouched and only the VM can reach it. The fixture's metadata store is in-memory, so the metadata
commit costs no syscall here and its true cost against badger or postgres is not visible either.
And the fsync is macOS `F_FULLFSYNC` on APFS, slow by an order of magnitude against the bench
volume -- every ratio above is a ratio between two runs on the same machine, which is what carries;
no absolute number here transfers to the rig.

**Consequence for steps 1-3.** The VM run is still worth doing, but it is no longer looking for an
unknown: it should confirm that the fan-out penalty survives a real device and a real client, and
it now has a specific thing to vary. Run it with the shard sweep in hand rather than as an open
profile hunt.

**What Phase 0 could not do, stated so nobody reads more into it.** The NFS WRITE/COMMIT
round-trip is not in this process: there is no client and no network, so the third suspect is
untouched and only the VM can reach it. The fixture's metadata store is in-memory, so the metadata
commit costs no syscall here and its true cost against badger or postgres is not visible either.
And the ~5 ms fsync is macOS `F_FULLFSYNC` on APFS, which is slow by an order of magnitude against
the bench volume -- the *shape* across `-cpu` is what carries, never the absolute number.

1. **Harness PR, its own PR:** add `DITTOFS_CONTROLPLANE_PPROF=true` to the `dfs start` line in
   `internal/dfsbench/backend/dittofs.go`. This is instrumentation, not a data-path change, and the
   prohibition below is scoped accordingly.
2. One VM, one system, one workload:
   ```
   dfsbench setup                       # POP2-8C-32G + /bench-data SBS volume
   dfsbench run --remote --config bench.yaml --skip-baseline \
     --systems dittofs-s3-nfs3 --workloads metadata --sizes medium --runtime 60
   dfsbench teardown                    # NOT optional — VM + volume bill until this runs
   ```
   No competitors and no matrix: nothing here compares systems. `--skip-baseline` is required, not
   tidiness — it defaults to `false` (`internal/dfsbench/run/cmd.go:72`) and `managed.go:54` then
   runs `measureLocalDiskCeiling` first: a layout pass plus sequential and random reads against
   local disk, which at `--runtime 60` is most of the run doing something this step is not asking
   for. Medians of 3 reps — read IOPS variance on this rig runs to ±70%, and a single rep will be
   quoted as fact.
3. Scrape from the VM while the cell runs, using the admin bearer token from `dfsctl login`:
   CPU (`/debug/pprof/profile?seconds=30`), **block and mutex** (the off-CPU wait profiles that
   attribute serialization), and `/debug/pprof/trace?seconds=10` for the timeline. Precedent with a
   worked example: `.planning/perf/metadata-cache-decision.md` §Part 2.

**Before trusting any engine or unit benchmark, mutation-probe it** — show it fails when the
production path is broken. `BenchmarkSequentialWrite8MB` profiled dead code for months because
`newWriteBenchEngine` never wired `LocalChunkIndex`, and nothing caught it.

**Measure on Linux amd64.** The reason is BLAKE3: it takes the generic path on arm64 and AVX2 on
the bench VM. (The Docker-Desktop 2.7x distortion is socket-specific and does not apply to badger.)

## Exit

A component is named only at **≥15% of profiled per-op server CPU**, or a demonstrated serialized
wait in the block/mutex profile or execution trace. Below that the axis closes as *cost is
distributed, no single lever* — an outcome, not a failure.

Signed off by someone who did not run the profile. An agent and its own reviewer agreeing is one
opinion.

## Where results go

`.planning/perf/wave7-writepath.md`, following `.planning/perf/metadata-cache-decision.md`'s shape
(Status section with checkboxes). Raw `.prof` and trace files alongside it — `bench-results/` is
wiped between runs. The verdict, win or null, also goes to the perf-attempts ledger: two axes in
this repo were re-attempted only because a null was never written down.

If Phase 0 re-runs cells, say explicitly whether `docs/BENCHMARKS.md` is updated or left alone —
it is user surface.

## Hazards

- No data-path changes in Phase 0. Harness and instrumentation changes land as their own PR.
- Never run two `dfsbench` runs concurrently — shared ports, they corrupt each other.
- `run` exits 0 even when most systems fail setup; check the results directory covers what you asked for.
- Sign every commit; rebase, never merge.
- Source test files are wave/lane/PR-number agnostic (`.planning/CONVENTIONS-WAVE-TEST-NAMES.md`) —
  no `wave7_perf_test.go`.
- Phase 1 will touch `pkg/block/engine`, `pkg/block/journal` and `pkg/metadata/store`, where a knob
  defaulted off or a cache with a staleness ceiling needs a `decision:` or `ponytail:` marker at the
  code site. Issue numbers stay out of comments — perf fixes are the most tempting place to break
  that rule.

## Phase 1

One issue per named component, each with its own measurement and its own stop rule. Nothing more is
planned here on purpose.
