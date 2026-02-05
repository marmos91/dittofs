---
phase: 04-percona-integration
plan: 01
subsystem: infra
tags: [percona, postgresql, operator, kubernetes, crd]

# Dependency graph
requires:
  - phase: 03-storage-management
    provides: StorageSpec with StorageClassName, S3CredentialsSecretRef types
provides:
  - PerconaConfig CRD type for PostgreSQL auto-creation
  - PerconaBackupConfig type for pgBackRest S3 backups
  - Percona scheme registration in operator
  - RBAC permissions for PerconaPGCluster resources
  - Controller watching owned PerconaPGCluster
affects: [04-02, 04-03, 05-status-lifecycle]

# Tech tracking
tech-stack:
  added: [percona-postgresql-operator/v2@v2.8.2]
  patterns: [external-crd-watching, owned-resource-pattern]

key-files:
  created: []
  modified:
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go
    - k8s/dittofs-operator/cmd/main.go
    - k8s/dittofs-operator/internal/controller/dittoserver_controller.go
    - k8s/dittofs-operator/config/rbac/role.yaml

key-decisions:
  - "Percona API import: github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2"
  - "PerconaBackupConfig.CredentialsSecretRef uses corev1.LocalObjectReference (not SecretKeySelector)"
  - "Controller owns PerconaPGCluster (deleted with DittoServer)"

patterns-established:
  - "External CRD watching: Import API types, register scheme, watch owned resources"
  - "RBAC markers for external CRDs: groups=pgv2.percona.com,resources=perconapgclusters"

# Metrics
duration: 4min
completed: 2026-02-05
---

# Phase 4 Plan 1: Percona CRD Types and Foundation Summary

**PerconaConfig and PerconaBackupConfig CRD types with Percona API scheme registration and RBAC for PerconaPGCluster management**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-05T10:12:00Z
- **Completed:** 2026-02-05T10:16:26Z
- **Tasks:** 3
- **Files modified:** 7

## Accomplishments
- PerconaConfig struct enabling auto-creation of PerconaPGCluster resources
- PerconaBackupConfig struct for pgBackRest S3 backup configuration
- Percona pgv2 API types imported and scheme registered
- RBAC permissions grant operator full CRUD on PerconaPGCluster resources
- Controller watches owned PerconaPGCluster for reconciliation triggers

## Task Commits

Each task was committed atomically:

1. **Task 1: Add Percona CRD types** - `362986e` (feat)
2. **Task 2: Register Percona scheme and add RBAC** - `b37c97f` (feat)
3. **Task 3: Run tests and verify build** - (verification only, no commit needed)

## Files Created/Modified

- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` - Added PerconaConfig, PerconaBackupConfig types and percona field to DittoServerSpec
- `k8s/dittofs-operator/api/v1alpha1/zz_generated.deepcopy.go` - Generated deepcopy functions for new types
- `k8s/dittofs-operator/cmd/main.go` - Import pgv2 API and register scheme
- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - Import pgv2, add RBAC markers, watch PerconaPGCluster
- `k8s/dittofs-operator/config/rbac/role.yaml` - RBAC rules for perconapgclusters CRUD and status read
- `k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml` - CRD schema with percona field
- `k8s/dittofs-operator/go.mod` / `go.sum` - Added percona-postgresql-operator/v2 dependency

## Decisions Made

| Decision | Rationale |
|----------|-----------|
| Import path: `github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2` | v2 module path required for Percona 2.x API types |
| PerconaBackupConfig.CredentialsSecretRef uses LocalObjectReference | Simplified reference - backup Secret contains full s3.conf format |
| Controller Owns() PerconaPGCluster | Enables automatic deletion when DittoServer is deleted |
| RBAC includes perconapgclusters/status read | Needed for checking PostgreSQL readiness state |

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

**Percona API version resolution:**
- Initial attempt to import `github.com/percona/percona-postgresql-operator/pkg/apis/...` failed (v1.6.0 module, no v2 package)
- Resolved by checking Percona v2.8.2 go.mod - module path is `github.com/percona/percona-postgresql-operator/v2`
- Correct import: `github.com/percona/percona-postgresql-operator/v2/pkg/apis/pgv2.percona.com/v2`

## User Setup Required

None - no external service configuration required. Percona Operator must be installed separately in the cluster.

## Next Phase Readiness

**Ready for Phase 4 Plan 2:**
- PerconaConfig types available for reconcilePerconaPGCluster implementation
- Scheme registered, RBAC in place
- Controller watches PerconaPGCluster for status changes

**Dependencies for next plan:**
- Implement reconcilePerconaPGCluster() to create owned PerconaPGCluster
- Wire DATABASE_URL from Percona-created Secret
- Add init container for PostgreSQL readiness check

---
*Phase: 04-percona-integration*
*Completed: 2026-02-05*
