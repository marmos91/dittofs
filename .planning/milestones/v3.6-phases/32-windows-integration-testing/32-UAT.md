---
status: complete
phase: 32-windows-integration-testing
source: 32-01-SUMMARY.md, 32-02-SUMMARY.md, 32-03-SUMMARY.md
started: 2026-02-28T10:00:00Z
updated: 2026-02-28T12:00:00Z
---

## Current Test

[testing complete]

## Tests

### 1. New SMB handler unit tests pass
expected: Running `go test ./internal/adapter/smb/... -v -count=1` passes all tests including TestHandleCreate_MxAcContext, TestHandleCreate_QFidContext, TestFileCompressionInformation, TestFileAttributeTagInformation
result: pass

### 2. Cross-platform config path tests pass
expected: Running `go test ./pkg/config/ -run TestGetConfigDir -v -count=1` and `go test ./pkg/controlplane/store/ -run TestApplyDefaults -v -count=1` both pass, verifying correct path resolution for the current platform
result: pass

### 3. smbtorture scripts are valid and executable
expected: `test/smb-conformance/smbtorture/run.sh` and `test/smb-conformance/smbtorture/parse-results.sh` are both executable. Running `run.sh --dry-run --profile memory` prints the commands it would execute without actually running Docker containers
result: pass

### 4. smbtorture Docker service runs first baseline
expected: Running `cd test/smb-conformance/smbtorture && make test` starts DittoFS in Docker, runs smbtorture full smb2.* suite, parse-results.sh classifies results into PASS/KNOWN/FAIL/SKIP counts, and a summary is printed. Exit code reflects only NEW (unexpected) failures
result: pass
reported: "Initially 0 known failures matched because bare test names lacked smb2. prefix. Fixed by adding normalization in parse-results.sh (both keyword-prefixed and subunit-style formats now prepend smb2. when missing)."
severity: resolved

### 5. WPTS BVT re-run shows improvement over Phase 29.8 baseline
expected: Running the WPTS BVT suite (`cd test/smb-conformance && make test`) shows improved pass rate over the 133/240 baseline from Phase 29.8, reflecting bug fixes from Phases 30-32 (sparse READ, directory listing, parent dir, oplock break, link count, MxAc, QFid, FileInfoClass)
result: pass
details: "150 passed, 90 known failures, 0 new failures, 95 skipped (335 total). Up from 133 baseline — +17 improvement. All failures are known, CI green."

### 6. Windows testing documentation is comprehensive
expected: `docs/WINDOWS_TESTING.md` contains: VM setup guide (UTM/VirtualBox/Hyper-V), networking instructions, guest auth GPO configuration for Windows 11 24H2, formal validation checklist covering Explorer, cmd.exe, PowerShell, Office, VS Code, NFS client, file size testing, known limitations section, and troubleshooting guide
result: pass

### 7. KNOWN_FAILURES.md reflects Phase 30-32 improvements
expected: `test/smb-conformance/KNOWN_FAILURES.md` has a Status column (Expected/Permanent/Potentially fixed), Phase 30-32 improvement annotations, and a Changelog section tracking baseline evolution
result: pass

### 8. Windows 11 Explorer file operations work via SMB
expected: From a Windows 11 VM: connect via `net use Z: \\host\smbbasic /user:wpts-admin TestPassword01!`, open Explorer, navigate to Z:, and perform create file, create folder, rename, delete, copy, move. Right-click Properties -> Security tab shows proper owner and DACL entries (not "Everyone: Full Control")
result: pass

### 9. Windows 11 cmd.exe and PowerShell operations work
expected: From Windows 11: dir, type, copy, move, ren, del, mkdir, rmdir, icacls, attrib all work from cmd.exe. Get-Item, Get-ChildItem, New-Item, Remove-Item, Get-Acl work from PowerShell. icacls and Get-Acl show proper permissions
result: pass

### 10. smbtorture CI workflow is correctly configured
expected: `.github/workflows/smb-conformance.yml` contains a parallel smbtorture job alongside WPTS, with tiered matrix (memory-only on PRs, full profiles on push/cron), artifact upload, and step summary
result: pass

## Summary

total: 10
passed: 10
issues: 0
pending: 0
skipped: 0

## Gaps

None — all tests passed after parser normalization fix.
