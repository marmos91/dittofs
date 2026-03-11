---
phase: 04-security-hardening
verified: 2026-02-10T22:00:00Z
status: passed
score: 14/14 must-haves verified
---

# Phase 4: Security Hardening Verification Report

**Phase Goal:** Static adapter configuration is fully removed and network access is restricted to only active adapter ports
**Verified:** 2026-02-10T22:00:00Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| #   | Truth                                                                                          | Status     | Evidence                                                                 |
| --- | ---------------------------------------------------------------------------------------------- | ---------- | ------------------------------------------------------------------------ |
| 1   | DittoServer CRD has no spec.nfsPort or spec.smb fields                                        | ✓ VERIFIED | No NFSPort, SMB, or SMBAdapterSpec types in dittoserver_types.go        |
| 2   | Operator does not emit adapter sections in generated DittoFS YAML config                      | ✓ VERIFIED | GenerateDittoFSConfig only emits infra config (logging, DB, metrics)    |
| 3   | Static -file Service is no longer created or reconciled                                       | ✓ VERIFIED | reconcileFileService removed from controller                            |
| 4   | Headless Service uses API port (8080) instead of NFS port                                     | ✓ VERIFIED | reconcileHeadlessService calls getAPIPort, not nfs.GetNFSPort           |
| 5   | Static NFS and SMB container ports are no longer emitted in the StatefulSet                   | ✓ VERIFIED | buildContainerPorts only emits api + metrics, adapter ports are dynamic |
| 6   | NFSEndpoint status field is removed                                                           | ✓ VERIFIED | No NFSEndpoint field in DittoServerStatus struct                        |
| 7   | Webhook validation no longer references NFS or SMB port fields                                | ✓ VERIFIED | No NFSPort/SMB references in dittoserver_webhook.go                     |
| 8   | All existing tests pass after field removal                                                   | ✓ VERIFIED | All controller tests pass (16.159s)                                     |
| 9   | For each running adapter, a NetworkPolicy exists allowing TCP ingress only on that port       | ✓ VERIFIED | reconcileNetworkPolicies creates NetworkPolicy per enabled+running      |
| 10  | When an adapter is stopped, its NetworkPolicy is deleted within one polling cycle             | ✓ VERIFIED | reconcileNetworkPolicies deletes NetworkPolicies not in desired set     |
| 11  | When an adapter is removed, its NetworkPolicy is deleted within one polling cycle             | ✓ VERIFIED | Same deletion logic as stop (desired set excludes stopped/removed)      |
| 12  | NetworkPolicies are garbage-collected when the DittoServer CR is deleted                      | ✓ VERIFIED | SetControllerReference in createAdapterNetworkPolicy                    |
| 13  | Operator has RBAC permission to manage NetworkPolicies                                        | ✓ VERIFIED | networking.k8s.io/networkpolicies in config/rbac/role.yaml              |
| 14  | NetworkPolicy changes do not affect adapter Services or container ports                       | ✓ VERIFIED | NetworkPolicy reconciler is separate, errors propagated independently   |

**Score:** 14/14 truths verified

### Required Artifacts

| Artifact                                                         | Expected                                            | Status     | Details                                                      |
| ---------------------------------------------------------------- | --------------------------------------------------- | ---------- | ------------------------------------------------------------ |
| `api/v1alpha1/dittoserver_types.go`                             | CRD spec without NFSPort and SMB fields             | ✓ VERIFIED | No NFSPort, SMB, SMBAdapterSpec, SMBTimeoutsSpec types       |
| `internal/controller/dittoserver_controller.go`                 | Controller without static adapter logic             | ✓ VERIFIED | 1,141 lines, no reconcileFileService, uses getAPIPort       |
| `internal/controller/networkpolicy_reconciler.go`               | Per-adapter NetworkPolicy lifecycle management      | ✓ VERIFIED | 267 lines, exports reconcileNetworkPolicies                  |
| `internal/controller/networkpolicy_reconciler_test.go`          | Comprehensive tests for NetworkPolicy reconciler    | ✓ VERIFIED | 418 lines, 9 test functions covering all scenarios          |
| `internal/controller/config/config.go`                          | Config generator without adapter sections           | ✓ VERIFIED | GenerateDittoFSConfig emits infrastructure-only config       |
| `config/crd/bases/dittofs.dittofs.com_dittoservers.yaml`        | CRD manifest without NFSPort/SMB/NFSEndpoint fields | ✓ VERIFIED | No matches for nfsPort, smb, or nfsEndpoint                  |
| `config/rbac/role.yaml`                                         | RBAC with networking.k8s.io permissions             | ✓ VERIFIED | networking.k8s.io/networkpolicies with create/delete verbs   |
| `utils/nfs/` (deleted)                                          | Static NFS utility package removed                  | ✓ VERIFIED | Directory does not exist                                     |
| `utils/smb/` (deleted)                                          | Static SMB utility package removed                  | ✓ VERIFIED | Directory does not exist                                     |

### Key Link Verification

| From                                         | To                           | Via                                                    | Status     | Details                                                      |
| -------------------------------------------- | ---------------------------- | ------------------------------------------------------ | ---------- | ------------------------------------------------------------ |
| dittoserver_controller.go                    | buildContainerPorts          | Only emits api + metrics ports, no static nfs/smb      | ✓ WIRED    | buildContainerPorts at line 1116 returns []ContainerPort     |
| dittoserver_controller.go                    | reconcileHeadlessService     | Uses getAPIPort instead of nfs.GetNFSPort              | ✓ WIRED    | Line 791: AddTCPPort("api", getAPIPort(dittoServer))        |
| dittoserver_controller.go                    | reconcileNetworkPolicies     | r.reconcileNetworkPolicies(ctx, dittoServer) in loop   | ✓ WIRED    | Line 283: reconcileNetworkPolicies called, errors propagated |
| networkpolicy_reconciler.go                  | lastKnownAdapters            | r.getLastKnownAdapters(ds) for desired state           | ✓ WIRED    | Line 104: DISC-03 safety check                              |
| dittoserver_controller.go (SetupWithManager) | networkingv1.NetworkPolicy   | Owns(&networkingv1.NetworkPolicy{}) for drift watches  | ✓ WIRED    | Line 628: Owns watch registered                             |
| config/config.go                             | DittoFSConfig struct         | Only includes infrastructure fields, no adapters field | ✓ WIRED    | Lines 34-48: cfg struct has no adapters/nfs/smb              |

### Requirements Coverage

| Requirement | Description                                                                               | Status       | Blocking Issue |
| ----------- | ----------------------------------------------------------------------------------------- | ------------ | -------------- |
| SECU-01     | Static `spec.nfsPort` and `spec.smb` fields removed from DittoServer CRD                 | ✓ SATISFIED  | None           |
| SECU-02     | Operator no longer emits adapter configuration in generated DittoFS YAML config          | ✓ SATISFIED  | None           |
| SECU-03     | Operator creates a NetworkPolicy per active adapter allowing ingress only on port        | ✓ SATISFIED  | None           |
| SECU-04     | Operator deletes NetworkPolicy when corresponding adapter is stopped or removed          | ✓ SATISFIED  | None           |

### Anti-Patterns Found

No anti-patterns detected. Code quality is high:

- No TODO, FIXME, or placeholder comments in modified files
- No empty function implementations
- No console.log-only handlers
- Test coverage is comprehensive (9 test cases for NetworkPolicy reconciler)
- Error handling is proper (NetworkPolicy errors propagated as security-critical)
- Naming conventions are consistent with existing patterns

### Human Verification Required

None. All phase goals are programmatically verifiable and have been verified against the codebase.

## Verification Details

### Plan 04-01: Static Adapter Field Removal

**Truths Verified:**
1. ✓ DittoServer CRD has no spec.nfsPort or spec.smb fields
   - Verified: No NFSPort, SMB, SMBAdapterSpec, SMBTimeoutsSpec, or SMBCreditsSpec in dittoserver_types.go
   - Verified: No WithNFSPort, WithSMB in dittoserver_types_builder.go

2. ✓ Operator does not emit adapter sections in generated DittoFS YAML config
   - Verified: GenerateDittoFSConfig in internal/controller/config/config.go only builds infrastructure config
   - Verified: DittoFSConfig struct has no adapters, nfs, or smb fields

3. ✓ Static -file Service is no longer created or reconciled
   - Verified: No reconcileFileService function in dittoserver_controller.go
   - Verified: No references to "-file" Service in controller code

4. ✓ Headless Service uses API port (8080) instead of NFS port
   - Verified: reconcileHeadlessService at line 781 calls getAPIPort(dittoServer)
   - Verified: No imports of utils/nfs or references to nfs.GetNFSPort

5. ✓ Static NFS and SMB container ports are no longer emitted in the StatefulSet
   - Verified: buildContainerPorts (line 1116) only emits api and metrics ports
   - Verified: Comment at line 1114: "Only emits infrastructure ports (api, metrics). Protocol adapter ports are managed dynamically by reconcileContainerPorts"

6. ✓ NFSEndpoint status field is removed
   - Verified: No NFSEndpoint field in DittoServerStatus struct
   - Verified: No WithNFSEndpoint in dittoserver_types_builder.go

7. ✓ Webhook validation no longer references NFS or SMB port fields
   - Verified: No matches for nfsPort, NFSPort, smb, or SMB in dittoserver_webhook.go (case-insensitive)

8. ✓ All existing tests pass after field removal
   - Verified: go test ./internal/controller/... passes (16.159s)
   - Verified: NetworkPolicy tests all pass (9/9 test functions)

**Artifacts Verified:**
- `api/v1alpha1/dittoserver_types.go`: Exists, substantive (DittoServerSpec struct without static fields), wired (imported by controller)
- `internal/controller/dittoserver_controller.go`: Exists (1,141 lines), substantive (no reconcileFileService, no static adapter logic), wired (reconciles all resources)
- `internal/controller/config/config.go`: Exists, substantive (GenerateDittoFSConfig emits infrastructure only), wired (called by reconcileConfigMap)
- `utils/nfs/`: Missing (deleted as expected)
- `utils/smb/`: Missing (deleted as expected)

**Key Links Verified:**
- buildContainerPorts: Returns only api + metrics ports, no static adapter ports
- reconcileHeadlessService: Uses getAPIPort instead of nfs.GetNFSPort
- GenerateDittoFSConfig: Returns YAML without adapter sections

### Plan 04-02: Per-Adapter NetworkPolicy Lifecycle

**Truths Verified:**
1. ✓ For each running adapter, a NetworkPolicy exists allowing TCP ingress only on that adapter's port
   - Verified: reconcileNetworkPolicies at line 100 creates NetworkPolicy for enabled+running adapters
   - Verified: buildAdapterNetworkPolicy at line 61 creates policy with single TCP port ingress
   - Verified: Test TestReconcileNetworkPolicies_CreatesForRunningAdapter passes

2. ✓ When an adapter is stopped, its NetworkPolicy is deleted within one polling cycle
   - Verified: Lines 159-165 delete NetworkPolicies not in desired set
   - Verified: Test TestReconcileNetworkPolicies_DeletesWhenAdapterStops passes

3. ✓ When an adapter is removed, its NetworkPolicy is deleted within one polling cycle
   - Verified: Same deletion logic as stopped adapters (desired set excludes both stopped and removed)
   - Verified: Test TestReconcileNetworkPolicies_EmptyAdapters_DeletesOrphans passes

4. ✓ NetworkPolicies are garbage-collected when the DittoServer CR is deleted
   - Verified: Line 175 in createAdapterNetworkPolicy calls controllerutil.SetControllerReference
   - Verified: Test TestReconcileNetworkPolicies_OwnerReferenceSet passes

5. ✓ Operator has RBAC permission to manage NetworkPolicies
   - Verified: config/rbac/role.yaml contains networking.k8s.io/networkpolicies with create, delete, get, list, patch, update verbs

6. ✓ NetworkPolicy changes do not affect adapter Services or container ports
   - Verified: NetworkPolicy reconciler is in separate function, independent of service_reconciler and container port reconciler
   - Verified: Errors are propagated independently (line 288: return ctrl.Result{}, err)

**Artifacts Verified:**
- `internal/controller/networkpolicy_reconciler.go`: Exists (267 lines), substantive (exports reconcileNetworkPolicies, complete lifecycle management), wired (called from controller Reconcile loop)
- `internal/controller/networkpolicy_reconciler_test.go`: Exists (418 lines), substantive (9 comprehensive test functions), wired (tests import and run reconcileNetworkPolicies)
- RBAC role: Updated with networking.k8s.io permissions

**Key Links Verified:**
- reconcileNetworkPolicies integration: Called at line 283 in dittoserver_controller.go, errors propagated
- getLastKnownAdapters: Called at line 104 for DISC-03 safety (nil check before reconciling)
- Owns watch: Line 628 registers Owns(&networkingv1.NetworkPolicy{}) for drift detection

**Test Coverage:**
9 test functions verified:
1. TestReconcileNetworkPolicies_NilAdapters_Skips — DISC-03 safety
2. TestReconcileNetworkPolicies_EmptyAdapters_DeletesOrphans — Cleanup orphans
3. TestReconcileNetworkPolicies_CreatesForRunningAdapter — Create for enabled+running
4. TestReconcileNetworkPolicies_MultipleAdapters — Multiple adapters handled correctly
5. TestReconcileNetworkPolicies_DeletesWhenAdapterStops — Delete when stopped
6. TestReconcileNetworkPolicies_UpdatesWhenPortChanges — Update on port change
7. TestReconcileNetworkPolicies_IgnoresDisabledAdapters — Only enabled+running get policies
8. TestReconcileNetworkPolicies_DoesNotTouchStaticResources — Label-based isolation
9. TestReconcileNetworkPolicies_OwnerReferenceSet — Garbage collection via owner refs

All tests pass.

---

_Verified: 2026-02-10T22:00:00Z_
_Verifier: Claude (gsd-verifier)_
