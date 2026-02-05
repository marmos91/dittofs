# Phase 5: Status Conditions and Lifecycle - Research

**Researched:** 2026-02-05
**Domain:** Kubernetes Operator Status Conditions, Finalizers, Events, Health Probes
**Confidence:** HIGH

## Summary

This phase implements operator observability and lifecycle management for the DittoFS Kubernetes operator. The research investigates three main areas: (1) status conditions using the standard metav1.Condition pattern, (2) finalizers for pre-deletion cleanup, and (3) Kubernetes events for operational visibility.

The operator already has a basic conditions utility in `utils/conditions/conditions.go` and basic liveness/readiness probes on the StatefulSet. This phase enhances these with full status condition tracking, adds finalizer-based cleanup for owned resources, and implements event emission for operational debugging.

DittoFS already exposes health endpoints on port 8080 (`/health`, `/health/ready`, `/health/stores`) and handles SIGTERM gracefully with connection draining. The operator needs to wire these up properly via HTTP-based probes rather than current TCP-based probes.

**Primary recommendation:** Use the standard `k8s.io/apimachinery/pkg/api/meta` condition helpers with `metav1.Condition`, implement finalizer pattern from Kubebuilder documentation, and add EventRecorder for debugging visibility.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `k8s.io/apimachinery/pkg/api/meta` | matches controller-runtime | Condition helpers (SetStatusCondition, FindStatusCondition) | Official Kubernetes utilities, handles observedGeneration correctly |
| `sigs.k8s.io/controller-runtime/pkg/controller/controllerutil` | matches controller-runtime | Finalizer helpers (AddFinalizer, RemoveFinalizer, ContainsFinalizer) | Kubebuilder standard, idempotent operations |
| `k8s.io/client-go/tools/record` | matches controller-runtime | EventRecorder for emitting events | Kubernetes standard, integrates with controller-runtime Manager |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `metav1.Condition` | k8s.io/apimachinery | Standard condition struct | All status conditions |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `k8s.io/apimachinery/pkg/api/meta` helpers | Custom `utils/conditions` package | Already exists but lacks observedGeneration handling - migrate to standard helpers |

**Note:** The existing `utils/conditions/conditions.go` already implements SetCondition with observedGeneration support. It can be retained as-is since it properly handles the metav1.Condition struct and observedGeneration field.

## Architecture Patterns

### Recommended Status Conditions

Based on CONTEXT.md decisions and Kubernetes conventions:

```go
// Condition types for DittoServer
const (
    // Ready indicates the DittoServer is fully operational
    ConditionReady = "Ready"

    // Available indicates the StatefulSet has minimum ready replicas
    ConditionAvailable = "Available"

    // ConfigReady indicates ConfigMap and secrets are valid
    ConditionConfigReady = "ConfigReady"

    // DatabaseReady indicates PostgreSQL (Percona) is ready (when enabled)
    ConditionDatabaseReady = "DatabaseReady"

    // Progressing indicates a change is being applied
    ConditionProgressing = "Progressing"
)
```

### Pattern 1: Status Condition Update Flow
**What:** Update conditions during reconciliation based on resource state
**When to use:** Every reconciliation pass after checking resource states
**Example:**
```go
// Source: Kubebuilder good practices + k8s.io/apimachinery/pkg/api/meta
import "k8s.io/apimachinery/pkg/api/meta"

// After checking StatefulSet status
if statefulSet.Status.ReadyReplicas == *statefulSet.Spec.Replicas {
    meta.SetStatusCondition(&dittoServer.Status.Conditions, metav1.Condition{
        Type:               ConditionAvailable,
        Status:             metav1.ConditionTrue,
        ObservedGeneration: dittoServer.Generation,
        Reason:             "StatefulSetReady",
        Message:            fmt.Sprintf("StatefulSet has %d/%d ready replicas",
            statefulSet.Status.ReadyReplicas, *statefulSet.Spec.Replicas),
    })
} else {
    meta.SetStatusCondition(&dittoServer.Status.Conditions, metav1.Condition{
        Type:               ConditionAvailable,
        Status:             metav1.ConditionFalse,
        ObservedGeneration: dittoServer.Generation,
        Reason:             "StatefulSetNotReady",
        Message:            fmt.Sprintf("Waiting for replicas: %d/%d ready",
            statefulSet.Status.ReadyReplicas, *statefulSet.Spec.Replicas),
    })
}
```

### Pattern 2: Finalizer for Pre-Deletion Cleanup
**What:** Add finalizer on creation, perform cleanup on deletion before removing finalizer
**When to use:** When owned resources need cleanup coordination (Percona DB deletion, graceful pod termination)
**Example:**
```go
// Source: https://book.kubebuilder.io/reference/using-finalizers
const finalizerName = "dittofs.dittofs.com/finalizer"

func (r *DittoServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    dittoServer := &dittoiov1alpha1.DittoServer{}
    if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Handle deletion
    if !dittoServer.ObjectMeta.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(dittoServer, finalizerName) {
            // Perform cleanup
            if err := r.performCleanup(ctx, dittoServer); err != nil {
                return ctrl.Result{}, err
            }

            // Remove finalizer
            controllerutil.RemoveFinalizer(dittoServer, finalizerName)
            if err := r.Update(ctx, dittoServer); err != nil {
                return ctrl.Result{}, err
            }
        }
        return ctrl.Result{}, nil
    }

    // Add finalizer if not present
    if !controllerutil.ContainsFinalizer(dittoServer, finalizerName) {
        controllerutil.AddFinalizer(dittoServer, finalizerName)
        if err := r.Update(ctx, dittoServer); err != nil {
            return ctrl.Result{}, err
        }
    }

    // Normal reconciliation...
}
```

### Pattern 3: Event Recording
**What:** Emit Kubernetes events for debugging and operational visibility
**When to use:** State changes, errors, configuration updates
**Example:**
```go
// Source: https://book.kubebuilder.io/reference/raising-events
// In reconciler struct:
type DittoServerReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}

// In main.go:
if err := (&controller.DittoServerReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: mgr.GetEventRecorderFor("dittoserver-controller"),
}).SetupWithManager(mgr); err != nil {
    // ...
}

// Usage in reconciler:
r.Recorder.Event(dittoServer, corev1.EventTypeNormal, "ConfigUpdated",
    fmt.Sprintf("ConfigMap %s-config updated, pods will restart", dittoServer.Name))

r.Recorder.Eventf(dittoServer, corev1.EventTypeWarning, "PerconaNotReady",
    "Waiting for PostgreSQL cluster %s to become ready", perconaClusterName)
```

### Pattern 4: HTTP-Based Health Probes
**What:** Use HTTP probes against DittoFS built-in health endpoints
**When to use:** StatefulSet pod template probes
**Example:**
```go
// Source: DittoFS router.go - health endpoints on port 8080
// /health - liveness (always returns 200 if server running)
// /health/ready - readiness (checks registry, shares, adapters)

LivenessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        HTTPGet: &corev1.HTTPGetAction{
            Path: "/health",
            Port: intstr.FromInt32(getAPIPort(dittoServer)),
        },
    },
    InitialDelaySeconds: 15,
    PeriodSeconds:       10,
    TimeoutSeconds:      5,
    FailureThreshold:    3,
},
ReadinessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        HTTPGet: &corev1.HTTPGetAction{
            Path: "/health/ready",
            Port: intstr.FromInt32(getAPIPort(dittoServer)),
        },
    },
    InitialDelaySeconds: 10,
    PeriodSeconds:       5,
    TimeoutSeconds:      5,
    FailureThreshold:    3,
},
StartupProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        HTTPGet: &corev1.HTTPGetAction{
            Path: "/health",
            Port: intstr.FromInt32(getAPIPort(dittoServer)),
        },
    },
    InitialDelaySeconds: 0,
    PeriodSeconds:       5,
    TimeoutSeconds:      5,
    FailureThreshold:    30, // 30 * 5s = 150s max startup time
},
```

### Pattern 5: Graceful Shutdown with PreStop Hook
**What:** PreStop hook to allow connection draining before SIGTERM
**When to use:** StatefulSet pod template lifecycle
**Example:**
```go
// Source: CONTEXT.md decisions - DittoFS handles SIGTERM for connection draining
Lifecycle: &corev1.Lifecycle{
    PreStop: &corev1.LifecycleHandler{
        Exec: &corev1.ExecAction{
            Command: []string{"/bin/sh", "-c", "sleep 5"},
        },
    },
},
```

### Recommended Status Struct Enhancement

```go
// DittoServerStatus defines the observed state of DittoServer
type DittoServerStatus struct {
    // ObservedGeneration is the generation last processed by the controller
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Replicas is the desired number of replicas
    Replicas int32 `json:"replicas,omitempty"`

    // ReadyReplicas is the number of pods with Ready condition
    ReadyReplicas int32 `json:"readyReplicas,omitempty"`

    // AvailableReplicas is the number of pods ready for at least minReadySeconds
    AvailableReplicas int32 `json:"availableReplicas,omitempty"`

    // NFSEndpoint that clients should use to mount
    NFSEndpoint string `json:"nfsEndpoint,omitempty"`

    // Phase of the DittoServer (Pending, Running, Failed, Stopped, Deleting)
    // +kubebuilder:validation:Enum=Pending;Running;Failed;Stopped;Deleting
    Phase string `json:"phase,omitempty"`

    // ConfigHash is the hash of current configuration (for debugging)
    ConfigHash string `json:"configHash,omitempty"`

    // PerconaClusterName is the name of the owned PerconaPGCluster (when enabled)
    // +optional
    PerconaClusterName string `json:"perconaClusterName,omitempty"`

    // Conditions represent the latest available observations
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### Anti-Patterns to Avoid
- **Updating conditions without observedGeneration:** Always set `ObservedGeneration: resource.Generation` to track reconciliation progress
- **Emitting too many events:** Follow "moderate verbosity" - errors, major state changes, not every reconciliation
- **Blocking finalizer cleanup:** Use timeouts, don't wait indefinitely for resources to terminate
- **Using TCP probes for health:** DittoFS has proper HTTP endpoints that return meaningful status codes

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Condition manipulation | Custom slice operations | `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition` | Handles LastTransitionTime, observedGeneration correctly |
| Finalizer add/remove | Manual string slice manipulation | `controllerutil.AddFinalizer/RemoveFinalizer` | Idempotent, returns bool for change detection |
| Event recording | Direct API calls | `record.EventRecorder` from Manager | Handles aggregation, proper event object creation |
| Health endpoint calls | Custom HTTP client | Kubernetes probe handlers | Handles retries, timeouts, threshold correctly |

**Key insight:** The Kubernetes ecosystem has standardized utilities for all these operations. The patterns are well-tested and handle edge cases (concurrent updates, retries, proper timestamps).

## Common Pitfalls

### Pitfall 1: Stale Status Conditions
**What goes wrong:** Conditions don't reflect current state because observedGeneration isn't updated
**Why it happens:** Forgetting to set ObservedGeneration field in condition
**How to avoid:** Always include `ObservedGeneration: resource.Generation` in every condition update
**Warning signs:** `kubectl get dittofs -o yaml` shows condition.observedGeneration < metadata.generation

### Pitfall 2: Finalizer Deadlock
**What goes wrong:** Resource stuck in Terminating state forever
**Why it happens:** Cleanup code fails or hangs, finalizer never removed
**How to avoid:** Implement timeout in cleanup, emit warning event if cleanup takes too long, consider force-removing finalizer after extended timeout
**Warning signs:** Resource shows DeletionTimestamp but never disappears

### Pitfall 3: Event Spam
**What goes wrong:** Cluster events flooded with operator messages
**Why it happens:** Emitting events on every reconciliation instead of state changes
**How to avoid:** Only emit events for: errors, state transitions, configuration changes
**Warning signs:** `kubectl get events` shows thousands of identical operator events

### Pitfall 4: TCP Probe Missing Application Issues
**What goes wrong:** Pod marked Ready when application isn't actually ready
**Why it happens:** TCP probe only checks port is open, not application state
**How to avoid:** Use HTTP probes against `/health/ready` endpoint
**Warning signs:** NFS mounts fail even though pod shows Ready

### Pitfall 5: Status Update Conflicts
**What goes wrong:** Status updates fail with "conflict" errors
**Why it happens:** Multiple reconciliation loops updating status concurrently
**How to avoid:** Always re-fetch the resource before status update, use Status().Update() subresource
**Warning signs:** Frequent "conflict" errors in operator logs

### Pitfall 6: Percona Deletion Orphaning Data
**What goes wrong:** User expects database to be deleted with DittoServer but it persists
**Why it happens:** PerconaPGCluster has owner reference but user expects explicit deletion
**How to avoid:** Implement `spec.percona.deleteWithServer` flag (default: false), emit warning event on deletion if true
**Warning signs:** User confusion about database state after DittoServer deletion

## Code Examples

Verified patterns from official sources:

### Condition Update with Meta Helpers
```go
// Source: https://pkg.go.dev/k8s.io/apimachinery/pkg/api/meta
import "k8s.io/apimachinery/pkg/api/meta"

// Setting a condition
meta.SetStatusCondition(&dittoServer.Status.Conditions, metav1.Condition{
    Type:               ConditionReady,
    Status:             metav1.ConditionTrue,
    ObservedGeneration: dittoServer.Generation,
    Reason:             "AllSystemsGo",
    Message:            "All components are ready",
})

// Checking a condition
if meta.IsStatusConditionTrue(dittoServer.Status.Conditions, ConditionReady) {
    // Resource is ready
}

// Finding a condition
cond := meta.FindStatusCondition(dittoServer.Status.Conditions, ConditionDatabaseReady)
if cond != nil && cond.Status == metav1.ConditionFalse {
    // Database not ready
}
```

### Complete Finalizer Flow
```go
// Source: https://book.kubebuilder.io/reference/using-finalizers
const finalizerName = "dittofs.dittofs.com/finalizer"

func (r *DittoServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    dittoServer := &dittoiov1alpha1.DittoServer{}
    if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Check if being deleted
    if !dittoServer.ObjectMeta.DeletionTimestamp.IsZero() {
        return r.handleDeletion(ctx, dittoServer)
    }

    // Add finalizer if not present
    if !controllerutil.ContainsFinalizer(dittoServer, finalizerName) {
        logger.Info("Adding finalizer")
        controllerutil.AddFinalizer(dittoServer, finalizerName)
        if err := r.Update(ctx, dittoServer); err != nil {
            return ctrl.Result{}, err
        }
        // Re-fetch after update
        return ctrl.Result{Requeue: true}, nil
    }

    // Normal reconciliation continues...
}

func (r *DittoServerReconciler) handleDeletion(ctx context.Context, ds *dittoiov1alpha1.DittoServer) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    if !controllerutil.ContainsFinalizer(ds, finalizerName) {
        return ctrl.Result{}, nil
    }

    logger.Info("Processing deletion")

    // Emit deletion event
    r.Recorder.Event(ds, corev1.EventTypeNormal, "Deleting",
        "DittoServer is being deleted, cleaning up resources")

    // Perform cleanup (e.g., wait for pods, optionally delete Percona)
    if err := r.performCleanup(ctx, ds); err != nil {
        r.Recorder.Eventf(ds, corev1.EventTypeWarning, "CleanupFailed",
            "Cleanup failed: %v", err)
        return ctrl.Result{RequeueAfter: 10 * time.Second}, err
    }

    // Remove finalizer
    logger.Info("Removing finalizer")
    controllerutil.RemoveFinalizer(ds, finalizerName)
    if err := r.Update(ctx, ds); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

### Event Recording Setup
```go
// Source: https://book.kubebuilder.io/reference/raising-events
// In cmd/main.go

import "k8s.io/client-go/tools/record"

// RBAC marker for events
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

if err := (&controller.DittoServerReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: mgr.GetEventRecorderFor("dittoserver-controller"),
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller")
    os.Exit(1)
}

// In reconciler - event examples
// Normal events
r.Recorder.Event(ds, corev1.EventTypeNormal, "Created", "StatefulSet created")
r.Recorder.Eventf(ds, corev1.EventTypeNormal, "ConfigUpdated",
    "ConfigMap %s-config updated, pods will restart", ds.Name)

// Warning events
r.Recorder.Event(ds, corev1.EventTypeWarning, "StorageClassNotFound",
    "StorageClass does not exist")
r.Recorder.Eventf(ds, corev1.EventTypeWarning, "PerconaNotReady",
    "Waiting for PostgreSQL cluster %s", perconaClusterName)
```

### kubectl Print Columns
```go
// Source: Kubebuilder markers
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="AVAILABLE",type=string,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="STATUS",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Custom condition helpers | `k8s.io/apimachinery/pkg/api/meta` standard helpers | Kubernetes 1.19+ | Standard helpers handle edge cases, observedGeneration |
| `GetEventRecorderFor` | `GetEventRecorder` | controller-runtime future | Old API deprecated, new events API recommended |
| TCP socket probes | HTTP probes with health endpoints | Always preferred | More accurate health checking |

**Deprecated/outdated:**
- `GetEventRecorderFor`: Deprecated in favor of `GetEventRecorder` returning `events.EventRecorder` - however current controller-runtime still uses the old API, migration in progress

## Open Questions

Things that couldn't be fully resolved:

1. **Percona Deletion Behavior**
   - What we know: CONTEXT.md specifies `spec.percona.deleteWithServer` (default: false)
   - What's unclear: Exact implementation - should we delete PerconaPGCluster CR or just remove owner reference?
   - Recommendation: Delete PerconaPGCluster CR when flag is true (cascade deletion), orphan when false

2. **Finalizer Timeout**
   - What we know: Need timeout to avoid stuck Terminating resources
   - What's unclear: How long to wait before force-completing finalizer
   - Recommendation: Use 60 seconds, emit warning event at 30 seconds

3. **Events API Migration**
   - What we know: `GetEventRecorderFor` is deprecated
   - What's unclear: When controller-runtime will fully migrate
   - Recommendation: Use current `GetEventRecorderFor` for now, stable and working

## Sources

### Primary (HIGH confidence)
- [Kubebuilder Using Finalizers](https://book.kubebuilder.io/reference/using-finalizers) - Complete finalizer pattern
- [Kubebuilder Raising Events](https://book.kubebuilder.io/reference/raising-events) - Event recording setup
- [Kubebuilder Good Practices](https://book.kubebuilder.io/reference/good-practices) - Status conditions guidance
- [k8s.io/apimachinery/pkg/api/meta](https://pkg.go.dev/k8s.io/apimachinery/pkg/api/meta) - Condition helper functions
- [controller-runtime controllerutil](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller/controllerutil) - Finalizer helpers

### Secondary (MEDIUM confidence)
- [Kubernetes API Conventions - Conditions](https://maelvls.dev/kubernetes-conditions/) - Condition type patterns
- [observedGeneration Best Practices](https://alenkacz.medium.com/kubernetes-operator-best-practices-implementing-observedgeneration-250728868792) - Generation tracking

### Tertiary (LOW confidence)
- None - all claims verified with official documentation

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Official Kubernetes/Kubebuilder packages with verified documentation
- Architecture: HIGH - Patterns from Kubebuilder book, verified with existing operator code
- Pitfalls: HIGH - Based on official documentation warnings and common community issues

**Research date:** 2026-02-05
**Valid until:** 60 days (stable Kubernetes APIs, unlikely to change significantly)

## Alignment with CONTEXT.md

The CONTEXT.md file from `05-status-lifecycle` contains locked decisions that this research supports:

| Decision | Research Support |
|----------|-----------------|
| Show "Progressing" with reason when waiting for database | ConditionProgressing pattern documented |
| Include full replica counts | Status struct enhancement includes replicas, readyReplicas, availableReplicas |
| Custom kubectl columns | Print column markers documented |
| Include observedGeneration | All condition examples use ObservedGeneration field |
| Include configHash in status | Status struct includes ConfigHash field |
| Moderate event verbosity | Event examples follow errors/state changes pattern |
| Use DittoFS health endpoints on port 8080 | HTTP probe patterns use `/health` and `/health/ready` |
| Add startup probe for slow startup | Startup probe pattern documented |
| Add preStop hook for SIGTERM | PreStop lifecycle hook documented |
| Configurable Percona deletion via deleteWithServer | Open question documents implementation approach |
| Finalizer for pre-deletion cleanup | Complete finalizer flow documented |
