# Wave 4 lane prompts — SMB triage re-derivation (2026-09-10)

Source audit: `.planning/audits/2026-09-07-smb-adapter/report.md` (157 findings, pinned at
`40884ad4f`, predating ~12 landed SMB fixes). Wave 4 is a **re-derivation pass, not a fix
pass**: for each finding, check the audit's *diagnosis* against current `origin/develop`
(`f514f733a`), and classify it. **No code changes. No PRs.**

## Lane format (every lane identical)

Each lane owns a tranche of findings from the audit report. For every finding in the tranche:

1. **Re-derive against develop.** Read the audit's Where/What/Fix at the cited locations in
   the *current* checkout. The code has moved: fixes #2387/#2343/#2424/#2426/#2461, Wave 3
   (#2501 identity, #2502 adapter lifecycle, #2503 credits), and #2505 (error consolidation)
   all landed since the audit was pinned. A finding may already be fixed, drifted, or intact.
2. **Classify exactly one of:**
   - `FIXED` — the cited defect is gone on develop; name the commit/PR that closed it.
   - `LIVE` — the defect is still present verbatim; confirm the file:line.
   - `DRIFTED` — the code changed around the finding; re-diagnose at the new location
     (the Kerberos AP-REP copies are known to have drifted).
   - `INVALID` — the audit's diagnosis was wrong at pin time.
   - `WAVE5` — structure-only finding (god objects, move-only refactors) that belongs to
     Wave 5, not a fix wave; cross-reference and stop.
3. **Quote evidence**: current file:line + the one code line that decides the verdict.

## Output (every lane identical)

Append to `.planning/2026-09-10-wave4-triage.md` under the lane's own heading, one table row
per finding:

```
| <audit finding ID/heading> | <FIXED|LIVE|DRIFTED|INVALID|WAVE5> | <current file:line> | <one-line evidence> |
```

Plus a lane summary line: `LANE=<name> TOTAL=<n> FIXED=<n> LIVE=<n> DRIFTED=<n> INVALID=<n> WAVE5=<n>`.

## Shared rules (verbatim for every lane)

- Base: current `origin/develop` (`f514f733a`). No pull/rebase/merge/push. No branch. No commit.
  This is read-only triage; the orchestrator writes the final doc.
- Never hand-edit `docs/guide/cli.md` (generated).
- The audit report is *not* authoritative — the code is. If the audit's line numbers are stale,
  find the code by symbol name (`rg`), not by line.
- Known already-fixed findings you may meet (do not re-litigate): tree scoping (#2424),
  CANCEL async-id (#2426), FileId bind (#2461), rename no-op (#2387), ChangeTime (#2343),
  session-user rename (#2501), listener lifecycle (#2502), async credits (#2503).
- Stop condition: return the per-finding table + summary as your final output. Do not write files.

## Lanes (disjoint by audit area; 157 findings across 6 lanes)

- **Lane T1 (52):** `handlers-set-info` (12) + `handlers-read-write` (12) + `handlers-negotiate-session` (12)
  - `handlers-durable` (12) + `smb-crypto` (3) + `pkg-smb-connection` (1)
- **Lane T2 (47):** `smb-compound` (10) + `handlers-tree-connect` (10) + `handlers-auth` (9)
  - `smb-response-pipeline` (8) + `smb-session` (6) + `smb-lease-manager` (5) — minus 1 overlap
- **Lane T3 (41):** `smb-conn-dispatch` (7) + `handlers-security-sd` (7) + `handlers-ioctl-core` (7)
  - `handlers-core-handler` (7) + `handlers-lease-oplock` (6) + `handlers-create-post-break` (6) — minus 1 overlap
- **Lane T4 (8):** `handlers-create` (8)
- **Lane T5 (13):** `pkg-smb-adapter` (5) + `smb-auth-ntlm-spnego` (4) + HIGH-priority cross-checks:
  unclaimed-SessionId keep (`session_setup.go` handleNTLMNegotiate), anonymous/guest encryption
  bypass (`response.go checkEncryptionRequired`), AppInstanceId force-close cross-check
- **Lane T6 (orchestrator, inline):** consolidate lane outputs into
  `.planning/2026-09-10-wave4-triage.md`, reconcile overlaps, produce the Wave 4 verdict table
  (what joins the SMB fix waves vs Wave 5 vs walk-out), update roadmap + memory.

## Priority flags for the fix-wave ordering (from the orchestrator's spot-check)

- LIVE HIGH: fresh SESSION_SETUP with unclaimed nonzero SessionId keeps the client-supplied ID
  (`session_setup.go` handleNTLMNegotiate — sessionID != 0 and not found falls through keeping it).
- LIVE HIGH: anonymous/guest bypass in `checkEncryptionRequired` defeats per-share
  `SMB2_SHAREFLAG_ENCRYPT_DATA` (`response.go:707`).
- Both need a design decision (MS-SMB2 3.3.5.2.9 anonymous-can't-encrypt tension), not a blind patch.
