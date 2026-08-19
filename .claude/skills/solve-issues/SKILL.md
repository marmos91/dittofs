---
name: solve-issues
description: Work several DittoFS issues at once by fanning out one subagent per issue, each running the fix-issue skill in its own worktree, then serializing the merges back into develop. Use whenever more than one DittoFS issue number is handed over in a single ask ("/solve-issues 1823 1843", "fix 1888 and 1923", "clear these four issues", "work through the audit backlog"), or when a list or milestone of issues should be worked in parallel. For a single issue, use fix-issue directly instead.
---

# Solving several issues in parallel

This is an orchestrator. All the actual work — investigation, reproduction,
fixing, verification, PR authoring, CI babysitting — lives in the `fix-issue`
skill, and each subagent runs that skill unchanged. Your job is what `fix-issue`
cannot do from inside a single issue's context: settle the open design questions
with the user in one pass instead of six, decide what can safely run at once,
keep the agents off each other's shared resources, and land the results in an
order that leaves `develop` green.

## 1. Triage before fanning out

Read every issue first, in one pass:

```bash
for n in <issues>; do gh issue view $n --json number,title,labels,state,body \
  -q '"#\(.number) [\(.state)] \(.title)"'; done
```

Drop anything already closed or already covered by a PR before spending an agent
on it. Search **all** PR states, not just open ones:

```bash
for n in <issues>; do gh pr list --state all --search "$n" \
  --json number,state,title -q '.[] | "  #\(.number) [\(.state)] \(.title)"'; done
```

A merged PR means the fix may already be on `develop` and the issue merely never
got closed — check before dispatching. A **closed-unmerged** PR matters even more:
someone already attempted this and abandoned it, and its diff and review comments
tell you why. Read it and hand that context to the agent rather than letting it
rediscover the dead end. Expect incidental hits too (a PR that merely mentions the
number); confirm the PR actually addresses the issue before dropping it.

Then group by the code they touch — `graphify query` on each issue's subject area
is fast and tells you which ones land in the same files.

**Issues that touch the same code go to one agent, sequentially.** Two agents
editing the same file produce two PRs that each pass CI alone and conflict on
merge, and the second one to rebase inherits a mess it has no context for. One
agent holding both issues writes one coherent change, or two commits that stack
cleanly. Independent issues each get their own agent.

## 2. Settle the open questions with the user — before any agent starts

Triage tells you what the issues are. It does not tell you what the user wants
built, and issues written as investigations routinely leave that open: a default
value nobody has picked, two viable designs, a contract question ("is this a bug
or the intended promise?"), a scope boundary ("does this PR also ship the repair
path?").

**Collect every such question across all the issues and ask them in one pass,
before fanning out.** An agent that guesses spends its whole run building the
wrong thing, and you find out at PR review — after the tokens are gone and with a
branch that has to be thrown away. Asking costs one round trip.

Use `AskUserQuestion`, batched — up to four questions per call, several calls if
you need more. Lead with your recommendation as the first option and mark it
`(Recommended)`; the user should be able to accept your judgement with one click.

For anything genuinely open, invoke the relevant process skill *first* rather than
inventing an approach:

- **`superpowers:brainstorming`** — when an issue proposes a direction but the
  design is unsettled, or when two approaches have real trade-offs. Brainstorm,
  then put the resulting options to the user.
- **`superpowers:systematic-debugging`** — when an issue's *premise* is a
  hypothesis rather than a measured fact ("this is latent and cannot happen
  today", "the cause is probably X"). Establish the premise before asking the
  user to choose between fixes for it; a wrong premise makes every option wrong.
  See `feedback_test_the_issues_premise_before_building_on_it`.

What NOT to ask: anything with a conventional default, anything the codebase
already answers, anything you can verify yourself. Those are your calls — make
them, state them in the agent prompt, and move on.

Whatever the user decides, **write the decision into the agent's prompt as
settled**, with an explicit "do not block on this" — otherwise the agent
re-litigates it from the issue text, which still contains the open question.

## 3. Fan out

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

**One more shared surface, and it bites silently: the scratchpad directory.**
Agents stage PR bodies and notes there, and a generic filename is a collision
waiting to happen — one agent wrote `scratchpad/pr-body.md`, a sibling
overwrote it, and `gh pr create --body-file` published the sibling's text. The
PR then carried the wrong `Closes #NNNN` and would have auto-closed an unrelated
issue on merge. Nothing errors; the PR looks fine unless you read it. Add to
every agent prompt: **scratchpad filenames must be issue-scoped**
(`pr-body-1958.md`, not `pr-body.md`), and re-read the file immediately before
publishing it. Verify `Closes #NNNN` on each PR yourself at merge time — it is
one `gh pr view --json body` and it catches this class outright.

**Use `Agent`, not `Workflow`, for this fan-out.** A `Workflow` script runs
detached and cannot call `AskUserQuestion`, which kills step 2 — and step 2 is
the part that stops six agents building the wrong thing. Merging into `develop`
and brokering the VM are human-judgement calls for the same reason. The fan-out
here is six independent items with no cross-item dependency, so deterministic JS
orchestration buys nothing over six concurrent `Agent` calls.

Where a workflow *does* pay is one level down, and `fix-issue` step 4 already
carries it: an adversarial refute-panel over the claimed root cause before any
fix is written. Each fanned-out agent inherits it automatically. You do not need
to ask for it in the prompt — but you do need to budget for it, since every
agent that decides its issue warrants a panel spends four more agents on it.
`fix-issue` tells them to skip it for mechanical fixes; if a batch is mostly
one-liners, expect most panels not to run.

## 4. Broker the shared resources

Expect agents to die on the 600s stream watchdog — in a six-way fan-out, most of
them will, usually mid-step. This is routine, not a failure: `SendMessage` to the
agent's id resumes it with its context intact. Before resuming, check the ground
truth yourself and put it in the message — branch, commits ahead of develop,
uncommitted file count, whether a PR exists — because the agent resumes believing
its last action succeeded:

```bash
for w in <slugs>; do d=~/dittofs-worktrees/$w; \
  echo "$w: $(git -C $d rev-parse --abbrev-ref HEAD) | commits=$(git -C $d rev-list --count origin/develop..HEAD) | dirty=$(git -C $d status --porcelain | wc -l)"; done
```

Tell any agent with uncommitted files to commit before anything else. See
`feedback_agents_commit_wip_between_steps`.

**Check for duplicate branches and worktrees per issue, not just per PR.** A
stalled-and-resumed agent can create a second worktree under a name you did not
assign and open a second PR from it — two PRs for one issue, minutes apart, both
authored by you, neither aware of the other. `git worktree list` and
`git ls-remote --heads origin` catch it; `gh pr list` alone reads as two
unrelated PRs. Diff them before closing either: the duplicate is not always the
worse one, and the surviving PR should be the superset.

While agents run, you hold the contended resources. When one asks:

- **Conformance or e2e suite** — grant to one agent at a time. Tell the others
  to wait rather than to skip; a skipped protocol suite is how interop
  regressions ship.
- **A VM** — read `.claude/skills/fix-issue/references/scw-vm.md`, provision one
  yourself, and hand the agent the IP. Share a single VM across agents if their
  reproductions do not conflict. You own the teardown, including the identity
  check that stops a development VM being destroyed.

## 5. Merge serially

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

## 6. Report

A short table, one row per issue: number, what was actually wrong, PR, merged or
blocked. Call out separately any issue whose **premise turned out to be false** —
that finding often matters more than the fix, and it is the thing most likely to
be lost when several issues are reported at once.

Do not report an issue as done until its PR is merged and the issue is closed.
