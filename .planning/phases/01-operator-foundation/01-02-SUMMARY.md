---
phase: 01-operator-foundation
plan: 02
subsystem: operator
tags: [kubernetes, rbac, crd, kubebuilder]

# Dependency graph
requires:
  - phase: 01-01
    provides: Operator relocated to k8s/dittofs-operator/
provides:
  - RBAC secrets permission for reading JWT keys, user passwords, S3 credentials
  - CRD shortName `dittofs` enabling `kubectl get dittofs`
  - Memory stores sample CR for Phase 1 validation
affects: [02-configmap-services, 05-status-lifecycle]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "kubebuilder RBAC markers for automatic RBAC generation"
    - "Multiple shortNames for CRD aliases"

key-files:
  created:
    - k8s/dittofs-operator/config/samples/dittofs_v1alpha1_dittofs_memory.yaml
  modified:
    - k8s/dittofs-operator/internal/controller/dittoserver_controller.go
    - k8s/dittofs-operator/config/rbac/role.yaml
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go
    - k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml

key-decisions:
  - "Secrets RBAC: get;list;watch only (no create/update/delete) for security"
  - "Memory sample uses BadgerDB+local instead of pure memory (no memory backend type)"

patterns-established:
  - "RBAC markers regenerate role.yaml via make manifests"
  - "CRD shortNames use semicolon separator (ditto;dittofs)"

# Metrics
duration: 4min
completed: 2026-02-04
---

# Phase 1 Plan 2: RBAC and CRD Fixes Summary

**RBAC secrets permission added, CRD shortName `dittofs` enabled, memory stores sample CR created for Phase 1 validation**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-04T11:45:00Z
- **Completed:** 2026-02-04T11:49:00Z
- **Tasks:** 3
- **Files modified:** 5

## Accomplishments

- Added RBAC permission for secrets (get, list, watch) - required for reading JWT keys, user password hashes, S3 credentials
- Added `dittofs` as additional CRD shortName enabling `kubectl get dittofs`
- Created minimal sample CR using ephemeral storage configuration for Phase 1 testing

## Task Commits

Each task was committed atomically:

1. **Task 1: Add secrets RBAC permission** - `ef2f8f1` (feat)
2. **Task 2: Add dittofs shortName to CRD** - `c0c3b2b` (feat)
3. **Task 3: Create memory stores sample CR** - `2b4d649` (feat)

## Files Created/Modified

- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - Added RBAC marker for secrets
- `k8s/dittofs-operator/config/rbac/role.yaml` - Regenerated with secrets permission
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` - Added dittofs shortName marker
- `k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml` - Regenerated with both shortNames
- `k8s/dittofs-operator/config/samples/dittofs_v1alpha1_dittofs_memory.yaml` - New minimal sample CR

## Decisions Made

1. **Secrets RBAC scope:** Read-only access (get, list, watch) - no need for create/update/delete since operator only reads existing secrets
2. **Memory sample approach:** DittoFS doesn't have pure memory backend, so sample uses BadgerDB for metadata (requires small 1Gi PVC) and local filesystem on /tmp for content (ephemeral)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- RBAC permissions complete for operator to read secrets
- CRD supports both `ditto` and `dittofs` shortNames
- Sample CR ready for Phase 1 validation testing
- Ready to proceed with Phase 1 Plan 3 (ConfigMap Generation Testing)

---
*Phase: 01-operator-foundation*
*Completed: 2026-02-04*
