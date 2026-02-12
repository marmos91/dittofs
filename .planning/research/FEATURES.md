# Feature Landscape

**Domain:** K8s operator dynamic adapter-driven service management for DittoFS
**Researched:** 2026-02-09
**Confidence:** HIGH (based on codebase analysis + K8s ecosystem patterns)

## Table Stakes

Features the operator must have for this milestone to be useful. Missing any of these means the feature is half-baked and users fall back to static configuration.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| Poll adapter API at configurable interval | Core mechanism -- operator needs to discover which adapters are running. Without this, nothing works. | Low | Use `RequeueAfter` in reconcile loop. Default 30s per PROJECT.md. CRD field `spec.adapterPolling.interval`. |
| Create LoadBalancer Service per running adapter | Users expect each protocol to get its own external IP. One LB per adapter (NFS, SMB) is the whole point. | Med | Service named `{instance}-{adapterType}` (e.g., `myserver-nfs`). Port from API response, not hardcoded. Owner reference to DittoServer CR. |
| Delete Service when adapter stops/is-removed | Stale Services pointing to nonexistent adapters are a security risk and confuse users. | Low | Reconcile loop compares API state vs existing Services. Delete orphaned ones. |
| Update container ports on StatefulSet to match active adapters | K8s best practice -- container ports should reflect what the pod actually listens on. Required for some CNI/NetworkPolicy enforcement. | Med | Must handle StatefulSet immutability carefully -- only pod template ports are mutable on update. |
| Auto-create DittoFS service account with operator role | Zero-config experience. Operator should not require manual user provisioning inside DittoFS. | Med | Operator creates user via `POST /api/v1/users` on startup if not exists. Stores credentials in a K8s Secret. Requires new "operator" role in DittoFS. |
| New "operator" role in DittoFS (least privilege) | Operator should not use admin credentials. Principle of least privilege -- read-only adapter access is all that is needed. | Med | New `UserRole` value in `pkg/controlplane/models/user.go`. New `RequireRole("operator","admin")` middleware or `RequireAdapterRead()`. Route `GET /api/v1/adapters` to accept operator role. |
| Remove static adapter config from CRD | API is single source of truth for adapters. Static `spec.nfsPort` and `spec.smb` fields create confusion when they disagree with runtime state. | Med | Remove `NFSPort *int32`, `SMB *SMBAdapterSpec` from `DittoServerSpec`. Remove `nfs`/`smb` utility packages. Update config generation. Webhook migration path. |
| Remove adapter section from DittoFS YAML config | Same rationale as CRD removal -- config file should not define adapters when the control plane API owns them. | Low | `EnsureDefaultAdapters()` already handles creation at startup. Config generation in operator just stops emitting adapter sections. |
| Graceful handling of DittoFS unavailability | Operator must not crash or spam errors when DittoFS pod is starting, restarting, or temporarily unavailable. | Low | On API error, log warning, keep existing Services, requeue with backoff. Only modify Services when API returns a successful response. |
| Headless service updates | Headless service for StatefulSet DNS must also reflect active adapter ports for proper pod DNS resolution. | Low | Update headless service ports to match active adapters, same as file service pattern. |

## Differentiators

Features that elevate the operator beyond basic functionality. Not strictly required but add significant value.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| NetworkPolicy per active adapter | Restrict traffic to only active adapter ports. Reduces attack surface dynamically as adapters start/stop. | Med | Create/delete NetworkPolicy resources alongside Services. Ingress rules allow traffic only to active adapter ports. Requires `networking.k8s.io/v1` RBAC. |
| Adapter status in CRD status | Users can `kubectl get dittoserver -o wide` and see which adapters are running and their endpoints -- no need to shell into the pod or call the API. | Low | Add `status.adapters: [{type, port, running, endpoint}]` to `DittoServerStatus`. Update on each poll cycle. |
| Adapter-level status conditions | Standard K8s condition pattern: `AdaptersReady` condition that reflects whether adapter polling succeeded and Services are in sync. | Low | Add `AdaptersReady` condition to existing conditions framework. Integrates with existing `updateReadyCondition` aggregate. |
| Token refresh and credential rotation | JWT tokens expire. Operator should handle token refresh automatically without restarting. | Med | Store refresh token in Secret. Use `RefreshToken()` API call before access token expires. Fall back to re-login if refresh fails. |
| Service annotations passthrough | Users need cloud-provider-specific annotations on per-adapter LoadBalancer Services (e.g., AWS NLB, internal LB). | Low | CRD field `spec.adapterService.annotations` applied to all adapter Services. Per-adapter override via `spec.adapterService.overrides.{type}.annotations`. |
| Service type configurability | Some environments need NodePort or ClusterIP instead of LoadBalancer for adapter services. | Low | CRD field `spec.adapterService.type` (default: LoadBalancer). Same as existing `spec.service.type` pattern. |
| Events for adapter lifecycle | K8s events give observability: "Created NFS Service on port 12049", "Deleted SMB Service -- adapter stopped". | Low | Use existing `Recorder.Event()` pattern. Already well-established in current controller. |
| Configurable service naming | Allow users to customize Service names for DNS predictability (e.g., `my-nfs-share` instead of `myserver-nfs`). | Low | CRD field `spec.adapterService.nameTemplate` with default `{instance}-{adapter}`. |
| Exponential backoff on API failures | Smarter retry than fixed interval when DittoFS is unhealthy, reducing unnecessary load. | Low | Increase RequeueAfter on consecutive failures, reset on success. Cap at e.g., 5 minutes. |

## Anti-Features

Features to explicitly NOT build. These are tempting but wrong for this milestone.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| Webhook/event-driven adapter discovery | No webhook system exists in DittoFS. Building one adds significant complexity to the server for marginal latency improvement over 30s polling. | Polling via `RequeueAfter` is sufficient. 30s default covers all practical use cases. Webhooks can be a future milestone if polling proves inadequate. |
| Operator-initiated adapter creation/deletion | The operator should be a consumer of adapter state, not a producer. Adapter lifecycle is owned by the control plane API (via `dittofsctl` or REST calls). | Operator only reads `GET /api/v1/adapters`. Never calls `POST`/`PUT`/`DELETE` on adapters. |
| Ingress resources for adapters | NFS and SMB are raw TCP protocols, not HTTP. Ingress controllers do not support arbitrary TCP. | Use LoadBalancer or NodePort Services. For TCP ingress, users can configure their own TCP proxy (e.g., Nginx TCP stream, MetalLB). |
| Multi-replica adapter awareness | DittoFS is single-replica (0 or 1). Building multi-replica adapter routing adds complexity for a use case that does not exist yet. | Enforce `spec.replicas` max=1 as currently done. Revisit if HA is added. |
| Per-adapter resource limits | Different resource limits per adapter adds CRD complexity for minimal benefit. Adapters share a process. | Single container resource limits apply to all adapters. If needed, use DittoFS server-side rate limiting per adapter. |
| Adapter config management via CRD | The whole point of this milestone is making the API the single source of truth. Adding adapter config back to the CRD defeats the purpose. | Adapter configuration is exclusively managed via `dittofsctl` or REST API. Operator only observes. |
| Automatic Percona/database migration | Database migration when removing static adapter config from the control plane store is out of scope. | DittoFS `EnsureDefaultAdapters()` already handles first-boot defaults. No migration needed for adapter configs themselves. |
| Service mesh integration (Istio, Linkerd) | Raw TCP protocols do not benefit from HTTP-level service mesh features. Adds complexity without clear value. | Document that NFS/SMB traffic should bypass service mesh sidecars via annotations if a mesh is present. |

## Feature Dependencies

```
"operator" role in DittoFS ------> Auto-create service account
                                        |
                                        v
                              Poll adapter API
                                        |
                                        v
                        +---------------+---------------+
                        |               |               |
                        v               v               v
                   Create/Delete   Update container  NetworkPolicy
                   LB Services     ports on STS      management
                        |               |
                        v               v
                   Adapter status   Headless service
                   in CRD status    port updates
                        |
                        v
                   AdaptersReady
                   condition

Remove static CRD fields -----------> Remove adapter YAML config
    (independent track, can be done    (depends on CRD removal to
     in parallel with polling)          avoid config/CRD conflict)
```

Key dependency chains:

1. **Role -> Account -> Polling -> Services**: The operator cannot poll until it has credentials, and it cannot get credentials until DittoFS supports the operator role.
2. **Polling -> Services + Ports + NetworkPolicy**: All resource management depends on having adapter state from the API.
3. **CRD removal -> Config removal**: Static config should only be removed after CRD fields are removed, to prevent a state where neither source defines adapters.
4. **Services -> Status**: Adapter status in CRD depends on Services being managed first (you report what you manage).

## MVP Recommendation

Prioritize (Phase 1):

1. **New "operator" role in DittoFS** -- gate for everything else. Small change to models + middleware.
2. **Auto-create service account** -- operator logs into DittoFS on startup, stores JWT in Secret.
3. **Poll adapter API** -- core reconcile loop with `RequeueAfter`. Configurable interval via CRD.
4. **Create/Delete LoadBalancer Services** -- the primary deliverable. One Service per running adapter.
5. **Update container ports** -- necessary for correctness when adapters change.
6. **Remove static CRD fields** -- removes confusion, makes API authoritative.
7. **Graceful unavailability handling** -- essential for production reliability.
8. **Events for adapter lifecycle** -- trivial to add alongside Service management, high observability value.

Defer to Phase 2:

- **NetworkPolicy management**: Valuable but not blocking. Requires additional RBAC and is a standalone concern.
- **Adapter status in CRD**: Nice-to-have observability, not functionally required.
- **AdaptersReady condition**: Depends on status fields being added.
- **Token refresh**: Default 15m access / 7d refresh tokens mean the operator can re-login on 401 initially. Proper refresh is a polish item.
- **Service annotations passthrough / type configurability**: Can use defaults first, customize later.
- **Remove adapter section from DittoFS YAML config**: Can be done after CRD removal is stable.

## Complexity Budget

| Feature Group | Estimated LOC | Risk Level |
|---------------|---------------|------------|
| DittoFS role + middleware changes | ~100 | Low -- additive, no breaking changes |
| Operator service account bootstrap | ~200 | Med -- needs API availability wait, Secret management |
| Adapter polling reconcile loop | ~150 | Low -- well-understood RequeueAfter pattern |
| Dynamic Service management | ~300 | Med -- Service merging, owner references, cleanup |
| StatefulSet port updates | ~100 | Med -- must handle rolling update carefully |
| CRD field removal + migration | ~200 | Med -- breaking change, needs webhook validation |
| **Total Phase 1** | **~1050** | |

## Sources

- Codebase analysis: DittoFS operator at `k8s/dittofs-operator/`, adapter API at `pkg/apiclient/adapters.go`, runtime at `pkg/controlplane/runtime/runtime.go`
- [Kubernetes Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) -- official K8s operator documentation
- [controller-runtime reconcile package](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile) -- RequeueAfter semantics
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/) -- common recommendations
- [Kubernetes Network Policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/) -- NetworkPolicy spec
- [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/) -- service account patterns
- [Kubernetes Conditions Pattern](https://maelvls.dev/kubernetes-conditions/) -- status condition best practices
- [Kubernetes Owner References](https://oneuptime.com/blog/post/2026-01-30-kubernetes-owner-references/view) -- lifecycle management
- [Kubernetes Garbage Collection](https://kubernetes.io/docs/concepts/architecture/garbage-collection/) -- owner reference cleanup
- [External Resource Management in Operators](https://github.com/operator-framework/operator-sdk/issues/6117) -- polling external APIs pattern
