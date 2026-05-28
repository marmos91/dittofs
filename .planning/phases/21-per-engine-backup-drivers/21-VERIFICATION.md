---
phase: 21-per-engine-backup-drivers
verified: 2026-05-27T15:00:00Z
status: human_needed
score: 4/4 success criteria verified (SC-4 partial — postgres untestable without DSN)
overrides_applied: 0
re_verification:
  previous_status: gaps_found
  previous_score: 3/4
  gaps_closed:
    - "Badger and Postgres restore verify CRC before committing data to durable storage"
    - "Memory backup snapshot does not race on rollupOffsets and synced fields"
  gaps_remaining: []
  regressions: []
human_verification:
  - test: "Run postgres backup conformance suite against a live PostgreSQL instance"
    expected: "All 5 subtests pass: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness"
    why_human: "No DITTOFS_TEST_POSTGRES_DSN available in CI environment; test skips without it. SC-3 and partial SC-4 cannot be verified programmatically here."
---

# Phase 21: Per-Engine Backup Drivers Verification Report

**Phase Goal:** Implement `metadata.Backupable` on each metadata store engine (memory, badger, postgres) with per-engine serialization, inline hash extraction, and conformance suite coverage.
**Verified:** 2026-05-27T15:00:00Z
**Status:** human_needed
**Re-verification:** Yes — after gap closure (Plans 04 and 05)

## Goal Achievement

### Observable Truths (from ROADMAP.md Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|---------|
| 1 | Memory store backup/restore via gob under mu.RLock() with correct HashSet | VERIFIED | backup.go 409 lines; mu.RLock() at line 101; rollupMu.RLock at line 125; syncedMu.RLock at line 135; hash extraction loop lines 185-193; all 5 conformance subtests PASS |
| 2 | Badger store backup/restore via custom db.View() streaming with correct HashSet | VERIFIED | backup.go 304 lines; NewWriter inside db.View at line 69; VerifyCRC at line 257 before wb.Flush at lines 270/278; all 5 conformance subtests PASS |
| 3 | Postgres store backup/restore via COPY TO/FROM in REPEATABLE READ txn with correct HashSet | VERIFIED for compilation; HUMAN-NEEDED for runtime | backup.go 401 lines; REPEATABLE READ at line 83; VerifyCRC at line 305 before COMMIT at line 310; builds and vets clean; runtime conformance skipped (no DSN) |
| 4 | All three pass the full conformance suite (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness) | PARTIAL — 2/3 verified at runtime | Memory: all 5 PASS (verified). Badger: all 5 PASS (verified). Postgres: SKIP (no DSN available) |

**Score:** 4/4 roadmap success criteria have passing or compile-verified implementations (SC-4 partial due to postgres DSN requirement)

### Re-verification Gap Status

Both blockers from the initial verification are confirmed CLOSED:

**BLOCKER 1 CLOSED — CR-01: Commit before CRC (Badger + Postgres)**

- Badger `Restore`: phase 1 collects all KV pairs into `[]kvEntry` slice (lines 204-252), phase 2 calls `backup.VerifyCRC(r, acc)` at line 257 BEFORE any `wb.Flush()` at lines 270/278. Corrupt stream leaves store empty and retryable.
- Postgres `Restore`: `backup.VerifyCRC(r, acc)` at line 305 BEFORE `pgRaw.Exec(ctx, "COMMIT")` at line 310. Deferred ROLLBACK (line 282) executes if CRC fails.

**BLOCKER 2 CLOSED — CR-02: Race on rollupOffsets and synced maps (Memory)**

Memory `Backup` now acquires `s.rollupMu.RLock()` at line 125 and `s.syncedMu.RLock()` at line 135, each held only for a shallow-copy into a fresh map. Both lock acquisitions are inside the `s.mu.RLock()` scope (line 101). `go test -race ./pkg/metadata/store/memory/ -run TestBackupConformance -count=5` passes clean.

**ADDITIONAL FIX — Signal ordering (Badger ConcurrentWriter)**

`backup.NewWriter(w, badgerEngineTag)` moved inside the `db.View` callback (line 69 inside View at line 67). The first Write to the output writer — which signals the `signalWriter` in the ConcurrentWriter test — now fires AFTER the MVCC snapshot is established.

**ADDITIONAL FIX — Allocation bounds (all three drivers)**

- Memory: `maxRestorePayloadSize = 256 << 20` guards `make([]byte, payloadLen)` at line 284.
- Badger: `maxRestoreAllocSize = 256 << 20` guards `make([]byte, keyLen)` at line 224 and `make([]byte, valLen)` at line 241.
- Postgres: `dataLen > uint64(math.MaxInt64)` guard at line 363 prevents int64 wrap in `io.LimitReader`.

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/metadata/store/memory/backup.go` | Backupable implementation for MemoryMetadataStore | VERIFIED | 409 lines; Backup + Restore methods; compile-time var _ assertion at line 90 |
| `pkg/metadata/store/memory/memory_conformance_test.go` | TestBackupConformance wiring | VERIFIED | TestBackupConformance calls storetest.RunBackupConformanceSuite |
| `pkg/metadata/store/badger/backup.go` | Backupable implementation for BadgerMetadataStore | VERIFIED | 304 lines; Backup + Restore methods; compile-time assertion at line 38 |
| `pkg/metadata/store/badger/badger_conformance_test.go` | TestBackupConformance wiring | VERIFIED | TestBackupConformance calls storetest.RunBackupConformanceSuite |
| `pkg/metadata/store/postgres/backup.go` | Backupable implementation for PostgresMetadataStore | VERIFIED | 401 lines; Backup + Restore methods; compile-time assertion at line 57 |
| `pkg/metadata/store/postgres/postgres_conformance_test.go` | TestBackupConformance wiring | VERIFIED | TestBackupConformance with DSN skip guard |
| `pkg/metadata/storetest/backup_conformance.go` | Conformance suite (5 subtests) | VERIFIED | 5 subtests; signalWriter for ConcurrentWriter determinism |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| memory/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter / backup.ReadHeader / backup.VerifyCRC | VERIFIED | NewWriter line 196, ReadHeader line 256, VerifyCRC line 303 |
| memory/backup.go | pkg/metadata/store/memory/store.go | rollupMu / syncedMu | VERIFIED | rollupMu.RLock line 125, syncedMu.RLock line 135 |
| memory_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Wired |
| badger/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter (inside View) / backup.ReadHeader / backup.VerifyCRC | VERIFIED | NewWriter line 69 inside db.View at 67; VerifyCRC line 257 before Flush |
| badger_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Wired |
| postgres/backup.go | pkg/metadata/backup/envelope.go | backup.NewWriter / backup.ReadHeader / backup.VerifyCRC | VERIFIED | VerifyCRC line 305 before COMMIT line 310 |
| postgres_conformance_test.go | pkg/metadata/storetest/backup_conformance.go | storetest.RunBackupConformanceSuite | VERIFIED | Wired with DSN skip guard |

### Data-Flow Trace (Level 4)

Not applicable — backup drivers are not rendering components. Data flows are verified through conformance suite tests (RoundTrip subtest exercises the full backup → restore → read-back data path).

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| Memory all 5 conformance subtests | `go test ./pkg/metadata/store/memory/ -run TestBackupConformance -v -count=1` | all 5 PASS in 0.414s | PASS |
| Memory conformance under race detector (5 runs) | `go test -race ./pkg/metadata/store/memory/ -run TestBackupConformance -count=5` | ok 1.425s | PASS |
| Badger all 5 conformance subtests | `go test -tags=integration ./pkg/metadata/store/badger/ -run TestBackupConformance -v -count=1` | all 5 PASS in 1.057s | PASS |
| Memory vet | `go vet ./pkg/metadata/store/memory/` | exit 0 | PASS |
| Badger vet | `go vet -tags=integration ./pkg/metadata/store/badger/` | exit 0 | PASS |
| Postgres build + vet | `go build -tags=integration && go vet -tags=integration ./pkg/metadata/store/postgres/` | exit 0 | PASS |
| Postgres conformance (no DSN) | `go test -tags=integration ./pkg/metadata/store/postgres/ -run TestBackupConformance` | SKIP (no DSN) | SKIP |
| Debt markers in modified files | grep TBD/FIXME/XXX across all three backup.go files | no matches | PASS |

### Probe Execution

No probe scripts declared in PLAN files. No `scripts/*/tests/probe-*.sh` files exist for this phase. Step 7c: SKIPPED (no probes declared or found).

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|---------|
| DRV-01 | 21-01-PLAN.md | Memory store implements Backupable — gob round-trip under mu.RLock() with hash extraction from files map | SATISFIED | pkg/metadata/store/memory/backup.go exists, compiles, all 5 conformance subtests pass; rollupMu/syncedMu race fix in plans 05 |
| DRV-02 | 21-02-PLAN.md | Badger store implements Backupable — custom streaming inside single db.View() with hash extraction from file entries | SATISFIED | pkg/metadata/store/badger/backup.go exists, compiles, all 5 conformance subtests pass; CRC-before-flush fix in plan 04; signal ordering fix in plan 05 |
| DRV-03 | 21-03-PLAN.md | Postgres store implements Backupable — COPY TO/FROM inside single REPEATABLE READ txn with hash extraction from file_block_refs | SATISFIED FOR COMPILATION — runtime conformance needs DSN | pkg/metadata/store/postgres/backup.go exists, compiles, TestBackupConformance wired; CRC-before-COMMIT fix in plan 04; dataLen overflow guard in plan 05 |
| DRV-04 | All plans | All three drivers pass the shared conformance suite | PARTIAL | Memory: PASS. Badger: PASS. Postgres: SKIP (no DSN) |

**Orphaned requirements check:** REQUIREMENTS.md maps DRV-01 through DRV-04 to Phase 21 and no others. No orphaned requirements found.

**Note:** REQUIREMENTS.md still shows DRV-01 through DRV-04 as `[ ]` (unchecked). Documentation debt only — not a blocker.

### Anti-Patterns Found

No blockers or unresolved debt markers found in the files modified by Plans 04 and 05. The previous WARNING-level allocation patterns are resolved:

| File | Line | Pattern | Severity | Resolution |
|------|------|---------|----------|------------|
| pkg/metadata/store/memory/backup.go | 284 | payloadLen > maxRestorePayloadSize guard | INFO | Fixed in Plan 05: 256 MiB cap added |
| pkg/metadata/store/badger/backup.go | 224, 241 | keyLen/valLen > maxRestoreAllocSize guards | INFO | Fixed in Plan 05: 256 MiB cap added |
| pkg/metadata/store/postgres/backup.go | 363 | dataLen > math.MaxInt64 guard | INFO | Fixed in Plan 05: overflow guard added |

### Human Verification Required

### 1. Postgres Backup Conformance Suite

**Test:** Set `DITTOFS_TEST_POSTGRES_DSN` to a test PostgreSQL DSN and run:
```
go test -tags=integration ./pkg/metadata/store/postgres/ -run TestBackupConformance -v -count=1 -timeout 120s
```
**Expected:** All 5 subtests pass: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness
**Why human:** No PostgreSQL instance available in the verification environment. Test skips without the DSN env var. DRV-03 and DRV-04 (postgres portion) cannot be fully verified programmatically.

### Gaps Summary

No blockers remain. The two blockers from the initial verification are confirmed closed by code inspection and live test execution. The only open item is the Postgres conformance suite requiring a live PostgreSQL instance, which is a human verification item (not a bug).

---

_Verified: 2026-05-27T15:00:00Z_
_Verifier: Claude (gsd-verifier)_
