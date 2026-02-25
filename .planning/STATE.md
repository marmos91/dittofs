# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-25)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.5 Adapter + Core Refactoring

## Current Position

Phase: 27 - NFS Adapter Restructuring
Current Plan: 1 of 4 (COMPLETE)
Status: Executing Phase 27
Last activity: 2026-02-25 -- Completed 27-01 (Directory rename and consolidation)

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |

## Performance Metrics

**Velocity:**
- Total plans completed: 92 (19 v1.0 + 42 v2.0 + 25 v3.0 + 6 v3.5)
- 3 milestones in 25 days

| Phase | Plan | Duration | Tasks | Files |
|-------|------|----------|-------|-------|
| 26 | 01 | 7min | 3 | 34 |
| 26 | 02 | 16min | 3 | 20 |
| 26 | 03 | 25min | 2 | 6 |
| 26 | 04 | 15min | 2 | 17 |
| 26 | 05 | 25min | 3 | 32 |
| 27 | 01 | 6min | 2 | 614 |

## Quick Tasks Completed

| # | Description | Branch | PR | Date |
|---|------------|--------|----|------|
| 1 | NFS adapter refactor (issue #148): split 3 oversized files, extract XDR decoder, fix metrics double-decode, add 32 tests | refactor/148-nfs-adapter-cleanup | - | 2026-02-19 |
| 2 | K8s operator: expose NFS portmapper port (Service 111->10111, NetworkPolicy, best-effort enablement) | feat/k8s-nfs-portmapper | #155 | 2026-02-20 |

## Accumulated Context

### Decisions

- v3.5 milestone inserted before v4.0: refactor adapter layer and core before adding NFSv4.2 features
- v3.6 milestone inserted: Windows compatibility (bugs #180/#181/#182 + ACL support + test suite validation)
- v4.0 phases renumbered from 26-32.5 to 33-39.5
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

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-02-25
Stopped at: Completed 27-01-PLAN.md
Resume file: Continue with 27-02-PLAN.md
