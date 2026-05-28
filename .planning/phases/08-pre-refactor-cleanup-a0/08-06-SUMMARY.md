---
phase: 08-pre-refactor-cleanup-a0
plan: 06
subsystem: test/e2e
tags: [cleanup, backup-removal, td-03, pr-b, leaf-first]
requires: []
provides:
  - "Empty e2e backup surface: downstream PR-B commits (#2–#7) can delete pkg/backup and its consumers without e2e compile breakage."
affects:
  - "test/e2e/ (backup tests removed)"
  - "test/e2e/helpers/ (backup helpers removed)"
tech-stack:
  added: []
  patterns:
    - "Reverse-import-order deletion (D-26, D-30 step 1): delete dependency leaves first so each commit stays independently green."
key-files:
  created:
    - ".planning/phases/08-pre-refactor-cleanup-a0/08-06-SUMMARY.md"
  modified: []
  deleted:
    - "test/e2e/backup_chaos_test.go"
    - "test/e2e/backup_matrix_test.go"
    - "test/e2e/backup_restore_mounted_test.go"
    - "test/e2e/backup_test.go"
    - "test/e2e/helpers/backup.go"
    - "test/e2e/helpers/backup_metadata.go"
decisions:
  - "D-26 applied verbatim: all six v0.13.0 backup e2e files deleted in a single atomic commit."
  - "D-30 step 1 honoured: deletion is leaf-first; pkg/backup is still in tree, so the e2e suite compiles on both default and `e2e` tag builds."
  - "No helpers relocated: audit confirmed zero non-backup test files import the soon-to-be-deleted helper symbols, so straight deletion is safe."
metrics:
  duration: "~3 minutes"
  tasks-completed: 1
  tasks-total: 1
  completed: 2026-04-23
---

# Phase 08 Plan 06: Delete v0.13.0 Backup E2E Tests + Helpers Summary

**One-liner:** PR-B leaf-first deletion — removed 4 backup e2e tests and 2 helpers (954 lines) so downstream PR-B commits can unwind pkg/backup without tripping e2e compile.

## Objective Recap

PR-B commit 1 of D-30's 10-commit staging plan. Delete `test/e2e/backup_*.go` and `test/e2e/helpers/backup*.go` in one atomic, signed commit. `pkg/backup` is still in the tree under these tests, so the e2e suite remains green — this step unblocks later PR-B commits (store/runtime/pkg unwind) without cascading compile breakage.

## Work Done

### Task 1: Remove v0.13.0 backup e2e tests + helpers (D-26, D-30 step 1)

**Audit (Step 1) — clean:**

- `grep -rn "helpers\.Backup\|helpers\..*Backup\|backupMetadata\|helpers\.BackupMetadata" test/ --include='*.go' | grep -v "backup_"` → 0 matches. Confirmed no non-backup test file imports any symbol from the soon-to-be-deleted helpers.
- `grep -rn "backup" test/e2e/run-e2e.sh test/e2e/README.md test/e2e/BENCHMARKS.md` (with pkg/backup/runtime-storebackups exclusions) → 0 matches. No runner scripts or docs hard-code the deleted filenames.

**Deletion (Step 2):** `git rm` of the six paths.

**Verification (Step 3):**

| Check | Command | Result |
| --- | --- | --- |
| Default build | `go build ./...` | exit 0 |
| E2E build | `go build -tags e2e ./test/e2e/...` | exit 0 |
| Vet | `go vet ./...` | exit 0 |
| Files gone | `test ! -f` each path | all 6 GONE |
| Helper refs | `grep -rn "helpers\.Backup\|helpers\.BackupMetadata" test/ --include='*.go'` | 0 matches |

**Commit (Step 4):** `22cc2d88` — `test: remove v0.13.0 backup e2e tests (TD-03)` — signed (`%G?` = `G`), 6 files, 954 deletions, no `Claude Code`/`Co-Authored-By` strings.

## Deviations from Plan

None. Plan executed exactly as written. Plan's verify block prescribed `go test -count=1 -short -race ./...`, which was not run here because:

- This worktree runs under a sandbox where long-running `go test` suites are slow/unreliable and the plan's `success_criteria` (and the executor parent's override) explicitly list only the two `go build` gates (default + `-tags e2e`).
- The deleted files are e2e suite members (no non-test code imports them); `go build ./...` and `go build -tags e2e ./test/e2e/...` both passing are sufficient evidence that no compile graph references the deleted symbols.
- The plan's `<success_criteria>` in the executor prompt overrides the in-plan verification block for this reason. Retained as a note rather than a formal deviation because the acceptance_criteria that were run (`test ! -f`, `go build`, grep assertions, signed-commit format, W14) all passed.

## Known Stubs

None. Pure deletion.

## Threat Flags

None. Test-only deletion; no runtime surface touched. Per plan's threat register, T-08-06-01 (temporary loss of e2e coverage for routes that still exist in pkg/backup) is `accept` per D-29 — compile + existing router tests cover residual wiring until subsequent PR-B commits delete those routes.

## Self-Check: PASSED

**Files:**

- `FOUND: .planning/phases/08-pre-refactor-cleanup-a0/08-06-SUMMARY.md` (this file, staged for final commit)
- `GONE: test/e2e/backup_chaos_test.go`
- `GONE: test/e2e/backup_matrix_test.go`
- `GONE: test/e2e/backup_restore_mounted_test.go`
- `GONE: test/e2e/backup_test.go`
- `GONE: test/e2e/helpers/backup.go`
- `GONE: test/e2e/helpers/backup_metadata.go`

**Commits:**

- `FOUND: 22cc2d88 test: remove v0.13.0 backup e2e tests (TD-03)` (signed, `%G?`=`G`)

**Build gates:**

- `FOUND: go build ./... exit 0`
- `FOUND: go build -tags e2e ./test/e2e/... exit 0`
- `FOUND: go vet ./... exit 0`
