---
phase: 05-adapters-auxiliary
plan: 02
subsystem: backup
tags: [backup, e2e, cli, controlplane]
dependency-graph:
  requires: [05-01]
  provides: [backup-e2e-tests, backup-cli-helpers]
  affects: []
tech-stack:
  added: []
  patterns: [json-backup-parsing, server-subprocess-backup]
key-files:
  created:
    - test/e2e/backup_test.go
  modified:
    - test/e2e/helpers/cli.go
    - test/e2e/helpers/server.go
decisions:
  - id: bak-api-only
    summary: Only API-created resources are backed up
    rationale: Config-file defined shares/stores are not persisted to control plane DB
    impact: Tests create resources via CLI before backup
metrics:
  duration: 4 min
  completed: 2026-02-02
---

# Phase 05 Plan 02: Backup E2E Tests Summary

Backup CLI helper functions and E2E test suite for control plane backup operations.

## One-liner

Backup E2E tests validating native/JSON formats with CLI-created resource verification.

## What was built

### Backup Helper Functions (cli.go)

Added backup-related helpers to the CLI runner:

1. **Types for backup parsing:**
   - `ControlPlaneBackup` - Main backup structure with users, groups, shares, stores, adapters
   - `BackupUser` - User representation in backup files
   - `BackupGroup` - Group representation in backup files

2. **Helper functions:**
   - `RunDittofsBackup(t, outputPath, configPath, format)` - Execute dittofs backup controlplane command
   - `ParseBackupFile(t, path)` - Read and parse JSON backup file

3. **ServerProcess enhancement:**
   - `ConfigFile()` - Returns path to server config file (needed for backup command)

### E2E Test Suite (backup_test.go)

Comprehensive test coverage for backup requirements:

| Test | Requirement | Description |
|------|-------------|-------------|
| BAK-01 backup native format | BAK-01 | Verifies native format creates valid DB file |
| BAK-01 backup as JSON | BAK-01 | Verifies JSON format contains users and groups |
| BAK-02 backup includes API-created resources | BAK-02 | Validates shares, metadata stores, payload stores |
| BAK-03 data integrity | BAK-03 | Comprehensive resource verification |
| BAK-04 invalid config | BAK-04 | Graceful failure with invalid config |
| BAK-04 nonexistent database | BAK-04 | Graceful failure with missing DB |

## Key Implementation Details

### Backup Content Model

The backup command captures data stored in the control plane database (SQLite/PostgreSQL):

**Included in backup:**
- Users (all fields except password hash for security)
- Groups (with share permissions)
- Shares created via API
- Metadata stores created via API
- Payload stores created via API
- Adapters (auto-created NFS/SMB)
- Settings

**NOT included in backup:**
- Shares defined in config file (not persisted to DB)
- Stores defined in config file (not persisted to DB)

### Test Pattern

Each test follows the pattern:
1. Start server with StartServerProcess
2. Login as admin with LoginAsAdmin
3. Create test resources via CLI (users, groups, stores, shares)
4. Run backup command with RunDittofsBackup
5. Verify backup content or error handling
6. Stop server gracefully

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed unused import in adapters_test.go**
- **Found during:** go vet verification
- **Issue:** `fmt` package imported but not used
- **Fix:** Linter auto-removed the unused import
- **Files modified:** test/e2e/adapters_test.go

### Clarifications Made

**Config vs API resources:** The plan mentioned testing shares from config, but discovered that config-defined resources are not persisted to the control plane database. Tests were updated to create resources via API (which ARE persisted) before verifying backup content.

## Testing

All tests pass:
```
=== RUN   TestBackupRestore
=== RUN   TestBackupRestore/BAK-01_backup_native_format_creates_valid_file
=== RUN   TestBackupRestore/BAK-01_backup_as_JSON_contains_test_data
=== RUN   TestBackupRestore/BAK-02_backup_includes_API-created_resources
=== RUN   TestBackupRestore/BAK-03_backup_data_integrity_-_all_created_resources_present
=== RUN   TestBackupRestore/BAK-04_invalid_config_fails_gracefully
=== RUN   TestBackupRestore/BAK-04_nonexistent_database_fails_gracefully
--- PASS: TestBackupRestore (1.12s)
```

## Commits

| Hash | Type | Description |
|------|------|-------------|
| 8ae5b2f | feat | Add backup helper functions |
| 8951825 | feat | Add backup E2E tests |

## Next Phase Readiness

Plan complete. Ready for 05-03 (Adapter lifecycle E2E tests) if applicable, or phase completion verification.
