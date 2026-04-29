# SMB Conformance Roadmap — Tier 1+2+3

**Created:** 2026-04-28
**Goal:** Close ~272 of the 359 known smbtorture failures (Tier 1+2+3). Tier 4 deferred.
**Estimated wall time with parallel Claude Code agents + active driver:** 7.5–10 weeks.

---

## Current state (2026-04-28)

After this session's round-7 lease cluster + Tier-1 partial-fix sweep:

```
Total active suite:    567 tests
  Passing:             ~210  (+2 from round 7 partials)
  Known failures:      ~357
  Skipped (no infra):   31  (out of scope)
```

Recently shipped (this session):
- `ed411326` — `fix(smb): suppress duplicate break on already-breaking lease (refs #436)` — partial; PR #465
- `0b432809` — `fix(smb): NTTIME sentinel handling — freeze/thaw (refs #434)` — partial; PR #468
- (in flight) PR #467 — `feat(smb): byte-range lock async parking + deadlock graph (refs #430)` — architectural groundwork
- PR #466 (#433 rename) **closed** — agent's first attempt regressed `smb2.lease.{break,v2_complex1}` + `smb2.rename.msword`; needs fresh approach

---

## Tracked GitHub issues (newly created)

23 cluster issues opened to track every unbucketed group. Issue numbers `#469`–`#491`.

### Tier 1 — blocks real Windows/Mac SMB workloads (~107 tests)
**Priority order. Most impactful for end-user adoption.**

| # | Issue | Tests | Status |
|---|---|---|---|
| #436 | Spurious lease break (`multichannel.leases.test3`) | 1 | partial fix shipped (#465); test3 still failing — **finish root cause** |
| #478 | File IDs / handle scheme | 2 | open |
| #433 | Rename — share-mode enforcement | 4 | **fresh attempt needed**; previous PR #466 closed for regressions |
| #476 | File Attributes limited support | 4 | open |
| #477 | Directory Operations — advanced queries | 4 | open |
| #475 | Delete-on-Close — advanced semantics | 5 | open |
| #474 | Sessions remaining (re-auth) | 5 | open |
| #434 | Timestamps — delayed-write + freeze/thaw | 5 | THAW shipped (#468); delayed-write subsystem **TODO** |
| #473 | Change Notify remaining | 8 | open |
| #472 | Compound Requests remaining | 11 | open |
| #431 | Durable Handles V1 — reconnect + lease coord | 12 | open |
| #471 | Alternate Data Streams (extends #146) | 14 | open |
| #470 | Directory Leases | 17 | open |
| #469 | ACLs / Security Descriptors | 17 | open — **needs ADR first** |
| #430 | Byte-Range Locks — async + contention | 19 | architecture in PR #467 (in flight); per-test debugging next |

**Tier 1 subtotal: ~107 tests across 15 issues.**

### Tier 2 — performance & coordination polish (~52 tests)

| # | Issue | Tests | Status |
|---|---|---|---|
| #481 | Query/Set Info — advanced scenarios | 7 | open |
| #480 | Create Contexts — advanced semantics | 10 | open |
| #479 | Oplocks — multi-client coordination | 35 | open — round-based, mirrors lease cluster #429 |

**Tier 2 subtotal: ~52 tests across 3 issues.**

### Tier 3 — multi-channel deep work (~99 tests)

| # | Issue | Tests | Status / Dependency |
|---|---|---|---|
| #361 | Multi-Channel — Phase 2 (cross-channel coordination) | 7 | foundational; Phase 1 already shipped (PR #404) |
| #483 | Session Binding deep | 38 | depends on #361 Phase 2 |
| #482 | Replay Protection | 54 | depends on #361 Phase 2 + partial #432 (Tier 4) |

**Tier 3 subtotal: ~99 tests across 3 issues.**

### Tier 4 — deferred, low priority (~115 tests)

Tracked for completeness but explicitly out of current scope:

| # | Issue | Tests |
|---|---|---|
| #432 | Durable Handles V2 | 32 |
| #484 | IOCTL/FSCTL coverage | 24 |
| #485 | Session Encryption variants | 4 |
| #486 | Session Signing variants | 3 |
| #487 | Connection / Tree Connect advanced | 2 |
| #488 | Read/Write advanced semantics | 2 |
| #489 | Previous Versions / Time Warp | 2 |
| #490 | Name Mangling | 2 |
| #491 | Anonymous Session | 2 |
| #435 | Charset edge cases | 1 |

**Tier 4 subtotal: ~74 tracked + ~41 misc 1-test entries = ~115. Skip for now.**

---

## Workflow discipline (mandatory for every issue)

Lessons from this session: agents over-claim wins, removing `KNOWN_FAILURES.md` entries before CI confirms. **The new workflow:**

```
1. Read smbtorture test source FIRST
   (Samba: source4/torture/smb2/<name>.c)
   Write down exact assertions before any code.

2. Pcap diff against reference
   docker run -p 11445:445 dperson/samba:latest ...
   Capture pcap on both DittoFS and Samba.
   Diff with tshark — identify divergence point.

3. Fix code change ONLY
   Do NOT touch KNOWN_FAILURES.md in this commit.

4. Push, wait for CI smbtorture (~30 min).

5. If CI confirms test flips:
   Second commit removes KNOWN_FAILURES.md entry.
   Push again.

6. Address Copilot review.
   Final CI cycle.

7. Squash-merge.
```

Per-issue cycles typically: 2–4 cycles × 90–120 min each.

---

## Phased execution plan

### Phase A — Tier 1 quick wins (4–6 days)
9 small issues (1–8 tests each). Run **5 parallel worktree agents**, drain queue twice.

**Wave 1 (5 parallel):** #436, #478, #476, #477, #475
**Wave 2 (4 parallel):** #474, #434 (delayed-write subsystem), #473, #433 (fresh attempt)

Bottleneck: PR review, not agent throughput.

**Expected output: ~38 test flips, 9 PRs merged.**

### Phase B — Tier 1 medium clusters (2.5–3 weeks)
Two parallel tracks:

**Track A (sequential, single driver): #469 ACLs**
- Week 1: ADR for SID-mapping layer + SD persistence in metadata store
- Week 2–3: 4-phase rollout (read SDs → write SDs → ACL evaluation → inheritance)

**Track B (parallel agents in rotation):**
- #430 finish (round-based, 4–6 days, 4–5 rounds)
- #470 Directory Leases (3–5 days, extends lease cluster patterns)
- #471 ADS (3–5 days, extends partial impl in metadata directory-children)
- #431 Durable V1 (5–7 days, reconnect + lease coord)
- #472 Compound (3–4 days, dispatch refactor)

**Expected output: ~90 test flips.**

### Phase C — Tier 2 polish (1.5–2 weeks)
- #479 Oplocks 35: round-based like lease cluster #429 (1.5–2 weeks)
- #480 Create Contexts 10 + #481 Q/SI 7: parallel inside #479's rounds (3–4 days each)

**Expected output: ~52 test flips.**

### Phase D — Tier 3 multi-channel (3–4 weeks)
Mostly sequential due to dependency chain:
1. #361 Phase 2: 4–6 days (cross-channel break fan-out, FSCTL_QUERY_NETWORK_INTERFACE_INFO, wide-channel coord)
2. #483 Session Binding deep: 1–1.5 weeks
3. #482 Replay Protection: 1.5–2 weeks (risk: may need partial #432 for replay-on-durable-v2 subset; if so, scope-cut to MC-only replay)

**Expected output: ~92 test flips.**

---

## Total estimate

| Phase | Issues | Tests | Wall time |
|---|---|---|---|
| A — Tier 1 quick | 9 | 38 | 4–6 days |
| B — Tier 1 medium | 6 | 90 | 2.5–3 wk |
| C — Tier 2 | 3 | 52 | 1.5–2 wk |
| D — Tier 3 | 3 | 92 | 3–4 wk |
| **TOTAL** | **21** | **272** | **7.5–10 wk** |

After completion: smbtorture passing ~482 / 567 active = **~85% conformance** (up from current 37%).

---

## Where parallelism stops paying

- **Cross-cluster file conflicts:** lease + directory-lease + oplock all touch `pkg/metadata/lock/`. Sequence them; don't run 3 agents on overlapping subsystems simultaneously.
- **CI capacity:** smbtorture ≈ 25–30 min/run. With 6 parallel PRs × 2–3 cycles each, CI consumes ~5h/day. Self-hosted runners or matrix-parallelism would shave another 20%.
- **Review fatigue:** sustained rate ≈ 3–4 PRs/day fully reviewed. Beyond that, quality drops.

## What stays human-authored (not agent-delegable)

| Work | Why |
|---|---|
| ACL ADR (SID mapping, SD persistence) | Cross-cutting metadata-store design |
| Replay/Durable-V2 dependency analysis | Hard scope-cut decisions |
| Round-N planning for big clusters (#430, #470, #479) | Which 3 tests per round, which to defer |
| First read of every smbtorture test source for new issues | Catches "spec says X but test asserts Y" upfront |

Agents handle implementation in each round; humans handle round design + ADRs.

---

## Three accelerators

1. **Reference Samba pod in CI** — auto-diff pcaps on test divergence. Cuts root-cause time from hours to minutes.
2. **Round-based PR convention** — each PR ships 2–5 test flips (lease cluster shipped 30+ tests across 7 rounds this way).
3. **Parallelize #469 (ACLs) and #470 (Dir Leases) from day 1 of Phase B** — zero file overlap, biggest Tier 1 surface. Saves ~2 weeks.

---

## Starting position for next session

After #467 merges (in flight on this session's branch):

1. **Clean state**: develop has all 7 rounds of lease cluster + #436 partial + #434 partial + #430 architecture
2. **Phase A wave 1 ready to spawn**: #436, #478, #476, #477, #475 — 5 parallel worktree agents with the new "don't touch KNOWN_FAILURES" prompt
3. **Track A (Phase B) can start in parallel**: read `pkg/metadata/acl/` and `source3/smbd/posix_acls.c`, draft ADR for #469

Suggested kickoff command:
```
Start Phase A wave 1: spawn parallel worktree agents for #436, #478,
#476, #477, #475 with the disciplined workflow (read test source first,
fix code only, do NOT touch KNOWN_FAILURES.md until CI confirms flips).
```
