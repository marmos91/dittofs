---
gsd_state_version: 1.0
milestone: v4.2
milestone_name: Adapter + Core Refactoring
status: unknown
last_updated: "2026-02-25T20:55:06.556Z"
progress:
  total_phases: 27
  completed_phases: 26
  total_plans: 95
  completed_plans: 94
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-25)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.5 Adapter + Core Refactoring

## Current Position

Phase: 28 - SMB Adapter Restructuring
Current Plan: 4 of 5 (COMPLETE)
Status: Executing Phase 28
Last activity: 2026-02-25 -- Completed 28-04 (Authenticator Interface)

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |

## Performance Metrics

**Velocity:**
- Total plans completed: 94 (19 v1.0 + 42 v2.0 + 25 v3.0 + 8 v3.5)
- 3 milestones in 25 days

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
| 28 | 03 | - | - | - |
| 28 | 04 | 6min | 2 | 5 |

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

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-02-25
Stopped at: Completed 28-04-PLAN.md
Resume file: Continue with 28-05-PLAN.md
