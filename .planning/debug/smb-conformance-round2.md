---
status: fixing
trigger: "SMB2.1 WPTS BVT conformance tests - Round 2 debugging"
created: 2026-02-26T19:00:00Z
updated: 2026-02-26T22:25:00Z
---

## Current Focus

hypothesis: Round 2 fixes confirmed: 6 bugs fixed yielding 139 passing tests (up from 133)
test: Verification complete via make test
expecting: All fixed tests pass, no regressions
next_action: Investigate remaining fixable tests (FileIdInformation, timestamp sentinels)

## Symptoms

expected: All SMB2.1 protocol-level tests in WPTS BVT should pass
actual: 133 tests pass, 107 fail, ~95 skipped. Round 1 KNOWN_FAILURES.md had 158 entries but 49 of those now PASS.
errors: Remaining failures include FilePositionInformation, FileIdInformation, Dir CreationTime drift, timestamp sentinel -1/-2 for ChangeTime/LastWriteTime, QueryDirectory FileId for dot/dotdot, SingleEntry flag
reproduction: cd test/smb-conformance && make test
started: Round 2 starts from existing Round 1 fixes

## Eliminated

- hypothesis: "SingleEntry test fails because EnumerationPattern is not tracked, so pattern changes are not detected"
  evidence: The test always uses "*" pattern (no pattern change). The real bug is that specialCount (for . and ..) is only computed on the first call (startingFresh=true) but subsequent calls in the same enumeration have startingFresh=false, making specialCount=0 and causing off-by-2 indexing.
  timestamp: 2026-02-26T22:10:00Z

- hypothesis: "FileIdInformation tests fail because FSCTL_READ_FILE_USN_DATA is missing"
  evidence: FSCTL_READ_FILE_USN_DATA handler was added but FileIdInformation tests still fail. Root cause is deeper (possibly USN_RECORD_V2 structure parsing on client side).
  timestamp: 2026-02-26T21:00:00Z

## Evidence

- timestamp: 2026-02-26T19:00:00Z
  checked: Latest TRX results (2026-02-26_194529)
  found: 133 pass, 107 fail, ~95 skipped. 49 tests from KNOWN_FAILURES.md now pass. 0 new failures (all failures are known).
  implication: Round 1 fixes were highly effective. Round 2 should focus on remaining fixable failures.

- timestamp: 2026-02-26T19:00:00Z
  checked: Categorized 107 remaining failures
  found: VHD/RSVD(24), ChangeNotify(17), ADS(9), FSCTL(12), SWN(6), SQOS(3), DFS(2), NamedPipe(2), Lock(2), Leasing(1), Durable(2), FsInfo(3), Timestamp algo(4) = ~87 unfixable. Fixable: FileInfo query(5) + Timestamp sentinels(9) + QueryDirectory(3) = 17 tests
  implication: Focus on 17 fixable tests. If all fixed, would reach 150 passing tests.

- timestamp: 2026-02-26T21:00:00Z
  checked: Warning status response body format in response.go
  found: StatusNoMoreFiles (0x80000006) is a WARNING, not ERROR. IsError() only checks severity 11, missing severity 10. Per MS-SMB2 2.2.2, any non-SUCCESS status must use 9-byte error body. Server was sending 8-byte QUERY_DIRECTORY body instead.
  implication: Root cause of "expected N more byte(s)" TCP stream errors across multiple tests.

- timestamp: 2026-02-26T21:30:00Z
  checked: FileId for "." and ".." in QueryDirectory
  found: WPTS expects "." FileId != 0 (directory's own reference) and ".." FileId == 0 (parent reference unknown). Fixed both buildFileIdBothDirInfo and buildFileIdFullDirInfo.
  implication: 2 tests fixed (BVT_QueryDirectory_FileIdBothDirectoryInformation, BVT_QueryDirectory_FileIdFullDirectoryInformation)

- timestamp: 2026-02-26T21:50:00Z
  checked: Test results after Warning fix + FileId fix
  found: 135 pass (up from 133), 105 known failures, 0 new failures
  implication: 2 more tests passing from FileId fix. Warning fix prevents framing errors.

- timestamp: 2026-02-26T22:10:00Z
  checked: SingleEntry enumeration bug
  found: specialCount (for . and ..) was only 2 when startingFresh=true (first call). On subsequent calls, startingFresh=false -> specialCount=0 -> off-by-2 indexing. Test enumerates 1002 entries but NO_MORE_FILES returned after 1000 because specialCount drops to 0.
  implication: Fix: use isWildcardSearch instead of includeSpecial for specialCount in SingleEntry block.

- timestamp: 2026-02-26T22:20:00Z
  checked: Test results after SingleEntry fix
  found: 136 pass (up from 135), 104 known failures, 0 new failures
  implication: SingleEntry test now passes. 3 total new tests passing in Round 2.

- timestamp: 2026-02-26T22:45:00Z
  checked: Directory CreationTime drift fix
  found: resolveDirEntryFields(nil, ".") used NowFiletime() for "." and ".." entries, causing timestamps to change between QUERY_DIRECTORY calls. Fixed by fetching directory's actual FileAttr via GetFile() and passing it to builders.
  implication: 2 tests fixed (FileInfo_Query_FileBothDirectoryInformation_Dir_CreationTime, FileInfo_Query_FileFullDirectoryInformation_Dir_CreationTime)

- timestamp: 2026-02-26T22:45:00Z
  checked: FilePositionInformation SET_INFO fix verified
  found: Added FilePositionInformation (class 14) handler to SET_INFO that checks buffer < 8 -> INFO_LENGTH_MISMATCH, else accepts as no-op. Test now passes.
  implication: 1 test fixed (FileInfo_Query_FilePositionInformation)

- timestamp: 2026-02-26T22:45:00Z
  checked: Test results after CreationTime + FilePositionInfo fixes
  found: 139 pass (up from 136), 101 known failures, 0 new failures
  implication: 3 more tests passing. Total Round 2: 6 new tests passing (133 -> 139).

## Resolution

root_cause: Six bugs found and fixed:
  1. Warning status responses used command body instead of error body (response.go)
  2. FileId for ".." was non-zero instead of 0 in QueryDirectory (query_directory.go)
  3. SingleEntry enumeration miscounted special entries on non-first calls (query_directory.go)
  4. Directory CreationTime drift: "." and ".." entries used NowFiletime() instead of actual dir times
  5. FilePositionInformation SET_INFO returned NOT_SUPPORTED instead of handling class 14
  Plus: search pattern change detection added (query_directory.go, handler.go)

fix: |
  1. response.go: Changed error body check from IsError() to (IsError() || IsWarning()) with exceptions
  2. query_directory.go: Set ".." FileId=0 in buildFileIdBothDirInfo and buildFileIdFullDirInfo
  3. query_directory.go: Use isWildcardSearch (not includeSpecial) for specialCount in SingleEntry
  4. handler.go + query_directory.go: Added EnumerationPattern tracking and pattern change reset
  5. query_directory.go: Pass actual dirAttr to buildDirInfoEntries for "." and ".." entries
  6. set_info.go: Added FilePositionInformation case (buffer < 8 -> INFO_LENGTH_MISMATCH, else no-op)

verification: |
  139 pass, 101 known failures, 0 new failures (results/2026-02-26_224219)
  6 new tests passing in Round 2:
    - BVT_QueryDirectory_FileIdBothDirectoryInformation
    - BVT_QueryDirectory_FileIdFullDirectoryInformation
    - Fs_CreateFiles_QueryDirectory_With_Single_Entry_Flag
    - FileInfo_Query_FileBothDirectoryInformation_Dir_CreationTime
    - FileInfo_Query_FileFullDirectoryInformation_Dir_CreationTime
    - FileInfo_Query_FilePositionInformation

files_changed:
  - internal/adapter/smb/response.go
  - internal/adapter/smb/v2/handlers/query_directory.go
  - internal/adapter/smb/v2/handlers/handler.go
  - internal/adapter/smb/v2/handlers/stub_handlers.go
  - test/smb-conformance/KNOWN_FAILURES.md
