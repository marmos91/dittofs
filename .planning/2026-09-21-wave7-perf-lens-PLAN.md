# Wave 7 — the perf lens: plan

**Status:** plan, not started · **Created:** 2026-09-21 · **Parent:**
`.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` §Wave 7

The master plan gives Wave 7 one paragraph: perf scored ~zero on all three audits, and the
perf-attempts ledger names the adapter layer as the remaining un-walked axis. That is a direction,
not a plan. This file is the entry condition.

## The rule this wave exists under

Wave 7 does **not** start with code. Every perf attempt in this repo that started with a fix
instead of a profile is in the refuted column below, and several were refuted twice because the
second attempt did not know about the first. The first deliverable is a measured assignment of
per-operation cost; implementation issues open only against a named component that measurement
put cost on.

Three standing rules from the ledger, all learned expensively:

1. **The rig lies about IOPS.** Trust server-side pprof and engine/unit benchmarks. A rig number
   moves for reasons that have nothing to do with the treatment.
2. **Profile the treatment, not the mechanism.** A coordinator that provably coalesces can leave
   the wall untouched, because the wall was somewhere else (#1573, three times).
3. **Measure on the Linux VM.** macOS distorts two things that matter here: Docker Desktop
   inflates DB round-trips ~2.7x, and BLAKE3 takes the generic path on arm64 while the bench VM's
   amd64 takes AVX2. Both have already produced wrong conclusions in this repo, in opposite
   directions.

## Do not re-walk these

Closed by measurement. Re-opening any of them needs new evidence, not a new idea.

| Axis | Verdict |
| --- | --- |
| Swap the metadata backend to fix create | **Refuted** (#1752). badger/postgres/sqlite all ~500-700 ops/s, same shape. Decisive control: postgres `synchronous_commit=off` changed nothing (452 vs 496) — never fsync-bound. |
| Group-commit the metadata fsync | **Refuted 4x** — #1684 `flushPendingWrite`, #1573 `syncIfRelaxed`, #1742 cross-shard journal, #1747 single-fd badger. The wall is badger's serialized single-writer commit, and sync-vs-write contention, not sync-vs-sync. |
| Postgres commit batching (`commit_delay`/`commit_siblings`) | **Refuted** (#1828 S8). Postgres already coalesces unaided at 0.76 syncs/commit; every setting measured a net regression. Also retires #1747's revival condition. |
| Cold read via prefetch depth / engine concurrency | **Refuted twice** (#1625 flat, #1628 167→122). The wall is S3 fetch latency; prefetch never stays ahead. |
| badger vlog / memtable / compactor config | **Eliminated** (2026-07-14). Metadata values sit below `ValueThreshold`, so they live in the LSM and `vlog.sync()` is a near-noop; no config changes how `DB.Sync` holds `db.lock.RLock` across the per-commit WAL fsync. |

## The two axes that are actually un-walked

### Axis A — NFS/metadata per-read

Post-#1648/#1651 the block engine is ~20x faster and warm random read sits at 7437 (large) /
24862 (medium) IOPS, roughly 56-58% of JuiceFS. The remaining gap was attributed to "per-read NFS
RPC + metadata lookup, not the block store".

**That attribution is a residual, not a measurement.** It is what was left after the block store
got fast, which is exactly the proxy-reported-as-the-thing shape this repo keeps hitting. Nobody
has profiled it and split the residual between RPC framing, the metadata lookup, and whatever else
is in there.

### Axis B — the per-op write path

#1752 put the create ceiling *above* the metadata engine and ruled out both fsync and CPU as the
binding constraint (dfs used ~1/8 cores). The ledger names three suspects that have never been
profiled **as a pipeline**: the `WriteFile` size/mtime metadata commit, the block-journal fsync,
and the NFS WRITE/COMMIT round-trip — specifically that they do not overlap.

Non-overlap is the interesting claim, and it is a claim about *scheduling*, which a CPU profile
alone will not show. It needs a timeline, not a flat profile.

#2398 (the global `sm.mu`) belongs to this axis in the master plan's framing. It landed on the
correctness side in Wave 2 (#2459, #2466); its perf effect was never measured and is a free
before/after if the rig is being stood up anyway.

## Phase 0 — the only phase currently authorized

**Deliverable: a per-operation cost assignment for both axes, with the residual assigned rather
than inferred.** No code changes, no knobs, no PRs against the data path.

Steps:

1. Stand up the rig per `internal/dfsbench/CLAUDE.md`: `dfsbench setup` on the POP2-8C-32G VM with
   the separate `/bench-data` SBS volume, `--config bench.yaml` (required for any S3 backend —
   without it only `local-disk` runs and `run` still exits 0).
2. Capture a **server-side pprof of `dfs`** during the warm random-read cell and during the
   create+4k-write cell, separately. The rig's IOPS number is context, not the result.
3. For Axis A, split the per-read cost into RPC decode/encode, metadata lookup, permission funnel,
   and block-engine time. The share-options cache landed because `GetShareOptions` → decode was
   17.4% of server CPU on exactly this workload, so the funnel is known to be non-trivial and is
   the first place to look.
4. For Axis B, produce a **timeline** of one create+write+close, not a flat profile: when each
   durable commit starts and ends relative to the NFS round trip. The question is whether they
   serialize, and a flat profile cannot answer it.
5. Re-confirm the #1752 controls still hold on the current tree before building on them — that
   measurement is from 2026-07-17 and the write path has moved since.

**Exit condition:** each axis has a named component carrying a stated share of per-op cost, or is
explicitly closed as "cost is distributed, no single lever". Either outcome is a result and gets
written into the ledger.

**Anti-goal:** a Phase 0 that ends in "looks like the metadata store" without a number. That is
where #1752 started, and it cost three refuted attempts to get out of.

## Phase 1 — conditional, not scheduled

Opens only per component Phase 0 assigns cost to, one issue each, each naming its own measurement
and its own stop rule. Nothing about Phase 1 is planned here on purpose: planning fixes before
knowing where the cost is, is the failure mode this wave is built to avoid.

## Stop rules

- No default shipped on an unmeasured win, per the standing stop-on-no-win rule.
- A knob that trades latency for throughput on a synchronous metadata path is the wrong currency
  regardless of the throughput arithmetic (this is why `commit_delay` shipped nothing).
- Every verdict — win, loss, or null — lands in the perf-attempts ledger before the branch closes.
  Two axes in this repo were re-attempted only because a prior null result was never written down.
