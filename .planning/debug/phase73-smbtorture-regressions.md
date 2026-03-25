---
status: awaiting_human_verify
trigger: "Phase 73 SMB conformance changes caused 23 test failures in CI"
created: 2026-03-24T00:00:00Z
updated: 2026-03-24T00:00:00Z
---

## Current Focus

hypothesis: CONFIRMED - Three root causes identified and fixed
test: go build + go test pass locally; need CI verification
expecting: CI should be green after these changes
next_action: User pushes to CI and verifies

## Symptoms

expected: All 23 tests should pass after Phase 73 code changes, OR remain in KNOWN_FAILURES
actual: CI fails with 21 new smbtorture failures and 2 new WPTS failures
errors: rw.invalid regression (STATUS_DISK_FULL check removed), kernel_oplocks5 regression (post-conflict lease granting), prematurely removed KNOWN_FAILURES entries
reproduction: Push to feat/smb-conformance-deep-dive branch
started: Phase 73 changes

## Eliminated

## Evidence

- timestamp: 2026-03-24
  checked: write.go diff
  found: writeEnd == maxFileSize STATUS_DISK_FULL check was removed by Phase 73 code review
  implication: smb2.rw.invalid regression - test expects STATUS_DISK_FULL at NTFS max boundary

- timestamp: 2026-03-24
  checked: leases.go diff (post-conflict granting)
  found: New code grants lease after cross-key conflict (old code returned None). For kernel_oplocks5 (same-client traditional oplocks), the second open gets R instead of None after breaking the first's BATCH.
  implication: smb2.kernel-oplocks.kernel_oplocks5 regression - test expects NONE for second open

- timestamp: 2026-03-24
  checked: KNOWN_FAILURES diff
  found: 28+ tests removed from KNOWN_FAILURES by executor agents claiming fixes, but tests still fail in CI
  implication: Tests need to be re-added to KNOWN_FAILURES

## Resolution

root_cause: |
  Three issues:
  1. write.go: The writeEnd == maxFileSize STATUS_DISK_FULL boundary check was removed during code review/cleanup, causing smb2.rw.invalid to fail.
  2. leases.go: New post-conflict lease granting logic (grant R after cross-key conflict resolves) changed behavior for same-client traditional oplocks, causing kernel_oplocks5 to get Level II instead of expected None.
  3. KNOWN_FAILURES: Executor agents prematurely removed ~28 tests claiming their code changes would fix them, but the changes were insufficient.

fix: |
  1. Restored writeEnd == maxFileSize STATUS_DISK_FULL check in write.go
  2. Reverted post-conflict lease granting in leases.go (return LeaseStateNone after conflict)
  3. Updated leases_test.go to match reverted behavior
  4. Re-added 28 tests to smbtorture KNOWN_FAILURES (9 DH V1, 11 DH V2, 7 lease, 1 notify valid-req, 1 freeze-thaw)
  5. Re-added 2 tests to WPTS KNOWN_FAILURES (ChangeNotify_ChangeSecurity, ChangeNotify_ServerReceiveSmb2Close)

verification: go build passes, all unit tests pass (lock, handlers, SMB adapter)
files_changed:
  - internal/adapter/smb/v2/handlers/write.go
  - pkg/metadata/lock/leases.go
  - pkg/metadata/lock/leases_test.go
  - test/smb-conformance/smbtorture/KNOWN_FAILURES.md
  - test/smb-conformance/KNOWN_FAILURES.md
