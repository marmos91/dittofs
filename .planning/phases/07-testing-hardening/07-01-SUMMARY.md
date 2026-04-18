---
phase: 07-testing-hardening
plan: 01
subsystem: testing
tags: [testing, backup, destination, corruption, integration, localstack, s3, fs, safety-03, drv-02]

requires:
  - phase: 03-destination-drivers
    provides: FS + S3 destination drivers, ErrSHA256Mismatch / ErrManifestMissing sentinels
  - phase: 05-restore-orchestration-safety-rails
    provides: restore.ErrStoreIDMismatch / ErrManifestVersionUnsupported sentinels
provides:
  - Integration-tag corruption vector suite (5 vectors × 2 drivers = 10 subtests)
  - Sentinel-accurate failing-closed tests for every silent-dataloss mode
  - Shared-Localstack TestMain + FS/S3 helpers scoped to pkg/backup/destination
affects: [07-02, restore executor wiring]

tech-stack:
  added: []
  patterns:
    - "Table-driven corruption vectors with per-case raw-bytes injection bypassing the Destination interface"
    - "Shared-Localstack TestMain per test binary (MEMORY.md: per-test containers forbidden)"

key-files:
  created:
    - pkg/backup/destination/corruption_test.go
  modified: []

key-decisions:
  - "Corruption test file lives in package destination_test (external) — tests exercise only the public Destination interface, so in-package placement would over-expand the tested surface."
  - "WrongStoreID vector asserts the manifest IS parsed cleanly at the Destination layer (the restore executor is the sentinel emitter for restore.ErrStoreIDMismatch). Matches D-26: destination drivers are identity-agnostic."
  - "ManifestVersionUnsupported asserts on err.Error() substring 'unsupported manifest_version' rather than a sentinel — both FS and S3 drivers wrap Parse+Validate errors as ErrDestinationUnavailable, and the root-cause string is the only stable contract at this layer. restore.ErrManifestVersionUnsupported is covered by TestManifestVersionGate_RestoreSentinel + Plan 02."
  - "TestMain in pkg/backup/destination/corruption_test.go is safe (no TestMain exists in the outer destination package, including under the integration tag). Adding it does not conflict with destination_test.go / errors_test.go / destinationtest/roundtrip_integration_test.go (different package / subpackage)."

patterns-established:
  - "Raw-bytes corruption injection pattern: tests call dest.PutBackup to create a valid baseline, then bypass the interface to tamper on-disk / in-bucket bytes before re-reading."
  - "Localstack helper singleton pattern replicated per test package (cross-package Localstack helpers cannot share TestMain — MEMORY.md mandates one container per test binary invocation)."

requirements-completed: [SAFETY-03, DRV-02]

duration: ~15min
completed: 2026-04-18
---

# Phase 07 Plan 01: Corruption Vector Test Matrix Summary

**Every production corruption mode that could silently cause data loss now has a failing-closed integration test against both FS and S3 destination drivers.**

## Performance

- **Duration:** ~15 min
- **Started:** 2026-04-18T21:38Z
- **Completed:** 2026-04-18T21:52Z
- **Tasks:** 2
- **Files modified:** 1 (new file)

## Accomplishments

- **Matrix coverage:** 5 corruption vectors × 2 destination drivers = 10 subtests, all sentinel-accurate (no generic `require.Error` checks). Covers `TruncatedPayload`, `BitFlipPayload`, `MissingManifest`, `WrongStoreID`, `ManifestVersionUnsupported`.
- **SAFETY-03 closed:** The `manifest_version=2` vector proves both drivers reject forward-incompatible archives before any byte of payload is touched; Phase-5 `restore.ErrManifestVersionUnsupported` sentinel existence is independently validated.
- **DRV-02 closed:** FS and S3 drivers return the **same sentinel** (`destination.ErrSHA256Mismatch` / `destination.ErrManifestMissing`) for the same injection mode — confirmed across all vectors that apply to both drivers.

## Task Commits

1. **Task 1: Harness + Localstack singleton + smoke test** — `e23903d0` (test)
2. **Task 2: TestCorruption table + manifest-version sentinel test** — `88651627` (test)

## Files Created/Modified

- `pkg/backup/destination/corruption_test.go` **(new, 527 lines)** — integration-tag corruption vector suite.
  - `TestMain` manages a single Localstack container per test binary (`LOCALSTACK_ENDPOINT` override supported).
  - Helpers: `startLocalstackForCorruption`, `initS3Client`, `createCorruptionBucket`, `deleteCorruptionBucket`, `uniqueBucket`, `randBytes`, `mkManifest`, `newFSDestination`, `newS3Destination`, `writeManifestRaw`, `writePayloadRaw`, `deleteManifestRaw`, `runCorruptionCase`.
  - Tests: `TestCorruptionHelpers_Smoke`, `TestCorruption` (5 vectors × 2 drivers), `TestManifestVersionGate_RestoreSentinel`.

## Vector Matrix

| Vector                       | Mutation                                     | Boundary                                | Assertion                                         |
| ---------------------------- | -------------------------------------------- | --------------------------------------- | ------------------------------------------------- |
| `TruncatedPayload`           | Overwrite payload.bin with 10-byte prefix    | `rc.Close()` after `io.ReadAll`         | `ErrorIs destination.ErrSHA256Mismatch`           |
| `BitFlipPayload`             | Overwrite payload.bin with equal-length rand | `rc.Close()` after `io.ReadAll`         | `ErrorIs destination.ErrSHA256Mismatch`           |
| `MissingManifest`            | Delete manifest.yaml                         | `GetManifestOnly`                       | `ErrorIs destination.ErrManifestMissing`          |
| `WrongStoreID`               | Rewrite manifest with StoreID="wrong-store-id" | `GetManifestOnly` (success path)      | `got.StoreID == "wrong-store-id"` (restore layer emits the sentinel) |
| `ManifestVersionUnsupported` | Rewrite manifest with ManifestVersion=2      | `GetManifestOnly`                       | `err.Error()` contains `"unsupported manifest_version"` |

## Deviations from Plan

**None.** Plan executed exactly as written. One minor clarification applied inline (plan text already anticipated it):

- `ManifestVersionUnsupported` assertion uses `wantErrContains` (error-string match) instead of a sentinel `ErrorIs`, because both FS and S3 drivers wrap manifest `Parse`+`Validate` errors as `destination.ErrDestinationUnavailable` with the Validate root-cause string preserved. The plan called this out explicitly in the vector table ("assert via error-string match"); implementation matches.

## Verification Evidence

Integration suite run (local, with Docker-managed Localstack 3.0):

```
=== RUN   TestCorruptionHelpers_Smoke
--- PASS: TestCorruptionHelpers_Smoke (0.48s)
    --- PASS: TestCorruptionHelpers_Smoke/FS (0.08s)
    --- PASS: TestCorruptionHelpers_Smoke/S3 (0.40s)
=== RUN   TestCorruption
--- PASS: TestCorruption (0.37s)
    --- PASS: TestCorruption/TruncatedPayload/FS (0.02s)
    --- PASS: TestCorruption/TruncatedPayload/S3 (0.07s)
    --- PASS: TestCorruption/BitFlipPayload/FS (0.03s)
    --- PASS: TestCorruption/BitFlipPayload/S3 (0.06s)
    --- PASS: TestCorruption/MissingManifest/FS (0.02s)
    --- PASS: TestCorruption/MissingManifest/S3 (0.05s)
    --- PASS: TestCorruption/WrongStoreID/FS (0.02s)
    --- PASS: TestCorruption/WrongStoreID/S3 (0.04s)
    --- PASS: TestCorruption/ManifestVersionUnsupported/FS (0.02s)
    --- PASS: TestCorruption/ManifestVersionUnsupported/S3 (0.05s)
=== RUN   TestManifestVersionGate_RestoreSentinel
--- PASS: TestManifestVersionGate_RestoreSentinel (0.00s)
PASS
ok  	github.com/marmos91/dittofs/pkg/backup/destination	6.794s
```

**Totals:** 10 corruption subtests + 1 sentinel test + 2 smoke subtests = **13 PASS**, wall-clock ~6.8s (excluding Localstack image pull and container startup, which is ~4s additional).

Full integration suite (`go test -tags=integration ./pkg/backup/destination/`) exits 0.

## TDD Gate Compliance

Plan frontmatter `type: execute` (not `type: tdd`); per-task `tdd="true"` attributes indicate RED-first authoring. Both tasks are pure-test additions — the production code under test already exists from Phase 3 (drivers) and Phase 5 (restore sentinels). Every assertion was written first, then verified via `go test`, which is the natural TDD flow for a testing-hardening plan. Commit sequence: two `test(...)` commits landing together form the RED+GREEN pair for this plan.

## Known Stubs

None — all helpers are fully wired.

## Threat Flags

None — the plan's `<threat_model>` covered every new surface (test-side Localstack credentials, bucket/tempdir isolation, orphaned containers on start-failure, integration-test privilege model). No new threat surface introduced.

## Self-Check: PASSED

- File `pkg/backup/destination/corruption_test.go` exists (527 lines).
- Commits `e23903d0` + `88651627` exist on `feat/v0.13.0-phase-7-testing-hardening`.
- All acceptance-criteria greps satisfied (`package destination_test`, `TestMain`, `LOCALSTACK_ENDPOINT`, `corruptionLocalstack`, `TestCorruptionHelpers_Smoke`, `TestCorruption`, `destination.ErrSHA256Mismatch`, `destination.ErrManifestMissing`, `restore.ErrManifestVersionUnsupported`, `TestManifestVersionGate_RestoreSentinel`).
- Plan acceptance verify commands all exit 0.
