# GitHub Issue: E2E - Investigate failing tests

## Summary

Several E2E tests are skipped and require investigation. This issue tracks the known failing tests after the NFSv4 metadata persistence fix.

## Skipped Tests

### SMB File Operations (`file_operations_smb_test.go`)
- **TestSMBFileOperations**: SMB-01 through SMB-06
- Status: SMB adapter has implementation issues causing file operation failures

### Cross-Protocol Tests (`cross_protocol_test.go`)
- **TestCrossProtocolInterop**: XPR-01 through XPR-06
- Status: Depends on SMB fixes

### Cross-Protocol Locking (`cross_protocol_lock_test.go`)
- **TestCrossProtocolLocking**: XPRO-01 through XPRO-04
- **TestCrossProtocolLockingByteRange**: Byte-range lock tests
- Status: Depends on SMB fixes

### File Size Matrix (`nfsv4_store_matrix_test.go`)
- **TestFileSizeMatrix**: Large file tests (10MB, 100MB)
- Status: NFSv4 large file handling issues
  - 10MB: Sometimes fails with sync "input/output error"
  - 100MB: Checksum mismatches and I/O errors

### Backup/Restore (`backup_test.go`)
- **TestBackupRestore**: BAK-01 through BAK-05
- Status: Needs investigation

### Control Plane V2 (`controlplane_v2_test.go`)
- **TestControlPlaneV2_SettingsVersionTracking**
- Status: Version tracking test needs investigation

### Grace Period (`grace_period_test.go`)
- **TestGracePeriodWithSMBLeases**
- Status: Depends on SMB fixes

## Environment Notes

- **SMB mount requirements**: `cifs-utils` package must be installed for `mount.cifs` binary
- The CIFS kernel module being loaded is not sufficient - the userspace tools are also required

## Investigation Steps

1. Run the SMB E2E tests in isolation:
   ```bash
   sudo go test -tags=e2e -v ./test/e2e/ -run "SMB"
   ```

2. Check SMB adapter logs for errors:
   ```bash
   DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
   ```

3. Test manual SMB mount:
   ```bash
   sudo mount -t cifs //localhost/export /mnt/test -o username=admin,password=admin123
   ```

4. Run file size matrix tests:
   ```bash
   sudo go test -tags=e2e -v ./test/e2e/ -run "TestFileSizeMatrix"
   ```

## Fixed in This Session

- NFSv4 server restart recovery test (metadata persistence on COMMIT/CLOSE)
- SMB permission enforcement pre-checks (skip if mount unavailable)
- TestStaleNFSHandle (both v3 and v4.0 pass)
- TestStoreMatrixOperations (all 9 store combinations pass)

## Suggested Labels

- bug
- e2e-tests
- smb
- nfsv4
