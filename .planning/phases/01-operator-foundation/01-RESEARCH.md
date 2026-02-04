# Phase 1: Operator Foundation - Research

**Researched:** 2026-02-04
**Domain:** Kubernetes Operator Development (Go) with Operator SDK
**Confidence:** HIGH

## Summary

This phase establishes the foundational operator scaffold for deploying DittoFS on Kubernetes. Research reveals that a substantial operator implementation **already exists** in `dittofs-operator/` directory with:

1. **Complete CRD schema** (`DittoServer` v1alpha1) covering all DittoFS configuration options
2. **Working reconciliation loop** that creates ConfigMap, StatefulSet, and Service
3. **Config generation** transforming CRD spec to DittoFS YAML configuration
4. **RBAC markers** for core Kubernetes resources
5. **Status conditions** with phase tracking

The phase work is primarily **validation and reorganization** rather than greenfield development. Key tasks include:
- Moving operator to `k8s/dittofs-operator/` directory structure per requirements
- Validating CRD schema matches DittoFS configuration capabilities
- Ensuring sample CR works end-to-end
- Verifying RBAC covers all necessary permissions

**Primary recommendation:** Leverage the existing operator scaffold in `dittofs-operator/`, relocate to required directory structure, and validate end-to-end functionality with hardcoded memory stores.

## Standard Stack

The established libraries/tools for Kubernetes operator development in Go:

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Operator SDK | v1.42.0 | Operator scaffolding, OLM integration | Industry standard, wraps Kubebuilder with OLM support |
| controller-runtime | v0.22.4 | Controller logic, reconciliation loops | Kubernetes-SIG maintained, powers all Go operators |
| controller-gen | v0.18.0 | CRD/RBAC/DeepCopy generation | Required for API code generation via `make manifests` |
| Go | 1.24+ | Language runtime | Required by Operator SDK v1.41+ |
| k8s.io/client-go | v0.34.x | Kubernetes API client | Required dependency, tied to controller-runtime version |
| k8s.io/api | v0.34.x | Kubernetes API types | Core API types for Pods, Services, PVCs, StatefulSets |
| k8s.io/apimachinery | v0.34.x | API machinery utilities | Condition helpers, runtime.Object, meta/v1 types |

### Testing

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| sigs.k8s.io/controller-runtime/pkg/envtest | (matches controller-runtime) | Integration testing | Testing controller logic without full cluster |
| github.com/onsi/ginkgo/v2 | v2.22.0 | BDD test framework | Controller tests, async assertions |
| github.com/onsi/gomega | v1.36.1 | Matcher library | Pairs with Ginkgo, `Eventually` for async |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Operator SDK | Kubebuilder (raw) | No OLM integration, slightly simpler setup |
| StatefulSet | Deployment | StatefulSet provides stable network identity, ordered deployment |
| VolumeClaimTemplates | Explicit PVCs | VolumeClaimTemplates are managed by StatefulSet, cleaner lifecycle |

**Installation:**
```bash
# Operator SDK is already installed (verify with operator-sdk version)
# The existing go.mod has correct dependencies

# Key commands
make generate    # Generate DeepCopy methods
make manifests   # Generate CRDs, RBAC from markers
make docker-build IMG=<registry>/dittofs-operator:tag
make deploy IMG=<registry>/dittofs-operator:tag
```

## Architecture Patterns

### Existing Project Structure (in `dittofs-operator/`)

```
dittofs-operator/
├── api/
│   └── v1alpha1/
│       ├── dittoserver_types.go       # DittoServer CRD spec/status (COMPLETE)
│       ├── dittoserver_types_builder.go # Fluent builder for tests
│       ├── dittoserver_webhook.go     # Validation webhooks
│       ├── groupversion_info.go       # API group metadata
│       └── zz_generated.deepcopy.go   # Generated DeepCopy
│
├── internal/
│   └── controller/
│       ├── dittoserver_controller.go  # Reconciliation logic (COMPLETE)
│       ├── config/
│       │   ├── config.go              # CRD-to-YAML transformer (COMPLETE)
│       │   └── types.go               # DittoFS config types
│       └── suite_test.go              # Test setup
│
├── utils/
│   ├── conditions/                    # Status condition helpers
│   ├── nfs/                           # NFS port helpers
│   └── smb/                           # SMB port helpers
│
├── config/
│   ├── crd/bases/                     # Generated CRD YAML
│   ├── manager/                       # Controller manager deployment
│   ├── rbac/                          # RBAC resources
│   └── samples/                       # Example CRs (TO BE CREATED)
│
├── cmd/main.go                        # Operator entrypoint
├── Dockerfile                         # Operator container image
├── Makefile                           # Build/deploy automation
└── PROJECT                            # Kubebuilder project config
```

### Target Directory Structure (per R1.5)

```
k8s/
└── dittofs-operator/
    ├── api/v1alpha1/                  # CRD types (move from dittofs-operator/)
    ├── internal/controller/           # Controller logic (move from dittofs-operator/)
    ├── config/                        # Kubernetes manifests
    │   ├── crd/bases/
    │   ├── manager/
    │   ├── rbac/
    │   └── samples/
    │       └── dittofs_v1alpha1_dittofs.yaml  # Sample CR
    ├── cmd/main.go
    ├── Dockerfile
    ├── Makefile
    └── PROJECT
```

### Pattern 1: Controller Reconciliation with Owned Resources

**What:** Controller creates StatefulSet, ConfigMap, Service with owner references.

**When to use:** Always for operator-managed resources.

**Example (from existing code):**
```go
// Source: dittofs-operator/internal/controller/dittoserver_controller.go
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&dittoiov1alpha1.DittoServer{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Named("dittoserver").
        Complete(r)
}
```

### Pattern 2: CreateOrUpdate with Owner Reference

**What:** Idempotent resource creation/update with ownership tracking.

**When to use:** For all reconciled resources.

**Example (from existing code):**
```go
// Source: dittofs-operator/internal/controller/dittoserver_controller.go
_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
    if err := controllerutil.SetControllerReference(dittoServer, configMap, r.Scheme); err != nil {
        return err
    }
    // Set desired state
    configMap.Data = map[string]string{
        "config.yaml": configYAML,
    }
    return nil
})
```

### Pattern 3: Status Condition Updates

**What:** Use `meta.SetStatusCondition` for standardized status reporting.

**When to use:** After reconciliation to report state.

**Example (from existing code):**
```go
// Source: dittofs-operator/utils/conditions/conditions.go
conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation,
    "Ready", metav1.ConditionTrue, "StatefulSetReady",
    fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, replicas))
```

### Anti-Patterns to Avoid

- **Update() instead of Status().Update():** Causes infinite reconciliation loops. Always update status via status subresource.
- **Hardcoding namespace:** Operator should work in any namespace. Use `req.Namespace` from reconcile request.
- **Polling instead of watching:** Use `Owns()` to watch secondary resources, not periodic requeue.

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Status conditions | Manual condition list management | `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition` | Handles deduplication, LastTransitionTime automatically |
| Owner references | Manual OwnerReference construction | `controllerutil.SetControllerReference` | Handles UID, BlockOwnerDeletion correctly |
| Resource create/update | Separate Get+Create or Update | `controllerutil.CreateOrUpdate` | Idempotent, handles optimistic locking |
| CRD generation | Manual YAML | controller-gen markers | Type-safe, always in sync with Go types |
| RBAC manifests | Manual ClusterRole YAML | controller-gen RBAC markers | Generated from code, can't drift |
| Container probes | Custom health endpoints | Kubernetes native probes | HTTP GET on /health already implemented in DittoFS |

**Key insight:** The existing operator already follows these patterns correctly. The work is validation, not reimplementation.

## Common Pitfalls

### Pitfall 1: ConfigMap Changes Don't Trigger Pod Restart

**What goes wrong:** User updates CRD spec, ConfigMap updates, but pod keeps running with old config.

**Why it happens:** Kubernetes doesn't automatically restart pods when mounted ConfigMaps change.

**How to avoid:** Implement checksum annotation pattern:
```go
// Add to pod template annotations
configMapHash := sha256.Sum256([]byte(configYAML))
podTemplate.Annotations["config-checksum"] = hex.EncodeToString(configMapHash[:])
```

**Warning signs:** Config changes "not taking effect", ConfigMap updated but pod unchanged.

**Note:** The existing operator does NOT implement this pattern. This should be addressed in Phase 2.

### Pitfall 2: Status Update Causes Infinite Reconciliation

**What goes wrong:** Every status update triggers reconciliation, leading to CPU spike.

**Why it happens:** Using `Update()` instead of `Status().Update()`, or status always changing.

**How to avoid:**
1. Use `Status().Update()` (already done in existing code)
2. Only update status when it actually changes
3. Use `meta.SetStatusCondition` which handles deduplication

**Warning signs:** Controller logs show reconciliation every few seconds.

### Pitfall 3: Missing RBAC for Secrets

**What goes wrong:** Operator fails to read Secrets referenced in CRD spec (for S3 credentials, passwords).

**Why it happens:** RBAC markers don't include Secrets permission.

**How to avoid:** Add RBAC marker:
```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
```

**Warning signs:** "secrets is forbidden" errors in operator logs.

**Note:** The existing operator DOES read secrets (for JWT, user passwords, S3 credentials) but the RBAC marker for secrets is **MISSING**. This must be fixed.

### Pitfall 4: CRD Naming Mismatch

**What goes wrong:** `kubectl get dittofs` doesn't work because CRD is named `DittoServer`.

**Why it happens:** The existing CRD uses `DittoServer` as the Kind, but requirements mention `DittoFS`.

**How to avoid:** Either:
1. Rename CRD Kind from `DittoServer` to `DittoFS` (breaking change)
2. Add shortName `dittofs` to CRD (already done: `+kubebuilder:resource:shortName=ditto`)
3. Accept current naming and document it

**Recommendation:** Keep `DittoServer` naming but add `dittofs` as additional shortName for user convenience.

### Pitfall 5: StatefulSet VolumeClaimTemplate Updates

**What goes wrong:** Changing storage size in CRD doesn't update PVC.

**Why it happens:** VolumeClaimTemplates are immutable after StatefulSet creation.

**How to avoid:** Document that storage size changes require:
1. Delete StatefulSet (with `--cascade=orphan`)
2. Manually resize PVCs (if StorageClass supports it)
3. Recreate StatefulSet

**Warning signs:** "field is immutable" errors for StatefulSet updates.

## Code Examples

Verified patterns from the existing operator implementation:

### Reconcile Function Signature

```go
// Source: dittofs-operator/internal/controller/dittoserver_controller.go
func (r *DittoServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := logf.FromContext(ctx)

    dittoServer := &dittoiov1alpha1.DittoServer{}
    if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Reconcile owned resources...
    if err := r.reconcileConfigMap(ctx, dittoServer); err != nil {
        logger.Error(err, "Failed to reconcile ConfigMap")
        return ctrl.Result{}, err
    }

    // Update status...
    if err := r.Status().Update(ctx, dittoServerCopy); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

### RBAC Markers

```go
// Source: dittofs-operator/internal/controller/dittoserver_controller.go
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// MISSING - needs to be added:
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
```

### CRD Type Definition with Markers

```go
// Source: dittofs-operator/api/v1alpha1/dittoserver_types.go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ditto
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="NFS Endpoint",type=string,JSONPath=`.status.nfsEndpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type DittoServer struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   DittoServerSpec   `json:"spec,omitempty"`
    Status DittoServerStatus `json:"status,omitempty"`
}
```

### StatefulSet with VolumeClaimTemplates

```go
// Source: dittofs-operator/internal/controller/dittoserver_controller.go
statefulSet.Spec = appsv1.StatefulSetSpec{
    Replicas:    &replicas,
    ServiceName: dittoServer.Name,
    Selector: &metav1.LabelSelector{
        MatchLabels: labels,
    },
    Template: corev1.PodTemplateSpec{
        // ... pod template spec
    },
    VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
        {
            ObjectMeta: metav1.ObjectMeta{Name: "metadata"},
            Spec: corev1.PersistentVolumeClaimSpec{
                AccessModes: []corev1.PersistentVolumeAccessMode{
                    corev1.ReadWriteOnce,
                },
                StorageClassName: dittoServer.Spec.Storage.StorageClassName,
                Resources: corev1.VolumeResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceStorage: metadataSize,
                    },
                },
            },
        },
    },
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| kube-rbac-proxy | controller-runtime native authn/authz | March 2025 | kube-rbac-proxy deprecated, GCR images unavailable |
| gcr.io/kubebuilder/* images | Official registry images | March 2025 | Use quay.io or custom registry |
| Logrus logging | controller-runtime/pkg/log with Zap | 2024 | Structured logging integrated with controller context |

**Deprecated/outdated:**
- **kube-rbac-proxy:** Discontinued. The existing operator doesn't use it (good).
- **gcr.io images:** The operator Dockerfile builds from scratch (good).

## Open Questions

Things that couldn't be fully resolved:

1. **Directory Location**
   - What we know: Requirements specify `k8s/dittofs-operator/`, existing code is in `dittofs-operator/`
   - What's unclear: Whether this is a hard requirement or preference
   - Recommendation: Move to `k8s/dittofs-operator/` per requirements, update go.mod module path

2. **CRD Naming: DittoServer vs DittoFS**
   - What we know: Existing CRD is `DittoServer`, requirements mention "DittoFS CRD"
   - What's unclear: Whether renaming is required
   - Recommendation: Keep `DittoServer` (breaking change risk), add `dittofs` shortName

3. **Hardcoded Config for Phase 1**
   - What we know: Phase 1 success criteria mentions "hardcoded config, memory stores"
   - What's unclear: How much of the existing full CRD spec to use
   - Recommendation: Create a minimal sample CR using memory stores, existing config generation works

4. **Cache PVC Missing**
   - What we know: DittoFS config requires cache path, existing operator only creates metadata/content PVCs
   - What's unclear: Whether cache should use emptyDir or PVC
   - Recommendation: For Phase 1 (memory stores), use emptyDir. Add cache PVC in Phase 3.

## Sources

### Primary (HIGH confidence)
- [Operator SDK Go Tutorial](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/) - Scaffolding, API creation, reconciliation
- [Kubebuilder Book - RBAC Markers](https://book.kubebuilder.io/reference/markers/rbac) - RBAC marker syntax
- [Kubebuilder Book - CRD Generation](https://book.kubebuilder.io/reference/generating-crd) - CRD markers
- [Kubebuilder Book - Secondary Resources](https://book.kubebuilder.io/reference/watching-resources/secondary-owned-resources) - Owner references, Owns()
- [Kubebuilder Book - Controller Implementation](https://book.kubebuilder.io/cronjob-tutorial/controller-implementation) - Reconcile patterns

### Secondary (MEDIUM confidence)
- [Kubernetes Operators 2025 Guide](https://outerbyte.com/kubernetes-operators-2025-guide/) - Best practices, patterns
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/common-recommendation/) - Common recommendations
- [Operator SDK GitHub Releases](https://github.com/operator-framework/operator-sdk/releases) - v1.42.0 release notes

### Existing Implementation (HIGH confidence)
- `/Users/marmos91/Projects/dittofs/dittofs-operator/` - Working operator scaffold
- `/Users/marmos91/Projects/dittofs/docs/CONFIGURATION.md` - DittoFS configuration reference

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Official documentation, existing implementation uses correct patterns
- Architecture: HIGH - Existing implementation follows Kubebuilder conventions
- Pitfalls: HIGH - Verified against multiple sources and existing code review

**Research date:** 2026-02-04
**Valid until:** 60 days (mature ecosystem, stable patterns)

---

## Summary for Planner

**Key Facts:**
1. Operator scaffold EXISTS and is mostly complete in `dittofs-operator/`
2. CRD (`DittoServer`) has full spec covering all DittoFS config options
3. Reconciler creates ConfigMap, StatefulSet, Service with owner references
4. RBAC markers exist but MISSING secrets permission
5. Move to `k8s/dittofs-operator/` required per R1.5

**Phase 1 Work:**
1. Relocate operator to `k8s/dittofs-operator/`
2. Add RBAC marker for secrets
3. Create sample CR with memory stores
4. Validate end-to-end: CR -> StatefulSet -> Pod running
5. Add `dittofs` shortName to CRD if not present

**Not in Scope for Phase 1:**
- ConfigMap checksum annotation (Phase 2)
- Cache PVC (Phase 3)
- PostgreSQL integration (Phase 4)
- Finalizers (Phase 5)
