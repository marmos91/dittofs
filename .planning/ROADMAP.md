# Roadmap: DittoFS Kubernetes Operator

## Overview

This roadmap delivers a production-ready Kubernetes operator for DittoFS, progressing from basic operator scaffolding through ConfigMap generation, storage management, PostgreSQL integration, and production lifecycle features. Each phase builds on the previous, culminating in validated deployment on Scaleway Kubernetes with full documentation.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

- [x] **Phase 1: Operator Foundation** - Functional operator skeleton with DittoFS CRD creating StatefulSet
- [ ] **Phase 2: ConfigMap Generation and Services** - ConfigMap from CRD spec; LoadBalancer Services for NFS, SMB, API
- [ ] **Phase 3: Storage Management** - VolumeClaimTemplates for metadata, payload, cache PVCs
- [ ] **Phase 4: Percona PostgreSQL Integration** - PerconaPGCluster watching; connection Secret extraction; readiness gating
- [ ] **Phase 5: Status Conditions and Lifecycle** - Full status conditions, finalizers, events, health probes
- [ ] **Phase 6: Documentation and Deployment** - Complete documentation and validation on Scaleway cluster

## Phase Details

### Phase 1: Operator Foundation
**Goal**: Functional operator skeleton with DittoFS CRD that creates a StatefulSet
**Depends on**: Nothing (first phase)
**Requirements**: R1.1, R1.2, R1.3, R1.4, R1.5
**Complexity**: Medium
**Success Criteria** (what must be TRUE):
  1. `kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml` creates a DittoFS CR
  2. Operator reconciles CR and creates a StatefulSet with single replica
  3. DittoFS pod starts successfully (hardcoded config, memory stores)
  4. `kubectl get dittofs` shows the custom resource with basic status
  5. Operator RBAC allows creating/managing StatefulSets, Services, ConfigMaps
**Key Deliverables**:
  - Operator SDK scaffold in `k8s/dittofs-operator/` directory
  - DittoFS CRD (v1alpha1) with complete spec schema
  - Basic controller reconciliation loop
  - RBAC (ServiceAccount, Role, RoleBinding)
  - Sample CR for testing
**Plans**: 3 plans

Plans:
- [x] 01-01-PLAN.md - Relocate operator to k8s/dittofs-operator/ with updated module path
- [x] 01-02-PLAN.md - Fix RBAC (add secrets), CRD shortName, create memory sample CR
- [x] 01-03-PLAN.md - End-to-end validation on local/test cluster

### Phase 2: ConfigMap Generation and Services
**Goal**: ConfigMap generated from CRD spec; LoadBalancer Services for NFS, SMB, API; checksum annotation for pod restart
**Depends on**: Phase 1
**Requirements**: R2.1, R2.2, R2.3, R2.4, R2.5, R2.6
**Complexity**: Medium
**Success Criteria** (what must be TRUE):
  1. CRD spec changes generate updated ConfigMap with DittoFS YAML configuration
  2. Pod restarts automatically when ConfigMap content changes (checksum annotation)
  3. NFS port accessible via LoadBalancer Service (default 2049)
  4. SMB port accessible via LoadBalancer Service (default 445)
  5. REST API accessible via LoadBalancer/ClusterIP Service (port 8080)
**Key Deliverables**:
  - pkg/configgen: CRD-to-ConfigMap transformer
  - pkg/resources: Builder pattern for Kubernetes resources
  - Checksum annotation pattern implementation
  - LoadBalancer Services for NFS, SMB, API
  - NodePort fallback when LoadBalancer unavailable
**Plans**: TBD

Plans:
- [ ] 02-01: ConfigMap generation from CRD spec
- [ ] 02-02: Checksum annotation and pod restart
- [ ] 02-03: LoadBalancer Services for NFS, SMB, API

### Phase 3: Storage Management
**Goal**: VolumeClaimTemplates for metadata, payload, cache PVCs; StorageClass validation
**Depends on**: Phase 2
**Requirements**: R3.1, R3.2, R3.3, R3.4, R3.5, R3.6
**Complexity**: Medium
**Success Criteria** (what must be TRUE):
  1. BadgerDB metadata store uses PVC that persists across pod restarts
  2. Filesystem payload store uses PVC that persists across pod restarts
  3. Cache (WAL persistence) uses PVC that survives pod restarts
  4. Memory store configuration works without PVC creation
  5. S3 store configuration accepts Cubbit DS3 credentials via Secret reference
**Key Deliverables**:
  - VolumeClaimTemplates in StatefulSet for metadata, payload, cache
  - StorageClass validation before PVC creation
  - Memory store configuration (no PVC)
  - S3 credentials Secret reference support
  - PVC lifecycle management (proper deletion order)
**Plans**: TBD

Plans:
- [ ] 03-01: VolumeClaimTemplates for persistent storage
- [ ] 03-02: Memory and S3 store configuration
- [ ] 03-03: StorageClass validation and PVC lifecycle

### Phase 4: Percona PostgreSQL Integration
**Goal**: PerconaPGCluster watching; connection Secret extraction; readiness gating
**Depends on**: Phase 3
**Requirements**: R4.1, R4.2, R4.3, R4.4, R4.5
**Complexity**: High
**Success Criteria** (what must be TRUE):
  1. Operator watches PerconaPGCluster resources in same namespace
  2. Connection details extracted from Percona-created Secret
  3. DittoFS pod waits for PostgreSQL readiness before starting (init container)
  4. ConfigMap includes PostgreSQL connection string for metadata store
  5. DittoFS successfully connects to PostgreSQL metadata store on startup
**Key Deliverables**:
  - pkg/percona: Percona operator integration package
  - PerconaPGCluster watching with predicates
  - Connection Secret extraction logic
  - Init container for PostgreSQL readiness check
  - PostgreSQL metadata store ConfigMap generation
**Plans**: TBD

Plans:
- [ ] 04-01: Percona operator CRD watching
- [ ] 04-02: Connection Secret extraction and init container
- [ ] 04-03: PostgreSQL metadata store configuration

### Phase 5: Status Conditions and Lifecycle
**Goal**: Full status conditions, finalizers, events, health probes
**Depends on**: Phase 4
**Requirements**: R5.1, R5.2, R5.3, R5.4, R5.5
**Complexity**: Medium
**Success Criteria** (what must be TRUE):
  1. `kubectl get dittofs -o yaml` shows conditions: Ready, Available, DatabaseReady, ConfigReady
  2. Deleting DittoFS CR cleans up all owned resources (finalizer)
  3. Important events visible via `kubectl describe dittofs <name>`
  4. DittoFS pod has working liveness and readiness probes
  5. Graceful shutdown completes within configured timeout
**Key Deliverables**:
  - Status conditions implementation (Ready, Available, Degraded, DatabaseReady, ConfigReady)
  - Finalizers for clean resource cleanup
  - Kubernetes events for debugging
  - Health probes configuration (liveness, readiness)
  - Graceful shutdown handling
**Plans**: TBD

Plans:
- [ ] 05-01: Status conditions implementation
- [ ] 05-02: Finalizers and resource cleanup
- [ ] 05-03: Events, health probes, graceful shutdown

### Phase 6: Documentation and Deployment
**Goal**: Complete documentation and validation on Scaleway cluster
**Depends on**: Phase 5
**Requirements**: R6.1, R6.2, R6.3, R6.4, R6.5
**Complexity**: Low
**Success Criteria** (what must be TRUE):
  1. CRD reference documentation covers all spec fields with examples
  2. Installation guide works for fresh cluster (kubectl apply or Helm)
  3. Percona operator integration guide enables PostgreSQL metadata store
  4. Troubleshooting guide covers common issues (LoadBalancer pending, PVC stuck)
  5. End-to-end validation passes on Scaleway `dittofs-demo` cluster
**Key Deliverables**:
  - CRD reference documentation with examples
  - Installation guide (kubectl apply / Helm chart)
  - Percona operator integration guide
  - Troubleshooting guide (common issues)
  - Deployment validation on Scaleway cluster
**Plans**: TBD

Plans:
- [ ] 06-01: CRD reference and installation documentation
- [ ] 06-02: Integration and troubleshooting guides
- [ ] 06-03: Scaleway cluster validation

## Progress

**Execution Order:**
Phases execute in numeric order: 1 -> 2 -> 3 -> 4 -> 5 -> 6

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Operator Foundation | 3/3 | ✓ Complete | 2026-02-04 |
| 2. ConfigMap and Services | 0/3 | Not started | - |
| 3. Storage Management | 0/3 | Not started | - |
| 4. Percona Integration | 0/3 | Not started | - |
| 5. Status and Lifecycle | 0/3 | Not started | - |
| 6. Documentation | 0/3 | Not started | - |

---
*Roadmap created: 2026-02-04*
*Milestone: v1.0*
