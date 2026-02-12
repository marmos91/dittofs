# Technology Stack

**Project:** DittoFS K8s Auto-Adapters (Dynamic Service Management)
**Researched:** 2026-02-09

## Recommended Stack

### Core Framework (Already In Place)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| Go | 1.25.1 | Language | Already used by the operator; no reason to change |
| controller-runtime | v0.22.4 | K8s operator framework | Already in go.mod; v0.22.x introduced native SSA support; v0.22.4 is latest patch in the 0.22 line |
| k8s.io/api | v0.34.1 | K8s API types | Already in go.mod; provides `networking/v1.NetworkPolicy`, `core/v1.Service`, `apps/v1.StatefulSet` |
| k8s.io/apimachinery | v0.34.1 | K8s shared machinery | Already in go.mod; required for metav1 types, labels, selectors |
| k8s.io/client-go | v0.34.1 | K8s client | Already in go.mod; provides typed clients and applyconfigurations for SSA |

**Confidence:** HIGH -- these are already in the project's `go.mod` and verified against Go package registry.

### New Dependencies Required

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `k8s.io/api/networking/v1` | v0.34.1 | NetworkPolicy types | Part of `k8s.io/api` already in go.mod; no new dependency needed. Provides `NetworkPolicy`, `NetworkPolicySpec`, `NetworkPolicyIngressRule`, `NetworkPolicyPort` |
| `k8s.io/client-go/applyconfigurations` | v0.34.1 | SSA apply configuration types | Part of `k8s.io/client-go` already in go.mod; provides typed apply configs for Services and NetworkPolicies |

**Confidence:** HIGH -- `networking/v1` is a sub-package of `k8s.io/api` which is already a dependency. No new external modules needed.

### DittoFS API Client (Internal)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `pkg/apiclient` | internal | REST client for DittoFS control plane | Already exists in the DittoFS codebase; provides `ListAdapters()`, `Login()`, `RefreshToken()` -- everything the operator needs to poll adapter state |

**Confidence:** HIGH -- verified by reading `pkg/apiclient/adapters.go` and `pkg/apiclient/auth.go`.

## Key Stack Decisions

### 1. Use `source.Channel` + Polling Goroutine (Not RequeueAfter)

**Decision:** Run a background goroutine that polls `GET /api/v1/adapters` and pushes events to a `source.Channel`, rather than using `RequeueAfter` on the main reconciler.

**Rationale:**
- The main DittoServer reconciler is CR-driven (watches DittoServer spec changes). Adapter state is external data, not part of the CR spec.
- `source.Channel` is controller-runtime's official mechanism for "events originating outside the cluster" (verified via pkg.go.dev docs).
- Mixing `RequeueAfter` for polling into the main reconciler conflates two concerns: (1) reconciling CR spec changes and (2) syncing external API state.
- A dedicated polling goroutine with `source.Channel` keeps the adapter sync loop cleanly separated and testable.
- The goroutine lifecycle is managed by the controller-runtime Manager (started when Manager starts, stopped on context cancellation).

**Pattern:**
```go
// Polling goroutine sends events to channel
events := make(chan event.TypedGenericEvent[*v1alpha1.DittoServer])

// Controller watches the channel
ctrl.NewControllerManagedBy(mgr).
    For(&v1alpha1.DittoServer{}).
    Watches(source.Channel(events, &handler.EnqueueRequestForObject{})).
    // ... existing watches
```

**Confidence:** HIGH -- `source.Channel` is documented in the official controller-runtime v0.22 source package.

### 2. Keep `controllerutil.CreateOrUpdate` (Not SSA for Dynamic Services)

**Decision:** Use `controllerutil.CreateOrUpdate` for dynamically managed Services and NetworkPolicies, not Server-Side Apply.

**Rationale:**
- The existing operator already uses `CreateOrUpdate` consistently for all resources (verified in `dittoserver_controller.go`).
- The dynamic adapter Services are single-owner resources (only the operator manages them). SSA's main benefit (multi-controller field ownership) does not apply here.
- `CreateOrUpdate` with retry-on-conflict is already implemented and tested (the `retryOnConflict` helper exists).
- SSA introduces complexity with zero-value fields and ApplyConfiguration types that add boilerplate without benefit for this use case.
- The operator already handles cloud controller preservation via `mergeServiceSpec()` and `mergePorts()`.

**Caveat:** If the project later adds multi-controller management of the same Services, revisit this decision. For now, single-owner `CreateOrUpdate` is simpler and already proven.

**Confidence:** HIGH -- verified existing patterns in the codebase; SSA would add complexity without benefit for single-owner resources.

### 3. Reuse `pkg/apiclient` for API Communication

**Decision:** Import and use `pkg/apiclient` from the main DittoFS codebase, not build a new HTTP client.

**Rationale:**
- `pkg/apiclient` already implements `ListAdapters()`, `Login()`, `RefreshToken()`, and proper error handling (`APIError` type with `IsAuthError()`, `IsNotFound()`).
- Token management is built in (`WithToken()`, `SetToken()`).
- Using the same client ensures API compatibility -- if the API changes, the client gets updated in one place.
- The operator module already imports the DittoFS Go module path (`github.com/marmos91/dittofs/k8s/dittofs-operator`), so adding a dependency on `pkg/apiclient` is straightforward.

**Implementation note:** The operator will need to import `pkg/apiclient` from the parent module. This may require a `replace` directive in `go.mod` if the modules are not published separately, or restructuring as a multi-module workspace.

**Confidence:** MEDIUM -- the apiclient code is verified and suitable; the module import path may need workspace configuration.

### 4. Use `networking.k8s.io/v1` NetworkPolicy (Not AdminNetworkPolicy)

**Decision:** Use the standard `NetworkPolicy` resource from `networking.k8s.io/v1`, not the newer `AdminNetworkPolicy` from `policy.networking.k8s.io`.

**Rationale:**
- `NetworkPolicy` (v1) is GA and universally supported by all CNI plugins that support network policies (Calico, Cilium, Antrea, etc.).
- `AdminNetworkPolicy` is a newer API from the Network Policy API working group (`sigs.k8s.io/network-policy-api`), still evolving and not universally supported.
- The operator's use case is simple: restrict ingress to adapter pods on specific ports. Standard `NetworkPolicy` handles this perfectly.
- No need for cluster-scoped policies or priority-based evaluation that AdminNetworkPolicy provides.

**Confidence:** HIGH -- `networking.k8s.io/v1` has been stable since Kubernetes 1.7 and is the standard approach.

### 5. Store Operator Credentials in a K8s Secret (Not ServiceAccount Token)

**Decision:** The operator authenticates to the DittoFS API using credentials stored in a Kubernetes Secret, not a K8s ServiceAccount token.

**Rationale:**
- DittoFS uses its own JWT-based auth system (`/api/v1/auth/login` returns access + refresh tokens). It does not integrate with Kubernetes RBAC/ServiceAccount tokens.
- The operator needs to authenticate as a DittoFS user with the "operator" role, not as a Kubernetes service account.
- Credentials (username + password or pre-generated token) are stored in a K8s Secret referenced by the DittoServer CR, similar to existing patterns for JWT secrets and S3 credentials.
- Token refresh is handled by the polling goroutine using `apiclient.RefreshToken()`.

**Confidence:** HIGH -- verified auth API in `pkg/apiclient/auth.go`; consistent with existing Secret-based credential patterns in the operator.

## Version Compatibility Matrix

| Component | Version | K8s Compat | Notes |
|-----------|---------|------------|-------|
| controller-runtime | v0.22.4 | K8s 1.34.x | Verified in go.mod |
| k8s.io/api | v0.34.1 | K8s 1.34.x | `networking/v1` NetworkPolicy included |
| k8s.io/client-go | v0.34.1 | K8s 1.34.x | applyconfigurations included |
| Go | 1.25.1 | N/A | Required by go.mod |

**Confidence:** HIGH -- all versions verified from the project's `go.mod`.

## Upgrade Path Consideration

The project is on controller-runtime v0.22.4. The latest stable is v0.23.1 (released Jan 26, 2025). v0.23.0 added subresource Apply support and generic webhook validators. Upgrading is optional for this milestone -- v0.22.4 has everything needed.

**Recommendation:** Stay on v0.22.4 for this milestone. Upgrade to v0.23.x in a separate maintenance milestone if the new features are needed.

## Alternatives Considered

| Category | Recommended | Alternative | Why Not |
|----------|-------------|-------------|---------|
| Polling mechanism | `source.Channel` + goroutine | `RequeueAfter` in reconciler | Conflates CR reconciliation with API polling; harder to test; mixes concerns |
| Resource mutation | `CreateOrUpdate` | Server-Side Apply | SSA adds complexity for single-owner resources; existing patterns use CreateOrUpdate |
| API client | `pkg/apiclient` (internal) | New HTTP client | Duplication; apiclient already has auth, token refresh, error handling |
| NetworkPolicy API | `networking.k8s.io/v1` | `policy.networking.k8s.io` AdminNetworkPolicy | AdminNetworkPolicy not GA; not widely supported; overkill for simple port-based ingress rules |
| Auth storage | K8s Secret | K8s ServiceAccount token | DittoFS has its own JWT auth; doesn't integrate with K8s RBAC |
| Event source | `source.Channel` | Custom Informer | Over-engineered; no K8s resource to watch; Channel is the idiomatic approach for external events |

## No New External Dependencies

All required functionality is available through packages already in the dependency tree:

```
# Already in go.mod (no changes needed):
k8s.io/api v0.34.1                     # networking/v1.NetworkPolicy
k8s.io/client-go v0.34.1               # applyconfigurations/*
sigs.k8s.io/controller-runtime v0.22.4  # source.Channel, controllerutil
```

The only new import will be `pkg/apiclient` from the parent DittoFS module, which is an internal dependency requiring module configuration.

## RBAC Additions Required

The operator already has RBAC markers for Services and Secrets. New markers needed:

```go
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
```

This is the only new RBAC permission. Service and StatefulSet permissions already exist.

**Confidence:** HIGH -- verified existing RBAC markers in `dittoserver_controller.go`.

## Sources

- [controller-runtime v0.22.4 releases](https://github.com/kubernetes-sigs/controller-runtime/releases) -- verified version and features
- [controller-runtime source.Channel docs](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/source) -- verified Channel API for external events
- [k8s.io/api networking/v1 package](https://pkg.go.dev/k8s.io/api/networking/v1) -- NetworkPolicy types
- [Kubernetes NetworkPolicy docs](https://kubernetes.io/docs/concepts/services-networking/network-policies/) -- NetworkPolicy spec and behavior
- [Kubernetes operator best practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/) -- operator pattern guidance
- [controller-runtime SSA support](https://github.com/kubernetes-sigs/controller-runtime/issues/2733) -- CreateOrUpdate vs SSA analysis
- Project source: `k8s/dittofs-operator/go.mod` -- current dependency versions
- Project source: `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- existing patterns
- Project source: `pkg/apiclient/adapters.go` -- adapter API client
- Project source: `pkg/apiclient/auth.go` -- authentication API
