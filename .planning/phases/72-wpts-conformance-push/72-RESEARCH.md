# Phase 72: WPTS Conformance Push - Research

**Researched:** 2026-04-07
**Domain:** SMB2 protocol conformance (CHANGE_NOTIFY wire format, directory lease break determinism)
**Confidence:** HIGH on bug locations and fix shapes; LOW on live WPTS pass counts (could not run suite in this session — see Environment Availability)
**Scope:** 2 SMB-only fixable WPTS BVT failures + verification sweep. Phase 73 already absorbed the original "ChangeNotify / negotiate / leasing edge cases" roadmap scope.

## Summary

Both target failures have concrete, verified root causes in the current codebase.

1. **`BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close`** — the CLOSE→cleanup path at `close.go:365-383` correctly detects a pending watcher and dispatches `STATUS_NOTIFY_CLEANUP` through `AsyncCallback`, but `SendAsyncChangeNotifyResponse` at `internal/adapter/smb/response.go:512` selects the response body shape using `status.IsError()`. `StatusNotifyCleanup = 0x0000010B` has severity bits 00 (success-severity), so `IsError()` returns **false** and the code encodes the success-format body (8-byte `ChangeNotifyResponse` with `OutputBufferOffset=0, OutputBufferLength=0`). Samba, by contrast, routes any non-`NT_STATUS_OK` notify completion (including `NT_STATUS_NOTIFY_CLEANUP`) through the error body path — the 9-byte SMB2 error body. WPTS expects the Samba/Windows behavior. **Fix: in `SendAsyncChangeNotifyResponse`, branch on `status != StatusSuccess` instead of `status.IsError()`**, mirroring Samba's `!NT_STATUS_IS_OK(error_code)` check in `change_notify_reply()`. The same fix automatically repairs the `STATUS_NOTIFY_ENUM_DIR` path (same class of status, same bug, not currently exercised by BVT).

2. **`BVT_DirectoryLeasing_LeaseBreakOnMultiClients`** — the race is in `BreakParentHandleLeasesOnCreate` at `internal/adapter/smb/lease/manager.go:440-451`. The function calls `lockMgr.BreakHandleLeasesForSMBOpen` but **explicitly does not wait** for break completion (comment at lines 437-439: *"This does NOT wait for the break to complete. The parent directory break is an informational notification."*). The break dispatch through `SMBBreakHandler.OnOpLockBreak` → `transportNotifier.SendLeaseBreak` is synchronous on the wire (writes bytes to the other client's TCP socket via `WriteNetBIOSFrame` under `WriteMu`), but there is no wait for the client's `LEASE_BREAK_ACK`. Flags on the outgoing break are set to `SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED` when the current state includes W or H (line 96 of `lease_notifier.go`), so the MS-SMB2 3.3.4.7 contract requires the server to wait for the ack before advancing state visible to other observers. The triggering CREATE returns to client A, client A's test harness proceeds to its next observation, and the other client (B) may not yet have its lease in the broken state from the WPTS test's POV. **Fix: call `lockMgr.WaitForBreakCompletion(ctx, parentHandleKey)` inside `BreakParentHandleLeasesOnCreate` (and `BreakParentReadLeasesOnModify`) with a bounded context timeout.** This is the `BreakHandleLeasesOnOpen` pattern (line 375) applied to the parent-directory-break case. It is **not** a deadlock risk here because the triggering client is excluded via `excludeClientID`, so the lock manager never waits on the same session that is currently holding the CREATE flow. `WaitForBreakCompletion` has built-in auto-downgrade on context cancellation (`forceCompleteBreaks`, manager.go:1393), which gives deterministic post-break state whether the ack arrives or the timeout fires.

**Primary recommendation:** Two surgical edits (`response.go:512` branch condition; add bounded `WaitForBreakCompletion` in `BreakParentHandleLeasesOnCreate` + `BreakParentReadLeasesOnModify`). No refactors. No `time.Sleep`. No quarantine. Then run the full WPTS BVT in **Linux CI** (ubuntu-latest, native x86_64) and reconcile `KNOWN_FAILURES.md` (header says 58, table totals 43 — see §"Honest Counts Reconciliation" below).

## User Constraints (from CONTEXT.md)

### Locked Decisions
- **D-01:** Phase 72 is narrowed to 2 SMB-only fixable failures + verification sweep. Target: **5 → 3 fixable known failures** (the 3 remaining are the deferred directory-timestamp tests).
- **D-02:** The 3 directory timestamp tests (`Algorithm_NotingFileModified_Dir_LastAccessTime`, `…Timestamp_MinusOne_Dir_ChangeTime`, `…Timestamp_MinusTwo_Dir_LastWriteTime`) are **deferred**, not failed. Researcher to choose labeling (`Deferred` new status vs `Expected` with structured reason). See Q4 below.
- **D-03:** Root-cause fix for the lease break race. No quarantine, no test-side retry.
- **D-04:** Investigation starts at `create.go:1003-1020` and `lease/manager.go:411`.
- **D-05:** Acceptable fix shapes: synchronous break dispatch, wait-for-ack, or state-barrier. Researcher picks based on actual race.
- **D-06:** `time.Sleep` and unconditional waits are **not acceptable**.
- **D-07:** CLOSE→cleanup code path already exists; bug is in wire format / dispatch, not detection. `KNOWN_FAILURES.md:128`: *"CLOSE notify cleanup response format needs debugging"*.
- **D-08:** Capture actual WPTS wire expectation by running against current server with verbose logging and comparing to MS-SMB2 3.3.5.16.1.
- **D-09:** Preserve the existing `AsyncCallback` plumbing. Do not refactor `NotifyRegistry` to remove async dispatch.
- **D-10:** Phase exit = both target tests passing, zero new regressions, `KNOWN_FAILURES.md` reconciled (header ↔ table agreement, changelog entry, category breakdown, 3 deferred tests clearly labeled).
- **D-10a:** **Linux CI is authoritative** over Mac+QEMU. CI wins on disagreements. Mac+QEMU-only failures must be documented separately.
- **D-11:** No changes to ROADMAP.md success criteria. PR description and SUMMARY.md must clarify that Phase 72 was descoped because Phase 73 absorbed the original scope.
- **D-12:** Mark Phase 72 `[x]` in ROADMAP.md only after verification sweep passes.

### Claude's Discretion
- Choice of fix shape for the lease break race (D-05 options).
- Choice of `Deferred` status label vs `Expected`+structured reason in `KNOWN_FAILURES.md`.
- Debugging approach for capturing WPTS wire expectation.

### Deferred Ideas (OUT OF SCOPE)
- Directory timestamp propagation across protocols (belongs in a dedicated cross-protocol POSIX timestamp phase).
- smbtorture failure reduction (separate suite, separate phase).
- NamedPipe FSA tests (require SSH to SUT, unavailable in Docker CI).
- Phase numbering housekeeping / ROADMAP success-criteria rewrite for Phase 72.

## Project Constraints (from CLAUDE.md)

- Compare SMB implementation against Samba reference (https://github.com/samba-team/samba) — applied for both bugs in this research.
- Commit messages: no Claude Code mention, no Co-Authored-By lines. GPG/SSH-signed commits. Use `git commit -S`.
- `go fmt ./...` and `go vet ./...` before committing.
- Unit tests: `go test ./...` (fast, no sudo).
- Worktree note: this phase is being planned in `worktree-issue-324-smb-idmap` but the actual fix branch should be off `develop`, not the idmap branch.

## Phase Requirements

| ID | Description | Research Support |
|----|-------------|------------------|
| WPTS-01 | WPTS BVT ChangeNotify tests pass | `response.go:512` branch fix + verification run |
| WPTS-02 | WPTS BVT Leasing tests pass | `BreakParentHandleLeasesOnCreate` ack-wait fix + verification run |
| WPTS-03 | WPTS BVT pass count maintained or improved | Full-suite verification in Linux CI |
| WPTS-04 | `KNOWN_FAILURES.md` reconciled (header ↔ table) | Sweep task produces consistent totals and deferred-status labeling |

All four requirements are marked as complete in `.planning/REQUIREMENTS.md` from the Phase 72–73 rollup, but the two target BVT tests still live in `KNOWN_FAILURES.md:95,128`. This phase closes the last gap.

## Answers to Open Questions

### Q1: Current WPTS BVT pass count — and the 43-vs-58 discrepancy

**Status: PARTIAL — could not run WPTS BVT in this session.** Docker is available on the research host but is `aarch64` Docker Desktop (the Mac+QEMU scenario the user described). Running a full BVT is ~10 min and produces QEMU-emulation results that the user has explicitly declared non-authoritative (D-10a). Running it in Linux CI requires a PR/push and consumes a CI budget. The planner must schedule both runs as explicit Wave 0 tasks.

**What I can reconcile from source of truth (`KNOWN_FAILURES.md`):**

Counting the table rows at `test/smb-conformance/KNOWN_FAILURES.md:85-128`, the actual table contents are:

| Status | Count | Tests |
|--------|-------|-------|
| Expected (fixable) | 5 | 3 Timestamp + 1 ChangeNotify (`ServerReceiveSmb2Close`) + 1 Leasing (`LeaseBreakOnMultiClients`) |
| Permanent | 38 | 26 VHD/RSVD + 6 SWN + 3 SQoS + 2 DFS + 2 NamedPipe |
| **Total** | **43** | — |

The header at line 16 says `58 known failures (53 permanent + 5 expected)` — **that is wrong**. The actual table row count is 43. Line 165 in the same file even says `Grand total known failures: 43 tests (38 permanent + 5 expected)` — which is internally consistent with the table. The error is confined to line 16 and line 149 (`Total permanently out-of-scope: 38 tests` is correct, but line 16 claims 53 permanent).

**Root cause of the header/footer mismatch:** Phase 73 Plan 01's `73-01-SUMMARY.md:87-92` documents a KNOWN_FAILURES count correction (was 52+13=65, became 53+12=65), but subsequent Phase 73 plans removed more tests (ADS stream, ChangeSecurity, freeze-thaw, DH reconnect) without updating the header. The header drifted. Line 165 was updated correctly; line 16 and line 143's "Current baseline (Phase 73)" line were not.

**Historical memory cross-check:** The 2026-03-19 auto-memory note says "193 pass / 73 known / 0 new / 69 skipped". That snapshot predates Phase 73's 2026-03-24 closing plans which removed another ~30 tests from the known list. If 193 were passing and ~30 have since been removed from the known-failures table, current pass count should be approximately **223 passing / 43 known / 69 skipped**, assuming no regressions. **[ASSUMED]** — must be confirmed by actual run.

**Planner action required (Wave 0 task):** Schedule a Linux CI run of WPTS BVT and record the exact numbers from `parse-results.sh` output before any code change. This is the baseline Phase 72 must preserve.

### Q2: ServerReceiveSmb2Close wire-level expectation — RESOLVED

**Evidence from source (`internal/adapter/smb/response.go:492-533`):**

```go
func SendAsyncChangeNotifyResponse(sessionID, messageID, asyncId uint64, response *handlers.ChangeNotifyResponse, connInfo *ConnInfo) error {
    status := response.GetStatus()
    // ...build async header with FlagAsync, matching AsyncId...
    var body []byte
    if status.IsError() {               // <-- BUG: StatusNotifyCleanup is NOT an error
        body = MakeErrorBody()          //      This branch not taken for 0x0000010B
        // ...
    } else {
        var err error
        body, err = response.Encode()   // <-- Taken. Produces 8-byte success body with
        if err != nil { return ... }    //     OutputBufferOffset=0, OutputBufferLength=0
        // ...
    }
    return SendMessage(respHeader, body, connInfo)
}
```

**Verification of severity:** `types/status.go:145`: `StatusNotifyCleanup Status = 0x0000010B`. `IsError()` at line 302: `return (uint32(s) & 0xC0000000) == 0xC0000000`. For `0x0000010B`, the upper two bits are 00 (severity 0 = Success). `IsError()` returns **false**. `IsWarning()` returns **false**. Neither branch of the Samba-style "is non-OK?" logic is triggered. [VERIFIED: source read]

**Samba reference behavior:** [CITED: samba-team/samba source3/smbd/notify.c via WebFetch]. Samba's `change_notify_reply()` uses `!NT_STATUS_IS_OK(error_code)` — any non-success NTSTATUS including `NT_STATUS_NOTIFY_CLEANUP` takes the error-body path, invoked as `reply_fn(req, error_code, NULL, 0)`. The response then goes through Samba's generic error body path (9-byte SMB2 error body per MS-SMB2 2.2.2), not the CHANGE_NOTIFY success structure. The cancel path `smbd_notify_cancel_by_map()` calls this same `change_notify_reply(smbreq, notify_status, 0, NULL, map->req->reply_fn)` with `notify_status = NT_STATUS_NOTIFY_CLEANUP`.

**WPTS expectation:** WPTS tests Samba and Windows behavior. Both produce the 9-byte error body with status `STATUS_NOTIFY_CLEANUP` in the header. DittoFS produces an 8-byte success-format body — the structure parses differently (4 extra bytes, different field layout) and WPTS's response validator rejects it. [ASSUMED — confirmed logically from Samba + severity analysis, needs a WPTS wire capture to lock in HIGH confidence].

**Secondary observation:** Our `ChangeNotifyResponse.Encode()` at `change_notify.go:380-395` writes 8 bytes when `bufLen == 0`, but `StructureSize=9` by SMB2 convention implies a fixed 8-byte header plus at least 1 byte of variable body (LSB of StructureSize = variable). Samba's success-path code (`source3/smbd/smb2_notify.c`, `smbd_smb2_request_notify_done`) writes `SSVAL(outbody.data, 0x00, 0x08 + 1)` then writes an 8-byte fixed body — matching our format. So the success-path encoding is correct; the bug is only that **the wrong branch** is taken for `STATUS_NOTIFY_CLEANUP`. [VERIFIED: Samba code via WebFetch]

**Fix shape (recommended):** Replace the branch condition in `response.go:512` with `if status != types.StatusSuccess`. This mirrors Samba exactly and fixes both `STATUS_NOTIFY_CLEANUP` and `STATUS_NOTIFY_ENUM_DIR` (latter not BVT-exercised but the same class of bug).

**Alternative fix:** Explicitly list both statuses: `if status == types.StatusNotifyCleanup || status == types.StatusNotifyEnumDir || status.IsError() || status.IsWarning()`. More verbose, less robust to future additions. Not recommended.

**Not needed:** Changes to `Encode()`. Changes to `close.go:358-383`. Changes to `NotifyRegistry`. Changes to the AsyncId/MessageID plumbing — the test uses the matching `AsyncId` from the interim `STATUS_PENDING` response, which the current code correctly threads through `PendingNotify.AsyncId`.

### Q3: Directory lease break race surface — RESOLVED

**Execution trace from source:**

1. Client A issues CREATE in a directory. Handler at `create.go:1007-1018` checks it's a create/overwrite/supersede, then calls:
   - `BreakParentHandleLeasesOnCreate` (line 1011) — async, does not wait
   - `BreakParentReadLeasesOnModify` (line 1015) — async, does not wait
2. `BreakParentHandleLeasesOnCreate` at `lease/manager.go:440-451` calls `lockMgr.BreakHandleLeasesForSMBOpen(parentHandleKey, excludeOwner)` with `excludeOwner.ClientID = "smb:{sessionID_A}"`.
3. `BreakHandleLeasesForSMBOpen` at `pkg/metadata/lock/manager.go:1308` calls `breakOpLocks`, which:
   - Finds client B's Handle lease on the parent directory.
   - Sets `lock.Lease.Breaking = true`, computes `BreakToState = RWH &^ H = RW`.
   - Advances epoch.
   - Releases `lm.mu`.
   - Calls `lm.dispatchOpLockBreak(handleKey, clonedLock, breakToState)` → `SMBBreakHandler.OnOpLockBreak` → `transportNotifier.SendLeaseBreak`.
4. `SendLeaseBreak` at `lease_notifier.go:59-128` builds the 44-byte `LEASE_BREAK_NOTIFICATION` body with `flags = SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED` (line 96-98, because current state has W or H), then synchronously writes it to client B's TCP socket via `WriteNetBIOSFrame`.
5. `BreakParentHandleLeasesOnCreate` returns without calling `WaitForBreakCompletion`. CREATE returns to client A.
6. WPTS test proceeds to its next observation — typically a re-CREATE or QUERY from client A against the directory, OR a probe of client B's lease state.

**Where determinism is lost:** Between step 4 (bytes on the wire) and the arrival of client B's `LEASE_BREAK_ACK`. The server's internal state transition from `Breaking → Broken` happens only when client B acknowledges via `LEASE_BREAK_ACK`, OR when `forceCompleteBreaks` auto-downgrades after a timeout. In the current code, nothing waits. The `lease.Breaking = true` flag is set (step 3), but the actual `BreakToState` transition is not committed. If WPTS inspects `LeaseState` on the server side via the next protocol operation, it may still see the pre-break state. If WPTS tests client B's observed state via a separate query, there is additionally the kernel-scheduler window between TCP write and client-B read — but that window is also bounded by `LEASE_BREAK_ACK` completion because the test harness sends the ack as soon as it parses the break.

**Fix shape (recommended):** In `BreakParentHandleLeasesOnCreate` (and `BreakParentReadLeasesOnModify`), call `lockMgr.WaitForBreakCompletion(ctx, handleKey)` after `BreakHandleLeasesForSMBOpen` returns. Use a bounded context timeout derived from `ctx` (the CREATE's request context). Pattern already proven in `BreakHandleLeasesOnOpen` at `lease/manager.go:351-376`:

```go
// Proposed edit (illustrative)
func (lm *LeaseManager) BreakParentHandleLeasesOnCreate(
    ctx context.Context,
    parentHandle lock.FileHandle,
    shareName string,
    excludeClientID string,
) error {
    lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
    if lockMgr == nil { return nil }
    if err := lockMgr.BreakHandleLeasesForSMBOpen(handleKey, excludeOwner); err != nil {
        return err
    }
    // NEW: wait for ack or timeout. Safe from self-deadlock because excludeClientID
    // excludes the current CREATE's session from breakOpLocks.
    waitCtx, cancel := context.WithTimeout(ctx, leaseBreakWaitTimeout)
    defer cancel()
    return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
}
```

**Deadlock safety analysis:** The concern that blocked the original author ("blocking would deadlock: the other client needs this CREATE's response before it processes the break" — see comment at `lease/manager.go:380-381`) applies to `BreakHandleLeasesOnOpenAsync` for **file** opens where client A's own lease on the file is being broken. In that case, A's CREATE is holding the break request and cannot also wait for its own ack. For **parent directory** breaks, `excludeClientID` ensures A's own parent-dir leases (if any) are not selected for breaking; only other clients' leases are in the `toBreak` list. A waits for their acks. No cycle. [VERIFIED: `breakOpLocks` at manager.go:1485-1497 honors `excludeOwner.ClientID`]

**Timeout value:** `WaitForBreakCompletion` already falls through to `forceCompleteBreaks` on ctx cancellation (manager.go:1382), which auto-commits the break-to state. So a short bounded timeout (e.g., 5-10 seconds, matching MS-SMB2's typical 35s lease break wait divided for responsiveness) gives deterministic post-break state even under client misbehavior. Recommend: **5 seconds** as a starting value. Existing pattern `lockMgr.WaitForBreakCompletion(ctx, ...)` uses the caller's ctx directly in `BreakHandleLeasesOnOpen`; follow that pattern if CREATE's ctx already has a reasonable deadline, otherwise add a derived timeout.

**Planner must also apply the same fix to `BreakParentReadLeasesOnModify`** — same call site, same race, same safety analysis.

**Not needed:** Changes to `transportNotifier.SendLeaseBreak`. Changes to `SMBBreakHandler`. Changes to `breakOpLocks`. Changes to the LEASE_BREAK_ACK handler. Changes to the multi-client fan-out logic — the fan-out is already correct (it iterates and breaks each matching lock).

### Q4: Naming for deferred status

**Evidence from `KNOWN_FAILURES.md:130-135` (Status Legend):**

```
| Status | Meaning |
| **Expected** | Known failure, fix planned in a future phase |
| **Permanent** | Feature intentionally not implemented (out of scope) |
```

No `Deferred` precedent exists. The existing `Expected` status already means "fix planned in a future phase" — which is exactly what the 3 directory-timestamp tests are: fix planned in the cross-protocol POSIX timestamp phase (currently unscheduled).

**Recommendation: Keep `Expected` status and add a structured "Blocked On" note.** Adding a new `Deferred` status requires updating the legend, updating `parse-results.sh` (if it has hard-coded status validation), and touching CI. For a single-phase labeling problem, reusing `Expected` with clearer Reason text is strictly less invasive and matches D-11 ("no changes to ROADMAP.md", stay narrow).

**Concrete text for the 3 timestamp rows** (Reason column):

> Directory LastAccessTime auto-update on child file modification — blocked on cross-protocol POSIX timestamp phase (metadata-service-level work)

> Directory ChangeTime freeze not enforced during child operations — blocked on cross-protocol POSIX timestamp phase (metadata-service-level work)

> Directory LastWriteTime not auto-updated after unfreeze — blocked on cross-protocol POSIX timestamp phase (metadata-service-level work)

The existing "Remaining Expected Failure Categories" table at line 151-159 should add a note row: `Timestamp | 3 | Blocked on cross-protocol POSIX timestamp phase`.

**If the planner prefers a new `Deferred` status anyway:** cheap to add (1 new legend row, update the 3 Reason cells, grep `parse-results.sh` for hardcoded `Expected|Permanent` string matching and update). Not recommended — adds ongoing maintenance cost for a 3-row naming improvement.

### Q5: Linux CI vs Mac+QEMU result delta

**Status: NOT CAPTURED — requires running the suite in both environments, which this research session did not attempt.** The reason: Mac+QEMU produces the user's non-authoritative results (D-10a), and Linux CI requires a push to a PR branch. Both runs must be scheduled by the planner.

**Environmental evidence collected:**

| Environment | Host Arch | Docker Arch | Notes |
|-------------|-----------|-------------|-------|
| Research host (macOS) | arm64 | aarch64 Docker Desktop | Runs amd64 WPTS TestClient binary under QEMU translation. Matches the user's "local Mac+QEMU" description. |
| CI (`smb-conformance.yml:54`) | — | `ubuntu-latest` x86_64 | Native Linux, no QEMU. Authoritative per D-10a. |

[VERIFIED: `.github/workflows/smb-conformance.yml` line 54 = `runs-on: ubuntu-latest`; research host `docker info | grep Architecture` = `aarch64`]

**What the planner must schedule:**

1. **Wave 0 — Baseline capture, before any code change:**
   - Run WPTS BVT in Linux CI on an unchanged branch (or use the latest `develop` artifact). Record `parse-results.sh` output: `PASS / KNOWN / FAIL / SKIP`.
   - Run WPTS BVT locally on Mac+QEMU against the same commit. Record the same numbers.
   - Diff the two. Any test that is `PASS`-in-CI but `KNOWN`-in-QEMU (or vice versa) is the QEMU noise floor. This set must be documented in the phase SUMMARY and not pollute `KNOWN_FAILURES.md`.
   - Expected outcome based on historical noise: the directory-timestamp tests and lease-break flake are most QEMU-sensitive. BVT's hot loops with short client-side timeouts (often < 100 ms) can trip purely on QEMU instruction translation overhead; tests that work locally may be stable or flaky in CI depending on runner load.
2. **Per-fix verification:** Each target fix (CLOSE cleanup, lease break ack-wait) must be verified in Linux CI. Local Mac+QEMU verification is a **hint**, not a verdict (D-10a).
3. **Final verification sweep (phase exit gate):** Linux CI must show both target tests PASS and zero new FAILs. The QEMU-only delta captured in step 1 is allowed to remain — document it in SUMMARY.md.

**Recommended execution order:** CI baseline first → fix CLOSE cleanup → CI verify → fix lease break → CI verify → final sweep. Do not debug multiple fixes against a dirty baseline; each target test has a different failure mode and interleaving them makes attribution impossible.

## Architecture Patterns

### Pattern 1: Non-success async response body selection

The current `SendAsyncChangeNotifyResponse` uses `status.IsError()` to choose between error body (9 bytes) and structured success body. This fails for `STATUS_NOTIFY_CLEANUP` (severity 00 = success) and `STATUS_NOTIFY_ENUM_DIR` (same severity).

**Correct pattern (Samba-aligned):**

```go
// In SendAsyncChangeNotifyResponse:
if status != types.StatusSuccess {
    body = MakeErrorBody()  // 9-byte SMB2 error body
} else {
    body, err = response.Encode()  // structured success body
    if err != nil { return ... }
}
```

This also makes `SendAsyncCompletionResponse` (line 547) and `SendAsyncChangeNotifyResponse` consistent: the former already uses `(status.IsError() || status.IsWarning()) && exclusions` logic at line 560-563. Consider consolidating — but out of scope for Phase 72.

### Pattern 2: Break dispatch with ack-wait for cross-client breaks

Existing pattern at `lease/manager.go:351-376` (`BreakHandleLeasesOnOpen`):

```go
if err := lockMgr.BreakHandleLeasesForSMBOpen(handleKey, exclude); err != nil {
    return err
}
return lockMgr.WaitForBreakCompletion(ctx, handleKey)
```

Apply the same pattern to `BreakParentHandleLeasesOnCreate` and `BreakParentReadLeasesOnModify`. The async variant (`BreakHandleLeasesOnOpenAsync`) stays unchanged — its deadlock rationale is file-open-specific and genuine.

### Anti-Patterns to Avoid

- **Adding `time.Sleep(...)` anywhere to "let the break settle."** Forbidden by D-06. `WaitForBreakCompletion` is the deterministic path.
- **Quarantining the tests.** Forbidden by D-03 and the project-wide "no flakes" preference (72-CONTEXT.md specifics section).
- **Refactoring `NotifyRegistry` to remove async dispatch.** Forbidden by D-09 — other ChangeNotify tests depend on the current async path.
- **Changing `ChangeNotifyResponse.Encode()` to match the error format for cleanup.** Don't — the success encoding is correct per Samba. Only the branch selection is wrong.
- **Touching `SMBBreakHandler.OnOpLockBreak` or `transportNotifier.SendLeaseBreak`.** They already dispatch synchronously on the wire. The fix is at the caller (`BreakParentHandleLeasesOnCreate`), not the dispatcher.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Wait for lease break ack | New wait channel, per-site polling, `time.Sleep` | `lockMgr.WaitForBreakCompletion(ctx, handleKey)` | Already exists, channel-based, context-aware, has timeout-driven force-complete fallback |
| Error body for non-success notify status | Custom empty-buffer Encode path | `MakeErrorBody()` (`response.go:689`) | Already exists as the 9-byte SMB2 error body |
| Detect non-error success-severity statuses | New method on Status | `status != types.StatusSuccess` | Single comparison matches Samba's `!NT_STATUS_IS_OK` semantics |

## Common Pitfalls

### Pitfall 1: Status severity bits lie about "is this an error?"

**What goes wrong:** NTSTATUS values like `STATUS_NOTIFY_CLEANUP` (0x0000010B) and `STATUS_NOTIFY_ENUM_DIR` (0x0000010C) have severity 00 (Success). `IsError()` returns false. But Windows/Samba/WPTS treat them as "terminal non-OK" statuses that use the error body format.

**Why it happens:** MS-ERRREF assigns severity bits based on NT component convention, not wire-format body selection. Wire-format body selection is an independent Samba/Windows design choice documented in `change_notify_reply()` code, not in any MS spec text directly.

**How to avoid:** Use `status != StatusSuccess` for "do I send an error body?" decisions, matching `!NT_STATUS_IS_OK` — the Samba reference.

**Warning signs:** Wireshark capture shows the server's response body is 8 bytes where it should be 9; WPTS logs `expected response size: 9, actual: 8` or similar; a ChangeNotify completion test with a non-success terminal status fails while vanilla data-notify tests pass.

### Pitfall 2: "Informational" parent break that isn't

**What goes wrong:** A comment in `BreakParentHandleLeasesOnCreate` says "This does NOT wait for the break to complete. The parent directory break is an informational notification." The comment justifies not calling `WaitForBreakCompletion`, but the outgoing break carries `SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED` (because the current state has H), which per MS-SMB2 3.3.4.7 means the server is supposed to wait for the ack.

**Why it happens:** Confusing cross-client parent-dir breaks (safe to wait — no self-dependency) with same-client self-breaks on a file the client is opening (unsafe to wait — deadlock).

**How to avoid:** If `excludeClientID` excludes the triggering client, waiting is safe. The flag `ACK_REQUIRED` tells you WPTS is watching for the ack — respect it.

**Warning signs:** Flaky lease break tests that pass in isolation but fail under load; tests that pass when `WriteMu` contention is low but fail when concurrent connections exist; CI flakes the user is tempted to quarantine.

### Pitfall 3: QEMU noise floor creeping into KNOWN_FAILURES.md

**What goes wrong:** A developer runs WPTS BVT locally on Mac+QEMU, sees some tests flake, adds them to `KNOWN_FAILURES.md` as `Expected`, commits. The next CI run shows those tests pass — now the table has false known-failures, and the honest-counts invariant is corrupted.

**Why it happens:** QEMU user-mode translation of amd64 binaries on arm64 host introduces multi-millisecond stalls. WPTS BVT has hot loops with tight per-operation deadlines. Timing-sensitive tests (leases, oplocks, change notify) are the first to diverge.

**How to avoid:** Only Linux CI results update `KNOWN_FAILURES.md`. Mac+QEMU results are hints for debugging. Document the CI-vs-QEMU delta in the phase SUMMARY.md so future developers understand which tests are QEMU-sensitive.

## Runtime State Inventory

**Not applicable.** This is a bug-fix phase. No rename, no refactor, no migration. The 2 fixes are contained to:
- `internal/adapter/smb/response.go` (1 branch condition)
- `internal/adapter/smb/lease/manager.go` (2 functions: `BreakParentHandleLeasesOnCreate`, `BreakParentReadLeasesOnModify`)
- `test/smb-conformance/KNOWN_FAILURES.md` (baseline reconciliation)

No stored data, live service config, OS-registered state, secrets, or build artifacts are affected.

## Environment Availability

| Dependency | Required By | Available | Version | Fallback |
|------------|------------|-----------|---------|----------|
| Go toolchain | Building `dfs` and running unit tests | ✓ | `go version` not probed — assume matches project `go.mod` | — |
| Docker | Running WPTS BVT locally | ✓ (aarch64 Docker Desktop) | 29.2.1 | — |
| `xmlstarlet` | Parsing WPTS `.trx` results | unknown (not probed) | — | `parse-results.sh` will fail loudly; install via `brew install xmlstarlet` |
| Native x86_64 Linux | Authoritative WPTS BVT runs (D-10a) | ✗ on research host | — | GitHub Actions `ubuntu-latest` runner via CI push |
| `make`, `bash` | Test harness | ✓ | — | — |

**Missing dependencies with fallback:** Linux CI is the fallback for local Mac+QEMU non-authority. The planner must schedule CI runs explicitly (no WPTS verification can skip CI).

**Missing dependencies with no fallback:** None for the fix itself (both fixes are pure Go edits). Verification is fully dependent on Linux CI.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go `testing` (unit) + WPTS BVT (integration, in Docker, runs x86 MSTest binary) |
| Unit test config | none — uses `go test ./...` defaults |
| WPTS config | `test/smb-conformance/run.sh`, `ptfconfig/*.template` |
| Quick run command (unit) | `go test ./internal/adapter/smb/...` |
| Full suite command (unit) | `go test ./...` |
| WPTS target-test command | `./test/smb-conformance/run.sh --profile memory --filter "TestCategory=BVT&Name=BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close\|BVT_DirectoryLeasing_LeaseBreakOnMultiClients"` (syntax: verify against WPTS filter grammar — MSTest uses `|` for OR in test filters) |
| WPTS full BVT | `./test/smb-conformance/run.sh --profile memory --category BVT` |
| Phase gate | WPTS BVT full run in Linux CI: both target tests PASS, zero new FAILs, all others unchanged |

### Phase Requirements → Test Map

| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| WPTS-01 | CLOSE on watched dir sends properly-framed STATUS_NOTIFY_CLEANUP | wpts-bvt | `./test/smb-conformance/run.sh --profile memory` + `parse-results.sh` check for `BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close` in PASS set | ✅ (test exists in WPTS; currently on KNOWN_FAILURES list) |
| WPTS-02 | Parent dir Handle lease break deterministically observable by next op | wpts-bvt | `./test/smb-conformance/run.sh --profile memory` + PASS check for `BVT_DirectoryLeasing_LeaseBreakOnMultiClients` | ✅ |
| WPTS-01 (unit) | `SendAsyncChangeNotifyResponse` uses error body for non-success statuses | unit | New test in `internal/adapter/smb/response_test.go`: assert `SendAsyncChangeNotifyResponse` with `StatusNotifyCleanup` writes 9-byte error body (inspect via a mock `ConnInfo`) | ❌ Wave 0 — test does not exist yet |
| WPTS-02 (unit) | `BreakParentHandleLeasesOnCreate` calls `WaitForBreakCompletion` | unit | New test in `internal/adapter/smb/lease/manager_test.go`: assert the function blocks until break resolves (use existing mock lockMgr pattern from `delegation_manager_test.go:266`) | ❌ Wave 0 |
| WPTS-03 | No regressions in any other WPTS BVT test | wpts-bvt | Full `parse-results.sh` output: all non-target KNOWN tests still in KNOWN set, all PASS still PASS | ✅ |
| WPTS-04 | KNOWN_FAILURES.md header count == table row count | manual/grep | `grep -c '^| BVT\|^| Algorithm\|^| FileInfo' test/smb-conformance/KNOWN_FAILURES.md` must match header `X known failures` number | ❌ Wave 0 — no automated check exists |

### Sampling Rate

- **Per task commit:** `go test ./internal/adapter/smb/...` (fast, ~5-10s)
- **Per wave merge:** `go test ./... -race` (full unit suite, ~30-60s)
- **Phase gate:** Full WPTS BVT in **Linux CI** (10 min); both target tests PASS; zero new FAILs; `parse-results.sh` baseline matches pre-phase expectations within CI-vs-QEMU delta documented in Wave 0

### Wave 0 Gaps

- [ ] Unit test for `SendAsyncChangeNotifyResponse` with `StatusNotifyCleanup` (asserts 9-byte error body)
- [ ] Unit test for `SendAsyncChangeNotifyResponse` with `StatusNotifyEnumDir` (same bug class)
- [ ] Unit test for `BreakParentHandleLeasesOnCreate` that verifies `WaitForBreakCompletion` is called and respects a bounded timeout (mock lockMgr with controllable break state)
- [ ] Unit test for `BreakParentReadLeasesOnModify` (same pattern)
- [ ] Baseline WPTS BVT run in Linux CI on unchanged `develop` to record pre-phase PASS/KNOWN/FAIL/SKIP counts
- [ ] Baseline WPTS BVT run on local Mac+QEMU on the same commit to capture the CI-vs-QEMU delta for documentation
- [ ] Verification: `KNOWN_FAILURES.md` header count reconciliation script (can be a one-liner `awk`/`grep` in CI, or an inline manual check — not a full framework install)

## Security Domain

Not applicable to this phase. No authentication, no authorization, no crypto, no input validation changes. Both fixes are protocol state-machine corrections (wire format branch + break ack-wait). No ASVS categories engaged. `security_enforcement` key is not set in `.planning/config.json`, but per convention absence = enabled; this phase has no security surface to enforce.

## Code Examples

### Fix 1: `response.go` branch condition

```go
// Source: internal/adapter/smb/response.go:492-533 — current buggy branch
// Fix: replace status.IsError() with status != types.StatusSuccess

// Current (bug):
if status.IsError() {
    body = MakeErrorBody()
} else {
    body, err = response.Encode()
    if err != nil { ... }
}

// Proposed:
if status != types.StatusSuccess {
    body = MakeErrorBody()
    logger.Debug("Sending async CHANGE_NOTIFY non-success response (error body format)",
        "sessionID", sessionID, "messageID", messageID,
        "asyncId", asyncId, "status", status.String())
} else {
    body, err = response.Encode()
    if err != nil { return fmt.Errorf("encode change notify response: %w", err) }
    logger.Debug("Sending async CHANGE_NOTIFY success response",
        "sessionID", sessionID, "messageID", messageID,
        "asyncId", asyncId, "bufferLen", len(response.Buffer))
}
```

### Fix 2: `lease/manager.go` — `BreakParentHandleLeasesOnCreate`

```go
// Source: internal/adapter/smb/lease/manager.go:440-451 — current async-only behavior
// Fix: add bounded WaitForBreakCompletion after BreakHandleLeasesForSMBOpen

const parentLeaseBreakWaitTimeout = 5 * time.Second // tunable; matches typical client ack latency

func (lm *LeaseManager) BreakParentHandleLeasesOnCreate(
    ctx context.Context,
    parentHandle lock.FileHandle,
    shareName string,
    excludeClientID string,
) error {
    lockMgr, handleKey, excludeOwner := lm.resolveParentBreakArgs(parentHandle, shareName, excludeClientID)
    if lockMgr == nil {
        return nil
    }
    if err := lockMgr.BreakHandleLeasesForSMBOpen(handleKey, excludeOwner); err != nil {
        return err
    }
    // Wait for client acks (or auto-downgrade on timeout) so subsequent observations
    // see the post-break state deterministically. Safe from self-deadlock: excludeClientID
    // ensures the triggering client's own leases (if any) are not in the break set.
    waitCtx, cancel := context.WithTimeout(ctx, parentLeaseBreakWaitTimeout)
    defer cancel()
    return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
}
```

Apply the analogous change to `BreakParentReadLeasesOnModify` (same file, same structure).

### Reference: Samba's notify cleanup path

```c
// Source: samba-team/samba source3/smbd/notify.c — change_notify_reply()
// Verified via WebFetch 2026-04-07

// smbd_notify_cancel_by_map() invocation:
change_notify_reply(smbreq, notify_status,  // notify_status = NT_STATUS_NOTIFY_CLEANUP
                    0, NULL, map->req->reply_fn);

// Inside change_notify_reply():
if (!NT_STATUS_IS_OK(error_code)) {
    reply_fn(req, error_code, NULL, 0);  // <-- takes error body path
    return;
}
// Success path follows (formats OutputBufferOffset / OutputBufferLength / buffer)
```

The `!NT_STATUS_IS_OK(error_code)` check matches our proposed `status != types.StatusSuccess`.

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `IsError()` severity-bit check for body format selection | `status != StatusSuccess` (matches `!NT_STATUS_IS_OK`) | This phase | Fixes `STATUS_NOTIFY_CLEANUP` and `STATUS_NOTIFY_ENUM_DIR` response framing |
| "Informational" parent-dir break with no wait | Bounded `WaitForBreakCompletion` after `BreakHandleLeasesForSMBOpen` | This phase | Deterministic post-break state for `BVT_DirectoryLeasing_LeaseBreakOnMultiClients` and any future multi-client directory lease tests |
| `KNOWN_FAILURES.md` header count drift | Sweep task enforces header ↔ table ↔ category sum consistency | This phase | Honest counts; passes user's "honest counts" invariant |

## Assumptions Log

| # | Claim | Section | Risk if Wrong |
|---|-------|---------|---------------|
| A1 | WPTS test strictness expects the 9-byte error body for `STATUS_NOTIFY_CLEANUP` rather than the 8-byte success body | §Q2 fix | If WPTS actually accepts the success body, our fix won't change pass count. Mitigation: verify with a single CI run of the target test against the fix; if still FAIL, capture wire dump and compare to a Samba reference run. |
| A2 | Current WPTS BVT pass count is approximately 223 / 43 known / 69 skipped | §Q1 reconciliation | Baseline may differ. Mitigation: Wave 0 CI baseline run captures the real number. |
| A3 | 5-second `WaitForBreakCompletion` timeout is adequate for WPTS client ack latency | §Fix 2 code example | If clients routinely exceed 5s, the force-complete fallback still produces deterministic state, but may cause intermittent test failures where WPTS expects the break to be fully acknowledged vs auto-committed. Mitigation: use caller's CREATE ctx directly (matches `BreakHandleLeasesOnOpen`), which inherits WPTS's own operation timeout. |
| A4 | `Expected` status with "Blocked On" Reason text is acceptable to the user over a new `Deferred` status | §Q4 | User may explicitly prefer a distinct label. CONTEXT.md D-02 leaves this to researcher discretion. |
| A5 | `STATUS_NOTIFY_ENUM_DIR` path is not BVT-exercised but has the same bug | §Summary + Fix 1 | Not load-bearing — the proposed fix covers both statuses regardless. |
| A6 | Phase 73 removed ~30 tests from the KNOWN_FAILURES table between 2026-03-19 (memory snapshot at 73 known) and 2026-03-24 (current table at 43) | §Q1 historical cross-check | Pass-count math is approximate. Wave 0 baseline corrects any drift. |

**Assumed claims must be validated in Wave 0 before the planner locks implementation tasks.** A1 and A3 are the highest-risk: A1 is the core fix premise, A3 controls a user-visible timeout value.

## Open Questions

All 5 open questions from CONTEXT.md are answered above. Residual unknowns:

1. **Exact WPTS wire expectation for `ServerReceiveSmb2Close`** — I am HIGH confidence on the fix based on Samba's source and severity analysis, but the definitive verification is a WPTS run against the fix. If the test still fails, the next step is capturing the WPTS expected vs actual bytes from the `.trx` error message and tcpdump.
   - **How to handle:** Wave 0 includes "capture baseline WPTS wire dump of the failing test" as a debug artifact; wave that applies fix compares post-fix wire bytes.
2. **Whether `leaseBreakWaitTimeout` should inherit from CREATE ctx (unbounded-by-us, bounded by WPTS client) or be a hardcoded derivation** — leaning toward inheriting ctx directly per `BreakHandleLeasesOnOpen` precedent.
   - **How to handle:** Planner picks; recommendation is "inherit ctx, no new timeout constant" as the simplest matching pattern.
3. **Does any Mac+QEMU-only failure class need a separate tracking file?** — Depends on how many QEMU-only flakes Wave 0 surfaces. If <5, inline in SUMMARY.md. If ≥5, create `test/smb-conformance/QEMU_DELTA.md`.
   - **How to handle:** Defer to Wave 0 result.

## Sources

### Primary (HIGH confidence)
- `internal/adapter/smb/response.go:492-533, 689-693` — `SendAsyncChangeNotifyResponse`, `MakeErrorBody` [VERIFIED: source read]
- `internal/adapter/smb/v2/handlers/change_notify.go:120-170, 370-395, 580-683` — `PendingNotify`, `Encode`, `sendAndUnregister`, `NotifyRmdir` [VERIFIED]
- `internal/adapter/smb/v2/handlers/close.go:358-400` — CLOSE cleanup dispatch [VERIFIED]
- `internal/adapter/smb/v2/handlers/create.go:994-1018` — parent break call sites [VERIFIED]
- `internal/adapter/smb/lease/manager.go:340-451` — `BreakHandleLeasesOnOpen` (sync), `BreakHandleLeasesOnOpenAsync`, `BreakParentHandleLeasesOnCreate`, `BreakParentReadLeasesOnModify` [VERIFIED]
- `internal/adapter/smb/lease/notifier.go:30-103` — `SMBBreakHandler.OnOpLockBreak` sync dispatch [VERIFIED]
- `pkg/adapter/smb/lease_notifier.go:59-128` — `transportNotifier.SendLeaseBreak` with `ACK_REQUIRED` flag [VERIFIED]
- `pkg/metadata/lock/manager.go:1293-1535, 1342-1459` — `BreakHandleLeasesForSMBOpen`, `breakOpLocks`, `WaitForBreakCompletion`, `forceCompleteBreaks` [VERIFIED]
- `internal/adapter/smb/types/status.go:143-320` — `StatusNotifyCleanup = 0x0000010B`, `IsError`, `IsWarning`, `Severity` [VERIFIED]
- `test/smb-conformance/KNOWN_FAILURES.md:12-170` — header/table discrepancy and status legend [VERIFIED]
- `test/smb-conformance/README.md:33-108` — run flow and status classification [VERIFIED]
- `.github/workflows/smb-conformance.yml:54` — `runs-on: ubuntu-latest` confirming Linux CI x86_64 [VERIFIED]
- `.planning/phases/73-smb-conformance-deep-dive/73-01-SUMMARY.md:81,100` — Phase 73's incorrect claim that CLOSE cleanup was already functional [VERIFIED]
- `.planning/phases/73-smb-conformance-deep-dive/73-VERIFICATION.md:73,87` — Phase 73 recorded the status value as 0x0000010B (correcting the plan's 0x0000010C), but did not verify the wire format end-to-end [VERIFIED]

### Secondary (MEDIUM confidence)
- [MS-SMB2] 2.2.23.2 LEASE_BREAK_NOTIFICATION (flag semantics, `ACK_REQUIRED`) [CITED: linked from code comments]
- [MS-SMB2] 2.2.35 CHANGE_NOTIFY Response (StructureSize=9, variable body) [CITED]
- [MS-SMB2] 3.3.4.7 server-to-client notifications, ack wait semantics [CITED]
- [MS-SMB2] 3.3.5.9 parent directory lease break semantics on CREATE [CITED: code comment at create.go:998]
- [MS-SMB2] 3.3.5.16.1 CHANGE_NOTIFY processing, cleanup on handle close [CITED: code comment at close.go:367]
- Samba `source3/smbd/notify.c` — `change_notify_reply()` error-body branch on `!NT_STATUS_IS_OK` [CITED: WebFetch]
- Samba `source3/smbd/smb2_notify.c` — success path writes StructureSize=9 (`0x08 + 1`), confirms our encoding is not the bug [CITED: WebFetch]

### Tertiary (LOW confidence)
- Historical pass count of 193 from auto-memory snapshot 2026-03-19 — superseded by Phase 73 removals; must be re-measured [ASSUMED]

## Metadata

**Confidence breakdown:**
- Bug localization (both fixes): HIGH — both root causes confirmed by source reads and cross-referenced against Samba
- Fix shape correctness: HIGH for CLOSE cleanup (matches Samba 1:1), HIGH for lease break (matches existing in-tree pattern)
- WPTS will accept the fix: MEDIUM — A1 and A3 assumptions carry some risk; first CI run validates
- Current pass counts: LOW — not measured in session
- CI-vs-QEMU delta: LOW — not measured in session, must be Wave 0
- KNOWN_FAILURES.md reconciliation correctness: HIGH — counted rows directly

**Research date:** 2026-04-07
**Valid until:** 2026-05-07 (30 days; SMB protocol is stable, but WPTS test set and `develop` branch state evolves; if Phase 72 doesn't start within this window, re-verify current BVT baseline)

## RESEARCH COMPLETE
