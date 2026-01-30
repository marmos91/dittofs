---
status: resolved
trigger: "postgres-posix-compliance-failures"
created: 2026-01-30T10:00:00Z
updated: 2026-01-30T14:30:00Z
---

## Current Focus

hypothesis: CONFIRMED - PostgreSQL CASCADE DELETE on parent_child_map.child_id foreign key deletes the entry before DeleteChild runs
test: Running POSIX compliance tests with PostgreSQL backend to verify fix
expecting: POSIX tests should now pass with PostgreSQL, specifically link/rmdir tests that were failing before
next_action: Setup and run POSIX tests with PostgreSQL, check for failures

## Symptoms

expected: PostgreSQL POSIX compliance tests should pass like memory and badger stores do
actual: Tests are failing with ENOENT when cleaning up directories after tests
errors: "tried 'rmdir pjdfstest_*', expected 0, got ENOENT"
reproduction: Run PostgreSQL POSIX compliance tests locally with sudo
started: Recent - possibly after recent PRs, develop branch seemed to pass historically

## Eliminated

- hypothesis: DeleteFile incorrectly deletes parent_child_map WHERE child_id = $1
  evidence: Removed that line but tests still failed - the CASCADE on foreign key is the real cause
  timestamp: 2026-01-30T08:55:00Z

## Evidence

- timestamp: 2026-01-30T08:45:00Z
  checked: link tests with memory store
  found: All 359 tests pass (Result: PASS)
  implication: Memory store implementation is correct

- timestamp: 2026-01-30T08:50:00Z
  checked: link tests with PostgreSQL store
  found: 25/359 tests failed (Result: FAIL)
  implication: PostgreSQL store has different behavior

- timestamp: 2026-01-30T09:35:00Z
  checked: Manual test - mkdir test123, check parent_child_map, rmdir test123
  found: parent_child_map had entry after mkdir, but was EMPTY after rmdir attempt (before rmdir completed)
  implication: Something deletes the entry before DeleteChild runs

- timestamp: 2026-01-30T09:40:00Z
  checked: Foreign key constraints on parent_child_map
  found: FOREIGN KEY (child_id) REFERENCES files(id) ON DELETE CASCADE
  implication: When DeleteFile deletes from files table, CASCADE deletes the parent_child_map entry

- timestamp: 2026-01-30T09:42:00Z
  checked: RemoveDirectory flow with PostgreSQL
  found: DeleteFile runs first (deletes from files), CASCADE deletes parent_child_map entry, then DeleteChild fails because entry is gone
  implication: The CASCADE DELETE on child_id causes DeleteChild to find RowsAffected==0

## Resolution

root_cause: The PostgreSQL parent_child_map table has a foreign key constraint with ON DELETE CASCADE: `FOREIGN KEY (child_id) REFERENCES files(id) ON DELETE CASCADE`. When `DeleteFile` deletes a file from the `files` table, the CASCADE automatically deletes the corresponding entry from `parent_child_map`. Then when `DeleteChild` runs, it finds no entry to delete and returns "child not found" error.

fix: Modify DeleteChild to NOT return an error when RowsAffected==0. If the entry is already gone (deleted by CASCADE), that's the desired outcome - the child mapping no longer exists. This matches the semantic intent: ensure the child mapping doesn't exist.

verification: VERIFIED - Fix successfully resolves the CASCADE DELETE issue
- All rmdir tests pass with PostgreSQL (145/145 tests, Result: PASS)
- All link tests pass with memory backend (359/359 tests, Result: PASS)
- Most link tests pass with PostgreSQL (346/359 tests pass)
- Remaining failure in link/03.t is a DIFFERENT issue (ENAMETOOLONG handling, not CASCADE DELETE)

files_changed:
  - pkg/metadata/store/postgres/transaction.go
