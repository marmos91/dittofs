# DittoServer CRD Reference

Complete API reference for the DittoServer Custom Resource Definition.

## Overview

| Property | Value |
|----------|-------|
| API Group | `dittofs.dittofs.com` |
| API Version | `v1alpha1` |
| Kind | `DittoServer` |
| Plural | `dittoservers` |
| Short Names | `ditto`, `dittofs` |
| Scope | Namespaced |

The DittoServer CRD defines infrastructure-only configuration. Dynamic configuration such as stores, shares, adapters, and users is managed via the REST API at runtime.

## Architecture

When you create a DittoServer resource, the operator creates and manages the following Kubernetes resources:

```mermaid
graph TB
    subgraph "User Creates"
        CR[DittoServer CR]
    end

    subgraph "Operator Manages"
        CM[ConfigMap<br>config.yaml]
        STS[StatefulSet<br>dittofs pod]
        SVC1[Service - Headless<br>pod DNS]
        SVC2[Service - File<br>NFS/SMB ports]
        SVC3[Service - API<br>REST API port]
        SVC4[Service - Metrics<br>optional]
        PVC1[PVC - Metadata<br>/data/metadata]
        PVC2[PVC - Cache<br>/data/cache]
        PVC3[PVC - Content<br>optional]
    end

    subgraph "Optional - Percona Integration"
        PG[PerconaPGCluster<br>PostgreSQL HA]
    end

    CR --> CM
    CR --> STS
    CR --> SVC1
    CR --> SVC2
    CR --> SVC3
    CR --> SVC4
    CR --> PVC1
    CR --> PVC2
    CR --> PVC3
    CR -.->|percona.enabled| PG
    STS --> PVC1
    STS --> PVC2
    STS --> PVC3
```

## Spec Fields

### Core Fields

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `image` | string | `marmos91c/dittofs:latest` | No | Container image for DittoFS server |
| `replicas` | int32 | `1` | No | Number of server replicas (0 or 1 only) |
| `resources` | [ResourceRequirements](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#resources) | - | No | Container resource requirements |
| `securityContext` | [SecurityContext](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#security-context) | - | No | Container security context |
| `podSecurityContext` | [PodSecurityContext](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#security-context-1) | - | No | Pod-level security context |

**Validation Rules:**
- `replicas` must be 0 or 1 (DittoFS is single-node)

### Storage Configuration (`storage`)

The storage section configures PVCs for the DittoFS server pod's internal use.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `storage.metadataSize` | string | - | **Yes** | Size for metadata store PVC (mounted at `/data/metadata`). Used by BadgerDB or other metadata backends. |
| `storage.cacheSize` | string | `5Gi` | **Yes** | Size for cache PVC (mounted at `/data/cache`). Required for WAL persistence - enables crash recovery. |
| `storage.contentSize` | string | - | No | Size for content store PVC (mounted at `/data/content`). Used for local filesystem content backend (not needed if using pure S3). |
| `storage.storageClassName` | string | - | No | StorageClass for the server's PVCs. If not specified, uses the cluster's default StorageClass. |

**Validation Rules:**
- Size fields must match pattern `^[0-9]+(Gi|Mi|Ti)$` (e.g., `10Gi`, `512Mi`, `1Ti`)
- `metadataSize` is required
- `cacheSize` is required (defaults to `5Gi`)

**Examples:**
```yaml
storage:
  metadataSize: "10Gi"
  cacheSize: "5Gi"
  contentSize: "50Gi"  # Optional, for local filesystem backend
  storageClassName: "fast-ssd"
```

### Database Configuration (`database`)

Configures the control plane database for storing users, groups, shares, and settings.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `database.type` | string | `sqlite` | No | Database type: `sqlite` or `postgres` |
| `database.sqlite.path` | string | `/data/controlplane/controlplane.db` | No | SQLite database file path inside the container |
| `database.postgresSecretRef` | SecretKeySelector | - | No | Reference to Secret containing PostgreSQL connection string. When configured, Postgres takes precedence over SQLite (regardless of `type` field). |

**Validation Rules:**
- `type` must be one of: `sqlite`, `postgres`
- When `postgresSecretRef` is set, Postgres is used regardless of `type` setting

**Examples:**

SQLite (default):
```yaml
database:
  type: sqlite
  sqlite:
    path: "/data/controlplane/controlplane.db"
```

PostgreSQL with external database:
```yaml
database:
  postgresSecretRef:
    name: postgres-credentials
    key: connection-string
```

### Cache Configuration (`cache`)

Configures the WAL-backed cache for crash recovery.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `cache.path` | string | `/data/cache` | No | Directory for cache WAL file |
| `cache.size` | string | `1GB` | No | Maximum cache size (e.g., `1GB`, `512MB`) |

**Examples:**
```yaml
cache:
  path: "/data/cache"
  size: "2GB"
```

### Service Configuration (`service`)

Configures the Kubernetes Service for the DittoFS server.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `service.type` | string | `ClusterIP` | No | Service type |
| `service.annotations` | map[string]string | - | No | Service annotations (e.g., for cloud load balancer configuration) |

**Validation Rules:**
- `type` must be one of: `ClusterIP`, `NodePort`, `LoadBalancer`

**Examples:**
```yaml
service:
  type: LoadBalancer
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
```

### NFS Port

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `nfsPort` | int32 | `12049` | No | NFS server port (non-privileged default) |

**Validation Rules:**
- Must be between 1 and 65535

### SMB Configuration (`smb`)

Configures the SMB protocol adapter.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `smb.enabled` | bool | `false` | No | Enable SMB protocol |
| `smb.port` | int32 | `445` | No | SMB port to listen on |
| `smb.maxConnections` | int32 | `0` | No | Maximum concurrent SMB connections (0 = unlimited) |
| `smb.maxRequestsPerConnection` | int32 | `100` | No | Maximum concurrent requests per SMB connection |
| `smb.metricsLogInterval` | string | `5m` | No | Metrics logging interval (e.g., `5m`, `10m`) |

**SMB Timeouts (`smb.timeouts`):**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `smb.timeouts.read` | string | `5m` | Maximum time to read request |
| `smb.timeouts.write` | string | `30s` | Maximum time to write response |
| `smb.timeouts.idle` | string | `5m` | Maximum idle time between requests |
| `smb.timeouts.shutdown` | string | `30s` | Graceful shutdown timeout |

**SMB Credits (`smb.credits`):**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `smb.credits.strategy` | string | `adaptive` | Credit grant strategy: `fixed`, `echo`, or `adaptive` |
| `smb.credits.minGrant` | int32 | `16` | Minimum credits per response |
| `smb.credits.maxGrant` | int32 | `8192` | Maximum credits per response |
| `smb.credits.initialGrant` | int32 | `256` | Credits for initial requests (NEGOTIATE) |
| `smb.credits.maxSessionCredits` | int32 | `65535` | Maximum outstanding credits per session |
| `smb.credits.loadThresholdHigh` | int32 | `1000` | Server load threshold for throttling (adaptive only) |
| `smb.credits.loadThresholdLow` | int32 | `100` | Server load threshold for boosting (adaptive only) |
| `smb.credits.aggressiveClientThreshold` | int32 | `256` | Outstanding requests threshold for client throttling (adaptive only) |

**Validation Rules:**
- `smb.port` must be between 1 and 65535
- `smb.credits.strategy` must be one of: `fixed`, `echo`, `adaptive`

**Examples:**
```yaml
smb:
  enabled: true
  port: 445
  maxConnections: 100
  timeouts:
    read: "10m"
    shutdown: "60s"
  credits:
    strategy: adaptive
    maxGrant: 4096
```

### Metrics Configuration (`metrics`)

Configures Prometheus metrics.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `metrics.enabled` | bool | `false` | No | Enable metrics exposure |
| `metrics.port` | int32 | `9090` | No | Metrics HTTP port |

**Validation Rules:**
- `port` must be between 1 and 65535

**Examples:**
```yaml
metrics:
  enabled: true
  port: 9090
```

### Control Plane Configuration (`controlPlane`)

Configures the REST API server.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `controlPlane.port` | int32 | `8080` | No | API server port |

**Validation Rules:**
- `port` must be between 1 and 65535

### Identity Configuration (`identity`)

Configures JWT authentication and admin user.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `identity.type` | string | `memory` | No | Type of identity store |

**JWT Configuration (`identity.jwt`):**

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `identity.jwt.secretRef` | SecretKeySelector | - | **Yes** (if jwt configured) | Reference to Secret containing JWT signing secret (at least 32 characters) |
| `identity.jwt.accessTokenDuration` | string | `15m` | No | Access token duration (e.g., `15m`, `1h`) |
| `identity.jwt.refreshTokenDuration` | string | `168h` | No | Refresh token duration (e.g., `168h` for 7 days) |
| `identity.jwt.issuer` | string | `dittofs` | No | Token issuer claim |

**Admin Configuration (`identity.admin`):**

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `identity.admin.username` | string | `admin` | No | Admin username |
| `identity.admin.passwordSecretRef` | SecretKeySelector | - | No | Reference to Secret containing admin password hash (bcrypt). If not set, a random password is generated and logged at startup. |

**Examples:**
```yaml
identity:
  type: memory
  jwt:
    secretRef:
      name: dittofs-jwt-secret
      key: secret
    accessTokenDuration: "30m"
    refreshTokenDuration: "24h"
  admin:
    username: "admin"
    passwordSecretRef:
      name: dittofs-admin-secret
      key: password
```

### S3 Configuration (`s3`)

Configures S3-compatible payload store credentials. Credentials are injected as environment variables for the AWS SDK.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `s3.region` | string | `eu-west-1` | No | Region for S3 bucket |
| `s3.bucket` | string | - | No | Bucket name (informational; actual config via REST API) |

**S3 Credentials (`s3.credentialsSecretRef`):**

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `s3.credentialsSecretRef.secretName` | string | - | **Yes** (if s3 configured) | Name of the Secret in the same namespace |
| `s3.credentialsSecretRef.accessKeyIdKey` | string | `accessKeyId` | No | Key for access key ID in Secret |
| `s3.credentialsSecretRef.secretAccessKeyKey` | string | `secretAccessKey` | No | Key for secret access key in Secret |
| `s3.credentialsSecretRef.endpointKey` | string | `endpoint` | No | Key for S3 endpoint URL (optional, for S3-compatible services like Cubbit DS3) |

**Examples:**
```yaml
s3:
  region: "eu-west-1"
  bucket: "my-dittofs-bucket"
  credentialsSecretRef:
    secretName: s3-credentials
    accessKeyIdKey: access-key
    secretAccessKeyKey: secret-key
    endpointKey: endpoint  # For S3-compatible services
```

### Percona PostgreSQL Configuration (`percona`)

Configures auto-creation of PerconaPGCluster for PostgreSQL metadata store. Requires Percona Operator to be installed in the cluster.

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `percona.enabled` | bool | `false` | No | Enable auto-creation of PerconaPGCluster |
| `percona.deleteWithServer` | bool | `false` | No | Delete PerconaPGCluster when DittoServer is deleted. **WARNING:** Setting to true will delete all PostgreSQL data! |
| `percona.replicas` | int32 | `1` | No | Number of PostgreSQL instances |
| `percona.storageSize` | string | `10Gi` | No | Storage size for PostgreSQL data volume |
| `percona.storageClassName` | string | - | No | StorageClass for PostgreSQL PVCs (may differ from DittoFS storage) |
| `percona.databaseName` | string | `dittofs` | No | Database name for DittoFS control plane |

**Percona Backup Configuration (`percona.backup`):**

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `percona.backup.enabled` | bool | `false` | No | Enable pgBackRest S3 backups |
| `percona.backup.credentialsSecretRef.name` | string | - | No | Secret with S3 credentials for pgBackRest |
| `percona.backup.bucket` | string | - | No | Bucket name for backups |
| `percona.backup.endpoint` | string | - | No | Endpoint for S3-compatible storage |
| `percona.backup.region` | string | `eu-west-1` | No | Region for S3 bucket |
| `percona.backup.fullSchedule` | string | `0 2 * * *` | No | Cron expression for full backups |
| `percona.backup.incrSchedule` | string | `0 * * * *` | No | Cron expression for incremental backups |
| `percona.backup.retentionDays` | int32 | `7` | No | Backup retention in days |

**Validation Rules:**
- `replicas` must be between 1 and 5
- `retentionDays` must be at least 1
- When `percona.enabled=true`, Percona Operator CRD must be installed

**Examples:**
```yaml
percona:
  enabled: true
  deleteWithServer: false  # Preserve data on delete
  replicas: 3
  storageSize: "50Gi"
  storageClassName: "fast-ssd"
  databaseName: "dittofs"
  backup:
    enabled: true
    bucket: "pg-backups"
    endpoint: "https://s3.cubbit.eu"
    region: "eu-west-1"
    credentialsSecretRef:
      name: pgbackrest-s3-credentials
    fullSchedule: "0 3 * * *"
    retentionDays: 14
```

## Status Fields

The status section is managed by the operator and reflects the current state of the DittoServer.

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | Generation last processed by the controller |
| `replicas` | int32 | Desired number of replicas |
| `readyReplicas` | int32 | Number of pods with Ready condition |
| `availableReplicas` | int32 | Number of pods ready for at least minReadySeconds |
| `nfsEndpoint` | string | Endpoint for NFS clients (format: `service.namespace.svc.cluster.local:12049`) |
| `phase` | string | Current phase: `Pending`, `Running`, `Failed`, `Stopped`, `Deleting` |
| `configHash` | string | Hash of current configuration (for debugging) |
| `perconaClusterName` | string | Name of owned PerconaPGCluster (when Percona enabled) |
| `conditions` | []Condition | Detailed status conditions |

### Status Conditions

| Condition | Meaning |
|-----------|---------|
| `Ready` | Aggregate condition - True when all other conditions are True |
| `Available` | At least one replica is available |
| `Progressing` | Deployment is progressing (creating/updating pods) |
| `ConfigReady` | ConfigMap has been created and is valid |
| `DatabaseReady` | Database is ready (always True for SQLite, checks PerconaPGCluster for Percona) |

**Example status:**
```yaml
status:
  observedGeneration: 3
  replicas: 1
  readyReplicas: 1
  availableReplicas: 1
  nfsEndpoint: "my-dittofs-file.default.svc.cluster.local:12049"
  phase: Running
  configHash: "abc123def"
  conditions:
    - type: Ready
      status: "True"
      reason: AllConditionsMet
      message: "All conditions are True"
      lastTransitionTime: "2024-01-15T10:00:00Z"
    - type: Available
      status: "True"
      reason: MinimumReplicasAvailable
      message: "1/1 replicas available"
      lastTransitionTime: "2024-01-15T10:00:00Z"
    - type: ConfigReady
      status: "True"
      reason: ConfigMapCreated
      message: "ConfigMap created successfully"
      lastTransitionTime: "2024-01-15T09:59:00Z"
    - type: DatabaseReady
      status: "True"
      reason: SQLiteConfigured
      message: "SQLite database configured"
      lastTransitionTime: "2024-01-15T09:59:00Z"
```

## Complete Examples

### Minimal Configuration (Memory Stores)

For development or testing with ephemeral storage:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-minimal
spec:
  storage:
    metadataSize: "1Gi"
    cacheSize: "1Gi"
```

### Production with S3 Payload Store

For production with S3-backed content storage:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-s3
spec:
  image: "marmos91c/dittofs:v1.0.0"
  replicas: 1

  storage:
    metadataSize: "10Gi"
    cacheSize: "20Gi"
    storageClassName: "fast-ssd"

  database:
    type: sqlite

  cache:
    size: "5GB"

  service:
    type: LoadBalancer
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: "nlb"

  metrics:
    enabled: true
    port: 9090

  identity:
    jwt:
      secretRef:
        name: dittofs-jwt
        key: secret
      accessTokenDuration: "1h"
    admin:
      username: "admin"
      passwordSecretRef:
        name: dittofs-admin
        key: password-hash

  s3:
    region: "eu-west-1"
    bucket: "my-dittofs-content"
    credentialsSecretRef:
      secretName: aws-s3-credentials

  resources:
    limits:
      cpu: "2"
      memory: "4Gi"
    requests:
      cpu: "500m"
      memory: "1Gi"
---
# JWT Secret
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-jwt
type: Opaque
stringData:
  secret: "your-jwt-secret-at-least-32-characters-long"
---
# Admin Password Secret (bcrypt hash)
apiVersion: v1
kind: Secret
metadata:
  name: dittofs-admin
type: Opaque
stringData:
  password-hash: "$2a$10$..."  # bcrypt hash of admin password
---
# S3 Credentials Secret
apiVersion: v1
kind: Secret
metadata:
  name: aws-s3-credentials
type: Opaque
stringData:
  accessKeyId: "AKIAIOSFODNN7EXAMPLE"
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
```

### Production with Percona PostgreSQL

For high-availability production with managed PostgreSQL:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-ha
spec:
  image: "marmos91c/dittofs:v1.0.0"
  replicas: 1

  storage:
    metadataSize: "10Gi"
    cacheSize: "20Gi"
    contentSize: "100Gi"
    storageClassName: "fast-ssd"

  percona:
    enabled: true
    deleteWithServer: false  # Preserve data
    replicas: 3
    storageSize: "50Gi"
    storageClassName: "fast-ssd"
    databaseName: "dittofs"
    backup:
      enabled: true
      bucket: "pg-backups-bucket"
      endpoint: "https://s3.cubbit.eu"
      region: "eu-west-1"
      credentialsSecretRef:
        name: pgbackrest-credentials
      fullSchedule: "0 2 * * *"
      incrSchedule: "0 * * * *"
      retentionDays: 14

  service:
    type: LoadBalancer

  metrics:
    enabled: true

  identity:
    jwt:
      secretRef:
        name: dittofs-jwt
        key: secret
    admin:
      passwordSecretRef:
        name: dittofs-admin
        key: password-hash

  s3:
    region: "eu-west-1"
    bucket: "dittofs-content"
    credentialsSecretRef:
      secretName: s3-credentials

  resources:
    limits:
      cpu: "4"
      memory: "8Gi"
    requests:
      cpu: "1"
      memory: "2Gi"
```

### With SMB Protocol Enabled

For environments requiring both NFS and SMB access:

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: dittofs-smb
spec:
  storage:
    metadataSize: "10Gi"
    cacheSize: "10Gi"

  nfsPort: 12049

  smb:
    enabled: true
    port: 445
    maxConnections: 200
    maxRequestsPerConnection: 50
    timeouts:
      read: "10m"
      write: "1m"
      idle: "10m"
      shutdown: "60s"
    credits:
      strategy: adaptive
      maxGrant: 4096
      maxSessionCredits: 32768

  service:
    type: LoadBalancer

  metrics:
    enabled: true
```

## Printer Columns

When listing DittoServer resources with `kubectl get dittoserver`, the following columns are displayed:

| Column | Source | Description |
|--------|--------|-------------|
| NAME | `metadata.name` | Resource name |
| Replicas | `spec.replicas` | Desired replicas |
| Ready | `status.readyReplicas` | Ready pod count |
| Available | `status.availableReplicas` | Available pod count |
| Status | `status.phase` | Current phase |
| Age | `metadata.creationTimestamp` | Resource age |

Example output:
```
NAME              REPLICAS   READY   AVAILABLE   STATUS    AGE
dittofs-minimal   1          1       1           Running   5m
dittofs-ha        1          1       1           Running   1h
```

## See Also

- [Installation Guide](INSTALL.md) - How to install the operator
- [Percona Integration](PERCONA.md) - Detailed Percona PostgreSQL setup
- [DittoFS Documentation](https://github.com/marmos91/dittofs) - Main project documentation
