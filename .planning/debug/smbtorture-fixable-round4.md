---
status: fixing
trigger: "Fix remaining fixable smbtorture test failures (rename sharing violations, getinfo buffer validation, setinfo attributes, deny table, mkdir, session-id)"
created: 2026-03-01T14:00:00Z
updated: 2026-03-01T16:30:00Z
---

## Current Focus

hypothesis: All 6 root causes identified and fixed, build compiles, unit tests pass
test: Awaiting human verification via smbtorture
expecting: Fixed tests should now pass in smbtorture
next_action: Request human verification

## Symptoms

expected: These ~8-10 smbtorture tests should pass since they test basic SMB2 protocol behavior
actual: Tests fail with wrong status codes or wrong attribute values
errors:
  1. rename tests (3-4) - STATUS_OK instead of STATUS_SHARING_VIOLATION or STATUS_ACCESS_DENIED
  2. getinfo buffer check (2) - STATUS_OK instead of STATUS_INFO_LENGTH_MISMATCH
  3. setinfo (1) - FILE_ATTRIBUTE_ARCHIVE instead of FILE_ATTRIBUTE_READONLY after SET_INFO
  4. deny tests (2) - Share mode deny table results wrong
  5. mkdir (1) - Protocol issue with directory creation
  6. session-id (1) - STATUS_OK instead of STATUS_USER_SESSION_DELETED after logoff
reproduction: cd test/smb-conformance/smbtorture && make test
started: Phase 29.8

## Eliminated

(none - all 6 hypotheses were confirmed)

## Evidence

- timestamp: 2026-03-01T14:10:00Z
  checked: session/manager.go GrantCredits and RequestStarted
  found: GrantCredits calls GetOrCreateSession which silently re-creates deleted sessions after LOGOFF
  implication: Root cause of session-id failure - ghost session allows requests to succeed

- timestamp: 2026-03-01T14:20:00Z
  checked: query_info.go buffer validation
  found: fileInfoClassMinSize only validates file info classes, not filesystem info classes
  implication: Root cause of qfs_buffercheck failure - no min size check for SMB2InfoTypeFilesystem

- timestamp: 2026-03-01T14:30:00Z
  checked: converters.go fileAttrToSMBAttributesInternal and set_info.go
  found: READONLY attribute never set on GET, mode never changed on SET
  implication: Root cause of setinfo failure - two-sided fix needed (GET and SET)

- timestamp: 2026-03-01T14:40:00Z
  checked: create.go disposition handling for directories
  found: FILE_OVERWRITE and FILE_SUPERSEDE dispositions allowed for existing directories
  implication: Root cause of mkdir failure - should return STATUS_INVALID_PARAMETER

- timestamp: 2026-03-01T14:50:00Z
  checked: set_info.go rename handling
  found: No share-delete conflict checking before rename
  implication: Root cause of rename failures - must check all open handles for FILE_SHARE_DELETE

- timestamp: 2026-03-01T15:00:00Z
  checked: create.go share mode enforcement
  found: No share mode conflict checking when opening existing files
  implication: Root cause of deny failures - must implement MS-FSA 2.1.5.1.2 deny table

## Resolution

root_cause: 6 distinct protocol compliance gaps:
  1. GrantCredits/RequestStarted re-create deleted sessions (session-id)
  2. Missing filesystem info class buffer validation (getinfo qfs_buffercheck)
  3. READONLY attribute not mapped in GET or SET directions (setinfo)
  4. Directory overwrite/supersede not rejected (mkdir)
  5. No share-delete conflict check on rename (rename tests)
  6. No share mode deny table during CREATE (deny tests)

fix: 6 targeted fixes applied:
  1. session/manager.go: GrantCredits uses GetSession with fallback, RequestStarted uses GetSession
  2. query_info.go: Added fsInfoClassMinSize() and validation for SMB2InfoTypeFilesystem
  3. converters.go: Added READONLY detection from mode; set_info.go: Added mode computation from FileAttributes
  4. create.go: Added STATUS_INVALID_PARAMETER for directory overwrite/supersede
  5. handler.go: Added checkShareDeleteConflict(); set_info.go: Added conflict check before rename
  6. handler.go: Added checkShareModeConflict() with full MS-FSA deny table; create.go: Added share mode check

verification: go build ./... succeeds, go test ./internal/adapter/smb/... all pass, go test ./... all pass

files_changed:
  - internal/adapter/smb/session/manager.go
  - internal/adapter/smb/v2/handlers/converters.go
  - internal/adapter/smb/v2/handlers/create.go
  - internal/adapter/smb/v2/handlers/handler.go
  - internal/adapter/smb/v2/handlers/query_info.go
  - internal/adapter/smb/v2/handlers/set_info.go
  - test/smb-conformance/smbtorture/KNOWN_FAILURES.md
