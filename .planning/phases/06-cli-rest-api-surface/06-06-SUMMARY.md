---
phase: 06-cli-rest-api-surface
plan: 06
subsystem: cli/dfsctl/restore
tags: [cli, dfsctl, restore, d-01, d-02, d-03, d-08, d-11, d-29, d-30, d-31, d-33, d-34, d-40]
requires:
  - phase: 06-cli-rest-api-surface
    provides: "Plan 02 apiclient StartRestore (returns *BackupJob), RestoreDryRun (returns *DryRunResult), RestorePreconditionError"
  - phase: 06-cli-rest-api-surface
    provides: "Plan 05 backup.WaitForJob shared poll helper + ErrPollDetached / ErrPollTimeout sentinels + SuccessExitCode"
  - phase: 06-cli-rest-api-surface
    provides: "Plan 04 share disable / enable verbs (precondition remediation path)"
provides:
  - "restore.Cmd (dfsctl store metadata <store> restore) — final Phase 6 verb"
  - "metadata parent registers all 8 subtrees: list / add / edit / remove / health / backup / repo / restore"
affects: []
tech-stack:
  added: []
  patterns:
    - "waitFn swappable seam around backup.WaitForJob so detach / timeout / transport paths unit-test cleanly without reaching backup-package-private globals (pollInterval, notifyInterrupt, interruptHandler)"
    - "confirmFunc test seam replaces promptui drive-through"
    - "humanSize duplicated (not imported) to keep restore package's dep graph independent of the backup package"
key-files:
  created:
    - cmd/dfsctl/commands/store/metadata/restore/restore.go
    - cmd/dfsctl/commands/store/metadata/restore/restore_test.go
    - cmd/dfsctl/commands/store/metadata/metadata_test.go
  modified:
    - cmd/dfsctl/commands/store/metadata/metadata.go
key-decisions:
  - "Introduced waitFn wrapper around backup.WaitForJob so tests inject detach/timeout sentinels without touching backup-package-private globals (pollInterval/notifyInterrupt/interruptHandler aren't exported; reaching around that would require modifying merged Plan 05 code)"
  - "Duplicated humanSize locally rather than promoting backup.HumanSize — 8 lines of trivial formatter vs. cross-package dependency for the restore leaf"
  - "Used confirmFunc var seam instead of stdin redirection so promptui never runs in tests"
  - "Detach (ErrPollDetached) returns nil (not os.Exit) — aligned with Plan 05 run.go Warning 10 guard; exit code stays 0"
  - "Task 3 split: automated portion completed here, live-server / interactive / Ctrl-C checkpoint deferred to the user (see 'Task 3 — Deferred human checkpoint' below)"
patterns-established:
  - "waitFn seam pattern for wrapping imported-package helpers when the imported package's test seams are private"
requirements-completed: [API-02]
metrics:
  duration: ~25min
  completed: 2026-04-17T00:00:00Z
---

# Phase 6 Plan 6: Restore CLI + parent wiring Summary

Ships the final Phase 6 verb — `dfsctl store metadata <store> restore` — plus
the 3-line wire-up that makes Plans 03/04/05/06 consumable end-to-end via the
`metadata` parent command.

## Scope

Two user-facing effects:

1. **New `restore` verb** (331 LOC + 515 LOC tests): confirmation prompt
   (D-30), server-side dry-run via Plan 02's `RestoreDryRun` endpoint (D-31),
   hard 409 gate with per-share disable hints (D-29), success hint to
   re-enable shares (D-33), client-side 26-char ULID validation (D-40),
   reuse of Plan 05 `WaitForJob` for `--wait` / `--async` / `--timeout`
   semantics (D-01..D-11).
2. **`metadata.go` parent wiring**: imports `backup`, `repo`, `restore`;
   adds three `AddCommand` calls; extends long-help examples.

## Files

| File | Action | LOC |
|------|--------|-----|
| `cmd/dfsctl/commands/store/metadata/restore/restore.go` | created | 331 |
| `cmd/dfsctl/commands/store/metadata/restore/restore_test.go` | created | 515 |
| `cmd/dfsctl/commands/store/metadata/metadata.go` | modified | 51 (was 35) |
| `cmd/dfsctl/commands/store/metadata/metadata_test.go` | created | 42 |

## metadata.go AddCommand order (post-wire)

```go
Cmd.AddCommand(listCmd)
Cmd.AddCommand(addCmd)
Cmd.AddCommand(editCmd)
Cmd.AddCommand(removeCmd)
Cmd.AddCommand(healthCmd)

// Phase 6 additions — backup / restore / repo subtrees.
Cmd.AddCommand(backup.Cmd)
Cmd.AddCommand(repo.Cmd)
Cmd.AddCommand(restore.Cmd)
```

`dfsctl store metadata --help` lists all 8 subcommands (verified via
`go run` smoke under Task 3 automated portion).

## Key confirmations

- **Dry-run routes through server endpoint.** `grep -n 'client.RestoreDryRun'
  cmd/dfsctl/commands/store/metadata/restore/restore.go` → line 248. The CLI
  never stubs dry-run client-side; it issues `POST /api/v1/store/metadata/{name}/restore/dry-run`
  and renders the full `DryRunResult{Record, ManifestValid, EnabledShares}`.
  Warning 8 closed.
- **Detach returns nil (exit 0).** `errors.Is(waitErr, backup.ErrPollDetached)`
  branch returns `nil` immediately — `exitFunc` is NOT invoked. Regression
  guard in `TestRestore_CtrlCDetach_ExitsZero` asserts `exit == -1` (sentinel
  for "exitFunc not called"). Aligned with Plan 05 run.go Warning 10.
- **Client-side ULID length validation fires before HTTP.** Sampled via
  `TestRestore_InvalidULID_ExitsBeforeAPI` which wires a stub server that
  would blow up if hit; `StartCalls == 0` and `DryRunCalls == 0` are
  asserted alongside the error string check.

## Sample outputs

### Happy-path restore (table mode)

```
Restore job 01HAJOB00000000000000000J started. Polling...
Status: pending
Status: pending -> running
Status: running -> succeeded
✓ Restore succeeded
Shares remain disabled. Re-enable with:
  dfsctl share <name> enable
```

Exit code: 0.

### 409 precondition failure (D-29)

```
Cannot restore: 2 share(s) enabled — disable them first.
  dfsctl share /a disable
  dfsctl share /b disable
Error: restore precondition failed: 2 share(s) still enabled
```

Exit code: 1 (cobra propagates RunE error).

### Dry-run (D-31) with enabled shares (does NOT error)

```
Dry run: pre-flight only (no data mutation, no payload download).
  Target store:     fast-meta
  Selected record:  01HABCDEFGHJKMNPQRSTUVWXY1  (created 2026-01-02 03:04:05 UTC, size 2.0KB)
  Manifest:         valid

  Note: 1 share(s) currently enabled on fast-meta:
    /still-on
  Disable them before running the real restore:
    dfsctl share /still-on disable
```

Exit code: 0 (shares-enabled gate is SKIPPED in dry-run per D-31).

### Ctrl-C branches

Under the shared Plan 05 `WaitForJob` poll loop:

- **[d]etach (default):** stderr-only `Detached — job still running. Poll:
  dfsctl store metadata fast-meta backup job show <id>` hint; stdout stays
  clean; exit 0.
- **[c]ancel:** issues `POST /backup-jobs/<id>/cancel`, loop reconverges on
  the resulting `interrupted` terminal; exit 1.
- **[C]ontinue:** spinner resumes, terminal reached normally.

(These branches are exercised directly by Plan 05's `TestWaitForJob_*` tests
plus `TestBackupRun_CtrlCDetach_ExitsZero`; the restore-side regression
guard `TestRestore_CtrlCDetach_ExitsZero` asserts that when WaitForJob
returns `ErrPollDetached`, `runRestore` returns nil without calling
exitFunc.)

## Test outcomes

| Package | Tests | Result |
|---------|-------|--------|
| `cmd/dfsctl/commands/store/metadata` (new `metadata_test.go`) | 2 | pass |
| `cmd/dfsctl/commands/store/metadata/restore` | 13 | pass |
| `cmd/dfsctl/commands/store/metadata/backup` (regression) | existing suite | pass |
| `cmd/dfsctl/commands/store/metadata/backup/job` | existing suite | pass |
| `cmd/dfsctl/commands/store/metadata/repo` | existing suite | pass |

Full `go test ./cmd/dfsctl/... -count=1` — all green. `go build ./...` and
`go vet ./cmd/dfsctl/...` clean.

Restore test list:

1. TestRestore_InvalidULID_ExitsBeforeAPI — D-40 client-side validation
2. TestRestore_Yes_SkipsConfirmation — D-30 force path
3. TestRestore_NoPromptDeclines_Aborts — D-30 decline path
4. TestRestore_SharesEnabled409_RendersHint — D-29 precondition surface
5. TestRestore_DryRun_CallsServerEndpoint — D-31 server-side round-trip
6. TestRestore_DryRun_ManifestInvalid_RendersWarning — manifest warning
7. TestRestore_DryRun_SkipsSharesEnabledGate — D-31 semantics
8. TestRestore_Async_EmitsJob — --async output shape
9. TestRestore_WaitSucceeds_PrintsReEnableHint — D-33 success hint
10. TestRestore_WaitFailed_ExitsNonZero — exit 1 on failed terminal
11. TestRestore_Timeout_ExitsTwo — D-11 timeout exit 2
12. TestRestore_CtrlCDetach_ExitsZero — Warning 10 detach regression
13. TestRestore_WaitGenericError_BubblesUp — transport error propagation

## Task 3 — Automated portion

Completed in-process (all on feature branch `feat/v0.13.0-phase-6-cli-rest-api`):

| Check | Result |
|-------|--------|
| `go build ./...` | clean |
| `go vet ./cmd/dfsctl/...` | clean |
| `go test ./cmd/dfsctl/... -count=1` | all green |
| `go run ./cmd/dfsctl store metadata --help` | 8 subcommands listed (list/add/edit/remove/health/backup/repo/restore) |
| `go run ./cmd/dfsctl store metadata backup --help` | renders; 5 sub-verbs + flags |
| `go run ./cmd/dfsctl store metadata backup job --help` | renders; 3 sub-verbs |
| `go run ./cmd/dfsctl store metadata restore --help` | renders; all 7 flags present |
| `go run ./cmd/dfsctl store metadata repo --help` | renders; 5 sub-verbs |
| `go run ./cmd/dfsctl share --help` | renders; per-share verb layout preserved |
| `grep -n 'client.RestoreDryRun' …restore.go` | 1 match (line 248) — Warning 8 regression guard |
| `grep 'os.Exit(1)' …restore.go \| grep -i 'detach'` | 0 matches — Warning 10 regression guard |

## Task 3 — Deferred human checkpoint

The plan's Task 3 checkpoint bundles live-server and interactive verification
that cannot be automated from this executor:

- [ ] **Step 1** — `dfsctl store metadata smoke-meta repo list` renders table with NAME/KIND/SCHEDULE/RETENTION/ENCRYPTED columns.
- [ ] **Step 2** — `dfsctl store metadata smoke-meta backup --repo smoke-backups` — spinner transitions pending→running→succeeded on stderr, final "✓ Backup completed" on stdout, exit 0.
- [ ] **Step 3** — `dfsctl store metadata smoke-meta backup list` — shows new record with short-ULID + "Xs ago" relative time.
- [ ] **Step 4** — `dfsctl store metadata smoke-meta backup show <record-id>` — grouped sections, all fields populated.
- [ ] **Step 5** — `dfsctl store metadata smoke-meta backup pin <record-id>` then `backup list` shows PINNED=yes.
- [ ] **Step 6** — `dfsctl store metadata smoke-meta backup job list` shows at least one completed job.
- [ ] **Step 7** — Ctrl-C three-way branches on a long-running backup:
  - [ ] Enter (`d` default) → stderr hint + **exit 0** (not 1).
  - [ ] `c` → Cancel POSTed (verify via `backup job list` — status `interrupted`).
  - [ ] `C` → Spinner resumes, ultimately terminal.
- [ ] **Step 8** — `dfsctl store metadata smoke-meta restore --yes` while share is enabled → stderr "Cannot restore: 1 share(s) enabled — disable them first." followed by `dfsctl share /smoke disable`. Exit 1.
- [ ] **Step 9** — `dfsctl store metadata smoke-meta restore --dry-run` WHILE share is enabled → stdout shows "Dry run:", "Target store:", "Selected record: <ULID>", "Manifest: valid", and under "Note:" lists `/smoke` + `dfsctl share /smoke disable` hint. Exit 0. Hits server `/restore/dry-run` (NOT a client-only stub).
- [ ] **Step 10** — `dfsctl share /smoke disable` then `dfsctl store metadata smoke-meta restore --yes` → restore runs, final "✓ Restore succeeded" + re-enable hint. Detach during polling → exit 0.
- [ ] **Step 11** — `dfsctl share /smoke show` renders Enabled row; `dfsctl share /smoke disable` / `enable` work.
- [ ] **Step 12** — `dfsctl store metadata smoke-meta repo remove smoke-backups --purge-archives` → prompts, type `y` → repo gone AND `/tmp/smoke-backups` emptied of Phase-3 archive files.

The orchestrator will hand this off to the user post-merge.

## Deviations from Plan

1. **[Rule 3 — Blocking issue, expected]** The plan's Task 3 is a single
   blocking human-verify checkpoint that bundles automated smoke tests with
   live-server and interactive verification. Per the orchestrator's scoping
   override, the automated portion (build / vet / test / `--help` smoke /
   regression-guard greps) was completed here; the 12 live-server steps are
   recorded above as pending. No code change; documented for traceability.

2. **[Rule 1 — Bug, minor plan drift]** The plan's pseudo-code used
   `cmdutil.GetOutputFormatEnum()` which does not exist in the codebase.
   Substituted the existing `parseFormat()` wrapper pattern from
   `backup/run.go` which calls `cmdutil.GetOutputFormatParsed()` — same
   semantics, matches the merged Plan 05 convention.

3. **[Rule 3 — Blocking issue]** The plan's example imported
   `prompt.ConfirmWithForce` directly and sketched the call site with it.
   That works in production but unit tests cannot drive promptui without
   a TTY. Introduced a `confirmFunc` var seam (`var confirmFunc =
   prompt.ConfirmWithForce`) so tests swap it; production semantics are
   unchanged — the initial value IS `prompt.ConfirmWithForce`. The plan's
   acceptance-criteria grep explicitly accepts either `prompt.ConfirmWithForce`
   OR `confirmFunc`; both patterns appear.

4. **[Rule 3 — Blocking issue]** The plan sketched direct use of
   `backup.WaitForJob(...)` from `runRestore`. The backup package's test
   seams (`pollInterval`, `notifyInterrupt`, `interruptHandler`) are all
   package-private, so detach / timeout / transport-error paths cannot be
   unit-tested from the `restore` package without modifying the merged
   Plan 05 code. Wrapped the call behind a `waitFn` var seam (initial
   value IS `backup.WaitForJob`) — production path unchanged, tests inject
   sentinel returns directly. Added one extra test
   (`TestRestore_WaitGenericError_BubblesUp`) to guard that non-sentinel
   errors propagate as Cobra RunE errors. The acceptance-criteria grep
   `grep -n 'backup.WaitForJob' …` still returns a match (line 55 — the
   seam's initial value assignment).

5. **Test expansion.** The plan listed 12 test cases; I shipped 13 (added
   `TestRestore_WaitGenericError_BubblesUp` as a safety net for the waitFn
   seam). All 13 pass.

## Self-Check

### Files exist
- FOUND: `cmd/dfsctl/commands/store/metadata/restore/restore.go` (331 LOC)
- FOUND: `cmd/dfsctl/commands/store/metadata/restore/restore_test.go` (515 LOC)
- FOUND: `cmd/dfsctl/commands/store/metadata/metadata.go` (modified — 51 LOC)
- FOUND: `cmd/dfsctl/commands/store/metadata/metadata_test.go` (42 LOC)

### Commits exist
- FOUND: `48bc953a` — feat(06-06): restore verb with confirm, server-side dry-run, async-poll
- FOUND: `f008d6a3` — feat(06-06): wire backup / repo / restore into metadata parent

## Self-Check: PASSED

## Phase 6 API-01..API-06 coverage

| Req | Realizer |
|-----|----------|
| API-01 | Plan 02 apiclient + Plan 04 share verbs + Plan 05 backup subtree |
| API-02 | **Plan 06 restore verb (this plan)** |
| API-03 | Plan 05 backup subtree (run + list + show + pin + job.*) |
| API-04 | Plan 04 share disable/enable (remediation path for D-29) |
| API-05 | Plan 02 apiclient typed errors + Plan 05/06 CLI rendering |
| API-06 | Plan 05 backup job verbs + Plan 06 restore reuses same poll helper |

All Phase 6 API-level requirements realized across Plans 01–06.
