# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across eight milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.7 establishes performance baselines via a comprehensive benchmarking suite comparing DittoFS against competitors, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, leases, Kerberos, and durable handles, and v4.0 completes the protocol suite with NFSv4.2 advanced features. Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [ ] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (planned)
- [ ] **v3.6 Windows Compatibility** - Phases 30-32.5 (planned)
- [ ] **v3.7 Benchmarking Suite** - Phases 33-38.5 (planned)
- [ ] **v3.8 SMB3 Protocol Upgrade** - Phases 39-44.5 (planned)
- [ ] **v4.0 NFSv4.2 Extensions** - Phases 45-51.5 (planned)

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

### v3.5 Adapter + Core Refactoring

- [x] **Phase 26: Generic Lock Interface & Protocol Leak Purge** - Unify lock model (OpLock/AccessMode/UnifiedLock), purge NFS/SMB types from generic layers (completed 2026-02-25)
- [ ] **Phase 27: NFS Adapter Restructuring** - Rename internal/protocol/ to internal/adapter/, consolidate NFS ecosystem, split v4/v4.1
- [ ] **Phase 28: SMB Adapter Restructuring** - Extract BaseAdapter, move framing/signing/dispatch to internal/, Authenticator interface
- [ ] **Phase 29: Core Layer Decomposition** - Store interface split, Runtime decomposition, Offloader rename/split, error unification
- [ ] **Phase 29.5: Manual Verification - Refactoring** USER CHECKPOINT - Verify NFS + SMB functionality preserved

### v3.6 Windows Compatibility

- [ ] **Phase 30: SMB Bug Fixes** - Fix sparse file READ (#180), renamed directory listing (#181)
- [ ] **Phase 31: Windows ACL Support** - NT Security Descriptors, Unix-to-SID mapping, icacls support (#182)
- [ ] **Phase 32: Windows Integration Testing** - smbtorture + Microsoft Protocol Test Suite + manual Windows 11 validation
- [ ] **Phase 32.5: Manual Verification - Windows** USER CHECKPOINT - Full Windows validation

### v3.7 Benchmarking Suite

- [ ] **Phase 33: Benchmark Infrastructure** - Docker Compose profiles, directory structure, DittoFS configs, prerequisites check (#194)
- [ ] **Phase 34: Benchmark Workloads** - fio job files (seq/random/mixed I/O) and metadata benchmark script (#195)
- [ ] **Phase 35: Competitor Setup** - Configuration and setup scripts for JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba (#198)
- [ ] **Phase 36: Orchestrator Scripts** - run-bench.sh main orchestrator, helper scripts, platform variants (macOS, SMB, Windows) (#196)
- [ ] **Phase 37: Analysis & Reporting** - Python scripts for parsing fio/metadata results, chart generation, markdown report (#197)
- [ ] **Phase 38: Profiling Integration** - Prometheus/Pyroscope/pprof capture during benchmarks, bottleneck analysis (#199)
- [ ] **Phase 38.5: Manual Verification - Benchmarks** USER CHECKPOINT - Run full benchmark suite, validate results

### v3.8 SMB3 Protocol Upgrade

- [ ] **Phase 39: SMB3 Security & Encryption** - Dialect negotiation (3.0/3.0.2/3.1.1), AES signing, AES encryption, preauth integrity
- [ ] **Phase 40: SMB3 Leases & Locking** - Read/Write/Handle leases, directory leases, break notifications, Unified Lock Manager integration
- [ ] **Phase 41: SMB3 Authentication & ACLs** - SPNEGO/Kerberos via shared layer, NTLM fallback, Windows security descriptors, ACL translation
- [ ] **Phase 42: SMB3 Resilience** - Durable handles v1/v2, handle state tracking, reconnection with restoration
- [ ] **Phase 43: SMB3 Cross-Protocol Integration** - Immediate write visibility, bidirectional lock coordination, cross-protocol ACL consistency
- [ ] **Phase 44: SMB3 Conformance Testing** - Microsoft WindowsProtocolTestSuites (FileServer), smbtorture SMB3 tests, Go integration tests, client compatibility
- [ ] **Phase 44.5: Manual Verification - SMB3** USER CHECKPOINT - Verify SMB3 with Windows 10/11, macOS, Linux clients

### v4.0 NFSv4.2 Extensions

- [ ] **Phase 45: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 46: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 47: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 47.5: Manual Verification - Advanced Ops** USER CHECKPOINT - Test copy/clone/sparse
- [ ] **Phase 48: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 49: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 50: Documentation** - Complete documentation for all new features
- [ ] **Phase 51: v4.0 Testing** - Final testing and pjdfstest POSIX compliance
- [ ] **Phase 51.5: Final Manual Verification** USER CHECKPOINT - Complete validation of all features

## Phase Details

---

## v3.5 Adapter + Core Refactoring

### Phase 26: Generic Lock Interface & Protocol Leak Purge
**Goal**: Unify lock types across NFS/SMB and purge protocol-specific code from generic layers
**Depends on**: Phase 25 (v3.0 complete)
**Requirements**: REF-01, REF-02
**Reference**: `docs/GENERIC_LOCK_INTERFACE_PLAN.md`, `docs/CORE_REFACTORING_PLAN.md` Phase 1
**Success Criteria** (what must be TRUE):
  1. `EnhancedLock` renamed to `UnifiedLock` with `OpLock` (was `LeaseInfo`) and `AccessMode` (was `ShareReservation`)
  2. All SMB lock types (ShareReservation, LeaseInfo, lease_break) removed from `pkg/metadata/lock/`
  3. SMB lease methods removed from MetadataService (CheckAndBreakLeases*, ReclaimLeaseSMB, OplockChecker)
  4. NLM methods moved from MetadataService to NFS adapter layer
  5. GracePeriodManager stays generic in `pkg/metadata/lock/` (used by NLM, NFSv4, SMB reconnect)
  6. Share model cleaned: NFS/SMB-specific fields moved to JSON config blob
  7. SquashMode, Netgroup, IdentityMapping removed from generic store/runtime interfaces
  8. NFS-specific API handlers, runtime fields, and `pkg/identity/` moved to NFS adapter
  9. All existing tests pass with renamed types
  10. Centralized conflict detection handles all cases (oplock vs oplock, oplock vs byte-range, byte-range vs byte-range, access mode)
**Plans**: 5 plans
Plans:
- [ ] 26-01-PLAN.md — Lock type renames (UnifiedLock, OpLock, AccessMode) + mechanical rename across all consumers
- [ ] 26-02-PLAN.md — Share model cleanup, adapter config table, Store interface extraction
- [ ] 26-03-PLAN.md — LockManager interface, ConflictsWith, typed break callbacks
- [ ] 26-04-PLAN.md — NLM extraction to NFS adapter, SMB lease purge from MetadataService
- [ ] 26-05-PLAN.md — Runtime/API/identity purge, adapter-scoped settings API

### Phase 27: NFS Adapter Restructuring
**Goal**: Restructure NFS adapter for clean directory layout and dispatch consolidation
**Depends on**: Phase 26
**Requirements**: REF-03
**Reference**: `docs/NFS_REFACTORING_PLAN.md` Steps 1-9
**Success Criteria** (what must be TRUE):
  1. `internal/protocol/` renamed to `internal/adapter/`
  2. Generic XDR, NLM, NSM, portmapper consolidated under `internal/adapter/nfs/`
  3. `internal/auth/` (ntlm+spnego) moved to `internal/adapter/smb/auth/`
  4. `pkg/adapter/nfs/` files renamed (remove `nfs_` prefix)
  5. v4/v4.1 split into nested hierarchy (`v4/v4_1/`)
  6. Dispatch consolidated: single `nfs.Dispatch()` entry point in `internal/adapter/nfs/`
  7. Connection code split by version concern (connection.go + connection_v4.go)
  8. Shared handler helpers extracted to `internal/adapter/nfs/helpers.go`
  9. Handler documentation added (3-5 lines each)
  10. Version negotiation tests added (v2 reject, v5 reject, minor=2 reject, unknown program)
**Plans**: TBD

### Phase 28: SMB Adapter Restructuring
**Goal**: Restructure SMB adapter to mirror NFS pattern, extract shared BaseAdapter
**Depends on**: Phase 27
**Requirements**: REF-04
**Reference**: `docs/SMB_REFACTORING_PLAN.md` Steps 4-11
**Success Criteria** (what must be TRUE):
  1. `pkg/adapter/smb/` files renamed (remove `smb_` prefix)
  2. `BaseAdapter` extracted to `pkg/adapter/base.go` (shared NFS+SMB lifecycle)
  3. NetBIOS framing moved to `internal/adapter/smb/framing.go`
  4. Signing verification moved to `internal/adapter/smb/signing.go`
  5. Dispatch + response logic consolidated in `internal/adapter/smb/dispatch.go`
  6. Compound request handling in `internal/adapter/smb/compound.go`
  7. `Authenticator` interface defined, NTLM + Kerberos implementations extracted
  8. Shared handler helpers extracted to `internal/adapter/smb/helpers.go`
  9. `pkg/adapter/smb/connection.go` reduced to ~150 lines (thin read/dispatch/write loop)
  10. Handler documentation added (3-5 lines each)
**Plans**: TBD

### Phase 29: Core Layer Decomposition
**Goal**: Decompose god objects, unify errors, reduce boilerplate
**Depends on**: Phase 26
**Requirements**: REF-05, REF-06
**Reference**: `docs/CORE_REFACTORING_PLAN.md` Phases 2-9
**Success Criteria** (what must be TRUE):
  1. ControlPlane Store interface decomposed into 9 sub-interfaces (UserStore, GroupStore, etc.)
  2. API handlers accept narrowest interface needed
  3. Runtime split: AdapterManager and MetadataStoreManager extracted (~500 lines remaining)
  4. TransferManager renamed to Offloader, package moved to `pkg/payload/offloader/`
  5. Offloader split into upload.go, download.go, dedup.go (~400 lines in main file)
  6. Structured PayloadError type with errors.Is() compatibility
  7. Generic GORM helpers reduce CRUD boilerplate
  8. API error mapping centralized
  9. `pkg/metadata/file.go` (1217 lines) split into file_create.go, file_modify.go, file_remove.go, file_helpers.go
  10. `pkg/metadata/authentication.go` (796 lines) split into identity.go, permissions.go
**Plans**: TBD

---

## v3.6 Windows Compatibility

### Phase 30: SMB Bug Fixes
**Goal**: Fix known SMB bugs found during Windows testing
**Depends on**: Phase 29 (refactoring complete)
**Requirements**: WIN-01
**Reference**: GitHub issues #180, #181
**Success Criteria** (what must be TRUE):
  1. Sparse file READ returns zeros for unwritten blocks instead of "block not found" error (#180)
  2. TransferManager/Offloader downloadBlock() handles ErrBlockNotFound as sparse region
  3. Renamed directories show as `<DIR>` in parent listing (#181)
  4. Move operation updates file Path field in metadata
  5. E2E tests cover both bug scenarios
**Plans**: TBD

### Phase 31: Windows ACL Support
**Goal**: Implement NT Security Descriptors for proper Windows ACL display and control
**Depends on**: Phase 30
**Requirements**: WIN-02
**Reference**: GitHub issue #182, MS-DTYP, MS-SMB2 Section 2.2.39
**Success Criteria** (what must be TRUE):
  1. QUERY_INFO SecurityInformation returns proper NT Security Descriptor (Owner SID, Group SID, DACL)
  2. Unix UID/GID mapped to Windows SIDs (well-known SIDs for common accounts, S-1-22-x-y for Unix)
  3. Unix file permissions (rwx) translated to Windows ACE entries in DACL
  4. `icacls` on mounted share shows meaningful permissions (not Everyone:(F))
  5. SET_INFO SecurityInformation accepts permission changes (best-effort mapping back to Unix)
  6. Directory inheritance flags set correctly (CONTAINER_INHERIT_ACE, OBJECT_INHERIT_ACE)
  7. SMB and NFSv4 ACLs remain interoperable through shared metadata model
**Plans**: TBD

### Phase 32: Windows Integration Testing
**Goal**: Comprehensive Windows compatibility validation using automated test suites and manual testing
**Depends on**: Phase 31
**Requirements**: WIN-03
**Reference**: Microsoft WindowsProtocolTestSuites (MIT), Samba smbtorture (GPL)
**Success Criteria** (what must be TRUE):
  1. Samba smbtorture SMB2 basic tests pass (smb2.connect, smb2.read, smb2.write, smb2.lock, smb2.oplock, smb2.lease)
  2. Samba smbtorture SMB2 ACL tests pass (smb2.acls, smb2.dir)
  3. Microsoft WindowsProtocolTestSuites File Server BVT suite passes (101 core tests)
  4. Microsoft WindowsProtocolTestSuites selected feature tests pass (lease, oplock, lock, signing, encryption categories)
  5. Windows 11 manual validation: Explorer file operations (create, rename, delete, copy, move, drag-and-drop)
  6. Windows 11 manual validation: cmd.exe operations (dir, type, copy, move, ren, del, mkdir, rmdir, icacls, fsutil)
  7. Windows 11 manual validation: PowerShell operations (Get-Item, Set-Item, Get-Acl, Set-Acl)
  8. All issues #180, #181, #182 verified fixed on Windows
  9. No regressions on Linux/macOS NFS or SMB mounts
**Plans**: TBD

---

## v3.7 Benchmarking Suite

### Phase 33: Benchmark Infrastructure
**Goal**: Create bench/ directory structure with Docker Compose profiles and configuration files
**Depends on**: Phase 32 (v3.6 complete)
**Requirements**: BENCH-01
**Reference**: GitHub #194
**Success Criteria** (what must be TRUE):
  1. `bench/` directory structure created (configs/, workloads/, scripts/, analysis/, results/)
  2. `docker-compose.yml` with profiles: dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs, juicefs, ganesha, rclone, kernel-nfs, samba, dittofs-smb, monitoring
  3. `.env.example` with S3, PostgreSQL, and benchmark configuration variables
  4. DittoFS config files for each backend combination (badger+s3, postgres+s3, badger+fs)
  5. `scripts/check-prerequisites.sh` validates fio, nfs-common, cifs-utils, python3, docker, jq, bc
  6. Only one profile active at a time (no resource contention)
  7. `results/` directory gitignored
**Plans**: TBD

### Phase 34: Benchmark Workloads
**Goal**: Create fio job files for all I/O workloads and a custom metadata benchmark script
**Depends on**: Phase 33
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

### Phase 35: Competitor Setup
**Goal**: Create configuration files and setup scripts for each competitor system
**Depends on**: Phase 33
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

### Phase 36: Orchestrator Scripts
**Goal**: Create main benchmark orchestrator and all helper scripts with platform variants
**Depends on**: Phase 34, Phase 35
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

### Phase 37: Analysis & Reporting
**Goal**: Create Python analysis pipeline for parsing results, generating charts, and producing reports
**Depends on**: Phase 34
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

### Phase 38: Profiling Integration
**Goal**: Integrate DittoFS observability stack for performance bottleneck identification
**Depends on**: Phase 36
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

## v3.8 SMB3 Protocol Upgrade

### Phase 39: SMB3 Security & Encryption
**Goal**: SMB3 dialect negotiation with encryption and signing, preventing eavesdropping and tampering
**Depends on**: Phase 38 (v3.7 complete)
**Requirements**: SEC-01 through SEC-12, SMB3-TEST-01, SMB3-TEST-02
**Reference**: feat/smb3 branch, MS-SMB2
**Success Criteria** (what must be TRUE):
  1. Windows 10/11 client can connect using SMB 3.1.1 dialect with encrypted traffic
  2. SMB 3.0.2 clients (older Windows, macOS) can connect with AES-CCM encryption
  3. Downgrade attacks blocked — client specifying 3.1.1 cannot be forced to 2.x
  4. Per-share encryption settings work (one share encrypted, another unencrypted)
  5. AES-CMAC signing (3.0+) and AES-GMAC signing (3.1.1) both functional
  6. Preauth integrity SHA-512 hash chain validated
  7. E2E tests verify encryption and signing for all cipher suites
**Plans**: TBD

### Phase 40: SMB3 Leases & Locking
**Goal**: SMB3 lease caching with break notifications, coordinated with NFS delegations via Unified Lock Manager
**Depends on**: Phase 39
**Requirements**: LEASE-01 through LEASE-07, SMB3-TEST-03
**Reference**: feat/smb3 branch
**Success Criteria** (what must be TRUE):
  1. SMB3 client can open file with oplock lease and cache reads locally
  2. Second client opening same file triggers lease break notification to first client
  3. Directory leases work — client caches directory listing until change notification
  4. SMB lease and NFS delegation conflict properly (SMB write lease breaks on NFS open)
  5. E2E tests verify lease acquisition, break, and cross-protocol coordination
**Plans**: TBD

### Phase 41: SMB3 Authentication & ACLs
**Goal**: Kerberos/SPNEGO authentication and Windows security descriptor support
**Depends on**: Phase 39
**Requirements**: AUTH-01 through AUTH-06, ACL-01 through ACL-04, SMB3-TEST-04
**Reference**: feat/smb3 branch, MS-DTYP
**Success Criteria** (what must be TRUE):
  1. Domain-joined Windows client can access share via Kerberos (no password prompt)
  2. Non-domain client falls back to NTLM authentication
  3. Guest/anonymous access works on configured shares
  4. Windows security properties dialog shows correct permissions (from control plane ACLs)
  5. Modifying permissions via Windows dialog updates control plane (SET_INFO)
  6. Cross-protocol ACL consistency maintained (SMB ACL <-> NFSv4 ACL <-> control plane)
**Plans**: TBD

### Phase 42: SMB3 Resilience
**Goal**: Durable handles for connection reliability across brief disconnects
**Depends on**: Phase 40
**Requirements**: RES-01 through RES-04
**Reference**: feat/smb3 branch
**Success Criteria** (what must be TRUE):
  1. Client with durable handle reconnects after 30-second network interruption without losing open file
  2. Durable handle v2 with create GUID allows proper reconnection identification
  3. Open files survive client network adapter reset
  4. Handle state tracking validates reconnection claims
**Plans**: TBD

### Phase 43: SMB3 Cross-Protocol Integration
**Goal**: Unified behavior across SMB3/NFSv3/NFSv4 — immediate visibility, bidirectional locking, consistent ACLs
**Depends on**: Phase 40, Phase 41, Phase 42
**Requirements**: XPROTO-01 through XPROTO-03, SMB3-TEST-05, SMB3-TEST-06
**Reference**: feat/smb3 branch
**Success Criteria** (what must be TRUE):
  1. Write via SMB3 immediately readable via NFS (no cache delay)
  2. SMB3 byte-range lock blocks NFS write to same range
  3. NFS byte-range lock blocks SMB3 write to same range
  4. ACLs set via SMB3 visible via NFSv4 ACL query (and vice versa)
  5. Windows 10/11, macOS, and Linux SMB clients all verified
**Plans**: TBD

### Phase 44: SMB3 Conformance Testing
**Goal**: Validate SMB3 implementation against industry-standard conformance test suites and verify client compatibility
**Depends on**: Phase 39, Phase 40, Phase 41, Phase 42, Phase 43
**Requirements**: SMB3-CONF-01 through SMB3-CONF-05
**Reference**: [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) (MIT), [Samba smbtorture](https://wiki.samba.org/index.php/Writing_Torture_Tests) (GPLv3)
**Success Criteria** (what must be TRUE):
  1. Microsoft WindowsProtocolTestSuites FileServer BVT suite passes (101 core tests via Docker image `mcr.microsoft.com/windowsprotocoltestsuites:fileserver`)
  2. Microsoft WPTS SMB3-specific feature tests pass: Encryption (AES-128/256-CCM/GCM), Signing, Negotiate (dialect contexts), DurableHandle (v1+v2), Leasing, Replay, SessionMgmt
  3. Microsoft WPTS dialect-filtered tests pass for Smb30, Smb302, and Smb311 categories
  4. Samba smbtorture SMB3 tests pass: smb2.durable_v2_open, smb2.lease, smb2.dirlease, smb2.replay, smb2.session, smb2.session_req_sign, smb2.compound, smb2.oplocks, smb2.lock, smb2.acls
  5. Go integration tests (hirochachacha/go-smb2, BSD-2-Clause) verify basic client-server interop with SMB3 dialects
  6. Client compatibility matrix validated: Windows 10, Windows 11 (SMB 3.1.1), macOS (SMB 3.0.2), Linux cifs.ko (SMB 3.1.1)
  7. No regressions on SMB2 clients or NFS mounts
  8. Test infrastructure Dockerized for CI repeatability (WPTS Docker image + smbtorture container)
**Plans**: TBD

---

## v4.0 NFSv4.2 Extensions

### Phase 45: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 44 (v3.8 complete)
**Requirements**: V42-01
**Success Criteria** (what must be TRUE):
  1. COPY operation copies data without client I/O
  2. Async COPY returns immediately with stateid for tracking
  3. OFFLOAD_STATUS reports copy progress
  4. OFFLOAD_CANCEL terminates in-progress copy
  5. Large file copy completes efficiently via block store
**Plans**: TBD

### Phase 46: Clone/Reflinks
**Goal**: Implement CLONE operation leveraging content-addressed storage
**Depends on**: Phase 45
**Requirements**: V42-02
**Success Criteria** (what must be TRUE):
  1. CLONE creates copy-on-write file instantly
  2. Cloned files share blocks until modification
  3. Modification triggers copy of affected blocks only
**Plans**: TBD

### Phase 47: Sparse Files
**Goal**: Implement sparse file operations (SEEK, ALLOCATE, DEALLOCATE)
**Depends on**: Phase 45
**Requirements**: V42-03
**Success Criteria** (what must be TRUE):
  1. SEEK locates DATA or HOLE regions in file
  2. ALLOCATE pre-allocates file space
  3. DEALLOCATE punches holes in file
  4. Sparse file metadata correctly tracks allocated regions
**Plans**: TBD

### Phase 48: Extended Attributes
**Goal**: Implement xattr storage and NFSv4.2/SMB exposure
**Depends on**: Phase 45
**Requirements**: V42-04
**Success Criteria** (what must be TRUE):
  1. GETXATTR retrieves extended attribute value
  2. SETXATTR stores extended attribute
  3. LISTXATTRS enumerates all xattr names
  4. REMOVEXATTR deletes extended attribute
  5. Xattrs accessible via both NFSv4.2 and SMB
**Plans**: TBD

### Phase 49: NFSv4.2 Operations
**Goal**: Implement remaining NFSv4.2 operations
**Depends on**: Phase 47
**Requirements**: V42-05
**Success Criteria** (what must be TRUE):
  1. IO_ADVISE accepts application I/O hints
  2. LAYOUTERROR and LAYOUTSTATS available if pNFS enabled
**Plans**: TBD

### Phase 50: Documentation
**Goal**: Complete documentation for all new features
**Depends on**: Phase 48
**Requirements**: (documentation)
**Success Criteria** (what must be TRUE):
  1. docs/NFS.md updated with NFSv4.1 and NFSv4.2 details
  2. docs/CONFIGURATION.md covers all new session and v4.2 options
  3. docs/SECURITY.md describes Kerberos security model for NFS and SMB
**Plans**: TBD

### Phase 51: v4.0 Testing
**Goal**: Final testing including pjdfstest POSIX compliance
**Depends on**: Phase 45, Phase 46, Phase 47, Phase 48, Phase 49, Phase 50
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

## Progress

**Execution Order:**
Phases execute in numeric order: 1 -> 2 -> 3 -> ... -> 51

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
| 26. Generic Lock Interface & Protocol Leak Purge | 5/5 | Complete    | 2026-02-25 | - |
| 27. NFS Adapter Restructuring | v3.5 | 0/? | Not started | - |
| 28. SMB Adapter Restructuring | v3.5 | 0/? | Not started | - |
| 29. Core Layer Decomposition | v3.5 | 0/? | Not started | - |
| 30. SMB Bug Fixes | v3.6 | 0/? | Not started | - |
| 31. Windows ACL Support | v3.6 | 0/? | Not started | - |
| 32. Windows Integration Testing | v3.6 | 0/? | Not started | - |
| 33. Benchmark Infrastructure | v3.7 | 0/? | Not started | - |
| 34. Benchmark Workloads | v3.7 | 0/? | Not started | - |
| 35. Competitor Setup | v3.7 | 0/? | Not started | - |
| 36. Orchestrator Scripts | v3.7 | 0/? | Not started | - |
| 37. Analysis & Reporting | v3.7 | 0/? | Not started | - |
| 38. Profiling Integration | v3.7 | 0/? | Not started | - |
| 39. SMB3 Security & Encryption | v3.8 | 0/? | Not started | - |
| 40. SMB3 Leases & Locking | v3.8 | 0/? | Not started | - |
| 41. SMB3 Authentication & ACLs | v3.8 | 0/? | Not started | - |
| 42. SMB3 Resilience | v3.8 | 0/? | Not started | - |
| 43. SMB3 Cross-Protocol Integration | v3.8 | 0/? | Not started | - |
| 44. SMB3 Conformance Testing | v3.8 | 0/? | Not started | - |
| 45. Server-Side Copy | v4.0 | 0/? | Not started | - |
| 46. Clone/Reflinks | v4.0 | 0/? | Not started | - |
| 47. Sparse Files | v4.0 | 0/? | Not started | - |
| 48. Extended Attributes | v4.0 | 0/? | Not started | - |
| 49. NFSv4.2 Operations | v4.0 | 0/? | Not started | - |
| 50. Documentation | v4.0 | 0/? | Not started | - |
| 51. v4.0 Testing | v4.0 | 0/? | Not started | - |

**Total:** 86/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
