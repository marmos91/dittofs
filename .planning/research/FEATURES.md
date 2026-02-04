# Feature Research: Kubernetes Operators for Stateful Applications

**Domain:** Kubernetes Operators for Stateful Applications (Databases, File Systems)
**Researched:** 2026-02-04
**Confidence:** MEDIUM-HIGH
**Focus:** DittoFS Kubernetes Operator (NFS/SMB virtual filesystem with PostgreSQL backend)

## Executive Summary

This research identifies the feature landscape for Kubernetes operators managing stateful applications, specifically focusing on what's needed for a production-ready DittoFS operator. The operator ecosystem has matured significantly, with clear patterns emerging from database operators (Percona, CloudNativePG, KubeDB) and storage operators (Rook, NFS operators).

Key insight: Most production operators are at capability Level 2-3 (seamless upgrades, backup/restore). Only ~5% reach Level 5 (autopilot). For DittoFS, targeting Level 3 initially with a path to Level 4 is realistic and valuable.

---

## Feature Landscape

### Table Stakes (Users Expect These)

Features users assume exist. Missing these = product feels incomplete or unusable in production.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Declarative CRD API** | Kubernetes-native UX; users expect `kubectl apply -f dittofs.yaml` | MEDIUM | Use kubebuilder/operator-sdk; follow API conventions |
| **Basic Install/Uninstall** | Level 1 capability; must deploy DittoFS reliably | MEDIUM | StatefulSet for DittoFS pods, proper RBAC |
| **PVC Management** | Stateful apps need persistent storage | MEDIUM | Dynamic provisioning, StorageClass integration |
| **Service Exposure (LoadBalancer)** | NFS/SMB must be reachable outside cluster | LOW | TCP LoadBalancer for ports 2049 (NFS), 445 (SMB), 8080 (API) |
| **ConfigMap/Secret Management** | Config must be Kubernetes-native | LOW | Mount config as ConfigMap, credentials as Secret |
| **Health Probes** | Kubernetes needs to know pod health | LOW | Liveness, readiness probes for DittoFS process |
| **Resource Requests/Limits** | Required for proper scheduling | LOW | CPU/memory limits in pod spec |
| **Status Conditions** | Users need to know deployment state | MEDIUM | Ready, Available, Degraded conditions on CRD status |
| **Basic Logging** | Debugging requires logs | LOW | stdout/stderr, structured JSON recommended |
| **Graceful Shutdown** | Data integrity on pod termination | MEDIUM | PreStop hooks, SIGTERM handling (DittoFS already has this) |
| **Namespace Isolation** | Multi-tenant environments | LOW | Namespaced CRDs, proper RBAC |
| **Seamless Upgrades** | Level 2 capability; rolling updates | HIGH | StatefulSet update strategy, version compatibility |
| **Basic Documentation** | Users need to know how to use it | LOW | README, CRD examples, troubleshooting guide |

### Differentiators (Competitive Advantage)

Features that set the product apart. Not required for MVP, but valuable for adoption.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Backup/Restore (Level 3)** | Data protection is critical for stateful apps | HIGH | Integrate with Velero, or custom backup CRD; includes PostgreSQL backup |
| **PostgreSQL Operator Integration** | Managed PostgreSQL via Percona operator | HIGH | Orchestrate Percona PG operator for metadata store; handle dependency |
| **Multi-Backend Storage Configuration** | Flexible storage (memory, filesystem, S3) | MEDIUM | CRD spec for metadata/content store selection |
| **Prometheus Metrics Integration** | Production observability | MEDIUM | ServiceMonitor CRD, DittoFS already exposes /metrics |
| **Automatic TLS/cert-manager** | Security without manual cert management | MEDIUM | Integrate with cert-manager for NFS/SMB/API TLS |
| **Horizontal Scaling (Read Replicas)** | Scale read capacity | HIGH | Multiple DittoFS instances sharing PostgreSQL; stateful complexity |
| **External Secrets Operator Integration** | Enterprise secret management | MEDIUM | Support ESO for credentials (S3, PostgreSQL) |
| **Validation Webhooks** | Prevent invalid configurations | MEDIUM | Reject malformed CRs before they reach etcd |
| **Conversion Webhooks** | API version migration | HIGH | Hub-spoke model for v1alpha1 -> v1beta1 -> v1 |
| **Deep Insights (Level 4)** | Metrics-driven alerts and dashboards | MEDIUM | PrometheusRule CRDs, Grafana dashboards |
| **GitOps-Friendly Design** | Declarative, drift-free management | LOW | Immutable spec, status separation, no imperative APIs |
| **Multi-Share Support** | Multiple NFS exports from one instance | MEDIUM | Array of share configs in CRD |
| **Cross-Namespace PVC Binding** | Share storage across namespaces | HIGH | Requires careful RBAC, often an anti-pattern |
| **Automatic Volume Expansion** | Grow storage without downtime | MEDIUM | Monitor PVC usage, trigger resize |
| **Pod Disruption Budgets** | Maintain availability during maintenance | LOW | PDB resource creation |
| **Network Policies** | Security isolation | LOW | NetworkPolicy resources |
| **Autopilot Features (Level 5)** | Self-healing, auto-scaling, auto-tuning | VERY HIGH | Requires deep operational knowledge in code |

### Anti-Features (Commonly Requested, Often Problematic)

Features that seem good but create problems. Explicitly NOT building these.

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| **Imperative APIs** | "Just run this command to backup" | Breaks GitOps, creates drift, hard to audit | Declarative backup CRD with schedule |
| **Embedded Database** | "Simpler, no external dependency" | Single point of failure, no HA, upgrade complexity | Delegate to Percona PG operator |
| **Auto-Magic Configuration** | "Just figure out what I need" | Unpredictable behavior, hard to debug | Explicit configuration with good defaults |
| **Cluster-Admin RBAC** | "Just make it work everywhere" | Security disaster, principle of least privilege violated | Minimal RBAC with explicit permissions |
| **Force Delete Operations** | "I need to delete this stuck resource" | Leaves orphaned external resources, data loss risk | Proper finalizer handling with timeout |
| **Real-Time Everything** | "Stream all changes instantly" | Complexity explosion, performance issues | Eventual consistency with polling intervals |
| **Single Giant CRD** | "One resource to rule them all" | Hard to version, validate, understand | Separate CRDs: DittoFS, DittoFSBackup, DittoFSShare |
| **In-Cluster NFS Client Mounts** | "Mount NFS inside other pods automatically" | CSI driver complexity, circular dependencies | Document manual mount; consider CSI later |
| **Automatic Schema Migrations** | "Just upgrade PostgreSQL schema" | Data loss risk, rollback complexity | Explicit migration CRD with approval |
| **Multi-Cluster Replication** | "Sync across clusters" | Massive complexity, CAP theorem | Single-cluster focus; document external replication |

---

## Feature Dependencies

```
[CRD API Definition]
    |
    +---> [Validation Webhook] (optional, enhances)
    |
    +---> [Controller Reconciliation Loop]
              |
              +---> [StatefulSet Management]
              |         |
              |         +---> [PVC Creation]
              |         |
              |         +---> [Service Creation (LoadBalancer)]
              |         |
              |         +---> [ConfigMap/Secret Mounting]
              |
              +---> [Status Condition Updates]
              |
              +---> [Finalizers for Cleanup]

[PostgreSQL Integration]
    |
    +---> [Percona Operator Dependency]
              |
              +---> [PostgreSQL Cluster CRD Creation]
              |
              +---> [Connection Secret Handling]
              |
              +---> [Backup Coordination]

[Observability Stack]
    |
    +---> [ServiceMonitor Creation] --requires--> [Prometheus Operator]
    |
    +---> [PrometheusRule Creation] --requires--> [Prometheus Operator]

[Security Features]
    |
    +---> [TLS Configuration] --enhances--> [cert-manager Integration]
    |
    +---> [Secret Management] --enhances--> [External Secrets Operator]

[Backup/Restore]
    |
    +---> [DittoFSBackup CRD] --requires--> [DittoFS Instance Running]
    |
    +---> [Velero Integration] --optional--> [Volume Snapshots]
```

### Dependency Notes

- **CRD API requires Controller:** The CRD is useless without a controller watching it
- **PostgreSQL integration requires Percona Operator:** We depend on Percona for PostgreSQL lifecycle
- **ServiceMonitor requires Prometheus Operator:** Metrics integration only works if Prometheus Operator is installed
- **Backup requires running instance:** Cannot backup what doesn't exist
- **cert-manager integration is optional:** Manual TLS still works, just more effort
- **Validation webhooks enhance CRD:** Not required, but prevents bad configs

---

## MVP Definition

### Launch With (v0.1.0 - Alpha)

Minimum viable operator to validate the concept works on Kubernetes.

- [x] **DittoFS CRD** - Single CRD defining a DittoFS instance
- [x] **Basic Controller** - Reconciles CRD to StatefulSet + Services
- [x] **PVC Management** - Creates PVC for persistent storage
- [x] **LoadBalancer Services** - Exposes NFS (2049), SMB (445), API (8080)
- [x] **ConfigMap for Configuration** - Mounts dittofs config.yaml
- [x] **Secret for Credentials** - S3 credentials, initial admin password
- [x] **Health Probes** - Liveness and readiness for DittoFS
- [x] **Status Conditions** - Ready/NotReady status on CRD
- [x] **Basic RBAC** - ServiceAccount, Role, RoleBinding
- [x] **Finalizers** - Clean up PVCs and services on deletion

**Why these are essential:**
- Without CRD+Controller, there's no operator
- Without PVC, data is lost on restart
- Without Services, NFS/SMB are unreachable
- Without health probes, Kubernetes can't manage pod lifecycle
- Without status, users can't tell if deployment succeeded

### Add After Validation (v0.2.0 - Beta)

Features to add once core deployment works reliably.

- [ ] **PostgreSQL Integration** - Orchestrate Percona PG operator (trigger: users need persistent metadata)
- [ ] **Validation Webhook** - Reject invalid configurations (trigger: users submit broken configs)
- [ ] **Prometheus ServiceMonitor** - Metrics integration (trigger: users want observability)
- [ ] **Rolling Update Strategy** - Seamless upgrades (trigger: users need to upgrade)
- [ ] **Multi-Backend Config** - Support S3, filesystem, memory backends (trigger: users need flexibility)
- [ ] **TLS Configuration** - Enable TLS for API server (trigger: security requirements)

### Future Consideration (v1.0.0+)

Features to defer until product-market fit is established.

- [ ] **DittoFSBackup CRD** - Separate CRD for backup/restore (defer: complex, needs design)
- [ ] **cert-manager Integration** - Automatic TLS certificates (defer: optional enhancement)
- [ ] **External Secrets Integration** - Enterprise secret management (defer: niche requirement)
- [ ] **Horizontal Scaling** - Multiple DittoFS instances (defer: requires architectural changes)
- [ ] **Conversion Webhooks** - API version migration (defer: only needed after API stabilization)
- [ ] **Level 5 Autopilot** - Auto-scaling, self-healing (defer: requires extensive operational data)
- [ ] **Multi-Share Support** - Multiple NFS exports per instance (defer: complexity)

---

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost | Priority | Phase |
|---------|------------|---------------------|----------|-------|
| DittoFS CRD + Controller | HIGH | HIGH | P1 | MVP |
| PVC Management | HIGH | LOW | P1 | MVP |
| LoadBalancer Services | HIGH | LOW | P1 | MVP |
| ConfigMap/Secret | HIGH | LOW | P1 | MVP |
| Health Probes | HIGH | LOW | P1 | MVP |
| Status Conditions | MEDIUM | MEDIUM | P1 | MVP |
| RBAC | HIGH | LOW | P1 | MVP |
| Finalizers | MEDIUM | MEDIUM | P1 | MVP |
| PostgreSQL Integration | HIGH | HIGH | P2 | Beta |
| Validation Webhook | MEDIUM | MEDIUM | P2 | Beta |
| ServiceMonitor | MEDIUM | LOW | P2 | Beta |
| Rolling Updates | HIGH | HIGH | P2 | Beta |
| Multi-Backend Config | MEDIUM | MEDIUM | P2 | Beta |
| TLS Configuration | MEDIUM | MEDIUM | P2 | Beta |
| Backup CRD | HIGH | HIGH | P3 | v1.0 |
| cert-manager Integration | LOW | MEDIUM | P3 | v1.0 |
| Horizontal Scaling | MEDIUM | VERY HIGH | P3 | v1.0+ |
| Level 5 Autopilot | LOW | VERY HIGH | P3 | v2.0+ |

**Priority key:**
- P1: Must have for MVP launch
- P2: Should have for beta, required for production readiness
- P3: Nice to have, future consideration

---

## Competitor Feature Analysis

| Feature | Percona PG Operator | CloudNativePG | Rook NFS | DittoFS Operator (Planned) |
|---------|---------------------|---------------|----------|----------------------------|
| Declarative CRD | Yes | Yes | Yes | Yes (MVP) |
| HA/Replication | Yes (sync/async) | Yes (streaming) | N/A | No (defer to v2) |
| Backup/Restore | Yes (pgBackRest) | Yes (Barman) | Via Velero | Planned (v1.0) |
| Monitoring | Yes (ServiceMonitor) | Yes (built-in) | Yes | Planned (Beta) |
| TLS | Yes (cert-manager) | Yes (auto) | No | Planned (Beta) |
| Auto-Failover | Yes | Yes | N/A | No (single instance) |
| Volume Expansion | Yes | Yes | Yes | Planned (v1.0) |
| Upgrade Strategy | Rolling | Rolling | Rolling | Planned (Beta) |
| Multi-Version CRD | Yes | Yes | Yes | Planned (v1.0) |
| External Secrets | Via ESO | Via ESO | Via ESO | Planned (v1.0) |
| Unique Value | Full PostgreSQL lifecycle | Lightweight, K8s native | NFS provisioning | NFS/SMB from S3/cloud storage |

### DittoFS Unique Differentiators

1. **Protocol Flexibility:** Both NFS and SMB from single deployment (competitors are single-protocol)
2. **Backend Abstraction:** Storage can be S3, filesystem, or memory (not just local PVCs)
3. **Metadata/Content Separation:** Different backends for metadata vs content
4. **Lightweight:** No complex distributed consensus (unlike Ceph, GlusterFS)

---

## Operator Capability Level Roadmap

Based on the [Operator Framework Capability Levels](https://sdk.operatorframework.io/docs/overview/operator-capabilities/):

| Level | Name | Status | Target Version |
|-------|------|--------|----------------|
| 1 | Basic Install | Target | v0.1.0 (MVP) |
| 2 | Seamless Upgrades | Target | v0.2.0 (Beta) |
| 3 | Full Lifecycle (Backup/Restore) | Target | v1.0.0 |
| 4 | Deep Insights | Stretch | v1.x |
| 5 | Auto Pilot | Future | v2.0+ |

**Level 1 Requirements (MVP):**
- Operator deploys DittoFS via `kubectl apply`
- Basic configuration via CRD spec
- Clean uninstall via finalizers

**Level 2 Requirements (Beta):**
- Rolling updates without downtime
- Version upgrades handled gracefully
- Configuration changes reconciled

**Level 3 Requirements (v1.0):**
- Backup and restore via dedicated CRD
- Point-in-time recovery (if PostgreSQL backend)
- Scheduled backups

**Level 4 Requirements (v1.x):**
- Metrics-based alerts
- Grafana dashboards
- Capacity planning recommendations

**Level 5 Requirements (v2.0+):**
- Auto-scaling based on metrics
- Self-healing from detected anomalies
- Auto-tuning configuration

---

## Sources

### Primary Sources (HIGH Confidence)
- [Operator Framework Capability Levels](https://sdk.operatorframework.io/docs/overview/operator-capabilities/)
- [Kubernetes Finalizers Documentation](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- [Kubernetes Status Conditions](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-conditions)
- [Percona Operator for PostgreSQL](https://docs.percona.com/percona-operator-for-postgresql/index.html)
- [CloudNativePG Documentation](https://cloudnative-pg.io/documentation/)

### Secondary Sources (MEDIUM Confidence)
- [Google Cloud: Best practices for building Kubernetes Operators](https://cloud.google.com/blog/products/containers-kubernetes/best-practices-for-building-kubernetes-operators-and-stateful-apps)
- [Red Hat: Kubernetes Operators Best Practices](https://www.redhat.com/en/blog/kubernetes-operators-best-practices)
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/)
- [Kubebuilder Book](https://book.kubebuilder.io/)
- [Prometheus Operator](https://prometheus-operator.dev/)

### Community Sources (LOW-MEDIUM Confidence)
- [Slack Engineering: Stateful Rollouts](https://slack.engineering/kube-stateful-rollouts/)
- [Martin Heinz: Stop Messing with Kubernetes Finalizers](https://martinheinz.dev/blog/74)
- [Codefresh: Kubernetes Anti-Patterns](https://codefresh.io/blog/kubernetes-antipatterns-1/)
- [Semaphore: Managing Stateful Applications on Kubernetes](https://semaphore.io/blog/stateful-applications-kubernetes)

---

*Feature research for: DittoFS Kubernetes Operator*
*Researched: 2026-02-04*
