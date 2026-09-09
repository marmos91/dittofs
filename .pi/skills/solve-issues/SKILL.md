---
name: solve-issues
description: Work several DittoFS issues at once by fanning out one subagent per issue group, each working the fix-issue skill in its own worktree, then serializing the merges back into develop. Use whenever more than one DittoFS issue number is handed over in a single ask ("/solve-issues 1823 1843", "fix 1888 and 1923", "clear these four issues", "work through the audit backlog"). For a single issue, work fix-issue directly instead.
---

# Solving several issues in parallel

This is an orchestrator. All the actual work — investigation, reproduction,
fixing, verification, PR authoring, CI babysitting — lives in the `fix-issue`
skill (`.pi/skills/fix-issue/SKILL.md` in this repo), and each subagent
works that skill unchanged. Your job is what a single issue's context cannot do:
settle the open design questions with the user in one pass instead of six,
decide what can safely run at once, broker shared resources, and land the
results in an order that leaves `develop` green.

## 1. Triage before fanning out

Read every issue first, in one pass:

```bash
for n in <issues>; do gh issue view $n --json number,title,labels,state,body \
  -q '"#\(.number) [\(.state)] \(.title)"'; done
```

Drop anything already closed or already covered by a PR before spending an
agent on it. Search **all** PR states, not just open ones:

```bash
for n in <issues>; do gh pr list --state all --search "$n" \
  --json number,state,title -q '.[] | "  #\(.number) [\(.state)] \(.title)"'; done
```

A merged PR means the fix may already be on `develop`. A **closed-unmerged** PR
matters even more: someone already attempted this and abandoned it — read its
diff and review comments and hand that context to the agent rather than letting
it rediscover the dead end.

Then group by the code they touch — `graphify query` on each issue's subject
area is fast and tells you which ones land in the same files. **Issues that
touch the same code go to one agent, sequentially.** Two agents editing the same
file produce two PRs that each pass CI alone and conflict on merge. Independent
issues each get their own agent.

## 2. Settle the open questions with the user — before any agent starts

Triage tells you what the issues are. It does not tell you what the user wants
built. **Collect every open design question across all the issues and ask them
in one pass, before fanning out.** An agent that guesses spends its whole run
building the wrong thing, and you find out at PR review — after the tokens are
gone and with a branch that has to be thrown away. Asking costs one round trip.

Whatever the user decides, **write the decision into the agent's prompt as
settled**, with an explicit "do not block on this" — otherwise the agent
re-litigates it from the issue text, which still contains the open question.

What NOT to ask: anything with a conventional default, anything the codebase
already answers, anything you can verify yourself. Those are your calls.

## 3. Fan out (pi-native)

Call the `subagent` tool ONCE with a `workflowScript` that fans out one child
per issue group — each child with `worktree: true` (isolated managed git
worktree, branched from clean HEAD) and the `fix-issue` skill loaded:

```js
subagent({
  workflowScript: `
    const groups = [ /* one entry per independent issue group */ ];
    const runs_ = groups.map((g, i) => ({
      key: 'issue-' + i,
      agent: 'worker',
      task: g.prompt,
      skill: 'fix-issue',
      worktree: true,
    }));
    return runs.all(runs_);
  `,
  cwd: '/Users/marmos91/Projects/dittofs',
  async: true,
})
```

Each child prompt says explicitly which skill to work and where the boundaries
are — the three genuinely shared resources below, plus this deviation:

- **STOP after the PR is green and every Copilot comment is addressed. Do NOT
  merge.** Report the PR number and a one-paragraph summary of the root cause.
  Merges are serialized by the orchestrator (step 5).
- **Scratchpad filenames must be issue-scoped** (`pr-body-1958.md`, not
  `pr-body.md`) and re-read immediately before publishing — a generic filename
  once carried the wrong `Closes #NNNN` onto a PR.

The fan-out is independent items with no cross-item dependency, so plain
`runs.all` is the whole orchestration.

## 4. Broker the shared resources

Children ask the parent through pi's `contact_supervisor` channel
(`reason: "need_decision"`); check pending requests with
`subagent_supervisor({ action: "pending" })` and reply with
`subagent_supervisor({ action: "reply", replyTo, message })`. Before resuming a
child that died mid-step, check ground truth yourself and put it in the reply —
branch, commits ahead of develop, uncommitted file count, whether a PR exists —
because the child resumes believing its last action succeeded:

```bash
for w in <slugs>; do d=~/dittofs-worktrees/$w; \
  echo "$w: $(git -C $d rev-parse --abbrev-ref HEAD) | commits=$(git -C $d rev-list --count origin/develop..HEAD) | dirty=$(git -C $d status --porcelain | wc -l)"; done
```

Tell any agent with uncommitted files to commit before anything else.

**Check for duplicate branches and worktrees per issue, not just per PR.** A
stalled-and-resumed agent can create a second worktree under a name you did not
assign and open a second PR from it. `git worktree list` and
`git ls-remote --heads origin` catch it. Diff duplicates before closing either;
the surviving PR should be the superset.

While children run, you hold the contended resources. When one asks:

- **Conformance or e2e suite** — grant to one agent at a time. Tell the others
  to wait rather than skip; a skipped protocol suite is how interop regressions
  ship.
- **A VM** — read `.pi/skills/fix-issue/references/scw-vm.md`, provision one
  yourself, and hand the agent the IP. You own the teardown, including the
  identity check.

## 5. Merge serially

Never merge concurrently. Each squash-merge moves `develop`, which invalidates
every other branch's base — CI green on a stale base proves nothing about the
tree that actually results. Order by blast radius, smallest first. For each PR
in turn:

```bash
gh pr checks <PR>                                  # still green on the current base?
gh pr merge <PR> --squash --delete-branch
```

Then, before the next one, rebase it:

```bash
cd ~/dittofs-worktrees/<next-slug>
git fetch origin && git rebase origin/develop && git push --force-with-lease
```

and wait for its checks again. If a rebase conflicts or turns a check red, hand
it back to that issue's agent through the supervisor channel — it has the
context you do not.

Once everything is landed, in the MAIN checkout on merged code:

```bash
git checkout develop && git pull
graphify update .
git worktree remove ~/dittofs-worktrees/<slug>     # for each
```

## 6. Report

A short table, one row per issue: number, what was actually wrong, PR, merged or
blocked. Call out separately any issue whose **premise turned out to be false** —
that finding often matters more than the fix. Do not report an issue as done
until its PR is merged and the issue is closed.
