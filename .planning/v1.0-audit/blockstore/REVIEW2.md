> **⚠️ STALE-TREE CORRECTION (2026-06-01).** This pass ran against a stale local-develop checkout (`e16f0b01`) MISSING merged PRs #918/#919/#920/#921. Re-verified against real `origin/develop` (`db4328b4`):
> - **H1 reap-no-op for Pending rows — CONFIRMED** (#832 genuinely not fixed on the production write path; coordinator.go + engine Pending-row logic unchanged by the merges).
> - **H2 LRU evicts unsynced chunk → silent data loss — CONFIRMED** (syncer.go/local-fs unchanged).
> - **H3 `bs.cache` lock-free swap — REFUTED:** #918 close-gate (`closeMu` RWMutex + `enter()`) serializes the swap (DestroyCache RLock, Close Lock).
> - **H4 no in-flight gate / use-after-close — REFUTED:** #918 added exactly this gate. The §1/§3 claims that "the #918 close-gate does not exist on develop" are WRONG — they reflect the stale tree.
> - **H5 ReadPayloadAt full-log replay — CONFIRMED** (perf only; readpayload.go unchanged).
> **Net valid HIGH on real develop: 3 (H1, H2, H5).**

# Area 1 — Blockstore + CAS + Engine + GC — ROUND-2 Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — NEEDS-FIX (4 HIGH integrity holes confirmed on current develop; PR-B required before v1.0 tag).
**Date**: 2026-06-01.
**Scope**: Round-2 missed-findings + integration-lens pass over `pkg/blockstore/` (engine, local/fs, remote/s3+memory, compression/encryption decorators, keyprovider, GC) **plus the cross-component seams** into `pkg/controlplane/runtime/shares/` (coordinator, service), `pkg/metadata/store/{memory,badger,postgres}`, and the live REST evict/remove paths. Round-1 audited each area in isolation; this round targets boundaries, error/failure paths, concurrency-under-load, and re-verifies that round-1 HIGHs are fixed on develop.
**Cross-check refs**: Round-1 REVIEW `.planning/v1.0-audit/blockstore/REVIEW.md` — every round-1 finding (C-1/C-2/I-1/S-1, B1/B2/B3, C-3, I-5/I-6/R-1, conformance gaps) treated as KNOWN and **not** re-reported. Round-1 HIGHs verified shipped/fixed on develop via #668/#834/#835/#840/#778/#918 and the C-2 dedup-rollback fix. Canonical-impl cross-checks (Samba, Linux fs/nfs) not protocol-relevant to this CAS/GC pass.

**Sub-audits consolidated** (5 parallel):
- `engine-concurrency` — close-gate / cache-field race / use-after-close
- `cas-refcount-integrity` — engine↔coordinator↔metadata-store reap seam
- `syncer-failure-paths` — eviction-vs-upload data-loss seam
- `s3-remote-errors` — remote backends + GC↔decorator boundary
- `perf-leaks` — read-path O(log-size) replay + re-verify of B1/B2/B3/C-3

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| engine-concurrency | 2 | 0 | 1 | 0 |
| cas-refcount-integrity | 1 | 2 | 0 | 0 |
| syncer-failure-paths | 1 | 0 | 1 | 0 |
| s3-remote-errors | 0 | 1 | 3 | 0 |
| perf-leaks | 1 | 0 | 2 | 0 |
| **TOTAL** | **5** | **3** | **7** | **0** |

**Verdict: NEEDS-FIX.** Round-2 surfaced **5 HIGH** that round-1's isolated, write-biased audit could not see — three of them are genuine **data-loss / data-integrity** holes living at component boundaries:

1. The `#868` over-retention fix (`DecrementRefCountAndReap`) is a **no-op on the production write path** — engine FileBlock rows are `Pending` forever, `GetByHash` is `Remote`-gated, so reap never fires; **#832 is not actually fixed** and every delete/truncate of a synced file permanently leaks both a metadata row and a remote CAS chunk.
2. The LRU can **evict an unsynced chunk before its first mirror pass**; `mirrorOnce` then *deletes* the hash from `pendingHashes` per a false comment, producing **silent unrecoverable data loss** on any capped remote-backed share.
3. The engine `*Store` has **no in-flight gate** and the `bs.cache` interface field is swapped lock-free, so the live REST evict/remove paths race in-flight NFS/SMB reads → **Go data race / use-after-close** (the prompt's assumed "#918 close-gate" does **not** exist on develop).

Plus a read-side perf analog of round-1's B1 (full-log replay per read) and two cross-backend refcount-accounting MED divergences.

**Architecture invariants hold.** No layer violations, no handler business-logic leakage, file handles still opaque, per-share block-store ownership intact. The HIGHs are correctness/concurrency holes *within* the established structure, not structural drift — consistent with a PATCH-grade *structure* but a NEEDS-FIX *integrity* posture. All four round-1 HIGHs (C-1, C-2, I-1/#668, S-1) and the B1/B2/B3/C-3 perf items are **verified fixed** on develop (see §6).

---

## 2. HIGH findings (ranked by blast radius)

### Group A — Data loss / data integrity (3)

**H1. `DecrementRefCountAndReap` is a no-op for `Pending` FileBlock rows — engine reap never fires; FileBlock rows + remote CAS chunks leak forever (#832 not actually fixed)** — `pkg/controlplane/runtime/shares/coordinator.go:146-165` (reap; resolves via `GetByHash:148`); engine calls at `pkg/blockstore/engine/readwrite.go:261` (Delete) + `:164` (Truncate); rows created `Pending` at `pkg/blockstore/engine/engine.go:177-182`; memory `GetByHash` `IsRemote` gate `pkg/metadata/store/memory/objects.go:421-424`; postgres `state=2` gate `pkg/metadata/store/postgres/objects.go:266-279`.

- **What**: The rollup `ObjectIDPersister` creates per-chunk FileBlock rows with `State=BlockStatePending` and no `BlockStoreKey` (`engine.go:177-182`), and **no production path ever transitions them to `Remote`** — durability is tracked in a separate `synced_hashes` table via syncer `MarkSynced` (`syncer.go:421`), which never touches the `file_blocks` row. The syncer's own comments confirm this ("the row state remains Pending/Syncing for the life of the payload", `syncer.go:510-518, 586-588`). The only non-test `State=BlockStateRemote` setter (`local/fs/recovery.go:120-121`) is gated on `BlockStoreKey != ""`, a legacy/dual-read condition the engine rows never satisfy. On Delete/Truncate the *only* row-removal mechanism is the coordinator's `DecrementRefCountAndReap(hash)` (`DeleteFile` does not cascade to `file_blocks`; `EvictMemory` only drops in-RAM state). But that path resolves hash→id via `GetByHash`, which returns `nil` for non-`IsRemote`/`state!=2` rows → coordinator returns `(0,nil)` at `coordinator.go:148-156` **without reaping**. The row survives forever, its hash stays in `EnumerateFileBlocks` (the GC mark-phase live set, `gc.go`), so the remote chunk is never swept.
- **Why (blast radius)**: This is the exact over-retention bug #832/#868 targeted; the #868 fix only operates on `Remote`-state rows that do not occur in normal operation. Concrete impact on **all 3 backends**: (1) every delete/truncate of a synced file permanently leaks FileBlock rows (unbounded metadata growth under churn); (2) the remote CAS chunk is never reclaimed by GC (permanent remote storage growth); (3) the `DeleteSynced` cascade (`readwrite.go:277`, gated on `newCount==0`) never fires so `synced_hashes` markers also leak.
- **Verifier rationale**: Confirmed at every cited location; the load-bearing claim (Pending rows never become Remote) is corroborated by the code's own comments and an exhaustive grep for the Remote setter. CI-invisible because the INV-02 fuzzer seeds `State=BlockStateRemote` (`inv02_fuzz.go:272`) and `engine_delete_test` uses a fake coordinator — neither drives a real `Pending` row through the real `Remote`-gated `GetByHash`.
- **Fix**: Make the reap path resolve+delete the engine's `Pending` rows. Options: (a) reap by payloadID-keyed row-ID prefix (`id` scheme is `payloadID/offset`) in addition to / instead of the hash-keyed reap — `engine.Delete` already knows the payloadID; (b) widen the *decrement-path* hash resolution to find `Pending` rows (distinct from the dedup `GetByHash`, which must stay `Remote`-gated); (c) add a `DeleteFile→file_blocks` cascade keyed on payloadID. Add an end-to-end conformance test that writes via the **real** engine (Pending rows), deletes, and asserts `EnumerateFileBlocks` no longer returns the hash **and** GC sweeps the remote chunk — driving the real coordinator, not pre-seeded `Remote`.

**H2. LRU evicts an unsynced chunk before upload; `mirrorOnce` silently drops it (data loss)** — `pkg/blockstore/engine/syncer.go:390` (false "eviction only runs on already-synced chunks" comment + `delete(m.pendingHashes,hash)`); `pkg/blockstore/local/fs/chunkstore.go:114` (LRU-add every fresh chunk); `pkg/blockstore/local/fs/fs.go:584-605` (`lruEvictOne`, no `IsSynced` check); `pkg/blockstore/local/fs/eviction.go:30-82` (`ensureSpace`).

- **What**: `StoreChunk` calls `lruTouch` on **every** fresh chunk with no sync-state qualification (`chunkstore.go:114`). `ensureSpace` loops `lruEvictOne` whenever `diskUsed+needed > maxDisk` and **deliberately does not** consult the metadata or synced-hash store; `lruEvictOne` pops the LRU back and `os.Remove`s the file with no `IsSynced` filter, short-circuited only by `RetentionPin`/`!evictionEnabled`. Eviction is enabled exactly when the remote is HEALTHY (`engine.go:359-360`) — i.e. normal operation — and there is no pin protecting unsynced chunks. Mirroring is deferred to the `periodicUploader` (default 2 s ticker, `syncer.go:747-751`); within that window a later write's `ensureSpace` can evict an unsynced chunk under a `max_size` cap. `mirrorOnce` then gets `ErrChunkNotFound` from `m.local.Get` (`syncer.go:388-389`) and, per the **false** comment at `:390-392`, executes `delete(m.pendingHashes,hash)` (`:396`) and continues.
- **Why (blast radius)**: Silent, unrecoverable data loss on any remote-backed share with a `max_size`/`maxDisk` cap. The recovery claim in the syncer comments (`:374-375`, "startup reconcile re-seeds the hash") is **also false** for the evicted case: `seedPendingFromDisk` re-seeds via `ListUnsynced` (`syncer.go:122`), which walks **on-disk** CAS chunks (`bc.Walk`, `blockstore_methods.go:228-273`); an evicted chunk's file is unlinked, so it cannot be rediscovered. The manifest FileBlock row (`State=Pending`) then references a hash present in neither local nor remote storage. Round-1 audited eviction and upload separately, so the seam was invisible.
- **Verifier rationale**: Confirmed exactly as described at every cited line; the `syncer.go:390` comment misdescribes the eviction guarantee and the re-seed recovery path provably cannot recover an unlinked file.
- **Fix**: Gate eviction on `IsSynced` when a remote is configured (or pin unsynced hashes so `ensureSpace` skips them); never drop a live unsynced hash from `pendingHashes` on `ErrChunkNotFound` — escalate it as an integrity error instead of silently continuing, and correct the false comment.

**H3. `bs.cache` interface field swapped by `Start()`/`DestroyCache()` with no synchronization while hot-path ops read it — data race / torn interface read** — `pkg/blockstore/engine/engine.go:378` (Start writes `bs.cache=realCache`), `:469` (DestroyCache writes `bs.cache=nullCache{}`); read at `readwrite.go:30,87,179,224,228`, `stats.go:69`, `dedup.go:107,379`, `engine.go:280`.

- **What**: `bs.cache` is a plain `cacheInterface` field (`engine.go:98`) with **no** mutex/atomic. `Start()` swaps `nullCache{}→*Cache` and `DestroyCache()` swaps back, both lock-free, while ~10 hot-path methods (`ReadAt`/`WriteAt`/`Truncate`/`Delete`/`GetStats` + the `OnChunkComplete` callback) read `bs.cache.*` unsynchronized. An interface value is two words (type ptr + data ptr); a concurrent read during the write can observe a torn value → panic or wrong-vtable dispatch (UB under the Go memory model). Reachable in production end-to-end: `POST /api/v1/blockstore/evict` (`internal/controlplane/api/handlers/blockstore.go:72`) → `runtime.EvictBlockStore` → `shares/service.go:1388 DestroyCache()` on a *running* share with no quiescing of in-flight NFS/SMB reads. The only lock taken on the resolve path is `s.mu.RLock` (registry pointer lookup, released before the data op via `GetBlockStoreForHandle:1135`), which provides zero serialization against the swap.
- **Why (blast radius)**: Round-1 found and PR-B fixed the structurally identical race for the *local* store's `onChunkComplete` field (C-1 → now `atomic.Pointer` + `persisterMu` in `local/fs/fs.go:309,263`), but the engine-level `bs.cache` field was never given the same treatment. A torn interface read under `-race` is a crash/UB, not a logical glitch. No existing test drives `DestroyCache` concurrently with reads, so CI `-race` never exercises it.
- **Verifier rationale**: Verified at every cited line; production reachability confirmed through the REST handler. One sub-claim (the intra-`Start` race, `OnChunkComplete` firing before line 378) is theoretical and does not undermine the primary, clearly-reachable DestroyCache-vs-reads race.
- **Fix**: Store the cache behind `atomic.Pointer[cacheInterface]` (mirror the `onChunkComplete` fix) or guard every access with an RWMutex read. Add a `-race` test that drives `ReadAt` in a goroutine loop while calling `DestroyCache`.

### Group B — Liveness / use-after-close (1)

**H4. `*engine.Store` has no in-flight gate: `Close()`/`DestroyCache()` race live data ops resolved via `GetBlockStoreForHandle` (use-after-close)** — `pkg/blockstore/engine/engine.go:406` (Close), `:464` (DestroyCache); `pkg/controlplane/runtime/shares/service.go:1129-1146` (resolve releases `s.mu` before handler use), `:784-813` (RemoveShare calls `bs.Close` with no quiesce); `pkg/controlplane/runtime/runtime.go:380` (RemoveShare only drains snapshot goroutines).

- **What**: `GetBlockStoreForHandle` takes `s.mu.RLock`, returns the bare `*engine.Store`, then releases the lock (`defer:1136`). Handlers then perform `ReadAt`/`WriteAt`/`Flush` holding **no** lock (e.g. `handler.go:1558`, `flush.go`, `ioctl_copychunk.go`, `common/resolve.go`). Concurrently `RemoveShare` deletes the share under `s.mu`, releases it, then calls `bs.Close()` (`service.go:810`) — running `cache.Close`/`syncer.Close`/`local.Close`/`remote.Close` — with no mechanism to wait for or reject the in-flight op. The `Store` struct (`engine.go:69-102`) has **no** `closed` flag, **no** `closeMu` RWMutex, **no** `enter()` gate, **no** op WaitGroup. The prompt's assumed "#918 close-gate (`closeMu` RWMutex + `enter()`)" **does not exist on develop** (grep confirms; the only `closed` fields are on `Syncer:79` and `cache:94`). `runtime.RemoveShare:380` drains only snapshot orchestration, not in-flight protocol reads. So an in-flight `readAtInternal → bs.local.ReadPayloadAt` can race `bs.local.Close()` (true use-after-close), and the `DestroyCache` field swap tears concurrent cache reads (the H3 race, reachable mid-op).
- **Why (blast radius)**: Cross-component seam (handler ↔ shares service ↔ engine) invisible to round-1's isolated engine audit. Removal/evict during active traffic is corruption/crash-prone rather than a clean `ErrClosed`. The "soft" assumption that the runtime quiesces before `RemoveShare` is enforced **nowhere** in `service.go`/`runtime.go`. The syncer *does* gate via `checkReady`/`canProcess` + `m.closed`; the engine `Store` wrapping it does not gate its own local/cache access.
- **Verifier rationale**: Every cited line/mechanism accurate. HIGH retained: the unsynchronized cache-field swap is a definite race regardless of timing; the `local.Close()` use-after-close requires concurrent operator/REST-triggered `RemoveShare`/evict during live traffic, slightly lowering exploitability but not the crash/corruption-vs-clean-`ErrClosed` outcome or the guaranteed `-race` violation.
- **Fix**: Add a `closeMu` RWMutex + `closed` flag (or atomic) to `*engine.Store`; data ops take `RLock` + early-return `ErrClosed` when closed; `Close()`/`DestroyCache()` take the write lock. OR have `RemoveShare`/`EvictBlocks` drain in-flight ops before `Close`. At minimum, make `bs.local`/`bs.cache` access reject-or-wait after close. (This and H3 share a fix surface — see PR-B1.)

### Group C — Read-path performance (1)

**H5. `ReadPayloadAt` replays the ENTIRE append log header-to-EOF on every read (O(log-size) per read, ignores `logIndex`)** — `pkg/blockstore/local/fs/readpayload.go:104-174` (`replayLogIntoDest`), called unconditionally from `ReadPayloadAt:66`; reached via `engine/read_internal.go:58`.

- **What**: `replayLogIntoDest` opens a fresh read-only fd (`os.Open:117`), seeks past the 64-byte header (`:139`), and loops `for{ readRecord(rf) }` to EOF (`:143-172`) on **every** `ReadPayloadAt` call — the primary FS read entry. It reads **and CRC-validates** every framed record in the un-rolled-up log; the intersection filter (`:153-155`) is applied only *after* the full frame is read, so there is no seek/index skip. The per-payload `logIndex` already maps file-offset ranges to exact record `logPos`/`payloadLen` via `EntriesForInterval`/`lookupInterval` — the rollup uses it (`rollup.go:243`) — but `readpayload.go` never references it (grep-confirmed). `maxLogBytes` defaults to 1 GiB (`fs.go:395`), so a hot file with a large unrolled log makes every small read scan up to ~1 GiB of records.
- **Why (blast radius)**: Read-side analog of round-1's B1 (per-op full walk). Round-1 explicitly never profiled a pure-read workload (round-1 REVIEW §5 H3 "No pure-read workload"), so it was invisible. Pathological in exactly the round-1 stress case: parallel small writes (macOS NFSv3) accumulate many log records before the stabilization-windowed rollup drains them; a read-after-write on the same payload pays O(un-rolled-up-log-bytes) CPU+IO per read, independent of read size. Throughput collapse under mixed read/write; no correctness impact.
- **Verifier rationale**: Verified directly; bug exists exactly as described. Calibration note: the "~1 GiB per read" figure is the backpressure ceiling, approached only as the unrolled log nears the limit; steady-state cost tracks accumulated-but-undrained records. The cited `readwrite.go:21` path label is slightly off (real chain is `read_internal.go:58 → ReadPayloadAt → replayLogIntoDest`); the load-bearing `readpayload.go:104-174` citation is exact.
- **Fix**: Use the existing `logIndex` on the read path — `idx.lookupInterval(offset,len)` (or `EntriesForInterval` into a pooled scratch) to get only intersecting records, then `pread` each frame by `logPos` directly (as `rollupFile` does via `readRecordAt`) instead of the sequential `readRecord` scan. Snapshot `idx` under the per-file mutex briefly like rollup does. Bounds per-read cost to O(overlapping-records).

---

## 3. Triage downgrades / RESOLVED

**None.** Every HIGH advanced by the five sub-audits survived adversarial verification (5/5 `real=true`, all `adjustedSeverity=HIGH`). No HIGH was refuted or downgraded this round.

The premise stated in the audit brief — that a "#918 close-gate (`closeMu` RWMutex + `enter()`)" already guards the 12 data ops — was itself **refuted by code**: no such gate exists on `*engine.Store` (grep finds no `closeMu`/`enter()`). That refutation is *load-bearing in the other direction* — it is the basis of H4, not a downgrade.

---

## 4. MED findings

### cas-refcount-integrity
- **M1. Cross-backend refcount divergence: postgres `AddRef` bumps ALL rows for a hash; memory/badger bump exactly ONE** — `postgres/objects.go:249-251` (`UPDATE … WHERE hash=$1 AND state=2`, no LIMIT) vs `memory/objects.go:355-365` (single `hashIndex` id) vs `badger/objects.go:313-348` (single `fb-hash:{hash}` id); coordinator decrement targets one `fb.ID` (`coordinator.go:157`). When multiple finalized rows share a hash, postgres increments N rows but the coordinator decrements 1, while memory/badger increment 1 / decrement 1 — backends drift in opposite directions and postgres rows can never all reach 0. Currently latent (engine rows stay `Pending`, see H1) but a live correctness/parity violation the moment any row reaches `Remote`. The INV-02 fuzzer uses unique per-(worker,op,blk) hashes so no two rows ever share one. **Fix**: pick one canonical multi-row-per-hash semantics across all 3 backends + conformance — either postgres `AddRef` targets a single deterministic row (`ORDER BY id LIMIT 1`) or the coordinator decrements all rows for the hash; add a storetest scenario that Puts two finalized rows with the *same* hash and asserts identical accounting.
- **M2. INV-02 conformance fuzzer cannot detect the engine's real refcount lifecycle** — `storetest/inv02_fuzz.go:266-279` (unique hash + `State=BlockStateRemote` seed), `:334-348` (delete via direct `DecrementRefCount` on a known id). It never (1) exercises the coordinator's hash→`GetByHash`→reap two-call resolution the engine actually uses, (2) creates two files sharing a content hash (the case that makes `RefCount>1` meaningful), or (3) uses `Pending`-state rows. This is exactly why H1 and M1 pass CI green; a green INV-02 run gives false assurance over the refcount class it claims to protect. **Fix**: add scenarios that seed `Pending` rows and assert the engine reap path removes them; create two files referencing the same hash and assert delete-one preserves the other's data while delete-both reaps; drive deletes through `DecrementRefCountAndReap`-by-hash (the engine path).

### s3-remote-errors
- **M3. GC `Walk` over a compression-over-encryption remote downloads + AEAD-decrypts EVERY object** — `compression/decorator.go:185-202` (`plaintextSizeFor → inner.GetRange(0,probeLen)`), `encryption/decorator.go:91-106` (`GetRange` ignores the probe length and `d.Get`s the full body + full AEAD `Open` + `provider.Unwrap`), `engine/gc.go:490-535` (`Walk` over the fully-decorated store; `meta.Size` used only for the `BytesFreed` counter at `:532`). On an encrypted+compressed bucket, every operator-triggered GC turns a metadata-only LIST sweep into a download-and-decrypt of the entire bucket (orders of magnitude more egress/CPU/GET charges, scaling with object count). Worse: `plaintextSizeFor` propagates any probe error, so a single transient 5xx/torn-read on one object's full Get aborts the whole fail-closed sweep, reclaiming nothing — enabling encryption makes GC *strictly more fragile*. Invisible to round-1's isolated GC/decorator audits. **Fix**: decouple GC from plaintext size — track wire bytes (raw s3-layer `meta.Size`) for `BytesFreed`, or add a `Walk` variant/context flag that skips the `plaintextSizeFor` probe; have `EncryptedRemote`/`Decorator.Walk` pass inner `meta.Size` through unmodified and document Walk as reporting wire size.

---

## 5. LOW findings

### engine-concurrency
- **L1. `Syncer.mirrorOnce` reads `m.remoteStore` unlocked vs `SetRemoteStore` write under `m.mu`** — `syncer.go:418` (unlocked `m.remoteStore.Put`) vs `:985` (write under `m.mu.Lock`). Dormant: `SetRemoteStore`'s own godoc (`:969`) says it is not wired into any production path and grep confirms zero non-test callers. Flagged so a future local-only→remote transition does not silently introduce a race. **Fix**: delete `SetRemoteStore` (dead in production) or snapshot `m.remoteStore` under `m.mu.RLock` at the top of `mirrorOnce`/`fetchBlock` (as `syncedHashStore` already is, `:363-365`).

### syncer-failure-paths
- **L2. `engine.Start` ordering drops the `firstOfflineRead` reset** — `engine.go:359`. `SetHealthCallback` after `syncer.Start` forwards the raw `fn`; `SetTransitionCallback` (`sync_health.go:112`) drops the `firstOfflineRead` reset wrapper. Observability-only. **Fix**: re-wrap `fn`, or set the callback before `syncer.Start`.

### s3-remote-errors
- **L3. GC sweep can race `RemoveShare` closing the same decorated store** — `runtime/blockgc.go:43-88` (`:78` runs the long sweep with no lock held), `shares/service.go:760-790`, `encryption/decorator.go:201-208` (`provider.Close` zeroes the master key in place, `keyprovider/local.go:238-244`). A concurrent `RemoveShare` dropping refCount to zero `Close`s the s3 store (`closed=true`) / zeroes the master key, so an in-flight GC `Walk` hits `ErrStoreClosed` or `newGCM(nil) "key must be 32 bytes"`. Errors not UB — GC fails confusingly and reclaims nothing; the decorated `entry.Store` is **not** the `nonClosingRemote` wrapper. **Fix**: have GC hold a ref on the remote for the sweep duration, or gate GC vs `RemoveShare`-driven Close.
- **L4. `verifier.readAllVerified` discards a fully-verified buffer on a benign trailing network error** — `s3/verifier.go:128-155` (`:141-153` oneByte peek). When `ContentLength` is known, after `io.ReadFull(r,data)` for exactly N bytes the 1-byte peek that forces EOF/`checkHash` can return a non-EOF, non-mismatch error (e.g. a connection reset arriving after the last byte, hash already matched) → `"read s3 object body trailer"` error discards the verified bytes (`:152`). Forces an unnecessary S3 re-fetch; no data loss (caller retries). **Fix**: if the verifier already observed EOF/ran `checkHash` successfully (`v.done && v.hashOK`), return the verified data even if the trailing Read surfaced a transport error; only treat genuine extra-bytes or a real mismatch as fatal.
- **L5. `isNotFoundError` string-fallback matches `'NotFound'`/`'404'` substrings broadly** — `s3/store.go:570-586` (`:583-585`). After the typed `*types.NoSuchKey` check, the `strings.Contains` fallback can misclassify an unrelated S3-compatible-backend error (wrapped DNS/endpoint error, body tokens) as `ErrChunkNotFound`. On read, `fetchBlock` (`engine/fetch.go:150-165`) treats that as live-data-loss with an alarming Error log, masking a likely-retryable cause; on `Has()` it returns false for an object that may exist. Narrow (production uses typed errors; fallback exists for MinIO/Localstack). **Fix**: require an HTTP 404 status via smithy `http.ResponseError` / typed `*types.NotFound`; drop the bare `'404'`/`'NotFound'` substring match or gate it behind a status-code check.

### perf-leaks
- **L6. `ReadPayloadAt` allocates a fresh `covered` bool-slice on every read** — `readpayload.go:56` (`make([]bool, len(dest))`). Per-read heap churn on the hottest path; a bool-per-byte is 8× a bitmask. Compounds H5. **Fix**: bitmask (len/8) or a size-classed pool (channel-pool like `reconstruct_pool.go`), cleared on get.
- **L7. `fillFromCASManifest` re-lists, re-allocates and re-sorts the full manifest on every CAS-touching read** — `readpayload.go:187-216` (`ListFileBlocks:191`, `make([]rowAbs:205`, `ParseChunkOffset` per row `:210`, `sort.Slice:216`); `readLocalByHash` (`read_internal.go:159`) similarly does an O(N) `findRowCoveringOffset` per chunk → O(K·N). For ~1000-row files every cold/post-eviction read does a full alloc+sort + N string-parses. **Fix**: cache the parsed+sorted `(absOffset,row)` projection per payload (invalidate on rollup commit/manifest change), or binary-search the already-ID-sorted rows; memoize `ParseChunkOffset` on the row.

---

## 6. Verified-correct

**Round-1 HIGHs re-verified FIXED on current develop:**
- **C-1** (`SetOnChunkComplete` field race) — fixed: `onChunkComplete` is now `atomic.Pointer[chunkCompleteCallback]` (`local/fs/fs.go:309`) + `objectIDPersister` behind `persisterMu` RWMutex (`fs.go:263`).
- **C-2** (`applyFileLevelDedupHit` rollback swallow) — fixed: `rollbackIncrements` failures surface as errors and abort the retry (`dedup.go:295-322`).
- **I-1 / #668** (rollup wedge) — fixed (shipped via #668 program).
- **S-1** (no upload verify) — fixed: `mirrorOnce` recomputes `blake3(data)==hash` before `remoteStore.Put` and refuses on mismatch with `ErrCASContentMismatch` (`syncer.go:410-417`, #840).
- **R-1 / I-6** (S3 HTTP `Timeout:0` + spin-wait) — fixed: `Timeout=2m` + `ResponseHeaderTimeout=60s` (`s3/store.go:36-40,132,137`, #778); `SyncNow` ticker no longer per-iter allocates and exits cleanly on ctx.Done.
- **I-5** (`inlineFetchOrWait` local-Put failure swallowed) — fixed: local-Put error now propagated to caller + all in-flight waiters via `completionErr` (`fetch.go:355-360`).
- **B1** (per-Flush full CAS walk) — fixed: in-memory `pendingHashes` set populated O(1) via `addPendingHash` and drained in `mirrorOnce` after `MarkSynced`; full disk walk only at startup + ~10 min drift reconcile (`syncer.go:91-130,362-429,859-913`).
- **B2** (`reconstructStream` per-pass full buffer) — fixed: `getReconstructBuf`/`putReconstructBuf` channel pools (`reconstruct_pool.go`), baseOff-anchored (`rollup.go:376-384,642-672`, #834/#835).
- **B3** (`logIndex.EntriesForInterval` per-call slice + O(n) scan) — fixed: per-index scratch reuse (`logindex.go:111,119-122`), `trimBelowFenceLocked` bounds entries to the unconsumed set (`:282-307`), binary-search coverage.
- **C-3** (`gcRootLocks` unbounded map) — fixed: refcounted with delete-on-zero (`gc.go:99-126`).
- **F-01** (engine.go god-file) — split into `engine.go`/`readwrite.go`/`flush.go`/`stats.go`/`health.go`/`read_internal.go` as planned.

**Concurrency / correctness checked OK this round:**
- Syncer locking sound: `addPendingHash`/`mirrorOnce` pending-set is race-correct under `m.pendingMu` (snapshot-at-start + CAS-hash idempotency make the lost-update window harmless — no data loss); the `uploading` atomic gate is consistently CAS-acquire/defer-release across all four holders (Flush/SyncNow/periodicUploader/DrainAllUploads); `completeInFlight` is called exactly once per path (mutually-exclusive `completed` flag vs deferred call; distinct `result.done`/`req.Done` channels); lock ordering `m.mu`→`m.pendingMu` is never inverted; `Syncer.Close` is idempotent and correctly ordered.
- `Cache` internal locking sound: `closed atomic.Bool` guards all entry points; `Get` re-checks under WLock before `MoveToFront`; `trackerMu` separated from `mu`; `Close` idempotent via CAS + `wg.Wait`. Prefetch worker pool leak-clean (bounded `reqCh`, ctx-cancel, drain-on-Close). `SyncQueue` worker lifecycle leak-safe (`stopCh`→`workerCtx` cancel + 5 min timeout, `wg.Wait`); `enqueueDownload` waiter has a `stopCh` arm.
- `engine.Delete`/`Truncate` decrement at most once per **distinct** hash matching the once-per-distinct-hash increment in `applyFileLevelDedupHit`/`CopyPayload` (the Copilot-caught per-`BlockRef` over-decrement from #868 is fixed; kept-hash guard correct). `DecrementRefCountAndReap` is row-level atomic and TOCTOU-free within each backend (memory single write-lock; badger single `db.Update`; postgres decrement+conditional-DELETE in one tx with `ref_count=0` predicate). Concurrent AddRef-vs-Decrement cascade serialization tested.
- Remote backends in good shape: s3 error mapping (`isNotFoundError→ErrChunkNotFound`, `%w` elsewhere, idempotent Delete, `checkClosed` gate); `ReadBlockVerified` fail-closed twice (header pre-check + streaming BLAKE3, early-close→`ErrCASContentMismatch`, bounded 16 KiB drain); KMIP 16 MiB inbound bound; keyprovider AES-256-GCM (32-byte key, fresh 96-bit nonce, `ErrWrongMasterKey` routing); encryption decorator binds plaintext hash as AAD + nonce-length validated; compression decompression-bomb guard (`MaxFramedPlaintextSize` + `LimitReader` exact-length post-check); memory.Store mirrors s3 semantics. s3 `GetRange` offset/overflow not exploitable in production (only decorator probes use offset=0; engine reads use full-Get `ReadBlockVerified`). GC `sweepPhase` fail-closed on zero `LastModified`/`Has` error/`Walk` error; within-grace-window objects preserved. `mirrorOnce` transient-error path leaves the hash in `pendingHashes` for retry (only the H2 "evicted before upload" case drops — see H2).

---

## 7. Recommended PR-B shape

All PRs branch off develop tip (not chained) to keep merge cost flat. Each HIGH ships with the missing test that would have caught it.

**Wave B-1 — engine close-gate (fixes H3 + H4, shared surface)**
Add `closeMu sync.RWMutex` + `closed` flag (or `atomic.Pointer` for the cache field) to `*engine.Store`; data ops take `RLock` + early-return `ErrClosed`; `Close()`/`DestroyCache()` take the write lock and swap the cache atomically. Add a `-race` test driving `ReadAt`/`WriteAt` loops concurrently with `DestroyCache` and `Close` (this builds the gate the audit brief erroneously assumed already existed). **Risk**: medium; data-plane hot path.

**Wave B-2 — refcount reap on Pending rows (fixes H1; conformance for M2)**
Reap by payloadID-keyed row prefix (or widen the decrement-path resolution to `Pending` rows, distinct from the dedup `GetByHash`). Add the end-to-end conformance test (real engine → Pending rows → delete → assert `EnumerateFileBlocks` clean + GC sweeps the remote chunk) and the M2 scenarios (Pending-state reap, shared-hash two-file delete, decrement-by-hash). **Risk**: medium-high; touches reap + GC live-set semantics across 3 backends. Highest data-correctness leverage.

**Wave B-3 — unsynced-eviction guard (fixes H2)**
Gate `lruEvictOne`/`ensureSpace` on `IsSynced` (or pin unsynced hashes) when a remote is configured; never silently `delete(pendingHashes,…)` on `ErrChunkNotFound` — escalate as integrity error; fix the false `syncer.go:390` comment. Add a test: capped share, write→evict-before-tick→assert no silent drop. **Risk**: medium; eviction interacts with the round-1 B1 pending-set.

**Wave B-4 — read-path index (fixes H5; folds in L6/L7)**
Use `logIndex.lookupInterval` on `ReadPayloadAt`, `pread`-by-`logPos` instead of sequential replay; pool/bitmask the `covered` slice; cache the parsed+sorted manifest projection per payload. Re-profile a pure-read workload (closes round-1 §5 H3 + H4 harness gaps). **Risk**: low-medium; perf-only, no correctness change, but it is the hottest read path.

**Defer as GitHub issues (MED/LOW):**
- M1 (cross-backend `AddRef` row-set divergence) — `v1.0-blockstore` correctness; latent until any row reaches `Remote` (gated by H1 fix landing).
- M3 (GC full-bucket decrypt over encrypted remote) — `v1.0-perf-blockstore`; operator-visible cost/fragility.
- L1 (`SetRemoteStore` dormant race — prefer **delete**), L2 (health-callback reset drop), L3 (GC↔RemoveShare close race), L4 (verifier trailing-error discard), L5 (`isNotFoundError` substring fallback). Cluster L1/L3 under engine-lifecycle hardening; L4/L5 under s3 error-classification.

---

## 8. Coverage

**Audited (round-2 lens):**
- `pkg/blockstore/engine/` — full concurrency sweep of `Store` lifecycle (Start/Close/DestroyCache), cache-field synchronization, syncer locking (`m.mu`/`m.pendingMu`/`uploading`/inFlight), `SyncQueue`/prefetch worker pools, read/write/rollup/fetch resource paths (post B1/B2/B3).
- engine↔coordinator↔metadata-store **refcount/reap seam** end-to-end across memory/badger/postgres (`GetByHash` gating, `DecrementRefCountAndReap`, `EnumerateFileBlocks` live-set, GC mark/sweep).
- `pkg/blockstore/local/fs/` read path (`ReadPayloadAt`/`replayLogIntoDest`/`fillFromCASManifest`), eviction (`ensureSpace`/`lruEvictOne`) vs upload seam.
- `pkg/blockstore/remote/` s3 + memory backends, compression/encryption decorators, keyprovider, KMIP, verifier — and their GC↔decorator + lifecycle (`RemoveShare`/Close) boundaries.
- Cross-component reachability into `pkg/controlplane/runtime/shares/service.go`, `runtime.go`, and the live REST evict/remove handlers (`internal/controlplane/api/handlers/blockstore.go`).
- Re-verification of all round-1 HIGHs (C-1/C-2/I-1/S-1) + perf (B1/B2/B3/C-3/R-1/I-5/I-6) on current develop.

**Not audited / out of scope:**
- Migration path (`pkg/blockstore/migrate/`) — round-1 I-4 stands; not re-examined.
- Snapshot/HoldProvider lifecycle internals (area #8) — only the GC live-set + GC↔RemoveShare boundary touched.
- Protocol-handler internals (areas #4/#5) — only the `GetBlockStoreForHandle` → handler → data-op seam traced for the H4 use-after-close class.
- Badger value-log GC and postgres migration mechanics (area #6) — only the `GetByHash`/`AddRef`/reap row semantics relevant to H1/M1/M2 examined.
- Macro throughput re-baseline (Scaleway) — H5/L6/L7 fixes should be validated against a pure-read macro workload at the post-fix acceptance gate (round-1 §5 H3 harness gap still open).
- `block`/`mutex` pprof + goroutine snapshots — still gated on the harness work tracked in round-1 (#671/#680).
