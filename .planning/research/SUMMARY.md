# Project Research Summary

**Project:** DittoFS Kubernetes Operator
**Domain:** Kubernetes Operator for Stateful Application (NFS/SMB filesystem server)
**Researched:** 2026-02-04
**Confidence:** MEDIUM-HIGH

## Executive Summary

The DittoFS Kubernetes Operator is a well-scoped project in a mature ecosystem. The 2025/2026 Kubernetes operator stack (Operator SDK v1.42+, controller-runtime v0.21+, Go 1.24) provides excellent foundations. The core challenge is managing a stateful TCP application with external operator dependencies (Percona PostgreSQL) and non-HTTP service exposure (NFS port 2049, SMB port 445).

The recommended approach is to use Operator SDK with Kubebuilder scaffolding, implementing separate controllers for each CRD (DittoFS, DittoFSShare, DittoFSBackup). The operator should start at Capability Level 1 (basic install) and progress to Level 2 (seamless upgrades) by beta. Critical success factors include proper handling of the Percona PostgreSQL dependency (wait for ready state before DittoFS starts), ConfigMap checksum patterns for configuration reloads, and robust finalizer cleanup.

Key risks center on three areas: (1) LoadBalancer services for TCP ports may fail on non-cloud or misconfigured clusters, (2) cross-operator coordination with Percona requires explicit readiness gating, and (3) PVC lifecycle management for BadgerDB metadata and filesystem payload needs careful design to avoid stuck terminating states. All three risks have well-documented mitigation patterns.

## Key Findings

### Recommended Stack

The stack is well-established with HIGH confidence. Operator SDK v1.42.0 wraps Kubebuilder and provides OLM integration. Go 1.24 is required. The kube-rbac-proxy is deprecated; use controller-runtime's native authn/authz instead.

**Core technologies:**
- **Operator SDK v1.42.0:** Operator scaffolding, OLM integration - industry standard, required
- **controller-runtime v0.21.0:** Controller logic, reconciliation loops - Kubernetes-SIG maintained
- **Go 1.24:** Language runtime - required by Operator SDK, provides log/slog
- **envtest:** Integration testing - official recommendation for operator testing
- **Kustomize v5.x:** Manifest management - built into kubectl, Operator SDK default

**What NOT to use:**
- kube-rbac-proxy (discontinued March 2025)
- gcr.io/kubebuilder images (GCR went away)
- Logrus (maintenance mode, use Zap via controller-runtime)

### Expected Features

The feature landscape is clear from database operator patterns (Percona, CloudNativePG, KubeDB).

**Must have (table stakes):**
- Declarative CRD API with `kubectl apply -f dittofs.yaml` UX
- StatefulSet with PVC management for persistent storage
- LoadBalancer Services for NFS (2049), SMB (445), API (8080)
- ConfigMap/Secret management for configuration and credentials
- Health probes (liveness, readiness)
- Status conditions (Ready, Available, Degraded)
- Finalizers for clean resource cleanup
- Basic RBAC (ServiceAccount, Role, RoleBinding)

**Should have (differentiators):**
- PostgreSQL integration via Percona operator (managed metadata store)
- Validation webhooks for early config rejection
- Prometheus ServiceMonitor integration
- Rolling update strategy for seamless upgrades
- Multi-backend configuration (memory, filesystem, S3, BadgerDB)

**Defer (v1.0+):**
- DittoFSBackup CRD for backup/restore workflows
- cert-manager integration for automatic TLS
- Horizontal scaling (multiple DittoFS instances)
- External Secrets Operator integration
- Level 5 autopilot features (auto-scaling, self-healing)

### Architecture Approach

The architecture follows the controller-runtime pattern with multi-controller design (one controller per CRD). The operator manages DittoFS as a StatefulSet with three PVCs (metadata, payload, cache), generates ConfigMaps from CRD spec, and watches external PerconaPGCluster resources for database connection details.

**Major components:**
1. **DittoFS Controller:** Main reconciler - creates ConfigMap, StatefulSet, Services, PVCs; watches PerconaPGCluster for connection details
2. **Share Controller:** Manages DittoFSShare resources - updates ConfigMap with share definitions, triggers pod reload
3. **Backup Controller (future):** Handles backup/restore operations using DittoFS backup CLI
4. **pkg/configgen:** CRD-to-ConfigMap transformer - separates transformation from reconciliation
5. **pkg/resources:** Builder pattern for Kubernetes resources - StatefulSet, Service, PVC builders
6. **pkg/percona:** Encapsulates Percona operator integration - watches PGCluster, extracts connection secrets

### Critical Pitfalls

The top 5 pitfalls from research, with prevention strategies:

1. **LoadBalancer pending for NFS/SMB ports** - Detect cloud provider at startup; provide NodePort fallback; add `ServiceExternalIPPending` status condition; document MetalLB for bare-metal
2. **Percona PostgreSQL not ready before DittoFS starts** - Use init containers; check PGCluster status conditions; implement exponential backoff; add `PostgreSQLReady` status condition
3. **ConfigMap changes don't trigger pod restart** - Implement checksum annotation pattern from day one: `configmap-hash: sha256(data)` in pod template
4. **PVC stuck terminating** - Validate StorageClass exists before creation; delete pods before PVCs; handle finalizers correctly; add `PVCBound` status condition
5. **Infinite reconciliation loop from status updates** - Use `UpdateStatus()` not `Update()`; only update when changed; use `meta.SetStatusCondition` for deduplication

## Implications for Roadmap

Based on research, suggested phase structure:

### Phase 1: Operator Foundation
**Rationale:** CRD and controller are prerequisites for everything else. Cannot test any functionality without basic reconciliation working.
**Delivers:** Functional operator skeleton with DittoFS CRD that creates a StatefulSet (hardcoded config, no Percona)
**Addresses:** Declarative CRD API, basic RBAC, namespace isolation
**Avoids:** Owner reference issues by defining hierarchy upfront; CRD version strategy from start (v1alpha1)

### Phase 2: ConfigMap Generation and Services
**Rationale:** Configuration is the interface between CRD and DittoFS application. Services are required before any external testing.
**Delivers:** ConfigMap generated from CRD spec; LoadBalancer Services for NFS, SMB, API; checksum annotation for pod restart
**Uses:** Kustomize for manifest management; builder pattern for resources
**Implements:** pkg/configgen transformer; pkg/resources builders
**Avoids:** ConfigMap reload pitfall via checksum pattern; LoadBalancer issues via status conditions and NodePort fallback

### Phase 3: Storage Management (PVCs)
**Rationale:** PVCs must be created after StatefulSet pattern is established. Stateful data is core to DittoFS value proposition.
**Delivers:** VolumeClaimTemplates for metadata, payload, cache PVCs; StorageClass validation
**Addresses:** PVC management, resource requests/limits
**Avoids:** PVC stuck pending via StorageClass validation; stuck terminating via proper deletion order

### Phase 4: Percona PostgreSQL Integration
**Rationale:** PostgreSQL integration is the most complex external dependency. Must be solved before production use.
**Delivers:** PerconaPGCluster watching; connection Secret extraction; readiness gating (DittoFS waits for PostgreSQL)
**Uses:** Percona PG operator types as Go dependency; `Watches()` with predicates
**Implements:** pkg/percona client and connection builder
**Avoids:** Dependency not ready pitfall via init containers and status condition checks

### Phase 5: Status Conditions and Lifecycle
**Rationale:** Production readiness requires clear status reporting and clean cleanup.
**Delivers:** Full status conditions (Ready, Available, DatabaseReady, ConfigReady); finalizers for cleanup; events for debugging
**Addresses:** Status conditions, graceful shutdown, health probes
**Avoids:** Infinite reconciliation via proper status subresource usage; finalizer stuck via idempotent cleanup

### Phase 6: Share Controller (Optional)
**Rationale:** Multi-share support is a differentiator but requires core operator to be stable first.
**Delivers:** DittoFSShare CRD and controller; cross-controller coordination; ConfigMap regeneration
**Addresses:** Multi-share support, hot reload or pod restart strategy

### Phase Ordering Rationale

- **Foundation first:** Phases 1-3 establish the core operator pattern before adding complexity
- **External dependency isolated:** Phase 4 (Percona) is deferred until basic operator works; allows testing without PostgreSQL
- **Status/lifecycle last:** Phase 5 polishes production readiness after core features work
- **Optional features separate:** Phase 6 can be skipped for MVP if timeline is tight
- **Pitfall prevention built-in:** Each phase explicitly addresses relevant pitfalls from research

### Research Flags

Phases likely needing deeper research during planning:
- **Phase 4 (Percona Integration):** Complex cross-operator coordination; Percona API may change; verify current CRD schema
- **Phase 6 (Share Controller):** Cross-controller coordination patterns; ConfigMap reload vs pod restart trade-offs

Phases with standard patterns (skip research-phase):
- **Phase 1-2 (Foundation, ConfigMap):** Well-documented Kubebuilder/Operator SDK patterns
- **Phase 3 (PVCs):** Standard StatefulSet volumeClaimTemplates pattern
- **Phase 5 (Status/Lifecycle):** Standard controller-runtime patterns

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | Official Operator SDK and controller-runtime documentation; clear version requirements |
| Features | MEDIUM-HIGH | Based on mature database operator patterns; some DittoFS-specific features unvalidated |
| Architecture | MEDIUM | Standard patterns well-documented; cross-operator coordination needs validation |
| Pitfalls | MEDIUM | Multiple sources agree on common issues; Scaleway-specific may need validation |

**Overall confidence:** MEDIUM-HIGH

### Gaps to Address

- **Percona PG Operator API version:** Research used general patterns; need to verify current Percona CRD schema during Phase 4 implementation
- **Scaleway LoadBalancer behavior:** Documented patterns may need validation on actual Scaleway Kubernetes cluster
- **DittoFS config hot-reload:** Current DittoFS may not support config reload without restart; investigate API endpoint
- **OLM v1 maturity:** Research indicates OLM v1 is available but production readiness should be confirmed before targeting OperatorHub

## Sources

### Primary (HIGH confidence)
- [Operator SDK Official Documentation](https://sdk.operatorframework.io/) - Tutorial, best practices, advanced topics
- [Kubebuilder Book](https://book.kubebuilder.io/) - controller-gen CLI, RBAC markers, CRD generation
- [Kubernetes Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Operator Framework Capability Levels](https://sdk.operatorframework.io/docs/overview/operator-capabilities/)
- [Kubernetes Finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)

### Secondary (MEDIUM confidence)
- [Percona Operator for PostgreSQL](https://docs.percona.com/percona-operator-for-postgresql/index.html) - Integration patterns
- [CloudNativePG Documentation](https://cloudnative-pg.io/documentation/) - Alternative PostgreSQL operator patterns
- [Google Cloud - Best practices for building Kubernetes Operators](https://cloud.google.com/blog/products/containers-kubernetes/best-practices-for-building-kubernetes-operators)
- [Red Hat - Kubernetes Operators Best Practices](https://www.redhat.com/en/blog/kubernetes-operators-best-practices)

### Tertiary (LOW confidence)
- [Scaleway LoadBalancer Troubleshooting](https://www.scaleway.com/en/docs/load-balancer/troubleshooting/k8s-errors/) - Scaleway-specific, needs validation
- Specific Percona API versions - verify against current Percona releases during implementation

---
*Research completed: 2026-02-04*
*Ready for roadmap: yes*
