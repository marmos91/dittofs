# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across fifteen milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, signing, leases, Kerberos, and durable handles, v4.0 refactors the storage layer into a clean two-tier block store model (Local + Remote), v4.2 delivers benchmarking infrastructure, v4.3 fixes NFS/SMB protocol gaps, v4.5 adds block-level compression and encryption, v4.6 hardens the system for production with signing fixes, quotas, client tracking, and trash, v4.7 adds offline/edge resilience for disconnected operation, v4.8 improves developer experience and CI, v4.9 fixes SMB protocol conformance gaps, and v5.0 completes the protocol suite with NFSv4.2 advanced features. Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [x] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (shipped 2026-02-26) — [archive](milestones/v3.5-ROADMAP.md)
- [x] **v3.6 Windows Compatibility** - Phases 29.8-32.5 (shipped 2026-02-28) — [archive](milestones/v3.6-ROADMAP.md)
- [x] **v3.8 SMB3 Protocol Upgrade** - Phases 33-40.5 (shipped 2026-03-04) — [archive](milestones/v3.8-ROADMAP.md)
- [ ] **v4.0 BlockStore Unification Refactor** - Phases 41-49 (planned)
- [x] **v4.2 Benchmarking & Performance** - Phases 57-62 (shipped 2026-03-04)
- [ ] **v4.3 Protocol Gap Fixes** - Phases 49.1-49.3 (planned)
- [ ] **v4.5 BlockStore Security** - Phases 49.4-49.5 (planned)
- [ ] **v4.6 Production Hardening** - Phases 63-67 (planned)
- [ ] **v4.7 Offline/Edge Resilience** - Phases 68-71 (planned)
- [ ] **v4.8 DX/UX Improvements** - Phases 72-74 (planned)
- [ ] **v4.9 SMB Protocol Fixes** - Phases 75-77 (planned)
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

### v4.0 BlockStore Unification Refactor

- [x] **Phase 41: Block State Enum and ListFileBlocks** - Rename states (Sealed->Local, Uploaded->Remote), update ListPendingUpload->ListLocalBlocks, add ListFileBlocks method (completed 2026-03-09)
- [ ] **Phase 42: Legacy Cleanup** - Remove DirectWriteStore interface and filesystem payload store
- [ ] **Phase 43: Local-Only Block Management** - Block management operations on cache, local-only offloader mode without remote store
- [ ] **Phase 44: Data Model and API/CLI** - BlockStoreConfig DB model, REST endpoints, dfsctl block store commands, `dfs cache clean` (#246), `dfs gc` (#189)
- [ ] **Phase 45: Package Restructure** - Create pkg/blockstore/ hierarchy absorbing cache, payload, offloader, gc
- [ ] **Phase 46: Per-Share Block Store Wiring** - Runtime manages per-share BlockStore instances replacing global PayloadService
- [ ] **Phase 47: L1 Read Cache, Disk Tier, and Prefetch** - Tiered read cache (memory + SSD) with sequential prefetch (#187)
- [ ] **Phase 48: Auto-Deduced Configuration and Memory Safety** - Derive sizes from CPU/memory, GOMEMLIMIT for OOM prevention (#258)
- [ ] **Phase 49: Testing and Documentation** - E2E tests for new CLI, multi-share isolation, updated documentation

### v4.3 Protocol Gap Fixes

- [ ] **Phase 49.1: NFSv4 READDIR Cookie Verifier** - Proper cookie verifier instead of zero bytes (#254)
- [ ] **Phase 49.2: READDIRPLUS Performance** - Skip redundant GetFile() when entry.Attr populated (#222)
- [ ] **Phase 49.3: LSA Named Pipe** - lsarpc for Windows SID-to-name resolution (#236)

### v4.5 BlockStore Security

- [ ] **Phase 49.4: Block-Level Compression** - LZ4/Zstd compression decorator for remote stores (#185)
- [ ] **Phase 49.5: Client-Side Encryption** - AES-256-GCM encryption decorator for zero-trust storage (#186)

### v4.6 Production Hardening

- [ ] **Phase 63: SMB3 Signing Fix** - Fix preauth integrity hash mismatch causing macOS mount_smbfs rejection (#252)
- [ ] **Phase 64: Protocol Correctness and Hot-Reload** - NTLM flag cleanup and runtime share visibility across adapters (#215, #235)
- [ ] **Phase 65: Storage Observability and Quotas** - Accurate payload stats and per-share quota enforcement (#216, #232)
- [ ] **Phase 66: Unified Client Tracking** - Protocol-agnostic client registry with REST API and CLI (#157)
- [ ] **Phase 67: Trash and Soft-Delete** - Move-to-trash with configurable retention and background cleanup (#190)

### v4.7 Offline/Edge Resilience

- [ ] **Phase 68: Offline Write Queue** - Queue writes locally when remote store unreachable, WAL-backed persistence for crash safety
- [ ] **Phase 69: Auto-Sync Engine** - Detect connectivity restoration, replay queued writes to remote store with conflict resolution
- [ ] **Phase 70: Cache-Served Offline Reads** - Warm cache serves reads when remote unreachable, configurable prefetch and eviction policies
- [ ] **Phase 71: Edge Integration Testing** - E2E tests for network partition, reconnect sync, cache-only reads, edge deployment scenarios

### v4.8 DX/UX Improvements

- [ ] **Phase 72: Build & CI Optimization** - Makefile targets for all test suites (#206), NFS CI scoped triggers + tiered matrix (#207)
- [ ] **Phase 73: Adapter Config API** - Netgroup-share association API for adapter configuration (#220)
- [ ] **Phase 74: Developer Tooling Polish** - Documentation improvements, developer workflow enhancements

### v4.9 SMB Protocol Fixes

- [ ] **Phase 75: SMB Credits & IOCTL** - Credit granting algorithm conformance, IOCTL handler completeness (#268)
- [ ] **Phase 76: SMB Multichannel & Timestamps** - Multichannel connection support, timestamp precision fixes (#268)
- [ ] **Phase 77: SMB Conformance Iteration** - Remaining smbtorture failures, iterative pass rate improvement (#268)

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
  1. Block state constants use new names: Dirty(0), Local(1), Syncing(2), Remote(3)
  2. All code referencing Sealed or Uploaded updated to Local and Remote
  3. ListLocalBlocks method replaces ListPendingUpload across all implementations
  4. ListRemoteBlocks method replaces ListEvictable across all implementations
  5. ListFileBlocks(payloadID) method exists and returns all blocks for a file
  6. BadgerDB secondary index uses fb-local: prefix instead of fb-sealed:
**Verification**: `go build ./...` && `go test ./...`
**Plans**: 2 plans
Plans:
- [x] 41-01-PLAN.md — Rename state enum, query methods, update all consumers in cache/offloader
- [ ] 41-02-PLAN.md — Add ListFileBlocks method, conformance tests for FileBlockStore

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
**Goal**: Create BlockStoreConfig model, REST/CLI endpoints for block stores, cache and GC commands
**Depends on**: Phase 43
**Requirements**: MODEL-01, MODEL-02, MODEL-03, MODEL-04, MODEL-05, API-01, API-02, API-03, CLI-01, CLI-02, CLI-03, CLI-04
**Reference**: GitHub #246, #189
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
  12. `dfs cache clean` command purges cached blocks with --dry-run support (#246)
  13. `dfs gc` command exposes orphan block scanning with --dry-run and --verbose (#189)
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

### Phase 47: L1 Read Cache, Disk Tier, and Prefetch
**Goal**: Add tiered read cache (memory + disk) with sequential prefetch for hot blocks
**Depends on**: Phase 46
**Requirements**: PERF-01, PERF-02, PERF-03, PERF-04
**Reference**: GitHub #187
**Success Criteria** (what must be TRUE):
  1. L1 read-through LRU cache (readcache.go) caches hot blocks in memory
  2. Cache invalidation on WriteAt removes stale entries
  3. Sequential prefetch (prefetch.go) triggered after 2+ sequential reads
  4. Prefetch worker pool bounded, non-blocking, avoids cache pollution
  5. Sequential read benchmark shows improved throughput with L1 cache
  6. Disk cache tier: evicted memory blocks demote to SSD, disk reads promote to memory (#187)
  7. Disk cache configurable (path, max_size) with free space monitoring
**Verification**: `go test ./pkg/blockstore/...` && sequential read benchmark
**Plans**: TBD

### Phase 48: Auto-Deduced Configuration and Memory Safety
**Goal**: Derive buffer/cache sizes and concurrency from system resources, prevent OOM
**Depends on**: Phase 47
**Requirements**: AUTO-01, AUTO-02, AUTO-03, AUTO-04, AUTO-05
**Reference**: GitHub #258
**Success Criteria** (what must be TRUE):
  1. WriteBufferMemory defaults to 25% of available memory if not configured
  2. ReadCacheMemory defaults to 12.5% of available memory if not configured
  3. ParallelUploads defaults to max(4, runtime.NumCPU()) if not configured
  4. ParallelDownloads defaults to max(8, runtime.NumCPU()*2) if not configured
  5. User-provided config values override auto-deduced defaults
  6. GOMEMLIMIT set based on configured cache size + headroom to prevent OOM (#258)
  7. Upload storm memory bounded: offloader respects global memory ceiling
**Verification**: `go test ./pkg/config/...` && config validation test
**Plans**: TBD

### Phase 49: Testing and Documentation
**Goal**: Comprehensive E2E testing, cache CLI/API, legacy cleanup, and documentation for block store architecture
**Depends on**: Phase 48
**Requirements**: TEST-01, TEST-02, TEST-03, DOCS-01, DOCS-02, DOCS-03, DOCS-04
**Success Criteria** (what must be TRUE):
  1. E2E test matrix updated to 18-combo 3D matrix (3 metadata x 2 local x 3 remote)
  2. Multi-share E2E test validates isolation with different local paths
  3. Cache-tiers benchmark validates L1 cache performance improvement
  4. docs/ARCHITECTURE.md updated to describe two-tier block store model
  5. docs/CONFIGURATION.md updated for new block store CLI and config schema
  6. CLAUDE.md updated to reflect pkg/blockstore/ structure
  7. --payload flag removed entirely (hard error pointing to --local/--remote)
  8. Zero payload store references remain in the repository
  9. Cache CLI (dfsctl cache stats/evict) and REST API operational
**Verification**: `go build ./...` && `go test -tags=e2e ./test/e2e/...` && documentation review
**Plans**: 5 plans
Plans:
- [ ] 49-01-PLAN.md — Cache CLI/API and backward compatibility clean break
- [ ] 49-02-PLAN.md — Full legacy payload store cleanup across repository
- [ ] 49-03-PLAN.md — 18-combo E2E store matrix and multi-share isolation tests
- [ ] 49-04-PLAN.md — Cache-tiers benchmark workload
- [ ] 49-05-PLAN.md — Documentation full sweep

---

## v4.3 Protocol Gap Fixes

### Phase 49.1: NFSv4 READDIR Cookie Verifier
**Goal**: Implement proper cookie verifier for NFSv4 READDIR instead of zero bytes
**Depends on**: Phase 49 (v4.0 complete)
**Reference**: GitHub #254
**Success Criteria** (what must be TRUE):
  1. NFSv4 READDIR writes meaningful cookie verifier (e.g., directory mtime hash)
  2. Verifier mismatch logged at debug level, entries served (lenient approach per #253)
  3. macOS Finder folder copy works without error -8062
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 49.2: READDIRPLUS Performance Optimization
**Goal**: Avoid redundant GetFile() calls in READDIRPLUS when entry attributes are populated
**Depends on**: Phase 49.1
**Reference**: GitHub #222
**Success Criteria** (what must be TRUE):
  1. READDIRPLUS uses entry.Attr directly when populated, skipping GetFile()
  2. Large directory listing metadata lookups reduced by ~50%
  3. Fallback to GetFile() when entry.Attr is nil
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 49.3: LSA Named Pipe for SID-to-Name Resolution
**Goal**: Implement \pipe\lsarpc for Windows Security tab friendly name display
**Depends on**: Phase 49.1
**Reference**: GitHub #236
**Success Criteria** (what must be TRUE):
  1. LsarOpenPolicy2 opens policy handle
  2. LsarLookupSids2/LsarLookupSids3 resolves SIDs to friendly names
  3. LsarClose closes policy handle
  4. Windows Security tab shows usernames instead of raw SIDs
**Verification**: `go build ./...` && `go test ./...` && Windows manual verification
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

## v4.6 Production Hardening

### Phase 63: SMB3 Signing Fix
**Goal**: macOS clients can mount DittoFS shares over SMB 3.1.1 with signing enabled
**Depends on**: Phase 62 (v4.2 complete)
**Requirements**: PROTO-01
**Reference**: GitHub #252
**Success Criteria** (what must be TRUE):
  1. Preauth integrity hash chain matches between server and macOS mount_smbfs client through NEGOTIATE and SESSION_SETUP exchanges
  2. Signing key derived from corrected preauth hash produces valid AES-CMAC/GMAC signatures accepted by macOS
  3. `mount_smbfs //user@host/share /mnt` succeeds on macOS without signature verification errors
  4. Windows 11 SMB 3.1.1 signing continues to work (no regression)
**Verification**: `go build ./...` && `go test ./...` && macOS mount_smbfs manual test
**Plans**: TBD

### Phase 64: Protocol Correctness and Hot-Reload
**Goal**: NTLM negotiation advertises only implemented capabilities, and shares created at runtime are immediately visible to all adapters
**Depends on**: Phase 63
**Requirements**: PROTO-02, RUNTIME-01
**Reference**: GitHub #215, #235
**Success Criteria** (what must be TRUE):
  1. NTLM CHALLENGE_MESSAGE no longer advertises NTLMSSP_NEGOTIATE_SEAL or NTLMSSP_NEGOTIATE_128/56 flags when encryption is not implemented
  2. Strict NTLM clients (e.g., smbclient with require signing) negotiate sessions without capability mismatch errors
  3. Running `dfsctl share create --name /new-share ...` while the server is running makes the share immediately accessible via NFS mount and SMB tree connect without restarting dfs
  4. Both NFS EXPORT/MNT and SMB tree connect enumerate the newly created share within seconds of creation
**Verification**: `go build ./...` && `go test ./...` && runtime share creation test
**Plans**: TBD

### Phase 65: Storage Observability and Quotas
**Goal**: Operators can see real storage usage, and per-share quotas enforce size limits reported accurately via NFS and SMB
**Depends on**: Phase 64
**Requirements**: OBS-01, OBS-02
**Reference**: GitHub #216, #232
**Success Criteria** (what must be TRUE):
  1. `PayloadService.GetStorageStats()` returns non-zero UsedSize reflecting actual aggregate block sizes from cache and underlying stores
  2. `dfsctl share create --quota 10GB` and `dfsctl share update --quota 50GB` configure per-share quota limits persisted in control plane
  3. Write operations to a share at quota return NFS3ERR_NOSPC / STATUS_DISK_FULL and refuse additional data
  4. NFSv3 FSSTAT/FSINFO and NFSv4 GETATTR report total/free/available space reflecting configured quota and actual usage
  5. SMB QUERY_FS_SIZE_INFO and QUERY_FS_FULL_SIZE_INFO report quota-aware sizes so Windows Explorer shows correct free space
**Verification**: `go build ./...` && `go test ./...` && quota enforcement test with NFS + SMB clients
**Plans**: TBD

### Phase 66: Unified Client Tracking
**Goal**: Operators can see all connected NFS and SMB clients in a single view with connection metadata
**Depends on**: Phase 64
**Requirements**: OPS-01
**Reference**: GitHub #157
**Success Criteria** (what must be TRUE):
  1. Protocol-agnostic `ClientRecord` struct in control plane tracks client IP, protocol, connected-at, last-activity, mount points, and authentication identity
  2. Both NFS and SMB adapters register/deregister clients via a shared `ClientRegistry` interface on connect/disconnect
  3. REST API endpoint `GET /api/v1/clients` returns list of active clients with protocol, IP, and mount details
  4. `dfsctl client list` displays active clients in table format, `dfsctl client list -o json` outputs JSON
  5. Stale client records are cleaned up automatically when connections close or timeout
**Verification**: `go build ./...` && `go test ./...` && manual NFS+SMB mount then `dfsctl client list`
**Plans**: TBD

### Phase 67: Trash and Soft-Delete
**Goal**: Deleted files are recoverable via a per-share trash folder with automatic expiration
**Depends on**: Phase 64
**Requirements**: OPS-02
**Reference**: GitHub #190
**Success Criteria** (what must be TRUE):
  1. File and directory deletion moves items to `.trash/{YYYY-MM-DD}/` within the share instead of permanent removal
  2. Trash retention period is configurable per share (default 7 days) via `dfsctl share create --trash-retention 14d` or `dfsctl share update --trash-retention 30d`
  3. Background goroutine scans trash directories and permanently deletes items older than retention period
  4. Trash is transparent to NFS/SMB clients: `.trash` directory is hidden from READDIR/READDIRPLUS/SMB QUERY_DIRECTORY unless explicitly accessed
  5. `dfsctl share trash list /share-name` shows trashed items with original path and deletion date
**Verification**: `go build ./...` && `go test ./...` && delete file via NFS, verify in .trash, wait for expiry
**Plans**: TBD

---

## v4.7 Offline/Edge Resilience

### Phase 68: Offline Write Queue
**Goal**: Queue writes locally when remote block store is unreachable with WAL-backed crash safety
**Depends on**: Phase 67 (v4.6 complete)
**Requirements**: EDGE-01, EDGE-02
**Success Criteria** (what must be TRUE):
  1. Offloader detects remote store unreachability (connection timeout, DNS failure, HTTP errors)
  2. Writes that would fail due to unreachable remote are queued in a local WAL
  3. Queued writes survive process restart (WAL persistence)
  4. Local reads of queued data return correct content from cache
  5. Queue has configurable size limit with backpressure when full
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 69: Auto-Sync Engine
**Goal**: Detect connectivity restoration and replay queued writes to remote store
**Depends on**: Phase 68
**Requirements**: EDGE-03, EDGE-04
**Success Criteria** (what must be TRUE):
  1. Background health check goroutine periodically probes remote store availability
  2. On connectivity restoration, queued writes are replayed in order to remote store
  3. Sync progress is observable via metrics (queued/synced/failed counters)
  4. Partial sync failures are retried with exponential backoff
  5. Concurrent new writes during sync are handled correctly (no data loss or corruption)
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 70: Cache-Served Offline Reads
**Goal**: Warm cache serves reads when remote store is unreachable, with configurable prefetch
**Depends on**: Phase 69
**Requirements**: EDGE-05, EDGE-06
**Success Criteria** (what must be TRUE):
  1. READ operations for cached blocks succeed even when remote store is unreachable
  2. READ operations for non-cached blocks return appropriate error (NFS3ERR_IO / STATUS_IO_DEVICE_ERROR)
  3. Configurable prefetch policy allows pre-warming cache for critical files/directories
  4. Cache eviction respects offline mode (no eviction of dirty or queued blocks when remote is down)
  5. Status API/CLI reports cache coverage percentage per share
**Verification**: `go build ./...` && `go test ./...`
**Plans**: TBD

### Phase 71: Edge Integration Testing
**Goal**: E2E tests for edge deployment scenarios with network partition simulation
**Depends on**: Phase 70
**Requirements**: EDGE-07
**Success Criteria** (what must be TRUE):
  1. E2E test: write files while remote store is unreachable, verify local reads succeed
  2. E2E test: restore connectivity, verify queued writes appear in remote store
  3. E2E test: warm cache serves reads after second disconnection
  4. E2E test: process restart during offline mode preserves queued writes
  5. Documentation covers edge deployment architecture and configuration
**Verification**: `go test -tags=e2e ./test/e2e/...` && documentation review
**Plans**: TBD

---

## v4.8 DX/UX Improvements

### Phase 72: Build & CI Optimization
**Goal**: Streamline build system with Makefile targets and optimize NFS CI pipeline
**Depends on**: Phase 71 (v4.7 complete)
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

### Phase 73: Adapter Config API
**Goal**: API support for netgroup-share association and adapter configuration management
**Depends on**: Phase 72
**Requirements**: DX-03
**Reference**: GitHub #220
**Success Criteria** (what must be TRUE):
  1. REST API endpoints for managing netgroup-share associations
  2. `dfsctl adapter config` commands for viewing and modifying adapter settings
  3. Netgroup-share mappings persisted in control plane store
  4. NFS adapter respects netgroup-share associations for export access control
**Verification**: `go build ./...` && `go test ./...` && manual CLI verification
**Plans**: TBD

### Phase 74: Developer Tooling Polish
**Goal**: Improve developer experience with documentation and workflow enhancements
**Depends on**: Phase 72
**Requirements**: DX-04
**Success Criteria** (what must be TRUE):
  1. Contributing guide updated with Makefile-based workflow
  2. Development environment setup documented with prerequisites check script
  3. Common development tasks automated (lint-fix, generate, clean)
  4. README quickstart reflects simplified build/test workflow
**Verification**: Documentation review && clean checkout build test
**Plans**: TBD

---

## v4.9 SMB Protocol Fixes

### Phase 75: SMB Credits & IOCTL
**Goal**: Fix SMB credit granting algorithm and complete IOCTL handler coverage
**Depends on**: Phase 74 (v4.8 complete)
**Requirements**: SMB-01, SMB-02
**Reference**: GitHub #268
**Success Criteria** (what must be TRUE):
  1. Credit granting follows MS-SMB2 algorithm (dynamic based on client behavior)
  2. IOCTL handlers implemented for all required function codes per smbtorture
  3. smbtorture credit-related test failures pass
  4. smbtorture IOCTL-related test failures pass
**Verification**: `go build ./...` && `go test ./...` && smbtorture credit/IOCTL suites
**Plans**: TBD

### Phase 76: SMB Multichannel & Timestamps
**Goal**: Add multichannel connection support and fix timestamp precision
**Depends on**: Phase 75
**Requirements**: SMB-03, SMB-04
**Reference**: GitHub #268
**Success Criteria** (what must be TRUE):
  1. SMB multichannel: clients can establish multiple TCP connections to same session
  2. Channel binding validates connection identity
  3. Timestamp precision matches NTFS resolution (100ns intervals)
  4. smbtorture multichannel and timestamp test failures pass
**Verification**: `go build ./...` && `go test ./...` && smbtorture multichannel/timestamp suites
**Plans**: TBD

### Phase 77: SMB Conformance Iteration
**Goal**: Address remaining smbtorture failures for comprehensive SMB protocol conformance
**Depends on**: Phase 76
**Requirements**: SMB-05
**Reference**: GitHub #268
**Success Criteria** (what must be TRUE):
  1. All 13 originally identified smbtorture failures resolved or documented as not-applicable
  2. smbtorture pass rate improved to 90%+ of applicable tests
  3. Known failures documented with rationale (intentional deviations vs. future work)
  4. Regression test suite prevents future conformance regressions
**Verification**: `go build ./...` && `go test ./...` && full smbtorture run
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
<summary>v4.2 Benchmarking & Performance (Phases 57-62) — SHIPPED 2026-03-04</summary>

Full phase details archived. 6 phases: benchmark infrastructure, fio workloads, competitor setup, orchestrator scripts, analysis pipeline, profiling integration.

</details>

---

## Progress

**Execution Order:**
v3.8 (33-40.5) -> v4.2 (57-62) -> v4.0 (41-49) -> v4.6 (63-67) -> v4.7 (68-71) -> v4.8 (72-74) -> v4.9 (75-77) -> v4.3 (49.1-49.3) -> v4.5 (49.4-49.5) -> v5.0 (50-56)

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1-5.5 | v1.0 | Complete | Complete | 2026-02-07 |
| 6-15.5 | v2.0 | Complete | Complete | 2026-02-20 |
| 16-25.5 | v3.0 | Complete | Complete | 2026-02-25 |
| 26-29.5 | v3.5 | Complete | Complete | 2026-02-26 |
| 29.8-32.5 | v3.6 | Complete | Complete | 2026-02-28 |
| 33-40.5 | v3.8 | Complete | Complete | 2026-03-04 |
| 57-62 | v4.2 | Complete | Complete | 2026-03-04 |
| 41. Block State Enum and ListFileBlocks | v4.0 | 2/2 | Complete | 2026-03-09 |
| 42. Legacy Cleanup | v4.0 | 0/? | Not started | - |
| 43. Local-Only Block Management | v4.0 | 0/? | Not started | - |
| 44. Data Model and API/CLI | v4.0 | 0/? | Not started | - |
| 45. Package Restructure | v4.0 | 0/? | Not started | - |
| 46. Per-Share Block Store Wiring | v4.0 | 0/? | Not started | - |
| 47. L1 Read Cache, Disk Tier, Prefetch | v4.0 | 0/? | Not started | - |
| 48. Auto-Deduced Config + Memory Safety | v4.0 | 0/? | Not started | - |
| 49. Testing and Documentation | v4.0 | 0/5 | Not started | - |
| 63. SMB3 Signing Fix | v4.6 | 0/? | Not started | - |
| 64. Protocol Correctness and Hot-Reload | v4.6 | 0/? | Not started | - |
| 65. Storage Observability and Quotas | v4.6 | 0/? | Not started | - |
| 66. Unified Client Tracking | v4.6 | 0/? | Not started | - |
| 67. Trash and Soft-Delete | v4.6 | 0/? | Not started | - |
| 68. Offline Write Queue | v4.7 | 0/? | Not started | - |
| 69. Auto-Sync Engine | v4.7 | 0/? | Not started | - |
| 70. Cache-Served Offline Reads | v4.7 | 0/? | Not started | - |
| 71. Edge Integration Testing | v4.7 | 0/? | Not started | - |
| 72. Build & CI Optimization | v4.8 | 0/? | Not started | - |
| 73. Adapter Config API | v4.8 | 0/? | Not started | - |
| 74. Developer Tooling Polish | v4.8 | 0/? | Not started | - |
| 75. SMB Credits & IOCTL | v4.9 | 0/? | Not started | - |
| 76. SMB Multichannel & Timestamps | v4.9 | 0/? | Not started | - |
| 77. SMB Conformance Iteration | v4.9 | 0/? | Not started | - |
| 49.1 NFSv4 READDIR Cookie Verifier | v4.3 | 0/? | Not started | - |
| 49.2 READDIRPLUS Performance | v4.3 | 0/? | Not started | - |
| 49.3 LSA Named Pipe | v4.3 | 0/? | Not started | - |
| 49.4 Block-Level Compression | v4.5 | 0/? | Not started | - |
| 49.5 Client-Side Encryption | v4.5 | 0/? | Not started | - |
| 50. Server-Side Copy | v5.0 | 0/? | Not started | - |
| 51. Clone/Reflinks | v5.0 | 0/? | Not started | - |
| 52. Sparse Files | v5.0 | 0/? | Not started | - |
| 53. Extended Attributes | v5.0 | 0/? | Not started | - |
| 54. NFSv4.2 Operations | v5.0 | 0/? | Not started | - |
| 55. Documentation | v5.0 | 0/? | Not started | - |
| 56. v5.0 Testing | v5.0 | 0/? | Not started | - |

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
*v3.5 shipped: 2026-02-26*
*v3.6 shipped: 2026-02-28*
*v3.8 shipped: 2026-03-04*
*v4.2 shipped: 2026-03-04*
*v4.0 roadmap created: 2026-03-09*
*v4.6 roadmap created: 2026-03-09*
*v4.7 roadmap created: 2026-03-09*
*v4.8 roadmap created: 2026-03-09*
*v4.9 roadmap created: 2026-03-09*
