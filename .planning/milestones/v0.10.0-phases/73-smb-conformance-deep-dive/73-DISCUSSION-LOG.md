# Phase 73: SMB Conformance Deep-Dive - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-03-24
**Phase:** 73-smb-conformance-deep-dive
**Areas discussed:** Prioritization strategy, ChangeNotify completion, Durable handle & lease fixes, Scope boundary

---

## Prioritization Strategy

| Option | Description | Selected |
|--------|-------------|----------|
| WPTS first, smbtorture second | Fix all 13 fixable WPTS failures first, then spend remaining effort on smbtorture | ✓ |
| Highest ROI across both suites | Attack easiest failures regardless of suite | |
| Category-focused deep dives | Pick 2-3 categories and fix thoroughly across both suites | |

**User's choice:** WPTS first, smbtorture second
**Notes:** WPTS is the Microsoft conformance standard — most valuable for credibility.

| Option | Description | Selected |
|--------|-------------|----------|
| ~460 smbtorture (30 fewer) | Focus on clearly fixable categories | ✓ |
| ~440 (50 fewer) | Adds multi-client oplock breaks (27 tests) | |
| Best effort, no hard target | Fix what falls naturally from WPTS work | |

**User's choice:** ~460 (30 fewer)

| Option | Description | Selected |
|--------|-------------|----------|
| Fix signing bug first | Stale watcher hypothesis well-formed, fix unblocks CI | ✓ |
| Only if it blocks other fixes | Investigate only if replay tests interfere | |
| Defer to separate fix | Track separately as Phase 72 regression | |

**User's choice:** Yes, fix it first

| Option | Description | Selected |
|--------|-------------|----------|
| Revise WPTS target to ~52 | 52 permanent failures genuinely unfixable | ✓ |
| Keep 35, find more to fix | Investigate whether some permanent are fixable | |
| Drop to ~50, stretch goal | Fix all 13 + investigate 2-3 permanent | |

**User's choice:** Revise to ~52

---

## ChangeNotify Completion

| Option | Description | Selected |
|--------|-------------|----------|
| Wire ADS notifications | ADS ops fire NotifyChange with appropriate completion filters | ✓ |
| Mark as permanent | ADS notifications are niche | |
| You decide | Claude's discretion | |

**User's choice:** Wire ADS ops to fire notifications (3 tests)

| Option | Description | Selected |
|--------|-------------|----------|
| Mark ChangeEa permanent | No EA support, genuinely unfixable | ✓ |
| Stub EA support | Minimal EA get/set for 1 test | |

**User's choice:** Mark permanent

| Option | Description | Selected |
|--------|-------------|----------|
| Debug locally in Docker | Fix stale watcher → run WPTS locally → debug with logging → push to CI | ✓ |
| Fix stale watcher only, reassess rest | One shot via stale watcher fix | |

**User's choice:** Debug locally in Docker
**Notes:** User asked "Can't we test this with docker locally first?" — confirmed WPTS runs in Docker locally.

---

## Durable Handle & Lease Fixes

| Option | Description | Selected |
|--------|-------------|----------|
| Fix DH2 reopen + lease state | DH2 reopen (5) + lease state (6) = ~11 tests | |
| Full DH + lease sweep | All DH + all lease subcategories = ~32 tests | |
| Minimal — only what WPTS needs | smbtorture DH/lease fixes are bonus only | |

**User's choice:** "Attach DH2 first. If you manage to fix all of them, try to extend to full DH"
**Notes:** Progressive approach — DH2 sweep first (reopen + purge + preservation = ~14 tests), extend to V1 if successful.

| Option | Description | Selected |
|--------|-------------|----------|
| Include epoch if time permits | Attempt after higher-priority items, defer if rabbit hole | ✓ |
| Defer to future phase | Complex, low-ROI for 3 tests | |
| You decide | Claude's discretion | |

**User's choice:** Include if time permits

| Option | Description | Selected |
|--------|-------------|----------|
| Fix all 6 ADS WPTS failures | Natural synergy with ChangeNotify ADS work | ✓ |
| Only 3 ChangeNotify ADS tests | Keep ADS scope to notifications only | |
| You decide based on effort | Include if straightforward | |

**User's choice:** Yes, fix all 6 ADS failures

| Option | Description | Selected |
|--------|-------------|----------|
| Read smbtorture source | Read Samba test code, cross-reference with MS-SMB2 spec | ✓ |
| Spec-first, tests second | Implement to spec, validate against smbtorture | |
| You decide | Claude's discretion | |

**User's choice:** Read smbtorture source (test-driven approach)

| Option | Description | Selected |
|--------|-------------|----------|
| Include DH2 purge in sweep | Purge tests share root causes with reopen | ✓ |
| Defer if reopen is complex | Only attempt if DH2 reopen goes smoothly | |

**User's choice:** Include in DH2 sweep

| Option | Description | Selected |
|--------|-------------|----------|
| Include lease upgrade with state fixes | Share the lease state machine | ✓ |
| Defer | Lower priority | |

**User's choice:** Include with lease state fixes

---

## Scope Boundary

| Option | Description | Selected |
|--------|-------------|----------|
| Defer multi-client to Phase 74 | Multi-channel naturally requires multi-connection testing | |
| Investigate feasibility | Check if Docker setup supports 2 clients | ✓ |
| Include, build the infra | Build multi-client infra, 27 tests payoff | |

**User's choice:** Investigate feasibility

**Smaller categories selected (all 4):**
- Compound edge cases (3 tests) — in scope
- Notify on rmdir (4 tests) — in scope
- Anonymous session encryption (3 tests) — in scope
- Session re-auth (5 tests) — in scope

| Option | Description | Selected |
|--------|-------------|----------|
| Fix re-auth here, Phase 74 inherits | Single-connection re-auth, distinct from session binding | ✓ |
| Defer all session work to Phase 74 | Keep session changes in one phase | |

**User's choice:** Fix here, Phase 74 inherits

| Option | Description | Selected |
|--------|-------------|----------|
| Include lock+oplock/rename if low effort | Attempt after higher-priority items | ✓ |
| Defer | Niche edge cases | |
| You decide | Claude's discretion | |

**User's choice:** Include if effort is low

| Option | Description | Selected |
|--------|-------------|----------|
| Generalize async response path | Build general-purpose STATUS_PENDING mechanism | ✓ |
| ChangeNotify-only async | Keep async specific to ChangeNotify | |
| You decide | Claude's discretion | |

**User's choice:** Generalize async response path

| Option | Description | Selected |
|--------|-------------|----------|
| Update roadmap targets | Keep roadmap realistic (WPTS ~52, smbtorture ~460) | ✓ |
| Leave roadmap as-is | Aspirational targets | |

**User's choice:** Yes, update roadmap

---

## Claude's Discretion

- Architecture of generalized async response mechanism
- Order of attack within each failure category
- Whether to extend from DH2 to full DH sweep based on observed effort
- Whether byte-range lock + oplock and rename share mode tests are quick wins
- Multi-client oplock investigation depth

## Deferred Ideas

- Multi-client oplock breaks (27 tests) — defer to Phase 74 if Docker infra isn't trivial
- Replay detection/pending tests (~20+ tests) — separate replay cache infrastructure needed
- Async compound processing (~10 tests) — fundamental compound dispatch refactor needed
- Kernel oplock break coordination (2 tests) — kernel-level integration needed
