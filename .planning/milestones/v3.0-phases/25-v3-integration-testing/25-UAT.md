---
status: complete
phase: 25-v3-integration-testing
source: 25-01-SUMMARY.md, 25-02-SUMMARY.md, 25-03-SUMMARY.md
started: 2026-02-23T10:15:00Z
updated: 2026-02-23T10:30:00Z
---

## Current Test

[testing complete]

## Tests

### 1. Full Build Succeeds
expected: Running `go build ./...` completes with zero errors. All new test files and SMB Kerberos code compile cleanly.
result: pass

### 2. Unit Tests Pass
expected: Running `go test -race ./...` passes all tests. No regressions from the new code (SMB Kerberos handler, test helpers).
result: pass

### 3. NFSv4.1 Mount Framework
expected: `test/e2e/framework/mount.go` has a case "4.1" in MountNFSExportWithVersion. `test/e2e/framework/helpers.go` has a SkipIfNFSv41Unsupported function. Both handle macOS gracefully with t.Skip.
result: pass

### 4. v4.1 Version Parametrization
expected: "4.1" appears in version slices in `test/e2e/nfsv4_basic_test.go` (3 test loops) and `test/e2e/nfsv4_store_matrix_test.go` (4 test loops). Total 7+ version slices now include "4.1".
result: pass

### 5. Coexistence Tests
expected: `test/e2e/nfsv41_coexistence_test.go` exists with TestNFSv41v40Coexistence (6 subtests) and TestNFSv41v3Coexistence (5 subtests) testing bidirectional file visibility between simultaneously mounted NFS versions.
result: pass

### 6. SMB Kerberos Auth Path
expected: `internal/protocol/smb/v2/handlers/session_setup.go` has Kerberos detection placed BEFORE NTLM extraction. A handleKerberosAuth method validates SPNEGO tokens via gokrb5. Handler struct in handler.go has a KerberosProvider field.
result: pass

### 7. SMB Kerberos Unit Tests
expected: `internal/protocol/smb/v2/handlers/session_setup_test.go` has tests for Kerberos detection, auth failure, and NTLM regression (existing NTLM still works after Kerberos addition).
result: pass

### 8. SMB Kerberos E2E and Cross-Protocol Tests
expected: `test/e2e/smb_kerberos_test.go` and `test/e2e/cross_protocol_kerberos_test.go` exist with tests covering SMB Kerberos auth, identity mapping (alice@REALM -> alice), NTLM+Kerberos coexistence, and cross-protocol NFS/SMB identity consistency.
result: pass

### 9. EOS Replay and Session Tests
expected: `test/e2e/nfsv41_session_test.go` exists with 5 test functions: EOS replay on reconnect (log scraping), connection disruption, session lifecycle, multiple concurrent sessions, session recovery after restart.
result: pass

### 10. Directory Delegation Tests
expected: `test/e2e/nfsv41_dirdeleg_test.go` exists with tests covering all 4 CB_NOTIFY mutation types (entry added, removed, renamed, attr changed) plus delegation cleanup on unmount.
result: pass

### 11. Backchannel Delegation Recall Test
expected: `test/e2e/nfsv4_delegation_test.go` has been extended with TestNFSv41BackchannelDelegationRecall verifying CB_RECALL delivered via fore-channel for v4.1 clients.
result: pass

### 12. Disconnect Robustness Tests
expected: `test/e2e/nfsv41_disconnect_test.go` exists with 3 disconnect scenarios (force-close during large write, readdir of 150+ files, session setup) plus forceUnmount helper and checkServerLogs panic/leak detector.
result: pass

## Summary

total: 12
passed: 12
issues: 0
pending: 0
skipped: 0

## Gaps

[none yet]
