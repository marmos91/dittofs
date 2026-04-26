# Phase 11: CAS write path + GC rewrite (A2) — Context

**Gathered:** 2026-04-25
**Status:** Ready for planning
**Milestone:** v0.15.0 Block Store + Core-Flow Refactor
**GH issue:** [#422](https://github.com/marmos91/dittofs/issues/422)
**Requirements:** BSCAS-01, BSCAS-03, BSCAS-06, GC-01, GC-02, GC-03, GC-04, STATE-01, STATE-02, STATE-03, LSL-07, LSL-08, TD-09, INV-01, INV-03, INV-04, INV-05, INV-06

<domain>
## Phase Boundary

Rewrite the sync/upload path to content-addressable storage (`cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}`), collapse the block lifecycle to three persisted states (Pending → Syncing → Remote), and replace path-prefix GC with a fail-closed mark-sweep algorithm. Make `TestBlockStoreImmutableOverwrites` pass — old hashes remain at their CAS keys after overwrite; new bytes go to new CAS keys; GC deletes only hashes absent from the live set.

**Core work (from ROADMAP.md):**
1. **BSCAS-01**: New `ParseCASKey` companion to existing `ContentHash.CASKey()`; engine uses CAS keys for all new uploads.
2. **BSCAS-03**: BLAKE3 (via `lukechampine.com/blake3`, locked Phase 08 D-08 amended) replaces SHA-256 for block identity on the sync path.
3. **BSCAS-06**: Every S3 PUT under `cas/...` carries `x-amz-meta-content-hash: blake3:{hex}` — externally verifiable.
4. **STATE-01..03**: Three-state lifecycle persisted on `FileBlock.State` indexed by ContentHash; no parallel state in memory buffers or fd pools.
5. **INV-03**: No orphan uploads — `State=Remote` only after PUT-success AND metadata-txn success.
6. **INV-04**: Fail-closed GC — any mark-phase error aborts sweep.
7. **INV-05**: Log length bound preserved (Phase 10 LSL-04 wiring confirmed; no regression).
8. **INV-06**: Every chunk downloaded from S3 is BLAKE3-verified before bytes reach the caller.
9. **GC-01..04**: Mark-sweep over the union of `FileAttr.Blocks[*].Hash`; sweep `cas/XX/YY/*` prefixes; fail-closed; no `BackupHoldProvider` coupling.
10. **LSL-07**: `LocalStore` interface narrowed from 22 → 17 methods (drops `MarkBlockRemote`, `MarkBlockSyncing`, `MarkBlockLocal`, `GetDirtyBlocks`, `SetSkipFsync`).
11. **LSL-08**: `LocalStore` no longer calls `FileBlockStore` on the write hot path; eviction driven from on-disk state via in-process LRU keyed by ContentHash.
12. **TD-09**: `flushBlock` no longer holds `mb.mu` during disk I/O — stage-and-release pattern.
13. **Dual-read shim**: legacy `{payloadID}/block-{idx}` reads continue to work alongside new `cas/...` reads through the Phase 11 → Phase 14 window; removed in Phase 15 (A6) per STATE.md decision.
14. **Canonical correctness E2E**: `TestBlockStoreImmutableOverwrites` ships green.

**Out of scope for Phase 11:**
- Engine API signature change (`engine.BlockStore.ReadAt(ctx, blocks []BlockRef, dest, offset)`) — that's API-01 in Phase 12 (A3). Phase 11 keeps the current `(ctx, payloadID, dest, offset)` signature; dual-read resolution is internal to the engine.
- `FileAttr.Blocks []BlockRef` schema reintroduction — META-01 in Phase 12 (A3).
- File-level dedup short-circuit (BSCAS-04/05) — Phase 13 (A4).
- Migration tooling (`dfsctl blockstore migrate`) — Phase 14 (A5).
- Legacy code removal (`FormatStoreKey`/`ParseStoreKey`, dual-read shim, legacy reader path) — Phase 15 (A6).
- Adapter-layer changes — landed in Phase 09; Phase 11 consumes `internal/adapter/common/` for error mapping but does not modify it.
- COMMIT/FLUSH-driven sync triggers — periodic-only retained; commit-driven deferred to a future RPO-focused phase.
- Per-share atomic backup integration (`BackupHoldProvider`) — v0.16.0 milestone, explicitly excluded by GC-04.
- Compression/encryption of block payloads — separate BlockStore Security milestone.
- Prometheus metrics surface for GC/syncer — observability phase.
- A generic, reusable crash-injection harness — Phase 11 ships hand-rolled crash-unit tests; reusable harness is a separate test-infra effort.

</domain>

<decisions>
## Implementation Decisions

### GC mark-sweep design (GC-01..04, INV-04)

- **D-01:** **Live set lives on disk under `<localStore>/gc-state/<runID>/` during mark phase.** Memory-bounded regardless of metadata size (typical enterprise NAS may have 10M+ unique hashes; in-memory exact set could exceed 600 MB, in-memory Bloom would silently retain garbage forever for false-positive hits). Backing impl is planner's discretion (recommend BadgerDB temp store since `github.com/dgraph-io/badger` is already a dependency, with flat sorted file as fallback). Crash detection: each run writes an `incomplete.flag` marker at start; next GC run detects stale dirs, deletes them, and starts fresh. Mark is idempotent so resume logic is not built — simpler test surface.
- **D-02:** **Mark-phase enumeration uses a new `MetadataStore.EnumerateFileBlocks(ctx, fn func(ContentHash) error) error` cursor.** Every backend (memory, Badger, Postgres) implements it natively — Badger via prefix iterator, Postgres via server-side cursor with batched fetch, memory via direct map iteration. Streams without loading the full file list into application memory. Added to `pkg/metadata/storetest/` conformance suite (CLAUDE.md invariant 7) — every metadata backend MUST pass the cursor scenarios (empty store, single file, large fanout, error mid-iteration → cursor returns error and aborts).
- **D-03:** **Cross-share coordination keys on remote-store identity.** GC mark phase iterates ALL shares whose `*engine.BlockStore` points at the same remote (`bucket+endpoint+prefix` triple), unioning their `FileAttr.Blocks[*].Hash`. Sweep then deletes only hashes absent from the global union. Honors STATE.md decision: "global per metadata store (RefCount spans shares when remote config shared)". Cross-share dedup safe by construction — an object is reachable from any share that references it.
- **D-04:** **Sweep parallelism: bounded worker pool over the 256 top prefixes.** Workers (default `gc.sweep_concurrency=16`, configurable up to 32) each walk all `YY` sub-prefixes for their assigned `XX` byte. Caps S3 connection pressure and goroutine count. Full 65,536-prefix parallelism rejected (rate-limit storm risk on real buckets).
- **D-05:** **Snapshot mark + grace TTL on object age.** Mark phase records snapshot time `T`. Sweep deletes only objects whose S3 `LastModified < T − grace`. Default `gc.grace_period = 1h` (configurable; warn in logs if set below 5 min). No syncer pause needed — fresh uploads in flight during mark are safe even if their metadata-txn lands after the snapshot; they'll be in the next mark cycle's live set.
- **D-06:** **Fail-closed = mark-phase error aborts the sweep entirely (INV-04).** Sweep workers do NOT start if mark returns any error; partial mark is treated as no live set, and the run is logged as failed. Exception only for sweep-side per-prefix DELETE errors (D-07).
- **D-07:** **Sweep-side DELETE errors: continue + capture; report at end.** Per-prefix worker captures errors and continues other prefixes. Run summary reports `total_deleted`, `total_errored`, first N error samples. A single S3 transient (503, network blip) does NOT kill the whole sweep run. Distinct from D-06's mark-phase fail-closed posture: mark errors threaten correctness (could delete live data); sweep errors only delay garbage reclamation.
- **D-08:** **GC trigger: periodic + on-demand both shipped.** Periodic `gc.interval` knob (default OFF in config — operator opt-in for v0.15.0 first deploy). On-demand: `dfsctl blockstore gc <share> [--dry-run]` CLI + REST endpoint at `/api/v1/shares/{name}/gc` (POST). Both share the same engine entry point.
- **D-09:** **Dry-run mode shipped: `--dry-run` + REST `dry_run=true` body field.** Executes mark + sweep enumeration; skips DELETEs; logs/returns the candidate-orphan list (capped to a configurable sample size, default 1000). Critical for first-time deployment confidence and for debugging suspected mark-phase bugs.
- **D-10:** **Observability via structured slog + persisted last-run summary.** Slog INFO at start/end with `run_id`, `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`, `error_count`. Persist summary to `<localStore>/gc-state/last-run.json` (overwritten each run); `dfsctl blockstore gc-status <share>` reads it. No Prometheus metrics in Phase 11 — metrics surface is a separate observability phase.

### State machine + atomicity (STATE-01..03, INV-03)

- **D-11:** **Upload first, then metadata-txn flips state to Remote (Claude pick).** Sequence: `(1)` syncer claims a Pending block (D-13); `(2)` PUT to S3 with `x-amz-meta-content-hash`; `(3)` `FileBlockStore.PutFileBlock` txn updates `State` to `Remote`. CAS keys are content-defined so any partial-failure re-upload writes the exact same bytes to the exact same key — idempotent by construction. The only failure mode is "S3 PUT succeeded, metadata-txn failed" → S3 object is a true orphan and GC reaps it next cycle. Three-phase metadata→upload→metadata rejected: doubles metadata write pressure; an Intent-state crash leaves stranded Syncing rows requiring extra recovery; CAS already makes the orphan window benign.
- **D-12:** **State physically lives on `FileBlock.State` indexed by ContentHash, persisted (Claude pick — literal STATE-01/03).** Three states: `Pending` (RefCount ≥ 1, not yet uploaded), `Syncing` (claimed by a syncer goroutine, upload in flight), `Remote` (PUT-success + metadata-txn success). No in-memory parallel state — STATE-03 is taken literally. Best introspection: "what's stuck in Syncing for >1h?" is a single backend query.
- **D-13:** **Pending → Syncing transition is batched per syncer claim cycle.** Syncer claims N Pending blocks per tick (`syncer.claim_batch_size`, default 32) in a single metadata txn that flips them all to Syncing with `last_sync_attempt_at = now`. Bounds metadata write rate to `claim_batches_per_tick`, not per-block. Claim is what serializes concurrent syncer instances against duplicate uploads.
- **D-14:** **Restart recovery via janitor pass at syncer startup.** Any `Syncing` row whose `last_sync_attempt_at > syncer.claim_timeout` (default 10 min) gets requeued back to `Pending` by a one-shot janitor at syncer Start. CAS idempotency means a duplicate re-upload (where the original eventually completes) is harmless — both writes hit the same key with the same bytes. Timeout configurable per-share if a workload demands different bounds.
- **D-15:** **Single owner of the Syncing → Remote transition: `engine.Syncer` in `pkg/blockstore/engine/syncer.go`.** The same goroutine that received the S3 200 calls `FileBlockStore.PutFileBlock(State=Remote)`. No callback up the LocalStore stack; LocalStore never sees the state machine. Background reconciler pattern rejected — eventual-consistency window not worth the complexity.
- **D-16:** **INV-03 verified by deterministic crash-injection unit test.** New `pkg/blockstore/engine/syncer_crash_test.go` uses `fakeRemoteStore` + `fakeMetadataStore` with kill-points injected at three positions: `pre-PUT`, `between-PUT-success-and-metadata-txn`, `post-metadata-txn`. Asserts: pre-PUT crash → no S3 object, no state change; mid crash → S3 object exists but `State=Syncing`, GC reaps it after grace; post crash → `State=Remote` consistent. No external dependencies (no Localstack); runs in `go test ./...`.
- **D-17:** **No `BackupHoldProvider` coupling (GC-04).** Phase 08 already deleted `BackupHoldProvider` + `FinalizationCallback` (v0.13.0 scaffolding). Phase 11 GC must NOT reintroduce a hold mechanism — the v0.16.0 atomic-backup design will use CAS immutability + manifest snapshots, which need no hold protocol on the GC side.

### Read-path verification + dual-read shim (INV-06, BSCAS-06 verify side)

- **D-18:** **BLAKE3 verification on S3 GET via streaming verifier wrapping `resp.Body`.** Implementation: an `io.Reader` wrapper that feeds bytes to a `blake3.Hasher` as the caller reads them. On EOF, compare `hasher.Sum(nil)` to the expected `ContentHash`. Zero extra allocation; verifier sees bytes once. On mismatch: discard the buffer, return wrapped `ErrCASContentMismatch`, do NOT surface bad bytes upstream. Aligns with Phase 10 BLAKE3 streaming pattern.
- **D-19:** **Header pre-check in addition to streaming recompute.** S3 GET response carries `x-amz-meta-content-hash` (set on PUT per BSCAS-06). Engine cheaply pre-checks header matches the expected `ContentHash` and rejects early on mismatch (saves the body read on a definitively wrong object). Then streaming recompute over the body provides the actual integrity guarantee. Fail-closed twice. Header alone is NOT sufficient (would trust S3 to never silently corrupt).
- **D-20:** **Hard perf gate: ≤5% rand-read regression vs. Phase 10 baseline.** New microbench in `pkg/blockstore/engine/` measures rand-read IOPS with verification enabled vs. disabled. Gate at ≤5% IOPS regression (within the global ≤6% budget set by STATE.md). If gate fails: profile (likely BLAKE3 throughput vs. network), iterate on the streaming impl. Block phase merge until met. Baseline target ≥1,350 IOPS (per ROADMAP key risk).
- **D-21:** **Dual-read resolution is by metadata key shape, NOT by S3 trial-and-error.** Engine consults metadata: if the requested `(payloadID, blockIdx)` has a corresponding `FileBlock` row with a `ContentHash`, read CAS path with verification. Otherwise (legacy file pre-Phase-11 with no FileBlock entry), fall back to legacy `FormatStoreKey(payloadID, blockIdx)` read with NO verification (legacy path is deprecated; verification cannot retroactively apply). One DB lookup per block; no doubled S3 GET cost. Legacy path is removed in Phase 15 (A6).
- **D-22:** **Cache key bifurcation during the dual-read window.** In-memory Cache keys CAS reads by `ContentHash`, legacy reads by legacy `storeKey`. Two key spaces coexist Phase 11 → Phase 14. Once Phase 14 migration completes, legacy entries naturally age out. Aligns with Phase 12 (CACHE) plan: hash-keyed cache + sequential prefetch. Single-key-space alternatives rejected: storeKey-only loses cross-file dedup; ContentHash-only forces hashing on legacy reads which weren't getting it before.

### Sync hot path + TD-09 + LocalStore narrowing (LSL-07/08, TD-09, INV-05)

- **D-23:** **TD-09: stage-bytes-and-release pattern in `flushBlock`.** Inside critical section: `bytes.Clone(mb.buf)` + snapshot `(offset, len)`, then `mb.mu.Unlock()`. Outside lock: `os.OpenFile` + `f.Write(staged)` + `f.Sync` + `f.Close`. Constant-cost copy per flush; concurrent readers/writers unblocked during the I/O. Minimal change vs. today's `local/fs/flush.go:94-169`. The "route through AppendWrite and retire flushBlock entirely" alternative is bigger scope than Phase 11 should absorb — flag for a follow-up cleanup once the CAS write path is stable.
- **D-24:** **Sync trigger: periodic only, retained from Phase 10.** Knob: `syncer.tick` (default 30s). Phase 10's pressure channel (LSL-04) ALSO triggers a sync drain on log fill — confirmed to route through the new CAS path post-Phase-11 (no separate code path). NFS COMMIT / SMB FLUSH-driven sync is a future RPO optimization deferred to a dedicated phase; protocol semantics today don't promise immediate remote durability.
- **D-25:** **Bounded share-wide upload concurrency pool.** `syncer.upload_concurrency` (default 8) goroutines per share drain the Pending claim batch. Caps S3 connections per share; predictable throughput. CAS-keyed deduplication naturally prevents duplicate uploads of the same hash within a tick (the Pending → Syncing claim already serializes, so two syncer goroutines won't claim the same block). Per-file serialization rejected — single-large-file scenarios would tank throughput.
- **D-26:** **LSL-07: drop the 5 named methods (22 → 17).** Removed: `MarkBlockRemote`, `MarkBlockSyncing`, `MarkBlockLocal` (state moves to `FileBlock.State` per STATE-03; engine.Syncer is the sole caller of state transitions per D-15); `GetDirtyBlocks` (replaced by direct on-disk state inspection from Phase 10's AppendWrite log + LSL-08 eviction); `SetSkipFsync` (S3-backend hint, irrelevant once writes go through AppendWrite). Net 22 → 17, exactly the spec target. Removing `WriteAt` and `EvictMemory` deferred to Phase 12+ (planner audit) — they have callers outside the syncer claim path that need to migrate first.
- **D-27:** **LSL-08: eviction driven from on-disk presence + in-process LRU keyed by ContentHash.** After Phase 10's `CommitChunks` atomically promotes a chunk into `blocks/{hash[0:2]}/{hash[2:4]}/{hash}`, LocalStore tracks an in-process LRU keyed by ContentHash. Eviction = unlink the file. No `FileBlockStore` lookup on the write hot path. The engine refetches from CAS on the next read if the local copy was evicted. Self-contained; no metadata-store callbacks. LRU bound is the existing local cache size knob.
- **D-28:** **`pkg/blockstore/sync/upload.go` rename → `pkg/blockstore/engine/syncer.go`.** Per ROADMAP "Files to touch". Single rename commit at the start of PR-A so subsequent diffs read cleanly. (Note: scout found the syncer is already at `pkg/blockstore/engine/syncer.go` post-Phase-10 — verify in planning whether the rename is already done; if so, this decision degrades to a no-op and is recorded for traceability.)
- **D-29:** **`ParseCASKey` companion shipped with `FormatCASKey` in `pkg/blockstore/types.go`.** Signature: `ParseCASKey(key string) (ContentHash, error)`. Validates the `cas/{hh}/{hh}/{hex}` shape, decodes the hash, returns typed error on malformed input. Symmetrical to `FormatStoreKey`/`ParseStoreKey` pattern (Phase 09 D-04 family).

### Structure & process

- **D-30:** **Three-PR split: write+state / read+verify+dual-read / GC+canonical-test.**
  - **PR-A** (BSCAS-01/03/06, STATE-01..03, INV-03, ParseCASKey, S3 PUT with `x-amz-meta-content-hash`, syncer rewrite to CAS, `FileBlock.State` schema + persistence, claim batching, restart-recovery janitor, `pkg/blockstore/sync/upload.go` → `engine/syncer.go` rename if needed, syncer crash-unit test). Unblocks Phase 12 (META-01 needs the FileBlock schema in place).
  - **PR-B** (INV-06 streaming verifier on S3 GET, header pre-check, dual-read engine resolver, cache key bifurcation, TD-09 stage-and-release, LSL-07 narrowing, LSL-08 eviction). Read path + write hot-path cleanup. Adds the rand-read perf gate (D-20).
  - **PR-C** (GC-01..04 mark-sweep with `EnumerateFileBlocks` cursor, `gc-state` on-disk live set, `dfsctl blockstore gc` + `--dry-run` + `gc-status`, conformance-suite addition for cursor, `TestBlockStoreImmutableOverwrites` E2E, GC-04 confirmation no `BackupHoldProvider`). Canonical-correctness gate.
  
  Each PR ships green independently. Matches Phase 08/09 multi-PR discipline.
- **D-31:** **Atomic per-requirement commits within each PR.** Bisect-friendly. Planner may subdivide further (e.g., split STATE-01..03 across two commits if FileBlock schema migration grows large; split GC-01..04 into mark / sweep / dry-run / observability commits).
- **D-32:** **Test scope: canonical E2E + crash-unit + conformance extensions + perf gate.**
  1. `test/e2e/TestBlockStoreImmutableOverwrites` — canonical correctness (ROADMAP success criterion #1).
  2. `pkg/blockstore/engine/syncer_crash_test.go` — INV-03 deterministic crash injection (D-16).
  3. `pkg/metadata/storetest/` extensions for `EnumerateFileBlocks` cursor (D-02 conformance — every backend MUST pass).
  4. `pkg/blockstore/local/localtest/` extensions for narrowed `LocalStore` + LSL-08 eviction (D-26, D-27).
  5. Microbench gate for INV-06 verification (D-20: ≤5% rand-read regression).
  6. Microbench gate confirming no rand-write regression vs. Phase 10 baseline (≤6% budget).
  
  Generic reusable crash-injection harness deferred — Phase 11 ships hand-rolled tests; harness is a separate test-infra effort.
- **D-33:** **`x-amz-meta-content-hash` external-verifier sanity check in test/e2e.** A small test that runs a write through the full server, then uses `aws s3api head-object` (or boto3) outside DittoFS to confirm the metadata header is set correctly. Satisfies BSCAS-06's "external tooling can verify without DittoFS metadata" criterion. Uses the existing Localstack fixture.
- **D-34:** **Docs updated: ARCHITECTURE, FAQ, IMPLEMENTING_STORES, CONFIGURATION.**
  - `docs/ARCHITECTURE.md` — replace path-prefix GC description with mark-sweep; add three-state lifecycle diagram; note CAS dual-read window; reference `gc-state/` directory structure.
  - `docs/FAQ.md` — add entries: "How does GC work in v0.15.0?", "What is the dual-read window?", "Why am I seeing residual `{payloadID}/block-...` keys after upgrading?".
  - `docs/IMPLEMENTING_STORES.md` — document the new `MetadataStore.EnumerateFileBlocks(ctx, fn)` cursor contract; include conformance-suite usage; document the `RemoteStore` PUT-with-metadata-headers contract requirement (BSCAS-06).
  - `docs/CONFIGURATION.md` — add `gc.interval`, `gc.sweep_concurrency`, `gc.grace_period`, `gc.dry_run_sample_size`, `syncer.upload_concurrency`, `syncer.claim_batch_size`, `syncer.claim_timeout` knobs.
  - `docs/CLI.md` — add `dfsctl blockstore gc <share> [--dry-run]` and `dfsctl blockstore gc-status <share>` reference.
  - `README.md` — no change (Phase 11 is internal infrastructure; user-visible behavior is unchanged for read/write semantics).
  - CHANGELOG — deferred to v0.15.0 shipment (matches Phase 08 D-24).
- **D-35:** **No metrics/Prometheus surface added in Phase 11.** Logs + persisted last-run summary cover Phase 11 needs. A metrics phase is the right home for `gc_*`, `syncer_*` Prometheus counters and would also wire metrics for v0.13.0 backup paths in one consistent surface.

### Claude's Discretion

- **Backing impl for `<localStore>/gc-state/` exact set** (D-01) — planner picks Badger temp store vs. flat sorted file vs. boltdb based on dependency-footprint analysis. Recommend Badger since `dgraph-io/badger` is already a dep.
- **Exact `MetadataStore.EnumerateFileBlocks` Go signature** (D-02) — `fn func(ContentHash) error` vs. iterator type vs. channel-based — planner picks based on existing storetest conventions.
- **Exact field layout for `FileBlock.State` + `last_sync_attempt_at`** (D-12, D-13) — planner picks the schema migration shape (new field on existing FileBlock vs. new table) per backend's idiomatic pattern.
- **Whether the syncer rename (D-28) is already done post-Phase-10** — planner verifies first; degrades to no-op if so.
- **Exact `dfsctl` REST endpoint paths** (D-08, D-09, D-10) — planner picks based on existing `/api/v1/shares/{name}/...` conventions.
- **Whether to remove `WriteAt` and `EvictMemory` from `LocalStore` in this phase** (D-26) — planner audits all callers; if all are syncer-claim-pathy, drop them; otherwise leave for a follow-up.
- **Per-PR commit subdivision** (D-31) — above the per-requirement floor; planner subdivides only where it improves reviewability.
- **Whether `docs/CONTRIBUTING.md` gets a "Adding a new metadata-backend EnumerateFileBlocks" recipe** (D-34) — include if the touched-docs diff stays small; otherwise defer.

### Folded Todos

None — no pending todos from the backlog matched Phase 11 scope.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner, executor) MUST read these before acting.**

### Roadmap & Requirements
- `.planning/ROADMAP.md` §"Phase 11: CAS write path + GC rewrite (A2)" — success criteria, files to touch, key risks, dependency on Phase 10.
- `.planning/REQUIREMENTS.md` §"Block-store CAS (BSCAS)" — BSCAS-01, BSCAS-03, BSCAS-06.
- `.planning/REQUIREMENTS.md` §"Local store — hybrid Logs + Blocks (LSL)" — LSL-07, LSL-08.
- `.planning/REQUIREMENTS.md` §"Garbage collection (GC)" — GC-01..GC-04.
- `.planning/REQUIREMENTS.md` §"Block state machine (STATE)" — STATE-01..STATE-03.
- `.planning/REQUIREMENTS.md` §"Invariants (INV)" — INV-01, INV-03, INV-04, INV-05, INV-06.
- `.planning/REQUIREMENTS.md` §"Tech-debt cleanup (TD)" — TD-09.
- `.planning/REQUIREMENTS.md` §"Traceability" — confirms Phase 11 ownership of all 18 requirements above; cross-checks GH #422.
- `.planning/PROJECT.md` §"Current Milestone: v0.15.0" — milestone scope and rationale.
- `.planning/STATE.md` §"v0.15.0 Decisions" — locked: BLAKE3 via `lukechampine.com/blake3`, CAS key format, dual-read shim window (A2–A5), v0.13.0 backup compat NOT required, ≤6% perf regression budget, global per-metadata-store dedup scope.

### Predecessor / Successor Phase Context
- `.planning/phases/08-pre-refactor-cleanup-a0/08-CONTEXT.md` — Phase 08 deleted `BackupHoldProvider`/`FinalizationCallback` (foundation for GC-04); established multi-PR + per-requirement-atomic-commit discipline; D-08 amendment locked `lukechampine.com/blake3` over `zeebo/blake3`.
- `.planning/phases/09-adapter-layer-cleanup-adapt/09-CONTEXT.md` — Phase 09 produced `internal/adapter/common/` shared helpers + the `metadata.ExportError → protocol-code` table that Phase 11's syncer/engine errors flow through; D-12 locked the "Phase 12 changes engine signature, not Phase 11" boundary.
- `.planning/ROADMAP.md` §"Phase 12: CDC read path + metadata schema + engine API (A3)" — Phase 12 consumes Phase 11's groundwork via META-01 (`FileAttr.Blocks []BlockRef` reintroduction) + API-01 (engine signature change). Phase 11's `FileBlock.State` schema work must land first.
- `.planning/ROADMAP.md` §"Phase 14: Migration tool (A5)" — Phase 14 ends the dual-read window; Phase 11's dual-read shim is explicitly time-bounded by it.
- `.planning/ROADMAP.md` §"Phase 15: Legacy cleanup (A6)" — Phase 15 removes `FormatStoreKey`/`ParseStoreKey` and the dual-read code path.

### Project invariants (architectural constraints that bind Phase 11)
- `CLAUDE.md` §"Architecture invariants" — rules 1, 2, 3, 4, 5, 6, 7 directly apply:
  - Rule 1: "Protocol handlers handle only protocol concerns" — engine.Syncer owns state transitions; handlers stay thin (already enforced by Phase 09's `common/`).
  - Rule 2: "Every operation carries an `*metadata.AuthContext`" — `MetadataStore.EnumerateFileBlocks` (D-02) takes auth context.
  - Rule 3: "File handles are opaque" — dual-read resolution (D-21) reads `FileBlock` rows by `(payloadID, blockIdx)` from metadata; never parses handles.
  - Rule 4: "Block stores are per-share" — GC cross-share aggregation (D-03) keys on remote-store identity, not share name.
  - Rule 5: "WRITE coordinates metadata + block store" — D-11's PUT-then-meta-txn ordering is the WRITE-path counterpart for CAS.
  - Rule 6: "Error codes: return `metadata.ExportError` values" — engine bubbles BLAKE3 mismatch, GC errors, syncer errors as typed errors mapped via `internal/adapter/common/`.
  - Rule 7: "Metadata store contract lives in `pkg/metadata/storetest/`" — D-02 explicitly extends the conformance suite for the new cursor.

### Source files to read (Phase 11 work)

**BSCAS-01 / BSCAS-03 / BSCAS-06 (CAS write path):**
- `pkg/blockstore/types.go` — `ContentHash` (post-Phase-10), `CASKey()`, `FormatStoreKey`/`ParseStoreKey` (legacy); add `FormatCASKey`/`ParseCASKey`.
- `pkg/blockstore/engine/syncer.go` — current `Syncer.periodicUploader`, `syncLocalBlocks`, `syncFileBlock` (the rewrite target).
- `pkg/blockstore/remote/s3/store.go:222-239` — `WriteBlock` (extend `PutObjectInput.Metadata` for `x-amz-meta-content-hash`).
- `pkg/blockstore/chunker/` (Phase 10 output) — chunker API the syncer drives.

**STATE-01..03, INV-03 (state machine + atomicity):**
- `pkg/blockstore/types.go:64-83` — current 4-state `BlockState` enum (target: collapse to 3, persisted on FileBlock).
- `pkg/blockstore/local/local.go:138-145` — current `MarkBlock{Local,Syncing,Remote}` setters (deletion targets per LSL-07).
- `pkg/metadata/` — `FileBlock` schema location (planner audits — likely `pkg/metadata/types.go` or a backend-specific file).
- `pkg/blockstore/engine/syncer.go` — single owner of new state transitions (D-15).

**INV-06, dual-read shim (read path verification):**
- `pkg/blockstore/remote/s3/store.go:241-261` — `ReadBlock` (wrap response body with streaming BLAKE3 verifier).
- `pkg/blockstore/engine/engine.go` — `BlockStore.ReadAt` entry (dual-read resolution lives here per D-21).
- `pkg/blockstore/cache/` — Cache key handling (bifurcation per D-22).

**TD-09 (lock scope fix):**
- `pkg/blockstore/local/fs/flush.go:94-169` — `flushBlock` current implementation; lock scope to fix.

**LSL-07 / LSL-08 (interface narrowing + eviction):**
- `pkg/blockstore/local/local.go:49-164` — `LocalStore` interface declaration (22 methods).
- `pkg/blockstore/local/fs/` — implementations of methods being removed; in-process LRU lives in this package.

**GC-01..04 (mark-sweep):**
- `pkg/blockstore/engine/gc.go` — current path-prefix `CollectGarbage` (full rewrite target).
- `pkg/metadata/` — add `EnumerateFileBlocks` cursor to `MetadataStore` interface; implement per-backend (memory, Badger, Postgres).
- `pkg/metadata/storetest/` — extend conformance suite for the new cursor (D-02).
- `pkg/controlplane/runtime/shares/service.go` — entry to enumerate all shares with same remote identity (D-03).

**Test targets:**
- `test/e2e/` — new `TestBlockStoreImmutableOverwrites` (canonical correctness, ROADMAP success criterion #1).
- `pkg/blockstore/engine/syncer_crash_test.go` — new (INV-03 deterministic crash injection, D-16).
- `pkg/metadata/storetest/` — `EnumerateFileBlocks` conformance scenarios.
- `pkg/blockstore/local/localtest/` — extensions for narrowed `LocalStore` + LSL-08 eviction.
- `test/e2e/` — Localstack-backed `x-amz-meta-content-hash` external-verifier sanity test (D-33).

### Docs affected
- `docs/ARCHITECTURE.md` — mark-sweep GC, three-state lifecycle, dual-read window, `gc-state/` directory (D-34).
- `docs/FAQ.md` — GC explanation, dual-read window, residual legacy keys (D-34).
- `docs/IMPLEMENTING_STORES.md` — `MetadataStore.EnumerateFileBlocks` cursor contract; `RemoteStore` PUT-with-metadata-headers requirement (D-34).
- `docs/CONFIGURATION.md` — new `gc.*` and `syncer.*` knobs (D-34).
- `docs/CLI.md` — `dfsctl blockstore gc` + `gc-status` (D-34).
- `docs/CONTRIBUTING.md` — Claude's discretion per D-34.

### External references
- BLAKE3 spec: https://github.com/BLAKE3-team/BLAKE3-specs/blob/master/blake3.pdf — for streaming-verifier hash semantics on partial reads.
- AWS S3 PutObject metadata: https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html — `x-amz-meta-*` user metadata header semantics.
- AWS S3 ListObjectsV2: https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html — for sweep prefix walking.

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- **`pkg/blockstore/chunker/`** (Phase 10) — FastCDC chunker; syncer drives this for new uploads. Public: `NewChunker()`, `Next(data, final) (cut, ok)`. Min/avg/max = 1/4/16 MiB.
- **`pkg/blockstore/types.go` `ContentHash.CASKey()`** (Phase 10) — returns `"blake3:" + hex.EncodeToString(h[:])`. Phase 11 adds `FormatCASKey`/`ParseCASKey` companions.
- **`pkg/blockstore/local/fs/appendwrite.go`** (Phase 10) — `AppendWrite` + per-payload mutex + interval tree; the post-Phase-11 write path. `flushBlock` (legacy) coexists during the transition.
- **`pkg/blockstore/engine/syncer.go`** — Syncer.periodicUploader / syncLocalBlocks / syncFileBlock; the target of the CAS rewrite. Goroutine-per-tick model retained; upload internals replaced.
- **`pkg/blockstore/remote/s3/store.go`** — `WriteBlock` / `ReadBlock` thin wrappers over AWS SDK v2; PutObjectInput / GetObjectOutput already structured for metadata-header expansion.
- **`pkg/metadata/storetest/`** — conformance suite for metadata stores; pattern to extend for `EnumerateFileBlocks` cursor (CLAUDE.md invariant 7).
- **`pkg/blockstore/local/localtest/`** — Phase 10 already extended for AppendWrite; pattern to extend for narrowed LocalStore + LSL-08 eviction.
- **`internal/adapter/common/` (Phase 09)** — error mapping table; engine errors (`ErrCASContentMismatch`, GC errors) flow through this without adapter-side changes.

### Established Patterns
- **Multi-PR atomic commits per requirement** — Phase 08 D-11/D-31, Phase 09 D-15/D-16 set the pattern. Phase 11 D-30/D-31 follows.
- **Conformance-suite contracts** — `pkg/metadata/storetest/`, `pkg/blockstore/local/localtest/`, `pkg/blockstore/remote/remotetest/` (CLAUDE.md invariant 7). Every backend MUST pass.
- **Per-share block-store ownership** — `*engine.BlockStore` is per-share, accessed via `runtime.GetBlockStoreForHandle` (CLAUDE.md invariant 4). GC cross-share coordination (D-03) groups by remote identity, not share name.
- **Streaming I/O via `io.Reader` wrapping** — Phase 10's chunker reads streamed; Phase 11's verifier (D-18) wraps S3 body the same way.
- **Periodic background workers + pressure channels** — Phase 10 LSL-04 established; Phase 11 syncer keeps the same shape, swaps internals.
- **Slog structured logging at INFO** — used across the codebase; Phase 11 GC observability (D-10) follows.

### Integration Points
- **Phase 12 (A3) consumes the FileBlock schema work**: META-01 reintroduces `FileAttr.Blocks []BlockRef` referencing the same FileBlock rows Phase 11 manages State on; API-01 changes engine ReadAt/WriteAt to accept `[]BlockRef`. Phase 11's dual-read engine resolver (D-21) is structured so the Phase 12 signature change touches `engine.go` internals and the dual-read becomes redundant once `[]BlockRef` is in metadata.
- **Phase 13 (A4) consumes the CAS write path**: BSCAS-04/05 file-level dedup uses Phase 11's `cas/...` keys + ContentHash addressing.
- **Phase 14 (A5) ends the dual-read window**: migration tool re-chunks legacy `{payloadID}/block-{idx}` data to CAS; Phase 11's dual-read resolver becomes the legacy-only path post-migration; deleted in Phase 15.
- **Adapter layer untouched**: Phase 09 already consolidated NFS/SMB error mapping. Phase 11 introduces new engine errors (`ErrCASContentMismatch`, GC errors) but they fold into Phase 09's `common/` table without adapter-side changes.
- **Runtime layer untouched**: `*runtime.Runtime` API is stable; Phase 11 only changes engine internals + adds a metadata-store interface method (extended via existing per-share dispatch).

</code_context>

<specifics>
## Specific Ideas

- **CAS immutability is the load-bearing invariant of this milestone** (BSCAS-01, INV-01, ROADMAP success criterion #1). `TestBlockStoreImmutableOverwrites` is the canonical proof: after overwriting a file, the OLD bytes still live at the OLD CAS key in S3 (until GC reaps them); the NEW bytes live at a NEW CAS key. Every Phase 11 design choice protects this — most importantly D-11's PUT-first ordering (re-uploads are content-defined identical → idempotent), D-13's batched claim (Pending → Syncing transition is the only serialization point against duplicate uploads), and D-21's metadata-driven dual-read (no S3 trial-and-error that could mask key-shape bugs).
- **Fail-closed posture is asymmetric (D-06 vs D-07)**: mark-phase errors abort the sweep entirely (any uncertainty about the live set could lead to deleting referenced data — unacceptable). Sweep-side per-prefix DELETE errors continue + capture (a single S3 transient should not waste an entire mark-phase computation). Both behaviors are intentional and tested.
- **STATE-03 taken literally** (D-12): no in-memory parallel state. The `Pending → Syncing` claim is a metadata write, not an in-process lock. This costs one batched metadata txn per syncer tick (cheap with `claim_batch_size=32`) but buys exact restart recovery (D-14) and exact introspection ("what's stuck?" is one query).
- **No commit-driven sync in Phase 11** (D-24). NFS COMMIT and SMB FLUSH today don't promise immediate remote durability — DittoFS treats them as durability-to-local-store fences. Adding remote-durability semantics requires a protocol-promise change and adapter-layer plumbing through Phase 09's `common/` helpers; that's a future RPO-focused phase, not this one.
- **Dual-read window time-bound** (D-21, D-22): the legacy code path lives Phase 11 → Phase 14 (A5). Phase 14's migration tool re-chunks all legacy data to CAS. Phase 15 (A6) deletes the legacy path. Anyone touching the dual-read code in Phase 11/12/13 should know it's intentionally on a deletion clock.
- **Backing impl for `gc-state/`** (D-01): Badger is the recommended choice because it's already a dependency for the metadata store and supports atomic batch writes for the mark-phase append rate. Flat sorted file is the alternative if planner finds Badger overhead unjustified.
- **No new external dependencies in Phase 11.** Reuses `lukechampine.com/blake3` (Phase 10) and `dgraph-io/badger` (existing metadata backend). The S3 metadata header is plain `map[string]string` — no new SDK surface.

</specifics>

<deferred>
## Deferred Ideas

- **Reusable crash-injection test harness** (D-32 alternative): build a generic crash-injection framework in `test/e2e/framework/` for Phase 12-14. Defer to a dedicated test-infra phase.
- **Prometheus metrics for GC + syncer** (D-10, D-35 alternative): defer to a metrics/observability phase that also covers v0.13.0 backup paths in one consistent surface.
- **COMMIT/FLUSH-driven sync** (D-24 alternative): NFS COMMIT and SMB FLUSH triggering immediate remote durability. Requires protocol-promise change and adapter-layer plumbing. Future RPO-focused phase.
- **Adaptive sweep concurrency** (D-04 alternative): ramp until S3 503 SlowDown, back off. Defer until we have real bucket-scale data.
- **Resume-on-restart for partial mark phase** (D-01 alternative): instead of clean-restart, resume from the last persisted batch. Doubles test surface for marginal benefit since mark is idempotent.
- **`WriteAt` and `EvictMemory` removal from `LocalStore`** (D-26 expansion): tighter than the LSL-07 spec target; defer to planner audit in Phase 12+.
- **Routing all writes through Phase 10 AppendWrite to retire `flushBlock` entirely** (D-23 alternative): bigger scope than Phase 11 should absorb. Follow-up cleanup once CAS write path is stable.
- **Three-phase metadata→upload→metadata ordering** (D-11 alternative): doubles metadata write pressure; CAS idempotency makes the simpler upload-first ordering safe.
- **Bloom-filter live set** (D-01 alternative): false positives would silently retain garbage forever; rejected.
- **Skip verification on cache-hit reads** (D-18/D-20 alternative): defers IOPS hit to cold-cache path; revisit only if the streaming verifier perf gate fails despite optimization.
- **Per-share atomic backup integration** (GC-04 reconfirmation): v0.16.0 milestone, explicitly excluded.
- **Block-payload compression/encryption**: separate BlockStore Security milestone.

</deferred>

---

*Phase: 11-cas-write-path-gc-rewrite-a2*
*Context gathered: 2026-04-25*
