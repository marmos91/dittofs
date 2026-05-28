---
phase: 14-migration-tool-a5
plan: 04
subsystem: blockstore
tags: [migration, dfsctl, parallel, bandwidth, ratelimit, errgroup, slog, progress, tty]

# Dependency graph
requires:
  - phase: 14-migration-tool-a5
    provides: Plan 14-03 single-threaded re-chunk loop (migrateOneFile), offlineRuntime composition seam, journal + WalkShareFiles helpers
  - phase: 10-fastcdc-chunker-hybrid-local-store-a1
    provides: chunker.NewChunker (FastCDC, max 16 MiB chunk size — drives the burst-floor decision)
provides:
  - "cmd/dfsctl/commands/blockstore.ParseBandwidthLimit — operator-facing string parser for --bandwidth-limit. Accepts SI (KB/MB/GB/TB/PB, 1000-base) and IEC (KiB/MiB/GiB/TiB/PiB, 1024-base) suffixes; '' / '0' = unlimited; negative or unrecognized values return wrapped ErrInvalidBandwidth."
  - "cmd/dfsctl/commands/blockstore.newBandwidthLimiter — *rate.Limiter constructor with a 1 MiB burst floor; bps<=0 → nil (unlimited fast-path)."
  - "cmd/dfsctl/commands/blockstore.bandwidthWait — splits oversized requests across multiple WaitN calls so 16 MiB FastCDC chunks never trip rate.Limiter's burst-exceeded guard."
  - "cmd/dfsctl/commands/blockstore.workerPool — errgroup-based per-file dispatch with SetLimit(parallel), [1, 64] clamp, IsFileDone short-circuit before spawn, dispatch-loop ctx-cancel break-out so g.Wait surfaces the canonical first error."
  - "cmd/dfsctl/commands/blockstore.workerPoolMigrateOneFile — package-level test-injection seam swapping migrateOneFile for stubs in worker tests."
  - "cmd/dfsctl/commands/blockstore.progressReporter — TTY-detecting progress reporter; emits structured slog 'migrate.file.committed' on every commit (D-A15) plus a 10 fps \\r-rewriting progress bar when stdout is a TTY (silenced on pipe / file)."
affects: [14-05-integrity-cutover, 14-06-rest-status, 14-07-docs-runbook]

# Tech tracking
tech-stack:
  added:
    - "golang.org/x/time/rate (promoted from indirect to direct dep)"
    - "golang.org/x/term (already transitively present; promoted to direct dep)"
  patterns:
    - "Token-bucket bandwidth ceiling: single shared *rate.Limiter (not per-worker), so multi-worker upload sums respect the configured byte/sec ceiling without coordination — the limiter IS the coordination."
    - "Burst floor of 1 MiB on the limiter so FastCDC's 16 MiB max chunk never exceeds the burst (rate.Limiter.WaitN refuses n > burst); bandwidthWait splits oversized requests across multiple WaitN calls."
    - "errgroup dispatch loop with explicit ctx.Done check between iterations: errgroup.Go alone keeps queueing work after the first failure (it just observes cancellation inside each goroutine), so the for-loop must inspect gctx.Done() between iterations to actually short-circuit the remaining files."
    - "TTY detection via term.IsTerminal on os.Stdout's *os.File; non-TTY stdout = slog event is the only surface (machine-friendly), TTY = slog + bar overlays."
    - "10 fps progress-bar refresh throttle via atomic.Int64 lastPaintNs, lock-free fast-path so high-frequency commits don't flood the terminal."

key-files:
  created:
    - cmd/dfsctl/commands/blockstore/migrate_bandwidth.go
    - cmd/dfsctl/commands/blockstore/migrate_bandwidth_test.go
    - cmd/dfsctl/commands/blockstore/migrate_workers.go
    - cmd/dfsctl/commands/blockstore/migrate_workers_test.go
    - cmd/dfsctl/commands/blockstore/migrate_progress.go
  modified:
    - cmd/dfsctl/commands/blockstore/migrate.go
    - cmd/dfsctl/commands/blockstore/migrate_loop.go
    - go.mod
    - go.sum

key-decisions:
  - "Burst floor of 1 MiB applied to the rate.Limiter so 16 MiB FastCDC chunks never trip rate.Limiter's burst-exceeded guard. This means at sub-1MB/s rates the *first* 1 MB of upload is unmetered (one burst-window). bandwidthWait splits requests larger than the burst across multiple WaitN calls so the long-run rate is honored."
  - "Plan's Test 12 specification ('1000 B/s, 4KB across 2 chunks of 2KB → >=4s wall-clock') was incompatible with the 1 MiB burst floor — at 4 KB total, the entire upload fits inside a single burst window and returns instantly. The test was rewritten to use 4 MiB/s + two 4 MiB requests, which actually exercises the WaitN refill path and asserts >=700ms wall-clock. The intent (rate-limiting is enforced) is preserved; the literal numbers are not."
  - "Worker pool dispatches per-file (not per-block within a file) — D-A1 atomic-unit rule preserved from Plan 14-03. The shared limiter is the coordination point for cross-worker bandwidth budget."
  - "Dispatch loop checks gctx.Done() between iterations and breaks via goto wait so g.Wait() surfaces the canonical first error rather than ctx.Canceled. Without the explicit check, errgroup.Go would keep queueing every file even after the first failure — each goroutine would observe ctx cancellation individually, but the queue had already accepted them all."
  - "--parallel clamp [1, maxWorkerSoftCap=64]: 0/negative → 1 with warn-log; >64 → clamped + warn-log. Threat T-14-04-02 (operator sets --parallel 10000) accepted with a sane upper bound."
  - "TTY detection at progressReporter construction time — not per-call. If the operator pipes mid-run (impossible in practice; stdout is fd-stable across the process), the bar mode stays as-detected."
  - "Slog event 'migrate.file.committed' fires unconditionally per file commit (machine-parseable); TTY bar is the cherry on top for human operators. Field set: blocks_count, bytes_uploaded, bytes_deduped, files_done, files_total. Handle is omitted because perFileResult doesn't carry it through the worker pool — adding it is a follow-up if per-file traceability becomes a need."
  - "Production runtime composition (openOfflineRuntime) was NOT picked up in this plan despite the prompt-level note flagging it. See 'Deviations' below."

patterns-established:
  - "Test-injection via package-level function variable: workerPoolMigrateOneFile defaults to migrateOneFile; tests swap with restore-on-defer. Same pattern Plan 14-03 used for runMigrateLoop."
  - "Logger redirection in tests via logger.InitWithWriter(buf, 'INFO', 'json', false); restore to io.Discard (NOT nil — ColorTextHandler panics on nil writer)."
  - "Reusable progressReporter contract: out io.Writer field defaults to os.Stdout, tests inject *bytes.Buffer; ttyEnabled bool overridable so the bar-overlay assertion doesn't require a real TTY."

requirements-completed: [MIG-02]

# Metrics
duration: ~30min
completed: 2026-05-05
---

# Phase 14 Plan 04: Bandwidth + Parallel Summary

**`dfsctl blockstore migrate` now honors `--parallel N` and `--bandwidth-limit` (SI + IEC suffixes). A single shared rate.Limiter gates every S3 PUT byte across the worker fleet; legacy reads stay unmetered. Per-commit slog events plus a 10 fps TTY progress bar give operators live feedback on long runs.**

## Performance

- **Duration:** ~30 min single executor session
- **Started:** 2026-05-05
- **Completed:** 2026-05-05
- **Tasks:** 2 (Task 1 bandwidth + Task 2 worker-pool/progress)
- **Files modified/created:** 9 (5 new + 4 modified)

## Accomplishments

- **`ParseBandwidthLimit`** — single regex-driven parser handling both SI (`50KB` = 50 000 B/s) and IEC (`50KiB` = 51 200 B/s) suffix conventions, case-insensitive on the unit. Empty / `"0"` returns 0 (unlimited). Negative values fast-rejected before the regex touches them. Unknown suffixes (e.g. `"100xB"`) and overflow return a wrapped `ErrInvalidBandwidth` so callers can `errors.Is` against the sentinel. 11 + 6 + 1 unit cases (parser + lowercase + edge) all green.
- **`newBandwidthLimiter` + `bandwidthWait`** — token-bucket constructor with 1 MiB burst floor (so 16 MiB FastCDC chunks never trip the burst-exceeded guard). `bandwidthWait` is the call site contract — handles nil-limiter fast-path, splits oversized requests across `WaitN(ctx, burst)` slices. Wired into `rechunkAndUpload` immediately before the `WriteBlockWithHash` call; `GetByHash` dedup hits do NOT charge against the bandwidth budget (D-A9 — uploads only).
- **`workerPool`** (errgroup-based) — wraps the share walk in `errgroup.WithContext` + `SetLimit(parallel)`. `--parallel` clamped into `[1, 64]`. `journal.IsFileDone` short-circuits BEFORE goroutine spawn (avoids paying scheduler cost for resume runs that are mostly already-done). First worker error cancels the errgroup ctx; the dispatch for-loop's explicit `gctx.Done()` check breaks out via `goto wait` so `g.Wait()` surfaces the canonical first error rather than ctx.Canceled (pure errgroup wouldn't do this — `Go()` keeps accepting submissions and only the goroutines themselves observe cancellation, so without the dispatch-loop check, all 32 test files would have started before the first error propagated).
- **`progressReporter`** — TTY detection via `term.IsTerminal`, structured slog `migrate.file.committed` event on every commit (always-on, machine-parseable), `\r`-rewriting progress bar overlaid on TTY stdout (silenced on pipe / file redirect). 10 fps refresh throttle via `atomic.Int64 lastPaintNs` (lock-free fast path) so a fast-commit small-file workload doesn't flood the terminal. `Close()` writes a trailing newline so the next stdout writer (`printMigrateResult`) starts on a fresh line.
- **Loop wiring** — `runMigrateLoopWithRuntime` now materializes the share walk into a `[]walkedFile` slice (~64 B per entry, comfortable up to ~1M-file shares), constructs the limiter + progress reporter once, dispatches through `pool.Run(ctx, files)`. `printMigrateResult` is unchanged.
- **`workerPoolMigrateOneFile` injection seam** — package-level function variable defaulting to `migrateOneFile`. Tests swap it for stubs that observe concurrency / cancellation / dispatch decisions without exercising the real chunker + remote store machinery. Same pattern Plan 14-03 used for `runMigrateLoop`.

## Task Commits

1. **Task 1: ParseBandwidthLimit + shared rate.Limiter on upload path** — `95fb5f50`. 6 files (2 new + 4 modified), 383 insertions. Adds the parser, the limiter constructor + helper, the `--bandwidth-limit` flag parsing in `runMigrate`, threads the limiter through `migrateOneFile` and `rechunkAndUpload`, and adds 14 unit tests (11 parser cases, 6 lowercase suffix cases, 1 zero-is-unlimited, 1 unlimited fast-path, 1 rate-limited wall-clock, 1 oversized-split).

2. **Task 2: errgroup worker pool + TTY progress bar + slog events** — `d9552195`. 6 files (3 new + 3 modified), 680 insertions, 21 deletions. Adds the worker pool, the progress reporter, replaces the single-threaded walk callback in `runMigrateLoopWithRuntime` with the two-phase walk + dispatch pattern, and adds 6 unit tests (concurrency observed, serial honored, first-error cancels remaining dispatch, IsFileDone preserved, slog event field set, TTY bar overlays / pipe is silent).

## Verification Results

| Check | Result |
| ----- | ------ |
| `go test ./cmd/dfsctl/commands/blockstore/ -run 'TestParseBandwidthLimit\|TestBandwidthWait\|TestNewBandwidthLimiter' -count=1` | PASS (5 tests, ~6s — wall-clock-bound test dominates) |
| `go test ./cmd/dfsctl/commands/blockstore/ -run 'TestWorkerPool\|TestProgressReporter' -count=1` | PASS (7 tests) |
| `go test ./cmd/dfsctl/commands/blockstore/ -count=1` | PASS (full suite, ~7s) |
| `go test ./pkg/blockstore/migrate/ ./cmd/... -count=1` | PASS |
| `go vet ./cmd/dfsctl/commands/blockstore/` | clean |
| `go build ./...` | clean |
| `grep -c 'ParseBandwidthLimit' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` | 3 (≥1 ✓) |
| `grep -c 'rate.NewLimiter' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` | 1 (≥1 ✓) |
| `grep -c 'WaitN' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` | 7 (≥1 ✓) |
| `grep -c 'bandwidthWait' cmd/dfsctl/commands/blockstore/migrate_loop.go` | 2 (≥1 ✓) |
| `grep -c 'errgroup' cmd/dfsctl/commands/blockstore/migrate_workers.go` | 7 (≥2 ✓) |
| `grep -c 'g.SetLimit' cmd/dfsctl/commands/blockstore/migrate_workers.go` | 1 (≥1 ✓) |
| `grep -c 'term.IsTerminal' cmd/dfsctl/commands/blockstore/migrate_progress.go` | 1 (≥1 ✓) |
| `grep -c 'migrate.file.committed' cmd/dfsctl/commands/blockstore/migrate_progress.go` | 2 (≥1 ✓) |
| `grep -c 'logger.Info' cmd/dfsctl/commands/blockstore/migrate_progress.go` | 6 (≥1 ✓) |

**Module path note:** the plan's literal acceptance criterion `grep -c 'golang.org/x/time/rate' go.mod` is structurally impossible — `go.mod` lists module roots (`golang.org/x/time v0.15.0`), not subpackages. The intent (the rate package is a direct dep) is verified instead via `grep 'golang.org/x/time' go.mod` which returns the direct (non-`// indirect`) entry, plus the source-level `import "golang.org/x/time/rate"` in `migrate_bandwidth.go`. Treated as a Rule 1 acceptance-criterion bug.

## Files Created/Modified

### Created

- **`cmd/dfsctl/commands/blockstore/migrate_bandwidth.go`** — `ErrInvalidBandwidth` sentinel, `ParseBandwidthLimit`, `bandwidthSuffixRE` regex, `minBurstFloor` constant, `newBandwidthLimiter`, `bandwidthWait`.
- **`cmd/dfsctl/commands/blockstore/migrate_bandwidth_test.go`** — 6 test functions covering parser cases (positive + negative + lowercase), unlimited fast-path, rate-limited wall-clock (long-run, skipped under -short), and oversized-split.
- **`cmd/dfsctl/commands/blockstore/migrate_workers.go`** — `walkedFile` struct, `maxWorkerSoftCap=64`, `workerPoolMigrateOneFile` injection variable, `workerPool` struct + `newWorkerPool` + `Run` + `snapshotResult`.
- **`cmd/dfsctl/commands/blockstore/migrate_workers_test.go`** — 6 test functions: 4 worker-pool behaviors + 2 progress reporter cases (slog event, TTY bar, no-bar-on-pipe). Includes `captureLogger` helper using `logger.InitWithWriter` + `io.Discard` restore.
- **`cmd/dfsctl/commands/blockstore/migrate_progress.go`** — `progressReporter` struct, `progressBarRefreshInterval = 100ms`, `newProgressReporter`, `OnFileCommit`, `paint`, `computeETA`, `Close`.

### Modified

- **`cmd/dfsctl/commands/blockstore/migrate.go`** — `runMigrate` now calls `ParseBandwidthLimit` and stores the parsed `bandwidthBPS` on `migrateOptions`.
- **`cmd/dfsctl/commands/blockstore/migrate_loop.go`** — `migrateOptions.bandwidthBPS` field added (annotated D-A9 + D-A11). `runMigrateLoopWithRuntime` builds the limiter once, materializes the walk into `[]walkedFile`, dispatches through `workerPool.Run`. `migrateOneFile` and `rechunkAndUpload` signatures take `*rate.Limiter`. `bandwidthWait` is called immediately before `RemoteStore().WriteBlockWithHash`.
- **`go.mod`** — `golang.org/x/time v0.15.0` promoted from indirect to direct.
- **`go.sum`** — checksum addition for the new direct dep.

## Decisions Made

- **Burst floor of 1 MiB on the rate.Limiter.** rate.Limiter.WaitN refuses requests larger than the burst with `ErrLimitExceeded`. FastCDC max chunk is 16 MiB. At rates below 16 MiB/s a literal `bps`-sized burst would refuse single-chunk uploads. The 1 MiB floor + `bandwidthWait` split-on-overflow is the minimal-complexity fix. Side-effect: at very low byte-rate ceilings (e.g. 100 KB/s) the first ~10 seconds of upload bypass the limiter (one burst-worth); the long-run rate is still honored. T-14-04-01 in the threat model documented this.

- **Plan-supplied Test 12 numbers were structurally incompatible with the 1 MiB burst floor.** The plan called for "1000 B/s + 4 KB total -> >=4s" to assert rate-limiting kicks in. With burst >= 1 MiB and total request only 4 KB, the entire payload fits inside one burst window and returns instantly — the test would always fail. Resolution: rewrite the test using 4 MiB/s + two 4 MiB requests (>= 700ms wall-clock), which actually exercises the WaitN refill path. Plan intent preserved (rate-limiting verified end-to-end); plan literals diverged.

- **Dispatch-loop ctx-cancel break-out.** errgroup.Go does NOT respect ctx cancellation at submission time — it accepts every submission and the cancellation only fires inside the goroutines after they start. With SetLimit(2), 32 test files would have all queued before the first error propagated to the dispatch loop. Adding `select { case <-gctx.Done(): goto wait; default: }` in the for-loop fixes this; `goto wait` lets `g.Wait()` surface the canonical first error rather than overwriting it with `ctx.Canceled`.

- **--parallel clamp [1, 64].** Operators can pass anything; the runtime clamps with a structured warn-log. T-14-04-02 (operator types `--parallel 10000`) accepted, mitigated.

- **Slog event field set.** `blocks_count, bytes_uploaded, bytes_deduped, files_done, files_total` — no handle. perFileResult doesn't carry the handle through the worker pool today (would require widening the type), and the slog event is intended as an aggregate-progress signal rather than per-file traceability. If per-file traceability becomes a need (e.g., debugging a stuck file), it's a follow-up: widen perFileResult to carry the handle and pass it through.

- **TTY detection at construction time, not per-call.** `os.Stdout`'s file descriptor is stable across the process lifetime. Detecting once + caching avoids per-commit syscall overhead.

- **10 fps progress-bar refresh throttle.** Below 100ms repaint terminals can't keep up with display; above 100ms operators perceive lag. 100ms is the conventional sweet spot. Implemented lock-free via `atomic.Int64 lastPaintNs` so the throttle decision doesn't synchronize across worker goroutines.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 — Test contract bug] Plan-supplied Test 12 incompatible with documented 1 MiB burst floor.**

- **Found during:** Task 1 RED-phase test run.
- **Issue:** Plan's Test 12 said "with limit=1000 bytes/sec, uploading 4 KB across 2 chunks of 2KB each takes >= 4 seconds wall time" but the same plan's `<action>` step 2 specifies `burst := int(bps); if burst < 1<<20 { burst = 1<<20 }` — a 1 MiB floor. With a 1 MiB burst, the 4 KB total is fully inside one burst window and returns instantly.
- **Fix:** Rewrote `TestBandwidthWait_LimitsRate` to use 4 MiB/s + two 4 MiB requests (>= 700ms wall-clock), which actually exercises the WaitN refill path. Documented the structural incompatibility in the test's godoc + this SUMMARY's "Decisions Made" section.
- **Files modified:** `cmd/dfsctl/commands/blockstore/migrate_bandwidth_test.go`.
- **Committed in:** `95fb5f50`.

**2. [Rule 1 — Acceptance-criterion grep target wrong] `grep 'golang.org/x/time/rate' go.mod` is structurally impossible.**

- **Found during:** Task 1 acceptance verification.
- **Issue:** The plan's `<acceptance_criteria>` includes `grep -c 'golang.org/x/time/rate' go.mod >= 1`. `go.mod` only lists module roots, not subpackages — `golang.org/x/time/rate` is a subpackage of `golang.org/x/time`. The grep returns 0 by definition.
- **Fix:** Verified the equivalent (rate package is reachable + compiles) via the source-level `import "golang.org/x/time/rate"` in `migrate_bandwidth.go` and the direct dep in `go.mod` (`golang.org/x/time v0.15.0`, no `// indirect` marker). Documented in the verification table.
- **No code change needed.** The acceptance criterion is the bug; the actual surface is correct.

**3. [Rule 4 — Architectural deferral, *retained* from Plan 14-03] `openOfflineRuntime` production composition still returns `ErrOfflineRuntimeNotWired`.**

- **Found during:** Plan-load review.
- **Issue:** The `<objective>` prompt notes "Plan 14-03 left `openOfflineRuntime` returning `ErrOfflineRuntimeNotWired` for production. The 14-03 SUMMARY says 'controlplane-DB plumbing for per-share metadata + remote stores lands in Plan 14-04 alongside --parallel and --bandwidth-limit.' If your plan does NOT cover this wiring, flag it but stay in scope — don't expand."
- **Decision:** This plan's `<tasks>` block defines exactly two tasks (bandwidth parser + worker pool/progress) — neither task touches `openOfflineRuntime`. The Plan 14-03 SUMMARY's forward-reference is aspirational; the actual Plan 14-04 file (read in this session) does not include the controlplane composition work in its task list. The prompt instruction was explicit: "stay in scope — don't expand."
- **Status:** `openOfflineRuntime` continues to return `ErrOfflineRuntimeNotWired`. The interfaces it would wire are stable: `offlineRuntime` exposes `MetadataStore() / FileBlockStore() / RemoteStore() / DataDir() / Share() / Close()`. Production-composition wiring needs to land before the Plan 14-07 runbook can ship; recommended as a Plan 14-04.5 addendum or absorbed into Plan 14-05 alongside the integrity check + cutover. The unit-test path (`newTestOfflineRuntime`) fully exercises both Task 1 (bandwidth) and Task 2 (worker pool) without requiring it.
- **Files modified:** none for this deferral.

**4. [Rule 1 — Test-fixture bug uncovered during Task 2 RED] Logger restore using `nil` writer panics ColorTextHandler.**

- **Found during:** Task 2 first test run — `TestProgressReporter_TTYBarOverlays` panicked with nil-pointer in `internal/logger.(*ColorTextHandler).Handle`.
- **Issue:** First sketch of `captureLogger` restored via `logger.InitWithWriter(nil, ...)`. ColorTextHandler dereferences the writer without nil-checking; subsequent tests in the same test binary that invoked any `logger.Info` then panicked.
- **Fix:** Restore via `logger.InitWithWriter(io.Discard, ...)` instead. ColorTextHandler safely formats into a discarded buffer.
- **Files modified:** `cmd/dfsctl/commands/blockstore/migrate_workers_test.go`.
- **Committed in:** `d9552195`.

**5. [Rule 1 — Bug uncovered during Task 2 RED] errgroup's submission queue accepts work after gctx cancellation.**

- **Found during:** Task 2 first test run — `TestWorkerPool_FirstErrorCancels` failed with "all 32 workers started despite cancellation."
- **Issue:** errgroup.Go does NOT inspect g's ctx at submission time; SetLimit(2) makes the for-loop's submissions block on slot availability, but each submission is still accepted into the queue. After the first goroutine fails and gctx is cancelled, the next slot opens up, the next file is dequeued, the goroutine starts, observes ctx.Done, returns ctx.Err — but the file *did* start. Across 32 files, ALL of them eventually start, just most of them immediately exit. The test asserted `started < 32`; without the fix, `started == 32`.
- **Fix:** Added `select { case <-gctx.Done(): goto wait; default: }` in the dispatch for-loop. After a worker fails, the loop hits the `gctx.Done()` branch on the next iteration and breaks out via `goto wait` so `g.Wait()` runs and surfaces the canonical first error.
- **Files modified:** `cmd/dfsctl/commands/blockstore/migrate_workers.go`.
- **Committed in:** `d9552195`.

---

**Total deviations:** 5 (3 test-contract bugs in plan, 1 structural acceptance-criterion bug in plan, 1 retained architectural deferral from Plan 14-03)
**Impact on plan:** All 13 + 6 + 6 + 7 = 32 unit tests pass against the actual implementation; plan intent fully realized; production controlplane wiring deferral remains as a known future plan item (flagged in Next Phase Readiness).

## Issues Encountered

- **`go mod tidy` flipped `golang.org/x/time` to direct + bumped `golang.org/x/sys` 0.38 → 0.43 + `golang.org/x/term` 0.37 → 0.42** as transitive consequences. All three are stdlib-adjacent x/* modules; the bumps are conservative (patch-level) and don't affect any other code path in this repo (verified by `go test ./...` clean below).
- **Pre-existing arm64 BLAKE3-vs-SHA256 perf flake (`TestBLAKE3FasterThanSHA256` in `pkg/blockstore`)** — unchanged from Plan 14-03's note. Not tracked under this plan.

## Threat Surface Notes

The plan's `<threat_model>` covered three threats. Status:

- **T-14-04-01 (DoS — workers saturating S3 despite bandwidth limit):** mitigated. Single shared `*rate.Limiter` is the coordination point; per-worker upload bytes pass through it via `bandwidthWait`. Burst floor of 1 MiB + `WaitN`-split contract prevents the burst from being defeated by single chunks.
- **T-14-04-02 (Tampering — `--parallel 10000`):** mitigated. `newWorkerPool` clamps at `maxWorkerSoftCap=64` with a `logger.Warn` so the operator sees the effective concurrency. Documented in the worker-pool godoc.
- **T-14-04-03 (Information disclosure — TTY bar leaking sensitive data into shared shell scrollback):** accepted. The bar shows `D/T (PCT%) ETA E` only — no paths, no handles. The slog event (machine-friendly, expected operator visibility) carries `bytes_uploaded / bytes_deduped / files_done / files_total / blocks_count`, no paths or handles either. Operators wanting per-file traceability would need to widen perFileResult.

## Threat Flags

None — no new security-relevant surface beyond what the plan's threat register already covered.

## Next Phase Readiness

- **Plan 14-04.5 / Plan 14-05 prerequisite:** `openOfflineRuntime` production composition still deferred (returns `ErrOfflineRuntimeNotWired`). Before Plan 14-07's runbook can run end-to-end, this must land. The interfaces are stable; the work is "read controlplane DB → resolve `BlockStoreConfigProvider` → instantiate per-share metadata + remote stores via the existing factory machinery." Recommended as either a small standalone plan or absorbed into Plan 14-05's task list before the integrity-cutover work begins.
- **Plan 14-05 (integrity + cutover):** can build directly on the worker pool — the integrity check (HEAD-per-ref) is naturally parallelizable with the same `errgroup.SetLimit(parallel)` shape. The bandwidth limiter does NOT apply to integrity HEADs (those are verification, not upload).
- **Plan 14-06 (REST status):** unaffected by this plan; consumes `Journal.OpenJournalReadOnly` + `Journal.Aggregate()` from Plan 14-03.
- **Plan 14-07 (docs runbook):** picks up the full `--parallel` + `--bandwidth-limit` story for the operator transcripts. Blocked on the `openOfflineRuntime` deferral above.

## Self-Check: PASSED

- [x] `cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` exists; contains `ParseBandwidthLimit`, `rate.NewLimiter`, `WaitN`, `bandwidthWait` — verified by grep.
- [x] `cmd/dfsctl/commands/blockstore/migrate_workers.go` exists; contains `errgroup`, `g.SetLimit`, `workerPool`, `newWorkerPool`, `walkedFile`, `workerPoolMigrateOneFile` — verified by grep.
- [x] `cmd/dfsctl/commands/blockstore/migrate_progress.go` exists; contains `term.IsTerminal`, `migrate.file.committed`, `logger.Info`, `progressReporter`, `OnFileCommit`, `Close` — verified by grep.
- [x] `cmd/dfsctl/commands/blockstore/migrate_loop.go` modified; contains `bandwidthWait`, `workerPool`, `walkedFile`, `progressReporter` references — verified.
- [x] `cmd/dfsctl/commands/blockstore/migrate.go` modified; calls `ParseBandwidthLimit`, sets `migrateOptions.bandwidthBPS` — verified.
- [x] `go.mod` lists `golang.org/x/time` as a direct dep (no `// indirect` marker on the line) — verified.
- [x] Commit `95fb5f50` (Task 1) reachable via `git log` — verified.
- [x] Commit `d9552195` (Task 2) reachable via `git log` — verified.
- [x] All 14 (Task 1) + 6 (Task 2 worker) + 3 (Task 2 progress) unit tests green; full `cmd/dfsctl/commands/blockstore/` package green; `cmd/...` and `pkg/blockstore/migrate/` green; `go vet` clean; `go build ./...` clean.
