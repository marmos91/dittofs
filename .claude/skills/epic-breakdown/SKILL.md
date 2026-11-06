---
name: epic-breakdown
description: Expand one epic of a DittoFS implementation plan (docs/internals/planning/rfc-*-plan.md) into its standalone epic document — every MUST and MUST NOT the RFC places on the epic numbered as R-requirements, ordered subtasks with acceptance criteria, tests and a man-day estimate each, a grep-able total, and a suggested, optional grouping into pull requests — written to docs/internals/planning/. Also says how to create the epic and its subtasks as GitHub issues that point at the document, in the "DittoFS" project with their estimates and under the RFC's roadmap epic, done only when explicitly asked and once the document has merged. Use whenever an epic is to be broken down, detailed, expanded into subtasks or made ready to start ("break down epic 5 of the journal plan", "expand rfc-1 epic 7", "/epic-breakdown 1 8"). Not for planning a whole RFC (rfc-to-epic-plan).
---

# Epic breakdown

Adapted from Addy Osmani's `planning-and-task-breakdown` skill (agent-skills, MIT licence)
for DittoFS's RFCs, plans and epics.

## Overview

Expand one epic of a plan into an epic document a single owner can work from, without
opening the plan or the RFC to find out what to build. The document has three parts that
the plan does not:

1. **The requirements.** Every MUST and MUST NOT that the RFC places on the epic, numbered
   R1…Rn, so that subtasks and tests can cite them and nothing is silently dropped.
2. **The subtasks.** Small, ordered and verifiable, each with acceptance criteria, its tests,
   the requirements it covers and a man-day estimate.
3. **The pull requests.** An optional suggestion of how the subtasks group into pull
   requests; one pull request per subtask works as well.

The reference result is [`example-epic.md`](example-epic.md), beside this file: an epic of
an invented RFC that follows every rule below, with its requirement tables, estimate lines
and suggested pull requests. Read it once for shape and depth. **Do not copy its content.**

**Do not write code while breaking down.** Read the RFC and the code you need to understand,
and write the document. That is the whole output.

## Terminology: epic and subtask, nothing else

The vocabulary is the one under "Terminology: epic and subtask, nothing else" in
`.claude/skills/rfc-to-epic-plan/SKILL.md`: read it there. The epic document adds `D` for its
decisions, and its subtask headings (`### T4.3 …`) and estimate lines
(`**Subtask T4.3 estimate: …**`) always carry the `T`, because the checks parse them.
Progress is tracked by mapping pull requests to subtask ids, so they must stay stable:

- **Keep the plan's ids.** Where the plan's candidate list numbers a subtask, keep its id
  for the same work. New subtasks get the next free numbers, after the plan's.
- **Split with a letter suffix, never by renumbering.** A subtask split after its id may
  have been cited becomes `T4.3a`, `T4.3b`; work mapped to `T4.3` then covers both parts.
- **Never reuse or drop an id that a pull request has cited.** Search both forms first, with
  the repository named as Step 6.1 says (`R=$(gh repo view --json nameWithOwner -q
  .nameWithOwner)`; without `-R` the search can return nothing, with no error):
  `gh pr list -R "$R" --state all --search '"T<epic>.<n>" in:title,body'` and
  `'"<epic>.<n>" in:title,body'`, since pull requests cite ids in their descriptions too,
  and discard the hits that only cite an RFC's §<epic>.<n>. If the work moved, say where
  in §9.

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
| 3 | Track | A, B, C or the team, from the plan's §2 (Step 1 has the fallback for an older plan); never ask for names |
| 4 | Existing epic issue | `gh issue list -R "$R" --state all --label epic --label rfc-<n>-epic-<k>`, with `-R` as Step 6.1 says (both labels must match; a text search for "epic <k>" also returns other epics). If one exists, note its number; when issues are created (Step 6), there must not be a second one |

Ask for whatever is missing with `AskUserQuestion`, in one batch, before reading further.
If you cannot ask (for example when running as a subagent), stop and report the missing
inputs rather than guess.

## Step 1 — Read

- The plan: the epic's row in §2, its section in §5, its blockers and synchronisation points
  in §3, the general rules in §1.5, and every §7 coverage row that names the epic. That
  gives the **owned sections**: the RFC sections, and the related-RFC sections, the epic is
  answerable for.
- **A plan written before these conventions** may lack some of that. Fall back, and say in
  the report which fallback was used:
  - no §7 coverage table → the sections cited in the epic's §5 section and its §2 row are
    only a starting point: a plan without §7 cites a section's parent and leaves out the
    sections it names in passing. Widen the list with the sections those cite, the RFC's
    requirement and test rows that name the epic's area, the RFC headings (`rg -n '^#'
    <rfc>`) matching the §5 bullets, and the **Sources** header of an existing document for
    this epic. Show the widened list to the user and have it confirmed before Step 2; it
    is then the owned sections for every later step.
    When nobody can confirm it, keep the widened list and name it in the report as an
    assumption;
  - no `**Epic <k> estimate:**` line → report "no plan figure" instead of a comparison;
  - no Track column → the track named for the epic in §3.4 or in the §2 row (a letter
    only), else "the team".
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

**2.1 Read each owned section for its obligations.** Read the owned sections in full and
list every statement that binds this epic's code. The markers below are what the RFC's
author uses to flag a candidate, not proof of an obligation: decide each one from what the
sentence says (a key word can sit in a heading, an example or a negation such as "is not
required", and a rule can carry no key word at all). Look for:

- the RFC 2119 key words (MUST, MUST NOT, SHALL, REQUIRED, SHOULD, SHOULD NOT), in capitals
  or in lower case: some RFCs write them only in lower case;
- **bold lead sentences**, which the RFCs use for rules even with no key word
  ("**Records, once written, are immutable.**"); each one is a rule;
- invariants stated in prose (`never`, `always`, `at most`, `at least`, `only`), when they
  constrain the code rather than describe it (`append-only` and "the only copy" usually
  describe);
- the numbers the sections put on the epic: cost bounds (`O(log n)`), limits, sizes and
  performance targets in tables, and the RFC's test rows (a §9-style table) that name this
  area.

Note each candidate with its RFC line number in a scratch file, not in the document.

**2.2 Turn the candidates into requirements.**

- One requirement per obligation. Merge sentences that state the same obligation in two
  places, and cite both sections. Split a sentence that carries two independent ones.
- Restate each one as a checkable sentence in the RFC's terms, and keep the polarity: a MUST
  NOT stays a prohibition ("never…", "is refused…").
- **Name only what the sources name.** Every identifier a row gives (a type, an error, a
  function, a config key, a metric) must appear in a section its `Source` cites. A name
  taken from somewhere else (another RFC, an earlier epic's document, the plan) adds that
  place to `Source`; a name found in none of them is replaced by the RFC's own words, since
  a requirement must not invent the API it holds the code to.
- A prose `never` / `always` becomes a requirement only if it constrains this epic's code. A
  sentence describing another component, or giving background, does not.
- A SHOULD becomes a requirement only if the epic adopts it. Otherwise record why not in §4
  (decisions).
- An obligation the RFC leaves open (it states the rule but not one case of it) stays a
  requirement, and names the subtask that decides it and records a `decision:` marker.
- An obligation in an owned section that another epic carries out: write the part that is
  this epic's, and name the epic that owns the rest ("the index offers the predicate; epic 9
  owns the hold rule"). Do not list obligations that belong wholly to another epic.

**2.3 Number and tabulate.** In a new document, number them R1…Rn in table order, grouped
under short bold headings by theme (structure, queries and cost, concurrency, limits, recovery, …). One table
per group:

```markdown
| # | Requirement | Kind | Source | Subtasks |
| --- | --- | --- | --- | --- |
| R1 | A file's held extents are disjoint. A superseding write splits or narrows, and never leaves two extents covering one offset | MUST | RFC 1 §4 | T4.2 |
| R13 | Covering extent of an offset, or its absence, in O(log n) | bound | §4.1 | T4.1, T4.2 |
```

`Kind` is one of `MUST`, `MUST NOT`, `SHOULD` (adopted), `invariant` (prose `never` /
`always`), `bound` (cost, size or performance target) or `test row` (a row of the RFC's own
test tables). `Source` cites the section, with the RFC named when the epic has more than
one source. Once the document exists, numbers are stable: a requirement added later takes
the next free number and goes in its group's table, so a table may read `R12, R47, R13`.
A number is never reused or renumbered, and a dropped requirement keeps its row, marked
"withdrawn" with the reason.

**2.4 Check coverage, both ways.**

- Every candidate noted in 2.1 maps to an R-number, to another epic, or to "not an
  obligation" (background, an example, a negation). Go through the scratch list one by one;
  an unmapped candidate is a gap.
- Every R-number has at least one subtask and at least one test that can fail if the
  requirement is broken.
- Every subtask covers at least one R-number, or says why it covers none (scaffolding that
  a later subtask's requirement needs).

## Step 3 — Carve the subtasks

**Slice vertically where the epic allows it.** Build one operation or one path end to end,
with its tests, rather than all the types, then all the methods, then all the tests:

```
Bad:  T4.1 all types   T4.2 all insert paths   T4.3 all queries   T4.4 the tests
Good: T4.1 B-tree container with its property test
      T4.2 held-extent insert and point lookup, tested at 10⁶ extents
      T4.3 span query with exact gaps, tested against an oracle
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

Estimate each subtask with the scale, the weighting and the number rules under "Estimating
in man-days" in `.claude/skills/rfc-to-epic-plan/SKILL.md`. That section is the only copy:
read it there rather than restating it here, so the plan's figures and these stay on one
scale.

Each subtask carries exactly one estimate line, on its own line, in this form:

```markdown
**Subtask T4.2 estimate: 2.5 man-days.**
```

The epic total goes in the document header, in the same form the plan uses:

```markdown
**Epic 4 estimate: 12 subtasks, 21.5 man-days.**
```

Numbers are plain multiples of 0.25: no `~`, no ranges, no comma, no unit inside the
number. Compute the total with the check beside this file, never by hand:

```bash
awk -f .claude/skills/epic-breakdown/epic-check.awk <epic-doc>
```

Only the `TOTAL` line may print; write it into the header. Anything else is a defect to fix:
a malformed line (`2,5`, `~2`, `0.3`, an indented or bulleted line, a missing word) is
reported rather than summed; a `### T<k>.<n>` subtask heading in §6 without exactly one
estimate line, an estimate line without a heading or under another subtask's heading, a
subtask id of another epic, a duplicate heading and a second epic line are reported, so a
deleted or misplaced line cannot go unnoticed; and a header line that no longer matches
the sum is reported.

`grep -E '^\*\*(Epic [0-9]+|Subtask T[0-9]+\.[0-9]+[a-z]?) estimate:' <epic-doc>` lists the whole estimate.

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
Header, one field per line: **Sources:** the RFC sections owned, with links.
        **Track:** A, B, C or the team.
        **Epic <k> estimate: <n> subtasks, <d> man-days.**   ← alone on its line, so a
                                                               grep anchored on ^ finds it
        **Depends on:** synchronisation points and epics, naming what each provides.
        **Blocks:** epics, and what they need.

## 1. Goal                     — what exists at the end; "at the end of the epic" bullets
### 1.1 In scope
### 1.2 Out of scope           — and which epic owns each item
### 1.3 Names used here        — a legend, one line each, of the plan's names the document
                                 and its copied rules use: every `E<n>` and `S<n>`, and
                                 role names such as "the integration epic", with what each
                                 is (from the plan's §2 and §3.2)
## 2. General rules for each epic — the plan's numbered §1.5 rules, copied word for word;
                                    not the instruction that opens §1.5, which speaks
                                    of "this plan"
### 2.1 How the rules apply    — table: rule | subtasks | how; a rule that does not apply
                                 is named, with the reason
## 3. Requirements and invariants — the R-tables from Step 2
## 4. Decisions this epic takes   — what the RFC leaves open, the choice and why; each one
                                    names the `decision:` or `ponytail:` marker it leaves
## 5. Target tree               — `text` block, each entry tagged with its subtask
## 6. Subtasks                  — one `### T<k>.<n> <title>` per subtask (below)
## 7. Order of work             — Mermaid graph of subtask dependencies, and checkpoints
## 8. Suggested pull requests   — Step 5.1
## 9. Open points
```

Each subtask in §6:

```markdown
### T4.2 Held-extent set for one file

**Subtask T4.2 estimate: 2.5 man-days.**

- **Track:** B. **Depends on:** T4.1.
- **Covers:** R1–R6, R13–R16.
- **Deliverables:** the types, functions and files it adds, with signatures where they are
  a contract.
- **Acceptance criteria:** specific, checkable conditions, in terms of the requirements.
- **Tests:** each test, and the bug it catches (a test that nothing plausible would make
  fail is not listed).
- **Rules:** how it meets each general rule that applies, unless §2.1 already says it.
```

Graphs are Mermaid, never ASCII art, styled with stroke only and quoted labels, as the
plan skill describes. Checkpoints are stadium nodes (`(["after T4.3: index API usable"])`).

### 5.1 Suggested pull requests (§8)

Suggest a grouping of the subtasks into pull requests, following "Subtasks and pull
requests" in `.claude/skills/rfc-to-epic-plan/SKILL.md`. It is a suggestion, not an
obligation: the owner may follow it, regroup, or open one pull request per subtask, and the
subtasks stay the unit that is estimated and tracked. Say so in one line under the table.

- **Size:** a suggested pull request is ideally up to 15 man-days, a ceiling to aim under
  rather than a target. The number of pull requests follows from that and from the
  groupings below.
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
| 1 | T4.1, T4.2, T4.4 | 7 | — | the container and counters every later subtask builds on |
| 2 | T4.3, T4.5–T4.7 | 8.5 | PR 1 | the file map, markers and floor buckets share the per-file lock |
| 3 | T4.8–T4.12 | 6 | PR 2 | bounds, accounting and rebuild, which read everything above |

A suggestion only: one pull request per subtask works as well.
```

Compute each pull request's man-days from the subtask lines, for example
`grep -E '^\*\*Subtask T4\.(1|2|4)[a-z]? estimate:' <epic-doc> | awk '{d+=$4} END{print d}'`
(the `[a-z]?` keeps split subtasks such as `T4.2a` and `T4.2b` in the sum). The
pull requests' man-days add up to the epic total. Each pull request's description names the
RFC and every subtask it delivers, one id per mention (`RFC 1, T4.1, T4.2`),
and says when it finishes a subtask only in part, so that it can be mapped to the subtasks
it delivers.

## Step 6 — GitHub issues: only when asked, as pointers to the documents

**Do not create issues unasked.** The user asks for this step when the team is ready to
assign work. The breakdown's report only reminds the user of it (see the end of
Verification).

This step is the **one description** of the RFC's issues: roadmap epics, epic issues and
subtask issues. `rfc-to-epic-plan` sends its issue requests here. Issues never copy the
specification: a body is a pointer to a section of the plan or of the epic document, with
a line or two of context, so revising a document never leaves an issue saying something
else.

Three requests reach this step:

- **a roadmap epic** for an RFC and a release (6.2);
- **epic issues for a merged plan**, before the epic documents exist (6.3). E2 needs them:
  each red test's `known.yaml` entry names the issue of the epic that turns it green, so
  every epic of the plan has its issue before E2's pull request lands;
- **an epic's issues once its document has merged**: the epic issue (6.3) and one issue per
  subtask (6.4).

### 6.1 Before creating anything

Do all of these first. Each one stops the step rather than letting it create half the issues.

1. **Name the repository on every call.** `gh issue list`, `gh pr list --search` and
   `gh label list` build their query from the local remote, and a clone whose `origin` still
   names an old repository gets `[]` back with no error. Resolve it once and pass it
   everywhere:
   ```bash
   R=$(gh repo view --json nameWithOwner -q .nameWithOwner)
   gh issue list -R "$R" --state all --label roadmap --json number,title,milestone
   ```
   `gh api` calls use `repos/$R/…`. **An empty lookup is not evidence**: that last command
   must list the roadmap epics the programme already has. If it prints `[]`, stop and report
   it rather than conclude that nothing exists.
2. **Check the token's `project` scope** with `gh auth status`. If `project` is missing, stop
   and ask the user to run `gh auth refresh -s project`, before any issue exists.
3. **Find what exists, before deciding what to create:** the RFC's roadmap epics (6.2), the
   epic issue (`gh issue list -R "$R" --state all --label epic --label rfc-<n>-epic-<k>`;
   both labels must match, since a text search for "epic <k>" returns other epics), and its
   sub-issues (6.4).
4. **Labels.** The epic issue carries `epic`, the RFC's label (`rfc-<n>-<slug>`, the slug
   being the RFC file's, `rfc-17-vfs` for `rfc-17-vfs.md`) and the epic's label
   (`rfc-<n>-epic-<k>`). Subtask issues carry the last two, **not** `epic`. A roadmap epic
   carries `roadmap` and the RFC's label, never `epic` or an epic's label. Check with
   `gh label list -R "$R" --search rfc-<n>` and create what is missing:
   ```bash
   gh label create -R "$R" rfc-<n>-<slug> --color 5319E7 --description "RFC <n> <title>"
   gh label create -R "$R" rfc-<n>-epic-<k> --color C5DEF5 --description "RFC <n> <component> v2, epic <k>: <title>"
   ```
5. **Every link resolves.** Link a document on a branch that holds it, which is not always
   `develop` (a programme branch such as `feat/storage-v2` holds documents `develop` does not
   have yet). Check each file before writing its link:
   `gh api "repos/$R/contents/<path>?ref=<branch>" --silent`. A 404 means linking another
   branch, never writing a dead link. Issues pointing at an epic document are created only
   **once its pull request has merged**, never during the breakdown.

### 6.2 The roadmap epic: the only place a release is chosen

A **roadmap epic** says what of one RFC ships in one milestone. It collects the RFC's epic
issues for that release as sub-issues, and it is the milestone's default for the RFC: a unit
of the RFC with no epic issue of its own ships with it, and the burnup reads it that way. An
RFC normally has one; an RFC split across releases has one per release. **It exists before
any epic issue of the RFC**, because epic issues are created under it. Epic issues never
choose a milestone: they copy their roadmap epic's, and their subtask issues copy the same
one. No document names a release.

**Finding it**, for an epic issue: `gh issue list -R "$R" --state all --label roadmap --label
rfc-<n>-<slug> --json number,title,state,milestone`. One open → use it, without asking.
Several (an RFC split across releases) → **ask the user** which, since that is the decision
of when the epic ships. None → **stop** and ask the user for one first; do not pick a
milestone instead.

**Creating it**, only when the user asks for a roadmap epic. Look first with the same
command: if one exists for that milestone, use it and create nothing, and if it is closed,
ask before reopening it. A second one for one RFC and milestone would make the RFC read as
split across releases.

- Title `roadmap <milestone>: RFC <n> — <RFC title>`, the title as the RFC's `#` heading
  writes it (`roadmap v1.0: RFC 17 — the filesystem service (VFS)`). One RFC per roadmap
  epic.
- The milestone: ask the user, never guess.
- Body, and nothing more:
  ```markdown
  Roadmap epic: what of RFC <n> — <title> ships in <milestone>.

  **In this release:** the whole RFC. | the whole RFC except epics <k>, …, which ship in <later milestone> (#<roadmap>). | epics <k> (<title>), …; the rest of the RFC ships in <milestone> (#<roadmap>).
  **RFC:** [`rfc-<n>-<slug>.md`](https://github.com/<R>/blob/<branch>/docs/internals/rfcs/rfc-<n>-<slug>.md)
  **Plan:** [`<plan>.md`](https://github.com/<R>/blob/<branch>/docs/internals/planning/<plan>.md) | none yet.

  The RFC's epic issues for this release are sub-issues of this one, and each carries this milestone. A unit of the RFC with no epic issue of its own ships with this roadmap epic. The plan and the epic documents say what to build; the milestone is the only place that says when.
  ```
- Add it to the project (6.5) with no `Estimate`.

### 6.3 The epic issue

One per epic, never two: if 6.1 found one, update it rather than create. Title
`<component> v2, epic <k>: <title>` (as in `journal v2, epic 4: placement index and
versions`). Body, and nothing more, in one of two forms.

**Before the epic document exists** (a merged plan, 6.3 for every epic that has no issue):
```markdown
Epic <k> of <component> v2 (RFC <n>): <the epic's goal, in one sentence>.

**Spec:** [Epic <k> — <title>](<plan link>#<heading anchor>). No epic document yet.
**Depends on:** epics <j> and <k> (from the plan's §3.3). **Blocks:** epics <m>, ….
```

**Once the epic document has merged:**
```markdown
Epic <k> of <component> v2 (RFC <n>): <§1's goal, in one sentence>.

**Specification:** [Epic <k>: <title>](https://github.com/<R>/blob/<branch>/docs/internals/planning/<file>.md)
**Sources:** RFC <n> §…; **Track:** <A|B|C>.
**Depends on:** epics <j> and <k> (or a sync point, as `S2`, or "every earlier epic").

The document is the specification: requirements, subtasks, acceptance criteria and
tests live there, not here. Each subtask is a sub-issue.
```

An existing epic issue whose body is still the first form, or holds no more than a line or
two of context, is rewritten to the second form when its document lands. Lines that place
it in the programme (`Part of #3119, stage 3.`, `Plan: #3546.`) are kept. A body someone
has written into is left as it is, and reported.

```bash
gh issue create -R "$R" --title "<component> v2, epic <k>: <title>" \
  --label epic,rfc-<n>-<slug>,rfc-<n>-epic-<k> --milestone <roadmap's milestone> --body-file <body.md>
```

Then attach it under its roadmap epic as a sub-issue (6.5).

### 6.4 One issue per subtask

**Match the existing sub-issues first** (`--paginate`, since the endpoint returns 30 per
page):
```bash
gh api --paginate "repos/$R/issues/<epic-number>/sub_issues" --jq '.[] | "\(.number) \(.state) \(.title)"'
```
A sub-issue matches a subtask by its id, in any of the three title forms in use, each with
its boundary: `subtask T6.1:` (this skill's), `epic 6.1:` (issues from before subtask ids
carried the `T`, as `journal v2, epic 4.1: …`) and `(T6.1)` (as `engine v2, epic 6: the
store's lifecycle view (T6.1)`):
```text
(subtask T6\.1:|epic 6\.1:|\(T6\.1\))
```
The boundary matters: a bare `epic 4.1` also matches 4.10 to 4.19. **If the epic has
sub-issues but none matches any subtask, stop and report the titles**: a fourth title form is
more likely than an epic whose every issue is missing. Create issues only for the subtasks
with no match. A subtask issue whose id the document no longer has (a withdrawn subtask, or
a split subtask's old issue) is obsolete and closed, as 6.6 says. Sub-issues that did not
come from the epic document (bugs, follow-ups) are not its subject: they are the team's.
A matched older issue keeps its title form;
if its body copies the specification (`Covers:`, `Done when`, a checklist), it is rewritten
to the pointer form below.

Title `<component> v2, subtask T<k>.<n>: <title>`. Body:
```markdown
Subtask T<k>.<n> of epic <k> (#<epic-number>): <the subtask's purpose, in one sentence>.

**Specification:** [T<k>.<n> <title>](<document link>#<heading anchor>)
**Depends on:** #<subtask-number>, … (or the sync point, as `S2`).
```
The anchor is GitHub's for the `### T<k>.<n> <title>` heading: lower case, dots and
punctuation other than hyphens dropped, spaces as hyphens (`### T3.1 Usage table` →
`#t31-usage-table`). Open one link to check it. Requirements, acceptance criteria, tests and
the estimate are not copied: the document holds the first three, the project's field the
last. Create it with the milestone of the roadmap epic the epic sits under, also when the
subtasks come long after the epic issue.

### 6.5 Sub-issues and the project

- **Sub-issue links**, not checklists in a body: a subtask under its epic, an epic under its
  roadmap epic. The API wants the child's numeric `id`, not its number:
  ```bash
  id=$(gh api "repos/$R/issues/<child-number>" --jq .id)
  gh api -X POST "repos/$R/issues/<parent-number>/sub_issues" -F sub_issue_id="$id"
  ```
- **Every issue joins the "DittoFS" project** (`marmos91`, project 3). Only subtask issues
  get `Estimate`, their man-days from §6: a figure on the epic or the roadmap epic too would
  count it twice in the project's sums. Look the ids up rather than hard-coding them:
  ```bash
  project=$(gh project view 3 --owner marmos91 --format json --jq .id)
  field=$(gh project field-list 3 --owner marmos91 --format json \
    --jq '.fields[] | select(.name == "Estimate") | .id')
  item=$(gh project item-add 3 --owner marmos91 --url <issue-url> --format json --jq .id)
  gh project item-edit --project-id "$project" --id "$item" --field-id "$field" --number <man-days>
  ```

### 6.6 When a document is revised, or an epic changes release

- The issues follow a revision **after its pull request has merged**, on request, never in
  the same pass: until then the links still show the old document. The sync covers a renamed
  subtask's title, a changed estimate's field, a new subtask's issue, a withdrawn subtask's
  issue closed as not planned, a split subtask (T4.3 into T4.3a and T4.3b: new issues, the
  old one closed), and a `**Depends on:**` line the revision changed. That line is the one
  part of a body that copies the documents, kept because release tracking reads it: an epic
  whose milestone is due before that of an epic it depends on is a conflict to report.
  Otherwise bodies need no change, because they say nothing the document can contradict.
- An epic changes release only by moving under the other release's roadmap epic (the user
  creates it if the RFC has none there yet); then update the milestone copy on the epic
  issue and its open sub-issues. Closed ones keep the release they shipped in.

### 6.7 Report

The issue links, each epic's roadmap epic, the subtask numbers, any body left as it was,
and any epic placed in an earlier release than an epic it depends on. Check the result with
`gh api --paginate "repos/$R/issues/<epic-number>/sub_issues" --jq '.[].title'`.

## Common rationalizations

| Rationalization | Reality |
| --- | --- |
| "The plan already lists the subtasks" | It lists candidates sized for a plan. Without the requirements next to them, nobody can tell which RFC obligation fell through. |
| "I'll pick out the important MUSTs" | The unimportant-looking ones are the ones nobody tests. Read every owned section in full, note every candidate, and map each one. |
| "This MUST is obvious, it needs no test" | Then the test is cheap. A requirement without a test is a claim. |
| "The estimate is close enough to the plan's" | Then the script will say so. Compute it; don't copy the plan's figure. |
| "Let's create the issues now, it saves a step" | They are created by hand when the team is ready to assign the subtasks, and only on request; their links point at the merged document, which does not exist until its pull request merges. |
| "The issue body could carry the acceptance criteria, it saves a click" | A copy goes stale at the first revision. The body points at the section; the document is the specification. |
| "The old epic document is out of date, I'll replace it" | Someone may be working from it. Stop and ask. |

## Verification

Before handing over, check:

- Every candidate of 2.1 (key words, bold lead sentences, invariants, bounds) is mapped,
  and the R-tables carry `Kind`, `Source` and `Subtasks` on every row.
- Every R-number has a subtask and a test; every subtask covers an R-number or says why not.
- Every identifier in an R row is read back in the sections its `Source` cites: list the
  row's backticked names, and `rg -n -w '<name>'` each one in the cited documents, keeping
  only the hits inside the cited sections' line ranges. A name with no hit there is a
  defect to fix before handing over, not to report: add the place it really comes from to
  `Source`, or rewrite the row in the RFC's words. This holds for rows this run did not
  write, too, when revising an existing document.
- No subtask is over 5 man-days without a reason, and none was cut to reach a count.
- `epic-check.awk` prints only its `TOTAL` line, and the header's `**Epic <k> estimate: …**`
  line says the same; the plan is touched only if the user asked for it.
- §8's suggested pull requests cover every subtask once, their man-days add up to the
  total, each names what it delivers and leaves nothing half-wired, and the table says the
  grouping is only a suggestion.
- The epic's units are called epics and subtasks: no other word (task, story, phase,
  stage, …) names them. Read the document for it; a word search only suggests where to look,
  since phase, stage and plan are also ordinary words and terms of some RFCs.
- The document never refers to the plan, has no checkboxes or status, and its graphs are
  Mermaid. `rg -n -i "\b(this|the) plan\b|\bplan's\b" <epic-doc>` shows where to look; read
  each hit, since some RFCs use "plan" as a term of their own. A copied rule that refers to
  the plan is a defect of the plan: copy it word for word, and report it so the plan is
  fixed.
- §2 holds the plan's general rules word for word, and §2.1 accounts for every rule.
- §1.3 explains every `E<n>` and `S<n>` the document uses, the copied rules included. This
  prints the ones missing from it, and must print nothing. A token that names something
  else, such as the S3 storage service, gets its own line in §1.3 saying so:

  ```bash
  rg -N -o -w 'E[0-9]+|S[0-9]+[a-z]?' <epic-doc> | sort -u |
    comm -23 - <(awk '/^### 1\.3 /{f=1; next} /^#/{f=0} f' <epic-doc> |
                 rg -N -o -w 'E[0-9]+|S[0-9]+[a-z]?' | sort -u)
  ```
- No GitHub issue was created or synced unless the user asked, and none pointing at the
  epic document before it merged. Every lookup named the repository with `-R`, the roadmap
  lookup returned the known roadmap epics, and the `project` scope was checked, all before
  the first issue was created. An existing epic issue whose body was still the plan's
  pointer now points at the epic document and keeps its `Part of`/`Plan:` lines; its
  sub-issues were matched by id in all three title forms before any was created, and a
  subtask issue whose id the document no longer has was closed. Issues that were created carry
  the labels, a pointer body with no copied specification, the sub-issue links (subtask under
  epic, epic under its RFC's roadmap epic), the project, the roadmap epic's milestone and, on
  subtasks only, the `Estimate` field.

Then report the output path, the requirement count, the subtask count and the man-day total
(and the plan's figure for comparison, or "no plan figure"), any Step 1 fallback used,
the suggested pull requests, the subtasks whose
estimate is least certain, and the open points, in a few lines. End with one line
reminding the user to create the GitHub issues with Step 6 once the epic document is
approved and its pull request has merged; for a revision of an epic that already has
issues, the line reminds them instead that the issues need syncing once the revision
merges. The line names Step 6 as the next step for the user to ask for; it does not offer
to run it now.
