# Domain Pitfalls: v0.10.0 Production Hardening + SMB Protocol Fixes

**Domain:** Adding production hardening (quotas, client tracking, trash, payload stats) and completing SMB3 protocol compliance (credits, multi-channel, WPTS fixes, macOS signing) to an existing ~290K LOC multi-protocol filesystem server
**Researched:** 2026-03-20
**Confidence:** HIGH for pitfalls 1-5 (verified against MS-SMB2 spec, existing DittoFS source, and Samba wiki); HIGH for pitfalls 6-9 (based on codebase analysis and architecture understanding); MEDIUM for pitfalls 10-15 (domain experience + WebSearch verified patterns); MEDIUM for pitfalls 16-19 (integration concerns based on architecture analysis)

---

## Critical Pitfalls

Mistakes that cause client disconnections, data loss, protocol violations, or deadlocks that block core functionality.

### Pitfall 1: SMB Credit Grant Drops to Zero -- Client Deadlock

**What goes wrong:** The SMB2 specification states: "The server MUST ensure that the number of credits held by the client is never reduced to zero. If the condition occurs, there is no way for the client to send subsequent requests for more credits." A server that grants zero credits in any response permanently deadlocks that client session.

**Why it happens:** The existing DittoFS `GrantCredits` in `session/manager.go` already has a `MinGrant` of 16, but the adaptive algorithm applies multiplicative factors (load, client behavior, session outstanding) that could theoretically drive the result below `MinGrant` before the floor check. More dangerously, the current code returns `1` when a session is not found (line 168 in manager.go) -- but if session deletion races with the credit grant call, there is a window where the response carries 0 credits because the grant call returns before the response is assembled.

**Consequences:** Windows clients with zero credits cannot send any requests, including LOGOFF. The session hangs indefinitely. The only recovery is TCP connection reset, which triggers durable handle timeouts and potential data loss for uncommitted writes.

**Prevention:**
1. **Never return 0 from any credit grant path.** Add a final safety floor: `if grant == 0 { grant = 1 }` as the absolute last check in `GrantCredits`
2. **Validate at the wire level:** In `buildResponseHeaderAndBody` (response.go line 285), assert that `credits > 0` before encoding the response header. Log an error if the credit manager returned 0
3. **Credit validation in NEGOTIATE:** Per MS-SMB2, the server MUST grant at least 1 credit in the NEGOTIATE response. Verify this in tests
4. **Test with minimum credits:** Create a test scenario where the adaptive algorithm is under maximum load with aggressive clients -- verify credits never reach 0

**Detection:** Client connects, authenticates, then suddenly stops sending requests. Server shows no errors. Client side shows "network error" or "connection reset."

**Phase:** Must be addressed first when implementing credit flow control. Validate before any other credit changes.

### Pitfall 2: Credit Charge Validation Missing -- Allows Resource Exhaustion

**What goes wrong:** Per MS-SMB2 section 3.3.5.2.5, when `Connection.SupportsMultiCredit` is TRUE, the server MUST verify that the `CreditCharge` field matches the payload size. Currently, DittoFS calculates `CalculateCreditCharge` (credits.go line 101) but never validates incoming requests against it. The dispatch path in `ProcessSingleRequest` (response.go line 43-44) calls `RequestStarted/RequestCompleted` and `GrantCredits`, but never checks whether the client has enough credits or whether the `CreditCharge` matches the payload.

**Why it happens:** The credit system was designed for grant tracking, not enforcement. The existing code tracks credits for adaptive grant decisions but never rejects requests that exceed the client's credit balance. This is a common pattern in incremental development -- tracking before enforcement.

**Consequences:** A malicious or misconfigured client can send unlimited concurrent requests with `CreditCharge=0`, each requesting large payloads. Without enforcement, the server allocates unbounded memory and goroutines. This is a denial-of-service vector.

**Prevention:**
1. **Validate CreditCharge against payload:** Before dispatching any request, compute the expected credit charge from the payload/response size. If `CreditCharge < expected`, return `STATUS_INVALID_PARAMETER`
2. **Track outstanding credits per session:** Before consuming credits, verify `session.Outstanding >= creditCharge`. If not, return `STATUS_INSUFFICIENT_RESOURCES` (not spec-mandated but Windows Server does this)
3. **Special case for SMB 2.0.2:** CreditCharge MUST be 0 for dialect 2.0.2 -- do not validate payload against charge for this dialect
4. **Sequence number validation:** Per MS-SMB2 3.3.5.2.1, the server should verify that `MessageID` falls within the valid credit window. This prevents replay and out-of-window requests

**Detection:** Server memory grows unboundedly under load. Prometheus metrics (if enabled) show `activeRequests` far exceeding credit grants. Goroutine count climbs linearly with malicious client activity.

**Phase:** Implement credit charge validation immediately after the grant safety fix (Pitfall 1).

### Pitfall 3: Multi-Channel Session Binding Race With Session State

**What goes wrong:** SMB3 multi-channel allows a client to bind an existing session to a new TCP connection. The session state (tree connects, open files, leases, signing keys) must be shared across all connections. If the session binding and a concurrent operation on the original connection race, the server can observe partially initialized state -- for example, a WRITE on connection A references a tree that session binding on connection B is still setting up.

**Why it happens:** DittoFS currently uses a per-connection session model. The `Connection` struct (pkg/adapter/smb/connection.go) tracks sessions created on that connection. The `SessionManager` (internal/adapter/smb/session/manager.go) uses `sync.Map` for sessions, which is thread-safe at the key level, but session binding requires atomically associating an existing session with a new connection while preserving all its state including crypto keys and tree connects.

**Consequences:** Data corruption (per Samba 4.4.0 release notes: "corner cases in the treatment of channel failures that may result in data corruption when race conditions hit"). File handle confusion across connections. Lease break notifications routed to the wrong connection. Signing key mismatch if session keys are derived per-connection rather than per-session.

**Prevention:**
1. **Session-level mutex for binding:** Add a binding lock to `Session` that is held during the entire `SESSION_SETUP` with `SMB2_SESSION_FLAG_BINDING` flag. All operations on the session must acquire a read lock; binding acquires a write lock
2. **Session-connection registry is critical:** The existing `sessionConns sync.Map` maps sessionID to ConnInfo. With multi-channel, one sessionID maps to MULTIPLE ConnInfos. Replace `sync.Map` with a concurrent-safe multi-value map (e.g., `map[uint64][]*ConnInfo` protected by `sync.RWMutex`)
3. **Session key verification:** Per MS-SMB2 3.3.5.2.4, the client must authenticate with the same credentials when binding. Verify the session key matches before allowing binding
4. **Connection failure isolation:** When one channel fails, do NOT tear down the session. Only remove that connection from the session's channel list. This is the primary source of Samba's data corruption bugs
5. **Lease break fan-out:** With multi-channel, lease breaks should be sent on any available channel for the session, not just the original connection. Fall back to other channels if the preferred one is broken

**Detection:** Intermittent `STATUS_USER_SESSION_DELETED` errors on the second connection. Files appear corrupted when accessed from different connections simultaneously. Lease break notifications fail silently.

**Phase:** Multi-channel is the highest-risk feature in this milestone. Implement after credit flow control is stable. Consider shipping as experimental/opt-in.

### Pitfall 4: macOS Preauth Integrity Hash Mismatch -- Platform-Specific Byte Ordering

**What goes wrong:** The existing DittoFS preauth integrity hash chain (hooks.go) was validated against Windows 11 clients. macOS's SMB client (smbfs) may produce different NEGOTIATE request bytes due to different negotiate context ordering, different salt values, or different capability flags. If the server's hash chain accumulates different bytes than the client expects, session key derivation produces different keys and all subsequent signing/encryption fails.

**Why it happens:** The preauth hash is computed over the exact wire bytes. macOS and Windows clients send slightly different NEGOTIATE payloads (different dialect lists, different negotiate contexts ordering, different padding). The hash chain itself is correct, but if the server modifies or reserializes any bytes between receiving the NEGOTIATE and hashing it, the hash diverges. The existing code in `preauthHashBeforeHook` hashes `rawMessage` which is reconstructed (`hdr.Encode() + body`) rather than the original wire bytes, introducing potential byte differences.

**Consequences:** macOS clients can connect via SMB 2.x but fail when negotiating SMB 3.1.1. Users see "connection failed" or "the operation could not be completed." This is the exact issue referenced in PROJECT.md as issue #252.

**Prevention:**
1. **Hash the ORIGINAL wire bytes, not reconstructed bytes.** In `Connection.Serve()` (connection.go line 191-193), the code reconstructs rawMessage from `hdr.Encode() + body`. This is the bug vector. Instead, pass the original undecoded bytes from `ReadRequest` to the hooks
2. **Capture wire bytes before parsing.** Modify `ReadRequest` to return the raw message bytes alongside the parsed header/body. The hooks need exact bytes, not re-encoded versions
3. **Test with macOS Sonoma/Sequoia.** The macOS SMB client sends negotiate contexts in a different order than Windows. Create a specific E2E test with a macOS SMB mount
4. **Log and compare hash chains.** Add debug logging of the hash value at each step. Capture a macOS packet trace and manually verify the hash chain matches

**Detection:** macOS clients fail to connect with SMB 3.1.1 but succeed when forced to SMB 2.x (`nsmb.conf` setting `smb_neg=smb2_only`). Windows clients work fine.

**Phase:** This should be the FIRST fix attempted because it is a known bug (#252) and the fix informs the architecture of hook/framing interaction for all other SMB work.

### Pitfall 5: WPTS Conformance Fixes Regress Existing Passing Tests

**What goes wrong:** Fixing one WPTS known failure causes a previously passing test to fail. This is common in SMB conformance because tests share server state -- a change to how leases are handled to fix BVT_Leasing_FileLeasingV2 may break BVT_DurableHandleV1_Reconnect_WithLeaseV1 because the durable handle reconnect path depends on specific lease break timing.

**Why it happens:** The current WPTS suite has 193 passing, 73 known failures, and 69 skipped tests. Many of the 73 known failures are in categories (leasing, durable handles, negotiate, ADS) that have deep interactions with passing tests. The WPTS tests run sequentially within a category but the test runner does not fully isolate server state between tests. Shared state includes: session table, tree connects, open file handles, lease table, durable handle store.

**Consequences:** Each conformance fix is a net-zero or net-negative change. The team spends time on whack-a-mole where fixing one test breaks another. CI becomes unreliable as the known failures list requires constant updating.

**Prevention:**
1. **Run the FULL WPTS suite after every change, not just the target test.** The existing `test/smb-conformance/run.sh` and `parse-results.sh` infrastructure supports this. Never merge a conformance fix without a clean full-suite run
2. **Categorize known failures by root cause, not by test name.** The current KNOWN_FAILURES.md lists tests by category. Group them by root cause instead: "lease break timing" (6 tests), "negotiate context ordering" (5 tests), etc. Fix root causes, not individual tests
3. **Use git bisect for regressions.** When a previously passing test fails, bisect immediately. The regression is often in the most recent 1-2 commits
4. **Isolate server state between test categories.** If feasible, restart the DittoFS server between WPTS test categories. This eliminates cross-test state pollution
5. **Pin WPTS version.** The Microsoft WindowsProtocolTestSuites repo receives updates. Pin to a specific commit in `docker-compose.yml` to avoid surprise test changes

**Detection:** CI passes on the target test but fails on a previously passing test. The `parse-results.sh` script will catch this because it reports NEW failures not in the known list.

**Phase:** Every WPTS conformance fix phase must include a full regression run. Budget 30% of conformance time for regression investigation.

---

## Moderate Pitfalls

Mistakes that cause performance degradation, incorrect behavior, or maintenance burden without blocking core functionality.

### Pitfall 6: Share Quota Enforcement TOCTOU Between Check and Write

**What goes wrong:** Quota enforcement follows a check-then-act pattern: read current usage, compare to limit, proceed with write. Between the check and the write, another concurrent request may have consumed the remaining space. Both requests pass the check, both write, and the share exceeds its quota.

**Why it happens:** DittoFS processes requests concurrently (goroutine per request). Multiple WRITE operations to the same share can be in-flight simultaneously. The metadata store tracks file sizes but does not provide atomic "increment-and-check" operations for quota tracking.

**Consequences:** Quotas are eventually consistent rather than strictly enforced. A share configured with 1GB quota might temporarily hold 1.1GB of data. For most production use cases this is acceptable (quotas are advisory), but if strict enforcement is required, the TOCTOU window is a correctness bug.

**Prevention:**
1. **Use an atomic counter for quota tracking.** Add a per-share `atomic.Int64` for `usedBytes` that is atomically incremented before the write and decremented on failure. This is faster than holding a mutex across the entire write path
2. **Accept eventual consistency as the default.** Document that quotas are "soft" by default. The counter is periodically reconciled with actual usage from the metadata store
3. **Reject on threshold, not exact limit.** Reject writes when usage exceeds `quota * 0.99`, leaving a 1% buffer for races. This turns the TOCTOU window into a non-issue for practical purposes
4. **Cross-protocol consistency:** Both NFS WRITE and SMB WRITE must check the same quota counter. Place enforcement in the metadata service layer, not in protocol handlers

**Detection:** Monitoring shows share usage exceeding configured quota by a small percentage. No user-visible errors.

**Phase:** Implement quota tracking (atomic counter) early, enforcement later. The tracking infrastructure is needed for FSSTAT/FSINFO reporting regardless.

### Pitfall 7: Payload Stats (UsedSize) Performance Regression from Scanning

**What goes wrong:** Computing actual storage usage (UsedSize) requires scanning all files in the metadata store to sum their sizes, or querying the block store for actual disk usage. If this computation runs on every FSSTAT/FSINFO request, it creates a hot path that scales linearly with file count.

**Why it happens:** The current `GetFilesystemStatistics` in the memory store (server.go line 180) calls `computeStatistics()` which iterates all files. For BadgerDB, the implementation counts entries and sums file sizes. For PostgreSQL, it runs aggregate queries. None of these are O(1).

**Consequences:** NFS FSSTAT and SMB QUERY_INFO for `FileFsFullSizeInformation` become slow as the share grows. With 100K files, each FSSTAT call scans 100K metadata entries. Some NFS clients call FSSTAT before every WRITE (to check available space), creating a multiplication effect.

**Prevention:**
1. **Maintain a running counter.** Track `usedBytes` as an in-memory counter updated on every write/truncate/delete. Persist it periodically (every N seconds or every N operations) rather than recomputing from scratch
2. **Cache with TTL.** If running counters are too complex, cache the computed statistics with a 5-10 second TTL. Most clients tolerate slightly stale usage data
3. **Separate block store usage from metadata size.** `UsedSize` should reflect actual block store disk usage (which the block store already tracks), not sum-of-file-sizes from metadata. The block store engine already has size tracking via `Cache.MaxSize()` and similar
4. **Do not block writes on stats computation.** Stats computation must not hold any lock that write paths need. Use copy-on-write or atomic snapshots

**Detection:** `FSSTAT` response time increases linearly with file count. NFS clients show slow `df` output. Prometheus histograms for FSSTAT latency show p99 growing over time.

**Phase:** Address when implementing payload stats. The running counter should be added to the metadata service before exposing it via FSSTAT.

### Pitfall 8: Client Tracking Memory Leak from Stale Entries

**What goes wrong:** Protocol-agnostic client tracking records (`ClientRecord`) accumulate in memory for clients that connected, did some work, and disconnected without clean logout. If the server does not actively garbage-collect stale entries, the client registry grows unboundedly over weeks of uptime.

**Why it happens:** NFS clients (especially NFSv3 over UDP) do not have explicit session teardown. SMB clients that lose network connectivity skip LOGOFF. The existing NFS v4.1 session manager has lease-based expiry, but NFSv3 mounts tracked as `ClientRecord` do not. The SMB session manager deletes sessions on LOGOFF or connection close, but the proposed `ClientRecord` is at a higher level of abstraction.

**Consequences:** Memory usage grows linearly with cumulative unique clients over server lifetime. After weeks of operation, the client registry consumes significant memory and `dfsctl client list` returns an unwieldy number of entries.

**Prevention:**
1. **TTL-based expiry.** Every `ClientRecord` has a `LastSeen` timestamp updated on each request. A background goroutine sweeps entries older than a configurable TTL (default: 24 hours). This is the pattern used by the existing NFSv4 lease manager
2. **Cap the registry size.** Set a maximum number of tracked clients (e.g., 10,000). When the limit is reached, evict the oldest entry. This provides a hard upper bound on memory
3. **Distinguish active vs historical.** Active clients (with open sessions/mounts) are never evicted. Historical clients (disconnected) are subject to TTL and cap limits
4. **Atomic updates.** Use `sync.Map` or a sharded map for the client registry, consistent with the existing `sessionConns` pattern. Do not introduce a global mutex on the hot path

**Detection:** Memory usage grows linearly over time even with constant client count. `dfsctl client list` response time increases. Prometheus gauge for tracked clients never decreases.

**Phase:** Implement TTL expiry from the start. Do not ship client tracking without a cleanup mechanism.

### Pitfall 9: Trash Soft-Delete Interacts Badly with Block Store GC

**What goes wrong:** When a file is moved to trash (soft-deleted), its metadata is preserved with a "deleted" flag, but the block store GC may garbage-collect the file's blocks because no "live" file references them. When the user tries to restore the file, the metadata exists but the blocks are gone.

**Why it happens:** The block store GC (pkg/blockstore/gc/) identifies unreferenced blocks by scanning live metadata. If the GC does not know about trashed files, it considers their blocks as unreferenced and deletes them. The GC runs asynchronously and does not coordinate with the trash retention system.

**Consequences:** Files restored from trash have zero-length content or corrupt data. This is a silent data loss scenario that only manifests when a user actually tries to restore a file.

**Prevention:**
1. **Trashed files are still "live" for GC purposes.** The GC must scan both live files AND trashed files when determining block references. Add trash entries to the GC's reference scan
2. **Alternative: Move blocks to a "trash" prefix.** Instead of keeping blocks in their original location, move them to a trash-specific storage path. The GC ignores the trash path entirely. This is simpler but doubles the storage during retention
3. **Purge coordination.** When trash retention expires and the file is permanently deleted, THEN the GC can reclaim blocks. Implement a trash-specific purge goroutine that permanently deletes trashed files after retention expires, which then makes their blocks eligible for GC
4. **Test the full cycle:** Create file -> write data -> delete file (goes to trash) -> wait for GC -> restore from trash -> verify data intact. This test must be in the E2E suite

**Detection:** Files restored from trash have size 0 or return I/O errors on read. Server logs show "block not found" errors during restore operations.

**Phase:** Design trash and GC coordination BEFORE implementing trash. The GC integration is the hardest part of trash, not the metadata flagging.

### Pitfall 10: Trash Cross-Protocol Visibility Inconsistency

**What goes wrong:** An NFS client deletes a file (which moves it to trash). An SMB client listing the directory no longer sees the file (correct). But when the NFS client tries to list the trash contents, the SMB client cannot see the trash at all because SMB does not expose a trash protocol concept. Conversely, an SMB client using `DELETE_ON_CLOSE` flag should bypass trash entirely, but the server's trash interception does not distinguish between SMB delete semantics and NFS unlink semantics.

**Why it happens:** Trash is not a protocol-level concept in either NFS or SMB. It is a server-side feature implemented at the metadata layer. The "move to .trash" operation must be invisible to protocol handlers, but different protocols have different delete semantics (NFS `REMOVE` vs SMB `CLOSE` with `DELETE_ON_CLOSE`, NFS `RMDIR` vs SMB `CLOSE` on a directory with `DELETE_ON_CLOSE`).

**Consequences:** Users see different behavior depending on which protocol they use. Files deleted via NFS appear in a `.trash` directory visible to SMB clients (confusing). Files deleted via SMB with `DELETE_ON_CLOSE` are supposed to be immediately deleted but end up in trash (wrong).

**Prevention:**
1. **Implement trash at the metadata service layer, not at the protocol handler layer.** The `RemoveFile` method in `pkg/metadata/` should check trash configuration and decide whether to soft-delete or hard-delete
2. **Honor protocol intent:** SMB `DELETE_ON_CLOSE` is an explicit "delete when I close this handle" directive. It should bypass trash. NFS `REMOVE` is a standard delete that should go through trash
3. **Hide the trash directory from normal listings.** The `.trash` prefix should be filtered from `READDIR`/`READDIRPLUS` and `QUERY_DIRECTORY` results. Only expose trash contents via the `dfsctl` CLI or REST API
4. **Per-share trash configuration.** Some shares may want trash (user home directories), others may not (temp directories). Make trash a per-share setting, not global

**Detection:** Users report seeing a `.trash` directory in their file manager. Files deleted with `DELETE_ON_CLOSE` are not immediately freed.

**Phase:** Design cross-protocol semantics before implementing trash metadata changes.

### Pitfall 11: Credit Flow Control Breaks Compound Requests

**What goes wrong:** Compound requests (multiple SMB2 commands in a single TCP message) share a single credit charge for the first command but each subsequent related command does not have its own credit charge. The existing compound processing in `compound.go` calls `GrantCredits` for each error response (line 314-318) but does not track per-compound credit accounting. Adding strict credit enforcement may reject valid compound requests.

**Why it happens:** Per MS-SMB2 3.3.5.2.7, "The server MUST consume 1 credit for each request in the compound chain." But the current code treats each command in a compound as having its own `CreditCharge` from the header, which is only set in the first command's header. Related commands inherit the first command's header fields but their individual `CreditCharge` values may be 0.

**Consequences:** After adding credit enforcement, compound requests (used heavily by Windows Explorer for `CREATE + QUERY_INFO + CLOSE` sequences) start failing with `STATUS_INSUFFICIENT_RESOURCES` or `STATUS_INVALID_PARAMETER`.

**Prevention:**
1. **Charge credits at the compound level, not per-command.** The credit charge for a compound is the sum of individual charges OR the first command's `CreditCharge` (whichever the implementation chose). Validate at the compound entry point, not per-command
2. **Grant credits only in the last response of a compound.** Per MS-SMB2, the server returns credits in the last response of a compound chain, not in each intermediate response
3. **Test with Windows Explorer compound patterns.** Explorer commonly sends `CREATE+CLOSE`, `CREATE+QUERY_INFO+CLOSE`, and `CREATE+SET_INFO+CLOSE` compounds. Validate these work correctly with credit enforcement

**Detection:** Windows Explorer fails to list directory contents or open files. Error is `STATUS_INSUFFICIENT_RESOURCES` in the second or third command of a compound.

**Phase:** Test compound request handling thoroughly when adding credit enforcement. Use smbtorture's compound tests.

### Pitfall 12: Multi-Channel Lease Break Notification Fan-Out Failure

**What goes wrong:** With multi-channel, a session has multiple TCP connections. Lease break notifications must be sent on a connection that the client is actively reading from. If the server picks a connection that has a full send buffer or is in a broken TCP state, the lease break notification is lost. The client continues using stale cached data, leading to silent data corruption.

**Why it happens:** The existing `transportNotifier` in `adapter.go` uses `sessionConns sync.Map` to route lease breaks to a specific ConnInfo. With multi-channel, there are multiple ConnInfo per session. The notifier must try all channels, not just the first one found.

**Consequences:** Lease breaks are silently lost. The SMB client continues reading stale cached data while another client has modified the file. This is the cross-channel data corruption scenario that Samba documented in their 4.4.0 release.

**Prevention:**
1. **Try all channels for lease break delivery.** If the first channel fails, fall back to other channels in the session. Only give up if ALL channels fail
2. **Use a round-robin or least-loaded channel selection.** Do not always pick the same channel for breaks -- distribute break notifications across channels
3. **Timeout for break acknowledgment.** If no ACK is received within the lease timeout, forcibly break the lease server-side and revoke caching
4. **Replay missing breaks on channel recovery.** Per Samba bug #11897, "SMB3 multichannel implementation is missing oplock/lease break request replay." When a channel reconnects, replay any unacknowledged lease breaks

**Detection:** File content diverges between two clients using different channels. Lease break metrics show delivery failures but no retry attempts.

**Phase:** Implement alongside multi-channel session binding. Cannot be deferred.

---

## Minor Pitfalls

Issues that cause suboptimal behavior, minor inconsistencies, or increased maintenance burden.

### Pitfall 13: Quota Reporting Inconsistency Between NFS and SMB

**What goes wrong:** NFS FSSTAT reports quotas in bytes (TotalSize, AvailableSize) while SMB `FileFsFullSizeInformation` reports in allocation units (TotalAllocationUnits, ActualAvailableAllocationUnits, CallerAvailableAllocationUnits). If the conversion between bytes and allocation units is inconsistent, clients see different available space depending on which protocol they use.

**Prevention:**
1. Store quotas in bytes internally
2. Convert to allocation units at the SMB handler level using a consistent unit size (typically 4096 bytes)
3. Test that NFS `df` and Windows Explorer "Properties" show the same available space for the same share

### Pitfall 14: Client Tracking Double-Counts Multi-Protocol Clients

**What goes wrong:** A client machine that mounts the same share via both NFS and SMB appears as two separate `ClientRecord` entries. The `dfsctl client list` output is confusing because it shows the same machine twice with different protocol labels.

**Prevention:**
1. Use client IP address as the primary key for `ClientRecord`, not protocol-specific session IDs
2. Track protocols as a set within the record: `protocols: [nfs, smb]`
3. Aggregate bandwidth and operation counts across protocols
4. Allow filtering by protocol in `dfsctl client list --protocol nfs`

### Pitfall 15: Trash Retention Purge Races with Block Store Sync

**What goes wrong:** The trash purge goroutine permanently deletes a file's metadata, making its blocks eligible for GC. But the block syncer (pkg/blockstore/sync/) may be in the middle of uploading those blocks to the remote store. The sync completes, uploading blocks that are immediately garbage-collected.

**Prevention:**
1. Purge order: delete remote blocks first, then local blocks, then metadata
2. Wait for any in-flight sync operations to complete before purging blocks
3. Use the existing syncer's flush mechanism to ensure all pending uploads are complete before purge starts

### Pitfall 16: WPTS Test Infrastructure Docker Image Staleness

**What goes wrong:** The WPTS Docker image (`test/smb-conformance/Dockerfile.dittofs`) builds against a specific version of the Microsoft test suite. If the test suite is updated upstream, the baseline results become invalid and new tests may appear that are neither in the passing nor known-failures list.

**Prevention:**
1. Pin the WindowsProtocolTestSuites commit hash in the Dockerfile
2. When updating the pin, run a full baseline comparison and update KNOWN_FAILURES.md
3. Add the pinned commit hash to CI output so regressions from upstream changes are obvious

### Pitfall 17: Signing Fix Changes Preauth Hash for ALL Clients

**What goes wrong:** Fixing the macOS signing issue (#252) by changing how wire bytes are captured in hooks will also change the preauth hash computation for Windows clients. If the fix introduces a subtle byte difference that was previously masked by the reconstruct-then-hash approach, Windows clients may break.

**Prevention:**
1. Capture wire bytes AND reconstructed bytes in debug mode. Compare them byte-for-byte
2. Run both WPTS and macOS E2E tests after any change to the preauth hash path
3. If wire bytes differ from reconstructed bytes, that IS the bug -- fix the byte capture, do not change the reconstruction

### Pitfall 18: Payload Stats Diverge from Block Store Reality

**What goes wrong:** The `UsedSize` metric tracks file sizes from metadata, but the actual disk usage in the block store may differ due to: content-addressed deduplication (multiple files share the same blocks), sparse file holes (metadata says 1GB but only 100MB of blocks exist), or pending sync (blocks are in local store but not yet uploaded to remote).

**Prevention:**
1. Report TWO sizes: logical size (sum of file sizes from metadata) and physical size (actual block store disk usage)
2. For quota purposes, use logical size (this is what users expect)
3. For capacity planning, expose physical size via REST API and `dfsctl`
4. Document the distinction clearly in FSSTAT/FSINFO responses

### Pitfall 19: Connection-per-Session Assumption Baked into Adapter Architecture

**What goes wrong:** The current SMB adapter architecture assumes one session lives on one connection. This assumption is embedded in: `Connection.sessions` map (connection.go line 32), `cleanupSessions` on connection close (connection.go line 250), `sessionConns sync.Map` mapping sessionID to single ConnInfo (adapter.go line 64), and `connRegistryTracker` that replaces the previous ConnInfo on session creation. Adding multi-channel requires changing all of these simultaneously, and missing one creates subtle bugs.

**Prevention:**
1. **Audit all session-connection coupling.** Search for every place that maps sessionID to a single connection/ConnInfo. Document each one before starting multi-channel work
2. **Introduce a `ChannelSet` abstraction.** Instead of `sessionConns sync.Map[uint64]*ConnInfo`, create `sessionChannels sync.Map[uint64]*ChannelSet` where `ChannelSet` manages multiple connections for a session
3. **Do not change `Connection.sessions` semantics.** Each `Connection` still tracks which sessions it hosts. But a session can appear in multiple `Connection.sessions` maps. The `cleanupSessions` logic must check whether other connections still reference the session before deleting it
4. **Feature flag.** Add a `multichannel_enabled` config option (default: false). When false, reject `SESSION_SETUP` with `SMB2_SESSION_FLAG_BINDING`. This allows shipping multi-channel incrementally

**Detection:** Session cleanup on connection close deletes sessions that are still active on other connections. Open files become inaccessible.

**Phase:** This is architectural preparation that should happen before implementing multi-channel itself.

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| macOS signing fix | Pitfall 4 (byte mismatch), Pitfall 17 (regression to Windows) | Capture raw wire bytes, test both platforms |
| Credit flow control | Pitfall 1 (zero credits), Pitfall 2 (no enforcement), Pitfall 11 (compound breakage) | Floor at 1, validate charge, test compounds |
| Multi-channel | Pitfall 3 (session binding race), Pitfall 12 (lease break fan-out), Pitfall 19 (architecture coupling) | Binding lock, channel set abstraction, feature flag |
| Share quotas | Pitfall 6 (TOCTOU), Pitfall 7 (scanning perf), Pitfall 13 (NFS/SMB inconsistency) | Atomic counter, cached stats, consistent units |
| Payload stats | Pitfall 7 (scanning perf), Pitfall 18 (divergence from reality) | Running counter, logical vs physical distinction |
| Client tracking | Pitfall 8 (memory leak), Pitfall 14 (double counting) | TTL expiry, IP-based keying |
| Trash / soft-delete | Pitfall 9 (GC interaction), Pitfall 10 (cross-protocol), Pitfall 15 (sync race) | GC-aware trash, protocol-specific semantics, purge ordering |
| WPTS conformance | Pitfall 5 (regression), Pitfall 16 (Docker staleness) | Full suite runs, pinned versions, root-cause grouping |

---

## Sources

- [MS-SMB2: Verifying the Credit Charge and the Payload Size](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fba3123b-f566-4d8f-9715-0f529e856d25) -- HIGH confidence, official specification
- [MS-SMB2: Algorithm for the Granting of Credits](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/2e366edb-b006-47e7-aa94-ef6f71043ced) -- HIGH confidence, official specification
- [Samba Wiki: SMB2 Credits](https://wiki.samba.org/index.php/SMB2_Credits) -- HIGH confidence, verified against spec
- [Samba 4.4.0 Release Notes](https://www.samba.org/samba/history/samba-4.4.0.html) -- HIGH confidence, documents multi-channel data corruption risk
- [Samba Bug #11897: SMB3 multichannel missing oplock/lease break replay](https://bugzilla.samba.org/show_bug.cgi?id=11897) -- MEDIUM confidence, Samba-specific but applicable
- [SMB 3.1.1 Pre-authentication integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- HIGH confidence, official Microsoft documentation
- [SNIA: SMB3 Multi-Channel and Beyond (Samba)](https://www.snia.org/sites/default/files/SDC/2016/presentations/smb/Michael_Adam_SMB3_in_Samba_Multi-Channel_and_Beyond.pdf) -- MEDIUM confidence, implementation experience
- [TOCTOU Race Conditions (Wikipedia)](https://en.wikipedia.org/wiki/Time-of-check_to_time-of-use) -- HIGH confidence, well-established concept
- [Azure Blob Soft Delete Overview](https://learn.microsoft.com/en-us/azure/storage/blobs/soft-delete-blob-overview) -- MEDIUM confidence, design pattern reference
- DittoFS source code analysis: `internal/adapter/smb/session/`, `internal/adapter/smb/compound.go`, `internal/adapter/smb/response.go`, `internal/adapter/smb/hooks.go`, `pkg/adapter/smb/connection.go`, `pkg/adapter/smb/adapter.go` -- HIGH confidence, direct code review
