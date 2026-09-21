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
1. **Harness PR, its own PR:** add `DITTOFS_CONTROLPLANE_PPROF=true` to the `dfs start` line in
   `internal/dfsbench/backend/dittofs.go`. This is instrumentation, not a data-path change, and the
   prohibition below is scoped accordingly.
2. One VM, one system, one workload:
   ```
   dfsbench setup                       # POP2-8C-32G + /bench-data SBS volume
   dfsbench run --remote --config bench.yaml \
     --systems dittofs-s3-nfs3 --workloads metadata --sizes medium --runtime 60
   dfsbench teardown                    # NOT optional — VM + volume bill until this runs
   ```
   No competitors and no matrix: nothing here compares systems. Medians of 3 reps — read IOPS
   variance on this rig runs to ±70%, and a single rep will be quoted as fact.
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
