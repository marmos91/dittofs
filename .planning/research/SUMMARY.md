# Project Research Summary

**Project:** DittoFS NFSv4 Protocol Evolution
**Domain:** Enterprise NFS server implementation (NFSv3 to NFSv4.2 migration)
**Researched:** 2026-02-04
**Confidence:** MEDIUM-HIGH

## Executive Summary

DittoFS needs to evolve from its current NFSv3 implementation to support NFSv4.0 through NFSv4.2, adding enterprise-grade authentication (Kerberos), stateful file locking, and modern performance features. The research reveals that the Go ecosystem lacks production-ready NFSv4 libraries, making the recommended approach to **extend DittoFS's existing custom XDR/RPC implementation** rather than adopt external dependencies. The project should implement NFSv4.1 as the minimum target (not NFSv4.0) due to fundamental NAT/firewall issues in NFSv4.0's callback architecture.

The core challenge is managing NFSv4's stateful protocol semantics—the server must track client identities, open files, byte-range locks, delegations, and sessions. This requires a new state management layer that integrates with DittoFS's existing MetadataService and LockManager. The unified lock manager foundation already exists, positioning DittoFS well for cross-protocol consistency when SMB support is eventually added.

Critical risks include stateid sequence number mismanagement (can cause data corruption), grace period violations (breaks crash recovery), and Kerberos integration complexity. These risks are mitigated by implementing phases incrementally, with comprehensive testing at each stage, and deferring complex features like delegations and pNFS until the foundation is proven stable.

## Key Findings

### Recommended Stack

The Go ecosystem for NFSv4 is nascent. No single library provides production-ready NFSv4.x with RPCSEC_GSS. The recommended approach is to extend DittoFS's existing custom XDR/RPC implementation (~400 lines) rather than adopt external libraries. This provides full control over NFSv4 compound operations, zero-copy optimizations, and tight RPCSEC_GSS integration.

**Core technologies:**
- **Custom XDR** (extend `internal/protocol/nfs/xdr/`) — Full control over NFSv4 compound encoding, zero-copy optimizations, avoids dependency churn
- **Custom RPC** (extend `internal/protocol/nfs/rpc/`) — Required for RPCSEC_GSS integration, session-level state tracking
- **jcmturner/gokrb5/v8** (already in go.mod v8.4.4) — Pure Go Kerberos, no CGO, AD integration, provides GSS-API primitives for RPCSEC_GSS

**Critical decision:** Extend existing implementation rather than adopt `davecgh/go-xdr` or `xdrpp/goxdr`. While code generators could work for greenfield projects, DittoFS's existing patterns and performance requirements favor hand-tuned code.

**What gokrb5 provides:** Kerberos ticket acquisition, GSS wrap/MIC tokens for integrity/privacy, AD PAC decoding.

**What gokrb5 does NOT provide:** RPCSEC_GSS protocol layer (must implement from RFC 2203), RPC message framing with GSS tokens, sequence window management.

### Expected Features

Based on analysis of enterprise NFS deployments (NetApp ONTAP, Hammerspace, NFS-Ganesha), the feature landscape is clear.

**Must have (table stakes):**
- NFSv4.0 byte-range locking (mandatory per RFC 7530)
- Lease management and renewal (foundation of stateful model)
- Grace period and crash recovery (required for production reliability)
- RPCSEC_GSS framework with Kerberos v5 (mandatory per RFC 7530)
- Stateful OPEN/CLOSE operations (share reservations, deny modes)
- NLM for NFSv3 backwards compatibility (until all clients migrate to v4)
- Unified lock manager for cross-protocol consistency (DittoFS innovation)

**Should have (competitive advantage):**
- NFSv4.1 sessions (exactly-once semantics, NAT-friendly callbacks)
- Read/write delegations (caching optimization for exclusive access)
- Directory delegations (reduce READDIR traffic)
- NFSv4 ACLs (Windows interop, fine-grained permissions)
- Kerberos integrity (krb5i) and privacy (krb5p)

**Defer to v2+ (complex, specialized):**
- pNFS support (requires fundamental architecture change to metadata/data server separation)
- NFSv4.2 server-side copy (useful but not essential)
- NFSv4.2 sparse files and extended attributes (VM/container workloads)
- NFSv4.2 labeled NFS (government/security use cases)
- NFS/RDMA (specialized hardware, niche)

**Competitive positioning:**
- vs. NetApp/EMC: Cannot compete on HA/enterprise features short-term. Compete on simplicity, cloud-native architecture, open source.
- vs. Hammerspace: Cannot match pNFS/global namespace initially. Compete on easier deployment, better documentation, lower complexity.
- vs. JuiceFS: Already ahead on protocol completeness. Extend lead with proper locking, NFSv4, and Kerberos.
- vs. NFS-Ganesha: Similar feature targets. Differentiate on modern Go implementation, cloud-native storage (S3), better observability, simpler configuration.

### Architecture Approach

NFSv4 is fundamentally stateful, unlike NFSv3's stateless model. The server must track client identities, open files, locks, delegations, and sessions. The standard architecture has six major components: Client Manager (clientid tracking), Session Manager (NFSv4.1 slot tables), Lease Manager (expiration and grace periods), StateID Manager (open/lock/delegation states), Lock Manager (byte-range with conflict detection), and Delegation Manager (grant/recall logic).

**Major components:**
1. **StateManager** (`pkg/nfs4state/`) — New public package for NFSv4 state management. Public visibility enables cross-protocol locking when SMB is added. Contains all stateid generation, validation, and lifecycle management.
2. **Compound Processor** (`internal/protocol/nfs/v4/handlers/compound.go`) — Executes multiple operations in single RPC. Maintains current/saved filehandle state, stops on first error, enables atomic multi-operation sequences.
3. **Recovery Store** (`pkg/nfs4state/store/`) — Persistent storage for client records, grace period timestamps, reclaim tracking. Supports file-based (single node) and database-backed (future HA).
4. **Callback Client** (`internal/protocol/nfs/v4/callback/`) — Server-initiated RPC to clients for delegation recalls. NFSv4.0 uses separate TCP connections (NAT issues), NFSv4.1 uses backchannel on same connection.
5. **Lease Manager** — Background "laundromat" goroutine for expired client cleanup. Implements "courteous server" behavior (keep expired state until conflict) for better user experience.

**Key integration points with existing DittoFS:**
- StateManager delegates file operations to existing MetadataService (no duplication)
- NFSv4 locks flow through existing LockManager with new owner types (ClientID + owner string)
- Existing BlockService handles READ/WRITE with stateid validation added
- Existing graceful shutdown extended for state cleanup
- Existing metrics/tracing extended for state operations

**Recommended project structure:**
```
internal/protocol/nfs/v4/           # NFSv4 handlers (40+ operations)
pkg/nfs4state/                      # State management (public for SMB integration)
├── manager.go                      # StateManager interface
├── client.go, session.go           # NFSv4 client/session tracking
├── stateid.go, open_state.go       # State types and lifecycle
├── lock_state.go, delegation.go    # Lock and delegation management
├── lease.go, grace.go, recovery.go # Lease, grace period, crash recovery
└── store/                          # Persistent recovery store
```

### Critical Pitfalls

Based on Linux kernel docs, vendor bug reports, and NFS-Ganesha issue tracker, these are the pitfalls that cause rewrites or data corruption.

1. **Stateid Sequence Number Mismanagement** — NFSv4 uses seqids to detect replayed requests. Multiple state types (open-owner, lock-owner) have independent seqids. Wraparound at 2^32 is often forgotten. Failure to increment at every state transition causes "bad sequence-id" errors. **Prevention:** Separate seqid tracking per owner type, monotonic increment at ALL transitions, wraparound handling, implement RELEASE_LOCKOWNER.

2. **Grace Period Violations** — Server must reject non-reclaim operations during grace period after restart. Granting new locks during grace period can "steal" pre-crash locks from other clients. **Prevention:** Track active clients in stable storage, reject READ/WRITE with NFS4ERR_GRACE during grace period, only allow reclaim from pre-restart clients, support RECLAIM_COMPLETE (NFSv4.1) for early termination.

3. **Pseudo-Filesystem vs NFSv3 Export Path Confusion** — NFSv4 has a "pseudo-filesystem" where all exports are children of virtual root (fsid=0). What was `server:/export/users` in v3 becomes `server:/users` in v4. **Prevention:** Designate one export as fsid=0 or auto-generate pseudo-fs, document path transformation clearly, test both protocols against same exports.

4. **Lease Expiration Race Conditions** — Race between lease expiration checks and new client operations causes state corruption. Active clients appear expired due to network delays. **Prevention:** Atomic operations for lease timestamp updates, implicit renewal on ANY state operation, implement "courteous server" behavior (keep expired state until conflict).

5. **Lock Owner vs Open Owner Conflation** — Treating open-owners and lock-owners as same entity causes seqid errors and state corruption. **Prevention:** Separate data structures and seqid tracking. Open-owners tied to OPEN/CLOSE, lock-owners tied to LOCK/LOCKU.

6. **Delegation Callbacks Failing in NAT/Container Environments** — NFSv4.0 delegations require server→client TCP connections, which fail behind NAT/firewalls. **Prevention:** Implement NFSv4.1 backchannel (NAT-safe), detect callback failures and disable delegations gracefully, make callback port configurable for v4.0.

7. **Kerberos Clock Skew and DNS Issues** — Kerberos has 5-minute clock skew tolerance. DNS misconfiguration (127.0.0.1 in /etc/hosts) breaks authentication. **Prevention:** Document NTP as hard requirement, verify /etc/hosts doesn't map hostname to localhost, require working forward and reverse DNS, test Kerberos separately before NFS integration.

## Implications for Roadmap

Based on feature dependencies, architecture complexity, and pitfall prevention, the recommended phase structure is:

### Phase 1: v1.0 - NLM + Unified Lock Manager
**Rationale:** Foundation for all locking functionality. NLM is required for NFSv3 backwards compatibility. The unified lock manager is the architectural innovation that enables future cross-protocol locking (NFSv4, SMB).

**Delivers:**
- Protocol-agnostic unified lock manager
- NLM protocol implementation (NFSv3 locking via separate daemon)
- NSM (Network Status Monitor) for crash recovery
- Basic grace period support
- Lock persistence (stable storage)
- Metrics for lock operations

**Addresses features:**
- NLM for NFSv3 (table stakes)
- Unified lock manager (competitive advantage)
- Grace period foundation (table stakes)

**Avoids pitfalls:**
- Lock Owner vs Open Owner Conflation (separate data structures from start)
- Grace period foundation for future phases

**Research flag:** Standard patterns, skip `/gsd:research-phase`. NLM is well-documented (RFC 1813, Open Group XNFS).

---

### Phase 2: v2.0 - NFSv4.0 + Kerberos
**Rationale:** Add stateful NFSv4 with enterprise authentication. This is the largest phase, introducing state management, compound operations, and RPCSEC_GSS. Must be rock-solid before adding advanced features.

**Delivers:**
- NFSv4 state management (Client Manager, StateID Manager, Lease Manager)
- NFSv4 byte-range locking (integrated into protocol, not NLM)
- Stateful OPEN/CLOSE with share reservations
- RPCSEC_GSS framework (RFC 2203 implementation)
- Kerberos v5 authentication (krb5) using gokrb5/v8
- NFSv4 ACL support (RECOMMENDED tier, Windows interop)
- Grace period with RECLAIM operations
- Pseudo-filesystem (fsid=0 root)

**Addresses features:**
- NFSv4.0 locking (table stakes)
- Lease management (table stakes)
- RPCSEC_GSS + Kerberos (table stakes)
- NFSv4 ACLs (competitive)
- Pseudo-filesystem (table stakes)

**Avoids pitfalls:**
- Stateid Sequence Number Mismanagement (comprehensive unit tests)
- Grace Period Violations (stable storage from start)
- Pseudo-fs Path Confusion (clear documentation, E2E tests)
- Lease Expiration Races (atomic operations, courteous server)
- Kerberos DNS/Clock Issues (clear prerequisites)

**Research flag:** Likely needs `/gsd:research-phase` for Kerberos integration specifics (keytab setup, AD integration). RPCSEC_GSS has sparse Go examples.

---

### Phase 3: v3.0 - NFSv4.1 Sessions
**Rationale:** Reliability and NAT-friendliness improvements. Sessions solve fundamental issues with NFSv4.0 callbacks and provide exactly-once semantics. This is a cleaner foundation for delegations than NFSv4.0.

**Delivers:**
- NFSv4.1 sessions (EXCHANGE_ID, CREATE_SESSION)
- Backchannel callbacks (NAT-friendly, on same TCP connection)
- Slot tables and duplicate request cache (DRC)
- SEQUENCE operation (session validation, implicit lease renewal)
- Directory delegations
- RECLAIM_COMPLETE operation
- Kerberos integrity (krb5i) and privacy (krb5p)

**Addresses features:**
- NFSv4.1 sessions (competitive advantage, solves NAT issues)
- Exactly-once semantics (reliability)
- Directory delegations (competitive)
- krb5i/krb5p (security)

**Avoids pitfalls:**
- Delegation Callbacks in NAT (backchannel solves this)
- Session replay protection (bounded DRC)

**Research flag:** Standard patterns from RFC 8881. Skip `/gsd:research-phase` unless pNFS is targeted.

---

### Phase 4: v4.0 - NFSv4.2 Extensions
**Rationale:** Modern features for specialized workloads (VMs, containers, data management). Optional but valuable for specific use cases.

**Delivers:**
- Server-side COPY operation (copy between files without client I/O)
- Sparse file support (ALLOCATE, DEALLOCATE, SEEK)
- Extended attributes (xattrs, RFC 8276)
- Labeled NFS (MAC security labels)
- IO_ADVISE operation (application hints)

**Addresses features:**
- NFSv4.2 server-side copy (competitive)
- Sparse files (competitive)
- Extended attributes (nice to have)
- Labeled NFS (government/security niche)

**Avoids pitfalls:**
- None specific; these are isolated features

**Research flag:** Skip `/gsd:research-phase`. RFC 7862 is straightforward.

---

### Phase Ordering Rationale

**Dependency-driven:**
- Phase 1 creates the lock manager foundation required by all subsequent phases
- Phase 2 builds state management on top of locking
- Phase 3 builds sessions on top of state management
- Phase 4 adds optional features to stable foundation

**Risk-driven:**
- Most complex/risky phase (NFSv4.0 + Kerberos) comes second, with foundation in place
- Delegations deferred to Phase 3 where backchannel solves NAT issues
- Optional features deferred to Phase 4 after core protocol proven

**Architecture-driven:**
- Unified lock manager enables clean NFSv4 integration (not bolted on)
- State management designed as public package for future SMB integration
- Pseudo-filesystem designed upfront (hard to change later)

**Pitfall-driven:**
- Separate owner types from Phase 1 prevents conflation
- Stable storage for grace period from Phase 2
- NFSv4.1 backchannel in Phase 3 avoids NFSv4.0 callback issues

### Research Flags

**Phases likely needing deeper research during planning:**
- **Phase 2 (NFSv4.0 + Kerberos):** RPCSEC_GSS implementation details, keytab setup, AD integration testing. Sparse Go examples for RPCSEC_GSS over RPC.
- **Future (pNFS):** If pNFS becomes a target, requires deep research into layout types, data server protocols, metadata/data separation.

**Phases with standard patterns (skip research-phase):**
- **Phase 1 (NLM):** Well-documented protocol (RFC 1813, Open Group XNFS spec)
- **Phase 3 (NFSv4.1):** Clear RFC 8881 specification, Linux kernel implementation as reference
- **Phase 4 (NFSv4.2):** Straightforward RFC 7862, isolated features

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | MEDIUM | Go ecosystem for NFSv4 is sparse. Recommendation to extend custom XDR/RPC is sound based on DittoFS's existing architecture, but no production Go NFSv4 server exists as validation. gokrb5/v8 is proven for Kerberos primitives. |
| Features | MEDIUM-HIGH | Feature landscape based on authoritative sources (RFCs, NetApp/Oracle docs, Linux kernel). Table stakes features are clear. Competitive features validated against NFS-Ganesha and vendor implementations. |
| Architecture | HIGH | State management patterns derived from Linux nfsd (HIGH confidence), nfs4j (MEDIUM confidence), and RFCs. Standard architecture is well-understood. Integration with DittoFS components is clear. |
| Pitfalls | MEDIUM-HIGH | Pitfalls sourced from Linux kernel docs (HIGH), Red Hat bug reports (MEDIUM), vendor documentation (MEDIUM). Stateid and grace period issues are well-documented. Kerberos issues are common knowledge in NFS community. |

**Overall confidence:** MEDIUM-HIGH

Well-grounded in RFCs and production implementations, but lack of Go NFSv4 ecosystem means implementation will break new ground. Architecture patterns are proven (Linux nfsd), but translating to Go's concurrency model adds uncertainty.

### Gaps to Address

**During Phase 2 planning:**
- RPCSEC_GSS implementation specifics for Go. RFC 2203 is clear, but translating to Go with gokrb5 GSS primitives needs experimentation.
- Keytab file format and parsing. gokrb5 supports keytabs, but integration with NFS service principals needs validation.
- Stateid generation entropy requirements. RFCs say "unique", but how much randomness is needed vs. sequential counters?

**During Phase 3 planning:**
- Session backchannel implementation for Go RPC. Linux kernel uses separate RPC layer; DittoFS's custom RPC needs extension.
- Slot table DRC sizing and memory bounds. How many slots per session? What's the memory impact at 10k clients?

**During implementation:**
- Cross-protocol lock testing when SMB is added. Research covers theory, but practical testing needs real SMB client behavior.
- pNFS feasibility. Current research says "defer", but if customer demand emerges, requires deep architectural research.

**Validation strategies:**
- Phase 1: Test against Linux NFSv3 client with fcntl() locking, verify lock conflicts across processes
- Phase 2: Test against Linux NFSv4.0 client, run Connectathon NFS test suite, test Kerberos with real KDC
- Phase 3: Test NFSv4.1 sessions behind NAT, verify backchannel delegation recalls work
- Phase 4: Test server-side copy with large files, verify sparse file hole-punching with VM images

## Sources

### Primary (HIGH confidence)
- [RFC 7530 - NFSv4.0](https://datatracker.ietf.org/doc/rfc7530/) - Core NFSv4 protocol specification
- [RFC 8881 - NFSv4.1](https://www.rfc-editor.org/rfc/rfc8881.html) - Sessions, pNFS, current authoritative spec
- [RFC 7862 - NFSv4.2](https://datatracker.ietf.org/doc/rfc7862/) - Server-side copy, sparse files, xattrs
- [RFC 2203 - RPCSEC_GSS](https://datatracker.ietf.org/doc/rfc2203/) - GSS security for RPC
- [RFC 4121 - Kerberos V5 GSS-API](https://datatracker.ietf.org/doc/rfc4121/) - Kerberos mechanism
- [Linux nfsd source - nfs4state.c](https://elixir.bootlin.com/linux/latest/source/fs/nfsd/nfs4state.c) - Reference implementation
- [Linux NFSv4.1 Server Documentation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html)
- [jcmturner/gokrb5](https://github.com/jcmturner/gokrb5) - Pure Go Kerberos (v8.4.4 in go.mod)

### Secondary (MEDIUM confidence)
- [NetApp NFSv4 Enhancements TR-3580](https://www.netapp.com/media/16398-tr-3580.pdf) - Enterprise NFS features
- [NetApp Multiprotocol NAS TR-4887](https://www.netapp.com/media/27436-tr-4887.pdf) - Cross-protocol locking
- [NFS-Ganesha Wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki) - Reference architecture
- [nfs4j - Java NFSv4](https://github.com/dCache/nfs4j) - State management patterns
- [Red Hat KBs](https://access.redhat.com) - Seqid errors (26169), Kerberos failures (84433)
- [Oracle NFSv4 Documentation](https://docs.oracle.com) - Grace period, courteous server

### Tertiary (LOW confidence)
- [Multiprotocol NAS Locking Blog](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/) - Cross-protocol theory
- [JuiceFS Distributed FS Comparison](https://juicefs.com/en/blog/engineering/distributed-filesystem-comparison/) - Competitive landscape

---
*Research completed: 2026-02-04*
*Ready for roadmap: yes*
