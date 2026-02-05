# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-04)

**Core value:** Enable one-command DittoFS deployment on Kubernetes with full configurability of storage backends through a declarative CRD
**Current focus:** Phase 6 - Documentation (Phase 5 Complete)

## Current Position

Phase: 6 of 6 (Documentation)
Plan: 2 of 3 in current phase
Status: Phase 6 in progress
Last activity: 2026-02-05 - Completed 06-02-PLAN.md (Percona, Troubleshooting, README)

Progress: [█████████████████░] 94%

## Performance Metrics

**Velocity:**
- Total plans completed: 17
- Average duration: 8 min
- Total execution time: ~132 min

**By Phase:**

| Phase | Plans | Total | Avg/Plan |
|-------|-------|-------|----------|
| 1. Operator Foundation | 3/3 | 47 min | 16 min |
| 2. ConfigMap and Services | 3/3 | 23 min | 8 min |
| 3. Storage Management | 3/3 | 21 min | 7 min |
| 4. Percona Integration | 3/3 | 9 min | 3 min |
| 5. Status and Lifecycle | 3/3 | 24 min | 8 min |
| 6. Documentation | 2/3 | 8 min | 4 min |

**Recent Trend:**
- Last 5 plans: 05-01 (~8m), 05-02 (8m), 05-03 (8m), 06-01 (~4m), 06-02 (4m)
- Trend: Documentation plans very fast (~4 min)

*Updated after each plan completion*

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-02-04 | Operator relocated to k8s/dittofs-operator/ | Per R1.5, Kubernetes code organized under k8s/ |
| 2026-02-04 | Module path: github.com/marmos91/dittofs/k8s/dittofs-operator | Matches directory structure |
| 2026-02-04 | Secrets RBAC: get;list;watch only | Security - operator only reads secrets |
| 2026-02-04 | Phase 1 complete despite pod CrashLoopBackOff | Image format mismatch expected - operator works |
| 2026-02-04 | cache.path required in ConfigMap | DittoFS requires cache configuration |
| 2026-02-04 | PostgresSecretRef takes precedence over Type field | If both SQLite type and PostgresSecretRef set, Postgres wins silently |
| 2026-02-04 | Infrastructure-only CRD schema | Stores, shares, adapters, users managed via REST API, not CRD |
| 2026-02-04 | Hash includes ConfigMap + secrets + generation | Complete change detection for pod restart |
| 2026-02-04 | ServiceName uses headless naming ({name}-headless) | Anticipates Plan 03 headless service |
| 2026-02-05 | CacheSize required field with 5Gi default | WAL persistence critical for crash recovery |
| 2026-02-05 | PVC retention policy Retain/Retain | Protect user data on DittoServer deletion/scaling |
| 2026-02-05 | AWS_ENDPOINT_URL is optional | AWS S3 doesn't need endpoint; Cubbit DS3 does |
| 2026-02-05 | S3 Secret keys all included in hash | Any credential change triggers pod restart |
| 2026-02-05 | StorageClass validation is hard error | Required for PVC creation - catch errors early |
| 2026-02-05 | S3 Secret validation is warning only | Allow CR creation before Secret exists (external-secrets, Vault) |
| 2026-02-05 | Percona API import: v2 module path | github.com/percona/percona-postgresql-operator/v2/... |
| 2026-02-05 | Controller Owns() PerconaPGCluster | Automatic deletion when DittoServer deleted |
| 2026-02-05 | No drift reconciliation for PerconaPGCluster | Users can modify directly, operator doesn't overwrite |
| 2026-02-05 | Init container 5-minute timeout for PostgreSQL | Balance waiting vs failing fast |
| 2026-02-05 | DATABASE_URL from Percona Secret 'uri' key | Full connection string, properly escaped |
| 2026-02-05 | CRD check via RESTMapper | Works without needing actual CRD type |
| 2026-02-05 | Percona precedence is warning not error | User might have both during migration |
| 2026-02-05 | Backup credentials warning only | Allow external-secrets pattern |
| 2026-02-05 | Finalizer: dittofs.dittofs.com/finalizer | Standard naming convention |
| 2026-02-05 | deleteWithServer=false by default | Preserve PostgreSQL data on accidental deletion |
| 2026-02-05 | 60-second cleanup timeout | Balance waiting vs stuck resources |
| 2026-02-05 | HTTP probes on API port | Better than TCP on NFS - checks actual health |
| 2026-02-05 | 150-second startup timeout | Allow slow database migrations |
| 2026-02-05 | 5-second preStop sleep | Connection draining for graceful shutdown |
| 2026-02-05 | PERCONA.md covers deleteWithServer lifecycle | Document data retention behavior clearly |
| 2026-02-05 | Troubleshooting uses Symptom/Cause/Solution format | Consistent structure, easy to follow |
| 2026-02-05 | README reduced to 133 lines | Entry point only, details in linked docs |

### Pending Todos

- **Update DittoFS Docker image** - Image needs to support new control plane config format (operator generates new format, published image expects old format). Address in Phase 6 or earlier.

### Blockers/Concerns

- DittoFS needs containerization before pods can run (future phase)
- Current sample CR expects image that doesn't exist yet
- Percona Operator must be installed in cluster before Percona integration works

## Phase 2 Summary - COMPLETE

**Plan 02-01: CRD and ConfigMap Simplification - COMPLETE**
- CRD simplified to infrastructure-only fields
- ConfigMap generates develop-branch format YAML
- PostgreSQL secret resolution implemented
- Sample CRs updated

**Plan 02-02: Checksum Annotation - COMPLETE**
- pkg/resources package with ComputeConfigHash utility
- collectSecretData helper gathers JWT, admin, postgres secrets
- StatefulSet pod template has dittofs.io/config-hash annotation
- ConfigMap reconciled before StatefulSet ensures hash computable

**Plan 02-03: Service Definitions - COMPLETE**
- ServiceBuilder fluent API in pkg/resources/service.go
- Four-service topology: headless, file, API, metrics (conditional)
- Port validation webhook with conflict detection
- Updated sample CR with infrastructure-only format

## Phase 3 Summary - COMPLETE

**Plan 03-01: Cache VolumeClaimTemplate - COMPLETE**
- CacheSize field added to StorageSpec (required, 5Gi default)
- Cache volume uses VolumeClaimTemplate (not EmptyDir)
- PersistentVolumeClaimRetentionPolicy Retain/Retain for data safety
- All tests updated and passing

**Plan 03-02: S3 Credentials Secret Reference - COMPLETE**
- S3CredentialsSecretRef and S3StoreConfig types added to CRD
- S3 credentials injected as AWS SDK environment variables
- buildS3EnvVars function wired to container Env field
- S3 Secret included in config hash for pod restart on change
- Sample S3 Secret and DittoServer CR added

**Plan 03-03: StorageClass Validation Webhook - COMPLETE**
- DittoServerValidator struct with Kubernetes client injection
- StorageClass existence validation (hard error if not found)
- S3 Secret existence and key validation (warnings only)
- SetupDittoServerWebhookWithManager function in main.go
- Comprehensive tests for all validation scenarios

## Phase 4 Summary - COMPLETE

**Plan 04-01: Percona CRD Types and Foundation - COMPLETE**
- PerconaConfig and PerconaBackupConfig CRD types added
- Percona pgv2 API types imported and scheme registered
- RBAC markers grant operator full CRUD on PerconaPGCluster
- Controller watches owned PerconaPGCluster resources
- All tests pass, build succeeds

**Plan 04-02: PerconaPGCluster Reconciliation - COMPLETE**
- pkg/percona package with BuildPerconaPGClusterSpec and status helpers
- reconcilePerconaPGCluster creates owned PerconaPGCluster
- Controller blocks StatefulSet until PostgreSQL ready
- Init container waits 5 minutes for PostgreSQL using pg_isready
- DATABASE_URL injected from Percona Secret 'uri' key
- Percona Secret included in config hash

**Plan 04-03: Webhook Validation, Sample CR, Human Verification - COMPLETE**
- Webhook validates PerconaPGCluster CRD installed when percona.enabled=true
- Webhook warns if both Percona and PostgresSecretRef set (Percona wins)
- Webhook validates Percona StorageClass and backup configuration
- Sample Percona CR demonstrates full integration setup
- Human verified: all tests pass, RBAC correct, CRD schema complete

## Phase 5 Summary - COMPLETE

**Plan 05-01: Status Conditions Implementation - COMPLETE**
- DittoServerStatus enhanced with comprehensive fields
- Five conditions: Ready, Available, Progressing, ConfigReady, DatabaseReady
- Ready is aggregate of other conditions
- updateConfigReadyCondition and IsConditionTrue helpers

**Plan 05-02: Finalizer Implementation - COMPLETE**
- DeleteWithServer field in PerconaConfig (default: false)
- Finalizer: dittofs.dittofs.com/finalizer
- handleDeletion with 60-second timeout
- performCleanup orphans or deletes PerconaPGCluster based on deleteWithServer
- Status phase shows "Deleting" during deletion

**Plan 05-03: Observability (Event Recorder and Probes) - COMPLETE**
- EventRecorder wired into reconciler via main.go
- Events emitted: Created, Deleting, CleanupTimeout, PerconaDeleted, PerconaOrphaned, PerconaNotReady, ReconcileFailed
- HTTP probes on API port: liveness /health, readiness /health/ready
- StartupProbe with 150-second max startup (30 failures * 5s)
- PreStop lifecycle hook with 5-second sleep for connection draining

## Phase 6 Summary - IN PROGRESS

**Plan 06-01: INSTALL.md and Helm Chart - COMPLETE**
- Helm chart generated with helmify
- INSTALL.md with kubectl and Helm instructions
- CRD_REFERENCE.md with comprehensive field documentation

**Plan 06-02: Percona, Troubleshooting, README - COMPLETE**
- PERCONA.md with complete PostgreSQL integration guide
- TROUBLESHOOTING.md with 9 common issues
- README.md rewritten as concise entry point (133 lines)

**Plan 06-03: Scaleway Deployment Validation - PENDING**
- Deploy to Scaleway Kapsule cluster
- Validate LoadBalancer and NFS mount
- Final documentation review

## Session Continuity

Last session: 2026-02-05T13:51:00Z
Stopped at: Completed 06-02-PLAN.md
Resume file: .planning/phases/06-documentation/06-03-PLAN.md

---
*State initialized: 2026-02-04*
*Milestone: v1.0*
