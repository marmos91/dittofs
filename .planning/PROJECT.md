# DittoFS Kubernetes Operator

## What This Is

A production-ready Kubernetes operator that deploys and manages DittoFS instances on Kubernetes clusters. The operator handles the complete lifecycle: provisioning PostgreSQL (via Percona operator) for control plane persistence, creating ConfigMaps from CRD specs, managing PVCs for storage backends, and exposing services via LoadBalancer/Ingress for API, NFS, and SMB access.

## Core Value

Enable one-command DittoFS deployment on Kubernetes with full configurability of storage backends (memory, disk, S3, PostgreSQL) through a declarative CRD, while automating all infrastructure dependencies.

## Requirements

### Validated

- ✓ Existing DittoFS server binary with control plane API — existing
- ✓ NFSv3 and SMB2 protocol adapters — existing
- ✓ Support for memory, BadgerDB, PostgreSQL metadata stores — existing
- ✓ Support for memory, filesystem, S3 payload stores — existing
- ✓ Docker image available — existing

### Active

- [ ] Restructure operator to `k8s/dittofs-operator/` directory
- [ ] Update CRD to support new control plane API configuration
- [ ] CRD support for all metadata store types (memory, badger, postgres)
- [ ] CRD support for all payload store types (memory, filesystem/PVC, S3)
- [ ] Automatic PostgreSQL provisioning via Percona operator (for control plane and/or metadata)
- [ ] ConfigMap generation from CRD spec
- [ ] PVC management for BadgerDB and filesystem stores
- [ ] S3 credentials management (Secrets) for Cubbit DS3 / external S3
- [ ] Ingress controller documentation and setup guide
- [ ] LoadBalancer services for NFS (TCP) and SMB (TCP)
- [ ] Ingress resource for REST API with TLS support
- [ ] Operator deployment on Scaleway cluster (`dittofs-demo` context)
- [ ] End-to-end validation: deploy DittoFS via operator, mount NFS/SMB, verify operations
- [ ] CRD documentation with examples for all store combinations
- [ ] Operator installation documentation (Helm chart or kubectl apply)
- [ ] Proper error handling, logging, and metrics in operator
- [ ] Single replica enforcement (replicas: 1, HA is future scope)

### Out of Scope

- Multi-replica / High Availability support — future project, current focus is single replica correctness
- MinIO deployment orchestration — using external S3 (Cubbit DS3)
- Custom ingress controller development — will use nginx-ingress
- Automated backup scheduling — manual backup via CLI for now
- Multi-cluster federation — single cluster deployment

## Context

**Existing Operator State:**
- Located at `./dittofs-operator/` (needs move to `k8s/dittofs-operator/`)
- Built with Operator SDK (Go)
- Has basic CRD (`DittoServer`) but needs updating for new control plane API
- Current implementation doesn't support PostgreSQL orchestration

**Target Environment:**
- Scaleway Kubernetes cluster
- Context: `dittofs-demo`
- Kubeconfig already configured

**External Dependencies:**
- Percona PostgreSQL operator for database provisioning
- nginx-ingress controller for HTTP ingress
- Cubbit DS3 for S3-compatible object storage (default)
- Scaleway LoadBalancer for TCP services (NFS, SMB)

**Protocol Exposure Research Needed:**
- NFS uses TCP port 2049 (configurable in DittoFS to 12049)
- SMB uses TCP port 445 (configurable in DittoFS to 12445)
- Neither is HTTP-based, so standard Ingress won't work
- Options: LoadBalancer per protocol, NodePort, or TCP Ingress via nginx

## Constraints

- **Replicas**: Single replica only (replicas: 1) — HA is future scope
- **Operator Framework**: Go with Operator SDK — existing choice, maintain consistency
- **PostgreSQL**: Percona operator for PostgreSQL provisioning
- **S3 Backend**: External S3 (Cubbit DS3 default) — no MinIO deployment
- **Cluster**: Scaleway Kubernetes, context `dittofs-demo`
- **Image Source**: Docker Registry (may use local registry during development)

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Move operator to k8s/dittofs-operator/ | Better organization, standard k8s convention | — Pending |
| Use Percona operator for PostgreSQL | Managed PostgreSQL with proper lifecycle handling | — Pending |
| LoadBalancer for NFS/SMB | TCP protocols can't use HTTP Ingress | — Pending |
| Single replica enforcement | Simplify initial deployment, defer HA complexity | — Pending |
| ConfigMap for DittoFS config | Standard K8s pattern, allows hot reload | — Pending |

---
*Last updated: 2026-02-04 after initialization*
