---
allowed-tools: Bash(graphify *), Bash(rg *), Read, Grep, Glob
description: DittoFS adapter-convergence program memory (update after each wave)
---

# DittoFS adapter convergence — program memory

Last updated: 2026-09-09 ~24:00 (Wave 2 closed, Wave 3 prepared)

## Program state (as of 2026-09-09)

| Wave | Scope | State |
| --- | --- | --- |
| 0 — Coordination | peer takeover, worktree cleanup | ✅ done (32-row #2340 re-derivation left) |
| 1 — Ownership class | #2393–#2396, #2413, #2414 | ✅ landed 2026-09-08 (7 PRs) |
| 2 — `sm.mu` + pynfs | #2398 + singles | ✅ **landed 2026-09-09** — PRs #2494-#2498, issues #2471/#2482/#2487/#2464/#2490 closed |
| 3 — Shared-layer reuse | errmap, identity, lifecycle | ❌ not started — **NEXT**, lane plan ready |
| 4 — SMB triage | ~155 untriaged findings | ❌ not started |
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

## Wave 3 lane plan (fan out immediately — 7 lanes, disjoint files)

0. GUARD — TestErrorMapCoverage enum walk + ErrConflict row + count 26 (LAND FIRST)
1. NFSv4 content-path wiring (read.go:143, write.go:215, read_plus.go:125 → MapToNFS4)
2. lock-errmap dead columns (delete MapLockToNFS3/4; fix ErrLockLimitExceeded drift)
3. identity (delete getUserIdentity; use ResolvedIdentity; AUTH_SYS exempt)
4. adapter lifecycle (listenerReady leak + EMFILE/ENFILE accept backoff, base.go)
5. async credit grants (response.go:1207/1278 bypass Session.credits)
6. citations (CLAUDE.md:105 + create.go:1874 → real symbols)

## Standing hazards (carry into every prompt)

- Audits pinned at `40884ad4f` predate #2356 — re-verify each premise on develop
- Postgres tests are `//go:build integration` — green `go test ./...` proves nothing about postgres
- Check `pg_stat_activity` before any dropdb (#2345)
- Merge via gh CLI (no github MCP): `PUT pulls/{n}/merge -f sha=<re-read head>`; sign every commit; rebase, never merge
- Assign every PR to marmos91; THREE reviews before open (correctness, simplification, adversarial); babysit Copilot + CI after open
- Known-failures tally 178 = SMB 86 + NFS 92; row regex `^\| *[A-Z]+[0-9]+[a-z]? *\|`
- Pre-existing stashes (refactor/nfs-dead-code, refactor/smb-1055-v2, replay-rebase) are unrelated — do not touch
- `graphify update .` after the last merge of the day, from the main checkout
