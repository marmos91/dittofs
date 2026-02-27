# Known Failures - SMB Conformance (WPTS BVT)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| Algorithm_NotingFileAccessed_Dir_LastAccessTime | Timestamp | LastAccessTime auto-update not implemented | - |
| Algorithm_NotingFileAccessed_File_LastAccessTime | Timestamp | LastAccessTime auto-update not implemented | - |
| Algorithm_NotingFileModified_Dir_LastAccessTime | Timestamp | Timestamp update algorithm not implemented | - |
| Algorithm_NotingFileModified_File_LastAccessTime | Timestamp | Timestamp update algorithm not implemented | - |
| AlternateDataStream_FileShareAccess_AlternateStreamExisted | ADS | ADS share access enforcement not implemented | - |
| AlternateDataStream_FileShareAccess_DataFileExisted | ADS | ADS share access enforcement not implemented | - |
| AlternateDataStream_FileShareAccess_DirectoryExisted | ADS | ADS share access enforcement not implemented | - |
| BVT_AlternateDataStream_DeleteStream_Dir | ADS | ADS management not implemented | - |
| BVT_AlternateDataStream_DeleteStream_File | ADS | ADS management not implemented | - |
| BVT_AlternateDataStream_ListStreams_Dir | ADS | ADS management not implemented | - |
| BVT_AlternateDataStream_ListStreams_File | ADS | ADS management not implemented | - |
| BVT_AlternateDataStream_RenameStream_Dir | ADS | ADS management not implemented | - |
| BVT_AlternateDataStream_RenameStream_File | ADS | ADS management not implemented | - |
| BVT_ApplySnapshot | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_ChangeTracking | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Convert_VHDFile_to_VHDSetFile | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Create_Delete_Checkpoint | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_DurableHandleV1_Reconnect_WithBatchOplock | DurableHandle | Durable handle reconnect not implemented | - |
| BVT_DurableHandleV1_Reconnect_WithLeaseV1 | DurableHandle | Durable handle reconnect not implemented | - |
| BVT_Extract_VHDSet | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_FileAccess_OpenNamedPipe | NamedPipe | Named pipe validation not implemented | - |
| BVT_FileAccess_OpenNamedPipe_InvalidPathName | NamedPipe | Named pipe validation not implemented | - |
| BVT_FsCtl_CreateOrGetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | - |
| BVT_FsCtl_CreateOrGetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | - |
| BVT_FsCtl_GetObjectId_Dir_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | - |
| BVT_FsCtl_GetObjectId_File_IsSupported | NTFS-FsCtl | NTFS object IDs not supported | - |
| BVT_FsCtl_MarkHandle_File_IsSupported | NTFS-FsCtl | FSCTL_MARK_HANDLE not supported | - |
| BVT_FsCtl_Query_File_Regions | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | - |
| BVT_FsCtl_Query_File_Regions_WithInputData | NTFS-FsCtl | FSCTL_QUERY_FILE_REGIONS not supported | - |
| BVT_Leasing_FileLeasingV1 | Leasing | File leasing break notification not implemented | - |
| BVT_OpLockBreak | OpLock | Oplock break notification not implemented | - |
| BVT_OpenCloseSharedVHD_V1 | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_OpenCloseSharedVHD_V2 | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_OpenSharedVHDSetByTargetSpecifier | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Optimize | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_QuerySharedVirtualDiskSupport | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_QueryVirtualDiskChanges | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Query_VHDSet_FileInfo_SnapshotEntry | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Query_VHDSet_FileInfo_SnapshotList | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_ReadSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_Resize | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_RootAndLinkReferralDomainV4ToDFSServer | DFS | DFS referrals not implemented | - |
| BVT_RootAndLinkReferralStandaloneV4ToDFSServer | DFS | DFS referrals not implemented | - |
| BVT_SMB2Basic_CancelRegisteredChangeNotify | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeAttributes | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeCreation | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeDirName | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeEa | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeFileName | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeLastAccess | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeLastWrite | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeSecurity | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeSize | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamName | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamSize | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ChangeStreamWrite | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb2002 | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_MaxTransactSizeCheck_Smb21 | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_NoFileListDirectoryInGrantedAccess | ChangeNotify | Change notification not implemented | - |
| BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close | ChangeNotify | Change notification not implemented | - |
| BVT_SWNGetInterfaceList_ClusterSingleNode | SWN | Service Witness Protocol not implemented | - |
| BVT_SWNGetInterfaceList_ScaleOutSingleNode | SWN | Service Witness Protocol not implemented | - |
| BVT_SWN_CheckProtocolVersion | SWN | Service Witness Protocol not implemented | - |
| BVT_Sqos_ProbePolicy | SQoS | Storage QoS not implemented | - |
| BVT_Sqos_SetPolicy | SQoS | Storage QoS not implemented | - |
| BVT_Sqos_UpdateCounters | SQoS | Storage QoS not implemented | - |
| BVT_TunnelCheckConnectionStatusToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelGetDiskInfoToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelGetFileInfoToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSCSIPersistentReserve_Preempt | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSCSIPersistentReserve_RegisterAndReserve | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSCSIPersistentReserve_ReserveAndRelease | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSCSIPersistentReserve_ReserveConflict | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSCSIToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelSRBStatusToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_TunnelValidateDiskToSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_IPChange | SWN | Service Witness Protocol not implemented | - |
| BVT_WitnessrRegister_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | - |
| BVT_WriteSharedVHD | VHD/RSVD | Virtual Hard Disk not implemented | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime | Timestamp | FSA directory ChangeTime freeze: SetFileAttributes auto-updates Ctime | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_Dir_LastWriteTime | Timestamp | Directory LastWriteTime not auto-updated after unfreeze | - |
| FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_File_LastAccessTime | Timestamp | LastAccessTime auto-update on READ not implemented | - |
| FsCtl_Get_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | - |
| FsCtl_Get_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | - |
| FsCtl_Set_IntegrityInformation_Dir_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | - |
| FsCtl_Set_IntegrityInformation_File_IsIntegritySupported | NTFS-FsCtl | NTFS integrity streams not supported | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsCompressionSupported | FsInfo | Compression not supported | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsEncryptionSupported | FsInfo | Encryption not supported | - |
| FsInfo_Query_FileFsAttributeInformation_File_IsObjectIDsSupported | FsInfo | Object IDs not supported | - |

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Determine the failure category and reason
3. Add a row to the table above
4. Reference the relevant GitHub issue or future phase

Format:
```
| ExactTestName | Category | Reason for expected failure | #issue or Phase N |
```
