# Requirements: DittoFS Kubernetes Operator v1.0

## Core Requirements

### Operator Foundation
- [x] **R1.1** Operator scaffold with Operator SDK v1.42.0 and Go 1.24+
- [x] **R1.2** DittoFS CRD (v1alpha1) with complete spec schema
- [x] **R1.3** Basic controller reconciliation loop
- [x] **R1.4** RBAC (ServiceAccount, Role, RoleBinding) for operator
- [x] **R1.5** Operator deployed to `k8s/dittofs-operator/` directory structure

### Configuration & Services
- [x] **R2.1** ConfigMap generation from CRD spec (all DittoFS config options)
- [x] **R2.2** Checksum annotation pattern for pod restart on config change
- [x] **R2.3** LoadBalancer Service for NFS (port 2049, configurable)
- [x] **R2.4** LoadBalancer Service for SMB (port 445, configurable)
- [x] **R2.5** LoadBalancer/Ingress for REST API (port 8080)
- [x] **R2.6** NodePort fallback when LoadBalancer unavailable

### Storage Management
- [x] **R3.1** PVC for metadata store (BadgerDB) via volumeClaimTemplate
- [x] **R3.2** PVC for payload store (filesystem) via volumeClaimTemplate
- [x] **R3.3** PVC for cache (WAL persistence) via volumeClaimTemplate
- [x] **R3.4** Memory store configuration (no PVC)
- [x] **R3.5** S3 store configuration (Cubbit DS3 credentials via Secret)
- [x] **R3.6** StorageClass validation before PVC creation

### PostgreSQL Integration
- [x] **R4.1** Percona PostgreSQL operator CRD watching
- [x] **R4.2** Connection Secret extraction from PerconaPGCluster
- [x] **R4.3** Init container waiting for PostgreSQL readiness
- [x] **R4.4** DATABASE_URL environment variable injected from Percona Secret
- [x] **R4.5** Sample CR with Percona prerequisites documented

### Status & Lifecycle
- [ ] **R5.1** Status conditions: Ready, Available, Degraded, DatabaseReady, ConfigReady
- [ ] **R5.2** Finalizers for clean resource cleanup
- [ ] **R5.3** Kubernetes events for debugging
- [ ] **R5.4** Health probes (liveness, readiness) configuration
- [ ] **R5.5** Graceful shutdown handling

### Documentation & Deployment
- [ ] **R6.1** CRD reference documentation with examples
- [ ] **R6.2** Installation guide (kubectl apply / Helm chart)
- [ ] **R6.3** Percona operator integration guide
- [ ] **R6.4** Troubleshooting guide (common issues)
- [ ] **R6.5** Deployment validation on Scaleway cluster (dittofs-demo context)

## Constraints

| Constraint | Rationale |
|------------|-----------|
| Single replica only | HA is future scope; simplifies initial implementation |
| Operator SDK v1.42.0 | Latest stable, provides OLM integration |
| Go 1.24+ | Required by Operator SDK v1.41+ |
| Percona for PostgreSQL | Managed lifecycle, production-ready |
| External S3 (Cubbit DS3) | No MinIO deployment complexity |
| Scaleway Kubernetes | Target environment, LoadBalancer support |

## Out of Scope (v1.0)

- Multi-replica / High Availability support
- DittoFSShare CRD (multi-share via separate CRD)
- DittoFSBackup CRD for backup workflows
- Horizontal Pod Autoscaling
- OLM / OperatorHub publication
- cert-manager TLS integration
- External Secrets Operator integration

## Success Criteria

1. `kubectl apply -f dittofs.yaml` deploys functional DittoFS instance
2. NFS mount succeeds: `mount -t nfs -o port=2049 <LB-IP>:/export /mnt`
3. SMB mount succeeds: `mount -t cifs //<LB-IP>/export /mnt`
4. REST API accessible via Ingress/LoadBalancer
5. PostgreSQL metadata store works via Percona operator
6. All store combinations functional: memory, badger, filesystem, s3, postgres
7. Config changes via CRD trigger pod restart
8. Clean deletion via `kubectl delete dittofs <name>`

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| R1.1 | Phase 1 | Complete |
| R1.2 | Phase 1 | Complete |
| R1.3 | Phase 1 | Complete |
| R1.4 | Phase 1 | Complete |
| R1.5 | Phase 1 | Complete |
| R2.1 | Phase 2 | Complete |
| R2.2 | Phase 2 | Complete |
| R2.3 | Phase 2 | Complete |
| R2.4 | Phase 2 | Complete |
| R2.5 | Phase 2 | Complete |
| R2.6 | Phase 2 | Complete |
| R3.1 | Phase 3 | Complete |
| R3.2 | Phase 3 | Complete |
| R3.3 | Phase 3 | Complete |
| R3.4 | Phase 3 | Complete |
| R3.5 | Phase 3 | Complete |
| R3.6 | Phase 3 | Complete |
| R4.1 | Phase 4 | Complete |
| R4.2 | Phase 4 | Complete |
| R4.3 | Phase 4 | Complete |
| R4.4 | Phase 4 | Complete |
| R4.5 | Phase 4 | Complete |
| R5.1 | Phase 5 | Pending |
| R5.2 | Phase 5 | Pending |
| R5.3 | Phase 5 | Pending |
| R5.4 | Phase 5 | Pending |
| R5.5 | Phase 5 | Pending |
| R6.1 | Phase 6 | Pending |
| R6.2 | Phase 6 | Pending |
| R6.3 | Phase 6 | Pending |
| R6.4 | Phase 6 | Pending |
| R6.5 | Phase 6 | Pending |

---
*Requirements defined: 2026-02-04*
*Milestone: v1.0*
