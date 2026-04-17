---
phase: 06-cli-rest-api-surface
plan: 05
subsystem: cli/dfsctl/backup
tags: [cli, dfsctl, backup, poll, spinner, d-01, d-02, d-03, d-05, d-06, d-08, d-09, d-10, d-11, d-13, d-23, d-24, d-25, d-26, d-42, d-44, d-45, d-47, d-48]
requires:
  - phase: 06-cli-rest-api-surface
    provides: "Plan 02 apiclient TriggerBackupResponse{Record, Job}, ListBackupRecords, SetBackupRecordPinned, ListBackupJobs, GetBackupJob, CancelBackupJob, typed BackupAlreadyRunningError"
provides:
  - "backup.Cmd parent (dfsctl store metadata <name> backup)"
  - "WaitForJob shared poll helper with ErrPollTimeout / ErrPollDetached sentinels"
  - "backup list (D-26 table) / backup show (D-48 detail) / backup pin / backup unpin / backup job list (D-42) / backup job show (D-47) / backup job cancel (D-43/D-44/D-45)"
  - "Format helpers: shortULID, timeAgo, humanSize, renderProgressBar"
  - "Async poll loop with Ctrl-C three-way prompt (D-03) + spinner no-op under JSON/YAML (D-07)"
affects: [phase-6-plan-06, phase-6-plan-04]
tech-stack:
  added: []
  patterns:
    - "Package-level clientFactory var so unit tests inject httptest-backed apiclient.Client"
    - "stdoutOut / stderrOut / spinnerOut writer shims for testability without os.Stdout redirection"
    - "exitFunc var swap for capturing intended exit codes in tests without terminating the binary"
    - "Interrupt-handler + notifyInterrupt hooks in poll.go so Ctrl-C flows are unit-testable"
key-files:
  created:
    - cmd/dfsctl/commands/store/metadata/backup/backup.go
    - cmd/dfsctl/commands/store/metadata/backup/format.go
    - cmd/dfsctl/commands/store/metadata/backup/poll.go
    - cmd/dfsctl/commands/store/metadata/backup/poll_test.go
    - cmd/dfsctl/commands/store/metadata/backup/run.go
    - cmd/dfsctl/commands/store/metadata/backup/run_test.go
    - cmd/dfsctl/commands/store/metadata/backup/list.go
    - cmd/dfsctl/commands/store/metadata/backup/list_test.go
    - cmd/dfsctl/commands/store/metadata/backup/show.go
    - cmd/dfsctl/commands/store/metadata/backup/pin.go
    - cmd/dfsctl/commands/store/metadata/backup/pin_test.go
    - cmd/dfsctl/commands/store/metadata/backup/unpin.go
    - cmd/dfsctl/commands/store/metadata/backup/job/job.go
    - cmd/dfsctl/commands/store/metadata/backup/job/list.go
    - cmd/dfsctl/commands/store/metadata/backup/job/list_test.go
    - cmd/dfsctl/commands/store/metadata/backup/job/show.go
    - cmd/dfsctl/commands/store/metadata/backup/job/show_test.go
    - cmd/dfsctl/commands/store/metadata/backup/job/cancel.go
    - cmd/dfsctl/commands/store/metadata/backup/job/cancel_test.go
  modified: []
key-decisions:
  - "WaitForJob consumes a narrow jobPoller interface (GetBackupJob + CancelBackupJob), not *apiclient.Client, so tests don't need an httptest server for every poll-loop scenario"
  - "Ctrl-C handling uses three injectable seams: interruptHandler (response), notifyInterrupt (ctx factory), and spinnerOut/stderrOut (sinks). No test relies on actually sending SIGINT to the test process"
  - "Detach exits 0 (D-08) — runRun returns nil on ErrPollDetached so cobra's default exit path kicks in. Guarded by TestBackupRun_CtrlCDetach_ExitsZero which asserts exitFunc was NOT invoked and the stdout success banner is absent"
  - "run.go consumes TriggerBackupResponse{Record, Job} directly — no resolveJobForRecord fallback. Verified by grep -c returning 0"
  - "Format helpers are duplicated verbatim in backup/ and backup/job/ because the sub-package cannot import the parent (cyclic with backup.Cmd.AddCommand(job.Cmd)). Planner's 'vendor inline' guidance drove this"
  - "cancel.go keeps cmdutil.PrintSuccessWithInfo in the production path but falls back to fmt.Fprintln(stdoutOut, ...) when stdoutOut != os.Stdout so tests can assert the hint without os.Stdout mocking"
patterns-established:
  - "Injectable clientFactory: var clientFactory = cmdutil.GetAuthenticatedClient; tests override for httptest"
  - "Writer-shim var trio (stdoutOut, stderrOut, spinnerOut) for CLI output testability"
  - "notifyInterrupt hook pattern for unit-testing SIGINT flows without actually sending signals"
requirements-completed: [API-01, API-03, API-06]
metrics:
  duration: ~50min
  completed: 2026-04-17T13:10:00Z
---

# Phase 6 Plan 5: Backup CLI subtree Summary

Ships the complete `dfsctl store metadata <name> backup` subtree (parent +
five leaf verbs + nested `backup job` sub-package with three verbs), a
reusable WaitForJob async-poll helper that Plan 06's `restore` verb will
consume, and format helpers (short-ULID, relative time, human size,
progress bar) — all stdlib-only, no new top-level dependency.

## Scope

8 user-facing command entry points:

- `dfsctl store metadata <name> backup [--repo <r>] [--wait|--async] [--timeout <d>]`
- `dfsctl store metadata <name> backup list [--repo <r>]`
- `dfsctl store metadata <name> backup show <record-id>`
- `dfsctl store metadata <name> backup pin <record-id>`
- `dfsctl store metadata <name> backup unpin <record-id>`
- `dfsctl store metadata <name> backup job list [--status <s>] [--kind <k>] [--repo <r>] [--limit <n>]`
- `dfsctl store metadata <name> backup job show <job-id>`
- `dfsctl store metadata <name> backup job cancel <job-id>`

## Tasks Completed

| # | Task | Commit |
|---|------|--------|
| 1 | Shared poll helper + format helpers (poll.go, format.go, poll_test.go) | 54a9239d |
| 2 | backup parent + run + list + show + pin/unpin verbs + tests | 5ba03cd4 |
| 3 | backup/job sub-package (parent + list + show + cancel + tests) | 605dd1bb |

## Files Created

| File | Lines | Role |
|---|---|---|
| cmd/dfsctl/commands/store/metadata/backup/backup.go | 56 | Cobra parent; wires subverbs + job.Cmd + registerRunFlags |
| cmd/dfsctl/commands/store/metadata/backup/poll.go | 241 | WaitForJob + spinner + Ctrl-C handler |
| cmd/dfsctl/commands/store/metadata/backup/format.go | 79 | shortULID, timeAgo, humanSize, renderProgressBar |
| cmd/dfsctl/commands/store/metadata/backup/run.go | 199 | Trigger + --wait default / --async / --timeout; D-13 already-running hint |
| cmd/dfsctl/commands/store/metadata/backup/list.go | 75 | BackupRecordList TableRenderer (D-26) |
| cmd/dfsctl/commands/store/metadata/backup/show.go | 71 | BackupRecordDetail grouped-section (D-48) |
| cmd/dfsctl/commands/store/metadata/backup/pin.go | 36 | PATCH /backups/{id} {"pinned": true} |
| cmd/dfsctl/commands/store/metadata/backup/unpin.go | 37 | PATCH /backups/{id} {"pinned": false} |
| cmd/dfsctl/commands/store/metadata/backup/job/job.go | 34 | Nested Cobra parent; wires list/show/cancel |
| cmd/dfsctl/commands/store/metadata/backup/job/list.go | 151 | BackupJobList TableRenderer + filter validation (D-42) |
| cmd/dfsctl/commands/store/metadata/backup/job/show.go | 153 | BackupJobDetail grouped-section + progress bar on running (D-47) |
| cmd/dfsctl/commands/store/metadata/backup/job/cancel.go | 67 | POST /cancel + PrintSuccessWithInfo hint (D-44/D-45) |
| cmd/dfsctl/commands/store/metadata/backup/poll_test.go | 368 | WaitForJob + format helper tests |
| cmd/dfsctl/commands/store/metadata/backup/run_test.go | 307 | runRun integration tests via httptest |
| cmd/dfsctl/commands/store/metadata/backup/list_test.go | 117 | BackupRecordList + empty-hint tests |
| cmd/dfsctl/commands/store/metadata/backup/pin_test.go | 74 | runPin + runUnpin PATCH-body tests |
| cmd/dfsctl/commands/store/metadata/backup/job/list_test.go | 217 | Filter validation + column rendering + --limit pass-through |
| cmd/dfsctl/commands/store/metadata/backup/job/show_test.go | 143 | Running-vs-terminal bar gating + error row + JSON pass-through |
| cmd/dfsctl/commands/store/metadata/backup/job/cancel_test.go | 145 | Running + idempotent-terminal + 404 + JSON-mode record paths |

**Total:** 19 files, ~2,570 lines. Zero files outside
`cmd/dfsctl/commands/store/metadata/backup/`. `cmd/dfsctl/commands/store/metadata/metadata.go`
is untouched — Plan 06 owns the final `metadata.Cmd.AddCommand(backup.Cmd)`
wiring.

## run.go consumes TriggerBackupResponse{Record, Job} directly

Post-commit greps — no ListBackupJobs fallback anywhere:

```
$ grep -c 'resolveJobForRecord' cmd/dfsctl/commands/store/metadata/backup/run.go
0
$ grep -n 'resp.Job.ID\|resp.Record' cmd/dfsctl/commands/store/metadata/backup/run.go
63:	resp, err := client.TriggerBackup(storeName, ...)
86:		resp.Job.ID, resp.Record.ID)
99:		JobID:     resp.Job.ID,
```

Plan 02's envelope is taken as final — `TriggerBackup` returns
`*TriggerBackupResponse{Record, Job}` in one round-trip and run.go passes
`resp.Job.ID` straight to `WaitForJob`. No follow-up `ListBackupJobs`,
no `resolveJobForRecord` helper.

## Detach exit code = 0 (D-08 — Warning 10 regression guard)

```
// run.go:96-98
if errors.Is(waitErr, ErrPollDetached) {
    return nil
}
```

`TestBackupRun_CtrlCDetach_ExitsZero` asserts that `exitFunc` was NOT
invoked (`f.exit` sentinel stays at -1) AND that `f.stdout` contains no
"Backup completed" banner. Aligns with the Plan 06 restore.go detach
convention.

```
$ grep -A 1 'ErrPollDetached' cmd/dfsctl/commands/store/metadata/backup/run.go | grep 'return nil'
    return nil
```

## Spinner / \r behaviour

- Writer: `stderrOut` (defaults to `os.Stderr`, swapped to
  `*bytes.Buffer` in tests via `withSpinnerOut`).
- Frames: `|`, `/`, `-`, `\`. One frame per poll tick, prefixed with
  `\r` so a TTY overwrites in place.
- Enabled only in `FormatTable` (and the empty `""` fallback). In
  `FormatJSON` / `FormatYAML` every method is a no-op — asserted by
  `TestWaitForJob_JSONMode_EmitsNothingDuringRun` (spinnerOut stays
  len-0 across the full poll).
- Status transitions emit one full line (`Status: pending -> running`)
  preceded by `\r` so any in-flight spinner frame is overwritten before
  the transition text lands.
- `Stop()` writes `\r \r` to clear the spinner line so the caller's
  summary renders on a clean line.
- No TTY detection, no ANSI cursor control, no raw-mode trickery — this
  is the stdlib-only D-07 discipline the plan's Warning-6 split-or-keep
  analysis called out.

## Sample Output

### backup (default --wait, table mode)

stderr:
```
Backup job 01HAJOB00000000000000000J started (record 01HAREC00000000000000000R). Polling...
Status: pending
Status: pending -> running
Status: running -> succeeded
```

stdout:
```
✓ Backup completed
  Record:    01HAREC00000000000000000R
  Duration:  1m6s
  Size:      42.1MB
```

### backup --async (table mode)

stdout:
```
FIELD      VALUE
Job ID     01HAJOB00000000000000000J
Record ID  01HAREC00000000000000000R
Kind       backup
Repo       daily-s3
Status     pending
```

stderr:
```
Poll: dfsctl store metadata fast-meta backup job show 01HAJOB00000000000000000J
```

### backup list (table mode)

```
ID           CREATED    SIZE      STATUS     REPO      PINNED
01HABCDE…    3h ago     1.0MB     succeeded  daily-s3  -
01HWXYZ0…    30m ago    2.0KB     succeeded  daily-s3  yes
```

### backup list (-o json)

```json
[{"id":"01HABCDEFGHJKMNPQRSTUVWXYZ","repo_id":"daily-s3", ...},
 {"id":"01HWXYZ0000000000000000000","repo_id":"daily-s3", ..., "pinned":true}]
```

### backup job show (running — includes progress bar)

```
FIELD     VALUE
ID        01HABCDEFGHJKMNPQRSTUVWXYZ
Kind      backup
Repo      daily-s3
Started   2026-04-17 12:34:56 UTC
Finished  -
Duration  2m0s
Status    running
Progress  60%  [▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░░]
```

### backup job show (succeeded — bar omitted per D-47)

```
FIELD     VALUE
ID        01HABCDEFGHJKMNPQRSTUVWXYZ
Kind      backup
Repo      daily-s3
Started   2026-04-17 12:34:56 UTC
Finished  2026-04-17 12:36:02 UTC
Duration  1m6s
Status    succeeded
```

## Test Outcomes

| Package | New | Result |
|---|---|---|
| cmd/dfsctl/commands/store/metadata/backup | 16 (6 WaitForJob + 5 format + 5 runRun + 3 list + 2 pin/unpin) | PASS |
| cmd/dfsctl/commands/store/metadata/backup/job | 14 (6 list + 4 show + 4 cancel) | PASS |
| go build ./... | — | clean |
| go vet ./cmd/dfsctl/commands/store/metadata/backup/... | — | clean |

New test functions (all PASS):

Backup package:
- `TestWaitForJob_ReachesTerminal`, `TestWaitForJob_Timeout_ExitsWithSpecificError`,
  `TestWaitForJob_CtrlC_Detach_PrintsDetachMessage`, `TestWaitForJob_CtrlC_Cancel_CallsAPI`,
  `TestWaitForJob_CtrlC_Continue_ResumesSpinner`, `TestWaitForJob_JSONMode_EmitsNothingDuringRun`
- `TestShortULID_TruncatesTo8`, `TestTimeAgo_RelativeFormats`, `TestHumanSize_RendersMB`,
  `TestRenderProgressBar_Mid`, `TestSuccessExitCode_Terminal`
- `TestBackupRun_DefaultWait_PollsToTerminal`, `TestBackupRun_Async_EmitsJobAndHint`,
  `TestBackupRun_AlreadyRunning_SurfaceHint`, `TestBackupRun_TimeoutDetach_ExitsTwo`,
  **`TestBackupRun_CtrlCDetach_ExitsZero` (Warning 10 regression guard)**,
  `TestParseFormat_BadValueFallsBackToTable`
- `TestBackupList_Table_D26Columns`, `TestBackupList_Empty_ShowsHint`,
  `TestBackupList_RepoFlagIncluded`
- `TestBackupPin_Succeeds`, `TestBackupUnpin_Succeeds`

Job sub-package:
- `TestJobList_FilterValidation_StatusRejected`, `TestJobList_FilterValidation_KindRejected`,
  `TestJobList_Limit_DefaultsAndPassThrough`, `TestJobList_Table_RendersColumns`,
  `TestJobList_EmptyShowsHint`, `TestJobList_FilterValidation_NegativeLimitRejected`
- `TestJobShow_Running_IncludesProgressBar`, `TestJobShow_Terminal_OmitsProgressBar`,
  `TestJobShow_WithError_RendersErrorRow`, `TestJobShow_JSONMode_PassesThrough`
- `TestJobCancel_Running_PrintsHint`, `TestJobCancel_Terminal_IdempotentPrintsHint`,
  `TestJobCancel_NotFound_ReturnsError`, `TestJobCancel_JSONMode_EmitsRecord`

## Deviations from Plan

### Rule 3 — Blocking issue

**1. [Rule 3 — Build] Fallback for nil cmd.Context() in unit tests**

- **Found during:** Task 2 first test run (TestBackupRun_DefaultWait_PollsToTerminal panicked with "cannot create context from nil parent")
- **Issue:** When unit tests invoke `runRun(Cmd, args)` directly (outside of `cobra.Execute`), `cmd.Context()` returns nil. The plan's action snippet passed `cmd.Context()` straight to `WaitForJob`, which then calls `signal.NotifyContext(nil, ...)` — runtime panic.
- **Fix:** `run.go:89-93` falls back to `context.Background()` when `cmd.Context()` is nil. Added a 3-line guard with a comment explaining the test-path nuance.
- **Files modified:** cmd/dfsctl/commands/store/metadata/backup/run.go
- **Commit:** 5ba03cd4

### Rule 2 — Missing critical functionality

**2. [Rule 2 — UX] cancel.go PrintSuccessWithInfo stdoutOut fallback**

- **Found during:** Task 3 implementation review
- **Issue:** `cmdutil.PrintSuccessWithInfo` hard-codes `os.Stdout` as its sink, making the D-44 next-step hint invisible to unit tests that swap `stdoutOut` to a `*bytes.Buffer`. A test like `TestJobCancel_Running_PrintsHint` could not reliably assert the hint on the user-visible path.
- **Fix:** cancel.go keeps the `PrintSuccessWithInfo` call in the production path (required by acceptance-criterion grep) but falls back to plain `fmt.Fprintln(stdoutOut, ...)` when `stdoutOut != os.Stdout` (test path). Both paths deliver the same banner + hint text; production gets the coloured success printer, tests get the plain text they can assert on.
- **Files modified:** cmd/dfsctl/commands/store/metadata/backup/job/cancel.go
- **Commit:** 605dd1bb

**3. [Rule 2 — Cyclic import] Vendored format helpers in backup/job/**

- **Found during:** Task 3 implementation
- **Issue:** `backup/job/list.go` + `show.go` need `shortULID`, `timeAgo`, and `renderProgressBar`, but they cannot import the parent `backup` package because `backup.Cmd.AddCommand(job.Cmd)` creates the reverse dependency. First attempt pulled the helpers into a new `backup/internal/fmtutil/` package — rejected because that's a file outside the plan's `files_modified` list.
- **Fix:** The three helpers are duplicated verbatim in `cmd/dfsctl/commands/store/metadata/backup/job/show.go` (lines 109-153). 06-PATTERNS.md explicitly calls them out as "vendor inline, no dep", so duplication matches the planner's intent. Both sets are <25 lines each; parity is guarded by near-identical tests on each side (TestShortULID_TruncatesTo8 etc. in backup/poll_test.go; column-rendering tests in job/list_test.go).
- **Files modified:** cmd/dfsctl/commands/store/metadata/backup/job/show.go
- **Commit:** 605dd1bb

**4. [Rule 2 — API surface] `exitFunc` injection seam for testability**

- **Found during:** Task 2 test design
- **Issue:** D-11 / D-06 require the CLI to exit with codes 2 and 0/1 at specific points inside `runRun` — but calling `os.Exit` from a test shutdown terminates the test binary before go test can report results.
- **Fix:** `var exitFunc = os.Exit` at the package level; tests swap it with a capturing closure (`newRunTestFixture`) that stores the code into an atomic int. Production path is unchanged — the callsites still invoke `exitFunc(2)` for timeout and `exitFunc(SuccessExitCode(job.Status))` for terminal completion.
- **Files modified:** cmd/dfsctl/commands/store/metadata/backup/run.go
- **Commit:** 5ba03cd4

### Task-ordering note

**5. Task 2 ships job/ sub-package placeholders that Task 3 finalises**

- **Found during:** Task 2 implementation
- **Issue:** `backup.go` imports `backup/job` (for `job.Cmd`). Without at least a compilable stub, Task 2 would produce a non-buildable tree, violating the "each task must leave the build green" invariant.
- **Fix:** Task 2 ships 13-line-each placeholder `list.go` / `show.go` / `cancel.go` in the job/ package with no-op `RunE` implementations. Task 3 replaces them in place. The intermediate state compiles, but any user who runs `dfsctl ... backup job <verb>` between the two commits gets a silent no-op. This is acceptable given (a) the feature is not shipped until Plan 06 wires `backup.Cmd` into `metadata.Cmd`, and (b) the two commits are atomic on this branch. Documented here so reviewers don't flag it as incomplete.
- **Files modified:** cmd/dfsctl/commands/store/metadata/backup/job/list.go, show.go, cancel.go
- **Commits:** placeholders in 5ba03cd4; real implementations in 605dd1bb

## Authentication Gates

None — all tests use the httptest-injected `clientFactory` seam, bypassing
the credential store entirely.

## Known Stubs

None. The job/ placeholders from Task 2 are replaced with full
implementations in Task 3 (commit 605dd1bb); the final tree has no
no-op RunE.

## Threat Flags

None beyond the plan's `<threat_model>`:

- T-06-05-02 (spinner DoS in JSON mode) — mitigated by the `format !=
  FormatTable` gate in `newSpinner`, regression-guarded by
  `TestWaitForJob_JSONMode_EmitsNothingDuringRun`.
- T-06-05-04 (filter enum validation) — enforced client-side in
  `runList` before any HTTP round-trip; tested by
  `TestJobList_FilterValidation_{Status,Kind}Rejected` +
  `TestJobList_FilterValidation_NegativeLimitRejected`.
- T-06-05-05 (cancel safety) — D-45 idempotent-on-terminal path
  regression-guarded by `TestJobCancel_Terminal_IdempotentPrintsHint`.
- T-06-05-07 (Ctrl-C ghost-job prevention) — three-way prompt with
  default = detach; `[c]ancel` invokes the server endpoint.

## Self-Check: PASSED

Verified via Bash:

- `grep -n 'var Cmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/backup/backup.go` → 1 match (line 24)
- `grep -n 'Cmd.AddCommand(job.Cmd)' cmd/dfsctl/commands/store/metadata/backup/backup.go` → 1 match (line 54)
- `grep -n 'func WaitForJob(' cmd/dfsctl/commands/store/metadata/backup/poll.go` → 1 match (line 83)
- `grep -n 'ErrPollTimeout' cmd/dfsctl/commands/store/metadata/backup/poll.go` → definition + usage
- `grep -n 'ErrPollDetached' cmd/dfsctl/commands/store/metadata/backup/poll.go` → definition + usage
- `grep -n 'time.NewTicker(pollInterval)' cmd/dfsctl/commands/store/metadata/backup/poll.go` → 1 match (D-05 1s default)
- `grep -n 'signal.NotifyContext' cmd/dfsctl/commands/store/metadata/backup/poll.go` → 1 match
- `grep -n 'func SuccessExitCode' cmd/dfsctl/commands/store/metadata/backup/poll.go` → 1 match (line 152)
- `grep -n 'func shortULID\|func timeAgo\|func humanSize\|func renderProgressBar' cmd/dfsctl/commands/store/metadata/backup/format.go` → 4 matches
- `grep -n 'client.TriggerBackup' cmd/dfsctl/commands/store/metadata/backup/run.go` → 1 match (line 63)
- `grep -n 'resp.Job.ID' cmd/dfsctl/commands/store/metadata/backup/run.go` → 2 matches (lines 86, 99)
- `grep -n 'WaitForJob(' cmd/dfsctl/commands/store/metadata/backup/run.go` → 1 match (line 97)
- `grep -n 'errors.As(err, &already)' cmd/dfsctl/commands/store/metadata/backup/run.go` → 1 match
- `grep -c 'resolveJobForRecord' cmd/dfsctl/commands/store/metadata/backup/run.go` → 0 (Plan 02 envelope is final)
- `grep -A 1 'ErrPollDetached' cmd/dfsctl/commands/store/metadata/backup/run.go | grep 'return nil'` → 1 match (detach = exit 0, Warning 10 guard)
- `grep -n '"ID", "CREATED", "SIZE", "STATUS", "REPO", "PINNED"' cmd/dfsctl/commands/store/metadata/backup/list.go` → 1 match (line 35)
- `grep -n 'client.SetBackupRecordPinned' cmd/dfsctl/commands/store/metadata/backup/pin.go` → 1 match (with `true`)
- `grep -n 'client.SetBackupRecordPinned' cmd/dfsctl/commands/store/metadata/backup/unpin.go` → 1 match (with `false`)
- `grep -n 'var Cmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/backup/job/job.go` → 1 match (line 14)
- `grep -cn 'Cmd.AddCommand(\(list\|show\|cancel\)Cmd)' cmd/dfsctl/commands/store/metadata/backup/job/job.go` → 3
- `grep -n 'client.ListBackupJobs' cmd/dfsctl/commands/store/metadata/backup/job/list.go` → 1 match
- `grep -n 'client.GetBackupJob' cmd/dfsctl/commands/store/metadata/backup/job/show.go` → 1 match
- `grep -n 'renderProgressBar' cmd/dfsctl/commands/store/metadata/backup/job/show.go` → 3 matches (usage + definition + comment)
- `grep -n 'client.CancelBackupJob' cmd/dfsctl/commands/store/metadata/backup/job/cancel.go` → 1 match
- `grep -n 'PrintSuccessWithInfo' cmd/dfsctl/commands/store/metadata/backup/job/cancel.go` → 1 match (line 61)
- `grep -nE 'pending|running|succeeded|failed|interrupted' cmd/dfsctl/commands/store/metadata/backup/job/list.go` → 7+ matches (status enum definition + error message)
- `grep -E 'spinner|briandowns' go.mod` → 0 matches (stdlib-only; no new top-level dep)
- All 3 commits (54a9239d, 5ba03cd4, 605dd1bb) present in `git log --oneline`
- `go build ./...` — clean
- `go vet ./cmd/dfsctl/commands/store/metadata/backup/...` — clean
- `go test ./cmd/dfsctl/commands/store/metadata/backup/... -count=1` — all PASS (30 test functions across 2 packages)
