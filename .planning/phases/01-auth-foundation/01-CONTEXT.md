# Phase 1: Auth Foundation - Context

**Gathered:** 2026-02-10
**Status:** Ready for planning

<domain>
## Phase Boundary

Operator can securely authenticate to the DittoFS control plane API using a dedicated least-privilege "operator" role. This includes: adding the operator role to DittoFS, auto-provisioning a service account, storing credentials in K8s, and handling token lifecycle. Adapter discovery and service management are separate phases.

</domain>

<decisions>
## Implementation Decisions

### Service Account Provisioning
- Operator auto-creates a DittoFS service account with a fixed username (e.g., `k8s-operator`) on startup
- Admin credentials for bootstrap: Claude's discretion on how to source them (K8s Secret ref or CRD field)
- Password is auto-generated (strong random) by the operator, not user-provided
- If the service account already exists: reuse it, log in with stored credentials. Do not recreate or rotate.
- If provisioning fails on first boot (DittoFS not ready): block and retry with backoff. Operator does not become ready until authenticated.
- User creation API already exists in DittoFS — operator calls it to create the service account
- On DittoServer CR deletion: operator deletes the DittoFS service account as part of teardown

### Credential Storage
- K8s Secret named with fixed convention: `{cr-name}-operator-credentials`
- Secret has ownerReference to the DittoServer CR — garbage collected on CR deletion
- Secret stores both the JWT token and the auto-generated password
- Storing the password enables re-login if the JWT becomes invalid (e.g., after DittoFS server restart)

### Token Lifecycle
- Token refresh strategy: Claude's discretion (proactive at ~80% TTL or reactive on 401, or both)
- If stored JWT is invalid on operator restart: re-login with stored password to get a fresh JWT
- Updated JWT is written back to the K8s Secret

### Resilience & Recovery
- When DittoFS API is unreachable during normal operation: Claude's discretion on retry strategy (exponential backoff recommended)
- Never delete existing K8s resources when API is unreachable — preserve all state
- DittoServer CR gets an `Authenticated` status condition reflecting auth health
- Operator readiness probe is auth-aware: not-ready until authentication succeeds

### Operator Role Design
- "operator" role is a built-in role in DittoFS, hardcoded in the server (like admin, user)
- Role scope: strictly GET /api/v1/adapters only — absolute minimum privilege
- Role enforcement: central authorization middleware (fail-closed), not per-handler checks
- DittoFS already has a role system — operator is a new role added to the existing system

### Claude's Discretion
- Admin credential bootstrapping mechanism (K8s Secret ref vs CRD spec field)
- Token refresh strategy details (proactive, reactive, or hybrid)
- Retry/backoff intervals and caps for API unavailability
- K8s event emission on provisioning failures (events + logs vs logs only)
- Exact error handling and logging levels

</decisions>

<specifics>
## Specific Ideas

- Block-and-retry on first boot aligns with K8s reconcile loop pattern — requeue until provisioning succeeds
- Readiness probe should reflect auth state so that dependent workloads don't route to an unauthenticated operator
- Middleware enforcement for the operator role is fail-closed: any new endpoints are denied by default

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-auth-foundation*
*Context gathered: 2026-02-10*
