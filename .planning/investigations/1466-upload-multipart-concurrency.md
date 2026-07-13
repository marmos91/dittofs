# #1466 — Upload residual gap: multipart vs. more concurrent PutObject

Investigation only. All code verified against `origin/develop` (head `0b2361b8`, PR #1567 merged).

## TL;DR

The adaptive controller settling at **~24 is correct, not a bug** — 24 is the real goodput
knee for standalone `PutObject`. Proof: pinned `c64 = 494 Mbit/s` is *lower* than `c24 = 561`.
More concurrent `PutObject` is a **dead lever**. The residual gap to rclone's 1253 (single-object)
/ 2703 (aggregate) is an **architecture** difference: DittoFS uploads each 16 MiB carved block as
**one `PutObject` = one HTTP PUT = one TCP flow**; rclone streams a single object as **N concurrent
multipart parts** over N flows. **Multipart is the dominant lever** — *contingent* on the 561 wall
being network/per-flow bound, not client-CPU bound. The `c64 < c24` regression is a
CPU/over-subscription fingerprint, so the benchmark's pprof must discriminate the two before we
commit to the multipart build.

---

## 1. Why the adaptive window settles ~24

Verified constants/paths on develop:

- `pkg/block/engine/upload_controller.go` — `goodputController`: `floor=16, ceiling=64,
  rampFactor=1.5, improveFrac=0.10, collapseFrac=0.5, backoffFactor=0.7, emaAlpha=0.5,
  stallLimit=3`. Ramps multiplicatively **only on window-limited samples**; on a plateau
  (`ema` not >10% over `best`) it holds and after 3 flat samples **settles back to `bestWindow`
  (the knee)** — "smallest window that delivered near-peak goodput".
- `pkg/block/engine/types.go:35-37,43` — `AdaptiveUploadFloor=16`, `Ceiling=64`,
  `uploadControlInterval=500ms`.
- Ramp sequence from floor: **16 → 24 → 36 → 54 → 64**.

The knee at 24 is inevitable given the measured curve `359/448/561/494` for conc `1/8/24/64`:

- Grow 16→24 is window-limited and improving (448→561, +25% > improveFrac) → recorded knee=24.
- Next grow 24→36 samples goodput that does **not** clear `561 × 1.10 = 617` (real curve peaks at
  561 then *degrades* to 494 by 64). So 36/54/64 are logged as plateau, never as a better
  `bestWindow`. After `stallLimit=3` the controller reverts `cur` to `bestWindow=24`.
- **Conclusion: the controller found the true knee. Lowering `improveFrac` to force it to 64 would
  make throughput worse (494 < 561).** No controller change is warranted for the plateau itself.

Second possibility — **app-limited / rollup-pipeline starvation** (`windowLimited=false` →
controller *holds*, never ramps past current):

- Feed path: writes → `addPendingHash` → `carveQ` → `carveFlush` (periodic ticker + explicit
  Flush) → `claimCarveBatch` (16 MiB target) → concurrent goroutines → `PutBlock`
  (`pkg/block/engine/carver.go:109-179`). `carveFlush` keeps claiming and dispatching within one
  pass as long as `carveQ` has batches; concurrency gated by `uploadLimiter.Acquire`; a `wg.Wait()`
  barrier ends the pass.
- For the benchmark case (large sequential file, fast local NFS/SMB write ≫ slow WAN drain) the
  `carveQ` backlog builds, so samples *should* be window-limited and the ramp *should* engage. This
  must be **confirmed from the timeline**: if `uploads_inflight` never reaches `upload_window`,
  the window is app-limited and the knee is a pipeline artifact, not a network knee.
- One structural bubble to watch: the per-pass `wg.Wait()` barrier. Sustained single-file writes
  stay inside one `carveFlush` pass so it's usually a non-issue, but a bursty writer that empties
  `carveQ` between ticks will drain-and-barrier repeatedly, capping effective inflight below the
  window. The timeline's `upload_queue_depth` vs `uploads_inflight` shows this directly.

## 2. Multipart vs. more concurrent PutObject — assessment & expected win

Confirmed on develop:

- `pkg/block/remote/s3/store.go` `PutBlock` (~L467): single `s.client.PutObject(...)`, `Body: r`
  where `r = bytes.NewReader(blockBytes)` (`carver.go:454`). Seekable reader → SDK sets
  `Content-Length`, standard SigV4 (no `aws-chunked` streaming-signing overhead). **No
  `feature/s3/manager` multipart.**
- HTTP transport (`store.go:155-176`): `MaxConnsPerHost = maxS3ConnsPerHost = 256`,
  `ForceAttemptHTTP2=false`, `Write/ReadBufferSize=256KiB`. **The pool is not the cap** (256 ≫ 24).

So the only structural difference from rclone is single-PUT-per-object vs. concurrent multipart
parts. Expected win depends on what the 561 wall actually is:

| If the wall is… | pprof signature | Multipart outcome | Est. single-stream |
|---|---|---|---|
| **Network / per-flow** (per-TCP-flow rate cap, SCW fairness) | CPU idle; goroutines blocked in `net` write; inflight == window | Splits one object across N flows → approaches aggregate 2703 | 561 → **~1000-1253** (1.8-2.2×) |
| **Client CPU** (TLS + double-BLAKE3 #1266 + GC at 24 workers) | CPU saturated; `c64<c24` from scheduler over-subscription | Amortizes per-request CPU only via **bigger parts + fewer objects**; same CPU wall otherwise | 561 → **~700-800** unless paired with CPU cuts |

The `c64 < c24` regression already points at a **client-side ceiling near 24 workers** (CPU or
Go-scheduler over-subscription), which is why "just raise concurrency" fails. Multipart's honest
value is: **(a)** where a single large file is only a few blocks, it provides *intra-object*
concurrency to fill the pipe (rclone's exact advantage), and **(b)** with a larger carve size it
cuts request count, amortizing SigV4 + TLS-session + per-request setup and the double-BLAKE3.

**Decision: pursue multipart + larger carve, gated on the pprof result. If CPU-bound, pair with
CPU reduction (single-hash, `GOMAXPROCS`, fewer/larger requests) — multipart alone won't reach
1253.**

## 3. Implementation plan (post-benchmark, gated)

Files:

1. `pkg/block/remote/s3/store.go` — `PutBlock`: for `len >= multipartThreshold` (≥ 5 MiB S3 min;
   use e.g. 16 MiB) upload via `feature/s3/manager.NewUploader(client, PartSize, Concurrency)`
   instead of `PutObject`. Keep `PutObject` for small blocks. Add `feature/s3/manager` to `go.mod`
   (already an `aws-sdk-go-v2` consumer — no new external dep family). `bytes.NewReader` body is
   already seekable, so the manager can size parts without buffering.
2. `pkg/block/engine/types.go:26` — make `DefaultBlockCarveBytes` configurable and raise the WAN
   default 16 → 64 MiB (fewer objects; feeds #1414 packing). A 64 MiB block = 4 × 16 MiB parts.
3. **Concurrency budget (the real design point).** Total in-flight connections =
   `blockWindow × partConcurrency`. This must not exceed the ~24 knee / 256-conn pool / CPU.
   - Lazy first cut: keep the block-level `dynamicSemaphore`/`goodputController` **as-is**, add
     multipart with a *fixed* `Concurrency` (e.g. 4) and `PartSize` 16 MiB. For a single large file
     (few blocks in flight) multipart fills the pipe; for many blocks the block window already
     parallelizes, so cap `blockWindow` lower (drop `AdaptiveUploadCeiling` to ~8-16) so the
     product stays near the measured knee. Measure, then decide if unification is needed.
   - Full version (only if the lazy cut leaves throughput on the table): repoint the
     `goodputController` to tune the **total connection budget** (parts), dividing it across active
     blocks, instead of block count. Reuses the entire controller unchanged — only the quantity it
     drives changes. `// ponytail: tune block-window first; unify budget only if pprof shows the
     product, not the block count, is the lever.`
4. Metrics: `uploads_inflight` should count in-flight **parts**, not blocks, once multipart lands,
   so the timeline stays honest (`pkg/block/engine/syncer.go` `SetUpload*` + the inflight gauge).

## 4. Before/After benchmark + pprof

Harness exists: `bench/parity` (`cmd/bench/parity.go`, `parity.go`, `dittofs.go`, `rclone.go`,
`timeline.go`, `scorecard.go`). `upload-large` quadrant, per-cell datapath gauge timeline, rclone
as the reference tool. WAN profile targets SCW S3 `fr-par`.

Env (SCW fr-par bucket + creds via `bench/parity/s3env.go` / `DITTOFS_*` S3 config). Run the `dfs`
server with pprof enabled (`net/http/pprof`, #671) so CPU/goroutine/block profiles can be scraped
per cell.

**BEFORE (baseline, current develop):**

```bash
# from repo root, dfs built at origin/develop
go run ./cmd/bench parity \
  --profile wan --quadrant upload-large \
  --conc 1,8,24,64 \
  --tools dittofs,rclone \
  --sample-ms 500 \
  --out-dir bench/results --label 1466-before
```

Per cell, in parallel with the run, scrape pprof at the two decisive concurrencies (c24 = knee,
c64 = regression) to discriminate the wall in the table above:

```bash
go tool pprof -proto -seconds 30 http://<dfs>:6060/debug/pprof/profile > 1466-before-c24.cpu.pb.gz   # during c24 cell
go tool pprof -proto -seconds 30 http://<dfs>:6060/debug/pprof/profile > 1466-before-c64.cpu.pb.gz   # during c64 cell
curl -s http://<dfs>:6060/debug/pprof/goroutine?debug=1 > 1466-before-c64.goroutine.txt
curl -s http://<dfs>:6060/debug/pprof/block > 1466-before-c64.block.pb.gz
```

**AFTER (multipart + 64 MiB carve branch):** identical command, `--label 1466-after`, same
pprof capture.

**Metric & readout** (`bench/analysis/analyze.sh` + `report.sh`):

- Primary: **throughput Mbit/s** per (tool, conc) cell; dittofs vs rclone, before vs after.
- **Effective inflight**: from the per-cell timeline, plot `uploads_inflight` vs `upload_window`
  vs `upload_goodput` vs `upload_queue_depth`. Confirms whether the window was saturated
  (window-limited) or starved (app-limited) — settles the §1 ambiguity.
- pprof readout: c24 vs c64 CPU total + top frames (TLS, `blake3`, GC, `net`). CPU-saturated at
  c24 ⇒ CPU wall (multipart limited; add CPU cuts). CPU idle + goroutines blocked on socket write
  ⇒ network wall (multipart should land the win).

**Targets:**

- Baseline reference: dittofs peak 561 (c24); rclone 1253 single-object, 2703 aggregate.
- **Success bar:** close ≥ 50% of the 561→1253 gap ⇒ **≥ 900 Mbit/s** single-stream, *and* the
  timeline shows effective inflight × per-connection rate that accounts for the number (no phantom
  gains).
- **Stretch:** parity with rclone single-object (1253) and aggregate (2703).
- **Guardrail:** small-file quadrants and CPU headroom must not regress (larger carve must not
  starve small-file latency; multipart must not blow the 256-conn pool or CPU).

## Risks

- **Multipart won't help if the wall is CPU** — the `c64<c24` fingerprint says this is plausible.
  Mitigation: pprof gate before building; be ready to pair with single-hash (#1266) / `GOMAXPROCS`.
- **Concurrency-budget double-counting**: `blockWindow × partConcurrency` can silently exceed the
  knee/pool and *regress* (exactly the c64 failure mode). Mitigation: lower `AdaptiveUploadCeiling`
  when multipart is on; count parts (not blocks) in the window and metrics.
- **Larger carve blocks** raise per-block RAM and change #1414 packing/GC assumptions and small-file
  behavior — keep it configurable, default only the WAN path up.
- **`bestWindow` settle logic** is correct today; don't "fix" the controller to chase 64. The lever
  is architecture (multipart), not the controller.
