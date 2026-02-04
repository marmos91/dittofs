# Requirements: DittoFS NFS Protocol Evolution

**Defined:** 2026-02-04
**Core Value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication

## v1.0 Requirements — NLM + Unified Lock Manager

### Locking Infrastructure (LOCK)

- [x] **LOCK-01**: Unified Lock Manager embedded in metadata service
- [x] **LOCK-02**: Lock state persistence in metadata store (per-share)
- [x] **LOCK-03**: Flexible lock model supporting NLM, NFSv4, and SMB semantics
- [x] **LOCK-04**: Lock translation at protocol boundary (cross-protocol visibility)
- [x] **LOCK-05**: Grace period handling for server restarts
- [x] **LOCK-06**: Per-adapter connection pool (unified stateless/stateful)
- [x] **LOCK-07**: Lock conflict detection across protocols

### NLM Protocol (NLM)

- [ ] **NLM-01**: NLM protocol implementation (RPC program 100021)
- [ ] **NLM-02**: NLM_TEST operation (test lock availability)
- [ ] **NLM-03**: NLM_LOCK operation (acquire lock)
- [ ] **NLM-04**: NLM_UNLOCK operation (release lock)
- [ ] **NLM-05**: NLM_CANCEL operation (cancel pending lock)
- [ ] **NLM-06**: Byte-range locking support
- [ ] **NLM-07**: Blocking lock support with callbacks
- [ ] **NLM-08**: Non-blocking lock support
- [ ] **NLM-09**: NLM handlers in internal/protocol/nfs/nlm/

### NSM Protocol (NSM)

- [ ] **NSM-01**: NSM protocol implementation (RPC program 100024)
- [ ] **NSM-02**: SM_MON operation (monitor client)
- [ ] **NSM-03**: SM_UNMON operation (unmonitor client)
- [ ] **NSM-04**: SM_NOTIFY operation (crash notification)
- [ ] **NSM-05**: Client status tracking
- [ ] **NSM-06**: Lock cleanup on client crash
- [ ] **NSM-07**: Server restart notification to clients

### SMB Locking (SMB)

- [ ] **SMB-01**: SMB2/3 Read lease support
- [ ] **SMB-02**: SMB2/3 Write lease support
- [ ] **SMB-03**: SMB2/3 Handle lease support
- [ ] **SMB-04**: Oplock break notifications
- [ ] **SMB-05**: Lease break acknowledgment handling
- [ ] **SMB-06**: Integration with Unified Lock Manager

### Cross-Protocol (XPRO)

- [ ] **XPRO-01**: NLM lock visible to SMB clients
- [ ] **XPRO-02**: SMB lease visible to NLM clients
- [ ] **XPRO-03**: Lock conflict triggers appropriate break/deny
- [ ] **XPRO-04**: Consistent file access across protocols

### Testing v1.0 (TEST1)

- [ ] **TEST1-01**: E2E tests for NLM locking scenarios
- [ ] **TEST1-02**: E2E tests for SMB lease scenarios
- [ ] **TEST1-03**: E2E tests for cross-protocol lock conflicts
- [ ] **TEST1-04**: Grace period recovery tests
- [ ] **TEST1-05**: Client crash recovery tests

## v2.0 Requirements — NFSv4.0 + Kerberos

### NFSv4 Protocol Foundation (NFS4)

- [ ] **NFS4-01**: COMPOUND operation dispatcher
- [ ] **NFS4-02**: Current/saved filehandle context management
- [ ] **NFS4-03**: NFSv4 pseudo-filesystem (single namespace)
- [ ] **NFS4-04**: Export mapping to pseudo-filesystem paths
- [ ] **NFS4-05**: NFSv4 error code mapping
- [ ] **NFS4-06**: UTF-8 filename validation
- [ ] **NFS4-07**: Version negotiation (min/max configurable)
- [ ] **NFS4-08**: NFSv4 handlers in internal/protocol/nfs/v4/

### NFSv4 Operations (OPS4)

- [ ] **OPS4-01**: ACCESS operation
- [ ] **OPS4-02**: CLOSE operation
- [ ] **OPS4-03**: COMMIT operation
- [ ] **OPS4-04**: CREATE operation
- [ ] **OPS4-05**: GETATTR operation
- [ ] **OPS4-06**: GETFH operation
- [ ] **OPS4-07**: LINK operation
- [ ] **OPS4-08**: LOCK operation
- [ ] **OPS4-09**: LOCKT operation (test lock)
- [ ] **OPS4-10**: LOCKU operation (unlock)
- [ ] **OPS4-11**: LOOKUP operation
- [ ] **OPS4-12**: LOOKUPP operation (parent lookup)
- [ ] **OPS4-13**: NVERIFY operation
- [ ] **OPS4-14**: OPEN operation
- [ ] **OPS4-15**: OPENATTR operation
- [ ] **OPS4-16**: OPEN_CONFIRM operation
- [ ] **OPS4-17**: OPEN_DOWNGRADE operation
- [ ] **OPS4-18**: PUTFH operation
- [ ] **OPS4-19**: PUTPUBFH operation
- [ ] **OPS4-20**: PUTROOTFH operation
- [ ] **OPS4-21**: READ operation
- [ ] **OPS4-22**: READDIR operation
- [ ] **OPS4-23**: READLINK operation
- [ ] **OPS4-24**: REMOVE operation
- [ ] **OPS4-25**: RENAME operation
- [ ] **OPS4-26**: RENEW operation
- [ ] **OPS4-27**: RESTOREFH operation
- [ ] **OPS4-28**: SAVEFH operation
- [ ] **OPS4-29**: SECINFO operation
- [ ] **OPS4-30**: SETATTR operation
- [ ] **OPS4-31**: SETCLIENTID operation
- [ ] **OPS4-32**: SETCLIENTID_CONFIRM operation
- [ ] **OPS4-33**: VERIFY operation
- [ ] **OPS4-34**: WRITE operation
- [ ] **OPS4-35**: RELEASE_LOCKOWNER operation

### State Management (STATE)

- [ ] **STATE-01**: Client ID (clientid) generation and tracking
- [ ] **STATE-02**: State ID (stateid) generation and validation
- [ ] **STATE-03**: Open-owner tracking
- [ ] **STATE-04**: Lock-owner tracking
- [ ] **STATE-05**: Stateid sequence number management
- [ ] **STATE-06**: Lease renewal via RENEW
- [ ] **STATE-07**: Lease expiration handling
- [ ] **STATE-08**: State recovery via metadata store
- [ ] **STATE-09**: Grace period for lock reclaim after restart

### Delegations (DELEG)

- [ ] **DELEG-01**: Read delegation grant
- [ ] **DELEG-02**: Write delegation grant
- [ ] **DELEG-03**: Delegation recall mechanism
- [ ] **DELEG-04**: Callback channel to client (CB_COMPOUND)
- [ ] **DELEG-05**: CB_RECALL operation
- [ ] **DELEG-06**: Client-first flush on delegation recall
- [ ] **DELEG-07**: Delegation conflict detection
- [ ] **DELEG-08**: Delegation timeout and revocation

### Kerberos Authentication (KRB)

- [ ] **KRB-01**: Shared Kerberos layer (pkg/auth/kerberos)
- [ ] **KRB-02**: RPCSEC_GSS framework implementation
- [ ] **KRB-03**: krb5 authentication flavor
- [ ] **KRB-04**: krb5i integrity flavor
- [ ] **KRB-05**: krb5p privacy flavor
- [ ] **KRB-06**: AUTH_SYS fallback (configurable per share)
- [ ] **KRB-07**: External KDC integration (Active Directory)
- [ ] **KRB-08**: Keytab file support
- [ ] **KRB-09**: Service principal configuration

### NFSv4 ACLs (ACL)

- [ ] **ACL-01**: Extend existing control plane ACL model
- [ ] **ACL-02**: NFSv4 ACL storage in metadata
- [ ] **ACL-03**: ACL evaluation for access decisions
- [ ] **ACL-04**: ACL inheritance for new files/directories
- [ ] **ACL-05**: ACL interoperability with SMB ACLs

### ID Mapping (IDMAP)

- [ ] **IDMAP-01**: NFSv4 user@domain to control plane user mapping
- [ ] **IDMAP-02**: NFSv4 group@domain to control plane group mapping
- [ ] **IDMAP-03**: ID domain configuration
- [ ] **IDMAP-04**: Nobody/nogroup fallback for unmapped identities

### Control Plane v2.0 (CP2)

- [ ] **CP2-01**: NFSv4 adapter configuration in control plane
- [ ] **CP2-02**: Per-share Kerberos requirements configuration
- [ ] **CP2-03**: Per-share AUTH_SYS allowance configuration
- [ ] **CP2-04**: Version range configuration (min/max)
- [ ] **CP2-05**: Lease timeout configuration
- [ ] **CP2-06**: Grace period configuration

### Testing v2.0 (TEST2)

- [ ] **TEST2-01**: E2E tests for basic NFSv4 mount/read/write
- [ ] **TEST2-02**: E2E tests for NFSv4 locking
- [ ] **TEST2-03**: E2E tests for delegations
- [ ] **TEST2-04**: E2E tests for Kerberos authentication
- [ ] **TEST2-05**: E2E tests for NFSv4 ACLs
- [ ] **TEST2-06**: Backward compatibility tests (NFSv3 still works)

## v3.0 Requirements — NFSv4.1

### Sessions (SESS)

- [ ] **SESS-01**: EXCHANGE_ID operation
- [ ] **SESS-02**: CREATE_SESSION operation
- [ ] **SESS-03**: DESTROY_SESSION operation
- [ ] **SESS-04**: BIND_CONN_TO_SESSION operation
- [ ] **SESS-05**: Session state management
- [ ] **SESS-06**: Slot table management
- [ ] **SESS-07**: SEQUENCE operation validation
- [ ] **SESS-08**: Duplicate Request Cache (DRC)

### Exactly-Once Semantics (EOS)

- [ ] **EOS-01**: Sequence ID tracking per slot
- [ ] **EOS-02**: Replay detection and response caching
- [ ] **EOS-03**: Operation retry handling
- [ ] **EOS-04**: Session persistence across reconnects

### Backchannel (BACK)

- [ ] **BACK-01**: Backchannel over fore channel connection
- [ ] **BACK-02**: CB_SEQUENCE operation
- [ ] **BACK-03**: Backchannel slot management
- [ ] **BACK-04**: NAT-friendly callback delivery
- [ ] **BACK-05**: Backchannel security (same as fore channel)

### Directory Delegations (DDIR)

- [ ] **DDIR-01**: Directory delegation grant
- [ ] **DDIR-02**: Directory delegation recall
- [ ] **DDIR-03**: CB_NOTIFY operation
- [ ] **DDIR-04**: Directory change notifications

### NFSv4.1 Operations (OPS41)

- [ ] **OPS41-01**: DESTROY_CLIENTID operation
- [ ] **OPS41-02**: FREE_STATEID operation
- [ ] **OPS41-03**: TEST_STATEID operation
- [ ] **OPS41-04**: RECLAIM_COMPLETE operation
- [ ] **OPS41-05**: SECINFO_NO_NAME operation

### Testing v3.0 (TEST3)

- [ ] **TEST3-01**: E2E tests for session establishment
- [ ] **TEST3-02**: E2E tests for exactly-once semantics
- [ ] **TEST3-03**: E2E tests for backchannel callbacks
- [ ] **TEST3-04**: E2E tests for directory delegations
- [ ] **TEST3-05**: NAT traversal tests

## v4.0 Requirements — NFSv4.2

### Server-Side Copy (COPY)

- [ ] **COPY-01**: COPY operation (intra-server)
- [ ] **COPY-02**: Async COPY with callback
- [ ] **COPY-03**: OFFLOAD_STATUS operation
- [ ] **COPY-04**: OFFLOAD_CANCEL operation
- [ ] **COPY-05**: Copy progress tracking
- [ ] **COPY-06**: Efficient implementation via block store

### Clone/Reflinks (CLONE)

- [ ] **CLONE-01**: CLONE operation
- [ ] **CLONE-02**: Copy-on-write via content-addressed storage
- [ ] **CLONE-03**: Block sharing until modification

### Sparse Files (SPARSE)

- [ ] **SPARSE-01**: SEEK operation (DATA/HOLE)
- [ ] **SPARSE-02**: ALLOCATE operation
- [ ] **SPARSE-03**: DEALLOCATE operation
- [ ] **SPARSE-04**: ZERO_RANGE operation (via DEALLOCATE or explicit)
- [ ] **SPARSE-05**: Sparse file metadata tracking

### Extended Attributes (XATTR)

- [ ] **XATTR-01**: GETXATTR operation
- [ ] **XATTR-02**: SETXATTR operation
- [ ] **XATTR-03**: LISTXATTRS operation
- [ ] **XATTR-04**: REMOVEXATTR operation
- [ ] **XATTR-05**: Xattr storage in metadata layer
- [ ] **XATTR-06**: Xattr exposure via SMB

### NFSv4.2 Operations (OPS42)

- [ ] **OPS42-01**: IO_ADVISE operation
- [ ] **OPS42-02**: LAYOUTERROR operation (if pNFS)
- [ ] **OPS42-03**: LAYOUTSTATS operation (if pNFS)

### Documentation (DOCS)

- [ ] **DOCS-01**: Update docs/NFS.md with NFSv4 details
- [ ] **DOCS-02**: Create docs/LOCKING.md for lock semantics
- [ ] **DOCS-03**: Create docs/KERBEROS.md for auth setup
- [ ] **DOCS-04**: Update docs/CONFIGURATION.md with new options
- [ ] **DOCS-05**: Update docs/SECURITY.md with Kerberos model
- [ ] **DOCS-06**: API documentation for new operations

### Testing v4.0 (TEST4)

- [ ] **TEST4-01**: E2E tests for server-side copy
- [ ] **TEST4-02**: E2E tests for clone/reflinks
- [ ] **TEST4-03**: E2E tests for sparse files
- [ ] **TEST4-04**: E2E tests for extended attributes
- [ ] **TEST4-05**: pjdfstest POSIX compliance (NFSv3 and NFSv4)
- [ ] **TEST4-06**: Performance benchmarks

## Out of Scope

| Feature | Reason |
|---------|--------|
| pNFS (parallel NFS) | Deferred until scale-out architecture needed |
| Labeled NFS (SELinux) | Not required for target enterprise use cases |
| NFSv3 xattr workarounds | Xattrs via NFSv4.2/SMB only |
| Cross-server COPY_NOTIFY | Single server focus |
| Bundled KDC | External AD/KDC only |
| NFS over RDMA | Standard TCP sufficient |
| NFSv2 | Obsolete, no demand |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| LOCK-01 | Phase 1: Locking Infrastructure | Complete |
| LOCK-02 | Phase 1: Locking Infrastructure | Complete |
| LOCK-03 | Phase 1: Locking Infrastructure | Complete |
| LOCK-04 | Phase 1: Locking Infrastructure | Complete |
| LOCK-05 | Phase 1: Locking Infrastructure | Complete |
| LOCK-06 | Phase 1: Locking Infrastructure | Complete |
| LOCK-07 | Phase 1: Locking Infrastructure | Complete |
| NLM-01 | Phase 2: NLM Protocol | Pending |
| NLM-02 | Phase 2: NLM Protocol | Pending |
| NLM-03 | Phase 2: NLM Protocol | Pending |
| NLM-04 | Phase 2: NLM Protocol | Pending |
| NLM-05 | Phase 2: NLM Protocol | Pending |
| NLM-06 | Phase 2: NLM Protocol | Pending |
| NLM-07 | Phase 2: NLM Protocol | Pending |
| NLM-08 | Phase 2: NLM Protocol | Pending |
| NLM-09 | Phase 2: NLM Protocol | Pending |
| NSM-01 | Phase 3: NSM Protocol | Pending |
| NSM-02 | Phase 3: NSM Protocol | Pending |
| NSM-03 | Phase 3: NSM Protocol | Pending |
| NSM-04 | Phase 3: NSM Protocol | Pending |
| NSM-05 | Phase 3: NSM Protocol | Pending |
| NSM-06 | Phase 3: NSM Protocol | Pending |
| NSM-07 | Phase 3: NSM Protocol | Pending |
| SMB-01 | Phase 4: SMB Leases | Pending |
| SMB-02 | Phase 4: SMB Leases | Pending |
| SMB-03 | Phase 4: SMB Leases | Pending |
| SMB-04 | Phase 4: SMB Leases | Pending |
| SMB-05 | Phase 4: SMB Leases | Pending |
| SMB-06 | Phase 4: SMB Leases | Pending |
| XPRO-01 | Phase 5: Cross-Protocol Integration | Pending |
| XPRO-02 | Phase 5: Cross-Protocol Integration | Pending |
| XPRO-03 | Phase 5: Cross-Protocol Integration | Pending |
| XPRO-04 | Phase 5: Cross-Protocol Integration | Pending |
| TEST1-01 | Phase 5: Cross-Protocol Integration | Pending |
| TEST1-02 | Phase 5: Cross-Protocol Integration | Pending |
| TEST1-03 | Phase 5: Cross-Protocol Integration | Pending |
| TEST1-04 | Phase 5: Cross-Protocol Integration | Pending |
| TEST1-05 | Phase 5: Cross-Protocol Integration | Pending |
| NFS4-01 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-02 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-03 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-04 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-05 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-06 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-07 | Phase 6: NFSv4 Protocol Foundation | Pending |
| NFS4-08 | Phase 6: NFSv4 Protocol Foundation | Pending |
| OPS4-01 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-02 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-03 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-04 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-05 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-06 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-11 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-12 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-14 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-18 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-19 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-20 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-21 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-22 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-23 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-24 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-34 | Phase 7: NFSv4 File Operations | Pending |
| OPS4-07 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-13 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-15 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-16 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-17 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-25 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-27 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-28 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-29 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-30 | Phase 8: NFSv4 Advanced Operations | Pending |
| OPS4-33 | Phase 8: NFSv4 Advanced Operations | Pending |
| STATE-01 | Phase 9: State Management | Pending |
| STATE-02 | Phase 9: State Management | Pending |
| STATE-03 | Phase 9: State Management | Pending |
| STATE-04 | Phase 9: State Management | Pending |
| STATE-05 | Phase 9: State Management | Pending |
| STATE-06 | Phase 9: State Management | Pending |
| STATE-07 | Phase 9: State Management | Pending |
| STATE-08 | Phase 9: State Management | Pending |
| STATE-09 | Phase 9: State Management | Pending |
| OPS4-26 | Phase 9: State Management | Pending |
| OPS4-31 | Phase 9: State Management | Pending |
| OPS4-32 | Phase 9: State Management | Pending |
| OPS4-08 | Phase 10: NFSv4 Locking | Pending |
| OPS4-09 | Phase 10: NFSv4 Locking | Pending |
| OPS4-10 | Phase 10: NFSv4 Locking | Pending |
| OPS4-35 | Phase 10: NFSv4 Locking | Pending |
| DELEG-01 | Phase 11: Delegations | Pending |
| DELEG-02 | Phase 11: Delegations | Pending |
| DELEG-03 | Phase 11: Delegations | Pending |
| DELEG-04 | Phase 11: Delegations | Pending |
| DELEG-05 | Phase 11: Delegations | Pending |
| DELEG-06 | Phase 11: Delegations | Pending |
| DELEG-07 | Phase 11: Delegations | Pending |
| DELEG-08 | Phase 11: Delegations | Pending |
| KRB-01 | Phase 12: Kerberos Authentication | Pending |
| KRB-02 | Phase 12: Kerberos Authentication | Pending |
| KRB-03 | Phase 12: Kerberos Authentication | Pending |
| KRB-04 | Phase 12: Kerberos Authentication | Pending |
| KRB-05 | Phase 12: Kerberos Authentication | Pending |
| KRB-06 | Phase 12: Kerberos Authentication | Pending |
| KRB-07 | Phase 12: Kerberos Authentication | Pending |
| KRB-08 | Phase 12: Kerberos Authentication | Pending |
| KRB-09 | Phase 12: Kerberos Authentication | Pending |
| ACL-01 | Phase 13: NFSv4 ACLs | Pending |
| ACL-02 | Phase 13: NFSv4 ACLs | Pending |
| ACL-03 | Phase 13: NFSv4 ACLs | Pending |
| ACL-04 | Phase 13: NFSv4 ACLs | Pending |
| ACL-05 | Phase 13: NFSv4 ACLs | Pending |
| IDMAP-01 | Phase 13: NFSv4 ACLs | Pending |
| IDMAP-02 | Phase 13: NFSv4 ACLs | Pending |
| IDMAP-03 | Phase 13: NFSv4 ACLs | Pending |
| IDMAP-04 | Phase 13: NFSv4 ACLs | Pending |
| CP2-01 | Phase 14: Control Plane v2.0 | Pending |
| CP2-02 | Phase 14: Control Plane v2.0 | Pending |
| CP2-03 | Phase 14: Control Plane v2.0 | Pending |
| CP2-04 | Phase 14: Control Plane v2.0 | Pending |
| CP2-05 | Phase 14: Control Plane v2.0 | Pending |
| CP2-06 | Phase 14: Control Plane v2.0 | Pending |
| TEST2-01 | Phase 15: v2.0 Testing | Pending |
| TEST2-02 | Phase 15: v2.0 Testing | Pending |
| TEST2-03 | Phase 15: v2.0 Testing | Pending |
| TEST2-04 | Phase 15: v2.0 Testing | Pending |
| TEST2-05 | Phase 15: v2.0 Testing | Pending |
| TEST2-06 | Phase 15: v2.0 Testing | Pending |
| SESS-01 | Phase 16: Session Infrastructure | Pending |
| SESS-02 | Phase 16: Session Infrastructure | Pending |
| SESS-03 | Phase 16: Session Infrastructure | Pending |
| SESS-04 | Phase 16: Session Infrastructure | Pending |
| SESS-05 | Phase 16: Session Infrastructure | Pending |
| SESS-06 | Phase 16: Session Infrastructure | Pending |
| SESS-07 | Phase 17: Exactly-Once Semantics | Pending |
| SESS-08 | Phase 17: Exactly-Once Semantics | Pending |
| EOS-01 | Phase 17: Exactly-Once Semantics | Pending |
| EOS-02 | Phase 17: Exactly-Once Semantics | Pending |
| EOS-03 | Phase 17: Exactly-Once Semantics | Pending |
| EOS-04 | Phase 17: Exactly-Once Semantics | Pending |
| BACK-01 | Phase 18: Backchannel | Pending |
| BACK-02 | Phase 18: Backchannel | Pending |
| BACK-03 | Phase 18: Backchannel | Pending |
| BACK-04 | Phase 18: Backchannel | Pending |
| BACK-05 | Phase 18: Backchannel | Pending |
| DDIR-01 | Phase 19: Directory Delegations | Pending |
| DDIR-02 | Phase 19: Directory Delegations | Pending |
| DDIR-03 | Phase 19: Directory Delegations | Pending |
| DDIR-04 | Phase 19: Directory Delegations | Pending |
| OPS41-01 | Phase 20: NFSv4.1 Operations | Pending |
| OPS41-02 | Phase 20: NFSv4.1 Operations | Pending |
| OPS41-03 | Phase 20: NFSv4.1 Operations | Pending |
| OPS41-04 | Phase 20: NFSv4.1 Operations | Pending |
| OPS41-05 | Phase 20: NFSv4.1 Operations | Pending |
| TEST3-01 | Phase 21: v3.0 Testing | Pending |
| TEST3-02 | Phase 21: v3.0 Testing | Pending |
| TEST3-03 | Phase 21: v3.0 Testing | Pending |
| TEST3-04 | Phase 21: v3.0 Testing | Pending |
| TEST3-05 | Phase 21: v3.0 Testing | Pending |
| COPY-01 | Phase 22: Server-Side Copy | Pending |
| COPY-02 | Phase 22: Server-Side Copy | Pending |
| COPY-03 | Phase 22: Server-Side Copy | Pending |
| COPY-04 | Phase 22: Server-Side Copy | Pending |
| COPY-05 | Phase 22: Server-Side Copy | Pending |
| COPY-06 | Phase 22: Server-Side Copy | Pending |
| CLONE-01 | Phase 23: Clone/Reflinks | Pending |
| CLONE-02 | Phase 23: Clone/Reflinks | Pending |
| CLONE-03 | Phase 23: Clone/Reflinks | Pending |
| SPARSE-01 | Phase 24: Sparse Files | Pending |
| SPARSE-02 | Phase 24: Sparse Files | Pending |
| SPARSE-03 | Phase 24: Sparse Files | Pending |
| SPARSE-04 | Phase 24: Sparse Files | Pending |
| SPARSE-05 | Phase 24: Sparse Files | Pending |
| XATTR-01 | Phase 25: Extended Attributes | Pending |
| XATTR-02 | Phase 25: Extended Attributes | Pending |
| XATTR-03 | Phase 25: Extended Attributes | Pending |
| XATTR-04 | Phase 25: Extended Attributes | Pending |
| XATTR-05 | Phase 25: Extended Attributes | Pending |
| XATTR-06 | Phase 25: Extended Attributes | Pending |
| OPS42-01 | Phase 26: NFSv4.2 Operations | Pending |
| OPS42-02 | Phase 26: NFSv4.2 Operations | Pending |
| OPS42-03 | Phase 26: NFSv4.2 Operations | Pending |
| DOCS-01 | Phase 27: Documentation | Pending |
| DOCS-02 | Phase 27: Documentation | Pending |
| DOCS-03 | Phase 27: Documentation | Pending |
| DOCS-04 | Phase 27: Documentation | Pending |
| DOCS-05 | Phase 27: Documentation | Pending |
| DOCS-06 | Phase 27: Documentation | Pending |
| TEST4-01 | Phase 28: v4.0 Testing | Pending |
| TEST4-02 | Phase 28: v4.0 Testing | Pending |
| TEST4-03 | Phase 28: v4.0 Testing | Pending |
| TEST4-04 | Phase 28: v4.0 Testing | Pending |
| TEST4-05 | Phase 28: v4.0 Testing | Pending |
| TEST4-06 | Phase 28: v4.0 Testing | Pending |

**Coverage:**
- v1.0 requirements: 33 total (Phases 1-5)
- v2.0 requirements: 75 total (Phases 6-15)
- v3.0 requirements: 26 total (Phases 16-21)
- v4.0 requirements: 28 total (Phases 22-28)
- **Total: 162 requirements mapped to 28 phases**

---
*Requirements defined: 2026-02-04*
*Traceability updated: 2026-02-04*
*Milestone: v1.0 through v4.0*
