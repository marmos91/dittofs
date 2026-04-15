# Phase 73: SMB Conformance Deep-Dive - Context

**Gathered:** 2026-03-24
**Status:** Ready for planning

<domain>
## Phase Boundary

Fix remaining WPTS BVT and smbtorture known failures by addressing ChangeNotify delivery gaps, ADS conformance issues, negotiate edge cases, leasing/durable handle state machine bugs, timestamp conformance, session re-auth, and async response plumbing. Clear all 13 fixable WPTS BVT failures and reduce smbtorture known failures by ~30. Update roadmap success criteria to realistic targets.

</domain>

<decisions>
## Implementation Decisions

### Prioritization
- **D-01:** WPTS BVT first, smbtorture second. Clear all 13 fixable WPTS failures before spending effort on smbtorture.
- **D-02:** Revised WPTS target: ~52 known failures (all 13 fixable cleared, 52 permanent remain — VHD, Witness Protocol, Storage QoS, DFS, NTFS Object IDs, etc. are genuinely unfixable).
- **D-03:** smbtorture target: ~460 known failures (down ~30 from 492). Focus on DH2, lease state, compounds, notify, session re-auth, anonymous encryption.
- **D-04:** Fix the stale ChangeNotify watcher signing bug FIRST (from debug note: add NotifyRegistry cleanup to closeFilesWithFilter). This unblocks reliable CI for all subsequent work.
- **D-05:** Update ROADMAP.md Phase 73 success criteria to match revised targets (WPTS ~52, smbtorture ~460).

### ChangeNotify Completion
- **D-06:** Wire ADS stream operations to fire ChangeNotify notifications (FILE_NOTIFY_CHANGE_STREAM_NAME, FILE_NOTIFY_CHANGE_STREAM_SIZE, FILE_NOTIFY_CHANGE_STREAM_WRITE). Clears 3 WPTS tests.
- **D-07:** Mark ChangeEa (Extended Attributes) as permanent — no EA support, genuinely unfixable without implementing EAs.
- **D-08:** Debug ChangeSecurity and ServerReceiveSmb2Close locally in Docker (run WPTS BVT locally, add debug logging to async delivery paths, iterate until fixed). Do NOT rely on CI for debugging — test locally first.
- **D-09:** Fix stale watcher bug first — it may resolve ChangeSecurity/ServerReceiveSmb2Close as a side effect.

### ADS Conformance
- **D-10:** Fix all 6 ADS-related WPTS expected failures (share access enforcement + ChangeNotify stream notifications). Natural synergy since we're already in the ADS code for ChangeNotify.

### Durable Handles & Leases
- **D-11:** DH2 first approach: fix DH2 reopen (5 tests), DH2 purge (5 tests), DH2 preservation (4 tests) = ~14 tests. If all go well, extend to V1 durable reopen (4 tests) and durable reopen with lease (2+ tests).
- **D-12:** Lease state handling (6 tests) and lease upgrade (3 tests) fixed together — they share the lease state machine.
- **D-13:** Lease V2 epoch tracking (3 tests) included if time permits after higher-priority items.
- **D-14:** Test-driven approach: read smbtorture test source code (Samba) to understand exact wire-level expectations for each failing test. Cross-reference with MS-SMB2 spec sections.

### Scope Boundary
- **D-15:** Multi-client oplock break coordination (27 tests): investigate Docker test infrastructure feasibility. If 2-client setup is easy, include; otherwise defer to Phase 74 (multi-channel naturally requires multi-connection testing).
- **D-16:** Compound edge cases (3 tests): in scope — newly reachable, likely simple parameter validation fixes.
- **D-17:** Notify on rmdir (4 tests): in scope — natural extension of ChangeNotify work.
- **D-18:** Anonymous session encryption (3 tests): in scope — partially implemented, likely small fix.
- **D-19:** Session re-auth (5 tests): in scope — single-connection re-authentication, distinct from Phase 74's session binding. Phase 74 inherits the improvement.
- **D-20:** Byte-range lock + oplock interaction (3 tests) and rename share mode enforcement (2 tests): include if effort is low, defer if rabbit hole.
- **D-21:** Interim/async responses (3 tests): generalize the async response path beyond ChangeNotify-only. Build a general-purpose STATUS_PENDING interim response mechanism usable by ChangeNotify, lock waits, and future async ops.

### Claude's Discretion
- Architecture of generalized async response mechanism (callback registry, async ID management, interim response formatting)
- Order of attack within each category (which specific tests to fix first)
- Whether to extend from DH2 to full DH sweep based on effort observed
- Whether byte-range lock + oplock and rename share mode tests are quick wins or rabbit holes
- Multi-client oplock investigation depth — quick feasibility check, not a deep infrastructure build

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### SMB Protocol Spec
- MS-SMB2 §2.2.35 — SMB2 CHANGE_NOTIFY Request/Response format
- MS-SMB2 §3.3.5.19 — Server processing of CHANGE_NOTIFY
- MS-SMB2 §3.3.5.9.12 — Durable handle V2 reconnect (DH2C processing)
- MS-SMB2 §3.3.5.9.7 — Durable handle V1 reconnect (DHnC processing)
- MS-SMB2 §3.3.5.22 — Lease break acknowledgment processing
- MS-SMB2 §3.3.1.4 — Lease state machine, breaking state transitions
- MS-SMB2 §3.3.5.2.11 — Lease V2 epoch handling
- MS-SMB2 §3.3.5.5 — Session re-authentication (SESSION_SETUP on existing session)
- MS-SMB2 §3.3.4.4 — Sending interim (STATUS_PENDING) responses
- MS-FSA §2.1.5.1.2 — Share mode enforcement (for ADS cross-stream enforcement)

### Reference Implementations
- https://github.com/samba-team/samba — smbtorture test source code for each failing test (read to understand exact expectations)
- Samba source: `source4/torture/smb2/durable_v2_open.c` — DH2 reopen/purge/preservation test logic
- Samba source: `source4/torture/smb2/lease.c` — Lease state/upgrade/epoch test logic
- Samba source: `source4/torture/smb2/notify.c` — ChangeNotify test logic
- Samba source: `source4/torture/smb2/session.c` — Session re-auth test logic
- Samba source: `source4/torture/smb2/compound.c` — Compound edge case test logic

### Existing DittoFS Code
- `internal/adapter/smb/v2/handlers/change_notify.go` — ChangeNotify handler (Phase 72)
- `internal/adapter/smb/v2/handlers/change_notify_test.go` — ChangeNotify unit tests
- `internal/adapter/smb/v2/handlers/handler.go` — closeFilesWithFilter (stale watcher bug location)
- `internal/adapter/smb/v2/handlers/durable_context.go` — DH V1/V2 create context handling
- `internal/adapter/smb/v2/handlers/durable_scavenger.go` — Disconnected handle scavenger
- `internal/adapter/smb/v2/handlers/lease_context.go` — Lease V1/V2 context processing
- `internal/adapter/smb/v2/handlers/set_info.go` — Timestamp freeze/unfreeze, ADS operations
- `internal/adapter/smb/v2/handlers/create.go` — File/ADS creation, durable handle wiring
- `internal/adapter/smb/v2/handlers/close.go` — Close handler, ChangeNotify cleanup
- `internal/adapter/smb/response.go` — Async response delivery, interim response path
- `internal/adapter/smb/compound.go` — Compound request processing

### Debug Context
- `.planning/debug/smb-signing-mismatch.md` — Stale watcher signing bug hypothesis and evidence
- `.planning/debug/phase72-ci-failures.md` — ChangeSecurity/ServerReceiveSmb2Close/freeze-thaw CI failure analysis

### Test Infrastructure
- `test/smb-conformance/KNOWN_FAILURES.md` — WPTS BVT known failures (65 total, 13 fixable)
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` — smbtorture known failures (492 total)
- `test/smb-conformance/` — Docker-based WPTS test infrastructure (run locally for debugging)

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `change_notify.go`: Full async ChangeNotify infrastructure from Phase 72 — NotifyRegistry, completion filter dispatch, async callback wiring, STATUS_PENDING interim responses
- `durable_context.go`: DH V1/V2 create context parsing and reconnect logic (needs fixes, not new implementation)
- `durable_scavenger.go`: Handle scavenger with configurable timeout (exists, may need state machine fixes)
- `lease_context.go`: Lease V1/V2 context processing with break coordination via metadata service
- ADS architecture: Streams stored as directory children with `file:stream:$DATA` naming. Share mode enforcement spans base file + all streams via path-based matching.

### Established Patterns
- ChangeNotify async: handler registers watcher → returns STATUS_PENDING interim → callback fires on change → sends async response with AsyncId
- Durable handles: DH2Q context on CREATE → handle preserved on disconnect → DH2C context on reconnect validates CreateGuid + session key
- Lease breaks: metadata service fires break → handler sends break notification → client acknowledges → state transition
- Debug approach: local Docker WPTS/smbtorture runs for iteration, CI for final validation

### Integration Points
- `closeFilesWithFilter` in handler.go — needs NotifyRegistry cleanup for stale watchers
- `response.go` async response path — needs generalization beyond ChangeNotify-only
- Lease state machine in metadata service — breaking/upgrade state transitions need fixes
- KNOWN_FAILURES.md files — update as tests are fixed (remove from known, verify CI passes)

</code_context>

<specifics>
## Specific Ideas

- Fix stale ChangeNotify watcher signing bug as very first task — unblocks everything else
- Debug ChangeSecurity/ServerReceiveSmb2Close locally in Docker, not in CI
- Read smbtorture test source for each failing DH/lease test before attempting fixes
- Generalize async response mechanism to support ChangeNotify, lock waits, and future async ops
- Multi-client oplock: quick feasibility check of running 2 smbtorture clients in Docker — don't invest heavily if it requires infrastructure changes

</specifics>

<deferred>
## Deferred Ideas

- Multi-client oplock break coordination (27 tests) — defer to Phase 74 if Docker multi-client infra isn't trivial
- Replay detection and replay+pending tests (~20+ tests) — requires separate replay cache infrastructure
- Async compound processing (~10 tests) — requires fundamental compound dispatch refactor
- Kernel oplock break coordination (2 tests) — requires kernel-level oplock integration

None — discussion stayed within phase scope

</deferred>

---

*Phase: 73-smb-conformance-deep-dive*
*Context gathered: 2026-03-24*
