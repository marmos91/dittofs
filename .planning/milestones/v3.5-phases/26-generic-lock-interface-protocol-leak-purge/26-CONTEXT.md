# Phase 26: Generic Lock Interface & Protocol Leak Purge - Context

**Gathered:** 2026-02-25
**Status:** Ready for planning

<domain>
## Phase Boundary

Unify the lock model to be protocol-agnostic (rename types, centralize conflict detection) and purge NFS/SMB-specific types from generic layers (pkg/metadata/, pkg/controlplane/). No new capabilities — this is a code organization and abstraction phase.

</domain>

<decisions>
## Implementation Decisions

### Type Naming & Structure
- `EnhancedLock` renamed to `UnifiedLock` — composed struct embedding `OpLock`, `AccessMode`, and `ByteRangeLock` as separate sub-types (not flat)
- `LeaseInfo` renamed to `OpLock` — truly opaque, no protocol field. Adapters register break callbacks; LockManager never knows the protocol
- `ShareReservation` renamed to `AccessMode` — bitmask implementation with `ACCESS_READ`, `ACCESS_WRITE`, `DENY_READ`, `DENY_WRITE`, `DENY_DELETE`
- OpLock levels: union of both protocols — `None`, `Read`, `Write`, `ReadWrite`, `Handle`, `ReadHandle`, `ReadWriteHandle`
- Conflict detection via `lock.ConflictsWith(other)` method on UnifiedLock (not a standalone detector)
- Break notifications via typed callbacks: `OnOpLockBreak()`, `OnByteRangeRevoke()`, `OnAccessConflict()` — compile-safe, adapter implements only what it uses
- All types stay in `pkg/metadata/lock/` (not elevated to top-level pkg/lock/)

### LockManager Interface
- Single `LockManager` interface (not split into sub-interfaces) — lean, both adapters use all lock types
- GracePeriodManager included as part of LockManager interface (grace periods are tied to lock recovery)
- NFSv4 state registers grace period needs through LockManager
- Adapters receive LockManager through Runtime (keep `SetRuntime()` for now, Phase 29 will decompose)
- Explore separate LockManager injection path as alternative to MetadataService getter; fall back to getter if simpler
- NFS lock handlers (LOCK/LOCKT/LOCKU) call LockManager directly — no adapter-local coordinator

### SMB Adapter Integration
- SMB adapter uses generic UnifiedLock interface only — no SMB-specific lease wrapper
- All SMB lease methods (CheckAndBreakLeases*, ReclaimLeaseSMB, OplockChecker) removed from MetadataService
- NLM methods moved from MetadataService to NFS adapter with simplified signatures (not just moved as-is)

### Protocol Boundary
- Generic identity layer: DittoFS users + UID/GID + name resolution stays in metadata
- Protocol-specific transforms: SquashMode/Netgroup to NFS adapter, SID mapping to SMB adapter
- `pkg/identity/` dissolved entirely — generic parts merge into metadata, NFS parts to NFS adapter
- Mount tracking: unified "mounts" concept across both protocols
  - `/api/mounts` — aggregate view across all protocols
  - `/api/adapters/nfs/mounts` — NFS-specific details
  - `/api/adapters/smb/mounts` — SMB sessions as "mounts"

### Share Model Config
- NFS/SMB-specific fields removed from Share model, moved to separate `share_adapter_configs` table
- Table: `share_id`, `adapter_type` (string), `config` (typed JSON)
- Each adapter registers typed config schema — validated at API level
- API pattern:
  - `/api/adapters/nfs/settings` — global NFS defaults
  - `/api/shares/{id}/adapters/nfs/settings` — per-share NFS overrides
- Layered config: adapter defaults apply unless share has explicit override
- Share creation uses adapter defaults automatically — separate `dfsctl share adapter-config set` for overrides
- GORM auto-migration for schema changes
- Old protocol-specific columns dropped from Share table (clean break, no deprecation)

### Migration Approach
- Clean API break — v3.5 dfsctl only works with v3.5+ servers, no backward-compat code
- K8s operator updated alongside each phase (not deferred)
- Auto-migrate DB on startup by default, `--no-auto-migrate` flag for manual control
- config.yaml unchanged — all adapter/share settings managed through API
- Lock test suites rewritten against new LockManager interface (not just renamed)
- Full E2E test suite run as validation after refactoring

### Claude's Discretion
- Config validation: API-level vs both API+startup (defense in depth assessment)
- Cross-protocol conflict test coverage: Phase 26 vs defer to Phase 29 (risk-based decision)
- Exact LockManager injection path: separate injection vs MetadataService getter (based on code analysis)

</decisions>

<specifics>
## Specific Ideas

- Break callbacks should be typed for security and debuggability — the user prioritizes code safety and readability
- OpLock must be truly opaque — no protocol information leaks into the generic layer
- AccessMode as bitmask maps naturally to both NFS OPEN share_access/share_deny and SMB FILE_SHARE flags
- API URL design explicitly specified: `/api/adapters/{type}/settings` and `/api/shares/{id}/adapters/{type}/settings`

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 26-generic-lock-interface-protocol-leak-purge*
*Context gathered: 2026-02-25*
