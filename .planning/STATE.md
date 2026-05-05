---
gsd_state_version: 1.0
milestone: v0.15.0
milestone_name: milestone
status: executing
stopped_at: Phase 14 Plan 03 shipped — migrate-tool-core (MIG-01, MIG-02)
last_updated: "2026-05-05T18:50:00.000Z"
last_activity: 2026-05-05
progress:
  total_phases: 8
  completed_phases: 4
  total_plans: 66
  completed_plans: 61
  percent: 92
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-04-23)

**Core value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility
**Current focus:** Phase 13 — merkle-root-file-level-dedup-a4

## Current Position

Milestone: v0.15.0
Phase: 14
Plan: 03 of 07 complete (migrate-tool-core — MIG-01 + MIG-02 partial)
Branch: `gsd/phase-12-cdc-read-path-metadata-engine-api`
Status: Plan 14-03 shipped; Plan 14-04 (parallel + bandwidth + production runtime composition) ready to execute
Last activity: 2026-05-05

## Next Actionable

Phase 12 (A3): CDC read path + metadata schema + engine API. 14 requirements across META-01/03/04, API-01/02/03/04, CACHE-01..06, INV-02. Estimated ~2 weeks. Dependencies satisfied: Phase 11 (A2, #422, shipped PR #453) + Phase 09 (ADAPT, #427, shipped PR #438).

- `/gsd-discuss-phase 12 --chain` — interactive discuss → auto plan + execute
- `/gsd-discuss-phase 12 --auto` — fully autonomous (Claude picks defaults)
- `/gsd-plan-phase 12` — skip discuss, go straight to planning
- GH issue: #423

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |
| v3.8 SMB3 Protocol Upgrade | 33-40.5 | 26 | Mar 1-4, 2026 | 2026-03-04 |
| v4.2 Benchmarking & Performance | 57-62 | -- | Mar 4, 2026 | 2026-03-04 |
| v4.0 BlockStore Unification | 41-49 | 24 | Mar 9-11, 2026 | 2026-03-11 |
| v4.3 Protocol Gap Fixes | 49.1-49.3 | 1 | Mar 12-13, 2026 | 2026-03-13 |
| v4.7 Offline/Edge Resilience | 63-68 | 10 | Mar 15-20, 2026 | 2026-03-20 |
| v0.10.0 Production Hardening + SMB fixes | 69-73.1 | — | Mar 20-25, 2026 | in flight |
| v0.13.0 Metadata Backup & Restore | 1-7 | 38 | Apr 2026 | phases complete; not released |

## Accumulated Context

### v0.15.0 Progress

| Phase | Name | Status | PR |
|-------|------|--------|-----|
| 08 | Pre-refactor cleanup (A0) | shipped | #437 |
| 09 | Adapter layer cleanup (ADAPT) | shipped | #438 |
| 10 | FastCDC chunker + hybrid local store (A1) | shipped | #443 |
| 11 | CAS write path + GC rewrite (A2) | shipped | #453 (squash 2b96c965, merged 2026-04-26) |
| 12 | CDC read path + metadata schema + engine API (A3) | **ready to plan** | #423 (issue) |
| 13 | Merkle root + file-level dedup (A4) | blocked by 12 | #424 |
| 14 | Migration tool (A5) | **in progress** — Plans 01–03 shipped 2026-05-05; MIG-01 + MIG-02 partial + MIG-03 routing seam complete; Plan 14-04 (parallel + bandwidth + production runtime composition) is next | #425 |
| 15 | Legacy cleanup (A6) | deferred until A5 in production | #426 |

### v0.15.0 Decisions

- Phase numbering continues from v0.13.0 last phase (7) → v0.15.0 starts at 8 and runs 08-15 (8 phases total)
- Phase directories under `.planning/phases/01-*` through `07-*` (v0.13.0) remain for historical reference; v0.15.0 phase dirs will be `08-*` through `15-*`
- v0.13.0 archive lives at `.planning/milestones/v0.13.0-archive/`
- Fine granularity (from config.json) — 8 phases preserving natural plan boundaries: A0, ADAPT, A1–A6
- Two parallel pre-cleanup tracks (A0 / ADAPT) converge at A3 (engine API change consumes ADAPT groundwork)
- Block key scheme: content-addressable `cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}` with BLAKE3 (via `lukechampine.com/blake3`; D-08 amended 2026-04-24 — swapped from `zeebo/blake3`, user-approved)
- D-41 gate is platform-aware (amended 2026-04-24, user-approved Option A): amd64 requires BLAKE3 ≥ 3.0× SHA-256; arm64 requires ≥ 1.0× (hw-SHA vs portable-Go BLAKE3 asymmetry). Hard 3× target validated on CI amd64 perf lane per D-43.
- Chunking: in-house FastCDC (~200 LoC), min=1MB / avg=4MB / max=16MB, normalization level 2
- Dedup scope: global per metadata store (RefCount spans shares when remote config shared)
- Merkle-root `FileAttr.ObjectID` is lazy (computed at file quiesce), not eager — revisit if dedup hit rate demands eager update
- Migration via `dfsctl blockstore migrate --share <name>`; dual-read shim lives A2–A5; removed in A6 after production rollout confirmed
- v0.13.0 backup backward compatibility NOT required (v0.13.0 never released) — backup code paths are free to break across phases
- Performance regression tolerance: ≤6% on random write (≥600 IOPS), random read (≥1350), sequential write (≥48 MB/s), sequential read (≥60 MB/s)
- A6 (Phase 15) intentionally deferred until A5 (Phase 14) rollout confirmed in production
- Phase 12 Plan 04 (META-03 / D-09): public `blockstore.FileBlockStore` is the spec-literal 6 methods (`GetByHash`, `Put`, `Delete`, `IncrementRefCount`, `DecrementRefCount`, `ListPending`); engine-internal helpers (`GetFileBlock`, `ListFileBlocks`) live on a separate `EngineFileBlockStore` interface and on `metadata.MetadataStore`; `EnumerateFileBlocks` lifted from `FileBlockStore` to `MetadataStore` (D-08).
- Phase 12 Plan 07 (API-01..04): engine API threads `[]BlockRef` end-to-end on ReadAt/WriteAt/Truncate/Delete/CopyPayload. CopyPayload is now O(1) via `MetadataCoordinator.IncrementRefCount` per unique source hash. API-02 strict-grep gate enforced: zero `pkg/metadata` imports under `pkg/blockstore/engine/*.go` production files except `gc.go` (preceded by `// API-02 justification:`). PayloadID stays as `string` at the engine seam — deviation from plan's `blockstore.PayloadID` alias avoided adapter-wide type-system churn (`metadata.PayloadID` is the convention).
- Phase 12 Plan 08 (CACHE-05 seam + CopyPayload): adds `common.CacheInvalidator` interface (defined in package common, not engine — Phase 09 narrow-interface pattern), `common.diffRemovedHashes` (BlockRef hash set-diff preserving multiplicity for refcount-aware callers), `common.CopyPayload` (atomic engine.CopyPayload + dst PutFile in one metadata txn — BLOCKER-2 resolution; mid-loop Increment failure rolls back ALL writes; cache.InvalidateFile fires only on commit success per D-35), and explicit `ErrBlockRefMissing` rows in content_errmap.go (D-23 — operators triage CAS-integrity failures via log inspection; wire surface is identical to ErrCASContentMismatch). `common.{Read,Write}ToBlockStore` signatures kept unchanged because the executor's strict critical_constraint forbids touching protocol handlers — the actual []BlockRef threading deferred to Plan 12-09 cache rewrite when an engine-side accessor pattern lands. Plan body's action-step-3 (which directs handler call-site updates) was reconciled in favor of the `<must_haves>` truth that handlers stay untouched (D-26).
- Phase 12 Plan 09 (CACHE-01..05 + Null Object): greenfield CAS-keyed `engine.Cache` replaces Phase 11's `ReadBuffer` (keyKindCoord/CAS/Legacy bifurcation from D-22) and the standalone `engine.Prefetcher` worker pool. Single keyspace (ContentHash), single budget, single Get/Put/InvalidateFile API. CACHE-03 sequential threshold raised from 2 to 3. CACHE-04 OnRead is the sole prefetch hint API; engine.ReadAt invokes it post-read with BlockRef hashes. CACHE-05 InvalidateFile is surgical (drops only listed hashes; preserves cross-file dedup CACHE-02). `nullCache{}` Null Object eliminates defensive `if bs.cache == nil` guards — verified by grep. Cache is hint-only for Plan 09 (engine.ReadAt does NOT serve from cache.Get); Plan 10 mmap reintroduces byte-serving without heap-copy cost. Flush auto-promote removed (Phase 11 behavior didn't translate to hash-keyed cache; OS page cache covers the hot-path benefit until Plan 10). CacheStats JSON shape preserved (read_buffer_entries / read_buffer_used / read_buffer_max) so dfsctl block stats keeps working. Deviation: Task 1's literal "replace cache.go entirely" would have broken the Task 1 build (engine.go still references ReadBuffer/Prefetcher), so the new Cache was added alongside in Task 1 and the legacy code deleted in Task 3. Plan 12-08's deferred []BlockRef threading through common.{Read,Write}ToBlockStore was NOT addressed in this plan — the engine accessor pattern that would unblock it is still missing; flagged for a future cleanup plan or absorbed into Plan 13's file-level dedup integration.

### v0.13.0 Decisions (archived context)

Historical v0.13.0 decisions preserved in `.planning/milestones/v0.13.0-archive/` for reference; the v0.15.0 refactor deletes `BackupHoldProvider` + `FinalizationCallback` (v0.13.0 scaffolding) in Phase 08.

- Phase 14 Plan 01 (MIG-03 / D-A6): per-share `block_layout` flag landed across Memory + Badger + Postgres backends. New `metadata.BlockLayout` enum (legacy / cas-only) on `ShareOptions`, `ParseBlockLayout` empty-string-coerces-to-legacy for forward-compat with pre-Phase-14 rows, unknown values surface as `ErrInvalidBlockLayout` (T-14-01-01). Postgres uses a dedicated `block_layout TEXT NOT NULL DEFAULT 'legacy'` column (migration 000014, reversible) authoritative over the legacy options JSON blob. Conformance suite `storetest.RunBlockLayoutSuite` invoked from all three backend test files; Memory + Badger pass green by default, Postgres compiles + skips cleanly without `DITTOFS_TEST_POSTGRES_DSN`. **Pre-existing Badger bug fixed:** `CreateRootDirectory.createNewRoot` (and the transactional equivalent) was overwriting `ShareOptions` with a fresh `metadata.Share{Name: shareName}` literal — silently wiping not just BlockLayout but every other share option. Fix preserves the existing `Share.Options` when materializing the root row. Commits: `67af6a8b` (types), `7eff1c34` (backends), `5b30ff05` (conformance + Badger fix).

- Phase 14 Plan 02 (MIG-03 / D-A8): per-share `BlockLayout` gate landed inside `engine.Syncer.dispatchRemoteFetch`. New `engine.ErrLegacyReadOnCASOnly` sentinel surfaces as a fail-loud signal when a legacy-shaped FileBlock (zero `Hash`) is encountered on a `cas-only` share — the function logs at Error with `block_id` + `store_key` and returns the wrapped sentinel rather than silently falling back to `ReadBlock`. CAS path untouched; legacy shares preserve the dual-read fallback unchanged (T-14-02-03 non-regression asserted). `BlockLayout` field lives on `Syncer` (binding the routing decision next to the gate); `BlockStore.BlockLayout()` getter delegates for tests + Plan 14-05 cutover reload. `shares.Service.createBlockStoreForShare` reads `BlockLayout` from `metadata.ShareOptions` (D-A6 source-of-truth, casts `EngineFileBlockStore` → `metadata.MetadataStore` mirroring the coordinator-wiring pattern) and threads into `SyncerConfig.BlockLayout`. Empty/unknown values coerce to legacy at `NewSyncer` time (defense-in-depth — engine never trusts the metadata layer's coercion was applied). Three new dual-read tests + getter round-trip + three wiring tests (cas-only / legacy / zero-value) cover the matrix. Commits: `501bc008` (gate + tests), `531fd20b` (wiring + getter).

- Phase 14 Plan 03 (MIG-01 + MIG-02 partial / D-A1..D-A5, D-A14): central FastCDC re-chunk loop landed end-to-end against memory fixtures. New importable `pkg/blockstore/migrate` package hosts the `Journal` (append-only JSONL with periodic snapshot rotation, atomic-rename invariant, read-only open variant for the REST status handler in Plan 14-06) and `WalkShareFiles` helper (composes `GetRootHandle` + `ListChildren` + `GetFile`, paginated, ctx-cancel-aware). Migration loop in `cmd/dfsctl/commands/blockstore`: per-file walk → FastCDC re-chunk over `legacyPayloadReader` → `FileBlockStore.GetByHash` dedup probe → upload-or-IncrementRefCount → `PutFile` Blocks + ObjectID in single metadata txn → journal Append (after PutFile success — T-14-03-02 ordering rule). First-committer-wins ObjectID conflict (D-14): `PutFile` retry with `ObjectID=zero` preserves Blocks while yielding the unique-index entry to the canonical first-committer. Sparse-block zero-fill in legacy reader matches dual-read shim's hole semantic. Empty files journal `file_skipped`. **Production composition deferred:** `openOfflineRuntime` returns `ErrOfflineRuntimeNotWired` today; controlplane-DB plumbing for per-share metadata + remote stores lands in Plan 14-04 alongside parallelism + bandwidth, end-to-end exercise via Plan 14-07 runbook. Loop is fully unit-tested via `newTestOfflineRuntime` test helper (8 loop tests + 8 journal tests + 6 walk tests). Plan-side `<action>` step 4 explicitly authorizes this split. BLOCKER 2 (zero daemon-runtime imports in `migrate_runtime.go`) and BLOCKER 3 (journal lives in `pkg/`, not `cmd/`) both verified by acceptance grep gates. Commits: `f87486fd` (Task 1 — command skeleton + daemon probe, prior session), `2c0263b1` (Task 2 — journal + walk), `3a9bd867` (Task 3 — loop + offline runtime).

### Pending Todos

- Phase 12 (A3) follow-ups carried from Phase 11 review:
  - DEFERRED IN-4-03: async GC + 202+poll REST surface (long-running mark-sweep would otherwise time out reverse proxies)
  - DEFERRED WR-3-02 wiring: `gc.interval` periodic-ticker not yet wired into Runtime startup; currently warn-only if set in config
  - DEFERRED WR-4-01 follow-up: dedup short-circuit still leaks donor refcount; needs design decision (mirror increment to `fb` vs. drop short-circuit vs. point `fb` at `existing.ID` with ID-mapping)
- Before Phase 14 (A5) ship: benchmark VM-fleet dedup fixture achieves ≥40% reduction (VER-03 gate)
- Before Phase 15 (A6) merge: confirm `dfsctl blockstore migrate status` reports 100% for every production share

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-05-05T18:50:00.000Z
Stopped at: Phase 14 Plan 03 shipped — migrate-tool-core (Tasks 2+3 resumed and completed atomically after prior stream-timeout)
Next action: Plan 14-04 (parallel + bandwidth + production runtime composition). Picks up `openOfflineRuntime` stub (currently returns `ErrOfflineRuntimeNotWired`) and replaces with controlplane-DB-backed composition + upload `*rate.Limiter` + worker-pool wrapping `rechunkAndUpload`. Interface seam (`offlineRuntime` accessors) is stable.

**Shipped Phase:** 11 (cas-write-path-gc-rewrite-a2) — 9 plans + ~30 review/fix commits — 2026-04-26T18:03:03Z (PR #453)

**Planned Phase:** 13 (Merkle root + file-level dedup (A4)) — 10 plans — 2026-04-28T09:12:48.291Z

### Plan 12-08 / 12-09 carry-forward to Plan 12-10 or later

Plan 12-09 completed the cache rewrite (CACHE-01..05) but did NOT pick up the deferred []BlockRef threading from Plan 12-08. The constraint is still active: the engine's `cache.OnRead` and `cache.InvalidateFile` are wired in `engine.ReadAt` / `engine.Delete` (where the engine has the BlockRef list directly), but `common.ReadFromBlockStore` / `common.WriteToBlockStore` still pass `nil []BlockRef` into the engine because doing otherwise requires changing handler call-site signatures.

Status:

- The seam is fully in place: `common.CacheInvalidator` interface (Plan 12-08), `common.diffRemovedHashes` helper (Plan 12-08), `*engine.Cache` implements `InvalidateFile` (Plan 12-09).
- engine.ReadAt fires cache.OnRead when called WITH non-empty []BlockRef (which only happens via callers that already have the BlockRef snapshot — currently engine_test.go and any future caller that bypasses the common helpers). Production NFS/SMB read paths still pass nil via common.ReadFromBlockStore.
- `common.WriteToBlockStore`'s `cache.InvalidateFile` still fires with the placeholder hash list (the helper has the cache reference but no real []BlockRef diff to invalidate against).

The actual end-to-end threading remains deferred. Most pragmatic path forward: pick this up alongside Plan 13's file-level dedup integration (which itself touches the read/write path and benefits from a real BlockRef threading through common helpers). Alternative: a small dedicated cleanup plan post-Plan-13 that updates the helpers + handler call sites in one go.
