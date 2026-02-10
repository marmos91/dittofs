---
phase: 04-security-hardening
plan: 02
subsystem: infra
tags: [kubernetes, operator, networkpolicy, security, ingress-control]

# Dependency graph
requires:
  - phase: 04-security-hardening
    provides: "Static adapter field removal, dynamic-only adapter management"
provides:
  - "Per-adapter NetworkPolicy lifecycle management (create/update/delete)"
  - "TCP ingress restricted to active adapter ports only"
  - "NetworkPolicy garbage collection via owner references"
  - "RBAC for networking.k8s.io/networkpolicies"
affects: []

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "NetworkPolicy per running adapter: ingress locked to single TCP port"
    - "Security-critical reconciliation: errors propagated, not best-effort"

key-files:
  created:
    - "k8s/dittofs-operator/internal/controller/networkpolicy_reconciler.go"
    - "k8s/dittofs-operator/internal/controller/networkpolicy_reconciler_test.go"
  modified:
    - "k8s/dittofs-operator/internal/controller/dittoserver_controller.go"
    - "k8s/dittofs-operator/config/rbac/role.yaml"

key-decisions:
  - "NetworkPolicy errors propagated (not best-effort like Services) because they are security-critical"
  - "Same naming convention as adapter Services: <cr>-adapter-<type>"

patterns-established:
  - "Security-critical reconciliation returns errors immediately rather than logging and continuing"

# Metrics
duration: 4min
completed: 2026-02-10
---

# Phase 4 Plan 2: Per-Adapter NetworkPolicy Lifecycle Summary

**Per-adapter NetworkPolicy reconciler restricting TCP ingress to active adapter ports, with create/update/delete lifecycle and 9 test cases**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-10T21:51:37Z
- **Completed:** 2026-02-10T21:55:34Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Implemented NetworkPolicy reconciler following the same pattern as service_reconciler.go
- NetworkPolicies are created for enabled+running adapters, updated on port change, deleted when adapter stops
- Integrated into controller Reconcile loop with error propagation (security-critical)
- Added Owns watch for NetworkPolicy drift detection and RBAC for networking.k8s.io
- 9 comprehensive tests covering nil safety, creation, deletion, port update, multiple adapters, disabled exclusion, static resource safety, and owner references

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement NetworkPolicy reconciler and integrate into controller** - `70b2634` (feat)
2. **Task 2: Add comprehensive tests for NetworkPolicy reconciler** - `9a37310` (test)

## Files Created/Modified
- `k8s/dittofs-operator/internal/controller/networkpolicy_reconciler.go` - Per-adapter NetworkPolicy lifecycle management (create/update/delete)
- `k8s/dittofs-operator/internal/controller/networkpolicy_reconciler_test.go` - 9 test cases covering full reconciler behavior
- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - RBAC marker, import, Reconcile integration, Owns watch
- `k8s/dittofs-operator/config/rbac/role.yaml` - Regenerated with networking.k8s.io permissions

## Decisions Made
- NetworkPolicy errors are propagated (return err) unlike adapter Services (which are best-effort) -- NetworkPolicies are security-critical
- Same naming convention as adapter Services: `<cr>-adapter-<type>` for consistency

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- SECU-03 satisfied: NetworkPolicy per running adapter allowing TCP ingress only on adapter port
- SECU-04 satisfied: NetworkPolicy deleted when adapter stops or is removed
- All operator security hardening complete (Phase 4 done)
- Full test suite passes (all existing + 9 new tests)

## Self-Check: PASSED

All 2 created files exist, all 2 modified files verified, both commit hashes (70b2634, 9a37310) found in git log, SUMMARY.md exists.

---
*Phase: 04-security-hardening*
*Completed: 2026-02-10*
