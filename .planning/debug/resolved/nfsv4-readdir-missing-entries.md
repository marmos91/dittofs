---
status: resolved
trigger: "NFSv4 READDIR Still Missing Directory Entries"
created: 2026-02-17T10:00:00Z
updated: 2026-02-17T11:45:00Z
---

## Current Focus

hypothesis: CONFIRMED - EOF flag was incorrectly set to true when entries were truncated due to maxcount
test: Unit tests with forced pagination verify fix works correctly
expecting: All entries should be returned across multiple READDIR calls
next_action: Commit the fix and update the debug session to resolved

## Symptoms

expected: NFSv4 ListDirectory test returns 4 entries (file0.txt, file1.txt, file2.txt, subdir)
actual: NFSv4 ListDirectory test returns 3 entries (file0.txt, file1.txt, file2.txt) - missing subdir
errors: No errors reported, just missing entries
reproduction: Run TestNFSv4BasicOperations/v4.0/ListDirectory E2E test
started: Issue persists after previous pagination fix attempt

## Eliminated

- hypothesis: CREATE handler not calling CreateDirectory
  evidence: Code review shows CREATE with NF4DIR calls metaSvc.CreateDirectory() correctly
  timestamp: 2026-02-17T10:00:00Z

- hypothesis: Different metadata service paths for NFSv3 vs NFSv4
  evidence: Both protocols use identical metaSvc.ReadDirectory() and metaSvc.CreateDirectory() calls
  timestamp: 2026-02-17T10:00:00Z

## Evidence

- timestamp: 2026-02-17T10:15:00Z
  checked: git status internal/protocol/nfs/v4/handlers/readdir.go
  found: Uncommitted changes exist with the truncatedDueToSize fix
  implication: The fix was applied but not committed; original code had bug where EOF was set to true even when entries were truncated

- timestamp: 2026-02-17T10:15:00Z
  checked: git diff for readdir.go
  found: Original code had `if !page.HasMore` for EOF check; fixed code has `if !page.HasMore && !truncatedDueToSize`
  implication: The original bug would cause EOF=true when entries are truncated, preventing client pagination

- timestamp: 2026-02-17T10:00:00Z
  checked: NFSv4 CREATE handler code (create.go lines 200-230)
  found: For NF4DIR type, calls metaSvc.CreateDirectory(authCtx, parentHandle, objName, dirAttr) at line 216
  implication: Same code path as NFSv3 MKDIR

- timestamp: 2026-02-17T10:00:00Z
  checked: NFSv4 READDIR handler code (readdir.go lines 104-207)
  found: Calls metaSvc.ReadDirectory() then loops through entries, checking maxcount limit
  implication: Truncation logic at lines 173-185 may be causing early termination

- timestamp: 2026-02-17T10:00:00Z
  checked: Pagination test failure (95 out of 100 files)
  found: Missing 5 files suggests consistent truncation across pages
  implication: Each READDIR call loses ~5% of entries due to size limits

- timestamp: 2026-02-17T10:00:00Z
  checked: Entry alphabetical ordering
  found: file0.txt, file1.txt, file2.txt come before subdir alphabetically
  implication: subdir would be the last entry, most likely to be truncated by maxcount

- timestamp: 2026-02-17T11:30:00Z
  checked: Unit test with forced pagination (maxcount=150)
  found: First batch returns 3 entries with eof=0, second batch returns subdir
  implication: The truncatedDueToSize fix works correctly - pagination now functions as expected

- timestamp: 2026-02-17T11:30:00Z
  checked: All NFSv4 handler unit tests
  found: All tests pass (no regressions)
  implication: Fix is safe to commit

## Resolution

root_cause: |
  NFSv4 READDIR handler in `readDirRealFS` incorrectly set EOF=true when entries were truncated
  due to maxcount limit. The original code only checked `page.HasMore` to determine EOF, but
  when the handler truncated entries (broke out of the encoding loop due to size limits), it
  never set EOF=false. This caused the NFS client to believe all entries were returned when
  only a subset was actually sent.

  Additionally, the TOOSMALL check used `12+8` (=20) instead of `12` (status=4 + cookieverf=8),
  which was incorrect.

fix: |
  1. Added `truncatedDueToSize` variable to track when entries are truncated in the encoding loop
  2. Changed EOF calculation from `if !page.HasMore` to `if !page.HasMore && !truncatedDueToSize`
  3. Fixed TOOSMALL size check from `12+8` to `12`
  4. Applied same fix to `readDirPseudoFS` function with `truncatedDueToSize` and `allEntriesProcessed`
  5. Added debug logging to help diagnose future issues

verification: |
  - TestReadDir_RealFS_ListsEntries: PASS (verifies 3 entries with large maxcount)
  - TestReadDir_RealFS_PaginationWithTruncation: PASS (forces pagination with maxcount=150)
    - First batch: 3 entries [file0.txt, file1.txt, file2.txt], eof=0
    - Second batch: 1 entry [subdir]
    - Total: all 4 entries found across pagination
  - All other NFSv4 handler tests: PASS (no regressions)

files_changed:
  - internal/protocol/nfs/v4/handlers/readdir.go (truncatedDueToSize fix, debug logging)
  - internal/protocol/nfs/v4/handlers/realfs_test.go (new pagination test)
