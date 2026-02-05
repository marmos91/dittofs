# Phase 4: Percona PostgreSQL Integration - Research

**Researched:** 2026-02-05
**Domain:** Kubernetes Operator Integration, Percona PostgreSQL Operator v2
**Confidence:** HIGH

## Summary

This phase integrates DittoFS operator with the Percona PostgreSQL Operator. The operator will auto-create PerconaPGCluster resources, extract connection credentials from Percona-managed Secrets, gate DittoFS readiness until PostgreSQL is available, and optionally configure pgBackRest backups to S3.

Research confirms:
1. **Percona API types are importable** via `github.com/percona/percona-postgresql-operator/pkg/apis/pgv2.percona.com/v2`
2. **Credential Secret format is well-documented**: `<clusterName>-pguser-<userName>` with keys `host`, `port`, `user`, `password`, `dbname`, `uri`
3. **PerconaPGCluster status** includes `state` field with values: `initializing`, `ready`, `paused`, `stopping`
4. **pgBackRest S3 configuration** is well-established with separate credentials Secret

**Primary recommendation:** Import Percona API types, watch PerconaPGCluster as secondary resource, use init container with `pg_isready` for double-check, extract `uri` key from credential Secret for `DATABASE_URL`.

## Standard Stack

The established libraries/tools for Percona integration:

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Percona PostgreSQL Operator | 2.8.x | PostgreSQL cluster management | Production-ready, Helm chart available |
| percona-postgresql-operator/pkg/apis | v2 | Go types for PerconaPGCluster | Official API, importable |
| controller-runtime | v0.22.4 | Watch external CRD | Already used by DittoFS operator |
| postgres:16-alpine | 16 | Init container base | Official PostgreSQL image with pg_isready |

### Helm Charts

| Chart | Repository | Purpose | When to Use |
|-------|------------|---------|-------------|
| pg-operator | percona | Install Percona Operator | Required dependency |
| pg-db | percona | PerconaPGCluster CR (reference only) | Not used directly; we create our own CR |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Percona Operator | CloudNativePG | CloudNativePG has different CRD schema, less backup flexibility |
| Import typed API | Unstructured/Dynamic client | Typed API provides compile-time safety |
| Init container pg_isready | Operator status check only | Init container provides double-check, handles race conditions |

**Installation:**
```bash
# Add Percona Helm repo (for Helm dependency)
helm repo add percona https://percona.github.io/percona-helm-charts/

# In Helm chart Chart.yaml dependencies:
dependencies:
  - name: pg-operator
    version: "2.8.2"
    repository: "https://percona.github.io/percona-helm-charts/"
    condition: percona.enabled
```

## Architecture Patterns

### PerconaPGCluster CRD Structure

```yaml
apiVersion: pgv2.percona.com/v2
kind: PerconaPGCluster
metadata:
  name: {dittofs-name}-postgres
  namespace: {same-namespace}
  ownerReferences:
    - apiVersion: dittofs.dittofs.com/v1alpha1
      kind: DittoServer
      name: {dittofs-name}
      uid: {dittofs-uid}
      controller: true
      blockOwnerDeletion: true
spec:
  crVersion: "2.8.0"
  postgresVersion: 16

  # Instance configuration
  instances:
    - name: instance1
      replicas: 1  # Configurable via perconaReplicas
      dataVolumeClaimSpec:
        accessModes:
          - ReadWriteOnce
        storageClassName: {perconaStorageClass}  # Optional, defaults to cluster default
        resources:
          requests:
            storage: 10Gi  # Default, configurable

  # User and database configuration
  users:
    - name: dittofs
      databases:
        - dittofs  # Configurable via database.name
      options: ""  # Not superuser

  # Backup configuration (optional)
  backups:
    pgbackrest:
      configuration:
        - secret:
            name: {dittofs-name}-pgbackrest-secret
      global:
        repo1-path: /pgbackrest/{dittofs-name}/repo1
        repo1-s3-uri-style: path
        repo1-storage-verify-tls: "y"
      repos:
        - name: repo1
          s3:
            bucket: {backup-bucket}
            endpoint: {backup-endpoint}
            region: {backup-region}
          schedules:
            full: "0 2 * * *"    # Daily at 2am
            incr: "0 * * * *"   # Hourly incremental
          # Retention defaults handled by pgBackRest
```

### Credential Secret Format (Percona-created)

Percona creates a Secret named `{cluster-name}-pguser-{user-name}`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: {dittofs-name}-postgres-pguser-dittofs
  namespace: {namespace}
type: Opaque
data:
  host: <base64>      # e.g., {cluster}-primary.{ns}.svc
  port: <base64>      # "5432"
  user: <base64>      # "dittofs"
  password: <base64>  # Auto-generated
  dbname: <base64>    # "dittofs"
  uri: <base64>       # postgresql://user:pass@host:port/dbname
  jdbc-uri: <base64>  # jdbc:postgresql://host:port/dbname?user=...
```

### Pattern 1: Watch External CRD (PerconaPGCluster)

**What:** Watch PerconaPGCluster as secondary resource without ownership.

**When to use:** When operator needs to react to changes in external resources.

**Implementation approach:**
```go
// Import Percona types
import (
    pgv2 "github.com/percona/percona-postgresql-operator/pkg/apis/pgv2.percona.com/v2"
)

// In main.go - add scheme
func init() {
    utilruntime.Must(pgv2.AddToScheme(scheme))
}

// In SetupWithManager - watch PerconaPGCluster
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&dittoiov1alpha1.DittoServer{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Owns(&pgv2.PerconaPGCluster{}).  // Owned by DittoServer
        Named("dittoserver").
        Complete(r)
}
```

### Pattern 2: Owned PerconaPGCluster Creation

**What:** Create PerconaPGCluster with owner reference so it's deleted with DittoServer.

**When to use:** Auto-create mode (perconaPGCluster field set with configuration).

**Implementation approach:**
```go
func (r *DittoServerReconciler) reconcilePerconaPGCluster(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
    if ds.Spec.Percona == nil || !ds.Spec.Percona.Enabled {
        return nil
    }

    pgCluster := &pgv2.PerconaPGCluster{
        ObjectMeta: metav1.ObjectMeta{
            Name:      ds.Name + "-postgres",
            Namespace: ds.Namespace,
        },
    }

    _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pgCluster, func() error {
        // Set owner reference
        if err := controllerutil.SetControllerReference(ds, pgCluster, r.Scheme); err != nil {
            return err
        }

        // Only set spec on CREATE (no drift reconciliation)
        if pgCluster.CreationTimestamp.IsZero() {
            pgCluster.Spec = buildPerconaPGClusterSpec(ds)
        }

        return nil
    })

    return err
}
```

### Pattern 3: Init Container for PostgreSQL Readiness

**What:** Init container waits for PostgreSQL to be ready before main container starts.

**When to use:** Always when PostgreSQL is configured.

**Init container spec:**
```yaml
initContainers:
  - name: wait-for-postgres
    image: postgres:16-alpine
    command:
      - /bin/sh
      - -c
      - |
        echo "Waiting for PostgreSQL at $PGHOST:$PGPORT..."
        until pg_isready -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -t 5; do
          echo "PostgreSQL not ready, waiting..."
          sleep 2
        done
        echo "PostgreSQL is ready!"
    env:
      - name: PGHOST
        valueFrom:
          secretKeyRef:
            name: {cluster-name}-pguser-dittofs
            key: host
      - name: PGPORT
        valueFrom:
          secretKeyRef:
            name: {cluster-name}-pguser-dittofs
            key: port
      - name: PGUSER
        valueFrom:
          secretKeyRef:
            name: {cluster-name}-pguser-dittofs
            key: user
      - name: PGPASSWORD
        valueFrom:
          secretKeyRef:
            name: {cluster-name}-pguser-dittofs
            key: password
      - name: PGDATABASE
        valueFrom:
          secretKeyRef:
            name: {cluster-name}-pguser-dittofs
            key: dbname
```

### Pattern 4: DATABASE_URL from Percona Secret

**What:** Construct DATABASE_URL environment variable from Percona's `uri` key.

**When to use:** When DittoFS needs PostgreSQL connection string.

**Implementation:**
```go
// In StatefulSet container env vars
env := []corev1.EnvVar{
    {
        Name: "DATABASE_URL",
        ValueFrom: &corev1.EnvVarSource{
            SecretKeyRef: &corev1.SecretKeySelector{
                LocalObjectReference: corev1.LocalObjectReference{
                    Name: perconaSecretName, // {dittofs-name}-postgres-pguser-dittofs
                },
                Key: "uri",
            },
        },
    },
}
```

### Anti-Patterns to Avoid

- **Copying Percona Secret to new Secret:** Reference directly. Percona may rotate credentials.
- **Overwriting user changes to PerconaPGCluster:** Only set spec on creation, not on updates.
- **Checking Percona status without init container:** Race condition - status may be Ready but connection may fail.
- **Using superuser for DittoFS:** Create dedicated `dittofs` user with limited permissions.

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| PostgreSQL readiness check | Custom TCP probe | `pg_isready` in init container | Handles auth check, proper exit codes |
| Connection string format | String concatenation | Percona's `uri` Secret key | Already formatted correctly |
| Password generation | Random string | Percona auto-generates | Secure, managed by operator |
| Backup scheduling | Custom CronJob | pgBackRest schedules in PerconaPGCluster | Native integration, WAL archiving |
| Credential rotation | Manual Secret update | Remove password from Secret, Percona regenerates | Atomic, handles all references |

**Key insight:** Percona handles all PostgreSQL lifecycle complexity. DittoFS operator just needs to create the CR and wait.

## Common Pitfalls

### Pitfall 1: PerconaPGCluster CRD Not Installed

**What goes wrong:** DittoFS CR creation fails with "no matches for kind PerconaPGCluster".

**Why it happens:** Percona Operator not installed, or CRDs not applied.

**How to avoid:** Validation webhook checks if PerconaPGCluster CRD exists before accepting DittoFS CR with Percona config.
```go
// In webhook validation
if ds.Spec.Percona != nil && ds.Spec.Percona.Enabled {
    // Check if PerconaPGCluster CRD exists
    gvk := schema.GroupVersionKind{
        Group:   "pgv2.percona.com",
        Version: "v2",
        Kind:    "PerconaPGCluster",
    }
    _, err := r.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
    if err != nil {
        return nil, fmt.Errorf("Percona PostgreSQL Operator CRD not installed")
    }
}
```

**Warning signs:** "no matches for kind" or "resource not found" errors.

### Pitfall 2: Secret Not Ready When StatefulSet Created

**What goes wrong:** Pod fails to start because Percona Secret doesn't exist yet.

**Why it happens:** PerconaPGCluster is created but Secret is not immediately available.

**How to avoid:**
1. Operator blocks StatefulSet creation until PerconaPGCluster status is `ready`
2. Init container provides secondary check
3. Use `optional: true` on secretKeyRef during transition (with warning)

**Warning signs:** Pod stuck in `CreateContainerConfigError`.

### Pitfall 3: Init Container Timeout

**What goes wrong:** Init container times out after 5 minutes.

**Why it happens:** PostgreSQL taking too long to start, or wrong credentials.

**How to avoid:**
1. Surface clear error in DittoServer status
2. Emit Kubernetes events: `PostgreSQLFailed`, `WaitingForDatabase`
3. Document troubleshooting steps

**Warning signs:** Pod stuck in `Init:0/1` for extended time.

### Pitfall 4: Precedence Confusion with External PostgreSQL

**What goes wrong:** User sets both `perconaPGClusterRef` AND `postgresSecretRef`, unclear which wins.

**Why it happens:** Two modes for PostgreSQL configuration.

**How to avoid:**
1. **Decision:** Percona wins if both set
2. Webhook emits warning when both are configured
3. Clear documentation

**Warning signs:** Unexpected database connection.

### Pitfall 5: Backup Credentials Not Set

**What goes wrong:** pgBackRest fails with S3 authentication error.

**Why it happens:** Backup requires separate Secret, not auto-created.

**How to avoid:**
1. Validate backup Secret exists in webhook (warning, not error)
2. Clear CRD field: `perconaBackup.credentialsSecretRef`
3. Document required Secret format

**Warning signs:** pgBackRest stanza creation fails.

## Code Examples

Verified patterns from official sources:

### PerconaPGCluster Spec Building

```go
// Source: Percona documentation + pg-db Helm values
func buildPerconaPGClusterSpec(ds *dittoiov1alpha1.DittoServer) pgv2.PerconaPGClusterSpec {
    replicas := int32(1)
    if ds.Spec.Percona.Replicas != nil {
        replicas = *ds.Spec.Percona.Replicas
    }

    storageSize := "10Gi"
    if ds.Spec.Percona.StorageSize != "" {
        storageSize = ds.Spec.Percona.StorageSize
    }

    dbName := "dittofs"
    if ds.Spec.Percona.DatabaseName != "" {
        dbName = ds.Spec.Percona.DatabaseName
    }

    spec := pgv2.PerconaPGClusterSpec{
        CRVersion:       "2.8.0",
        PostgresVersion: 16,
        Instances: []pgv2.PGInstanceSetSpec{
            {
                Name:     "instance1",
                Replicas: &replicas,
                DataVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
                    AccessModes: []corev1.PersistentVolumeAccessMode{
                        corev1.ReadWriteOnce,
                    },
                    Resources: corev1.VolumeResourceRequirements{
                        Requests: corev1.ResourceList{
                            corev1.ResourceStorage: resource.MustParse(storageSize),
                        },
                    },
                },
            },
        },
        Users: []pgv2.PostgresUserSpec{
            {
                Name:      "dittofs",
                Databases: []string{dbName},
            },
        },
    }

    // Set storage class if specified
    if ds.Spec.Percona.StorageClassName != nil {
        spec.Instances[0].DataVolumeClaimSpec.StorageClassName = ds.Spec.Percona.StorageClassName
    }

    return spec
}
```

### CRD Fields for Percona Integration

```go
// Add to DittoServerSpec
type DittoServerSpec struct {
    // ... existing fields ...

    // Percona configures auto-creation of PerconaPGCluster
    // +optional
    Percona *PerconaConfig `json:"percona,omitempty"`
}

type PerconaConfig struct {
    // Enabled triggers auto-creation of PerconaPGCluster
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`

    // Replicas for PostgreSQL instances
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=5
    Replicas *int32 `json:"replicas,omitempty"`

    // StorageSize for PostgreSQL data
    // +kubebuilder:default="10Gi"
    StorageSize string `json:"storageSize,omitempty"`

    // StorageClassName for PostgreSQL PVCs
    // +optional
    StorageClassName *string `json:"storageClassName,omitempty"`

    // DatabaseName for DittoFS control plane
    // +kubebuilder:default="dittofs"
    DatabaseName string `json:"databaseName,omitempty"`

    // Backup configures pgBackRest S3 backups
    // +optional
    Backup *PerconaBackupConfig `json:"backup,omitempty"`
}

type PerconaBackupConfig struct {
    // Enabled activates backup configuration
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`

    // CredentialsSecretRef references Secret with S3 credentials
    // Secret must contain keys: s3.conf (with repo1-s3-key and repo1-s3-key-secret)
    // +kubebuilder:validation:Required
    CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

    // Bucket name for backups
    // +kubebuilder:validation:Required
    Bucket string `json:"bucket"`

    // Endpoint for S3-compatible storage
    // +kubebuilder:validation:Required
    Endpoint string `json:"endpoint"`

    // Region for S3 bucket
    // +kubebuilder:default="eu-west-1"
    Region string `json:"region,omitempty"`

    // FullSchedule cron expression for full backups
    // +kubebuilder:default="0 2 * * *"
    FullSchedule string `json:"fullSchedule,omitempty"`

    // IncrSchedule cron expression for incremental backups
    // +kubebuilder:default="0 * * * *"
    IncrSchedule string `json:"incrSchedule,omitempty"`

    // RetentionDays for backup retention
    // +kubebuilder:default=7
    // +kubebuilder:validation:Minimum=1
    RetentionDays *int32 `json:"retentionDays,omitempty"`
}
```

### Status Conditions for PostgreSQL

```go
// Add to DittoServerStatus.Conditions
const (
    ConditionDatabaseReady = "DatabaseReady"
)

// In reconciler
if ds.Spec.Percona != nil && ds.Spec.Percona.Enabled {
    pgCluster := &pgv2.PerconaPGCluster{}
    err := r.Get(ctx, client.ObjectKey{
        Namespace: ds.Namespace,
        Name:      ds.Name + "-postgres",
    }, pgCluster)

    if err != nil {
        conditions.SetCondition(&dsCopy.Status.Conditions, ds.Generation,
            ConditionDatabaseReady, metav1.ConditionFalse, "PerconaPGClusterNotFound",
            "Waiting for PerconaPGCluster to be created")
    } else if pgCluster.Status.State == pgv2.AppStateReady {
        conditions.SetCondition(&dsCopy.Status.Conditions, ds.Generation,
            ConditionDatabaseReady, metav1.ConditionTrue, "PostgreSQLReady",
            "PostgreSQL cluster is ready")
    } else {
        conditions.SetCondition(&dsCopy.Status.Conditions, ds.Generation,
            ConditionDatabaseReady, metav1.ConditionFalse, "WaitingForPostgreSQL",
            fmt.Sprintf("PostgreSQL cluster state: %s", pgCluster.Status.State))
    }
}
```

### RBAC for PerconaPGCluster

```go
// Add to controller RBAC markers
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgv2.percona.com,resources=perconapgclusters/status,verbs=get
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Manual PostgreSQL StatefulSet | Operator-managed (Percona/CloudNativePG) | 2024 | Automatic HA, backups, upgrades |
| Connection string concatenation | Secret `uri` key | Percona 2.0+ | Properly escaped, includes all params |
| Superuser for applications | Dedicated limited user | Best practice | Principle of least privilege |
| Manual backup scripts | pgBackRest integrated | Percona 2.0+ | WAL archiving, point-in-time recovery |

**Deprecated/outdated:**
- **Percona Operator v1:** Incompatible with v2. Must use v2 API (`pgv2.percona.com/v2`)
- **Creating Secret copies:** Reference Percona Secret directly for credential rotation support

## Open Questions

Things that couldn't be fully resolved:

1. **Percona API Import Compatibility**
   - What we know: Import path is `github.com/percona/percona-postgresql-operator/pkg/apis/pgv2.percona.com/v2`
   - What's unclear: Exact version tag to pin in go.mod for Percona 2.8.x
   - Recommendation: Use latest v2 release tag, test compatibility

2. **pgBackRest Retention Configuration**
   - What we know: Retention is configurable in pgBackRest
   - What's unclear: Exact CRD field for retention days in PerconaPGCluster spec
   - Recommendation: Use pgBackRest `global` options for retention policy

3. **Helm Dependency Activation**
   - What we know: pg-operator chart can be Helm dependency
   - What's unclear: How to make it optional (only install if Percona integration used)
   - Recommendation: Use `condition: percona.enabled` in Chart.yaml dependencies

## Sources

### Primary (HIGH confidence)
- [Percona PostgreSQL Operator Documentation](https://docs.percona.com/percona-operator-for-postgresql/index.html) - Official docs, CRD reference
- [Percona v2 Go API Package](https://pkg.go.dev/github.com/percona/percona-postgresql-operator/pkg/apis/pgv2.percona.com/v2) - Go types, schema
- [Percona Helm Charts](https://github.com/percona/percona-helm-charts) - pg-operator, pg-db charts
- [PostgreSQL pg_isready](https://www.postgresql.org/docs/current/app-pg-isready.html) - Official pg_isready documentation

### Secondary (MEDIUM confidence)
- [Kubebuilder Watching Resources](https://book.kubebuilder.io/reference/watching-resources) - External CRD watching patterns
- [Percona Community Forums](https://forums.percona.com/) - S3 backup configuration examples

### Tertiary (LOW confidence)
- [Medium: Init Containers Pattern](https://medium.com/@xcoulon/initializing-containers-in-order-with-kubernetes-18173b9cc222) - Init container examples

## Metadata

**Confidence breakdown:**
- Percona CRD structure: HIGH - Official documentation and Go package
- Secret format: HIGH - Official documentation, verified
- Init container pattern: HIGH - Standard Kubernetes pattern, official pg_isready docs
- Backup configuration: MEDIUM - Community examples, some fields unclear

**Research date:** 2026-02-05
**Valid until:** 90 days (Percona 2.8.x is current stable)

---

## Summary for Planner

**Key Facts:**
1. Import Percona types: `github.com/percona/percona-postgresql-operator/pkg/apis/pgv2.percona.com/v2`
2. Secret naming: `{cluster}-pguser-{user}` with keys: `host`, `port`, `user`, `password`, `dbname`, `uri`
3. PerconaPGCluster status.state values: `initializing`, `ready`, `paused`, `stopping`
4. Use `uri` Secret key for `DATABASE_URL` environment variable
5. pgBackRest S3 requires separate credentials Secret with `s3.conf` format

**Phase 4 Work:**
1. Add CRD fields: `PerconaConfig`, `PerconaBackupConfig`
2. Import Percona API types and register scheme
3. Add RBAC for PerconaPGCluster resources
4. Implement `reconcilePerconaPGCluster()` with owner reference
5. Add init container for PostgreSQL readiness
6. Wire DATABASE_URL from Percona Secret
7. Add DatabaseReady status condition
8. Add webhook validation for Percona CRD existence
9. Add Helm chart dependency for pg-operator
10. Configure pgBackRest backup (optional)

**Not in Scope:**
- Restore via DittoFS CRD (use PerconaPGCluster directly)
- Drift reconciliation of PerconaPGCluster
- Multi-namespace PerconaPGCluster support
