# NFSv4 Server Implementation Pitfalls

**Domain:** NFSv4 Protocol Server (adding to existing NFSv3 implementation)
**Researched:** 2026-02-04
**Confidence:** MEDIUM-HIGH (based on RFCs, Linux kernel docs, vendor documentation, and community bug reports)

## Critical Pitfalls

Mistakes that cause rewrites, data corruption, or major interoperability failures.

### Pitfall 1: Stateid Sequence Number Mismanagement

**What goes wrong:**
Server fails to properly increment stateid sequence numbers (seqid) at each state transition, or client/server seqids get out of sync. Results in "bad sequence-id" errors flooding logs, clients hanging, and potential data corruption from replayed operations.

**Why it happens:**
- NFSv4 uses seqids to detect duplicate/replayed requests and ensure exactly-once semantics
- Multiple state types (open-owner, lock-owner) each have independent seqids
- Wraparound handling at 2^32 is often forgotten
- RELEASE_LOCKOWNER not implemented, exhausting server stateids

**How to avoid:**
1. Maintain separate seqid tracking per open-owner AND per lock-owner (they are distinct)
2. Increment seqid monotonically at EVERY state transition, not just some
3. Implement proper wraparound handling (accounting for unsigned overflow)
4. Implement RELEASE_LOCKOWNER to allow clients to clean up lock state
5. Map NFS4ERR_RESOURCE to proper seqid-incrementing behavior per RFC 3530

**Warning signs:**
- Log messages containing "bad sequence-id" or NFS4ERR_BAD_SEQID
- Clients repeatedly sending same operations
- State accumulation without cleanup
- Tests passing individually but failing under concurrent load

**Phase to address:**
Phase 1 (Core State Management) - This is foundational; getting it wrong invalidates all subsequent work.

---

### Pitfall 2: Grace Period Violations

**What goes wrong:**
Server grants new locks or allows READ/WRITE operations during the grace period after restart, violating protocol requirements. Can lead to data corruption when pre-crash locks are "stolen" by other clients.

**Why it happens:**
- Grace period logic is complex with many edge cases
- Desire to "be responsive" leads to skipping grace period checks
- Not tracking which clients are allowed to reclaim
- Ending grace period too early or not at all
- Not properly rejecting non-reclaim operations with NFS4ERR_GRACE

**How to avoid:**
1. During grace period: MUST reject READ/WRITE and non-reclaim LOCK/OPEN with NFS4ERR_GRACE
2. Maintain stable storage record of active clients at shutdown
3. Only allow clients from pre-restart list to reclaim state
4. Support RECLAIM_COMPLETE (NFSv4.1) to allow early grace period termination when all clients done
5. Default grace period: 90 seconds; must be >= lease period

**Warning signs:**
- New clients able to acquire locks immediately after server restart
- Pre-existing clients losing locks they should have reclaimed
- No NFS4ERR_GRACE errors visible during restart testing
- Tests passing only when run in isolation

**Phase to address:**
Phase 2 (State Persistence & Recovery) - Requires stable storage foundation from Phase 1.

---

### Pitfall 3: Pseudo-Filesystem / Export Path Confusion

**What goes wrong:**
NFSv3 exports work differently than NFSv4 exports. Servers that expose exports the same way for both protocols create broken mount paths, missing directories, or inaccessible shares.

**Why it happens:**
- NFSv4 has a "pseudo-filesystem" concept where all exports are children of a virtual root
- What was `server:/export/users` in NFSv3 becomes `server:/users` in NFSv4 (with `/export` as fsid=0 root)
- DittoFS already has exports configured for NFSv3; naive NFSv4 addition will break paths
- Multiple filesystems cannot have fsid=0

**How to avoid:**
1. Designate ONE export as fsid=0 (pseudo-root) or let server auto-generate pseudo-filesystem
2. Document the path transformation clearly for users
3. Consider supporting both NFSv3-style full paths AND NFSv4-style relative paths
4. Test mount commands for BOTH protocol versions against same exports
5. Pseudo-filesystem filehandles should be expected to be volatile

**Warning signs:**
- NFSv3 mounts work but NFSv4 mounts fail with "No such file or directory"
- Clients seeing different directory hierarchies between protocol versions
- `showmount -e` showing different exports than NFSv4 clients can access
- Mount paths requiring different syntax for v3 vs v4

**Phase to address:**
Phase 1 (Protocol Foundation) - Must be designed correctly from the start; changing later breaks all clients.

---

### Pitfall 4: Lease Expiration Race Conditions

**What goes wrong:**
Race conditions between lease expiration checks and new client operations cause state corruption, lost locks, or denial of service. Either locks are incorrectly revoked from active clients, or expired state is not cleaned up properly.

**Why it happens:**
- Lease renewal can happen on ANY operation (implicit renewal), not just explicit RENEW
- Network delays can make active clients appear expired
- Multiple goroutines checking/modifying lease state without proper synchronization
- Aggressive cleanup removing state before client can renew
- Courteous server behavior (keeping expired state until conflict) not implemented

**How to avoid:**
1. Use atomic operations or proper locking for lease timestamp updates
2. Implement implicit lease renewal on ANY state-modifying operation
3. Default lease period: 30-90 seconds; renewal should happen at half the period
4. Consider implementing "Courteous Server" behavior - keep expired state until conflict
5. When revoking expired state that conflicts with new request, revoke atomically

**Warning signs:**
- Spurious lock loss during normal operations
- "Expired lease" errors on clients that were actively using files
- Memory growth from never-cleaned state
- Race condition failures in stress tests

**Phase to address:**
Phase 2 (State Management) - After basic state structures exist but before production use.

---

### Pitfall 5: Delegation Callbacks Failing in NAT/Container Environments

**What goes wrong:**
NFSv4.0 delegations require the server to initiate TCP connections BACK to the client for recall. This fails completely behind NAT, firewalls, or in containerized environments where the client IP is not routable from the server.

**Why it happens:**
- Callback address is embedded in NFS requests by client
- NAT rewrites source addresses, making callback address unreachable
- Container networking adds another layer of address translation
- Firewalls block server-initiated connections to clients
- Many implementations just silently fail to delegate

**How to avoid:**
1. For NFSv4.0: Detect when callback path is non-functional and disable delegations for that client
2. Implement NFSv4.1 which uses "backchannel" on SAME TCP connection (no NAT issues)
3. If supporting NFSv4.0 callbacks: make callback port configurable
4. Test callback functionality explicitly before granting delegations
5. Have graceful fallback when callbacks fail (just don't delegate)

**Warning signs:**
- Delegations granted but never recalled (stale data served)
- "Callback channel down" errors
- Delegations working in development but failing in production
- Performance degradation when delegations should help

**Phase to address:**
Phase 3 (Delegations) - Can be deferred; system works without delegations, just less efficiently.

---

### Pitfall 6: Lock Owner vs Open Owner Conflation

**What goes wrong:**
Implementation treats open-owners and lock-owners as the same entity, causing state corruption, failed lock operations, and seqid synchronization failures.

**Why it happens:**
- Both are "owners" in NFSv4 terminology
- Both have seqids that need tracking
- Tempting to unify for simpler code
- RFC uses similar language for both

**How to avoid:**
1. Maintain SEPARATE data structures for open-owners and lock-owners
2. Open-owners are tied to file opens (OPEN/CLOSE operations)
3. Lock-owners are tied to byte-range locks (LOCK/LOCKU operations)
4. Each has independent seqid tracking
5. Lock-owners can exist without open-owners in some edge cases
6. FreeBSD fix: Add filehandle to lock_owner string to make it per-process-per-file

**Warning signs:**
- LOCK operations returning unexpected seqid errors
- State corruption when same process has multiple opens and locks
- Tests with single file/process passing but multi-file failing
- RELEASE_LOCKOWNER affecting CLOSE operations incorrectly

**Phase to address:**
Phase 1 (Core State Management) - Fundamental data structure decision.

---

### Pitfall 7: Kerberos/RPCSEC_GSS Clock Skew and DNS Issues

**What goes wrong:**
Kerberos authentication fails intermittently or completely due to clock skew, DNS resolution issues, or /etc/hosts misconfiguration. Error messages are often cryptic ("GSS_S_FAILURE").

**Why it happens:**
- Kerberos has 5-minute default clock skew tolerance
- gssd daemon looks up its own IP and reports it to KDC; if /etc/hosts has 127.0.0.1 for hostname, authentication fails
- Reverse DNS must work for Kerberos principals
- rpcsec_gss_krb5 kernel module must be loaded
- Keytab files must have correct principals (nfs/hostname@REALM)

**How to avoid:**
1. Document NTP/time synchronization as a hard requirement
2. Verify /etc/hosts does not map hostname to 127.0.0.1
3. Require working forward AND reverse DNS for all hosts
4. Test Kerberos completely separately before integrating with NFS
5. Provide clear error messages that point to common causes
6. Consider making Kerberos optional and clearly documenting AUTH_SYS limitations

**Warning signs:**
- Authentication works sometimes but not others
- "Too many open files" errors from gssd
- "GSS_S_FAILURE" in logs without clear cause
- Authentication working from some clients but not others
- Works in dev (local network) but fails in production

**Phase to address:**
Phase 4 (Security) - Kerberos is optional but complex; defer until core functionality works.

---

## Technical Debt Patterns

Shortcuts that seem reasonable but create long-term problems.

| Shortcut | Immediate Benefit | Long-term Cost | When Acceptable |
|----------|-------------------|----------------|-----------------|
| Skip NFSv4.1 sessions, implement only NFSv4.0 | Simpler state model | NAT callback issues, no exactly-once semantics, less efficient | Never for new implementations |
| Ignore DENY share reservations | POSIX compatible, simpler | Windows clients fail, cross-protocol locking broken | Acceptable if Windows/SMB not a target |
| Volatile filehandles only | No persistence requirements | Clients must cache path->handle mappings, more traffic | Only for pseudo-filesystem, not real files |
| Single-threaded state management | No lock contention | Performance bottleneck, won't scale | Early prototyping only |
| Skip compound operation error handling | Simpler per-operation logic | Partial failures leave inconsistent state | Never |
| Hardcode lease/grace periods | Fewer configuration options | Cannot tune for different environments | Early development only |

## Integration Gotchas

Common mistakes when connecting NFSv4 to existing DittoFS components.

| Integration | Common Mistake | Correct Approach |
|-------------|----------------|------------------|
| NFSv3 exports | Exposing same paths for v3 and v4 | v4 needs pseudo-filesystem; paths are relative to fsid=0 root |
| MetadataStore | Using NFSv3 filehandle format | NFSv4 needs stateid embedded or associated; handles may need to be persistent |
| BlockService | Ignoring LAYOUTGET/LAYOUTRETURN (pNFS) | Either implement pNFS or clearly reject layout operations |
| Cache layer | Not coordinating with delegations | Delegation state must inform cache invalidation |
| WAL | Not persisting NFSv4 state | State must survive restart for grace period recovery |
| SMB adapter | Independent locking | Must coordinate locks across protocols or clients will corrupt data |

## Performance Traps

Patterns that work at small scale but fail as usage grows.

| Trap | Symptoms | Prevention | When It Breaks |
|------|----------|------------|----------------|
| Global lock for state operations | High latency under load | Per-client or per-file locking | >100 concurrent clients |
| Linear client ID lookup | Slow SETCLIENTID/EXCHANGE_ID | Hash map for client IDs | >1000 clients |
| Unbounded state storage | Memory exhaustion | Lease expiration + cleanup | Long-running server with client churn |
| Per-operation stable storage sync | Disk I/O bottleneck | Batched writes, async where safe | >10,000 ops/sec |
| TEST_STATEID polling overhead | Network saturation | Proper error handling to avoid polling | Interrupted connections under load |
| Single callback thread | Delegation recalls block | Thread pool for callbacks | >100 delegations |

## Security Mistakes

Domain-specific security issues beyond general NFS security.

| Mistake | Risk | Prevention |
|---------|------|------------|
| Trusting client-asserted UID/GID with AUTH_SYS | Any client can impersonate any user | Use Kerberos or document limitation clearly |
| Not validating stateid ownership | Client can operate on another client's state | Always verify stateid belongs to requesting client |
| Exposing pseudo-filesystem structure | Information disclosure of server layout | Limit pseudo-fs to necessary exports only |
| Ignoring NFS4ERR_WRONGSEC | Client bypasses security flavor requirements | Always check and enforce security flavor per export |
| Allowing lock stealing after grace period | Data corruption | Track pre-restart clients, don't allow reclaim after grace |
| Callback to arbitrary client-specified address | Server as attack vector | Validate callback address matches connection source |

## UX Pitfalls

Common user experience mistakes in NFS server administration.

| Pitfall | User Impact | Better Approach |
|---------|-------------|-----------------|
| Different mount paths for v3 vs v4 | Confusion, broken scripts | Document clearly, consider compatibility layer |
| Cryptic error codes (NFS4ERR_*) | Unable to debug issues | Translate to human-readable messages in logs |
| Silent delegation failures | Unexpected performance | Log when delegations disabled and why |
| Inconsistent ID mapping | Permission denied, wrong owners | Clear idmapd configuration guidance |
| Grace period delays without explanation | "Server slow after restart" | Show grace period status, time remaining |
| No visibility into state | Cannot debug lock issues | Admin API to query client state, locks, delegations |

## "Looks Done But Isn't" Checklist

Things that appear complete but are missing critical pieces.

- [ ] **OPEN operation:** Often missing share reservation (DENY mode) handling - verify Windows clients can DENY_WRITE
- [ ] **LOCK operation:** Often missing byte-range split/merge for sub-range locks - verify overlapping lock upgrade/downgrade
- [ ] **State recovery:** Often missing stable storage - verify state survives server restart
- [ ] **Compound operations:** Often missing proper error propagation - verify partial failure cleans up properly
- [ ] **Grace period:** Often missing client tracking - verify only pre-restart clients can reclaim
- [ ] **Delegations:** Often missing callback channel verification - verify recalls actually reach clients
- [ ] **ID mapping:** Often missing domain configuration - verify user/group names resolve correctly
- [ ] **ACLs:** Often missing mode-to-ACL synchronization - verify chmod updates ACL and vice versa
- [ ] **LAYOUTGET (pNFS):** Often returns success but doesn't actually work - verify data path to storage devices
- [ ] **EXCHANGE_ID (v4.1):** Often missing trunking support - verify multi-homed servers work

## Recovery Strategies

When pitfalls occur despite prevention, how to recover.

| Pitfall | Recovery Cost | Recovery Steps |
|---------|---------------|----------------|
| Stateid sequence corruption | MEDIUM | Clear client state, force re-SETCLIENTID; clients will re-establish |
| Grace period violation (locks stolen) | HIGH | Cannot automatically recover; affected files may have data corruption |
| Pseudo-fs path mismatch | LOW | Update documentation, provide migration script for client fstab |
| Lease race conditions | MEDIUM | Add proper locking, existing state will expire and renew correctly |
| Callback failures | LOW | Disable delegations for affected clients; fallback to non-delegated ops |
| Lock/Open owner conflation | HIGH | Data structure redesign required; coordinate with client re-mount |
| Kerberos misconfiguration | LOW | Fix configuration, restart services; no state impact |

## Pitfall-to-Phase Mapping

How roadmap phases should address these pitfalls.

| Pitfall | Prevention Phase | Verification |
|---------|------------------|--------------|
| Stateid sequence mismanagement | Phase 1: Core State | Unit tests with concurrent state transitions, seqid wraparound tests |
| Grace period violations | Phase 2: State Persistence | Integration test: restart server, verify only listed clients can reclaim |
| Pseudo-fs path confusion | Phase 1: Protocol Foundation | E2E test: mount same export with v3 and v4, verify both work |
| Lease expiration races | Phase 2: State Management | Stress test with artificial network delays, verify no spurious expirations |
| Delegation callback failures | Phase 3: Delegations | Test behind NAT; verify graceful degradation |
| Lock/Open owner conflation | Phase 1: Core State | Unit tests with multi-file, multi-lock per process |
| Kerberos integration | Phase 4: Security | Integration tests against real KDC; document all prerequisites |
| Cross-protocol lock coordination | Phase 5: SMB Integration | Test NFS lock visibility from SMB and vice versa |
| Compound partial failures | Phase 1: Protocol Foundation | Unit tests with forced mid-compound errors |
| ACL interoperability | Phase 4: Security | Test ACL round-trip between NFS and SMB clients |

## NFSv4.0 vs NFSv4.1+ Decision Matrix

Critical decision point: which minor version to implement.

| Aspect | NFSv4.0 | NFSv4.1+ | Recommendation |
|--------|---------|----------|----------------|
| Callbacks | Server initiates to client (NAT breaks) | Backchannel on same TCP (NAT-safe) | **Implement v4.1** |
| Sessions | None | Exactly-once semantics, slots | **Implement v4.1** |
| Replay cache | Unbounded per-client | Bounded per-slot | **Implement v4.1** |
| pNFS | Not available | Full parallel NFS support | v4.1 if pNFS needed |
| Complexity | Lower | Higher | v4.0 only for legacy |
| Client support | Universal | Very wide (all modern clients) | v4.1 preferred |

**Strong recommendation:** Implement NFSv4.1 as the minimum. NFSv4.0 has fundamental NAT/firewall issues that make it unsuitable for modern deployments. The session model in v4.1 also provides better exactly-once semantics.

## Sources

### Official Specifications
- [RFC 7530 - NFSv4.0 Protocol](https://datatracker.ietf.org/doc/html/rfc7530) - MEDIUM confidence (read for state management details)
- [RFC 8881 - NFSv4.1 Protocol](https://www.rfc-editor.org/rfc/rfc8881.html) - HIGH confidence (current authoritative spec)
- [RFC 5661 - NFSv4.1 (obsoleted by 8881)](https://www.rfc-editor.org/rfc/rfc5661) - Referenced in older implementations

### Linux Kernel Documentation
- [NFSv4.1 Server Implementation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) - HIGH confidence (Linux kernel docs)
- [NFSv4 Client Identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html) - HIGH confidence

### Vendor Documentation
- [NetApp NFSv4 Best Practices](https://www.netapp.com/media/16398-tr-3580.pdf) - MEDIUM confidence
- [Oracle NFSv4 Delegation Guide](https://docs.oracle.com/cd/E19253-01/816-4555/rfsrefer-140/index.html) - MEDIUM confidence
- [Oracle NFSv4 Grace Period](https://docs.oracle.com/cd/E19120-01/open.solaris/819-1634/rfsrefer-138/index.html) - MEDIUM confidence
- [Oracle NFSv4 Courteous Server](https://blogs.oracle.com/linux/nfsv4-courteous-server) - MEDIUM confidence

### Bug Reports and Community
- [Red Hat: Bad Sequence-ID Errors](https://access.redhat.com/solutions/26169) - MEDIUM confidence
- [Red Hat: NFSv4 Stale Stateid](https://access.redhat.com/solutions/469263) - MEDIUM confidence
- [Red Hat: Kerberos GSS Failures](https://access.redhat.com/solutions/84433) - MEDIUM confidence
- [Linux NFS Wiki: ACLs](http://wiki.linux-nfs.org/wiki/index.php/ACLs) - MEDIUM confidence
- [Linux NFS Wiki: Pseudo-filesystem](http://linux-nfs.org/wiki/index.php/Pseudofilesystem_improvements) - MEDIUM confidence
- [Linux NFS Wiki: pNFS Implementation Issues](https://wiki.linux-nfs.org/wiki/index.php/PNFS_Implementation_Issues) - MEDIUM confidence

### Security
- [IETF Draft: NFSv4 Security](https://datatracker.ietf.org/doc/draft-dnoveck-nfsv4-security/) - MEDIUM confidence
- [IETF Draft: NFSv4 ACLs](https://datatracker.ietf.org/doc/draft-dnoveck-nfsv4-acls/) - MEDIUM confidence
- [NFS-Ganesha: RPCSEC_GSS](https://github.com/nfs-ganesha/nfs-ganesha/wiki/RPCSEC_GSS) - MEDIUM confidence

### Cross-Protocol
- [NetApp: Multiprotocol NAS Locking](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/) - MEDIUM confidence
- [Oracle: Cross-Protocol Locking](https://docs.oracle.com/en/operating-systems/solaris/oracle-solaris/11.4/manage-smb/cross-protocol-locking.html) - MEDIUM confidence

---
*Pitfalls research for: NFSv4 Server Implementation*
*Researched: 2026-02-04*
*Context: Adding NFSv4 to existing DittoFS NFSv3 server*
