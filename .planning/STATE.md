---
gsd_state_version: 1.0
milestone: v0.15.0
milestone_name: milestone
status: ready-to-plan
stopped_at: Phase 11 (A2) shipped via PR #453 (squash 2b96c965). Phase 12 (A3) ready to plan.
last_updated: "2026-04-26T18:30:00.000Z"
last_activity: 2026-04-26
progress:
  total_phases: 8
  completed_phases: 5
  total_plans: 52
  completed_plans: 52
  percent: 62.5
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-04-23)

**Core value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility
**Current focus:** Phase 12 — CDC read path + metadata schema + engine API (A3)

## Current Position

Milestone: v0.15.0
Phase: 12 (cdc-read-path-metadata-engine-api-a3) — READY TO PLAN
Branch: `gsd/phase-12-cdc-read-path-metadata-engine-api`
Status: Phase 11 (A2) shipped; Phase 12 (A3) unblocked
Last activity: 2026-04-26

## Next Actionable

Phase 12 (A3): CDC read path + metadata schema + engine API. 14 requirements across META-01/03/04, API-01/02/03/04, CACHE-01..06, INV-02. Estimated ~2 weeks. Dependencies satisfied: Phase 11 (A2, #422, shipped PR #453) + Phase 09 (ADAPT, #427, shipped PR #438).

- `/gsd-discuss-phase 12 --chain` — interactive discuss → auto plan + execute
- `/gsd-discuss-phase 12 --auto` — fully autonomous (Claude picks defaults)
- `/gsd-plan-phase 12` — skip discuss, go straight to planning
- GH issue: #423

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
| v0.10.0 Production Hardening + SMB fixes | 69-73.1 | — | Mar 20-25, 2026 | in flight |
| v0.13.0 Metadata Backup & Restore | 1-7 | 38 | Apr 2026 | phases complete; not released |

## Accumulated Context

### v0.15.0 Progress

| Phase | Name | Status | PR |
|-------|------|--------|-----|
| 08 | Pre-refactor cleanup (A0) | shipped | #437 |
| 09 | Adapter layer cleanup (ADAPT) | shipped | #438 |
| 10 | FastCDC chunker + hybrid local store (A1) | shipped | #443 |
| 11 | CAS write path + GC rewrite (A2) | shipped | #453 (squash 2b96c965, merged 2026-04-26) |
| 12 | CDC read path + metadata schema + engine API (A3) | **ready to plan** | #423 (issue) |
| 13 | Merkle root + file-level dedup (A4) | blocked by 12 | #424 |
| 14 | Migration tool (A5) | blocked by 13 | #425 |
| 15 | Legacy cleanup (A6) | deferred until A5 in production | #426 |

### v0.15.0 Decisions

- Phase numbering continues from v0.13.0 last phase (7) → v0.15.0 starts at 8 and runs 08-15 (8 phases total)
- Phase directories under `.planning/phases/01-*` through `07-*` (v0.13.0) remain for historical reference; v0.15.0 phase dirs will be `08-*` through `15-*`
- v0.13.0 archive lives at `.planning/milestones/v0.13.0-archive/`
- Fine granularity (from config.json) — 8 phases preserving natural plan boundaries: A0, ADAPT, A1–A6
- Two parallel pre-cleanup tracks (A0 / ADAPT) converge at A3 (engine API change consumes ADAPT groundwork)
- Block key scheme: content-addressable `cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}` with BLAKE3 (via `lukechampine.com/blake3`; D-08 amended 2026-04-24 — swapped from `zeebo/blake3`, user-approved)
- D-41 gate is platform-aware (amended 2026-04-24, user-approved Option A): amd64 requires BLAKE3 ≥ 3.0× SHA-256; arm64 requires ≥ 1.0× (hw-SHA vs portable-Go BLAKE3 asymmetry). Hard 3× target validated on CI amd64 perf lane per D-43.
- Chunking: in-house FastCDC (~200 LoC), min=1MB / avg=4MB / max=16MB, normalization level 2
- Dedup scope: global per metadata store (RefCount spans shares when remote config shared)
- Merkle-root `FileAttr.ObjectID` is lazy (computed at file quiesce), not eager — revisit if dedup hit rate demands eager update
- Migration via `dfsctl blockstore migrate --share <name>`; dual-read shim lives A2–A5; removed in A6 after production rollout confirmed
- v0.13.0 backup backward compatibility NOT required (v0.13.0 never released) — backup code paths are free to break across phases
- Performance regression tolerance: ≤6% on random write (≥600 IOPS), random read (≥1350), sequential write (≥48 MB/s), sequential read (≥60 MB/s)
- A6 (Phase 15) intentionally deferred until A5 (Phase 14) rollout confirmed in production

### v0.13.0 Decisions (archived context)

Historical v0.13.0 decisions preserved in `.planning/milestones/v0.13.0-archive/` for reference; the v0.15.0 refactor deletes `BackupHoldProvider` + `FinalizationCallback` (v0.13.0 scaffolding) in Phase 08.

### Pending Todos

- Phase 12 (A3) follow-ups carried from Phase 11 review:
  - DEFERRED IN-4-03: async GC + 202+poll REST surface (long-running mark-sweep would otherwise time out reverse proxies)
  - DEFERRED WR-3-02 wiring: `gc.interval` periodic-ticker not yet wired into Runtime startup; currently warn-only if set in config
  - DEFERRED WR-4-01 follow-up: dedup short-circuit still leaks donor refcount; needs design decision (mirror increment to `fb` vs. drop short-circuit vs. point `fb` at `existing.ID` with ID-mapping)
- Before Phase 14 (A5) ship: benchmark VM-fleet dedup fixture achieves ≥40% reduction (VER-03 gate)
- Before Phase 15 (A6) merge: confirm `dfsctl blockstore migrate status` reports 100% for every production share

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-04-26T18:30:00.000Z
Stopped at: Phase 11 (A2) shipped via PR #453 (squash 2b96c965, merged 2026-04-26). Branched to `gsd/phase-12-cdc-read-path-metadata-engine-api`, ready to plan.
Next action: `/gsd-discuss-phase 12 --chain` (interactive discuss → auto plan + execute) OR `/gsd-plan-phase 12` (skip discuss)

**Shipped Phase:** 11 (cas-write-path-gc-rewrite-a2) — 9 plans + ~30 review/fix commits — 2026-04-26T18:03:03Z (PR #453)
**Planned Phase:** 12 (cdc-read-path-metadata-engine-api-a3) — TBD plans — branch: `gsd/phase-12-cdc-read-path-metadata-engine-api`
