---
status: awaiting_human_verify
trigger: "SMB2.1 WPTS BVT conformance — 145 pass, 95 known failures. Round 3 focuses on the last potentially fixable tests."
created: 2026-02-27T10:00:00Z
updated: 2026-02-27T11:30:00Z
---

## Current Focus

hypothesis: All three root causes confirmed and fixed, verified with test suite
test: Full WPTS BVT suite run
expecting: 150 pass, 90 known, 0 new failures
next_action: Await human verification

## Symptoms

expected: All fixable SMB2.1 protocol tests should pass
actual: 145 pass, 95 known failures. Potentially fixable: FileIdInformation (2), LockAndUnLock (1), Timestamp auto-update (4), Timestamp sentinel (3)
errors: No server crashes. Tests cleanly report pass/fail.
reproduction: cd test/smb-conformance && make test
started: Round 2 just completed (145 pass). Round 3 starts now.

## Eliminated

- hypothesis: FSCTL_GET_NTFS_VOLUME_DATA uses code 0x00090060
  evidence: Server logs show actual code 0x00090064. CTL_CODE(9, 25, 0, 0) = (9<<16)|(25<<2) = 0x90064
  timestamp: 2026-02-27T11:10

## Evidence

- timestamp: 2026-02-27T10:05
  checked: FileIdInformation test output and WPTS source code
  found: Test sends FsCtlReadFileUSNData(3, 3) requesting USN_RECORD_V3, then TypeMarshal.ToStruct<USN_RECORD_V3>(). We return USN_RECORD_V2 (MajorVersion=2). V3 uses FILE_ID_128 (16 bytes) for FileReferenceNumber vs V2's 8-byte DWORDLONG. Client tries to parse 80-byte V2 record as V3 and runs out of bytes.
  implication: Must return USN_RECORD_V3 when MaxMajorVersion >= 3 in FSCTL_READ_FILE_USN_DATA input

- timestamp: 2026-02-27T10:10
  checked: LockAndUnLock test output
  found: "All opens MUST NOT be allowed to write within the range when SMB2_LOCKFLAG_SHARED_LOCK set, actually server returns STATUS_SUCCESS." Test: Client1 takes SHARED lock, then Client1 writes to same range. Our CheckIOConflict returns false (no conflict) when sessionID matches, allowing the write.
  implication: SMB shared locks must block writes even from the same session. This differs from POSIX read locks.

- timestamp: 2026-02-27T10:15
  checked: WPTS source code for FileIdInformation test (github.com/microsoft/WindowsProtocolTestSuites)
  found: Line 60: `status = this.fsaAdapter.FsCtlReadFileUSNData(3, 3, out outputBuffer);` followed by `USN_RECORD_V3 record = TypeMarshal.ToStruct<USN_RECORD_V3>(outputBuffer);` - confirms V3 is required.
  implication: Need to implement USN_RECORD_V3 format with 16-byte FILE_ID_128 for FileReferenceNumber and ParentFileReferenceNumber

- timestamp: 2026-02-27T11:00
  checked: WPTS source for FsCtl_Get_NTFS_Volume_Data and FileIdInformation step 4
  found: After USN_RECORD_V3 fix, test proceeds to FSCTL_GET_NTFS_VOLUME_DATA. Test verifies VolumeSerialNumber, TotalClusters, BytesPerSector all match between NTFS_VOLUME_DATA_BUFFER, FileIdInformation, and FileFsFullSizeInformation.
  implication: Need FSCTL_GET_NTFS_VOLUME_DATA handler returning consistent values

- timestamp: 2026-02-27T11:10
  checked: Server logs for FSCTL code received
  found: WPTS sends ctlCode=0x00090064 (not 0x00090060). CTL_CODE(FILE_DEVICE_FILE_SYSTEM=9, Function=25, METHOD_BUFFERED=0, FILE_ANY_ACCESS=0) = (9<<16)|(25<<2) = 0x90064
  implication: Initial constant value was wrong; corrected to 0x00090064

- timestamp: 2026-02-27T11:25
  checked: Full test suite after all fixes
  found: 150 pass, 90 known failures, 0 new failures. All 5 target tests now pass.
  implication: All fixes verified working

## Resolution

root_cause:
  1. FileIdInformation: FSCTL_READ_FILE_USN_DATA always returned USN_RECORD_V2 but WPTS requests V3 (with FILE_ID_128). Client deserialization failed because V2 buffer too small for V3 struct.
  2. FileIdInformation + FsCtl_Get_NTFS_Volume_Data: FSCTL_GET_NTFS_VOLUME_DATA (0x00090064) not implemented. Test step 4 requires NTFS_VOLUME_DATA_BUFFER with matching VolumeSerialNumber, TotalClusters, BytesPerSector.
  3. LockAndUnLock: CheckIOConflict skipped conflict check for same-session, allowing writes through shared locks from the lock holder. SMB spec requires shared locks to block ALL writes.
fix:
  1. handleReadFileUsnData: Parse READ_FILE_USN_DATA input for MaxMajorVersion, return V3 format (76-byte header with FILE_ID_128) when MaxMajorVersion >= 3.
  2. handleGetNtfsVolumeData: New FSCTL handler returning 96-byte NTFS_VOLUME_DATA_BUFFER with VolumeSerialNumber=0x12345678 (matching FileIdInformation), TotalClusters and BytesPerSector from GetFilesystemStatistics (matching FileFsFullSizeInformation).
  3. CheckIOConflict: Same session + write + shared lock = BLOCK (was: ALLOW). Same session + write + exclusive lock = ALLOW (lock holder writes OK).
verification: Full suite: 150 pass (+5), 90 known failures (-5), 0 new failures
files_changed:
  - internal/adapter/smb/v2/handlers/stub_handlers.go
  - pkg/metadata/lock/manager.go
  - test/smb-conformance/KNOWN_FAILURES.md
