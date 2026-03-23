---
gsd_state_version: 1.0
milestone: v0.10.0
milestone_name: Production Hardening + SMB Protocol Fixes
status: Phase complete — ready for verification
stopped_at: Completed 71-02-PLAN.md
last_updated: "2026-03-22T21:41:08.952Z"
progress:
  total_phases: 4
  completed_phases: 3
  total_plans: 10
  completed_plans: 9
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-03-20)

**Core value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility
**Current focus:** Phase 71 — operational-visibility

## Current Position

Phase: 71 (operational-visibility) — EXECUTING
Plan: 2 of 2

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |
| v3.8 SMB3 Protocol Upgrade | 33-40.5 | 26 | Mar 1-4, 2026 | 2026-03-04 |
| v4.2 Benchmarking & Performance | 57-62 | -- | Mar 4, 2026 | 2026-03-04 |
| v4.0 BlockStore Unification | 41-49 | 24 | Mar 9-11, 2026 | 2026-03-11 |
| v4.3 Protocol Gap Fixes | 49.1-49.3 | 1 | Mar 12-13, 2026 | 2026-03-13 |
| v4.7 Offline/Edge Resilience | 63-68 | 10 | Mar 15-20, 2026 | 2026-03-20 |

## Accumulated Context

### Decisions

All decisions archived in PROJECT.md Key Decisions table.

- **70-03**: Interface assertions for GetUsedBytes/SetQuotaForShare to decouple from concrete store types
- **70-03**: QuotaBytes=0 displayed as 'unlimited' in CLI, empty string in API JSON
- **70-03**: UsagePercent capped at 100 even when over-quota
- **70-01**: Track only regular file sizes (directories, symlinks, devices excluded from counter)
- **70-01**: Delta tracking at transaction layer (PutFile/DeleteFile) for consistency across all stores
- **70-01**: Badger GetFilesystemStatistics still scans for file count but reads bytes from atomic counter
- **69-02**: Used absolute low/high watermark tracking for sequence window bitmap (avoids corruption during compaction)
- **69-02**: NEGOTIATE exempt only when SessionID=0 (pre-auth semantics)
- **69-03**: SequenceWindow Grant deferred until after successful wire write
- **69-03**: Compound credit validation only for first command per MS-SMB2 3.2.4.1.4
- **69-03**: SupportsMultiCredit set via NEGOTIATE after-hook based on dialect >= 0x0210
- [Phase 69-01]: Cherry-picked PR #288 for signing enforcement instead of re-implementing
- [Phase 69-01]: MS-SMB2 spec section references as code comments for long-term audit trail
- [Phase 70-03]: Interface assertions for GetUsedBytes/SetQuotaForShare to decouple from concrete store types
- [Phase 70-02]: Quota enforcement at PrepareWrite layer (after file type check, before permission check) for early rejection
- [Phase 70-02]: Quota overlay in MetadataService.GetFilesystemStatistics rather than per-store
- [Phase 70-02]: 1 PiB (1<<50) as unlimited sentinel across all stores (was 1TB in memory/badger)
- [Phase 71]: Default TTL 5 min for stale client cleanup, sweep interval TTL/2
- [Phase 71]: Deep copy Shares slice and protocol detail structs for copy-on-read safety
- [Phase 71]: Local clientDisconnecter interface in adapters package to avoid import cycle
- [Phase 71]: ForceCloseByAddress leverages existing ActiveConnections sync.Map
- [Phase 71]: NFS-specific session handlers split to nfs_clients.go, kept under /adapters/nfs/

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-03-22T21:41:08.949Z
Stopped at: Completed 71-02-PLAN.md
Next action: Phase 70 complete. All 3 plans executed successfully.
