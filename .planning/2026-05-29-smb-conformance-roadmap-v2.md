# SMB Conformance Roadmap v2 — v1.0 Tag Gate

**Created:** 2026-05-29
**Supersedes:** `.planning/2026-04-28-smb-conformance-roadmap.md` (archived to `planning-archive` orphan branch in PR #666)
**Goal:** close every remaining SMB-related issue in milestone `v1.0.0` before tag. Kerberos (70 tests) deferred to `v1.0+kerberos`.
**Umbrella tracker:** [#673](https://github.com/marmos91/dittofs/issues/673)

---

## Current state (2026-05-29)

```
test/smb-conformance/smbtorture/KNOWN_FAILURES.md   ~238 entries
test/smb-conformance/KNOWN_FAILURES.md (WPTS)        ~58 entries
test/smb-conformance/smbtorture/KNOWN_FAILURES_KERBEROS.md  ~70 entries
                                                    ----
Total                                                ~366 KF rows
```

Develop tip `a438a54b`. v1-roadmap entirely closed — all 32 issues (#429–#436, #469–#491, #361) shipped between 2026-04-28 and 2026-05-29. New batch of 13 open issues (#738–#751) bucketed below.

Recent ship velocity (last 7 days): PR #659, #660, #661, #662, #665, #677, #682–#685, #699, #701, #705, #708–#711, #713, #723, #725, #727, #730, #731, #735–#737. Workflow proven: parallel worktrees + iterate-until-green + admin-merge + Copilot+CI gate.

---

## Open v1.0 SMB issues (13)

| # | Issue | Tests | Subsystem | Conflict-group | Difficulty |
|---|---|---|---|---|---|
| 738 | Durable Handles V1 — reconnect + lease + DOC residuals | 14 | DH state + CREATE | **DH/CREATE** | M |
| 739 | Durable Handles V2 — reopen + disconnected-handle + persistent | 32 | DH state + CREATE + persistence | **DH/CREATE** | H |
| 740 | Oplocks — LEVEL_II coercion + stat-only + response mapping | 10 | lock/lease | **lease-family** | M |
| 741 | Create Contexts — impersonation/blob/mkdir/gentest residuals | 7 | CREATE handler | **DH/CREATE** | M |
| 743 | Directory Leases — break/ack ordering + DELETE_PENDING + requeued CREATE | 6 | lock/lease | **lease-family** | M |
| 744 | Byte-Range Lock — replay residuals | 3 | lock + replay | lock | S |
| 745 | Multi-Channel residuals — leases.test1 + leases.test3 | 2 | lease fan-out | **lease-family** | M |
| 746 | Session reconnect + re-auth + anon-encryption residuals | 14 | session | **session** | M |
| 747 | Session binding — cross-channel sign/encrypt negotiation matrix | 30 | session/sign | **session** | H |
| 748 | Session signing/encryption algorithm-variant + anon-signing | 9 | session/crypto | **session** | M |
| 749 | Replay protection — channel seq + DH V2 + lease/oplock pending matrix | 48 | session + DH | **session+DH** | XL |
| 750 | Misc protocol gaps (DOS attrs, setinfo, sharemode, max-allowed, tcon, maxfid, EA-ACL, ts-res, notify-mask) | 9 | distributed | **none** | S/M |
| 751 | Flake — `smb2.lease.statopen4` intermittent | 1 | lease | **lease-family** | flake |
| **Total** |  | **185 tests + 1 flake** |  |  |  |

Plus #674 (rename `internal/adapter/smb/v2/` — tech-debt, mechanical, v1.0 wave1 audit).

Plus umbrella #673 (closes when subs close).

WPTS (~58) + Kerberos (~70): deferred KF cleanup. See "WPTS" / "Kerberos" sections below.

---

## Conflict groups

File-overlap forces sequencing within a group. Cross-group is fully parallel.

| Group | Issues | Files | Sequencing |
|---|---|---|---|
| **session** | 746, 747, 748, 749 (partial) | `internal/adapter/smb/session/*`, `internal/adapter/smb/v2/{negotiate,session_setup,logoff}.go`, signing/encryption layer | 1 worktree at a time per file scope; 746 then 748 then 747 then 749 |
| **DH/CREATE** | 738, 739, 741, 749 (partial) | `internal/adapter/smb/v2/handlers/create*.go`, DH state store, persistence backend | 741 → 738 → 739 → 749/DH-slice |
| **lease-family** | 740, 743, 745, 751 | `pkg/metadata/lock/*`, `internal/adapter/smb/lease/*` | 740 → 743 → 745 → 751 |
| **lock** | 744 | `pkg/metadata/lock/byterange*` | independent |
| **none (misc)** | 750 | distributed (dosmode, setinfo, sharemode, maxfid, tcon, ea, ts, notify-mask) | split into 2–3 PRs internally |
| **rename** | 674 | adapter dir rename | mechanical; do after waves clear |

---

## Wave plan

### Wave 1 — 6 parallel worktrees (conflict-isolated)

Target: small/medium issues across all groups, one per conflict-group head, plus 2 splits of #750.

| Slot | Issue | Tests | Why now |
|---|---|---|---|
| W1-a | **#744** BR lock replay | 3 | only `lock` issue, isolated |
| W1-b | **#740** oplocks LEVEL_II | 10 | head of `lease-family` |
| W1-c | **#741** create contexts | 7 | head of `DH/CREATE` |
| W1-d | **#746** session reconn/re-auth | 14 | head of `session` |
| W1-e | **#750-A** misc subset A (dosmode + setinfo + timestamp_resolution) | 3 | distributed, no overlap |
| W1-f | **#750-B** misc subset B (maximum_allowed + ea.acl_xattr + sharemode.bug14375) | 3 | distributed, no overlap |

**Wave 1 output target:** 40 test flips, 6 PRs merged, closes 5 issues + half of #750.

### Wave 2 — 6 parallel worktrees

| Slot | Issue | Tests |
|---|---|---|
| W2-a | **#738** DH V1 residuals | 14 |
| W2-b | **#743** dir leases | 6 |
| W2-c | **#747** session binding | 30 |
| W2-d | **#748** sign/enc variants | 9 |
| W2-e | **#750-C** misc subset C (tcon + maxfid + notify.mask-change) | 3 |
| W2-f | **#674** adapter dir rename (mechanical) | — |

**Wave 2 output target:** 62 test flips, 5 PRs, closes 5 issues + #750 fully.

### Wave 3 — heavyweights + stabilization

| Slot | Issue | Tests |
|---|---|---|
| W3-a | **#739** DH V2 | 32 |
| W3-b | **#749** Replay protection | 48 |
| W3-c | **#745** Multi-channel residuals | 2 |
| W3-d | **#751** Flake fix | 1 |

W3-a / W3-b will be multi-round (split internally like lease cluster #429 round 1–7). Likely 3–4 sub-PRs each.

**Wave 3 output target:** 83 test flips, ~8 PRs, closes 4 issues.

### Wave 4 — WPTS + KF cleanup + tag

- Triage 58 WPTS entries; flip what's fixable; promote rest to Permanently Unimplementable appendix with documented per-test reason.
- Verify `smbtorture/KNOWN_FAILURES.md` has zero unjustified rows above the appendix.
- Kerberos 70 tests: confirm `v1.0+kerberos` milestone deferral; add CHANGELOG note.
- Close #673 umbrella.
- Tag `v1.0.0`.

---

## Per-issue execution recipe

Every agent prompt MUST include:

```
WORKFLOW (mandatory, no shortcuts):
1. Branch from develop. Worktree isolated.
2. Read smbtorture test source FIRST (Samba: source4/torture/smb2/<name>.c).
   Write down exact assertions in agent scratch before any code.
3. If protocol-divergence suspected: pcap diff against dperson/samba:latest on port 11445
   (see CLAUDE.md "Debugging protocol interop"). Identify byte-level divergence.
4. Code fix only. Do NOT touch KNOWN_FAILURES.md in this commit.
5. Run pre-PR gate:
   - gofmt -s -w .
   - go vet ./...
   - golangci-lint run --timeout=5m
   - subagent: code-simplifier:code-simplifier
   - subagent: feature-dev:code-reviewer
   Re-run gates after each subagent edit.
6. Commit signed (-S). Conventional Commit format. No AI mentions, no Co-Authored-By AI.
7. Push. Open PR --assignee marmos91. Body links the GH issue with "Closes #N".
8. Wait 15 min for Copilot + CI smbtorture.
9. Iterate on findings until green + Copilot clean.
10. If CI confirms tests flip: second commit removes the KNOWN_FAILURES.md rows.
    Push again, wait CI.
11. Admin-merge (squash). Verify "Verified" badge. Confirm issue auto-closed.
12. If auto-close didn't fire (e.g., merge target != GH default), close manually.
```

Defaults to bake in: branch from develop (not main), sign all commits, assign PRs to marmos91, never mention Claude/AI/Co-Authored-By, two-commit pattern for KF walkback.

---

## Parallelism limits

Same constraints as v1, validated empirically:

- **CI smbtorture ≈ 25–30 min/run.** With 6 parallel PRs × 2–3 cycles each → ~5h/day cluster occupancy. OK.
- **Review fatigue ≈ 3–4 PRs/day** to author quality. Wave 1 (6 PRs) likely needs 2 review days.
- **File overlap:** never run 2 agents in same conflict-group simultaneously. The wave table above enforces this.
- **Determinate FlakeHub outages:** historical — fall back to `nix --extra-experimental-features` + admin-merge when blocked (PR #682–#685 pattern, filed #694).

---

## Acceptance for v1.0 tag

- [ ] `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` — zero rows above the Permanently Unimplementable appendix.
- [ ] `test/smb-conformance/KNOWN_FAILURES.md` (WPTS) — zero rows above appendix.
- [ ] Kerberos: either fixed or `v1.0+kerberos` milestone created + CHANGELOG deferral note.
- [ ] CI green on full smbtorture + WPTS BVT.
- [ ] #673 closed.
- [ ] All 13 sub-issues (#738–#751) closed.
- [ ] #674 (adapter rename) closed.
- [ ] `v1.0.0` tag pushed.

---

## Time estimate (with disciplined parallel waves)

| Wave | Issues | Tests | Wall time |
|---|---|---|---|
| 1 | 5 + ½ of #750 | 40 | 5–7 days |
| 2 | 4 + rest of #750 + #674 | 62 | 7–10 days |
| 3 | 4 (incl. 2 multi-round) | 83 | 10–14 days |
| 4 | WPTS triage + tag | 58 → ≤10 | 3–5 days |
| **TOTAL** |  | **~185 + 58 WPTS** | **~25–36 days** |

Kerberos deferred. Assumes 6-worktree concurrency sustained, CI healthy, no major regressions.

---

## Where parallelism breaks down

- **Replay (#749) + DH V2 (#739)** share session-state machinery — sequence in wave 3, not parallel.
- **WPTS triage** is sequential read-through; no benefit from agents.
- **Adapter rename (#674)** is one big diff that touches every SMB file. Schedule between waves, freeze SMB work for that PR.
