# Roadmap: DittoFS K8s Auto-Adapters

## Overview

Transform the DittoFS K8s operator from static adapter configuration to dynamic, API-driven adapter management. The operator will authenticate against the DittoFS control plane, discover active adapters via polling, and create/destroy K8s resources (Services, NetworkPolicies, container ports) to match runtime adapter state. This eliminates manual K8s resource management and reduces attack surface by only exposing running adapters.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3, 4): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

- [ ] **Phase 1: Auth Foundation** - Operator can authenticate to DittoFS API with least-privilege access
- [ ] **Phase 2: Adapter Discovery** - Operator discovers active adapters by polling the DittoFS API
- [ ] **Phase 3: Dynamic Services & Ports** - Operator creates/removes K8s Services and updates container ports based on adapter state
- [ ] **Phase 4: Security Hardening** - Static adapter config removed from CRD and NetworkPolicies enforce per-adapter traffic rules

## Phase Details

### Phase 1: Auth Foundation
**Goal**: Operator can securely authenticate to the DittoFS control plane API using a dedicated least-privilege role
**Depends on**: Nothing (first phase)
**Requirements**: AUTH-01, AUTH-02, AUTH-03, AUTH-04, AUTH-05
**Success Criteria** (what must be TRUE):
  1. DittoFS server accepts API requests from a user with the "operator" role and returns adapter list data, but rejects mutations (create/update/delete adapters)
  2. When the operator starts and DittoFS is ready, a service account with operator role exists and its JWT is stored in a K8s Secret
  3. When the DittoFS API is unreachable, the operator logs warnings and retries with backoff without crashing or deleting existing K8s resources
  4. The operator's JWT token is refreshed automatically before expiry without requiring a pod restart
**Plans**: TBD

Plans:
- [ ] 01-01: Implement operator role and service account provisioning
- [ ] 01-02: Implement credential storage and token lifecycle

### Phase 2: Adapter Discovery
**Goal**: Operator reliably discovers the current state of all protocol adapters by polling the DittoFS API
**Depends on**: Phase 1
**Requirements**: DISC-01, DISC-02, DISC-03
**Success Criteria** (what must be TRUE):
  1. Operator polls the adapter list endpoint at the interval specified in the CRD spec (defaulting to 30s)
  2. Changing the polling interval in the CRD spec takes effect without restarting the operator
  3. When the API returns an error or empty response, the operator preserves all existing adapter Services and does not delete or modify them
**Plans**: TBD

Plans:
- [ ] 02-01: Implement adapter polling loop with configurable interval and safety guards

### Phase 3: Dynamic Services & Ports
**Goal**: K8s Services and StatefulSet container ports automatically reflect the set of running adapters
**Depends on**: Phase 2
**Requirements**: SRVC-01, SRVC-02, SRVC-03, SRVC-04, SRVC-05, SRVC-06, SRVC-07, SRVC-08
**Success Criteria** (what must be TRUE):
  1. When an adapter is enabled and running, a dedicated LoadBalancer Service exists exposing that adapter's port (as reported by the API)
  2. When an adapter is stopped or deleted via the DittoFS API, the corresponding LoadBalancer Service is removed within one polling cycle
  3. Adapter Services are owned by the DittoServer CR and are automatically garbage-collected when the CR is deleted
  4. StatefulSet container ports match the set of active adapters (added when adapter starts, removed when adapter stops)
  5. Adapter Service type (LoadBalancer/NodePort/ClusterIP) and custom annotations are configurable via CRD spec, and K8s events are emitted for service lifecycle changes
**Plans**: TBD

Plans:
- [ ] 03-01: Implement per-adapter Service lifecycle management
- [ ] 03-02: Implement StatefulSet port reconciliation, service configurability, and K8s events

### Phase 4: Security Hardening
**Goal**: Static adapter configuration is fully removed and network access is restricted to only active adapter ports
**Depends on**: Phase 3
**Requirements**: SECU-01, SECU-02, SECU-03, SECU-04
**Success Criteria** (what must be TRUE):
  1. The DittoServer CRD no longer has `spec.nfsPort` or `spec.smb` fields, and the operator no longer generates adapter sections in the DittoFS YAML config
  2. For each running adapter, a NetworkPolicy exists allowing ingress traffic only on that adapter's port
  3. When an adapter is stopped or removed, its NetworkPolicy is deleted within one polling cycle, blocking traffic to that port
**Plans**: TBD

Plans:
- [ ] 04-01: Remove static adapter fields from CRD and config generation
- [ ] 04-02: Implement per-adapter NetworkPolicy lifecycle management

## Progress

**Execution Order:**
Phases execute in numeric order: 1 -> 2 -> 3 -> 4

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Auth Foundation | 0/2 | Not started | - |
| 2. Adapter Discovery | 0/1 | Not started | - |
| 3. Dynamic Services & Ports | 0/2 | Not started | - |
| 4. Security Hardening | 0/2 | Not started | - |
