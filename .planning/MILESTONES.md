# Project Milestones: DittoFS NFS Protocol Evolution

## v4.0 BlockStore Unification Refactor (Shipped: 2026-03-11)

**Delivered:** Complete storage layer refactor replacing confusing PayloadService/Cache/DirectWrite layers with clean two-tier block store model (Local + Remote), per-share isolation, read caching, and auto-configuration.

**Phases completed:** 41-49 (9 phases, 24 plans, ~49 tasks)

**Key accomplishments:**

- Renamed block state enum (Sealed/Uploaded -> Local/Remote) with clean terminology across all consumers
- Removed DirectWriteStore and filesystem payload store (-1,305 lines dead code)
- Added block lifecycle methods with eviction control for local-only mode (no remote store required)
- Created BlockStoreConfig data model with local/remote Kind discriminator, full REST API, and CLI commands
- Restructured into clean `pkg/blockstore/` hierarchy (local/fs, local/memory, remote/s3, remote/memory, engine, sync, gc, io)
- Implemented per-share BlockStore lifecycle with ref-counted shared remote stores and isolated local paths
- Built LRU read cache with copy-on-read semantics and adaptive sequential prefetcher
- Platform-aware auto-deduction (darwin sysctl, linux cgroup, windows GlobalMemoryStatusEx) for buffer/cache sizing
- Cache observability via REST API and CLI with per-share breakdown, syncer queue stats, and safety-checked eviction

**Stats:**

- 9 phases, 24 plans, ~49 tasks
- 394 files changed, +38,015 / -11,410 lines (net +26,605)
- 283,695 LOC Go
- Mar 9 - Mar 11, 2026 (2 days)

**Archive:** [v4.0-ROADMAP.md](milestones/v4.0-ROADMAP.md) | [v4.0-REQUIREMENTS.md](milestones/v4.0-REQUIREMENTS.md)

---

## v3.6 Windows Compatibility (Shipped: 2026-02-28)

**Delivered:** Full Windows SMB compatibility with NT Security Descriptors, SMB bug fixes, conformance test infrastructure, and comprehensive Windows 11 validation.

**Phases completed:** 29.8-32 (4 phases, 12 plans, 24 tasks)

**Key accomplishments:**

- SMB conformance test infrastructure: Dockerized WPTS (150/335 BVT) + smbtorture CI with known-failure classification
- Sparse file READ fix: zero-fill for unwritten blocks, fixing Windows Explorer read failures (#180)
- Directory rename path propagation: BFS recursive descendant update on Move (#181)
- NT Security Descriptors: POSIX-to-DACL synthesis, Samba-style SID mapping, well-known SIDs, lsarpc pipe (#182)
- SMB protocol enhancements: MxAc/QFid create contexts, FileInfoClass handlers, cross-protocol oplock breaks
- Windows 11 validation: 70+ test items validated, VM setup guide, KNOWN_FAILURES baseline

**Stats:**

- 4 phases, 12 plans, 24 tasks
- 136 files changed, +9,779 / -980 lines
- 262,607 LOC Go
- Feb 26 - Feb 28, 2026 (3 days)

**Archive:** [v3.6-ROADMAP.md](milestones/v3.6-ROADMAP.md) | [v3.6-REQUIREMENTS.md](milestones/v3.6-REQUIREMENTS.md)

---

## v1.0 NLM + Unified Lock Manager (Shipped: 2026-02-07)

**Delivered:** Unified cross-protocol locking foundation with NLM, NSM, SMB leases, and cross-protocol lock coordination.

**Phases completed:** 1-5 (19 plans total)

**Key accomplishments:**

- Unified Lock Manager embedded in metadata service with protocol-agnostic ownership model
- NLM protocol (RPC 100021) with blocking lock queue and GRANTED callbacks
- NSM protocol (RPC 100024) with crash recovery and parallel SM_NOTIFY
- SMB2/3 lease support (Read/Write/Handle) with 35s break timeout
- Cross-protocol lock coordination: NLM locks visible to SMB, SMB leases visible to NLM
- E2E tests for NLM locking, SMB leases, cross-protocol conflicts, and grace period recovery

**Stats:**

- 5 phases, 19 plans
- Feb 1 - Feb 7, 2026

**Archive:** [v1.0-ROADMAP.md](milestones/v1.0-ROADMAP.md) | [v1.0-REQUIREMENTS.md](milestones/v1.0-REQUIREMENTS.md)

---

## v2.0 NFSv4.0 + Kerberos (Shipped: 2026-02-20)

**Delivered:** Full NFSv4.0 stateful protocol implementation with RPCSEC_GSS Kerberos authentication, delegations, ACLs, identity mapping, and comprehensive E2E test suite.

**Phases completed:** 6-15 (42 plans total)

**Key accomplishments:**

- NFSv4.0 COMPOUND dispatcher with pseudo-filesystem and 33+ operation handlers
- Stateful NFSv4 protocol: client IDs, stateids, open/lock-owners, lease management, grace period
- Read/write delegations with CB_RECALL, recall timers, revocation, and anti-storm protection
- RPCSEC_GSS Kerberos authentication (krb5/krb5i/krb5p) with keytab hot-reload
- NFSv4 ACLs with identity mapping, SMB Security Descriptor interop, and control plane integration
- Comprehensive E2E test suite: 50+ NFSv4 tests covering locking, delegations, Kerberos, ACLs, POSIX compliance

**Stats:**

- 10 phases, 42 plans
- 224,306 LOC Go
- Feb 7 - Feb 20, 2026 (13 days)

**Known tech debt:**

- ACL evaluation not yet integrated into metadata CheckAccess (wire format works)
- Delegation Prometheus metrics not instrumented (log scraping workaround)
- Netgroup mount enforcement not implemented (CRUD works via API)

**Archive:** [v2.0-ROADMAP.md](milestones/v2.0-ROADMAP.md) | [v2.0-REQUIREMENTS.md](milestones/v2.0-REQUIREMENTS.md) | [v2.0-MILESTONE-AUDIT.md](milestones/v2.0-MILESTONE-AUDIT.md)

---


## v3.0 NFSv4.1 Sessions (Shipped: 2026-02-25)

**Delivered:** NFSv4.1 session infrastructure with exactly-once semantics, backchannel multiplexing, directory delegations, trunking, and SMB Kerberos authentication.

**Phases completed:** 16-25 (25 plans total)

**Key accomplishments:**

- NFSv4.1 XDR types and constants: 19 forward ops, 10 callback ops, 40+ error codes, full encode/decode
- Session infrastructure: EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION with slot table allocation and channel negotiation
- Exactly-once semantics via per-slot replay cache with SEQUENCE validation on every v4.1 COMPOUND
- Backchannel multiplexing: CB_SEQUENCE over fore-channel TCP connection (NAT-friendly, no separate dial-out)
- Connection management and trunking: BIND_CONN_TO_SESSION, multi-connection sessions, server_owner consistency
- Client lifecycle: DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID, RECLAIM_COMPLETE, grace period API
- Directory delegations: GET_DIR_DELEGATION, CB_NOTIFY with batched notifications and conflict recall
- SMB Kerberos: SPNEGO/Kerberos in SESSION_SETUP with shared Kerberos layer and identity mapping
- v4.0/v4.1 coexistence: minorversion routing, independent state, simultaneous mounts
- E2E tests: session lifecycle, EOS replay, backchannel delegation recall, directory notifications, disconnect robustness

**Stats:**

- 10 phases, 25 plans
- 256,842 LOC Go
- 336 files changed, +61,004 / -5,037 lines
- Feb 20 - Feb 25, 2026 (5 days)

**Known tech debt:**

- ACL enforcement in CheckAccess (carried from v2.0)
- Delegation Prometheus metrics (carried from v2.0)
- Netgroup mount enforcement (carried from v2.0)
- LIFE-01 through LIFE-04 traceability entries stale (work complete, table not updated)

**Archive:** [v3.0-ROADMAP.md](milestones/v3.0-ROADMAP.md) | [v3.0-REQUIREMENTS.md](milestones/v3.0-REQUIREMENTS.md)

---


## v3.5 Adapter + Core Refactoring (Shipped: 2026-02-26)

**Delivered:** Clean separation of protocol-specific code from generic layers, unified lock model, restructured NFS/SMB adapters with shared infrastructure, and decomposed core objects for maintainability.

**Phases completed:** 26-29.4 (5 phases, 22 plans)

**Key accomplishments:**

- Unified lock model (OpLock/AccessMode/UnifiedLock) shared by NFS, SMB, and NLM with centralized conflict detection
- Protocol leak purge: removed ~15 protocol-specific types/methods from generic metadata, controlplane, and lock layers
- NFS adapter restructured: `internal/protocol/` -> `internal/adapter/nfs/`, v4/v4.1 hierarchy split, consolidated dispatch
- SMB adapter restructured: BaseAdapter shared with NFS, Authenticator interface, framing/signing/dispatch extracted
- Core decomposed: Store interface split into 9 sub-interfaces, Runtime into 6 sub-services, Offloader renamed and split into 8 files
- Error and boilerplate reduction: PayloadError type, generic GORM/API helpers, centralized API error mapping, metadata file splits

**Stats:**

- 5 phases, 22 plans
- 244 files changed, +23,305 / -10,771 lines
- Feb 25 - Feb 26, 2026 (2 days)

**Known tech debt:**

- REF-01.8/REF-01.9 adapter translation layers deferred to v3.8
- 4 TODO(plan-03) cross-protocol oplock break markers (requires v3.8)
- PayloadError defined but not yet wired into production error paths

**Archive:** [v3.5-ROADMAP.md](milestones/v3.5-ROADMAP.md) | [v3.5-REQUIREMENTS.md](milestones/v3.5-REQUIREMENTS.md) | [v3.5-MILESTONE-AUDIT.md](milestones/v3.5-MILESTONE-AUDIT.md)

---


## v3.8 SMB3 Protocol Upgrade (Shipped: 2026-03-04)

**Delivered:** Full SMB3.0/3.0.2/3.1.1 protocol support with enterprise security (AES encryption, signing, Kerberos), leases, durable handles, cross-protocol integration, and conformance testing.

**Phases completed:** 33-40.5 (8 phases, 26 plans)

**Key accomplishments:**

- SMB3 dialect negotiation (3.0/3.0.2/3.1.1) with preauth integrity hash chain and VALIDATE_NEGOTIATE_INFO
- AES encryption (128/256-bit GCM/CCM) with transform header framing, per-session and per-share enforcement
- SP800-108 KDF key derivation with AES-CMAC/GMAC signing replacing legacy HMAC-SHA256
- SPNEGO/Kerberos authentication via shared Kerberos layer with automatic NTLM fallback
- SMB3 Lease V2 with directory leasing, epoch tracking, and cross-protocol coordination with NFS delegations
- Durable handles V1/V2 with 14+ reconnect checks, persistent state store, and scavenger lifecycle
- Cross-protocol integration: bidirectional SMB lease/NFS delegation breaks, unified lock manager coordination
- Conformance testing: smbtorture baselines, WPTS BVT, go-smb2 E2E, multi-OS CI, Windows 11 verification

**Stats:**

- 8 phases, 26 plans
- 262 files changed, +44,956 / -5,352 lines
- Mar 1 - Mar 4, 2026 (4 days)

**Known gaps:**

- TEST-01: smbtorture full pass rate — infrastructure built, iterative improvement ongoing

**Archive:** [v3.8-ROADMAP.md](milestones/v3.8-ROADMAP.md) | [v3.8-REQUIREMENTS.md](milestones/v3.8-REQUIREMENTS.md)

---

