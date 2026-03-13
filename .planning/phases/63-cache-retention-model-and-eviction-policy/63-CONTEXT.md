# Phase 63: Cache Retention Model and Eviction Policy - Context

**Gathered:** 2026-03-13
**Status:** Ready for planning

<domain>
## Phase Boundary

Per-share cache retention configuration (pin/ttl/lru) with control plane API/CLI and eviction policy enforcement. Operators configure how local blocks are evicted per share. Metrics, quotas, and edge test infrastructure are separate phases.

</domain>

<decisions>
## Implementation Decisions

### TTL Semantics
- TTL tracking is **per-file** (all blocks in a file share one TTL). Accessing any block keeps the entire file cached. Rationale: if a user opens a movie, they expect to re-open it.
- **Any read or write** resets the TTL clock for the file.
- TTL starts from **last access time** (resets on each touch), not from first cache time.
- Eviction check is **on-demand only** — TTL-expired blocks become eviction candidates but are only evicted when `ensureSpace()` runs (disk pressure). No background sweeper.
- **No minimum TTL** — trust the operator to set appropriate values.
- Eviction is **transparent to NFS/SMB clients** — evicted blocks are re-downloaded from S3 on next read, slightly slower but invisible.
- TTL is configurable **per-share only**, not per-file.

### Pin Behavior
- Pinned shares return **ENOSPC** (NFS3ERR_NOSPC / STATUS_DISK_FULL) when disk is full. No cross-share eviction to make room.
- Pin + sync: pinned blocks are **never evicted locally** but **still sync to S3** for durability.
- Pin works **with or without a remote store** (local-only mode allowed).
- CLI/API returns a **warning** (not error) when pinning a share on a disk using >80% capacity.
- Switching pin → lru/ttl: existing blocks **immediately become evictable** under the new policy. No retroactive TTL — they start fresh.
- Switching to pin does **NOT trigger a pre-fetch** of remote blocks. Only prevents future eviction.
- Pin does **not interact with GC** — GC continues to clean orphan blocks from S3 regardless of pin status.
- No per-share disk quota in this phase (deferred to Phase 69).

### Cross-Share Eviction
- Eviction is **per-share only** — each share manages its own disk budget independently. No global disk awareness or cross-share coordination.
- LRU mode uses **true LRU ordering** by file access time (not current "first remote block found" approach). The same per-file last-access timestamp used for TTL works for LRU ordering.
- Pin, TTL, and LRU are **mutually exclusive** — no combining (e.g., no "LRU with TTL floor").
- TTL-expired blocks are evicted **oldest first** (by last access time) among expired blocks.
- All blocks treated **equally regardless of DataSize** — no size-based eviction priority.
- **No eviction metrics** in this phase — deferred to Phase 69 (Storage Observability).

### Defaults and Configuration
- New shares default to **lru** (backward compatible, per CACHE-06).
- **Server-level default** configurable for both mode and TTL duration. New shares inherit server default. Falls back to lru if not set.
- `dfsctl share show` displays retention as **inline fields** (separate Retention and Retention TTL fields).
- `dfsctl share list` includes a **Retention column** in table output.
- Setting TTL mode **requires explicit --retention-ttl** duration — omitting it is an error.
- **NULL/empty retention_policy means lru** — no database migration needed for existing shares.
- Retention policy is **always updatable** at runtime (no confirmation required for changes).
- Server-level default includes both **default_retention_policy** and **default_retention_ttl**.

### Code Architecture
- Retention logic lives in **FSStore eviction layer** (`pkg/blockstore/local/fs/eviction.go`). `ensureSpace()` checks the share's policy before evicting.
- **Simple enum + switch** pattern (RetentionPolicy type with Pin/TTL/LRU constants). No strategy interface.
- Config flows via **Share GORM model fields** (RetentionPolicy, RetentionTTL added to model). Not the JSON Config blob.
- Last-access timestamp updates are **batched** in memory, flushed periodically (matching existing SyncFileBlocks pattern). No synchronous metadata write on every read.
- Policy changes apply **lazily** — next eviction cycle uses the new policy. No immediate eviction scan on change.
- **Dedicated unit tests** for eviction policy logic (pin skip, TTL threshold, LRU ordering).

### Claude's Discretion
- Exact GORM field types and migration approach
- Server settings key names and storage format
- Internal data structures for batched access time tracking
- ListRemoteBlocks query modification for LRU ordering
- CLI flag validation and error message wording
- Warning threshold implementation details (>80% disk check)

</decisions>

<specifics>
## Specific Ideas

- "If I access a Movie, I expect to be able to re-open it" — per-file TTL ensures all blocks of an accessed file stay cached together.
- Edge deployment scenario: movies vanish after 3 days — this is the root problem driving the entire v4.7 milestone. Pin mode and TTL mode both solve this.
- Operators should be trusted (no minimum TTL, no confirmation on policy changes). DittoFS targets enterprise operators who know their workloads.

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/blockstore/local/fs/eviction.go`: `ensureSpace()` and `evictBlock()` — the eviction loop to extend with policy checks
- `pkg/blockstore/local/local.go`: `SetEvictionEnabled(bool)` — already exists as a toggle, pin mode can disable eviction entirely
- `pkg/controlplane/models/share.go`: Share model with GORM fields — add RetentionPolicy and RetentionTTL here
- `pkg/blockstore/local/fs/fs.go`: FSStore struct — add retention config fields and last-access tracking

### Established Patterns
- GORM AutoMigrate: new fields are added via `gorm:"default:..."` tags, NULL handling for backward compat
- `SyncFileBlocks()` periodic flush: existing batched metadata pattern to reuse for access time updates
- `ListRemoteBlocks(ctx, limit)`: current eviction query — needs modification for LRU ordering and TTL filtering
- Server settings: `SettingsStore` with key-value pairs — use for server-level defaults

### Integration Points
- `pkg/controlplane/runtime/shares/service.go`: Share struct — retention config must be passed through here to FSStore
- `cmd/dfsctl/commands/share/`: CLI commands — add --retention and --retention-ttl flags
- `internal/controlplane/api/handlers/`: REST API — share create/update endpoints need retention fields
- `pkg/blockstore/engine/engine.go`: Config struct — pass retention policy to local store

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 63-cache-retention-model-and-eviction-policy*
*Context gathered: 2026-03-13*
