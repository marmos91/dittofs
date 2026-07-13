# Blocks-Only Storage — Follow-up Roadmap (post-#1493)

**Status date:** 2026-07-05
**Supersedes the "Out of scope" section of** `.planning/2026-06-30-1493-blocks-only-roadmap.md`.

## Where we are

Epic **#1493 is complete on `develop` and CLOSED**: local logblob packing + remote-write
flip to `blocks/<id>` + an automatic one-shot **cas→blocks startup migration**
(`migrateLegacyCAS`, `pkg/block/engine/engine.go` → `Store.Start`; blocking, resumable,
idempotent, purges `cas/`).

**All of it is UNRELEASED** — landed after the last release **v0.23.2** (2026-06-26). So the
*next* release will run the migration on real deployments' `cas/` data for the first time.
That fact drives the priority order below.

---

## Workstreams & status

### A. Correctness & upgrade safety — release gate
The next release runs the migration in production for the first time; these de-risk it.

| Issue | Item | State |
|---|---|---|
| #1554 | sqlite-backed startup/reconcile **deadlock test** (closes the memory-only-suite blind spot) | **DONE — PR #1556 merged to develop** |
| #1554 (rescoped) | **slow sqlite+S3 cold-start** — readiness >120s after deadlock fixes; latent startup/migration serialization. NOT covered by #1555 (steady-state throughput). | **PR #1564 open (gates passed, awaiting CI+Copilot).** ROOT CAUSE (corrected): the one-shot cas→blocks migration's standalone-DETECTION scan runs on EVERY boot, synchronously, before `go rt.Serve` binds `:8080`. Detection = `EnumerateSynced` all synced hashes + one serial `GetLocator` per hash → O(N) sequential statements on the sqlite `MaxOpenConns(1)` pool. LB sees connection-refused/503 until bind. (NOT the S3 purge — that's a one-time first-boot cost; the every-boot GetLocator scan is the persistent cold-start.) `/health/ready` provably always 200 once registry!=nil, so the non-200 is the LB during the pre-bind window, not the handler. FIX (per #1556 `TestSQLiteReconcile_LocatorRoundTripsAreLinear` prescription): fold the locator columns into the `EnumerateSynced` callback (same `synced_hashes` row) → detection is ONE scan, N+1→1, and the nested-query deadlock class is structurally removed at all 5 sites (migration/reconcile/reclaim/compaction/gc-sweep). Migration stays synchronous/blocking; no startup reorder, no sentinel. |
| #1557 | **real-release cas→blocks migration round-trip test** (v0.23.2 `cas/` → develop, byte-identical reads, `cas/` purge, crash-resume) | **DONE — PR #1561 merged, #1557 closed.** Two-level coverage. **(a) CI unit test** (`legacy_cas_roundtrip_test.go`): real v0.23.2 `cas/` key format (`FormatCASKey` shard fanout), LIST pagination, byte-identical `ReadLegacyChunkVerified`, `cas/` purge, `blocks/`-survivor data-loss guard, idempotency; models an unencrypted deployment (`SealChunk`=identity). **(b) Live VM run** (one-shot, not in CI): actual v0.23.2 binary wrote NFS files → 5 real `cas/<hash>` in MinIO → develop binary on the *same* badger DB + bucket ran `migrateLegacyCAS` → repacked 5 chunks into 1 block, purged `cas/`, all files read back byte-identical over NFS. **Residual:** crash-resume mid-migration still rests on the reconcile-class tests; the full orchestration is CI-covered only at the S3-surface layer (the end-to-end repack is the one-shot VM validation, not repeatable in CI). |

### B. Space efficiency

| Issue | Item | State |
|---|---|---|
| #1487 | GC **compaction** of partially-dead blocks (one knob `gc.compaction_live_ratio`, default 0 = off) | **DONE — PR #1559 merged to develop, #1487 closed.** Review gate caught a major (client-read EIO on relocated chunk); fixed by relocating the one-shot re-resolve into the shared `dispatchRemoteFetch` chokepoint (covers client-demand + background paths), regression-tested. |

### C. Performance / rclone parity (tracker #1466)

| Issue | Item | State |
|---|---|---|
| #1555 | **Per-stage CPU + memory profiling** (append / FastCDC / streamer / syncer / GC+compaction), reusing `cmd/bench` | **DONE — PR #1562 merged, #1555 closed.** Baseline (M1 Max, in-mem): CPU floor = FastCDC boundary scan (0.63 ms/MiB, ~3× hash); allocations = streamer (~346 MB per 64 MiB, `bytes.Buffer` doubling + remote body copy). |
| #1432 | Upload throughput (single-stream WAN) | open |

### D. Deferred enhancements — gated, not scheduled

| Issue | Item | Gate |
|---|---|---|
| #1488 | chunk-range refetch on read miss | **GATE VERDICT: don't build.** #1555 profiling shows the write path is CPU-bound on the FastCDC scan, not read-miss granularity; read miss is not a bottleneck. Revisit only if a read-path profile says otherwise. |
| #1491 | decoupled log-blob/block sizing | **GATE VERDICT: don't build the knob.** #1555 shows streamer allocation is transient `bytes.Buffer` doubling — a one-line carve-buffer pre-size captures the win without a sizing knob (minimize-surface). Follow-up: the pre-size itself (cheap, no new config). |
| #1489 | remote manifest files (namespace DR) | needs design; moves with DR epic #1417 |
| #1490 | background corruption scrubber | post-1.0 |
| #1498 | Merkle tree over block hashes (anti-entropy) | post-1.0 |
| #1492 | group-commit fsync | **DONE** — already shipped on develop, closed |

### E. Cleanup

| Item | State |
|---|---|
| `fileblock.go`→`filechunk.go`, `BlockRef` comment stragglers, dead `NewFileChunk` delete | **DONE — PR #1560 merged to develop** |
| `BlockState`→`ChunkSyncState` rename (170 sites, cosmetic) | **recommended WON'T-DO** — decision pending |

---

## Sequencing (waves)

1. **DONE:** review gate → merged **#1556 → #1559 → #1560** (order avoided read-path conflicts).
   Space-efficiency (#1487) and both cleanup/deadlock items all landed on develop.
2. **DONE:** **#1557** (PR #1561) + **#1555** (PR #1562). Migration round-trip + profiling both landed.
3. **Profiling gate resolved:** **#1488** and **#1491** → **CLOSED not-planned** (see D). The one cheap
   follow-up the gate identified — streamer carve-buffer pre-size — **SHIPPED (PR #1563):**
   thread the exact claimed batch size out of `claimCarveBatch`, pre-grow the block buffer to
   body + per-chunk headroom. Bench: 345.6 → 294.5 MB/op (−14.8%), +18.5% throughput. 256 B/chunk
   headroom covers codec + decorated-sealer inflation; `Grow` size int64 range-checked (no overflow panic).
4. **Now / remaining:** **#1554** (slow sqlite+S3 cold-start investigation) — the last open
   release-adjacent item, not yet started (heavier: needs real S3 / VM validation).
5. **Parked (post-1.0 / needs design):** #1489, #1490, #1498.

**Release-gate status:** #1556 + #1557 both landed → the cas→blocks upgrade path is de-risked
(purge-safety in CI + a live v0.23.2→develop round-trip on the VM). Cold-start latency (#1554)
is a quality issue, not a data-safety blocker.

**Release gate:** before cutting the next release, land **#1556** + **#1557** (upgrade safety).
Compaction (#1559) and profiling are enhancements, not release blockers.

---

## Guardrails (hard-won this session — apply to all block-store work)

- **Verify against `origin/develop`, not the working tree.** The checked-out branch is often a
  stale side-branch (e.g. `chore/graphify-docs`, seen ~5300 lines behind). Use
  `git show origin/develop:<f>` / `git grep <p> origin/develop` / graphify (served from develop).
- **sqlite `MaxOpenConns(1)`:** never issue a metadata query (e.g. `GetLocator`) inside an
  `EnumerateSynced`/`WalkBlockRecords` callback — deadlocks. **Collect-then-query.** Invisible to
  the e2e suite (memory-metadata only) — needs sqlite-backed tests.
- **GC/compaction locking:** the single-sweeper lock is **per-`GCStateRoot`, not per-remote**, and
  the auto-GC scheduler bypasses the `gcReg` single-active gate. Compaction/reclaim that mutates a
  shared remote MUST ride the existing `remoteGCLock(configID)` closure — no new lock scope.
- **Minimize surface:** perf-deferred issues are gated on profiling; no speculative knobs.
- **Migration crash windows** map to existing reconcile classes (orphan object = class 3, leaked
  record = class 2) — keep that ordering (PutBlock → atomic commit → delete).
