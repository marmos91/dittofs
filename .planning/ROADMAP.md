# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across eight milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, signing, leases, Kerberos, and durable handles, v4.0 refactors the storage layer into a clean two-tier block store model (Local + Remote), and v4.1 completes the protocol suite with NFSv4.2 advanced features. Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [x] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (shipped 2026-02-26) — [archive](milestones/v3.5-ROADMAP.md)
- [x] **v3.6 Windows Compatibility** - Phases 29.8-32.5 (shipped 2026-02-28) — [archive](milestones/v3.6-ROADMAP.md)
- [x] **v3.8 SMB3 Protocol Upgrade** - Phases 33-40.5 (shipped 2026-03-04) — [archive](milestones/v3.8-ROADMAP.md)
- [ ] **v4.0 BlockStore Unification Refactor** - Phases 41-49 (planned)
- [ ] **v4.1 NFSv4.2 Extensions** - Phases 50-56 (planned)
- [ ] **v4.2 Benchmarking & Performance** - Phases 57-62 (planned)

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

### v4.0 BlockStore Unification Refactor

- [ ] **Phase 41: Block State Enum and ListFileBlocks** - Rename states (Sealed→Local, Uploaded→Remote), update ListPendingUpload→ListLocalBlocks, add ListFileBlocks method
- [ ] **Phase 42: Legacy Cleanup** - Remove DirectWriteStore interface and filesystem payload store
- [ ] **Phase 43: Local-Only Block Management** - Block management operations on cache, local-only offloader mode without remote store
- [ ] **Phase 44: Data Model and API/CLI** - BlockStoreConfig DB model, REST endpoints, dfsctl block store commands
- [ ] **Phase 45: Package Restructure** - Create pkg/blockstore/ hierarchy absorbing cache, payload, offloader, gc
- [ ] **Phase 46: Per-Share Block Store Wiring** - Runtime manages per-share BlockStore instances replacing global PayloadService
- [ ] **Phase 47: L1 Read Cache and Prefetch** - Read-through LRU cache with sequential prefetch for hot blocks
- [ ] **Phase 48: Auto-Deduced Configuration** - Derive buffer/cache sizes and concurrency from CPU/memory
- [ ] **Phase 49: Testing and Documentation** - E2E tests for new CLI, multi-share isolation, updated documentation

### v4.1 NFSv4.2 Extensions

- [ ] **Phase 50: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 51: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 52: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 53: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 54: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 55: Documentation** - Complete documentation for all new features
- [ ] **Phase 56: v4.1 Testing** - Final testing and pjdfstest POSIX compliance

### v4.2 Benchmarking & Performance

- [ ] **Phase 57: Benchmark Infrastructure** - Docker Compose profiles and directory structure (carried from previous Phase 33, already COMPLETE)
- [ ] **Phase 58: Benchmark Workloads** - fio job files and metadata benchmark scripts
- [ ] **Phase 59: Competitor Setup** - Configuration for JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba
- [ ] **Phase 60: Orchestrator Scripts** - Main benchmark runner with platform variants
- [ ] **Phase 61: Analysis & Reporting** - Python pipeline for charts and markdown reports
- [ ] **Phase 62: Profiling Integration** - Prometheus, Pyroscope, pprof for bottleneck identification

## Phase Details

---

<details>
<summary>v3.8 SMB3 Protocol Upgrade (Phases 33-40.5) — SHIPPED 2026-03-04</summary>

Full phase details archived to [milestones/v3.8-ROADMAP.md](milestones/v3.8-ROADMAP.md).

8 phases, 26 plans: dialect negotiation, KDF/signing, encryption, Kerberos, leases, durable handles, cross-protocol integration, conformance testing.

</details>

---

## v4.0 BlockStore Unification Refactor

### Phase 41: Block State Enum and ListFileBlocks
**Goal**: Rename block state enum values and update query methods to reflect new terminology
**Depends on**: Phase 40.5 (v3.8 complete)
**Requirements**: STATE-01, STATE-02, STATE-03, STATE-04, STATE-05, STATE-06
**Success Criteria** (what must be TRUE):
  1. Block state constants use new names: Dirty(0), Local(1), Uploading(2), Remote(3)
  2. All code referencing Sealed or Uploaded updated to Local and Remote
  3. ListLocalBlocks method replaces ListPendingUpload across all implementations
  4. ListRemoteBlocks method replaces ListEvictable across all implementations
  5. ListFileBlocks(payloadID) method exists and returns all blocks for a file
  6. BadgerDB secondary index uses fb-local: prefix instead of fb-sealed:
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 42: Legacy Cleanup
**Goal**: Remove DirectWriteStore interface and filesystem payload store dead code
**Depends on**: Phase 41
**Requirements**: CLEAN-01, CLEAN-02, CLEAN-03, CLEAN-04, CLEAN-05, CLEAN-06
**Success Criteria** (what must be TRUE):
  1. DirectWriteStore interface removed from pkg/payload/store/store.go
  2. pkg/payload/store/fs/ directory deleted entirely
  3. Cache methods directWritePath, SetDirectWritePath, IsDirectWrite removed
  4. Offloader no longer checks IsDirectWrite or handles direct-write paths
  5. init.go removed blockfs import and DirectWriteStore detection logic
  6. All direct-write conditional branches removed from cache operations
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 43: Local-Only Block Management
**Goal**: Add block management operations to cache and support offloader without remote store
**Depends on**: Phase 42
**Requirements**: LOCAL-01, LOCAL-02, LOCAL-03, LOCAL-04
**Success Criteria** (what must be TRUE):
  1. Cache provides DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk methods
  2. SetEvictionEnabled method exists to control local block retention
  3. Offloader accepts nil blockStore and operates in local-only mode
  4. Local-only flush marks blocks BlockStateLocal without upload attempt
  5. Runtime wiring creates local-only BlockStore when no remote store configured
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 44: Data Model and API/CLI
**Goal**: Create BlockStoreConfig model and REST/CLI endpoints for local and remote block stores
**Depends on**: Phase 43
**Requirements**: MODEL-01, MODEL-02, MODEL-03, MODEL-04, MODEL-05, API-01, API-02, API-03, CLI-01, CLI-02, CLI-03, CLI-04
**Success Criteria** (what must be TRUE):
  1. BlockStoreConfig model exists with ID, Name, Kind (local/remote), Type, Config, CreatedAt fields
  2. Share model has LocalBlockStoreID (mandatory) and RemoteBlockStoreID (nullable)
  3. Database migration renames payload_store_configs to block_store_configs with kind column
  4. Database migration splits Share.PayloadStoreID into LocalBlockStoreID + RemoteBlockStoreID
  5. BlockStoreConfigStore interface with CRUD methods filtered by kind replaces PayloadStoreConfigStore
  6. REST endpoints /api/v1/block-stores/local and /api/v1/block-stores/remote exist for CRUD
  7. Share endpoints accept --local (required) and --remote (optional) parameters
  8. dfsctl store block local add/list/edit/remove commands work
  9. dfsctl store block remote add/list/edit/remove commands work
  10. dfsctl share create --local X --remote Y replaces --payload flag
  11. API client methods for block store operations replace payload store methods
**Verification**: `go build ./...` && `go test ./pkg/controlplane/...` && manual CLI test
**Plans**: TBD

### Phase 45: Package Restructure
**Goal**: Reorganize storage code into clean pkg/blockstore/ hierarchy
**Depends on**: Phase 44
**Requirements**: PKG-01, PKG-02, PKG-03, PKG-04, PKG-05, PKG-06, PKG-07, PKG-08, PKG-09, PKG-10, PKG-11
**Success Criteria** (what must be TRUE):
  1. pkg/blockstore/local/local.go defines LocalStore interface
  2. pkg/blockstore/remote/remote.go defines RemoteStore interface
  3. pkg/cache/ code moved to pkg/blockstore/local/fs/
  4. pkg/blockstore/local/memory/ created for test MemoryLocalStore
  5. pkg/payload/store/s3/ moved to pkg/blockstore/remote/s3/
  6. pkg/payload/store/memory/ moved to pkg/blockstore/remote/memory/
  7. pkg/payload/offloader/ moved to pkg/blockstore/offloader/
  8. pkg/payload/gc/ moved to pkg/blockstore/gc/
  9. pkg/blockstore/blockstore.go orchestrator absorbs PayloadService responsibilities
  10. All consumer imports updated (NFS handlers, SMB handlers, runtime, shares)
  11. pkg/cache/ and pkg/payload/ directories deleted
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 46: Per-Share Block Store Wiring
**Goal**: Replace global PayloadService with per-share BlockStore instances
**Depends on**: Phase 45
**Requirements**: SHARE-01, SHARE-02, SHARE-03, SHARE-04
**Success Criteria** (what must be TRUE):
  1. Runtime manages map[shareID]*BlockStore instead of single PayloadService
  2. EnsureBlockStore(share) creates BlockStore with share's local + remote configs
  3. NFS/SMB handlers resolve BlockStore per share handle via getBlockStore(shareHandle)
  4. Multiple shares with different local paths operate in isolation
  5. Share deletion cleans up associated BlockStore
**Verification**: `go build ./...` && `go test ./pkg/controlplane/...` && multi-share E2E test
**Plans**: TBD

### Phase 47: L1 Read Cache and Prefetch
**Goal**: Add read-through LRU cache with sequential prefetch for hot blocks
**Depends on**: Phase 46
**Requirements**: PERF-01, PERF-02, PERF-03, PERF-04
**Success Criteria** (what must be TRUE):
  1. L1 read-through LRU cache (readcache.go) caches hot blocks in memory
  2. Cache invalidation on WriteAt removes stale entries
  3. Sequential prefetch (prefetch.go) triggered after 2+ sequential reads
  4. Prefetch worker pool bounded, non-blocking, avoids cache pollution
  5. Sequential read benchmark shows improved throughput with L1 cache
**Verification**: `go test ./pkg/blockstore/...` && sequential read benchmark
**Plans**: TBD

### Phase 48: Auto-Deduced Configuration
**Goal**: Derive buffer/cache sizes and concurrency from system resources
**Depends on**: Phase 47
**Requirements**: AUTO-01, AUTO-02, AUTO-03, AUTO-04, AUTO-05
**Success Criteria** (what must be TRUE):
  1. WriteBufferMemory defaults to 25% of available memory if not configured
  2. ReadCacheMemory defaults to 12.5% of available memory if not configured
  3. ParallelUploads defaults to max(4, runtime.NumCPU()) if not configured
  4. ParallelDownloads defaults to max(8, runtime.NumCPU()*2) if not configured
  5. User-provided config values override auto-deduced defaults
**Verification**: `go test ./pkg/config/...` && config validation test
**Plans**: TBD

### Phase 49: Testing and Documentation
**Goal**: Update E2E tests and documentation for new block store architecture
**Depends on**: Phase 48
**Requirements**: TEST-01, TEST-02, TEST-03, DOCS-01, DOCS-02, DOCS-03, DOCS-04
**Success Criteria** (what must be TRUE):
  1. E2E test matrix updated for new CLI (dfsctl store block local/remote)
  2. Multi-share E2E test validates isolation with different local paths
  3. Sequential read benchmark validates L1 cache performance improvement
  4. docs/ARCHITECTURE.md updated to describe two-tier block store model
  5. docs/CONFIGURATION.md updated for new block store CLI and config schema
  6. docs/CLAUDE.md updated to reflect pkg/blockstore/ structure
  7. --payload flag shows deprecation warning but still works for backward compatibility
**Verification**: `go test -tags=e2e ./test/e2e/...` && documentation review
**Plans**: TBD

---

## v4.1 NFSv4.2 Extensions

### Phase 50: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 49 (v4.0 complete)
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

### Phase 56: v4.1 Testing
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

## v4.2 Benchmarking & Performance

### Phase 57: Benchmark Infrastructure
**Goal**: Create bench/ directory structure with Docker Compose profiles and configuration files
**Depends on**: Phase 56 (v4.1 complete)
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
**Verification**: Docker Compose validation
**Plans**: 2/2 (COMPLETED)
Plans:
- [x] 48-01-PLAN.md — Docker Compose infrastructure, directory structure, DittoFS configs
- [x] 48-02-PLAN.md — Prerequisites check, cleanup scripts, shared library, Makefile

### Phase 58: Benchmark Workloads
**Goal**: Create fio job files for all I/O workloads and a custom metadata benchmark script
**Depends on**: Phase 57
**Requirements**: BENCH-02
**Reference**: GitHub #195
**Success Criteria** (what must be TRUE):
  1. fio job files: seq-read-large (1MB), seq-write-large (1MB), rand-read-4k, rand-write-4k, mixed-rw-70-30, large-file-1gb
  2. Common parameters: runtime=60, time_based=1, output-format=json+, parameterized threads/mountpoint
  3. macOS variants with posixaio engine and direct=0
  4. `scripts/metadata-bench.sh` measuring create/stat/readdir/delete ops for 1K/10K files
  5. Deep tree benchmark (depth=5, fan=10) with create and walk
  6. Metadata script outputs JSON with ops/sec and total time
**Verification**: fio job validation
**Plans**: TBD

### Phase 59: Competitor Setup
**Goal**: Create configuration files and setup scripts for each competitor system
**Depends on**: Phase 57
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
**Verification**: Configuration validation
**Plans**: TBD

### Phase 60: Orchestrator Scripts
**Goal**: Create main benchmark orchestrator and all helper scripts with platform variants
**Depends on**: Phase 58, Phase 59
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
**Verification**: Script validation
**Plans**: TBD

### Phase 61: Analysis & Reporting
**Goal**: Create Python analysis pipeline for parsing results, generating charts, and producing reports
**Depends on**: Phase 58
**Requirements**: BENCH-05
**Reference**: GitHub #197
**Success Criteria** (what must be TRUE):
  1. `parse_fio.py` extracts throughput (MB/s), IOPS, latency (p50/p95/p99/p99.9) with mean/stddev
  2. `parse_metadata.py` extracts create/stat/readdir/delete ops/sec across iterations
  3. `generate_charts.py` produces charts: tier1 throughput/IOPS/latency, tier2 userspace comparison, tier3 metadata, tier4 scaling, SMB comparison
  4. `generate_report.py` with Jinja2 template producing markdown report with environment details, summary tables, per-tier details, methodology section
  5. `requirements.txt` with pandas, matplotlib, seaborn, jinja2
  6. Results organized in `results/YYYY-MM-DD_HHMMSS/` with raw/, metrics/, charts/, report.md, summary.csv
**Verification**: Python script validation
**Plans**: TBD

### Phase 62: Profiling Integration
**Goal**: Integrate DittoFS observability stack for performance bottleneck identification
**Depends on**: Phase 60
**Requirements**: BENCH-06
**Reference**: GitHub #199
**Success Criteria** (what must be TRUE):
  1. DittoFS config with metrics + telemetry + profiling enabled when --with-profiling passed
  2. Monitoring stack: Prometheus (1s scrape), Pyroscope (continuous CPU + memory), Grafana (optional)
  3. `collect-metrics.sh` captures Prometheus range queries, pprof CPU/heap/mutex/goroutine profiles
  4. Analysis identifies bottlenecks: CPU flame graphs, S3 vs metadata latency, GC pauses, mutex contention, cache effectiveness
  5. Benchmark-specific Grafana dashboard for before/during/after metrics
  6. Results in `results/YYYY-MM-DD/metrics/` with prometheus/, pprof/, summary.json
**Verification**: Profiling validation
**Plans**: TBD

---

## Progress

**Execution Order:**
v3.8 (33-40.5) -> v4.0 (41-49) -> v4.1 (50-56) -> v4.2 (57-62)

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
| 33. SMB3 Dialect Negotiation and Preauth Integrity | v3.8 | 3/3 | Complete | 2026-02-28 |
| 34. Key Derivation and Signing | v3.8 | 2/2 | Complete | 2026-03-01 |
| 35. Encryption and Transform Header | v3.8 | Complete | Complete | 2026-03-01 |
| 36. Kerberos SMB3 Integration | v3.8 | Complete | Complete | 2026-03-01 |
| 37. SMB3 Leases and Directory Leasing | v3.8 | Complete | Complete | 2026-03-02 |
| 38. Durable Handles | v3.8 | 3/3 | Complete | 2026-03-02 |
| 39. Cross-Protocol Integration and Documentation | v3.8 | 3/3 | Complete | 2026-03-02 |
| 40. SMB3 Conformance Testing | v3.8 | 6/6 | Complete | 2026-03-02 |
| 41. Block State Enum and ListFileBlocks | v4.0 | 0/? | Not started | - |
| 42. Legacy Cleanup | v4.0 | 0/? | Not started | - |
| 43. Local-Only Block Management | v4.0 | 0/? | Not started | - |
| 44. Data Model and API/CLI | v4.0 | 0/? | Not started | - |
| 45. Package Restructure | v4.0 | 0/? | Not started | - |
| 46. Per-Share Block Store Wiring | v4.0 | 0/? | Not started | - |
| 47. L1 Read Cache and Prefetch | v4.0 | 0/? | Not started | - |
| 48. Auto-Deduced Configuration | v4.0 | 0/? | Not started | - |
| 49. Testing and Documentation | v4.0 | 0/? | Not started | - |
| 50. Server-Side Copy | v4.1 | 0/? | Not started | - |
| 51. Clone/Reflinks | v4.1 | 0/? | Not started | - |
| 52. Sparse Files | v4.1 | 0/? | Not started | - |
| 53. Extended Attributes | v4.1 | 0/? | Not started | - |
| 54. NFSv4.2 Operations | v4.1 | 0/? | Not started | - |
| 55. Documentation | v4.1 | 0/? | Not started | - |
| 56. v4.1 Testing | v4.1 | 0/? | Not started | - |
| 57. Benchmark Infrastructure | v4.2 | 2/2 | Complete | 2026-02-27 |
| 58. Benchmark Workloads | v4.2 | 0/? | Not started | - |
| 59. Competitor Setup | v4.2 | 0/? | Not started | - |
| 60. Orchestrator Scripts | v4.2 | 0/? | Not started | - |
| 61. Analysis & Reporting | v4.2 | 0/? | Not started | - |
| 62. Profiling Integration | v4.2 | 0/? | Not started | - |

**Total:** 124/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
*v3.5 shipped: 2026-02-26*
*v3.6 shipped: 2026-02-28*
*v3.8 shipped: 2026-03-04*
*v4.0 roadmap created: 2026-03-09*
