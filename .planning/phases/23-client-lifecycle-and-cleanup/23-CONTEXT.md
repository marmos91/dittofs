# Phase 23: Client Lifecycle and Cleanup - Context

**Gathered:** 2026-02-22
**Status:** Ready for planning

<domain>
## Phase Boundary

Server supports full client lifecycle management for NFSv4.1: graceful client teardown (DESTROY_CLIENTID), grace period completion signaling (RECLAIM_COMPLETE), individual stateid lifecycle management (FREE_STATEID, TEST_STATEID), and rejection of v4.0-only operations for minorversion=1 compounds. K8s operator updates for grace period awareness are included.

</domain>

<decisions>
## Implementation Decisions

### DESTROY_CLIENTID behavior
- **Strict RFC compliance**: Reject with NFS4ERR_CLIENTID_BUSY if any sessions remain — client must destroy all sessions first
- State purging approach: Claude's discretion (sync vs async teardown)
- Delegation handling on destroy: Claude's discretion (immediate revoke vs recall-then-revoke)
- Lock/open release on destroy: Claude's discretion (immediate vs brief hold)
- Idempotency behavior: Claude's discretion (based on RFC requirements)
- **No API exposure**: Client destroy events are NOT exposed via REST API — logs only
- **Structured logging**: Log destroy events with client ID, session count, state count — consistent with Phase 21 observability

### Grace period (RECLAIM_COMPLETE)
- **Fixed 90-second grace period** — RFC default, not configurable
- Per-client vs global unlock after RECLAIM_COMPLETE: Claude's discretion (RFC semantics)
- Behavior when client doesn't complete reclaim: Claude's discretion (based on RFC guidance)
- Grace state persistence: Claude's discretion (in-memory vs persisted)
- Auto-end when all clients reclaim: Claude's discretion
- **Grace period visible in health endpoint AND `dfs status`**
- **`dfs status` shows countdown**: "Grace period: 47s remaining (3/5 clients reclaimed)" format
- **Admin API to force-end grace period** — REST endpoint for fast recovery in dev/test
- **`dfsctl grace status` and `dfsctl grace end` commands** — CLI wrappers for the admin API
- **Structured logging**: Log RECLAIM_COMPLETE events with client ID, reclaim duration, number of states reclaimed
- Health endpoint status during grace: Claude's discretion (healthy vs degraded, considering k8s readiness probe implications)

### Stateid lifecycle (FREE_STATEID / TEST_STATEID)
- **TEST_STATEID returns per-stateid error codes** in the result array — RFC 5661 compliance, not fail-on-first
- Freeable stateid types: Claude's discretion (based on RFC requirements)
- Cascading behavior (lock → open): Claude's discretion (likely lock-only per RFC)
- **FREE_STATEID does NOT trigger cache flush** — trust existing COMMIT/cache/WAL flow for data safety
- Seqid validation depth for TEST_STATEID: Claude's discretion (based on RFC)
- Batch size limits for TEST_STATEID: Claude's discretion
- Special stateid handling for FREE_STATEID: Claude's discretion (based on RFC)
- **Structured logging** for both operations with stateid details, client ID, and result

### v4.0 operation rejection
- Scope of rejected operations: Claude's discretion (check RFC 5661 for complete list beyond the 5 listed)
- Dispatch integration point: Claude's discretion (compound-level vs per-handler)
- **Debug-level logging** for v4.0 rejections — not noisy in production
- Stub/catch-all implementation: Claude's discretion (based on existing dispatch patterns)
- Testing approach: Claude's discretion (dedicated table-driven test vs integration)
- Log format (op name vs code): Claude's discretion
- Minorversion=0 compound handling: Claude's discretion (verify existing behavior)

### Code structure and design
- **Per-operation handler files**: `destroy_clientid.go`, `reclaim_complete.go`, `free_stateid.go`, `test_stateid.go` — matches existing one-file-per-handler convention
- **State logic in existing `state/` package**: Extend `state/client.go`, `state/grace.go`, `state/stateid.go` rather than creating new sub-packages
- **Extend existing StateManager methods** rather than adding new ones where possible
- **Verify existing XDR types** before implementing handlers — fix gaps if any
- Implementation order: Claude's discretion (based on dependencies)
- API route structure for grace endpoints: Claude's discretion
- **K8s operator updates**: Claude's discretion on what needs changing (readiness probes, grace awareness)

### Testing approach
- **No mocks** — test against real in-memory StateManager with real state setup
- Handler tests alongside handlers, state tests in `state/` — follow existing convention
- Testing level (handler-level vs full dispatch): Claude's discretion based on existing test patterns
- **Race condition tests required** — concurrent DESTROY_CLIENTID and FREE_STATEID tests with `-race` flag

### Claude's Discretion
- Sync vs async state purging on DESTROY_CLIENTID
- Delegation recall vs immediate revoke
- Lock release timing
- DESTROY_CLIENTID idempotency
- Prometheus metrics for lifecycle operations
- Per-client grace unlock semantics
- Grace period state persistence strategy
- Auto-end grace when all clients reclaim
- Health endpoint degraded vs healthy during grace
- Freeable stateid types, cascading behavior, batch limits, special stateid handling
- v4.0 rejection scope, dispatch point, and testing approach
- Implementation order
- K8s operator integration specifics

</decisions>

<specifics>
## Specific Ideas

- Grace period countdown in `dfs status` should show: "Grace period: 47s remaining (3/5 clients reclaimed)"
- `dfsctl grace status` and `dfsctl grace end` for operational control
- Admin API for force-ending grace period — useful for fast recovery in dev/test environments
- Structured logging should match Phase 21 observability patterns (connection lifecycle events)
- Race tests specifically for concurrent destroy/free operations

</specifics>

<deferred>
## Deferred Ideas

- K8s operator grace-aware rolling updates — evaluate during operator sync, may be out of scope for this phase

</deferred>

---

*Phase: 23-client-lifecycle-and-cleanup*
*Context gathered: 2026-02-22*
