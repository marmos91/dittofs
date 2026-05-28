---
phase: 65-offline-read-write-paths
verified: 2026-03-16T23:15:00Z
status: passed
score: 11/11 must-haves verified
re_verification: false
---

# Phase 65: Offline Read/Write Paths Verification Report

**Phase Goal:** Offline read/write paths — health-gated read path with ErrRemoteUnavailable sentinel and error propagation, plus storage health observability for operators

**Verified:** 2026-03-16T23:15:00Z

**Status:** PASSED

**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | READ of a locally-cached block succeeds when S3 is unreachable | ✓ VERIFIED | TestOfflineReadCachedBlockSucceeds passes; cached reads bypass health check |
| 2 | READ of a remote-only block returns ErrRemoteUnavailable (not a network timeout) when S3 is unreachable | ✓ VERIFIED | TestOfflineReadRemoteOnlyBlockFails passes; errors.Is(err, blockstore.ErrRemoteUnavailable) == true |
| 3 | WRITE succeeds by storing in local cache when S3 is unreachable | ✓ VERIFIED | TestOfflineWriteSucceeds passes; writes go to local store, remote health not checked |
| 4 | NFS clients receive NFS3ERR_IO / NFS4ERR_IO for remote-unavailable reads | ✓ VERIFIED | internal/adapter/nfs/xdr/errors.go:197-199 maps ErrRemoteUnavailable to types.NFS3ErrIO |
| 5 | SMB clients receive STATUS_UNEXPECTED_IO_ERROR for remote-unavailable reads | ✓ VERIFIED | internal/adapter/smb/v2/handlers/converters.go:382-384 maps ErrRemoteUnavailable to types.StatusUnexpectedIOError |
| 6 | Prefetch and download enqueues are rejected immediately when S3 is unhealthy | ✓ VERIFIED | fetch.go:339-343 (enqueueDownload) and fetch.go:400-403 (enqueuePrefetch) check IsRemoteHealthy() |
| 7 | CacheStats includes OfflineReadsBlocked counter | ✓ VERIFIED | engine.go:335 defines field, engine.go:369 populates from syncer.OfflineReadsBlocked() |
| 8 | GET /health returns 200 with status 'degraded' when any remote store is unhealthy | ✓ VERIFIED | health.go:89-92 returns degradedResponse with http.StatusOK when anyDegraded==true |
| 9 | dfs status shows per-share remote health inline (healthy/offline with duration and pending count) | ✓ VERIFIED | cmd/dfs/commands/status.go:193-217 displays per-share health with color codes |
| 10 | dfsctl status shows per-share remote health and aggregate storage_health | ✓ VERIFIED | cmd/dfsctl/commands/status.go:152-176 displays per-share health with color codes |
| 11 | Health endpoint does NOT return 503 when remotes are unhealthy (edge nodes expected to run offline) | ✓ VERIFIED | health.go:92 explicitly uses http.StatusOK (200) with degraded status |

**Score:** 11/11 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/blockstore/errors.go` | ErrRemoteUnavailable sentinel error | ✓ VERIFIED | Lines 134-147: sentinel defined with protocol mapping docs |
| `pkg/blockstore/sync/fetch.go` | Health-gated read path with early rejection | ✓ VERIFIED | 5 occurrences of IsRemoteHealthy() checks (fetchBlock, EnsureAvailableAndRead, EnsureAvailable, enqueueDownload, enqueuePrefetch) |
| `pkg/blockstore/engine/engine.go` | OfflineReadsBlocked counter in CacheStats, health-gated GetSize/Exists | ✓ VERIFIED | CacheStats field line 335, populated line 369; GetSize/Exists delegate to syncer which has health gates |
| `internal/adapter/nfs/xdr/errors.go` | ErrRemoteUnavailable -> NFS3ERR_IO mapping | ✓ VERIFIED | Lines 197-199: errors.Is check returns types.NFS3ErrIO |
| `internal/adapter/smb/v2/handlers/converters.go` | ErrRemoteUnavailable -> StatusUnexpectedIOError mapping | ✓ VERIFIED | Lines 382-384: errors.Is check returns types.StatusUnexpectedIOError |
| `internal/controlplane/api/handlers/health.go` | Liveness endpoint with degraded status and per-share health | ✓ VERIFIED | Lines 17-31: ShareHealthInfo/StorageHealthInfo structs; lines 239-273: getStorageHealth method |
| `internal/controlplane/api/handlers/response.go` | degradedResponse helper | ✓ VERIFIED | Lines 56-66: degradedResponse function returning status="degraded" |
| `internal/cli/health/types.go` | StorageHealth and ShareHealth CLI types | ✓ VERIFIED | Lines 18-29: StorageHealth and ShareHealth structs |
| `cmd/dfs/commands/status.go` | dfs status with per-share remote health display | ✓ VERIFIED | Lines 67, 120-122, 193-217: StorageHealth field, parsing, display logic |
| `cmd/dfsctl/commands/status.go` | dfsctl status with per-share remote health display | ✓ VERIFIED | Lines 47, 92-94, 152-176: StorageHealth field, parsing, display logic |
| `pkg/blockstore/engine/engine_offline_test.go` | Integration tests for offline scenarios | ✓ VERIFIED | 5 test functions: TestOfflineReadCachedBlockSucceeds, TestOfflineReadRemoteOnlyBlockFails, TestOfflineWriteSucceeds, TestOfflineReadsBlockedCounter, TestPrefetchSuppressedWhenUnhealthy |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| `pkg/blockstore/sync/fetch.go` | `pkg/blockstore/sync/syncer.go` | syncer.IsRemoteHealthy() check before remote operations | ✓ WIRED | 5 occurrences in fetch.go (fetchBlock:40, EnsureAvailableAndRead:105, EnsureAvailable:296, enqueueDownload:339, enqueuePrefetch:401) |
| `pkg/blockstore/engine/engine.go` | `pkg/blockstore/sync/syncer.go` | syncer.IsRemoteHealthy() check in GetSize/Exists fallback | ✓ WIRED | GetSize/Exists delegate to syncer.GetFileSize/Exists which check health (syncer.go:302, 346) |
| `internal/adapter/nfs/xdr/errors.go` | `pkg/blockstore/errors.go` | errors.Is(err, blockstore.ErrRemoteUnavailable) | ✓ WIRED | errors.go imports blockstore package, line 197 checks errors.Is |
| `internal/controlplane/api/handlers/health.go` | `pkg/blockstore/engine/engine.go` | GetCacheStats() for per-share health info | ✓ WIRED | health.go:256 calls share.BlockStore.GetCacheStats() |
| `cmd/dfs/commands/status.go` | `internal/controlplane/api/handlers/health.go` | HTTP client fetching /health endpoint | ✓ WIRED | status.go:108 constructs /health URL, line 13 imports health package for types |

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|-------------|-------------|--------|----------|
| RESIL-01 | 65-01, 65-02 | Read path serves locally cached blocks when S3 is unreachable (graceful degradation) | ✓ SATISFIED | TestOfflineReadCachedBlockSucceeds proves local reads work; cached blocks bypass health check in ReadAt path |
| RESIL-02 | 65-01, 65-02 | Read path returns clear error for blocks only in S3 when unreachable (not generic I/O error) | ✓ SATISFIED | TestOfflineReadRemoteOnlyBlockFails proves ErrRemoteUnavailable is returned with outage context; errors.Is() matches sentinel; NFS/SMB mappers convert to protocol-specific IO errors |
| RESIL-03 | 65-01, 65-02 | Write path accepts writes to local store when S3 is unreachable | ✓ SATISFIED | TestOfflineWriteSucceeds proves writes succeed to local cache; syncer.WriteAt has no health check (writes are always local-first) |

### Anti-Patterns Found

None detected. Code follows established patterns:
- Health checks before remote operations (not in hot path)
- Sentinel error with structured wrapping (includes outage duration context)
- Atomic counters for observability
- First-occurrence WARN logging with subsequent DEBUG (log spam prevention)
- HTTP 200 with degraded status (K8s-friendly for edge deployments)

### Human Verification Required

None. All verification is automated via unit/integration tests and code inspection.

### Integration Test Results

```bash
$ go test ./pkg/blockstore/engine/ -run "TestOffline|TestPrefetchSuppressed" -count=1 -timeout 60s
ok  	github.com/marmos91/dittofs/pkg/blockstore/engine	0.519s
```

All 5 integration tests pass:
1. **TestOfflineReadCachedBlockSucceeds** — Proves RESIL-01: Cached reads work when remote is unhealthy
2. **TestOfflineReadRemoteOnlyBlockFails** — Proves RESIL-02: Remote-only reads return ErrRemoteUnavailable sentinel
3. **TestOfflineWriteSucceeds** — Proves RESIL-03: Writes succeed to local cache when remote is unhealthy
4. **TestOfflineReadsBlockedCounter** — Verifies CacheStats.OfflineReadsBlocked increments on blocked reads
5. **TestPrefetchSuppressedWhenUnhealthy** — Verifies prefetch is skipped during outages (no wasted queue slots)

### Build Verification

```bash
$ go build ./...
(no errors)
```

Full codebase builds successfully with all changes integrated.

---

## Summary

Phase 65 **PASSED** all verification checks. All 11 observable truths are verified, all 11 required artifacts exist and are substantive, all 5 key links are wired, and all 3 requirements (RESIL-01, RESIL-02, RESIL-03) are satisfied with integration test evidence.

**Core Resilience Logic:**
- ErrRemoteUnavailable sentinel error defined with protocol mapping guidance
- Health-gated read path: 7 methods check IsRemoteHealthy() before remote operations (fetchBlock, EnsureAvailableAndRead, EnsureAvailable, enqueueDownload, enqueuePrefetch, GetFileSize, Exists)
- Protocol error mapping: NFS and SMB handlers convert ErrRemoteUnavailable to appropriate IO error codes
- Observability: CacheStats exposes OfflineReadsBlocked counter; health endpoint reports "degraded" (not "unhealthy") with per-share remote health
- CLI integration: Both dfs and dfsctl status commands display per-share remote health inline with outage duration and pending upload count

**Key Design Decisions Validated:**
- Health checks at syncer level (not engine) — keeps logic close to remote operations
- HTTP 200 with "degraded" status — prevents K8s from restarting edge nodes during planned offline operation
- First-occurrence WARN logging — operators get immediate alert without log spam during extended outages
- Atomic counters — zero-cost observability when healthy, precise accounting when degraded

**Next Phase Readiness:**
Phase 65 is complete with full offline read/write resilience and observability. System can operate in degraded mode during S3 outages with clear error reporting to clients and operators.

---

_Verified: 2026-03-16T23:15:00Z_

_Verifier: Claude (gsd-verifier)_
