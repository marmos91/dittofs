---
phase: 01-auth-foundation
plan: 02
subsystem: auth
tags: [k8s-operator, service-account, jwt, token-refresh, secrets, reconciler]

# Dependency graph
requires:
  - phase: 01-auth-foundation/01
    provides: "RoleOperator constant, RequireRole middleware, split adapter routes"
provides:
  - ConditionAuthenticated condition type
  - Operator credentials Secret auto-provisioning and lifecycle
  - Admin credentials Secret auto-generation with DITTOFS_ADMIN_INITIAL_PASSWORD injection
  - DittoFSClient (minimal HTTP client for Login, CreateUser, RefreshToken, DeleteUser)
  - Auth reconciliation integrated into main Reconcile loop
  - Token refresh at 80% TTL via RequeueAfter
  - Exponential backoff (2s-5min) for API unavailability
  - Best-effort service account cleanup on CR deletion
affects: [02-adapter-lifecycle, k8s-operator-adapter-polling]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Auth reconciler: sub-reconciler method pattern on DittoServerReconciler"
    - "Credential Secret lifecycle: auto-provision, refresh, cleanup via ownerReference"
    - "Transient error detection: net.Error + pattern matching for backoff vs permanent failure"
    - "Annotation-based retry tracking: authRetryAnnotation for exponential backoff state"

key-files:
  created:
    - k8s/dittofs-operator/internal/controller/auth_reconciler.go
    - k8s/dittofs-operator/internal/controller/dittofs_client.go
    - k8s/dittofs-operator/internal/controller/auth_reconciler_test.go
  modified:
    - k8s/dittofs-operator/utils/conditions/conditions.go
    - k8s/dittofs-operator/api/v1alpha1/helpers.go
    - k8s/dittofs-operator/internal/controller/dittoserver_controller.go
    - k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go

key-decisions:
  - "Authenticated condition skipped from Ready aggregate when replicas=0 (Stopped server has no API to authenticate against)"
  - "DittoFSClient is self-contained in operator module (no pkg/apiclient import to avoid dependency tree coupling)"
  - "Admin credentials auto-generated only when user has NOT provided spec.identity.admin.passwordSecretRef"
  - "Auth retry count tracked via annotation dittofs.dittofs.com/auth-retry-count (persists across reconciler restarts)"
  - "Transient errors (net.Error, connection refused) get backoff; permanent errors (wrong credentials) propagate to controller-runtime"

patterns-established:
  - "Sub-reconciler pattern: auth methods on DittoServerReconciler called from main Reconcile loop"
  - "Condition-based status: setAuthCondition() does independent Status().Update() to avoid race with main updateStatus"
  - "Best-effort cleanup: cleanupOperatorServiceAccount always returns nil, logs failures"
  - "Secret lifecycle: CreateOrUpdate for admin Secret, Create with AlreadyExists fallback for operator Secret"

# Metrics
duration: 10min
completed: 2026-02-10
---

# Phase 1 Plan 2: Operator Auth Reconciler Summary

**Operator service account auto-provisioning with credential Secret lifecycle, 80% TTL token refresh, exponential backoff for API unavailability, and Authenticated condition in Ready aggregate**

## Performance

- **Duration:** 10 min
- **Started:** 2026-02-10T19:58:58Z
- **Completed:** 2026-02-10T20:08:28Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Implemented full auth reconciliation lifecycle: provision operator service account, store credentials in K8s Secret, refresh tokens proactively at 80% TTL, handle API unavailability with exponential backoff (2s-5min cap)
- Created self-contained DittoFSClient within operator module providing Login, CreateUser, RefreshToken, DeleteUser without importing pkg/apiclient
- Auto-generate admin credentials Secret and inject DITTOFS_ADMIN_INITIAL_PASSWORD env var into DittoFS pod for bootstrap
- Added ConditionAuthenticated to Ready aggregate (operator not-ready until authenticated, skipped when replicas=0)
- Best-effort operator service account deletion on CR cleanup
- 10 unit tests covering all auth flows: provision, conflict, refresh, re-login fallback, API unreachable, cleanup, admin credential generation, backoff computation

## Task Commits

Each task was committed atomically:

1. **Task 1: Add infrastructure - conditions, helpers, DittoFS API client** - `202e9b4` (feat)
2. **Task 2: Implement auth reconciler and integrate into controller lifecycle** - `d314ad2` (feat)

## Files Created/Modified
- `k8s/dittofs-operator/utils/conditions/conditions.go` - Added ConditionAuthenticated constant
- `k8s/dittofs-operator/api/v1alpha1/helpers.go` - Added operator/admin Secret name helpers and GetAPIServiceURL()
- `k8s/dittofs-operator/internal/controller/dittofs_client.go` - Minimal DittoFS HTTP client (Login, CreateUser, RefreshToken, DeleteUser)
- `k8s/dittofs-operator/internal/controller/auth_reconciler.go` - Full auth lifecycle: reconcileAdminCredentials, reconcileAuth, provisionOperatorAccount, refreshOperatorToken, cleanupOperatorServiceAccount, computeBackoff
- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - Integrated admin credentials, auth reconciliation, DITTOFS_ADMIN_INITIAL_PASSWORD env var, Authenticated in Ready aggregate, Owns(&Secret{})
- `k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go` - Updated "ready StatefulSet" test for Authenticated condition
- `k8s/dittofs-operator/internal/controller/auth_reconciler_test.go` - 10 test cases for auth reconciliation

## Decisions Made
- Authenticated condition is only required in Ready aggregate when replicas > 0 (Stopped servers have no API to authenticate against)
- DittoFSClient is intentionally duplicated from pkg/apiclient to keep the operator module self-contained without complex go.mod dependencies
- Admin credentials auto-generation is skipped when user provides spec.identity.admin.passwordSecretRef (respects user-managed credentials)
- Auth retry state is tracked via annotation rather than in-memory state, surviving operator pod restarts
- Transient errors (network failures) trigger backoff and requeue; permanent errors (wrong credentials) propagate to controller-runtime for standard error handling

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed Authenticated condition blocking Ready when replicas=0**
- **Found during:** Task 2 (controller integration)
- **Issue:** Adding Authenticated to the Ready aggregate caused the "Stopped" state test to fail because there's no API to authenticate against when replicas=0
- **Fix:** Made the Authenticated check conditional on desiredReplicas > 0, matching the pattern used for DatabaseReady with Percona
- **Files modified:** `k8s/dittofs-operator/internal/controller/dittoserver_controller.go`
- **Verification:** All existing controller tests pass including replicas=0 Stopped state
- **Committed in:** d314ad2 (Task 2 commit)

**2. [Rule 1 - Bug] Updated "ready StatefulSet" test expectations for Authenticated condition**
- **Found during:** Task 2 (controller integration)
- **Issue:** Existing test expected AllConditionsMet/Ready=True for a StatefulSet with ReadyReplicas=2, but auth reconciliation attempts to connect to a non-existent API, so Authenticated is now False
- **Fix:** Updated test to expect ConditionsNotMet/Ready=False with a clarifying comment
- **Files modified:** `k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go`
- **Verification:** Test passes correctly with new expectations
- **Committed in:** d314ad2 (Task 2 commit)

---

**Total deviations:** 2 auto-fixed (2 bug fixes)
**Impact on plan:** Both fixes are necessary consequences of adding Authenticated to the Ready aggregate. The conditional check for replicas=0 is the correct semantic behavior. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Operator can now authenticate with DittoFS API and store credentials in K8s Secret
- DittoFSClient is available for future adapter polling (Phase 2)
- ConditionAuthenticated feeds into Ready aggregate, making operator not-ready until authenticated
- Ready for Phase 2 (adapter lifecycle) to use the authenticated client for adapter discovery and management

## Self-Check: PASSED

- File k8s/dittofs-operator/internal/controller/auth_reconciler.go: FOUND
- File k8s/dittofs-operator/internal/controller/dittofs_client.go: FOUND
- File k8s/dittofs-operator/internal/controller/auth_reconciler_test.go: FOUND
- File k8s/dittofs-operator/utils/conditions/conditions.go: FOUND
- File k8s/dittofs-operator/api/v1alpha1/helpers.go: FOUND
- File k8s/dittofs-operator/internal/controller/dittoserver_controller.go: FOUND
- File k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go: FOUND
- Commit 202e9b4 verified in git log
- Commit d314ad2 verified in git log
- ConditionAuthenticated constant verified in conditions.go
- DittoFSClient verified in dittofs_client.go
- reconcileAuth integrated in dittoserver_controller.go
- 10 auth reconciler tests pass
- 10 existing controller tests pass
- Full operator build passes (go build ./...)
- Full operator vet passes (go vet ./...)

---
*Phase: 01-auth-foundation*
*Completed: 2026-02-10*
