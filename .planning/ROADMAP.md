# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across four milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, and v4.0 completes the protocol suite with NFSv4.2 advanced features (server-side copy, sparse files, extended attributes). Each milestone delivers complete, testable functionality.

## Milestones

- [ ] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (33 requirements + manual verification)
- [ ] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (75 requirements + 3 manual checkpoints)
- [ ] **v3.0 NFSv4.1 Sessions** - Phases 16-21.5 (26 requirements + 2 manual checkpoints)
- [ ] **v4.0 NFSv4.2 Extensions** - Phases 22-28.5 (28 requirements + 2 manual checkpoints)

**USER CHECKPOINT** phases require your manual testing before proceeding. Use `/gsd:verify-work` to validate.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

### v1.0 NLM + Unified Lock Manager

- [x] **Phase 1: Locking Infrastructure** ✓ - Unified lock manager embedded in metadata service
- [x] **Phase 2: NLM Protocol** ✓ - Network Lock Manager for NFSv3 clients
- [ ] **Phase 3: NSM Protocol** - Network Status Monitor for crash recovery
- [ ] **Phase 4: SMB Leases** - SMB2/3 oplock and lease support
- [ ] **Phase 5: Cross-Protocol Integration** - Lock visibility across NFS and SMB
- [ ] **Phase 5.5: Manual Verification v1.0** USER CHECKPOINT - Test NFS+SMB locking manually

### v2.0 NFSv4.0 + Kerberos

- [ ] **Phase 6: NFSv4 Protocol Foundation** - Compound operations and pseudo-filesystem
- [ ] **Phase 7: NFSv4 File Operations** - Lookup, read, write, create, remove
- [ ] **Phase 7.5: Manual Verification - Basic NFSv4** USER CHECKPOINT - Test NFSv4 mount and basic ops
- [ ] **Phase 8: NFSv4 Advanced Operations** - Link, rename, verify, security info
- [ ] **Phase 9: State Management** - Client ID, state ID, and lease tracking
- [ ] **Phase 10: NFSv4 Locking** - Integrated byte-range locking (LOCK/LOCKT/LOCKU)
- [ ] **Phase 11: Delegations** - Read/write delegations with callback channel
- [ ] **Phase 12: Kerberos Authentication** - RPCSEC_GSS framework with krb5/krb5i/krb5p
- [ ] **Phase 12.5: Manual Verification - Kerberos** USER CHECKPOINT - Test Kerberos auth manually
- [ ] **Phase 13: NFSv4 ACLs** - Extended ACL model with Windows interoperability
- [ ] **Phase 14: Control Plane v2.0** - NFSv4 adapter configuration and settings
- [ ] **Phase 15: v2.0 Testing** - Comprehensive E2E tests for NFSv4.0
- [ ] **Phase 15.5: Manual Verification v2.0** USER CHECKPOINT - Full NFSv4.0 validation

### v3.0 NFSv4.1 Sessions

- [ ] **Phase 16: Session Infrastructure** - EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION
- [ ] **Phase 17: Exactly-Once Semantics** - Slot tables and duplicate request cache
- [ ] **Phase 18: Backchannel** - NAT-friendly callbacks over fore channel
- [ ] **Phase 18.5: Manual Verification - Sessions** USER CHECKPOINT - Test session behavior
- [ ] **Phase 19: Directory Delegations** - Directory change notifications
- [ ] **Phase 20: NFSv4.1 Operations** - DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID
- [ ] **Phase 21: v3.0 Testing** - Session and backchannel E2E tests
- [ ] **Phase 21.5: Manual Verification v3.0** USER CHECKPOINT - Full NFSv4.1 validation

### v4.0 NFSv4.2 Extensions

- [ ] **Phase 22: Server-Side Copy** - Async COPY with OFFLOAD_STATUS polling
- [ ] **Phase 23: Clone/Reflinks** - Copy-on-write via content-addressed storage
- [ ] **Phase 24: Sparse Files** - SEEK, ALLOCATE, DEALLOCATE operations
- [ ] **Phase 24.5: Manual Verification - Advanced Ops** USER CHECKPOINT - Test copy/clone/sparse
- [ ] **Phase 25: Extended Attributes** - xattrs in metadata layer, exposed via NFS/SMB
- [ ] **Phase 26: NFSv4.2 Operations** - IO_ADVISE and optional pNFS operations
- [ ] **Phase 27: Documentation** - Complete documentation for all new features
- [ ] **Phase 28: v4.0 Testing** - Final testing and pjdfstest POSIX compliance
- [ ] **Phase 28.5: Final Manual Verification** USER CHECKPOINT - Complete validation of all features

## Phase Details

---

## v1.0 NLM + Unified Lock Manager

### Phase 1: Locking Infrastructure
**Goal**: Build the protocol-agnostic unified lock manager that serves as the foundation for all locking across NFS and SMB
**Depends on**: Nothing (first phase)
**Requirements**: LOCK-01, LOCK-02, LOCK-03, LOCK-04, LOCK-05, LOCK-06, LOCK-07
**Success Criteria** (what must be TRUE):
  1. Lock manager accepts lock requests with protocol-agnostic semantics
  2. Lock state persists in metadata store and survives server restart
  3. Lock conflicts are detected across different lock types (read/write, shared/exclusive)
  4. Grace period rejects new locks while allowing reclaims after restart
  5. Connection pool manages client connections per adapter
**Plans**: 4 plans

Plans:
- [x] 01-01-PLAN.md — Enhanced lock types, POSIX splitting, deadlock detection, config
- [x] 01-02-PLAN.md — Lock persistence (LockStore interface, memory/badger/postgres)
- [x] 01-03-PLAN.md — Grace period state machine, connection tracking, metrics
- [x] 01-04-PLAN.md — Refactor: Move lock code to pkg/metadata/lock/, errors to pkg/metadata/errors/

### Phase 2: NLM Protocol
**Goal**: Implement the Network Lock Manager protocol (RPC 100021) for NFSv3 locking
**Depends on**: Phase 1
**Requirements**: NLM-01, NLM-02, NLM-03, NLM-04, NLM-05, NLM-06, NLM-07, NLM-08, NLM-09
**Success Criteria** (what must be TRUE):
  1. NFSv3 client can acquire and release byte-range locks via fcntl()
  2. Blocking lock requests queue and notify when lock becomes available
  3. NLM_TEST correctly reports lock conflicts with owner information
  4. Lock cancellation stops pending blocking locks
**Plans**: 3 plans in 3 waves

Plans:
- [x] 02-01-PLAN.md — Shared XDR utilities extraction, NLM types and constants
- [x] 02-02-PLAN.md — NLM dispatcher integration and core handlers (NULL, TEST, LOCK, UNLOCK, CANCEL)
- [x] 02-03-PLAN.md — Blocking lock queue and NLM_GRANTED callback mechanism

### Phase 3: NSM Protocol
**Goal**: Implement Network Status Monitor (RPC 100024) for crash recovery
**Depends on**: Phase 2
**Requirements**: NSM-01, NSM-02, NSM-03, NSM-04, NSM-05, NSM-06, NSM-07
**Success Criteria** (what must be TRUE):
  1. Server monitors registered clients and detects crashes
  2. Client crash triggers automatic lock cleanup
  3. Server restart sends SM_NOTIFY to all previously registered clients
  4. Clients can reclaim locks during grace period after restart
**Plans**: TBD

Plans:
- [ ] 03-01: NSM protocol dispatcher and state tracking
- [ ] 03-02: SM_MON, SM_UNMON, SM_NOTIFY operations
- [ ] 03-03: Lock cleanup and restart notification

### Phase 4: SMB Leases
**Goal**: Add SMB2/3 oplock and lease support integrated with unified lock manager
**Depends on**: Phase 1
**Requirements**: SMB-01, SMB-02, SMB-03, SMB-04, SMB-05, SMB-06
**Success Criteria** (what must be TRUE):
  1. SMB client can acquire Read, Write, and Handle leases
  2. Oplock break notifications sent when conflicting access occurs
  3. Lease break acknowledgments correctly transition lease state
  4. SMB leases flow through unified lock manager (not separate tracking)
**Plans**: TBD

Plans:
- [ ] 04-01: SMB lease types and state machine
- [ ] 04-02: Oplock break notification mechanism
- [ ] 04-03: Integration with unified lock manager

### Phase 5: Cross-Protocol Integration
**Goal**: Enable lock visibility and conflict detection across NFS (NLM) and SMB protocols
**Depends on**: Phase 2, Phase 3, Phase 4
**Requirements**: XPRO-01, XPRO-02, XPRO-03, XPRO-04, TEST1-01, TEST1-02, TEST1-03, TEST1-04, TEST1-05
**Success Criteria** (what must be TRUE):
  1. NLM lock on a file blocks conflicting SMB write access
  2. SMB exclusive lease triggers NLM lock denial
  3. Cross-protocol file access maintains consistency (no data corruption)
  4. E2E tests verify locking scenarios across both protocols
  5. Grace period recovery works for both NFS and SMB clients
**Plans**: TBD

Plans:
- [ ] 05-01: Cross-protocol lock translation and conflict detection
- [ ] 05-02: E2E tests for NLM locking scenarios
- [ ] 05-03: E2E tests for SMB lease scenarios
- [ ] 05-04: E2E tests for cross-protocol conflicts

---

## v2.0 NFSv4.0 + Kerberos

### Phase 6: NFSv4 Protocol Foundation
**Goal**: Implement NFSv4 compound operation dispatcher and pseudo-filesystem
**Depends on**: Phase 5 (v1.0 complete)
**Requirements**: NFS4-01, NFS4-02, NFS4-03, NFS4-04, NFS4-05, NFS4-06, NFS4-07, NFS4-08
**Success Criteria** (what must be TRUE):
  1. COMPOUND operations execute multiple ops in single RPC, stopping on first error
  2. Current/saved filehandle context maintained across operations in compound
  3. Pseudo-filesystem presents unified namespace for all exports
  4. NFSv4 error codes correctly mapped from internal errors
  5. UTF-8 filenames validated on creation
**Plans**: TBD

Plans:
- [ ] 06-01: NFSv4 XDR types and error mapping
- [ ] 06-02: COMPOUND dispatcher with filehandle context
- [ ] 06-03: Pseudo-filesystem implementation

### Phase 7: NFSv4 File Operations
**Goal**: Implement core file operations (lookup, read, write, create, remove)
**Depends on**: Phase 6
**Requirements**: OPS4-01, OPS4-02, OPS4-03, OPS4-04, OPS4-05, OPS4-06, OPS4-11, OPS4-12, OPS4-14, OPS4-18, OPS4-19, OPS4-20, OPS4-21, OPS4-22, OPS4-23, OPS4-24, OPS4-34
**Success Criteria** (what must be TRUE):
  1. NFSv4 client can mount, navigate directories, and read files
  2. File creation and deletion work through NFSv4 operations
  3. OPEN/CLOSE operations manage file access state correctly
  4. READDIR returns directory entries with requested attributes
  5. Symbolic links readable via READLINK
**Plans**: TBD

Plans:
- [ ] 07-01: PUTFH, PUTROOTFH, PUTPUBFH, GETFH operations
- [ ] 07-02: LOOKUP, LOOKUPP, ACCESS operations
- [ ] 07-03: GETATTR, CREATE, REMOVE operations
- [ ] 07-04: OPEN, CLOSE, READ, WRITE, COMMIT operations
- [ ] 07-05: READDIR, READLINK operations

### Phase 8: NFSv4 Advanced Operations
**Goal**: Implement remaining NFSv4 operations (link, rename, verify, security)
**Depends on**: Phase 7
**Requirements**: OPS4-07, OPS4-13, OPS4-15, OPS4-16, OPS4-17, OPS4-25, OPS4-27, OPS4-28, OPS4-29, OPS4-30, OPS4-33
**Success Criteria** (what must be TRUE):
  1. Hard links creatable via LINK operation
  2. RENAME moves files within and across directories
  3. VERIFY/NVERIFY enable conditional operations
  4. SAVEFH/RESTOREFH enable complex compound sequences
  5. SECINFO returns available security mechanisms for path
**Plans**: TBD

Plans:
- [ ] 08-01: LINK, RENAME operations
- [ ] 08-02: SAVEFH, RESTOREFH operations
- [ ] 08-03: VERIFY, NVERIFY operations
- [ ] 08-04: SETATTR, OPENATTR operations
- [ ] 08-05: SECINFO, OPEN_CONFIRM, OPEN_DOWNGRADE operations

### Phase 9: State Management
**Goal**: Implement NFSv4 stateful model (client ID, state ID, leases)
**Depends on**: Phase 7
**Requirements**: STATE-01, STATE-02, STATE-03, STATE-04, STATE-05, STATE-06, STATE-07, STATE-08, STATE-09, OPS4-26, OPS4-31, OPS4-32
**Success Criteria** (what must be TRUE):
  1. SETCLIENTID/SETCLIENTID_CONFIRM establish client identity
  2. State IDs generated for open and lock operations
  3. Lease renewal via RENEW extends client state lifetime
  4. Expired leases trigger state cleanup after grace period
  5. Server restart preserves client records for reclaim
**Plans**: TBD

Plans:
- [ ] 09-01: Client ID management (SETCLIENTID, SETCLIENTID_CONFIRM)
- [ ] 09-02: State ID generation and validation
- [ ] 09-03: Open-owner and lock-owner tracking
- [ ] 09-04: Lease management (RENEW, expiration)
- [ ] 09-05: State recovery and grace period handling

### Phase 10: NFSv4 Locking
**Goal**: Implement NFSv4 integrated byte-range locking
**Depends on**: Phase 9
**Requirements**: OPS4-08, OPS4-09, OPS4-10, OPS4-35
**Success Criteria** (what must be TRUE):
  1. LOCK acquires byte-range locks with proper state tracking
  2. LOCKT tests for lock conflicts without acquiring
  3. LOCKU releases locks correctly
  4. NFSv4 locks integrate with unified lock manager (cross-protocol aware)
  5. RELEASE_LOCKOWNER cleans up lock-owner state
**Plans**: TBD

Plans:
- [ ] 10-01: LOCK operation with stateid management
- [ ] 10-02: LOCKT, LOCKU operations
- [ ] 10-03: RELEASE_LOCKOWNER operation
- [ ] 10-04: Integration with unified lock manager

### Phase 11: Delegations
**Goal**: Implement read/write delegations with callback mechanism
**Depends on**: Phase 9
**Requirements**: DELEG-01, DELEG-02, DELEG-03, DELEG-04, DELEG-05, DELEG-06, DELEG-07, DELEG-08
**Success Criteria** (what must be TRUE):
  1. Read delegation granted when client has exclusive read access
  2. Write delegation granted when client has exclusive write access
  3. Delegation recall via CB_RECALL when conflict detected
  4. Client flushes dirty data before returning delegation
  5. Delegation revoked after timeout if client unresponsive
**Plans**: TBD

Plans:
- [ ] 11-01: Delegation state tracking and grant logic
- [ ] 11-02: Callback channel (CB_COMPOUND, CB_RECALL)
- [ ] 11-03: Conflict detection and recall triggering
- [ ] 11-04: Delegation timeout and revocation

### Phase 12: Kerberos Authentication
**Goal**: Implement RPCSEC_GSS framework with Kerberos v5 support
**Depends on**: Phase 6
**Requirements**: KRB-01, KRB-02, KRB-03, KRB-04, KRB-05, KRB-06, KRB-07, KRB-08, KRB-09
**Success Criteria** (what must be TRUE):
  1. NFSv4 client authenticates via Kerberos (krb5)
  2. Integrity protection (krb5i) verifies message authenticity
  3. Privacy protection (krb5p) encrypts RPC payload
  4. AUTH_SYS fallback available for shares that allow it
  5. External KDC (Active Directory) integration works
**Plans**: TBD

Plans:
- [ ] 12-01: Shared Kerberos layer (pkg/auth/kerberos)
- [ ] 12-02: RPCSEC_GSS framework implementation
- [ ] 12-03: krb5 authentication flavor
- [ ] 12-04: krb5i integrity and krb5p privacy
- [ ] 12-05: Keytab and service principal configuration

### Phase 13: NFSv4 ACLs
**Goal**: Extend ACL model for NFSv4 with Windows interoperability
**Depends on**: Phase 7
**Requirements**: ACL-01, ACL-02, ACL-03, ACL-04, ACL-05, IDMAP-01, IDMAP-02, IDMAP-03, IDMAP-04
**Success Criteria** (what must be TRUE):
  1. NFSv4 ACLs stored and retrieved via GETATTR/SETATTR
  2. ACL evaluation determines access decisions
  3. New files/directories inherit ACLs from parent
  4. NFSv4 user@domain mapped to control plane users
  5. ACLs interoperable between NFSv4 and SMB
**Plans**: TBD

Plans:
- [ ] 13-01: NFSv4 ACL storage in metadata
- [ ] 13-02: ACL evaluation and inheritance
- [ ] 13-03: ID mapping (user@domain to control plane)
- [ ] 13-04: ACL interoperability with SMB

### Phase 14: Control Plane v2.0
**Goal**: Add NFSv4 configuration support to control plane
**Depends on**: Phase 6
**Requirements**: CP2-01, CP2-02, CP2-03, CP2-04, CP2-05, CP2-06
**Success Criteria** (what must be TRUE):
  1. NFSv4 adapter configurable via control plane API
  2. Per-share Kerberos requirements configurable
  3. Per-share AUTH_SYS allowance configurable
  4. Version range (min/max) configurable
  5. Lease and grace period timeouts configurable
**Plans**: TBD

Plans:
- [ ] 14-01: NFSv4 adapter configuration
- [ ] 14-02: Per-share security settings
- [ ] 14-03: Lease and grace period configuration

### Phase 15: v2.0 Testing
**Goal**: Comprehensive E2E testing for all NFSv4.0 functionality
**Depends on**: Phase 10, Phase 11, Phase 12, Phase 13, Phase 14
**Requirements**: TEST2-01, TEST2-02, TEST2-03, TEST2-04, TEST2-05, TEST2-06
**Success Criteria** (what must be TRUE):
  1. NFSv4 mount, read, write E2E tests pass
  2. NFSv4 locking E2E tests verify lock/unlock cycles
  3. Delegation E2E tests verify grant and recall
  4. Kerberos E2E tests verify all three flavors
  5. NFSv3 backward compatibility confirmed (still works)
**Plans**: TBD

Plans:
- [ ] 15-01: Basic NFSv4 operation E2E tests
- [ ] 15-02: NFSv4 locking E2E tests
- [ ] 15-03: Delegation E2E tests
- [ ] 15-04: Kerberos authentication E2E tests
- [ ] 15-05: ACL and backward compatibility tests

---

## v3.0 NFSv4.1 Sessions

### Phase 16: Session Infrastructure
**Goal**: Implement NFSv4.1 session establishment and management
**Depends on**: Phase 15 (v2.0 complete)
**Requirements**: SESS-01, SESS-02, SESS-03, SESS-04, SESS-05, SESS-06
**Success Criteria** (what must be TRUE):
  1. EXCHANGE_ID establishes client identity with server
  2. CREATE_SESSION creates session with negotiated parameters
  3. DESTROY_SESSION cleanly terminates session
  4. BIND_CONN_TO_SESSION associates connection with session
  5. Multiple connections bindable to single session
**Plans**: TBD

Plans:
- [ ] 16-01: EXCHANGE_ID operation
- [ ] 16-02: CREATE_SESSION operation
- [ ] 16-03: DESTROY_SESSION, BIND_CONN_TO_SESSION operations
- [ ] 16-04: Session state management

### Phase 17: Exactly-Once Semantics
**Goal**: Implement slot tables and duplicate request cache
**Depends on**: Phase 16
**Requirements**: SESS-07, SESS-08, EOS-01, EOS-02, EOS-03, EOS-04
**Success Criteria** (what must be TRUE):
  1. SEQUENCE operation validates slot and sequence ID
  2. Duplicate requests return cached response (no re-execution)
  3. Out-of-order sequence IDs detected and rejected
  4. Session state survives client reconnection
**Plans**: TBD

Plans:
- [ ] 17-01: Slot table management
- [ ] 17-02: SEQUENCE operation validation
- [ ] 17-03: Duplicate request cache (DRC)
- [ ] 17-04: Session persistence across reconnects

### Phase 18: Backchannel
**Goal**: Implement NAT-friendly callback channel over fore channel connection
**Depends on**: Phase 16
**Requirements**: BACK-01, BACK-02, BACK-03, BACK-04, BACK-05
**Success Criteria** (what must be TRUE):
  1. Backchannel established over same TCP connection as fore channel
  2. CB_SEQUENCE validates backchannel slot
  3. Delegation recalls delivered via backchannel
  4. Callbacks work through NAT/firewall (no separate connection needed)
**Plans**: TBD

Plans:
- [ ] 18-01: Backchannel infrastructure
- [ ] 18-02: CB_SEQUENCE operation
- [ ] 18-03: Backchannel slot management
- [ ] 18-04: NAT traversal verification

### Phase 19: Directory Delegations
**Goal**: Implement directory delegations with change notifications
**Depends on**: Phase 18
**Requirements**: DDIR-01, DDIR-02, DDIR-03, DDIR-04
**Success Criteria** (what must be TRUE):
  1. Directory delegation granted for exclusive directory access
  2. CB_NOTIFY sent when directory contents change
  3. Directory delegation recalled on conflicting access
  4. Client caches directory entries until recall
**Plans**: TBD

Plans:
- [ ] 19-01: Directory delegation state tracking
- [ ] 19-02: CB_NOTIFY operation
- [ ] 19-03: Directory change detection
- [ ] 19-04: Directory delegation recall

### Phase 20: NFSv4.1 Operations
**Goal**: Implement remaining NFSv4.1-specific operations
**Depends on**: Phase 17
**Requirements**: OPS41-01, OPS41-02, OPS41-03, OPS41-04, OPS41-05
**Success Criteria** (what must be TRUE):
  1. DESTROY_CLIENTID removes all client state
  2. FREE_STATEID releases specific state
  3. TEST_STATEID validates state IDs
  4. RECLAIM_COMPLETE ends grace period early
  5. SECINFO_NO_NAME returns security for implicit lookup
**Plans**: TBD

Plans:
- [ ] 20-01: DESTROY_CLIENTID operation
- [ ] 20-02: FREE_STATEID, TEST_STATEID operations
- [ ] 20-03: RECLAIM_COMPLETE operation
- [ ] 20-04: SECINFO_NO_NAME operation

### Phase 21: v3.0 Testing
**Goal**: Comprehensive E2E testing for NFSv4.1 functionality
**Depends on**: Phase 18, Phase 19, Phase 20
**Requirements**: TEST3-01, TEST3-02, TEST3-03, TEST3-04, TEST3-05
**Success Criteria** (what must be TRUE):
  1. Session establishment E2E tests pass
  2. Exactly-once semantics verified (replays return cached response)
  3. Backchannel callbacks work through NAT
  4. Directory delegations grant and recall correctly
  5. Multi-connection session trunking works
**Plans**: TBD

Plans:
- [ ] 21-01: Session E2E tests
- [ ] 21-02: Exactly-once semantics E2E tests
- [ ] 21-03: Backchannel and NAT traversal tests
- [ ] 21-04: Directory delegation E2E tests

---

## v4.0 NFSv4.2 Extensions

### Phase 22: Server-Side Copy
**Goal**: Implement async server-side COPY operation
**Depends on**: Phase 21 (v3.0 complete)
**Requirements**: COPY-01, COPY-02, COPY-03, COPY-04, COPY-05, COPY-06
**Success Criteria** (what must be TRUE):
  1. COPY operation copies data without client I/O
  2. Async COPY returns immediately with stateid for tracking
  3. OFFLOAD_STATUS reports copy progress
  4. OFFLOAD_CANCEL terminates in-progress copy
  5. Large file copy completes efficiently via block store
**Plans**: TBD

Plans:
- [ ] 22-01: COPY operation implementation
- [ ] 22-02: Async COPY with callback
- [ ] 22-03: OFFLOAD_STATUS, OFFLOAD_CANCEL operations
- [ ] 22-04: Efficient block store integration

### Phase 23: Clone/Reflinks
**Goal**: Implement CLONE operation leveraging content-addressed storage
**Depends on**: Phase 22
**Requirements**: CLONE-01, CLONE-02, CLONE-03
**Success Criteria** (what must be TRUE):
  1. CLONE creates copy-on-write file instantly
  2. Cloned files share blocks until modification
  3. Modification triggers copy of affected blocks only
**Plans**: TBD

Plans:
- [ ] 23-01: CLONE operation implementation
- [ ] 23-02: Copy-on-write block sharing
- [ ] 23-03: Block copy on modification

### Phase 24: Sparse Files
**Goal**: Implement sparse file operations (SEEK, ALLOCATE, DEALLOCATE)
**Depends on**: Phase 22
**Requirements**: SPARSE-01, SPARSE-02, SPARSE-03, SPARSE-04, SPARSE-05
**Success Criteria** (what must be TRUE):
  1. SEEK locates DATA or HOLE regions in file
  2. ALLOCATE pre-allocates file space
  3. DEALLOCATE punches holes in file
  4. Sparse file metadata correctly tracks allocated regions
**Plans**: TBD

Plans:
- [ ] 24-01: Sparse file metadata tracking
- [ ] 24-02: SEEK operation (DATA/HOLE)
- [ ] 24-03: ALLOCATE, DEALLOCATE operations

### Phase 25: Extended Attributes
**Goal**: Implement xattr storage and NFSv4.2/SMB exposure
**Depends on**: Phase 22
**Requirements**: XATTR-01, XATTR-02, XATTR-03, XATTR-04, XATTR-05, XATTR-06
**Success Criteria** (what must be TRUE):
  1. GETXATTR retrieves extended attribute value
  2. SETXATTR stores extended attribute
  3. LISTXATTRS enumerates all xattr names
  4. REMOVEXATTR deletes extended attribute
  5. Xattrs accessible via both NFSv4.2 and SMB
**Plans**: TBD

Plans:
- [ ] 25-01: Xattr storage in metadata layer
- [ ] 25-02: GETXATTR, SETXATTR operations
- [ ] 25-03: LISTXATTRS, REMOVEXATTR operations
- [ ] 25-04: SMB xattr exposure

### Phase 26: NFSv4.2 Operations
**Goal**: Implement remaining NFSv4.2 operations
**Depends on**: Phase 24
**Requirements**: OPS42-01, OPS42-02, OPS42-03
**Success Criteria** (what must be TRUE):
  1. IO_ADVISE accepts application I/O hints
  2. LAYOUTERROR and LAYOUTSTATS available if pNFS enabled
**Plans**: TBD

Plans:
- [ ] 26-01: IO_ADVISE operation
- [ ] 26-02: Optional pNFS operations (LAYOUTERROR, LAYOUTSTATS)

### Phase 27: Documentation
**Goal**: Complete documentation for all new features
**Depends on**: Phase 25
**Requirements**: DOCS-01, DOCS-02, DOCS-03, DOCS-04, DOCS-05, DOCS-06
**Success Criteria** (what must be TRUE):
  1. docs/NFS.md updated with NFSv4 details
  2. docs/LOCKING.md documents all lock semantics
  3. docs/KERBEROS.md explains authentication setup
  4. docs/CONFIGURATION.md covers all new options
  5. docs/SECURITY.md describes Kerberos security model
**Plans**: TBD

Plans:
- [ ] 27-01: NFS and locking documentation
- [ ] 27-02: Kerberos and security documentation
- [ ] 27-03: Configuration and API documentation

### Phase 28: v4.0 Testing
**Goal**: Final testing including pjdfstest POSIX compliance
**Depends on**: Phase 22, Phase 23, Phase 24, Phase 25, Phase 26, Phase 27
**Requirements**: TEST4-01, TEST4-02, TEST4-03, TEST4-04, TEST4-05, TEST4-06
**Success Criteria** (what must be TRUE):
  1. Server-side copy E2E tests pass for various file sizes
  2. Clone/reflinks E2E tests verify block sharing
  3. Sparse file E2E tests verify hole handling
  4. Xattr E2E tests verify cross-protocol access
  5. pjdfstest POSIX compliance passes for NFSv3 and NFSv4
  6. Performance benchmarks establish baseline
**Plans**: TBD

Plans:
- [ ] 28-01: Server-side copy E2E tests
- [ ] 28-02: Clone and sparse file E2E tests
- [ ] 28-03: Extended attributes E2E tests
- [ ] 28-04: pjdfstest POSIX compliance
- [ ] 28-05: Performance benchmarks

---

## Progress

**Execution Order:**
Phases execute in numeric order: 1 -> 2 -> 3 -> ... -> 28

| Phase | Milestone | Plans Complete | Status | Completed |
|-------|-----------|----------------|--------|-----------|
| 1. Locking Infrastructure | v1.0 | 4/4 | Complete | 2026-02-04 |
| 2. NLM Protocol | v1.0 | 3/3 | Complete | 2026-02-05 |
| 3. NSM Protocol | v1.0 | 0/3 | Not started | - |
| 4. SMB Leases | v1.0 | 0/3 | Not started | - |
| 5. Cross-Protocol Integration | v1.0 | 0/4 | Not started | - |
| 6. NFSv4 Protocol Foundation | v2.0 | 0/3 | Not started | - |
| 7. NFSv4 File Operations | v2.0 | 0/5 | Not started | - |
| 8. NFSv4 Advanced Operations | v2.0 | 0/5 | Not started | - |
| 9. State Management | v2.0 | 0/5 | Not started | - |
| 10. NFSv4 Locking | v2.0 | 0/4 | Not started | - |
| 11. Delegations | v2.0 | 0/4 | Not started | - |
| 12. Kerberos Authentication | v2.0 | 0/5 | Not started | - |
| 13. NFSv4 ACLs | v2.0 | 0/4 | Not started | - |
| 14. Control Plane v2.0 | v2.0 | 0/3 | Not started | - |
| 15. v2.0 Testing | v2.0 | 0/5 | Not started | - |
| 16. Session Infrastructure | v3.0 | 0/4 | Not started | - |
| 17. Exactly-Once Semantics | v3.0 | 0/4 | Not started | - |
| 18. Backchannel | v3.0 | 0/4 | Not started | - |
| 19. Directory Delegations | v3.0 | 0/4 | Not started | - |
| 20. NFSv4.1 Operations | v3.0 | 0/4 | Not started | - |
| 21. v3.0 Testing | v3.0 | 0/4 | Not started | - |
| 22. Server-Side Copy | v4.0 | 0/4 | Not started | - |
| 23. Clone/Reflinks | v4.0 | 0/3 | Not started | - |
| 24. Sparse Files | v4.0 | 0/3 | Not started | - |
| 25. Extended Attributes | v4.0 | 0/4 | Not started | - |
| 26. NFSv4.2 Operations | v4.0 | 0/2 | Not started | - |
| 27. Documentation | v4.0 | 0/3 | Not started | - |
| 28. v4.0 Testing | v4.0 | 0/5 | Not started | - |

**Total:** 7/108 plans complete

---
*Roadmap created: 2026-02-04*
*Phase 1 completed: 2026-02-04*
*Phase 2 completed: 2026-02-05*
*Requirements coverage: 162/162 mapped*
