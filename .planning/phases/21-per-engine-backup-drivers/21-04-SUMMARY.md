---
phase: 21-per-engine-backup-drivers
plan: 04
subsystem: metadata-backup
tags: [crc, integrity, badger, postgres, restore, gap-closure]
dependency_graph:
  requires: []
  provides: [crc-before-commit-badger, crc-before-commit-postgres]
  affects: [pkg/metadata/store/badger/backup.go, pkg/metadata/store/postgres/backup.go]
tech_stack:
  added: []
  patterns: [verify-before-commit, collect-then-flush]
key_files:
  created: []
  modified:
    - pkg/metadata/store/badger/backup.go
    - pkg/metadata/store/postgres/backup.go
decisions:
  - Badger collects KV entries into RAM slice before CRC verification (bounded by metadata size)
  - Postgres reorders VerifyCRC before COMMIT using existing deferred ROLLBACK for cleanup
metrics:
  duration: 128s
  completed: "2026-05-27T14:18:26Z"
  tasks_completed: 2
  tasks_total: 2
  files_modified: 2
---

# Phase 21 Plan 04: CRC-Before-Commit Ordering Fix Summary

Fix CRC verification ordering in Badger and Postgres restore paths so corrupt streams never reach durable storage.

## What Changed

### Task 1: Badger -- verify CRC before flushing WriteBatch (81e3b77c)

Restructured the Badger `Restore` method from a stream-and-flush pattern to a collect-verify-flush pattern:

**Before (broken):** Read KV pairs into WriteBatch -> `wb.Flush()` (point of no return) -> `backup.VerifyCRC()` (too late -- data already in Badger).

**After (fixed):** Read KV pairs into `[]kvEntry` slice -> `backup.VerifyCRC()` -> iterate slice writing via WriteBatch -> `wb.Flush()`.

A new `kvEntry` struct (Key/Value `[]byte` fields) holds collected entries. Memory usage is bounded by metadata store size (typically megabytes). The existing `restoreBatchSize` flush cadence is preserved during the write phase.

### Task 2: Postgres -- verify CRC before COMMIT (55521928)

Reordered two lines in the Postgres `Restore` method:

**Before (broken):** Table restore loop ends -> `pgRaw.Exec("COMMIT")` (point of no return) -> `backup.VerifyCRC()` (too late -- deferred ROLLBACK is a no-op after COMMIT).

**After (fixed):** Table restore loop ends -> `backup.VerifyCRC()` -> `pgRaw.Exec("COMMIT")`.

The deferred `ROLLBACK` at the top of the method now correctly cleans up if VerifyCRC fails, because COMMIT has not been issued.

## Verification Results

| Check | Result |
|-------|--------|
| Badger conformance (5 subtests) | PASS |
| Badger race detector (count=3) | PASS |
| Postgres build (integration tags) | PASS |
| go vet (both packages) | PASS |
| VerifyCRC before wb.Flush (Badger) | Line 233 before lines 246/254 |
| VerifyCRC before COMMIT (Postgres) | Line 304 before line 309 |

## Deviations from Plan

None -- plan executed exactly as written.

## Known Stubs

None.

## Self-Check

Verified below.
