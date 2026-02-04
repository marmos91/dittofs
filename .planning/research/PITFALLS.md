# Pitfalls Research: DittoFS Kubernetes Operator

**Domain:** Kubernetes Operator for Stateful TCP Services (NFS/SMB)
**Researched:** 2026-02-04
**Confidence:** MEDIUM (verified with multiple sources, some domain-specific patterns from experience)

---

## Critical Pitfalls

### Pitfall 1: LoadBalancer Service for NFS/SMB Ports Fails on Non-Cloud or Misconfigured Clusters

**What goes wrong:**
LoadBalancer service for NFS (port 2049) or SMB (port 445) gets stuck with EXTERNAL-IP "pending" forever. Users cannot mount the filesystem from outside the cluster.

**Why it happens:**
- LoadBalancer services require cloud provider integration or MetalLB for bare-metal
- Scaleway requires all LB modifications through Kubernetes annotations, never through console
- NFS/SMB use non-standard ports; Ingress only handles HTTP/HTTPS (80/443)
- Some managed Kubernetes providers don't provision LBs for non-HTTP ports correctly

**How to avoid:**
1. Detect cloud provider at operator startup and validate LB support
2. Document MetalLB requirement for bare-metal/self-hosted Kubernetes
3. Use Scaleway-specific annotations: `service.beta.kubernetes.io/scw-loadbalancer-use-hostname: "true"` for internal cluster connectivity
4. Provide NodePort fallback with clear documentation for when LoadBalancer fails
5. Add operator status condition `ServiceExternalIPPending` with actionable error message

**Warning signs:**
- Service stuck in "pending" for EXTERNAL-IP longer than 5 minutes
- Events showing "no load balancer available for service"
- Scaleway: health check failures if `externalTrafficPolicy: Local` instead of `Cluster`

**Phase to address:**
Phase 2 (Service Exposure) - Must be solved before NFS/SMB can be accessed externally.

---

### Pitfall 2: Percona PostgreSQL Operator Dependency Not Ready Before DittoFS Starts

**What goes wrong:**
DittoFS operator creates DittoFS CR, which references PostgreSQL cluster. PostgreSQL isn't ready yet, so DittoFS pod crashes with connection refused or enters CrashLoopBackOff.

**Why it happens:**
- Operators manage their own lifecycle independently
- No built-in Kubernetes mechanism for cross-operator coordination
- "Operator sprawl" - each operator manages its domain without awareness of others
- DittoFS expects PostgreSQL to be immediately available

**How to avoid:**
1. Use init containers to wait for PostgreSQL to be ready before main container starts
2. Check for PostgreSQL CR status conditions (`Ready: True`) in DittoFS reconciliation loop
3. Implement exponential backoff when PostgreSQL isn't ready (don't just error and requeue immediately)
4. Add DittoFS status condition `PostgreSQLReady: False` with clear message
5. Consider OLM (Operator Lifecycle Manager) for dependency declaration if targeting OpenShift

**Warning signs:**
- DittoFS pods in CrashLoopBackOff with connection errors
- Rapid reconciliation requeues (infinite loop pattern)
- Percona PostgreSQL CR shows `Initializing` or similar non-ready state

**Phase to address:**
Phase 1 (CRD Design) - Define dependency relationships. Phase 3 (PostgreSQL Integration) - Implement waiting/retry logic.

---

### Pitfall 3: ConfigMap/Secret Changes Don't Trigger Pod Restart

**What goes wrong:**
User updates ConfigMap or Secret via CRD spec. Operator updates the ConfigMap. Pods continue running with old configuration because Kubernetes doesn't automatically restart pods when mounted ConfigMaps/Secrets change.

**Why it happens:**
- Kubernetes does NOT reload ConfigMaps/Secrets automatically into running pods
- Volume-mounted ConfigMaps eventually update (kubelet sync period), but env vars never update
- Application must implement hot-reload or pods must be restarted

**How to avoid:**
1. Use checksum annotation pattern: add `configmap-hash: sha256(configmap-data)` to pod template annotations
2. When ConfigMap content changes, hash changes, triggering rolling restart
3. For Secrets, same pattern: `secret-hash: sha256(secret-data)`
4. Consider Reloader operator as alternative (but adds dependency complexity)
5. Document which config changes require restart vs. hot-reload

**Warning signs:**
- Users report config changes "not taking effect"
- ConfigMap updated timestamp is newer than pod start time
- Application logs show old configuration values

**Phase to address:**
Phase 2 (ConfigMap Generation) - Implement checksum annotation pattern from day one.

---

### Pitfall 4: PVC Stuck in Terminating or Pending State

**What goes wrong:**
PVC gets stuck in "Pending" (no matching PV or StorageClass issue) or "Terminating" (finalizer preventing deletion). Operator cannot create or delete DittoFS instances cleanly.

**Why it happens:**
- Pending: StorageClass not found, no available PV, capacity mismatch, access mode mismatch
- Terminating: PVC has finalizer `kubernetes.io/pvc-protection` and pod still mounts it
- Terminating: Volume still attached to node (FailedAttachVolume)
- Scaleway: PVC fails to attach due to node pool configuration errors

**How to avoid:**
1. Validate StorageClass exists before creating PVC
2. Set reasonable defaults matching Scaleway's storage offerings
3. On deletion: delete pods first, wait for confirmation, then delete PVC
4. Use owner references correctly so garbage collection works (see Pitfall 6)
5. Add status condition `PVCBound: False` with specific error from events
6. For expansion: verify StorageClass has `allowVolumeExpansion: true`

**Warning signs:**
- PVC status shows "Pending" with no events or "no persistent volumes available"
- PVC stuck in "Terminating" for more than 5 minutes
- Events show "FailedAttachVolume" or "FailedMount"
- Multi-attach errors when using ReadWriteOnce with multiple pods

**Phase to address:**
Phase 3 (Storage Management) - PVC lifecycle must be carefully designed.

---

### Pitfall 5: Infinite Reconciliation Loop from Status Updates

**What goes wrong:**
Operator updates status subresource, which triggers watch event, which triggers reconciliation, which updates status, creating infinite loop. Controller CPU spikes, API server gets hammered.

**Why it happens:**
- Controller watches all changes to CR, including status changes
- Not using status subresource properly (updating full object instead of status)
- Status changes even when nothing meaningful changed (timestamps, counters)

**How to avoid:**
1. Configure controller to use status subresource: `UpdateStatus()` not `Update()`
2. Only update status when it actually changes (compare before write)
3. Use `meta.SetStatusCondition` which handles deduplication
4. Return `ctrl.Result{}` (no requeue) when status update is the only change
5. Test reconciliation loop with logging to detect rapid requeues

**Warning signs:**
- Controller logs show reconciliation every few seconds
- High API server request rate from operator
- Operator CPU usage unexpectedly high
- Same status condition repeatedly logged as "updated"

**Phase to address:**
Phase 1 (CRD Design) - Define status subresource. Phase 2 (Controller Implementation) - Implement proper status handling.

---

### Pitfall 6: Finalizer Blocks Deletion Forever

**What goes wrong:**
User deletes DittoFS CR. CR stays in "Terminating" state forever because operator's finalizer isn't being removed. Could be caused by operator crash, bug in cleanup logic, or dependent resource that can't be deleted.

**Why it happens:**
- Finalizer added but cleanup logic errors before removing finalizer
- Operator pod not running (crash, eviction, node drain)
- Cleanup tries to delete resource that's already gone (404 error not handled)
- Circular dependency: resource A waits for B, B waits for A

**How to avoid:**
1. Handle "not found" errors gracefully in cleanup - resource gone = success
2. Set timeout for cleanup operations - don't wait forever
3. Log detailed cleanup progress so users can diagnose stuck deletions
4. Consider making cleanup idempotent - safe to run multiple times
5. Add webhook validation to prevent deletion if in unsafe state
6. Document manual finalizer removal as escape hatch (but with warnings)

**Warning signs:**
- CR stuck in Terminating for more than operator's reconciliation period
- Operator logs show cleanup errors or no logs at all (operator not running)
- `kubectl describe` shows finalizers present but deletionTimestamp set

**Phase to address:**
Phase 1 (CRD Design) - Plan finalizer strategy. Phase 4 (Lifecycle Management) - Implement robust cleanup.

---

### Pitfall 7: Owner References Cause Unexpected Garbage Collection

**What goes wrong:**
Cluster-scoped resources (like PVs) or resources in other namespaces get unexpectedly deleted when parent CR is deleted. Or conversely, resources are orphaned when they should be cleaned up.

**Why it happens:**
- Cross-namespace owner references are disallowed by design
- Namespace-scoped owner to cluster-scoped dependent doesn't work
- UID mismatch or stale owner reference triggers garbage collection
- Controller-manager restart can trigger unexpected deletions

**How to avoid:**
1. Never use owner references across namespaces
2. For cluster-scoped resources: use finalizer + explicit deletion instead of owner refs
3. Use foreground deletion policy when order matters
4. For shared resources (like PostgreSQL cluster): don't set owner reference, use labels + finalizer cleanup
5. Test deletion scenarios including controller-manager restart

**Warning signs:**
- Resources disappearing unexpectedly after unrelated events
- Controller-manager logs showing OwnerRefInvalidNamespace events
- Resources with ownerReferences to non-existent objects

**Phase to address:**
Phase 1 (CRD Design) - Define ownership hierarchy. Phase 4 (Lifecycle Management) - Implement correct cleanup patterns.

---

### Pitfall 8: CRD Version Upgrade Migration Breaks Existing Resources

**What goes wrong:**
New operator version has CRD schema changes (v1alpha1 -> v1beta1). Existing CRs fail validation or lose data during conversion. storedVersions in CRD status causes upgrade failures.

**Why it happens:**
- CRD storedVersions tracks all versions ever persisted in etcd
- Removing a version from CRD while storedVersions still references it fails
- Conversion webhooks not implemented or buggy
- Breaking schema changes without migration path

**How to avoid:**
1. Start with versioned CRD from v1alpha1, plan for future versions
2. Only additive changes within same API version
3. Use conversion webhook for major version bumps
4. Implement storage version migration job for existing resources
5. Test upgrade path: v1alpha1 CR exists, deploy v1beta1 operator, verify conversion
6. Document required upgrade sequence (can't skip versions)

**Warning signs:**
- Operator fails to start with CRD validation errors
- Existing CRs show validation errors after operator upgrade
- `status.storedVersions` contains old versions that need migration

**Phase to address:**
Phase 1 (CRD Design) - Version schema from start. Phase 5+ (Upgrades) - Implement migration strategy.

---

## Technical Debt Patterns

Shortcuts that seem reasonable but create long-term problems.

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| Hardcoding namespace | Simpler code | Can't deploy in arbitrary namespace | Never in production operator |
| Skipping status conditions | Faster initial development | Users can't diagnose issues, no tooling integration | Only in prototype |
| Using `Update()` instead of `UpdateStatus()` | One less API call to understand | Infinite reconciliation loops, race conditions | Never |
| Not implementing finalizers | Simpler deletion | Orphaned resources, leaked cloud resources (LB, PV) | Only for purely ephemeral resources |
| Polling instead of watching | Simpler to implement | API server load, delayed reactions | Only for external resources that can't be watched |
| Single reconcile for everything | Less code | Impossible to debug, hard to test | Only for trivial operators |

---

## Integration Gotchas

Common mistakes when connecting to external services.

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| Percona PostgreSQL | Assuming cluster ready when CR exists | Check `status.pgCluster.state: "ready"` condition |
| Percona PostgreSQL | Hardcoding internal service name | Use service name from Percona CR status |
| Scaleway LoadBalancer | Modifying LB via console | Always use Kubernetes annotations only |
| Scaleway LoadBalancer | Using `externalTrafficPolicy: Local` | Use `Cluster` unless specific need for source IP preservation |
| S3 (for DittoFS content store) | Assuming virtual-hosted style | Check if provider needs `forcePathStyle: true` |
| External Secrets Operator | Expecting automatic pod restart | ESO doesn't restart pods; implement checksum pattern |

---

## Performance Traps

Patterns that work at small scale but fail as usage grows.

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Watching all namespaces | High memory, slow startup | Use namespace selector or predicate filters | >100 DittoFS instances |
| No rate limiting on reconcile | API server throttled | Use workqueue rate limiter (default in controller-runtime) | During bulk operations |
| Fetching full object on every reconcile | High API server load | Use cached client, only fetch when needed | >50 reconciles/second |
| Logging every reconcile | Log explosion, storage costs | Log only meaningful events, use debug level | Always in production |
| Not setting resource limits | Operator evicted under memory pressure | Set realistic requests/limits based on testing | Memory pressure events |
| Full resync on every reconcile | CPU spike, API thundering herd | Use informer cache, only resync periodically | >500 watched resources |

---

## Security Mistakes

Domain-specific security issues beyond general web security.

| Mistake | Risk | Prevention |
|---------|------|------------|
| Storing PostgreSQL password in ConfigMap | Anyone with ConfigMap view access sees password | Use Secret, restrict with RBAC |
| Using default ServiceAccount | Operator has minimal permissions, fails silently | Create dedicated SA with precise RBAC |
| Over-permissive ClusterRole | Operator can modify any resource cluster-wide | Use namespace-scoped Role where possible |
| Not encrypting etcd Secrets | Secrets stored base64 (readable), not encrypted | Enable encryption at rest, or use external secret manager |
| Running operator as root | Container escape has full host access | Use securityContext with non-root user |
| Mounting SA token when not needed | Compromised pod can access API server | Set `automountServiceAccountToken: false` on workload pods |

---

## "Looks Done But Isn't" Checklist

Things that appear complete but are missing critical pieces.

- [ ] **NFS Service:** Often missing - ReadinessProbe verification. Mount test from outside cluster, not just internal.
- [ ] **PostgreSQL Integration:** Often missing - Connection retry logic. First connection attempt might fail.
- [ ] **ConfigMap from CRD:** Often missing - Checksum annotation for pod restart.
- [ ] **PVC Creation:** Often missing - StorageClass validation before creation.
- [ ] **Status Conditions:** Often missing - Negative conditions (Ready=False with reason).
- [ ] **Finalizer Cleanup:** Often missing - Handling of already-deleted resources (404 = success).
- [ ] **RBAC:** Often missing - Permissions for status subresource update.
- [ ] **Leader Election:** Often missing - Required if running multiple operator replicas.
- [ ] **Webhook TLS:** Often missing - Cert-manager integration for webhook certificates.
- [ ] **Helm Chart:** Often missing - CRD install hooks (pre-install, not during upgrade).

---

## Recovery Strategies

When pitfalls occur despite prevention, how to recover.

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| LoadBalancer pending | LOW | Switch to NodePort, document LB requirements |
| PostgreSQL not ready | LOW | Implement init container, restart affected pods |
| ConfigMap not reloaded | LOW | Delete affected pods to force restart |
| PVC stuck terminating | MEDIUM | Check pod mounts, remove finalizer if safe |
| Infinite reconciliation | MEDIUM | Fix status update code, restart operator |
| Finalizer blocking deletion | MEDIUM | Manual finalizer removal with kubectl patch |
| Owner ref garbage collection | HIGH | Restore from backup, manually recreate resources |
| CRD version migration failed | HIGH | Rollback operator version, fix migration logic, retry |

---

## Pitfall-to-Phase Mapping

How roadmap phases should address these pitfalls.

| Pitfall | Prevention Phase | Verification |
|---------|------------------|--------------|
| LoadBalancer pending | Phase 2 (Service Exposure) | E2E test: mount NFS from external host |
| Percona dependency not ready | Phase 3 (PostgreSQL Integration) | Integration test: create DittoFS before PostgreSQL ready |
| ConfigMap not triggering restart | Phase 2 (ConfigMap Generation) | Test: update config, verify pod restart |
| PVC stuck | Phase 3 (Storage Management) | Test: full deletion, verify no orphaned PVCs |
| Infinite reconciliation | Phase 2 (Controller Core) | Test: verify reconcile count doesn't spike |
| Finalizer stuck | Phase 4 (Lifecycle) | Test: delete with operator stopped, then restart |
| Owner ref issues | Phase 1 (CRD Design) | Test: delete CR, verify all owned resources deleted |
| CRD migration | Phase 1 (CRD Design) | Plan: versioning strategy documented |

---

## Sources

**Kubernetes Operator Patterns:**
- [Kubernetes Operator Pattern (Official)](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Operator SDK Best Practices](https://sdk.operatorframework.io/docs/best-practices/best-practices/)
- [Red Hat - Kubernetes Operators Best Practices](https://www.redhat.com/en/blog/kubernetes-operators-best-practices)
- [Kubernetes Blog - 7 Common Pitfalls](https://kubernetes.io/blog/2025/10/20/seven-kubernetes-pitfalls-and-how-to-avoid/)

**Finalizers and Garbage Collection:**
- [Kubernetes Finalizers (Official)](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- [Stop Messing with Kubernetes Finalizers](https://martinheinz.dev/blog/74)
- [Hard Lessons Learned about Kubernetes Garbage Collection](https://opensource.com/article/20/6/kubernetes-garbage-collection)

**ConfigMap/Secret Management:**
- [ConfigMaps & Secrets in Kubernetes: Real-World Guide](https://medium.com/devops-diaries-hub/configmaps-secrets-in-kubernetes-the-complete-real-world-guide-with-patterns-pitfalls-and-1a142b344f15)
- [Production-grade Configuration & Secrets Guide](https://dev.to/jumptotech/production-grade-guide-to-configuration-secrets-in-kubernetes-1ogk)

**PVC and Stateful Apps:**
- [Kubernetes PVC: Examples and Best Practices](https://www.plural.sh/blog/kubernetes-pvc-guide/)
- [VolumeClaimTemplate PVC Update Issues](https://medium.com/@tylerauerbeck/plight-of-the-volumeclaimtemplate-how-to-update-your-pvc-once-its-been-created-by-a-managed-9f5886feeb93)

**TCP Service Exposure:**
- [Exposing Custom Ports in Kubernetes (Red Hat 2026)](https://developers.redhat.com/articles/2026/01/28/exposing-custom-ports-kubernetes)
- [GitHub Issue: NFS Service Between Pods](https://github.com/kubernetes/kubernetes/issues/74266)

**Scaleway-Specific:**
- [Scaleway LoadBalancer Troubleshooting](https://www.scaleway.com/en/docs/load-balancer/troubleshooting/k8s-errors/)
- [Scaleway Kubernetes LoadBalancer Guide](https://www.scaleway.com/en/docs/kubernetes/reference-content/kubernetes-load-balancer/)

**Percona PostgreSQL Operator:**
- [Percona Operator for PostgreSQL Documentation](https://docs.percona.com/percona-operator-for-postgresql/index.html)
- [Run PostgreSQL on Kubernetes - Percona Guide](https://www.percona.com/blog/run-postgresql-on-kubernetes-a-practical-guide-with-benchmarks-best-practices/)

**Operator Dependency Management:**
- [The Runaway Problem of Kubernetes Operators and Dependency Lifecycles](https://thenewstack.io/the-runaway-problem-of-kubernetes-operators-and-dependency-lifecycles/)

**CRD Version Migration:**
- [Kubernetes CRD: The Versioning Joy](https://dev.to/jotak/kubernetes-crd-the-versioning-joy-6g0)
- [How to Fix Karpenter CRD Migration Issues](https://medium.com/@chillcaley/how-to-fix-karpenter-crd-migration-issues-during-upgrade-0-x-1-x-b488935ba2bc)

**Reconciliation Loop Issues:**
- [Operator SDK - Subresource Status Infinite Loop Issue](https://github.com/operator-framework/operator-sdk/issues/2795)
- [Kubernetes Reconciliation Loop Pattern](https://medium.com/@inchararlingappa/kubernetes-reconciliation-loop-74d3f38e382f)

---
*Pitfalls research for: DittoFS Kubernetes Operator*
*Researched: 2026-02-04*
