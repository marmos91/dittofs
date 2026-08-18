---
name: fix-issue
description: End-to-end workflow for taking a DittoFS GitHub issue from report to merged fix — read the issue, test its premise, reproduce, fix the root cause, verify, open a PR against develop, babysit Copilot and CI, squash-merge, and close the issue. Use this whenever a DittoFS issue is referenced by number or URL and any work on it is implied ("fix #1934", "address issue 1888", "look into 1923", "what's going on with https://github.com/.../issues/1956"), including when the ask is only to investigate — the investigation half applies either way. Also use when resuming an in-flight issue branch or PR.
---

# Fixing a DittoFS issue

Take the issue from report to merged, autonomously. The stop conditions at the
end are the only places to come back.

The most expensive failure mode here is **a confident fix for the wrong
problem**. Issues describe symptoms and often carry a theory about the cause;
that theory is frequently wrong — sometimes written by a past agent that read
code without running it.

## 1. Set up an isolated workspace

Other agents work this repo concurrently. Never build in the primary checkout.

```bash
cd "$(git rev-parse --show-toplevel)"
git fetch origin && git log --oneline -1 origin/develop   # know your base
git worktree add ~/dittofs-worktrees/<slug> -b fix/<issue>-<slug> origin/develop
```

`<slug>` is 2-4 words naming the defect (`1934-resolve-covering`,
`1888-unplaceable-guard`); use `feat/` instead of `fix/` for feature issues.
Branch from `origin/develop`, never from a local `develop` that may be stale and
never from a stash. Worktrees live in `~/dittofs-worktrees/`, never `/tmp` —
they outlive the session and you may need to come back to one.

## 2. Read the issue, then distrust it

```bash
gh issue view <N> --comments
```

Keep three things separate:

- **The observed symptom** — what someone actually saw. This is evidence.
- **The claimed cause** — what the issue says is happening. This is a hypothesis.
- **The claimed reachability** — "latent", "cannot happen today", "only affects
  X". This is the most load-bearing and least verified claim in most issues.

**Verify the premise before building on it.** Whether a defect is reachable
decides which fix is correct, so a wrong premise doesn't just waste time — it
produces a fix that is wrong in a way that passes review. #1934 was filed as
latent-and-unreachable; a randomized mutation soak on unmodified `develop` hit
it in 3 of 24 seeds, and that discovery flipped the fix from "return an error"
(which broke live reads) to "resolve to the greatest start".

Verify refutations with the same rigor as confirmations. Concluding "this can't
happen" closes a door quietly, and a wrong refutation is invisible. Prefer
structural arguments (this call path has no caller; this field is never written)
over numeric ones (I ran it 100 times and it didn't reproduce).

If the premise turns out to be wrong, say so plainly in the PR body — that
finding is often worth more than the diff.

## 3. Investigate

Start with the knowledge graph; it returns a scoped subgraph far smaller than
grep output or the full architecture report:

```bash
graphify query "<the question you actually have>"
graphify path "<A>" "<B>"      # how two things relate
graphify explain "<concept>"   # focused concept
```

Then read the specific code. `docs/internals/architecture.md` is the map;
`docs/internals/debugging.md` has the SMB/NFS pcap-diff playbook. Use
`superpowers:systematic-debugging` for a stubborn bug.

Respect the invariants in `CLAUDE.md` — protocol handlers stay protocol-only,
every op carries an `*metadata.AuthContext`, file handles are opaque, block
stores are per-share, errors are `metadata.ExportError` values. A fix that
violates one of these will be correct and still get rejected.

**For protocol questions, go to the source of truth** — guessing at wire
behaviour is how interop bugs are born:

- RFCs for NFS — RFC 1813 (v3), RFC 7530 (v4.0), RFC 8881 (v4.1).
- MS-SMB2 and MS-FSCC on Microsoft Learn for SMB2/3 — use the
  `microsoft-docs` skill, which searches the official docs directly.
- Samba and nfs-ganesha source for "what do real implementations actually do
  when the spec is ambiguous". If Samba also fails a conformance case, that
  case is likely testing something unimplementable, not a DittoFS bug (#1923,
  #1832 both resolved this way).

## 4. Reproduce deterministically before fixing

A fix you cannot demonstrate failing before and passing after is a guess. Build
the smallest reliable reproduction you can, and prefer one that lives in the
repo afterward as a regression test. For races and CI-only failures, invest in
determinism rather than retrying — #1701's dir-lease double-break only became
fixable once it reproduced on demand.

Two traps that have bitten repeatedly:

- **Fake sinks and in-memory doubles do not validate durability or ordering
  work.** A green unit suite (with `-race`) sat alongside a rig that wedged on
  real hardware. If the change touches carve/flip, journal, fsync ordering, or
  eviction, the reproduction has to run against something real.
- **Reading code is not verifying behaviour.** Say "predicted" until a
  checksummed read-back, a packet capture, or a failing assertion confirms it.

## 5. Fix the root cause

Grep every caller of the function you are about to change. The symptom named in
the issue is usually one of several paths through a shared seam, and patching
only the named path leaves the siblings broken — one guard in the shared
function is both smaller and more correct than a guard per caller.

When the seam is an access point (a fault-in, a cache lookup, a handle
resolution), enumerate **all** the operations that cross it — read, write,
truncate, delete, clone — not just the one in the report. A review that checked
only `ReadAt` shipped #1831 with `WriteAt`, `Delete` and `Truncate` still broken.

Keep the change small and match the surrounding style. **Comments describe
behaviour only** — no issue or PR numbers, no CI or OS names, no phase or plan
IDs in source; that context belongs in the commit message and PR body.

Commit signed as you go, in small steps — long runs get killed by watchdogs and
rate limits, and uncommitted work dies with them:

```bash
git commit -S -m "fix(block): ..."
# if SSH signing fails with "Couldn't get agent socket":
git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "..."
```

## 6. Verify

Climb only as far as the change requires — each tier costs an order of
magnitude more than the last.

```bash
go build ./... && go vet ./... && go fmt ./...
go test ./...            # then -race on anything concurrent
```

Docker is available locally for protocol conformance. Pick the suite that
matches the change — they cover different things:

```bash
cd test/smb-conformance && ./run.sh     # SMB2/3: WPTS BVT, Samba-derived
cd test/nfs-conformance && ./run.sh     # Kerberos/sec=krb5 mounts ONLY, not general NFS
sudo ./test/posix/run-posix.sh          # pjdfstest: the real NFS-side FS-semantics suite
```

`test/posix/` is where an NFSv3/v4 semantics change gets proven — `setup-posix.sh`
first, `teardown-posix.sh` after. `test/e2e/` needs sudo and a kernel NFS client:
`cd test/e2e && sudo ./run-e2e.sh`. **Never run two suites concurrently** — they
collide on ports and mounts.

If the change is about crash behaviour, device loss, real kernel-client
interaction, or throughput, a laptop cannot prove it. Read
`references/scw-vm.md` before provisioning anything — it covers the recipe and
the teardown check that keeps you from destroying a development VM.

If a listed known-failure starts passing, or a new one fails, do not edit the
list on a hunch — CI is the authority on what actually fails there. There are
several, one per suite: `test/posix/KNOWN_FAILURES.md` and
`KNOWN_FAILURES_V4.md`, `test/smb-conformance/KNOWN_FAILURES.md`, and
`test/smb-conformance/smbtorture/KNOWN_FAILURES{,_KERBEROS}.md`. Flakes get
fixed, never retried.

## 7. Simplify and review before the PR

Two agents, in this order, every time. They catch different things and both
have caught real bugs that a self-review missed.

```
Agent(subagent_type: "code-simplifier:code-simplifier")
Agent(subagent_type: "feature-dev:code-reviewer")
```

The reviewer **cannot see a branch that exists only in your worktree** — it has
no git access to your working state. Hand it the actual diff content and
absolute file paths, not "review branch fix/1934-...". Otherwise it reviews
`develop` and reports nothing.

Then update whatever documentation the change invalidates. If any Cobra command
or flag moved, regenerate — never hand-edit the generated file:

```bash
go run ./cmd/gendocs      # rewrites docs/guide/cli.md
```

## 8. Open the PR

Rebase onto current `origin/develop` — never merge `develop` in.

```bash
git fetch origin && git rebase origin/develop
git push -u origin fix/<issue>-<slug>
gh pr create --base develop --assignee marmos91 --title "..." --body "..."
```

The PR body must contain a closing keyword (`Closes #<N>`). GitHub's auto-close
never fires for us — it only triggers on the default branch, and we merge to
`develop` — so `.github/workflows/close-linked-issues.yml` greps PR bodies
instead, at release time. The keyword is what makes that work.

Write the body for a reviewer who has not read the issue: what was actually
wrong, why the obvious fix is wrong if it is, and what evidence you have. If you
refuted the issue's premise, lead with that. Include the reproduction output.
Never mention Claude Code, AI tooling, or add `Co-Authored-By` lines for AI.

## 9. Babysit the PR

Stay with the PR until it merges.

**Copilot review.** Wait for it and read every comment. It has repeatedly caught
real bugs that both the reviewer agent and I missed (#1837, #1838, #1831).
Address what is real; reply explaining why on what is not.

```bash
gh pr view <PR> --comments
gh pr diff <PR>
```

**CI.** Ten workflows run per PR — `unit-tests`, `lint`, `integration-tests`,
`posix-tests`, `smb-conformance`, `operator-tests`, `windows-build`,
`secret-scan`, `ci-health`, and `nfs-kerberos` (path-scoped to the kerberos and
NFS adapter trees). The protocol suites are slow.

```bash
gh pr checks <PR> --watch
```

Do not wait on `e2e-tests`, `smb-client-compat`, or `ad-dc` — they are
postsubmit/nightly only and never appear on a PR. Waiting for them hangs step 9
forever.

When a check goes red, investigate the actual failure rather than re-running.
Pull the failing job's log, find the assertion, and check whether the same job
fails on `origin/develop` before assuming it is yours — `develop` is
occasionally red for unrelated reasons, which is not cause for alarm.

A flaky SMB-conformance run is a known condition and does not block. Everything
else that is red gets fixed, on this branch, and the loop returns to step 6.

## 10. Merge and close

When CI is green and every Copilot comment is addressed:

```bash
gh pr merge <PR> --squash --delete-branch
gh issue close <N> --comment "Fixed in #<PR>."     # manual — see step 8
graphify update .                                   # keep the graph current
git worktree remove ~/dittofs-worktrees/<slug>
```

Closing is manual because that automation only fires when work reaches `main` at
release time. An issue left open here stays open for weeks.

## When to stop and come back

Autonomy ends where a decision is not yours to make:

- The fix contradicts a documented decision — a `KNOWN_FAILURES.md` entry
  justified as architectural, an ADR, an invariant in `CLAUDE.md`. #1832's
  replay6 case is mutually exclusive with conformant durable-handle behaviour
  and Samba fails it too; re-attempting it wastes a day. Report, don't re-litigate.
- The change needs to touch `main`, rewrite published history, or force-push a
  protected branch.
- The issue is a duplicate, already fixed, or its premise collapses entirely and
  there is nothing left to fix.
- A VM teardown target fails the identity check in `references/scw-vm.md`.

In each case: post what you found on the issue, leave the worktree in place, and
report back.
