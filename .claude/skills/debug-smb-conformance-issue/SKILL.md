---
name: debug-smb-conformance-issue
description: "Rigorous investigate-before-fix workflow for SMB conformance bugs in DittoFS. Research MS-SMB2 / MS-FSA specs, compare against Samba, read the smbtorture / WPTS test source, run /gsd-debug for isolation, diff byte-level traffic against dperson/samba, then propose a plan — never a workaround to make a test pass."
argument-hint: "[issue number | failing smbtorture test name | short description]"
allowed-tools:
  - Read
  - Bash
  - Grep
  - Glob
  - WebFetch
  - WebSearch
  - Agent
  - Skill
  - AskUserQuestion
---

<objective>
Address an SMB conformance bug — `smbtorture` subtest failing, WPTS FSA test failing, Samba/Windows client misbehaving against DittoFS, pcap-level interop divergence — by finding the true root cause and proposing a plan grounded in MS-SMB2 / MS-FSA spec evidence and Samba behavior.

**Never shape the fix around making a specific test pass.** If you catch yourself writing code whose only justification is "to make smb2.X.Y go green", stop and re-investigate.

Use for:
- smbtorture subtest failures (`smb2.*`, `smb2.session.*`, `smb2.delete-on-close-perms.*`, `smb2.lease.*`, …)
- WPTS FSA failures
- Windows / macOS / Samba client interop complaints
- Hangs, timeouts, or wrong-answer responses on SMB traffic
- Anything involving signing, SPNEGO, NTLMSSP, Kerberos, oplocks, leases, durable handles, ADS, ChangeNotify

Not for: unit-test-only bugs that don't touch protocol semantics, build/CI issues, NFS-only bugs.
</objective>

<context>
User's input: $ARGUMENTS

Resolve to a concrete target before investigating:
- If $ARGUMENTS is `#N` or `N`, run `gh issue view N` and use the issue body as the source of truth for symptoms.
- If $ARGUMENTS is a test name like `smb2.session.reauth5`, locate it in `test/smb-conformance/smbtorture/KNOWN_FAILURES*.md` and in the most recent `test/smb-conformance/results/smbtorture-*/dittofs.log`.
- Otherwise treat it as a freeform description.
</context>

<workflow>

## 1. Frame the symptom precisely

Extract from the issue / logs:
- **Exact wire command(s)** failing — CREATE / CLOSE / SESSION_SETUP / IOCTL / etc., with opcode decoded.
- **Decoded flag bits** — `desiredAccess`, `createOptions`, `shareAccess`, `createDisposition`, `createContexts`. Never leave these as raw hex — expand against MS-SMB2 §2.2.13.
- **Server response** (NT_STATUS) vs **expected** per test.
- **Log line that reveals internal state** — grep `test/smb-conformance/results/smbtorture-*/dittofs.log` for the test name and the failing op.
- **Recent related commits**: `git log --oneline -20 -- internal/adapter/smb/ pkg/metadata/`.

Write one paragraph stating the divergence. If ambiguous, ask the user before spending research effort.

## 2. Consult the MS specs

Always cite sections, not vibes:
- **MS-SMB2** — wire protocol, opcodes, flag layouts, signing/encryption. https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2
- **MS-FSA** — file-system algorithm (permission checks, open semantics, delete-on-close, lease break state machines). https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa
- **MS-SPNG** — SPNEGO token format and GSS flags.
- **MS-KILE** — Kerberos extensions as used by SMB.
- **MS-DFSC**, **MS-FSCC** — referrals, FS control codes, FileInformation classes.

Use `WebFetch` on the relevant MS spec page to ground claims. If the spec is ambiguous, note that explicitly — Samba's behavior is tiebreaker.

## 3. Compare against Samba

Samba is the canonical SMB implementation. For any operation DittoFS handles differently than expected, find the Samba equivalent:

- Server side: https://github.com/samba-team/samba — `source3/smbd/smb2_*.c`, `source4/smb_server/smb2/`.
- Torture test source (authoritative expectations): `source4/torture/smb2/*.c`. For a failing `smb2.session.reauth5`, read `source4/torture/smb2/session.c::test_session_reauth5` — the test source literally encodes what the server is expected to do.
- NTVFS / posix mapping: `source3/smbd/open.c`, `source3/smbd/close.c` for delete-on-close; `source3/smbd/smb2_lease.c` for leases; `source3/smbd/smb2_create.c` for create contexts.

Quote the Samba line that contradicts DittoFS behavior, with a commit-permalink when possible.

## 4. Read the smbtorture / WPTS test

The test source is ground truth for what the client expects. Read it before writing any fix:

- smbtorture — `source4/torture/smb2/*.c` in Samba. Look for the `struct torture_suite_entry` matching the failing test name.
- WPTS — https://github.com/microsoft/WindowsProtocolTestSuites under `TestSuites/FileServer/src/TestSuite/FSA/`.

Often the reported failure is a downstream effect of an earlier protocol-level divergence. Read upstream to confirm.

## 5. Run /gsd-debug for systematic isolation

When code-reading alone doesn't produce a clear root cause:

```
/gsd-debug <brief SMB bug description>
```

This runs the scientific-method debug session in an isolated subagent with persistent state (`.planning/debug/<slug>.md`). Use for:
- State-machine bugs (lease/oplock/DH) where the trigger is non-obvious.
- Intermittent failures — the session tracks hypotheses across reproductions.
- Multi-layer bugs where you suspect interaction between SMB handler, metadata store, and block store.

Skip when the code already points to an obvious root cause.

## 6. Pcap-diff against dperson/samba

**For any SMB bug that could plausibly be on the wire, do this.** Source-reading alone misses SPNEGO token layout, NTLMSSP flag ordering, SMB2 error-body format (MS-SMB2 §2.2.2), mechListMIC presence, nonce derivation, lease-break packet structure.

Use the playbook already documented in `CLAUDE.md` — "Debugging protocol interop (SMB/NFS)". Reproduced here for immediate use:

```bash
# 1. Reference Samba on 11445 (DittoFS owns 12445; keep them distinct)
docker run --rm -d --name ref-server --platform linux/amd64 -p 11445:445 \
  -v /tmp/ref-share:/share \
  dperson/samba:latest \
  -u "testuser;TestPassword01!" \
  -s "share;/share;yes;no;no;testuser"

# 2. Capture on both
docker exec -u root -d ref-server tcpdump -i any -w /tmp/ref.pcap -s 0 port 445
# Capture DittoFS traffic similarly on its port (12445).

# 3. Run the same smbtorture subtest against each

# 4. Diff with tshark — DittoFS on non-standard port needs the NBSS hint
docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/ref.pcap -V \
  -Y "smb2.cmd==<opcode> and smb2.flags.response==1" \
  -c 1 2>/dev/null

docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/dittofs.pcap -d tcp.port==12445,nbss -V \
  -Y "smb2.cmd==<opcode> and smb2.flags.response==1" \
  -c 1 2>/dev/null
```

**Go straight to pcap when** the symptom involves signing, SPNEGO, NTLMSSP flags, encryption, error-body format, lease/oplock state, create contexts, or compound request boundaries.

Pitfalls (also in CLAUDE.md):
- Apple Silicon → `--platform linux/amd64` always.
- Non-445 SMB → `-d tcp.port==N,nbss` to decode NBSS framing.
- Always `docker compose down -v` between runs.
- Keep both pcaps for post-mortem.

Alternative reference images: `quay.io/samba/samba:latest` (full, DC-capable — needed for Kerberos/AD tests), `erichough/nfs-server` (for NFS side).

## 7. Check KNOWN_FAILURES for precedent

Read both files before declaring a bug novel:
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md`
- `test/smb-conformance/smbtorture/KNOWN_FAILURES_KERBEROS.md`

A related failure may already be documented with root-cause analysis. If the new failure is the same class, note it and fix the cluster together rather than one-off.

Also scan `.planning/debug/` for prior investigations of adjacent bugs — DittoFS has accumulated a lot of SMB debug notes there.

## 8. Formulate the fix plan

The plan must:

- **Name the real root cause**, not the symptom. If multiple causes compound (e.g. a permission bug + an error-swallowing bug that together cause a hang), enumerate each (Bug A, Bug B, …) and fix all of them.
- **Cite MS-SMB2/MS-FSA section** governing correct behavior.
- **Cite Samba** for how the canonical implementation does it.
- **Preserve POSIX / NFS semantics** where DittoFS honors them — SMB fixes must not regress unix-client behavior. Flag metadata-layer changes for their NFS blast radius.
- **List concrete files and line numbers** to change (usually: `internal/adapter/smb/v2/handlers/*.go`, `internal/adapter/smb/session/*.go`, `pkg/metadata/*.go`).
- **List conformance-suite tests** in `pkg/metadata/storetest/`, `pkg/blockstore/local/localtest/`, `pkg/blockstore/remote/remotetest/` that need updated coverage.
- **List verification steps**:
  - `go test ./pkg/metadata/... ./internal/adapter/smb/...`
  - `cd test/smb-conformance && ./run-smbtorture.sh [--kerberos] <subsuite>`
  - Pcap-diff against dperson/samba if wire layout was part of the root cause.
  - Regression sweep: adjacent smbtorture subsuites (e.g. `smb2.session` full run if touching session layer).
  - NFS regression: `go test ./internal/adapter/nfs/...` + `sudo ./run-e2e.sh` if metadata layer changed.
- **List KNOWN_FAILURES updates** — which entries should flip to passing, which should be pruned, which (if any) need a new entry.
- **Identify residual known-failures** that this fix does *not* address, with a one-line note about why they're separate.

Deliver via ExitPlanMode if plan mode is active, otherwise as a structured response.

## 9. Commit & PR

- Sign commits (`git commit -S`) — see MEMORY.md "Always sign commits".
- Never mention Claude Code or add `Co-Authored-By` lines.
- Keep messages concise. Pattern: `fix(smb): <specific behavior> — close #NNN`.
- One logical change per commit. If the fix is split between `pkg/metadata/` and `internal/adapter/smb/`, land them as separate commits.

## 10. Anti-patterns to avoid

- **Error-swallowing**: logging an error and returning `STATUS_SUCCESS` is almost always wrong. Protocol clients trust status codes — a lie causes hangs, loops, and corruption.
- **Test-shaped fixes**: reverse-engineering "what does this test want" and making that exact thing pass. The test is a symptom; the protocol spec is the truth.
- **Workarounds gated behind flags**: default-off fixes stay off.
- **Folding refactors into the fix**: land the fix first, refactor separately.
- **Skipping the spec because the code looks reasonable**: reasonable code that doesn't match MS-FSA is still wrong.
- **Declaring success on a green subtest without running the adjacent suite**: SMB fixes often ripple — run at least the full parent suite (`smb2.session`, `smb2.create`, etc.) before claiming done.

</workflow>

<project_refs>
- Invariants and the Docker/tshark playbook: project `CLAUDE.md` → "Architecture invariants" and "Debugging protocol interop (SMB/NFS)".
- SMB adapter layout: `internal/adapter/smb/` (sessions, v2 handlers, crypto).
- Metadata + permissions: `pkg/metadata/` (especially `auth_permissions.go`, `file_remove.go`, `directory.go`).
- Conformance results and known failures: `test/smb-conformance/`.
- Prior debug notes: `.planning/debug/smb-*.md`, `.planning/debug/oplock-*.md`, `.planning/debug/lease-*.md`.
- Samba reference: https://github.com/samba-team/samba
- smbtorture test source: `source4/torture/smb2/*.c` in the Samba tree.
</project_refs>
