# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-20)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** v3.0 NFSv4.1 Sessions — Phase 17 complete, ready for Phase 18

## Current Position

Phase: 18 of 25 (EXCHANGE_ID Handler)
Plan: 1 of ? in current phase
Status: Ready
Last activity: 2026-02-20 — Completed Phase 17 (Slot Table and Session Data Structures)

Progress: [########################################] 97% (69/70 plans complete)

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |

## Performance Metrics

**Velocity:**
- Total plans completed: 61 (19 v1.0 + 42 v2.0)
- Average duration: ~7 min
- Total execution time: ~7 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | COMPLETE |
| 03-nsm-protocol | 3 | 19 min | 6.3 min | COMPLETE |
| 04-smb-leases | 3 | 29 min | 9.7 min | COMPLETE |
| 05-cross-protocol-integration | 6 | 37 min | 6.2 min | COMPLETE |
| 06-nfsv4-protocol-foundation | 3 | 30 min | 10.0 min | COMPLETE |
| 07-nfsv4-file-operations | 3 | 35 min | 11.7 min | COMPLETE |
| 08-nfsv4-advanced-operations | 3 | 18 min | 6.0 min | COMPLETE |
| 09-state-management | 4 | 33 min | 8.3 min | COMPLETE |
| 10-nfsv4-locking | 3 | 33 min | 11.0 min | COMPLETE |
| 11-delegations | 4 | 41 min | 10.3 min | COMPLETE |
| 12-kerberos-authentication | 5 | 48 min | 9.6 min | COMPLETE |
| 13-nfsv4-acls | 5 | 43 min | 8.6 min | COMPLETE |
| 14-control-plane-v2-0 | 7 | 48 min | 6.9 min | COMPLETE |
| 15-v2-0-testing | 5 | 24 min | 4.8 min | COMPLETE |
| Phase 16 P01 | 7min | 2 tasks | 9 files |
| Phase 16 P02 | 5min | 2 tasks | 12 files |
| Phase 16 P03 | 7min | 2 tasks | 26 files |
| Phase 16 P04 | 6min | 2 tasks | 18 files |
| Phase 16 P05 | 6min | 2 tasks | 4 files |
| Phase 17 P01 | 3min | 2 tasks | 2 files |
| Phase 17 P02 | 2min | 2 tasks | 2 files |

## Quick Tasks Completed

| # | Description | Branch | PR | Date |
|---|------------|--------|----|------|
| 1 | NFS adapter refactor (issue #148): split 3 oversized files, extract XDR decoder, fix metrics double-decode, add 32 tests | refactor/148-nfs-adapter-cleanup | - | 2026-02-19 |
| 2 | K8s operator: expose NFS portmapper port (Service 111->10111, NetworkPolicy, best-effort enablement) | feat/k8s-nfs-portmapper | #155 | 2026-02-20 |

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

- [v3.0 roadmap]: 10 phases derived from 32 requirements, ordered by dependency chain (types -> slot table -> EXCHANGE_ID -> CREATE_SESSION -> SEQUENCE -> connections -> backchannel -> dir delegations)
- [v3.0 roadmap]: SMB Kerberos (SMBKRB-01, SMBKRB-02) placed in Phase 25 (testing) since it reuses shared Kerberos layer from v2.0
- [Phase 16]: SessionId4 encoded as raw 16 bytes (no length prefix) per RFC 4506 fixed-size opaque
- [Phase 16-02]: Response types use status-gated encoding -- if Status != NFS4_OK only status is encoded/decoded
- [Phase 16-03]: LAYOUTCOMMIT uses bool-gated conditional unions for newoffset/time_modify/layout_update fields
- [Phase 16-03]: DeviceId4 encoded as fixed 16 bytes (no length prefix) per RFC 8881 Section 3.3.14
- [Phase 16-04]: CB_NOTIFY entries stored as raw opaque deferring sub-type parsing to Phase 24
- [Phase 16-04]: CB_NOTIFY_DEVICEID uses conditional encoding (Immediate only for CHANGE, not DELETE)
- [Phase 16-05]: v41StubHandler uses typed decoder closures to validate XDR args and prevent stream desync
- [Phase 16-05]: v4.0 ops accessible from v4.1 compounds via fallback to opDispatchTable (per RFC 8881)
- [Phase 17-01]: Per-SlotTable mutex instead of global StateManager.mu for SEQUENCE hot path
- [Phase 17-01]: SequenceValidation is separate type from v4.0 SeqIDValidation (v4.1 seqid wraps through 0)
- [Phase 17-01]: CachedReply stores full XDR bytes for complete replay detection
- [Phase 17-02]: Session struct is independent of StateManager -- registration is Phase 19's job
- [Phase 17-02]: crypto/rand session ID with deterministic fallback (clientID + nanotime)
- [Phase 17-02]: Back channel slot table only allocated when CONN_BACK_CHAN flag is set

### Pending Todos

None.

### Blockers/Concerns

- Phase 20 (SEQUENCE + COMPOUND bifurcation) is highest risk — touches every v4.1 request path
- Phase 22 (backchannel) requires new bidirectional I/O pattern on TCP connections

## Session Continuity

Last session: 2026-02-20
Stopped at: Completed 17-02-PLAN.md (Phase 17 complete)
Resume file: `/gsd:execute-phase 18`
