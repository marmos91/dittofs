# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across eight milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, signing, leases, Kerberos, and durable handles, v4.0 completes the protocol suite with NFSv4.2 advanced features, and v4.1 establishes performance baselines via a comprehensive benchmarking suite and iterative optimization. Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [x] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (shipped 2026-02-26) — [archive](milestones/v3.5-ROADMAP.md)
- [x] **v3.6 Windows Compatibility** - Phases 29.8-32.5 (shipped 2026-02-28) — [archive](milestones/v3.6-ROADMAP.md)
- [ ] **v3.8 SMB3 Protocol Upgrade** - Phases 33-40.5 (in progress)
- [ ] **v4.0 NFSv4.2 Extensions** - Phases 41-47.5 (planned)
- [ ] **v4.1 Benchmarking & Performance** - Phases 48-53.5 (planned)

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

### v3.8 SMB3 Protocol Upgrade

- [x] **Phase 33: SMB3 Dialect Negotiation and Preauth Integrity** - 3.0/3.0.2/3.1.1 dialect selection, negotiate contexts, SHA-512 preauth hash chain, secure dialect validation IOCTL (completed 2026-02-28)
- [x] **Phase 34: Key Derivation and Signing** - SP800-108 KDF, dialect-aware key derivation (3.0 vs 3.1.1), AES-CMAC/GMAC signing abstraction (completed 2026-03-01)
- [ ] **Phase 35: Encryption and Transform Header** - AES-128/256-CCM/GCM encryption, transform header framing, per-session and per-share encryption enforcement
- [ ] **Phase 36: Kerberos SMB3 Integration** - SPNEGO/Kerberos session setup with session key extraction, AP-REP mutual auth, NTLM fallback, guest sessions
- [ ] **Phase 37: SMB3 Leases and Directory Leasing** - Lease V2 with ParentLeaseKey/epoch, directory leases, break coordination via metadata service
- [ ] **Phase 38: Durable Handles** - V1/V2 durable handles with CreateGuid, state persistence, reconnect validation (14+ checks), timeout management
- [ ] **Phase 39: Cross-Protocol Integration and Documentation** - Bidirectional SMB3 lease/NFS delegation coordination, directory lease breaks on NFS ops, documentation
- [ ] **Phase 40: SMB3 Conformance Testing** - smbtorture SMB3 suites, WPTS FileServer BVT, Go integration tests, client compatibility matrix
- [ ] **Phase 40.5: Manual Verification - SMB3** USER CHECKPOINT - Verify SMB3 with Windows 10/11, macOS, Linux clients

### v4.0 NFSv4.2 Extensions

- [ ] **Phase 41: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 42: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 43: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 43.5: Manual Verification - Advanced Ops** USER CHECKPOINT - Test copy/clone/sparse
- [ ] **Phase 44: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 45: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 46: Documentation** - Complete documentation for all new features
- [ ] **Phase 47: v4.0 Testing** - Final testing and pjdfstest POSIX compliance
- [ ] **Phase 47.5: Final Manual Verification** USER CHECKPOINT - Complete validation of all features

### v4.1 Benchmarking & Performance

- [ ] **Phase 48: Benchmark Infrastructure** - Docker Compose profiles and directory structure (carried from previous Phase 33, already COMPLETE)
- [ ] **Phase 49: Benchmark Workloads** - fio job files and metadata benchmark scripts
- [ ] **Phase 50: Competitor Setup** - Configuration for JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba
- [ ] **Phase 51: Orchestrator Scripts** - Main benchmark runner with platform variants
- [ ] **Phase 52: Analysis & Reporting** - Python pipeline for charts and markdown reports
- [ ] **Phase 53: Profiling Integration** - Prometheus, Pyroscope, pprof for bottleneck identification

## Phase Details

---

## v3.8 SMB3 Protocol Upgrade

### Phase 33: SMB3 Dialect Negotiation and Preauth Integrity
**Goal**: Windows 10/11 clients can connect using SMB 3.0/3.0.2/3.1.1 dialects with preauth integrity protection against downgrade attacks
**Depends on**: Phase 32.5 (v3.6 complete)
**Requirements**: NEG-01, NEG-02, NEG-03, NEG-04, SDIAL-01, ARCH-02
**Success Criteria** (what must be TRUE):
  1. Windows 10/11 client negotiates SMB 3.1.1 dialect (visible in `Get-SmbConnection` output)
  2. macOS client negotiates SMB 3.0.2 dialect successfully
  3. Preauth integrity SHA-512 hash chain computed over raw wire bytes and stored on Connection and Session
  4. FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL succeeds for SMB 3.0/3.0.2 clients, preventing silent downgrade
  5. SMB internal package contains only protocol encoding/decoding/framing with no business logic
**Plans**: 3 plans
Plans:
- [x] 33-01-PLAN.md — smbenc binary codec, negotiate context types, ConnectionCryptoState (completed 2026-02-28)
- [x] 33-02-PLAN.md — Negotiate handler 3.x refactor, dispatch hooks, preauth hash chain (completed 2026-02-28)
- [x] 33-03-PLAN.md — IOCTL dispatch table, VALIDATE_NEGOTIATE_INFO, handler migration to smbenc, ARCH-02 (completed 2026-02-28)

### Phase 34: Key Derivation and Signing
**Goal**: All SMB3 sessions derive correct cryptographic keys and sign messages using AES-CMAC/GMAC instead of HMAC-SHA256
**Depends on**: Phase 33
**Requirements**: KDF-01, KDF-02, KDF-03, SIGN-01, SIGN-02, SIGN-03
**Success Criteria** (what must be TRUE):
  1. SMB 3.0/3.0.2 sessions derive keys using SP800-108 KDF with constant label/context strings
  2. SMB 3.1.1 sessions derive keys using preauth integrity hash as KDF context
  3. All SMB 3.x signed messages use AES-128-CMAC (replacing HMAC-SHA256)
  4. SMB 3.1.1 clients can use AES-128-GMAC signing when negotiated via signing capabilities context
  5. Signing algorithm abstraction dispatches correctly by negotiated dialect (HMAC-SHA256 for 2.x, CMAC for 3.0+, GMAC for 3.1.1)
**Plans**: 2 plans
Plans:
- [ ] 34-01-PLAN.md — SP800-108 KDF package, Signer interface with HMAC/CMAC/GMAC implementations, test vectors
- [ ] 34-02-PLAN.md — SessionCryptoState, SIGNING_CAPABILITIES negotiate context, session setup KDF integration, framing migration

### Phase 35: Encryption and Transform Header
**Goal**: SMB3 traffic can be encrypted end-to-end with AES-CCM/GCM, enforced per-session or per-share
**Depends on**: Phase 34
**Requirements**: ENC-01, ENC-02, ENC-03, ENC-04, ENC-05, ENC-06
**Success Criteria** (what must be TRUE):
  1. Windows 10/11 client connects with AES-128-GCM encrypted traffic (verified via packet capture showing 0xFD transform headers)
  2. AES-128-CCM encryption works for SMB 3.0/3.0.2 compatibility
  3. AES-256-GCM and AES-256-CCM cipher variants functional
  4. Per-session encryption enforced (Session.EncryptData flag forces encryption on all traffic)
  5. Per-share encryption enforced (one share encrypted, another unencrypted on same connection)
**Plans**: TBD

### Phase 36: Kerberos SMB3 Integration
**Goal**: Domain-joined Windows clients authenticate via Kerberos/SPNEGO with proper SMB3 key derivation, with NTLM and guest fallback
**Depends on**: Phase 34
**Requirements**: AUTH-01, AUTH-02, AUTH-03, AUTH-04, KDF-04, ARCH-03
**Success Criteria** (what must be TRUE):
  1. Domain-joined Windows client authenticates via SPNEGO/Kerberos without password prompt
  2. Server extracts Kerberos session key from AP-REQ and derives SMB3 signing/encryption keys via KDF
  3. Mutual authentication completes (AP-REP token returned in SPNEGO accept-complete)
  4. Non-domain client falls back from Kerberos to NTLM within SPNEGO negotiation
  5. Guest sessions function without encryption or signing (no session key available)
**Plans**: TBD

### Phase 37: SMB3 Leases and Directory Leasing
**Goal**: SMB3 clients can cache file and directory data locally using Lease V2 with epoch tracking, with lease management in the metadata service layer
**Depends on**: Phase 33
**Requirements**: LEASE-01, LEASE-02, LEASE-03, LEASE-04, ARCH-01
**Success Criteria** (what must be TRUE):
  1. SMB3 client receives Lease V2 with ParentLeaseKey and epoch in CREATE responses
  2. Directory leases (Read-caching) granted for SMB 3.0+ clients opening directories
  3. Directory lease broken when another client creates, deletes, or renames a file within the directory
  4. All lease management logic lives in metadata service layer (not in SMB internal package)
  5. Lease epoch tracking prevents stale break acknowledgments
**Plans**: TBD

### Phase 38: Durable Handles
**Goal**: SMB3 clients survive brief network interruptions without losing open files, with handle state persisted for reconnection
**Depends on**: Phase 37
**Requirements**: DH-01, DH-02, DH-03, DH-04, DH-05
**Success Criteria** (what must be TRUE):
  1. Client with durable handle V1 reconnects after network interruption and resumes file operations
  2. Durable handle V2 with CreateGuid enables idempotent reconnection (same CreateGuid = same handle)
  3. Durable handle state persists in control plane store across client disconnects
  4. Server validates all 14+ reconnect conditions per MS-SMB2 spec (CreateGuid, lease key, security context, etc.)
  5. Durable handle management logic lives in metadata service layer, reusing NFSv4 state patterns
**Plans**: TBD

### Phase 39: Cross-Protocol Integration and Documentation
**Goal**: SMB3 leases and NFS delegations coordinate bidirectionally through the metadata service, with comprehensive documentation
**Depends on**: Phase 37, Phase 38
**Requirements**: XPROT-01, XPROT-02, XPROT-03, DOC-01
**Success Criteria** (what must be TRUE):
  1. SMB3 file write triggers NFS delegation recall on the same file
  2. NFS file open triggers SMB3 lease break on the same file
  3. NFS directory operations (create/delete/rename) trigger SMB3 directory lease breaks
  4. All cross-protocol coordination logic lives in metadata service (shared abstract layer)
  5. docs/ updated with SMB3 protocol documentation covering configuration, capabilities, security, and cross-protocol behavior
**Plans**: TBD

### Phase 40: SMB3 Conformance Testing
**Goal**: SMB3 implementation validated against industry conformance suites and real clients across Windows, macOS, and Linux
**Depends on**: Phase 33, Phase 34, Phase 35, Phase 36, Phase 37, Phase 38, Phase 39
**Requirements**: TEST-01, TEST-02, TEST-03, TEST-04, TEST-05, TEST-06
**Success Criteria** (what must be TRUE):
  1. smbtorture SMB3 tests pass (durable_v2, lease, replay, session, encryption suites)
  2. Microsoft WPTS FileServer SMB3 BVT tests pass
  3. Go integration tests (go-smb2) validate native client-server SMB3 interop
  4. Cross-protocol integration tests validate SMB3 leases vs NFS delegations under concurrent load
  5. Windows 10/11 (SMB 3.1.1), macOS (SMB 3.0.2), and Linux cifs.ko (SMB 3.1.1) all connect and operate correctly
  6. E2E tests cover encryption, signing, leases, Kerberos, and durable handle scenarios end-to-end
**Plans**: TBD

---

## v4.0 NFSv4.2 Extensions

### Phase 41: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 40 (v3.8 complete)
**Requirements**: V42-01
**Success Criteria** (what must be TRUE):
  1. COPY operation copies data without client I/O
  2. Async COPY returns immediately with stateid for tracking
  3. OFFLOAD_STATUS reports copy progress
  4. OFFLOAD_CANCEL terminates in-progress copy
  5. Large file copy completes efficiently via block store
**Plans**: TBD

### Phase 42: Clone/Reflinks
**Goal**: Implement CLONE operation leveraging content-addressed storage
**Depends on**: Phase 41
**Requirements**: V42-02
**Success Criteria** (what must be TRUE):
  1. CLONE creates copy-on-write file instantly
  2. Cloned files share blocks until modification
  3. Modification triggers copy of affected blocks only
**Plans**: TBD

### Phase 43: Sparse Files
**Goal**: Implement sparse file operations (SEEK, ALLOCATE, DEALLOCATE)
**Depends on**: Phase 41
**Requirements**: V42-03
**Success Criteria** (what must be TRUE):
  1. SEEK locates DATA or HOLE regions in file
  2. ALLOCATE pre-allocates file space
  3. DEALLOCATE punches holes in file
  4. Sparse file metadata correctly tracks allocated regions
**Plans**: TBD

### Phase 44: Extended Attributes
**Goal**: Implement xattr storage and NFSv4.2/SMB exposure
**Depends on**: Phase 41
**Requirements**: V42-04
**Success Criteria** (what must be TRUE):
  1. GETXATTR retrieves extended attribute value
  2. SETXATTR stores extended attribute
  3. LISTXATTRS enumerates all xattr names
  4. REMOVEXATTR deletes extended attribute
  5. Xattrs accessible via both NFSv4.2 and SMB
**Plans**: TBD

### Phase 45: NFSv4.2 Operations
**Goal**: Implement remaining NFSv4.2 operations
**Depends on**: Phase 43
**Requirements**: V42-05
**Success Criteria** (what must be TRUE):
  1. IO_ADVISE accepts application I/O hints
  2. LAYOUTERROR and LAYOUTSTATS available if pNFS enabled
**Plans**: TBD

### Phase 46: Documentation
**Goal**: Complete documentation for all new features
**Depends on**: Phase 44
**Requirements**: (documentation)
**Success Criteria** (what must be TRUE):
  1. docs/NFS.md updated with NFSv4.1 and NFSv4.2 details
  2. docs/CONFIGURATION.md covers all new session and v4.2 options
  3. docs/SECURITY.md describes Kerberos security model for NFS and SMB
**Plans**: TBD

### Phase 47: v4.0 Testing
**Goal**: Final testing including pjdfstest POSIX compliance
**Depends on**: Phase 41, Phase 42, Phase 43, Phase 44, Phase 45, Phase 46
**Requirements**: V42-06
**Success Criteria** (what must be TRUE):
  1. Server-side copy E2E tests pass for various file sizes
  2. Clone/reflinks E2E tests verify block sharing
  3. Sparse file E2E tests verify hole handling
  4. Xattr E2E tests verify cross-protocol access
  5. pjdfstest POSIX compliance passes for NFSv3 and NFSv4
  6. Performance benchmarks establish baseline
**Plans**: TBD

---

## v4.1 Benchmarking & Performance

### Phase 48: Benchmark Infrastructure
**Goal**: Create bench/ directory structure with Docker Compose profiles and configuration files
**Depends on**: Phase 47 (v4.0 complete)
**Requirements**: BENCH-01
**Reference**: GitHub #194
**Status**: COMPLETE (merged 2026-02-27, PR #224 — completed as previous Phase 33)
**Success Criteria** (what must be TRUE):
  1. `bench/` directory structure created (configs/, workloads/, scripts/, analysis/, results/)
  2. `docker-compose.yml` with profiles: dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs, juicefs, ganesha, rclone, kernel-nfs, samba, dittofs-smb, monitoring
  3. `.env.example` with S3, PostgreSQL, and benchmark configuration variables
  4. DittoFS config files for each backend combination (badger+s3, postgres+s3, badger+fs)
  5. `scripts/check-prerequisites.sh` validates fio, nfs-common, cifs-utils, python3, docker, jq, bc
  6. Only one profile active at a time (no resource contention)
  7. `results/` directory gitignored
**Plans**: 2/2 (COMPLETED)
Plans:
- [x] 48-01-PLAN.md — Docker Compose infrastructure, directory structure, DittoFS configs
- [x] 48-02-PLAN.md — Prerequisites check, cleanup scripts, shared library, Makefile

### Phase 49: Benchmark Workloads
**Goal**: Create fio job files for all I/O workloads and a custom metadata benchmark script
**Depends on**: Phase 48
**Requirements**: BENCH-02
**Reference**: GitHub #195
**Success Criteria** (what must be TRUE):
  1. fio job files: seq-read-large (1MB), seq-write-large (1MB), rand-read-4k, rand-write-4k, mixed-rw-70-30, large-file-1gb
  2. Common parameters: runtime=60, time_based=1, output-format=json+, parameterized threads/mountpoint
  3. macOS variants with posixaio engine and direct=0
  4. `scripts/metadata-bench.sh` measuring create/stat/readdir/delete ops for 1K/10K files
  5. Deep tree benchmark (depth=5, fan=10) with create and walk
  6. Metadata script outputs JSON with ops/sec and total time
**Plans**: TBD

### Phase 50: Competitor Setup
**Goal**: Create configuration files and setup scripts for each competitor system
**Depends on**: Phase 48
**Requirements**: BENCH-03
**Reference**: GitHub #198
**Success Criteria** (what must be TRUE):
  1. JuiceFS config: format + mount script using same PostgreSQL + S3 as DittoFS, cache-size matched
  2. NFS-Ganesha config: FSAL_VFS export configuration (VFS backend, local FS comparison)
  3. RClone config: S3 remote with `serve nfs`, vfs-cache-max-size matched to DittoFS
  4. Kernel NFS config: exports file + erichough/nfs-server image (gold standard baseline)
  5. Samba config: smb.conf for SMB benchmarking (VFS backend)
  6. DittoFS setup script: automated store/share/adapter creation via dfsctl
  7. Fairness ensured: matched cache sizes, same S3 endpoints, symmetric Docker overhead
**Plans**: TBD

### Phase 51: Orchestrator Scripts
**Goal**: Create main benchmark orchestrator and all helper scripts with platform variants
**Depends on**: Phase 49, Phase 50
**Requirements**: BENCH-04
**Reference**: GitHub #196
**Success Criteria** (what must be TRUE):
  1. `run-bench.sh` orchestrator with --systems, --tiers, --iterations, --threads, --output, --with-monitoring, --with-profiling, --quick flags
  2. Helper scripts: setup-systems.sh, start-system.sh, stop-system.sh, mount-nfs.sh, mount-smb.sh, umount-all.sh, drop-caches.sh, warmup.sh, collect-metrics.sh
  3. Between-test cleanup: sync, drop caches, 5s cooldown, volume prune between system switches
  4. `run-bench-macos.sh` variant with posixaio, purge, resvport
  5. `run-bench-smb.sh` for Linux SMB testing (mount -t cifs)
  6. `run-bench-smb.ps1` for Windows SMB testing (PowerShell + diskspd)
  7. Health check wait before benchmark start
**Plans**: TBD

### Phase 52: Analysis & Reporting
**Goal**: Create Python analysis pipeline for parsing results, generating charts, and producing reports
**Depends on**: Phase 49
**Requirements**: BENCH-05
**Reference**: GitHub #197
**Success Criteria** (what must be TRUE):
  1. `parse_fio.py` extracts throughput (MB/s), IOPS, latency (p50/p95/p99/p99.9) with mean/stddev
  2. `parse_metadata.py` extracts create/stat/readdir/delete ops/sec across iterations
  3. `generate_charts.py` produces charts: tier1 throughput/IOPS/latency, tier2 userspace comparison, tier3 metadata, tier4 scaling, SMB comparison
  4. `generate_report.py` with Jinja2 template producing markdown report with environment details, summary tables, per-tier details, methodology section
  5. `requirements.txt` with pandas, matplotlib, seaborn, jinja2
  6. Results organized in `results/YYYY-MM-DD_HHMMSS/` with raw/, metrics/, charts/, report.md, summary.csv
**Plans**: TBD

### Phase 53: Profiling Integration
**Goal**: Integrate DittoFS observability stack for performance bottleneck identification
**Depends on**: Phase 51
**Requirements**: BENCH-06
**Reference**: GitHub #199
**Success Criteria** (what must be TRUE):
  1. DittoFS config with metrics + telemetry + profiling enabled when --with-profiling passed
  2. Monitoring stack: Prometheus (1s scrape), Pyroscope (continuous CPU + memory), Grafana (optional)
  3. `collect-metrics.sh` captures Prometheus range queries, pprof CPU/heap/mutex/goroutine profiles
  4. Analysis identifies bottlenecks: CPU flame graphs, S3 vs metadata latency, GC pauses, mutex contention, cache effectiveness
  5. Benchmark-specific Grafana dashboard for before/during/after metrics
  6. Results in `results/YYYY-MM-DD/metrics/` with prometheus/, pprof/, summary.json
**Plans**: TBD

---

## Progress

**Execution Order:**
v3.8 (33-40.5) -> v4.0 (41-47.5) -> v4.1 (48-53.5)

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Locking Infrastructure | v1.0 | 4/4 | Complete | 2026-02-04 |
| 2. NLM Protocol | v1.0 | 3/3 | Complete | 2026-02-05 |
| 3. NSM Protocol | v1.0 | 3/3 | Complete | 2026-02-05 |
| 4. SMB Leases | v1.0 | 3/3 | Complete | 2026-02-05 |
| 5. Cross-Protocol Integration | v1.0 | 6/6 | Complete | 2026-02-12 |
| 6. NFSv4 Protocol Foundation | v2.0 | 3/3 | Complete | 2026-02-13 |
| 7. NFSv4 File Operations | v2.0 | 3/3 | Complete | 2026-02-13 |
| 8. NFSv4 Advanced Operations | v2.0 | 3/3 | Complete | 2026-02-13 |
| 9. State Management | v2.0 | 4/4 | Complete | 2026-02-14 |
| 10. NFSv4 Locking | v2.0 | 3/3 | Complete | 2026-02-14 |
| 11. Delegations | v2.0 | 4/4 | Complete | 2026-02-14 |
| 12. Kerberos Authentication | v2.0 | 5/5 | Complete | 2026-02-15 |
| 13. NFSv4 ACLs | v2.0 | 5/5 | Complete | 2026-02-16 |
| 14. Control Plane v2.0 | v2.0 | 7/7 | Complete | 2026-02-16 |
| 15. v2.0 Testing | v2.0 | 5/5 | Complete | 2026-02-18 |
| 15.5. Manual Verification v2.0 | v2.0 | - | Complete | 2026-02-19 |
| 16. NFSv4.1 Types and Constants | v3.0 | 5/5 | Complete | 2026-02-20 |
| 17. Slot Table and Session Data Structures | v3.0 | 2/2 | Complete | 2026-02-20 |
| 18. EXCHANGE_ID and Client Registration | v3.0 | 2/2 | Complete | 2026-02-20 |
| 19. Session Lifecycle | v3.0 | 1/1 | Complete | 2026-02-21 |
| 20. SEQUENCE and COMPOUND Bifurcation | v3.0 | 2/2 | Complete | 2026-02-21 |
| 21. Connection Management and Trunking | v3.0 | 2/2 | Complete | 2026-02-21 |
| 22. Backchannel Multiplexing | v3.0 | 2/2 | Complete | 2026-02-21 |
| 23. Client Lifecycle and Cleanup | v3.0 | 3/3 | Complete | 2026-02-22 |
| 24. Directory Delegations | v3.0 | 3/3 | Complete | 2026-02-22 |
| 25. v3.0 Integration Testing | v3.0 | 3/3 | Complete | 2026-02-23 |
| 25.5. Manual Verification v3.0 | v3.0 | - | Complete | 2026-02-25 |
| 26. Generic Lock Interface & Protocol Leak Purge | v3.5 | 5/5 | Complete | 2026-02-25 |
| 27. NFS Adapter Restructuring | v3.5 | 4/4 | Complete | 2026-02-25 |
| 28. SMB Adapter Restructuring | v3.5 | 5/5 | Complete | 2026-02-25 |
| 29. Core Layer Decomposition | v3.5 | 7/7 | Complete | 2026-02-26 |
| 29.4 Verification & Requirements Cleanup | v3.5 | 1/1 | Complete | 2026-02-26 |
| 29.8. Microsoft Protocol Test Suite CI | v3.6 | 2/2 | Complete | 2026-02-26 |
| 30. SMB Bug Fixes | v3.6 | 4/4 | Complete | 2026-02-27 |
| 31. Windows ACL Support | v3.6 | 3/3 | Complete | 2026-02-27 |
| 32. Windows Integration Testing | v3.6 | 3/3 | Complete | 2026-02-28 |
| 33. SMB3 Dialect Negotiation and Preauth Integrity | 2/3 | Complete    | 2026-02-28 | - |
| 34. Key Derivation and Signing | 2/2 | Complete    | 2026-03-01 | - |
| 35. Encryption and Transform Header | v3.8 | 0/? | Not started | - |
| 36. Kerberos SMB3 Integration | v3.8 | 0/? | Not started | - |
| 37. SMB3 Leases and Directory Leasing | v3.8 | 0/? | Not started | - |
| 38. Durable Handles | v3.8 | 0/? | Not started | - |
| 39. Cross-Protocol Integration and Documentation | v3.8 | 0/? | Not started | - |
| 40. SMB3 Conformance Testing | v3.8 | 0/? | Not started | - |
| 41. Server-Side Copy | v4.0 | 0/? | Not started | - |
| 42. Clone/Reflinks | v4.0 | 0/? | Not started | - |
| 43. Sparse Files | v4.0 | 0/? | Not started | - |
| 44. Extended Attributes | v4.0 | 0/? | Not started | - |
| 45. NFSv4.2 Operations | v4.0 | 0/? | Not started | - |
| 46. Documentation | v4.0 | 0/? | Not started | - |
| 47. v4.0 Testing | v4.0 | 0/? | Not started | - |
| 48. Benchmark Infrastructure | v4.1 | 2/2 | Complete | 2026-02-27 |
| 49. Benchmark Workloads | v4.1 | 0/? | Not started | - |
| 50. Competitor Setup | v4.1 | 0/? | Not started | - |
| 51. Orchestrator Scripts | v4.1 | 0/? | Not started | - |
| 52. Analysis & Reporting | v4.1 | 0/? | Not started | - |
| 53. Profiling Integration | v4.1 | 0/? | Not started | - |

**Total:** 124/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
*v3.5 shipped: 2026-02-26*
*v3.6 shipped: 2026-02-28*
*v3.8 roadmap created: 2026-02-28*
