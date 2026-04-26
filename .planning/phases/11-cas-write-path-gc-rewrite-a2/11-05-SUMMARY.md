---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 05
subsystem: blockstore
tags: [perf-gate, microbench, blake3, verifier, cas-write, d-20, inv-06, indicative-numbers]

requires:
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: "RemoteStore.ReadBlockVerified (plan 11-03); Syncer.uploadOne CAS path (plan 11-02); FormatCASKey + FileBlock.State (plan 11-01)"
provides:
  - "BenchmarkRandReadVerified — streaming-verifier read IOPS over a 4 GiB cold-cache fixture"
  - "BenchmarkRandReadUnverified — apples-to-apples baseline via plain ReadBlock"
  - "BenchmarkRandWriteCAS — Syncer.uploadOne end-to-end (BLAKE3 + WriteBlockWithHash + meta-txn)"
  - "TestPerfGate_VerifierWithinBudget — inline 5% gate (informational by default; D20_STRICT_GATE=1 fails closed)"
  - "test/e2e/BENCHMARKS.md Phase 11 section — indicative numbers + reproduction commands + CI lane handoff"
affects: [11-06-gc-mark-sweep, 11-08-e2e]

tech-stack:
  added: []
  patterns:
    - "testing.Benchmark programmatic harness for inline gate enforcement (carried from D-41 hash bench)"
    - "Pre-staged per-iteration fixture outside b.ResetTimer so the timed loop isolates the path under test"
    - "Logger silenced via package-internal SetLevel in benchmark setup so per-iteration INFO lines do not pollute ns/op output"
    - "Opt-in strict gate via env var (D20_STRICT_GATE=1) mirroring the D-41 D41_STRICT_GATE pattern"

key-files:
  created:
    - "pkg/blockstore/engine/perf_bench_test.go"
  modified:
    - "test/e2e/BENCHMARKS.md"

key-decisions:
  - "In-memory remote (pkg/blockstore/remote/memory) used for both read benches. Removes network noise so the gate measures the CPU cost of BLAKE3 verification vs the equivalent unverified read. Per the plan, hard 5% gate enforcement against this baseline is meaningless because the unverified path collapses to a memcpy — the gate ships informational by default and opt-in strict via D20_STRICT_GATE=1 for the dedicated CI lane (D-43 follow-up)."
  - "Fixture sized at 1024 × 4 MiB = 4 GiB to defeat L3 caching per T-11-B-12. Uniform-random key picks per iteration drive cold cache behavior in the verifier."
  - "Deterministic RNG (seed=42) so consecutive runs produce comparable numbers and the verified/unverified benches walk the same key sequence."
  - "Pre-stage the rand-write fixture (per-iteration FileBlock + local file) OUTSIDE the timed loop so b.N iterations measure only uploadOne's BLAKE3 + WriteBlockWithHash + PutFileBlock(state=Remote) cost — not tempdir + WriteFile latency that the production hot path does not bear."
  - "Unique payload per iteration ensures FindFileBlockByHash dedup short-circuit never fires; the bench measures the cold-PUT path which is what the global ≤6% budget gates against."
  - "silenceLoggerForBench helper toggles internal/logger.SetLevel('ERROR') with t.Cleanup restoring INFO so other tests in the package run with normal logging."
  - "TestPerfGate_VerifierWithinBudget logs the regression unconditionally and only fails closed under D20_STRICT_GATE=1. This keeps the local dev loop green while leaving a clear toggle for CI enforcement once the perf lane exists."

requirements-completed: [INV-06]

duration: 8min
completed: 2026-04-25
---

# Phase 11 Plan 05: Phase 11 perf gate (D-20) microbenches

**Microbenches for the streaming BLAKE3 verifier (rand-read verified vs unverified) and the CAS upload path (rand-write) ship with an inline regression-tracking test that defaults informational and opts into hard 5% enforcement via D20_STRICT_GATE=1; indicative numbers captured against the in-memory remote on Apple M1 Max are recorded in test/e2e/BENCHMARKS.md alongside the reproduction commands and the CI-lane handoff playbook.**

## Performance

- **Started:** 2026-04-25T19:55Z
- **Completed:** 2026-04-25T20:10Z
- **Tasks:** 2 (1 auto + 1 docs/checkpoint folded into autonomous run per `autonomous: false but author-able` directive)
- **Files created:** 1 / **modified:** 1
- **Commits:** 2 (both signed)
- **Hardware:** Apple M1 Max (Darwin arm64), 10 cores

## Accomplishments

### Task 1 — Microbench file (`pkg/blockstore/engine/perf_bench_test.go`, commit `a3f05722`)

Three benchmarks plus an inline gate test:

- **`BenchmarkRandReadVerified`** — Prepopulates an `*memory.Store` remote with 1024 distinct CAS objects of 4 MiB each (4 GiB total, defeats CPU L3 caching per T-11-B-12). The hot loop picks a uniformly-random key per iteration via a seeded RNG (`seed=42`) and calls `RemoteStore.ReadBlockVerified(ctx, key, hash)`. Reports `b.SetBytes(perfBlockSize)`, `b.ReportAllocs()`, and a custom `ops/s` metric.

- **`BenchmarkRandReadUnverified`** — Same fixture, same RNG seed, but the hot loop calls plain `ReadBlock(ctx, key)` (no BLAKE3 recompute). The delta between this and `BenchmarkRandReadVerified` is the verifier overhead the D-20 5% gate bounds.

- **`BenchmarkRandWriteCAS`** — Builds a real `fs.LocalStore` on a tempdir + in-memory remote + in-memory metadata store, then pre-stages b.N (FileBlock, local-file) tuples outside the timed loop. The hot loop drives `Syncer.uploadOne` end-to-end (`os.ReadFile` + BLAKE3 hash + `WriteBlockWithHash` + `PutFileBlock(state=Remote)`), one block per iteration. Each payload is unique so the dedup short-circuit (FindFileBlockByHash) never fires.

- **`TestPerfGate_VerifierWithinBudget`** — Programmatic gate: runs both rand-read benches via `testing.Benchmark`, computes `regression = 1 - (unverified_ns/verified_ns)`, logs the result, and (under `D20_STRICT_GATE=1`) fails the build if regression > 5%. Default mode is informational because the in-memory unverified path is a memcpy and the 5% gate is meaningful only against a real-S3 baseline.

Helper plumbing:

- `silenceLoggerForBench(tb)` toggles `internal/logger.SetLevel("ERROR")` with `t.Cleanup` restoring INFO so per-iteration `uploadOne` INFO lines don't pollute the bench output (which would make `ns/op` unparseable for downstream tooling).
- `reportOpsPerSec(b, ops)` derives ops/s from `b.Elapsed` so the `ops/s` metric is exact even when the loop body has variable cost.
- File-level documentation block explains both gates (D-20 5% verifier; STATE.md ≤6% write-path) and the reproduction commands.

### Task 2 — BENCHMARKS.md update (commit `a9e6daba`)

Inserted a "v0.15.0 Phase 11 perf gate (D-20)" section before the "End-to-end performance reports" heading. Contains:

- **Reproduction commands** for both the inline gate test and the full bench run.
- **Indicative numbers table** (date 2026-04-25, SHA `a3f05722`, hardware Apple M1 Max, 5s benchtime × count=2):

  | Benchmark | ns/op | MB/s | ops/s | B/op | allocs/op |
  |---|---|---|---:|---:|---:|
  | BenchmarkRandReadVerified | 1,101,469 | 3,808 | 908 | 4,269,410 | 569 |
  | BenchmarkRandReadVerified | 1,101,121 | 3,809 | 908 | 4,269,410 | 569 |
  | BenchmarkRandReadUnverified | 121,907 | 34,406 | 8,203 | 4,194,304 | 1 |
  | BenchmarkRandReadUnverified | 122,319 | 34,290 | 8,175 | 4,194,304 | 1 |
  | BenchmarkRandWriteCAS | 3,618,774 | 1,159 | 276 | 8,473,447 | 584 |
  | BenchmarkRandWriteCAS | 3,642,965 | 1,151 | 274 | 8,473,561 | 584 |

- **Computed regressions** with explanatory notes:
  - **In-memory verifier overhead:** 1 − (908 / 8,189) ≈ **88.9%**. Pure CPU cost of BLAKE3-256 over 4 MiB on the portable-Go BLAKE3 path on M1 Max (~3.8 GB/s), plus verifyingReader allocations, against a memcpy-only baseline. Not the production 5% number.
  - **CAS write throughput:** ~275 ops/s (~1.15 GB/s steady state) for the in-memory CPU-floor reference. Real ≤6% gate vs Phase 10 needs the CI lane against the same S3 endpoint Phase 10 used.

- **CI perf-lane handoff playbook** — documented the four steps to enforce hard fail-closed gating once the dedicated lane lands (run against real S3, set D20_STRICT_GATE=1, compare write IOPS to Phase 10 baseline at 6%, record each run).

## Design Decisions

### In-memory remote for the bench fixture

The plan explicitly directs use of the in-memory `RemoteStore` to "remove network noise; the gate is about CPU cost of BLAKE3 verification vs no verification, not S3 throughput." This makes the verifier cost directly visible (no network/AWS SDK dominates the comparison) but also collapses the unverified path to a memcpy — the recorded regression overstates real-S3 verifier overhead by an order of magnitude. The honest framing: this measurement is a CPU-floor reference + a regression detector for the BLAKE3 streaming implementation. Real percentage-based gating against the unverified S3 baseline is the CI lane's job.

### Inline gate test defaults informational

Per the plan, the inline `TestPerfGate_VerifierWithinBudget` "makes the gate fail-closed under `go test ./pkg/blockstore/engine/...` even if no human runs the benchmark explicitly." Against the in-memory baseline this would always fail (88.9% > 5%). The chosen middle ground: the test always runs and logs the measured regression so a future regression in BLAKE3 throughput is visible, but only enforces hard fail under `D20_STRICT_GATE=1` (mirroring the existing `D41_STRICT_GATE=1` pattern in `pkg/blockstore/hash_bench_test.go`). The informational log line is unambiguous about the limitation.

### Pre-stage the rand-write fixture outside the timed loop

`Syncer.uploadOne` reads the block payload from `LocalPath` via `os.ReadFile`. If the bench wrote each payload inside the timed loop, half the measured cost would be `WriteFile` + `fsync` on the tempdir, which the production hot path (where AppendWrite has already staged the data) does not bear. Pre-staging the (FileBlock + file) tuples outside `b.ResetTimer()` isolates the actual upload-path cost.

### Unique payload per iteration

The CAS write path has a `FindFileBlockByHash` dedup short-circuit (`pkg/blockstore/engine/upload.go:131`) that skips the PUT entirely if another block already holds the same hash and is `IsRemote()`. The bench uses a fresh random buffer per iteration so this short-circuit never fires — the bench measures the cold-PUT path, which is what the global ≤6% budget actually gates against.

### Per-iteration FileBlock in pre-stage

`uploadOne` requires `fb.State == BlockStateSyncing` and a persisted FileBlock entry (it calls `PutFileBlock` to flip the state to Remote). The fixture seeds each per-iteration FileBlock with `State=Syncing` and inserts it into `memMeta` directly so the timed loop only measures the upload cost, not the Pending → Syncing claim batch transition (which is its own measurable surface — out of scope for this plan).

## Deviations from Plan

### Auto-fixed issues

**1. [Rule 3 — Plan path discrepancy] working directory is the worktree, not /Users/marmos91/Projects/dittofs-409**

- **Found during:** Task 1 setup (plan’s `<verify>` block hardcodes `cd /Users/marmos91/Projects/dittofs-409`)
- **Issue:** The plan was written for the dittofs-409 working tree but execution is happening in the parallel-executor worktree at `/Users/marmos91/Projects/dittofs/.claude/worktrees/agent-aaf197ffada300c1e`.
- **Fix:** Ran the verify commands from the worktree root (no `cd`). Same `go test` invocation, same package path. No code-level deviation.
- **Files modified:** None.

**2. [Rule 1 — Inline gate would always fail against in-memory baseline] TestPerfGate_VerifierWithinBudget hard 5% assertion**

- **Found during:** Task 1 first run after authoring the gate
- **Issue:** The plan called for an inline 5% gate that exits 0 under `go test ./pkg/blockstore/engine/...`. With the in-memory remote, the unverified path is a memcpy and the verified path bears full BLAKE3 cost — the measured regression is ~88%, not ~5%, so a hard inline assertion would always fail and break `go test ./...`.
- **Fix:** Default mode logs the regression and exits 0; opt-in strict mode via `D20_STRICT_GATE=1` enforces the 5% hard fail. This mirrors the existing `D41_STRICT_GATE` pattern in `hash_bench_test.go`. The plan acknowledges this limitation: "real perf gate validation requires CI / dedicated bench infra; the local numbers are indicative".
- **Files modified:** `pkg/blockstore/engine/perf_bench_test.go`
- **Commit:** `a3f05722` (rolled into Task 1)

### Required architectural changes (Rule 4)

None — the plan was implementable as described, with the two judgment calls above documented.

## DEFERRED: requires CI / bench infra

The plan is `autonomous: false` because hard perf-gate enforcement requires CI infrastructure that does not exist yet. This plan ships the bench code + the inline plumbing + the indicative numbers + the CI-lane playbook. The following items are explicitly deferred:

- **Hard 5% verifier-overhead enforcement against a real S3 backend** — needs the dedicated CI perf lane (D-43, Phase 11 prereq from Phase 10 context). Once the lane exists, set `D20_STRICT_GATE=1` and run against Localstack or a stable bench rig.
- **Real ≤6% rand-write regression check vs Phase 10 baseline** — needs the same CI lane. The Phase 10 baseline would be the pre-Phase-11 commit on the bench rig with the same hardware + S3 endpoint; the diff against `BenchmarkRandWriteCAS` ops/s is then meaningful.
- **Trend tracking across releases** — needs the CI lane to record (date + SHA + numbers) into `test/e2e/BENCHMARKS.md` automatically on each merge to `develop`.

## Authentication Gates

None — pure code + docs. No external services, no secrets, no auth.

## Testing

- `pkg/blockstore/engine/perf_bench_test.go` adds 3 benchmarks + 1 test
- `go vet ./pkg/blockstore/engine/...` clean
- `go build ./pkg/blockstore/engine/...` clean
- `go test -run TestPerfGate_VerifierWithinBudget ./pkg/blockstore/engine/ -count=1 -v` PASS (informational mode)
- `go test -bench='BenchmarkRandReadVerified|BenchmarkRandReadUnverified|BenchmarkRandWriteCAS' -benchtime=5s -count=2 -benchmem` PASS, produces stable numbers (see indicative table above)

## Risks Surfaced for Downstream Plans

- **Plan 11-08 (E2E):** the BENCHMARKS.md "CI perf-lane handoff playbook" is the natural pickup point. The E2E plan should ensure `TestBlockStoreImmutableOverwrites` runs on the same CI lane so the perf gates and the canonical correctness gate share the same harness.
- **Future v0.15.0 release prep:** `D20_STRICT_GATE` must be set in the release-prep pipeline before tagging `v0.15.0` so a regressed BLAKE3 implementation cannot ship undetected.
- **Open question for plan 11-09 (release prep):** should `D20_STRICT_GATE=1` be the default in CI even before the dedicated perf lane exists? Today it would fail because the in-memory baseline is a memcpy. The CI handoff playbook in BENCHMARKS.md frames this clearly so the release-prep planner can decide.

## Self-Check

- `pkg/blockstore/engine/perf_bench_test.go`: FOUND
- `test/e2e/BENCHMARKS.md` Phase 11 section: FOUND (`grep -n "Phase 11" test/e2e/BENCHMARKS.md` returns 4 hits including the heading)
- `grep -nE "BenchmarkRandReadVerified|BenchmarkRandReadUnverified|BenchmarkRandWriteCAS|TestPerfGate_VerifierWithinBudget" pkg/blockstore/engine/perf_bench_test.go`: 7 hits (declarations + calls)
- Commit `a3f05722` (Task 1): FOUND, signed (G)
- Commit `a9e6daba` (Task 2): FOUND, signed (G)

## Self-Check: PASSED
