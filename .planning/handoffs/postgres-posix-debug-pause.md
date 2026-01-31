# Context Handoff: PostgreSQL POSIX Compliance Debug

**Paused:** 2026-01-30
**Branch:** feat/cli
**Status:** Verifying fix

---

## Summary

Debugging PostgreSQL POSIX compliance test failures. Root cause identified and fix applied. Full verification test running in background.

## Root Cause Found

**Problem:** The PostgreSQL `parent_child_map` table has a foreign key constraint:
```sql
FOREIGN KEY (child_id) REFERENCES files(id) ON DELETE CASCADE
```

When `DeleteFile` deletes a file from the `files` table, the CASCADE automatically deletes the corresponding entry from `parent_child_map`. Then when `DeleteChild` runs, it finds no entry to delete and returns "child not found" error, causing RMDIR to fail with ENOENT.

**Fix Applied:** Two changes in `pkg/metadata/store/postgres/transaction.go`:
1. Removed the line `DELETE FROM parent_child_map WHERE child_id = $1` from `DeleteFile()` (the CASCADE already handles this)
2. Removed the `RowsAffected() == 0` check in `DeleteChild()` (idempotent delete - entry may already be gone via CASCADE)

## Current State

- **Uncommitted changes:** 1 file (`pkg/metadata/store/postgres/transaction.go`)
- **Background test running:** Full POSIX test suite with PostgreSQL (~3462 lines of output so far, 39 failures at last check - may be decreasing with the fix)
- **Debug file:** `.planning/debug/postgres-posix-compliance-failures.md`

## To Resume

1. Check if background test completed:
   ```bash
   ps aux | grep prove
   cat /tmp/posix-full-test.log | tail -50
   grep -c "not ok" /tmp/posix-full-test.log
   ```

2. If tests pass, commit the fix:
   ```bash
   git add pkg/metadata/store/postgres/transaction.go
   git commit -m "fix(postgres): remove redundant parent_child_map delete handled by CASCADE"
   ```

3. If tests still failing, resume debug agent:
   - Agent ID: a3e355f
   - Debug file has full investigation history

## Key Files

- `pkg/metadata/store/postgres/transaction.go` - The fixed file
- `.planning/debug/postgres-posix-compliance-failures.md` - Debug session notes
- `/tmp/posix-full-test.log` - Full test output
- `test/posix/configs/config.yaml` - PostgreSQL test config

## Background Processes

- DittoFS server running on port 12049 with PostgreSQL backend
- PostgreSQL container running (docker)
- POSIX test suite (prove) may still be running
