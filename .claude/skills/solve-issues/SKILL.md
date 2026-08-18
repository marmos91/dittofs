---
name: solve-issues
description: Work several DittoFS issues at once by fanning out one subagent per issue, each running the fix-issue skill in its own worktree, then serializing the merges back into develop. Use whenever more than one DittoFS issue number is handed over in a single ask ("/solve-issues 1823 1843", "fix 1888 and 1923", "clear these four issues", "work through the audit backlog"), or when a list or milestone of issues should be worked in parallel. For a single issue, use fix-issue directly instead.
---

# Solving several issues in parallel

This is an orchestrator. All the actual work — investigation, reproduction,
fixing, verification, PR authoring, CI babysitting — lives in the `fix-issue`
skill, and each subagent runs that skill unchanged. Your job is the three things
`fix-issue` cannot do from inside a single issue's context: decide what can
safely run at once, keep the agents off each other's shared resources, and land
the results in an order that leaves `develop` green.

## 1. Triage before fanning out

Read every issue first, in one pass:

```bash
for n in <issues>; do gh issue view $n --json number,title,labels,state,body \
  -q '"#\(.number) [\(.state)] \(.title)"'; done
```

Drop anything already closed or already covered by an open PR
(`gh pr list --search "<N>"`) before spending an agent on it. Then group by the
code they touch — `graphify query` on each issue's subject area is fast and
tells you which ones land in the same files.

**Issues that touch the same code go to one agent, sequentially.** Two agents
editing the same file produce two PRs that each pass CI alone and conflict on
merge, and the second one to rebase inherits a mess it has no context for. One
agent holding both issues writes one coherent change, or two commits that stack
cleanly. Independent issues each get their own agent.

## 2. Fan out

One `Agent` call per group, all in a single message so they run concurrently.
Each prompt says explicitly which skill to run and where the boundaries are:

```
Use the fix-issue skill to work DittoFS issue(s) #<N>[, #<M> in order].

Worktree: ~/dittofs-worktrees/<slug>   (create it yourself, branch off origin/develop)

Deviations from fix-issue for this parallel run:
- STOP after the PR is green and every Copilot comment is addressed.
  Do NOT merge. Report the PR number and a one-paragraph summary of the
  root cause. Merges are serialized by the orchestrator.
- Do NOT run test/e2e/run-e2e.sh, test/smb-conformance/run.sh, or
  test/nfs-conformance/run.sh without asking me first — they collide on
  ports, mounts and sudo, and only one agent may hold them at a time.
- Do NOT provision or tear down any Scaleway VM. Ask me instead.

Commit signed and often; report back even if you conclude there is nothing
to fix.
```

Those three carve-outs are the only genuinely shared state: the protocol suites
bind fixed ports and mount points, there is one `scw` account and one
`.bench-vm.json`, and `develop` is a single branch. Worktrees, builds,
`go test` and graphify queries all parallelize fine.

## 3. Broker the shared resources

While agents run, you hold the contended resources. When one asks:

- **Conformance or e2e suite** — grant to one agent at a time. Tell the others
  to wait rather than to skip; a skipped protocol suite is how interop
  regressions ship.
- **A VM** — read `.claude/skills/fix-issue/references/scw-vm.md`, provision one
  yourself, and hand the agent the IP. Share a single VM across agents if their
  reproductions do not conflict. You own the teardown, including the identity
  check that stops a development VM being destroyed.

## 4. Merge serially

Never merge concurrently. Each squash-merge moves `develop`, which invalidates
every other branch's base — CI green on a stale base proves nothing about the
tree that actually results.

Order by blast radius, smallest first, so the risky change rebases onto known-good
work rather than the reverse. For each PR in turn:

```bash
gh pr checks <PR>                                  # still green on the current base?
gh pr merge <PR> --squash --delete-branch
gh issue close <N> --comment "Fixed in #<PR>."     # manual — auto-close only fires on main
```

Then, before the next one:

```bash
cd ~/dittofs-worktrees/<next-slug>
git fetch origin && git rebase origin/develop && git push --force-with-lease
```

and wait for its checks again. If a rebase produces conflicts or turns a check
red, hand it back to that issue's agent with the failure — it has the context
you do not.

Once everything is landed:

```bash
graphify update .
git worktree remove ~/dittofs-worktrees/<slug>     # for each
```

## 5. Report

A short table, one row per issue: number, what was actually wrong, PR, merged or
blocked. Call out separately any issue whose **premise turned out to be false** —
that finding often matters more than the fix, and it is the thing most likely to
be lost when several issues are reported at once.

Do not report an issue as done until its PR is merged and the issue is closed.
