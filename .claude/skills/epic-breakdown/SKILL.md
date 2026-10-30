---
name: epic-breakdown
description: Expand one epic of a DittoFS implementation plan (docs/internals/planning/rfc-*-plan.md) into its standalone epic document — every MUST and MUST NOT the RFC places on the epic numbered as R-requirements, ordered subtasks with acceptance criteria, tests and a man-day estimate each, a grep-able total, and a suggested split into one to three pull requests — written to docs/internals/planning/. Also says how to create the epic and its subtasks as GitHub issues, done only when explicitly asked. Use whenever an epic is to be broken down, detailed, expanded into subtasks or made ready to start ("break down epic 5 of the journal plan", "expand rfc-1 epic 7", "/epic-breakdown 1 8"). Not for planning a whole RFC (rfc-to-epic-plan).
---

# Epic breakdown

Adapted from Addy Osmani's `planning-and-task-breakdown` skill (agent-skills, MIT licence,
see `LICENSE-agent-skills` beside this file) for DittoFS's RFCs, plans and epics.

## Overview

Expand one epic of a plan into an epic document a single owner can work from, without
opening the plan or the RFC to find out what to build. The document has three parts that
the plan does not:

1. **The requirements.** Every MUST and MUST NOT that the RFC places on the epic, numbered
   R1…Rn, so that subtasks and tests can cite them and nothing is silently dropped.
2. **The subtasks.** Small, ordered and verifiable, each with acceptance criteria, its tests,
   the requirements it covers and a man-day estimate.
3. **The pull requests.** A suggestion of how the subtasks group into one to three pull
   requests.

The reference results are `docs/internals/planning/rfc-1-journal-epic3.md` (structure,
decisions, depth) and `rfc-1-journal-epic4.md` (the requirements tables). Read both once.
**Do not copy their content.**

**Do not write code while breaking down.** Read the RFC and the code you need to understand,
and write the document. That is the whole output.

## Terminology: epic and subtask, nothing else

The units of work are the **epic** and its **subtasks**. No other word names them: not
task, ticket, story, step, phase, stage, milestone or work item. That holds in headings,
table columns, ids and prose. A subtask's id is `<epic>.<n>` (`4.3`), never `E4-3` or `Task 3`.
Progress is tracked by mapping pull requests to these ids, so they must stay stable:

- **Keep the plan's ids.** Where the plan's candidate list numbers a subtask, keep its id
  for the same work. New subtasks get the next free numbers, after the plan's.
- **Split with a letter suffix, never by renumbering.** A subtask split after its id may
  have been cited becomes `4.3a`, `4.3b`; work mapped to `4.3` then covers both parts.
- **Never reuse or drop an id that a pull request has cited.** Search first:
  `gh pr list --state all --search "<epic>.<n>"`. If the work moved, say where in §9.

A **track** (A, B or C, one line of work through the epics) and a **synchronisation point** (S1, S2, …)
are not units of work and keep their names. A **pull request** groups subtasks (§8 below);
it is not a unit of the plan either.

## When to use

- An epic of an existing plan is about to start and needs its detailed breakdown.
- An epic's subtasks are too vague to estimate or to hand to someone.
- The owner wants to know which RFC obligations the epic is answerable for.

**When not to use:** there is no plan yet (use `rfc-to-epic-plan`); the epic is "planned
after Sx" and Sx has not been reached (say so, and stop unless the user insists); the
epic's document already exists and the user did not ask to replace it.

## Step 0 — Inputs

| # | Input | How to fill it |
| --- | --- | --- |
| 1 | Plan path and epic number | **required**, from the request |
| 2 | Output path | default `docs/internals/planning/rfc-<n>-<slug>-epic<k>.md`. **If the file exists, stop and ask** whether to replace it, revise it in place, or write elsewhere. Never overwrite it on your own: someone may be working from it |
| 3 | Track | A, B, C or the team, from the plan's §2; never ask for names |
| 4 | Existing epic issue | `gh issue list --label epic --state all --search "epic <k>"`. If one exists, note its number; if issues are ever created (Step 6), there must not be a second one |

Ask for whatever is missing with `AskUserQuestion`, in one batch, before reading further.

## Step 1 — Read

- The plan: the epic's row in §2, its section in §5, its blockers and synchronisation points
  in §3, the general rules in §1.5, and every §7 coverage row that names the epic. That
  gives the **owned sections**: the RFC sections, and the related-RFC sections, the epic is
  answerable for.
- The RFC: the owned sections in full, and the sections they cite. Large RFCs exceed one
  `Read`; list headings with `rg -n '^#' <rfc>` and read by line range.
- The epics this one depends on: their documents if they exist, for the interfaces this epic
  builds on. And the epics it blocks, for what they expect from it.
- Code only as far as the plan's rules allow. If the plan says v2 ignores v1, do not read v1
  to design v2.

## Step 2 — Extract the requirements

This is the step the plan does not do, and the one that matters most. The goal is that
**every obligation the RFC places on this epic appears exactly once, numbered**, and that a
reviewer can check the list against the RFC without rereading it.

**2.1 Sweep, mechanically.** For each owned section, take its line range from the heading
list and pull every normative line:

```bash
sed -n '<from>,<to>p' <rfc> | rg -n -w 'MUST( NOT)?|SHALL( NOT)?|REQUIRED|SHOULD( NOT)?'
```

The RFCs use RFC 2119 key words in capitals, so the match is case-sensitive. Then run a
second, case-insensitive sweep for invariants stated in prose, which the RFCs write often:
`never`, `always`, `at most`, `at least`, `only`. Finally, collect the numbers the sections
put on the epic: cost bounds (`O(log n)`), limits, sizes and performance targets in tables,
and the RFC's test rows (a §9-style table) that name this area.

Keep the raw hits in a scratch file, not in the document. Step 2.4 checks against them.

**2.2 Turn the hits into requirements.**

- One requirement per obligation. Merge sentences that state the same obligation in two
  places, and cite both sections. Split a sentence that carries two independent ones.
- Restate each one as a checkable sentence in the RFC's terms, and keep the polarity: a MUST
  NOT stays a prohibition ("never…", "is refused…").
- A prose `never` / `always` becomes a requirement only if it constrains this epic's code. A
  sentence describing another component, or giving background, does not.
- A SHOULD becomes a requirement only if the epic adopts it. Otherwise record why not in §4
  (decisions).
- An obligation the RFC leaves open (it states the rule but not one case of it) stays a
  requirement, and names the subtask that decides it and records a `decision:` marker.
- An obligation in an owned section that another epic carries out: write the part that is
  this epic's, and name the epic that owns the rest ("the index offers the predicate; epic 9
  owns the hold rule"). Do not list obligations that belong wholly to another epic.

**2.3 Number and tabulate.** Number them R1…Rn in table order, grouped under short bold
headings by theme (structure, queries and cost, concurrency, limits, recovery, …). One table
per group:

```markdown
| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A file's held extents are disjoint. A superseding write splits or narrows, and never leaves two extents covering one offset | MUST NOT | RFC 1 §4 | 4.2 |
| R13 | Covering extent of an offset, or its absence, in O(log n) | bound | §4.1 | 4.1, 4.2 |
```

`Kind` is one of `MUST`, `MUST NOT`, `SHOULD` (adopted), `invariant` (prose `never` /
`always`), `bound` (cost, size or performance target) or `test row` (a row of the RFC's own
test tables). `Source` cites the section, with the RFC named when the epic has more than
one source. A requirement number is never reused or renumbered once subtasks cite it.

**2.4 Check coverage, both ways.**

- Every raw hit from 2.1 maps to an R-number, to another epic, or to "not an obligation"
  (background, an example). Go through the scratch file line by line; an unmapped hit is a
  gap.
- Every R-number has at least one subtask and at least one test that can fail if the
  requirement is broken.
- Every subtask covers at least one R-number, or says why it covers none (scaffolding that
  a later subtask's requirement needs).

## Step 3 — Carve the subtasks

**Slice vertically where the epic allows it.** Build one operation or one path end to end,
with its tests, rather than all the types, then all the methods, then all the tests:

```
Bad:  4.1 all types   4.2 all insert paths   4.3 all queries   4.4 the tests
Good: 4.1 B-tree container with its property test
      4.2 held-extent insert and point lookup, tested at 10⁶ extents
      4.3 span query with exact gaps, tested against an oracle
```

Each subtask leaves the tree building and its tests green, lands its own tests, and creates
only what it or an earlier subtask consumes.

**Size.** One engineer, ideally up to 5 man-days. That is a ceiling to aim under, not a
target, and there is no target number of subtasks. Cut by what makes sense: one logical
change, readable on its own, following the dependencies, delivering something that can be
named. A subtask over 5 man-days says why it cannot be split without leaving a half-built
state. Signs that a subtask should be split: its acceptance criteria take many bullets, it
touches two independent subsystems, or its title needs an "and" between two unrelated
things.

**Order.** Dependencies first, high-risk subtasks early so they fail fast, and checkpoints
(§7 below) wherever something larger than one subtask can be shown to work. Checkpoints are
the natural places to cut a pull request.

## Step 4 — Estimate each subtask in man-days

Man-days of one engineer, in multiples of 0.25. This is effort, not calendar time: no
parallelism, no waiting on reviews or on other epics. Each estimate includes the subtask's
tests, doc comments, review fixes and what the general rules require of it (fuzzing,
metrics, profiling, known-failure bookkeeping). Place each subtask on this scale, the same
one the plan's epic figures use:

| Man-days | Typical work |
| --- | --- |
| 0.25 | a very small change: wording or a comment, a minor visual tweak, a one-line default |
| 0.5 | a type or constant set with tests; a doc table; a config key |
| 1 | a small codec, or one well-specified function with edge-case tests |
| 2 | a component with a few interacting rules, or about five conformance checks |
| 3 | concurrency, crash safety or on-disk ordering in one component |
| 5 | a subsystem's core algorithm under race or crash testing; the ceiling for one subtask |

Weight upward for crash or durability semantics, lock ordering, per-OS code, anything
verified only with fault injection, and burn-down work whose size cannot be known in advance
(estimate it, and name it in §9). Weight downward for pure declaration work: signatures,
stubs, types the RFC already states. Do not drop work the epic owns because no subtask
names it: tests, CI wiring, fuzz targets, metrics, docs, the review step, per-platform
variants each belong to some subtask. Size test-catalogue work by the number of checks the
RFC names.

Each subtask carries exactly one estimate line, on its own line, in this form:

```markdown
**Subtask 4.2 estimate: 2.5 man-days.**
```

The epic total goes in the document header, in the same form the plan uses:

```markdown
**Epic 4 estimate: 12 subtasks, 21.5 man-days.**
```

Numbers are plain: no `~`, no ranges, no unit inside the number. Compute the total with this
script, never by hand:

```bash
grep -E '^\*\*Subtask [0-9]+\.[0-9]+[a-z]? estimate: [0-9]' <epic-doc> |
  awk '{n++; d+=$4} END{printf "%d subtasks, %g man-days\n", n, d}'
```

`grep -E '^\*\*(Epic|Subtask) [0-9.]+[a-z]? estimate:' <epic-doc>` lists the whole estimate.

**Updating the plan is not required.** Its estimate line for this epic was the figure when
the plan was written, and the epic document's line supersedes it for this epic. The plan
records decisions, not progress: it is never edited to track how the epic is going, but it
may be revised, through a pull request, when the breakdown adjusts it (a new estimate, a
subtask moving to another epic, a changed dependency). Do that only when the user asks.

## Step 5 — Write the document

The document follows the instruction that opens the plan's §1.5: **it stands alone** (it
never refers to the plan, links to it or says "see the plan"), and **it records decisions,
not progress** (no status, no checkboxes, no "TBD", nothing to keep up to date as the work
goes on; it is revised through a pull request when a decision changes). Acceptance criteria
are plain bullets, not checkboxes.

Use this structure. Section numbers are fixed so epic documents can be compared.

```
# Epic <k>: <component> v2 <title>
Header: **Sources:** the RFC sections owned, with links. **Track:** A, B, C or the team.
        **Epic <k> estimate: <n> subtasks, <d> man-days.** **Depends on:** synchronisation
        points and epics, naming what each provides. **Blocks:** epics, and what they need.

## 1. Goal                     — what exists at the end; "at the end of the epic" bullets
### 1.1 In scope
### 1.2 Out of scope           — and which epic owns each item
## 2. General rules for each epic — the plan's §1.5 rules, copied word for word
### 2.1 How the rules apply    — table: rule | subtasks | how; a rule that does not apply
                                 is named, with the reason
## 3. Requirements and invariants — the R-tables from Step 2
## 4. Decisions this epic takes   — what the RFC leaves open, the choice and why; each one
                                    names the `decision:` or `ponytail:` marker it leaves
## 5. Target tree               — `text` block, each entry tagged with its subtask
## 6. Subtasks                  — one `### <k>.<n> <title>` per subtask (below)
## 7. Order of work             — Mermaid graph of subtask dependencies, and checkpoints
## 8. Suggested pull requests   — Step 5.1
## 9. Open points
```

Each subtask in §6:

```markdown
### 4.2 Held-extent set for one file

**Subtask 4.2 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** 4.1.
- **Covers:** R1–R6, R13–R16.
- **Deliverables:** the types, functions and files it adds, with signatures where they are
  a contract.
- **Acceptance criteria:** specific, checkable conditions, in terms of the requirements.
- **Tests:** each test, and the bug it catches (a test that nothing plausible would make
  fail is not listed).
- **Rules:** how it meets each general rule that applies, unless §2.1 already says it.
```

Graphs are Mermaid, never ASCII art, styled with stroke only and quoted labels, as the
plan skill describes. Checkpoints are stadium nodes (`(["after 4.3: index API usable"])`).

### 5.1 Suggested pull requests (§8)

Suggest how the subtasks group into **one, two or at most three** pull requests. It is only
a suggestion: the owner opens the pull requests they see fit, and the subtasks stay the unit
that is estimated and tracked. A subtask is not a pull request.

- **Size:** a pull request is ideally up to 20 man-days, a ceiling to aim under rather than a
  target. An epic that does not fit three pull requests of that size is too big, and that
  goes in §9 as an open point.
- **What goes together:** group by logic, readability and dependencies, so that each pull
  request delivers a piece of functionality that can be named. Take consecutive subtasks in
  the order of work. Each pull request builds, passes its tests and leaves nothing
  half-wired; a requirement's test lands in the same pull request as the code that meets
  it. Cut at a checkpoint where you can.
- **Parallel tracks:** two pull requests with no dependency between them can be open at
  once, and the table says so.

```markdown
| PR | Subtasks | Man-days | After | Why together |
| --- | --- | --- | --- | --- |
| 1 | 4.1, 4.2, 4.4 | 7 | — | the container and counters every later subtask builds on |
| 2 | 4.3, 4.5–4.7 | 8.5 | PR 1 | the file map, markers and floor buckets share the per-file lock |
| 3 | 4.8–4.12 | 6 | PR 2 | bounds, accounting and rebuild, which read everything above |
```

Compute each pull request's man-days from the subtask lines, for example
`grep -E '^\*\*Subtask 4\.(1|2|4) estimate:' <epic-doc> | awk '{d+=$4} END{print d}'`. The
pull requests' man-days add up to the epic total. Each pull request's description names the
RFC and every subtask it delivers, one id per mention (`RFC 1, subtask 4.1, subtask 4.2`),
and says when it finishes a subtask only in part, so that it can be mapped to the subtasks
it delivers.

## Step 6 — GitHub issues: only when asked

**Do not create issues, and do not ask whether to.** Creating them is not needed. The plan
and the breakdown are revised often, through pull requests of their own, and issues copied
from them go stale at each revision. Mention in the final report, in one line, that the
issues can be created on request.

When the user explicitly asks for them, follow these steps. If the epic issue already exists
(Step 0 #4), add only the missing subtask issues to it, and never create a second epic issue.

1. **Labels.** The epic issue carries `epic`, the RFC's label (`rfc-<n>-<slug>`, for example
   `rfc-1-journal`) and the epic's label (`rfc-<n>-epic-<k>`). Subtask issues carry the
   last two, **not** `epic`. Check with `gh label list --search rfc-<n>`, and create a
   missing epic label in the existing style:
   `gh label create rfc-<n>-epic-<k> --color C5DEF5 --description "RFC <n> <component> v2, epic <k>: <title>"`.
2. **The epic issue.** Title `<component> v2, epic <k>: <title>` (as in
   `journal v2, epic 4: placement index and versions`). Body: the epic document. If it
   exceeds GitHub's 65,536-character limit, the body is §1, §3 and the estimate line, with a
   link to the document on `develop`.
   ```bash
   gh issue create --title "<component> v2, epic <k>: <title>" \
     --label epic,rfc-<n>-<slug>,rfc-<n>-epic-<k> --body-file <body.md>
   ```
3. **One issue per subtask.** Title `<component> v2, epic <k>.<n>: <title>`. Body: the
   subtask's §6 section, the rows of §3 it covers, and "Depends on #…" for the subtask
   issues it depends on. Like the epic, it stands alone and does not refer to the plan.
4. **Attach each subtask to the epic as a sub-issue**, not as a checklist in the epic's
   body. The API wants the subtask issue's numeric `id`, not its number:
   ```bash
   id=$(gh api repos/{owner}/{repo}/issues/<subtask-number> --jq .id)
   gh api -X POST repos/{owner}/{repo}/issues/<epic-number>/sub_issues -F sub_issue_id="$id"
   ```
5. **Report** the epic issue's link and the subtask numbers. Check the result with
   `gh api repos/{owner}/{repo}/issues/<epic-number>/sub_issues --jq '.[].title'`.

## Common rationalizations

| Rationalization | Reality |
| --- | --- |
| "The plan already lists the subtasks" | It lists candidates sized for a plan. Without the requirements next to them, nobody can tell which RFC obligation fell through. |
| "I'll pick out the important MUSTs" | The unimportant-looking ones are the ones nobody tests. Sweep mechanically, then map every hit. |
| "This MUST is obvious, it needs no test" | Then the test is cheap. A requirement without a test is a claim. |
| "The estimate is close enough to the plan's" | Then the script will say so. Compute it; don't copy the plan's figure. |
| "Let's create the issues now, it saves a step" | The plan and the breakdown will be revised, and issues copied from them go stale. Only when asked. |
| "This epic needs one pull request per subtask" | Subtasks are units of work, not of review. Group them by what reads well together. |
| "The old epic document is out of date, I'll replace it" | Someone may be working from it. Stop and ask. |

## Red flags

- A requirement table built by reading rather than by the sweep, or a raw hit that maps to
  nothing.
- A requirement with no subtask, or no test that can fail.
- A subtask over 5 man-days with no reason given.
- Subtasks cut to reach a count, or pull requests grouped by size alone rather than by what
  they deliver.
- An estimate total typed by hand, or a `~` in a number.
- The plan edited to record progress, or edited when the user did not ask.
- More than three suggested pull requests, or one that leaves the tree half-wired.
- The words task, ticket, story, step or phase naming this epic's units; ids like `E4-3`.
- Checkboxes, status fields, "TBD", or a reference to the plan in the document.
- Issues created, or offered with a question, without the user asking for them.

## Verification

Before handing over, check:

- Every raw hit of the sweep is mapped, and the R-tables carry `Kind`, `Source` and
  `Subtasks` on every row.
- Every R-number has a subtask and a test; every subtask covers an R-number or says why not.
- Every subtask has exactly one estimate line; the script's total matches the header's
  `**Epic <k> estimate: …**` line; the plan is touched only if the user asked for it.
- §8 suggests one to three pull requests whose subtasks cover every subtask once, and whose
  man-days add up to the total; each one names what it delivers.
- `rg -n -i -w 'tasks?|tickets?|stor(y|ies)|phases?|milestones?|work items?' <epic-doc>` finds
  nothing naming this epic's units.
- The document never refers to the plan, has no checkboxes or status, and its graphs are
  Mermaid.
- §2 holds the plan's general rules word for word, and §2.1 accounts for every rule.
- No GitHub issue was created unless the user asked, and then with the labels and sub-issue
  links above.

Then report the output path, the requirement count, the subtask count and the man-day total
(and the plan's figure for comparison), the suggested pull requests, the subtasks whose
estimate is least certain, and the open points, in a few lines. End with one line saying the
GitHub issues can be created on request.
