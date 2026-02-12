# Architecture Patterns: K8s Operator Dynamic Adapter Service Management

**Domain:** Kubernetes operator extension -- polling external REST API to dynamically manage K8s Services
**Researched:** 2026-02-09
**Confidence:** HIGH (based on deep analysis of existing codebase + established K8s patterns)

## Current Architecture (Baseline)

The existing operator follows standard controller-runtime patterns. Understanding it is critical because the new component must integrate cleanly.

```
DittoServer CR (spec change)
        |
        v
SetupWithManager:
  For(DittoServer) + Owns(StatefulSet, Service, ConfigMap)
        |
        v
DittoServerReconciler.Reconcile()
  |
  +-- reconcileJWTSecret
  +-- reconcileConfigMap
  +-- reconcileHeadlessService      <-- STATIC: always NFS port
  +-- reconcileFileService          <-- STATIC: NFS + optional SMB from CRD spec
  +-- reconcileAPIService           <-- STATIC: control plane port
  +-- reconcileMetricsService       <-- CONDITIONAL: only if metrics enabled
  +-- reconcilePerconaPGCluster     <-- CONDITIONAL: only if percona enabled
  +-- reconcileStatefulSet          <-- STATIC: ports from CRD spec
  +-- updateStatus
```

**Key observations about the current design:**

1. **All reconciliation is CRD-driven.** Ports come from `spec.nfsPort` and `spec.smb.enabled/port`. The operator never queries DittoFS at runtime.
2. **Owner references handle cleanup.** Services, ConfigMap, StatefulSet are owned by DittoServer via `SetControllerReference`. K8s GC deletes them automatically.
3. **Service creation uses CreateOrUpdate with mergeServiceSpec** to preserve cloud controller fields (ClusterIP, NodePort, HealthCheckNodePort).
4. **The ServiceBuilder** provides a fluent API in `pkg/resources/service.go`.
5. **No external API calls exist** in the operator today. The operator uses only the K8s API.

## Recommended Architecture

### Design Principle: Two Reconciliation Domains

The core architectural insight is that the operator now has **two sources of truth** that operate at different cadences:

| Domain | Source of Truth | Trigger | Cadence |
|--------|----------------|---------|---------|
| **Infrastructure** | DittoServer CRD | K8s watch events | Event-driven |
| **Adapter Services** | DittoFS REST API | Periodic polling | Time-driven (30s default) |

These must be **separated** because:
- CRD reconciliation is fast and event-driven (no external calls)
- Adapter reconciliation requires HTTP calls that can fail, timeout, or return stale data
- Mixing them would make CRD reconciliation dependent on DittoFS availability

### High-Level Component Architecture

```
DittoServer CR change                Timer tick (30s)
        |                                   |
        v                                   v
  DittoServerReconciler            AdapterReconciler
  (existing, event-driven)         (new, polling-based)
        |                                   |
        |   +-----------+                   |
        +-->| K8s API   |<-----------------+
        |   | Server    |                   |
        |   +-----------+                   |
        |                                   |
        |                          +------------------+
        |                          | DittoFS API      |
        |                          | (GET /adapters)  |
        |                          +------------------+
        |                                   |
        v                                   v
  StatefulSet, ConfigMap,          Per-adapter Services,
  Headless/API/Metrics Svc,        StatefulSet port patches,
  Secrets, Percona                 NetworkPolicies (optional),
                                   Status.ActiveAdapters
```

### Component Boundaries

| Component | Responsibility | Location | Communicates With |
|-----------|---------------|----------|-------------------|
| **DittoServerReconciler** | Infrastructure resources (existing) | `internal/controller/dittoserver_controller.go` | K8s API |
| **AdapterReconciler** | Dynamic adapter service management | `internal/controller/adapter_reconciler.go` (new) | K8s API, DittoFS API |
| **APIClientFactory** | Authenticated DittoFS API client lifecycle | `pkg/dittoclient/` (new) | DittoFS REST API |
| **AdapterServiceBuilder** | Constructs per-adapter K8s Service objects | `pkg/resources/adapter_service.go` (new) | None (pure builder) |
| **AdapterDiff** | Computes desired vs actual adapter state | `pkg/resources/adapter_diff.go` (new) | None (pure logic) |

### Detailed Component Design

#### 1. AdapterReconciler (Core New Component)

```go
// internal/controller/adapter_reconciler.go

type AdapterReconciler struct {
    client.Client
    Scheme        *runtime.Scheme
    Recorder      record.EventRecorder
    ClientFactory *dittoclient.Factory  // Creates authenticated DittoFS clients
    PollInterval  time.Duration         // Default 30s, configurable
}
```

**Reconciliation flow:**

```
1. Get DittoServer CR
2. Check preconditions:
   - DittoServer exists and not deleting
   - StatefulSet ready (at least 1 replica)
   - API Service has an endpoint
3. Get authenticated API client (via Factory)
4. Poll GET /api/v1/adapters
5. Compute diff: desired adapter Services vs existing adapter Services
6. Apply changes:
   - CREATE Services for new adapters
   - UPDATE Services for changed ports
   - DELETE Services for removed adapters
7. Patch StatefulSet container ports if changed
8. Update DittoServer status.activeAdapters
9. Return RequeueAfter(PollInterval)
```

**Registration with manager:**

```go
// In SetupWithManager or cmd/main.go
func (r *AdapterReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&dittoiov1alpha1.DittoServer{}).
        Named("dittoserver-adapters").
        Complete(r)
}
```

This is a **separate controller** from the infrastructure reconciler. controller-runtime supports multiple controllers for the same resource type using the `Named()` method. Both controllers share the same informer cache, so there is no duplication of watches.

**Why a separate controller, not adding to existing Reconcile():**
- Polling logic adds `RequeueAfter` on every reconcile, which would force the infrastructure reconciler to run unnecessarily every 30s
- API call failures in adapter reconciliation should not block infrastructure reconciliation
- The two concerns have fundamentally different error recovery strategies
- Testing is cleaner (mock the DittoFS API without affecting infrastructure tests)

#### 2. APIClientFactory (Authentication Management)

```go
// pkg/dittoclient/factory.go

type Factory struct {
    client     client.Client     // K8s client to read Secrets
    mu         sync.Mutex
    tokens     map[string]*tokenEntry  // per-DittoServer token cache
}

type tokenEntry struct {
    accessToken  string
    refreshToken string
    expiresAt    time.Time
}
```

**Authentication flow:**

```
1. Factory.GetClient(ctx, dittoServer) called
2. Check token cache for this DittoServer
3. If cached token valid (not expired - 1min buffer):
   - Return apiclient.Client with cached token
4. If cached token expired but refresh token valid:
   - Call POST /api/v1/auth/refresh
   - Cache new tokens
   - Return client
5. If no cached tokens:
   - Read operator credentials Secret from K8s
   - Call POST /api/v1/auth/login
   - Cache tokens
   - Return client
6. On any auth failure: clear cache, retry login once
```

**Credentials source:** The operator reads credentials from a Secret referenced in the DittoServer CRD (new field: `spec.operatorAuth.secretRef`). The Secret contains username/password for the dedicated "operator" service account in DittoFS.

**Why a Factory, not a field on the reconciler:**
- Token lifecycle spans multiple reconcile calls
- Thread-safe caching avoids redundant logins
- Factory can be shared if we add more controllers later
- Clean testability (mock Factory in tests)

#### 3. AdapterDiff (State Comparison)

```go
// pkg/resources/adapter_diff.go

type AdapterState struct {
    Type    string  // "nfs", "smb"
    Port    int
    Enabled bool
    Running bool
}

type DiffResult struct {
    ToCreate []AdapterState  // Adapter running in DittoFS, no K8s Service
    ToUpdate []AdapterState  // Adapter port changed
    ToDelete []string        // K8s Service exists, adapter removed from DittoFS
    PortsChanged bool        // StatefulSet needs port update
}

func ComputeDiff(apiAdapters []AdapterState, existingServices []corev1.Service) DiffResult
```

This is pure logic with no side effects, easy to test exhaustively.

**Matching strategy:** Services are identified by a label `dittofs.io/adapter-type: nfs` (or `smb`). The adapter type is the join key between the API response and K8s Services.

#### 4. AdapterServiceBuilder (K8s Service Construction)

```go
// pkg/resources/adapter_service.go

func NewAdapterService(
    dittoServer *v1alpha1.DittoServer,
    adapterType string,
    port int32,
) *corev1.Service
```

Builds a Service like:
```yaml
apiVersion: v1
kind: Service
metadata:
  name: {dittoserver-name}-{adapter-type}     # e.g., "myserver-nfs"
  namespace: {namespace}
  labels:
    app: dittofs-server
    instance: {dittoserver-name}
    dittofs.io/adapter-type: {adapter-type}    # join key for diff
    dittofs.io/managed-by: adapter-reconciler  # distinguish from static services
  ownerReferences:
    - kind: DittoServer                         # GC on DittoServer deletion
spec:
  type: {spec.service.type}                     # from CRD (LoadBalancer default)
  selector:
    app: dittofs-server
    instance: {dittoserver-name}
  ports:
    - name: {adapter-type}
      port: {port}
      targetPort: {port}
      protocol: TCP
```

### Data Flow

```
                    DittoFS Control Plane
                    (inside the pod)
                           |
                    GET /api/v1/adapters
                           |
                    +------v------+
                    | AdapterRecon|
                    |   ciler     |
                    +------+------+
                           |
              +------------+------------+
              |            |            |
              v            v            v
         K8s Service  K8s Service  StatefulSet
         (nfs)        (smb)        (port patch)
              |            |            |
              v            v            v
         LoadBalancer  LoadBalancer  Pod restarts
         IP assigned   IP assigned   (if ports changed)
```

**Data flow details:**

1. **AdapterReconciler -> DittoFS API**: HTTP GET via API Service's cluster DNS (`{name}-api.{ns}.svc.cluster.local:{apiPort}`). The operator resolves the API endpoint from the DittoServer name, namespace, and control plane port. No external ingress needed.

2. **AdapterReconciler -> K8s API**: Standard controller-runtime client. Uses `CreateOrUpdate` with `SetControllerReference` for GC, plus `mergeServiceSpec` to preserve cloud controller fields (same pattern as existing services).

3. **StatefulSet port patches**: Only needed when the set of adapter ports changes. Uses strategic merge patch on the container spec. This triggers a rolling update (unavoidable for port changes).

### Service Naming Convention

| Service | Name Pattern | Type | Purpose |
|---------|-------------|------|---------|
| Headless | `{name}-headless` | ClusterIP (None) | StatefulSet DNS (existing) |
| API | `{name}-api` | from CRD | Control plane REST API (existing) |
| Metrics | `{name}-metrics` | ClusterIP | Prometheus scraping (existing) |
| NFS adapter | `{name}-nfs` | from CRD | Dynamic, per-adapter (new) |
| SMB adapter | `{name}-smb` | from CRD | Dynamic, per-adapter (new) |

**Migration note:** The existing `{name}-file` service (which currently carries NFS + optional SMB) will be **replaced** by per-adapter services. This is a breaking change that requires migration handling (see Pitfalls).

### CRD Changes

**Fields to remove:**
- `spec.nfsPort` -- port comes from DittoFS API response
- `spec.smb` (entire section) -- adapter config managed via REST API

**Fields to add:**
```yaml
spec:
  # Operator authentication for DittoFS API polling
  operatorAuth:
    # Secret containing operator service account credentials
    # Required keys: "username", "password"
    secretRef:
      name: "{name}-operator-credentials"

  # Adapter service management
  adapters:
    # Polling interval for adapter state (default: 30s)
    pollInterval: "30s"

    # Service configuration for dynamically created adapter services
    service:
      # Annotations to apply to all adapter services
      annotations: {}
      # Service type override (defaults to spec.service.type)
      type: ""
```

**Status additions:**
```yaml
status:
  # Active adapters discovered from DittoFS API
  activeAdapters:
    - type: "nfs"
      port: 12049
      running: true
      serviceReady: true
    - type: "smb"
      port: 12445
      running: true
      serviceReady: true

  # Replace single nfsEndpoint with map
  endpoints:
    nfs: "myserver-nfs.default.svc.cluster.local:12049"
    smb: "myserver-smb.default.svc.cluster.local:12445"
```

### Conditions

Add new condition types:

| Condition | When True | When False |
|-----------|-----------|------------|
| `AdaptersReady` | All running adapters have ready Services | Service creation pending or failed |
| `APIReachable` | Last API poll succeeded | API unreachable or auth failed |

These conditions feed into the existing aggregate `Ready` condition.

### Error Handling Strategy

| Error | Impact | Recovery |
|-------|--------|----------|
| DittoFS API unreachable | No adapter state updates | Requeue with backoff (30s -> 60s -> 120s, cap at 5min). Keep existing Services. Set condition `APIReachable=False`. |
| Auth failure (401/403) | Cannot poll adapters | Clear token cache, retry login. If repeated, emit Warning event. |
| API returns empty adapters | Could mean no adapters configured OR server restarting | Do NOT delete existing Services immediately. Require N consecutive empty polls (default 3) before cleanup. Protects against DittoFS restart. |
| Service creation fails | Adapter not externally reachable | Requeue immediately. Standard K8s retry. |
| StatefulSet port patch conflict | Rolling update delayed | `retryOnConflict` (existing pattern in codebase). |

### Headless Service Port Management

The headless service (`{name}-headless`) is required for StatefulSet DNS and currently only has the NFS port. With dynamic adapters, the headless service needs all adapter ports for proper pod DNS resolution. The AdapterReconciler should **also update the headless service** with all active adapter ports.

### Build Order (Dependencies)

```
Phase 1: Foundation
  |
  +-- pkg/dittoclient/         (API client factory, auth)
  +-- pkg/resources/adapter_diff.go  (pure diff logic)
  +-- pkg/resources/adapter_service.go (service builder)
  |
  |   All three have ZERO dependency on the reconciler.
  |   Can be built and tested in isolation.
  |
Phase 2: Reconciler Core
  |
  +-- internal/controller/adapter_reconciler.go
  |   Depends on: Phase 1 components
  |   Wires everything together.
  |
Phase 3: CRD + Integration
  |
  +-- api/v1alpha1/dittoserver_types.go  (CRD changes)
  +-- api/v1alpha1/dittoserver_webhook.go (validation updates)
  +-- cmd/main.go  (register new controller)
  +-- Remove static nfsPort/smb handling from existing reconciler
  |
Phase 4: Migration + Cleanup
  |
  +-- Remove {name}-file service
  +-- Migration logic for existing deployments
  +-- Update Helm chart, RBAC
  +-- E2E tests
```

## Patterns to Follow

### Pattern 1: Labeled Ownership for Dynamic Resources

**What:** Use labels to identify operator-managed adapter Services rather than relying solely on owner references.

**When:** Any time the operator creates/deletes Services dynamically based on external state.

**Why:** Owner references tell K8s GC to delete children when parent is deleted, but they do not help the operator LIST its own children. Labels provide the query mechanism.

**Example:**
```go
// List all adapter-managed services for this DittoServer
var services corev1.ServiceList
err := r.List(ctx, &services,
    client.InNamespace(ds.Namespace),
    client.MatchingLabels{
        "dittofs.io/managed-by":   "adapter-reconciler",
        "dittofs.io/instance":     ds.Name,
    },
)
```

### Pattern 2: Consecutive-Failure Guard for Deletions

**What:** Require N consecutive polls returning "adapter absent" before deleting its Service.

**When:** The operator would delete a Service based on adapter absence from the API.

**Why:** DittoFS restarts cause temporary API unavailability. Deleting Services immediately would cause traffic interruption during restarts.

**Example:**
```go
// In DittoServer status or an annotation on the Service
// Track consecutive absence count
const requiredAbsenceCount = 3

annotations["dittofs.io/absence-count"] = strconv.Itoa(count + 1)
if count+1 >= requiredAbsenceCount {
    // Safe to delete
}
```

### Pattern 3: Separate Controllers for Separate Cadences

**What:** Use Named controllers to register multiple reconcilers for the same resource type.

**When:** Different aspects of the resource need different reconciliation strategies (event-driven vs polling).

**Why:** Prevents polling RequeueAfter from forcing unnecessary infrastructure reconciliation.

**Example:**
```go
// In cmd/main.go
// Infrastructure controller (event-driven)
if err := (&controller.DittoServerReconciler{...}).SetupWithManager(mgr); err != nil {
    // ...
}
// Adapter controller (polling)
if err := (&controller.AdapterReconciler{...}).SetupWithManager(mgr); err != nil {
    // ...
}
```

## Anti-Patterns to Avoid

### Anti-Pattern 1: Polling Inside the Infrastructure Reconciler

**What:** Adding DittoFS API calls and RequeueAfter to the existing `DittoServerReconciler.Reconcile()`.

**Why bad:** Every CRD change would trigger an API call to DittoFS. Every 30s poll would re-reconcile all infrastructure resources unnecessarily. An API timeout would block Service/ConfigMap updates.

**Instead:** Use a separate named controller with its own reconcile loop.

### Anti-Pattern 2: Storing Adapter State in CRD Spec

**What:** Having users specify adapters in the CRD spec and having the operator create them via API.

**Why bad:** The CRD spec is the "desired state" for K8s resources. Adapter configuration is the "desired state" for DittoFS itself, managed via its own REST API. Mixing these creates a confusing dual-source-of-truth.

**Instead:** The CRD spec contains only K8s infrastructure concerns. DittoFS adapter configuration is managed via `dittofsctl` or the REST API. The operator *reads* adapter state and creates K8s Services to match.

### Anti-Pattern 3: Immediate Deletion on Absence

**What:** Deleting a Service the instant the API stops reporting an adapter.

**Why bad:** DittoFS pod restarts, brief network partitions, or API rate limiting can cause temporary absence. Deleting Services causes LoadBalancer IP loss, DNS propagation delays, and client disruption.

**Instead:** Use the consecutive-failure guard pattern (N=3 polls, ~90s with 30s interval).

### Anti-Pattern 4: Embedding Credentials in the Operator Binary/ConfigMap

**What:** Hardcoding or mounting operator service account credentials directly.

**Why bad:** No rotation, no per-DittoServer isolation, secrets in environment variables are logged.

**Instead:** Reference a K8s Secret per DittoServer. The operator reads it at runtime. Standard K8s Secret rotation tooling applies.

## Scalability Considerations

| Concern | 1 DittoServer | 10 DittoServers | 100 DittoServers |
|---------|--------------|-----------------|-------------------|
| API polling | 1 HTTP call/30s | 10 calls/30s | 100 calls/30s -- consider staggering |
| Token cache | 1 entry | 10 entries | 100 entries -- negligible memory |
| Services created | 2-3 per server | 20-30 total | 200-300 -- within K8s limits |
| Reconciler goroutines | 1 | Up to MaxConcurrentReconciles (default 1) | Consider increasing to 5-10 |
| K8s API rate | Negligible | Negligible | May need client-side rate limiting |

**Recommendation:** For 100+ DittoServers, add jitter to `RequeueAfter` to prevent thundering herd:
```go
jitter := time.Duration(rand.Int63n(int64(5 * time.Second)))
return ctrl.Result{RequeueAfter: r.PollInterval + jitter}, nil
```

## RBAC Requirements

The existing ClusterRole already has full CRUD on Services. New requirements:

| Resource | Verbs | Reason |
|----------|-------|--------|
| `networking.k8s.io/networkpolicies` | create, delete, get, list, patch, update, watch | If NetworkPolicy management is added |
| `core/secrets` | get (already present) | Read operator credentials Secret |

No new RBAC needed for the core feature (Services + StatefulSet patches are already covered).

## Sources

- [controller-runtime reconcile package](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) -- HIGH confidence
- [controller-runtime client package (SSA, Patch)](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client) -- HIGH confidence
- [Kubernetes Operators in 2025 Best Practices](https://outerbyte.com/kubernetes-operators-2025-guide/) -- MEDIUM confidence
- [External State Drift: Self-Healing Controllers](https://anynines.com/blog/external-state-drift-kubernetes-controller-self-healing-design/) -- MEDIUM confidence
- [operator-framework best practices](https://sdk.operatorframework.io/docs/best-practices/common-recommendation/) -- HIGH confidence
- [controller-runtime SSA feature discussion](https://github.com/kubernetes-sigs/controller-runtime/issues/3183) -- MEDIUM confidence
- Existing DittoFS operator codebase at `/Users/marmos91/Projects/dittofs/k8s/dittofs-operator/` -- HIGH confidence
- Existing DittoFS `pkg/apiclient/` -- HIGH confidence
