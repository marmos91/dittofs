# Codebase Concerns

**Analysis Date:** 2026-02-02

## Tech Debt

### NTLM Encryption Not Implemented
- **Issue**: NTLM security negotiation flags advertise 128-bit and 56-bit encryption support (`Flag128`, `Flag56`), but no actual encryption is implemented
- **Files**: `internal/auth/ntlm/ntlm.go` (lines 343-354)
- **Impact**: SMB clients may fail authentication if they require encrypted connections; security weakness for sensitive data in transit
- **Fix approach**: Implement session key derivation and RC4/AES encryption per MS-NLMP specification; requires adding ciphertext handling in SMB request/response pipeline

### Link Count Hardcoded in SMB
- **Issue**: SMB handler returns hardcoded `NumberOfLinks: 1` instead of tracking actual hard link count
- **Files**: `internal/protocol/smb/v2/handlers/converters.go` (line 140)
- **Impact**: SMB clients receive incorrect link count; hard links created via NFS won't be properly reported to SMB clients
- **Fix approach**: Thread actual link count from `FileAttr.LinkCount` through converter; ensure all backends populate link count correctly (memory, badger, postgres all support it)

### Storage Stats Tracking Not Implemented
- **Issue**: `PayloadService.GetStorageStats()` returns hardcoded `UsedSize: 0` instead of calculating actual used space
- **Files**: `pkg/payload/service.go` (line 326)
- **Impact**: REST API reports zero disk usage; admin tools can't monitor storage consumption
- **Fix approach**: Implement proper cache stats aggregation; consider storing in metadata service for efficiency (currently requires scanning all files)

### Prefetch GET Errors Silently Ignored
- **Issue**: Prefetch requests in transfer queue silently ignore download errors without logging
- **Files**: `pkg/payload/transfer/queue.go` (line 277): `_ = q.processDownload(ctx, req) // Best effort - ignore errors`
- **Impact**: Prefetch failures go undetected; metrics don't reflect download failures for prefetch operations
- **Fix approach**: Log prefetch errors at DEBUG level; track failed prefetch attempts in metrics

### Share List Cached Inefficiently in Pipe CREATE
- **Issue**: SMB pipe CREATE handler refreshes entire share list from registry on every pipe creation
- **Files**: `internal/protocol/smb/v2/handlers/create.go` (lines 618-620)
- **Impact**: High latency under concurrent pipe creation; scales linearly with number of shares
- **Fix approach**: Cache share list with invalidation events on share add/remove; use event-driven updates instead of eager refresh

### ReadDirPlus GetFile() Duplication
- **Issue**: `ReadDirPlus` calls `GetFile()` to fetch entry attributes even if directory listing already populated them
- **Files**: `internal/protocol/nfs/v3/handlers/readdirplus.go`
- **Impact**: Unnecessary metadata store lookups; degrades performance for large directories
- **Fix approach**: Check if entry attributes already populated from listing before calling `GetFile()`

## Known Bugs

### Parent Directory Navigation Not Handled in SMB
- **Issue**: SMB CREATE handler doesn't properly handle parent directory navigation in path (`..`)
- **Files**: `internal/protocol/smb/v2/handlers/create.go` (line 754)
- **Impact**: Clients attempting to open files in parent directories may fail; affects relative path handling
- **Fix approach**: Implement parent directory traversal in path resolution; validate against share root boundary

## Security Considerations

### NTLM Flags Advertise Unsupported Encryption
- **Risk**: NTLM client may select encryption algorithms (RC4/AES) that aren't implemented, leading to authentication failure or fallback to unencrypted
- **Files**: `internal/auth/ntlm/ntlm.go` (lines 343-355)
- **Current mitigation**: Dummy signatures included (Flag_AlwaysSign), but actual message encryption not implemented
- **Recommendations**:
  1. Either implement encryption per MS-NLMP or remove encryption flags from negotiation
  2. Implement session key derivation (HMAC-MD5 over client/server nonces + credential material)
  3. Add AES-CCM-128 (preferred) or RC4-HMAC-MD5 (legacy) encryption for message confidentiality

### Limited Auth Provider Support
- **Risk**: Only AUTH_UNIX (NFS) and NTLM (SMB) supported; no Kerberos, no strong auth for NFS
- **Files**: `internal/auth/ntlm/ntlm.go`, `internal/protocol/nfs/dispatch.go`
- **Current mitigation**: Network-level security must be provided (VPN recommended)
- **Recommendations**:
  1. Document Kerberos as non-supported; recommend operating in isolated networks
  2. Consider SPNEGO support for Kerberos interoperability in future releases

### Hard Link Reference Counting Not Validated
- **Risk**: While link counting logic is implemented, there's no atomic guarantee that decrementing RefCount and deleting files happens atomically
- **Files**: `pkg/metadata/file.go` (various operations that modify link counts)
- **Current mitigation**: Single-threaded metadata operations within transactions
- **Recommendations**: Add assertions that validate link count invariants during filesystem recovery; implement reference count validation tool

## Performance Bottlenecks

### Prefetch Queue Fills Without Backpressure
- **Problem**: Prefetch requests can overload transfer queue without limiting incoming requests; low-priority prefetch may starve uploads
- **Files**: `pkg/payload/transfer/manager.go`, `pkg/payload/transfer/queue.go`
- **Cause**: Prefetch enqueue returns immediately; no capacity check before enqueuing; workers process in priority order but queue still accumulates
- **Improvement path**:
  1. Add queue depth monitoring per transfer type
  2. Implement dynamic backpressure: when prefetch queue > threshold, drop oldest prefetch requests
  3. Reduce `PrefetchBlocks` config default from 4 to 2 for better responsiveness

### Cache Eviction Algorithm Not Optimal
- **Problem**: LRU eviction is O(n) scan per eviction; high CPU usage under cache pressure
- **Files**: `pkg/cache/eviction.go`
- **Cause**: No sorted LRU list; must scan all entries to find LRU candidates
- **Improvement path**:
  1. Maintain ordered list (doubly-linked list or heap) of entries by access time
  2. Move accessed entry to head on write (O(1) removal + insertion)
  3. Evict from tail efficiently without scan

### S3 Stats Caching May Be Too Aggressive
- **Problem**: Stats caching with TTL can return stale size information to clients
- **Files**: `pkg/metadata/store/badger/store.go` (stats cache)
- **Cause**: BadgerDB caches stat results to avoid expensive scans (macOS Finder hammers FSSTAT)
- **Improvement path**:
  1. Document stat freshness guarantees in configuration
  2. Allow disabling cache for accuracy-critical deployments
  3. Consider hybrid: serve cached stats for human-facing tools, bypass for programmatic clients

### Concurrent Connection Semaphore Contention
- **Problem**: Each connection acquisition/release acquires semaphore lock; high lock contention at scale
- **Files**: `pkg/adapter/nfs/nfs_adapter.go` (connection semaphore)
- **Cause**: Single semaphore protecting all connections; Acquire/Release have lock overhead
- **Improvement path**:
  1. Use lock-free atomic compare-and-swap for semaphore (if counter-based)
  2. Consider per-CPU semaphore sharding for NUMA systems
  3. Benchmark contention at 1000+ concurrent connections

## Fragile Areas

### Metadata Transaction Nesting
- **Files**: `pkg/metadata/service.go`, `pkg/metadata/store/badger/transaction.go`, `pkg/metadata/store/postgres/transaction.go`
- **Why fragile**: Code doesn't explicitly prevent nested transactions; if caller begins transaction and delegates to another function that also begins transaction, deadlock occurs
- **Safe modification**: Document and enforce "no nested transactions" rule; consider adding runtime detection in development builds
- **Test coverage**: Limited testing of nested transaction scenarios; E2E tests should cover concurrent operations

### Hard Link Cycle Detection
- **Files**: `pkg/metadata/file.go` (rename operations), `pkg/metadata/store/*/transaction.go`
- **Why fragile**: While cycle checks exist, they're inline with rename logic; if rename path changes, cycle check may be bypassed
- **Safe modification**: Extract cycle detection into separate, independently testable function; add assertion in rename handlers
- **Test coverage**: Edge cases like renaming across shares not covered

### File Handle Encoding/Decoding
- **Files**: `pkg/metadata/store/memory/store.go` (unsafe.String usage), `internal/protocol/nfs/xdr/filehandle.go`
- **Why fragile**: Memory store uses `unsafe.String()` for handle conversion; assumes handle bytes don't change during conversion
- **Safe modification**: Use standard string conversion with allocation; profile impact before assuming performance is critical
- **Test coverage**: Handle stability tests exist but don't cover all backends

### SMB Session State Machine
- **Files**: `internal/protocol/smb/session/manager.go`, `pkg/adapter/smb/smb_connection.go`
- **Why fragile**: Session state transitions aren't explicitly guarded; concurrent operations may transition state incorrectly
- **Safe modification**: Implement explicit state machine with guard clauses; log state transitions at DEBUG level
- **Test coverage**: Limited race condition testing; should run with `-race` flag more thoroughly

### Deferred Metadata Commits
- **Files**: `pkg/metadata/service.go` (lines 53-59), `pkg/metadata/pending_writes.go`
- **Why fragile**: Deferred commits merge pending state without triggering store updates; if pending state accumulates unbounded, memory grows
- **Safe modification**:
  1. Add periodic flush of pending state (every N writes or time-based)
  2. Add metrics to track pending write count
  3. Implement bounded pending write buffer
- **Test coverage**: Load tests should verify pending state doesn't accumulate indefinitely

## Scaling Limits

### Single Cache Instance for All Shares
- **Current capacity**: 1 global cache; bounded by `maxSize` parameter
- **Limit**: With 100+ shares, cache contention increases; single lock contects become bottleneck
- **Scaling path**:
  1. Implement per-file micro-caching or partition cache by hash(payloadID)
  2. Add cache sharding to reduce lock contention
  3. Profile cache lock hold times under workload

### Memory Store Metadata Is Ephemeral
- **Current capacity**: Unbounded in-memory map growth with number of files
- **Limit**: Server crash or OOM if file count exceeds available RAM
- **Scaling path**: Use BadgerDB or PostgreSQL backend; memory store is testing-only

### Control Plane Store Scale
- **Current capacity**: SQLite single-file; BadgerDB single-instance; PostgreSQL distributed
- **Limit**: SQLite performance degrades with 1000+ users/groups; no HA support
- **Scaling path**:
  1. For production: use PostgreSQL with read replicas
  2. For HA: implement clustering at application layer (future work)

## Dependencies at Risk

### NTLM Implementation Incomplete
- **Risk**: Security flags indicate support for encryption but don't implement it
- **Impact**: SMB clients requiring encryption will fail; potential for security downgrades
- **Migration plan**:
  1. Short term: Remove encryption flags from negotiation to prevent failures
  2. Medium term: Implement session key derivation and AES-CCM-128 encryption
  3. Document as breaking change in release notes

### BadgerDB Stats Caching TTL
- **Risk**: Cached stats may serve stale information; no invalidation on write
- **Impact**: Admin tools show outdated storage usage; capacity planning based on incorrect data
- **Migration plan**:
  1. Add configuration option for stats cache TTL (0 = disabled)
  2. Implement write-through invalidation (when file truncated, invalidate stats)
  3. Document stat freshness guarantees

## Test Coverage Gaps

### SMB Pipe CREATE Performance
- **What's not tested**: Concurrent pipe creation with many shares
- **Files**: `internal/protocol/smb/v2/handlers/create.go`
- **Risk**: Performance regression under load; share list refresh on every create is inefficient
- **Priority**: High (affects SMB client compatibility)

### Hard Link Stability Across Restarts
- **What's not tested**: Hard links created in one session, server restarted, link count still correct
- **Files**: `pkg/metadata/file.go`, `pkg/metadata/store/badger/`, `pkg/metadata/store/postgres/`
- **Risk**: Link counts may be inconsistent after crash recovery
- **Priority**: Medium (affects data integrity)

### Deferred Commit Unbounded Growth
- **What's not tested**: Pending write buffer size under sustained high write load
- **Files**: `pkg/metadata/service.go`, `pkg/metadata/pending_writes.go`
- **Risk**: Memory exhaustion if pending writes accumulate faster than committed
- **Priority**: Medium (affects long-running servers under load)

### Concurrent RENAME Operations
- **What's not tested**: Concurrent renames on same source/destination; cycle detection under races
- **Files**: `pkg/metadata/file.go` (rename logic)
- **Risk**: Potential for infinite loops or missed cycles
- **Priority**: Medium (affects concurrent workloads)

### SMB Session Teardown Under Load
- **What's not tested**: Abrupt disconnection with pending requests
- **Files**: `pkg/adapter/smb/smb_connection.go`, `internal/protocol/smb/session/manager.go`
- **Risk**: Goroutine leaks if cleanup is incomplete; session state corruption
- **Priority**: High (affects long-running servers)

### Transfer Queue Prefetch Overload
- **What's not tested**: Prefetch queue behavior when overwhelmed (queue full, worker starvation)
- **Files**: `pkg/payload/transfer/queue.go`, `pkg/payload/transfer/manager.go`
- **Risk**: Prefetch requests may drop silently; memory consumption unbounded if queue fills
- **Priority**: Medium (affects performance under load)

---

*Concerns audit: 2026-02-02*
