# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-09)

**Core value:** Operator ensures protocol adapters are only externally accessible when running, reducing attack surface and making adapter lifecycle fully dynamic.
**Current focus:** Phase 2 - Adapter Discovery (COMPLETE)

## Current Position

Phase: 2 of 4 (Adapter Discovery)
Plan: 1 of 1 in current phase (COMPLETE)
Status: Phase 2 complete, ready for Phase 3
Last activity: 2026-02-10 -- Completed 02-01-PLAN.md (adapter discovery polling)

Progress: [█████░░░░░] 50%

## Performance Metrics

**Velocity:**
- Total plans completed: 3
- Average duration: 6 min
- Total execution time: 17 min

**By Phase:**

| Phase | Plans | Total | Avg/Plan |
|-------|-------|-------|----------|
| 01-auth-foundation | 2/2 | 13 min | 7 min |
| 02-adapter-discovery | 1/1 | 4 min | 4 min |

**Recent Trend:**
- Last 5 plans: 01-01 (3 min), 01-02 (10 min), 02-01 (4 min)
- Trend: improving

*Updated after each plan completion*

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

- Polling over webhooks (no webhook system exists; polling is simpler)
- One LoadBalancer per adapter (clean separation, independent IPs)
- New "operator" role with least privilege (read-only adapter access)
- source.Channel vs RequeueAfter still to be finalized (research recommends source.Channel)
- RequireRole is fail-closed: zero allowed roles means all requests denied (01-01)
- GET /api/v1/adapters/{type} stays admin-only per least-privilege (01-01)
- Authenticated condition skipped from Ready aggregate when replicas=0 (01-02)
- DittoFSClient self-contained in operator module, no pkg/apiclient import (01-02)
- Admin credentials auto-generated only when user has NOT provided passwordSecretRef (01-02)
- Auth retry count tracked via annotation, persists across operator restarts (01-02)
- Transient errors get backoff; permanent errors propagate to controller-runtime (01-02)
- AdapterInfo uses minimal 4-field subset; Go JSON decoder ignores extra API fields (02-01)
- Polling interval read fresh from CRD spec every reconcile, never cached (02-01)
- Empty adapter list stored as valid state (empty slice, not nil) (02-01)
- Re-fetch DittoServer after auth reconciliation to get updated conditions (02-01)

### Pending Todos

None yet.

### Blockers/Concerns

- Module import path for `pkg/apiclient`: RESOLVED -- operator uses its own DittoFSClient (01-02)
- Adapter API `running` field: CONFIRMED -- exists in AdapterResponse, populated by IsAdapterRunning() (02-01)
- Verify DittoFS supports `DITTOFS_ADMIN_INITIAL_PASSWORD` env var: CONFIRMED -- exists in models/admin.go (01-02)

## Session Continuity

Last session: 2026-02-10
Stopped at: Completed 02-01-PLAN.md (adapter discovery polling) -- Phase 2 complete
Resume file: None
