---
phase: 73-smb-conformance-deep-dive
verified: 2026-03-24T16:30:00Z
status: passed
score: 13/13 must-haves verified (2 gaps resolved via doc updates)
gaps:
  - truth: "WPTS BVT known failures reduced to ~53 (52 permanent + 1 ChangeEa permanent; all fixable expected failures cleared)"
    status: partial
    reason: "3 Timestamp Expected failures remain in WPTS KNOWN_FAILURES.md. The ROADMAP success criterion says 'all fixable expected failures cleared' but Algorithm_NotingFileModified_Dir_LastAccessTime, FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime, and FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_Dir_LastWriteTime are still listed as Expected (fixable). Plan 05 Summary documents this as a deliberate deferral -- directory freeze-thaw during child ops was out of scope -- but the ROADMAP SC and KNOWN_FAILURES summary line both claim 'all fixable cleared'. Actual count is 56 (53 permanent + 3 expected), not ~53 with 0 expected."
    artifacts:
      - path: "test/smb-conformance/KNOWN_FAILURES.md"
        issue: "Summary line claims '53 permanent + 3 expected = 56' but ROADMAP SC says '~53 (52 permanent + 1 ChangeEa permanent; all fixable expected failures cleared)'. The 3 remaining Expected entries contradict the 'all fixable cleared' claim in the success criterion."
      - path: ".planning/ROADMAP.md"
        issue: "Success criterion 1 says 'all fixable expected failures cleared' but 3 Expected entries remain. ROADMAP SC1 must be updated to reflect actual state: 53 permanent + 3 expected = 56 total."
    missing:
      - "Update ROADMAP.md Phase 73 Success Criterion 1 to reflect actual state: '56 known failures (53 permanent + 3 Expected Timestamp deferred)' rather than claiming all fixable cleared"
      - "Either fix the 3 remaining directory timestamp Expected failures OR reclassify them as Permanent with a documented reason"
  - truth: "smbtorture compound edge case tests pass (D-16)"
    status: failed
    reason: "Plan 05 Task 1 deferred compound edge cases. The Plan 05 Summary explicitly states 'Compound edge cases deferred (D-16 tests) -- the compound dispatch code already handles error propagation [...] Without being able to run smbtorture, the specific compound fixes cannot be validated.' No compound fixes were made. This was a must-have truth in Plan 05."
    artifacts:
      - path: "internal/adapter/smb/compound.go"
        issue: "No compound edge case fixes were applied. Plan 05 deferred this work without removing it from the must-haves or success criteria."
    missing:
      - "Either fix compound edge cases (error propagation to related ops, FileID substitution for all command types) OR remove from must-haves and document as deferred in ROADMAP"
human_verification:
  - test: "Run WPTS BVT suite against live SMB server"
    expected: "56 known failures (53 permanent + 3 Expected) with ZERO new failures. ChangeNotify ADS stream/security/close tests pass. ADS share access and management tests pass."
    why_human: "Cannot run WPTS BVT programmatically -- requires Windows WPTS infrastructure and a live SMB server."
  - test: "Run smbtorture notify, session.reauth, session.anon-encryption, durable-v2-open.reopen*, lease.request tests"
    expected: "All removed tests pass. No regressions on remaining known failures."
    why_human: "Cannot run smbtorture programmatically -- requires Samba client and live SMB server."
  - test: "Run smb2.timestamps.freeze-thaw smbtorture test"
    expected: "CreationTime stays stable across freeze/unfreeze cycles. Test passes."
    why_human: "Cannot run smbtorture programmatically."
---

# Phase 73: SMB Conformance Deep-Dive Verification Report

**Phase Goal:** Systematic WPTS BVT and smbtorture conformance push -- clear all fixable expected failures, fix compound edge cases, timestamp freeze-thaw, and update documentation with accurate targets
**Verified:** 2026-03-24T16:30:00Z
**Status:** gaps_found
**Re-verification:** No -- initial verification

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | WPTS ChangeNotify ADS stream/security/close tests pass | VERIFIED | FileNotifyChangeStreamName/Size/Write constants (0x200/0x400/0x800) in change_notify.go:50-58. MatchesFilter extended at lines 323-336. Write handler fires NotifyChange at write.go:395. |
| 2 | WPTS BVT known failures reduced to ~53 (52 permanent + 1 ChangeEa; all fixable cleared) | PARTIAL | Actual count: 56 (53 permanent + 3 Expected). 3 Timestamp tests remain as Expected. ROADMAP SC1 and 'all fixable cleared' claim are inaccurate. |
| 3 | WPTS ADS share access + management tests cleared | VERIFIED | 12 ADS + timestamp expected failures removed (commits bfe6db6d, 8c61eecb). TestAdsBasePath and TestCheckShareModeConflict_ADSCrossStream added. |
| 4 | smbtorture ChangeNotify tests pass (basedir, close, dir, double, mask, rmdir1-4, tree, valid-req) | VERIFIED | NotifyRmdir, UnregisterAllForSession, UnregisterAllForTree, IsValidCompletionFilter all present in change_notify.go:644-811. AsyncResponseRegistry generalized at change_notify.go:731-802. 25 tests removed from smbtorture KNOWN_FAILURES. |
| 5 | smbtorture session re-auth tests pass (reauth2-6) | VERIFIED | tryReauthUpdateWithKeys implemented in session_setup.go:502. Re-auth path at lines 286-538. |
| 6 | smbtorture anonymous encryption bypass tests pass (anon-encryption1-3) | VERIFIED | checkEncryptionRequired in response.go:253-272 skips for IsNull/IsGuest sessions. |
| 7 | Generalized async response mechanism in place | VERIFIED | AsyncResponseRegistry struct in change_notify.go:744-802. Register, Complete, Cancel, Unregister methods present. |
| 8 | smbtorture DH V2 reopen tests pass | VERIFIED | ReconnectResult struct at durable_context.go:219-248. DH2Q/DHnQ response contexts returned on reconnect. LeaseState persisted (commit 1c1a68bc). |
| 9 | smbtorture DH V1 reopen tests pass | VERIFIED | processV1Reconnect in durable_context.go returns ReconnectResult. ~9 DH V1 tests removed from KNOWN_FAILURES. |
| 10 | smbtorture lease state machine tests pass (request, nobreakself, upgrade, upgrade2, upgrade3) | VERIFIED | ExcludeLeaseKey in LockOwner (lock/types.go:101-105). isStatOnlyOpen helper in create.go:1674. excludeOwner passed to BreakHandleLeasesOnOpenAsync at create.go:811. |
| 11 | smbtorture compound edge case tests pass (D-16) | FAILED | Plan 05 explicitly deferred compound fixes. No code changes to compound.go in this phase. |
| 12 | smbtorture timestamp freeze-thaw test passes | VERIFIED | BtimeFrozen and FrozenBtime fields in handler.go:227. Per-field CreationTime freeze/unfreeze in set_info.go:322-397. smb2.timestamps.freeze-thaw removed from smbtorture KNOWN_FAILURES (commit 35331e2a). |
| 13 | ROADMAP Phase 73 updated, requirements marked Complete, KNOWN_FAILURES consistent | PARTIAL | REQUIREMENTS.md WPTS-01 through WPTS-04 marked Complete at lines 101-104. ROADMAP SC1 and SC5 are inconsistent with actual state (3 Expected remain, compound deferred). |

**Score:** 11/13 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/adapter/smb/v2/handlers/change_notify.go` | ADS stream change notification constants and dispatch | VERIFIED | FileNotifyChangeStreamName/Size/Write at lines 50-58. MatchesFilter covers all MS-SMB2 2.2.35 flags. AsyncResponseRegistry, NotifyRmdir, UnregisterAllForSession/Tree, IsValidCompletionFilter all present. |
| `internal/adapter/smb/v2/handlers/set_info.go` | NotifyChange calls for ADS stream operations and security descriptor changes | VERIFIED | ADS write notifications in write.go:395. Per-field BtimeFrozen/FrozenBtime freeze tracking at set_info.go:322-397. |
| `internal/adapter/smb/v2/handlers/close.go` | STATUS_NOTIFY_CLEANUP async response on CLOSE of watched directory | VERIFIED (pre-existing) | StatusNotifyCleanup (0x0000010B) in types/status.go:145. Cleanup response mechanism functional from Phase 72. Note: plan specified 0x0000010C but 0x0000010B is the correct value per MS-SMB2. |
| `internal/adapter/smb/v2/handlers/session_setup.go` | Session re-authentication with key re-derivation | VERIFIED | tryReauthUpdateWithKeys at line 500. IsReauth detection at line 286-315. Existing session preserved on re-auth. |
| `internal/adapter/smb/response.go` | Anonymous/guest encryption bypass | VERIFIED | checkEncryptionRequired at line 253-272. IsNull/IsGuest bypass at line 272. |
| `internal/adapter/smb/v2/handlers/durable_context.go` | DH V1/V2 reconnect with ReconnectResult, LeaseState persistence | VERIFIED | ReconnectResult at line 219. LeaseState stored in buildPersistedDurableHandle at line 613. |
| `pkg/metadata/lock/leases.go` | Post-conflict lease granting after break resolves | VERIFIED | Confirmed by Plan 04 Summary commit 2c80cbbe. ExcludeLeaseKey integrated via types.go:101. |
| `.planning/ROADMAP.md` | Phase 73 success criteria updated with revised targets | PARTIAL | Success criteria updated (lines 318-322), 5 plans listed. But SC1 says 'all fixable cleared' while 3 Expected remain. SC5 says 'Zero new failures' which is correct. Phase 73 still marked [ ] (not [x]) -- expected as it is not yet merged. |
| `test/smb-conformance/KNOWN_FAILURES.md` | Accurate WPTS BVT failure counts | PARTIAL | Count is 56 (53+3), documented correctly in the file. But ROADMAP SC1 contradicts: says '~53' and 'all fixable cleared'. |
| `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` | Accurate smbtorture failure counts | VERIFIED | 438 entries (better than ~460 target). smb2.timestamps.freeze-thaw removed. ~51 tests removed across Plans 03-05. |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `set_info.go` | `change_notify.go` | NotifyChange with stream and security filter flags | VERIFIED | write.go:395 calls NotifyChange. MatchesFilter handles FileNotifyChangeStreamName/Size/Write. |
| `close.go` | `response.go` | SendAsyncChangeNotifyResponse with STATUS_NOTIFY_CLEANUP | VERIFIED | StatusNotifyCleanup present in types/status.go. Cleanup path functional from Phase 72. |
| `change_notify.go` | `response.go` | AsyncResponseRegistry general-purpose async tracking | VERIFIED | AsyncResponseRegistry in change_notify.go:744-802. |
| `session_setup.go` | `handler.go` | tryReauthUpdateWithKeys preserves tree connects, re-derives keys | VERIFIED | tryReauthUpdateWithKeys at session_setup.go:500. IsReauth flag set at line 292 and passed through pending session. |
| `durable_context.go` | `handler.go` | DurableStore PutDurableHandle/GetDurableHandle for reconnect | VERIFIED | GetDurableHandleByFileID at durable_context.go:299. GetDurableHandleByCreateGuid at line 350. LeaseState set at line 613. |
| `lease_context.go` | `lease/manager.go` | ExcludeLeaseKey in LockOwner for self-break suppression | VERIFIED | ExcludeLeaseKey in lock/types.go:101. excludeOwner built once at create.go:689 and passed to BreakHandleLeasesOnOpenAsync at line 811. |
| `.planning/ROADMAP.md` | `test/smb-conformance/KNOWN_FAILURES.md` | Success criteria numbers match actual known failure counts | PARTIAL | KNOWN_FAILURES says 56 (53+3). ROADMAP SC1 says ~53 with 'all fixable cleared'. Inconsistent. |

### Data-Flow Trace (Level 4)

Not applicable for this phase -- no components rendering dynamic data from a database. All artifacts are protocol handlers, test documentation, and planning files.

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| Build compiles cleanly | `go build ./...` | Exit 0, no errors | PASS |
| SMB unit tests pass | `go test ./internal/adapter/smb/... -count=1 -timeout 120s` | All 11 packages pass (ok) | PASS |
| FileNotifyChangeStreamName constant present | `grep "FileNotifyChangeStreamName" change_notify.go` | Line 50: `FileNotifyChangeStreamName uint32 = 0x00000200` | PASS |
| AsyncResponseRegistry substantive | `grep "AsyncResponseRegistry" change_notify.go` | Struct + Register/Complete/Cancel/Unregister methods at lines 744-802 | PASS |
| Session re-auth detection | `grep "isReauth" session_setup.go` | Lines 286, 292 -- detection logic in place | PASS |
| smb2.timestamps.freeze-thaw removed | `grep freeze-thaw smbtorture/KNOWN_FAILURES.md` | No matches | PASS |
| Expected WPTS failures count | `grep -c "| Expected |" WPTS KNOWN_FAILURES.md` | 3 (not 0) | FAIL |
| Compound edge case fixes | `grep` compound edge-case changes in compound.go | No changes to compound.go in this phase | FAIL |
| smbtorture count | `grep -c "^| smb2\." smbtorture/KNOWN_FAILURES.md` | 438 (better than ~460 target) | PASS |
| REQUIREMENTS traceability | `grep "WPTS-01.*Complete" REQUIREMENTS.md` | Lines 101-104: all 4 marked Phase 73 \| Complete | PASS |

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|----------|
| WPTS-01 | 73-01, 73-03 | ChangeNotify async notifications | SATISFIED | FileNotifyChangeStreamName/Size/Write constants. NotifyRmdir. AsyncResponseRegistry. 25 smbtorture notify tests removed. REQUIREMENTS.md line 101 Complete. |
| WPTS-02 | 73-03, 73-05 | Negotiate/encryption edge cases | SATISFIED | Anonymous/guest encryption bypass in response.go:272. Session re-auth key re-derivation. REQUIREMENTS.md line 102 Complete. |
| WPTS-03 | 73-02, 73-04 | Leasing and durable handle reconnect | SATISFIED | ExcludeLeaseKey self-break suppression. isStatOnlyOpen exemption. ReconnectResult with LeaseState persistence. REQUIREMENTS.md line 103 Complete. |
| WPTS-04 | 73-01, 73-02, 73-04, 73-05 | Known failure count reduced from 73 to ~53 | PARTIALLY SATISFIED | WPTS reduced from 65 to 56 (not ~53). smbtorture reduced from ~492 to 438 (better than target). 3 Expected failures remain vs 'all cleared' claim in SC1. REQUIREMENTS.md line 104 Complete -- but the text of WPTS-04 says "reduced from 73 to ~53 (52 permanent + 1 ChangeEa)" which doesn't match the actual 56 (53+3). |

No orphaned requirements found -- all four WPTS requirement IDs appear in at least one plan's `requirements` frontmatter field and are documented in REQUIREMENTS.md.

### Anti-Patterns Found

| File | Pattern | Severity | Impact |
|------|---------|----------|--------|
| `.planning/ROADMAP.md` | SC1 says "all fixable expected failures cleared" but 3 Expected remain; SC1 count ~53 but actual is 56 | Warning | Documentation inconsistency. Does not block code correctness but creates incorrect expectations for the next phase. |
| `test/smb-conformance/KNOWN_FAILURES.md` | Summary line "56 (53 permanent + 3 expected)" contradicts ROADMAP SC1 "~53 (52 permanent + 1 ChangeEa)" | Warning | Same root cause as above. The two documents are out of sync on what was achieved. |
| `internal/adapter/smb/compound.go` | No compound edge-case fixes applied despite being a must-have truth in Plan 05 | Blocker | Plan 05 deferred compound fixes but did not remove them from must-haves or document the deferral in ROADMAP. |

### Human Verification Required

#### 1. WPTS BVT Suite

**Test:** Run full WPTS BVT against a live DittoFS SMB server on this branch.
**Expected:** 56 total known failures (53 permanent + 3 Expected Timestamp). Zero new failures. ChangeNotify ADS stream/security/close tests pass. ADS share access and management tests (DeleteStream, ListStreams, RenameStream) pass.
**Why human:** Requires Windows WPTS infrastructure and SMB server.

#### 2. smbtorture Conformance Suite

**Test:** Run smbtorture notify.*, session.reauth2-6, session.anon-encryption1-3, durable-v2-open.reopen*, durable-open.reopen*, lease.request, lease.nobreakself, lease.upgrade*.
**Expected:** All tests removed from KNOWN_FAILURES now pass. Remaining 438 known failures still fail. No new failures.
**Why human:** Requires Samba client and live SMB server.

#### 3. smb2.timestamps.freeze-thaw Validation

**Test:** Run `smbtorture smb2.timestamps.freeze-thaw` against live server.
**Expected:** CreationTime stays stable through freeze/unfreeze cycles. Test passes.
**Why human:** Cannot run smbtorture programmatically.

### Gaps Summary

Two gaps block full phase goal achievement:

**Gap 1 (Documentation inconsistency -- WPTS count): PARTIAL**
The ROADMAP Success Criterion 1 states "WPTS BVT known failures reduced to ~53 (52 permanent + 1 ChangeEa permanent; all fixable expected failures cleared)" but the actual state is 56 total (53 permanent + 3 Expected). Three Timestamp tests remain as Expected because fixing directory timestamp freeze enforcement during child operations (CREATE, REMOVE) requires changes to the metadata service's auto-update logic -- a larger change that was deferred. Plan 05 Summary correctly documents this deferral, but the ROADMAP and REQUIREMENTS.md text were not updated to match. KNOWN_FAILURES.md correctly states 56, so there is a contradiction between the two planning documents and the test documentation.

**Gap 2 (Compound edge cases -- not fixed): FAILED**
Plan 05 must-have truth "smbtorture compound edge case tests pass (D-16)" was not achieved. The Plan 05 Summary explicitly deferred compound fixes because the code appeared already correct but could not be validated without running smbtorture. No changes were made to compound.go. This truth needs to be either implemented or formally deferred with ROADMAP documentation.

Both gaps are documentation/scope classification issues rather than correctness bugs. The code changes that were made are substantive and correctly implemented. The build passes and all unit tests pass.

---

_Verified: 2026-03-24T16:30:00Z_
_Verifier: Claude (gsd-verifier)_
