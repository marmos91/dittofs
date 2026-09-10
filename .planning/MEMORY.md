---
allowed-tools: Bash(graphify *), Bash(rg *), Read, Grep, Glob
description: DittoFS adapter-convergence program memory (update after each wave)
---

# DittoFS adapter convergence — program memory

Last updated: 2026-09-10 (Wave 3.5 LANDED, Wave 4 SMB triage NEXT)

## Program state (as of 2026-09-10)

| Wave | Scope | State |
| --- | --- | --- |
| 0 — Coordination | peer takeover, worktree cleanup | ✅ done (32-row #2340 re-derivation left) |
| 1 — Ownership class | #2393–#2396, #2413, #2414 | ✅ landed 2026-09-08 (7 PRs) |
| 2 — `sm.mu` + pynfs | #2398 + singles | ✅ **landed 2026-09-09** — PRs #2494-#2498, issues #2471/#2482/#2487/#2464/#2490 closed |
| 3 — Shared-layer reuse | errmap, identity, lifecycle | ✅ **landed 2026-09-10** — 6 PRs (#2499-#2504) squash-merged, review-green, no open issues |
| 3.5 — Error-universe consolidation | sentinel normalization + StatusFor extraction | ✅ **landed 2026-09-10** — #2505 @ `4f418ff83` (StatusFor/StatusForErr in all three types packages, common/ reduced to errclassify+normalize+payload helpers), #2506 pynfs walk-out @ `75b45e987` (38→15 rows), #2507 follow-up @ `b2927215d` |
| 4 — SMB triage | ~157 untriaged findings (7 HIGH / 62 MED / 88 LOW), audit predates ~12 landed SMB fixes — re-derivation pass required | ❌ not started — umbrella + area tranches, re-derive against develop |
| 5 — God objects | manager.go 3,985 LOC | ❌ blocked behind 1-4 |
| 7 — Perf lens | bufpool, measurement plan | ❌ not started |

## Key artifacts

- Master plan: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md`
- Wave 2 closure + premise verification: `.planning/2026-09-09-wave2-closure.md`
- Wave 3 lane plan: `.planning/2026-09-09-wave3-plan.md`
- Diagnostics verdicts: `.planning/2026-09-09-diag-2329-2464.md`

## Wave 2 — what landed (all 2026-09-09, squash to 7e5cd2e40)

| PR | Issue | Content |
| --- | --- | --- |
| #2494 | #2471 | CREATE_SESSION channel-size floors (256/256), unknown-flag INVAL (suite), NOSPC session cap |
| #2496 | #2482 | client-index fold: one clientsByID map + MinorVersion + v40/v41ClientLocked filters |
| #2495 | #2487 | reclaim-persist repair: v4.0 flag set, both persists OFF sm.mu, chain dedup/adoption |
| #2497 | #2464 | sync SettingsWatcher.RefreshNFSSettings after settings writes + harness lease guard |
| #2498 | #2490 | LockExisting special-stateid → NFS4ERR_BAD_STATEID (both forms), before grace check |

## Wave 3 — what landed (all 2026-09-10, squash to 7d7033b39)

| PR | Content | Squash commit |
| --- | --- | --- |
| #2499 | guard: enum walk + ErrConflict row + delete MapLockToNFS3/4 | `6ffc4262b` |
| #2500 | 5 content-path sites → MapContentToNFS4 | `0a0e96c34` |
| #2501 | getUserIdentity → uidGIDFromSessionUser rename | `cc4a5864d` |
| #2502 | Stop-path listenerReady close + accept backoff | `7d7033b39` |
| #2503 | async grant sites → GrantCredits + SequenceWindow.Grant | `c69818453` |
| #2504 | stale CheckExportAccess citation fixes | `0390b3fb8` |

Deferred disclosures: awaitListener false-'Adapter started' narrow race (#2502 body); ResolvedIdentity direct consumption (#2501 body). No lane maps to an open issue — no Closes #N.

## Wave 3.5 — consolidation plan (NEXT, one PR)

Per roadmap Step 2.5: (1) payload-choke-point sentinel normalization — ReadFromBlockStore/WriteToBlockStore/CommitBlockStore wrap block sentinels as real *merrs.StoreError (engine.ErrStoreClosed → ErrStaleHandle, everything else → ErrIOError), delete content_errmap.go, convert the 10 MapContentTo* sites plus #2500's five v4 sites to uniform MapToNFS3/4/SMB; (2) StatusFor extraction — nfs/types + nfs/v4/types get StatusFor(merrs.ErrorCode) uint32, smb/types gets StatusFor + StatusForLock (MS-SMB2 3.3.5.14 split), delete errmap.go + lock_errmap.go, one enum-walk test per package. Zero behaviour change. Supersedes the discarded fix/errmap-single-file two-fold.

## Wave 3 lane plan (superseded by the table above)

- COMBINED GUARD (fix/errmap-coverage-dead-columns) — enum walk + ErrConflict row + exoticCodes guard + delete MapLockToNFS3/4 (L0+L2 merged: shared errmap_test.go)
- v4 content wiring (fix/v4-content-errmap-wiring) — read/read_plus/write/commit/deallocate → **MapContentToNFS4** (NOT MapToNFS4: fmt.Errorf wrap defeats StoreError lookup, would regress ErrRemoteUnavailable IO→SERVERFAULT)
- identity (fix/smb-resolved-identity) — ResolvedIdentity direct; handler.go:2038 second production caller; AUTH_SYS exempt
- lifecycle (fix/adapter-lifecycle-accept) — listenerReady Stop-leak (sync.Once-guarded close; base.go:225 unconditional normal-path close would double-close) + accept backoff
- credits+citations (fix/async-credit-grants, fix/export-acl-citations) — 2 async grant sites (1232/1303) through GrantCredits + SessionManager nil-check; CLAUDE.md:103 (not 105) + create.go:1874 → buildV4AuthContext/ResolveSharePermission

Full corrections: `.planning/2026-09-09-wave3-plan.md` (plan-validation review, verdict CONCERNS, all baked in).

## Standing hazards (carry into every prompt)

- Audits pinned at `40884ad4f` predate #2356 — re-verify each premise on develop
- Postgres tests are `//go:build integration` — green `go test ./...` proves nothing about postgres
- Check `pg_stat_activity` before any dropdb (#2345)
- Merge via gh CLI (no github MCP): `PUT pulls/{n}/merge -f sha=<re-read head>`; sign every commit; rebase, never merge
- Assign every PR to marmos91; THREE reviews before open (correctness, simplification, adversarial); babysit Copilot + CI after open
- Known-failures tally 178 = SMB 86 + NFS 92; row regex `^\| *[A-Z]+[0-9]+[a-z]? *\|`
- Pre-existing stashes (refactor/nfs-dead-code, refactor/smb-1055-v2, replay-rebase) are unrelated — do not touch
- `graphify update .` after the last merge of the day, from the main checkout
