---
status: awaiting_human_verify
trigger: "BVT_DirectoryLeasing_LeaseBreakOnMultiClients still fails in Linux WPTS CI post-Plan-01 with System.TimeoutException — Plan 01 fix targeted wrong path"
created: 2026-04-07
updated: 2026-04-09
---

## Current Focus

hypothesis: ROOT CAUSE CONFIRMED via code reading. The cross-key conflict path in `requestLeaseImpl` (leases.go:279) calls `WaitForBreakCompletion` with a hardcoded 35s timeout. In the WPTS test scenario, **the test orchestrates Client1's ACK only AFTER Client2's CREATE returns** — so Client2's CREATE blocks the test's main goroutine, the test never drives Client1 to ack, and the wait deadlocks the test for 35s. WPTS client gives up at 8s → System.TimeoutException. The exact same async-vs-sync deadlock is documented at `internal/adapter/smb/lease/manager.go:389` (`BreakHandleLeasesOnOpenAsync`): "blocking would deadlock: the other client needs this CREATE's response before it processes the break."
test: Write a Go unit test mirroring `TestRequestLease_CrossKeyConflict` (line 205) but with a callback that NEVER acks. Measure how long Client2's RequestLease blocks. Bound it with t.Deadline / per-test timeout to assert <1s.
expecting: On unmodified code the test hangs for 35s. After fix it returns in <100ms.
next_action: Write the failing test, run it (RED), apply the fix (drop the wait — the breaking lease is already correctly handled by `OpLocksConflict` which treats Breaking leases as having their BreakToState — see oplock.go:229-233), run again (GREEN).

## Symptoms

expected: Client2's CREATE on existing dir with new lease key returns within ~1s with downgraded lease; WPTS observes lease break on Client1 and test passes within 8s deadline.
actual: Client2's CREATE hangs ≥8s; WPTS CreateOpenFromClient throws System.TimeoutException; server eventually closes the connection.
errors: System.TimeoutException at LeasingExtendedTest.CreateOpenFromClient (DirectoryLeasingExtendedTest.cs:799) called from BVT_DirectoryLeasing_LeaseBreakOnMultiClients (line 180).
reproduction: Code-level only — write a Go unit test on Manager.RequestLease with two clients, same dir handle, different lease keys, R|H each, break ack never sent.
started: Pre-Plan-01 (already in KNOWN_FAILURES.md as Expected). Plan 01 attempted fix in wrong functions.

## Eliminated

- hypothesis: Hang is in BreakParentHandleLeasesOnCreate / BreakParentReadLeasesOnModify
  evidence: Per prior audit + 72-postfix-ci.txt: WPTS test opens existing directory twice, no FileCreated/FileOverwritten/FileSuperseded, so the parent-break block in create.go:1007 is skipped entirely. Also no "CREATE: parent directory Handle lease break failed" log line in dittofs.log for the failing run.
  timestamp: 2026-04-07 (prior audit)

## Evidence

- timestamp: 2026-04-07
  checked: 72-postfix-ci.txt server-side dittofs.log excerpt (line 76868)
  found: "RequestLease: cross-key conflict, initiating break ... existingState=RW requestedState=RW breakToState=R"
  implication: The CI log says existingState=RW (has Write bit). leases.go:230 computes breakTo = state &^ Write → R. Therefore the break IS dispatched (not skipped at line 235), and the 35s wait at line 279 IS reached.

- timestamp: 2026-04-07
  checked: leases.go:211-291 cross-key conflict path
  found: After dispatchOpLockBreak, code calls WaitForBreakCompletion with ctx.WithTimeout(ctx, 35*time.Second). 35s >> WPTS 8s test-step deadline.
  implication: If client never acks within 8s, Client2's RequestLease blocks for 35s, client gives up at 8s, test fails. This is the hang site.

- timestamp: 2026-04-07
  checked: 72-postfix-ci.txt log paraphrase ambiguity (existingState=RW vs pure R|H)
  found: The CI log paraphrase says existingState=RW. The test "BVT_DirectoryLeasing_LeaseBreakOnMultiClients" by name and convention typically uses R|H (READ|HANDLE) leases on directories — Write isn't valid on directory leases anyway. Need to verify whether the log was paraphrased or whether something in the lease pipeline transforms R|H to RW (unlikely).
  implication: There's residual ambiguity. Either (a) the log was paraphrased and the real existingState is RH, in which case breakTo = RH &^ W = RH (no change), and the cross-key block exits at line 235 WITHOUT dispatching a break — meaning the hang is somewhere else; OR (b) the log is verbatim and existingState really is RW somehow. Option (a) is more consistent with directory R|H semantics. Need code reading + repro to disambiguate.

## Resolution

root_cause: requestLeaseImpl (pkg/metadata/lock/leases.go:279) called WaitForBreakCompletion with a 35s hardcoded timeout after dispatching a cross-key lease break. In multi-client scenarios where a single test driver orchestrates both clients, the existing client (Client1) cannot ack until the new opener's (Client2) CREATE returns — but Client2 is parked inside this wait. Pure deadlock. WPTS BVT client times out at ~8s with System.TimeoutException. Plan 01's BreakParentHandleLeasesOnCreate / BreakParentReadLeasesOnModify fix is correct but unrelated: that path is only hit on FileCreated/FileOverwritten/FileSuperseded, while the WPTS test opens an existing directory twice.

fix: Drop the WaitForBreakCompletion call in requestLeaseImpl entirely. The dispatchOpLockBreak above it is already synchronous (LEASE_BREAK_NOTIFICATION is on the wire by the time it returns), satisfying MS-SMB2 3.3.4.7 ordering. The existing breaking lease stays in unifiedLocks with Breaking=true and BreakToState set, and OpLocksConflict (oplock.go:229-233) already evaluates conflicts against BreakToState in that case — so bestGrantableState computes the correct downgraded grant for the new opener immediately, without waiting for the ack. This mirrors the documented async pattern at internal/adapter/smb/lease/manager.go:389 (BreakHandleLeasesOnOpenAsync), whose comment explicitly calls out "blocking would deadlock: the other client needs this CREATE's response before it processes the break".

Note on the prior log evidence: 72-postfix-ci.txt's "existingState=RW requestedState=RW breakToState=R" log line is consistent with the actual server log format (notifier.go:85, leases.go:244) and matches the in-tree directory-lease semantics — directories CAN hold RW (TestRequestLease_DirectoryState_RW asserts this; valid dir states are None/R/RW per oplock.go:191). The WPTS test therefore really does request RW directory leases and the cross-key conflict path really is hit. The wire-trace lines (22:11:22.193 etc.) showing "READ|HANDLE → READ" are paraphrased commentary, not raw log lines. Treat the [DEBUG] lines as authoritative.

verification:
- New unit test TestRequestLease_CrossKeyConflict_DoesNotBlockOnAck asserts RequestLease returns within 1s when the existing client never acks. RED before fix (1.00s timeout fired), GREEN after fix (0.00s).
- TestAcknowledgeLeaseBreak_CompletesBreak updated to use assert.Eventually for the async ack-to-None landing (previously relied on the synchronous wait to make ack ordering deterministic). Passes.
- Full pkg/metadata/lock/... test suite passes (with -race).
- Full internal/adapter/smb/... test suite passes (no regressions).
- Full pkg/metadata/... test suite passes.
- go vet ./pkg/metadata/lock/... clean.
- WPTS BVT_DirectoryLeasing_LeaseBreakOnMultiClients verification requires Linux CI (Windows SUT not available locally) — pending human-verify checkpoint.

files_changed:
  - pkg/metadata/lock/leases.go (drop WaitForBreakCompletion in cross-key path)
  - pkg/metadata/lock/leases_test.go (RED-then-GREEN test + Eventually for ack)

commits:
  - 87ddabfa test(72-01): add red test for cross-key lease conflict deadlock
  - ede32b1f fix(72-01): drop blocking ack-wait in cross-key lease conflict path
