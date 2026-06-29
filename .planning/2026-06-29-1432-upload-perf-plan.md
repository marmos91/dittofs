# #1432 — S3 upload throughput plan

**Date:** 2026-06-29
**Issue:** #1432 — "S3 upload ~30× slower than rclone (~25 Mbit/s vs ~450)"
**Status:** root cause confirmed by benchmark; PRs not yet started.

## TL;DR

The reporter's 25 Mbit/s is **dittofs uploading on a single TCP stream** (`inflight=1`) in
steady state. A single stream to Cubbit is ~25 Mbit/s for *everyone* — reproduced exactly from a
Scaleway VM. It is **not** Cubbit throttling, **not** the reporter's connection, **not** multipart.
The fix is to **sustain upload concurrency** so `inflight ≈ window` during steady streaming, not
only when a deep backlog happens to exist.

## How it was measured

Disposable Scaleway POP2-8C-32G VM in fr-par → Cubbit (`s3.cubbit.eu`, 17.8 ms RTT) and SCW S3
(`s3.fr-par.scw.cloud`, 0.9 ms RTT). Apples-to-apples with dittofs's **own** S3 client via a
throwaway `parallelput` tool (clean bounded-errgroup PUTs of 4 MiB CAS-sized objects — no engine).
Full dfs server reproduction: NFS mount → `dd` 1.5 GiB → `dfsctl system drain-uploads`, sampling
`/metrics` (`datapath_upload_window` / `_uploads_inflight` / `_upload_queue_depth`) at 0.5 s.

CAS chunk sizing: FastCDC min 1 / **avg 4** / max 16 MiB. So ~375 objects per 1.5 GiB.

## Data

Cubbit, same VM/bucket:

| Path | Throughput |
|---|---|
| rclone **multipart** 16M / conc64 | **402 Mbit/s** |
| dittofs **parallel-PUT** 4MiB / conc64 (our client) | **337 Mbit/s** |
| parallel-PUT 4MiB / conc16 | 197 |
| parallel-PUT 4MiB / conc8 | ~150 |
| parallel-PUT 4MiB / conc4 | 101 |
| **parallel-PUT 4MiB / conc1 (single stream)** | **25** ← reporter's number, exactly |
| dittofs **engine** (dd→drain, deep backlog) | 280 (bursts to inflight=64) |

Near-linear: throughput ≈ streams × ~25 Mbit. conc32 dipped to 115 = tail-variance (only 96 ops),
**not** 503s.

Engine `/metrics` timeline (Cubbit): window ramps 16→64 and holds; **`inflight` sits at 1–2 for the
whole steady state**, bursting to 64 only when `dd` had piled a deep backlog. Explicit
`drain-uploads` (one big `mirrorOnce` pass) fills the window to 61 — proving dispatch *can*
parallelize; the periodic/continuous path fails to.

## Root cause

1. **Primary — steady-state `inflight=1`.** The periodic uploader drains in tiny per-pass batches as
   chunks trickle in (snapshot → dispatch → `g.Wait()` → return), so during steady streaming it runs
   ~one PUT at a time = single TCP stream = 25 Mbit. It only reaches `inflight=64` when a deep backlog
   already exists (reporter's queue stayed shallow 10–18 → never bursts). `mirrorOnce`/`mirrorChunk`
   (`pkg/block/engine/syncer.go`) hold no lock — the limiter is fine; the problem is the *batching
   structure* of the continuous path. Confirmed: explicit drain hits inflight 61.
2. **Not Cubbit.** Zero 503 / SlowDown. Conn pool already 256.
3. **Not multipart.** parallel-PUT 337 ≈ multipart 402 at conc64. Multipart's ~20% edge = bigger
   parts (fewer round-trips), not the protocol.
4. **Bonus bug.** `drain-uploads` is hard-capped at the controlplane `write_timeout` (30 s):
   `Drain uploads failed error=context canceled duration=30.000s`. Large explicit flushes are killed
   mid-drain. (Also invalidated the SCW engine measurement: egress 728 MiB < 1.5 GiB.)
5. **Observability bug.** `datapath_upload_queue_depth` is a stale per-pass gauge (set once at
   snapshot, never decrements — froze at 178). Misleading; made diagnosis ambiguous.

## Plan (ordered by impact)

### PR 1 — Sustain upload concurrency in steady state  *(the 13×; this is the issue)*
Replace the per-pass snapshot + `g.Wait()` drain with a **continuous worker pool**: N persistent
workers pull from the pending channel, each grabbing the next chunk the instant its PUT returns. No
barrier between "passes." Goal: `inflight ≈ window` during steady streaming, not only on deep
backlogs. Keep the adaptive window (#1407) — it already computes 64; the bug is `inflight` never
reaching it. Branch off develop.
- **Success:** streaming workload (not backlog-burst) sustains inflight 32–64 and ~300+ Mbit.

### PR 2 — Fix `drain-uploads` 30 s timeout  *(quick, independent)*
Decouple the drain handler from the HTTP `write_timeout` (background / `context.WithoutCancel`, or
stream progress). Ship alongside PR 1.

### PR 3 — Bigger PUT objects = **#1414 object packing**  *(secondary, ~20%)*
Fewer/larger objects close most of the 337→402 gap. Do **after** PR 1; park on #1414. **Not**
multipart adoption.

### Minor — fix stale `queue_depth` gauge
Make it real-time so future diagnosis isn't misled.

## Explicitly NOT doing
- Multipart rewrite (parallel-PUT ≈ multipart at concurrency).
- Connection-pool tuning (256 already; 0× 503).
- Bigger FastCDC blocks *instead of* fixing dispatch — masks the single-stream bug, hurts dedup/RAM.

## Validation
Re-spin the disposable SCW-VM → Cubbit harness after PR 1. Pass = steady-state (streaming, shallow
queue) inflight 32–64 + ~300 Mbit, i.e. no longer single-stream-bound.

## Cross-refs
#1407 (adaptive window — ramps, but inflight≪window is the real gap) · #1411 (shallow-queue /
rollup-starve) · #1414 (object packing = Lever 2) · #1415 (parallel rollup, shipped).
