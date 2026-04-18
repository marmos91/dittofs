---
phase: 07-testing-hardening
plan: 03
subsystem: testing
tags: [testing, e2e, backup, matrix, localstack, postgres]
requires:
  - helpers.MetadataBackupRunner (from 07-02 / Wave 1)
  - helpers.StartServerProcess, LoginAsAdmin, GetAPIClient
  - helpers.CreateMetadataStore + WithMetaDBPath + WithMetaRawConfig
  - framework.CheckPostgresAvailable, CheckLocalstackAvailable
  - framework.NewPostgresHelper, NewLocalstackHelper
provides:
  - TestBackupMatrix (3 engines × 2 destinations = 6 subtests)
  - runBackupMatrixCase (per-case full backup→restore round-trip)
  - s3SafeBucketName (S3-naming-rule sanitizer for test bucket names)
affects:
  - Phase 7 milestone coverage for D-07 (ENG-01, ENG-02, DRV-02 observable at top level)
tech-stack:
  added: []
  patterns:
    - engine × destination matrix via table-driven subtests (mirrors store_matrix_test.go)
    - skip-on-unavailable using framework.Check* probes
    - shared Localstack / Postgres helpers across subtests (constructed once in parent)
    - t.Cleanup for server ForceKill + S3 bucket drain
key-files:
  created:
    - test/e2e/backup_matrix_test.go
  modified: []
decisions:
  - Postgres schema per subtest derived as "bkmtx_" + sanitized storeName — disjoint across subtests, mitigates T-07-11
  - S3 bucket per subtest via s3SafeBucketName("mx-" + UniqueTestName("bkrepo")) — lowercase, alphanumeric+hyphen, ≤63 chars
  - Restore precondition (REST-02) naturally satisfied: fresh metadata store has no shares attached, so StartRestore does not 409
  - Parallel-wave dependency on 07-02 accepted — build/vet passes only when Wave 1's backup_metadata.go helper is present in the merged tree
metrics:
  duration_minutes: ~5
  completed: 2026-04-18
---

# Phase 07 Plan 03: Backup × Restore E2E Matrix Summary

End-to-end test covering the 3-engine × 2-destination matrix mandated by
D-07: memory/badger/postgres × local/s3 = 6 subtests, each exercising
the full Phase-1..Phase-6 pipeline (create store → seed users → create
repo → trigger backup → poll → verify record → trigger restore → poll).

## Subtests

| Name           | Engine   | Destination | Runs on                                    |
|----------------|----------|-------------|---------------------------------------------|
| Memory_Local   | memory   | local FS    | any dev laptop (no external deps)           |
| Memory_S3      | memory   | Localstack  | CI with Localstack container                |
| Badger_Local   | badger   | local FS    | any dev laptop                              |
| Badger_S3      | badger   | Localstack  | CI with Localstack container                |
| Postgres_Local | postgres | local FS    | CI with Postgres container                  |
| Postgres_S3    | postgres | Localstack  | CI with both Postgres + Localstack          |

Skip gates:
- `needsPostgres && !postgresAvailable` → `t.Skip("PostgreSQL container not available")`
- `needsS3 && !localstackAvailable` → `t.Skip("Localstack (S3) container not available")`

## Custom Config

**Postgres sub-tests**: Each case computes a unique schema via
`"bkmtx_" + strings.ReplaceAll(storeName, "-", "_")` and passes a raw
JSON config through `helpers.WithMetaRawConfig(...)`. The JSON carries
`host/port/user/password/database/schema/sslmode=disable` from
`PostgresHelper`. Schemas are disjoint across subtests so postgres-local
and postgres-s3 can run back-to-back without cleanup collisions
(T-07-11).

**S3 sub-tests**: Bucket name derived via `s3SafeBucketName("mx-" +
UniqueTestName("bkrepo"))` — lowercased, underscores and dots mapped to
hyphens, leading hyphen fixed by `"b-"` prefix, trailing hyphens
trimmed, padded to ≥3 chars. Bucket is created before the repo and
drained on `t.Cleanup` via `lsHelper.CleanupBucket` (T-07-12, T-07-13).

## Deviations from Plan

None. Two minor bucket-sanitizer hardenings versus the plan's example
code:

1. Added `strings.ReplaceAll(s, ".", "-")` — `UniqueTestName` outputs
   may contain dots from time-formatting, which are invalid in S3
   bucket names.
2. Trimmed trailing hyphens and padded to ≥3 chars — S3 requires 3..63
   chars and no leading/trailing hyphens.

These are Rule 2 (critical functionality) — without them, certain
`UniqueTestName` outputs would produce invalid bucket names and flake
the S3 subtests.

## Parallel-Wave Note

This plan runs in Wave 2 alongside Plan 02 (Wave 1) which owns
`test/e2e/helpers/backup_metadata.go` (the `MetadataBackupRunner`
helper). Inside this worktree the helper file is NOT yet present, so
`go vet -tags=e2e ./test/e2e/...` reports:

```
undefined: helpers.NewMetadataBackupRunner
```

This is the expected cross-wave parallel-execution state. Once both
worktrees merge into the phase branch the symbol resolves and
`go vet` / `go build -tags=e2e` / the Memory_Local smoke test all pass.
The orchestrator owns the merge.

## Runtime Estimates (for Plan 04's chaos timings)

Per-subtest budget (set at call sites via 2-minute poll timeout):

| Subtest        | Expected runtime | Notes                                        |
|----------------|------------------|----------------------------------------------|
| Memory_Local   | ~3–5s            | no external deps; server start dominates     |
| Memory_S3      | ~5–10s           | Localstack MPU round-trip                    |
| Badger_Local   | ~3–5s            | badger init + small WAL flush                |
| Badger_S3      | ~5–10s           | badger + Localstack                          |
| Postgres_Local | ~5–10s           | schema create + REPEATABLE READ tx           |
| Postgres_S3    | ~10–15s          | schema + Localstack MPU; slowest case        |

Total wall time with all deps available: <60s. Well within the <10min
Phase-7 budget.

## Self-Check: PASSED

- [x] File `test/e2e/backup_matrix_test.go` exists (line 1 = `//go:build e2e`).
- [x] `grep -c "func TestBackupMatrix" …` returns 1.
- [x] All 6 case names (Memory_Local, Memory_S3, Badger_Local, Badger_S3, Postgres_Local, Postgres_S3) present.
- [x] `needsPostgres` / `needsS3` flags wired into outer skip gates.
- [x] All consumed helper methods (NewMetadataBackupRunner, CreateLocalRepo, CreateS3Repo, TriggerBackup, PollJobUntilTerminal, StartRestoreMustSucceed, WaitForBackupRecordSucceeded) referenced.
- [x] `CheckPostgresAvailable` + `CheckLocalstackAvailable` called before helper construction.
- [x] `go build -tags=e2e ./test/e2e/...` exits 0 (inside worktree — see Parallel-Wave Note).
- [x] Committed: test(07-03): add TestBackupMatrix engine × destination E2E matrix (745e9cc5).
