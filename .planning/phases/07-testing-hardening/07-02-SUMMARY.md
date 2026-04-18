---
phase: 07-testing-hardening
plan: 02
subsystem: testing
tags: [testing, backup, restore, helpers, manifest-gate, safety-03]
requires:
  - pkg/apiclient metadata-backup surface (TriggerBackup, StartRestore, CreateBackupRepo, GetBackupJob, ListBackupRecords)
  - pkg/backup/restore executor (RunRestore with manifest-version re-check)
  - pkg/backup/destination/fs (fs.New + PutBackup + GetManifestOnly)
  - pkg/backup/manifest (Parse/Validate version gate)
provides:
  - test/e2e/helpers.MetadataBackupRunner (shared helper consumed by Plans 03, 04)
  - test/e2e/helpers.ListLocalstackMultipartUploads (chaos-test ghost-MPU assertion helper)
  - pkg/backup/restore.TestRestoreExecutor_RejectsFutureManifestVersion (executor-level SAFETY-03 proof)
  - pkg/backup/restore.TestManifestParse_RejectsFutureManifestVersion (parse-layer SAFETY-03 proof)
affects:
  - Phase-7 downstream plans (03 E2E backup flows, 04 chaos tests) — now have a single helper to consume
tech-stack:
  added: []
  patterns:
    - functional-helper struct (MetadataBackupRunner) mirroring stores.go style
    - require.Eventually polling with caller-supplied timeout (T-07-08: no infinite spin)
    - typed-error assertion via errors.As (RestorePreconditionError)
    - parallel executor-level + parse-layer gate coverage (defense in depth for SAFETY-03)
key-files:
  created:
    - test/e2e/helpers/backup_metadata.go
    - pkg/backup/restore/version_gate_restore_test.go
  modified: []
decisions:
  - "Placed TestManifestParse_RejectsFutureManifestVersion in pkg/backup/restore/ (alongside the executor-level test) rather than pkg/backup/manifest/ — keeps both SAFETY-03 gate proofs colocated and exercises the fs-destination integration point (GetManifestOnly) without crossing package boundaries."
  - "Manifest package exposes no typed sentinel for version rejection — it returns a plain fmt.Errorf with message \"unsupported manifest_version N\". The parse-layer test asserts on the message string as the stable contract; the executor-level test asserts on the typed restore.ErrManifestVersionUnsupported sentinel that the restore package adds."
  - "StartRestore helper returns the error instead of failing the test, so Plan 04 chaos tests can assert on *RestorePreconditionError via errors.As. Added StartRestoreMustSucceed + StartRestoreExpectPrecondition wrappers for the two common patterns."
metrics:
  duration: ~8 minutes
  completed: 2026-04-18
---

# Phase 07 Plan 02: Backup E2E Helpers + Restore Version-Gate Test — Summary

Ship the two supporting assets Phase-7 needs before Plans 03 and 04 can
land: (1) a shared `MetadataBackupRunner` E2E helper that wraps the
`pkg/apiclient` metadata-backup surface, and (2) a unit test that
proves SAFETY-03's manifest-version gate is enforced at BOTH the
restore executor boundary AND the parse layer (defense in depth).

## MetadataBackupRunner public surface

Constructor:
- `NewMetadataBackupRunner(t, client, storeName) *MetadataBackupRunner`

Methods (all `t.Helper()`):
- `CreateLocalRepo(repoName, path string) *apiclient.BackupRepo`
- `CreateS3Repo(repoName, bucket, endpoint string) *apiclient.BackupRepo`
- `TriggerBackup(repoName string) *apiclient.TriggerBackupResponse`
- `PollJobUntilTerminal(jobID string, timeout time.Duration) *apiclient.BackupJob`
- `StartRestore(fromBackupID string) (*apiclient.BackupJob, error)` — returns error (no auto-fail)
- `StartRestoreMustSucceed(fromBackupID string) *apiclient.BackupJob`
- `StartRestoreExpectPrecondition(fromBackupID string) []string` — asserts `*RestorePreconditionError`, returns enabled-shares slice
- `ListRecords(repoName string) []apiclient.BackupRecord`
- `WaitForBackupRecordSucceeded(repoName string, timeout time.Duration) *apiclient.BackupRecord`

Package-level helper (independent of the runner):
- `ListLocalstackMultipartUploads(t, lsHelper, bucket) []s3types.MultipartUpload` — used by Plan 04 chaos tests to assert ghost-MPU cleanup (DRV-02).

All helpers live behind `//go:build e2e` so they compile only for the E2E suite. No collision with existing `test/e2e/helpers/backup.go` (which handles control-plane config backups, a different concept).

## Restore executor re-checks ManifestVersion

The restore executor (`pkg/backup/restore/restore.go` lines 272–275) explicitly re-checks `m.ManifestVersion != manifest.CurrentVersion` after calling `Dst.GetManifestOnly`, wrapping `ErrManifestVersionUnsupported`. This is independent of the parse-layer check inside `manifest.Validate` — meaning the gate holds even if a caller hands the executor a programmatically-constructed future-version `*Manifest` (bypassing YAML parsing). The executor-level test (`TestRestoreExecutor_RejectsFutureManifestVersion`) exercises this path directly via a fake destination that returns `Manifest{ManifestVersion: 2}` without going through `Parse`.

## Parse-layer gate via fs destination

`pkg/backup/manifest.Parse` rejects non-`CurrentVersion` with a plain `fmt.Errorf("unsupported manifest_version %d (this build supports %d)", …)` — no typed sentinel exported. `pkg/backup/destination/fs.Store.GetManifestOnly` calls `manifest.ReadFrom` → `Parse`, so a tampered on-disk `manifest.yaml` is surfaced as an error from `GetManifestOnly` before the executor ever sees a decoded manifest. `TestManifestParse_RejectsFutureManifestVersion` writes a real backup via `fs.PutBackup`, overwrites `manifest.yaml` with `ManifestVersion=2`, and asserts both `manifest.Parse` and `fs.Store.GetManifestOnly` reject the tampered bytes. The assertion uses the documented error-message substring (`"unsupported manifest_version"`) as the stable contract — noted in Decisions above.

## Which sentinel surfaced in the test

- **Executor-level test** — `restore.ErrManifestVersionUnsupported` (typed sentinel, asserted via `errors.Is`).
- **Parse-layer test** — plain `fmt.Errorf` string; asserted via `require.Contains(err.Error(), "unsupported manifest_version")` for both `manifest.Parse` and `fs.Store.GetManifestOnly`.

## Test placement — plan vs PATTERNS.md

07-PATTERNS.md (line 12, line 197) maps the version-gate test to `pkg/backup/manifest/version_gate_test.go`. This plan deliberately placed the test at `pkg/backup/restore/version_gate_restore_test.go` because:

1. The primary subject is the **restore executor's** gate (SAFETY-03 at the executor boundary), not the manifest package's Parse/Validate (that layer already has coverage in `pkg/backup/manifest/manifest_test.go`).
2. Colocating both gate proofs in `pkg/backup/restore/` lets the parse-layer test reuse `fs.New` / `fs.PutBackup` without crossing package import cycles.
3. The executor-level test needs the package's internal fakes (`fakeDest`, `fakeStores`, `newFakeJobStore`, `validManifest`, `buildParams`) which are `package restore` internals.

Matched plan placement; did NOT fall back to PATTERNS.md placement.

## apiclient API — no gaps found

All method signatures referenced in the plan's `<interfaces>` block matched the actual `pkg/apiclient/backup_repos.go` + `pkg/apiclient/backups.go` + `pkg/apiclient/backup_jobs.go`. No adjustments required.

## Verification

```
$ go build -tags=e2e ./test/e2e/...                                   # exit 0
$ go vet -tags=e2e ./test/e2e/helpers/...                              # exit 0
$ go test -run '^TestRestoreExecutor_RejectsFutureManifestVersion$|^TestManifestParse_RejectsFutureManifestVersion$' \
    -count=1 -v ./pkg/backup/restore/...                               # PASS (both tests, <1s)
$ go test -count=1 -timeout=30s ./pkg/backup/restore/...               # ok (full package)
$ go vet ./pkg/backup/restore/...                                       # exit 0
```

## Deviations from Plan

None — plan executed exactly as written, with the one documented decision (test placement in `pkg/backup/restore/` deliberately diverging from PATTERNS.md, as the plan explicitly justified in Task 2 step 5).

## Self-Check: PASSED

- FOUND: test/e2e/helpers/backup_metadata.go
- FOUND: pkg/backup/restore/version_gate_restore_test.go
- FOUND: commit 6051fa97 (Task 1)
- FOUND: commit adabb4de (Task 2)
