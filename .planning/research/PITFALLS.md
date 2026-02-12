# Domain Pitfalls

**Domain:** Dynamic adapter-driven K8s Service/NetworkPolicy management for DittoFS operator
**Researched:** 2026-02-09

## Critical Pitfalls

Mistakes that cause rewrites, data loss, or prolonged outages.

### Pitfall 1: Orphaned LoadBalancer Services on Adapter Removal

**What goes wrong:** When the operator deletes a LoadBalancer Service because an adapter was removed or disabled, the cloud controller may have already allocated an external IP. If the operator creates-then-deletes Services rapidly (e.g., adapter toggled on/off in quick succession), cloud-allocated resources like external IPs and backend pools may leak. Worse, if the owner reference is not set correctly, deleting the DittoServer CR leaves LoadBalancer Services behind -- each costing money and consuming IP quota.

**Why it happens:** The operator creates Services dynamically based on polling results. Unlike the current static Services (headless, file, API, metrics) which are created once per reconciliation, dynamic per-adapter Services have a lifecycle tied to external REST API state that can change between polls. The operator may create a Service, but on the next poll the adapter is gone, triggering deletion. The cloud LB controller works asynchronously -- it may still be provisioning the LB when deletion is requested.

**Consequences:**
- Leaked cloud load balancers costing real money (each LB is ~$15-25/month on AWS/GCP/Azure)
- Exhausted IP address quotas blocking future Service creation
- Stale Services pointing to ports that no longer accept traffic
- On DittoServer CR deletion, dynamic Services not cleaned up if owner references were missed

**Warning signs:**
- `kubectl get svc` shows Services with `<pending>` external IP for adapters that are no longer running
- Cloud billing shows unexpected load balancer charges
- `kubectl get svc -l app=dittofs-server --field-selector metadata.ownerReferences=` shows Services without owner references

**Prevention:**
1. **Always** set `controllerutil.SetControllerReference()` on every dynamically created Service, exactly as the existing operator does for static Services
2. Add the dynamic Services to `.Owns(&corev1.Service{})` in `SetupWithManager()` (already present -- the existing operator watches Services, so any owned Service deletion/change triggers reconcile)
3. Before deleting a Service, check if the LoadBalancer has a finalizer from the cloud controller. If it does, the deletion will block until the cloud controller cleans up -- this is correct behavior. Do NOT force-remove the finalizer
4. Add a label like `dittofs.io/adapter-type: nfs` to dynamically created Services so the operator can list only its dynamic Services and compare against the current adapter set
5. On each reconcile, compute the "desired set" of Services from the API response and delete any Services that exist but are not in the desired set (garbage collection pattern)

**Phase:** Phase 1 (core dynamic Service creation). This must be correct from the start -- fixing orphaned cloud resources retroactively is painful.

**Confidence:** HIGH -- based on existing operator patterns (owner references already used), Kubernetes GC documentation, and known cloud LB billing issues.

---

### Pitfall 2: StatefulSet Rolling Restart Storm from Container Port Changes

**What goes wrong:** Every time the operator detects an adapter change (new adapter added, adapter port changed, adapter removed), it updates the StatefulSet's container ports. Since container ports are part of `spec.template`, ANY change triggers a rolling restart of ALL pods. With a 30-second poll interval, rapid adapter changes (e.g., admin creating and configuring adapters right after deployment) can cause continuous restarts where pods never stabilize.

**Why it happens:** The current operator already sets container ports statically via `buildContainerPorts()`. Moving to dynamic ports means the port list changes based on external API state. Each poll that detects a difference triggers `CreateOrUpdate` on the StatefulSet, which mutates `spec.template.spec.containers[0].ports`, causing Kubernetes to initiate a rolling update. The config hash mechanism (existing `resources.ConfigHashAnnotation`) makes this even worse because port changes also change the config hash in the pod annotation.

**Consequences:**
- Active NFS/SMB connections dropped during pod restart (client-visible outage)
- Pod stuck in `CrashLoopBackOff` if the adapter port is changed to a value the DittoFS binary is not yet listening on
- Cascading restarts if the DittoFS API is temporarily unavailable during restart, causing the operator to see "no adapters" and remove all ports

**Warning signs:**
- StatefulSet `observedGeneration` rapidly incrementing
- Pod restart count climbing
- `kubectl describe sts` shows frequent `spec.template` changes
- NFS clients reporting `ESTALE` or disconnects

**Prevention:**
1. **Decouple container port declarations from adapter state.** Container ports in K8s are purely informational (documentation/probes) -- they do NOT control which ports the container actually listens on. The DittoFS binary opens ports based on its runtime adapter configuration, not container port declarations. Consider making container ports static (always declare the well-known NFS/SMB/API/metrics ports) and only updating Services dynamically
2. If container port updates ARE required: batch changes. Only update the StatefulSet once per reconcile cycle, not on every individual adapter change. Compare current vs. desired ports and only write if different
3. Add a stabilization period: after detecting an adapter change, wait for N consecutive polls (e.g., 2-3) showing the same state before updating the StatefulSet. This prevents thrashing from transient API states
4. Separate the config hash for "infrastructure changes" (which should trigger restarts) from "adapter discovery changes" (which should NOT trigger restarts unless absolutely necessary)

**Phase:** Phase 2 (StatefulSet port management). The architectural decision about whether to update container ports at all should be made early in Phase 1 design.

**Confidence:** HIGH -- the rolling restart behavior is well-documented K8s behavior. The current operator already demonstrates awareness of this via the config hash pattern.

---

### Pitfall 3: Chicken-and-Egg: Operator Cannot Poll Until DittoFS Is Running

**What goes wrong:** The operator needs to poll `GET /api/v1/adapters` to discover which Services to create. But DittoFS needs to be running first. And DittoFS needs its Services and StatefulSet to be created by the operator to start. This creates a circular dependency: operator waits for API --> API needs pod --> pod needs StatefulSet --> StatefulSet needs operator to reconcile.

**Why it happens:** The current operator creates all resources (ConfigMap, Services, StatefulSet) in a single reconcile pass with no dependency on the DittoFS REST API. The new design adds an API polling step that cannot execute until the pod is healthy. If the operator blocks on polling before creating the StatefulSet, nothing will ever start.

**Consequences:**
- DittoServer CR stuck in `Pending` forever on fresh deployment
- Operator logs show repeated "connection refused" or "no such host" errors
- Users think the operator is broken

**Warning signs:**
- Fresh deployment shows DittoServer in `Pending` for more than a few minutes
- Operator logs filled with API connection errors
- No StatefulSet created

**Prevention:**
1. **Two-phase reconciliation.** Phase A: Create all infrastructure resources (ConfigMap, headless Service, API Service, StatefulSet) WITHOUT any adapter-dependent resources. This is essentially the current reconcile flow. Phase B: Once the pod is healthy (readiness probe passes), THEN poll the adapter API and create/update dynamic Services
2. Use the existing readiness probe path (`/health/ready`) to gate the polling step. Only attempt adapter discovery when `status.readyReplicas >= 1`
3. On fresh deployment, return `RequeueAfter: 10s` after creating infrastructure, giving the pod time to start before attempting adapter discovery
4. Consider creating default Services (NFS on 12049, SMB on 1445) during Phase A based on DittoFS defaults, then refining them in Phase B. This provides connectivity even before the first poll completes. Note that the `AdapterResponse` from the API already includes the `running` field, so the operator knows whether the adapter is actually serving.

**Phase:** Phase 1 (polling architecture). This is a fundamental design decision that shapes the entire reconcile loop.

**Confidence:** HIGH -- this is a structural issue visible from the current code (`Reconcile()` runs sequentially through resource creation).

---

### Pitfall 4: Service Account Bootstrap Race with JWT Authentication

**What goes wrong:** The operator needs to auto-create a DittoFS service account with a new "operator" role to authenticate with the REST API. But creating this account requires calling `POST /api/v1/users` which requires admin authentication. The operator needs to know the admin credentials (JWT secret + admin password) to bootstrap the operator account, creating a complex auth bootstrapping sequence.

**Why it happens:** The DittoFS API requires JWT authentication (`RequireAdmin()` middleware on adapter endpoints). The operator needs a token to call `GET /api/v1/adapters`. Getting a token requires `POST /api/v1/auth/login` with valid credentials. The operator account does not exist yet on a fresh deployment. Creating it requires admin credentials. The admin password is either auto-generated (logged at startup) or set via a Secret referenced in the CRD.

**Consequences:**
- Operator cannot authenticate with DittoFS API on first deployment
- If admin password changes, operator loses access until manually reconfigured
- JWT tokens expire (default 15m), requiring refresh logic in the operator
- Token refresh failures cascade to adapter discovery failures

**Warning signs:**
- Operator logs show `401 Unauthorized` or `403 Forbidden` from adapter API
- Adapter Services not being created despite pod being healthy
- Status condition showing "AdapterDiscoveryFailed" or similar

**Prevention:**
1. **Reuse the admin credentials the operator already has access to.** The operator already mounts the JWT secret and admin password via environment variables (see `buildSecretEnvVars()`). The operator pod can read the same Secrets to construct API credentials
2. Implement a credential manager in the operator that:
   a. Reads the JWT secret and admin password from the referenced K8s Secrets
   b. Calls `POST /api/v1/auth/login` to obtain an access token
   c. Caches the token in memory
   d. Refreshes before expiry (proactively, not reactively)
   e. On 401 response, force-refresh and retry once
3. Store the token in the reconciler struct, not in a K8s Secret (it is short-lived and per-operator-instance)
4. Alternatively, add a new auth bypass mechanism in DittoFS for local/in-pod authentication (e.g., a shared secret file mounted in both containers, or a Unix socket), but this adds complexity to DittoFS itself

**Phase:** Phase 1 (authentication). Must be solved before any polling can work.

**Confidence:** HIGH -- the auth flow is visible in the existing handler code (`RequireAdmin` middleware) and the operator already references the Secrets.

---

## Moderate Pitfalls

### Pitfall 5: Stale Adapter State from API Unavailability

**What goes wrong:** The DittoFS pod restarts (crash, rolling update, node eviction). During restart, the operator polls `GET /api/v1/adapters` and gets a connection error. The operator must decide: keep existing Services (stale but possibly correct) or delete them (premature, causes unnecessary downtime).

**Why it happens:** The polling model means the operator's view of adapter state is always potentially stale by up to one polling interval. During pod restarts, the API is completely unavailable. The "last known good" state may or may not reflect the desired state when the pod comes back.

**Prevention:**
1. **Never delete Services on API failure.** Treat API unavailability as "no information" rather than "no adapters." Only modify Services when the API returns a successful response
2. Set a dedicated status condition like `AdaptersSynced` that reflects whether the operator's view is current. Set to `False` when the API is unreachable, `True` when a successful poll returns
3. Add exponential backoff on API failures (separate from the regular poll interval) to avoid hammering a recovering DittoFS instance
4. Record the last successful adapter state in the DittoServer status (e.g., `status.lastKnownAdapters`) so it survives operator restarts

**Phase:** Phase 1 (polling resilience).

**Confidence:** HIGH -- this is a well-known pattern from any operator that depends on external state.

---

### Pitfall 6: CreateOrUpdate Conflict Loops on Dynamic Services

**What goes wrong:** The operator uses `controllerutil.CreateOrUpdate` to manage Services. Cloud controllers (AWS LB controller, GCP Cloud Controller Manager, etc.) also modify these Services, adding annotations, setting `status.loadBalancer.ingress`, and modifying `spec.healthCheckNodePort`. Both the operator and the cloud controller are writing to the same object, causing optimistic locking conflicts that trigger repeated retries.

**Why it happens:** The existing operator already handles this well with `mergeServiceSpec()` and `mergePorts()` which preserve cloud-managed fields. However, dynamic Services add a new dimension: the operator may be creating and deleting Services more frequently, and each creation triggers the cloud controller to provision a load balancer (async, 30-120 seconds). During that provisioning window, both controllers are actively modifying the Service.

**Prevention:**
1. **Continue using the merge pattern** from the existing `mergeServiceSpec()` and `mergePorts()`. Apply it to all dynamically created Services
2. Keep the existing `retryOnConflict()` wrapper (already in the codebase) for all Service operations
3. Avoid setting `status.loadBalancer` -- that is exclusively owned by the cloud controller
4. Add `spec.externalTrafficPolicy: Local` only if explicitly configured, never as a default (it changes how the cloud controller provisions the LB)
5. Use Server-Side Apply (SSA) instead of CreateOrUpdate for dynamic Services. SSA has field ownership tracking and avoids conflicts entirely. This is the modern controller-runtime recommendation

**Phase:** Phase 1 (Service creation).

**Confidence:** HIGH -- the existing operator has already solved this for static Services. The same patterns apply.

---

### Pitfall 7: NetworkPolicy Blocks Traffic to Newly Created Adapter Services

**What goes wrong:** The operator creates a NetworkPolicy that only allows ingress on ports matching active adapters. When a new adapter is added, the Service is created first but the NetworkPolicy is updated slightly later (or vice versa). During this window, traffic to the new adapter port is blocked by the existing NetworkPolicy.

**Why it happens:** NetworkPolicies are additive and whitelist-based. If a pod is selected by ANY NetworkPolicy with an `ingress` section, all ingress not explicitly allowed is denied. If the operator first updates the NetworkPolicy to remove the old port and then adds the new port, there is a brief window where neither policy allows the new port.

**Prevention:**
1. **Order of operations matters:** When adding a new adapter, update the NetworkPolicy FIRST (to allow the new port), THEN create the Service. When removing an adapter, delete the Service FIRST, THEN update the NetworkPolicy to remove the port
2. Consider making the NetworkPolicy additive: use one NetworkPolicy per adapter rather than a single policy with all ports. This way, adding/removing an adapter only adds/removes a NetworkPolicy, never modifying an existing one
3. If using a single NetworkPolicy, always compute the full desired port list and apply it atomically. Never do incremental add/remove
4. Evaluate whether NetworkPolicies are truly needed for this use case. The operator already controls which ports the Service exposes. If the pod only listens on adapter ports, traffic to other ports is already rejected at the application level. NetworkPolicies add defense-in-depth but also add significant complexity to the reconcile loop
5. Not all clusters have a NetworkPolicy-capable CNI. Check for Calico/Cilium/etc. before creating NetworkPolicies, or make it opt-in via a CRD field

**Phase:** Phase 3 (NetworkPolicy management). This should come after Services are stable and tested.

**Confidence:** MEDIUM -- depends on the specific CNI plugin. The behavior is well-documented for Calico and Cilium but may vary.

---

### Pitfall 8: LoadBalancer IP Stuck in Pending Blocks Status Reporting

**What goes wrong:** The operator creates a LoadBalancer Service for a new adapter. The cloud controller takes 30-120 seconds (or longer on some providers) to provision the load balancer and assign an external IP. During this time, `status.loadBalancer.ingress` is empty. If the operator reports the adapter endpoint in the CRD status, it has no IP to report. If the operator waits for the IP, the reconcile blocks.

**Why it happens:** LoadBalancer provisioning is asynchronous and provider-dependent. AWS NLB/ALB, GCP, and Azure all have different provisioning times. Some environments (bare metal without MetalLB) will never provision an IP.

**Prevention:**
1. **Do not block reconciliation on LoadBalancer IP allocation.** Create the Service and immediately continue. The IP will appear eventually
2. Report Service status in the DittoServer CR status: include both the Service name (always available) and the external IP (populated when available, empty when pending)
3. Watch Service status changes (already handled by `.Owns(&corev1.Service{})` in the existing operator) to update the CRD status when the IP becomes available
4. Add a timeout condition: if a LoadBalancer IP is not allocated within a configurable timeout (e.g., 5 minutes), set a warning condition on the DittoServer status but do not delete the Service
5. Document that bare-metal clusters need MetalLB or similar for LoadBalancer Services, or allow overriding the Service type to NodePort/ClusterIP via the CRD

**Phase:** Phase 2 (status reporting). The Service creation itself (Phase 1) does not need this.

**Confidence:** HIGH -- LoadBalancer provisioning delays are universal across cloud providers.

---

## Minor Pitfalls

### Pitfall 9: Polling Interval Tuning -- Too Fast Wastes Resources, Too Slow Misses Changes

**What goes wrong:** A 30-second default poll interval means adapter changes take up to 30 seconds to reflect in K8s resources. For development/demo, this feels slow. Setting it too low (e.g., 1 second) generates unnecessary API calls and reconciliation loops.

**Prevention:**
1. Default to 30 seconds (as specified in requirements)
2. Allow per-CRD override via `spec.adapterPolling.interval`
3. Add a minimum floor of 10 seconds to prevent abuse
4. Consider adaptive polling: poll frequently when the DittoServer is in `Pending` or `Progressing` state, less frequently when stable
5. Log the polling interval on startup so operators can verify it

**Phase:** Phase 1 (polling configuration).

**Confidence:** HIGH -- purely a configuration decision.

---

### Pitfall 10: CRD Migration -- Removing Static Adapter Fields Breaks Existing Deployments

**What goes wrong:** The project requirement includes removing `spec.nfsPort` and `spec.smb` from the CRD. Existing DittoServer resources in production clusters have these fields set. If the CRD is updated without a migration strategy, Kubernetes will either reject the update (if the validation webhook requires the fields) or silently drop the values (if the fields are removed from the schema).

**Prevention:**
1. **Phase the removal.** First, make the fields optional and ignored by the operator code. The operator reads adapter state from the API, not the CRD. This is a non-breaking change
2. Add a deprecation warning in the operator logs when these fields are set but ignored
3. In a later release, remove the fields from the CRD schema entirely
4. Provide migration documentation showing users how to configure adapters via the REST API instead of CRD fields
5. The config generation code (`GenerateDittoFSConfig()`) currently does not include adapter config -- it already generates infrastructure-only YAML. This is good; the removal of CRD fields is purely a CRD schema change

**Phase:** Phase 1 (CRD changes). But the actual removal should be in a separate minor version bump.

**Confidence:** HIGH -- CRD schema changes and backward compatibility are well-understood K8s upgrade concerns.

---

### Pitfall 11: RBAC Permissions for NetworkPolicy Management Not in Current Operator

**What goes wrong:** The current operator has RBAC markers for core resources (Services, ConfigMaps, Secrets, StatefulSets) and Percona CRDs. It does NOT have RBAC for `networking.k8s.io/networkpolicies`. If the operator tries to create a NetworkPolicy without the right RBAC, it will get a `403 Forbidden` error.

**Prevention:**
1. Add kubebuilder RBAC markers:
   ```go
   // +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
   ```
2. Add `serviceaccounts` RBAC if the operator auto-creates a DittoFS service account:
   ```go
   // +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
   ```
3. Run `make manifests` to regenerate the ClusterRole from markers
4. Test in a namespace with restrictive RBAC policies (not just the default namespace)

**Phase:** Phase 1 (RBAC setup) for ServiceAccounts, Phase 3 (NetworkPolicy) for networking RBAC.

**Confidence:** HIGH -- direct observation from the current RBAC markers in the codebase.

---

### Pitfall 12: Adapter API Response Missing `running` Field Causes False Negatives

**What goes wrong:** The operator assumes the adapter API response always includes the `running` field accurately. If DittoFS has a bug where `running` is false even though the adapter is accepting connections (or vice versa), the operator will create/delete Services incorrectly.

**Prevention:**
1. The operator should consider an adapter "active" if `enabled == true` AND the API returned it (presence implies the adapter configuration exists). The `running` field is secondary -- it indicates current runtime state, which may be transient
2. Only delete a Service if the adapter is not present in the API response at all, or if `enabled == false`. A temporarily non-running adapter (e.g., during restart) should keep its Service
3. Log discrepancies between `enabled` and `running` for debugging

**Phase:** Phase 1 (polling logic).

**Confidence:** MEDIUM -- depends on the correctness of the DittoFS `IsAdapterRunning()` implementation, which is internal to the runtime.

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| Polling architecture (Phase 1) | Chicken-and-egg: cannot poll before pod runs | Two-phase reconciliation: infrastructure first, then discovery |
| Authentication (Phase 1) | Operator cannot get JWT token on fresh deploy | Reuse admin credentials from existing K8s Secrets |
| Dynamic Service creation (Phase 1) | Orphaned LoadBalancer Services | Owner references + label-based garbage collection |
| StatefulSet port updates (Phase 2) | Rolling restart storm from frequent port changes | Decouple container ports from adapter state; ports are informational |
| CRD field removal (Phase 1) | Breaks existing deployments | Phase removal: make optional first, remove later |
| NetworkPolicy (Phase 3) | Temporary traffic blocking during policy updates | Order of operations: allow first, then expose; or per-adapter policies |
| RBAC (Phase 1/3) | Missing permissions for new resource types | Add kubebuilder RBAC markers before implementation |
| Status reporting (Phase 2) | Blocking on LoadBalancer IP allocation | Non-blocking: report Service name immediately, IP when available |
| API resilience (Phase 1) | Deleting Services when API is unreachable | Never modify Services on API failure; keep last known state |
| Conflict handling (Phase 1) | Optimistic locking conflicts with cloud controllers | Use existing merge patterns; consider Server-Side Apply |

## Sources

- [Kubernetes Garbage Collection](https://kubernetes.io/docs/concepts/architecture/garbage-collection/) -- Owner reference cascading deletion behavior
- [Kubebuilder Good Practices](https://book.kubebuilder.io/reference/good-practices) -- Reconciliation idempotency, status subresource patterns
- [Kubebuilder: Owned Resources](https://book.kubebuilder.io/reference/watching-resources/secondary-owned-resources) -- Owner reference patterns for secondary resources
- [Operator SDK Common Recommendations](https://sdk.operatorframework.io/docs/best-practices/common-recommendation/) -- Reconcile loop best practices
- [Controller-runtime cross-namespace owner refs PR](https://github.com/kubernetes-sigs/controller-runtime/pull/675/files) -- Cross-namespace owner reference restrictions
- [Kubernetes Controllers at Scale: Conflicts](https://medium.com/@timebertt/kubernetes-controllers-at-scale-clients-caches-conflicts-patches-explained-aa0f7a8b4332) -- CreateOrUpdate conflict handling
- [Understanding Conflict Errors in Operators](https://alenkacz.medium.com/kubernetes-operators-best-practices-understanding-conflict-errors-d05353dff421) -- Stale data and optimistic locking
- [Helm ClusterIP immutability issue](https://github.com/helm/helm/issues/7956) -- Service immutable fields
- [Self-Healing Controllers for External State](https://anynines.com/blog/external-state-drift-kubernetes-controller-self-healing-design/) -- RequeueAfter patterns for external state
- [Jenkins K8s Operator Reconcile Failures](https://github.com/jenkinsci/kubernetes-operator/issues/358) -- Real-world example of API polling failures in reconcile loops
- [Kubernetes Network Policies Guide](https://kubernetes.io/docs/concepts/services-networking/network-policies/) -- NetworkPolicy behavior and default deny semantics
- [LoadBalancer IP Stuck in Pending](https://paul-boone.medium.com/kubernetes-loadbalancer-ip-stuck-in-pending-6ddea72b8ff5) -- Cloud LB provisioning issues
- DittoFS operator codebase analysis: `k8s/dittofs-operator/internal/controller/dittoserver_controller.go` -- existing patterns for Service management, conflict retry, config hashing
- DittoFS adapter API: `internal/controlplane/api/handlers/adapters.go` -- API response format including `running` field
- DittoFS API client: `pkg/apiclient/adapters.go` -- existing client library for adapter operations
