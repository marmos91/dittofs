# Phase 70: Storage Observability and Quotas - Context

**Gathered:** 2026-03-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Per-share storage quotas with unified NFS/SMB space reporting, accurate usage tracking, and logical vs physical size distinction. Operators can set byte quotas per share, see accurate storage consumption, and clients see quota-adjusted values via `df` (NFS) and Explorer (SMB). Remove all legacy hardcoded filesystem statistics.

</domain>

<decisions>
## Implementation Decisions

### Quota Semantics
- Quota basis: **logical file sizes** (sum of file sizes as seen by clients). Block overhead/dedup invisible to users
- Enforcement: **hard limit, no grace period**. Once quota reached, writes immediately rejected. In-flight writes that started before the limit are allowed to complete
- Default: **optional, unlimited by default**. QuotaBytes=0 means unlimited. Shares have no quota unless explicitly set
- Hot update: quota changes take effect **immediately** on next write check, no restart needed
- Over-quota (quota reduced below current usage): **block new writes only**. Existing data stays, reads/deletes/renames work normally
- ADS (alternate data streams): **count against quota**. ADS are real data stored as children
- Truncate (shrink): **always allowed** even at quota. Helps users recover from full quota
- File/directory creation (zero-byte): **allowed at quota**. Only data writes blocked

### Size Reporting
- NFS `df` total: **quota as total size** when quota is set. Standard behavior matching how NFS quotas work everywhere
- No-quota total: **local-only shares → actual LocalStoreSize capacity; remote-backed shares → 1 PiB constant**
- Physical size: **exposed via API/CLI only** (not in NFS/SMB protocol responses). Operators see physical_bytes alongside logical used_bytes
- UsedSize calculation: **incremental tracking** via atomic counter in metadata store. Updated on every write/truncate/delete. O(1) per FSSTAT call
- SMB vs NFS values: **identical underlying bytes**. SMB divides by cluster size for AllocationUnits but same source data

### Write Rejection
- Enforcement point: **MetadataService.WriteFile** — single shared enforcement point for both NFS and SMB
- Partial writes: **reject entire write** if it would push usage over quota. No partial state
- Error codes: **standard protocol errors only** — NFS3ERR_NOSPC / NFS4ERR_NOSPC / STATUS_DISK_FULL. No extra detail in wire response
- Quota tracking is **abstract and shared** between SMB and NFS. Single implementation in metadata layer, no per-protocol duplication

### CLI/API Design
- Flag: `--quota-bytes` accepting human-readable values (e.g., '10GiB'). Matches existing `--local-store-size` pattern
- `--quota-bytes 0` means **unlimited** (removes quota). Consistent with LocalStoreSize convention
- `dfsctl share list`: add **Quota** and **Used** columns (e.g., '10 GiB' / 'unlimited' and '3.2 GiB')
- `dfsctl share edit`: **add quota-bytes to interactive prompts** alongside local-store-size and read-buffer-size
- API: **extend GET /api/v1/shares** response with `quota_bytes`, `used_bytes`, `physical_bytes`, `usage_percent` fields
- JSON output includes **usage_percent** for monitoring/alerting convenience

### NFS Quotas & Interoperability
- No RQUOTA protocol — out of scope per REQUIREMENTS.md. Quotas via FSSTAT/FSINFO and NFSv4 GETATTR
- NFSv4 GETATTR: **implement quota_avail_hard, quota_avail_soft, quota_used** attributes. quota_avail_soft = same as hard (hard limits only)
- macOS and Linux: **same `df` output** — both query same underlying GetFilesystemStatistics()

### Legacy Cleanup
- **Remove all hardcoded filesystem statistics**: the 1TB defaults in memory metadata store, the hardcoded fallback values in SMB handlers (1M total / 500K available blocks)
- **Replace with quota-aware stats path**: all FSSTAT/FSINFO/FileFsSizeInformation calls go through the unified GetFilesystemStatistics() which is quota-aware
- **No parallel systems**: one stats path, one quota tracking mechanism, one enforcement point

### Tracking Layer
- Used bytes tracked at **metadata store level** — atomic counter in each metadata store instance, updated on Create/Write/Truncate/Remove
- Physical size from **BlockStore.GetStats()** — wire LocalDiskUsed through API/CLI as physical_bytes

### Competitive Patterns (from industry research)
- **min(physical, quota) for space reporting** (Samba model): `GetFilesystemStatistics()` should return `TotalBytes = min(physical_capacity, quota_limit)` and `AvailableBytes = min(physical_free, quota_remaining)`. This handles both physical and administrative limits correctly
- **Incremental atomic counters** (GlusterFS model): Our planned approach matches GlusterFS's marker translator — update usage atomically on every write/truncate/delete. GlusterFS stores counters as xattrs and propagates up directory tree; we use `sync/atomic.Int64` per metadata store instance
- **Counter reconciliation on failure**: GlusterFS has a dedicated scan to fix counter drift after crashes. We need a **startup scan** to initialize (or reconcile) the atomic counter from actual file sizes. FSRM also has Incomplete→Rebuilding→Complete states for this
- **Server-side enforcement is mandatory**: CephFS's cooperative client-side enforcement is unreliable. All competitors that enforce properly do it server-side (Samba VFS, GlusterFS enforcer, FSRM minifilter, NTFS driver)
- **Three-value space model**: Both NFS FSSTAT and SMB FileFsFullSizeInformation distinguish Total/Free/Available. Windows NTFS adds `ActualAvailableAllocationUnits` (physical, ignoring quotas) vs `CallerAvailableAllocationUnits` (quota-aware). Our `FilesystemStatistics` struct maps cleanly
- **Abstract stats interface**: NFS-Ganesha's `get_fs_dynamic_info()` via FSAL is directly analogous to our `GetFilesystemStatistics()` — confirms quota awareness belongs at the metadata store level, not protocol handlers

### Claude's Discretion
- Exact atomic counter implementation details (sync/atomic vs mutex-protected)
- NFSv4 quota attribute bitmap handling
- Migration path for existing shares (initialize counter from file size scan on startup)
- Cluster size constant for SMB allocation unit calculations

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Filesystem Statistics (existing infrastructure)
- `pkg/metadata/types.go` §281-318 — `FilesystemStatistics` struct definition (TotalBytes, UsedBytes, AvailableBytes)
- `pkg/metadata/interface.go` §160-161 — `GetFilesystemStatistics()` interface method
- `pkg/metadata/store/memory/server.go` §100-176 — Memory store implementation (has maxStorageBytes, sums file sizes)

### BlockStore Stats (existing infrastructure)
- `pkg/blockstore/engine/engine.go` §309-402 — Rich `BlockStoreStats` struct and `GetStats()` method
- `pkg/blockstore/store.go` §117-125 — `blockstore.Stats` interface (UsedSize is TODO)
- `pkg/blockstore/engine/engine.go` §274-283 — `Stats()` method with `UsedSize: 0 // TODO`

### NFS FSSTAT/FSINFO handlers
- `internal/adapter/nfs/v3/handlers/fsstat.go` §82-203 — NFSv3 FSSTAT handler (calls GetFilesystemStatistics)
- `internal/adapter/nfs/v3/handlers/fsinfo.go` §104-241 — NFSv3 FSINFO handler (static capabilities)

### SMB FileFsSizeInformation handlers
- `internal/adapter/smb/v2/handlers/query_info.go` §847-903 — FileFsSizeInformation and FileFsFullSizeInformation (calls GetFilesystemStatistics, has hardcoded fallbacks to remove)

### Share model and API
- `pkg/controlplane/models/share.go` §17-124 — Share model (has LocalStoreSize, needs QuotaBytes field)
- `internal/controlplane/api/handlers/shares.go` §64-231 — Share create/update handlers (needs quota fields)
- `cmd/dfsctl/commands/share/create.go` §28-161 — CLI create command (needs --quota-bytes flag)
- `cmd/dfsctl/commands/share/edit.go` §14-313 — CLI edit command (needs quota in interactive flow)

### Per-share BlockStore wiring
- `pkg/controlplane/runtime/shares/service.go` — Share registration, BlockStore creation, GetBlockStoreForHandle

### Requirements
- `.planning/REQUIREMENTS.md` §36-46 — QUOTA-01 through QUOTA-05 and STATS-01 through STATS-03

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `MetadataService.GetFilesystemStatistics()`: Already called by both NFS FSSTAT and SMB FileFsSizeInformation — single modification point for quota-aware reporting
- `engine.BlockStore.GetStats()`: Rich per-share stats (LocalDiskUsed, block counts, remote health) — wire to API for physical_bytes
- `bytesize.Parse()`: Already used for LocalStoreSize/ReadBufferSize — reuse for --quota-bytes
- Share model `LocalStoreSize` field: Precedent for per-share size config with human-readable CLI input

### Established Patterns
- Per-share isolation via `shares.Service` — each share has its own BlockStore and metadata store
- Human-readable byte sizes in CLI/API (e.g., "10GiB") via bytesize package
- Share model fields persisted in GORM, exposed through REST API, managed via dfsctl
- Memory metadata store already calculates UsedBytes by summing file sizes (lines 130-137)

### Integration Points
- `GetFilesystemStatistics()` in each metadata store implementation — add quota awareness here
- `WriteFile()` in MetadataService — add quota check before write
- Share model in `pkg/controlplane/models/share.go` — add QuotaBytes field
- Share API handlers — add quota fields to create/update request/response
- CLI share commands — add --quota-bytes flag and table columns

</code_context>

<specifics>
## Specific Ideas

- Remove all legacy/hardcoded stats — no parallel systems. One path for all filesystem statistics
- Quota tracking must be abstract and shared between protocols. Single enforcement in metadata layer
- Physical size (block storage) exposed for operators via API/CLI but not in protocol responses

</specifics>

<deferred>
## Deferred Ideas

- **RQUOTA protocol (RFC 4559)**: Separate RPC program (100011) for `quota`/`repquota` commands. DittoFS uses per-share quotas (not per-user), so RQUOTA semantics don't map cleanly. FSSTAT + NFSv4 attrs cover `df` which is the primary use case. Defer unless there's demand.
- **Per-user/group quotas**: Explicitly out of scope per REQUIREMENTS.md. AUTH_UNIX is spoofable, massive complexity.
- **Quota alerts/webhooks**: Notify operators when shares approach quota limits. Could be a future observability feature.

</deferred>

---

*Phase: 70-storage-observability-quotas*
*Context gathered: 2026-03-20*
