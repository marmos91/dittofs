# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-09)

**Core value:** Operator ensures protocol adapters are only externally accessible when running, reducing attack surface and making adapter lifecycle fully dynamic.
**Current focus:** Phase 4 - Security Hardening

## Current Position

Phase: 4 of 4 (Security Hardening)
Plan: 2 of 2 in current phase (04-02 COMPLETE)
Status: All Phases Complete
Last activity: 2026-02-10 -- Completed 04-02-PLAN.md (per-adapter NetworkPolicy lifecycle)

Progress: [██████████] 100%

## Performance Metrics

**Velocity:**
- Total plans completed: 7
- Average duration: 5 min
- Total execution time: 34 min

**By Phase:**

| Phase | Plans | Total | Avg/Plan |
|-------|-------|-------|----------|
| 01-auth-foundation | 2/2 | 13 min | 7 min |
| 02-adapter-discovery | 1/1 | 4 min | 4 min |
| 03-dynamic-services-ports | 2/2 | 8 min | 4 min |
| 04-security-hardening | 2/2 | 9 min | 5 min |

**Recent Trend:**
- Last 5 plans: 02-01 (4 min), 03-01 (4 min), 03-02 (4 min), 04-01 (5 min), 04-02 (4 min)
- Trend: stable/fast

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
- Adapter Services use dittofs.io/adapter-service=true label for isolation from static Services (03-01)
- Default adapter Service type is LoadBalancer, configurable via CRD spec.adapterServices.type (03-01)
- DISC-03 safety preserved: skip service reconciliation when no successful poll (nil adapters) (03-01)
- Adapter Service reconciliation is best-effort: errors logged but don't block reconciliation (03-01)
- Dynamic container ports use adapter-{type} prefix to avoid collision with static port names (03-02)
- Static and dynamic ports coexist in Phase 3; Phase 4 removes static ones (03-02)
- Container port comparison before update prevents unnecessary StatefulSet rolling restarts (03-02)
- Headless Service port changed from NFS to API -- API always available, sufficient for StatefulSet DNS (04-01)
- SMB test cases removed entirely since SMB types deleted -- adapter testing via dynamic service reconciler (04-01)
- NetworkPolicy errors propagated (not best-effort) because they are security-critical (04-02)
- Same naming convention for NetworkPolicies as adapter Services: <cr>-adapter-<type> (04-02)

### Pending Todos

None yet.

### Blockers/Concerns

- Module import path for `pkg/apiclient`: RESOLVED -- operator uses its own DittoFSClient (01-02)
- Adapter API `running` field: CONFIRMED -- exists in AdapterResponse, populated by IsAdapterRunning() (02-01)
- Verify DittoFS supports `DITTOFS_ADMIN_INITIAL_PASSWORD` env var: CONFIRMED -- exists in models/admin.go (01-02)

## Session Continuity

Last session: 2026-02-10
Stopped at: Completed 04-02-PLAN.md (per-adapter NetworkPolicy lifecycle) -- all phases complete
Resume file: None
