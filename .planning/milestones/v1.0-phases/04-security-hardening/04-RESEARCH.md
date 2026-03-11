# Phase 4: Security Hardening - Research

**Researched:** 2026-02-10
**Domain:** Kubernetes CRD field removal, config generation cleanup, NetworkPolicy lifecycle
**Confidence:** HIGH

## Summary

Phase 4 has two distinct workstreams: (1) removing legacy static adapter fields from the CRD and config generation, and (2) implementing per-adapter NetworkPolicy lifecycle management. Both are well-scoped with clear boundaries.

The CRD removal is primarily a deletion exercise. The `spec.nfsPort` and `spec.smb` fields must be removed from `DittoServerSpec`, along with all code that reads them (static Service ports, static container ports, config generation of adapter sections, probe endpoints using NFS port). After Phase 3, dynamic adapter ports (prefixed `adapter-`) coexist with static ones (named `nfs`, `smb`). Phase 4 removes the static side entirely, leaving only the dynamic adapter-driven approach.

The NetworkPolicy work follows the exact same pattern as the adapter Service reconciler from Phase 3: list desired state from `lastKnownAdapters`, diff against existing NetworkPolicy resources (selected by label), create/update/delete to converge. The `networking.k8s.io/v1` NetworkPolicy API is stable (GA since Kubernetes 1.7) and available in `k8s.io/api/networking/v1`, which is already a transitive dependency of `k8s.io/api v0.34.1`.

**Primary recommendation:** Follow the Phase 3 adapter Service pattern exactly for NetworkPolicy lifecycle. For CRD removal, proceed methodically through every reference to `NFSPort`, `SMB`, `nfs.GetNFSPort()`, `smb.GetSMBPort()`, and the static `buildContainerPorts`/`buildServicePorts` functions.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `k8s.io/api/networking/v1` | v0.34.1 | NetworkPolicy Go types | Part of existing `k8s.io/api` dependency |
| `sigs.k8s.io/controller-runtime` | v0.22.4 | Client for CRUD on NetworkPolicy | Already used for all other resources |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `k8s.io/apimachinery/pkg/apis/meta/v1` | v0.34.1 | ObjectMeta, LabelSelector | Already used everywhere |
| `k8s.io/apimachinery/pkg/util/intstr` | v0.34.1 | IntOrString for NetworkPolicy ports | Already imported |

### Alternatives Considered
None. All required libraries are already in use or are transitive dependencies of existing imports. No new external dependencies needed.

**Installation:**
```bash
# No new dependencies needed. k8s.io/api/networking/v1 is already available
# through the existing k8s.io/api v0.34.1 dependency.
go mod tidy  # After adding the import
```

## Architecture Patterns

### Recommended Project Structure

All changes are within the existing `k8s/dittofs-operator/` tree:

```
k8s/dittofs-operator/
├── api/v1alpha1/
│   ├── dittoserver_types.go          # MODIFY: Remove NFSPort, SMB fields
│   ├── dittoserver_types_builder.go  # MODIFY: Remove WithNFSPort, WithSMB builders
│   ├── helpers.go                    # MODIFY: Remove nfsPort references in API URL
│   └── zz_generated.deepcopy.go     # REGENERATE: After type changes
├── internal/controller/
│   ├── dittoserver_controller.go     # MODIFY: Remove static port/service functions, update probes
│   ├── service_reconciler.go         # NO CHANGE (already adapter-driven)
│   ├── networkpolicy_reconciler.go   # NEW: Per-adapter NetworkPolicy lifecycle
│   └── networkpolicy_reconciler_test.go  # NEW: Tests
├── internal/controller/config/
│   ├── config.go                     # NO CHANGE (already infrastructure-only, no adapters)
│   └── types.go                      # NO CHANGE
├── utils/
│   ├── nfs/nfs.go                    # DELETE: No longer needed
│   └── smb/smb.go                    # DELETE: No longer needed (or remove adapter-related exports)
└── config/
    └── crd/bases/                    # REGENERATE: After type changes
```

### Pattern 1: Label-Based Resource Diff (from Phase 3)

**What:** Use labels to identify operator-managed resources, then diff desired vs actual state.
**When to use:** Any time the operator manages a set of child resources keyed by adapter type.
**Example:**
```go
// Source: existing service_reconciler.go pattern
const (
    adapterNetworkPolicyLabel = "dittofs.io/adapter-networkpolicy"
    adapterTypeLabel          = "dittofs.io/adapter-type"
)

func networkPolicyName(crName, adapterType string) string {
    return fmt.Sprintf("%s-adapter-%s", crName, adapterType)
}

func networkPolicyLabels(crName, adapterType string) map[string]string {
    return map[string]string{
        "app":                      "dittofs-server",
        "instance":                 crName,
        adapterNetworkPolicyLabel:  "true",
        adapterTypeLabel:           adapterType,
    }
}
```

### Pattern 2: NetworkPolicy Spec Construction

**What:** Build a NetworkPolicy that allows ingress only on a specific adapter port.
**When to use:** For each enabled+running adapter.
**Example:**
```go
// Source: Kubernetes networking.k8s.io/v1 API
import networkingv1 "k8s.io/api/networking/v1"

func buildAdapterNetworkPolicy(crName, namespace, adapterType string, port int32) *networkingv1.NetworkPolicy {
    return &networkingv1.NetworkPolicy{
        ObjectMeta: metav1.ObjectMeta{
            Name:      networkPolicyName(crName, adapterType),
            Namespace: namespace,
            Labels:    networkPolicyLabels(crName, adapterType),
        },
        Spec: networkingv1.NetworkPolicySpec{
            PodSelector: metav1.LabelSelector{
                MatchLabels: map[string]string{
                    "app":      "dittofs-server",
                    "instance": crName,
                },
            },
            PolicyTypes: []networkingv1.PolicyType{
                networkingv1.PolicyTypeIngress,
            },
            Ingress: []networkingv1.NetworkPolicyIngressRule{
                {
                    Ports: []networkingv1.NetworkPolicyPort{
                        {
                            Protocol: protocolPtr(corev1.ProtocolTCP),
                            Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: port},
                        },
                    },
                },
            },
        },
    }
}
```

### Pattern 3: Static Field Removal with Compilation Guard

**What:** Remove CRD fields and let the Go compiler find all broken references.
**When to use:** When removing deprecated fields.
**How:**
1. Remove the field from `dittoserver_types.go`
2. Run `go build ./...` -- compiler errors show every reference
3. Fix each reference systematically
4. Run `make generate manifests` to regenerate CRD and deepcopy

### Anti-Patterns to Avoid
- **Keeping deprecated fields as optional:** The requirement explicitly says "no longer has" these fields. Do not keep them with deprecation markers.
- **Multiple NetworkPolicies per pod with overlapping selectors:** NetworkPolicies are additive. Having separate per-adapter policies is correct -- they are ORed together, so traffic matching ANY policy is allowed.
- **Default-deny NetworkPolicy:** Do NOT create a blanket deny policy. The requirement is to allow ingress on active adapter ports, not to deny everything else. The per-adapter NetworkPolicies already scope ingress. If a default-deny is desired, it should be deployed separately by the cluster admin, not by this operator.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| NetworkPolicy YAML struct | Custom struct | `networkingv1.NetworkPolicy` from `k8s.io/api/networking/v1` | Full type safety, deepcopy, serialization |
| Label-based list/diff | Custom filtering | `client.MatchingLabels{}` selector | Already proven in service_reconciler.go |
| Owner reference setup | Manual UID wiring | `controllerutil.SetControllerReference()` | Handles GVK, UID, blockOwnerDeletion correctly |
| Optimistic locking retry | Custom retry loop | Existing `retryOnConflict()` helper | Already defined in controller, handles backoff |
| Service builder | Raw struct construction | `resources.NewServiceBuilder()` | Already exists in pkg/resources |

**Key insight:** Phase 4 should introduce zero new patterns. Every mechanism needed (label-based diff, owner references, event recording, retry-on-conflict, RBAC markers) already exists from Phases 1-3.

## Common Pitfalls

### Pitfall 1: Forgetting to Update RBAC Markers
**What goes wrong:** The operator creates NetworkPolicy objects but lacks RBAC permission, causing silent failures.
**Why it happens:** kubebuilder RBAC markers must be added for the new resource type.
**How to avoid:** Add `+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete` marker and run `make manifests` to regenerate ClusterRole.
**Warning signs:** "forbidden" errors in operator logs when creating NetworkPolicies.

### Pitfall 2: Breaking Probes When Removing NFS Port
**What goes wrong:** The current liveness/readiness probes reference `getAPIPort()` but the startup probe and the NFSEndpoint status field use `nfs.GetNFSPort()`. Removing the NFS port utility without updating probes causes pod crash loops.
**Why it happens:** The `nfs.GetNFSPort()` function is used in the `NFSEndpoint` status field and the file Service. After removal, these need updating.
**How to avoid:** Search for ALL usages of `nfs.GetNFSPort()` and `smb.GetSMBPort()` before deleting the utility packages. The NFSEndpoint status field and the file Service are the main consumers.
**Warning signs:** Compilation errors (caught early, good). The static `-file` Service currently includes the NFS port -- this Service may need to be made dynamic or removed.

### Pitfall 3: NetworkPolicy Pod Selector Must Match StatefulSet Labels
**What goes wrong:** NetworkPolicy targets wrong pods or no pods.
**Why it happens:** The `podSelector` in the NetworkPolicy must match the labels on the DittoFS server pods.
**How to avoid:** Use the same label set (`app: dittofs-server`, `instance: <cr-name>`) that the StatefulSet template uses.
**Warning signs:** NetworkPolicy exists but traffic is not filtered (policy has no effect because selector doesn't match).

### Pitfall 4: Static File Service Becomes Orphaned
**What goes wrong:** The `-file` Service currently includes the static NFS port and optional SMB port. After removing `spec.nfsPort` and `spec.smb`, the file Service has no ports to expose.
**Why it happens:** The `reconcileFileService()` function in the controller reads these fields to build service ports.
**How to avoid:** Either remove the `-file` Service entirely (adapter Services from Phase 3 already handle per-adapter exposure), or repurpose it. Since adapter Services provide per-adapter LoadBalancers, the `-file` Service is redundant and should be removed.
**Warning signs:** Empty or broken `-file` Service after field removal.

### Pitfall 5: CRD Regeneration Breaks Existing CRs
**What goes wrong:** Existing DittoServer CRs in a cluster have `nfsPort` or `smb` fields set. After CRD update, these fields are rejected by validation.
**Why it happens:** kubebuilder validation rejects unknown fields by default.
**How to avoid:** Since this is `v1alpha1` (pre-GA), breaking changes are acceptable. Document the migration in release notes. The fields being removed were `+optional`, so existing CRs without them are unaffected. CRs with them need manual removal of the deprecated fields.
**Warning signs:** CRD upgrade fails or existing CRs become invalid.

### Pitfall 6: Headless Service Still References NFS Port
**What goes wrong:** The headless Service (used for StatefulSet DNS) currently uses the NFS port as its sole port.
**Why it happens:** `reconcileHeadlessService()` calls `nfs.GetNFSPort()`.
**How to avoid:** Change the headless Service to use the API port (8080) instead -- the API port is always available and the headless Service only needs a port for DNS resolution (the actual port number is less critical for headless services).
**Warning signs:** Headless Service fails to create after NFS port removal.

### Pitfall 7: NetworkPolicy Without CNI Support
**What goes wrong:** NetworkPolicies are created but have no effect.
**Why it happens:** NetworkPolicy enforcement requires a CNI plugin that supports NetworkPolicies (Calico, Cilium, etc.). Vanilla clusters with basic CNI may not enforce them.
**How to avoid:** This is an operational concern, not an operator code issue. Document in the operator docs that a NetworkPolicy-aware CNI is required for SECU-03/SECU-04 enforcement. The operator should create the NetworkPolicy regardless -- it is the cluster admin's responsibility to have the right CNI.
**Warning signs:** Traffic still reaches closed adapter ports despite NetworkPolicy deletion.

## Code Examples

### Example 1: NetworkPolicy Reconciler (Main Loop)

Following the exact pattern of `reconcileAdapterServices()`:

```go
// Source: modeled after service_reconciler.go
func (r *DittoServerReconciler) reconcileNetworkPolicies(ctx context.Context, ds *dittoiov1alpha1.DittoServer) error {
    logger := logf.FromContext(ctx)

    // Safety: skip if no successful poll has occurred yet.
    adapters := r.getLastKnownAdapters(ds)
    if adapters == nil {
        logger.V(1).Info("No adapter poll yet, skipping NetworkPolicy reconciliation")
        return nil
    }

    // Build desired set: enabled AND running adapters.
    desired := make(map[string]AdapterInfo)
    for _, a := range adapters {
        if a.Enabled && a.Running {
            desired[a.Type] = a
        }
    }

    // List existing adapter NetworkPolicies.
    var existingList networkingv1.NetworkPolicyList
    if err := r.List(ctx, &existingList,
        client.InNamespace(ds.Namespace),
        client.MatchingLabels{
            adapterNetworkPolicyLabel: "true",
            "instance":               ds.Name,
        },
    ); err != nil {
        return fmt.Errorf("failed to list adapter network policies: %w", err)
    }

    // Build actual set keyed by adapter type.
    actual := make(map[string]*networkingv1.NetworkPolicy)
    for i := range existingList.Items {
        np := &existingList.Items[i]
        adapterType := np.Labels[adapterTypeLabel]
        if adapterType != "" {
            actual[adapterType] = np
        }
    }

    // Create NetworkPolicies for desired adapters not yet present.
    for adapterType, info := range desired {
        if _, exists := actual[adapterType]; !exists {
            if err := r.createAdapterNetworkPolicy(ctx, ds, adapterType, info); err != nil {
                return err
            }
        }
    }

    // Update NetworkPolicies when port changes.
    for adapterType, np := range actual {
        if info, stillDesired := desired[adapterType]; stillDesired {
            if err := r.updateAdapterNetworkPolicyIfNeeded(ctx, ds, np, info); err != nil {
                return err
            }
        }
    }

    // Delete NetworkPolicies for stopped/removed adapters.
    for adapterType, np := range actual {
        if _, stillDesired := desired[adapterType]; !stillDesired {
            if err := r.deleteAdapterNetworkPolicy(ctx, ds, np, adapterType); err != nil {
                return err
            }
        }
    }

    return nil
}
```

### Example 2: Removing Static Fields from CRD Types

```go
// BEFORE (in dittoserver_types.go):
type DittoServerSpec struct {
    // ... other fields ...
    NFSPort *int32 `json:"nfsPort,omitempty"`
    SMB *SMBAdapterSpec `json:"smb,omitempty"`
}

// AFTER:
type DittoServerSpec struct {
    // ... other fields ...
    // NFSPort REMOVED (SECU-01)
    // SMB REMOVED (SECU-01)
}
```

### Example 3: Updating SetupWithManager for NetworkPolicy Watches

```go
// Source: existing SetupWithManager pattern
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
    builder := ctrl.NewControllerManagedBy(mgr).
        For(&dittoiov1alpha1.DittoServer{}).
        Owns(&appsv1.StatefulSet{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Owns(&corev1.Secret{}).
        Owns(&networkingv1.NetworkPolicy{}).  // NEW: Watch owned NetworkPolicies
        Named("dittoserver")

    // ... rest unchanged ...
}
```

### Example 4: RBAC Marker Addition

```go
// Add to dittoserver_controller.go RBAC markers:
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
```

## State of the Art

| Old Approach (Phase 3) | New Approach (Phase 4) | When Changed | Impact |
|-------------------------|------------------------|--------------|--------|
| Static `spec.nfsPort` + dynamic adapter ports coexist | Only dynamic adapter ports | Phase 4 | CRD breaking change (v1alpha1) |
| Static `spec.smb` adapter config in CRD | Adapters managed entirely via REST API | Phase 4 | CRD breaking change (v1alpha1) |
| Static `-file` Service with NFS/SMB ports | Per-adapter Services only | Phase 4 | Service topology change |
| No network traffic restriction | Per-adapter NetworkPolicy ingress rules | Phase 4 | Security improvement |

**Deprecated/outdated:**
- `utils/nfs/nfs.go`: Entire package becomes dead code after `spec.nfsPort` removal
- `utils/smb/smb.go`: All adapter-specific exports become dead code
- `buildContainerPorts()`: Static port builder in controller is replaced by dynamic `reconcileContainerPorts()`
- `buildServicePorts()`: Static service port builder -- no longer called
- `reconcileFileService()`: Static file Service reconciler -- replaced by per-adapter Services

## Scope Mapping to Requirements

### SECU-01: Remove static CRD fields
- Remove `NFSPort *int32` from `DittoServerSpec`
- Remove `SMB *SMBAdapterSpec` from `DittoServerSpec`
- Remove all associated types: `SMBAdapterSpec`, `SMBTimeoutsSpec`, `SMBCreditsSpec`
- Remove builder functions: `WithNFSPort()`, `WithSMB()`
- Regenerate CRD manifests and deepcopy

### SECU-02: Stop emitting adapter config in YAML
- Verify `config.GenerateDittoFSConfig()` -- it already does NOT emit adapter sections (confirmed by reading `config.go`). The config is infrastructure-only.
- The requirement is already satisfied by the Phase 1-3 design. However, we should verify no adapter-related env vars or config keys are emitted from the removed fields.
- Remove any references to NFS/SMB ports in environment variables or ConfigMap generation.

### SECU-03: Create NetworkPolicy per active adapter
- New `reconcileNetworkPolicies()` function
- Uses same `lastKnownAdapters` data as adapter Service reconciler
- Creates NetworkPolicy allowing TCP ingress on adapter port
- Sets owner reference for GC
- Emits K8s events

### SECU-04: Delete NetworkPolicy when adapter stops
- Same diff-based approach as adapter Service deletion
- Within one polling cycle (30s default)
- Emits K8s events

## Impact Analysis: What References NFSPort/SMB

All references found in the `k8s/dittofs-operator/` tree:

### `nfs.GetNFSPort()` Callers
1. `dittoserver_controller.go:reconcileHeadlessService()` -- headless Service port
2. `dittoserver_controller.go:reconcileFileService()` -- file Service NFS port
3. `dittoserver_controller.go:buildContainerPorts()` -- static NFS container port
4. `dittoserver_controller.go:updateStatus()` -- NFSEndpoint status field

### `smb.GetSMBPort()` Callers
1. `dittoserver_controller.go:reconcileFileService()` -- file Service SMB port
2. `dittoserver_controller.go:buildContainerPorts()` -- static SMB container port

### `spec.NFSPort` Direct References
1. `dittoserver_types.go` -- field definition
2. `dittoserver_types_builder.go` -- `WithNFSPort()` builder
3. `utils/nfs/nfs.go` -- `GetNFSPort()` reads it

### `spec.SMB` Direct References
1. `dittoserver_types.go` -- field definition + all SMB-related types
2. `dittoserver_types_builder.go` -- `WithSMB()` builder
3. `utils/smb/smb.go` -- all `Get*()` functions read it
4. `dittoserver_controller.go:buildContainerPorts()` -- checks `SMB.Enabled`
5. `dittoserver_controller.go:reconcileFileService()` -- checks `SMB.Enabled`

### Resolution Plan
- **Headless Service:** Change port to API port (8080) -- always available
- **File Service:** Remove entirely (adapter Services handle this)
- **buildContainerPorts():** Remove static NFS/SMB ports. Keep only `api` and optionally `metrics`. Dynamic ports are added by `reconcileContainerPorts()`.
- **NFSEndpoint status:** Remove or repurpose. Since NFS port is no longer statically configured, this field loses meaning. Consider removing it or making it derived from adapter state.
- **utils/nfs, utils/smb:** Delete entirely

## Open Questions

1. **Should the `-file` Service be removed or repurposed?**
   - What we know: Adapter Services from Phase 3 provide per-adapter LoadBalancer/NodePort/ClusterIP. The `-file` Service served the same purpose statically.
   - What's unclear: Whether any documentation or user workflows depend on the `-file` Service name.
   - Recommendation: Remove it. The adapter Services (`<name>-adapter-nfs`, `<name>-adapter-smb`) are the replacement. Document the migration.

2. **What happens to `status.nfsEndpoint` after NFS port removal?**
   - What we know: This field computes the endpoint from the static NFS port. Without static port, it has no source.
   - What's unclear: Whether any external tooling reads this field.
   - Recommendation: Remove the field from status. If needed later, derive from adapter state. Since `v1alpha1`, breaking status changes are acceptable.

3. **Should NetworkPolicies also allow API and metrics ports?**
   - What we know: The requirements specify "per active adapter" NetworkPolicies. API (8080) and metrics (9090) ports are infrastructure, not adapters.
   - What's unclear: Whether the API/metrics Services need their own NetworkPolicies.
   - Recommendation: Phase 4 scope is adapter NetworkPolicies only. API/metrics ports are exposed via their own ClusterIP Services and can be protected separately. Do not conflate concerns.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` -- current CRD fields
- Codebase analysis: `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- all static port references
- Codebase analysis: `k8s/dittofs-operator/internal/controller/service_reconciler.go` -- adapter Service pattern to replicate
- Codebase analysis: `k8s/dittofs-operator/internal/controller/config/config.go` -- confirmed no adapter config in YAML
- Codebase analysis: `k8s/dittofs-operator/utils/nfs/nfs.go` and `utils/smb/smb.go` -- utility functions to remove
- [Kubernetes NetworkPolicy API](https://kubernetes.io/docs/concepts/services-networking/network-policies/) -- GA API, stable
- [k8s.io/api/networking/v1 Go Package](https://pkg.go.dev/k8s.io/api/networking/v1) -- Go types for NetworkPolicy

### Secondary (MEDIUM confidence)
- Codebase analysis: `k8s/dittofs-operator/internal/controller/auth_reconciler_test.go` -- test patterns (`setupAuthReconciler`, fake client)
- Codebase analysis: `k8s/dittofs-operator/internal/controller/service_reconciler_test.go` -- test patterns for resource lifecycle

### Tertiary (LOW confidence)
- None. All findings are from direct codebase analysis and stable Kubernetes APIs.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- no new dependencies, uses existing k8s.io/api and controller-runtime
- Architecture: HIGH -- exact replication of proven Phase 3 patterns
- Pitfalls: HIGH -- identified through direct code analysis of all reference sites
- CRD removal scope: HIGH -- comprehensive grep of all NFSPort/SMB references in codebase

**Research date:** 2026-02-10
**Valid until:** No expiration (stable APIs, codebase-specific findings)
