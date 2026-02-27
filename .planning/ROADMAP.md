# Roadmap: DittoFS NFS Protocol Evolution

## Overview

DittoFS evolves from NFSv3 to full NFSv4.2 support across eight milestones. v1.0 builds the unified locking foundation (NLM + SMB leases), v2.0 adds NFSv4.0 stateful operations with Kerberos authentication, v3.0 introduces NFSv4.1 sessions for reliability and NAT-friendliness, v3.5 refactors the adapter layer and core for clean protocol separation, v3.6 achieves Windows SMB compatibility with proper ACL support, v3.8 upgrades the SMB implementation to SMB3.0/3.0.2/3.1.1 with encryption, leases, Kerberos, and durable handles, v4.0 completes the protocol suite with NFSv4.2 advanced features, and v4.1 establishes performance baselines via a comprehensive benchmarking suite and iterative optimization. Each milestone delivers complete, testable functionality.

## Milestones

- [x] **v1.0 NLM + Unified Lock Manager** - Phases 1-5.5 (shipped 2026-02-07) — [archive](milestones/v1.0-ROADMAP.md)
- [x] **v2.0 NFSv4.0 + Kerberos** - Phases 6-15.5 (shipped 2026-02-20) — [archive](milestones/v2.0-ROADMAP.md)
- [x] **v3.0 NFSv4.1 Sessions** - Phases 16-25.5 (shipped 2026-02-25) — [archive](milestones/v3.0-ROADMAP.md)
- [x] **v3.5 Adapter + Core Refactoring** - Phases 26-29.5 (shipped 2026-02-26) — [archive](milestones/v3.5-ROADMAP.md)
- [ ] **v3.6 Windows Compatibility** - Phases 30-32.5 (planned)
- [ ] **v3.8 SMB3 Protocol Upgrade** - Phases 39-44.5 (planned)
- [ ] **v4.0 NFSv4.2 Extensions** - Phases 45-51.5 (planned)
- [ ] **v4.1 Benchmarking & Performance** - Phases 33-38.5 (infrastructure started, Phase 33 complete)

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

### v3.6 Windows Compatibility

- [x] **Phase 29.8: Microsoft Protocol Test Suite CI Integration** INSERTED - Dockerized WPTS FileServer harness running MS-SMB2 BVT tests against DittoFS on custom port in CI (completed 2026-02-26)
- [x] **Phase 30: SMB Bug Fixes** - Fix sparse file READ (#180), renamed directory listing (#181), parent dir navigation (#214), oplock break wiring (#213), hardcoded link count (#221), pipe share list caching (#223) (completed 2026-02-27)
- [ ] **Phase 31: Windows ACL Support** - NT Security Descriptors, Unix-to-SID mapping, icacls support (#182)
- [ ] **Phase 32: Windows Integration Testing** - smbtorture + manual Windows 11 validation, Windows CI (#173), Windows client compat (#172), Unix path fixes (#169), SMB capability flags (#141)
- [ ] **Phase 32.5: Manual Verification - Windows** USER CHECKPOINT - Full Windows validation

### v3.8 SMB3 Protocol Upgrade

- [ ] **Phase 39: SMB3 Security & Encryption** - Dialect negotiation (3.0/3.0.2/3.1.1), AES signing, AES encryption, preauth integrity, NTLM encryption (#215)
- [ ] **Phase 40: SMB3 Leases & Locking** - Read/Write/Handle leases, directory leases, break notifications, Unified Lock Manager integration
- [ ] **Phase 40.5: SMB2/3 Change Notify** INSERTED - Directory change notifications for Windows Explorer auto-refresh (18 WPTS BVT failures)
- [ ] **Phase 41: SMB3 Authentication & ACLs** - SPNEGO/Kerberos via shared layer (#124), NTLM fallback, Windows security descriptors, ACL translation, enhanced identity model (#147)
- [ ] **Phase 42: SMB3 Resilience** - Durable handles v1/v2, handle state tracking, reconnection with restoration
- [ ] **Phase 43: SMB3 Advanced Features & Cross-Protocol** - Server-side copy FSCTL_SRV_COPYCHUNK (#145), Alternate Data Streams (#146), immediate write visibility, bidirectional lock coordination, cross-protocol ACL consistency
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

## v3.6 Windows Compatibility

### Phase 29.8: Microsoft Protocol Test Suite CI Integration (INSERTED)
**Goal**: Integrate Microsoft WindowsProtocolTestSuites FileServer test suite into CI, running MS-SMB2 BVT tests against DittoFS on a custom port
**Depends on**: Phase 29.5 (refactoring complete, NFS+SMB verified)
**Requirements**: WIN-00 (test infrastructure)
**Reference**: [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) (MIT), [FileServer User Guide](https://github.com/Microsoft/WindowsProtocolTestSuites/blob/main/TestSuites/FileServer/docs/FileServerUserGuide.md)
**Success Criteria** (what must be TRUE):
  1. Docker Compose setup: DittoFS server container + WPTS FileServer container (`mcr.microsoft.com/windowsprotocoltestsuites:fileserver`) in shared network
  2. ptfconfig configured for workgroup mode (no domain controller) with `TransportPort=12445`, `SutComputerName` pointing at DittoFS, NTLM auth credentials matching DittoFS control plane users
  3. DittoFS bootstrap script creates required shares (`SMBBasic`, `SMBEncrypted`) and test users via `dfsctl` before tests run
  4. MS-SMB2 BVT (Build Verification Tests) category runs and reports results as NUnit XML
  5. Test runner script (`test/smb-conformance/run.sh`) orchestrates: start DittoFS, wait healthy, bootstrap shares/users, run WPTS container, collect results, exit code reflects pass/fail
  6. CI job (GitHub Actions) runs the SMB conformance suite on every PR touching `pkg/adapter/smb/` or `internal/adapter/smb/`
  7. Results summary printed to CI log with pass/fail/skip counts; full NUnit XML archived as artifact
  8. Known-failing tests documented in `test/smb-conformance/KNOWN_FAILURES.md` with issue references
  9. Phase 44 (SMB3 Conformance Testing) updated to extend this infrastructure rather than build from scratch
**Plans**: 2 plans (COMPLETED)
Plans:
- [x] 29.8-01-PLAN.md — Docker infrastructure, ptfconfig templates, bootstrap script, DittoFS configs
- [x] 29.8-02-PLAN.md — Test runner, TRX parser, known failures, CI workflow, documentation

### Phase 30: SMB Bug Fixes
**Goal**: Fix known SMB bugs blocking Windows file operations
**Depends on**: Phase 29.8 (WPTS CI in place for regression testing)
**Requirements**: BUG-01, BUG-02, BUG-03, BUG-04, BUG-05, BUG-06
**Reference**: GitHub issues #180 (sparse READ), #181 (renamed directory listing), #214 (parent dir navigation), #213 (oplock break wiring), #221 (hardcoded NumberOfLinks), #223 (pipe share list caching)
**Success Criteria** (what must be TRUE):
  1. User can read from sparse file regions in Windows Explorer without errors (zeros returned for unwritten blocks)
  2. Payload layer treats missing blocks as sparse (zero-fill) when offset is within file size
  3. User can rename directory and immediately see children listed correctly in parent (no stale paths)
  4. Metadata Move operation updates Path field before persisting to store
  5. Multi-component paths with `..` segments correctly navigate to parent directory (#214)
  6. NFS v3 write/setattr/remove operations trigger oplock break for SMB clients holding locks on same files (#213)
  7. FileStandardInfo.NumberOfLinks reads actual link count from metadata attributes (#221)
  8. Share list cached for pipe CREATE operations, invalidated on share add/remove events (#223)
  9. E2E tests verify sparse READ, renamed directory, parent navigation, and oplock break scenarios
  10. WPTS BVT suite shows no regressions from bug fixes
**Plans**: 4 plans
Plans:
- [ ] 30-01-PLAN.md — Sparse file READ zero-fill (payload/offloader/download.go, cache/read.go, E2E test)
- [ ] 30-02-PLAN.md — Renamed directory path update (metadata/file_modify.go, store impls, E2E test)
- [ ] 30-03-PLAN.md — Parent dir navigation and link count fix (handlers/create.go path resolver, converters.go NumberOfLinks)
- [ ] 30-04-PLAN.md — Cross-protocol oplock break wiring and pipe share list caching (v3/handlers, adapter/smb)

### Phase 31: Windows ACL Support
**Goal**: Windows users see meaningful permissions in Explorer and icacls instead of "Everyone: Full Control"
**Depends on**: Phase 30 (bug fixes complete)
**Requirements**: SD-01, SD-02, SD-03, SD-04, SD-05, SD-06, SD-07, SD-08
**Reference**: GitHub issue #182, MS-DTYP (Security Descriptors), MS-SMB2 Section 2.2.39 (QUERY_INFO SecurityInformation)
**Success Criteria** (what must be TRUE):
  1. Windows Explorer Properties → Security tab shows Owner, Group, and Permissions (not blank or Everyone)
  2. icacls command displays DACL with owner/group/other permissions derived from POSIX mode bits
  3. ACE entries ordered in Windows canonical order (deny before allow, inherited after explicit)
  4. Well-known SIDs present in default DACLs (NT AUTHORITY\SYSTEM, BUILTIN\Administrators)
  5. Directory ACEs have inheritance flags set (CONTAINER_INHERIT_ACE, OBJECT_INHERIT_ACE)
  6. User UID 1000 and group GID 1000 have distinct SIDs (no collision)
  7. QUERY_INFO with SACL flag returns valid empty SACL structure (not omitted)
  8. SET_INFO SecurityInformation accepts permission changes (best-effort mapping to Unix mode)
  9. NFSv4 ACL queries and SMB Security Descriptor queries remain consistent
**Plans**: 3 plans
Plans:
- [ ] 31-01-PLAN.md — Machine SID generation, SID mapper enhancements (user/group RID separation)
- [ ] 31-02-PLAN.md — POSIX-to-DACL synthesis, canonical ACE ordering, well-known SIDs, inheritance flags
- [ ] 31-03-PLAN.md — SD control flags (SE_DACL_AUTO_INHERITED, SE_DACL_PROTECTED), SACL stub, integration tests

### Phase 32: Windows Integration Testing
**Goal**: Comprehensive conformance validation ensures DittoFS works correctly with Windows 11 clients and passes industry-standard test suites
**Depends on**: Phase 31 (ACL support complete)
**Requirements**: WIN-02, WIN-03, WIN-05, WIN-06, TEST-01, TEST-02, TEST-03, WIN-07, WIN-08, WIN-09, WIN-10
**Reference**: Samba smbtorture (GPLv3), Microsoft WindowsProtocolTestSuites (MIT), Windows 11 23H2+, GitHub #141, #173, #172, #169
**Already completed** (by Phase 29.8 conformance PR):
  - ~~WIN-01~~: CREATE response context wire encoding (lease contexts serialized correctly)
  - ~~WIN-04~~: SMB signing enforced for authenticated sessions
  - ~~WIN-05 partial~~: FilePositionInformation, FileModeInformation, FileAlignmentInformation handlers added
**Success Criteria** (what must be TRUE):
  1. smbtorture SMB2 basic tests pass: smb2.connect, smb2.read, smb2.write, smb2.lock, smb2.oplock, smb2.lease
  2. smbtorture SMB2 ACL tests pass: smb2.acls, smb2.dir
  3. WPTS BVT suite passes with ≥150 tests (current baseline), no regressions from v3.6 changes
  4. Windows 11 user can: create file/folder in Explorer, rename, delete, copy, move, drag-and-drop
  5. Windows 11 cmd.exe operations work: dir, type, copy, move, ren, del, mkdir, rmdir, icacls, fsutil
  6. Windows 11 PowerShell operations work: Get-Item, Set-Item, Get-Acl, Set-Acl
  7. MxAc (Maximal Access) create context response returned to clients (#141)
  8. QFid (Query on Disk ID) create context response returned to clients (#141)
  9. Remaining FileInfoClass handlers added: FileCompressionInformation, FileAttributeTagInformation (#141)
  10. Guest access signing negotiation handled for Windows 11 24H2
  11. FileFsAttributeInformation capability flags updated to reflect supported features (#141)
  12. Windows CI build step added to GitHub Actions (#173)
  13. NFS and SMB client compatibility validated from Windows (#172)
  14. Hardcoded Unix paths fixed for Windows compatibility (#169)
  15. Issues #180, #181, #182, #214, #213, #221, #223 verified fixed on Windows 11
  16. No regressions on Linux/macOS NFS or SMB mounts
  17. KNOWN_FAILURES.md updated with current pass/fail status and issue links
**Plans**: 3 plans
Plans:
- [ ] 32-01-PLAN.md — Windows 11 compatibility fixes (MxAc/QFid contexts, remaining FileInfoClass handlers, guest signing, capability flags)
- [ ] 32-02-PLAN.md — smbtorture integration, Windows CI build step, Unix path fixes (#173, #169, test suite run, failure triage)
- [ ] 32-03-PLAN.md — Manual Windows 11 validation and regression testing (Explorer, cmd, PowerShell, Windows client compat #172, KNOWN_FAILURES.md update)

---

## v3.8 SMB3 Protocol Upgrade

### Phase 39: SMB3 Security & Encryption
**Goal**: SMB3 dialect negotiation with encryption and signing, preventing eavesdropping and tampering
**Depends on**: Phase 32.5 (v3.6 complete)
**Requirements**: SEC-01 through SEC-12, SMB3-TEST-01, SMB3-TEST-02
**Reference**: feat/smb3 branch, MS-SMB2, GitHub #215
**Success Criteria** (what must be TRUE):
  1. Windows 10/11 client can connect using SMB 3.1.1 dialect with encrypted traffic
  2. SMB 3.0.2 clients (older Windows, macOS) can connect with AES-CCM encryption
  3. Downgrade attacks blocked — client specifying 3.1.1 cannot be forced to 2.x
  4. Per-share encryption settings work (one share encrypted, another unencrypted)
  5. AES-CMAC signing (3.0+) and AES-GMAC signing (3.1.1) both functional
  6. Preauth integrity SHA-512 hash chain validated
  7. NTLM CHALLENGE_MESSAGE encryption flags (Flag128, Flag56) backed by actual session key derivation and encryption (#215)
  8. E2E tests verify encryption and signing for all cipher suites
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

### Phase 40.5: SMB2/3 Change Notify (INSERTED)
**Goal**: Implement SMB2 CHANGE_NOTIFY for Windows Explorer auto-refresh and file system monitoring
**Depends on**: Phase 40 (leases in place for coordinated notifications)
**Requirements**: CN-01 through CN-05
**Reference**: MS-SMB2 Section 2.2.35 (CHANGE_NOTIFY), MS-FSA Section 2.1.5.10
**Success Criteria** (what must be TRUE):
  1. Windows Explorer auto-refreshes when files are created, renamed, or deleted by another client
  2. CHANGE_NOTIFY request with FILE_LIST_DIRECTORY access queued as async until change occurs
  3. Supported completion filters: FILE_NOTIFY_CHANGE_FILE_NAME, FILE_NOTIFY_CHANGE_DIR_NAME, FILE_NOTIFY_CHANGE_ATTRIBUTES, FILE_NOTIFY_CHANGE_SIZE, FILE_NOTIFY_CHANGE_LAST_WRITE, FILE_NOTIFY_CHANGE_LAST_ACCESS, FILE_NOTIFY_CHANGE_CREATION, FILE_NOTIFY_CHANGE_EA, FILE_NOTIFY_CHANGE_SECURITY, FILE_NOTIFY_CHANGE_STREAM_NAME, FILE_NOTIFY_CHANGE_STREAM_SIZE, FILE_NOTIFY_CHANGE_STREAM_WRITE
  4. WATCH_TREE flag enables recursive subdirectory monitoring
  5. CANCEL request cancels pending CHANGE_NOTIFY and returns STATUS_CANCELLED
  6. Pending notifications flushed on CLOSE of directory handle
  7. FILE_NOTIFY_INFORMATION records returned with correct Action, FileNameLength, FileName fields
  8. MaxTransactSize honored — overflow returns STATUS_NOTIFY_ENUM_DIR
  9. All 18 BVT_SMB2Basic_ChangeNotify_* WPTS tests pass
  10. No regressions on existing WPTS BVT pass count
**Plans**: TBD

### Phase 41: SMB3 Authentication & ACLs
**Goal**: Kerberos/SPNEGO authentication and Windows security descriptor support
**Depends on**: Phase 39
**Requirements**: AUTH-01 through AUTH-06, ACL-01 through ACL-04, SMB3-TEST-04
**Reference**: feat/smb3 branch, MS-DTYP, GitHub #124, #147
**Success Criteria** (what must be TRUE):
  1. Domain-joined Windows client can access share via Kerberos (no password prompt)
  2. Non-domain client falls back to NTLM authentication
  3. Guest/anonymous access works on configured shares
  4. Windows security properties dialog shows correct permissions (from control plane ACLs)
  5. Modifying permissions via Windows dialog updates control plane (SET_INFO)
  6. Cross-protocol ACL consistency maintained (SMB ACL <-> NFSv4 ACL <-> control plane)
  7. SMB2 adapter processes Kerberos tokens via SPNEGO, not just NTLM (#124)
  8. Enhanced identity model prevents fidelity loss in cross-protocol ACL scenarios (#147)
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

### Phase 43: SMB3 Advanced Features & Cross-Protocol Integration
**Goal**: Advanced SMB features and unified behavior across SMB3/NFSv3/NFSv4 — server-side copy, ADS, immediate visibility, bidirectional locking, consistent ACLs
**Depends on**: Phase 40, Phase 41, Phase 42
**Requirements**: XPROTO-01 through XPROTO-03, ADV-01, ADV-02, SMB3-TEST-05, SMB3-TEST-06
**Reference**: feat/smb3 branch, GitHub #145, #146
**Success Criteria** (what must be TRUE):
  1. Write via SMB3 immediately readable via NFS (no cache delay)
  2. SMB3 byte-range lock blocks NFS write to same range
  3. NFS byte-range lock blocks SMB3 write to same range
  4. ACLs set via SMB3 visible via NFSv4 ACL query (and vice versa)
  5. FSCTL_SRV_COPYCHUNK server-side copy avoids data round-trip through client (#145)
  6. Alternate Data Streams (ADS) support for NTFS-compatible named streams (#146)
  7. Windows 10/11, macOS, and Linux SMB clients all verified
**Plans**: TBD

### Phase 44: SMB3 Conformance Testing
**Goal**: Validate SMB3 implementation against industry-standard conformance test suites and verify client compatibility
**Extends**: Phase 29.8 WPTS infrastructure with SMB3-specific test categories, updated ptfconfig capabilities, and smbtorture integration
**Depends on**: Phase 39, Phase 40, Phase 40.5, Phase 41, Phase 42, Phase 43
**Requirements**: SMB3-CONF-01 through SMB3-CONF-05
**Reference**: [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) (MIT), [Samba smbtorture](https://wiki.samba.org/index.php/Writing_Torture_Tests) (GPLv3)
**Baseline**: 133/240 BVT tests passing as of Phase 29.8 (2026-02-26)
**Success Criteria** (what must be TRUE):
  1. WPTS BVT suite passes ≥200/240 tests (up from 133 baseline), with remaining failures only in permanently out-of-scope categories
  2. Microsoft WPTS SMB3-specific feature tests pass: Encryption (AES-128/256-CCM/GCM), Signing, Negotiate (dialect contexts), DurableHandle (v1+v2), Leasing, Replay, SessionMgmt
  3. Microsoft WPTS dialect-filtered tests pass for Smb30, Smb302, and Smb311 categories
  4. All 18 BVT_SMB2Basic_ChangeNotify_* tests pass (covered by Phase 40.5)
  5. BVT_Leasing_FileLeasingV1 and BVT_OpLockBreak tests pass (covered by Phase 40)
  6. BVT_DurableHandleV1_Reconnect_* tests pass (covered by Phase 42)
  7. Samba smbtorture SMB3 tests pass: smb2.durable_v2_open, smb2.lease, smb2.dirlease, smb2.replay, smb2.session, smb2.session_req_sign, smb2.compound, smb2.oplocks, smb2.lock, smb2.acls
  8. Go integration tests (hirochachacha/go-smb2, BSD-2-Clause) verify basic client-server interop with SMB3 dialects
  9. Client compatibility matrix validated: Windows 10, Windows 11 (SMB 3.1.1), macOS (SMB 3.0.2), Linux cifs.ko (SMB 3.1.1)
  10. No regressions on SMB2 clients or NFS mounts
  11. Test infrastructure Dockerized for CI repeatability (WPTS Docker image + smbtorture container)
**Permanently out-of-scope failure categories** (expected to remain as known failures):
  - VHD/RSVD (Virtual Hard Disk): Not a filesystem feature
  - SWN (Service Witness Protocol): Requires clustering
  - SQoS (Storage QoS): Requires storage virtualization
  - DFS (Distributed File System): Not implemented
  - NTFS-FsCtl (Object IDs, integrity streams): NTFS-specific internals
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

## v4.1 Benchmarking & Performance

### Phase 33: Benchmark Infrastructure
**Goal**: Create bench/ directory structure with Docker Compose profiles and configuration files
**Depends on**: Phase 51 (v4.0 complete)
**Requirements**: BENCH-01
**Reference**: GitHub #194
**Status**: COMPLETE (merged 2026-02-27, PR #224)
**Success Criteria** (what must be TRUE):
  1. `bench/` directory structure created (configs/, workloads/, scripts/, analysis/, results/)
  2. `docker-compose.yml` with profiles: dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs, juicefs, ganesha, rclone, kernel-nfs, samba, dittofs-smb, monitoring
  3. `.env.example` with S3, PostgreSQL, and benchmark configuration variables
  4. DittoFS config files for each backend combination (badger+s3, postgres+s3, badger+fs)
  5. `scripts/check-prerequisites.sh` validates fio, nfs-common, cifs-utils, python3, docker, jq, bc
  6. Only one profile active at a time (no resource contention)
  7. `results/` directory gitignored
**Plans**: 2/2 (COMPLETED)
Plans:
- [x] 33-01-PLAN.md — Docker Compose infrastructure, directory structure, DittoFS configs
- [x] 33-02-PLAN.md — Prerequisites check, cleanup scripts, shared library, Makefile

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

## Progress

**Execution Order:**
v3.6 (30-32.5) → v3.8 (39-44.5) → v4.0 (45-51.5) → v4.1 (33-38.5)

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
| 30. SMB Bug Fixes | 4/4 | Complete    | 2026-02-27 | - |
| 31. Windows ACL Support | 2/3 | In Progress|  | - |
| 32. Windows Integration Testing | v3.6 | 0/3 | Not started | - |
| 39. SMB3 Security & Encryption | v3.8 | 0/? | Not started | - |
| 40. SMB3 Leases & Locking | v3.8 | 0/? | Not started | - |
| 40.5 SMB2/3 Change Notify | v3.8 | 0/? | Not started | - |
| 41. SMB3 Authentication & ACLs | v3.8 | 0/? | Not started | - |
| 42. SMB3 Resilience | v3.8 | 0/? | Not started | - |
| 43. SMB3 Advanced Features & Cross-Protocol | v3.8 | 0/? | Not started | - |
| 44. SMB3 Conformance Testing | v3.8 | 0/? | Not started | - |
| 45. Server-Side Copy | v4.0 | 0/? | Not started | - |
| 46. Clone/Reflinks | v4.0 | 0/? | Not started | - |
| 47. Sparse Files | v4.0 | 0/? | Not started | - |
| 48. Extended Attributes | v4.0 | 0/? | Not started | - |
| 49. NFSv4.2 Operations | v4.0 | 0/? | Not started | - |
| 50. Documentation | v4.0 | 0/? | Not started | - |
| 51. v4.0 Testing | v4.0 | 0/? | Not started | - |
| 33. Benchmark Infrastructure | v4.1 | 2/2 | Complete | 2026-02-27 |
| 34. Benchmark Workloads | v4.1 | 0/? | Not started | - |
| 35. Competitor Setup | v4.1 | 0/? | Not started | - |
| 36. Orchestrator Scripts | v4.1 | 0/? | Not started | - |
| 37. Analysis & Reporting | v4.1 | 0/? | Not started | - |
| 38. Profiling Integration | v4.1 | 0/? | Not started | - |

**Total:** 112/? plans complete

---
*Roadmap created: 2026-02-04*
*v1.0 shipped: 2026-02-07*
*v2.0 shipped: 2026-02-20*
*v3.0 shipped: 2026-02-25*
*v3.5 shipped: 2026-02-26*
*v3.6 roadmap refined: 2026-02-26*
*Priority reorder + benchmark merge: 2026-02-27*
