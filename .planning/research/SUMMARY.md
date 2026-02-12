# Research Summary: DittoFS K8s Auto-Adapters (Stack Dimension)

**Domain:** K8s operator dynamic adapter-driven service management
**Researched:** 2026-02-09
**Overall confidence:** HIGH

## Executive Summary

The DittoFS K8s operator needs to dynamically manage Kubernetes Services, NetworkPolicies, and container ports based on the runtime state of DittoFS protocol adapters (NFS, SMB). This research focused on the stack dimension: what technologies and patterns to use for this capability.

The core finding is that **no new external dependencies are needed**. The existing operator stack (Go 1.25.1, controller-runtime v0.22.4, k8s.io/api v0.34.1) provides everything required. The `networking.k8s.io/v1` NetworkPolicy API is already available as a sub-package of `k8s.io/api`. The `source.Channel` mechanism in controller-runtime is purpose-built for triggering reconciliation from external events (like polling a REST API). The existing `pkg/apiclient` library already implements the adapter listing and JWT authentication APIs the operator needs.

The key architectural decision is how to integrate external API polling into the controller-runtime reconciliation model. Two approaches exist: (1) `RequeueAfter` on a separate named controller, and (2) `source.Channel` with a background polling goroutine. Both are viable; the previous research round recommended approach (1), while this research recommends approach (2) as the more idiomatic controller-runtime pattern for external events. The choice between them is a design decision, not a blocking concern -- both work with the same stack.

For resource mutation (creating/deleting Services and NetworkPolicies), the recommendation is to continue using `controllerutil.CreateOrUpdate` rather than adopting Server-Side Apply. The dynamic resources are single-owner (only the operator manages them), so SSA's multi-controller field ownership benefits do not apply. The existing codebase already has robust patterns for `CreateOrUpdate` including retry-on-conflict, service spec merging, and port preservation.

## Key Findings

**Stack:** No new external dependencies. Use controller-runtime `source.Channel` for polling, `networking.k8s.io/v1` for NetworkPolicy, `pkg/apiclient` for DittoFS API communication. Stay on controller-runtime v0.22.4.

**Architecture:** Two polling approaches are viable (RequeueAfter vs source.Channel). Both require a clear separation between infrastructure reconciliation (CRD-driven) and adapter reconciliation (API-driven).

**Critical pitfall:** The chicken-and-egg problem -- the operator cannot poll the DittoFS API until the pod is running, but the pod requires infrastructure resources created by the operator. Two-phase reconciliation (infrastructure first, then adapter discovery) is mandatory.

## Implications for Roadmap

Based on research, suggested phase structure:

1. **DittoFS Role and Auth Foundation** - Create the "operator" role in DittoFS, implement operator authentication via K8s Secret
   - Addresses: operator authentication, new DittoFS role
   - Avoids: auth bootstrap pitfall by reusing existing admin credential Secrets

2. **Polling Infrastructure** - Implement the polling goroutine/channel (or RequeueAfter controller), API client factory with token refresh
   - Addresses: adapter state discovery, graceful unavailability handling
   - Avoids: chicken-and-egg pitfall via readiness gate

3. **Dynamic Service Management** - Create/delete per-adapter LoadBalancer Services, update headless service ports
   - Addresses: primary deliverable, owner references, service lifecycle
   - Avoids: orphaned LB pitfall via owner references + label-based GC

4. **StatefulSet Port and CRD Cleanup** - Update container ports, remove static adapter fields from CRD
   - Addresses: pod spec correctness, CRD simplification
   - Avoids: rolling restart storm by making ports informational-only or batching

5. **NetworkPolicy Management** - Per-adapter NetworkPolicy creation/deletion
   - Addresses: defense-in-depth security
   - Avoids: traffic blocking pitfall via proper operation ordering

6. **Status and Observability** - Adapter status in CRD, AdaptersReady condition, K8s events
   - Addresses: kubectl observability, condition aggregation

**Phase ordering rationale:**
- Auth must come first because polling depends on it
- Polling must come before Service management (no state = no Services to manage)
- Service management before NetworkPolicy (policies protect Services that must exist first)
- CRD cleanup can partially parallel with polling/Service work but should land after polling proves stable
- Status is last because it depends on all other pieces being in place

**Research flags for phases:**
- Phase 1 (Auth): May need deeper research on auto-creating the operator service account vs manual provisioning
- Phase 2 (Polling): Standard patterns; the source.Channel vs RequeueAfter decision should be finalized early
- Phase 3 (Services): Standard patterns, unlikely to need additional research
- Phase 5 (NetworkPolicy): May need research into CNI compatibility and opt-in configuration

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | All versions verified from go.mod; no new dependencies; APIs confirmed via official docs |
| Features | HIGH | Based on deep codebase analysis; requirements clearly specified in PROJECT.md |
| Architecture | HIGH | Two viable approaches identified; both well-documented in controller-runtime |
| Pitfalls | HIGH | Identified from codebase structure and known K8s patterns; chicken-and-egg confirmed |

## Gaps to Address

- **Module import path for pkg/apiclient**: The operator is a separate Go module. Importing `pkg/apiclient` from the parent DittoFS module may require a `replace` directive or Go workspace. This needs validation during implementation.
- **Adapter API `running` field semantics**: The `Adapter` struct in `pkg/apiclient/adapters.go` does not include a `running` field. The `pkg/controlplane/models/adapter.go` only has `Enabled`. PROJECT.md states the API "already returns port, enabled, running" -- this needs verification of whether `running` is computed at the handler level or needs to be added.
- **source.Channel vs RequeueAfter**: Both approaches work. The previous research recommended RequeueAfter on a separate named controller; this research recommends source.Channel. The roadmap should make a decision early and stick with it. Neither requires different dependencies.

## Sources

- [controller-runtime releases](https://github.com/kubernetes-sigs/controller-runtime/releases)
- [controller-runtime source.Channel docs](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/source)
- [k8s.io/api networking/v1](https://pkg.go.dev/k8s.io/api/networking/v1)
- [Kubernetes NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [Kubernetes Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/)
- [External Resource Management in Operators](https://github.com/operator-framework/operator-sdk/issues/6117)
- [Self-Healing Controllers for External State](https://anynines.com/blog/external-state-drift-kubernetes-controller-self-healing-design/)
- [Kubernetes Controllers at Scale](https://medium.com/@timebertt/kubernetes-controllers-at-scale-clients-caches-conflicts-patches-explained-aa0f7a8b4332)
