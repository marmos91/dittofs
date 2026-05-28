---
phase: 21-per-engine-backup-drivers
plan: 03
subsystem: metadata-backup
tags: [postgres, backup, restore, copy-protocol, conformance]
dependency_graph:
  requires: [21-01]
  provides: [postgres-backupable, postgres-backup-conformance]
  affects: [pkg/metadata/store/postgres]
tech_stack:
  added: []
  patterns: [COPY TO/FROM STDOUT/STDIN, REPEATABLE READ snapshot isolation, length-prefixed binary framing]
key_files:
  created:
    - pkg/metadata/store/postgres/backup.go
  modified:
    - pkg/metadata/store/postgres/postgres_conformance_test.go
decisions:
  - CSV format chosen over binary COPY for hash extraction (simpler hex parsing, no binary header complexity)
  - Single TRUNCATE CASCADE for restore cleanup (simpler than reverse-order individual truncations)
  - Table name validation against hardcoded list during restore (T-21-10 SQL injection mitigation)
metrics:
  duration: 3m 46s
  completed: 2026-05-27T13:36:23Z
  tasks_completed: 2
  tasks_total: 2
  files_created: 1
  files_modified: 1
---

# Phase 21 Plan 03: Postgres Backup Driver Summary

Postgres COPY TO/FROM streaming backup with REPEATABLE READ snapshot isolation and dedicated hash extraction query.

## Task Completion

| Task | Name | Commit | Files |
|------|------|--------|-------|
| 1 | Implement Postgres Backup and Restore | 57b747a3 | pkg/metadata/store/postgres/backup.go |
| 2 | Wire Postgres Backup Conformance Suite | c5740c8d | pkg/metadata/store/postgres/postgres_conformance_test.go |

## Implementation Details

### Backup Method
- Acquires raw `*pgconn.PgConn` for wire-level COPY protocol access
- Opens `REPEATABLE READ` transaction for snapshot isolation (ConcurrentWriter conformance)
- Iterates 15 metadata tables in FK-safe dependency order via `COPY <table> TO STDOUT WITH (FORMAT csv, HEADER true)`
- Each table section is length-prefixed (uint16 name length + uint64 data length) for deterministic restore parsing
- Dedicated hash extraction via `COPY (SELECT DISTINCT hash FROM file_block_refs) TO STDOUT WITH (FORMAT csv)` within the same snapshot
- Hex-encoded BYTEA values (`\x...`) parsed into `blockstore.ContentHash` for the HashSet

### Restore Method
- Empty-store guard: `SELECT EXISTS(SELECT 1 FROM shares)` returns `ErrRestoreDestinationNotEmpty`
- Envelope verification: engine tag + schema version + CRC32 integrity
- `TRUNCATE ... CASCADE` clears AutoMigrate-seeded data before COPY FROM
- Table name validated against hardcoded `backupTables` slice (T-21-10 mitigation)
- Per-table `COPY <table> FROM STDIN WITH (FORMAT csv, HEADER true)` from length-delimited stream sections

### Conformance Test Wiring
- Extracted shared `newPostgresStoreFactory()` helper from existing `TestConformance`
- Added `TestBackupConformance` calling `storetest.RunBackupConformanceSuite`
- All 5 subtests wired: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness
- Guarded by `//go:build integration` + `DITTOFS_TEST_POSTGRES_DSN` env var skip

## Deviations from Plan

None -- plan executed exactly as written.

## Self-Check: PASSED

- [x] `pkg/metadata/store/postgres/backup.go` exists
- [x] Commit 57b747a3 verified in git log
- [x] Commit c5740c8d verified in git log
- [x] `go build ./pkg/metadata/store/postgres/` exits 0
- [x] `go vet ./pkg/metadata/store/postgres/` exits 0
- [x] `go build -tags=integration ./pkg/metadata/store/postgres/` exits 0
- [x] No stubs or placeholders found
