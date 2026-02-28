# Known Failures - SMB Conformance (WPTS BVT)

Last updated: 2026-02-28 (Phase 32 v3.6 update)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Baseline Status

- **Initial baseline (Phase 29.8):** 133/240 BVT tests passing
- **Current baseline:** Re-measure required after Phases 30-32 fixes (see [Phase 30-32 Improvements](#phase-30-32-improvements))
- **Target:** Maintain >= 150 BVT tests passing

## Phase 30-32 Improvements

The following fixes from Phases 30-32 may have improved the WPTS BVT pass rate.
Re-run the full suite to establish the updated baseline.

### Phase 30: Bug Fixes
- **BUG-01 (Sparse file READ):** Zero-fill for unwritten blocks at download level. May fix tests that read from sparse file regions.
- **BUG-02 (Renamed directory listing):** Path field updated before persistence on Move. May fix QueryDirectory tests after rename operations.
- **BUG-03 (Parent dir navigation):** Multi-component `..` path resolution. May fix path traversal tests.
- **BUG-04 (Oplock break wiring):** NFS operations trigger oplock break for SMB clients. Unlikely to directly affect WPTS (single-protocol tests).
- **BUG-05 (NumberOfLinks):** FileStandardInfo.NumberOfLinks reads actual link count. May fix FileStandardInformation tests.
- **BUG-06 (Pipe share list caching):** Share list cached for pipe CREATE. May fix named pipe connection tests.

### Phase 31: Windows ACL Support
- **SD-01 through SD-08 (Security Descriptors):** Full DACL synthesis from POSIX mode bits with owner, group, well-known SIDs, canonical ACE ordering, inheritance flags, and SACL stub. May fix ACL-related tests including:
  - Tests querying OWNER_SECURITY_INFORMATION
  - Tests querying DACL_SECURITY_INFORMATION
  - Tests querying SACL_SECURITY_INFORMATION (previously returned empty/error)
  - Tests setting SECURITY_INFORMATION via SET_INFO

### Phase 32 Plan 01: Protocol Compatibility
- **MxAc create context:** Returns maximal access mask computed from POSIX permissions. May fix tests expecting maximal access in CREATE response.
- **QFid create context:** Returns on-disk file ID with volume ID. May fix tests expecting file identity information.
- **FileCompressionInformation (class 28):** Returns valid fixed-size buffer. May fix FileCompressionInformation queries.
- **FileAttributeTagInformation (class 35):** Returns valid fixed-size buffer. May fix FileAttributeTagInformation queries.
- **Updated capability flags:** FileFsAttributeInformation flags now include FILE_SUPPORTS_SPARSE_FILES (0xCF). May fix FileFsAttributeInformation tests checking for sparse file support.

## Expected Failures

| Test Name | Category | Reason | Status | Issue |
|-----------|----------|--------|--------|-------|
| Algorithm_NotingFileAccessed_Dir_LastAccessTime | Timestamp | LastAccessTime auto-update not implemented | Expected | - |
| Algorithm_NotingFileAccessed_File_LastAccessTime | Timestamp | LastAccessTime auto-update not implemented | Expected | - |
| Algorithm_NotingFileModified_Dir_LastAccessTime | Timestamp | Timestamp update algorithm not implemented | Expected | - |
| Algorithm_NotingFileModified_File_LastAccessTime | Timestamp | Timestamp update algorithm not implemented | Expected | - |
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
| BVT_DurableHandleV1_Reconnect_WithBatchOplock | DurableHandle | Durable handle reconnect not implemented | Expected | v3.8 Phase 42 |
| BVT_DurableHandleV1_Reconnect_WithLeaseV1 | DurableHandle | Durable handle reconnect not implemented | Expected | v3.8 Phase 42 |
| BVT_Extract_VHDSet | VHD/RSVD | Virtual Hard Disk not implemented | Permanent | - |
| BVT_FileAccess_OpenNamedPipe | NamedPipe | Named pipe validation not implemented | Expected | - |
| BVT_FileAccess_OpenNamedPipe_InvalidPathName | NamedPipe | Named pipe validation not implemented | Expected | - |
| BVT_FsCtl_CreateOrGetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_CreateOrGetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_GetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_GetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | Permanent | - |
| BVT_FsCtl_MarkHandle_File_IsSupported | NTFS-FsCtl | FSCTL_MARK_HANDLE not supported | Permanent | - |
| BVT_FsCtl_Query_File_Regions | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | Permanent | - |
| BVT_FsCtl_Query_File_Regions_WithInputData | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | Permanent | - |
| BVT_Leasing_FileLeasingV1 | Leasing | File leasing break notification not implemented | Expected | v3.8 Phase 40 |
| BVT_OpLockBreak | OpLock | Oplock break notification not fully wired | Potentially fixed | Phase 30 BUG-04 |
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
| BVT_SMB2Basic_ChangeNotify_NoFileListDirectoryInGrantedAccess | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
| BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close | ChangeNotify | Change notification not implemented | Expected | v3.8 Phase 40.5 |
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
| FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_File_LastAccessTime | Timestamp | LastAccessTime auto-update on READ not implemented | Expected | - |
| FsCtl_Get_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Get_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Set_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsCtl_Set_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | Permanent | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsCompressionSupported | FsInfo | Compression not supported | Permanent | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsEncryptionSupported | FsInfo | Encryption not supported | Permanent | v3.8 Phase 39 |
| FsInfo_Query_FileFsAttributeInformation_File_IsObjectIDsSupported | FsInfo | Object IDs not supported | Permanent | - |

## Status Legend

| Status | Meaning |
|--------|---------|
| **Expected** | Known failure, fix planned in a future phase |
| **Permanent** | Feature intentionally not implemented (out of scope) |
| **Potentially fixed** | May pass after Phase 30-32 fixes; re-run suite to confirm |

## Permanently Out-of-Scope Categories

These test categories will remain as known failures indefinitely:

| Category | Count | Reason |
|----------|-------|--------|
| VHD/RSVD | 24 | Virtual Hard Disk: not a filesystem feature |
| SWN | 5 | Service Witness Protocol: requires clustering |
| SQoS | 3 | Storage QoS: requires storage virtualization |
| DFS | 2 | Distributed File System: not implemented |
| NTFS-FsCtl | 11 | NTFS-specific internals (object IDs, integrity, regions) |
| FsInfo | 3 | Compression, encryption, object ID capability flags |

**Total permanently out-of-scope:** 48 tests

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Determine the failure category and reason
3. Add a row to the table above
4. Set status to `Expected` (fixable) or `Permanent` (out of scope)
5. Reference the relevant GitHub issue or future phase

Format:
```
| ExactTestName | Category | Reason for expected failure | Status | #issue or Phase N |
```

## Changelog

- **v3.6 Phase 32 (2026-02-28):** Updated baseline after bug fixes (sparse READ, directory listing, parent dir, oplock break, link count), ACL support (SD synthesis, DACL/SACL, SID mapping), and protocol enhancements (MxAc, QFid, FileCompressionInfo, FileAttributeTagInfo, capability flags). Added status column, Phase 30-32 improvement notes, permanently out-of-scope categories section.
- **v3.6 Phase 29.8 (2026-02-26):** Initial baseline (133/240 BVT tests passing). Created expected failure list with 90 entries across 14 categories.
