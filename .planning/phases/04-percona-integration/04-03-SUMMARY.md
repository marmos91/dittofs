---
phase: 04-percona-integration
plan: 03
subsystem: infra
tags: [percona, postgresql, operator, kubernetes, webhook, validation, samples]

# Dependency graph
requires:
  - phase: 04-percona-integration
    plan: 02
    provides: PerconaPGCluster reconciliation, DATABASE_URL wiring
provides:
  - Percona CRD existence validation in webhook
  - Percona/PostgresSecretRef precedence warning
  - Backup configuration validation
  - Sample Percona-enabled DittoServer CR
  - Webhook tests for Percona validation scenarios
affects: [05-status-lifecycle, 06-documentation]

# Tech tracking
tech-stack:
  added: []
  patterns: [crd-existence-check, precedence-warning-pattern, conditional-backup-validation]

key-files:
  created:
    - k8s/dittofs-operator/config/samples/dittofs_v1alpha1_dittoserver_percona.yaml
  modified:
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook.go
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook_test.go

key-decisions:
  - "CRD existence check uses RESTMapper (works even without CRD installed)"
  - "Percona takes precedence over PostgresSecretRef with warning (not error)"
  - "Backup validation: bucket and endpoint required when enabled"
  - "Backup credentials secret validation is warning only (allow external-secrets pattern)"

patterns-established:
  - "CRD existence validation: check RESTMapper before using external CRD types"
  - "Precedence warning pattern: warn user about ignored configuration"
  - "Conditional backup validation: only validate backup fields when backup.enabled=true"

# Metrics
duration: 2min
completed: 2026-02-05
---

# Phase 4 Plan 3: Webhook Validation, Sample CR, Human Verification Summary

**Added Percona CRD validation webhook, sample Percona-enabled CR, comprehensive tests, and verified complete integration**

## Performance

- **Duration:** 2 min (autonomous tasks) + human verification
- **Started:** 2026-02-05T11:29:00Z
- **Completed:** 2026-02-05T11:32:00Z
- **Tasks:** 3 (2 auto + 1 human-verify)
- **Files created:** 1
- **Files modified:** 2

## Accomplishments

- Webhook validates PerconaPGCluster CRD is installed when percona.enabled=true
- Webhook warns if both Percona and PostgresSecretRef are set (Percona wins)
- Webhook validates Percona StorageClass exists if specified
- Webhook validates backup config: bucket and endpoint required when enabled
- Webhook warns if backup credentials secret is missing
- Sample Percona CR demonstrates complete integration setup
- Comprehensive tests cover Percona validation scenarios

## Task Commits

Each task was committed atomically:

1. **Task 1: Add Percona CRD validation to webhook** - `9f3b629` (feat)
2. **Task 2: Add sample Percona CR and update tests** - `c10987f` (feat)
3. **Task 3: Human verification checkpoint** - Approved (all verifications passed)

## Files Created

- `k8s/dittofs-operator/config/samples/dittofs_v1alpha1_dittoserver_percona.yaml` - Sample DittoServer with Percona PostgreSQL integration, includes commented backup configuration

## Files Modified

- `k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook.go`:
  - Added Percona CRD existence check via RESTMapper
  - Added Percona/PostgresSecretRef precedence warning
  - Added Percona StorageClass validation
  - Added backup configuration validation (bucket, endpoint, credentials)

- `k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook_test.go`:
  - Added TestPerconaDisabled_NoCRDNeeded
  - Added TestPerconaPrecedenceWarning
  - Added TestPerconaBackupRequiredFields

## Decisions Made

| Decision | Rationale |
|----------|-----------|
| CRD check via RESTMapper | Works without needing the actual CRD type, fails gracefully |
| Precedence is warning not error | User might have both configured during migration |
| Backup credentials warning only | Allow external-secrets or Vault injection patterns |
| Sample includes commented backup | Shows full configuration without requiring S3 setup |

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all tasks completed smoothly.

## Verification Results

All verifications passed:
- `make test` - All tests pass
- `make build` - Build succeeds
- RBAC includes perconapgclusters permissions
- CRD schema includes percona field with all subfields
- Sample Percona CR is valid YAML

## Phase 4 Complete

**Complete Percona PostgreSQL integration delivered:**
1. PerconaConfig and PerconaBackupConfig CRD types (04-01)
2. Percona API scheme registration and RBAC (04-01)
3. reconcilePerconaPGCluster with owner reference (04-02)
4. Init container for PostgreSQL readiness (04-02)
5. DATABASE_URL injection from Percona Secret (04-02)
6. Webhook validation for Percona CRD existence (04-03)
7. Precedence and backup configuration validation (04-03)
8. Sample Percona-enabled CR (04-03)

## Next Phase Readiness

**Ready for Phase 5: Status and Lifecycle**
- All Percona integration complete
- Webhook validation ensures CRD prerequisite
- Sample CR demonstrates usage

**Dependencies for Phase 5:**
- Add DatabaseReady status condition
- Add reconciliation status conditions
- Implement finalizer for cleanup

---
*Phase: 04-percona-integration*
*Completed: 2026-02-05*
