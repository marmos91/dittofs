# Requirements: DittoFS BlockStore Unification Refactor

**Defined:** 2026-03-09
**Core Value:** Replace confusing layered storage architecture with clean two-tier block store model (Local + Remote) for per-share isolation and maintainability

## v4.0 Requirements

### Block State Model

- [x] **STATE-01**: Block state enum uses new names: Dirty(0), Local(1), Uploading(2), Remote(3)
- [x] **STATE-02**: All consumers updated for renamed states (Sealed->Local, Uploaded->Remote)
- [x] **STATE-03**: ListPendingUpload renamed to ListLocalBlocks across interface and implementations
- [x] **STATE-04**: ListEvictable renamed to ListRemoteBlocks across interface and implementations
- [x] **STATE-05**: ListFileBlocks(ctx, payloadID) method added to FileBlockStore interface and all implementations
- [x] **STATE-06**: BadgerDB secondary index updated from fb-sealed: to fb-local: prefix

### Legacy Cleanup

- [x] **CLEAN-01**: DirectWriteStore interface removed from pkg/payload/store/store.go
- [x] **CLEAN-02**: pkg/payload/store/fs/ entirely deleted
- [x] **CLEAN-03**: directWritePath, SetDirectWritePath, IsDirectWrite removed from cache
- [x] **CLEAN-04**: IsDirectWrite checks removed from offloader
- [x] **CLEAN-05**: blockfs import and DirectWriteStore detection removed from init.go
- [x] **CLEAN-06**: All direct-write branches removed from cache write.go, read.go, flush.go

### Local-Only Mode

- [x] **LOCAL-01**: pkg/cache/manage.go provides DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk, SetEvictionEnabled
- [x] **LOCAL-02**: Offloader accepts nil blockStore and operates in local-only mode
- [x] **LOCAL-03**: Local-only flush marks blocks BlockStateLocal (no upload)
- [x] **LOCAL-04**: init.go wires local-only mode when no remote store configured

### Data Model

- [x] **MODEL-01**: BlockStoreConfig model with ID, Name, Kind (local/remote), Type, Config, CreatedAt
- [x] **MODEL-02**: Share model updated with LocalBlockStoreID (mandatory) + RemoteBlockStoreID (nullable)
- [x] **MODEL-03**: Migration renames payload_store_configs -> block_store_configs with kind column
- [x] **MODEL-04**: Migration splits Share.PayloadStoreID into LocalBlockStoreID + RemoteBlockStoreID
- [x] **MODEL-05**: BlockStoreConfigStore interface with CRUD filtered by kind replaces PayloadStoreConfigStore

### API & CLI

- [x] **API-01**: REST endpoints for local block store CRUD (/api/v1/store/block/local)
- [x] **API-02**: REST endpoints for remote block store CRUD (/api/v1/store/block/remote)
- [x] **API-03**: Share endpoints accept --local (required) and --remote (optional)
- [x] **CLI-01**: `dfsctl store block local add/list/edit/remove` commands
- [x] **CLI-02**: `dfsctl store block remote add/list/edit/remove` commands
- [x] **CLI-03**: `dfsctl share create --local X --remote Y` replacing --payload
- [x] **CLI-04**: API client methods for block store operations replacing payload store methods

### Package Architecture

- [x] **PKG-01**: pkg/blockstore/local/local.go defines LocalStore interface
- [x] **PKG-02**: pkg/blockstore/remote/remote.go defines RemoteStore interface
- [x] **PKG-03**: pkg/cache/ moved to pkg/blockstore/local/fs/
- [x] **PKG-04**: pkg/blockstore/local/memory/ created for test MemoryLocalStore
- [x] **PKG-05**: pkg/payload/store/s3/ moved to pkg/blockstore/remote/s3/
- [x] **PKG-06**: pkg/payload/store/memory/ moved to pkg/blockstore/remote/memory/
- [x] **PKG-07**: pkg/payload/offloader/ moved to pkg/blockstore/offloader/
- [x] **PKG-08**: pkg/payload/gc/ moved to pkg/blockstore/gc/
- [x] **PKG-09**: pkg/blockstore/blockstore.go (BlockStore orchestrator) absorbs PayloadService
- [x] **PKG-10**: All consumer imports updated (~18 files: NFS handlers, SMB handlers, runtime, shares)
- [x] **PKG-11**: pkg/cache/ and pkg/payload/ deleted after migration

### Per-Share Isolation

- [x] **SHARE-01**: Runtime manages per-share BlockStore instances (map[shareID]*BlockStore) replacing global PayloadService
- [x] **SHARE-02**: EnsureBlockStore(share) creates BlockStore with share's local + remote configs
- [x] **SHARE-03**: NFS/SMB handlers resolve BlockStore per share handle (getBlockStore(shareHandle))
- [x] **SHARE-04**: Multiple shares with different local paths operate in isolation

### Read Performance

- [x] **PERF-01**: L1 read-through LRU cache (readcache.go) for hot blocks
- [x] **PERF-02**: L1 cache invalidation on WriteAt
- [x] **PERF-03**: Sequential prefetch (prefetch.go) after 2+ sequential reads
- [x] **PERF-04**: Bounded prefetch worker pool, non-blocking

### Auto-Configuration

- [x] **AUTO-01**: WriteBufferMemory derived from 25% of available memory
- [x] **AUTO-02**: ReadCacheMemory derived from 12.5% of available memory
- [x] **AUTO-03**: ParallelUploads derived from max(4, cpus)
- [x] **AUTO-04**: ParallelDownloads derived from max(8, cpus*2)
- [x] **AUTO-05**: User config overrides auto-deduced defaults

### Testing & Documentation

- [x] **TEST-01**: E2E store matrix updated for new CLI (block local/remote)
- [x] **TEST-02**: Multi-share test with different local paths
- [x] **TEST-03**: Sequential read benchmark validates L1 cache
- [x] **DOCS-01**: ARCHITECTURE.md updated for block store model
- [x] **DOCS-02**: CONFIGURATION.md updated for new CLI and config
- [x] **DOCS-03**: CLAUDE.md updated for new package structure
- [x] **DOCS-04**: --payload flag backward compat with deprecation warning

## v4.6 Requirements -- Production Hardening

### Protocol Correctness

- [ ] **PROTO-01**: SMB 3.1.1 signing works on macOS -- preauth integrity hash chain produces matching signing keys so macOS mount_smbfs accepts sessions (#252)
- [ ] **PROTO-02**: NTLM CHALLENGE_MESSAGE only advertises encryption flags (Flag128/Flag56) when encryption is actually implemented, or flags are removed to prevent interop issues with strict clients (#215)

### Runtime Robustness

- [ ] **RUNTIME-01**: Shares created via `dfsctl share create` after adapter startup are immediately visible to all protocol adapters (NFS and SMB) without requiring a server restart (#235)

### Observability

- [ ] **OBS-01**: `PayloadService.GetStorageStats()` returns actual UsedSize by aggregating block sizes from cache and underlying stores, enabling capacity planning (#216)
- [ ] **OBS-02**: Per-share quota configuration (`dfsctl share create --quota 10GB`, `dfsctl share update --quota 50GB`) with enforcement on writes and accurate FSSTAT/FSINFO/SMB filesystem size reporting (#232)

### Operational Features

- [ ] **OPS-01**: Protocol-agnostic `ClientRecord` in control plane tracking all active NFS and SMB connections, with REST API endpoint and `dfsctl client list [-o json|table]` command (#157)
- [ ] **OPS-02**: Trash/soft-delete where deleted files move to `.trash/{date}/` per share, with configurable retention period (default 7 days) and background cleanup goroutine (#190)

## Future Requirements

### v4.3 -- Protocol Gap Fixes
- **GAP-01**: NFSv4 READDIR proper cookie verifier (#254)
- **GAP-02**: READDIRPLUS skip redundant GetFile() (#222)
- **GAP-03**: LSA named pipe for SID-to-name resolution (#236)

### v4.5 -- BlockStore Security
- **SEC-01**: Block-level LZ4/Zstd compression (#185)
- **SEC-02**: Client-side AES-256-GCM encryption (#186)

### v4.7 -- Offline/Edge Resilience
- **EDGE-01**: Offloader detects remote store unreachability and queues writes locally
- **EDGE-02**: Queued writes persisted in WAL for crash safety with configurable size limit
- **EDGE-03**: Background health check probes remote store, replays queued writes on restoration
- **EDGE-04**: Sync progress observable via metrics, partial failures retried with backoff
- **EDGE-05**: Cached blocks served for reads when remote store unreachable
- **EDGE-06**: Cache eviction disabled for dirty/queued blocks during offline mode
- **EDGE-07**: E2E tests for offline write, reconnect sync, cache-only reads, process restart

### v4.8 -- DX/UX Improvements
- **DX-01**: Makefile with targets for unit, integration, E2E, lint, build, fmt (#206)
- **DX-02**: NFS CI scoped path triggers + tiered test matrix (#207)
- **DX-03**: Adapter config API for netgroup-share association (#220)
- **DX-04**: Updated contributing guide, prerequisites check, automated dev tasks

### v4.9 -- SMB Protocol Fixes
- **SMB-01**: Credit granting algorithm conformance per MS-SMB2 (#268)
- **SMB-02**: IOCTL handler completeness for required function codes (#268)
- **SMB-03**: Multichannel connection support with channel binding (#268)
- **SMB-04**: Timestamp precision matching NTFS 100ns intervals (#268)
- **SMB-05**: Remaining smbtorture failures resolved, 90%+ pass rate (#268)

### v5.0 -- NFSv4.2
- **NFS42-01**: Server-side COPY with async OFFLOAD_STATUS polling
- **NFS42-02**: CLONE/reflinks via content-addressed storage
- **NFS42-03**: Sparse files: SEEK, ALLOCATE, DEALLOCATE
- **NFS42-04**: Extended attributes in metadata layer
- **NFS42-05**: Application I/O hints (IO_ADVISE)

## Out of Scope

| Feature | Reason |
|---------|--------|
| Distributed block stores | Single-node focus, multi-node deferred |
| Block-level encryption at rest | S3 provides SSE, local FS uses OS-level encryption |
| Block dedup across shares | Current dedup is per-share, cross-share dedup adds complexity |
| Custom block sizes per share | 8MB block size is fixed, tuning deferred |
| Tiered storage policies | Auto-eviction by LRU is sufficient for v4.0 |
| Cross-protocol oplock wiring (#213) | Already implemented in Phase 30 (v3.6) |
| Embedded portmapper (#119) | Already implemented |
| NFSv4.1 session limits wiring (#217) | Already implemented in settings.go |
| Per-user quotas | Future extension of per-share quotas |
| Webhook system (#117) | Deferred to future milestone |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| STATE-01 | Phase 41 | Complete |
| STATE-02 | Phase 41 | Complete |
| STATE-03 | Phase 41 | Complete |
| STATE-04 | Phase 41 | Complete |
| STATE-05 | Phase 41 | Complete |
| STATE-06 | Phase 41 | Complete |
| CLEAN-01 | Phase 42 | Complete |
| CLEAN-02 | Phase 42 | Complete |
| CLEAN-03 | Phase 42 | Complete |
| CLEAN-04 | Phase 42 | Complete |
| CLEAN-05 | Phase 42 | Complete |
| CLEAN-06 | Phase 42 | Complete |
| LOCAL-01 | Phase 43 | Complete |
| LOCAL-02 | Phase 43 | Complete |
| LOCAL-03 | Phase 43 | Complete |
| LOCAL-04 | Phase 43 | Complete |
| MODEL-01 | Phase 44 | Complete |
| MODEL-02 | Phase 44 | Complete |
| MODEL-03 | Phase 44 | Complete |
| MODEL-04 | Phase 44 | Complete |
| MODEL-05 | Phase 44 | Complete |
| API-01 | Phase 44 | Complete |
| API-02 | Phase 44 | Complete |
| API-03 | Phase 44 | Complete |
| CLI-01 | Phase 44 | Complete |
| CLI-02 | Phase 44 | Complete |
| CLI-03 | Phase 44 | Complete |
| CLI-04 | Phase 44 | Complete |
| PKG-01 | Phase 45 | Complete |
| PKG-02 | Phase 45 | Complete |
| PKG-03 | Phase 45 | Complete |
| PKG-04 | Phase 45 | Complete |
| PKG-05 | Phase 45 | Complete |
| PKG-06 | Phase 45 | Complete |
| PKG-07 | Phase 45 | Complete |
| PKG-08 | Phase 45 | Complete |
| PKG-09 | Phase 45 | Complete |
| PKG-10 | Phase 45 | Complete |
| PKG-11 | Phase 45 | Complete |
| SHARE-01 | Phase 46 | Complete |
| SHARE-02 | Phase 46 | Complete |
| SHARE-03 | Phase 46 | Complete |
| SHARE-04 | Phase 46 | Complete |
| PERF-01 | Phase 47 | Complete |
| PERF-02 | Phase 47 | Complete |
| PERF-03 | Phase 47 | Complete |
| PERF-04 | Phase 47 | Complete |
| AUTO-01 | Phase 48 | Complete |
| AUTO-02 | Phase 48 | Complete |
| AUTO-03 | Phase 48 | Complete |
| AUTO-04 | Phase 48 | Complete |
| AUTO-05 | Phase 48 | Complete |
| TEST-01 | Phase 49 | Complete |
| TEST-02 | Phase 49 | Complete |
| TEST-03 | Phase 49 | Complete |
| DOCS-01 | Phase 49 | Complete |
| DOCS-02 | Phase 49 | Complete |
| DOCS-03 | Phase 49 | Complete |
| DOCS-04 | Phase 49 | Complete |
| PROTO-01 | Phase 63 | Pending |
| PROTO-02 | Phase 64 | Pending |
| RUNTIME-01 | Phase 64 | Pending |
| OBS-01 | Phase 65 | Pending |
| OBS-02 | Phase 65 | Pending |
| OPS-01 | Phase 66 | Pending |
| OPS-02 | Phase 67 | Pending |

**Coverage:**
- v4.0 requirements: 55 total -> Mapped: 55 -> Unmapped: 0
- v4.6 requirements: 7 total -> Mapped: 7 -> Unmapped: 0

---
*Requirements defined: 2026-03-09*
*Last updated: 2026-03-09 after v4.7/v4.8/v4.9 milestones created*
