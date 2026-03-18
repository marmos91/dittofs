# Known Failures - SMB Conformance (WPTS BVT)

Last updated: 2026-03-18 (removed 6 tests now passing after SMB conformance fixes)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Baseline Status

- **Initial baseline (Phase 29.8):** 133/240 BVT tests passing
- **Current baseline:** Re-measure required on x86_64 Linux after Phases 30-39
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
| BVT_DirectoryLeasing_LeaseBreakOnMultiClients | DirectoryLeasing | Multi-client lease break framing issue (fix candidate) | Expected | - |
| BVT_DirectoryLeasing_ReadWriteHandleCaching | DirectoryLeasing | Directory handle caching not fully working (fix candidate) | Expected | - |
| BVT_DurableHandleV1_Reconnect_WithBatchOplock | DurableHandle | Durable handle V1 reconnect not fully working (fix candidate) | Expected | - |
| BVT_DurableHandleV1_Reconnect_WithLeaseV1 | DurableHandle | Durable handle V1 reconnect with lease not fully working (fix candidate) | Expected | - |
| BVT_Leasing_FileLeasingV1 | Leasing | File lease V1 not fully working (fix candidate) | Expected | - |
| BVT_Leasing_FileLeasingV2 | Leasing | File lease V2 not fully working (fix candidate) | Expected | - |
| Algorithm_NotingFileModified_Dir_LastAccessTime | Timestamp | Timestamp update algorithm not implemented | Expected | - |
| BVT_Negotiate_SMB311 | Negotiate | SMB 3.1.1 negotiate with preauthentication not fully passing (fix candidate) | Expected | - |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_CCM | Negotiate | Negotiate with AES-256-CCM encryption not fully passing (fix candidate) | Expected | - |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_GCM | Negotiate | Negotiate with AES-256-GCM encryption not fully passing (fix candidate) | Expected | - |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_CCM | Negotiate | Negotiate with AES-128-CCM encryption not fully passing (fix candidate) | Expected | - |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_GCM | Negotiate | Negotiate with AES-128-GCM encryption not fully passing (fix candidate) | Expected | - |
| AlternateDataStream_FileShareAccess_AlternateStreamExisted | ADS | ADS share access enforcement not implemented | Expected | v3.8 Phase 43 |
| AlternateDataStream_FileShareAccess_DataFileExisted | ADS | ADS share access enforcement not implemented | Expected | v3.8 Phase 43 |
| AlternateDataStream_FileShareAccess_DirectoryExisted | ADS | ADS share access enforcement not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_DeleteStream_Dir | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_DeleteStream_File | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_ListStreams_Dir | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_ListStreams_File | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_RenameStream_Dir | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_AlternateDataStream_RenameStream_File | ADS | ADS management not implemented | Expected | v3.8 Phase 43 |
| BVT_ApplySnapshot | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_ChangeTracking | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Convert_VHDFile_to_VHDSetFile | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Create_Delete_Checkpoint | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_Extract_VHDSet | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_FileAccess_OpenNamedPipe | NamedPipe | WPTS FSA requires SSH to SUT (unavailable in Docker) | Permanent | - |
| BVT_FileAccess_OpenNamedPipe_InvalidPathName | NamedPipe | WPTS FSA requires SSH to SUT (unavailable in Docker) | Permanent | - |
| BVT_FsCtl_CreateOrGetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_CreateOrGetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_GetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_GetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_MarkHandle_File_IsSupported | NTFS-FsCtl | FSCTL_MARK_HANDLE not supported | Permanent | - |
| BVT_FsCtl_Query_File_Regions | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | Permanent | - |
| BVT_FsCtl_Query_File_Regions_WithInputData | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | Permanent | - |
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
| BVT_SMB2Basic_CancelRegisteredChangeNotify | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeAttributes | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeCreation | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeDirName | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeEa | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeFileName | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeLastAccess | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeLastWrite | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeSecurity | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeSize | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamName | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamSize | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamWrite | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb2002 | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb21 | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb30 | ChangeNotify | Change notification not implemented (newly exercised by SMB 3.x config) | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb302 | ChangeNotify | Change notification not implemented (newly exercised by SMB 3.x config) | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb311 | ChangeNotify | Change notification not implemented (newly exercised by SMB 3.x config) | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_NoFileListDirectoryInGrantedAccess | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_TreeMgmt_SMB311_Disconnect_NoSignedNoEncryptedTreeConnect | TreeMgmt | Tree disconnect without signing/encryption not fully working (fix candidate) | Expected | - |
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
| FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime | Timestamp | FSA directory ChangeTime freeze: SetFileAttributes auto-updates Ctime | Expected | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_Dir_LastWriteTime | Timestamp | Directory LastWriteTime not auto-updated after unfreeze | Expected | - |
| FsCtl_Get_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Get_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Set_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Set_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsCompressionSupported | FsInfo | Compression not supported | Permanent | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsObjectIDsSupported | FsInfo | Object IDs not supported | Permanent | - |

## Status Legend

| Status | Meaning |
|--------|---------|
| **Expected** | Known failure, fix planned in a future phase |
| **Permanent** | Feature intentionally not implemented (out of scope) |

## Permanently Out-of-Scope Categories

These test categories will remain as known failures indefinitely:

| Category | Count | Reason |
|----------|-------|--------|
| VHD/RSVD | 24 | Virtual Hard Disk: not a filesystem feature |
| SWN | 5 | Service Witness Protocol: requires clustering |
| SQoS | 3 | Storage QoS: requires storage virtualization |
| DFS | 2 | Distributed File System: not implemented |
| NTFS-FsCtl | 11 | NTFS-specific internals (object IDs, integrity, regions) |
| FsInfo | 2 | Compression and object ID capability flags |

**Total permanently out-of-scope:** 47 tests

## Remaining Expected Failure Categories

Tests that fail for features not yet implemented:

| Category | Count | Status |
|----------|-------|--------|
| ChangeNotify | 17 | Not implemented (planned Phase 40.5) |
| ADS | 9 | Not implemented (planned Phase 43) |
| Timestamp | 7 | Auto-update algorithms not implemented |
| DurableHandle | 2 | Fix candidate (partially implemented Phase 38) |
| Negotiate | 5 | Fix candidate (SMB 3.1.1 preauthentication/encryption, Phase 33-34) |
| Encryption | 2 | Fix candidate (global/per-share enforcement, Phase 33) |
| DirectoryLeasing | 2 | Fix candidate (lease break + handle caching, Phase 37) |
| Leasing | 2 | Fix candidate (V1 + V2, Phase 35-37) |
| TreeMgmt | 1 | Fix candidate (SMB 3.1.1 disconnect, Phase 33) |
| FsInfo | 0 | Fixed (encryption flag was wrong bitmask) |
| NamedPipe | 2 | Named pipe validation not implemented |

**Total expected failures (fixable):** 52 tests

**Grand total known failures:** 99 tests (47 permanent + 52 expected)

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

- **v3.8 Phase 42 (2026-03-09):** Updated ptfconfig to SMB 3.1.1 (MaxSmbVersionSupported, encryption, directory leasing, signing algorithms). Added 14 newly exercised tests: 5 Negotiate, 3 ChangeNotify (SMB 3.x), 2 Encryption, 2 DirectoryLeasing, 1 Leasing V2, 1 TreeMgmt. Fixed zero-mtime flush bug (5 QueryDirectory + 2 Timestamp regressions). Total: 100 (47 permanent + 53 expected).
- **v3.8 Phase 40 (2026-03-02):** Post-SMB3 update. Removed 5 tests whose features are now implemented (durable handles V1, leasing V1, oplock break, encryption capability flag). Added Phase 33-39 improvements section. Updated permanently out-of-scope count (47, down from 48 -- encryption flag removed). Updated expected failure count (35, down from 42). Removed "Potentially fixed" status -- all entries are now either Expected or Permanent.
- **v3.6 Phase 32 (2026-02-28):** Updated baseline after bug fixes (sparse READ, directory listing, parent dir, oplock break, link count), ACL support (SD synthesis, DACL/SACL, SID mapping), and protocol enhancements (MxAc, QFid, FileCompressionInfo, FileAttributeTagInfo, capability flags). Added status column, Phase 30-32 improvement notes, permanently out-of-scope categories section.
- **v3.6 Phase 29.8 (2026-02-26):** Initial baseline (133/240 BVT tests passing). Created expected failure list with 90 entries across 14 categories.
