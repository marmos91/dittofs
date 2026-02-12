# DittoFS NFS Protocol Evolution

## What This Is

A comprehensive NFS protocol upgrade for DittoFS that adds NFSv4.0/4.1/4.2 support with Kerberos authentication, unified cross-protocol locking (NFS + SMB), and advanced features like server-side copy, sparse files, and delegations. The implementation follows an incremental approach: NLM first (v1.0), then NFSv4.0 with Kerberos (v2.0), NFSv4.1 sessions (v3.0), and finally NFSv4.2 features (v4.0).

Target: Cloud-native enterprise NAS with feature parity exceeding JuiceFS and Hammerspace, particularly in security (Kerberos) and cross-protocol consistency.

## Core Value

Enable enterprise-grade multi-protocol file access (NFSv3, NFSv4.x, SMB3) with unified locking, Kerberos authentication, and immediate cross-protocol visibility — all deployable in containerized Kubernetes environments.

## Requirements

### Validated

- ✓ NFSv3 protocol implementation — existing
- ✓ SMB2/3 protocol implementation — existing
- ✓ Pluggable metadata stores (memory, BadgerDB, PostgreSQL) — existing
- ✓ Pluggable block stores (memory, filesystem, S3) — existing
- ✓ Control plane with user/group management — existing
- ✓ ACL model in control plane (for SMB) — existing
- ✓ Block-aware caching with WAL persistence — existing
- ✓ E2E test framework — existing

### Active

#### v1.0 — NLM + Unified Lock Manager
- [x] Unified Lock Manager embedded in metadata service
- [x] Lock state persistence in metadata store (per-share)
- [x] Flexible lock model (native semantics, translate at boundary)
- [x] NLM protocol (RPC program 100021) for NFSv3
- [x] NSM protocol (RPC program 100024) for crash recovery
- [x] SMB2/3 lease support (Read, Write, Handle leases)
- [x] Cross-protocol lock coordination (NLM ↔ SMB)
- [x] Grace period handling for server restarts
- [x] Per-adapter connection pool (unified stateless/stateful)
- [x] E2E tests for locking scenarios

#### v2.0 — NFSv4.0 + Kerberos
- [ ] NFSv4.0 compound operations (COMPOUND/CB_COMPOUND)
- [ ] NFSv4 pseudo-filesystem (single namespace for all exports)
- [ ] Client ID and state ID management
- [ ] NFSv4 integrated locking (LOCK/LOCKT/LOCKU)
- [ ] NFSv4 lock integration with Unified Lock Manager
- [ ] Read/write delegations
- [ ] Delegation recall via callbacks
- [ ] Client-first flush on delegation recall (cache coherence)
- [ ] NFSv4 ACLs (extend existing control plane ACL model)
- [ ] Shared Kerberos abstraction layer (pkg/auth/kerberos)
- [ ] RPCSEC_GSS for NFSv4 (krb5, krb5i, krb5p)
- [ ] AUTH_SYS fallback (configurable per share)
- [ ] External KDC integration (Active Directory)
- [ ] NFSv4 ID mapping (user@domain → control plane users)
- [ ] Lease management (renewal, expiration, ~90s default)
- [ ] State recovery via metadata store
- [ ] UTF-8 filename validation
- [ ] Version negotiation (min/max configurable)
- [ ] Control plane updates for NFSv4 configuration
- [ ] NFSv4 handlers in internal/protocol/nfs/v4/
- [ ] E2E tests (TDD: tests first, then implementation)

#### v3.0 — NFSv4.1
- [ ] Sessions and sequence IDs
- [ ] Exactly-once semantics
- [ ] Directory delegations
- [ ] Backchannel over existing connection (NAT-friendly callbacks)
- [ ] Multiple connections per session (trunking-ready)
- [ ] DESTROY_CLIENTID for graceful cleanup
- [ ] Optional: SMB Kerberos via shared layer

#### v4.0 — NFSv4.2
- [ ] Server-side COPY (async with OFFLOAD_STATUS polling)
- [ ] OFFLOAD_CANCEL for in-progress copies
- [ ] CLONE/reflinks (leverage content-addressed storage)
- [ ] Sparse files: SEEK (data/hole), ALLOCATE, DEALLOCATE
- [ ] ZERO_RANGE for efficient zeroing
- [ ] Extended attributes (GETXATTR, SETXATTR, LISTXATTRS, REMOVEXATTR)
- [ ] Xattrs in metadata layer, exposed via NFSv4.2 and SMB
- [ ] Application I/O hints (IO_ADVISE)
- [ ] Default version: NFSv4.2 (configurable down to NFSv3)
- [ ] pjdfstest POSIX compliance (NFSv3 and NFSv4)
- [ ] Full documentation updates in docs/

### Out of Scope

- pNFS (parallel NFS) — deferred until scale-out architecture needed
- Labeled NFS (SELinux labels) — not required for target use cases
- NFSv3 xattr workarounds — xattrs via NFSv4.2/SMB only
- Cross-server COPY_NOTIFY — single server focus
- Bundled KDC — external AD/KDC only
- NFS over RDMA — standard TCP sufficient
- NFSv2 — obsolete, no demand

## Context

**Existing Codebase:**
- NFSv3 fully implemented in `internal/protocol/nfs/v3/`
- SMB adapter exists but lacks oplock/lease support
- Control plane has user/group/ACL infrastructure
- Metadata service architecture supports embedding lock manager
- Content-addressed storage enables efficient CLONE

**Target Environment:**
- Kubernetes-first (containerized)
- No kernel modules or privileged access required
- External Active Directory for Kerberos
- Single-instance initially (multi-instance future)

**Competitive Landscape:**
- JuiceFS: NFSv3 only, no v4, no Kerberos
- Hammerspace: NFSv3/v4/v4.1, limited v4.2, enterprise pricing
- DittoFS target: Full NFSv4.2 + Kerberos + cross-protocol locks

**Reference Implementations:**
- [Linux kernel fs/nfs](https://github.com/torvalds/linux/tree/master/fs/nfs) — client
- [Linux kernel fs/nfsd](https://github.com/torvalds/linux/tree/master/fs/nfsd) — server
- [nfs4j](https://github.com/dCache/nfs4j) — pure Java NFSv4.2

## Constraints

- **Code Location**: NFSv4 handlers in `internal/protocol/nfs/v4/`, follow existing patterns
- **Lock Manager**: Embedded in metadata service, not separate component
- **Lock Storage**: Same store as metadata (per-share)
- **Connection Pool**: Per-adapter (NFS pool, SMB pool), unified stateless/stateful
- **Kerberos**: External KDC only (Active Directory), AUTH_SYS fallback available
- **Testing**: TDD approach — E2E tests first, then implementation
- **Documentation**: Update `docs/` for all new features
- **Single Port**: NFSv4 uses port 2049 only (no mountd, NLM ports for v4)

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| NLM before NFSv4 | Build locking foundation first, reuse for NFSv4 | Completed — Phases 01-02 |
| Unified Lock Manager | Single lock model for NFS+SMB, translate at boundary | Completed — Phase 01 |
| Lock state in metadata store | Atomic with file operations, survives restarts | Completed — Phase 01 |
| Flexible lock model | Preserve native semantics (NLM/NFSv4/SMB), translate at boundary | Completed — Phase 05 |
| Full SMB2/3 leases in v1.0 | Cross-protocol consistency from day one | Completed — Phase 04 |
| Kerberos with NFSv4.0 | Standard pairing, security + stateful protocol | — Pending |
| Shared Kerberos layer | Reuse for NFSv4 (RPCSEC_GSS) and SMB (SPNEGO) | — Pending |
| External KDC only | Enterprise target uses AD, simplifies implementation | — Pending |
| Client-first flush | Standard delegation behavior, simpler consistency | — Pending |
| NFSv4.1 backchannel | NAT-friendly callbacks, works in containers | — Pending |
| Extend existing ACL model | Unified ACLs for NFSv4 and SMB | — Pending |
| Xattrs in metadata layer | Clean abstraction, expose via NFSv4.2 and SMB | — Pending |
| Async COPY with polling | Better for large files, standard NFSv4.2 pattern | — Pending |
| CLONE via content-addressed storage | Efficient reflinks using existing dedup infrastructure | — Pending |
| Auto-register with system rpcbind | NFS clients discover NLM via portmapper; warn (not fail) if rpcbind unavailable. NFS-Ganesha pattern. | — Pending |
| Per-adapter connection pools | Isolation between NFS and SMB, simpler limits | — Pending |
| TDD approach | E2E tests first ensures correct behavior | — Pending |

---
*Last updated: 2026-02-12*
