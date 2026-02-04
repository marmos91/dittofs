# Phase 2: ConfigMap Generation and Services - Context

**Gathered:** 2026-02-04
**Status:** Ready for planning

<domain>
## Phase Boundary

Generate Kubernetes ConfigMap from CRD spec that matches DittoFS config format; create LoadBalancer Services for NFS, SMB, and API; implement checksum annotation for automatic pod restart on configuration changes.

Store configurations, shares, adapters, and users are managed via DittoFS REST APIs — NOT in the CRD or ConfigMap. The CRD focuses on infrastructure: database, cache, API access, and Kubernetes deployment settings.

</domain>

<decisions>
## Implementation Decisions

### ConfigMap Structure
- **CRD scope is minimal**: Only database, cache, API, and admin configuration
- Store configurations (metadata, payload) removed from CRD — managed via APIs
- Shares and adapters managed via APIs, not config file
- **Database configuration**: Both SQLite and Postgres options in CRD (mutually exclusive)
  - If both specified, Postgres takes precedence silently
  - SQLite: PVC path; Postgres: Secret reference for connection string
- **Cache configuration**: Sensible defaults (1GB size); user can override path/size in CRD
- **Logging/telemetry**: Hardcoded sensible defaults in ConfigMap; override via environment variables
- **Metrics**: Disabled by default; user enables via CRD field
- **API server**: Enabled by default, port 8080
  - JWT secret from user-provided Secret reference (required)
  - Admin credentials from user-provided Secret reference (required)
- **Filesystem volumes**: VolumeClaimTemplate approach for optional filesystem store PVCs
- ConfigMap matches DittoFS config format on develop branch

### Service Topology
- **Two LoadBalancer Services**: One for file protocols (NFS+SMB), one for API
- **Headless Service**: Created for StatefulSet DNS requirement (required by spec)
- **Service type**: LoadBalancer default with CRD override (ClusterIP, NodePort)
- **Same type for both**: File Service and API Service use same serviceType from CRD
- **Annotations pass-through**: CRD field for LoadBalancer annotations (cloud-specific)
- **Service naming**: Auto-generated from CR name
  - `{cr-name}-file` for NFS+SMB
  - `{cr-name}-api` for API
  - `{cr-name}-headless` for StatefulSet DNS
- **Metrics Service**: Created conditionally only when metrics enabled in CRD

### Config Change Detection
- **Checksum annotation pattern**: SHA256 hash in pod template annotation `dittofs.io/config-hash`
- **Hash scope includes**:
  - ConfigMap content
  - Referenced Secrets (JWT secret, admin password, database credentials)
  - CRD metadata.generation for extra safety
- **Always compute**: Every reconcile computes hash and sets annotation (idempotent)
- Annotation change triggers StatefulSet rolling update via Kubernetes native mechanism

### Port Configuration
- **Default ports**: NFS 2049, SMB 445, API 8080
- **Configurable in CRD**: All protocol ports can be overridden
- **Port mapping**: Service port = container port (always match, simpler)
- **Validation webhook**: Rejects invalid port combinations
  - Range validation: 1-65535
  - Uniqueness validation: No duplicate ports across protocols
- **Privileged port warning**: Ports < 1024 accepted but warning added to CR status

### Kubernetes Deployment Settings (in CRD)
- Image (registry/tag) configurable
- Replicas field (default 1)
- Resource limits (CPU/memory requests and limits)

### Claude's Discretion
- Exact default cache size (recommend 1GB)
- Logging format default (JSON for Kubernetes)
- StatefulSet update strategy details
- Internal error message wording

</decisions>

<specifics>
## Specific Ideas

- ConfigMap must match the new DittoFS config format on develop branch exactly
- Operator should generate minimal config — only infrastructure settings
- "Dynamic" configuration (stores, shares, users, adapters) managed via REST API at runtime
- User creates Secrets for JWT and admin credentials before deploying CR

</specifics>

<deferred>
## Deferred Ideas

- **Ingress support** — Add optional Ingress for API Service (future phase)
- **TLS termination** — HTTPS support for API (handled via Ingress/Gateway, not operator)
- **Pod Disruption Budget** — May add in Phase 5 (Status and Lifecycle)

</deferred>

---

*Phase: 02-configmap-services*
*Context gathered: 2026-02-04*
