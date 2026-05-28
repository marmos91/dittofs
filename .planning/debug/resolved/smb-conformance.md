---
status: resolved
trigger: "SMB2.1 conformance test failures - server crashes during FSA test sequences"
created: 2026-02-26T10:00:00Z
updated: 2026-02-26T20:00:00Z
---

## Current Focus

hypothesis: N/A - resolved
test: N/A
expecting: N/A
next_action: N/A

## Symptoms

expected: All SMB2.1 feature tests in WPTS BVT should pass
actual: 163 known failures, ~70 are SMB2.1 features that should work. Server crashes during FSA test sequences.
errors: "unexpected end of channel stream: expected N more byte(s)" - server disconnects mid-test
reproduction: Run WPTS conformance tests via `cd test/smb-conformance && make test`
started: Tests set up in Phase 29.8. 79 tests pass, crashes affect many remaining tests.

## Resolution

root_cause: Multiple independent protocol conformance issues across QUERY_INFO, SET_INFO, QueryDirectory, and CREATE handlers. The most impactful single bug was QueryInfoResponse.Encode() placing data at offset 9 instead of 8, violating the MS-SMB2 StructureSize convention (StructureSize=9 means 8-byte fixed part). This alone caused 38+ test failures.

fix: Applied 12+ targeted fixes across handlers:
1. QueryInfoResponse.Encode() offset 9->8 (38+ tests)
2. SET_INFO FileBasicInformation buffer validation (40 bytes)
3. STATUS_INFO_LENGTH_MISMATCH for undersized buffers
4. DOS_STAR wildcard matching (consume through last dot)
5. FileModeInformation with CreateOptions
6. FileNormalizedNameInformation path format
7. NTFS stream suffix stripping in CREATE
8. FileId=0 for "." and ".." entries
9. SET_INFO attribute validation
10. SingleEntry flag continuation in QueryDirectory
11. SMB1 negotiate dialect parsing
12. Error response body encoding

verification: WPTS BVT results: 133 PASS, 107 KNOWN, 0 NEW failures (up from 79 PASS)

files_changed:
- internal/adapter/smb/v2/handlers/query_info.go
- internal/adapter/smb/v2/handlers/set_info.go
- internal/adapter/smb/v2/handlers/query_directory.go
- internal/adapter/smb/v2/handlers/create.go
- internal/adapter/smb/v2/handlers/handler.go
- internal/adapter/smb/v2/handlers/converters.go
- internal/adapter/smb/v2/handlers/query_info_test.go
- internal/adapter/smb/types/status.go
- internal/adapter/smb/types/constants.go
- internal/adapter/smb/response.go
- test/smb-conformance/KNOWN_FAILURES.md
