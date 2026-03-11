---
phase: 01-auth-foundation
plan: 01
subsystem: auth
tags: [rbac, middleware, operator-role, chi-router]

# Dependency graph
requires: []
provides:
  - RoleOperator constant and IsValid() accepting "operator"
  - RequireRole middleware (fail-closed, parameterized by allowed roles)
  - ContextWithClaims helper for testing middleware
  - IsOperator() and HasRole() helpers on JWT Claims
  - Split adapter routes: GET /api/v1/adapters accessible to admin+operator
affects: [01-02-PLAN, k8s-operator-api-client]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "RequireRole middleware pattern: parameterized, fail-closed authorization"
    - "Route splitting: same resource, different auth per HTTP method"

key-files:
  created: []
  modified:
    - pkg/controlplane/models/user.go
    - internal/controlplane/api/auth/claims.go
    - internal/controlplane/api/middleware/auth.go
    - internal/controlplane/api/middleware/auth_test.go
    - pkg/controlplane/api/router.go
    - internal/controlplane/api/handlers/users.go

key-decisions:
  - "RequireRole is fail-closed: zero allowed roles means all requests denied"
  - "ContextWithClaims exported as public helper for test ergonomics"
  - "GET /api/v1/adapters/{type} stays admin-only per least-privilege principle"

patterns-established:
  - "RequireRole middleware: use for any future role-based route authorization"
  - "Route splitting: Group() within Route() to apply different middleware per method"

# Metrics
duration: 3min
completed: 2026-02-10
---

# Phase 1 Plan 1: Operator Role & Authorization Middleware Summary

**Operator role with fail-closed RequireRole middleware and split adapter routes for least-privilege K8s service account access**

## Performance

- **Duration:** 3 min
- **Started:** 2026-02-10T19:53:38Z
- **Completed:** 2026-02-10T19:56:29Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Added RoleOperator constant to models with full IsValid() support
- Implemented RequireRole middleware with fail-closed behavior and comprehensive test coverage (5 scenarios, 16 assertions)
- Split adapter routes so GET /api/v1/adapters is accessible to admin+operator while all write operations and GET /{type} remain admin-only
- Added IsOperator() and HasRole() helpers on JWT Claims for role checks
- Operator role users do NOT get MustChangePassword forced (confirmed existing logic)

## Task Commits

Each task was committed atomically:

1. **Task 1: Add operator role to DittoFS model and claims** - `4ccb416` (feat)
2. **Task 2: Implement RequireRole middleware and split adapter routes** - `ef80e3c` (feat)

## Files Created/Modified
- `pkg/controlplane/models/user.go` - Added RoleOperator constant, updated IsValid()
- `internal/controlplane/api/auth/claims.go` - Added IsOperator() and HasRole() methods
- `internal/controlplane/api/handlers/users.go` - Updated role validation error messages
- `internal/controlplane/api/middleware/auth.go` - Added RequireRole middleware and ContextWithClaims helper
- `internal/controlplane/api/middleware/auth_test.go` - Added 16 test cases for RequireRole
- `pkg/controlplane/api/router.go` - Split adapter routes for role-based access

## Decisions Made
- RequireRole is fail-closed: if zero allowed roles are passed, all requests are denied (safest default)
- Exported ContextWithClaims as a public helper to make middleware and handler testing ergonomic without exposing the context key type
- GET /api/v1/adapters/{type} stays admin-only per the absolute minimum privilege principle -- operator only needs the list, not individual adapter details

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Operator role is fully functional in the DittoFS server
- RequireRole middleware pattern available for any future role-based access requirements
- Ready for Plan 02 (K8s operator API client and controller) to create operator users and authenticate against the adapter list endpoint

## Self-Check: PASSED

- All 7 files verified present on disk
- Commit 4ccb416 verified in git log
- Commit ef80e3c verified in git log
- RoleOperator constant verified in models
- RequireRole middleware verified in router
- IsOperator helper verified in claims
- Full test suite passes (go test ./...)
- Full build passes (go build ./...)

---
*Phase: 01-auth-foundation*
*Completed: 2026-02-10*
