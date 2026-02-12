---
phase: 04-security-hardening
plan: 01
subsystem: infra
tags: [kubernetes, operator, crd, security-hardening, nfs, smb]

# Dependency graph
requires:
  - phase: 03-dynamic-services-ports
    provides: "Dynamic per-adapter Services and container port reconciliation"
provides:
  - "CRD without static adapter fields (NFSPort, SMB, NFSEndpoint)"
  - "Controller without static -file Service or static adapter ports"
  - "Headless Service using API port for StatefulSet DNS"
  - "Clean webhook validation without NFS/SMB port checks"
affects: [04-02-PLAN]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Infrastructure-only CRD: all adapter config is dynamic via REST API"

key-files:
  created: []
  modified:
    - "k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go"
    - "k8s/dittofs-operator/api/v1alpha1/dittoserver_types_builder.go"
    - "k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook.go"
    - "k8s/dittofs-operator/api/v1alpha1/zz_generated.deepcopy.go"
    - "k8s/dittofs-operator/internal/controller/dittoserver_controller.go"
    - "k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go"
    - "k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml"
    - "k8s/dittofs-operator/chart/crds/dittoservers.yaml"

key-decisions:
  - "Headless Service port changed from NFS to API -- API is always available and sufficient for StatefulSet DNS"
  - "SMB test cases removed entirely since SMB types are deleted -- adapter testing is now via dynamic service reconciler"

patterns-established:
  - "No static adapter ports in CRD: all protocol ports are discovered and managed dynamically"

# Metrics
duration: 5min
completed: 2026-02-10
---

# Phase 4 Plan 1: Static Adapter Field Removal Summary

**Removed NFSPort, SMB, and NFSEndpoint from CRD; deleted -file Service and utils/nfs,smb packages; headless Service now uses API port**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-10T21:42:12Z
- **Completed:** 2026-02-10T21:47:21Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- Removed all static adapter fields (NFSPort, SMB, NFSEndpoint) from the DittoServer CRD spec and status
- Deleted the static -file Service and all NFS/SMB utility packages
- Updated headless Service to use API port for StatefulSet DNS resolution
- buildContainerPorts now emits only api + metrics; dynamic adapter ports handled by service_reconciler.go
- All Go code compiles and all tests pass after cleanup

## Task Commits

Each task was committed atomically:

1. **Task 1: Remove static adapter fields from CRD types and builder** - `bb1333e` (refactor)
2. **Task 2: Remove all static adapter references from controller, webhook, utils, and tests** - `1aac027` (feat)

## Files Created/Modified
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` - Removed NFSPort, SMB fields from spec; removed SMBAdapterSpec, SMBTimeoutsSpec, SMBCreditsSpec types; removed NFSEndpoint from status
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types_builder.go` - Removed WithNFSPort, WithSMB, WithNFSEndpoint builder functions
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook.go` - Removed NFS/SMB port constants and validation blocks
- `k8s/dittofs-operator/api/v1alpha1/zz_generated.deepcopy.go` - Regenerated (removed SMB/NFS deepcopy)
- `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` - Removed nfs/smb imports, reconcileFileService, NFSEndpoint; updated headless Service and buildContainerPorts
- `k8s/dittofs-operator/internal/controller/dittoserver_controller_test.go` - Removed SMB test cases, file service verification, NFSEndpoint assertion
- `k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml` - Regenerated CRD manifest
- `k8s/dittofs-operator/chart/crds/dittoservers.yaml` - Updated Helm chart CRD
- `k8s/dittofs-operator/utils/nfs/nfs.go` - Deleted
- `k8s/dittofs-operator/utils/smb/smb.go` - Deleted

## Decisions Made
- Headless Service port changed from NFS (12049) to API (8080) -- the headless Service only needs a port for StatefulSet DNS resolution and the API port is always available
- SMB test cases fully removed rather than converted -- adapter testing now happens through the dynamic service reconciler path

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- SECU-01 and SECU-02 satisfied: no static adapter fields in CRD, no adapter config in generated YAML
- Ready for 04-02 (adapter config lockdown / additional security hardening)
- All dynamic adapter infrastructure from Phase 3 is now the sole path for adapter management

## Self-Check: PASSED

All 8 modified files exist, 2 deleted files confirmed absent, both commit hashes (bb1333e, 1aac027) found in git log, SUMMARY.md exists.

---
*Phase: 04-security-hardening*
*Completed: 2026-02-10*
