# Phase 65: Offline Read/Write Paths - Context

**Gathered:** 2026-03-16
**Status:** Ready for planning

<domain>
## Phase Boundary

NFS/SMB clients can continue reading cached files and writing new data when S3 is unreachable. Reads of locally-cached blocks succeed normally; reads of remote-only blocks return a clear, descriptive error. Writes go to local store and queue for sync on reconnect. The health monitoring infrastructure (Phase 64) provides the IsRemoteHealthy() signal; this phase makes the read/write paths respond to it. Edge test infrastructure is Phase 66.

</domain>

<decisions>
## Implementation Decisions

### Read Path Degradation
- **Partial reads supported**: When a READ spans both locally-cached and remote-only blocks, serve the cached portion and return an I/O error only for blocks that need S3. NFS clients handle partial reads gracefully.
- **Check health before fetch**: Before attempting remote download in `EnsureAvailableAndRead` and `fetchBlock`, check `IsRemoteHealthy()`. If unhealthy, immediately return `ErrRemoteUnavailable` instead of waiting for a network timeout.
- **Transparent retry on recovery**: NFS clients automatically retry failed READs. Once S3 is healthy again, retries succeed transparently. No file handle state to clean up.
- **COW reads follow same pattern**: Copy-on-write source reads degrade the same way — serve from local cache if available, error if remote-only blocks are needed and S3 is down.
- **Suppress prefetch when unhealthy**: Skip `enqueuePrefetch()` entirely when S3 is unhealthy. Prefetching remote blocks during an outage would queue failures. Local-only prefetch (L1 cache) continues.
- **Reject download enqueues immediately**: Check `IsRemoteHealthy()` before enqueuing downloads in `enqueueDownload`. Return `ErrRemoteUnavailable` immediately. Avoids filling the queue with doomed requests.
- **Exists() and GetSize() also check health**: If local cache has the file, return immediately. If not and S3 is unhealthy, return `ErrRemoteUnavailable`. Consistent with read path.
- **DEBUG-level logging**: Log at DEBUG level when reads are served from local cache during outage. No dedicated counter for offline reads served (Phase 64's `remote_healthy=false` in CacheStats is sufficient for that signal).

### Write Path Offline Behavior
- **No extra changes needed**: The existing architecture already handles offline writes: `local.WriteAt` succeeds, syncer queues blocks, circuit breaker (Phase 64) pauses uploads. Just verify this works and add test coverage.
- **ENOSPC when disk fills during outage**: Same as pin mode behavior (Phase 63). Return NFS3ERR_NOSPC / STATUS_DISK_FULL. No emergency eviction of un-re-downloadable blocks. Already decided in Phase 64.
- **Flush succeeds locally**: `Flush()` (NFS COMMIT) persists dirty blocks to local disk (already works). S3 upload is async via syncer. COMMIT returns success because local persistence is durable.
- **Truncate and Delete work offline**: Local state modified immediately. Remote cleanup (S3 block deletion) deferred via GC. During outage, GC is suspended (Phase 64).
- **Transparent backlog handling**: No write-side awareness of sync backlog. Writes continue normally. Backlog drains at normal rate on recovery. Already decided in Phase 64.

### Error Reporting to Clients
- **New sentinel error**: Add `blockstore.ErrRemoteUnavailable` sentinel error. Protocol handlers check `errors.Is()` to map to correct NFS/SMB codes. Clear separation between "remote down" and "actual I/O failure".
- **Error includes outage duration**: Wrap `ErrRemoteUnavailable` with context: `"remote store unavailable (offline for Xm Ys)"`. Helps operators correlate log entries with outage timeline.
- **NFS error code**: `NFS3ERR_IO` / `NFS4ERR_IO`. Standard I/O error that NFS clients understand and will retry.
- **SMB status code**: `STATUS_IO_DEVICE_ERROR` (0xC0000185). Standard SMB I/O error. Windows clients show "The request could not be performed because of an I/O device error."
- **Explicit error mapping**: Add `ErrRemoteUnavailable` to error mapping in both NFS and SMB handlers explicitly. NFS maps to `NFS3ERR_IO`/`NFS4ERR_IO`, SMB maps to `STATUS_IO_DEVICE_ERROR`.
- **WARN on first, DEBUG after**: Log WARN the first time a read fails due to remote unavailability after a health transition. Subsequent failures log at DEBUG. Reset the "first occurrence" flag on each healthy->unhealthy transition via an atomic bool per syncer.

### Status and Observability
- **Per-share health in dfs status/dfsctl status**: Each share shows remote health inline, e.g., `/archive [remote: offline 2h15m, 47 pending]` or `/archive [remote: healthy]`. Include a summary line like `2/5 shares offline, 47 blocks pending sync`.
- **Server health endpoint reports degraded**: GET /health returns 200 with `"status": "degraded"` when any remote is unhealthy. Not 503 — K8s probes shouldn't restart pods for S3 outage. Edge nodes are expected to operate offline.
- **dfs status inline display**: Offline shares displayed inline in the share list with remote health, outage duration, and pending upload count.
- **New CacheStats field**: Add `offline_reads_blocked` counter (int) to CacheStats. Tracks reads that were blocked because requested blocks were remote-only during an outage. Helps operators understand outage impact.
- **Aggregate storage_health in status response**: Add `storage_health` field to GET /api/v1/status: `"healthy"` (all remotes up), `"degraded"` (some remotes down), with per-share breakdown in shares section.

### Health State Propagation
- **Syncer checks its own HealthMonitor**: `EnsureAvailableAndRead`, `fetchBlock`, `enqueueDownload` check `syncer.IsRemoteHealthy()` before attempting remote ops. No new wiring — syncer already owns the HealthMonitor.
- **WARN log reset on health transition**: Atomic bool per syncer tracks "first read failure since transition". Resets on each healthy->unhealthy transition. First read failure after transition logs WARN, subsequent ones log DEBUG.

### Claude's Discretion
- Exact code placement for health checks within existing methods
- Whether to add the health check at the engine level or syncer level for Exists/GetSize
- Internal structure of the offline_reads_blocked counter (atomic int, etc.)
- dfs status / dfsctl status display formatting details
- How to surface storage_health in the status REST API response structure
- Test design for verifying offline read/write paths

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements
- `.planning/REQUIREMENTS.md` -- RESIL-01, RESIL-02, RESIL-03 are the requirements for this phase

### Engine read/write paths
- `pkg/blockstore/engine/engine.go` -- ReadAt, WriteAt, readAtInternal, ensureAndReadFromCache, Flush, Truncate, Delete, GetSize, Exists, GetCacheStats, CacheStats struct
- `pkg/blockstore/engine/engine.go:184` -- ReadAt entry point (delegates to readAtInternal)
- `pkg/blockstore/engine/engine.go:215` -- WriteAt (local.WriteAt + L1 invalidation)
- `pkg/blockstore/engine/engine.go:542` -- ensureAndReadFromCache (syncer.EnsureAvailableAndRead + local.ReadAt fallback)
- `pkg/blockstore/engine/engine.go:309` -- CacheStats struct (add offline_reads_blocked here)

### Syncer fetch and download paths
- `pkg/blockstore/sync/fetch.go` -- EnsureAvailableAndRead, EnsureAvailable, fetchBlock, inlineFetchOrWait, enqueueDownload, enqueuePrefetch
- `pkg/blockstore/sync/syncer.go` -- IsRemoteHealthy, RemoteOutageDuration, canProcess

### Health monitor (Phase 64)
- `pkg/blockstore/sync/health.go` -- HealthMonitor, IsHealthy, OutageDuration, SetTransitionCallback
- `pkg/blockstore/sync/syncer.go:117` -- SetHealthCallback for wiring health transitions

### Error types
- `pkg/blockstore/errors.go` -- Existing error sentinels (ErrBlockNotFound, ErrFileBlockNotFound). Add ErrRemoteUnavailable here.

### NFS error mapping
- `pkg/metadata/errors.go` -- ExportError types (ErrNotDirectory, ErrAccess, etc.)
- `internal/adapter/nfs/v3/handlers/` -- NFSv3 procedure handlers (READ, WRITE, COMMIT)
- `internal/adapter/nfs/v4/handlers/` -- NFSv4 procedure handlers

### SMB error mapping
- `internal/adapter/smb/v2/handlers/` -- SMB2 command handlers (READ, WRITE, FLUSH)

### Prior phase context
- `.planning/phases/63-cache-retention-model-and-eviction-policy/63-CONTEXT.md` -- Retention policies, ENOSPC behavior for pin mode
- `.planning/phases/64-s3-health-check-and-syncer-resilience/64-CONTEXT.md` -- HealthMonitor, circuit breaker, eviction suspension, recovery behavior

### Status and health endpoints
- `internal/controlplane/api/handlers/health.go` -- Server health endpoint
- `cmd/dfs/commands/status.go` -- dfs status command
- `cmd/dfsctl/commands/` -- dfsctl status commands

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `syncer.IsRemoteHealthy()` (syncer.go:153): Atomic bool check — fast path for health gating in read path
- `syncer.RemoteOutageDuration()` (syncer.go:162): Returns outage duration for error messages
- `HealthMonitor.SetTransitionCallback()` (health.go): Existing callback mechanism — can wire WARN log reset
- `CacheStats` struct (engine.go:309): Already has `RemoteHealthy`, `EvictionSuspended`, `OutageDurationSecs` — add `OfflineReadsBlocked`
- `blockstore.ErrBlockNotFound` (errors.go): Existing sentinel error pattern to follow for `ErrRemoteUnavailable`

### Established Patterns
- `errors.Is()` for sentinel error checks throughout NFS/SMB handlers
- `syncer.canProcess(ctx)` guard at top of fetch methods — health check follows same guard pattern
- `engine.readAtInternal` L1 -> local -> syncer cascade — health check fits at syncer level
- Atomic bool for lock-free state checks (used in HealthMonitor, uploading guard)

### Integration Points
- `syncer.EnsureAvailableAndRead()` (fetch.go:81): Add health check before remote download attempt
- `syncer.EnsureAvailable()` (fetch.go:265): Add health check before enqueuing downloads
- `syncer.fetchBlock()` (fetch.go:29): Add health check before remote ReadBlock
- `syncer.enqueueDownload()` (fetch.go:312): Add health check before queue enqueue
- `syncer.enqueuePrefetch()` (fetch.go:367): Add health check to skip when unhealthy
- `engine.GetSize()` (engine.go:196): Add health check when falling back to syncer
- `engine.Exists()` (engine.go:205): Add health check when falling back to syncer
- `engine.GetCacheStats()` (engine.go:338): Add OfflineReadsBlocked counter
- NFS READ/WRITE handlers: Add `ErrRemoteUnavailable` -> `NFS3ERR_IO`/`NFS4ERR_IO` mapping
- SMB READ/WRITE handlers: Add `ErrRemoteUnavailable` -> `STATUS_IO_DEVICE_ERROR` mapping

</code_context>

<specifics>
## Specific Ideas

- The write path already works offline by design — writes go to local store, syncer queues, circuit breaker pauses uploads. Phase 65 is primarily about making the **read path** degrade gracefully.
- Key insight: check `IsRemoteHealthy()` early (before attempting remote ops) to avoid network timeouts. The health monitor already does the probing.
- "dfs status" and "dfsctl status" should make offline shares immediately visible to operators, with outage duration and pending sync count inline.
- Server health endpoint should report "degraded" (not "unhealthy") when remotes are down — edge nodes are expected to operate offline and K8s shouldn't restart them.
- The `ErrRemoteUnavailable` sentinel with outage duration wrapping gives operators actionable error messages in both logs and protocol responses.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 65-offline-read-write-paths*
*Context gathered: 2026-03-16*
