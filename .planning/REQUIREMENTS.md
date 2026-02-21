# Requirements: DittoFS NFS Protocol Evolution

**Defined:** 2026-02-20
**Core Value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and immediate cross-protocol visibility

## v3.0 Requirements

Requirements for NFSv4.1 Sessions milestone. Each maps to roadmap phases.

### Session Infrastructure

- [x] **SESS-01**: Server handles EXCHANGE_ID to register v4.1 clients with owner/implementation ID tracking
- [x] **SESS-02**: Server handles CREATE_SESSION to establish sessions with negotiated channel attributes and slot tables
- [x] **SESS-03**: Server handles DESTROY_SESSION to tear down sessions and release slot table memory
- [x] **SESS-04**: Server handles SEQUENCE as first operation in every v4.1 COMPOUND with slot validation and lease renewal
- [x] **SESS-05**: NFSv4.1 constants, types, and XDR structures defined for all new operations (ops 40-58, CB ops 5-14)

### Exactly-Once Semantics

- [x] **EOS-01**: Slot table caches full COMPOUND response for replay detection on duplicate requests
- [x] **EOS-02**: Sequence ID validation detects retries, misordered requests, and stale slots
- [x] **EOS-03**: Server supports dynamic slot count adjustment via target_highest_slotid in SEQUENCE response

### Backchannel

- [ ] **BACK-01**: Server sends callbacks via CB_SEQUENCE over the client's existing TCP connection (no separate dial)
- [ ] **BACK-02**: Server handles BIND_CONN_TO_SESSION to associate connections with sessions for fore/back/both directions
- [ ] **BACK-03**: Server handles BACKCHANNEL_CTL to update backchannel security and attributes
- [ ] **BACK-04**: Existing CB_RECALL works over backchannel for v4.1 clients (fallback to separate TCP for v4.0)

### Client Lifecycle

- [ ] **LIFE-01**: Server handles DESTROY_CLIENTID for graceful client cleanup (all sessions destroyed first)
- [ ] **LIFE-02**: Server handles RECLAIM_COMPLETE to signal end of grace period reclaim for a client
- [ ] **LIFE-03**: Server handles FREE_STATEID to release individual stateids
- [ ] **LIFE-04**: Server handles TEST_STATEID to batch-validate stateid liveness
- [ ] **LIFE-05**: v4.0-only operations (SETCLIENTID, SETCLIENTID_CONFIRM, RENEW, OPEN_CONFIRM, RELEASE_LOCKOWNER) return NFS4ERR_NOTSUPP for minorversion=1

### Directory Delegations

- [ ] **DDELEG-01**: Server handles GET_DIR_DELEGATION to grant directory delegations with notification bitmask
- [ ] **DDELEG-02**: Server sends CB_NOTIFY when directory entries change (add/remove/rename/attr change)
- [ ] **DDELEG-03**: Directory delegation state tracked in StateManager with recall and revocation support

### Trunking

- [ ] **TRUNK-01**: Multiple connections can be bound to a single session via BIND_CONN_TO_SESSION
- [x] **TRUNK-02**: Server reports consistent server_owner in EXCHANGE_ID for trunking detection

### Coexistence

- [x] **COEX-01**: COMPOUND dispatcher routes minorversion=0 to existing v4.0 path and minorversion=1 to v4.1 path
- [x] **COEX-02**: v4.0 clients continue working unchanged when v4.1 is enabled
- [x] **COEX-03**: Per-owner seqid validation bypassed for v4.1 operations (slot table provides replay protection)

### SMB Kerberos

- [ ] **SMBKRB-01**: SMB adapter authenticates via SPNEGO/Kerberos using the shared Kerberos layer from v2.0
- [ ] **SMBKRB-02**: Kerberos principal maps to control plane identity for SMB sessions (same identity mapping as NFS)

### Testing

- [ ] **TEST-01**: E2E tests with Linux NFS client using vers=4.1 mount option
- [ ] **TEST-02**: EOS replay verification (retry same slot+seqid, confirm cached response returned)
- [ ] **TEST-03**: Backchannel delegation recall test (CB_RECALL over fore-channel)
- [ ] **TEST-04**: v4.0/v4.1 coexistence test (both versions mounted simultaneously)
- [ ] **TEST-05**: Directory delegation notification test (CB_NOTIFY on directory mutation)

## Future Requirements

Deferred to v4.0 or later. Tracked but not in current roadmap.

### NFSv4.2 Extensions

- **V42-01**: Server-side COPY with async OFFLOAD_STATUS polling
- **V42-02**: CLONE/reflinks leveraging content-addressed storage
- **V42-03**: Sparse file support (SEEK, ALLOCATE, DEALLOCATE, ZERO_RANGE)
- **V42-04**: Extended attributes (GETXATTR, SETXATTR, LISTXATTRS, REMOVEXATTR)
- **V42-05**: Application I/O hints (IO_ADVISE)
- **V42-06**: pjdfstest POSIX compliance for NFSv3 and NFSv4

### Tech Debt

- **DEBT-01**: ACL enforcement in metadata CheckAccess (deferred from v2.0)
- **DEBT-02**: Delegation Prometheus metrics (log scraping workaround in v2.0)
- **DEBT-03**: Netgroup mount enforcement (CRUD works, enforcement deferred)

## Out of Scope

Explicitly excluded. Documented to prevent scope creep.

| Feature | Reason |
|---------|--------|
| pNFS (LAYOUTGET, LAYOUTCOMMIT, etc.) | Scale-out architecture not needed for single-instance |
| SP4_SSV state protection | SP4_MACH_CRED sufficient; SSV requires complex GSS pseudo-mechanism |
| SESSION4_PERSIST (persistent sessions) | Sessions are ephemeral; server restart requires re-establishment |
| WANT_DELEGATION | Optional optimization; return NFS4ERR_NOTSUPP initially |
| CB_PUSH_DELEG | Optional server-initiated delegation offer; not needed for MVP |
| CB_RECALL_ANY | Advanced reclaim; not needed initially |
| CB_RECALL_SLOT | Dynamic slot reduction via callback; defer to future |
| Labeled NFS (SELinux) | Not required for target use cases |
| NFS over RDMA | Standard TCP sufficient |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| SESS-01 | Phase 18 | Complete |
| SESS-02 | Phase 19 | Complete |
| SESS-03 | Phase 19 | Complete |
| SESS-04 | Phase 20 | Complete |
| SESS-05 | Phase 16 | Complete |
| EOS-01 | Phase 17 | Complete |
| EOS-02 | Phase 17 | Complete |
| EOS-03 | Phase 17 | Complete |
| BACK-01 | Phase 22 | Pending |
| BACK-02 | Phase 21 | Pending |
| BACK-03 | Phase 22 | Pending |
| BACK-04 | Phase 22 | Pending |
| LIFE-01 | Phase 23 | Pending |
| LIFE-02 | Phase 23 | Pending |
| LIFE-03 | Phase 23 | Pending |
| LIFE-04 | Phase 23 | Pending |
| LIFE-05 | Phase 23 | Pending |
| DDELEG-01 | Phase 24 | Pending |
| DDELEG-02 | Phase 24 | Pending |
| DDELEG-03 | Phase 24 | Pending |
| TRUNK-01 | Phase 21 | Pending |
| TRUNK-02 | Phase 18 | Complete |
| COEX-01 | Phase 20 | Complete |
| COEX-02 | Phase 20 | Complete |
| COEX-03 | Phase 20 | Complete |
| SMBKRB-01 | Phase 25 | Pending |
| SMBKRB-02 | Phase 25 | Pending |
| TEST-01 | Phase 25 | Pending |
| TEST-02 | Phase 25 | Pending |
| TEST-03 | Phase 25 | Pending |
| TEST-04 | Phase 25 | Pending |
| TEST-05 | Phase 25 | Pending |

**Coverage:**
- v3.0 requirements: 32 total
- Mapped to phases: 32
- Unmapped: 0

---
*Requirements defined: 2026-02-20*
*Last updated: 2026-02-20 after roadmap creation (traceability populated)*
