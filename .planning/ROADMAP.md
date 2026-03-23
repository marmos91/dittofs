# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across fifteen milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, signing, leases, Kerberos, and durable handles, v4.0 refactors the storage layer into a clean two-tier block store model (Local + Remote), v4.2 delivers benchmarking infrastructure, v4.3 fixes NFS/SMB protocol gaps, v4.7 adds offline/edge resilience with cache retention policies and disconnected operation, v0.10.0 hardens the system for production with share quotas, client tracking, trash/soft-delete, full SMB credit flow control, WPTS conformance fixes, and multi-channel session binding. Future milestones include block-level compression/encryption (v4.5), developer experience improvements (v4.8), and NFSv4.2 extensions (v5.0). Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [x] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (shipped 2026-02-26) — [archive](milestones/v3.5-ROADMAP.md)
- [x] **v3.6 Windows Compatibility** - Phases 29.8-32.5 (shipped 2026-02-28) — [archive](milestones/v3.6-ROADMAP.md)
- [x] **v3.8 SMB3 Protocol Upgrade** - Phases 33-40.5 (shipped 2026-03-04) — [archive](milestones/v3.8-ROADMAP.md)
- [x] **v4.0 BlockStore Unification Refactor** - Phases 41-49 (shipped 2026-03-11) — [archive](milestones/v4.0-ROADMAP.md)
- [x] **v4.2 Benchmarking & Performance** - Phases 57-62 (shipped 2026-03-04)
- [x] **v4.3 Protocol Gap Fixes** - Phases 49.1-49.3 (shipped 2026-03-13)
- [x] **v4.7 Offline/Edge Resilience** - Phases 63-68 (shipped 2026-03-20) — [archive](milestones/v4.7-ROADMAP.md)
- [ ] **v0.10.0 Production Hardening + SMB Protocol Fixes** - Phases 69-75 (planned)
- [ ] **v4.5 BlockStore Security** - Phases 49.4-49.5 (planned)
- [ ] **v4.8 DX/UX Improvements** - Phases 76-78 (planned)
- [ ] **v5.0 NFSv4.2 Extensions** - Phases 50-56 (planned)

**USER CHECKPOINT** phases require your manual testing before proceeding. Use `/gsd:verify-work` to validate.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

<details>
<summary>[x] v1.0 NLM + Unified Lock Manager (Phases 1-5) - SHIPPED 2026-02-07</summary>

- [x] **Phase 1: Locking Infrastructure** - Unified lock manager embedded in metadata service
- [x] **Phase 2: NLM Protocol** - Network Lock Manager for NFSv3 clients
- [x] **Phase 3: NSM Protocol** - Network Status Monitor for crash recovery
- [x] **Phase 4: SMB Leases** - SMB2/3 oplock and lease support
- [x] **Phase 5: Cross-Protocol Integration** - Lock visibility across NFS and SMB
- [x] **Phase 5.5: Manual Verification v1.0** USER CHECKPOINT

</details>

<details>
<summary>[x] v2.0 NFSv4.0 + Kerberos (Phases 6-15) - SHIPPED 2026-02-20</summary>

- [x] **Phase 6: NFSv4 Protocol Foundation** - Compound operations and pseudo-filesystem
- [x] **Phase 7: NFSv4 File Operations** - Lookup, read, write, create, remove
- [x] **Phase 7.5: Manual Verification - Basic NFSv4** USER CHECKPOINT
- [x] **Phase 8: NFSv4 Advanced Operations** - Link, rename, verify, security info
- [x] **Phase 9: State Management** - Client ID, state ID, and lease tracking
- [x] **Phase 10: NFSv4 Locking** - Integrated byte-range locking (LOCK/LOCKT/LOCKU)
- [x] **Phase 11: Delegations** - Read/write delegations with callback channel
- [x] **Phase 12: Kerberos Authentication** - RPCSEC_GSS framework with krb5/krb5i/krb5p
- [x] **Phase 12.5: Manual Verification - Kerberos** USER CHECKPOINT
- [x] **Phase 13: NFSv4 ACLs** - Extended ACL model with Windows interoperability
- [x] **Phase 14: Control Plane v2.0** - NFSv4 adapter configuration and settings
- [x] **Phase 15: v2.0 Testing** - Comprehensive E2E tests for NFSv4.0
- [x] **Phase 15.5: Manual Verification v2.0** USER CHECKPOINT

</details>

<details>
<summary>[x] v3.0 NFSv4.1 Sessions (Phases 16-25) - SHIPPED 2026-02-25</summary>

- [x] **Phase 16: NFSv4.1 Types and Constants** - Operation numbers, error codes, XDR structures for all v4.1 wire types (completed 2026-02-20)
- [x] **Phase 17: Slot Table and Session Data Structures** - SlotTable, SessionRecord, ChannelAttrs, EOS replay cache with per-table locking (completed 2026-02-20)
- [x] **Phase 18: EXCHANGE_ID and Client Registration** - v4.1 client identity establishment with owner/implementation tracking (completed 2026-02-20)
- [x] **Phase 19: Session Lifecycle** - CREATE_SESSION, DESTROY_SESSION with slot table allocation and channel negotiation (completed 2026-02-21)
- [x] **Phase 20: SEQUENCE and COMPOUND Bifurcation** - v4.1 request processing with EOS enforcement and v4.0/v4.1 coexistence (completed 2026-02-21)
- [x] **Phase 20.5: Manual Verification - Sessions** USER CHECKPOINT
- [x] **Phase 21: Connection Management and Trunking** - BIND_CONN_TO_SESSION, multi-connection sessions, server_owner consistency (completed 2026-02-21)
- [x] **Phase 22: Backchannel Multiplexing** - CB_SEQUENCE over fore-channel, bidirectional I/O, NAT-friendly callbacks (completed 2026-02-21)
- [x] **Phase 23: Client Lifecycle and Cleanup** - DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID, RECLAIM_COMPLETE, v4.0-only rejections (completed 2026-02-22)
- [x] **Phase 24: Directory Delegations** - GET_DIR_DELEGATION, CB_NOTIFY, delegation state tracking with recall (completed 2026-02-22)
- [x] **Phase 25: v3.0 Integration Testing** - E2E tests for sessions, EOS, backchannel, directory delegations, and coexistence (completed 2026-02-23)
- [x] **Phase 25.5: Manual Verification v3.0** USER CHECKPOINT

</details>

<details>
<summary>[x] v3.5 Adapter + Core Refactoring (Phases 26-29.4) - SHIPPED 2026-02-26</summary>

- [x] **Phase 26: Generic Lock Interface & Protocol Leak Purge** - Unify lock model (OpLock/AccessMode/UnifiedLock), purge NFS/SMB types from generic layers (completed 2026-02-25)
- [x] **Phase 27: NFS Adapter Restructuring** - Rename internal/protocol/ to internal/adapter/, consolidate NFS ecosystem, split v4/v4.1 (completed 2026-02-25)
- [x] **Phase 28: SMB Adapter Restructuring** - Extract BaseAdapter, move framing/signing/dispatch to internal/, Authenticator interface (completed 2026-02-25)
- [x] **Phase 29: Core Layer Decomposition** - Store interface split, Runtime decomposition, Offloader rename/split, error unification (completed 2026-02-26)
- [x] **Phase 29.4: Verification Artifacts & Requirements Cleanup** INSERTED - Formal verification for Phases 28/29, REQUIREMENTS.md traceability update (completed 2026-02-26)
- [x] **Phase 29.5: Manual Verification - Refactoring** USER CHECKPOINT

</details>

<details>
<summary>[x] v3.6 Windows Compatibility (Phases 29.8-32) - SHIPPED 2026-02-28</summary>

- [x] **Phase 29.8: Microsoft Protocol Test Suite CI Integration** INSERTED - Dockerized WPTS FileServer harness (completed 2026-02-26)
- [x] **Phase 30: SMB Bug Fixes** - Sparse READ, renamed dir listing, parent navigation, oplock breaks, link count, pipe caching (completed 2026-02-27)
- [x] **Phase 31: Windows ACL Support** - NT Security Descriptors, SID mapping, icacls support (completed 2026-02-27)
- [x] **Phase 32: Windows Integration Testing** - smbtorture, Windows 11 validation, Windows CI (completed 2026-02-28)
- [x] **Phase 32.5: Manual Verification - Windows** USER CHECKPOINT (UAT 10/10 passed)

</details>

<details>
<summary>[x] v3.8 SMB3 Protocol Upgrade (Phases 33-40.5) - SHIPPED 2026-03-04</summary>

- [x] **Phase 33: SMB3 Dialect Negotiation and Preauth Integrity** - 3.0/3.0.2/3.1.1 dialect selection, negotiate contexts, SHA-512 preauth hash chain, secure dialect validation IOCTL (completed 2026-02-28)
- [x] **Phase 34: Key Derivation and Signing** - SP800-108 KDF, dialect-aware key derivation (3.0 vs 3.1.1), AES-CMAC/GMAC signing abstraction (completed 2026-03-01)
- [x] **Phase 35: Encryption and Transform Header** - AES-128/256-CCM/GCM encryption, transform header framing, per-session and per-share encryption enforcement (completed 2026-03-01)
- [x] **Phase 36: Kerberos SMB3 Integration** - SPNEGO/Kerberos session setup with session key extraction, AP-REP mutual auth, NTLM fallback, guest sessions (completed 2026-03-01)
- [x] **Phase 37: SMB3 Leases and Directory Leasing** - Lease V2 with ParentLeaseKey/epoch, directory leases, break coordination via metadata service (completed 2026-03-02)
- [x] **Phase 38: Durable Handles** - V1/V2 durable handles with CreateGuid, state persistence, reconnect validation (14+ checks), timeout management (completed 2026-03-02)
- [x] **Phase 39: Cross-Protocol Integration and Documentation** - Bidirectional SMB3 lease/NFS delegation coordination, directory lease breaks on NFS ops, documentation (completed 2026-03-02)
- [x] **Phase 40: SMB3 Conformance Testing** - smbtorture SMB3 suites, WPTS FileServer BVT, Go integration tests, client compatibility matrix
- [x] **Phase 40.5: Manual Verification - SMB3** USER CHECKPOINT (Windows 11 verified 2026-03-04)

</details>

<details>
<summary>[x] v4.0 BlockStore Unification Refactor (Phases 41-49) - SHIPPED 2026-03-11</summary>

- [x] **Phase 41: Block State Enum and ListFileBlocks** - Rename states, query methods, conformance tests (completed 2026-03-09)
- [x] **Phase 42: Legacy Cleanup** - Remove DirectWriteStore and fs payload store (completed 2026-03-09)
- [x] **Phase 43: Local-Only Block Management** - Block lifecycle methods, local-only offloader mode (completed 2026-03-09)
- [x] **Phase 44: Data Model and API/CLI** - BlockStoreConfig model, REST API, CLI commands (completed 2026-03-09)
- [x] **Phase 45: Package Restructure** - pkg/blockstore/ hierarchy absorbing cache, payload, offloader, gc (completed 2026-03-09)
- [x] **Phase 46: Per-Share Block Store Wiring** - Per-share BlockStore instances replacing global PayloadService (completed 2026-03-10)
- [x] **Phase 47: L1 Read Cache and Prefetch** - LRU read cache with adaptive sequential prefetcher (completed 2026-03-10)
- [x] **Phase 48: Auto-Deduced Configuration** - Platform-aware sysinfo, auto-deduced defaults (completed 2026-03-10)
- [x] **Phase 49: Testing and Documentation** - E2E store matrix, cache CLI/API, documentation sweep (completed 2026-03-11)

</details>

<details>
<summary>[x] v4.3 Protocol Gap Fixes (Phases 49.1-49.3) -- SHIPPED 2026-03-13</summary>

Full phase details archived to [milestones/v4.3-ROADMAP.md](milestones/v4.3-ROADMAP.md).

3 phases, 1 plan: NFSv4 READDIR cookie verifier, READDIRPLUS performance (pre-existing), LSA named pipe (pre-existing).

</details>

<details>
<summary>[x] v4.7 Offline/Edge Resilience (Phases 63-68) -- SHIPPED 2026-03-20</summary>

- [x] **Phase 63: Cache Retention Model and Eviction Policy** - Per-share cache retention config (pin/ttl/lru), control plane API/CLI, eviction policy enforcement (completed 2026-03-13)
- [x] **Phase 64: S3 Health Check and Syncer Resilience** - Periodic connectivity detection, syncer backoff during outages, auto-resume and ordered drain on reconnect (completed 2026-03-16)
- [x] **Phase 65: Offline Read/Write Paths** - Graceful degradation serving cached blocks offline, local write acceptance when S3 unreachable (completed 2026-03-16)
- [x] **Phase 66: Edge Test Infrastructure** - Pulumi Scaleway deployment, persistence verification, offline simulation via iptables, auto-sync validation (delivered via PR #286)
- [x] **Phase 68: Protocol Correctness and Hot-Reload** - NTLM flag cleanup and share hot-reload integration tests (completed 2026-03-20)

</details>

### v0.10.0 Production Hardening + SMB Protocol Fixes

- [ ] **Phase 69: SMB Protocol Foundation** - macOS signing fix, credit charge validation, credit granting, multi-credit I/O, compound credit accounting
- [ ] **Phase 70: Storage Observability and Quotas** - Per-share quotas with FSSTAT/SMB reporting, accurate UsedSize, logical vs physical size distinction
- [ ] **Phase 71: Operational Visibility** - Protocol-agnostic client tracking with REST API and CLI
- [ ] **Phase 72: WPTS Conformance Push** - ChangeNotify implementation, negotiate/encryption fixes, leasing edge cases, known failure reduction
- [ ] **Phase 73: Trash and Soft-Delete** - Per-share trash with configurable retention, background scavenger, GC-aware, quota-aware
- [ ] **Phase 74: SMB Multi-Channel** - Session binding, per-channel signing, lease break fan-out, connection cleanup, config flag
- [ ] **Phase 75: Manual Verification v0.10.0** USER CHECKPOINT

### v4.5 BlockStore Security

- [ ] **Phase 49.4: Block-Level Compression** - LZ4/Zstd compression decorator for remote stores (#185)
- [ ] **Phase 49.5: Client-Side Encryption** - AES-256-GCM encryption decorator for zero-trust storage (#186)

### v4.8 DX/UX Improvements

- [ ] **Phase 76: Build & CI Optimization** - Makefile targets for all test suites (#206), NFS CI scoped triggers + tiered matrix (#207)
- [ ] **Phase 77: Adapter Config API** - Netgroup-share association API for adapter configuration (#220)
- [ ] **Phase 78: Developer Tooling Polish** - Documentation improvements, developer workflow enhancements

### v5.0 NFSv4.2 Extensions

- [ ] **Phase 50: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 51: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 52: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 53: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 54: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 55: Documentation** - Complete documentation for all new features
- [ ] **Phase 56: v5.0 Testing** - Final testing and pjdfstest POSIX compliance

<details>
<summary>[x] v4.2 Benchmarking & Performance (Phases 57-62) - SHIPPED 2026-03-04</summary>

- [x] **Phase 57: Benchmark Infrastructure** - Docker Compose profiles and directory structure (completed 2026-02-27)
- [x] **Phase 58: Benchmark Workloads** - fio job files and metadata benchmark scripts (completed 2026-03-04)
- [x] **Phase 59: Competitor Setup** - Configuration for JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba (completed 2026-03-04)
- [x] **Phase 60: Orchestrator Scripts** - Main benchmark runner with platform variants (completed 2026-03-04)
- [x] **Phase 61: Analysis & Reporting** - Python pipeline for charts and markdown reports (completed 2026-03-04)
- [x] **Phase 62: Profiling Integration** - Prometheus, Pyroscope, pprof for bottleneck identification (completed 2026-03-04)

</details>

## Phase Details

---

<details>
<summary>v3.8 SMB3 Protocol Upgrade (Phases 33-40.5) -- SHIPPED 2026-03-04</summary>

Full phase details archived to [milestones/v3.8-ROADMAP.md](milestones/v3.8-ROADMAP.md).

8 phases, 26 plans: dialect negotiation, KDF/signing, encryption, Kerberos, leases, durable handles, cross-protocol integration, conformance testing.

</details>

---

<details>
<summary>v4.0 BlockStore Unification Refactor (Phases 41-49) -- SHIPPED 2026-03-11</summary>

Full phase details archived to [milestones/v4.0-ROADMAP.md](milestones/v4.0-ROADMAP.md).

9 phases, 24 plans: block state rename, legacy cleanup, local-only mode, data model/API/CLI, package restructure, per-share wiring, L1 read cache, auto-configuration, testing and documentation.

</details>

---

<details>
<summary>v4.3 Protocol Gap Fixes (Phases 49.1-49.3) -- SHIPPED 2026-03-13</summary>

Full phase details archived to [milestones/v4.3-ROADMAP.md](milestones/v4.3-ROADMAP.md).

3 phases, 1 plan: NFSv4 READDIR cookie verifier, READDIRPLUS performance (pre-existing), LSA named pipe (pre-existing).

</details>

---

<details>
<summary>v4.7 Offline/Edge Resilience (Phases 63-68) -- SHIPPED 2026-03-20</summary>

Full phase details archived to [milestones/v4.7-ROADMAP.md](milestones/v4.7-ROADMAP.md).

4 phases, 10 plans: cache retention policies (pin/ttl/lru), S3 health monitoring with circuit breaker, offline read/write paths, NTLM flag cleanup, share hot-reload tests.

</details>

---

## v0.10.0 Production Hardening + SMB Protocol Fixes

### Phase 69: SMB Protocol Foundation
**Goal**: macOS clients can mount DittoFS shares over SMB 3.1.1 with signing, and the server enforces MS-SMB2 credit flow control for all clients
**Depends on**: Phase 68 (v4.7 complete)
**Requirements**: SMB-01, SMB-02, SMB-03, SMB-04, SMB-05
**Success Criteria** (what must be TRUE):
  1. `mount_smbfs //user@host/share /mnt` succeeds on macOS without signature verification errors, and Windows 11 signing continues to work (no regression)
  2. Server rejects requests where CreditCharge is insufficient for the payload size (e.g., READ/WRITE > 64KB with CreditCharge=1 returns error)
  3. Every SMB response grants at least 1 credit; a client that sends valid requests never reaches zero credits and deadlocks
  4. Multi-credit I/O operations correctly validate CreditCharge = ceil(PayloadSize / 65536) before dispatching to handlers
  5. Compound requests account credits at the compound level and grant credits only in the final response of the compound
**Verification**: `go build ./...` && `go test ./...` && macOS mount_smbfs manual test && WPTS BVT regression check
**Plans**: 3 plans

Plans:
- [ ] 69-01-PLAN.md — macOS signing fix (PR #288 absorb) + full MS-SMB2 signing audit
- [ ] 69-02-PLAN.md — CommandSequenceWindow + credit charge validation helpers + minimum grant enforcement
- [ ] 69-03-PLAN.md — Credit validation wiring into request pipeline + compound credit accounting

### Phase 70: Storage Observability and Quotas
**Goal**: Operators can see accurate storage consumption per share, and per-share quotas enforce size limits reported consistently via NFS and SMB
**Depends on**: Phase 69
**Requirements**: QUOTA-01, QUOTA-02, QUOTA-03, QUOTA-04, QUOTA-05, STATS-01, STATS-02, STATS-03
**Success Criteria** (what must be TRUE):
  1. `BlockStore.Stats()` returns non-zero UsedSize reflecting actual aggregate block sizes, with logical size (sum of file sizes) and physical size (block storage consumption) distinguished
  2. `dfsctl share create --quota-bytes 10GB` and `dfsctl share update --quota-bytes 50GB` persist per-share quota limits in the control plane; `dfsctl share list` displays quota and usage
  3. Write operations to a share at quota return NFS3ERR_NOSPC (NFSv3) / NFS4ERR_NOSPC (NFSv4) / STATUS_DISK_FULL (SMB) and refuse additional data
  4. NFSv3 FSSTAT and NFSv4 GETATTR(space_total/space_free/space_avail) report quota-adjusted values so `df` shows quota as total and (quota - used) as available
  5. SMB FileFsSizeInformation and FileFsFullSizeInformation return quota-aware sizes so Windows Explorer shows correct free space matching the NFS view
**Verification**: `go build ./...` && `go test ./...` && quota enforcement test with NFS + SMB clients && `df` on both protocols matches
**Plans**: 3 plans

Plans:
- [ ] 70-01-PLAN.md — Data model foundation (QuotaBytes field, atomic usage counters, BlockStore.Stats() UsedSize)
- [ ] 70-02-PLAN.md — Quota enforcement and protocol reporting (PrepareWrite check, quota-aware FSSTAT, NFSv4 attrs, SMB cleanup)
- [ ] 70-03-PLAN.md — CLI/API quota management (--quota-bytes flags, share list columns, runtime wiring)

### Phase 71: Operational Visibility
**Goal**: Operators can see all connected NFS and SMB clients in a single unified view with connection metadata and automatic stale cleanup
**Depends on**: Phase 69
**Requirements**: CLIENT-01, CLIENT-02, CLIENT-03, CLIENT-04
**Success Criteria** (what must be TRUE):
  1. Protocol-agnostic `ClientRecord` model in the runtime tracks client IP, protocol (NFS/SMB), connected-at timestamp, last-activity timestamp, mount points/tree connects, and authentication identity
  2. NFS and SMB adapters register clients on connect and deregister on disconnect via a shared `ClientRegistry` interface in the runtime
  3. `dfsctl client list` displays active clients in table format with columns for IP, protocol, user, share, and connected duration; `-o json` outputs JSON
  4. REST API endpoint `GET /api/clients` returns the list of active client records with protocol, IP, shares, and auth identity
  5. Stale client records are automatically removed via configurable TTL-based cleanup (default 5 minutes after last activity)
**Verification**: `go build ./...` && `go test ./...` && mount via NFS and SMB, then `dfsctl client list` shows both
**Plans**: 2 plans
Plans:
- [x] 71-01-PLAN.md — ClientRecord model, Registry service, TTL sweeper, Runtime wiring
- [ ] 71-02-PLAN.md — NFS/SMB adapter integration, REST API, apiclient, CLI commands

### Phase 72: WPTS Conformance Push
**Goal**: WPTS known failure count reduced from 73 to approximately 40-45, primarily by implementing ChangeNotify and fixing negotiate/leasing edge cases
**Depends on**: Phase 69 (credit flow baseline stabilizes protocol compliance)
**Requirements**: WPTS-01, WPTS-02, WPTS-03, WPTS-04
**Success Criteria** (what must be TRUE):
  1. SMB2 CHANGE_NOTIFY requests are accepted, held as pending async requests, and dispatched with FILE_NOTIFY_INFORMATION when files are created, removed, renamed, or modified in a watched directory; ~20 BVT ChangeNotify tests pass
  2. Negotiate and encryption edge cases fixed (preauth hash improvements cascade from Phase 69 macOS fix); ~5 additional tests pass
  3. Leasing and durable handle reconnect edge cases resolved (lease break state transitions, epoch tracking, DH V2 reconnect corner cases); ~4-6 additional tests pass
  4. WPTS known failure count is at or below 45 (reduced from 73), with zero new failures introduced (193+ passing tests maintained)
**Verification**: `go build ./...` && `go test ./...` && full WPTS suite run showing pass/known/new/skipped counts
**Plans**: TBD

### Phase 73: Trash and Soft-Delete
**Goal**: Deleted files are recoverable via a per-share server-side trash system with automatic expiration, invisible to protocol clients, and integrated with quotas and block GC
**Depends on**: Phase 70 (trash must count against quota; quota enforcement infrastructure must exist)
**Requirements**: TRASH-01, TRASH-02, TRASH-03, TRASH-04, TRASH-05, TRASH-06, TRASH-07
**Success Criteria** (what must be TRUE):
  1. Per-share trash is configurable via `dfsctl share create --trash-retention 14d` and `dfsctl share update --trash-retention 0` (0 disables trash); default is disabled
  2. When trash is enabled, file and directory deletion moves items to a hidden `.dfs-trash/` directory within the share's metadata namespace instead of permanent removal
  3. `.dfs-trash/` directory and its contents are invisible in NFS READDIR/READDIRPLUS and SMB QueryDirectory responses (filtered at protocol handler level)
  4. Background scavenger goroutine purges expired trash items based on the configured retention period; `dfsctl trash list /share` shows trashed items with original path, deletion date, and size; `dfsctl trash restore` and `dfsctl trash purge` work correctly
  5. Trashed files count as live references for block GC (blocks are not collected while trash references them) and trashed file sizes count against share quota
**Verification**: `go build ./...` && `go test ./...` && delete file via NFS, verify hidden from listing, verify in trash via CLI, verify quota accounting, wait for expiry
**Plans**: TBD

### Phase 74: SMB Multi-Channel
**Goal**: SMB clients can establish multiple TCP connections to the same session for aggregate bandwidth and fault tolerance, gated behind a configuration flag
**Depends on**: Phase 69 (SMB protocol foundation), Phase 72 (WPTS conformance stabilized)
**Requirements**: MCH-01, MCH-02, MCH-03, MCH-04, MCH-05, MCH-06
**Success Criteria** (what must be TRUE):
  1. Server advertises SMB2_GLOBAL_CAP_MULTI_CHANNEL in NEGOTIATE response when multi-channel is enabled in configuration; FSCTL_QUERY_NETWORK_INTERFACE_INFO IOCTL returns available network interfaces
  2. SESSION_SETUP with SMB2_SESSION_FLAG_BINDING successfully binds a new TCP connection to an existing session, sharing tree connects, open files, and lease state
  3. Each channel derives its own signing key from the channel-specific preauth integrity hash, and signatures are validated per-channel
  4. Lease break notifications are sent to all active channels for a session; if one channel fails delivery, other channels still receive the break
  5. Connection cleanup checks session refcount before destroying session state (last channel close tears down session; earlier channel closes only remove that channel)
  6. Multi-channel is gated behind `adapters.smb.multichannel.enabled` config flag (default: false); when disabled, session binding requests are rejected
**Verification**: `go build ./...` && `go test ./...` && Windows multi-NIC client binds 2 channels && WPTS regression check
**Plans**: TBD

### Phase 75: Manual Verification v0.10.0
**Goal**: End-to-end manual verification of all v0.10.0 features across macOS, Windows, and Linux clients
**Depends on**: Phase 74
**Success Criteria** (what must be TRUE):
  1. macOS mount_smbfs with signing works without errors
  2. Share quota enforcement verified on both NFS (`df` shows quota) and SMB (Explorer shows correct free space)
  3. `dfsctl client list` shows active NFS and SMB clients with correct protocol, share, and user information
  4. WPTS suite confirms known failures at or below 45 with zero new failures
  5. Trash: delete via NFS, invisible in listing, visible via `dfsctl trash list`, restore works, expiry purges automatically
  6. Multi-channel (if enabled): Windows client binds second channel, I/O uses both connections
**Verification**: Manual testing checklist
**Plans**: TBD

---

## v4.5 BlockStore Security

### Phase 49.4: Block-Level Compression
**Goal**: Add transparent LZ4/Zstd compression before remote store upload
**Depends on**: Phase 49.3 (v4.3 complete)
**Reference**: GitHub #185
**Success Criteria** (what must be TRUE):
  1. CompressedBlockStore decorator wraps any RemoteStore implementation
  2. LZ4 and Zstd algorithms supported, configurable per share
  3. Block header identifies compression algorithm and original size
  4. Already-compressed data detected and skipped (incompressibility check)
  5. 30-70% storage reduction for compressible data (logs, text, JSON)
**Verification**: `go build ./...` && `go test ./...` && compression ratio benchmark
**Plans**: TBD

### Phase 49.5: Client-Side Encryption
**Goal**: Add AES-256-GCM encryption before remote store upload for zero-trust storage
**Depends on**: Phase 49.4
**Reference**: GitHub #186
**Success Criteria** (what must be TRUE):
  1. EncryptedBlockStore decorator wraps any RemoteStore implementation
  2. Hybrid RSA + AES-256-GCM: per-block random symmetric keys
  3. Block format: [header: version + algo + RSA(key) + nonce] [AES-GCM ciphertext]
  4. Key file configurable per share, passphrase via environment variable
  5. Encryption composable with compression (compress then encrypt)
**Verification**: `go build ./...` && `go test ./...` && encryption round-trip test
**Plans**: TBD

---

## v4.8 DX/UX Improvements

### Phase 76: Build & CI Optimization
**Goal**: Streamline build system with Makefile targets and optimize NFS CI pipeline
**Depends on**: Phase 75 (v0.10.0 complete)
**Requirements**: DX-01, DX-02
**Reference**: GitHub #206, #207
**Success Criteria** (what must be TRUE):
  1. Makefile with targets: `make test`, `make test-unit`, `make test-integration`, `make test-e2e`, `make lint`, `make build`, `make fmt`
  2. `make test-e2e` handles sudo and build tags automatically
  3. NFS CI workflow uses scoped path triggers (only runs on NFS-related changes)
  4. NFS CI uses tiered test matrix (fast unit tests first, slow E2E only on merge)
  5. CI runtime reduced by 30%+ compared to current configuration
**Verification**: `make test` && CI pipeline runs successfully
**Plans**: TBD

### Phase 77: Adapter Config API
**Goal**: API support for netgroup-share association and adapter configuration management
**Depends on**: Phase 76
**Requirements**: DX-03
**Reference**: GitHub #220
**Success Criteria** (what must be TRUE):
  1. REST API endpoints for managing netgroup-share associations
  2. `dfsctl adapter config` commands for viewing and modifying adapter settings
  3. Netgroup-share mappings persisted in control plane store
  4. NFS adapter respects netgroup-share associations for export access control
**Verification**: `go build ./...` && `go test ./...` && manual CLI verification
**Plans**: TBD

### Phase 78: Developer Tooling Polish
**Goal**: Improve developer experience with documentation and workflow enhancements
**Depends on**: Phase 76
**Requirements**: DX-04
**Success Criteria** (what must be TRUE):
  1. Contributing guide updated with Makefile-based workflow
  2. Development environment setup documented with prerequisites check script
  3. Common development tasks automated (lint-fix, generate, clean)
  4. README quickstart reflects simplified build/test workflow
**Verification**: Documentation review && clean checkout build test
**Plans**: TBD

---

## v5.0 NFSv4.2 Extensions

### Phase 50: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 49.5 (v4.5 complete)
**Requirements**: NFS42-01
**Success Criteria** (what must be TRUE):
  1. COPY operation copies data without client I/O
  2. Async COPY returns immediately with stateid for tracking
  3. OFFLOAD_STATUS reports copy progress
  4. OFFLOAD_CANCEL terminates in-progress copy
  5. Large file copy completes efficiently via block store
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 51: Clone/Reflinks
**Goal**: Implement CLONE operation leveraging content-addressed storage
**Depends on**: Phase 50
**Requirements**: NFS42-02
**Success Criteria** (what must be TRUE):
  1. CLONE creates copy-on-write file instantly
  2. Cloned files share blocks until modification
  3. Modification triggers copy of affected blocks only
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 52: Sparse Files
**Goal**: Implement sparse file operations (SEEK, ALLOCATE, DEALLOCATE)
**Depends on**: Phase 50
**Requirements**: NFS42-03
**Success Criteria** (what must be TRUE):
  1. SEEK locates DATA or HOLE regions in file
  2. ALLOCATE pre-allocates file space
  3. DEALLOCATE punches holes in file
  4. Sparse file metadata correctly tracks allocated regions
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 53: Extended Attributes
**Goal**: Implement xattr storage and NFSv4.2/SMB exposure
**Depends on**: Phase 50
**Requirements**: NFS42-04
**Success Criteria** (what must be TRUE):
  1. GETXATTR retrieves extended attribute value
  2. SETXATTR stores extended attribute
  3. LISTXATTRS enumerates all xattr names
  4. REMOVEXATTR deletes extended attribute
  5. Xattrs accessible via both NFSv4.2 and SMB
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 54: NFSv4.2 Operations
**Goal**: Implement remaining NFSv4.2 operations
**Depends on**: Phase 52
**Requirements**: NFS42-05
**Success Criteria** (what must be TRUE):
  1. IO_ADVISE accepts application I/O hints
  2. LAYOUTERROR and LAYOUTSTATS available if pNFS enabled
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 55: Documentation
**Goal**: Complete documentation for all new features
**Depends on**: Phase 53
**Requirements**: (documentation)
**Success Criteria** (what must be TRUE):
  1. docs/NFS.md updated with NFSv4.1 and NFSv4.2 details
  2. docs/CONFIGURATION.md covers all new session and v4.2 options
  3. docs/SECURITY.md describes Kerberos security model for NFS and SMB
**Verification**: Documentation review
**Plans**: TBD

### Phase 56: v5.0 Testing
**Goal**: Final testing including pjdfstest POSIX compliance
**Depends on**: Phase 50, Phase 51, Phase 52, Phase 53, Phase 54, Phase 55
**Requirements**: (testing)
**Success Criteria** (what must be TRUE):
  1. Server-side copy E2E tests pass for various file sizes
  2. Clone/reflinks E2E tests verify block sharing
  3. Sparse file E2E tests verify hole handling
  4. Xattr E2E tests verify cross-protocol access
  5. pjdfstest POSIX compliance passes for NFSv3 and NFSv4
  6. Performance benchmarks establish baseline
**Verification**: `go test -tags=e2e ./test/e2e/...` && pjdfstest
**Plans**: TBD

---

<details>
<summary>[x] v4.2 Benchmarking & Performance (Phases 57-62) -- SHIPPED 2026-03-04</summary>

Full phase details archived. 6 phases: benchmark infrastructure, fio workloads, competitor setup, orchestrator scripts, analysis pipeline, profiling integration.

</details>

---

## Progress

**Execution Order:**
v3.8 (33-40.5) -> v4.2 (57-62) -> v4.0 (41-49) -> v4.3 (49.1-49.3) -> v4.7 (63-68) -> v0.10.0 (69-75) -> v4.5 (49.4-49.5) -> v4.8 (76-78) -> v5.0 (50-56)

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1-5.5 | v1.0 | Complete | Complete | 2026-02-07 |
| 6-15.5 | v2.0 | Complete | Complete | 2026-02-20 |
| 16-25.5 | v3.0 | Complete | Complete | 2026-02-25 |
| 26-29.5 | v3.5 | Complete | Complete | 2026-02-26 |
| 29.8-32.5 | v3.6 | Complete | Complete | 2026-02-28 |
| 33-40.5 | v3.8 | Complete | Complete | 2026-03-04 |
| 57-62 | v4.2 | Complete | Complete | 2026-03-04 |
| 41-49 | v4.0 | 24/24 | Complete | 2026-03-11 |
| 49.1-49.3 | v4.3 | 1/1 | Complete | 2026-03-13 |
| 63-68 | v4.7 | 10/10 | Complete | 2026-03-20 |
| 69. SMB Protocol Foundation | 3/3 | Complete   | 2026-03-20 | - |
| 70. Storage Observability and Quotas | 3/3 | Complete    | 2026-03-21 | - |
| 71. Operational Visibility | v0.10.0 | 1/2 | In Progress|  |
| 72. WPTS Conformance Push | v0.10.0 | 0/? | Not started | - |
| 73. Trash and Soft-Delete | v0.10.0 | 0/? | Not started | - |
| 74. SMB Multi-Channel | v0.10.0 | 0/? | Not started | - |
| 75. Manual Verification v0.10.0 | v0.10.0 | 0/? | Not started | - |
| 49.4 Block-Level Compression | v4.5 | 0/? | Not started | - |
| 49.5 Client-Side Encryption | v4.5 | 0/? | Not started | - |
| 76. Build & CI Optimization | v4.8 | 0/? | Not started | - |
| 77. Adapter Config API | v4.8 | 0/? | Not started | - |
| 78. Developer Tooling Polish | v4.8 | 0/? | Not started | - |
| 50-56 | v5.0 | 0/? | Not started | - |

**Total:** 148/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
*v3.5 shipped: 2026-02-26*
*v3.6 shipped: 2026-02-28*
*v3.8 shipped: 2026-03-04*
*v4.2 shipped: 2026-03-04*
*v4.0 shipped: 2026-03-11*
*v4.3 shipped: 2026-03-13*
*v4.7 roadmap created: 2026-03-13*
*v4.7 shipped: 2026-03-20*
*v0.10.0 roadmap created: 2026-03-20*
