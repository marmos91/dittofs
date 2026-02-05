# Phase 5: Status Conditions and Lifecycle - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement operator observability and resource lifecycle management: status conditions for monitoring DittoServer health, Kubernetes events for debugging, health probes for pod management, and finalizers for clean resource deletion.

</domain>

<decisions>
## Implementation Decisions

### Status Conditions
- Show "Progressing" with reason when waiting for database initialization
- Include full replica counts: replicas, readyReplicas, availableReplicas (like Deployments)
- Custom kubectl columns: READY (e.g., 1/1), STATUS, AGE
- Include observedGeneration for detecting pending reconciliation
- Include configHash in status for debugging pod restarts
- Include perconaClusterName when Percona is enabled
- Include S3 validation status when S3 storage configured

### Event Emission
- Moderate verbosity: errors, major state changes, config changes, secret rotations, scaling events
- Include specific resource names in event messages (e.g., "ConfigMap ditto-config updated")
- Emit Warning events on webhook validation failures
- Emit events for Percona cluster state changes (ready, backup completed)

### Health Probes
- Use DittoFS's built-in liveness and readiness endpoints on port 8080
- Readiness probe should consider database connectivity
- Use startup probe for slow initial startup (database migrations)
- Sensible defaults for probe timing (no CRD configurability needed)
- Keep current init container with pg_isready loop for Percona (Phase 4 implementation)
- Add preStop hook that relies on DittoFS SIGTERM handling for connection draining

### Cleanup Behavior
- Configurable Percona deletion via `spec.percona.deleteWithServer` (default: false, orphans database)
- Use finalizer for pre-deletion cleanup
- Finalizer actions: emit deletion event AND wait for StatefulSet pods to terminate gracefully
- Keep current PVC retention policy (Retain/Retain from Phase 3)

### Claude's Discretion
- Exact status conditions to implement (2-5 conditions based on what's useful)
- Health endpoint paths (check DittoFS codebase for actual routes)
- Probe timing values (initialDelay, period, threshold)
- Finalizer timeout for waiting on pod termination
- Event message formatting

</decisions>

<specifics>
## Specific Ideas

- DittoFS already handles SIGTERM for graceful shutdown with connection draining
- DittoFS exposes health endpoints on the API port (8080)
- Percona database should be preserved by default when DittoServer is deleted (data protection)

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 05-status-lifecycle*
*Context gathered: 2026-02-05*
