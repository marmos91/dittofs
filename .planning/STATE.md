---
gsd_state_version: 1.0
milestone: v4.0
milestone_name: BlockStore Unification Refactor
status: completed
stopped_at: Completed 48-02-PLAN.md
last_updated: "2026-03-10T15:31:11.071Z"
last_activity: 2026-03-10 — Phase 48 Plan 02 complete
progress:
  total_phases: 22
  completed_phases: 9
  total_plans: 21
  completed_plans: 21
  percent: 100
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-03-09)

**Core value:** Replace confusing layered storage architecture with clean two-tier block store model (Local + Remote) for per-share isolation and maintainability
**Current focus:** Phase 48 - Auto-Deduced Configuration

## Current Position

Phase: 48 of 49 (Auto-Deduced Configuration)
Milestone: v4.0 BlockStore Unification Refactor
Plan: 2 of 2 complete (48-02 done)
Status: Phase 48 complete
Last activity: 2026-03-10 — Phase 48 Plan 02 complete

Progress: [██████████] 100% (136/136 total plans across all milestones)

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |
| v3.8 SMB3 Protocol Upgrade | 33-40.5 | 26 | Mar 1-4, 2026 | 2026-03-04 |

## Performance Metrics

**Velocity:**
- Total plans completed: 146 (across 6 shipped milestones)
- Average: ~4.6 plans/day
- Trend: Stable velocity maintained

**v4.0 Current Milestone:**
- 9 phases defined (41-49)
- 55 requirements mapped
- Phases 41-46 complete
- 21 plans completed (41-01, 41-02, 42-01, 42-02, 43-01, 43-02, 43-03, 44-01, 44-02, 44-03, 45-01, 45-02, 45-03, 45-04, 46-01, 46-02, 46-03, 47-01, 47-02, 48-01, 48-02)

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting v4.0 work:

- **Two-tier block store model**: Clean Local+Remote replaces confusing PayloadService/Cache/DirectWrite layers (Pending v4.0)
- **Per-share block stores**: Different local paths and remote backends per share, replaces global PayloadService (Pending v4.0)
- **BlockStore refactor before NFSv4.2**: Clean storage architecture enables easier feature development (Pending v4.0)
- **Kept numeric values unchanged (0-3)**: Avoids data migration for persisted FileBlock data (Phase 41, Plan 01)
- **Log messages updated to sync terminology now**: Method/file renames deferred to Phase 45 (Phase 41, Plan 01)
- **Block index sorting in Go**: Numeric sort after DB fetch for correct multi-digit ordering (Phase 41, Plan 02)
- **BadgerDB fb-file: index always maintained**: On every PutFileBlock regardless of state (Phase 41, Plan 02)
- **Single table with Kind discriminator for block stores**: Not separate tables -- simpler queries, matches MetadataStoreConfig pattern (Phase 44, Plan 01)
- **RemoteBlockStoreID as *string pointer**: GORM nullable FK with pointer type for optional remote references (Phase 44, Plan 01)
- **Two-phase migration strategy**: Pre-AutoMigrate for table rename, post-AutoMigrate for data migration (Phase 44, Plan 01)
- **API route /store/block/{kind}**: Kind-aware CRUD replaces /payload-stores (Phase 44, Plan 01)
- **Type/kind validation on block store create**: Local accepts fs,memory; remote accepts s3,memory (Phase 44, Plan 02)
- **Unified /api/v1/store/ route prefix**: Metadata at /store/metadata, blocks at /store/block/{kind} (Phase 44, Plan 02)
- **Share create uses name-based fields**: local_block_store/remote_block_store accept names, resolved to IDs server-side (Phase 44, Plan 02)
- **Local block store defaults to fs type**: Most common use case for local storage (Phase 44, Plan 03)
- **Share create --local required via cobra**: MarkFlagRequired enforces local block store at CLI level (Phase 44, Plan 03)
- **Share edit supports --local/--remote flags**: Enables store migration via share update (Phase 44, Plan 03)
- **Type aliases for backward-compatible extraction**: metadata/object.go uses Go type aliases (type X = Y) to re-export blockstore types without breaking consumers (Phase 45, Plan 01)
- **blockstore as leaf dependency**: pkg/blockstore has zero imports from pkg/metadata, preventing circular dependencies (Phase 45, Plan 01)
- **Conformance suite delegation**: metadata/storetest delegates FileBlockOps to blockstore/storetest via factory adapter (Phase 45, Plan 01)
- **gosync alias for sync package**: Go's standard sync must be aliased as gosync in pkg/blockstore/sync/ due to package name collision (Phase 45, Plan 02)
- **Tests use fs.FSStore not cache.BlockCache**: Old cache.BlockCache doesn't implement new local.LocalStore interface -- test helpers use fs.New() (Phase 45, Plan 02)
- **testEnv.cache uses interface type**: local.LocalStore interface type for test portability between fs and memory implementations (Phase 45, Plan 02)
- **engine sub-package for BlockStore orchestrator**: Import cycles prevent placing orchestrator in blockstore root; engine/ sub-package breaks the cycle (Phase 45, Plan 03)
- **string() conversion at adapter boundaries**: BlockStore methods use plain string; adapters convert metadata.PayloadID at call sites (Phase 45, Plan 03)
- **Deprecated aliases for backward compat**: OffloaderConfig=SyncerConfig, GetPayloadService/GetBlockService/EnsurePayloadService kept as wrappers (Phase 45, Plan 03)
- **Removed all deprecated payload aliases**: GetPayloadService, GetBlockService, EnsurePayloadService, SetOffloaderConfig, OffloaderConfig all removed (Phase 45, Plan 04)
- **PayloadServiceEnsurer renamed to BlockStoreEnsurer**: Interface and all method signatures updated to blockstore terminology (Phase 45, Plan 04)
- **pkg/cache and pkg/payload deleted**: 42 files (10,715 lines) of dead code removed after full consumer migration (Phase 45, Plan 04)
- [Phase 46]: nonClosingRemote wrapper prevents engine.Close() from closing shared remote stores; ref counting in shares.Service manages actual close
- [Phase 46]: BlockStoreConfigProvider narrow interface avoids importing full store.Store into shares package
- [Phase 46-02]: Validation ordering matters -- validate request parameters before per-handle resolution to get correct NFS error codes
- [Phase 46-02]: Health endpoint changed from single block_store to per-share block_stores array (breaking API change)
- [Phase 46]: Removed CreateRemoteStoreFromConfig from init.go since shares.Service has its own copy and EnsureBlockStore was the only caller
- [Phase 46]: Removed CacheConfig/SyncerConfig/SetCacheConfig/SetSyncerConfig -- per-share defaults via SetLocalStoreDefaults/SetSyncerDefaults are the canonical path
- [Phase 47-01]: ReadCache uses RWMutex with RLock for reads, WLock for mutations (matches existing BlockCache pattern)
- [Phase 47-01]: Copy-on-read: Get copies into caller buffer, never returns internal slice
- [Phase 47-01]: Adaptive prefetch depth 1->2->4->8 capped at maxPrefetchDepth=8 (Linux readahead pattern)
- [Phase 47-01]: Dependency-injected LoadBlockFn avoids import cycles with engine package
- [Phase 47-01]: NewPrefetcher returns nil if cache is nil (can't prefetch without cache target)
- [Phase 47-02]: Prefetcher created in Start() not New() to avoid chicken-and-egg with loadBlock closure
- [Phase 47-02]: L1 only used for primary reads (no COW source) to avoid caching stale data
- [Phase 47-02]: Auto-promote reads from local store after flush (OS page cache makes this free I/O)
- [Phase 47-02]: ReadCacheBytes in LocalStoreDefaults, PrefetchWorkers in SyncerDefaults (follows existing pattern)
- [Phase 47-02]: Default ReadCacheSize=128MB, PrefetchWorkers=4 for good out-of-box performance
- [Phase 48-01]: SystemDetector interface in pkg/blockstore mirrors sysinfo.Detector to avoid internal/ import from pkg/
- [Phase 48-01]: PrefetchWorkers fixed at 4, not CPU-scaled, per research recommendation
- [Phase 48-01]: formatBytes helper local to each package (sysinfo and blockstore) to avoid cross-package dependencies
- [Phase 48]: SystemDetector interface in pkg/blockstore mirrors sysinfo.Detector to avoid internal/ import from pkg/
- [Phase 48-02]: CacheConfig/OffloaderConfig removed as breaking change (acceptable for experimental project)
- [Phase 48-02]: SyncerDefaults zero-value fields use buildSyncerConfigFromDefaults internal defaults
- [Phase 48-02]: Config template replaced cache/offloader with auto-deduction comment block

### Pending Todos

None.

### Blockers/Concerns

None yet.

## Session Continuity

Last session: 2026-03-10T14:40:19Z
Stopped at: Completed 48-02-PLAN.md
Resume file: None
Next action: Phase 48 complete
