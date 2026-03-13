# DittoFS NFS Protocol Evolution

## What This Is

A comprehensive multi-protocol virtual filesystem with NFSv3/NFSv4.0/NFSv4.1 and SMB3.1.1 support, Kerberos authentication, unified cross-protocol locking, and advanced features like delegations, leases, durable handles, and encryption. v1.0 through v4.3 shipped. Next: Offline/Edge Resilience (v4.7) — fix cache eviction for edge deployments and enable disconnected operation with local-first reads/writes and auto-sync on reconnect.

Target: Cloud-native enterprise NAS with feature parity exceeding JuiceFS and Hammerspace, particularly in security (Kerberos + AES encryption), session reliability (EOS), cross-protocol consistency, Windows SMB3.1.1 compatibility, and edge/offline resilience.

## Current Milestone: v4.7 Offline/Edge Resilience

**Goal:** Fix cache eviction for edge deployments and enable disconnected operation with local-first reads/writes and auto-sync on reconnect.

**Target features:**
- Diagnose & fix local block eviction (movies vanish after 3 days on edge nodes)
- Per-share retention policy (pin mode + configurable TTL) in control plane share config
- Offline read resilience — serve from local cache when S3 is unreachable
- Offline write support — accept writes locally, queue for S3 sync on reconnect
- Connectivity detection & auto-sync on reconnect
- Test infrastructure — Scaleway VMs via Pulumi, edge scenario reproduction, offline simulation via S3 endpoint block

## Upcoming Milestones

- **v4.5 BlockStore Security** — Block-level compression and encryption
- **v4.6 Production Hardening** — SMB3 signing fix, quotas, client tracking, trash
- **v4.8 DX/UX Improvements** — Makefile, CI optimization, adapter config API
- **v4.9 SMB Protocol Fixes** — Credits, multichannel, conformance
- **v5.0 NFSv4.2 Extensions** — Server-side copy, clone/reflinks, sparse files, xattrs

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
- ✓ Sparse file READ fix (#180) — zero-fill for unwritten blocks — v3.6
- ✓ Renamed directory listing fix (#181) — recursive path update on Move — v3.6
- ✓ NT Security Descriptors (#182) — POSIX-to-DACL synthesis, SID mapping, lsarpc — v3.6
- ✓ Unix-to-Windows SID mapping with Samba-style RID allocation — v3.6
- ✓ smbtorture SMB2 conformance testing with Docker-isolated infrastructure — v3.6
- ✓ Microsoft WindowsProtocolTestSuites BVT (150/335 passing) — v3.6
- ✓ Windows 11 manual validation (Explorer, cmd, PowerShell, icacls) — v3.6
- ✓ Cross-protocol oplock break coordination (NFS ops trigger SMB breaks) — v3.6
- ✓ MxAc/QFid create contexts and FileInfoClass handler completeness — v3.6
- ✓ SMB 3.0/3.0.2/3.1.1 dialect negotiation with negotiate contexts — v3.8
- ✓ Preauth integrity (SHA-512 hash chain) and secure dialect negotiation — v3.8
- ✓ AES encryption (128/256-bit CCM/GCM) configurable per share — v3.8
- ✓ AES signing (CMAC for 3.0+, GMAC for 3.1.1) — v3.8
- ✓ SMB3 leases (Read/Write/Handle + directory) with Unified Lock Manager integration — v3.8
- ✓ SMB lease <-> NFS delegation cross-protocol coordination — v3.8
- ✓ SPNEGO/Kerberos via shared layer, NTLM fallback, guest access — v3.8
- ✓ Durable handles v1/v2 for connection resilience — v3.8
- ✓ Cross-protocol integration (immediate visibility, bidirectional locking, ACL consistency) — v3.8
- ✓ E2E tests for encryption, signing, leases, Kerberos, and cross-protocol scenarios — v3.8
- ✓ Client compatibility: Windows 11 verified, multi-OS CI — v3.8
- ✓ Microsoft WindowsProtocolTestSuites FileServer conformance (BVT + SMB3 infrastructure) — v3.8
- ✓ Go integration tests (hirochachacha/go-smb2) for native client-server interop — v3.8
- ✓ Two-tier block store model (Local + Remote) replacing PayloadService — v4.0
- ✓ Per-share BlockStore with isolated local paths and ref-counted remote stores — v4.0
- ✓ pkg/blockstore/ package hierarchy (local/fs, remote/s3, engine, sync, gc, io) — v4.0
- ✓ BlockStoreConfig data model with Kind discriminator, REST API, CLI commands — v4.0
- ✓ L1 read cache (LRU) with adaptive sequential prefetcher — v4.0
- ✓ Platform-aware auto-deduced configuration (darwin/linux/windows) — v4.0
- ✓ Cache observability via REST API and CLI — v4.0
- ✓ NFSv4 READDIR cookie verifier (mtime-based, advisory validation) — v4.3
- ✓ READDIRPLUS performance (DirEntry.Attr in all stores) — v4.0 (verified v4.3)
- ✓ LSA named pipe (lsarpc with SID-to-name identity resolution) — v3.6/v3.8 (verified v4.3)

### Active

#### v4.7 — Offline/Edge Resilience
- [ ] Cache eviction diagnosis — identify why local blocks vanish after 3 days on edge deployments
- [ ] Per-share retention policy — pin mode (default) + configurable TTL in share config
- [ ] Offline read resilience — serve from local cache when S3 is unreachable
- [ ] Offline write support — write locally, queue for S3 sync on reconnect
- [ ] Connectivity detection & auto-sync — detect S3 availability, resume sync automatically
- [ ] Edge test infrastructure — Scaleway VMs via Pulumi, offline simulation via S3 endpoint block

#### v4.6 — Production Hardening
- [ ] SMB 3.1.1 signing on macOS — fix preauth integrity hash mismatch (#252)
- [ ] Share hot-reload — runtime shares visible to all adapters without restart (#235)
- [ ] NTLM encryption flags — stop advertising unimplemented capabilities (#215)
- [ ] Share quotas — per-share quota with FSSTAT/FSINFO/SMB reporting (#232)
- [ ] Payload stats — UsedSize returns actual storage usage (#216)
- [ ] Protocol-agnostic ClientRecord — unified NFS+SMB client tracking, `dfsctl client list` (#157)
- [ ] Trash / soft-delete — move-to-trash with configurable retention (#190)

#### v5.0 — NFSv4.2
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

**Current State (post-v4.3):**
- ~283,700 LOC Go
- NFSv3 + NFSv4.0 + NFSv4.1 + NLM + SMB3.1.1 fully implemented
- SMB3 security: AES encryption (128/256 GCM/CCM), AES signing (CMAC/GMAC), preauth integrity
- SMB3 features: Lease V2 with directory leasing, durable handles V1/V2, VALIDATE_NEGOTIATE_INFO
- SPNEGO/Kerberos authentication with automatic NTLM fallback and guest session support
- Cross-protocol coordination: SMB3 leases <-> NFS delegations bidirectional breaks
- Conformance testing: smbtorture baselines, WPTS BVT, go-smb2 E2E, multi-OS CI
- Windows 11 ARM64 verified: 10/10 manual verification tests passed over SMB 3.1.1
- K8s operator with portmapper support

**Known tech debt:**
- ACL enforcement in CheckAccess deferred (POSIX permissions enforced instead)
- Delegation Prometheus metrics not instrumented
- SACL is empty stub (requires audit logging infrastructure)
- Short name (8.3) generation deferred
- CHANGE_NOTIFY cleanup on disconnect deferred
- smbtorture full pass rate (infrastructure built, iterative improvement ongoing)

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

- **Code Location**: NFSv4 handlers in `internal/adapter/nfs/v4/`
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
| Windows ACLs before NFSv4.2 | SMB is primary Windows use case, validate before adding features | ✓ Good — v3.6 |
| OpLock as generic abstraction | Unifies SMB leases and NFSv4 delegations, fix once for all | ✓ Good — v3.5 |
| smbtorture + MS Protocol Suite | Open-source conformance testing for SMB compatibility | ✓ Good — v3.6 |
| Samba-style RID allocation | uid*2+1000, gid*2+1001 prevents user/group SID collisions | ✓ Good — v3.6 |
| POSIX-to-DACL synthesis | Generate Windows ACLs from Unix mode bits, no separate ACL store | ✓ Good — v3.6 |
| Docker-isolated smbtorture | GPL compliance via container boundary, no direct binary contact | ✓ Good — v3.6 |
| Zero-fill sparse reads at download level | Single fix benefits both NFS and SMB protocol paths | ✓ Good — v3.6 |
| BFS for descendant path updates | Iterative queue avoids stack overflow on deep directory trees | ✓ Good — v3.6 |
| Two-tier block store model | Clean Local+Remote replaces confusing PayloadService/Cache/DirectWrite layers | ✓ Good — v4.0 |
| Per-share block stores | Different local paths and remote backends per share, replaces global PayloadService | ✓ Good — v4.0 |
| BlockStore refactor before NFSv4.2 | Clean storage architecture enables easier feature development | ✓ Good — v4.0 |
| Copy-on-read L1 cache | Prevents buffer corruption from concurrent reads | ✓ Good — Phase 47 |
| Platform-aware sysinfo | Auto-deduce config from darwin/linux/windows system resources | ✓ Good — Phase 48 |
| Ref-counted shared remote stores | Multiple shares share one remote store, nonClosingRemote prevents premature close | ✓ Good — Phase 46 |
| Xattrs in metadata layer | Clean abstraction, expose via NFSv4.2 and SMB | — Pending (v4.1) |
| Async COPY with polling | Better for large files, standard NFSv4.2 pattern | — Pending (v4.1) |
| CLONE via content-addressed storage | Efficient reflinks using existing dedup infrastructure | — Pending (v4.1) |
| Benchmark after NFSv4.2 | Complete protocol implementation before performance iteration | — Pending (v4.2) |
| Docker Compose per-system profiles | Fair comparison: one system at a time, symmetric overhead | — Pending (v4.2) |
| SMB3 before NFSv4.2 | Complete SMB protocol upgrade, validate cross-protocol before adding NFS features | ✓ Good — v3.8 |
| Shared Kerberos layer for SMB3 | Reuse existing RPCSEC_GSS infrastructure from NFSv4 | ✓ Good — Phase 36 |
| Buffer-based smbenc codec | Error accumulation, not streaming, for SMB3 wire encoding | ✓ Good — Phase 33 |
| Dispatch hooks for cross-cutting concerns | Before/after hooks per command for preauth hash, signing, encryption | ✓ Good — Phase 33 |
| SP800-108 KDF with dialect-aware context | Preauth hash for 3.1.1, constant strings for 3.0/3.0.2 | ✓ Good — Phase 34 |
| Vendored CCM from pion/dtls | Avoid 50+ transitive dependencies for AES-CCM | ✓ Good — Phase 35 |
| Business logic in metadata service | Leases, durable handles, state in shared layer, not SMB internal | ✓ Good — Phase 37-38 |
| Recently-broken cache for anti-storm | 5s TTL prevents lease grant-break storms | ✓ Good — Phase 37 |
| Auto-register with system rpcbind | NFS clients discover NLM via portmapper | ✓ Good — Embedded portmapper |
| Per-adapter connection pools | Isolation between NFS and SMB, simpler limits | ✓ Good — Phase 01 |
| Mtime-based NFSv4 READDIR cookie verifier | Matches NFSv3 pattern, advisory-only mismatch validation | ✓ Good — Phase 49.1 |
| Advisory verifier (never NFS4ERR_NOT_SAME) | Lenient approach avoids breaking clients on directory changes | ✓ Good — Phase 49.1 |

---
*Last updated: 2026-03-13 after v4.7 Offline/Edge Resilience milestone started*
