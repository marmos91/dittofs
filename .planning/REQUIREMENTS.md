# Requirements: DittoFS Offline/Edge Resilience

**Defined:** 2026-03-13
**Core Value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility — including disconnected edge deployments

## v4.7 Requirements

Requirements for offline/edge resilience. Each maps to roadmap phases.

### Local Block Retention & Eviction

- [x] **CACHE-01**: Share config includes `retention_policy` field with modes: `pin` (never evict), `ttl` (time-based), `lru` (current default)
- [x] **CACHE-02**: Pin mode excludes blocks from eviction candidates regardless of disk pressure
- [x] **CACHE-03**: TTL mode adds configurable `retention_ttl` — blocks become eviction-eligible only after TTL expires since last access
- [x] **CACHE-04**: Control plane REST API exposes retention settings on share CRUD endpoints
- [x] **CACHE-05**: `dfsctl share create/update` CLI supports `--retention` and `--retention-ttl` flags
- [x] **CACHE-06**: Existing shares default to `lru` mode (backward compatible)

### Offline Resilience

- [ ] **RESIL-01**: Read path serves locally cached blocks when S3 is unreachable (graceful degradation)
- [ ] **RESIL-02**: Read path returns clear error for blocks only in S3 when unreachable (not generic I/O error)
- [ ] **RESIL-03**: Write path accepts writes to local store when S3 is unreachable
- [ ] **RESIL-04**: Eviction is suspended when remote store health check fails (don't evict blocks that can't be re-downloaded)
- [ ] **RESIL-05**: Periodic S3 health check detects connectivity loss and restoration
- [ ] **RESIL-06**: Syncer pauses upload attempts during connectivity loss (exponential backoff)
- [ ] **RESIL-07**: Syncer auto-resumes uploads when connectivity returns
- [ ] **RESIL-08**: Queued blocks drain in upload order (oldest first) on reconnect

### Test Infrastructure

- [ ] **INFRA-01**: Pulumi stack deploys Scaleway VM with DittoFS (BadgerDB + local FS + S3)
- [ ] **INFRA-02**: Test script uploads files, waits, verifies reads persist after delay
- [ ] **INFRA-03**: Test script simulates S3 disconnection via iptables and verifies offline reads/writes
- [ ] **INFRA-04**: Test script verifies auto-sync on reconnect (blocks uploaded after S3 returns)

## Future Requirements

### v4.5 — BlockStore Security
- **BSEC-01**: Block-level compression (gzip/lz4/zstd) before upload to remote store
- **BSEC-02**: Block-level encryption (AES-256-GCM) before upload to remote store

### v4.6 — Production Hardening
- **PROD-01**: SMB 3.1.1 signing on macOS fix
- **PROD-02**: Share hot-reload without restart
- **PROD-03**: Per-share quotas with FSSTAT/FSINFO/SMB reporting
- **PROD-04**: Protocol-agnostic client tracking

## Out of Scope

| Feature | Reason |
|---------|--------|
| Conflict resolution for concurrent offline edits | Single-node architecture, no multi-master |
| Partial block sync (delta uploads) | Full block upload sufficient for v4.7 |
| Offline metadata operations (create/delete) | Metadata is in BadgerDB locally, already works offline |
| Multi-site replication | Single-node focus, S3 replication handles this |
| Automatic failover to alternate remote stores | Single remote store per share is sufficient |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| CACHE-01 | Phase 63 | Complete |
| CACHE-02 | Phase 63 | Complete |
| CACHE-03 | Phase 63 | Complete |
| CACHE-04 | Phase 63 | Complete |
| CACHE-05 | Phase 63 | Complete |
| CACHE-06 | Phase 63 | Complete |
| RESIL-01 | Phase 65 | Pending |
| RESIL-02 | Phase 65 | Pending |
| RESIL-03 | Phase 65 | Pending |
| RESIL-04 | Phase 64 | Pending |
| RESIL-05 | Phase 64 | Pending |
| RESIL-06 | Phase 64 | Pending |
| RESIL-07 | Phase 64 | Pending |
| RESIL-08 | Phase 64 | Pending |
| INFRA-01 | Phase 66 | Pending |
| INFRA-02 | Phase 66 | Pending |
| INFRA-03 | Phase 66 | Pending |
| INFRA-04 | Phase 66 | Pending |

**Coverage:**
- v4.7 requirements: 18 total
- Mapped to phases: 18
- Unmapped: 0

---
*Requirements defined: 2026-03-13*
*Last updated: 2026-03-13 after roadmap creation*
