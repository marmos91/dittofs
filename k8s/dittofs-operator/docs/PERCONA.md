# Percona PostgreSQL Integration

This guide explains how to use the Percona PostgreSQL Operator for a production-grade metadata store with DittoFS.

## Overview

By default, DittoFS uses SQLite for the control plane database. While SQLite is simple and requires no external dependencies, it has limitations for production deployments:

- Single-node only (no horizontal scaling)
- File-based storage (data loss risk without proper backup)
- Limited concurrent write performance

The **Percona Operator for PostgreSQL** provides:

- Managed PostgreSQL clusters with automatic failover
- Built-in backup and restore via pgBackRest
- High availability with streaming replication
- Point-in-time recovery

When you enable Percona integration, the DittoFS operator automatically:

1. Creates a `PerconaPGCluster` resource owned by your `DittoServer`
2. Waits for PostgreSQL to become ready before starting DittoFS
3. Injects the `DATABASE_URL` into the DittoFS container
4. Optionally manages the PostgreSQL lifecycle with your DittoServer

## Architecture

```mermaid
flowchart TB
    subgraph "DittoServer CR"
        CR[DittoServer<br/>percona.enabled=true]
    end

    subgraph "DittoFS Operator"
        CTRL[Controller]
    end

    subgraph "Created by DittoFS Operator"
        STS[StatefulSet]
        INIT[Init Container<br/>wait-for-postgres]
        MAIN[Main Container<br/>DittoFS]
    end

    subgraph "Created by Percona Operator"
        PG[PerconaPGCluster]
        PGPOD[PostgreSQL Pods]
        PGSEC[Credentials Secret<br/>uri key]
    end

    CR --> CTRL
    CTRL -->|creates & owns| PG
    CTRL -->|creates| STS
    PG -->|managed by| PGPOD
    PG -->|creates| PGSEC
    STS --> INIT
    INIT -->|pg_isready| PGPOD
    INIT -.->|success| MAIN
    PGSEC -->|DATABASE_URL| MAIN
```

## Prerequisites

### 1. Install the Percona Operator

The Percona Operator must be installed in your cluster before enabling Percona integration.

**Using kubectl:**

```bash
# Install Percona Operator v2
kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/bundle.yaml
```

**Using Helm:**

```bash
# Add Percona Helm repository
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update

# Install the operator
helm install percona-pg-operator percona/pg-operator --namespace percona-system --create-namespace
```

### 2. Verify Installation

Confirm the Percona CRD is registered:

```bash
kubectl get crd perconapgclusters.pgv2.percona.com

# Expected output:
# NAME                                    CREATED AT
# perconapgclusters.pgv2.percona.com      2026-02-05T10:00:00Z
```

Verify the operator is running:

```bash
kubectl get pods -n percona-system

# Expected output:
# NAME                                    READY   STATUS    RESTARTS   AGE
# percona-postgresql-operator-xxx         1/1     Running   0          5m
```

## Enabling Percona Integration

Add the `percona` section to your DittoServer spec:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-sample
spec:
  storage:
    metadataSize: "10Gi"
    cacheSize: "5Gi"

  # Enable Percona PostgreSQL
  percona:
    enabled: true
    replicas: 1
    storageSize: "10Gi"
    databaseName: "dittofs"
```

### Configuration Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `percona.enabled` | bool | `false` | Enable PerconaPGCluster creation |
| `percona.replicas` | int | `1` | PostgreSQL instance count (1-5) |
| `percona.storageSize` | string | `"10Gi"` | PVC size for PostgreSQL data |
| `percona.storageClassName` | string | (cluster default) | StorageClass for PostgreSQL PVCs |
| `percona.databaseName` | string | `"dittofs"` | Database name for DittoFS |
| `percona.deleteWithServer` | bool | `false` | Delete PostgreSQL when DittoServer is deleted |
| `percona.backup.*` | object | - | Backup configuration (see below) |

## How It Works

### 1. PerconaPGCluster Creation

When you create a DittoServer with `percona.enabled: true`:

```bash
kubectl apply -f my-dittoserver.yaml
```

The operator creates a `PerconaPGCluster` owned by your DittoServer:

```bash
kubectl get perconapgcluster

# NAME                      ENDPOINT                        STATUS   POSTGRES   PGBOUNCER   AGE
# dittofs-sample-postgres   dittofs-sample-postgres:5432    ready    1          0           5m
```

### 2. Init Container Wait

The DittoFS StatefulSet includes an init container that waits for PostgreSQL:

```bash
kubectl describe pod dittofs-sample-0

# Init Containers:
#   wait-for-postgres:
#     Image: postgres:16
#     Command: pg_isready -h <postgres-host> -p 5432
```

The init container:
- Polls PostgreSQL every 5 seconds
- Times out after 5 minutes (60 attempts)
- Blocks DittoFS startup until PostgreSQL is ready

### 3. DATABASE_URL Injection

Once PostgreSQL is ready, Percona Operator creates a Secret with connection credentials. The DittoFS operator extracts the `uri` key and injects it as `DATABASE_URL`:

```bash
# View the Percona-created secret
kubectl get secret dittofs-sample-postgres-pguser-dittofs -o yaml

# The 'uri' key contains the full connection string:
# postgresql://dittofs:password@dittofs-sample-postgres:5432/dittofs?sslmode=prefer
```

The DittoFS container receives this as an environment variable:

```yaml
env:
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef:
        name: dittofs-sample-postgres-pguser-dittofs
        key: uri
```

## Backup Configuration

Enable S3 backups for PostgreSQL data protection:

```yaml
spec:
  percona:
    enabled: true
    replicas: 1
    storageSize: "10Gi"

    backup:
      enabled: true
      bucket: "my-pg-backups"
      endpoint: "https://s3.amazonaws.com"  # or https://s3.cubbit.eu for Cubbit
      region: "eu-west-1"
      credentialsSecretRef:
        name: pgbackrest-s3-credentials
      fullSchedule: "0 2 * * *"     # Daily at 2 AM
      incrSchedule: "0 * * * *"     # Hourly incremental
      retentionDays: 7
```

### Create Backup Credentials Secret

```bash
kubectl create secret generic pgbackrest-s3-credentials \
  --from-literal=s3-key=YOUR_ACCESS_KEY_ID \
  --from-literal=s3-key-secret=YOUR_SECRET_ACCESS_KEY
```

Or via YAML:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pgbackrest-s3-credentials
type: Opaque
stringData:
  s3-key: "YOUR_ACCESS_KEY_ID"
  s3-key-secret: "YOUR_SECRET_ACCESS_KEY"
```

### Backup Configuration Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backup.enabled` | bool | `false` | Enable backup configuration |
| `backup.bucket` | string | - | S3 bucket name |
| `backup.endpoint` | string | - | S3 endpoint URL |
| `backup.region` | string | `"eu-west-1"` | S3 region |
| `backup.credentialsSecretRef.name` | string | - | Secret containing S3 credentials |
| `backup.fullSchedule` | string | `"0 2 * * *"` | Cron for full backups |
| `backup.incrSchedule` | string | `"0 * * * *"` | Cron for incremental backups |
| `backup.retentionDays` | int | `7` | Days to retain backups |

## The deleteWithServer Flag

The `deleteWithServer` field controls what happens to the PostgreSQL cluster when you delete the DittoServer.

### Default Behavior (deleteWithServer: false)

When you delete a DittoServer, the PerconaPGCluster is **orphaned** - it continues running independently:

```bash
kubectl delete dittoserver dittofs-sample

# The PerconaPGCluster remains:
kubectl get perconapgcluster
# NAME                      STATUS   AGE
# dittofs-sample-postgres   ready    1d
```

This is the **safe default** because:
- Your PostgreSQL data is preserved
- You can recreate the DittoServer and reconnect to the same database
- Accidental deletions don't cause data loss

To clean up orphaned clusters manually:

```bash
kubectl delete perconapgcluster dittofs-sample-postgres
```

### Dangerous: deleteWithServer: true

Setting `deleteWithServer: true` means the PostgreSQL cluster **and all its data** are deleted with the DittoServer:

```yaml
spec:
  percona:
    enabled: true
    deleteWithServer: true  # WARNING: Deletes all PostgreSQL data!
```

**Use this only for:**
- Development/test environments
- Ephemeral deployments
- When data is backed up externally

**Never use in production** unless you have verified backup/restore procedures.

## Precedence Rules

### Percona vs PostgresSecretRef

If you have both `percona.enabled: true` AND `database.postgresSecretRef` configured:

```yaml
spec:
  database:
    type: postgres
    postgresSecretRef:
      name: external-postgres
      key: connection-string

  percona:
    enabled: true  # This takes precedence
```

**Percona always wins.** The operator emits a warning event:

```
Warning  ConfigConflict  Both percona.enabled and database.postgresSecretRef are set; Percona takes precedence
```

This allows migration scenarios where you're transitioning from external PostgreSQL to Percona.

### Percona vs SQLite

When `percona.enabled: true`, the `database.type` field is ignored:

```yaml
spec:
  database:
    type: sqlite  # Ignored when Percona is enabled

  percona:
    enabled: true  # PostgreSQL is used
```

## Troubleshooting

### Percona CRD Not Found

**Symptom:**

```
Error: no matches for kind "PerconaPGCluster" in version "pgv2.percona.com/v2"
```

**Cause:** Percona Operator not installed.

**Solution:**

```bash
# Install Percona Operator
kubectl apply --server-side -f https://raw.githubusercontent.com/percona/percona-postgresql-operator/v2.6.0/deploy/bundle.yaml

# Verify CRD exists
kubectl get crd perconapgclusters.pgv2.percona.com
```

### PerconaPGCluster Stuck in Initializing

**Symptom:**

```bash
kubectl get perconapgcluster
# NAME                      STATUS        AGE
# dittofs-sample-postgres   initializing  10m
```

**Cause:** Percona Operator cannot create PostgreSQL pods. Common reasons:
- PVC not binding (StorageClass issue)
- Image pull errors
- Resource constraints

**Solution:**

```bash
# Check Percona Operator logs
kubectl logs -n percona-system deployment/percona-postgresql-operator

# Check PostgreSQL pods
kubectl get pods -l postgres-operator.crunchydata.com/cluster=dittofs-sample-postgres

# Check events
kubectl describe perconapgcluster dittofs-sample-postgres
```

### Init Container Timeout

**Symptom:**

```bash
kubectl describe pod dittofs-sample-0
# Events:
#   Warning  Failed  Init container "wait-for-postgres" timed out
```

**Cause:** PostgreSQL didn't become ready within 5 minutes.

**Solution:**

```bash
# Check PerconaPGCluster status
kubectl get perconapgcluster dittofs-sample-postgres -o yaml

# Check PostgreSQL pod logs
kubectl logs dittofs-sample-postgres-instance1-0

# Manually test connectivity
kubectl run pg-test --rm -it --image=postgres:16 -- \
  pg_isready -h dittofs-sample-postgres -p 5432
```

### Secret Not Found

**Symptom:**

```bash
kubectl describe pod dittofs-sample-0
# Warning  Failed  Error: secret "dittofs-sample-postgres-pguser-dittofs" not found
```

**Cause:** Percona Operator hasn't created the credentials secret yet.

**Solution:**

Wait for PerconaPGCluster to reach `ready` status:

```bash
# Watch for ready status
kubectl get perconapgcluster -w

# Once ready, the secret should exist
kubectl get secret dittofs-sample-postgres-pguser-dittofs
```

### StorageClass Not Found

**Symptom:**

```
Error: storageclass.storage.k8s.io "nonexistent-class" not found
```

**Cause:** Specified `storageClassName` doesn't exist.

**Solution:**

```bash
# List available StorageClasses
kubectl get storageclass

# Use an existing class or omit to use default
spec:
  percona:
    storageClassName: "standard"  # Use existing class
```

## Complete Example

See [config/samples/dittofs_v1alpha1_dittoserver_percona.yaml](../config/samples/dittofs_v1alpha1_dittoserver_percona.yaml) for a complete example with all options.

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-percona
spec:
  image: marmos91c/dittofs:latest
  replicas: 1

  storage:
    metadataSize: "10Gi"
    contentSize: "50Gi"
    cacheSize: "5Gi"

  percona:
    enabled: true
    replicas: 1
    storageSize: "10Gi"
    databaseName: "dittofs"
    # deleteWithServer: false  # Safe default

  controlPlane:
    port: 8080

  identity:
    type: memory
    jwt:
      secretRef:
        name: dittofs-percona-jwt
        key: secret

  service:
    type: LoadBalancer
---
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-percona-jwt
type: Opaque
stringData:
  secret: "change-this-to-a-secure-random-string-at-least-32-chars"
```

Apply with:

```bash
kubectl apply -f dittofs_v1alpha1_dittoserver_percona.yaml
```

Monitor progress:

```bash
# Watch DittoServer status
kubectl get dittoserver dittofs-percona -w

# Check conditions
kubectl get dittoserver dittofs-percona -o jsonpath='{.status.conditions}' | jq

# View events
kubectl describe dittoserver dittofs-percona
```
