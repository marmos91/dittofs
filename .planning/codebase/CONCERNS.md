# Codebase Concerns

**Analysis Date:** 2026-02-09

## Tech Debt

### Missing NTLM Encryption Implementation

**Issue:** NTLM authentication advertises encryption support (Flag128, Flag56) but does not actually implement encryption.

**Files:** `internal/auth/ntlm/ntlm.go` (lines 343-356)

**Impact:** SMB clients connecting with encryption negotiated will believe traffic is encrypted when it is not. This is a critical security gap for SMB shares exposed over untrusted networks.

**Current state:**
- Server advertises `Flag128` and `Flag56` capability flags
- Session key exchange is negotiated
- Message signing is advertised and processed (dummy signatures only)
- Actual RC4/AES traffic encryption is not implemented per MS-NLMP spec

**Fix approach:**
1. Implement session key derivation (NLSSP signing key / sealing key)
2. Implement RC4 stream cipher for sealing (backwards compatibility with legacy clients)
3. Add AES-CCM support for newer clients
4. Remove or correct capability flags if encryption not implemented
5. Add integration tests verifying encrypted traffic end-to-end

**Priority:** High (security issue)

### Incomplete Storage Statistics Tracking

**Issue:** `UsedSize` field in `StorageStats` is hardcoded to zero and never updated.

**Files:** `pkg/payload/service.go` (line 326)

**Impact:** Cannot determine actual disk usage from API calls. Users cannot monitor storage consumption or implement storage quotas. Affects capacity planning and billing scenarios.

**Current state:**
```go
UsedSize: 0, // TODO: Implement proper stats tracking
```

**Fix approach:**
1. Track cumulative bytes written per file via cache dirty bytes
2. Account for uploaded blocks in block store (deduped blocks count once)
3. Add mechanism to query block store for actual stored size (S3: use ListObjects summaries)
4. Update StorageStats on flush and cache eviction
5. Add test coverage for stats accuracy

**Priority:** Medium (observability/UX)

### SMB Pipe Share List Refresh Inefficiency

**Issue:** Share list is queried from registry on every SMB pipe CREATE, causing N+1 lookups under load.

**Files:** `internal/protocol/smb/v2/handlers/create.go` (lines 638-654)

**Impact:** Each pipe creation (e.g., during share enumeration via SRVSVC) triggers a registry scan. This scales poorly with number of shares and concurrent clients. Can cause latency spikes.

**Current state:**
```go
// TODO: This is called on every pipe CREATE which is inefficient under high load.
// Consider caching the share list and invalidating on share add/remove events.
if h.Registry != nil {
    shareNames := h.Registry.ListShares()
    // ... iterate and build share info
}
```

**Fix approach:**
1. Cache share list in pipe handler with TTL (e.g., 30 seconds)
2. Implement cache invalidation on share create/delete events from Runtime
3. Alternatively, move share info to pipe-local state instead of dynamic lookup
4. Add metrics to measure cache hit ratio and request latency

**Priority:** Medium (performance)

### Redundant File Attribute Lookups in READDIRPLUS

**Issue:** `READDIRPLUS` calls `GetFile()` after using `GetChild()`, even when entry attributes are already available.

**Files:** `internal/protocol/nfs/v3/handlers/readdirplus.go` (lines 488-494)

**Impact:** Double metadata fetches for each directory entry, especially inefficient with BadgerDB/PostgreSQL backends. Scales with directory size (O(N) redundant queries).

**Current state:**
```go
// TODO: Use entry.Attr if populated to avoid this GetFile() call
entryFile, err := metaSvc.GetFile(ctx.Context, entryHandle)
```

**Fix approach:**
1. Return file attributes from `GetChild()` operation
2. Check if `entry.Attr` is populated before calling `GetFile()`
3. Only fetch missing attributes if entry.Attr is nil
4. Add benchmark comparing old vs. new approach
5. Consider extending MetadataStore interface to return attributes directly

**Priority:** Medium (performance optimization)

## Known Bugs

### NFS Client Panic with Apple Filesystems

**Issue:** Apple NFS client sometimes sends null pointer dereference with com.apple.filesystems.nfs.

**Files:** `pkg/adapter/nfs/nfs_connection.go` (line 295)

**Symptom:** NFS operations occasionally trigger a panic in Apple's NFS driver when server behavior is at protocol boundary.

**Trigger:** Unidentified specific sequence; documented but not fully reproducible.

**Workaround:** Connection-level panic recovery is in place; individual requests are isolated from one another.

**Status:** Mitigated (not fully root-caused). Should investigate with Apple support or NSF client testing on specific macOS versions.

## Security Considerations

### Lack of Production Security Audit

**Risk:** DittoFS has not undergone security audit. Authentication, authorization, and data protection mechanisms have not been reviewed by security experts.

**Files:** `internal/auth/ntlm/`, `internal/auth/spnego/`, `pkg/controlplane/api/`

**Current mitigation:**
- AUTH_UNIX credentials are extracted from client TCP connection only
- User/group validation done via database lookup
- JWT tokens used for API authentication (standard implementation)
- No built-in TLS (recommended to run behind VPN/network encryption)

**Recommendations:**
1. Deploy behind VPN or encrypted tunnel (WireGuard, VPN gateway, etc.)
2. Use network-level firewall to restrict client access
3. Enable API authentication and never expose HTTP port to untrusted networks
4. Consider third-party security audit before production use with sensitive data
5. Implement comprehensive audit logging for compliance scenarios

**Priority:** High (for production deployments)

### NTLM Security Issues (Above)

See "Missing NTLM Encryption Implementation" section.

### Weak API Default Configuration

**Risk:** Default API configuration may expose services when misconfigured.

**Files:** `pkg/config/config.go`, `pkg/controlplane/api/router.go`

**Current state:**
- API listens on 0.0.0.0:8080 by default
- Metrics listens on 0.0.0.0:9090 by default
- No TLS by default
- Authentication required only for API endpoints

**Recommendations:**
1. Document that API should never be exposed to untrusted networks
2. Add warnings in config validation if API binds to 0.0.0.0 in non-container environments
3. Consider default binding to localhost (127.0.0.1) instead
4. Add TLS support with self-signed cert generation option

**Priority:** Medium (operational security)

## Performance Bottlenecks

### Cache Miss Overhead for Large Files

**Problem:** Files larger than cache capacity cause repeated downloads of the same blocks. Sequential reads on multi-GB files may thrash cache.

**Files:** `pkg/cache/read.go` (lines 46-68), `pkg/payload/transfer/manager.go` (lines 874-942)

**Current behavior:**
- Cache size configurable but typically 1-4GB
- Files larger than cache are read with repeated eviction/re-download
- Prefetch helps but doesn't prevent eviction of just-read blocks
- LRU eviction may evict blocks needed by sequential read workloads

**Improvement path:**
1. Implement sequential access pattern detection
2. Adjust prefetch window based on read pattern (larger for sequential, smaller for random)
3. Consider pinning blocks during sequential reads
4. Add metrics to measure prefetch hit ratio and cache efficiency
5. Document cache sizing recommendations for workload

**Priority:** Low (addressed by proper cache sizing)

### S3 Multipart Upload Complexity

**Problem:** Large file uploads coordinate multiple goroutines with potential for partial failures during upload.

**Files:** `pkg/payload/store/s3/store.go`, `pkg/payload/transfer/manager.go`

**Current behavior:**
- Multipart uploads are chunked (default 5MB per part)
- If single part fails, entire upload may be orphaned
- Cleanup is not guaranteed on upload abort
- Concurrent uploads may fragment object store

**Risk:** AWS S3 charges for orphaned multipart uploads. Extended outages could lead to storage cost overages.

**Improvement path:**
1. Add lifecycle policy validation/recommendations in docs
2. Implement multipart upload cleanup on transfer manager shutdown
3. Add metrics for failed uploads and cleanup attempts
4. Consider implementing exponential backoff with jitter (currently implemented)
5. Add integration tests for partial upload recovery

**Priority:** Low (cloud provider management recommended)

## Fragile Areas

### Transfer Manager with Content-Addressed Deduplication

**Files:** `pkg/payload/transfer/manager.go` (1345 lines)

**Why fragile:**
- Complex state machine (pending → uploading → uploaded with concurrent transitions)
- Multiple error recovery paths (block hash conflicts, ObjectStore failures, block store failures)
- In-flight deduplication tracking adds synchronization complexity
- Finalization callback ordering sensitive to race conditions
- WAL recovery on startup must handle partial uploads

**Safe modification:**
1. Always test with concurrent uploads and failures
2. Use table-driven tests for state transitions
3. Add race detector tests (go test -race)
4. Document state machine transitions in comments
5. Test block deduplication scenarios explicitly

**Test coverage:**
- Manager unit tests: `manager_test.go` (1187 lines)
- Queue tests: `queue_test.go`
- Integration tests: `test/integration/`
- Missing: Chaos tests for S3 failures during upload

**Priority:** Medium (add chaos testing)

### NFS Dispatch and Protocol Handler Integration

**Files:** `internal/protocol/nfs/dispatch.go` (962 lines), `internal/protocol/nfs/v3/handlers/`

**Why fragile:**
- Auth context extraction tightly coupled to RPC message parsing
- Export-level access control (AllSquash, RootSquash) applies during mount
- Error handling differs between handler and NFS error codes
- File handle encoding varies by metadata backend (path vs UUID)
- WCC (weak cache consistency) data requires pre/post attributes

**Safe modification:**
1. Keep protocol-level changes isolated from business logic
2. Test with multiple NFS clients (Linux, macOS, Windows Subsystem for Linux)
3. Verify error codes against RFC 1813
4. Test with various squash configurations
5. Ensure file handles remain stable across server restarts

**Test coverage:**
- E2E tests: `test/e2e/`
- pjdfstest compliance: 8788/8789 tests pass (99.99%)
- Missing: Edge cases around simultaneous client operations

**Priority:** Medium (add concurrent client tests)

### PostgreSQL Metadata Store Link Count Handling

**Files:** `pkg/metadata/store/postgres/transaction.go` (1061 lines)

**Why fragile:**
- Link counts tracked in separate table (link_counts), not files.nlink
- Silly rename handling (NFS) sets link_count=0, requires GREATEST() protection
- Cross-directory directory moves require parent nlink updates
- SGID directory inheritance has specific clearance rules

**Safe modification:**
1. Test all link count scenarios (hard link, mkdir, rmdir, cross-dir move)
2. Verify silly rename cleanup works correctly
3. Test SGID inheritance with group membership checks
4. Run full pjdfstest suite after changes
5. Test concurrent directory operations

**Test coverage:**
- PostgreSQL-specific tests in metadata test suite
- pjdfstest includes link count tests
- Missing: Concurrent link count modification stress tests

**Priority:** Low (well-tested but complex)

### SMB Session State Machine

**Files:** `pkg/adapter/smb/smb_adapter.go` (805 lines), `internal/protocol/smb/session/manager.go`

**Why fragile:**
- Session lifecycle tied to TCP connection but can outlive it
- Signing state must be maintained across multiple requests
- Context variables tied to session (encryption keys, signing keys)
- Concurrent requests on same session require serialization

**Safe modification:**
1. Test session creation/destruction with concurrent requests
2. Verify signing/encryption state consistency
3. Test session timeout handling
4. Test with concurrent clients and session reuse
5. Add metrics for session lifecycle

**Test coverage:**
- Handler tests: `handler_test.go` (720 lines)
- Integration tests for SMB operations
- Missing: Concurrent session stress tests

**Priority:** Medium (add concurrent session tests)

## Scaling Limits

### Single-Node Architecture

**Current capacity:**
- Single server process
- Limited by machine CPU and memory
- Throughput depends on machine specs and network
- No replication or HA

**Limit:** Enterprise deployments requiring 99.99% uptime, disaster recovery, or geo-distribution.

**Scaling path:**
1. Consider stateless design for horizontal scaling
2. Implement distributed metadata store (PostgreSQL already supports this)
3. Add block store replication via S3 cross-region replication
4. Design cache coordination for multi-node scenarios
5. Implement distributed locking for concurrent access

**Priority:** Low (future enhancement)

### Cache Size Limitations

**Current capacity:**
- Cache size configurable (default unlimited)
- Large writes to cache can cause OOM
- ErrCacheFull indicates backpressure but clients may not handle it

**Limit:** Large concurrent file transfers or workloads with many small files.

**Scaling path:**
1. Implement bounded queue before cache (backpressure earlier)
2. Add adaptive cache sizing based on available memory
3. Implement spill-to-disk (mmap-backed cache) for overflow
4. Add circuit breaker pattern for overload scenarios

**Priority:** Medium (affects large file workloads)

## Dependencies at Risk

### Cobra CLI Framework

**Risk:** Stable but major version is v1; framework not under active development.

**Impact:** New CLI commands require manual setup; no built-in features for newer patterns.

**Migration plan:** Continue using Cobra; it is mature and stable. No alternative required unless new major CLI patterns emerge.

**Priority:** Low (framework is stable)

### GORM Database ORM

**Risk:** Major abstraction over SQL; can hide query inefficiencies.

**Impact:** Complex queries (link count handling, permission checks) may not be optimized. Requires SQL-level understanding.

**Current mitigation:**
- SQL queries are visible and auditable
- Test coverage includes performance benchmarks
- PostgreSQL store has been optimized for common operations

**Recommendations:**
1. Continue monitoring query performance metrics
2. Use EXPLAIN plans for new complex queries
3. Consider raw SQL for performance-critical paths
4. Add query timeout protection

**Priority:** Low (mitigated by test coverage)

### AWS SDK Dependency Chain

**Risk:** Large transitive dependency tree; security updates may be frequent.

**Impact:** Vulnerability in S3 client or its dependencies could affect all users.

**Current mitigation:**
- Use official AWS SDK (aws-sdk-go-v2)
- CI should run go mod tidy and check for vulnerabilities
- Pin versions in go.mod

**Recommendations:**
1. Monitor security advisories in AWS SDK
2. Update dependencies regularly
3. Use go mod verify to detect tampering
4. Consider alternatives (MinIO SDK) for object storage abstraction

**Priority:** Low (standard practice)

## Missing Critical Features

### Network Lock Manager (NLM) Protocol

**Problem:** File locking (flock, fcntl, lockf) is not implemented.

**Impact:** Applications requiring file locks cannot use DittoFS. Multiple clients can write to same file simultaneously without coordination.

**Blocks:** Enterprise applications (databases, ERP systems) that require advisory locking.

**Effort estimate:** High (NLM protocol is complex; requires distributed lock coordinator).

**Alternative approaches:**
1. Document lack of support clearly
2. Recommend application-level locking
3. Consider NFSv4 in future (has integrated locking)

**Priority:** Low (known limitation, documented)

### Block-Level Snapshots

**Problem:** No snapshot capability for backup/recovery scenarios.

**Impact:** Cannot create point-in-time backups without file-level consistency guarantees.

**Blocks:** Backup/disaster recovery workflows.

**Effort estimate:** Medium (requires backup API + storage backend snapshot integration).

**Alternative approaches:**
1. Document S3 snapshot/versioning as mitigation
2. Provide backup/restore API for control plane state
3. Consider incremental backup support in future

**Priority:** Low (S3 versioning provides some mitigation)

### Rate Limiting per Share/User

**Problem:** Global rate limiting only; cannot limit specific users or shares.

**Impact:** Cannot isolate noisy neighbors; DoS from single share affects all shares.

**Blocks:** Multi-tenant deployments, SLA enforcement.

**Effort estimate:** Medium (requires per-user/share request tracking).

**Current mitigation:**
- Global rate limiting available (config: `server.rate_limiting`)
- Network-level rate limiting recommended
- Can run separate server instances per tenant

**Priority:** Low (low priority for current use cases)

## Test Coverage Gaps

### Concurrent Metadata Operations

**Untested area:** Simultaneous operations on same file/directory from multiple clients.

**Files:** All metadata store implementations and NFS/SMB handlers.

**Risk:** Race conditions in file creation, deletion, rename, and permission changes. Metadata stores may have non-atomic transitions.

**Priority:** High

**Recommended tests:**
1. Parallel file creation in same directory (race on nlink)
2. Concurrent reads while file is being written
3. Simultaneous rename and delete on same file
4. Parallel chmod on same file
5. Concurrent hard link creation

### S3 Failure Scenarios

**Untested area:** Partial S3 upload failures, network timeouts, throttling responses.

**Files:** `pkg/payload/transfer/manager.go`, `pkg/payload/store/s3/store.go`

**Risk:** Uploads may hang indefinitely on transient S3 errors; retry logic may not be comprehensive.

**Priority:** Medium

**Recommended tests:**
1. Inject S3 API failures at random points during upload
2. Test timeout handling on slow uploads
3. Test throttling (429) response handling
4. Test multipart upload abort cleanup
5. Verify orphaned upload cleanup on server restart

### Edge Cases in Block Addressing

**Untested area:** Block addressing for files at chunk/block boundaries and very large files (2TB+).

**Files:** `pkg/cache/read.go`, `pkg/payload/transfer/manager.go` (offset/length calculations)

**Risk:** Off-by-one errors in block index calculations; potential data corruption for large files.

**Priority:** Medium

**Recommended tests:**
1. Write/read at exact chunk boundaries (64MB)
2. Write/read at exact block boundaries (4MB)
3. Files > 1TB (tests chunkIdx/blockIdx wrapping)
4. Reads that span multiple chunks (currently exercised)
5. Sparse file operations (zero gap between blocks)

### SMB Signing and Encryption

**Untested area:** SMB message signing with various algorithms, encryption negotiation.

**Files:** `internal/protocol/smb/signing/`, `internal/auth/ntlm/`

**Risk:** Signing may not be cryptographically correct; clients may reject signatures.

**Priority:** High (security-relevant)

**Recommended tests:**
1. Verify HMAC-SHA256 signing with known test vectors
2. Test with SMB clients that require signing (Windows Server)
3. Test encryption flag negotiation and fallback
4. Test with non-ASCII filenames requiring UTF-16 encoding
5. Fuzz test SMB message parsing

## Performance Analysis

### Memory Allocations in Hot Paths

**Area:** NFS READ/WRITE handling allocates buffers per request.

**Current:** Buffer pooling implemented (3-tier pool: 4KB, 64KB, 1MB).

**Status:** Mitigated. Go allocator performance is good; profiling recommended if throughput concerns arise.

### Context Deadline Propagation

**Area:** Context deadlines from client requests propagated through entire call stack.

**Current:** All methods accept context.Context parameter.

**Status:** Good. Should verify timeout configurations are appropriate for operations.

### Goroutine Leak Prevention

**Area:** Connection handlers spawn goroutines; must clean up on connection close.

**Current:** Defer-based cleanup in place; panic recovery added.

**Status:** Good. Should verify no goroutine leaks in load tests.

---

*Concerns audit: 2026-02-09*
