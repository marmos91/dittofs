# Requirements: DittoFS v0.10.0

**Defined:** 2026-03-20
**Core Value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility

## v0.10.0 Requirements

Requirements for Production Hardening + SMB Protocol Fixes. Each maps to roadmap phases.

### SMB Protocol Compliance

- [ ] **SMB-01**: SMB 3.1.1 signing works correctly on macOS (fix preauth integrity hash mismatch #252)
- [x] **SMB-02**: Server enforces credit charge validation before dispatching requests (reject insufficient credits)
- [x] **SMB-03**: Server grants credits in every response, never reducing client credits to zero
- [x] **SMB-04**: Multi-credit I/O operations (READ/WRITE > 64KB) validate CreditCharge = ceil(length/65536)
- [ ] **SMB-05**: Compound requests handle credit accounting correctly (charge at compound level, grant in last response)

### SMB Multi-Channel

- [ ] **MCH-01**: Server advertises SMB2_GLOBAL_CAP_MULTI_CHANNEL in NEGOTIATE when enabled
- [ ] **MCH-02**: SESSION_SETUP with SMB2_SESSION_FLAG_BINDING binds new connection to existing session
- [ ] **MCH-03**: Each channel derives its own signing key from the connection's preauth integrity hash
- [ ] **MCH-04**: Lease break notifications fan out across all channels for a session
- [ ] **MCH-05**: Connection cleanup checks session refcount before destroying session state
- [ ] **MCH-06**: Multi-channel is gated behind configuration flag (default: disabled)

### WPTS Conformance

- [ ] **WPTS-01**: ChangeNotify dispatches async notifications on file create/remove/rename/setattr (~20 tests)
- [ ] **WPTS-02**: Negotiate/encryption edge cases fixed (preauth hash fixes cascade, ~5 tests)
- [ ] **WPTS-03**: Leasing and durable handle reconnect edge cases resolved (~4-6 tests)
- [ ] **WPTS-04**: Known failure count reduced from 73 to ~40-45

### Share Quotas

- [ ] **QUOTA-01**: Per-share byte quota configurable via REST API and dfsctl
- [ ] **QUOTA-02**: Write operations rejected with NFS3ERR_NOSPC / STATUS_DISK_FULL when quota exceeded
- [ ] **QUOTA-03**: NFS FSSTAT returns quota-adjusted TotalBytes and AvailableBytes
- [ ] **QUOTA-04**: SMB FileFsSizeInformation and FileFsFullSizeInformation return quota-adjusted values
- [ ] **QUOTA-05**: `dfsctl share create/update --quota-bytes` manages quotas

### Payload Stats

- [ ] **STATS-01**: UsedSize returns actual block storage consumption (not just metadata file sizes)
- [ ] **STATS-02**: Per-share storage usage available via REST API and CLI
- [ ] **STATS-03**: Logical size (file sizes) and physical size (block storage) distinguished

### Client Tracking

- [ ] **CLIENT-01**: Protocol-agnostic ClientRecord aggregates NFS mounts and SMB sessions
- [ ] **CLIENT-02**: `dfsctl client list` shows connected clients with protocol, share, user, connection time
- [ ] **CLIENT-03**: REST API endpoint `GET /api/clients` returns client records
- [ ] **CLIENT-04**: Stale client records expire via TTL-based cleanup

### Trash / Soft-Delete

- [ ] **TRASH-01**: Per-share trash enabled/disabled via configuration
- [ ] **TRASH-02**: Deleted files moved to hidden `.dfs-trash/` directory instead of permanent deletion
- [ ] **TRASH-03**: Trash items invisible in NFS READDIR and SMB QueryDirectory
- [ ] **TRASH-04**: Background scavenger purges expired trash items based on configurable retention
- [ ] **TRASH-05**: `dfsctl trash list/restore/purge` commands for admin management
- [ ] **TRASH-06**: Trash-aware block GC (trashed files count as live references)
- [ ] **TRASH-07**: Trash counts against share quota

## Future Requirements

### BlockStore Security
- **SEC-01**: Block-level compression (zstd/lz4)
- **SEC-02**: Block-level encryption (AES-256-GCM)

### NFSv4.2
- **NFS42-01**: Server-side COPY with OFFLOAD_STATUS polling
- **NFS42-02**: Extended attributes via NFSv4.2 and SMB

## Out of Scope

| Feature | Reason |
|---------|--------|
| Per-user/group quotas | Massively complex, AUTH_UNIX is spoofable, share quotas cover 90% of use cases |
| SMB compression (LZ77/LZNT1) | Large effort for marginal benefit; focus compression on BlockStore layer |
| SMB persistent handles | Requires handle serialization to disk; only useful for continuous availability clustering |
| RQUOTA protocol | Separate RPC program; quotas reported via FSSTAT/FSINFO instead |
| DFS referrals | Windows-specific namespace feature outside DittoFS scope |
| Client-side recycle bin ($RECYCLE.BIN) | Server-side trash is protocol-agnostic and simpler |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| SMB-01 | Phase 69 | Pending |
| SMB-02 | Phase 69 | Complete |
| SMB-03 | Phase 69 | Complete |
| SMB-04 | Phase 69 | Complete |
| SMB-05 | Phase 69 | Pending |
| MCH-01 | Phase 74 | Pending |
| MCH-02 | Phase 74 | Pending |
| MCH-03 | Phase 74 | Pending |
| MCH-04 | Phase 74 | Pending |
| MCH-05 | Phase 74 | Pending |
| MCH-06 | Phase 74 | Pending |
| WPTS-01 | Phase 72 | Pending |
| WPTS-02 | Phase 72 | Pending |
| WPTS-03 | Phase 72 | Pending |
| WPTS-04 | Phase 72 | Pending |
| QUOTA-01 | Phase 70 | Pending |
| QUOTA-02 | Phase 70 | Pending |
| QUOTA-03 | Phase 70 | Pending |
| QUOTA-04 | Phase 70 | Pending |
| QUOTA-05 | Phase 70 | Pending |
| STATS-01 | Phase 70 | Pending |
| STATS-02 | Phase 70 | Pending |
| STATS-03 | Phase 70 | Pending |
| CLIENT-01 | Phase 71 | Pending |
| CLIENT-02 | Phase 71 | Pending |
| CLIENT-03 | Phase 71 | Pending |
| CLIENT-04 | Phase 71 | Pending |
| TRASH-01 | Phase 73 | Pending |
| TRASH-02 | Phase 73 | Pending |
| TRASH-03 | Phase 73 | Pending |
| TRASH-04 | Phase 73 | Pending |
| TRASH-05 | Phase 73 | Pending |
| TRASH-06 | Phase 73 | Pending |
| TRASH-07 | Phase 73 | Pending |

**Coverage:**
- v0.10.0 requirements: 34 total
- Mapped to phases: 34
- Unmapped: 0

---
*Requirements defined: 2026-03-20*
*Last updated: 2026-03-20 after roadmap creation (phases 69-75)*
