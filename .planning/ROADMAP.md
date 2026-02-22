# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across four milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, and v4.0 completes the protocol suite with NFSv4.2 advanced features (server-side copy, sparse files, extended attributes). Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [ ] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (32 requirements across 10 phases)
- [ ] **v4.0 NFSv4.2 Extensions** - Phases 26-32.5 (planned)

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

### v3.0 NFSv4.1 Sessions

- [x] **Phase 16: NFSv4.1 Types and Constants** - Operation numbers, error codes, XDR structures for all v4.1 wire types (completed 2026-02-20)
- [x] **Phase 17: Slot Table and Session Data Structures** - SlotTable, SessionRecord, ChannelAttrs, EOS replay cache with per-table locking (completed 2026-02-20)
- [x] **Phase 18: EXCHANGE_ID and Client Registration** - v4.1 client identity establishment with owner/implementation tracking (completed 2026-02-20)
- [x] **Phase 19: Session Lifecycle** - CREATE_SESSION, DESTROY_SESSION with slot table allocation and channel negotiation (completed 2026-02-21)
- [x] **Phase 20: SEQUENCE and COMPOUND Bifurcation** - v4.1 request processing with EOS enforcement and v4.0/v4.1 coexistence (completed 2026-02-21)
- [ ] **Phase 20.5: Manual Verification - Sessions** USER CHECKPOINT - Test session establishment and EOS
- [x] **Phase 21: Connection Management and Trunking** - BIND_CONN_TO_SESSION, multi-connection sessions, server_owner consistency (completed 2026-02-21)
- [x] **Phase 22: Backchannel Multiplexing** - CB_SEQUENCE over fore-channel, bidirectional I/O, NAT-friendly callbacks (completed 2026-02-21)
- [x] **Phase 23: Client Lifecycle and Cleanup** - DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID, RECLAIM_COMPLETE, v4.0-only rejections (completed 2026-02-22)
- [x] **Phase 24: Directory Delegations** - GET_DIR_DELEGATION, CB_NOTIFY, delegation state tracking with recall (completed 2026-02-22)
- [ ] **Phase 25: v3.0 Integration Testing** - E2E tests for sessions, EOS, backchannel, directory delegations, and coexistence
- [ ] **Phase 25.5: Manual Verification v3.0** USER CHECKPOINT - Full NFSv4.1 validation with Linux client

### v4.0 NFSv4.2 Extensions

- [ ] **Phase 26: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 27: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 28: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 28.5: Manual Verification - Advanced Ops** USER CHECKPOINT - Test copy/clone/sparse
- [ ] **Phase 29: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 30: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 31: Documentation** - Complete documentation for all new features
- [ ] **Phase 32: v4.0 Testing** - Final testing and pjdfstest POSIX compliance
- [ ] **Phase 32.5: Final Manual Verification** USER CHECKPOINT - Complete validation of all features

## Phase Details

---

## v3.0 NFSv4.1 Sessions

### Phase 16: NFSv4.1 Types and Constants
**Goal**: All NFSv4.1 wire types, operation numbers, error codes, and XDR structures are defined and available for subsequent phases
**Depends on**: Phase 15 (v2.0 complete)
**Requirements**: SESS-05
**Success Criteria** (what must be TRUE):
  1. NFSv4.1 operation numbers (ops 40-58) and callback operations (CB ops 5-14) are defined as constants
  2. XDR encode/decode structures exist for all v4.1 request/response types (EXCHANGE_ID, CREATE_SESSION, SEQUENCE, etc.)
  3. New NFSv4.1 error codes (NFS4ERR_BACK_CHAN_BUSY, NFS4ERR_CONN_NOT_BOUND_TO_SESSION, etc.) are defined
  4. Existing v4.0 constants and types compile unchanged (no regressions)
**Plans**: 5 plans
Plans:
- [x] 16-01-PLAN.md -- Foundation: constants, error codes, XDR interfaces, shared session types, test fixtures
- [x] 16-02-PLAN.md -- Core session ops: EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, SEQUENCE, BIND_CONN_TO_SESSION, BACKCHANNEL_CTL
- [x] 16-03-PLAN.md -- Remaining forward ops: FREE_STATEID, TEST_STATEID, DESTROY_CLIENTID, RECLAIM_COMPLETE, SECINFO_NO_NAME, SET_SSV, WANT_DELEGATION, GET_DIR_DELEGATION, pNFS layout ops
- [x] 16-04-PLAN.md -- Callback ops: CB_SEQUENCE, CB_LAYOUTRECALL, CB_NOTIFY, and 7 remaining CB operations
- [x] 16-05-PLAN.md -- COMPOUND v4.1 dispatch: minorversion bifurcation, v4.1 dispatch table with NOTSUPP stubs, protocol CLAUDE.md

### Phase 17: Slot Table and Session Data Structures
**Goal**: Session infrastructure data structures are implemented and unit-tested, ready for use by operation handlers
**Depends on**: Phase 16
**Requirements**: EOS-01, EOS-02, EOS-03
**Success Criteria** (what must be TRUE):
  1. SlotTable stores full COMPOUND responses for replay detection with per-slot sequence ID tracking
  2. Sequence ID validation correctly identifies retries (same seqid), misordered requests, and stale slots
  3. Server can dynamically adjust slot count via target_highest_slotid signaling
  4. Per-SlotTable mutex provides concurrency without serializing on the global StateManager RWMutex
**Plans**: 2 plans
Plans:
- [x] 17-01-PLAN.md -- SlotTable struct, Slot struct, sequence validation algorithm (RFC 8881), dynamic sizing, unit tests
- [x] 17-02-PLAN.md -- Session record struct, NewSession constructor, slot table wiring, session tests

### Phase 18: EXCHANGE_ID and Client Registration
**Goal**: NFSv4.1 clients can register with the server and receive a client ID for session creation
**Depends on**: Phase 17
**Requirements**: SESS-01, TRUNK-02
**Success Criteria** (what must be TRUE):
  1. Client sends EXCHANGE_ID with owner string and receives a unique clientid and sequence ID
  2. Server tracks implementation ID (name, domain, build date) for each registered v4.1 client
  3. Server reports consistent server_owner across calls so clients can detect trunking opportunities
  4. Duplicate EXCHANGE_ID from same owner updates existing client record (idempotent)
**Plans**: 2 plans
Plans:
- [x] 18-01-PLAN.md -- V41ClientRecord, ServerIdentity, ExchangeID on StateManager, handler + dispatch wiring, unit/integration tests
- [x] 18-02-PLAN.md -- REST API /clients endpoint, /health server info, apiclient methods, dfsctl client list/evict commands

### Phase 19: Session Lifecycle
**Goal**: NFSv4.1 clients can create and destroy sessions with negotiated channel attributes
**Depends on**: Phase 18
**Requirements**: SESS-02, SESS-03
**Success Criteria** (what must be TRUE):
  1. CREATE_SESSION allocates a session with fore-channel and back-channel slot tables using negotiated attributes
  2. Session ID is returned to client and usable for subsequent SEQUENCE operations
  3. DESTROY_SESSION tears down session, releases all slot table memory, and unbinds connections
  4. Channel attribute negotiation respects server-imposed limits (max slots, max request/response size)
**Plans**: 1 plan
Plans:
- [x] 19-01-PLAN.md -- StateManager session methods, CREATE_SESSION/DESTROY_SESSION handlers, channel negotiation, replay detection, reaper, metrics, REST API, dfsctl CLI

### Phase 20: SEQUENCE and COMPOUND Bifurcation
**Goal**: Every v4.1 COMPOUND is gated by SEQUENCE validation, providing exactly-once semantics while v4.0 clients continue working unchanged
**Depends on**: Phase 19
**Requirements**: SESS-04, COEX-01, COEX-02, COEX-03
**Success Criteria** (what must be TRUE):
  1. COMPOUND dispatcher routes minorversion=0 to existing v4.0 path and minorversion=1 to v4.1 path with SEQUENCE enforcement
  2. SEQUENCE validates slot ID, sequence ID, and session ID before any other v4.1 operation executes
  3. Duplicate v4.1 requests (same slot + seqid) return the cached response without re-execution
  4. v4.0 clients continue working unchanged with per-owner seqid validation
  5. Per-owner seqid validation is bypassed for v4.1 operations (slot table provides replay protection)
**Plans**: 2 plans
Plans:
- [ ] 20-01-PLAN.md -- SEQUENCE handler, dispatchV41 SEQUENCE gating, replay cache, seqid bypass, lease renewal, status flags
- [ ] 20-02-PLAN.md -- Prometheus metrics, minor version range config (full stack), v4.0 regression + coexistence + concurrent tests, benchmark

### Phase 21: Connection Management and Trunking
**Goal**: Multiple TCP connections can be bound to a single session, enabling trunking and reconnection after network disruption
**Depends on**: Phase 20
**Requirements**: BACK-02, TRUNK-01
**Success Criteria** (what must be TRUE):
  1. BIND_CONN_TO_SESSION associates a new TCP connection with an existing session in fore, back, or both directions
  2. Multiple connections bound to one session can each send COMPOUND requests and receive responses
  3. Server tracks which connections are bound to which sessions and cleans up on disconnect
**Plans**: 2 plans
Plans:
- [x] 21-01-PLAN.md -- Core binding model: connection ID plumbing, StateManager connection methods, BIND_CONN_TO_SESSION handler, auto-bind on CREATE_SESSION, disconnect cleanup, draining support, unit tests
- [x] 21-02-PLAN.md -- Observability & API: Prometheus connection metrics, REST API session detail extension (connection breakdown), V4MaxConnectionsPerSession config full stack, CLI updates, multi-connection integration tests

### Phase 22: Backchannel Multiplexing
**Goal**: Server sends callbacks to v4.1 clients over the fore-channel TCP connection without requiring a separate connection
**Depends on**: Phase 21
**Requirements**: BACK-01, BACK-03, BACK-04
**Success Criteria** (what must be TRUE):
  1. Server sends CB_SEQUENCE + CB_RECALL over a connection bound for backchannel traffic (no separate dial-out)
  2. BACKCHANNEL_CTL allows client to update backchannel security parameters
  3. Existing CB_RECALL works over backchannel for v4.1 clients while v4.0 clients continue using separate TCP callback
  4. Callbacks work through NAT/firewall (server never initiates new TCP connections for v4.1 clients)
**Plans**: 2 plans
Plans:
- [ ] 22-01-PLAN.md -- Core backchannel infrastructure: shared wire-format helpers, BackchannelSender goroutine, read-loop demux, callback routing, BACKCHANNEL_CTL handler, GetStatusFlags update
- [ ] 22-02-PLAN.md -- Prometheus backchannel metrics, integration tests with TCP loopback, BACKCHANNEL_CTL handler tests, protocol documentation

### Phase 23: Client Lifecycle and Cleanup
**Goal**: Server supports full client lifecycle management including graceful cleanup, stateid validation, and v4.0-only operation rejection
**Depends on**: Phase 20
**Requirements**: LIFE-01, LIFE-02, LIFE-03, LIFE-04, LIFE-05
**Success Criteria** (what must be TRUE):
  1. DESTROY_CLIENTID removes all client state after all sessions are destroyed
  2. RECLAIM_COMPLETE signals end of grace period reclaim, allowing server to free reclaim-tracking resources
  3. FREE_STATEID releases individual stateids and TEST_STATEID batch-validates stateid liveness
  4. v4.0-only operations (SETCLIENTID, SETCLIENTID_CONFIRM, RENEW, OPEN_CONFIRM, RELEASE_LOCKOWNER) return NFS4ERR_NOTSUPP for minorversion=1
**Plans**: 3 plans
Plans:
- [x] 23-01-PLAN.md -- State methods: DestroyV41ClientID, FreeStateid, TestStateids, grace enrichment (Status, ForceEnd, ReclaimComplete), state tests with race detection
- [ ] 23-02-PLAN.md -- Handlers + dispatch: 4 handler files (destroy_clientid, reclaim_complete, free_stateid, test_stateid), v4.0-only rejection in v4.1 COMPOUNDs, DESTROY_CLIENTID session-exempt, handler tests
- [ ] 23-03-PLAN.md -- Grace API/CLI: REST endpoints (GET /api/v1/grace, POST /api/v1/grace/end), health enrichment, `dfs status` countdown, `dfsctl grace status/end` commands

### Phase 24: Directory Delegations
**Goal**: Server can grant directory delegations and notify clients of directory changes via backchannel
**Depends on**: Phase 22
**Requirements**: DDELEG-01, DDELEG-02, DDELEG-03
**Success Criteria** (what must be TRUE):
  1. GET_DIR_DELEGATION grants a delegation with notification bitmask specifying which changes the client wants to hear about
  2. CB_NOTIFY sent over backchannel when directory entries are added, removed, renamed, or have attributes changed
  3. Directory delegation state is tracked in StateManager with recall and revocation support (same pattern as file delegations)
  4. Directory delegation is recalled when a conflicting client modifies the directory
**Plans**: 3 plans
Plans:
- [ ] 24-01-PLAN.md -- State model: DelegationState extensions, DirNotification type, CB_NOTIFY sub-type encoders, GrantDirDelegation, NotifyDirChange, batch flush, config fields
- [ ] 24-02-PLAN.md -- GET_DIR_DELEGATION handler, DELEGRETURN flush, dispatch registration, config full stack (store, API, apiclient, CLI, settings watcher)
- [ ] 24-03-PLAN.md -- Mutation handler hooks (CREATE, REMOVE, RENAME, LINK, OPEN, SETATTR), conflict recall, Prometheus metrics, integration tests, docs/NFS.md

### Phase 25: v3.0 Integration Testing
**Goal**: All NFSv4.1 functionality verified end-to-end with real Linux NFS client mounts
**Depends on**: Phase 22, Phase 23, Phase 24
**Requirements**: TEST-01, TEST-02, TEST-03, TEST-04, TEST-05, SMBKRB-01, SMBKRB-02
**Success Criteria** (what must be TRUE):
  1. Linux NFS client mounts with vers=4.1 and performs basic file operations (create, read, write, delete, rename)
  2. EOS replay verification passes: retrying same slot+seqid returns cached response without re-execution
  3. Backchannel delegation recall works: CB_RECALL delivered over fore-channel connection to v4.1 client
  4. v4.0 and v4.1 clients coexist: both versions mounted simultaneously with independent state
  5. SMB adapter authenticates via SPNEGO/Kerberos using shared Kerberos layer with correct identity mapping
**Plans**: TBD

---

## v4.0 NFSv4.2 Extensions

### Phase 26: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 25 (v3.0 complete)
**Requirements**: V42-01
**Success Criteria** (what must be TRUE):
  1. COPY operation copies data without client I/O
  2. Async COPY returns immediately with stateid for tracking
  3. OFFLOAD_STATUS reports copy progress
  4. OFFLOAD_CANCEL terminates in-progress copy
  5. Large file copy completes efficiently via block store
**Plans**: TBD

### Phase 27: Clone/Reflinks
**Goal**: Implement CLONE operation leveraging content-addressed storage
**Depends on**: Phase 26
**Requirements**: V42-02
**Success Criteria** (what must be TRUE):
  1. CLONE creates copy-on-write file instantly
  2. Cloned files share blocks until modification
  3. Modification triggers copy of affected blocks only
**Plans**: TBD

### Phase 28: Sparse Files
**Goal**: Implement sparse file operations (SEEK, ALLOCATE, DEALLOCATE)
**Depends on**: Phase 26
**Requirements**: V42-03
**Success Criteria** (what must be TRUE):
  1. SEEK locates DATA or HOLE regions in file
  2. ALLOCATE pre-allocates file space
  3. DEALLOCATE punches holes in file
  4. Sparse file metadata correctly tracks allocated regions
**Plans**: TBD

### Phase 29: Extended Attributes
**Goal**: Implement xattr storage and NFSv4.2/SMB exposure
**Depends on**: Phase 26
**Requirements**: V42-04
**Success Criteria** (what must be TRUE):
  1. GETXATTR retrieves extended attribute value
  2. SETXATTR stores extended attribute
  3. LISTXATTRS enumerates all xattr names
  4. REMOVEXATTR deletes extended attribute
  5. Xattrs accessible via both NFSv4.2 and SMB
**Plans**: TBD

### Phase 30: NFSv4.2 Operations
**Goal**: Implement remaining NFSv4.2 operations
**Depends on**: Phase 28
**Requirements**: V42-05
**Success Criteria** (what must be TRUE):
  1. IO_ADVISE accepts application I/O hints
  2. LAYOUTERROR and LAYOUTSTATS available if pNFS enabled
**Plans**: TBD

### Phase 31: Documentation
**Goal**: Complete documentation for all new features
**Depends on**: Phase 29
**Requirements**: (documentation)
**Success Criteria** (what must be TRUE):
  1. docs/NFS.md updated with NFSv4.1 and NFSv4.2 details
  2. docs/CONFIGURATION.md covers all new session and v4.2 options
  3. docs/SECURITY.md describes Kerberos security model for NFS and SMB
**Plans**: TBD

### Phase 32: v4.0 Testing
**Goal**: Final testing including pjdfstest POSIX compliance
**Depends on**: Phase 26, Phase 27, Phase 28, Phase 29, Phase 30, Phase 31
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
Phases execute in numeric order: 1 -> 2 -> 3 -> ... -> 32

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
| 20. SEQUENCE and COMPOUND Bifurcation | 2/2 | Complete    | 2026-02-21 | - |
| 21. Connection Management and Trunking | 2/2 | Complete    | 2026-02-21 | - |
| 22. Backchannel Multiplexing | 2/2 | Complete    | 2026-02-21 | - |
| 23. Client Lifecycle and Cleanup | 3/3 | Complete    | 2026-02-22 | - |
| 24. Directory Delegations | 3/3 | Complete   | 2026-02-22 | - |
| 25. v3.0 Integration Testing | v3.0 | 0/? | Not started | - |
| 26. Server-Side Copy | v4.0 | 0/? | Not started | - |
| 27. Clone/Reflinks | v4.0 | 0/? | Not started | - |
| 28. Sparse Files | v4.0 | 0/? | Not started | - |
| 29. Extended Attributes | v4.0 | 0/? | Not started | - |
| 30. NFSv4.2 Operations | v4.0 | 0/? | Not started | - |
| 31. Documentation | v4.0 | 0/? | Not started | - |
| 32. v4.0 Testing | v4.0 | 0/? | Not started | - |

**Total:** 82/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 roadmap created: 2026-02-20*
