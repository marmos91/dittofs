# Phase 72: WPTS Conformance Push - Context

**Gathered:** 2026-04-07
**Revised:** 2026-04-07 (post-#323 supersession)
**Status:** Ready for planning (slim 2-plan phase)

<supersession_note>
## Supersession history

This CONTEXT.md was originally written assuming 5 fixable WPTS BVT failures remained on develop. Between writing the context and starting execution, **PR #323 (`914291cb`, "fix(smb): close last ChangeNotify failure + harden empty-buffer encoders")** landed on develop and shipped a fix for `BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close` — Plan 02's entire original scope.

PR #323's fix was different from (and better than) the researcher's branch-condition theory in 72-RESEARCH.md:
- Real bug 1: wire ordering — `close.go` was firing the notify cleanup BEFORE the CLOSE response reached the dispatcher, violating MS-SMB2 3.3.4.1. Fix: introduced `SMBHandlerContext.PostSend` hook so cleanup runs strictly after the CLOSE response is on the wire.
- Real bug 2: empty variable-section padding — `ChangeNotifyResponse.Encode` (and 4 other encoders) emitted only the fixed portion when buffer was empty, but `StructureSize=N` per MS-SMB2 means "(N-1) fixed + ≥1 byte variable section". WPTS silently drops short responses. Same class of bug fixed in Read, Ioctl, QueryDir, QueryInfo encoders.

PR #323 also removed `BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close` from `KNOWN_FAILURES.md`. Current state on develop: 4 fixable expected failures (3 timestamp deferred + 1 lease flake).

**Revised Phase 72 scope is now 1 fix + reconciliation:** the lease break ack-wait fix + the verification sweep that closes out the BVT fixable list. Phase 72 plan structure was reduced from 4 plans to 2:
- Plan 01 (was 03): Lease break ack-wait fix in `BreakParentHandleLeasesOnCreate` + `BreakParentReadLeasesOnModify`
- Plan 02 (was 04): Verification sweep + KNOWN_FAILURES.md reconciliation + ROADMAP mark-done + SUMMARY/PR

The original Plans 01 (baselines) and 02 (ChangeNotify fix) were deleted as superseded by current develop state and PR #323. The relevant sections of 72-RESEARCH.md (Plan 02 ChangeNotify analysis) are now historical context only — the lease analysis (Plan 01) remains valid and accurate against current develop.

**The decisions D-01 through D-12 below were authored against the pre-#323 reality and are preserved verbatim for traceability. Where they reference "5 fixable failures" or "2 SMB-only fixable failures", read as "1 fixable failure" post-#323. Where they reference Plan 02's ChangeNotify fix, read as "shipped by PR #323, not by Phase 72". The lease break decisions (D-03 through D-06) are unaffected.**
</supersession_note>

<domain>
## Phase Boundary (revised post-#323)

Close out the last 1 SMB-only fixable WPTS BVT failure and reconcile the conformance baseline. The original Phase 72 scope (ChangeNotify, negotiate, leasing, ≤45 known) was rolled into Phase 73 and exceeded; PR #323 then closed the last ChangeNotify failure. This phase is the trailing cleanup of the lease break race + the bookkeeping sweep.

**In scope (revised):**
1. `BVT_DirectoryLeasing_LeaseBreakOnMultiClients` — directory lease break race (root cause fix, not quarantine)
2. Verification sweep: run WPTS BVT in Linux CI on the post-fix branch, reconcile the count discrepancy in `test/smb-conformance/KNOWN_FAILURES.md` (header still says 58 from Phase 73 era, footer says 42 but table actually has 43), refresh changelog
3. ROADMAP mark-done with descope note crediting both PR #323 and this branch

**Inherited from PR #323 (already on develop, NOT this branch's work):**
- `BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close` — fixed via wire-ordering + variable-section padding
- 4 other encoders hardened against empty-buffer bug class (Read, Ioctl, QueryDir, QueryInfo)

**Out of scope (deferred):**
- 3 directory timestamp tests (`Algorithm_NotingFileModified_Dir_LastAccessTime`, `…Timestamp_MinusOne_Dir_ChangeTime`, `…Timestamp_MinusTwo_Dir_LastWriteTime`) — require metadata-service-level directory timestamp propagation that affects all protocols. Belongs in a dedicated cross-protocol timestamp phase.
- All 38+ permanent failures (VHD, SWN, SQoS, DFS, NamedPipe) — genuinely unimplemented features, not Phase 72 work.
- smbtorture failure reduction — separate test suite, not Phase 72's gate.
</domain>

<decisions>
## Implementation Decisions

### Scope and Targets
- **D-01:** Phase 72 narrowed to 2 SMB-only fixable failures + verification sweep. The original "≤45 known" target is already met (currently 43); this phase pushes to **41 known fixable failures** (5 → 3) and reconciles the count discrepancy.
- **D-02:** 3 directory timestamp tests are explicitly **deferred**, not failed. They require metadata-service work and will be picked up in a future cross-protocol timestamp phase. Mark as `Deferred` (new status, distinct from `Expected` and `Permanent`) in `KNOWN_FAILURES.md`, or keep as `Expected` with a clear "blocked on metadata-service work" note — researcher to choose based on existing conventions.

### Lease Break Flake (`BVT_DirectoryLeasing_LeaseBreakOnMultiClients`)
- **D-03:** **Root-cause fix**, not quarantine and not test-side retry. The user's preference is to make break delivery deterministic before the WPTS test polls.
- **D-04:** Investigation starts at `internal/adapter/smb/v2/handlers/create.go:1003-1020` (parent directory Handle/Read lease break dispatch on CREATE) and `internal/adapter/smb/lease/manager.go:411` (multi-client break path). The race is most likely between async break dispatch and the test's subsequent CREATE/QUERY observing the broken state. Researcher must identify whether the race is in (a) break message wire send, (b) client ack handling, or (c) server-side handle state visibility.
- **D-05:** Acceptable fix shapes (researcher picks based on the actual race):
  - Synchronous break dispatch on the CREATE→break path (block until break sent or timeout)
  - Wait-for-ack with bounded timeout before completing the triggering CREATE
  - State barrier so subsequent observations see the post-break state regardless of break delivery latency
- **D-06:** Adding `time.Sleep` or unconditional waits is **not acceptable**. The fix must be deterministic with respect to a real protocol event.

### ChangeNotify CLOSE Cleanup (`BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close`)
- **D-07:** The CLOSE→cleanup code path already exists at `internal/adapter/smb/v2/handlers/close.go:358-383` (calls `NotifyRegistry.Unregister` then dispatches `STATUS_NOTIFY_CLEANUP` via `AsyncCallback`). The bug is in **wire format / dispatch path**, not in detection. Per `KNOWN_FAILURES.md` line 128: "CLOSE notify cleanup response format needs debugging".
- **D-08:** Researcher must capture the actual WPTS expectation by running the failing test against the current server with verbose logging, comparing wire bytes against MS-SMB2 3.3.5.16.1. Most likely culprits to investigate:
  - Whether `STATUS_NOTIFY_CLEANUP` should be delivered as an async response (using the AsyncId from the prior interim response) or as a sync response on the original MessageID
  - Whether the `OutputBufferLength` field must be 0 vs absent vs a properly framed empty buffer
  - Whether signing/encryption headers on the cleanup response match the session state at CLOSE time (not at CHANGE_NOTIFY arrival time)
- **D-09:** The fix must preserve the existing `AsyncCallback` plumbing — do not refactor `NotifyRegistry` to remove async dispatch. Other ChangeNotify tests passing depend on the current async path.

### Verification Sweep
- **D-10:** Phase exit requires:
  1. Both target tests passing in WPTS BVT
  2. Zero new failures introduced (193+ passing tests maintained per memory; researcher to confirm current pass count by running the suite)
  3. `test/smb-conformance/KNOWN_FAILURES.md` updated with: (a) reconciled total count (header and table agree), (b) changelog entry for Phase 72, (c) updated category breakdown, (d) the 3 deferred timestamp tests clearly labeled
- **D-10a:** **CI vs local-host discrepancy is real and must be accounted for.** WPTS BVT results differ between Linux CI and the developer's macOS host running Docker under QEMU emulation. The QEMU layer introduces timing and (likely) syscall-translation differences that produce different pass/fail sets. The reconciled `KNOWN_FAILURES.md` baseline is **Linux CI**, not local. When investigating either target failure, reproduce against Linux CI semantics — local Mac+QEMU repro is a hint, not a verdict. If a test passes locally but fails in CI (or vice versa), CI wins. Researcher should document any Mac+QEMU-only failures separately so they don't pollute the canonical baseline.
- **D-11:** No changes to ROADMAP.md success criteria. Phase 72's roadmap entry already over-specifies the original ChangeNotify/negotiate/leasing work — that's all done. The PR description and SUMMARY.md should clarify that Phase 72 was descoped because Phase 73 absorbed the original scope.
- **D-12:** Mark Phase 72 as `[x]` complete in ROADMAP.md only after the verification sweep passes.
</decisions>

<specifics>
## User Preferences and Constraints

- **No quarantining flakes.** The user explicitly chose root-cause fix over the cheaper quarantine option for the lease break race. Apply this preference if researcher discovers other test-infrastructure-style suppressions are needed — push back and ask first.
- **Honest counts.** The KNOWN_FAILURES.md header/table discrepancy bothered the user enough to make the sweep mandatory. When updating the file, both numbers must match and both must reflect the actual test run.
- **Don't retroactively rewrite history.** Phase 73 already did the bulk of WPTS work; Phase 72's CONTEXT/PLAN/SUMMARY should acknowledge that, not pretend Phase 72 did it from scratch.
- **Worktree note:** This phase is being planned in `worktree-issue-324-smb-idmap` (an unrelated SMB identity-mapping branch). Planning artifacts are committed here, but the actual fix work should happen on a Phase 72 branch off `develop`, not on the idmap branch.
</specifics>

<canonical_refs>
## Canonical References

**MS-SMB2 Spec Sections:**
- [MS-SMB2] 3.3.5.16.1 — CHANGE_NOTIFY processing including CLOSE→STATUS_NOTIFY_CLEANUP requirement
- [MS-SMB2] 3.3.5.15 — CHANGE_NOTIFY request validation
- [MS-SMB2] 2.2.35 — CHANGE_NOTIFY Response wire format
- [MS-SMB2] 3.3.4.6 — Server lease break processing (multi-client fan-out semantics)

**Source files (current implementation):**
- `internal/adapter/smb/v2/handlers/change_notify.go` — CHANGE_NOTIFY handler, NotifyRegistry, completion filter constants, NotifyRmdir cleanup precedent (lines 644-680)
- `internal/adapter/smb/v2/handlers/close.go:358-383` — CLOSE→pending notify cleanup path (current bug location)
- `internal/adapter/smb/v2/handlers/create.go:1003-1020` — parent directory lease break dispatch on CREATE (likely race location)
- `internal/adapter/smb/lease/manager.go:411` — multi-client break helper (referenced in code comment as the path that enables `BVT_DirectoryLeasing_LeaseBreakOnMultiClients`)
- `internal/adapter/smb/v2/handlers/result.go` — Async response framing (AsyncId, AsyncCallback signature)

**Test infrastructure:**
- `test/smb-conformance/KNOWN_FAILURES.md` — source of truth for expected failures and pass counts
- `test/smb-conformance/README.md` — how to run WPTS BVT and parse results
- `test/smb-conformance/baseline-results.md` — current baseline run

**Prior phase context (must read before planning):**
- `.planning/phases/73-smb-conformance-deep-dive/73-CONTEXT.md` — D-01 through D-04 explain the Phase 73 prioritization that left these 2 items
- `.planning/phases/73-smb-conformance-deep-dive/73-01-PLAN.md` and `73-01-SUMMARY.md` — ChangeNotify completion work (ADS stream, ChangeSecurity, ServerReceiveSmb2Close *attempted*)
- `.planning/phases/73-smb-conformance-deep-dive/73-04-PLAN.md` and `73-04-SUMMARY.md` — DH/lease state machine fixes
</canonical_refs>

<deferred>
## Deferred Ideas (not Phase 72)

- **Directory timestamp propagation across protocols** (3 WPTS failures + similar NFS gaps). Needs metadata-service-level work touching `pkg/metadata/file_modify.go` and all store implementations. Should be its own phase under a "cross-protocol POSIX timestamp conformance" theme.
- **smbtorture failure reduction.** Per memory, smbtorture is at ~438 known failures after Phase 73. Phase 72's gate is WPTS BVT only. A separate phase can target smbtorture once WPTS is locked at ≤43.
- **NamedPipe FSA tests** (2 permanent failures). Would require running WPTS with SSH access to the SUT, which is unavailable in Docker CI. Out of scope unless CI infrastructure changes.
- **Phase numbering housekeeping.** Phase 72's roadmap entry now over-promises relative to its actual scope. A future roadmap-edit pass should rewrite Phase 72's success criteria to match what was actually delivered (this phase) and what Phase 73 absorbed.
</deferred>

<open_questions>
## Open Questions for Researcher

1. **Current WPTS BVT pass count.** Memory says 193 passing as of 2026-03-19, but the latest KNOWN_FAILURES.md changelog is Phase 73 (2026-03-24) and shows the file totals 43 known failures while the header says 58. Run the suite and report: pass / known / new / skipped, plus reconcile the discrepancy. This is the baseline Phase 72 must preserve.
2. **ServerReceiveSmb2Close test expectation.** Capture the WPTS test's actual wire-level expectation for the CLOSE→cleanup response. Is it expecting the cleanup on the original sync MessageID, on the AsyncId from a prior interim, or as a fresh frame? The MS-SMB2 spec is permissive here; WPTS may be stricter.
3. **Directory lease break race surface.** Does the race appear in the parent-Handle break path, the multi-client fan-out, or both? `internal/adapter/smb/lease/manager.go:411` is the entry point. Trace what happens between the triggering CREATE and the test's next observation, identify where determinism is lost.
4. **Naming for deferred status.** Does `KNOWN_FAILURES.md` already have a precedent for distinguishing "blocked on cross-protocol work" from generic `Expected`? If yes, use that. If no, propose adding a `Deferred` status or use `Expected` with a structured reason field.
5. **Linux CI vs Mac+QEMU result delta.** Run the WPTS BVT suite in **both** environments (Linux CI runner and the developer's macOS host running Docker via QEMU) and produce a diff: which tests pass in one but fail in the other? This delta is the QEMU-emulation noise floor and must be characterized before either target failure is debugged. Without it, a "fix" verified locally may regress in CI or vice versa. Document the diff in the phase research output so the planner knows which environment is authoritative for each test.
</open_questions>
