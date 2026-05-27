---
phase: 21-per-engine-backup-drivers
verified: 2026-05-27T14:00:00Z
status: gaps_found
score: 3/4 success criteria verified (SC-4 partially — postgres untestable without DSN)
overrides_applied: 0
gaps:
  - truth: "Badger and Postgres restore verify CRC before committing data to durable storage"
    status: failed
    reason: "Both drivers flush/commit all data BEFORE calling backup.VerifyCRC. On CRC failure the store is left in a partially-restored-but-corrupt state and is permanently unrecoverable (subsequent Restore returns ErrRestoreDestinationNotEmpty). The ROLLBACK defer in the postgres driver is a no-op after COMMIT has been issued."
    artifacts:
      - path: "pkg/metadata/store/badger/backup.go"
        issue: "Lines 232-240: wb.Flush() commits data at line 232, backup.VerifyCRC called at line 238 — too late to undo"
      - path: "pkg/metadata/store/postgres/backup.go"
        issue: "Lines 301-307: pgRaw.Exec(COMMIT) at line 301, backup.VerifyCRC called at line 306 — ROLLBACK defer is a no-op post-COMMIT"
    missing:
      - "Badger: verify CRC before wb.Flush() — either buffer all KV pairs then verify before writing, or move VerifyCRC before the final Flush"
      - "Postgres: reorder to VerifyCRC before COMMIT — the tee reader has already accumulated all payload bytes by end of table loop"

  - truth: "Memory backup snapshot does not race on rollupOffsets and synced fields"
    status: failed
    reason: "Backup acquires s.mu.RLock() but reads s.rollupOffsets (line 113) and s.synced (line 114) without holding their dedicated secondary mutexes (rollupMu and syncedMu). Concurrent SetRollupOffset/MarkSynced calls acquire only rollupMu/syncedMu — not s.mu — creating an unsynchronized concurrent map read."
    artifacts:
      - path: "pkg/metadata/store/memory/backup.go"
        issue: "Lines 113-114: s.rollupOffsets and s.synced read under s.mu.RLock() only; rollupMu and syncedMu not held"
      - path: "pkg/metadata/store/memory/store.go"
        issue: "Lines 245, 254: rollupMu and syncedMu are separate from s.mu — confirmed separate mutex governance"
    missing:
      - "Acquire s.rollupMu.RLock() around line 113 and s.syncedMu.RLock() around line 114 while inside s.mu.RLock() section"

human_verification:
  - test: "Run postgres backup conformance suite against a live PostgreSQL instance"
    expected: "All 5 subtests pass: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness"
    why_human: "No DITTOFS_TEST_POSTGRES_DSN available in CI environment; test skips without it. SC-3 and partial SC-4 cannot be verified programmatically here."
---

# Phase 21: Per-Engine Backup Drivers Verification Report

**Phase Goal:** Implement `metadata.Backupable` on each metadata store engine (memory, badger, postgres) with per-engine serialization, inline hash extraction, and conformance suite coverage.
**Verified:** 2026-05-27T14:00:00Z
**Status:** gaps_found
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths (from ROADMAP.md Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| 1 | Memory store backup/restore via gob under mu.RLock() with correct HashSet | VERIFIED | backup.go 346 lines; mu.RLock() at line 96; hash extraction loop lines 159-165; all 5 conformance subtests PASS (go test output) |
| 2 | Badger store backup/restore via custom db.View() streaming with correct HashSet | VERIFIED (with latent defect CR-04) | backup.go 265 lines; db.View() MVCC at line 66; custom KV framing; all 5 conformance subtests PASS; CR-04 (signal fires before View enters) is a latent non-determinism |
| 3 | Postgres store backup/restore via COPY TO/FROM in REPEATABLE READ txn with correct HashSet | VERIFIED for compilation; HUMAN-NEEDED for runtime | backup.go 389 lines; REPEATABLE READ at line 82; backupTable helper with COPY TO; extractHashes dedicated query; COPY FROM in restoreTable; builds clean; runtime conformance skipped (no DSN) |
| 4 | All three pass the full conformance suite (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness) | PARTIAL — 2/3 verified | Memory: all 5 PASS (verified). Badger: all 5 PASS (verified). Postgres: SKIP (no DSN available) |

**Score:** 3/4 roadmap success criteria fully verified (SC-4 partial due to postgres DSN requirement)

### Must-Have Truths from PLAN Frontmatter

**Plan 01 (Memory, DRV-01 + DRV-04):**

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| D-01 | Memory store Backup serializes full in-memory state via gob under mu.RLock and returns correct HashSet | VERIFIED | backup.go lines 96-208; all map fields included in memoryBackupSnapshot |
| D-04 | Hash extraction inline — iterates files map, calls hs.Add(br.Hash) | VERIFIED | Lines 159-165: for range s.files, for range fd.Attr.Blocks, hs.Add(br.Hash) |
| D-06 | Empty-store detection via len(shares) > 0 before Restore | VERIFIED | Lines 221-226: s.mu.RLock(); hasShares := len(s.shares) > 0; return ErrRestoreDestinationNotEmpty |
| D-07 | Schema version uint32 LE at payload start (version 1) | VERIFIED | Lines 175-179: binary.LittleEndian.PutUint32(vBuf[:], memorySchemaVersion); envW.Write |
| D-08 | Self-contained driver — no shared driver-level helpers | VERIFIED | backup.go uses only standard library + backup envelope + blockstore.HashSet |
| Memory Restore rebuilds functional store from backup stream | VERIFIED | Conformance RoundTrip PASS — GetRootHandle, GetChild, GetFile all work after restore |
| Memory passes all 5 conformance subtests | VERIFIED | go test output: all 5 PASS including sub-sub-tests |
| Race condition on rollupOffsets/synced under concurrent writes | FAILED | s.rollupOffsets (line 113) and s.synced (line 114) read under s.mu.RLock() only; rollupMu and syncedMu not held; race detector clean on conformance suite because ConcurrentWriter test does not call SetRollupOffset/MarkSynced |

**Plan 02 (Badger, DRV-02 + DRV-04):**

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| D-02 | Badger Backup uses custom KV stream inside db.View() MVCC snapshot | VERIFIED | Lines 66-126: s.db.View() with full-DB iterator, uint32 LE framing, sentinel |
| D-04 | Hash extraction inline from f: prefix entries | VERIFIED | Lines 106-116: HasPrefix check, json.Unmarshal into metadata.File, hs.Add(br.Hash) |
| D-06 | Empty-store detection via s: prefix seek | VERIFIED | isStoreEmpty() at lines 247-263: db.View, prefix seek, it.ValidForPrefix |
| D-07 | Schema version uint32 LE at payload start | VERIFIED | Lines 57-61: binary.LittleEndian.PutUint32(verBuf[:], badgerSchemaVersion), envW.Write |
| D-08 | Self-contained driver | VERIFIED | No shared helpers beyond envelope and blockstore |
| Badger Restore rebuilds functional store | VERIFIED | Conformance RoundTrip PASS |
| Badger passes all 5 conformance subtests | VERIFIED | go test -tags=integration output: all 5 PASS |
| CRC verified before data committed to durable storage | FAILED | Lines 232-240: wb.Flush() at 232, VerifyCRC at 238 — data committed before integrity check |
| Badger ConcurrentWriter snapshot ordering correct | PARTIAL | Tests pass currently; CR-04 documents that NewWriter (which signals the test) fires before db.View() enters — latent non-determinism if scheduler allows concurrent writer to run in that window |

**Plan 03 (Postgres, DRV-03 + DRV-04):**

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| D-03 | Postgres Backup uses COPY TO STDOUT per table inside REPEATABLE READ txn | VERIFIED | Lines 80-84: BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ; backupTable iterates backupTables slice |
| D-04 | Hash extraction inline within same REPEATABLE READ snapshot | VERIFIED | extractHashes() at lines 183-218: COPY (SELECT DISTINCT hash FROM file_block_refs) TO STDOUT, hex parse |
| D-05 | Dedicated COPY query for hash extraction | VERIFIED | Separate extractHashes function, not co-mingled with table loop |
| D-06 | Empty-store detection via SELECT EXISTS(SELECT 1 FROM shares) | VERIFIED | Lines 231-237: pool.QueryRow, Scan(&hasShares), return ErrRestoreDestinationNotEmpty |
| D-07 | Schema version uint32 LE at payload start | VERIFIED | Lines 93-98: binary.LittleEndian.PutUint32, envW.Write |
| D-08 | Self-contained driver | VERIFIED | No shared helpers |
| Postgres Restore rebuilds functional store via COPY FROM STDIN | VERIFIED FOR COMPILATION | restoreTable calls raw.CopyFrom; builds clean; runtime unverifiable without DSN |
| Postgres passes all 5 conformance subtests | HUMAN-NEEDED | Test skips without DITTOFS_TEST_POSTGRES_DSN |
| CRC verified before data committed (COMMIT) | FAILED | Lines 301-307: pgRaw.Exec(COMMIT) at 301, VerifyCRC at 306 — ROLLBACK defer is no-op post-COMMIT |

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/metadata/store/memory/backup.go` | Backupable implementation for MemoryMetadataStore | VERIFIED | 378 lines; Backup + Restore methods; compile-time var _ assertion at line 85 |
| `pkg/metadata/store/memory/memory_conformance_test.go` | TestBackupConformance wiring | VERIFIED | Line 17-21: TestBackupConformance calls storetest.RunBackupConformanceSuite |
| `pkg/metadata/store/badger/backup.go` | Backupable implementation for BadgerMetadataStore | VERIFIED | 265 lines; Backup + Restore methods; compile-time assertion at line 33 |
| `pkg/metadata/store/badger/badger_conformance_test.go` | TestBackupConformance wiring | VERIFIED | Lines 37-49: TestBackupConformance (note: misplaced comment from previous function above, function itself correct) |
| `pkg/metadata/store/postgres/backup.go` | Backupable implementation for PostgresMetadataStore | VERIFIED | 389 lines; Backup + Restore methods; compile-time assertion at line 56 |
| `pkg/metadata/store/postgres/postgres_conformance_test.go` | TestBackupConformance wiring | VERIFIED | Lines 67-75: TestBackupConformance with DSN skip guard |
| `pkg/metadata/storetest/backup_conformance.go` | Conformance suite (5 subtests + signalWriter) | VERIFIED | 650 lines; 5 subtests; signalWriter added for ConcurrentWriter determinism |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| memory/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter / backup.ReadHeader / backup.VerifyCRC | VERIFIED | All three used: NewWriter line 169, ReadHeader line 229, VerifyCRC line 271 |
| memory/backup.go | pkg/blockstore/hashset.go | blockstore.NewHashSet / hs.Add | VERIFIED | NewHashSet line 158, hs.Add line 165 |
| memory_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Line 18 |
| badger/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter / backup.ReadHeader / backup.VerifyCRC | VERIFIED | NewWriter line 51, ReadHeader line 153, VerifyCRC line 238 |
| badger/backup.go | pkg/blockstore/hashset.go | blockstore.NewHashSet / hs.Add | VERIFIED | NewHashSet line 63, hs.Add line 114 |
| badger_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Line 38 |
| postgres/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter / backup.ReadHeader / backup.VerifyCRC | VERIFIED | NewWriter line 88, ReadHeader line 240, VerifyCRC line 306 |
| postgres/backup.go | pkg/blockstore/hashset.go | blockstore.NewHashSet / hs.Add | VERIFIED | NewHashSet line 190, hs.Add line 214 |
| postgres_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Line 74 |

### Data-Flow Trace (Level 4)

Not applicable — backup drivers are not rendering components. Data flows are verified through conformance suite tests (RoundTrip subtest exercises the full backup → restore → read-back data path).

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| Memory backup builds | `go build ./pkg/metadata/store/memory/` | exit 0 | PASS |
| Memory vet | `go vet ./pkg/metadata/store/memory/` | exit 0 | PASS |
| Memory all 5 conformance subtests | `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v` | all 5 PASS in 0.240s | PASS |
| Memory conformance under race detector | `go test -race ./pkg/metadata/store/memory/ -run TestBackupConformance -count=5` | ok (1.329s) | PASS |
| Badger backup builds (with integration tag) | `go build -tags=integration ./pkg/metadata/store/badger/` | exit 0 | PASS |
| Badger all 5 conformance subtests | `go test -tags=integration ./pkg/metadata/store/badger/ -run TestBackupConformance -v` | all 5 PASS in 1.780s | PASS |
| Badger conformance under race detector | `go test -race -tags=integration ./pkg/metadata/store/badger/ -run TestBackupConformance` | ok (2.670s) | PASS |
| Postgres backup builds (with integration tag) | `go build -tags=integration ./pkg/metadata/store/postgres/` | exit 0 | PASS |
| Postgres vet | `go vet ./pkg/metadata/store/postgres/` | exit 0 | PASS |
| Postgres conformance (no DSN) | `go test -tags=integration ./pkg/metadata/store/postgres/ -run TestBackupConformance` | SKIP (no DSN) | SKIP |

### Probe Execution

No probe scripts declared in PLAN files. No `scripts/*/tests/probe-*.sh` files exist for this phase. Step 7c: SKIPPED (no probes declared or found).

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|---------|
| DRV-01 | 21-01-PLAN.md | Memory store implements Backupable — gob round-trip under mu.RLock() with hash extraction | SATISFIED | pkg/metadata/store/memory/backup.go exists, compiles, all 5 conformance subtests pass |
| DRV-02 | 21-02-PLAN.md | Badger store implements Backupable — custom streaming inside single db.View() with hash extraction | SATISFIED | pkg/metadata/store/badger/backup.go exists, compiles, all 5 conformance subtests pass |
| DRV-03 | 21-03-PLAN.md | Postgres store implements Backupable — COPY TO/FROM inside single REPEATABLE READ txn with hash extraction | SATISFIED FOR COMPILATION — runtime conformance needs DSN | pkg/metadata/store/postgres/backup.go exists, compiles, TestBackupConformance wired; runtime pass requires human verification |
| DRV-04 | All plans | All three drivers pass the shared conformance suite | PARTIAL | Memory: PASS. Badger: PASS. Postgres: SKIP (no DSN) |

**Orphaned requirements check:** REQUIREMENTS.md maps DRV-01 through DRV-04 to Phase 21 and no others. No orphaned requirements found.

**Note:** REQUIREMENTS.md still shows DRV-01 through DRV-04 as `[ ]` (unchecked). This is a documentation debt in REQUIREMENTS.md — the traceability table shows them as "Pending" even though the implementations exist. Not a blocker for the phase goal but the requirements doc should be updated.

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| pkg/metadata/store/badger/backup.go | 232-240 | wb.Flush() before VerifyCRC | BLOCKER | Corrupt restore stream leaves BadgerDB with data written but no CRC confirmation; next Restore call returns ErrRestoreDestinationNotEmpty; store permanently unrecoverable without manual intervention |
| pkg/metadata/store/postgres/backup.go | 301-307 | COMMIT before VerifyCRC | BLOCKER | Postgres transaction committed before CRC verified; ROLLBACK defer is a no-op; same permanent unrecoverability as CR-01-Badger |
| pkg/metadata/store/memory/backup.go | 113-114 | rollupOffsets/synced read under s.mu.RLock() only | BLOCKER | Data race: rollupMu/syncedMu not held; race detector doesn't catch it in the current conformance suite because ConcurrentWriter only writes file/share data, not rollup/synced state |
| pkg/metadata/store/badger/backup.go | 51-66 | NewWriter (signal) called before db.View() (snapshot) | WARNING | ConcurrentWriter conformance test non-deterministic: signal fires before MVCC snapshot is established; concurrent writer can write into the snapshot window; tests pass today but can spuriously fail |
| pkg/metadata/store/memory/backup.go | 254-258 | make([]byte, payloadLen) with uint64 from untrusted stream | WARNING | Crafted stream with large payloadLen causes OOM before io.ReadFull fails; no upper bound check |
| pkg/metadata/store/badger/backup.go | 197,211 | make([]byte, keyLen) and make([]byte, valLen) with uint32 from stream | WARNING | Up to 8 GiB per KV pair allocation from crafted stream |
| pkg/metadata/store/postgres/backup.go | 353-356 | uint64 dataLen cast to int64 for io.LimitReader without validation | WARNING | Values above math.MaxInt64 wrap negative; LimitReader returns EOF immediately; stream desynchronized |

### Human Verification Required

### 1. Postgres Backup Conformance Suite

**Test:** Set `DITTOFS_TEST_POSTGRES_DSN` to a test PostgreSQL DSN and run:
```
go test -tags=integration ./pkg/metadata/store/postgres/ -run TestBackupConformance -v -count=1 -timeout 120s
```
**Expected:** All 5 subtests pass: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness
**Why human:** No PostgreSQL instance available in the verification environment. Test skips without the DSN env var.

### Gaps Summary

Two blockers and one required human verification step prevent this phase from being declared passed:

**BLOCKER 1 — CR-01: Commit before CRC (Badger + Postgres).** Both the Badger and Postgres `Restore` implementations write all data to durable storage before verifying the CRC envelope. A corrupt stream therefore leaves the store permanently in an unrecoverable state: data is written, CRC fails, but the next `Restore` call sees a non-empty destination and returns `ErrRestoreDestinationNotEmpty`. This inverts the expected atomicity guarantee of restore. Fix: for Postgres, move `VerifyCRC` before `COMMIT` (the tee reader accumulates all bytes during the table loop). For Badger, either buffer KV pairs before flushing or seek backward; the simplest fix is to read + verify CRC before issuing the final `wb.Flush()`.

**BLOCKER 2 — CR-02: Race on rollupOffsets and synced maps (Memory).** The memory `Backup` method reads `s.rollupOffsets` and `s.synced` while holding only `s.mu.RLock()`. Both fields are governed by their own separate mutexes (`rollupMu` and `syncedMu`). A concurrent call to any method that modifies these fields without holding `s.mu` creates an unsynchronized concurrent map access. The race detector does not catch this today because the conformance suite's `ConcurrentWriter` subtest only writes file and share data, not rollup offsets or synced hashes. Fix: acquire `s.rollupMu.RLock()` and `s.syncedMu.RLock()` around the reads of those fields while inside the `s.mu.RLock()` section.

These two blockers are correctness bugs introduced by the phase — the conformance suite passes because the test scenarios do not exercise the affected code paths under load. A future test that writes rollup offsets concurrently with backup, or that restores from a truncated stream and then attempts a second restore, would expose both bugs.

---

_Verified: 2026-05-27T14:00:00Z_
_Verifier: Claude (gsd-verifier)_
