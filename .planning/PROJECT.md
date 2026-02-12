# DittoFS K8s Auto-Adapters

## What This Is

Dynamic adapter port management for DittoFS on Kubernetes. The K8s operator automatically discovers which protocol adapters (NFS, SMB) are active on the control plane and creates/removes the corresponding K8s resources (LoadBalancer Services, container ports, NetworkPolicies) so that only running adapters are externally exposed. This also includes removing static adapter configuration from both the DittoFS YAML config and the CRD, making the control plane API the single source of truth for adapter management.

## Core Value

The operator ensures that protocol adapters are only externally accessible when they are actually running, reducing the attack surface and making adapter lifecycle fully dynamic — no manual K8s resource management needed.

## Requirements

### Validated

(None yet — ship to validate)

### Active

- [ ] Remove adapter configuration section from DittoFS YAML config file
- [ ] Remove `nfsPort` and `smb` spec fields from the DittoServer CRD
- [ ] Add "operator" role to DittoFS with read-only adapter access (least privilege)
- [ ] Operator auto-creates a DittoFS service account with operator role on startup
- [ ] Operator polls `GET /api/v1/adapters` at a configurable interval (default 30s)
- [ ] For each enabled+running adapter, operator creates a dedicated LoadBalancer Service with the adapter's port
- [ ] For disabled/removed adapters, operator deletes the corresponding LoadBalancer Service
- [ ] Operator updates StatefulSet container ports to match active adapters
- [ ] Operator manages NetworkPolicy rules to allow traffic only to active adapter ports
- [ ] Polling interval is configurable via CRD spec
- [ ] Operator handles DittoFS restart gracefully (re-polls and reconciles after readiness)
- [ ] One LoadBalancer per adapter (NFS and SMB get separate external IPs)

### Out of Scope

- Webhook/event-driven adapter discovery — polling is sufficient for now
- Ingress resources — NFS/SMB are TCP protocols, not HTTP
- Multi-replica DittoFS — still single-replica (0 or 1)
- Building a DittoFS webhook/event system
- Changes to the adapter API response format (already returns port, enabled, running)

## Context

**Existing operator:** Go operator using controller-runtime at `k8s/dittofs-operator/`. Currently manages StatefulSet, Services, ConfigMap, Secrets, and optional Percona PostgreSQL.

**Current CRD adapter fields (to be removed):**
- `spec.nfsPort` — static NFS port (default 12049)
- `spec.smb` — full SMB adapter spec (enabled, port, timeouts, credits, etc.)

**Current adapter API (already sufficient):**
- `GET /api/v1/adapters` returns `[{type, port, enabled, running, config}]`
- `POST /api/v1/adapters` creates adapter
- `PUT /api/v1/adapters/{type}` updates adapter
- `DELETE /api/v1/adapters/{type}` deletes adapter
- All endpoints require admin auth (`RequireAdmin()` middleware)

**Default adapters:** DittoFS creates NFS (port 12049) and SMB (port 1445) on first boot via `EnsureDefaultAdapters()`. Both enabled by default.

**Operator service architecture:**
- Currently creates: headless service, file service (NFS+SMB), API service, metrics service
- File service statically includes NFS port + SMB port (if enabled in CRD)
- New: per-adapter LoadBalancer Services created dynamically based on API state

## Constraints

- **Backward compatibility**: Existing DittoFS deployments using static adapter config must still work during migration
- **Auth model**: Operator needs a new "operator" role — not full admin, just adapter read access
- **K8s resource ownership**: Dynamically created Services must be owned by the DittoServer CR for proper cleanup
- **Port source**: Adapter ports come from the API response, not hardcoded in operator
- **Service type**: LoadBalancer for production exposure (configurable to NodePort for dev)

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Polling over webhooks | No webhook system exists; polling is simpler and sufficient | — Pending |
| One LoadBalancer per adapter | Clean separation, independent IPs for NFS and SMB | — Pending |
| Remove adapter config from YAML + CRD | Control plane API is single source of truth | — Pending |
| New "operator" role (not admin) | Least privilege — operator only needs to read adapter state | — Pending |
| Auto-create service account | Operator self-provisions when running in K8s, no setup needed outside K8s | — Pending |
| Configurable poll interval | Default 30s, adjustable via CRD for different environments | — Pending |

---
*Last updated: 2026-02-09 after initialization*
