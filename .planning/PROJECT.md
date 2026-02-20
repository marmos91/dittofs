# DittoFS NFS Protocol Evolution

## What This Is

A comprehensive NFS protocol upgrade for DittoFS that adds NFSv4.0/4.1/4.2 support with Kerberos authentication, unified cross-protocol locking (NFS + SMB), and advanced features like server-side copy, sparse files, and delegations. v1.0 (NLM + unified locking) and v2.0 (NFSv4.0 + Kerberos) are shipped. Next: NFSv4.1 sessions (v3.0), then NFSv4.2 features (v4.0).

Target: Cloud-native enterprise NAS with feature parity exceeding JuiceFS and Hammerspace, particularly in security (Kerberos) and cross-protocol consistency.

## Current Milestone: v3.0 NFSv4.1 Sessions

**Goal:** Add NFSv4.1 session infrastructure with exactly-once semantics, backchannel multiplexing, and directory delegations for NAT-friendly, reliable enterprise NFS.

**Target features:**
- Sessions and sequence IDs (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION)
- Exactly-once semantics via session slot table replay cache
- Directory delegations (GET_DIR_DELEGATION, CB_NOTIFY)
- Backchannel over existing connection (NAT-friendly, no separate TCP)
- Multiple connections per session (trunking-ready)
- DESTROY_CLIENTID for graceful client cleanup
- Optional: SMB Kerberos via shared RPCSEC_GSS layer

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
- ✓ Unified Lock Manager embedded in metadata service — v1.0
- ✓ Lock state persistence in metadata store (per-share) — v1.0
- ✓ Flexible lock model (native semantics, translate at boundary) — v1.0
- ✓ NLM protocol (RPC program 100021) for NFSv3 — v1.0
- ✓ NSM protocol (RPC program 100024) for crash recovery — v1.0
- ✓ SMB2/3 lease support (Read, Write, Handle leases) — v1.0
- ✓ Cross-protocol lock coordination (NLM <-> SMB) — v1.0
- ✓ Grace period handling for server restarts — v1.0
- ✓ Per-adapter connection pool (unified stateless/stateful) — v1.0
- ✓ E2E tests for locking scenarios — v1.0
- ✓ NFSv4.0 compound operations (COMPOUND/CB_COMPOUND) — v2.0
- ✓ NFSv4 pseudo-filesystem (single namespace for all exports) — v2.0
- ✓ Client ID and state ID management — v2.0
- ✓ NFSv4 integrated locking (LOCK/LOCKT/LOCKU) — v2.0
- ✓ Read/write delegations with callback recall — v2.0
- ✓ NFSv4 ACLs (extend existing control plane ACL model) — v2.0
- ✓ RPCSEC_GSS for NFSv4 (krb5, krb5i, krb5p) — v2.0
- ✓ External KDC integration (Active Directory) — v2.0
- ✓ NFSv4 ID mapping (user@domain -> control plane users) — v2.0
- ✓ Lease management (renewal, expiration, ~90s default) — v2.0
- ✓ UTF-8 filename validation — v2.0
- ✓ Version negotiation (min/max configurable) — v2.0
- ✓ Control plane updates for NFSv4 configuration — v2.0
- ✓ NFSv4 handlers in internal/protocol/nfs/v4/ — v2.0
- ✓ Comprehensive E2E tests for NFSv4.0 — v2.0

### Active

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
- ACL enforcement in CheckAccess — deferred tech debt from v2.0 (POSIX permissions enforced instead)

## Context

**Current State (post-v2.0):**
- 224,306 LOC Go across ~1,294 files
- NFSv3 + NFSv4.0 + NLM + SMB fully implemented
- RPCSEC_GSS Kerberos (krb5/krb5i/krb5p) with keytab hot-reload
- 33+ NFSv4 operation handlers, StateManager with client IDs, stateids, leases
- Read/write delegations with CB_RECALL, recall timers, revocation
- NFSv4 ACLs with identity mapping and SMB Security Descriptor interop
- Control plane v2.0 with settings watcher, netgroup CRUD, blocked operations
- 50+ NFSv4 E2E tests covering all major features
- K8s operator with portmapper support

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
| NLM before NFSv4 | Build locking foundation first, reuse for NFSv4 | ✓ Good — Phases 01-02 |
| Unified Lock Manager | Single lock model for NFS+SMB, translate at boundary | ✓ Good — Phase 01 |
| Lock state in metadata store | Atomic with file operations, survives restarts | ✓ Good — Phase 01 |
| Flexible lock model | Preserve native semantics (NLM/NFSv4/SMB), translate at boundary | ✓ Good — Phase 05 |
| Full SMB2/3 leases in v1.0 | Cross-protocol consistency from day one | ✓ Good — Phase 04 |
| Kerberos with NFSv4.0 | Standard pairing, security + stateful protocol | ✓ Good — Phase 12 |
| Shared Kerberos layer | Reuse for NFSv4 (RPCSEC_GSS) and SMB (SPNEGO) | ✓ Good — Phase 12 |
| External KDC only | Enterprise target uses AD, simplifies implementation | ✓ Good — Phase 12 |
| Client-first flush | Standard delegation behavior, simpler consistency | ✓ Good — Phase 11 |
| Extend existing ACL model | Unified ACLs for NFSv4 and SMB | ✓ Good — Phase 13 |
| Streaming XDR decode | io.Reader cursor avoids pre-parsing all COMPOUND ops | ✓ Good — Phase 06 |
| StateManager single RWMutex | Avoids deadlocks across state types | ✓ Good — Phase 09 |
| Async CB_RECALL via goroutine | Prevents holding state lock during TCP callback | ✓ Good — Phase 11 |
| Package-level SetIdentityMapper | Runtime configuration without handler signature changes | ✓ Good — Phase 13 |
| SettingsWatcher 10s polling | Simple, reliable settings propagation to adapters | ✓ Good — Phase 14 |
| NFSv4.1 backchannel | NAT-friendly callbacks, works in containers | — Pending |
| Xattrs in metadata layer | Clean abstraction, expose via NFSv4.2 and SMB | — Pending |
| Async COPY with polling | Better for large files, standard NFSv4.2 pattern | — Pending |
| CLONE via content-addressed storage | Efficient reflinks using existing dedup infrastructure | — Pending |
| Auto-register with system rpcbind | NFS clients discover NLM via portmapper | ✓ Good — Embedded portmapper |
| Per-adapter connection pools | Isolation between NFS and SMB, simpler limits | ✓ Good — Phase 01 |

---
*Last updated: 2026-02-20 after v3.0 milestone start*
