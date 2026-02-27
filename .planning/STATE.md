---
gsd_state_version: 1.0
milestone: v4.2
milestone_name: Windows Compatibility
status: unknown
last_updated: "2026-02-27T17:33:09.426Z"
progress:
  total_phases: 34
  completed_phases: 33
  total_plans: 114
  completed_plans: 114
  percent: 100
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-26)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.6 Windows Compatibility — SMB bug fixes, NT Security Descriptors, conformance testing

## Current Position

Phase: 31 (Windows ACL Support)
Plan: 3 of 3 complete
Status: Complete
Last activity: 2026-02-27 — Phase 31 Plan 03 complete (SD handlers + lsarpc named pipe)

**Progress:** [██████████] 100%

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |

## Performance Metrics

**Velocity:**
- Total plans completed: 110 (19 v1.0 + 42 v2.0 + 25 v3.0 + 22 v3.5 + 2 v3.6-inserted)
- 4 milestones in 26 days
- Average: ~4.2 plans/day

| Phase | Plan | Duration | Tasks | Files |
|-------|------|----------|-------|-------|
| 26 | 01 | 7min | 3 | 34 |
| 26 | 02 | 16min | 3 | 20 |
| 26 | 03 | 25min | 2 | 6 |
| 26 | 04 | 15min | 2 | 17 |
| 26 | 05 | 25min | 3 | 32 |
| 27 | 01 | 6min | 2 | 614 |
| 27 | 03 | 35min | 2 | 6 |
| 28 | 01 | 6min | 2 | 13 |
| 28 | 02 | 8min | 2 | 9 |
| 28 | 03 | 35min | 2 | 6 |
| 28 | 04 | 6min | 2 | 5 |
| 28 | 05 | 12min | 2 | 7 |
| 29 | 01 | 15min | 2 | 24 |
| 29 | 02 | 21min | 2 | 28 |
| 29 | 03 | 12min | 2 | 8 |
| 29 | 04 | 18min | 2 | 17 |
| 29 | 05 | 9min | 2 | 12 |
| 29 | 06 | 10min | 2 | 20 |
| 29 | 07 | 13min | 2 | 11 |
| 29.4 | 01 | 5min | 3 | 3 |
| 30 | 01 | 5min | 2 | 4 |
| 30 | 02 | 6min | 2 | 5 |
| 30 | 03 | 6min | 2 | 4 |
| 30 | 04 | 4min | 2 | 9 |
| Phase 31 P02 | 3min | 2 tasks | 5 files |
| Phase 31 P01 | 8min | 2 tasks | 9 files |
| Phase 31 P03 | 13min | 2 tasks | 5 files |

## Quick Tasks Completed

| # | Description | Branch | PR | Date |
|---|------------|--------|----|------|
| 1 | NFS adapter refactor (issue #148): split 3 oversized files, extract XDR decoder, fix metrics double-decode, add 32 tests | refactor/148-nfs-adapter-cleanup | - | 2026-02-19 |
| 2 | K8s operator: expose NFS portmapper port (Service 111->10111, NetworkPolicy, best-effort enablement) | feat/k8s-nfs-portmapper | #155 | 2026-02-20 |

## Accumulated Context

### Decisions

- v3.5 milestone inserted before v4.0: refactor adapter layer and core before adding NFSv4.2 features
- v3.6 milestone inserted: Windows compatibility (bugs #180/#181/#182 + ACL support + test suite validation)
- v3.7 milestone inserted: benchmarking suite (GitHub #193-#199) — compare DittoFS vs JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba
- v3.8 milestone inserted: SMB3 protocol upgrade (from feat/smb3 branch) — 3.0/3.0.2/3.1.1, encryption, leases, Kerberos, durable handles
- v3.8 Phase 44 added: SMB3 Conformance Testing (Microsoft WPTS + smbtorture + Go integration)
- v4.0 phases renumbered from 26-32.5 to 45-51.5 (after benchmark, SMB3, and conformance testing)
- Test suites chosen: smbtorture (GPL) + Microsoft WindowsProtocolTestSuites (MIT)
- [v3.6] Phase structure: 30 (Bug Fixes), 31 (Windows ACL Support), 32 (Integration Testing)
- [v3.6] Plan counts: Phase 30=2 plans, Phase 31=3 plans, Phase 32=3 plans
- [v3.6] 100% requirement coverage: all 19 requirements mapped to phases 30-32
- [26-01] Combined Task 1+2 into atomic commit (types and consumers must rename together)
- [26-01] Kept AccessMode as int enum for backward compat (bitmask conversion deferred)
- [26-02] SquashMode stays in models/permission.go (shared by NFS adapter and runtime identity mapping)
- [26-02] Runtime Share struct retains NFS fields for fast handler access (populated from adapter config at load)
- [26-02] Router conditionally registers netgroup/identity routes via type assertion
- [26-03] ConflictsWith as method on UnifiedLock rather than standalone function
- [26-03] Break callbacks dispatched outside lock to avoid deadlock
- [26-03] TestLockByParams wrapper added for backward compat with service.go
- [26-04] routingNLMService resolves per-share lock manager from NLM file handles
- [26-04] CheckAndBreakLeases* replaced with TODO(plan-03) placeholders (Plan 03 dependency)
- [26-04] ErrLeaseBreakPending defined locally in SMB handlers (removed from metadata)
- [26-05] Package-level DNS cache (sync.Once) instead of Runtime struct fields
- [26-05] Kept shareChangeCallbacks in Runtime (generic mechanism, not NFS-specific)
- [26-05] NFS handler code stays in internal/controlplane/api/handlers/ (simpler import graph)
- [26-05] pkg/identity dissolved to pkg/adapter/nfs/identity (no import cycles)
- [27-01] Package pool renamed from bufpool to match directory convention (4 consumer call sites updated)
- [27-01] Generic XDR uses package xdr declaration despite living in core/ directory (preserves call sites)
- [27-01] Comments referencing old paths updated alongside import rewrites for consistency
- [27-03] DemuxBackchannelReply placed in internal/adapter/nfs/ (not v4/) to avoid creating new Go package in vendor-mode project
- [27-03] V4/NLM/NSM/Portmap dispatch uses interfaces instead of direct imports to break circular dependency
- [27-03] Auth extraction delegates to middleware package but keeps forwarding function for backward compat
- [27-03] Connection code split keeps NFSConnection struct in pkg/ while sharing RPC framing utilities
- [Phase 28]: [28-01] Auth packages flattened from ntlm/spnego to single auth package (no naming conflicts)
- [Phase 28]: [28-01] Test function names updated to match new type names (TestConnection_ instead of TestSMBConnection_)
- [Phase 28]: BaseAdapter uses pointer embedding (*adapter.BaseAdapter) to avoid go vet sync primitive copy warnings
- [Phase 28]: [28-04] Standalone AUTH_UNIX parser in nfs/auth to avoid RPC package import dependency
- [Phase 28]: [28-04] SMBAuthenticator uses sync.Map for pending auth state tracking across concurrent sessions
- [Phase 28]: [28-04] Unknown UIDs produce synthetic users (unix:UID) for NFS backward compat
- [Phase 28]: [28-03] Created response.go instead of expanding dispatch.go to keep dispatch table separate from response/send logic
- [Phase 28]: [28-03] ConnInfo struct + SessionTracker interface pattern decouples pkg/ Connection from internal/ dispatch without circular imports
- [Phase 28]: [28-03] sessionSigningVerifier moved to internal/adapter/smb/framing.go as NewSessionSigningVerifier for co-location with framing logic
- [Phase 28]: [28-05] Skipped files with adequate existing Godoc, focused edits on under-documented exports (handler types, converter functions, change_notify registry)
- [29-01] MapError stub on BaseAdapter rather than NFS/SMB — both embed BaseAdapter so they inherit the stub
- [29-01] createWithID accepts currentID and idSetter callback rather than interface constraint on models
- [29-01] API client listResources returns []T (value slice), GORM listAll returns []*T (pointer slice) matching existing patterns
- [Phase 29]: [29-02] Split 1361-line manager.go into 8 focused files by responsibility (offloader/upload/download/dedup/queue/entry/types/wal_replay)
- [Phase 29]: [29-02] GC extracted to standalone pkg/payload/gc/ with duplicated parseShareName and MetadataReconciler for zero coupling
- [Phase 29]: [29-03] Flat file split (same package) instead of sub-packages to avoid Go circular imports
- [Phase 29]: [29-03] Operation-based naming: file_create.go, file_modify.go, auth_identity.go, auth_permissions.go
- [Phase 29]: [29-04] GuestUser/IsGuestEnabled folded into UserStore (returns *User, per research)
- [Phase 29]: [29-04] ShareHandler gets custom composite ShareHandlerStore (6 sub-interfaces) since it needs cross-entity queries
- [Phase 29]: [29-04] NetgroupStore and IdentityMappingStore kept outside composite Store (accessed via type assertion)
- [Phase 29]: [29-04] Router unchanged -- Go implicit interface satisfaction narrows full Store to sub-interfaces automatically
- [Phase 29]: [29-05] io sub-package local interfaces (CacheReader, CacheWriter, CacheStateManager, BlockDownloader, BlockUploader) to avoid circular imports
- [Phase 29]: [29-05] Sentinel error bridging via package-level variables set in parent init() for cross-package error detection
- [Phase 29]: [29-05] Conformance test StoreFactory pattern: func(t *testing.T) MetadataStore for store-specific setup
- [Phase 29]: [29-06] Share/ShareConfig types moved to shares/ sub-package with parent type aliases for zero-change consumer migration
- [Phase 29]: [29-06] Sub-services define narrow local interfaces (ShareProvider, MetadataStoreProvider, etc.) to avoid import cycles
- [Phase 29]: [29-06] adapters.RuntimeSetter uses any-typed runtime parameter to break import cycle with parent package
- [Phase 29]: [29-06] Lifecycle.Serve accepts dependency interfaces rather than importing sibling sub-packages
- [Phase 29]: [29-07] IdentityMappingAdapter as separate interface (not embedded in ProtocolAdapter) to avoid breaking all existing adapters
- [Phase 29]: [29-07] MapIdentity default stub on BaseAdapter so NFS/SMB inherit without code changes
- [Phase 29]: [29-07] Kerberos Provider.Authenticate returns Authenticated:false by design (full token validation in protocol-specific layers)
- [Phase 29]: [29-07] HandleStoreError wraps MapStoreError + WriteProblem for one-line handler error responses
- [Phase 29]: [29-07] Converted all groups.go handlers as demonstration; other handlers can adopt incrementally
- [Phase 29.4]: [29.4-01] REF-04.5 signing verification co-located in framing.go (not separate signing.go) -- marked SATISFIED with deviation note
- [Phase 29.4]: [29.4-01] REF-04.6 dispatch split into dispatch.go + response.go -- marked SATISFIED with deviation note
- [Phase 29.4]: [29.4-01] REF-06.6 txutil intent satisfied via storetest conformance suite (no standalone txutil package)
- [Phase 30]: [30-01] Zero-fill at downloadBlock level so both NFS and SMB benefit from single sparse fix
- [Phase 30]: [30-01] Cache miss after successful EnsureAvailable treated as sparse (Go zeroes memory on allocation)
- [Phase 30]: Memory store must persist File.Path for Move path propagation to work
- [Phase 30]: Queue-based BFS (iterative) for descendant path updates to avoid stack overflow on deep trees
- [Phase 30]: [30-03] Used metaSvc.Lookup for '..' resolution (already handles parent via GetParent in store)
- [Phase 30]: [30-03] Used max() builtin (Go 1.21+) for Nlink minimum-1 fallback
- [Phase 30]: [30-03] walkPath test uses runtime.New(nil) to avoid payload service initialization overhead
- [Phase 30]: Fire-and-forget oplock breaks in NFS handlers (per Samba behavior)
- [Phase 30]: [30-04] Best-effort child handle lookup for oplock break in remove/rename (failure does not block operation)
- [Phase 31]: Well-known SIDs use string identifiers (SYSTEM@, ADMINISTRATORS@) that SMB translator converts to binary SIDs
- [Phase 31]: Owner always gets alwaysGrantedMask (admin rights) even when rwx=0
- [Phase 31]: Zero-value ACLSource (empty string) means unknown/legacy for backward compat
- [Phase 31]: Samba-style RID allocation (uid*2+1000, gid*2+1001) prevents user/group SID collisions
- [Phase 31]: Machine SID persisted in SettingsStore under 'machine_sid' key, initialized in lifecycle before adapters
- [Phase 31]: Package-level defaultSIDMapper with SetSIDMapper/GetSIDMapper for handler access (no interface changes needed)
- [Phase 31]: PipeHandler interface with HandleBind/HandleRequest for polymorphic named pipe dispatch
- [Phase 31]: SD field ordering follows Windows convention (SACL, DACL, Owner, Group) for smbtorture compatibility
- [Phase 31]: SACL always empty stub (revision=2, count=0) — real SACL requires metadata store changes

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-02-27
Stopped at: Completed 31-03-PLAN.md (SD handlers + lsarpc named pipe)
Resume file: Continue with Phase 32
