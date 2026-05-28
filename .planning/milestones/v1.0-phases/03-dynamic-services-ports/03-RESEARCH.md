# Phase 3: Dynamic Services & Ports - Research

**Researched:** 2026-02-10
**Domain:** K8s operator Service lifecycle management, StatefulSet port reconciliation, owner references, event recording
**Confidence:** HIGH

## Summary

Phase 3 transforms the operator from static infrastructure management to dynamic per-adapter resource management. The adapter state data populated by Phase 2 (`getLastKnownAdapters()`) drives the creation and deletion of dedicated K8s Services and the update of StatefulSet container ports. Each running adapter gets its own LoadBalancer Service (configurable type), and when adapters stop or are removed, their corresponding Services are deleted.

The existing codebase provides all the building blocks: `ServiceBuilder` for fluent Service construction, `createOrUpdateService` with cloud controller field preservation, `retryOnConflict` for optimistic locking, `conditions.SetCondition` for condition management, and `record.EventRecorder` for K8s events. The primary new code is a `reconcileAdapterServices` function that computes the diff between desired state (from adapter polling) and actual state (Services in the cluster), then creates/deletes Services and updates container ports accordingly.

**Primary recommendation:** Implement a single `service_reconciler.go` file containing the adapter Service lifecycle logic. Use a label selector (`dittofs.io/adapter-service: "true"`, `dittofs.io/adapter-type: "nfs"`) to identify operator-managed adapter Services versus static Services (headless, API, metrics). This separation is critical -- the service reconciler must NEVER touch static Services. For StatefulSet port updates, patch the PodTemplateSpec container ports to match active adapters, which triggers a rolling restart (acceptable since adapter changes are infrequent operational events).

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `sigs.k8s.io/controller-runtime` | v0.22.4 | Reconciler, client.Client, controllerutil | Already in use |
| `k8s.io/api/core/v1` | v0.34.1 | Service, Container, ContainerPort types | Already in use |
| `k8s.io/api/apps/v1` | v0.34.1 | StatefulSet type for port updates | Already in use |
| `k8s.io/apimachinery` | v0.34.1 | metav1, labels, client.MatchingLabels | Already in use |
| `k8s.io/client-go/tools/record` | v0.34.1 | EventRecorder for K8s events | Already in use |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `pkg/resources` (internal) | local | ServiceBuilder fluent API | Building adapter Service objects |
| `utils/conditions` (internal) | local | SetCondition, GetCondition | Condition management |
| `fmt`, `sort`, `strings` | stdlib | String formatting, deterministic ordering | Service naming, port ordering |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| One LoadBalancer per adapter | Single Service with all adapter ports | Single Service ties adapter lifecycles together; per-adapter allows independent IPs and independent deletion |
| Label selector for adapter Services | Naming convention only | Labels support `client.MatchingLabels` for efficient listing; naming convention requires string parsing |
| Patching StatefulSet PodTemplateSpec | Separate Job to update ports | Unnecessarily complex; PodTemplateSpec is mutable in StatefulSets and triggers rolling update automatically |

## Architecture Patterns

### Recommended Project Structure

New and modified files:
```
k8s/dittofs-operator/
├── internal/controller/
│   ├── service_reconciler.go        # NEW: per-adapter Service lifecycle + StatefulSet port updates
│   ├── service_reconciler_test.go   # NEW: tests
│   ├── adapter_reconciler.go        # MODIFIED: call reconcileAdapterServices after storing adapters
│   ├── dittoserver_controller.go    # MODIFIED: integrate service reconciler, CRD spec changes
│   └── dittofs_client.go            # UNCHANGED
├── api/v1alpha1/
│   ├── dittoserver_types.go         # MODIFIED: add AdapterServiceConfig to CRD spec
│   └── dittoserver_types_builder.go # MODIFIED: add builder option
├── pkg/resources/
│   └── service.go                   # UNCHANGED (already has ServiceBuilder)
└── utils/conditions/
    └── conditions.go                # UNCHANGED (or add new condition type)
```

### Pattern 1: Desired-vs-Actual Diff Reconciliation

**What:** Compare desired adapter Services (from `getLastKnownAdapters()`) against actual adapter Services in the cluster (queried by label selector), then create missing and delete orphaned.

**When to use:** Every reconcile cycle after adapter polling succeeds.

**Flow:**
```
1. Get lastKnownAdapters (from Phase 2 in-memory cache)
   - If nil (no successful poll yet): skip service reconciliation entirely
   - If empty slice (all adapters removed): proceed to delete orphaned Services

2. List existing adapter Services in cluster (label selector: dittofs.io/adapter-service=true)

3. Build desired set: map[adapterType]AdapterInfo for adapters where Enabled && Running

4. Diff:
   - For each desired adapter NOT in actual: CREATE Service
   - For each actual Service NOT in desired: DELETE Service
   - For each desired adapter IN actual: UPDATE if port changed

5. Update StatefulSet container ports to match desired set

6. Emit K8s events for each create/delete/update
```

**Example:**
```go
func (r *DittoServerReconciler) reconcileAdapterServices(
    ctx context.Context,
    dittoServer *dittoiov1alpha1.DittoServer,
) error {
    adapters := r.getLastKnownAdapters(dittoServer)
    if adapters == nil {
        // No successful poll yet -- skip service reconciliation
        return nil
    }

    // Build desired state: only enabled+running adapters get Services
    desired := make(map[string]AdapterInfo)
    for _, a := range adapters {
        if a.Enabled && a.Running {
            desired[a.Type] = a
        }
    }

    // List existing adapter Services
    var existingServices corev1.ServiceList
    if err := r.List(ctx, &existingServices,
        client.InNamespace(dittoServer.Namespace),
        client.MatchingLabels{
            adapterServiceLabel: "true",
            instanceLabel:       dittoServer.Name,
        },
    ); err != nil {
        return fmt.Errorf("failed to list adapter services: %w", err)
    }

    // Build actual state
    actual := make(map[string]*corev1.Service)
    for i := range existingServices.Items {
        svc := &existingServices.Items[i]
        adapterType := svc.Labels[adapterTypeLabel]
        actual[adapterType] = svc
    }

    // Create missing Services
    for adapterType, info := range desired {
        if _, exists := actual[adapterType]; !exists {
            if err := r.createAdapterService(ctx, dittoServer, adapterType, info); err != nil {
                return err
            }
        } else {
            // Update if port changed
            if err := r.updateAdapterServiceIfNeeded(ctx, dittoServer, actual[adapterType], info); err != nil {
                return err
            }
        }
    }

    // Delete orphaned Services
    for adapterType, svc := range actual {
        if _, exists := desired[adapterType]; !exists {
            if err := r.deleteAdapterService(ctx, dittoServer, svc, adapterType); err != nil {
                return err
            }
        }
    }

    // Update StatefulSet container ports
    return r.reconcileContainerPorts(ctx, dittoServer, desired)
}
```

### Pattern 2: Label-Based Adapter Service Identification

**What:** Use dedicated labels to distinguish dynamically-managed adapter Services from static infrastructure Services.

**Why critical:** The operator already manages 4 static Services (`-headless`, `-file`, `-api`, `-metrics`). The adapter service reconciler must NEVER delete these. Labels provide a safe, efficient mechanism.

**Labels:**
```go
const (
    adapterServiceLabel = "dittofs.io/adapter-service"  // "true" for adapter Services
    adapterTypeLabel    = "dittofs.io/adapter-type"      // "nfs", "smb", etc.
    instanceLabel       = "app.kubernetes.io/instance"   // CR name for scoping
)
```

**Service naming convention:**
```
{cr-name}-adapter-{type}
```
Example: `my-dittofs-adapter-nfs`, `my-dittofs-adapter-smb`

### Pattern 3: Owner Reference for Garbage Collection

**What:** Every adapter Service has `SetControllerReference` to the DittoServer CR.

**Why:** When the DittoServer CR is deleted, K8s garbage collector automatically deletes all owned Services. No custom cleanup needed for adapter Services -- the existing finalizer handles Percona cleanup; adapter Services are covered by owner references.

**Constraint:** `SetControllerReference` ensures only ONE controller owns the resource. Since we already use this pattern for static Services, adapter Services follow the same pattern.

### Pattern 4: CRD Spec Extension for Service Configurability

**What:** Add an optional `adapterServices` section to the CRD spec for configuring adapter Service type and annotations.

```go
// AdapterServiceConfig configures dynamically created per-adapter Services.
type AdapterServiceConfig struct {
    // Type of Service to create for each adapter (LoadBalancer, NodePort, ClusterIP).
    // +kubebuilder:default="LoadBalancer"
    // +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
    // +optional
    Type string `json:"type,omitempty"`

    // Annotations to apply to adapter Services (e.g., cloud LB configuration).
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}
```

In `DittoServerSpec`:
```go
// AdapterServices configures dynamically created per-adapter Services.
// +optional
AdapterServices *AdapterServiceConfig `json:"adapterServices,omitempty"`
```

**Defaults:** If nil or empty, use `LoadBalancer` type and no extra annotations.

### Pattern 5: StatefulSet Container Port Update

**What:** After reconciling Services, update the StatefulSet's PodTemplateSpec container ports to match active adapters.

**Why:** Container ports in the PodTemplateSpec are informational but important for monitoring tools, service mesh sidecars, and `kubectl port-forward`. More critically, some cloud providers and CNI plugins use declared container ports.

**Mechanism:** Modify the existing `buildContainerPorts` approach. Currently, container ports are built from CRD spec (NFSPort, SMB). Phase 3 adds dynamic adapter ports. The reconciler patches the StatefulSet's container ports list.

**Rolling restart:** Changing container ports in the PodTemplateSpec triggers a rolling update of the StatefulSet. This is acceptable because:
1. Adapter changes are infrequent operational events (not high-frequency)
2. The rolling update is graceful (one pod at a time for StatefulSet)
3. The config hash annotation already triggers rolling updates for config changes

**Important:** Only ports for dynamically-discovered adapters should be managed. The API port and metrics port remain static and are not affected.

### Anti-Patterns to Avoid

- **Deleting Services by name pattern instead of label selector:** String parsing is fragile. Always use labels.
- **Modifying static Services (-headless, -file, -api, -metrics) in the adapter service reconciler:** These have their own reconciliation logic. The adapter service reconciler should ONLY touch Services with `dittofs.io/adapter-service=true`.
- **Deleting Services on API poll failure:** DISC-03 safety guard carries into Phase 3. If `getLastKnownAdapters()` returns nil (no poll yet), skip reconciliation. If it returns empty slice (legitimate empty state), delete orphaned Services.
- **Updating StatefulSet VolumeClaimTemplates:** VolumeClaimTemplates are immutable after creation. Only PodTemplateSpec (including container ports) can be updated.
- **Creating Services before adapter state is known:** If no successful poll has occurred, do not create or delete any adapter Services.
- **Using CreateOrUpdate for adapter Services that should be deleted:** Deletion must be explicit `r.Delete()`, not CreateOrUpdate.
- **Forgetting to handle the "port changed" case:** An adapter may keep running but change its port. The Service must be updated to reflect the new port.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Service construction | Manual corev1.Service{} | `resources.NewServiceBuilder()` | Already exists, fluent API, consistent patterns |
| Owner reference | Manual OwnerReference{} | `controllerutil.SetControllerReference()` | Handles UID, GVK, controller flag |
| Optimistic locking retry | Manual retry loop | `retryOnConflict()` | Already exists in controller, handles backoff |
| Service field merging | Overwrite entire spec | `mergeServiceSpec()` | Already exists, preserves cloud controller fields |
| Annotation merging | Overwrite annotations | `mergeAnnotations()` | Already exists, preserves external annotations |
| Event recording | Log-only notification | `r.Recorder.Eventf()` | Already exists, emits K8s Events visible via kubectl |
| Condition management | Manual slice manipulation | `conditions.SetCondition()` | Already exists with generics, handles transitions |
| Label matching | Manual filtering | `client.MatchingLabels{}` | Built-in controller-runtime, server-side filtering |

**Key insight:** Phase 3 is primarily orchestration code. Almost every building block already exists in the codebase. The new logic is the diff algorithm and the StatefulSet port patching.

## Common Pitfalls

### Pitfall 1: Nil vs Empty Adapter List Confusion
**What goes wrong:** Operator deletes all adapter Services because it confuses "no successful poll yet" with "all adapters removed."
**Why it happens:** `getLastKnownAdapters()` returns nil when no poll has occurred, and empty slice when API returns empty array. Both have `len() == 0`.
**How to avoid:** Check for nil explicitly. `nil` = skip reconciliation entirely. `[]AdapterInfo{}` (non-nil, empty) = legitimate state, delete orphaned Services.
**Warning signs:** Adapter Services being deleted on operator startup before first successful poll.

### Pitfall 2: Static Service Deletion
**What goes wrong:** The adapter service reconciler deletes the headless, file, API, or metrics Services.
**Why it happens:** Listing all Services in the namespace without filtering by adapter labels.
**How to avoid:** ALWAYS filter by `dittofs.io/adapter-service=true` label when listing or deleting. Test explicitly that static Services survive adapter reconciliation.
**Warning signs:** StatefulSet DNS resolution fails (headless service gone), API unreachable, NFS unmountable.

### Pitfall 3: StatefulSet VolumeClaimTemplate Update Error
**What goes wrong:** Trying to update StatefulSet VolumeClaimTemplates returns "Forbidden: updates to statefulset spec for fields other than..."
**Why it happens:** K8s makes VolumeClaimTemplates immutable after creation. Only `replicas`, `template`, `updateStrategy`, `persistentVolumeClaimRetentionPolicy`, `minReadySeconds`, and `ordinals` can be updated.
**How to avoid:** ONLY update the PodTemplateSpec (where container ports live). Never touch VolumeClaimTemplates in the update path.
**Warning signs:** `Forbidden: updates to statefulset spec` errors in operator logs.

### Pitfall 4: Unnecessary Rolling Restarts
**What goes wrong:** Every reconcile cycle updates the StatefulSet even when nothing changed, causing unnecessary pod restarts.
**Why it happens:** Container ports are rebuilt every cycle and written to StatefulSet, even if identical.
**How to avoid:** Compare desired container ports with current StatefulSet container ports. Only update if they differ. Use a deterministic sort of ports for comparison.
**Warning signs:** Pods constantly restarting, high pod churn in `kubectl get pods -w`.

### Pitfall 5: Race Between Service Creation and Adapter Polling
**What goes wrong:** Adapter appears in poll, Service is created, but adapter is stopped before next poll. Service exists for one full polling interval with no backing adapter.
**Why it happens:** Normal timing behavior -- polling is periodic, not real-time.
**How to avoid:** This is expected behavior, not a bug. Document that Service removal happens within one polling cycle. Do NOT add webhooks or watches to reduce this gap -- polling is the architectural decision.
**Warning signs:** None -- this is by design.

### Pitfall 6: CreateOrUpdate Conflict on Service with Cloud Controller Annotations
**What goes wrong:** Cloud load balancer controller adds annotations/status to the Service. Our update conflicts with their update.
**Why it happens:** Optimistic locking -- two controllers writing to the same object.
**How to avoid:** Use `retryOnConflict()` wrapper (already exists) and `mergeServiceSpec()` / `mergeAnnotations()` to preserve cloud controller fields. These are already implemented and tested.
**Warning signs:** `Conflict` errors in operator logs for adapter Services.

### Pitfall 7: Port Name Collision Between Static and Dynamic Ports
**What goes wrong:** Dynamic adapter Service uses port name "nfs" which conflicts with the static file Service's "nfs" port.
**Why it happens:** Both Services expose the same protocol name.
**How to avoid:** Adapter Services use protocol-specific names and have a single port each (per-adapter Service model). The name is fine since Services are separate objects. However, container port names within the StatefulSet must be unique. Use `adapter-{type}` naming for dynamic ports to avoid collision with static port names.
**Warning signs:** `duplicate container port name` validation error from K8s API.

## Code Examples

### Adapter Service Labels
```go
const (
    adapterServiceLabel = "dittofs.io/adapter-service"
    adapterTypeLabel    = "dittofs.io/adapter-type"
)

func adapterServiceName(crName, adapterType string) string {
    return fmt.Sprintf("%s-adapter-%s", crName, adapterType)
}

func adapterServiceLabels(crName, adapterType string) map[string]string {
    return map[string]string{
        "app":                  "dittofs-server",
        "instance":             crName,
        adapterServiceLabel:    "true",
        adapterTypeLabel:       adapterType,
    }
}
```

### Create Adapter Service
```go
func (r *DittoServerReconciler) createAdapterService(
    ctx context.Context,
    ds *dittoiov1alpha1.DittoServer,
    adapterType string,
    info AdapterInfo,
) error {
    labels := adapterServiceLabels(ds.Name, adapterType)
    svcType := getAdapterServiceType(ds)
    annotations := getAdapterServiceAnnotations(ds)

    svc := resources.NewServiceBuilder(adapterServiceName(ds.Name, adapterType), ds.Namespace).
        WithLabels(labels).
        WithSelector(map[string]string{
            "app":      "dittofs-server",
            "instance": ds.Name,
        }).
        WithType(svcType).
        WithAnnotations(annotations).
        AddTCPPort(adapterType, int32(info.Port)).
        Build()

    if err := controllerutil.SetControllerReference(ds, svc, r.Scheme); err != nil {
        return fmt.Errorf("failed to set owner reference on adapter service: %w", err)
    }

    if err := r.Create(ctx, svc); err != nil {
        if apierrors.IsAlreadyExists(err) {
            // Race: another reconcile created it. Update instead.
            return r.updateAdapterServiceIfNeeded(ctx, ds, svc, info)
        }
        return fmt.Errorf("failed to create adapter service %s: %w", adapterType, err)
    }

    r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceCreated",
        "Created %s Service %s for adapter %s (port %d)",
        svcType, svc.Name, adapterType, info.Port)

    return nil
}
```

### Delete Adapter Service
```go
func (r *DittoServerReconciler) deleteAdapterService(
    ctx context.Context,
    ds *dittoiov1alpha1.DittoServer,
    svc *corev1.Service,
    adapterType string,
) error {
    if err := r.Delete(ctx, svc); err != nil {
        if apierrors.IsNotFound(err) {
            return nil // Already deleted
        }
        return fmt.Errorf("failed to delete adapter service %s: %w", adapterType, err)
    }

    r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceDeleted",
        "Deleted Service %s for adapter %s", svc.Name, adapterType)

    return nil
}
```

### Update Adapter Service (Port Changed)
```go
func (r *DittoServerReconciler) updateAdapterServiceIfNeeded(
    ctx context.Context,
    ds *dittoiov1alpha1.DittoServer,
    existing *corev1.Service,
    info AdapterInfo,
) error {
    // Check if port changed
    currentPort := int32(0)
    for _, p := range existing.Spec.Ports {
        if p.Name == info.Type {
            currentPort = p.Port
        }
    }

    if currentPort == int32(info.Port) {
        return nil // No change needed
    }

    // Re-fetch for freshest version
    fresh := &corev1.Service{}
    if err := r.Get(ctx, client.ObjectKeyFromObject(existing), fresh); err != nil {
        return err
    }

    // Update port
    for i, p := range fresh.Spec.Ports {
        if p.Name == info.Type {
            fresh.Spec.Ports[i].Port = int32(info.Port)
            fresh.Spec.Ports[i].TargetPort = intstr.FromInt32(int32(info.Port))
        }
    }

    if err := r.Update(ctx, fresh); err != nil {
        return fmt.Errorf("failed to update adapter service port: %w", err)
    }

    r.Recorder.Eventf(ds, corev1.EventTypeNormal, "AdapterServiceUpdated",
        "Updated Service %s port from %d to %d", fresh.Name, currentPort, info.Port)

    return nil
}
```

### StatefulSet Container Port Reconciliation
```go
func (r *DittoServerReconciler) reconcileContainerPorts(
    ctx context.Context,
    ds *dittoiov1alpha1.DittoServer,
    activeAdapters map[string]AdapterInfo,
) error {
    statefulSet := &appsv1.StatefulSet{}
    if err := r.Get(ctx, client.ObjectKey{
        Namespace: ds.Namespace,
        Name:      ds.Name,
    }, statefulSet); err != nil {
        return fmt.Errorf("failed to get StatefulSet: %w", err)
    }

    if len(statefulSet.Spec.Template.Spec.Containers) == 0 {
        return nil
    }

    // Build desired ports: static ports (API, metrics) + dynamic adapter ports
    desiredPorts := buildStaticContainerPorts(ds)
    for adapterType, info := range activeAdapters {
        desiredPorts = append(desiredPorts, corev1.ContainerPort{
            Name:          fmt.Sprintf("adapter-%s", adapterType),
            ContainerPort: int32(info.Port),
            Protocol:      corev1.ProtocolTCP,
        })
    }
    // Sort for deterministic comparison
    sort.Slice(desiredPorts, func(i, j int) bool {
        return desiredPorts[i].Name < desiredPorts[j].Name
    })

    currentPorts := statefulSet.Spec.Template.Spec.Containers[0].Ports
    if portsEqual(currentPorts, desiredPorts) {
        return nil // No change needed
    }

    // Update container ports
    statefulSet.Spec.Template.Spec.Containers[0].Ports = desiredPorts
    if err := r.Update(ctx, statefulSet); err != nil {
        return fmt.Errorf("failed to update StatefulSet container ports: %w", err)
    }

    return nil
}
```

### CRD Spec Extension
```go
// In DittoServerSpec:
// AdapterServices configures dynamically created per-adapter Services.
// +optional
AdapterServices *AdapterServiceConfig `json:"adapterServices,omitempty"`

// AdapterServiceConfig configures dynamically created per-adapter Services.
type AdapterServiceConfig struct {
    // Type of Service to create for each adapter.
    // +kubebuilder:default="LoadBalancer"
    // +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
    // +optional
    Type string `json:"type,omitempty"`

    // Annotations to apply to adapter Services (e.g., cloud LB configuration).
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}
```

### Helper Functions
```go
func getAdapterServiceType(ds *dittoiov1alpha1.DittoServer) corev1.ServiceType {
    if ds.Spec.AdapterServices != nil && ds.Spec.AdapterServices.Type != "" {
        return corev1.ServiceType(ds.Spec.AdapterServices.Type)
    }
    return corev1.ServiceTypeLoadBalancer
}

func getAdapterServiceAnnotations(ds *dittoiov1alpha1.DittoServer) map[string]string {
    if ds.Spec.AdapterServices != nil {
        return ds.Spec.AdapterServices.Annotations
    }
    return nil
}

func buildStaticContainerPorts(ds *dittoiov1alpha1.DittoServer) []corev1.ContainerPort {
    ports := []corev1.ContainerPort{
        {
            Name:          "api",
            ContainerPort: getAPIPort(ds),
            Protocol:      corev1.ProtocolTCP,
        },
    }
    if ds.Spec.Metrics != nil && ds.Spec.Metrics.Enabled {
        ports = append(ports, corev1.ContainerPort{
            Name:          "metrics",
            ContainerPort: getMetricsPort(ds),
            Protocol:      corev1.ProtocolTCP,
        })
    }
    return ports
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Static NFS/SMB Service from CRD spec | Dynamic per-adapter Services from API polling | Phase 3 (this phase) | Adapters can be added/removed at runtime without CRD changes |
| Fixed container ports in StatefulSet | Dynamic port list matching active adapters | Phase 3 (this phase) | Container ports accurately reflect running adapters |
| Single file Service for all protocols | One LoadBalancer per adapter | Phase 3 (this phase) | Independent IPs, independent lifecycle, clean separation |

**Transition note:** Phase 3 adds dynamic adapter Services alongside the existing static Services. Phase 4 (Security Hardening) will remove the static `spec.nfsPort` and `spec.smb` fields, fully transitioning to dynamic management. During Phase 3, both static and dynamic Services may coexist.

## Integration Points

### Where Service Reconciler Runs in Reconcile Loop

The service reconciler should run after adapter polling succeeds (inside the same `Authenticated=True` block):

```
... existing steps 1-8 (finalizer, secrets, configmap, static services, statefulset) ...
9.  reconcileAuth() -- Phase 1
10. reconcileAdapters() -- Phase 2 (polls API, stores adapters)
11. reconcileAdapterServices() -- Phase 3 (NEW: creates/deletes Services, updates ports)
    - Only runs if getLastKnownAdapters() is not nil
    - Uses adapter state from step 10
12. Update Status (existing)
```

### Impact on Existing reconcileStatefulSet

Currently, `reconcileStatefulSet` calls `buildContainerPorts()` which reads static NFS/SMB ports from the CRD spec. Phase 3 must handle the coexistence of:
1. Static ports (from CRD spec -- NFS, SMB, API, metrics)
2. Dynamic ports (from adapter polling -- any protocol the server reports)

**Approach:** During Phase 3, keep static port building as-is. The `reconcileContainerPorts` function in the service reconciler will reconcile dynamic adapter ports AFTER the StatefulSet is initially created. This avoids modifying `reconcileStatefulSet` in Phase 3 -- that cleanup happens in Phase 4.

**Important consequence:** There may be duplicate ports during Phase 3 (e.g., NFS port from static spec AND NFS port from adapter discovery). The service reconciler must handle this gracefully by either:
- Skipping container port reconciliation for adapter types that already have static port definitions
- Or using distinct port names (`nfs` for static, `adapter-nfs` for dynamic)

The recommended approach is to use distinct names (`adapter-{type}`) for dynamic ports, and Phase 4 will remove the static ones.

### Impact on SetupWithManager

The controller already watches `Owns(&corev1.Service{})`. Since adapter Services are owned by the DittoServer CR (via SetControllerReference), any external modification to adapter Services will trigger a reconcile, ensuring the operator re-converges to the desired state.

No changes needed to `SetupWithManager`.

### RBAC

The operator already has full RBAC for Services:
```yaml
- apiGroups: [""]
  resources: [services]
  verbs: [get, list, watch, create, update, patch, delete]
```

And for events:
```yaml
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
```

No RBAC changes needed.

## Open Questions

1. **Should the static `-file` Service be removed in Phase 3 or Phase 4?**
   - What we know: Phase 4 is explicitly about removing static adapter config from the CRD. The roadmap says Phase 4 removes `spec.nfsPort` and `spec.smb`.
   - What's unclear: Whether the `-file` Service should remain as a backward-compatible aggregate or be replaced entirely by per-adapter Services.
   - Recommendation: Keep the `-file` Service in Phase 3. Remove it in Phase 4. During Phase 3, both coexist: the static `-file` Service for backward compatibility, and the new per-adapter Services for dynamic access. This minimizes blast radius.

2. **Should port changes trigger an immediate re-poll or wait for next cycle?**
   - What we know: Polling interval drives the reconciliation cadence (default 30s).
   - What's unclear: Whether a CRD spec change to `adapterServices.type` should immediately re-reconcile adapter Services.
   - Recommendation: Yes, it should -- and it will automatically. CRD spec changes trigger a reconcile (the controller watches its own CRD). The adapter Service reconciler reads CRD spec fresh each time. No special mechanism needed.

3. **Should there be a dedicated condition for adapter Services (e.g., AdaptersReady)?**
   - What we know: Phase 2 research deferred this question. The adapter polling is transparent.
   - What's unclear: Whether downstream users need a condition to know if adapter Services are correctly reconciled.
   - Recommendation: Not in Plan 03-01 (keep it minimal). Consider adding in Plan 03-02 if events alone don't provide enough observability. K8s events for create/delete/update may be sufficient.

## Sources

### Primary (HIGH confidence)
- DittoFS operator codebase direct inspection (all files listed in Architecture Patterns section)
  - `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- existing Service reconciliation, createOrUpdateService, mergeServiceSpec, buildContainerPorts
  - `k8s/dittofs-operator/internal/controller/adapter_reconciler.go` -- getLastKnownAdapters(), reconcileAdapters()
  - `k8s/dittofs-operator/internal/controller/dittofs_client.go` -- AdapterInfo struct, ListAdapters()
  - `k8s/dittofs-operator/pkg/resources/service.go` -- ServiceBuilder fluent API
  - `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` -- CRD spec structure, ServiceSpec
  - `k8s/dittofs-operator/utils/conditions/conditions.go` -- Condition management
  - `k8s/dittofs-operator/config/rbac/role.yaml` -- RBAC for Services and events
  - `internal/controlplane/api/handlers/adapters.go` -- AdapterResponse struct confirming API response format
- [controller-runtime controllerutil package](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil) -- SetControllerReference for owner-based GC
- Phase 2 Research and Verification documents confirming adapter polling infrastructure

### Secondary (MEDIUM confidence)
- [Kubernetes StatefulSet documentation](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/) -- StatefulSet update semantics, mutable fields (template spec is mutable)
- [Kubernetes garbage collection](https://kubernetes.io/docs/concepts/architecture/garbage-collection/) -- Owner reference cascade deletion
- [Kubebuilder watching resources](https://book.kubebuilder.io/reference/watching-resources) -- Owns() watches for owned resource changes

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in use, no new dependencies
- Architecture: HIGH -- extends established patterns (ServiceBuilder, createOrUpdateService, label-based filtering, owner references)
- Pitfalls: HIGH -- derived from reading actual codebase, understanding of K8s Service and StatefulSet semantics, and Phase 2 safety guard requirements
- CRD extension: HIGH -- follows exact same pattern as existing ServiceSpec and AdapterDiscoverySpec

**Research date:** 2026-02-10
**Valid until:** 2026-03-10 (stable -- no fast-moving dependencies)
