---
phase: 12-kerberos-authentication
plan: 05
subsystem: auth
tags: [kerberos, rpcsec-gss, keytab, hot-reload, prometheus, metrics, lifecycle-test, gokrb5]

# Dependency graph
requires:
  - phase: 12-04
    provides: "krb5i/krb5p security services, SECINFO RPCSEC_GSS pseudo-flavor advertisement"
provides:
  - "Keytab hot-reload with 60s polling and atomic swap (no server restart needed)"
  - "DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL env var overrides"
  - "Prometheus metrics for RPCSEC_GSS operations (dittofs_gss_ prefix)"
  - "Full RPCSEC_GSS lifecycle integration test: INIT -> DATA -> duplicate rejection -> DESTROY -> stale handle"
affects: [12-kerberos-authentication, nfs-observability]

# Tech tracking
tech-stack:
  added: []
  patterns: ["Functional options for GSSProcessor (WithMetrics)", "Polling-based hot-reload for keytab files", "Prometheus metric nil-safety for zero-overhead disabled metrics"]

key-files:
  created:
    - pkg/auth/kerberos/keytab.go
    - pkg/auth/kerberos/keytab_test.go
    - internal/protocol/nfs/rpc/gss/metrics.go
  modified:
    - pkg/auth/kerberos/kerberos.go
    - internal/protocol/nfs/rpc/gss/framework.go
    - internal/protocol/nfs/rpc/gss/framework_test.go

key-decisions:
  - "Polling (60s) over fsnotify for keytab hot-reload - more reliable across platforms for atomically replaced files"
  - "DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL as new env vars (legacy vars also supported)"
  - "Functional option pattern (WithMetrics) for GSSProcessor to avoid breaking existing constructor calls"
  - "GSSMetrics nil-safe methods for zero-overhead when metrics disabled"
  - "Metrics record in handleInit/handleData/handleDestroy at specific failure and success points"

patterns-established:
  - "KeytabManager polling pattern: Stat ModTime comparison, reload on change, keep old on failure"
  - "GSSProcessorOption functional option pattern for extensible configuration"
  - "Prometheus registry isolation in tests via prometheus.NewRegistry()"

# Metrics
duration: 8min
completed: 2026-02-15
---

# Phase 12 Plan 05: Keytab Hot-Reload, Prometheus Metrics, and Lifecycle Integration Test Summary

**Keytab hot-reload with 60s polling, Prometheus metrics for RPCSEC_GSS operations (dittofs_gss_ prefix), and full lifecycle integration test**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-15T13:43:43Z
- **Completed:** 2026-02-15T13:51:43Z
- **Tasks:** 2
- **Files modified:** 6 (3 created, 3 modified)

## Accomplishments
- KeytabManager with 60-second polling for keytab file changes and atomic reload
- resolveKeytabPath/resolveServicePrincipal for DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL env var overrides
- GSSMetrics struct tracking context creations, destructions, active count, auth failures by reason, data requests by service level, and operation duration histograms
- Full RPCSEC_GSS lifecycle integration test: INIT -> DATA (success x2) -> duplicate rejection -> DESTROY -> stale handle error
- 15 new tests (12 keytab + 3 GSS lifecycle/metrics) all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: Keytab Hot-Reload and Environment Variables** - `685ad9d` (feat)
2. **Task 2: GSS Prometheus Metrics and Lifecycle Integration Test** - `28ee8b8` (feat)

## Files Created/Modified
- `pkg/auth/kerberos/keytab.go` - KeytabManager with polling, resolveKeytabPath, resolveServicePrincipal
- `pkg/auth/kerberos/keytab_test.go` - 12 tests for keytab loading, reload, env vars, manager lifecycle
- `pkg/auth/kerberos/kerberos.go` - Provider integrates KeytabManager, uses resolve functions, Close stops manager
- `internal/protocol/nfs/rpc/gss/metrics.go` - GSSMetrics with dittofs_gss_ prefix Prometheus collectors
- `internal/protocol/nfs/rpc/gss/framework.go` - WithMetrics option, metrics recording in INIT/DATA/DESTROY handlers
- `internal/protocol/nfs/rpc/gss/framework_test.go` - Full lifecycle test and metrics integration test

## Decisions Made
- Used 60-second polling over fsnotify for keytab hot-reload because polling is more reliable for keytab files that may be atomically replaced via rename by key management tools (kadmin, k5srvutil).
- Added DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL as the primary env var names while keeping backward compatibility with DITTOFS_KERBEROS_KEYTAB_PATH and DITTOFS_KERBEROS_SERVICE_PRINCIPAL.
- Used functional option pattern (WithMetrics) for GSSProcessor to avoid breaking the 20+ existing constructor call sites.
- All GSSMetrics methods handle nil receiver gracefully, providing zero overhead when metrics are disabled.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- gokrb5 keytab.Entry type is unexported, preventing direct construction in tests. Resolved by using keytab.AddEntry() API instead.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 12 (Kerberos Authentication) is COMPLETE with all 5 plans delivered
- Full RPCSEC_GSS stack: types, context state machine, RPC integration, krb5i/krb5p, keytab hot-reload, metrics, lifecycle tests
- Ready for production Kerberos deployment with KDC configuration

## Self-Check: PASSED

- All 6 key files verified on disk
- Both task commits (685ad9d, 28ee8b8) verified in git log
- All tests pass with race detection
- Full build and vet pass cleanly

---
*Phase: 12-kerberos-authentication*
*Completed: 2026-02-15*
