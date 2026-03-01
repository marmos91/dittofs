---
gsd_state_version: 1.0
milestone: v3.8
milestone_name: SMB3 Protocol Upgrade
status: unknown
last_updated: "2026-02-28T23:38:08.844Z"
progress:
  total_phases: 36
  completed_phases: 35
  total_plans: 120
  completed_plans: 120
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-28)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.8 SMB3 Protocol Upgrade — Phase 34 (SMB3 KDF and Signing)

## Current Position

Phase: 34 of 40 (next: SMB3 KDF and Signing)
Plan: Phase 33 complete (3/3 plans), Phase 34 not yet planned
Status: Phase 33 complete — ready for Phase 34 planning
Last activity: 2026-02-28 — Completed 33-03 (IOCTL dispatch, VALIDATE_NEGOTIATE_INFO, full smbenc migration)

Progress: [##░░░░░░░░] 13%

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |

## Performance Metrics

**Velocity:**
- Total plans completed: 127 (19 v1.0 + 42 v2.0 + 25 v3.0 + 22 v3.5 + 12 v3.6 + 4 inserted + 3 v3.8)
- 5 milestones in 28 days
- Average: ~4.5 plans/day

| Phase | Plan | Duration | Tasks | Files |
|-------|------|----------|-------|-------|
| 33    | 01   | 9min     | 2     | 12    |
| 33    | 02   | 13min    | 2     | 10    |
| 33    | 03   | 45min    | 2     | 29    |

## Accumulated Context

### Decisions

- v3.8: Business logic (leases, durable handles, state) in metadata service layer, not SMB internal package
- v3.8: SMB internal package = protocol encoding/decoding/framing only
- v3.8: Reuse NFSv4 infrastructure (delegations, state management, Kerberos) for SMB3
- v3.8: Shared Kerberos layer for SMB3 via existing RPCSEC_GSS infrastructure
- v3.8: Dependency order — negotiate -> KDF/signing -> encryption -> Kerberos -> leases -> durable handles -> cross-protocol -> testing
- 4 TODO(plan-03) cross-protocol oplock break markers from v3.5 (to resolve in v3.8)
- REF-01.8/REF-01.9 adapter translation layers deferred to v3.8
- 33-01: smbenc uses buffer-based pattern with error accumulation (not streaming io.Reader)
- 33-01: ConnectionCryptoState placed in internal/adapter/smb to avoid circular imports
- 33-01: CryptoState created eagerly for all connections (minimal overhead, simpler code path)
- 33-02: CryptoState interface in handlers/ to break circular imports with smb/ package
- 33-02: Dispatch hooks pattern (before/after per command) for cross-cutting concerns like preauth hash
- 33-02: Server cipher preference: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM
- 33-02: DropConnection on HandlerResult for fatal protocol violations requiring TCP close
- 33-03: Map-based IOCTL dispatch table (IOCTLHandler func type) mirrors command dispatch pattern
- 33-03: VALIDATE_NEGOTIATE_INFO reads all 4 fields from CryptoState, never re-computes
- 33-03: 3.1.1 connections drop TCP on VNEG per MS-SMB2 3.3.5.15.12
- 33-03: All SMB handler binary encoding goes through smbenc codec (ARCH-02 enforced)

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-02-28
Stopped at: Completed 33-03-PLAN.md (Phase 33 complete)
Resume file: None
