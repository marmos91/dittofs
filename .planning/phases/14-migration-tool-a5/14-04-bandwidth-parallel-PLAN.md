---
phase: 14-migration-tool-a5
plan: 04
type: execute
wave: 4
depends_on: [14-03-migrate-tool-core]
files_modified:
  - cmd/dfsctl/commands/blockstore/migrate.go
  - cmd/dfsctl/commands/blockstore/migrate_loop.go
  - cmd/dfsctl/commands/blockstore/migrate_workers.go
  - cmd/dfsctl/commands/blockstore/migrate_bandwidth.go
  - cmd/dfsctl/commands/blockstore/migrate_progress.go
  - cmd/dfsctl/commands/blockstore/migrate_workers_test.go
  - cmd/dfsctl/commands/blockstore/migrate_bandwidth_test.go
  - go.mod
  - go.sum
autonomous: true
requirements: [MIG-02]
tags: [bandwidth, parallel, errgroup, ratelimit, progress, slog]
must_haves:
  truths:
    - "Plan 03's single-threaded loop becomes a worker pool of size --parallel (default 4) using errgroup; per-file work is the unit dispatched to workers (D-A1 + D-A10)"
    - "All S3 PUT bytes pass through a single shared *rate.Limiter; workers WaitN(ctx, len(chunkBytes)) before PUT (D-A9)"
    - "--bandwidth-limit accepts decimal (KB/MB/GB → 1000-base) and binary (KiB/MiB/GiB → 1024-base) suffixes; 0 / unset = unlimited (D-A11)"
    - "Legacy reads from local disk are NOT metered (D-A9 — uploads only)"
    - "Progress reporting: structured slog.Info events on every per-file commit; when stdout is a TTY, a per-second-refreshed progress bar overlays (D-A15)"
  artifacts:
    - path: cmd/dfsctl/commands/blockstore/migrate_workers.go
      provides: "errgroup-based worker pool over the file walk"
      contains: "errgroup"
    - path: cmd/dfsctl/commands/blockstore/migrate_bandwidth.go
      provides: "ParseBandwidthLimit + token-bucket Limiter wired into PutBlock path"
      contains: "rate.Limiter"
    - path: cmd/dfsctl/commands/blockstore/migrate_progress.go
      provides: "TTY progress bar + slog event emitter"
      contains: "term.IsTerminal"
  key_links:
    - from: cmd/dfsctl/commands/blockstore/migrate_workers.go
      to: cmd/dfsctl/commands/blockstore/migrate_loop.go
      via: "workers call migrateOneFile"
      pattern: "migrateOneFile"
    - from: cmd/dfsctl/commands/blockstore/migrate_bandwidth.go
      to: cmd/dfsctl/commands/blockstore/migrate_loop.go
      via: "PutBlock takes a *rate.Limiter and calls limiter.WaitN(ctx, len(chunk))"
      pattern: "WaitN"
---

<objective>
Wrap Plan 03's single-threaded loop with: (a) a `--parallel N` worker pool (errgroup), (b) a shared `*rate.Limiter` token bucket gating S3 PUT bytes, (c) decimal/binary suffix parsing for `--bandwidth-limit`, (d) structured slog progress events + TTY progress bar. (MIG-02; D-A9, D-A10, D-A11, D-A15.)

Purpose: Plan 03 ships the correctness path. Plan 04 makes it operationally usable on TB-scale shares: parallel workers fan out, the bandwidth ceiling avoids saturating the operator's S3 budget, and the progress bar gives confidence on long runs.

Output: `--parallel`, `--bandwidth-limit`, and progress reporting fully working.

**Type sources from Plan 14-03 revisions (review iteration 1):** the worker pool consumes `*offlineRuntime` (pointer, defined in cmd/dfsctl/commands/blockstore/migrate_runtime.go), `*migrate.Journal` (defined in pkg/blockstore/migrate/journal.go — NOT cmd/), and walks the share via `migrate.WalkShareFiles` (pkg/blockstore/migrate/walk.go). The S3 PUT call site is `svc.Coordinator().Put(ctx, hash, chunk)` — not the phantom `svc.PutBlock` from earlier drafts. Snippets in this plan that show `appendOnlyJournal` or `WalkFiles` are pre-revision shorthand; the executor uses the real types from Plan 14-03 SUMMARY.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-03-SUMMARY.md
@.planning/codebase/CONVENTIONS.md

<interfaces>
<!-- Existing patterns to mirror. -->

From pkg/blockstore/engine/syncer.go (existing errgroup worker pool):
```go
import "golang.org/x/sync/errgroup"
g, gctx := errgroup.WithContext(ctx)
g.SetLimit(parallelism)
for ... {
    g.Go(func() error { ... })
}
return g.Wait()
```

From pkg/config (existing bandwidth/byte-size parsing — grep for ParseBytes / parseSize / "1MB" in pkg/config/ before reinventing).

From golang.org/x/time/rate:
```go
limiter := rate.NewLimiter(rate.Limit(bytesPerSecond), burstBytes)
err := limiter.WaitN(ctx, len(chunkBytes))   // blocks until tokens available
```

From github.com/schollz/progressbar/v3 (or similar — grep go.mod for any existing progress-bar dep before adding a new one; if none, fall back to a simple home-rolled writer that updates `\r{bar} {pct}% {rate}/s ETA {eta}`).

term.IsTerminal pattern (existing — grep `term.IsTerminal` or `isatty` across the codebase; if not found, use `golang.org/x/term` which is likely already a transitive dep via cobra or jwt).
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: ParseBandwidthLimit + shared rate.Limiter wiring through PutBlock</name>
  <files>
    cmd/dfsctl/commands/blockstore/migrate_bandwidth.go,
    cmd/dfsctl/commands/blockstore/migrate_bandwidth_test.go,
    cmd/dfsctl/commands/blockstore/migrate_loop.go,
    go.mod,
    go.sum
  </files>
  <read_first>
    - .planning/phases/14-migration-tool-a5/14-CONTEXT.md (D-A9, D-A11 — uploads only, decimal+binary suffix, golang.org/x/time/rate)
    - cmd/dfsctl/commands/blockstore/migrate_loop.go (Plan 03 output — find the PutBlock call site; the limiter waits before it)
    - any existing byte-size parser in pkg/config or internal/bytesize (grep `func.*Parse.*Size\|func.*Parse.*Bytes` to discover; reuse if present)
  </read_first>
  <behavior>
    - Test 1: ParseBandwidthLimit("") → 0 bytes/sec, nil (= unlimited).
    - Test 2: ParseBandwidthLimit("0") → 0 bytes/sec, nil.
    - Test 3: ParseBandwidthLimit("100") → 100 bytes/sec, nil (no suffix = bytes).
    - Test 4: ParseBandwidthLimit("50KB") → 50_000 bytes/sec.
    - Test 5: ParseBandwidthLimit("50KiB") → 51_200 bytes/sec.
    - Test 6: ParseBandwidthLimit("100MB") → 100_000_000 bytes/sec.
    - Test 7: ParseBandwidthLimit("100MiB") → 104_857_600 bytes/sec.
    - Test 8: ParseBandwidthLimit("1GB") → 1_000_000_000 bytes/sec.
    - Test 9: ParseBandwidthLimit("1GiB") → 1_073_741_824 bytes/sec.
    - Test 10: ParseBandwidthLimit("garbage") → 0, non-nil error wrapping ErrInvalidBandwidth.
    - Test 11: ParseBandwidthLimit("-50MB") → 0, error (negative not allowed).
    - Test 12 (integration): with limit=1000 bytes/sec, uploading 4 KB across 2 chunks of 2KB each takes >= 4 seconds wall time.
    - Test 13: limit=0 (unlimited) → no waiting; the limiter helper returns nil quickly even for huge chunks.
  </behavior>
  <action>
    1. Add `golang.org/x/time/rate` to go.mod (run `go get golang.org/x/time/rate@latest` and `go mod tidy`).

    2. Create `cmd/dfsctl/commands/blockstore/migrate_bandwidth.go`:
       ```go
       package blockstore

       import (
           "context"
           "errors"
           "fmt"
           "math"
           "regexp"
           "strconv"
           "strings"

           "golang.org/x/time/rate"
       )

       var ErrInvalidBandwidth = errors.New("invalid --bandwidth-limit value")

       // ParseBandwidthLimit parses an operator-supplied --bandwidth-limit
       // string and returns bytes-per-second. Suffixes:
       //   "" / "0"           → 0  (unlimited; Limiter is nil in caller)
       //   "100"              → 100 B/s
       //   "50KB" / "50kB"    → 50_000 B/s   (decimal, 1000-base)
       //   "50KiB"            → 51_200 B/s   (binary, 1024-base)
       //   "100MB" / "1GB"    → 1e8 / 1e9 B/s
       //   "100MiB" / "1GiB"  → 1<<27 / 1<<30 B/s
       //
       // Negative values and unrecognized suffixes return ErrInvalidBandwidth.
       // D-A11.
       func ParseBandwidthLimit(s string) (int64, error) { ... }

       // newBandwidthLimiter returns a *rate.Limiter that allows bps bytes per
       // second with a 1-second burst window. Pass bps==0 for "unlimited" —
       // returns nil; callers must nil-check before calling WaitN.
       func newBandwidthLimiter(bps int64) *rate.Limiter {
           if bps <= 0 { return nil }
           burst := int(bps)
           if burst < 1<<20 { burst = 1<<20 } // floor at 1 MB to avoid pathologic burst-of-1 behavior
           return rate.NewLimiter(rate.Limit(bps), burst)
       }

       // bandwidthWait blocks until len(n) bytes can be uploaded under the
       // shared limiter. nil limiter = no waiting. Splits requests >burst into
       // sub-WaitN calls because rate.Limiter.WaitN refuses n > burst.
       func bandwidthWait(ctx context.Context, l *rate.Limiter, n int) error { ... }
       ```

       Implementation notes:
       - Suffix table is case-insensitive on the unit but preserves "B" semantics:
         decimal = K M G T P; binary = Ki Mi Gi Ti Pi (followed by `B`).
         `KB` and `KiB` differ; the parser strips `B` then inspects the residual.
       - Use a regex like `^(\d+(\.\d+)?)\s*(K|M|G|T|P)?(i)?B?$` (case-insensitive) — concrete pattern is in the test cases.
       - Floats round down to nearest int64.
       - Disallow values exceeding `int64` upper bound; return ErrInvalidBandwidth.

    3. In `cmd/dfsctl/commands/blockstore/migrate.go`, parse the flag in runMigrate:
       ```go
       bw, _ := cmd.Flags().GetString("bandwidth-limit")
       bps, err := ParseBandwidthLimit(bw)
       if err != nil { return err }
       opts.bandwidthBPS = bps
       ```

    4. In `cmd/dfsctl/commands/blockstore/migrate_loop.go`:
       - Build `limiter := newBandwidthLimiter(opts.bandwidthBPS)` once at runMigrateLoop entry.
       - Pass `limiter` into migrateOneFile via the perFile call site.
       - Inside the chunk loop, immediately before the `svc.PutBlock(ctx, hash, chunk)` call, insert `if err := bandwidthWait(ctx, limiter, len(chunk)); err != nil { return ... }`. Skip the call when limiter is nil.
       - Do NOT meter legacy reads — those bytes are local fs reads (or, in the offline-utility runtime, local-store reads from the cache dir). D-A9 explicit.

    5. Add `migrate_bandwidth_test.go` covering all 13 behaviors. For Test 12 (integration timing), use a small test limiter (1000 B/s) with a stub PutBlock that records timestamps; assert wall-clock delta >= 4 seconds (with some tolerance — `>= 3.5s` to avoid CI flake).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestParseBandwidthLimit|TestBandwidthWait' -count=1 &amp;&amp; go vet ./cmd/dfsctl/commands/blockstore/ &amp;&amp; grep -q 'golang.org/x/time/rate' go.mod &amp;&amp; grep -q 'WaitN' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'golang.org/x/time/rate' go.mod` >= 1.
    - `grep -c 'ParseBandwidthLimit' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` >= 1.
    - `grep -c 'rate.NewLimiter' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` >= 1.
    - `grep -c 'WaitN' cmd/dfsctl/commands/blockstore/migrate_bandwidth.go` >= 1.
    - `grep -c 'bandwidthWait' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - All 13 unit tests pass.
    - `go vet ./cmd/dfsctl/commands/blockstore/` clean.
  </acceptance_criteria>
  <done>
    Bandwidth flag accepts both decimal and binary suffixes; a single shared rate.Limiter gates every S3 PUT byte; legacy reads remain unmetered; unlimited is the default.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: errgroup worker pool over file walk + TTY progress bar + slog events</name>
  <files>
    cmd/dfsctl/commands/blockstore/migrate_workers.go,
    cmd/dfsctl/commands/blockstore/migrate_workers_test.go,
    cmd/dfsctl/commands/blockstore/migrate_progress.go,
    cmd/dfsctl/commands/blockstore/migrate_loop.go
  </files>
  <read_first>
    - pkg/blockstore/engine/syncer.go (existing errgroup pattern — see how SetLimit is used, how gctx propagates cancellation)
    - .planning/phases/14-migration-tool-a5/14-CONTEXT.md (D-A15 — slog + TTY bar)
    - cmd/dfsctl/commands/blockstore/migrate_loop.go (Plan 03 output — find the migrate.WalkShareFiles closure; that becomes the worker dispatch)
    - any existing TTY detection helper (grep `term.IsTerminal\|isatty\|IsTerminal` in cmd/dfsctl/ and internal/ before adding a new dep)
  </read_first>
  <behavior>
    - Test 1: With --parallel=4 and 8 files, observed concurrency reaches 4 (test stub records max in-flight workers).
    - Test 2: With --parallel=1, files are migrated strictly serially.
    - Test 3: First worker error (ctx cancellation or per-file fatal err) cancels the rest via errgroup ctx; remaining files don't start.
    - Test 4: Worker pool preserves journal-IsFileDone skip semantics (a partial-resume run with --parallel=4 still skips already-done files; no double-migration).
    - Test 5 — Progress events: per-file commit emits an `slog.Info("migrate.file.committed", ...)` with structured fields (handle, blocks_count, bytes_uploaded, bytes_deduped, files_done, files_total).
    - Test 6 — TTY progress bar: when stdout is a TTY (mocked via fake terminal), output contains a `\r`-overwriting line with current percent. When stdout is a pipe/file, no `\r` lines appear (machine-friendly).
  </behavior>
  <action>
    1. Create `cmd/dfsctl/commands/blockstore/migrate_workers.go`:
       ```go
       package blockstore

       import (
           "context"
           "sync"
           "sync/atomic"

           "golang.org/x/sync/errgroup"

           "github.com/marmos91/dittofs/internal/logger"
           "github.com/marmos91/dittofs/pkg/metadata"
       )

       // workerPool dispatches per-file migration over a fixed worker count.
       // Mirrors pkg/blockstore/engine/syncer.go's errgroup pattern.
       type workerPool struct {
           parallel int
           svc      offlineRuntime
           journal  *migrate.Journal  // pkg/blockstore/migrate.Journal — see Plan 14-03 Task 2
           opts     migrateOptions
           limiter  *rate.Limiter
           progress *progressReporter

           mu    sync.Mutex
           result migrateResult // accumulates under mu
       }

       func (wp *workerPool) Run(ctx context.Context, files []walkedFile  // type alias for (handle, *metadata.File) tuple) (migrateResult, error) {
           g, gctx := errgroup.WithContext(ctx)
           g.SetLimit(wp.parallel)

           for _, f := range files {
               f := f
               if wp.journal.IsFileDone(string(f.Handle)) {
                   wp.mu.Lock()
                   wp.result.FilesSkipped++
                   wp.mu.Unlock()
                   continue
               }
               g.Go(func() error {
                   r, err := migrateOneFile(gctx, wp.svc, wp.journal, wp.opts, wp.limiter, f.Handle, f.Attr)
                   if err != nil { return err }
                   wp.mu.Lock()
                   wp.result.FilesDone++
                   wp.result.BytesUploaded += r.BytesUploaded
                   wp.result.BytesDeduped += r.BytesDeduped
                   wp.mu.Unlock()
                   wp.progress.OnFileCommit(r)
                   return nil
               })
           }
           if err := g.Wait(); err != nil { return wp.result, err }
           return wp.result, nil
       }
       ```

       In runMigrateLoop, replace the linear walk-callback with a two-pass:
       - Pass 1: collect all (handle, attr) tuples into a slice (this is bounded by the share's file count; for TB-scale shares with 100M files this could be expensive — fall back to a streaming dispatcher if file count exceeds say 1M; for the initial implementation, slice is fine and the runbook's TB-scale transcript notes the consideration).
       - Pass 2: workerPool.Run(ctx, files).

    2. Create `cmd/dfsctl/commands/blockstore/migrate_progress.go`:
       ```go
       type progressReporter struct {
           ttyEnabled bool
           total      int
           done       atomic.Int32
           startedAt  time.Time
       }

       func newProgressReporter(total int) *progressReporter {
           tty := false
           if f, ok := os.Stdout.(*os.File); ok {
               tty = term.IsTerminal(int(f.Fd()))
           }
           return &progressReporter{ttyEnabled: tty, total: total, startedAt: time.Now()}
       }

       func (p *progressReporter) OnFileCommit(r perFileResult) {
           done := p.done.Add(1)
           // Always emit slog event (machine-parseable; D-A15)
           logger.Info("migrate.file.committed",
               "handle", r.Handle,
               "blocks_count", len(r.Blocks),
               "bytes_uploaded", r.BytesUploaded,
               "bytes_deduped", r.BytesDeduped,
               "files_done", done,
               "files_total", p.total,
           )
           // Conditionally render TTY bar
           if p.ttyEnabled {
               pct := float64(done) / float64(p.total) * 100
               eta := computeETA(p.startedAt, int(done), p.total)
               fmt.Fprintf(os.Stdout, "\rMigrating: %d/%d (%.1f%%) ETA %s", done, p.total, pct, eta)
           }
       }

       func (p *progressReporter) Close() {
           if p.ttyEnabled {
               fmt.Fprintln(os.Stdout) // newline to terminate the \r-overwrite line
           }
       }
       ```

       Use `golang.org/x/term.IsTerminal` (add to go.mod if not present — likely is via cobra). Refresh-throttling: in the simplest impl, write on every commit; if the migration is fast (lots of small files), throttle to 100ms via a `last atomic.Int64`. Mention this in the runbook.

    3. Wire into runMigrateLoop:
       ```go
       progress := newProgressReporter(len(files))
       defer progress.Close()
       wp := &workerPool{parallel: opts.parallel, svc: svc, journal: journal, opts: opts, limiter: limiter, progress: progress}
       result, err := wp.Run(ctx, files)
       ```

       The single-threaded path from Plan 03 is replaced by the worker pool. If `--parallel=1`, workerPool.Run still works (g.SetLimit(1) → strict serial).

    4. Add tests in `migrate_workers_test.go` for behaviors 1–4 above; tests for behaviors 5–6 in a `migrate_progress_test.go` (new file). For Test 6 (TTY detection), use a `*os.File` opened on `os.DevNull` to simulate non-TTY; for the TTY case, inject the `ttyEnabled` flag directly via a test constructor.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestWorkerPool|TestProgressReporter' -count=1 &amp;&amp; go vet ./cmd/dfsctl/commands/blockstore/ &amp;&amp; grep -q 'errgroup' cmd/dfsctl/commands/blockstore/migrate_workers.go &amp;&amp; grep -q 'term.IsTerminal' cmd/dfsctl/commands/blockstore/migrate_progress.go &amp;&amp; grep -q 'migrate.file.committed' cmd/dfsctl/commands/blockstore/migrate_progress.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'errgroup' cmd/dfsctl/commands/blockstore/migrate_workers.go` >= 2 (import + WithContext call).
    - `grep -c 'g.SetLimit' cmd/dfsctl/commands/blockstore/migrate_workers.go` >= 1.
    - `grep -c 'term.IsTerminal' cmd/dfsctl/commands/blockstore/migrate_progress.go` >= 1.
    - `grep -c 'migrate.file.committed' cmd/dfsctl/commands/blockstore/migrate_progress.go` >= 1.
    - `grep -c 'slog\|logger.Info' cmd/dfsctl/commands/blockstore/migrate_progress.go` >= 1.
    - All 4 worker tests + 2 progress tests pass.
    - `go vet ./cmd/dfsctl/commands/blockstore/` clean.
    - Test 1 specifically asserts `maxInflight == 4` when `parallel=4` is configured.
  </acceptance_criteria>
  <done>
    Worker pool of size --parallel; cancellation propagates via errgroup ctx; journal IsFileDone preserved; per-commit slog event emitted; TTY progress bar overlays on a TTY and silent on pipes.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Operator-supplied --bandwidth-limit string | Untrusted in the sense of malformed input; trusted otherwise (operator runs the tool). Parsing rejects negative + unrecognized values. |
| Worker pool → metadata store | Multiple goroutines call PutFile concurrently for *different* files. Concurrency is per-file, not per-block within a file — matches D-A1 atomic-unit-is-one-file. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-04-01 | Denial of service | Worker pool saturating S3 endpoint despite bandwidth limit | mitigate | Shared rate.Limiter is *bytes*-based, not requests-based. Burst is floored at 1 MB so single huge chunks (16 MB max FastCDC) split across multiple WaitN calls when the per-second limit is below 16 MB/s. |
| T-14-04-02 | Tampering | --parallel set to absurdly high value (e.g., 10000) | accept | Operator-supplied; cost is local goroutine scheduling pressure + S3 throughput throttling. Clamp at 64 in newWorkerPool with a warn-log; cap is documented in --help text. |
| T-14-04-03 | Information disclosure | TTY progress bar leaking sensitive data into shared shell scrollback | accept | Bar shows file counts + bytes only — no paths, no handles. The slog event has more detail but that's expected operator visibility for a maintenance run. |
</threat_model>

<verification>
- ParseBandwidthLimit covers 13 cases including unlimited, decimal, binary, edge cases.
- Bandwidth integration test confirms wall-clock latency under a tight limit.
- Worker pool concurrency observable.
- Progress events emit on every commit; TTY bar conditionally rendered.
- `go test ./cmd/dfsctl/commands/blockstore/...` and `go vet` clean.
</verification>

<success_criteria>
- `--parallel N` honored end-to-end (default 4).
- `--bandwidth-limit` accepts {KB,MB,GB,KiB,MiB,GiB} and unset = unlimited.
- Single shared rate.Limiter gates every S3 PUT byte across all workers.
- Legacy reads unmetered.
- slog events on every per-file commit.
- TTY progress bar overlays on stdout when terminal; silent on pipe.
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-04-SUMMARY.md` documenting the bandwidth parser, worker pool, and progress reporter contracts.
</output>
