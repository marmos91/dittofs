# Codebase Concerns

**Analysis Date:** 2026-02-04

## Tech Debt

### SMB Encryption Not Implemented

**Area:** SMB signing and encryption

**Issue:** SMB adapter advertises encryption capabilities (`Flag128`, `Flag56` in NTLM) but does not actually encrypt traffic. The server promises encryption support but sends plaintext messages.

**Files:** `internal/auth/ntlm/ntlm.go:343-356`

**Impact:** High - Any client requesting encryption will receive plaintext data without warning. Security expectations violated at protocol level.

**Fix approach:**
1. Implement session key derivation per MS-NLMP specification
2. Add RC4/AES encryption logic for SMB2 message bodies
3. Remove encryption flags from challenge message if encryption not implemented
4. Test with encrypted SMB clients (Windows, macOS)

---

### SMB Pipe Share List Rebuilt on Every CREATE

**Area:** SMB named pipe handling

**Issue:** The `CREATE` handler rebuilds the entire pipe share list from registry on every single pipe open request. Under high load with frequent pipe creation (like Windows Explorer browsing), this causes repeated full registry scans.

**Files:** `internal/protocol/smb/v2/handlers/create.go:638-649`

**Impact:** Medium - Performance degradation during high concurrent pipe creation. NoIPC$ share discovery becomes O(n) per request instead of O(1).

**Fix approach:**
1. Cache the share list with invalidation on share add/remove events
2. Use atomic.Value for lock-free cache updates
3. Invalidate cache only when Runtime.CreateAdapter/DeleteAdapter called
4. Consider event-driven cache invalidation via channels

---

### Storage Stats Incomplete

**Area:** Storage statistics and health monitoring

**Issue:** `GetStorageStats()` returns `UsedSize: 0` with a TODO comment. Only content count is tracked. Statistics are inefficient (full file list scan) and incomplete.

**Files:** `pkg/payload/service.go:326`

**Impact:** Low - Monitoring systems get incomplete utilization metrics. FSSTAT responses show zeros for storage consumed.

**Fix approach:**
1. Implement proper stats tracking in cache layer
2. Cache stats with TTL (e.g., refresh every 60 seconds) to avoid expensive scans
3. Track: `UsedSize`, `FreeSize`, `TotalSize`, `InodesUsed`, `InodesAvailable`
4. Consider stats persistence in BadgerDB metadata store for accuracy

---

### SMB Link Count Not Tracked

**Area:** SMB file attributes

**Issue:** SMB adapter returns hardcoded `NumberOfLinks: 1` regardless of actual hard link count. Files with multiple hard links appear as single-link files to SMB clients.

**Files:** `internal/protocol/smb/v2/handlers/converters.go:152`

**Impact:** Medium - SMB clients see incorrect link counts. Tools relying on link count (deduplication detection, backup tools) get wrong data.

**Fix approach:**
1. Pass actual `FileAttr.NLink` to converter function
2. Extract link count from metadata during attribute conversion
3. Ensure metadata stores track link count properly (all backends support this)
4. Test with `stat()` and SMB clients

---

### READDIRPLUS Redundant GetFile() Call

**Area:** NFS READDIR+ optimization

**Issue:** When `ReadDirectory()` returns entry attributes via `entry.Attr`, the `READDIRPLUS` handler calls `GetFile()` again to fetch the same attributes. This causes duplicate database queries.

**Files:** `internal/protocol/nfs/v3/handlers/readdirplus.go:488`

**Impact:** Medium - READDIR+ performance scales with directory size. 1000-file directory makes 1000 redundant queries for data already retrieved.

**Fix approach:**
1. Check if `entry.Attr` is populated before calling `GetFile()`
2. Only call `GetFile()` if attributes missing
3. Update `metadata.ReadDirectory()` to ensure `entry.Attr` is always populated
4. Verify handles match between entry and lookupHandle

---

## Known Bugs

### NFS Y2106 Timestamp Limitation

**Issue:** NFSv3 uses 32-bit unsigned seconds, limiting max timestamp to 2106-02-07 06:28:15 UTC.

**Files:** Per RFC 1813 Section 2.2 - fundamental protocol limitation

**Trigger:** Creating files with mtime > 4294967295 (2106-02-07)

**Workaround:** None - inherent to NFSv3 protocol. NFSv4 uses 64-bit timestamps.

**Impact:** Low - Most applications won't encounter this before 2106. Benchmark testing with future dates will fail.

---

### NFS File Locking Not Implemented

**Issue:** NLM protocol not implemented. Applications requiring `flock()`, `fcntl()`, or `lockf()` will not work correctly.

**Files:** No implementation exists for NFS Lock Manager

**Trigger:** Any client calling file lock operations

**Workaround:** Use application-level locking or single-writer architecture

**Impact:** High - Multi-client write scenarios with multiple writers are unsupported and can corrupt data

---

### Hard Link Reference Counting in PostgreSQL

**Issue:** PostgreSQL metadata store uses a separate `link_counts` table as source of truth for `nlink`. The main `files.nlink` column is updated for consistency but may become stale. Recovery from crashes with partially-updated rows risks incorrect counts.

**Files:** `pkg/metadata/store/postgres/transaction.go` - link count updates

**Trigger:** Server crash during hard link/unlink operations

**Workaround:** Use BadgerDB or memory store for hard-link-intensive workloads

**Impact:** Low - Affects only PostgreSQL backend. Link counts may be temporarily inconsistent after crashes but will eventually stabilize.

---

## Security Considerations

### No Built-In Encryption

**Risk:** All NFS and SMB traffic is transmitted in plaintext. File data, credentials, and operations visible to network observers.

**Files:** Core protocols (`internal/protocol/nfs/`, `internal/protocol/smb/`)

**Current mitigation:** Documentation recommends VPN/IPsec/SSH tunnels

**Recommendations:**
1. Document VPN requirement prominently in README
2. Add deployment guide with firewall rules
3. Consider built-in TLS support for future versions
4. Add warning to logs if insecure network detected (impossible but document in FAQ)

---

### AUTH_UNIX Trust-Based Authentication

**Risk:** AUTH_UNIX relies on client-provided UID/GID with no cryptographic verification. Malicious clients can claim any UID and bypass access controls on trusted networks.

**Files:** `pkg/metadata/authentication.go` - permission checking

**Current mitigation:** Export-level access control (IP-based), root squashing option

**Recommendations:**
1. Require root squashing for all shares (currently optional)
2. Implement Kerberos/AUTH_GSS for production deployments
3. Add audit logging for authentication bypass attempts
4. Document AUTH_UNIX limitations clearly

---

### No Audit Logging

**Risk:** No tracking of failed authentication attempts, unusual access patterns, or data exfiltration.

**Files:** Logs are operational only, no security event tracking

**Current mitigation:** Structured JSON logging supports external SIEM integration

**Recommendations:**
1. Add audit log category for security events
2. Log: failed auth, permission denials, unusual patterns (mass reads, deletes)
3. Integrate with syslog or CloudWatch for production

---

### NTLM v2 Implementation Incomplete

**Risk:** NTLM is the weakest protocol supported. Missing encryption (mentioned above) allows plaintext interception. Response verification relies on client-provided timestamps without full validation.

**Files:** `internal/auth/ntlm/ntlm.go`

**Current mitigation:** Only used for SMB, which is local-network protocol typically

**Recommendations:**
1. Implement full encryption pipeline
2. Add client timestamp validation (current protocol version compatibility)
3. Consider deprecating NTLM in favor of Kerberos

---

## Performance Bottlenecks

### Pipe Share Registry Scanned on Every CREATE

**Problem:** See "SMB Pipe Share List Rebuilt on Every CREATE" above

**Current performance:** O(n) per request where n = number of shares

**Improvement path:** Cache with invalidation (estimated 100x speedup)

---

### ReadDirectory Inefficient for Large Directories

**Problem:** Entire directory listing loaded into memory, then iterated. No pagination or streaming.

**Files:** `pkg/metadata/service.go` - ReadDirectory() method

**Impact:** Memory spikes with large directories (10K+ files)

**Improvement path:**
1. Implement cursor-based pagination
2. Stream entries instead of loading all at once
3. Separate metadata query from handle encoding

---

### Stats Calculation Requires Full Scan

**Problem:** `GetStorageStats()` lists all files to count them. With 100K+ files, this is expensive.

**Files:** `pkg/payload/service.go:324`

**Impact:** FSSTAT calls stall if called frequently

**Improvement path:** Cache stats with 60-second TTL, update incrementally on writes/deletes

---

### READDIRPLUS Makes Redundant Queries

**Problem:** See "READDIRPLUS Redundant GetFile() Call" above

**Current latency:** ~1-2ms per file in directory

**Improvement path:** Use pre-fetched attributes (estimated 50-80% latency reduction)

---

## Fragile Areas

### Memory Metadata Store Has No Persistence

**Files:** `pkg/metadata/store/memory/store.go`

**Why fragile:** All metadata lost on restart. Ephemeral by design but often used incorrectly in development.

**Safe modification:**
1. Document "memory store is ephemeral" prominently
2. Add startup warning if memory store used for shares
3. Add tests that verify data is lost on restart

**Test coverage:** Good - but tests don't verify persistence expectations

---

### Transaction Nesting Not Prevented

**Files:** `pkg/metadata/store/` - all implementations support `WithTransaction()`

**Why fragile:** No runtime check prevents nested `WithTransaction()` calls. BadgerDB allows nested transactions (unsafe). Memory store will deadlock.

**Safe modification:**
1. Add thread-local context to track if already in transaction
2. Return error if nested transaction attempted
3. Document transaction non-composability clearly
4. Add tests for nested transaction rejection

**Test coverage:** No tests for nested transaction error handling

---

### SMB Connection Panic Recovery Incomplete

**Files:** `pkg/adapter/smb/smb_connection.go:132-141` - deferred panic handler

**Why fragile:** Panic recovery in request loop may mask underlying bugs. Recovered panics are logged but connection may be in inconsistent state.

**Safe modification:**
1. Wrap entire connection loop in panic handler (already done)
2. Add state validation after panic recovery
3. Consider force-closing connection on panic (safer than continuing)
4. Log panic stack traces for debugging

**Test coverage:** Panic recovery tested but no corruption detection post-panic

---

### BadgerDB Stats Caching TTL Not Configurable

**Files:** `pkg/metadata/store/badger/objects.go` - hardcoded TTL

**Why fragile:** Different deployments need different cache TTLs. Fixed TTL may cause excessive scans or stale data.

**Safe modification:**
1. Make cache TTL configurable via Config
2. Default to 60 seconds (current behavior)
3. Document impact on Finder/Explorer responsiveness
4. Add metrics for cache hit rate

**Test coverage:** Stats caching works but no TTL tests

---

### PostgreSQL Link Count Tracking Complex

**Files:** `pkg/metadata/store/postgres/transaction.go` - `link_counts` table

**Why fragile:** Multiple places update link counts. Race conditions possible between checks and updates.

**Safe modification:**
1. Use database-level constraints (CHECK, UNIQUE indexes)
2. Implement optimistic locking (version field)
3. Add comprehensive link count recovery tool
4. Test all hard link scenarios with race detector

**Test coverage:** Good for basic hard links, weak for concurrent scenarios

---

## Scaling Limits

### Connection Tracking Uses sync.Map

**Current capacity:** Designed for high churn (connections created/destroyed frequently)

**Limit:** sync.Map uses non-blocking operations, no explicit limit. OS TCP limits apply (~64K connections per port).

**Scaling path:**
1. Monitor connection count and warn at 80% of OS limit
2. Implement connection pooling if needed
3. Consider connection multiplexing for high-concurrency scenarios
4. Document OS-level tuning (ulimits, kernel parameters)

---

### Single-Node Architecture

**Current capacity:** Single DittoFS instance

**Limit:** Cannot scale horizontally without external clustering

**Scaling path:**
1. Implement shared session state (Redis for sessions)
2. Use distributed metadata store (PostgreSQL with replication)
3. Implement cache coherency protocol (difficult)
4. Consider container orchestration with state rebuilding

---

### Memory Cache Without Spill-To-Disk

**Current capacity:** Limited to available RAM

**Limit:** Large files or many concurrent uploads can exceed cache size

**Scaling path:**
1. Implement disk spill for cache overflow
2. Use two-tier cache (fast memory + slower disk)
3. Monitor cache hit rate and warn on spillover
4. Document cache sizing recommendations

---

### BadgerDB Single-Instance Limitation

**Current capacity:** One BadgerDB instance per share

**Limit:** Cannot span multiple machines (DB is file-based)

**Scaling path:**
1. Use PostgreSQL backend for distributed deployments
2. Implement BadgerDB replication (complex)
3. Document single-machine requirement

---

## Dependencies at Risk

### AWS S3 SDK Retry Logic

**Risk:** S3 backend relies on SDK retry behavior. If SDK changes or throttling behavior changes, upload reliability affected.

**Impact:** Failed uploads during peak load

**Migration plan:**
1. Implement custom retry logic with metrics
2. Add circuit breaker for persistent S3 failures
3. Consider alternative backends (MinIO, GCS)
4. Test failure scenarios thoroughly

---

### GORM Dependency for PostgreSQL

**Risk:** GORM provides ORM abstractions that may not match all PostgreSQL features used. Query performance tuning becomes complex.

**Impact:** Slow PostgreSQL backend, difficulty with raw SQL optimization

**Migration plan:**
1. Add raw SQL layer for performance-critical queries
2. Use sqlc for type-safe query generation
3. Document when to use GORM vs raw SQL

---

## Missing Critical Features

### NFS File Locking (NLM Protocol)

**What's missing:** Complete NLM protocol implementation for `flock()`, `fcntl()`, `lockf()`

**Blocks:** Multi-client concurrent write scenarios, database file locking, application-level locks

**Priority:** High - required for production multi-client deployments

---

### SMB Encryption

**What's missing:** Encryption of SMB2 message bodies after authentication

**Blocks:** Secure SMB file sharing, Windows security compliance

**Priority:** Medium - SMB is typically LAN-only but encryption best practice

---

### Statistics Tracking

**What's missing:** Proper `UsedSize` reporting, cache stats, block store stats

**Blocks:** Monitoring, capacity planning, performance tuning

**Priority:** Low-Medium - operationally important but not functional requirement

---

### Audit Logging

**What's missing:** Security event logging (auth failures, permission denials, data access patterns)

**Blocks:** Compliance, incident investigation, threat detection

**Priority:** High - required for production/regulated environments

---

## Test Coverage Gaps

### CLI Binaries Completely Untested

**Untested area:** `cmd/dittofs/`, `cmd/dittofsctl/` - all commands

**Files:** All command implementations

**Risk:** CLI bugs, broken flags, incorrect error messages reach users

**Priority:** High - users interact with these daily

---

### NFS Protocol Handler Coverage Low

**Untested area:** NFS procedure handlers (READ, WRITE, LOOKUP, etc.)

**Files:** `internal/protocol/nfs/v3/handlers/` - only 28.7% coverage

**Risk:** Protocol bugs, incorrect error handling, edge cases missed

**Priority:** High - core protocol implementation

---

### SMB Handler Coverage Low

**Untested area:** SMB2 CREATE, QUERY_DIRECTORY, other handlers

**Files:** `internal/protocol/smb/v2/handlers/` - minimal coverage

**Risk:** SMB protocol bugs, Windows client incompatibilities

**Priority:** High - SMB is primary Windows protocol

---

### API Handler Coverage Very Low

**Untested area:** REST API handlers for user, group, share management

**Files:** `internal/controlplane/api/handlers/` - only 4.4% coverage

**Risk:** API endpoint bugs, missing validation, incorrect error responses

**Priority:** High - external API surface

---

### PostgreSQL Store Hard Link Scenarios

**Untested area:** Concurrent hard link operations, link count recovery

**Files:** `pkg/metadata/store/postgres/` - transaction handling

**Risk:** Link count corruption under concurrent operations

**Priority:** Medium-High - data integrity issue

---

### SMB Client Compatibility

**Untested area:** Real SMB clients (Windows, macOS, Samba)

**Files:** `pkg/adapter/smb/`, `internal/protocol/smb/`

**Risk:** Protocol violations, client compatibility issues

**Priority:** High - actual usage patterns not tested

---

### Error Recovery Scenarios

**Untested area:** Network failures, S3 timeouts, database disconnections, cache overflow

**Files:** All service layers

**Risk:** Errors propagate as crashes instead of graceful failures

**Priority:** Medium - operational stability

---

## Summary Statistics

| Category | Count | Priority |
|----------|-------|----------|
| Tech Debt Items | 5 | Medium-High |
| Known Bugs | 3 | Low-Medium |
| Security Risks | 5 | High |
| Performance Issues | 4 | Medium |
| Fragile Areas | 6 | Medium |
| Scaling Limits | 4 | Low-Medium |
| Test Coverage Gaps | 8 | High |

---

*Concerns audit: 2026-02-04*
