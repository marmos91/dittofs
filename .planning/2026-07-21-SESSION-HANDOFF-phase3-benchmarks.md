# Session handoff — Phase 3 benchmarks (2026-07-21)

**Read this first if resuming in a fresh session.** Phases 1 (issue sweep) and 2 (#1828 plan)
are DONE. The next action the user approved is **Phase 3: run the benchmarks (#1767, #1743)**
to capture updated data now that the write-amplification fix has shipped.

---

## What's DONE (this session, all merged to develop)

Program roadmap: `.planning/2026-07-21-reliability-scale-features-ROADMAP.md`. Driver memory:
`project_reliability_scale_features_roadmap.md`.

- **Phase 0 — v0.28.1 RELEASED** (replay6 classified, Port=0, mutation-safety, smbtorture timeout).
- **Phase 1 — COMPLETE.** Merged this session: **#1836** (nix hash-bot tag-push fix + stale
  vendorHash), **#1837** srvsvc NetrShareGetInfo 502 (#1616) + **#1840** (fixed a real
  request-NDR bug Copilot caught — NetName is a `[string]` ref pointer, inline no-referent),
  **#1838** metadata Wave-1 hygiene (dead `d:`, ShareSession relocate, FilesystemMeta→cap:fs),
  **#1841** badger `fm:` manifest split (the write-amp fix — chmod no longer rewrites the
  manifest; SQL half already shipped in **#1820**), **#1842** (nlink single-source + the
  PutFilesystemMeta error-contract Copilot fix). Closed: #1832 (Windows-only KF), #1658 (fixed
  by renameScanMu), #1616, #1839, #1432 (temp-closed — awaiting reporter retest on develop).
  #1715 stays OPEN (multi-wave; Wave 1 done, Waves 2/3 fold into #1828). #1692 verified (all
  three walls shipped) — left open until the #1715 Wave-2/3 tail lands.
- **Phase 2 — COMPLETE.** #1828 metadata-unification plan APPROVED, saved at
  **`/Users/marmos91/.claude/plans/zazzy-petting-goblet.md`**. Plan-only — do NOT execute #1828
  yet (queued after Phase 3). Key: two families + `basestore`; merge sqlite+postgres into one
  `sql` impl over a `dialect/` subpackage; retry per-family; #1715 Wave 3 (UpdateAttrs/
  SetManifest) lands FIRST. ~12k duplicated lines removed.

develop tip after this session = the #1842 merge. Local `develop` checkout is STALE (see
Housekeeping).

---

## Phase 3 — the benchmark task (what to run)

**Goal:** capture updated benchmark data on develop now that the write-amp fix (#1820/#1841)
and carve/adapter wins have shipped — so BENCHMARKS.md reflects the improved write path.

Two tracker issues:
- **#1743** — "bench: complete the fair matrix — all systems + large size + cold-read pass."
  The main deliverable: the full apples-to-apples QoS × protocol × system matrix.
- **#1767** — "Cold-read benchmark barrier broken for DittoFS: drain-uploads stalls + evict is
  a no-op below 1GiB." Fix/validate the cold-read measurement path so the cold cells are real.

**Plans + harness (read these):**
- `.planning/perf/2026-07-18-full-qos-matrix-PLAN.md` — the authoritative matrix plan: 8 systems
  (dittofs, ganesha, juicefs, rclone, s3fs, zerofs, goofys, s3ql), the QoS-support map (most
  cells N/A — verify each), per-system setup gotchas, hard-won SSH/VM notes.
- `.planning/perf/BENCHMARK-PLAN.md` — broader benchmark plan.
- Harness: `internal/dfsbench/` + `cmd/bench` (the `dfsbench` tool). Pure-Go Backend×Protocol,
  single SCW VM, **`--resume`** to reuse competitor baselines (competitors never run dfs).
- `bench.yaml` (bucket `dittofs-bench`, endpoint `s3.fr-par.scw.cloud`).

**Bench VM state:** `.bench-vm.json` currently records
`server_id=e5d1f206-dda5-4a51-85c1-11e66cfbabc3`, ip `51.158.68.183`, `fr-par-1`. **Before
touching it: verify via `scw` CLI that this server_id is a real bench VM, NOT a coder VM** (see
hazards). It may or may not still be running — check; if stale, provision a FRESH VM (SCW
POP2-8C-32G, fr-par-1) and run the bench from a git worktree.

---

## MANDATORY benchmark hazards & conventions (memory — obey these)

- **NEVER touch SCW coder VMs** (`feedback_never_touch_coder_vms`). Coder instances belong to
  Cubbit devs. **Verify `.bench-vm.json` server_id before any teardown.** Run the bench from a
  worktree, never the main checkout.
- **Heartbeat-monitor long benchmark runs** (`feedback_benchmark_heartbeat_monitor`): a
  stall-detecting poll alongside the run; the `--remote` poller hangs on a stall.
- **Apples-to-apples** (`project_bench_fairness_apples_to_apples`, #1739): ZeroFS is the fair
  durable comparator; the server is ~97% idle at rig load; #1736 rand-write regression was real.
- **Bench rig lies about read IOPS** (`feedback_bench_rig_confounds`): 3 confounds; trust
  pprof + unit tests; scw-CLI S3 creds (not pulumi — 403).
- **Competitor cells are build-independent** (`feedback_bench_competitor_cells_build_independent`):
  reuse baselines via `--resume`; competitors never run dfs.
- **Conventions** (`feedback_bench_conventions`): prod binary, pure Go, measure-don't-assume,
  document every column.
- **Perf attempts ledger** (`reference_perf_attempts_ledger`): read before any perf work so
  refuted approaches (group-commit ×3, io_uring, etc.) aren't re-attempted.
- **Tear down VMs when done** — and only the verified bench VM.
- **Known SMB3 write wall:** DittoFS SMB3 write cells previously FAILed EDEADLK = badger SSI
  conflict in CommitBlock (`project_smb3_bench_edeadlk_ssi`, task #39). Check whether the
  write-amp fix (#1841) changed this.

---

## Pending housekeeping (needs a user decision — NOT blocking benchmarks)

Local `develop` is STALE (at `befb29a6`, pre-v0.28.1) and the working tree holds the **user's
uncommitted WIP that must NOT be disturbed**: a README "DittoFS PRO" section (references
`assets/pro-dashboard.png`) plus `bench.yaml`, `bench-cache-results/`,
`pkg/metadata/create_pipeline_bench_test.go`. Because of this:
- `graphify update` after this session's merges is DEFERRED.
- Committing the `.planning` cleanup (the stale-plan deletion the user asked for) is DEFERRED.
Both need the user to say whether that WIP is theirs to keep (work around it) or safe to set
aside so develop can be brought current. PR merges were unaffected (done via `gh` on origin).

## Worktree cleanup
- `/Users/marmos91/dittofs-worktrees/1828-plan` — created for the #1828 design; safe to remove
  (`git worktree remove --force`) unless #1828 execution starts.
- Many stale worktrees from prior sessions exist under `~/dittofs-worktrees/` — not urgent.

## After Phase 3 (roadmap tail)
Phase 4 = HA scalability (brainstorm+design multi-instance) + Features (share snapshots v2 —
restore a snapshot onto a NEW share; metadata backup to S3 for sqlite/badger/postgres;
single-share import/export across instances). Then #1715 Waves 2/3 fold into #1828 execution.
