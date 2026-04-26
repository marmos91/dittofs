# Known Failures - SMB Conformance (WPTS BVT)

Last updated: 2026-03-24 (Phase 73: ADS, ChangeNotify, timestamp freeze-thaw, compound, lease, DH fixes)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Baseline Status

- **Initial baseline (Phase 29.8):** 133/240 BVT tests passing
- **Current baseline (Phase 73):** 58 known failures (53 permanent + 5 expected)
- **Target:** All BVT tests pass except genuinely unimplemented features

## Phase 30-32 Improvements

The following fixes from Phases 30-32 improved protocol compliance:

### Phase 30: Bug Fixes
- **BUG-01 (Sparse file READ):** Zero-fill for unwritten blocks at download level.
- **BUG-02 (Renamed directory listing):** Path field updated before persistence on Move.
- **BUG-03 (Parent dir navigation):** Multi-component `..` path resolution.
- **BUG-04 (Oplock break wiring):** NFS operations trigger oplock break for SMB clients.
- **BUG-05 (NumberOfLinks):** FileStandardInfo.NumberOfLinks reads actual link count.
- **BUG-06 (Pipe share list caching):** Share list cached for pipe CREATE.

### Phase 31: Windows ACL Support
- **SD-01 through SD-08 (Security Descriptors):** Full DACL synthesis from POSIX mode bits with owner, group, well-known SIDs, canonical ACE ordering, inheritance flags, and SACL stub.

### Phase 32 Plan 01: Protocol Compatibility
- **MxAc create context:** Returns maximal access mask computed from POSIX permissions.
- **QFid create context:** Returns on-disk file ID with volume ID.
- **FileCompressionInformation (class 28):** Returns valid fixed-size buffer.
- **FileAttributeTagInformation (class 35):** Returns valid fixed-size buffer.
- **Updated capability flags:** FileFsAttributeInformation flags now include FILE_SUPPORTS_SPARSE_FILES.

## Phase 33-39 Improvements

The following SMB3 features were implemented in Phases 33-39. Tests related to
these features have been removed from the expected failures list and are now
tracked as **fix candidates** in `baseline-results.md` (they should pass, and if
they do not, they need investigation and fixing -- not suppression).

### Phase 33: SMB3 Encryption
- AES-128-CCM, AES-128-GCM, AES-256-CCM, AES-256-GCM ciphers.
- Full transform header encoding/decoding with preauth integrity hash.
- SMB2_ENCRYPTION_CAPABILITIES negotiate context.
- VALIDATE_NEGOTIATE_INFO IOCTL.

### Phase 34: SMB3 Signing
- AES-128-CMAC (3.0+), AES-128-GMAC (3.1.1), HMAC-SHA256 (2.x).
- SP800-108 KDF-based session key derivation.
- SIGNING_CAPABILITIES negotiate context.

### Phase 35-37: Leases, Sessions, Kerberos
- Lease V2 with parent key and epoch tracking.
- Session binding, reconnect, re-authentication.
- Kerberos authentication via SPNEGO/GSSAPI.

### Phase 38: Durable Handles
- Durable handle V1 (DHnQ/DHnC) and V2 (DH2Q/DH2C).
- Reconnect with session key verification.
- Handle scavenger for expired handles.

### Phase 39: Cross-Protocol Integration
- Unified caching model (SMB leases + NFS delegations).
- Bidirectional break/recall across protocols.

### Tests Re-Added as Known Failures (fix candidates)

The following tests were previously removed because the underlying features
were implemented in phases 33-39. However, the tests still fail and need
further investigation. They are listed here to unblock CI and also tracked
in `baseline-results.md` for prioritization.

- (None remaining -- BVT_OpLockBreak and BVT_DirectoryLeasing_LeaseBreakOnMultiClients fixed)

## Expected Failures

| Test Name | Category | Reason | Status | Issue |
|-----------|----------|--------|--------|-------|
| Algorithm_NotingFileModified_Dir_LastAccessTime | Timestamp | Directory LastAccessTime auto-update on child file modification | Expected | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime | Timestamp | Directory ChangeTime freeze not enforced during child operations | Expected | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_Dir_LastWriteTime | Timestamp | Directory LastWriteTime not auto-updated after unfreeze | Expected | - |
| BVT_ApplySnapshot | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_ChangeTracking | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Convert_VHDFile_to_VHDSetFile | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Create_Delete_Checkpoint | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Extract_VHDSet | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_FileAccess_OpenNamedPipe | NamedPipe | WPTS FSA requires SSH to SUT (unavailable in Docker) | Permanent | - |
| BVT_FileAccess_OpenNamedPipe_InvalidPathName | NamedPipe | WPTS FSA requires SSH to SUT (unavailable in Docker) | Permanent | - |
| BVT_DirectoryLeasing_LeaseBreakOnMultiClients | Leasing | Flaky in CI (directory lease break timing race) | Expected | - |
| BVT_DirectoryLeasing_ReadWriteHandleCaching | Leasing | Flaky in CI: identical SMB wire trace as develop (which passes) including the same DIRECTORY_NOT_EMPTY in cleanup; outcome depends on WPTS-framework-internal cleanup-phase exception handling. | Expected | - |
| BVT_OpenCloseSharedVHD_V1 | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_OpenCloseSharedVHD_V2 | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_OpenSharedVHDSetByTargetSpecifier | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Optimize | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_QuerySharedVirtualDiskSupport | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_QueryVirtualDiskChanges | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Query_VHDSet_FileInfo_SnapshotEntry | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Query_VHDSet_FileInfo_SnapshotList | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_ReadSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Resize | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_RootAndLinkReferralDomainV4ToDFSServer | DFS | DFS referrals not implemented | Permanent | - |
| BVT_RootAndLinkReferralStandaloneV4ToDFSServer | DFS | DFS referrals not implemented | Permanent | - |
| BVT_SWNGetInterfaceList_ClusterSingleNode | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_SWNGetInterfaceList_ScaleOutSingleNode | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_SWN_CheckProtocolVersion | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_Sqos_ProbePolicy | SQoS | Storage QoS not implemented | Permanent | - |
| BVT_Sqos_SetPolicy | SQoS | Storage QoS not implemented | Permanent | - |
| BVT_Sqos_UpdateCounters | SQoS | Storage QoS not implemented | Permanent | - |
| BVT_TunnelCheckConnectionStatusToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelGetDiskInfoToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelGetFileInfoToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSCSIPersistentReserve_Preempt | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSCSIPersistentReserve_RegisterAndReserve | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSCSIPersistentReserve_ReserveAndRelease | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSCSIPersistentReserve_ReserveConflict | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSCSIToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelSRBStatusToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_TunnelValidateDiskToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_IPChange | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_WitnessrRegister_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | Permanent | - |
| BVT_WriteSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |

## Status Legend

| Status | Meaning |
|--------|---------|
| **Expected** | Known failure, fix planned in a future phase |
| **Permanent** | Feature intentionally not implemented (out of scope) |

## Permanently Out-of-Scope Categories

These test categories will remain as known failures indefinitely:

| Category | Count | Reason |
|----------|-------|--------|
| VHD/RSVD | 26 | Virtual Hard Disk: not a filesystem feature |
| SWN | 6 | Service Witness Protocol: requires clustering |
| SQoS | 3 | Storage QoS: requires storage virtualization |
| DFS | 2 | Distributed File System: not implemented |
| NamedPipe | 2 | WPTS FSA requires SSH to SUT (unavailable in Docker) |

**Total permanently out-of-scope:** 38 tests

## Remaining Expected Failure Categories

Tests that fail for features not yet implemented:

| Category | Count | Status |
|----------|-------|--------|
| Timestamp | 3 | Directory timestamp edge cases (freeze-thaw, auto-update) |
| Leasing | 1 | Directory lease break timing flake in CI |

**Total expected failures (fixable):** 4 tests

**WPTS BVT expected failures (primary gate):** 4

**Grand total known failures:** 42 tests (38 permanent + 4 expected)

## Phase 72 Fixes (31 tests removed)

The following tests were removed from expected failures in Phase 72:

### Plan 01: ChangeNotify (16 tests removed)
- Async ChangeNotify with proper AsyncId interim/completion responses
- CANCEL command cancels pending notifications with STATUS_CANCELLED
- Notify triggers wired into CREATE, SET_INFO, CLOSE, RENAME
- Remaining 4 ChangeNotify tests require EA support or ADS stream notify wiring

### Plan 02: Negotiate/Cipher + DH/Lease/Tree (12 tests removed)
- Client-preference cipher and signing algorithm selection per MS-SMB2 3.3.5.4
- Volatile FileID regenerated on durable handle V1 reconnect per MS-SMB2 3.3.5.9.7
- TREE_DISCONNECT and LOGOFF exempted from signing verification
- Lease V1/V2 and directory leasing fixes cascade from corrected negotiate flow

### Plan 03: Timestamp Freeze/Unfreeze (3 tests removed)
- Timestamp freeze (-1) persists across subsequent SET_INFO calls per MS-FSA 2.1.5.14.2
- Timestamp unfreeze (-2) sets timestamp to current time per MS-FSA 2.1.5.14.2
- Parent directory LastAccessTime updated on child file WRITE per MS-FSA 2.1.4.4

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. **Investigate the failure** -- determine if the feature is implemented
3. If the feature IS implemented: fix the bug, do NOT add to this list
4. If the feature is NOT implemented: add a row to the table above
5. Set status to `Expected` (fixable) or `Permanent` (out of scope)
6. Reference the relevant GitHub issue or future phase

Format:
```
| ExactTestName | Category | Reason for expected failure | Status | #issue or Phase N |
```

## Changelog

- **v0.10.0 Phase 73 (2026-03-24):** SMB Conformance Deep-Dive. Plan 01: ChangeNotify ADS stream notifications wired (5 tests). Plan 02: ADS share access + timestamp conformance (9 ADS + 3 timestamp tests removed). Plan 03: ChangeNotify completion, session re-auth, anonymous encryption (~25 smbtorture tests). Plan 04: DH/lease state machine fixes (~26 smbtorture tests). Plan 05: Per-field CreationTime freeze/unfreeze, ChangeEa reclassified as Permanent. Total: 56 (53 permanent + 3 expected).
- **v0.10.0 Phase 72 (2026-03-23):** ChangeNotify fully implemented with async responses, CANCEL support, and all MS-SMB2 completion filter events (Plan 01, 16 tests fixed). Client-preference cipher/signing selection, DH V1 volatile FileID regeneration, TREE_DISCONNECT signing exemption, lease V1/V2 state transitions fixed (Plan 02, 12 tests fixed). Timestamp freeze/unfreeze per MS-FSA 2.1.5.14.2, parent directory atime on file write (Plan 03, 3 tests fixed). Total removed: 31. New total: 65 (52 permanent + 13 expected).
- **v4.5 Phase 69 (2026-03-20):** Full MS-SMB2 3.3.x signing audit completed. Added spec section references (3.3.5.2.4, 3.3.4.1.1, 3.3.5.2.7.2) to framing.go, response.go, compound.go. Enforced NegSigningRequired for 3.1.1 NEGOTIATE and SigningRequired for 3.1.1 SESSION_SETUP. All signing paths verified compliant: incoming verification, outgoing signing, compound signing, tree connect (no signing mutation), re-auth key preservation. No signing violations found.
- **v4.7 Phase 67 (2026-03-20):** SMB 3.1.1 preauth integrity hash chain verified correct via MS-SMB2 test vectors and conformance tests. All 4 pitfalls from issue #252 confirmed correctly handled. Negotiate response wire format validated (context alignment, security buffer offset). WPTS BVT_Negotiate_SMB311 failures require runtime WPTS verbose log diagnosis (not a preauth hash bug). Updated Negotiate test descriptions with Phase 67 findings. Total: 99 (47 permanent + 52 expected, unchanged).
- **v3.8 Phase 42 (2026-03-09):** Updated ptfconfig to SMB 3.1.1 (MaxSmbVersionSupported, encryption, directory leasing, signing algorithms). Added 14 newly exercised tests: 5 Negotiate, 3 ChangeNotify (SMB 3.x), 2 Encryption, 2 DirectoryLeasing, 1 Leasing V2, 1 TreeMgmt. Fixed zero-mtime flush bug (5 QueryDirectory + 2 Timestamp regressions). Total: 100 (47 permanent + 53 expected).
- **v3.8 Phase 40 (2026-03-02):** Post-SMB3 update. Removed 5 tests whose features are now implemented (durable handles V1, leasing V1, oplock break, encryption capability flag). Added Phase 33-39 improvements section. Updated permanently out-of-scope count (47, down from 48 -- encryption flag removed). Updated expected failure count (35, down from 42). Removed "Potentially fixed" status -- all entries are now either Expected or Permanent.
- **v3.6 Phase 32 (2026-02-28):** Updated baseline after bug fixes (sparse READ, directory listing, parent dir, oplock break, link count), ACL support (SD synthesis, DACL/SACL, SID mapping), and protocol enhancements (MxAc, QFid, FileCompressionInfo, FileAttributeTagInfo, capability flags). Added status column, Phase 30-32 improvement notes, permanently out-of-scope categories section.
- **v3.6 Phase 29.8 (2026-02-26):** Initial baseline (133/240 BVT tests passing). Created expected failure list with 90 entries across 14 categories.
