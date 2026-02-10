# Requirements: DittoFS K8s Auto-Adapters

**Defined:** 2026-02-09
**Core Value:** The operator ensures protocol adapters are only externally accessible when running, reducing attack surface and making adapter lifecycle fully dynamic.

## v1 Requirements

Requirements for initial release. Each maps to roadmap phases.

### Authentication & Authorization

- [ ] **AUTH-01**: DittoFS has a new "operator" role with read-only access to adapter list endpoint
- [ ] **AUTH-02**: Operator auto-creates a DittoFS service account with operator role on startup if it doesn't exist
- [ ] **AUTH-03**: Operator stores service account JWT credentials in a K8s Secret
- [ ] **AUTH-04**: Operator handles DittoFS API unavailability gracefully -- preserves existing Services, logs warning, requeues with backoff
- [ ] **AUTH-05**: Operator automatically refreshes JWT tokens before expiry without requiring restart

### Adapter Discovery

- [ ] **DISC-01**: Operator polls `GET /api/v1/adapters` at a configurable interval (default 30s)
- [ ] **DISC-02**: Polling interval is configurable via CRD spec field
- [ ] **DISC-03**: Operator only acts on successful API responses -- never deletes Services based on failed/empty responses

### Service Management

- [ ] **SRVC-01**: Operator creates a dedicated LoadBalancer Service for each enabled+running adapter
- [ ] **SRVC-02**: Each adapter Service uses the port returned by the adapter API response
- [ ] **SRVC-03**: Operator deletes a LoadBalancer Service when its corresponding adapter is stopped or removed
- [ ] **SRVC-04**: Adapter Services have owner references to the DittoServer CR for automatic cleanup
- [ ] **SRVC-05**: Operator updates StatefulSet container ports to match active adapters
- [ ] **SRVC-06**: Adapter Services support configurable type (LoadBalancer, NodePort, ClusterIP) via CRD spec
- [ ] **SRVC-07**: Adapter Services support custom annotations via CRD spec (for cloud LB configuration)
- [ ] **SRVC-08**: Operator emits K8s events for adapter Service lifecycle changes (created, deleted, updated)

### Security

- [ ] **SECU-01**: Static `spec.nfsPort` and `spec.smb` fields removed from DittoServer CRD
- [ ] **SECU-02**: Operator no longer emits adapter configuration in generated DittoFS YAML config
- [ ] **SECU-03**: Operator creates a NetworkPolicy per active adapter allowing ingress only on the adapter's port
- [ ] **SECU-04**: Operator deletes NetworkPolicy when corresponding adapter is stopped or removed

## v2 Requirements

Deferred to future release. Tracked but not in current roadmap.

### Observability

- **OBSV-01**: Adapter status (type, port, running, endpoint) visible in CRD status field
- **OBSV-02**: AdaptersReady condition aggregated into Ready condition
- **OBSV-03**: Exponential backoff on consecutive API polling failures (cap at 5 minutes)

### Advanced Configuration

- **CONF-01**: Configurable service naming template (e.g., `{instance}-{adapter}`)
- **CONF-02**: Per-adapter service annotation overrides

## Out of Scope

| Feature | Reason |
|---------|--------|
| Webhook/event-driven adapter discovery | No webhook system exists in DittoFS; polling is sufficient |
| Operator-initiated adapter creation/deletion | Operator is a consumer of adapter state, not a producer |
| Ingress resources for adapters | NFS/SMB are TCP protocols, not HTTP |
| Multi-replica adapter awareness | DittoFS is single-replica (0 or 1) |
| Per-adapter resource limits | Adapters share a process; use server-side rate limiting |
| Adapter config management via CRD | API is the single source of truth |
| Service mesh integration | Raw TCP doesn't benefit from HTTP-level mesh features |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| AUTH-01 | Phase 1 | Pending |
| AUTH-02 | Phase 1 | Pending |
| AUTH-03 | Phase 1 | Pending |
| AUTH-04 | Phase 1 | Pending |
| AUTH-05 | Phase 1 | Pending |
| DISC-01 | Phase 2 | Pending |
| DISC-02 | Phase 2 | Pending |
| DISC-03 | Phase 2 | Pending |
| SRVC-01 | Phase 3 | Pending |
| SRVC-02 | Phase 3 | Pending |
| SRVC-03 | Phase 3 | Pending |
| SRVC-04 | Phase 3 | Pending |
| SRVC-05 | Phase 3 | Pending |
| SRVC-06 | Phase 3 | Pending |
| SRVC-07 | Phase 3 | Pending |
| SRVC-08 | Phase 3 | Pending |
| SECU-01 | Phase 4 | Pending |
| SECU-02 | Phase 4 | Pending |
| SECU-03 | Phase 4 | Pending |
| SECU-04 | Phase 4 | Pending |

**Coverage:**
- v1 requirements: 20 total
- Mapped to phases: 20
- Unmapped: 0

---
*Requirements defined: 2026-02-09*
*Last updated: 2026-02-10 after roadmap creation*
