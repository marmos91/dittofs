---
phase: 21-per-engine-backup-drivers
plan: 02
subsystem: metadata-backup
tags: [badger, backup, restore, conformance]
dependency_graph:
  requires: [21-01]
  provides: [badger-backupable]
  affects: [pkg/metadata/store/badger]
tech_stack:
  added: []
  patterns: [custom-kv-streaming, mvcc-snapshot-backup, write-batch-restore]
key_files:
  created:
    - pkg/metadata/store/badger/backup.go
  modified:
    - pkg/metadata/store/badger/badger_conformance_test.go
decisions:
  - "Used custom length-prefixed KV stream (not Badger built-in backup) for portability per D-02"
  - "Used db.View() MVCC snapshot for consistency isolation during concurrent writes"
  - "Used WriteBatch with 10k-entry periodic flush for restore to handle large databases"
  - "JSON decode errors on f: entries logged as warnings, not fatal, per T-21-08 threat mitigation"
metrics:
  duration: 98s
  completed: 2026-05-27T13:33:43Z
  tasks_completed: 2
  tasks_total: 2
  files_created: 1
  files_modified: 1
requirements_completed: [DRV-02, DRV-04]
---

# Phase 21 Plan 02: Badger Backup Driver Summary

Badger Backup/Restore using custom length-prefixed KV streaming inside db.View() MVCC snapshot with inline hash extraction from f: prefix entries.

## Tasks

| # | Name | Commit | Files |
|---|------|--------|-------|
| 1 | Implement Badger Backup and Restore | 59aa9bdd | pkg/metadata/store/badger/backup.go |
| 2 | Wire Badger Backup Conformance Suite | 7834aca5 | pkg/metadata/store/badger/badger_conformance_test.go |

## Implementation Details

### Backup Method
- Creates envelope via `backup.NewWriter` with `badgerEngineTag = "badger"`
- Writes schema version uint32 LE (version 1) at payload start
- Enters `db.View()` for MVCC snapshot isolation
- Iterates all KV pairs with prefetch enabled (PrefetchSize=100)
- Each pair encoded as: key_len (uint32 LE) + key + value_len (uint32 LE) + value
- Stream terminated by sentinel key_len=0
- Hash extraction inline: f: prefix entries decoded as metadata.File, BlockRef hashes added to HashSet
- Malformed f: entries logged as warnings, not fatal (T-21-08 threat mitigation)
- Trailing CRC written via envW.Finish()

### Restore Method
- Empty-store detection via s: prefix seek in db.View()
- Envelope header read + engine tag verification + schema version check
- KV pairs read in loop until sentinel key_len=0
- WriteBatch with periodic flush every 10k entries for large-database safety
- CRC verification via backup.VerifyCRC on original reader

### Conformance Results
All 5 subtests pass under `-tags=integration`:
- RoundTrip: backup-then-restore produces identical shares and files
- ConcurrentWriter: snapshot isolation verified (concurrent writes excluded)
- Corruption: truncated, bit-flip, and wrong engine tag all detected
- NonEmptyDest: ErrRestoreDestinationNotEmpty returned correctly
- HashSetCorrectness: exact hash match and dedup verification

Race detector clean (`go test -race`).

## Deviations from Plan

None - plan executed exactly as written.

## Threat Mitigations Applied

| Threat | Mitigation |
|--------|-----------|
| T-21-05 (Restore KV injection) | CRC32 Castagnoli verification, schema version check, engine tag match |
| T-21-06 (Large backup DoS) | WriteBatch with periodic flush prevents OOM on restore |
| T-21-08 (Malformed f: entry) | JSON decode errors logged as warnings, do not abort backup |

## Self-Check: PASSED
