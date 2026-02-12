# Feature Research: NFS Server with Unified Locking

**Domain:** Enterprise NFS Server (NFSv3/v4 + Cross-Protocol Locking)
**Researched:** 2026-02-04
**Confidence:** MEDIUM (RFC specifications HIGH, enterprise feature comparisons LOW-MEDIUM)

## Feature Landscape

### Table Stakes (Users Expect These)

Features users assume exist. Missing these = product feels incomplete or unusable in enterprise environments.

#### Protocol Features

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| NFSv4.0 byte-range locking | Integrated into NFSv4 protocol; replaces NLM | HIGH | Mandatory per RFC 7530. Lease-based state management required. |
| NFSv4.0 file delegations (read/write) | Core NFSv4 caching optimization | HIGH | Requires callback infrastructure for recalls. Optional to grant, mandatory to recall on conflict. |
| Lease management & renewal | Foundation of NFSv4 stateful model | MEDIUM | 90s default lease. RENEW (v4.0) or SEQUENCE (v4.1) heartbeats. |
| Grace period & crash recovery | Required for lock persistence across restarts | HIGH | Stable storage for client state. 45-90s grace period typical. |
| RPCSEC_GSS framework | Mandatory per RFC 7530 for NFSv4 | HIGH | Kerberos v5 mechanism required. AUTH_SYS optional but common. |
| NFSv4 ACLs (RECOMMENDED) | Windows interop, fine-grained permissions | MEDIUM | Not mandatory per spec, but expected by enterprise users. |
| NLM for NFSv3 | Required for NFSv3 file locking | MEDIUM | Ancillary protocol with statd/lockd. Problematic but necessary. |
| Stateful OPEN/CLOSE | Core NFSv4 operation model | MEDIUM | Share reservations, deny modes. |
| Callback channel | Required for delegation recalls | MEDIUM | Separate TCP connection (v4.0) or backchannel (v4.1). NAT issues in v4.0. |

#### Cross-Protocol Features

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| Unified lock manager | Prevents corruption in mixed NFS+SMB environments | HIGH | NetApp invented this in 1990s. File system owns locks. |
| Advisory to mandatory lock bridging | SMB uses mandatory locks, NFS uses advisory | HIGH | nbmand mount option on Solaris. Needs filesystem support. |
| Oplock/delegation coordination | SMB oplocks must break when NFS accesses file | HIGH | Prevents stale cache issues. Critical for data integrity. |
| Share mode coordination | SMB share modes must be respected by NFS | MEDIUM | Deny-read, deny-write semantics across protocols. |

#### Operational Features

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| Dynamic export management | Add/remove exports at runtime | LOW | DBus interface in Ganesha. REST API in modern implementations. |
| Metrics/observability | Prometheus metrics, tracing | LOW | Already partially implemented in DittoFS. Extend for locking. |
| Graceful shutdown | Drain connections, preserve state | MEDIUM | Already implemented in DittoFS. Extend for lock state. |

### Differentiators (Competitive Advantage)

Features that set the product apart. Not required, but provide significant value.

#### NFSv4.1+ Features

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| NFSv4.1 sessions | Exactly-once semantics, NAT-friendly callbacks | HIGH | Eliminates callback NAT issues. Major reliability improvement. |
| Directory delegations | Cache directory listings | MEDIUM | Reduces READDIR traffic. Introduced in NFSv4.1. |
| RECLAIM_COMPLETE | Proper recovery signaling | LOW | v4.1 mandatory. Enables early grace period end. |
| pNFS support | Parallel I/O across data servers | VERY HIGH | Hammerspace's key differentiator. Major architecture change. |
| NFSv4.2 server-side copy | COPY operation between files | MEDIUM | Reduces client I/O. Valuable for data management. |
| NFSv4.2 sparse files | ALLOCATE/DEALLOCATE operations | MEDIUM | Punch holes, space reservations. VM image optimization. |
| NFSv4.2 extended attributes | User-defined metadata (xattrs) | LOW | RFC 8276. Different from named attributes. |
| NFSv4.2 labeled NFS | MAC security labels on files | MEDIUM | SELinux/AppArmor integration. Government/security use cases. |

#### Security Features

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Kerberos krb5i (integrity) | Checksums on all NFS messages | HIGH | Detects tampering. Requires KDC integration. |
| Kerberos krb5p (privacy) | Full encryption of NFS traffic | HIGH | Encrypts all data in transit. Performance impact. |
| Multi-realm Kerberos | Cross-domain authentication | VERY HIGH | Enterprise federation. Complex KDC configuration. |
| User principal mapping | Map Kerberos principals to UIDs | MEDIUM | nfsidmap, LDAP integration. |

#### Enterprise Features

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| HA/clustered lock state | Locks survive node failures | VERY HIGH | Requires distributed state. Rados-based in Ganesha. |
| Global namespace | Unified view across storage backends | HIGH | Hammerspace's FlexFile. Multi-backend coordination. |
| Data tiering with locking | Locks persist during data migration | HIGH | VAST Data differentiator. Complex state coordination. |
| Quota support | Per-user/group storage limits | MEDIUM | Integrate with RQUOTA protocol or filesystem quotas. |

#### Performance Features

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Lease-based lock caching | Reduce lock protocol overhead | MEDIUM | Cache locks during lease. Similar to delegations. |
| Compound operation optimization | Batch multiple ops in single RPC | LOW | Already in NFSv4. Optimize handler dispatch. |
| Zero-copy I/O | RDMA support for NFS | VERY HIGH | NFS/RDMA. Specialized NICs required. |

### Anti-Features (Commonly Requested, Often Problematic)

Features that seem good but create complexity or problems.

| Feature | Why Requested | Why Problematic | Alternative |
|---------|---------------|-----------------|-------------|
| Full pNFS from day one | Performance scaling | Massive architecture change. Layout types, data servers, metadata servers. | Start with single-server performance optimization. Add pNFS in v3.0+. |
| Custom lock semantics | Application-specific locking | Breaks protocol compatibility. Clients won't understand. | Use standard locks + application-level coordination. |
| Named attributes (NFSv4) | Windows compatibility | Complex, poorly supported in Linux. Different from xattrs. | Use NFSv4.2 xattrs (RFC 8276) instead. |
| Global mandatory locking | Prevent all conflicts | Performance killer. Breaks POSIX semantics for NFS clients. | Advisory locks + application awareness. Cross-protocol mandatory only when SMB involved. |
| NFSv4.0 callbacks without sessions | Backward compatibility | NAT/firewall issues cause delegation failures | Support v4.1 sessions as primary. v4.0 without delegations is acceptable fallback. |
| Proprietary extensions | Unique features | Breaks interoperability. Clients need modifications. | Stick to RFC-defined operations. Extend via optional features only. |
| Real-time lock notification | App-level lock awareness | Not in protocol. Would require proprietary channel. | Use existing callback/lease mechanisms. |
| Automatic lock recovery magic | User convenience | Violates protocol semantics. Can cause data corruption. | Proper grace period + client responsibility. |

## Feature Dependencies

```
                    ┌─────────────────────┐
                    │ Unified Lock Manager │
                    │    (Foundation)     │
                    └──────────┬──────────┘
                               │
              ┌────────────────┼────────────────┐
              │                │                │
              ▼                ▼                ▼
    ┌─────────────────┐ ┌───────────┐ ┌─────────────────┐
    │   NLM (NFSv3)   │ │NFSv4 State│ │SMB Lock Bridge  │
    │   Lock Support  │ │ Management│ │  (Future)       │
    └────────┬────────┘ └─────┬─────┘ └────────┬────────┘
             │                │                │
             └────────────────┼────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
              ▼                               ▼
    ┌───────────────────┐           ┌───────────────────┐
    │   Grace Period    │           │ Callback Channel  │
    │   & Recovery      │           │ (v4.0 + v4.1)     │
    └─────────┬─────────┘           └─────────┬─────────┘
              │                               │
              │         ┌─────────────────────┘
              │         │
              ▼         ▼
    ┌───────────────────────────┐
    │      Delegations          │
    │  (Read, Write, Directory) │
    └─────────────┬─────────────┘
                  │
                  ▼
    ┌───────────────────────────┐
    │    NFSv4.1 Sessions       │
    │ (Exactly-once, backchannel)│
    └─────────────┬─────────────┘
                  │
    ┌─────────────┴─────────────┐
    │                           │
    ▼                           ▼
┌────────────────┐    ┌─────────────────┐
│ RPCSEC_GSS     │    │   NFSv4.2       │
│ (Kerberos)     │    │   Extensions    │
└────────────────┘    └─────────────────┘
```

### Dependency Notes

- **Unified Lock Manager requires nothing** - Foundation layer. Must be built first.
- **NLM requires Unified Lock Manager** - NLM is the NFSv3 interface to the lock manager.
- **NFSv4 State Management requires Unified Lock Manager** - NFSv4 locks flow through same backend.
- **Grace Period requires State Management** - Must track what locks to allow reclaim for.
- **Callback Channel requires State Management** - Must know which clients hold delegations.
- **Delegations require Callback Channel** - Cannot grant without ability to recall.
- **NFSv4.1 Sessions enhance Callbacks** - Backchannel solves NAT issues. Not strictly required.
- **RPCSEC_GSS independent of locking** - Can be implemented in parallel.
- **NFSv4.2 requires NFSv4.1** - All v4.2 features are optional additions to v4.1.
- **SMB Lock Bridge requires Unified Lock Manager** - SMB locking integrates at foundation.

## Phased Implementation Recommendation

### Phase 1: v1.0 - NLM + Unified Lock Manager

Foundation for all locking functionality.

- [x] Unified lock manager abstraction (protocol-agnostic)
- [ ] NLM protocol implementation (NFSv3 locking)
- [ ] NSM (Network Status Monitor) for crash recovery
- [ ] Basic grace period support
- [ ] Lock persistence (stable storage)
- [ ] Metrics for lock operations

**Why essential:** Without NLM, NFSv3 clients cannot lock files. This is table stakes for any serious NFSv3 deployment. The unified lock manager provides foundation for NFSv4 and future SMB support.

### Phase 2: v2.0 - NFSv4.0 + Kerberos

Add stateful NFSv4 with proper security.

- [ ] NFSv4 state management (client IDs, leases)
- [ ] NFSv4 byte-range locking (integrated, not NLM)
- [ ] Read/Write delegations
- [ ] Callback channel (server→client)
- [ ] RPCSEC_GSS framework
- [ ] Kerberos v5 authentication (krb5)
- [ ] NFSv4 ACL support (RECOMMENDED tier)

**Trigger for adding:** Enterprise customers require authentication beyond AUTH_SYS. NFSv4 is expected for new deployments.

### Phase 3: v3.0 - NFSv4.1 Sessions

Reliability and performance improvements.

- [ ] NFSv4.1 sessions (exactly-once semantics)
- [ ] Backchannel callbacks (NAT-friendly)
- [ ] Directory delegations
- [ ] RECLAIM_COMPLETE operation
- [ ] Kerberos integrity (krb5i)
- [ ] Kerberos privacy (krb5p)

**Trigger for adding:** NAT/firewall issues with v4.0 callbacks. Enterprise reliability requirements.

### Phase 4: v4.0 - NFSv4.2 Extensions

Modern features for specialized workloads.

- [ ] Server-side COPY operation
- [ ] Sparse file support (ALLOCATE/DEALLOCATE)
- [ ] Extended attributes (xattrs, RFC 8276)
- [ ] Labeled NFS (MAC security)
- [ ] IO_ADVISE operation

**Trigger for adding:** VM/container workloads need sparse files. Data management needs server-side copy.

### Future Consideration (v5+)

Features to defer until product-market fit is established.

- [ ] pNFS support - Requires fundamental architecture change (metadata/data server separation)
- [ ] NFS/RDMA - Specialized hardware requirement, niche use case
- [ ] HA/clustered locks - Requires distributed state management (etcd, Raft)
- [ ] SMB integration - Separate protocol implementation, shares lock manager

## Feature Prioritization Matrix

| Feature | User Value | Implementation Cost | Priority |
|---------|------------|---------------------|----------|
| NLM (NFSv3 locking) | HIGH | MEDIUM | P1 |
| Unified lock manager | HIGH | MEDIUM | P1 |
| Grace period recovery | HIGH | MEDIUM | P1 |
| NFSv4 state management | HIGH | HIGH | P1 |
| NFSv4 byte-range locking | HIGH | MEDIUM | P1 |
| Kerberos authentication | HIGH | HIGH | P1 |
| Delegations (read/write) | MEDIUM | HIGH | P2 |
| Callback channel | MEDIUM | MEDIUM | P2 |
| NFSv4 ACLs | MEDIUM | MEDIUM | P2 |
| NFSv4.1 sessions | MEDIUM | HIGH | P2 |
| Directory delegations | LOW | MEDIUM | P3 |
| NFSv4.2 server-side copy | MEDIUM | LOW | P3 |
| NFSv4.2 sparse files | MEDIUM | MEDIUM | P3 |
| NFSv4.2 xattrs | LOW | LOW | P3 |
| Labeled NFS | LOW | MEDIUM | P3 |
| pNFS | HIGH | VERY HIGH | P4 |

**Priority key:**
- P1: Must have for serious enterprise use
- P2: Should have, add when foundation stable
- P3: Nice to have, future consideration
- P4: Major architecture change, long-term roadmap

## Competitor Feature Analysis

| Feature | NetApp ONTAP | Hammerspace | JuiceFS | Ganesha | DittoFS Target |
|---------|--------------|-------------|---------|---------|----------------|
| NFSv3 + NLM | Yes | Yes | Via gateway | Yes | v1.0 |
| NFSv4.0 | Yes | Yes | No | Yes | v2.0 |
| NFSv4.1 | Yes | Yes | No | Yes | v3.0 |
| NFSv4.2 | Yes | Partial | No | Partial | v4.0 |
| pNFS | Yes | Yes (FlexFile) | No | Dormant | Future |
| Kerberos | Full | Full | No | Yes | v2.0 |
| Unified NFS+SMB locks | Yes (invented) | Yes | No | No | v1.0 foundation |
| HA clustering | Yes | Yes | Via metadata DB | Yes (Pacemaker) | Future |
| Cloud-native | No | Yes | Yes | No | Already (S3 backend) |
| Global namespace | Yes | Yes | Yes | No | Future |

### Competitive Positioning

**vs. NetApp/EMC:** Cannot compete on HA/enterprise features short-term. Compete on simplicity, cloud-native architecture, and open source.

**vs. Hammerspace:** Cannot match pNFS/global namespace. Compete on easier deployment, better documentation, and lower operational complexity.

**vs. JuiceFS:** Already ahead on protocol completeness (NFSv3). Extend lead with proper locking, NFSv4, and Kerberos. JuiceFS requires FUSE gateway for NFS.

**vs. Ganesha:** Similar feature set targets. Differentiate on:
- Modern Go implementation (vs. C)
- Cloud-native storage backends (S3)
- Better observability (Prometheus, OpenTelemetry)
- Simpler configuration
- Active unified lock development (Ganesha's pNFS dormant)

## Sources

### RFC Specifications (HIGH confidence)
- [RFC 7530 - NFSv4.0](https://datatracker.ietf.org/doc/html/rfc7530) - Core NFSv4 protocol
- [RFC 5661 - NFSv4.1](https://datatracker.ietf.org/doc/rfc5661/) - Sessions, directory delegations, pNFS
- [RFC 7862 - NFSv4.2](https://datatracker.ietf.org/doc/html/rfc7862) - Copy, sparse files, xattrs
- [RFC 8276 - NFSv4 Extended Attributes](https://datatracker.ietf.org/doc/rfc8276/) - xattrs specification
- [Open Group XNFS - NLM Protocol](https://pubs.opengroup.org/onlinepubs/9629799/chap14.htm) - NLM v4 specification

### Vendor Documentation (MEDIUM confidence)
- [NetApp NFSv4 Enhancements TR-3580](https://www.netapp.com/media/16398-tr-3580.pdf)
- [NetApp Multiprotocol NAS TR-4887](https://www.netapp.com/media/27436-tr-4887.pdf)
- [NetApp Cross-Protocol Locking](https://docs.netapp.com/us-en/ontap/nfs-admin/file-locking-between-protocols-concept.html)
- [VAST Data Kerberos for NFSv4.1](https://support.vastdata.com/s/document-item?bundleId=vast-cluster-administrator-s-guide4.6&topicId=managing-access-protocols/nfs-file-sharing-protocol/kerberos-authentication-for-nfsv4-1.html)
- [Dell PowerScale NFS Design](https://www.delltechnologies.com/asset/en-us/products/storage/industry-market/h17240_wp_isilon_onefs_nfs_design_considerations_bp.pdf)
- [Hammerspace pNFS Overview](https://hammerspace.com/parallel-nfs-a-modern-protocol-for-high-performance-workloads/)

### Implementation References (MEDIUM confidence)
- [NFS-Ganesha Wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki)
- [NFS-Ganesha Delegations](https://github.com/nfs-ganesha/nfs-ganesha/wiki/NFSv4-Delegations)
- [Linux NFS Wiki - Lock Recovery](https://linux-nfs.org/wiki/index.php/NFS_lock_recovery_notes)
- [Linux NFS Wiki - Server Recovery](https://linux-nfs.org/wiki/index.php/Nfsd4_server_recovery)
- [Linux Kernel NFSv4 Client Identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html)
- [Linux Kernel RPCSEC_GSS](https://docs.kernel.org/filesystems/nfs/rpc-server-gss.html)

### Community Resources (LOW confidence - verify before relying)
- [Multiprotocol NAS Locking Blog](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/)
- [JuiceFS Blog - Distributed Filesystem Comparison](https://juicefs.com/en/blog/engineering/distributed-filesystem-comparison/)
- [JuiceFS Direct-Mode NFS](https://juicefs.com/en/blog/usage-tips/direct-nfs)

---
*Feature research for: Enterprise NFS Server with Unified Locking*
*Researched: 2026-02-04*
