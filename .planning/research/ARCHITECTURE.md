# Architecture Research: DittoFS Kubernetes Operator

**Domain:** Kubernetes Operator for Stateful Application (NFS/SMB filesystem server)
**Researched:** 2026-02-04
**Confidence:** MEDIUM

## Executive Summary

This document describes the recommended architecture for a Kubernetes operator that deploys and manages DittoFS instances. The operator follows established Kubernetes operator patterns with specific considerations for:

1. **External operator dependency**: PostgreSQL managed by Percona Operator
2. **Stateful storage requirements**: BadgerDB metadata and filesystem payload stores via PVCs
3. **Multi-protocol service exposure**: NFS (TCP 2049), SMB (TCP 445), REST API (TCP 8080)
4. **Configuration generation**: ConfigMaps derived from CRD specifications

The architecture uses the **controller-runtime** pattern with Kubebuilder scaffolding, implementing separate controllers for each CRD to maintain separation of concerns.

## System Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          DittoFS Operator                                   │
│                    (Controller Manager Pod)                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────┐  ┌─────────────────────┐  ┌─────────────────────┐ │
│  │  DittoFS Controller │  │  Share Controller   │  │  Backup Controller  │ │
│  │  (main reconciler)  │  │  (share lifecycle)  │  │  (backup/restore)   │ │
│  └──────────┬──────────┘  └──────────┬──────────┘  └──────────┬──────────┘ │
│             │                        │                        │             │
│             └────────────────────────┼────────────────────────┘             │
│                                      │                                      │
└──────────────────────────────────────┼──────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes API Server                                  │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Custom Resources (CRDs)              │  Generated Resources                 │
│  ┌─────────────────────────────────┐  │  ┌─────────────────────────────────┐│
│  │ dittofs.dittofs.io/v1alpha1     │  │  │ ConfigMap (dittofs-config)      ││
│  │   - DittoFS                     │──┼─▶│ StatefulSet (dittofs)           ││
│  │   - DittoFSShare                │  │  │ Service (nfs, smb, api)         ││
│  │   - DittoFSBackup               │  │  │ PVC (metadata, payload, cache)  ││
│  └─────────────────────────────────┘  │  │ Secret (jwt-secret)             ││
│                                       │  └─────────────────────────────────┘│
│  External Resources (watched)         │                                      │
│  ┌─────────────────────────────────┐  │                                      │
│  │ pgv2.percona.com/v2             │  │                                      │
│  │   - PerconaPGCluster            │──┼──▶ PostgreSQL connection details    │
│  └─────────────────────────────────┘  │                                      │
│                                       │                                      │
└───────────────────────────────────────┴──────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                          Running DittoFS Instance                             │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  StatefulSet: dittofs                                                        │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │  Pod: dittofs-0                                                          ││
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐          ││
│  │  │ NFS Port 2049   │  │ SMB Port 445    │  │ API Port 8080   │          ││
│  │  └────────┬────────┘  └────────┬────────┘  └────────┬────────┘          ││
│  │           │                    │                    │                    ││
│  │           └────────────────────┼────────────────────┘                    ││
│  │                                ▼                                         ││
│  │  ┌─────────────────────────────────────────────────────────────────────┐││
│  │  │                      DittoFS Runtime                                │││
│  │  │                                                                     │││
│  │  │  ConfigMap Mount: /etc/dittofs/config.yaml                          │││
│  │  │  ┌───────────────┐ ┌───────────────┐ ┌───────────────┐              │││
│  │  │  │ PostgreSQL    │ │ BadgerDB PVC  │ │ Filesystem    │              │││
│  │  │  │ (via Percona) │ │ /data/meta    │ │ PVC /data/pay │              │││
│  │  │  │ Metadata      │ │ Metadata Alt  │ │ Payload Store │              │││
│  │  │  └───────┬───────┘ └───────┬───────┘ └───────┬───────┘              │││
│  │  │          │                 │                 │                      │││
│  │  │          └─────────────────┼─────────────────┘                      │││
│  │  │                            ▼                                        │││
│  │  │                     Cache PVC: /data/cache                          │││
│  │  └─────────────────────────────────────────────────────────────────────┘││
│  └─────────────────────────────────────────────────────────────────────────┘│
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

## Component Responsibilities

| Component | Responsibility | Communicates With |
|-----------|----------------|-------------------|
| **DittoFS Controller** | Main reconciler: creates ConfigMap, StatefulSet, Services, PVCs; watches PerconaPGCluster for connection details | Kubernetes API, Percona Operator CRDs |
| **Share Controller** | Manages DittoFSShare resources; updates ConfigMap with share definitions; signals DittoFS pods to reload | DittoFS Controller (via ConfigMap), DittoFS REST API |
| **Backup Controller** | Handles backup/restore operations using DittoFS backup CLI | DittoFS REST API, External storage (S3/PVC) |
| **ConfigMap Generator** | Transforms CRD spec into DittoFS YAML configuration | Embedded in DittoFS Controller |
| **StatefulSet** | Runs DittoFS pod(s) with mounted configuration and PVCs | ConfigMap, PVCs, Services |
| **Services** | Expose NFS (ClusterIP/LoadBalancer), SMB (ClusterIP), REST API (ClusterIP) | External clients, other operators |

## Recommended Project Structure

```
dittofs-operator/
├── api/
│   └── v1alpha1/
│       ├── dittofs_types.go           # DittoFS CRD spec/status
│       ├── dittofs_share_types.go     # DittoFSShare CRD spec/status
│       ├── dittofs_backup_types.go    # DittoFSBackup CRD spec/status
│       ├── groupversion_info.go       # API group metadata
│       └── zz_generated.deepcopy.go   # Generated deepcopy
│
├── internal/
│   └── controller/
│       ├── dittofs_controller.go      # Main DittoFS reconciler
│       ├── dittofs_controller_test.go
│       ├── share_controller.go        # Share reconciler
│       ├── share_controller_test.go
│       ├── backup_controller.go       # Backup reconciler
│       ├── backup_controller_test.go
│       └── suite_test.go              # Controller test suite
│
├── pkg/
│   ├── configgen/
│   │   ├── configgen.go               # CRD-to-ConfigMap transformer
│   │   └── configgen_test.go
│   ├── resources/
│   │   ├── configmap.go               # ConfigMap builder
│   │   ├── statefulset.go             # StatefulSet builder
│   │   ├── service.go                 # Service builder
│   │   ├── pvc.go                     # PVC builder
│   │   └── secret.go                  # Secret builder
│   └── percona/
│       ├── client.go                  # Percona PGCluster watcher
│       └── connection.go              # Connection string builder
│
├── config/
│   ├── crd/
│   │   └── bases/                     # Generated CRD YAML
│   ├── manager/
│   │   └── manager.yaml               # Controller manager deployment
│   ├── rbac/                          # RBAC resources
│   ├── samples/                       # Example CRs
│   └── default/                       # Kustomize overlay
│
├── cmd/
│   └── main.go                        # Operator entrypoint
│
├── Dockerfile                         # Operator container image
├── Makefile                           # Build/deploy automation
├── PROJECT                            # Kubebuilder project config
└── go.mod
```

### Structure Rationale

- **api/v1alpha1/:** All CRD type definitions in one version directory. Enables clear API evolution.
- **internal/controller/:** Private controller implementations. One controller per CRD following Kubebuilder conventions.
- **pkg/configgen/:** Reusable ConfigMap generation logic. Separates transformation from reconciliation.
- **pkg/resources/:** Builder pattern for Kubernetes resources. Makes resource creation testable and reusable.
- **pkg/percona/:** Encapsulates Percona operator integration. Isolates external dependency.

## Architectural Patterns

### Pattern 1: External Operator Dependency (Percona PostgreSQL)

**What:** Watch resources created by another operator (PerconaPGCluster) and extract connection details.

**When to use:** When your application depends on resources managed by a third-party operator.

**Trade-offs:**
- Pro: Leverages battle-tested database operator
- Pro: Separation of concerns (database lifecycle vs application lifecycle)
- Con: Coupling to external CRD schema
- Con: Must handle "not ready" states gracefully

**Implementation:**

```go
// Watch PerconaPGCluster as external resource (no ownerReference)
func (r *DittoFSReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&dittofsv1alpha1.DittoFS{}).
        Owns(&corev1.ConfigMap{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.PersistentVolumeClaim{}).
        // Watch external PerconaPGCluster - triggers reconcile when status changes
        Watches(
            &source.Kind{Type: &pgv2.PerconaPGCluster{}},
            handler.EnqueueRequestsFromMapFunc(r.findDittoFSForPGCluster),
            builder.WithPredicates(predicate.GenerationChangedPredicate{}),
        ).
        Complete(r)
}

// Find DittoFS instances that reference this PGCluster
func (r *DittoFSReconciler) findDittoFSForPGCluster(obj client.Object) []reconcile.Request {
    pgCluster := obj.(*pgv2.PerconaPGCluster)

    // Find all DittoFS instances referencing this cluster
    var dittoFSList dittofsv1alpha1.DittoFSList
    if err := r.List(context.TODO(), &dittoFSList, client.InNamespace(pgCluster.Namespace)); err != nil {
        return nil
    }

    var requests []reconcile.Request
    for _, dfs := range dittoFSList.Items {
        if dfs.Spec.Database.PostgresRef.Name == pgCluster.Name {
            requests = append(requests, reconcile.Request{
                NamespacedName: types.NamespacedName{
                    Name:      dfs.Name,
                    Namespace: dfs.Namespace,
                },
            })
        }
    }
    return requests
}

// Extract connection details from PerconaPGCluster status
func (r *DittoFSReconciler) getPostgresConnection(ctx context.Context, ref *PostgresRef, namespace string) (*PostgresConnection, error) {
    var pgCluster pgv2.PerconaPGCluster
    if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, &pgCluster); err != nil {
        return nil, err
    }

    // Check if cluster is ready
    if pgCluster.Status.State != "ready" {
        return nil, ErrDatabaseNotReady
    }

    // Get connection secret
    var secret corev1.Secret
    secretName := fmt.Sprintf("%s-pguser-%s", ref.Name, ref.User)
    if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, &secret); err != nil {
        return nil, err
    }

    return &PostgresConnection{
        Host:     string(secret.Data["host"]),
        Port:     string(secret.Data["port"]),
        Database: string(secret.Data["dbname"]),
        User:     string(secret.Data["user"]),
        Password: string(secret.Data["password"]),
    }, nil
}
```

### Pattern 2: ConfigMap Generation from CRD Spec

**What:** Transform CRD specification into DittoFS configuration YAML stored in a ConfigMap.

**When to use:** When your application reads configuration from a file rather than environment variables.

**Trade-offs:**
- Pro: Single source of truth in CRD
- Pro: Familiar configuration format for DittoFS
- Pro: Supports complex nested configuration
- Con: ConfigMap size limits (1MB)
- Con: Pod restart may be needed for config changes

**Implementation:**

```go
// CRD Spec (simplified)
type DittoFSSpec struct {
    // Database configuration
    Database DatabaseSpec `json:"database"`

    // Cache configuration
    Cache CacheSpec `json:"cache"`

    // Metadata store configuration
    Metadata MetadataSpec `json:"metadata"`

    // Payload store configuration
    Payload PayloadSpec `json:"payload"`

    // Protocol adapter configuration
    Adapters AdaptersSpec `json:"adapters"`

    // Image and resources
    Image    string                      `json:"image"`
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type DatabaseSpec struct {
    // Reference to PerconaPGCluster
    PostgresRef *PostgresRef `json:"postgresRef,omitempty"`
    // Or inline SQLite config
    SQLite *SQLiteSpec `json:"sqlite,omitempty"`
}

type PostgresRef struct {
    Name string `json:"name"`
    User string `json:"user"`
}

// ConfigMap generator
func GenerateConfigMap(dfs *dittofsv1alpha1.DittoFS, pgConn *PostgresConnection) (*corev1.ConfigMap, error) {
    config := map[string]interface{}{
        "logging": map[string]interface{}{
            "level":  "INFO",
            "format": "json",
        },
        "cache": map[string]interface{}{
            "path": "/data/cache",
            "size": dfs.Spec.Cache.Size,
        },
        "database": buildDatabaseConfig(dfs.Spec.Database, pgConn),
        "metadata": buildMetadataConfig(dfs.Spec.Metadata),
        "payload":  buildPayloadConfig(dfs.Spec.Payload),
        "adapters": buildAdaptersConfig(dfs.Spec.Adapters),
    }

    yamlBytes, err := yaml.Marshal(config)
    if err != nil {
        return nil, err
    }

    return &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fmt.Sprintf("%s-config", dfs.Name),
            Namespace: dfs.Namespace,
            Labels:    labelsForDittoFS(dfs),
        },
        Data: map[string]string{
            "config.yaml": string(yamlBytes),
        },
    }, nil
}
```

### Pattern 3: Stateful Application with PVCs

**What:** Use StatefulSet with PersistentVolumeClaims for durable storage of BadgerDB metadata, filesystem payload, and cache WAL.

**When to use:** When application requires persistent storage that survives pod restarts.

**Trade-offs:**
- Pro: Data persists across restarts and rescheduling
- Pro: StatefulSet provides stable network identity
- Con: PVC binding can delay pod startup
- Con: Storage class must support required access modes

**Implementation:**

```go
func buildStatefulSet(dfs *dittofsv1alpha1.DittoFS, configMapName string) *appsv1.StatefulSet {
    return &appsv1.StatefulSet{
        ObjectMeta: metav1.ObjectMeta{
            Name:      dfs.Name,
            Namespace: dfs.Namespace,
            Labels:    labelsForDittoFS(dfs),
        },
        Spec: appsv1.StatefulSetSpec{
            Replicas:    ptr.To(int32(1)), // Single replica for NFS consistency
            ServiceName: fmt.Sprintf("%s-headless", dfs.Name),
            Selector: &metav1.LabelSelector{
                MatchLabels: labelsForDittoFS(dfs),
            },
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{
                    Labels: labelsForDittoFS(dfs),
                },
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{{
                        Name:  "dittofs",
                        Image: dfs.Spec.Image,
                        Args:  []string{"start", "--config", "/etc/dittofs/config.yaml"},
                        Ports: []corev1.ContainerPort{
                            {Name: "nfs", ContainerPort: 2049, Protocol: corev1.ProtocolTCP},
                            {Name: "smb", ContainerPort: 445, Protocol: corev1.ProtocolTCP},
                            {Name: "api", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
                        },
                        VolumeMounts: []corev1.VolumeMount{
                            {Name: "config", MountPath: "/etc/dittofs", ReadOnly: true},
                            {Name: "metadata", MountPath: "/data/metadata"},
                            {Name: "payload", MountPath: "/data/payload"},
                            {Name: "cache", MountPath: "/data/cache"},
                        },
                        Resources: dfs.Spec.Resources,
                        LivenessProbe: &corev1.Probe{
                            ProbeHandler: corev1.ProbeHandler{
                                HTTPGet: &corev1.HTTPGetAction{
                                    Path: "/health",
                                    Port: intstr.FromString("api"),
                                },
                            },
                            InitialDelaySeconds: 10,
                            PeriodSeconds:       30,
                        },
                    }},
                    Volumes: []corev1.Volume{{
                        Name: "config",
                        VolumeSource: corev1.VolumeSource{
                            ConfigMap: &corev1.ConfigMapVolumeSource{
                                LocalObjectReference: corev1.LocalObjectReference{
                                    Name: configMapName,
                                },
                            },
                        },
                    }},
                },
            },
            VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
                buildPVC("metadata", dfs.Spec.Metadata.Storage),
                buildPVC("payload", dfs.Spec.Payload.Storage),
                buildPVC("cache", dfs.Spec.Cache.Storage),
            },
        },
    }
}
```

### Pattern 4: Multi-Controller Architecture (One CRD Per Controller)

**What:** Separate controllers for DittoFS, DittoFSShare, and DittoFSBackup resources.

**When to use:** When CRDs represent distinct lifecycle concerns that should be managed independently.

**Trade-offs:**
- Pro: Clear separation of concerns
- Pro: Independent scaling and testing
- Pro: Follows controller-runtime best practices
- Con: Cross-CRD coordination requires careful design
- Con: More boilerplate code

**Controller responsibilities:**

| Controller | Owns | Watches | Creates |
|------------|------|---------|---------|
| DittoFS | DittoFS CR | PerconaPGCluster, Secret | ConfigMap, StatefulSet, Service, PVC |
| Share | DittoFSShare CR | DittoFS status | Updates ConfigMap (via DittoFS controller) |
| Backup | DittoFSBackup CR | DittoFS status | Job (backup command), PVC (backup target) |

## Data Flow

### CRD Change to Running DittoFS

```
User creates/updates DittoFS CR
           │
           ▼
┌─────────────────────────────┐
│ DittoFS Controller watches  │
│ dittofs.dittofs.io/v1alpha1 │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐    ┌─────────────────────────┐
│ Check PerconaPGCluster      │───▶│ Get connection details  │
│ readiness                   │    │ from Secret             │
└──────────────┬──────────────┘    └─────────────┬───────────┘
               │                                  │
               ▼                                  │
┌─────────────────────────────┐                  │
│ Generate ConfigMap YAML     │◀─────────────────┘
│ (embed PG connection)       │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Create/Update ConfigMap     │
│ (owner: DittoFS CR)         │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Create/Update StatefulSet   │
│ (mounts ConfigMap)          │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Create/Update Services      │
│ (NFS, SMB, API)             │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Update DittoFS status       │
│ (ready/not ready)           │
└─────────────────────────────┘
```

### Share Configuration Flow

```
User creates DittoFSShare CR
           │
           ▼
┌─────────────────────────────┐
│ Share Controller watches    │
│ dittofs.dittofs.io/v1alpha1 │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Find referenced DittoFS     │
│ instance                    │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Check DittoFS is Ready      │
│ (status.ready == true)      │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ Update DittoFS annotation   │
│ to trigger reconcile        │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ DittoFS Controller          │
│ regenerates ConfigMap       │
│ (includes all shares)       │
└──────────────┬──────────────┘
               │
               ▼
┌─────────────────────────────┐
│ StatefulSet pod restarts    │
│ (ConfigMap changed)         │
│ OR                          │
│ Call DittoFS reload API     │
└─────────────────────────────┘
```

## Anti-Patterns

### Anti-Pattern 1: Single Controller for Multiple CRDs

**What people do:** One controller that manages DittoFS, Share, and Backup in a single reconcile loop.

**Why it's wrong:**
- Violates Single Responsibility Principle
- Harder to test individual features
- Reconciliation becomes complex with many conditional branches
- Scaling one feature requires scaling all features

**Do this instead:** Implement separate controllers for each CRD. Use status fields and annotations for cross-CRD coordination.

### Anti-Pattern 2: Direct Child Resource Modification

**What people do:** Manually edit the ConfigMap or StatefulSet created by the operator.

**Why it's wrong:**
- Operator will overwrite changes on next reconcile (config drift)
- Creates confusion about source of truth
- Breaks declarative model

**Do this instead:** All configuration changes go through CRD. Implement `spec.configOverrides` for escape hatches.

### Anti-Pattern 3: Polling Instead of Watching

**What people do:** Use `RequeueAfter` to periodically check external resource status.

**Why it's wrong:**
- Wastes API server resources
- Slow to react to changes
- Unnecessary reconciliation loops

**Do this instead:** Use `Watches()` with appropriate predicates to trigger reconciliation only when relevant changes occur.

### Anti-Pattern 4: Missing Finalizers for External Resources

**What people do:** Delete CRD without cleaning up resources not covered by ownerReferences (cross-namespace resources, external API calls).

**Why it's wrong:**
- Orphaned resources accumulate
- Security risk (dangling database credentials)
- Cost implications (unused cloud resources)

**Do this instead:** Add finalizers when creating resources that need explicit cleanup. Remove finalizer only after cleanup completes.

```go
const dittoFSFinalizer = "dittofs.dittofs.io/finalizer"

func (r *DittoFSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var dfs dittofsv1alpha1.DittoFS
    if err := r.Get(ctx, req.NamespacedName, &dfs); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Handle deletion
    if !dfs.ObjectMeta.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(&dfs, dittoFSFinalizer) {
            // Perform cleanup
            if err := r.cleanupExternalResources(ctx, &dfs); err != nil {
                return ctrl.Result{}, err
            }

            // Remove finalizer
            controllerutil.RemoveFinalizer(&dfs, dittoFSFinalizer)
            if err := r.Update(ctx, &dfs); err != nil {
                return ctrl.Result{}, err
            }
        }
        return ctrl.Result{}, nil
    }

    // Add finalizer if not present
    if !controllerutil.ContainsFinalizer(&dfs, dittoFSFinalizer) {
        controllerutil.AddFinalizer(&dfs, dittoFSFinalizer)
        if err := r.Update(ctx, &dfs); err != nil {
            return ctrl.Result{}, err
        }
    }

    // Normal reconciliation...
}
```

## Integration Points

### External Services

| Service | Integration Pattern | Notes |
|---------|---------------------|-------|
| **Percona PGCluster** | Watch CRD status, read connection Secret | Must handle "not ready" state gracefully; requeue with backoff |
| **S3 (payload store)** | Pass credentials via Secret mounted as env vars | Use IRSA on AWS EKS; workload identity on GKE |
| **Metrics (Prometheus)** | ServiceMonitor CR if prometheus-operator installed | Expose DittoFS metrics on :9090 |
| **Scaleway Kubernetes** | Standard LoadBalancer service for NFS | May need annotation for external IP allocation |

### Internal Boundaries

| Boundary | Communication | Notes |
|----------|---------------|-------|
| DittoFS Controller <-> Share Controller | Share Controller triggers DittoFS reconcile via annotation update | Avoids tight coupling; maintains single source of truth |
| DittoFS Controller <-> Backup Controller | Backup Controller reads DittoFS status for endpoint discovery | One-way dependency; backup won't run if DittoFS not ready |
| ConfigMap <-> StatefulSet | StatefulSet watches ConfigMap hash annotation | Pod restart on config change (or use config reload API) |

## Scaling Considerations

| Scale | Architecture Adjustments |
|-------|--------------------------|
| 1-5 DittoFS instances | Single operator deployment is sufficient |
| 5-20 instances | Increase operator replicas, use leader election |
| 20+ instances | Consider sharding by namespace or label selector |

### Scaling Priorities

1. **First bottleneck:** API server rate limiting - use exponential backoff, cache reads
2. **Second bottleneck:** Controller memory - implement pagination for list operations

## Build Order Implications

Based on component dependencies, recommended implementation phases:

### Phase 1: Core Operator Infrastructure (Foundation)
- CRD definitions (DittoFS, DittoFSShare)
- Basic DittoFS Controller skeleton
- ConfigMap generation from spec
- StatefulSet creation (no Percona integration yet)
- Service creation (NFS, API)

**Dependency:** None. This is the foundation.

### Phase 2: Percona PostgreSQL Integration
- PerconaPGCluster watching
- Connection Secret extraction
- Database configuration injection into ConfigMap
- Readiness gating (DittoFS not ready until PG ready)

**Dependency:** Phase 1 (controller exists to add watching to)

### Phase 3: Storage Configuration
- PVC creation for BadgerDB, filesystem, cache
- StorageClass configuration
- Volume mount setup in StatefulSet

**Dependency:** Phase 1 (StatefulSet exists to add volumes to)

### Phase 4: Share Controller
- DittoFSShare CRD and controller
- Cross-controller coordination
- ConfigMap regeneration trigger
- Hot reload or pod restart strategy

**Dependency:** Phase 1, Phase 2 (working DittoFS instance)

### Phase 5: Production Readiness
- Finalizers for cleanup
- Status conditions (Ready, Available, Degraded)
- Events for debugging
- RBAC fine-tuning
- Health checks and probes

**Dependency:** All previous phases

### Phase 6: Backup Controller (Optional)
- DittoFSBackup CRD and controller
- Backup Job creation
- Restore workflow

**Dependency:** Phase 4 (Share Controller for complete system)

## Status Conditions

Following Kubernetes conventions, DittoFS should expose standard conditions:

```go
type DittoFSStatus struct {
    // Conditions represent the latest available observations
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // Ready indicates the DittoFS instance is serving requests
    Ready bool `json:"ready"`

    // Phase provides a simple, high-level summary
    Phase DittoFSPhase `json:"phase,omitempty"`

    // Endpoint provides the connection information
    Endpoint string `json:"endpoint,omitempty"`
}

// Recommended conditions
const (
    ConditionDatabaseReady  = "DatabaseReady"
    ConditionConfigReady    = "ConfigReady"
    ConditionStatefulSetReady = "StatefulSetReady"
    ConditionAvailable      = "Available"
)
```

## Sources

**Official Documentation (HIGH confidence):**
- [Kubernetes Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Kubebuilder Good Practices](https://book.kubebuilder.io/reference/good-practices)
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/)
- [Kubernetes Finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- [Kubebuilder Using Finalizers](https://book.kubebuilder.io/reference/using-finalizers)
- [Kubebuilder Watching Resources](https://book.kubebuilder.io/reference/watching-resources)

**Percona Operator (MEDIUM confidence):**
- [Percona Operator for PostgreSQL](https://docs.percona.com/percona-operator-for-postgresql/index.html)
- [Percona Operator GitHub](https://github.com/percona/percona-postgresql-operator)
- [Percona Operator 2025 Wrap Up](https://www.percona.com/blog/percona-operator-for-postgresql-2025-wrap-up-and-what-we-are-focusing-on-next/)

**Community Patterns (MEDIUM confidence):**
- [Google Cloud - Best practices for building Kubernetes Operators](https://cloud.google.com/blog/products/containers-kubernetes/best-practices-for-building-kubernetes-operators)
- [Operator SDK Common Recommendations](https://sdk.operatorframework.io/docs/best-practices/common-recommendation/)
- [Kubernetes Operators 2025 Guide](https://outerbyte.com/kubernetes-operators-2025-guide/)
- [iximiuz - Exploring Kubernetes Operator Pattern](https://iximiuz.com/en/posts/kubernetes-operator-pattern/)

**Cross-Operator Patterns (MEDIUM confidence):**
- [Kubebuilder For vs Owns vs Watches](https://yash-kukreja-98.medium.com/develop-on-kubernetes-series-demystifying-the-for-vs-owns-vs-watches-controller-builders-in-c11ab32a046e)
- [Azure Service Operator - Type References and Ownership](https://azure.github.io/azure-service-operator/design/type-references-and-ownership/)

---
*Architecture research for: DittoFS Kubernetes Operator*
*Researched: 2026-02-04*
