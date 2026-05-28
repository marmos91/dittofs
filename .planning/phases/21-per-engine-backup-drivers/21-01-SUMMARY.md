---
phase: 21-per-engine-backup-drivers
plan: 01
subsystem: metadata/backup
tags: [backup, memory-store, gob, conformance]
dependency_graph:
  requires: [Phase 20 Backupable interface, Phase 20 envelope, Phase 20 conformance suite]
  provides: [memory Backupable implementation, memory backup conformance passing]
  affects: [pkg/metadata/store/memory, pkg/metadata/storetest]
tech_stack:
  added: []
  patterns: [gob serialization with length-prefix framing, signalWriter for deterministic concurrent testing]
key_files:
  created:
    - pkg/metadata/store/memory/backup.go
  modified:
    - pkg/metadata/store/memory/memory_conformance_test.go
    - pkg/metadata/storetest/backup_conformance.go
decisions:
  - "Gob payload length prefix (uint64 LE) before gob data to prevent gob's internal buffered reader from consuming trailing CRC bytes"
  - "ServerConfig.CustomSettings JSON-encoded to []byte workaround for gob map[string]any limitation"
  - "Lazy sub-stores serialized via exported snapshot wrapper structs (fileBlockSnapshotData, lockSnapshotData, etc.)"
  - "usedBytes recomputed from files on restore rather than serialized (transient counter, not data)"
  - "signalWriter in conformance suite ensures deterministic ConcurrentWriter test by synchronizing backup start with concurrent writer"
metrics:
  duration: 10m
  completed: 2026-05-27
---

# Phase 21 Plan 01: Memory Backup Driver Summary

Gob-based Backupable implementation for MemoryMetadataStore with inline hash extraction, envelope framing, and all 5 conformance subtests passing.

## Tasks Completed

| Task | Name | Commit | Files |
|------|------|--------|-------|
| 1 | Implement Memory Backup and Restore | e42e70ff | pkg/metadata/store/memory/backup.go |
| 1-fix | Gob payload length prefix for CRC safety | 7890dc24 | pkg/metadata/store/memory/backup.go |
| 2 | Wire Memory Backup Conformance Suite | 597bb5f2 | pkg/metadata/store/memory/memory_conformance_test.go, pkg/metadata/storetest/backup_conformance.go |

## Implementation Details

### backup.go (346 lines)

- `memoryEngineTag = "memory"`, `memorySchemaVersion = uint32(1)`
- `memoryBackupSnapshot` struct mirrors all data fields of MemoryMetadataStore
- `Backup` acquires `s.mu.RLock()`, builds snapshot from live maps, extracts hashes inline via `hs.Add(br.Hash)` for all `Attr.Blocks` entries, gob-encodes to buffer, writes length prefix + payload through `backup.NewWriter` envelope, calls `Finish()` for trailing CRC
- `Restore` checks `len(s.shares) > 0` for non-empty destination, reads envelope via `backup.ReadHeader`, verifies engine tag via `backup.VerifyEngine`, reads 4-byte schema version, reads 8-byte payload length, reads exact payload via `io.ReadFull`, gob-decodes, verifies CRC via `backup.VerifyCRC(r, acc)`, rebuilds store under write lock
- CustomSettings gob workaround: JSON pre-encode to `[]byte` on backup, JSON decode on restore
- Lazy sub-stores (fileBlockData, lockStore, clientStore, durableStore) snapshot via exported wrapper structs; nil on restore for lazy init
- Transient state (sortedDirCache, sessions) re-initialized; usedBytes recomputed from files

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Gob decoder consumes trailing CRC bytes**
- **Found during:** Task 2 (RoundTrip conformance test failure)
- **Issue:** The envelope format has no payload-length field. Gob's internal buffered reader reads ahead, consuming the trailing 4-byte CRC from the stream. `backup.VerifyCRC` then hits EOF.
- **Fix:** Added 8-byte LE uint64 payload length prefix before gob data. Restore reads exact length via `io.ReadFull` into a `[]byte`, then gob-decodes from a `bytes.Reader` so the CRC bytes remain untouched in the original reader.
- **Files modified:** pkg/metadata/store/memory/backup.go
- **Commit:** 7890dc24

**2. [Rule 1 - Bug] ConcurrentWriter conformance test race condition**
- **Found during:** Task 2 (ConcurrentWriter test consistently failing)
- **Issue:** The test spawns backup and concurrent writer as parallel goroutines, but Go's scheduler consistently runs the concurrent writer to completion before the backup goroutine acquires its lock. The test then correctly captures the concurrent file in the snapshot but fails the assertion that expects it to be absent.
- **Fix:** Added `signalWriter` wrapper in the conformance suite that closes a channel on the first `Write` call. The concurrent writer waits on this channel before starting its writes, ensuring the backup has acquired its snapshot lock first.
- **Files modified:** pkg/metadata/storetest/backup_conformance.go
- **Commit:** 597bb5f2

## Verification

- `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v -count=1` -- all 5 subtests PASS
- `go test -race ./pkg/metadata/store/memory/ -run TestBackupConformance -count=1` -- no race conditions
- `go vet ./pkg/metadata/store/memory/` -- clean
- `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -count=10` -- deterministic (10/10 pass)
- `go test ./pkg/metadata/store/memory/ -run TestConformance -count=1` -- existing suite still passes (no regressions)
