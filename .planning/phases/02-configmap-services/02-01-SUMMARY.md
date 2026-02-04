---
phase: 02-configmap-services
plan: 01
subsystem: operator-crd
tags: [crd, configmap, operator, infrastructure-config]
dependency-graph:
  requires: [01-01, 01-02, 01-03]
  provides: [infrastructure-only-crd, develop-branch-config-format]
  affects: [02-02, 02-03]
tech-stack:
  added: []
  patterns: [infrastructure-only-config, secret-resolution]
key-files:
  created: []
  modified:
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go
    - k8s/dittofs-operator/api/v1alpha1/zz_generated.deepcopy.go
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types_builder.go
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_webhook.go
    - k8s/dittofs-operator/internal/controller/config/types.go
    - k8s/dittofs-operator/internal/controller/config/config.go
    - k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
    - k8s/dittofs-operator/config/samples/*.yaml
decisions:
  - id: D-02-01-01
    decision: PostgresSecretRef takes precedence over Type field
    rationale: Per CONTEXT.md - if both SQLite type and PostgresSecretRef are set, Postgres wins silently
metrics:
  duration: 6 min
  completed: 2026-02-04
---

# Phase 02 Plan 01: CRD and ConfigMap Simplification Summary

Refactored CRD and ConfigMap generation to infrastructure-only config matching DittoFS develop branch.

## One-liner

CRD simplified to infrastructure-only fields (database, cache, metrics, controlPlane, identity); ConfigMap generates develop-branch format YAML with PostgreSQL secret resolution.

## What Was Done

### Task 1: Simplify CRD spec to infrastructure-only fields
- Removed dynamic config types: `DittoConfig`, `ShareConfig`, `BackendConfig`, `CacheConfig`
- Removed user management types: `UserManagementSpec`, `UserSpec`, `GroupSpec`, `GuestSpec`
- Removed identity mapping types: `IdentityMappingConfig`, `DirectoryAttributesConfig`
- Added new infrastructure types: `DatabaseConfig`, `SQLiteConfig`, `InfraCacheConfig`, `MetricsConfig`, `ControlPlaneAPIConfig`
- CRD spec now contains only: image, replicas, storage, database, cache, metrics, controlPlane, identity, service, nfsPort, smb, resources, securityContext

### Task 2: Rewrite config types to match DittoFS develop branch format
- Replaced types.go with infrastructure-only types matching `pkg/config/config.go`
- Removed: `MetadataStore`, `ContentStore`, `Share`, `User`, `Group`, `Guest`, `AdaptersConfig`, `NFSAdapter`, `SMBAdapter`
- Added: `DittoFSConfig`, `LoggingConfig`, `TelemetryConfig`, `DatabaseConfig`, `MetricsConfig`, `ControlPlaneConfig`, `JWTConfig`, `CacheConfig`, `AdminConfig`
- File size reduced from 227 lines to 74 lines

### Task 3: Rewrite ConfigMap generation for develop branch format
- Removed all backend/store resolution code
- Removed share, user/group/guest configuration generation
- Removed NFS/SMB adapter configuration in config file
- Added `buildDatabaseConfig` with PostgresSecretRef precedence
- Added `buildMetricsConfig`, `buildControlPlaneConfig`, `buildCacheConfig`
- PostgreSQL connection string resolved from Secret and included in config YAML

## Key Files

| File | Change |
|------|--------|
| `api/v1alpha1/dittoserver_types.go` | Simplified CRD spec with infrastructure-only fields |
| `api/v1alpha1/dittoserver_types_builder.go` | Updated builder functions for new fields |
| `api/v1alpha1/dittoserver_webhook.go` | Simplified validation (no shares/backends to validate) |
| `internal/controller/config/types.go` | New types matching develop branch format |
| `internal/controller/config/config.go` | Infrastructure-only ConfigMap generation |
| `config/crd/bases/dittofs.dittofs.com_dittoservers.yaml` | Generated CRD manifest |
| `config/samples/*.yaml` | Updated sample CRs for new schema |

## Generated Config Format

The ConfigMap now generates YAML matching this structure:

```yaml
logging:
  level: INFO
  format: json
  output: stdout
telemetry:
  enabled: false
shutdown_timeout: 30s
database:
  type: sqlite  # or postgres
  sqlite:
    path: /data/controlplane/controlplane.db
  # OR postgres: "postgres://..." (when PostgresSecretRef is set)
metrics:
  enabled: false
  port: 9090
controlplane:
  port: 8080
  jwt:
    secret: <from-secret>
    issuer: dittofs
    access_token_duration: 15m
    refresh_token_duration: 168h
cache:
  path: /data/cache
  size: 1GB
admin:
  username: admin
  password_hash: <from-secret>
```

## Decisions Made

1. **PostgresSecretRef takes precedence over Type field**: When both `database.type: sqlite` and `database.postgresSecretRef` are set, PostgreSQL is used. This follows the "Postgres takes precedence silently" rule from CONTEXT.md.

2. **Infrastructure-only CRD schema**: Stores, shares, adapters, and users are now managed via REST API at runtime, not through the CRD. This matches DittoFS develop branch architecture.

## Commits

| Hash | Message |
|------|---------|
| afe7b6d | feat(02-01): simplify CRD spec to infrastructure-only fields |
| ccaebf9 | feat(02-01): rewrite config types to match develop branch format |
| 36915aa | feat(02-01): rewrite ConfigMap generation for develop branch format |
| 4f7ace9 | fix(02-01): update sample CRs for infrastructure-only schema |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Updated sample CRs for new schema**
- **Found during:** Task verification
- **Issue:** All sample CR files used old schema with `config.backends`, `config.shares`, `users` fields that no longer exist
- **Fix:** Rewrote all sample CRs to use new infrastructure-only fields
- **Files modified:** 6 sample YAML files in config/samples/
- **Commit:** 4f7ace9

**2. [Rule 3 - Blocking] Updated builder and webhook files**
- **Found during:** Task 2 compilation
- **Issue:** Builder and webhook files referenced removed types (DittoConfig, UserManagementSpec)
- **Fix:** Rewrote builder functions for new CRD fields, simplified webhook validation
- **Files modified:** dittoserver_types_builder.go, dittoserver_webhook.go
- **Commit:** ccaebf9

## Verification Results

All verification criteria passed:

1. `make generate && make manifests` - SUCCESS
2. `go build ./...` - SUCCESS
3. CRD contains only infrastructure fields (database, cache, metrics, controlPlane, identity) - VERIFIED
4. Config types match DittoFS `pkg/config/config.go` structure - VERIFIED
5. `buildDatabaseConfig` checks PostgresSecretRef FIRST - VERIFIED
6. PostgreSQL connection string resolved from Secret - VERIFIED

## Next Phase Readiness

Ready for Plan 02-02 (Service Definitions):
- CRD schema finalized
- ConfigMap generation working
- Sample CRs updated
- All code compiles

No blockers identified.
