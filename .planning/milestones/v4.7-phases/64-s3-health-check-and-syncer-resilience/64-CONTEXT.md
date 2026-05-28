# Phase 64: S3 Health Check and Syncer Resilience - Context

**Gathered:** 2026-03-16
**Status:** Ready for planning

<domain>
## Phase Boundary

The syncer detects S3 connectivity loss, stops wasting resources on failed uploads, suspends eviction to protect cached blocks, and automatically resumes when connectivity returns. This phase covers health monitoring, circuit breaker, eviction suspension, and recovery drain. Offline read/write paths for NFS/SMB clients are Phase 65. Test infrastructure is Phase 66.

</domain>

<decisions>
## Implementation Decisions

### Health Check Strategy
- **Dedicated health monitor goroutine** in each syncer probes S3 periodically via HeadBucket.
- Default probe interval: **30 seconds** when healthy, **5 seconds** when unhealthy (faster recovery detection).
- **3 consecutive failures** required to mark S3 unhealthy (tolerates transient blips).
- **1 successful probe** immediately marks S3 healthy (fast recovery).
- Health check interval is **global only** (server-level config, not per-share).
- **One health monitor per syncer** (no shared monitor for ref-counted remote stores). HeadBucket is cheap enough that redundant probes across shares sharing a remote are acceptable.
- Health monitor **skipped for local-only shares** (no remote store). They always report healthy.
- New shares **start healthy** and discover outage independently within one probe interval.
- Log **WARN on healthy->unhealthy** transition, **INFO on unhealthy->healthy** transition. No per-probe logging except at DEBUG level.

### Circuit Breaker
- **Full circuit breaker**: when health monitor marks S3 unhealthy, `periodicUploader` skips `syncLocalBlocks()` entirely. No exponential backoff on uploads — they just stop.
- **Health monitor is the single source of truth** for S3 status. Individual upload failures (revert-to-local in `syncFileBlock`) do NOT contribute to health state.
- In-flight uploads at the time of outage **fail naturally** — no active cancellation. Failed ones revert to `BlockStateLocal` as they do today.
- **SyncQueue continues accepting** new upload requests during outage. Blocks accumulate as `BlockStateLocal`. When S3 recovers, `periodicUploader` picks them up.
- **No maximum offline duration** — syncer stays in circuit-breaker mode indefinitely. Health probe keeps running. Edge deployments can be offline for days.
- Circuit breaker applies to **all retention modes** including pin. Pin shares already have eviction disabled; circuit breaker just pauses their uploads too.

### Eviction Suspension
- When S3 goes unhealthy, health monitor triggers **auto-suspend of eviction** per-share via `SetEvictionEnabled(false)`. When S3 recovers, re-enables eviction.
- Mechanism: syncer accepts a **HealthTransitionCallback(healthy bool)** via setter. Engine registers a callback that toggles `SetEvictionEnabled` on the local store.
- When disk fills during outage (eviction suspended + disk full): return **ENOSPC** to NFS/SMB clients. Same as pin mode behavior. No emergency eviction of un-re-downloadable blocks.
- **No immediate eviction sweep** on recovery. Re-enable the flag only; normal `ensureSpace()` triggers on next write if disk is full.
- REST API shows **effective eviction state**: both configured retention policy (lru/ttl/pin) and whether eviction is currently suspended due to remote offline. Operators see why eviction isn't happening.

### Recovery Behavior
- **Oldest-first drain** via existing `ListLocalBlocks` ordering. No special priority queue — `syncLocalBlocks` already picks up oldest blocks first by BadgerDB key ordering.
- **Normal upload rate** on recovery (maxUploadBatch=4 per 2s tick). No throttling, no burst. S3 handles it.
- **No special recovery sweep** — just resume `periodicUploader`. Blocks drain naturally at normal cadence.
- On recovery, **log backlog size**: count pending local blocks and include in the INFO transition log (e.g., "S3 recovered after 2h15m, 47 blocks pending upload").
- **Track outage duration**: record timestamp when S3 goes unhealthy. On recovery, compute and log duration. Include `outage_duration_seconds` in cache stats.
- **No block verification on recovery** — trust `BlockStateLocal` means not yet uploaded. Existing content-hash dedup in `syncFileBlock` already handles the case where a block was partially uploaded.
- **No manual override API** in this phase. Health is automatic-only. Manual force healthy/unhealthy can be added later if operators need it.

### Code Structure
- Health monitor lives in a **new file `pkg/blockstore/sync/health.go`** with a `HealthMonitor` type. Matches existing file-per-concern pattern (upload.go, fetch.go, queue.go).
- `HealthTransitionCallback` set via **setter method** on Syncer (like existing `SetFinalizationCallback`). Engine calls `syncer.SetHealthCallback(fn)` after creating both syncer and local store.
- Health check config fields added to **existing `sync.Config`**: `HealthCheckInterval`, `HealthCheckFailureThreshold`, `UnhealthyCheckInterval`.
- HealthMonitor accepts a **`func(ctx context.Context) error`** probe function for testability. Tests inject fake probes with controlled failure patterns.
- Cache stats REST API: **additive fields only** (backward compatible). New fields: `remote_healthy` (bool), `eviction_suspended` (bool), `outage_duration_seconds` (int, 0 when healthy), `last_health_check` (timestamp).
- Unit tests use **short real intervals** (10-50ms) matching existing syncer test patterns. No testable clock abstraction.

### Claude's Discretion
- Exact HealthMonitor struct fields and goroutine lifecycle management
- How to expose IsHealthy() to periodicUploader (atomic bool, method, etc.)
- Unhealthy probe interval default value (suggested 5s but Claude can adjust)
- Whether to add a `HealthState` type or use raw bool
- Test helper design for simulating S3 failures

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements
- `.planning/REQUIREMENTS.md` -- RESIL-04 through RESIL-08 are the requirements for this phase

### Existing syncer implementation
- `pkg/blockstore/sync/syncer.go` -- Syncer struct, periodicUploader, Start/Close, HealthCheck, SetRemoteStore
- `pkg/blockstore/sync/upload.go` -- syncLocalBlocks, syncFileBlock, uploadBlock, revertToLocal
- `pkg/blockstore/sync/fetch.go` -- EnsureAvailableAndRead, fetchBlock, inlineFetchOrWait
- `pkg/blockstore/sync/queue.go` -- SyncQueue with workers, stats, LastError
- `pkg/blockstore/sync/types.go` -- Config struct (where to add health check fields)

### Remote store interface and S3 implementation
- `pkg/blockstore/remote/remote.go` -- RemoteStore interface including HealthCheck
- `pkg/blockstore/remote/s3/store.go` -- S3 Store with HeadBucket health check

### Engine orchestrator
- `pkg/blockstore/engine/engine.go` -- BlockStore orchestrator, SetEvictionEnabled delegation, GetCacheStats, Start wiring

### Eviction layer (from Phase 63)
- `pkg/blockstore/local/fs/eviction.go` -- ensureSpace, evictBlock, evictOneTTL, evictOneLRU
- `pkg/blockstore/local/fs/manage.go` -- SetEvictionEnabled implementation
- `pkg/blockstore/local/local.go` -- LocalStore interface including SetEvictionEnabled

### Prior phase context
- `.planning/phases/63-cache-retention-model-and-eviction-policy/63-CONTEXT.md` -- Retention policy decisions that interact with eviction suspension

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `Syncer.HealthCheck()` (syncer.go:453): Already delegates to `remoteStore.HealthCheck()`. Can be reused by health monitor.
- `SetEvictionEnabled(bool)` (local.go:122, manage.go:14): Already exists on LocalStore. Health callback just needs to call this.
- `SetFinalizationCallback(fn)` (syncer.go:131): Existing setter-callback pattern to follow for `SetHealthCallback`.
- `SyncQueue.LastError()` (queue.go:188): Tracks last failure time/error. Useful for stats but not for health decisions.
- `CacheStats` struct (engine.go:294): Existing stats struct to extend with health fields.

### Established Patterns
- Periodic goroutine with ticker + stopCh + ctx.Done() select loop (syncer.go:393 periodicUploader). Health monitor will follow the same pattern.
- Atomic guard for overlapping ticks (syncer.go:408 uploading.CompareAndSwap). Health monitor is simpler (no overlap risk) but same style.
- nil-safe remote store checks throughout syncer: `if m.remoteStore == nil { return }`. Health monitor skips entirely when remote is nil.
- File-per-concern in sync/: upload.go, fetch.go, queue.go, dedup.go, types.go. health.go follows this pattern.

### Integration Points
- `Syncer.Start(ctx)` (syncer.go:345): Start health monitor goroutine here when remoteStore is non-nil.
- `Syncer.Close()` (syncer.go:425): Stop health monitor goroutine here.
- `Syncer.periodicUploader()` (syncer.go:393): Check `IsHealthy()` before calling `syncLocalBlocks()`.
- `engine.BlockStore.Start()` (engine.go:89): Wire up health callback after syncer.Start().
- `engine.CacheStats` (engine.go:294): Add health fields here.
- `engine.GetCacheStats()` (engine.go:318): Populate new health fields from syncer/health monitor.
- `internal/controlplane/api/handlers/health.go`: Existing health endpoint. May need to aggregate per-share remote health.

</code_context>

<specifics>
## Specific Ideas

- Edge deployment scenario: S3 connectivity is unreliable. Server must keep operating with local cache, not waste bandwidth on doomed uploads, and catch up automatically when connectivity returns.
- "No limit on offline duration" -- edge nodes can be offline for days. Syncer must be patient.
- Circuit breaker is the key insight: don't try uploads at all when S3 is known-down. Separate health probing from upload logic.
- Eviction suspension is critical safety net: evicting blocks you can't re-download would cause silent data loss.

</specifics>

<deferred>
## Deferred Ideas

- Manual override API to force healthy/unhealthy -- useful for maintenance windows, but adds complexity. Consider for a future phase.
- Prometheus metrics for health state -- decided REST API only for now. Can add Prometheus gauge later.
- Shared health monitor for ref-counted remote stores -- each syncer monitors independently for now.

</deferred>

---

*Phase: 64-s3-health-check-and-syncer-resilience*
*Context gathered: 2026-03-16*
