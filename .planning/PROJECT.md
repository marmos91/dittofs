# DittoFS NFS Protocol Evolution

## What This Is

A comprehensive NFS protocol upgrade for DittoFS that adds NFSv4.0/4.1/4.2 support with Kerberos authentication, unified cross-protocol locking (NFS + SMB), and advanced features like server-side copy, sparse files, and delegations. v1.0 (NLM + unified locking), v2.0 (NFSv4.0 + Kerberos), and v3.0 (NFSv4.1 sessions) are shipped. Next: adapter/core refactoring (v3.5), Windows compatibility (v3.6), benchmarking suite (v3.7), and SMB3 protocol upgrade (v3.8) before NFSv4.2 features (v4.0).

Target: Cloud-native enterprise NAS with feature parity exceeding JuiceFS and Hammerspace, particularly in security (Kerberos), session reliability (EOS), cross-protocol consistency, and Windows SMB compatibility.

## Current Milestone: v3.6 Windows Compatibility

**Goal:** Full Windows SMB compatibility with proper ACL support, bug fixes, and comprehensive testing.

**Target outcomes:**
- SMB bugs fixed (#180 sparse file READ, #181 renamed dir listing)
- NT Security Descriptors with Unix-to-SID mapping and icacls support (#182)
- Improved SMB compatibility driven by existing smbtorture/WPTS test suite results
- Validated with smbtorture, Microsoft WindowsProtocolTestSuites, and manual Windows 11 testing

## Upcoming Milestone: v3.7 Benchmarking Suite

**Goal:** Comprehensive benchmarking suite comparing DittoFS against competitors (JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba) to prove performance advantage of pure-Go, FUSE-less architecture.

**Target outcomes:**
- Docker Compose infrastructure with profiles for each system under test (#194)
- fio workloads (sequential, random, mixed) and metadata benchmark scripts (#195)
- Orchestrator scripts with platform variants (Linux, macOS, Windows SMB) (#196)
- Python analysis pipeline with charts and markdown report generation (#197)
- Competitor configuration and setup scripts for fair comparison (#198)
- DittoFS profiling integration (Prometheus, Pyroscope, pprof) for bottleneck identification (#199)
- `dfs bench` CLI command for user-facing performance evaluation (#188)

**Tracking:** [GitHub #193](https://github.com/marmos91/dittofs/issues/193)

## Upcoming Milestone: v3.8 SMB3 Protocol Upgrade

**Goal:** Upgrade SMB implementation from SMB2.0.2/2.1 to full SMB3.0/3.0.2/3.1.1 support with enterprise-grade security, leases, Kerberos authentication, durable handles, and cross-protocol integration.

**Target outcomes:**
- SMB3 dialect negotiation (3.0, 3.0.2, 3.1.1) with preauth integrity
- AES encryption (128/256-bit CCM/GCM) and signing (CMAC/GMAC)
- SMB3 leases (Read/Write/Handle + directory) integrated with Unified Lock Manager
- SPNEGO/Kerberos authentication via shared Kerberos layer + NTLM fallback
- Windows security descriptors (SID/ACE/DACL) with control plane ACL translation
- Durable handles v1/v2 for connection resilience
- Cross-protocol integration (immediate visibility, bidirectional locking, ACL consistency)

**Source:** feat/smb3 branch planning

## Core Value

Enable enterprise-grade multi-protocol file access (NFSv3, NFSv4.x, SMB3) with unified locking, Kerberos authentication, and immediate cross-protocol visibility — all deployable in containerized Kubernetes environments with first-class Windows client support.

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
- ✓ Sessions and sequence IDs (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION) — v3.0
- ✓ Exactly-once semantics via session slot table replay cache — v3.0
- ✓ Directory delegations (GET_DIR_DELEGATION, CB_NOTIFY) — v3.0
- ✓ Backchannel over existing connection (NAT-friendly callbacks) — v3.0
- ✓ Multiple connections per session (trunking-ready) — v3.0
- ✓ DESTROY_CLIENTID for graceful cleanup — v3.0
- ✓ SMB Kerberos via shared RPCSEC_GSS layer — v3.0
- ✓ Generic lock interface (OpLock/AccessMode/UnifiedLock) unifying NFS+SMB+NLM — v3.5
- ✓ Protocol leak purge from generic layers — v3.5
- ✓ NFS adapter restructuring (internal/adapter/, v4/v4.1 split, dispatch consolidation) — v3.5
- ✓ SMB adapter restructuring (BaseAdapter, framing/signing/dispatch to internal/) — v3.5
- ✓ Store interface decomposition (60+ methods -> 9 sub-interfaces) — v3.5
- ✓ Runtime decomposition (AdapterManager, MetadataStoreManager extraction) — v3.5
- ✓ TransferManager -> Offloader rename and split — v3.5
- ✓ Error unification and boilerplate reduction — v3.5

### Active

#### v3.6 — Windows Compatibility
- [ ] Sparse file READ fix (#180) — return zeros for unwritten blocks
- [ ] Renamed directory listing fix (#181) — update Path in Move operation
- [ ] NT Security Descriptors (#182) — Owner SID, Group SID, DACL encoding
- [ ] Unix-to-Windows SID mapping for icacls support
- [ ] smbtorture SMB2 conformance testing
- [ ] Microsoft WindowsProtocolTestSuites BVT and feature tests
- [ ] Full Windows 11 manual validation (Explorer, cmd, PowerShell)

#### v3.7 — Benchmarking Suite
- [ ] Docker Compose infrastructure with per-system profiles and shared services (#194)
- [ ] fio workload files (seq read/write, random 4K, mixed rw) and metadata benchmark script (#195)
- [ ] Orchestrator scripts (run-bench.sh) with platform variants for Linux, macOS, Windows (#196)
- [ ] Python analysis pipeline (parse fio/metadata, generate charts, markdown report) (#197)
- [ ] Competitor configs and setup scripts (JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba) (#198)
- [ ] DittoFS profiling integration (Prometheus, Pyroscope, pprof capture) (#199)
- [ ] Statistical rigor: 3+ iterations, p50/p95/p99, stddev across all workloads
- [ ] Benchmark CLI (`dfs bench <mountpoint>`) for user-facing performance evaluation (#188)

#### v3.8 — SMB3 Protocol Upgrade
- [ ] SMB 3.0/3.0.2/3.1.1 dialect negotiation with negotiate contexts
- [ ] Preauth integrity (SHA-512 hash chain) and secure dialect negotiation
- [ ] AES encryption (128/256-bit CCM/GCM) configurable per share
- [ ] AES signing (CMAC for 3.0+, GMAC for 3.1.1)
- [ ] SMB3 leases (Read/Write/Handle + directory) with Unified Lock Manager integration
- [ ] SMB lease <-> NFS delegation cross-protocol coordination
- [ ] SPNEGO/Kerberos via shared layer, NTLM fallback, guest access
- [ ] Windows security descriptors (SID/ACE/DACL) with control plane translation
- [ ] Durable handles v1/v2 for connection resilience
- [ ] Cross-protocol integration (immediate visibility, bidirectional locking, ACL consistency)
- [ ] E2E tests for encryption, signing, leases, Kerberos, and cross-protocol scenarios
- [ ] Client compatibility: Windows 10/11, macOS, Linux
- [ ] Microsoft WindowsProtocolTestSuites FileServer conformance (BVT + SMB3 feature tests)
- [ ] Samba smbtorture SMB3 protocol torture tests (durable_v2, lease, replay, session)
- [ ] Go integration tests (hirochachacha/go-smb2) for native client-server interop

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

**Current State (post-v3.5):**
- ~269,000 LOC Go across ~1,600 files (net +12,534 from refactoring)
- NFSv3 + NFSv4.0 + NFSv4.1 + NLM + SMB fully implemented
- Clean adapter layer: internal/adapter/nfs/ and internal/adapter/smb/ with shared BaseAdapter
- Decomposed core: 9 store sub-interfaces, 6 runtime sub-services, Offloader split into 8 files
- NFSv4.1 sessions with exactly-once semantics, backchannel multiplexing, directory delegations
- RPCSEC_GSS Kerberos (krb5/krb5i/krb5p) with keytab hot-reload
- SMB Kerberos via SPNEGO with shared identity mapping
- 33+ NFSv4 operation handlers + 19 v4.1 handlers
- StateManager with client IDs, sessions, stateids, slot tables, leases
- Connection management with trunking support (multi-conn per session)
- Read/write/directory delegations with CB_RECALL and CB_NOTIFY
- NFSv4 ACLs with identity mapping and SMB Security Descriptor interop
- Control plane v2.0 with settings watcher, netgroup CRUD
- 50+ NFSv4 E2E tests + NFSv4.1 session/EOS/backchannel/delegation tests
- K8s operator with portmapper support
- Windows build support (cross-compilation)

**Known tech debt (to be addressed in v3.5):**
- Protocol-specific types leak into generic layers (~15 types/methods)
- God objects: Runtime (1,258 lines), MetadataService (994 lines), TransferManager (1,361 lines)
- Monolithic ControlPlane Store interface (60+ methods)
- SMB connection.go is 1,060-line monolith
- NFS adapter has `nfs_` prefix redundancy, v4/v4.1 code interleaved

**Target Environment:**
- Kubernetes-first (containerized)
- No kernel modules or privileged access required
- External Active Directory for Kerberos
- Single-instance initially (multi-instance future)
- Windows 11 clients as primary SMB target

**Competitive Landscape:**
- JuiceFS: NFSv3 only, no v4, no Kerberos
- Hammerspace: NFSv3/v4/v4.1, limited v4.2, enterprise pricing
- DittoFS target: Full NFSv4.2 + Kerberos + cross-protocol locks + sessions + Windows ACLs

**Reference Implementations:**
- [Linux kernel fs/nfs](https://github.com/torvalds/linux/tree/master/fs/nfs) — client
- [Linux kernel fs/nfsd](https://github.com/torvalds/linux/tree/master/fs/nfsd) — server
- [nfs4j](https://github.com/dCache/nfs4j) — pure Java NFSv4.2
- [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) — SMB2 conformance (MIT)
- [Samba smbtorture](https://wiki.samba.org/index.php/Writing_Torture_Tests) — SMB protocol testing (GPL)

## Constraints

- **Code Location**: NFSv4 handlers in `internal/protocol/nfs/v4/` (will become `internal/adapter/nfs/v4/` in v3.5)
- **Lock Manager**: Embedded in metadata service, not separate component
- **Lock Storage**: Same store as metadata (per-share)
- **Connection Pool**: Per-adapter (NFS pool, SMB pool), unified stateless/stateful
- **Kerberos**: External KDC only (Active Directory), AUTH_SYS fallback available
- **Testing**: TDD approach — E2E tests first, then implementation
- **Documentation**: Update `docs/` for all new features
- **Single Port**: NFSv4 uses port 2049 only (no mountd, NLM ports for v4)
- **Refactoring**: Each step must compile and pass all tests independently

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| NLM before NFSv4 | Build locking foundation first, reuse for NFSv4 | ✓ Good — Phases 01-02 |
| Unified Lock Manager | Single lock model for NFS+SMB, translate at boundary | ✓ Good — Phase 01 |
| Lock state in metadata store | Atomic with file operations, survives restarts | ✓ Good — Phase 01 |
| Flexible lock model | Preserve native semantics (NLM/NFSv4/SMB), translate at boundary | ✓ Good — Phase 05 |
| Full SMB2/3 leases in v1.0 | Cross-protocol consistency from day one | ✓ Good — Phase 04 |
| Kerberos with NFSv4.0 | Standard pairing, security + stateful protocol | ✓ Good — Phase 12 |
| Shared Kerberos layer | Reuse for NFSv4 (RPCSEC_GSS) and SMB (SPNEGO) | ✓ Good — Phase 12, 25 |
| External KDC only | Enterprise target uses AD, simplifies implementation | ✓ Good — Phase 12 |
| Client-first flush | Standard delegation behavior, simpler consistency | ✓ Good — Phase 11 |
| Extend existing ACL model | Unified ACLs for NFSv4 and SMB | ✓ Good — Phase 13 |
| Streaming XDR decode | io.Reader cursor avoids pre-parsing all COMPOUND ops | ✓ Good — Phase 06 |
| StateManager single RWMutex | Avoids deadlocks across state types | ✓ Good — Phase 09 |
| Async CB_RECALL via goroutine | Prevents holding state lock during TCP callback | ✓ Good — Phase 11 |
| Package-level SetIdentityMapper | Runtime configuration without handler signature changes | ✓ Good — Phase 13 |
| SettingsWatcher 10s polling | Simple, reliable settings propagation to adapters | ✓ Good — Phase 14 |
| Per-SlotTable mutex | Avoids global lock contention on SEQUENCE hot path | ✓ Good — Phase 17 |
| Separate connMu RWMutex | Connection state isolation from global state lock | ✓ Good — Phase 21 |
| Backchannel over fore-channel | NAT-friendly callbacks, works in containers | ✓ Good — Phase 22 |
| Separate NotifMu per delegation | Avoids holding global lock during backchannel sends | ✓ Good — Phase 24 |
| v4.0/v4.1 coexistence | Minorversion routing, independent state, simultaneous mounts | ✓ Good — Phase 20 |
| Refactor before NFSv4.2 | Clean architecture enables faster v4.2 implementation | ✓ Good — v3.5 |
| Windows ACLs before NFSv4.2 | SMB is primary Windows use case, validate before adding features | — Pending (v3.6) |
| OpLock as generic abstraction | Unifies SMB leases and NFSv4 delegations, fix once for all | ✓ Good — v3.5 |
| smbtorture + MS Protocol Suite | Open-source conformance testing for SMB compatibility | — Pending (v3.6) |
| Xattrs in metadata layer | Clean abstraction, expose via NFSv4.2 and SMB | — Pending (v4.0) |
| Async COPY with polling | Better for large files, standard NFSv4.2 pattern | — Pending (v4.0) |
| CLONE via content-addressed storage | Efficient reflinks using existing dedup infrastructure | — Pending (v4.0) |
| Benchmark before NFSv4.2 | Establish performance baseline before adding complexity | — Pending (v3.7) |
| Docker Compose per-system profiles | Fair comparison: one system at a time, symmetric overhead | — Pending (v3.7) |
| SMB3 before NFSv4.2 | Complete SMB protocol upgrade, validate cross-protocol before adding NFS features | — Pending (v3.8) |
| Shared Kerberos layer for SMB3 | Reuse existing RPCSEC_GSS infrastructure from NFSv4 | — Pending (v3.8) |
| Auto-register with system rpcbind | NFS clients discover NLM via portmapper | ✓ Good — Embedded portmapper |
| Per-adapter connection pools | Isolation between NFS and SMB, simpler limits | ✓ Good — Phase 01 |

---
*Last updated: 2026-02-26 after starting v3.6 milestone*
