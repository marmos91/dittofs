# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-25)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.5 Adapter + Core Refactoring

## Current Position

Phase: Starting milestone v3.5 (Phase 26 next)
Status: Milestone planning complete, ready for /gsd:discuss-phase
Last activity: 2026-02-25 -- Planned v3.5 and v3.6 milestones

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |

## Performance Metrics

**Velocity:**
- Total plans completed: 86 (19 v1.0 + 42 v2.0 + 25 v3.0)
- 3 milestones in 25 days

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

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-02-25
Stopped at: Completed milestone planning for v3.5 and v3.6
Resume file: Ready for /gsd:discuss-phase on Phase 26
