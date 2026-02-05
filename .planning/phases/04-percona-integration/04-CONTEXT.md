# Phase 4: Percona PostgreSQL Integration - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Integrate DittoFS operator with Percona PostgreSQL Operator. The operator will auto-create PerconaPGCluster resources, extract connection credentials, gate DittoFS readiness until PostgreSQL is available, and optionally configure backups. Also supports external PostgreSQL via direct Secret reference.

</domain>

<decisions>
## Implementation Decisions

### Cluster Discovery
- **Reference by explicit name**: CRD field `perconaPGClusterRef.name` specifies exact cluster name
- **Same namespace only**: PerconaPGCluster must be in same namespace as DittoFS CR (simpler RBAC)
- **Optional field**: If not set, use SQLite metadata store (current behavior)
- **Helm dependency**: Percona Operator installed via Helm chart dependency
- **Auto-create PerconaPGCluster**: Operator creates PerconaPGCluster CR based on DittoFS spec
- **Naming convention**: Auto-created cluster named `{dittofs-name}-postgres`
- **Owned resource**: PerconaPGCluster owned by DittoFS CR — deleted when DittoFS is deleted
- **Storage class**: Configurable via separate `perconaStorageClass` field (database workloads may need faster storage)
- **Storage size**: Default 10Gi for auto-created clusters
- **Replicas**: Configurable via `perconaReplicas` field, default 1 (single instance)
- **PostgreSQL version**: 16 (latest stable)
- **Precedence**: If both PerconaPGClusterRef AND PostgresSecretRef are set, Percona wins (with warning)
- **Backups included**: pgBackRest configuration part of this phase

### External PostgreSQL Support
- **Two modes supported**:
  1. Percona mode: Auto-create and watch PerconaPGCluster
  2. External mode: User provides PostgreSQL connection Secret directly (existing `PostgresSecretRef` pattern)
- External PostgreSQL supports separate Kubernetes cluster or any PostgreSQL instance

### Secret Extraction
- **Dedicated user**: Create `dittofs` user with limited permissions (not superuser)
- **Auto-create database**: Percona provisions database via `users` section in PerconaPGCluster spec
- **Database name**: Configurable via CRD field, default `dittofs`
- **Reference directly**: Mount Percona's credential Secret into DittoFS pod (no copy)
- **Hash included**: Percona Secret included in config hash — pod restarts on credential change
- **Connection format**: Single `DATABASE_URL` environment variable with connection string
- **Schema migration**: DittoFS GORM auto-migrate handles table creation

### Readiness Gating
- **Init container**: Uses `postgres:16-alpine` with `pg_isready` to wait for PostgreSQL
- **Timeout**: 5 minutes before init container gives up
- **Double check**: Operator blocks StatefulSet creation until PerconaPGCluster status is Ready AND init container verifies auth
- **Full auth check**: Init container connects as dittofs user to dittofs database (not just TCP)
- **DatabaseReady condition**: DittoFS status shows separate condition for PostgreSQL readiness

### Failure Scenarios
- **DB deleted**: DittoFS goes unhealthy (health probes fail, status shows degraded). Pod stays running.
- **Transient failures**: DittoFS handles reconnection at app level. Operator doesn't intervene.
- **Missing Percona CRD**: Validation webhook rejects DittoFS CR if PerconaPGCluster CRD not installed
- **Error surfacing**: DittoFS status shows Percona provisioning errors (e.g., "PostgreSQL provisioning failed: StorageClass not found")
- **Events**: Operator emits events: PostgreSQLReady, PostgreSQLFailed, WaitingForDatabase
- **No drift reconciliation**: Operator doesn't overwrite user modifications to PerconaPGCluster

### Backup Configuration
- **Optional**: Backup configuration is optional. When not provided, no pgBackRest setup.
- **Storage**: S3-compatible (Cubbit DS3)
- **Separate credentials**: Different Secret for PostgreSQL backups (more isolation)
- **Schedule configurable**: CRD fields for backup schedule, with sensible defaults
- **Default schedule**: Daily full backup at 2am, hourly incremental
- **Retention configurable**: CRD field for retention days
- **Default retention**: 7 days when not specified
- **Bucket configurable**: CRD field to specify backup bucket (can be same or different from payload)
- **Restore via Percona**: User triggers restore directly via PerconaPGCluster, not DittoFS CRD

### Claude's Discretion
- Exact PerconaPGCluster spec structure (within Percona schema constraints)
- pgBackRest configuration details
- Init container shell script implementation
- How to construct DATABASE_URL from Percona Secret keys
- Event message wording

</decisions>

<specifics>
## Specific Ideas

- Helm chart should include Percona Operator as dependency so users don't need to install it separately
- For test/dev scenarios, backups can be completely omitted
- Auto-created PerconaPGCluster uses minimal defaults — user can customize it directly if needed (operator won't reconcile it back)

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 04-percona-integration*
*Context gathered: 2026-02-05*
