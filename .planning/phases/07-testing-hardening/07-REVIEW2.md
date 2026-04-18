---
phase: 07-testing-hardening
reviewed: 2026-04-18T00:00:00Z
depth: quick
files_reviewed: 3
files_reviewed_list:
  - pkg/backup/destination/corruption_test.go
  - test/e2e/backup_chaos_test.go
  - pkg/backup/concurrent_write_backup_restore_test.go
findings:
  critical: 0
  warning: 2
  info: 1
  total: 3
status: issues_found
---

# Phase 07: Code Review Report (Round 2)

**Reviewed:** 2026-04-18
**Depth:** quick
**Files Reviewed:** 3
**Status:** issues_found

## Summary

These three files are the recently-simplified Phase 7 testing & hardening additions:
a corruption-vector integration test suite for the backup destination layer, an E2E
chaos test for kill-mid-backup and kill-mid-restore, and a concurrent-write backup/restore
integration test. All five previously-flagged code-review fixes appear applied correctly:
no hardcoded secrets in production-path code, no dangerous functions, no debug artifacts
(console.log/debugger), no empty catch blocks, and no commented-out code blocks. No new
critical issues were introduced.

Two warnings and one info item were found, all pre-existing patterns that survived
simplification.

## Warnings

### WR-01: writerErrs counter inflated by expected context-cancelled errors

**File:** `pkg/backup/concurrent_write_backup_restore_test.go:92-149`

**Issue:** The goroutine checks `writerCtx.Err() != nil` at the top of each loop
iteration, but each of the five operations (`GenerateHandle`, `DecodeFileHandle`,
`PutFile`, `SetParent`, `SetChild`, `SetLinkCount`) also receives `writerCtx`.
When the 100 ms timeout fires mid-way through building a file, one or more of
those calls will return `context.DeadlineExceeded`. Each such return increments
`writerErrs` and the test asserts `require.Zero(t, writerErrs.Load())` at line 149.
In practice, the final loop iteration almost always straddles the deadline, so
`writerErrs` will typically be non-zero, causing the test to fail spuriously.

**Fix:** Distinguish context-cancellation errors from real store errors. Either
tolerate context errors in the counter or check for them explicitly before adding:

```go
if err != nil {
    if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
        writerErrs.Add(1)
    }
    i++
    continue
}
```

Apply the same guard to every error-check arm inside the goroutine (lines 97–138).

---

### WR-02: sp1 left running if test panics before ForceKill in KillMidRestore

**File:** `test/e2e/backup_chaos_test.go:145-189`

**Issue:** In `TestBackupChaos_KillMidRestore`, `sp1` is started at line 145 with
no `t.Cleanup` guard (by design, because it is intended to be killed manually at
line 189). If the test panics or a `require.*` assertion fires between lines 145
and 189 (e.g., the backup poll at line 173 times out), `sp1`'s process is never
killed. This leaves a dangling server process that holds the state directory and
port, which can cause subsequent test runs in the same CI job to fail.

The same pattern is already handled correctly in `TestBackupChaos_KillMidBackup`
(line 64 comment acknowledges it), but that test exits quickly after kill.
KillMidRestore has a significantly longer window (lines 145–189 include two
`require.Equal` assertions and a `PollJobUntilTerminal` call) where a panic or
failed assertion could leave sp1 alive.

**Fix:** Register a conditional cleanup that only fires if the manual kill did not
already run, using a flag:

```go
sp1Killed := false
t.Cleanup(func() {
    if !sp1Killed {
        sp1.ForceKill()
    }
})
// ... test body ...
sp1.ForceKill()
sp1Killed = true
```

Or use `sp1.ForceKill()` idempotently (if `ForceKill` is safe to call twice,
simply register `t.Cleanup(sp1.ForceKill)` unconditionally, as is done for `sp2`).

---

## Info

### IN-01: `_ = errors.Is` sentinel reference in TestCorruptionHelpers_Smoke

**File:** `pkg/backup/destination/corruption_test.go:295`

**Issue:** Line 295 contains `_ = errors.Is` — a blank-identifier reference used
solely to prevent an "imported and not used" compile error for the `errors` package,
because the smoke test itself does not call `errors.Is` directly. This is a
minor code smell; the `errors` package is only needed in `runCorruptionCase` (same
file), which already uses it. Since both are in the same package the import is
satisfied by that usage alone; the blank assignment is dead weight.

**Fix:** Remove line 295. The `errors` import at line 21 is justified by
`errors.Is` usage elsewhere in the file; the blank reference is redundant.

---

_Reviewed: 2026-04-18_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: quick_
