---
name: rfc-to-epic-plan
description: Turn a DittoFS RFC (docs/internals/rfcs/rfc-*.md) into an implementation plan split into epics and subtasks, each epic estimated in man-days, with blockers, synchronisation points and a split across the team, written to docs/internals/planning/. Use whenever an RFC is to be broken down, planned, split into epics or subtasks, or handed to the team ("plan RFC 12", "split rfc-8 into epics", "make the epic plan for the engine RFC"). Not for expanding a single epic of an existing plan into detailed subtasks (use epic-breakdown).
---

# From an RFC to an epic plan

The output is one Markdown plan that a small team can pick up and work in parallel.
The reference result is `docs/internals/planning/rfc-1-journal-plan.md`: read it once for
tone and depth. **Do not copy its content.** Its epics, packages and rules are about the
journal, and the next RFC has a different shape.

Naming used below: **v1** is the component that exists today, and **v2** is the one the
RFC describes. When the RFC builds something new, there is no v1 and every v1 clause
below is dropped.

## Terminology: epic and subtask, nothing else

The plan has two kinds of unit of work: an **epic**, and the **subtasks** it is split into.
No other word names them: not task, ticket, story, step, phase, stage, milestone, work
package or work item. That holds in headings, table columns, ids and prose. A subtask's id
is `<epic>.<n>` (`4.3`), never `E4-3` or `T3`: pull requests cite that form, and progress is
tracked by mapping pull requests to those ids.

Two other words keep their own meaning because they are not units of work. A **track** (A, B or C) is
one line of work through the epics. A **synchronisation point** (S1, S2, …) is where the
team converges. If the programme (Step 0 #8) names its own units, such as "stage 3", quote
them as the programme's, and never use that word for this plan's epics or subtasks. The
Steps of this skill are its procedure, not units of the plan.

## Step 0 — Gather the inputs, and stop if any required one is missing

Read the RFC's frontmatter first (`rfc`, `title`, `component`, `status`, `depends_on`).
Then fill the checklist below. Take an answer from the user's request or from the repo
where you can. Where you cannot, **ask** with `AskUserQuestion`, in one batch. **Do not
start planning while a required item is still open.** Defaults are only for the items
marked as having one, and the plan's header states each default that was used.

| # | Input | Required? | How to fill it |
| --- | --- | --- | --- |
| 1 | RFC path | **required** | from the request |
| 2 | Does the RFC replace an existing component? If so, v1's paths (packages, config keys, on-disk dirs) | **required** | propose candidates found with `rg`, then confirm with the user |
| 3 | Transition policy from v1 | **required if 2 = yes** | offer these options: offline switch refused while v1 holds uncommitted state / fresh installs only, no upgrade / migration tool / other |
| 4 | Related RFCs and how far each is in scope | **required** | propose the `depends_on` list, then ask, for each: *in scope*, *only the parts this RFC cannot work without*, or *constraints only* |
| 5 | Where v2 lives (package path and name) | default: proposed by you in §4 of the plan, flagged as an open point | |
| 6 | Tracks | fixed: three tracks named **A, B and C** (Step 2), with no owner | do not ask; who works which track is decided outside the plan |
| 7 | Target branch and external dependencies (PRs or other plans that must land first) | default: none known, listed as an open point | check `docs/internals/planning/` for plans this RFC depends on |
| 8 | The programme this RFC belongs to: an umbrella issue or plan that stages the work, assigns owners, and says which existing code is being replaced | **required** | ask for the link, then read its latest decisions; if there is none, say so in the header |
| 9 | Dashboards | default: **no**, unless the RFC asks for them | the RFC's named alerts are alert rules in the integration epic's exporters, not a dashboard epic |
| 10 | Output path | default: `docs/internals/planning/rfc-<n>-<slug>-plan.md` | |
| 11 | RFC `status: deferred` | **ask** | plan the whole RFC, or only the hooks the RFC says must exist before it is built? |

## Step 1 — Read

- Read the RFC in full, not just its headings.
- For each related RFC, read the sections that the in-scope answer from Step 0 covers.
- If there is a v1, read its package's exported surface and its callers. You need this to
  write the v1/v2 comparison and to scope the integration epic. You do **not** need it to
  design v2: v2 ignores v1 (rule 7).
- Read the existing plans in `docs/internals/planning/` that this RFC touches, and align
  with them. Where you diverge, say so as an open point.
- Read the programme from Step 0 #8: its stages, who owns each, and what each stage
  replaces. Never plan integration against code a later stage deletes.

## Step 2 — Carve the epics

### Fixed epics

The first two and the last two epics always have these roles, and a dashboards epic
follows them only when Step 0 #9 says so. Number them by position: with N middle epics, the
last two are N+3 and N+4.

| Role | Content |
| --- | --- |
| **E1 Interfaces** | The public surface only: the exported symbols with their exact signatures, the error set, `Stats`, and the injection points (options, a `Tracer`, a logger). Stub bodies. No behaviour, except where a type's encoding *is* the contract. An internal interface belongs here only if it has two consumers before any implementation exists (for example a storage seam that both an option and E2's crash-simulating fake consume). Every other internal package — codecs, index, state machines — is created by the middle epic that implements it, as concrete types, with its first caller (rule 12). |
| **E2 Red e2e suite and test foundations** | E2E tests for every use case and scenario that can be named: from the RFC, from the in-scope related RFCs, and from other sources (protocol specs, past issues, v1's own bug history). Every test starts red and is listed as a known failure. The small test components that everything else needs (fakes for clocks and faults, a reference model, a profiling harness, a known-failure list the runner reads) also go here. **A test lands with the epic that creates the packages it imports:** at S2 only E1's packages exist, so a test that needs a middle epic's package (an encoder, a platform layer) belongs to that epic and is listed there. Format checks in E2 write their bytes by hand from the RFC, which also makes them an oracle independent of the encoder. **The catalogue holds only what can run:** a scenario that needs a host nobody provisions is parked in §6, and a system-level scenario that a component-level fake already covers is dropped. |
| **Integration** | Wire v2 into the system as the programme (Step 0 #8) stages it, not into code a later stage replaces. Typical shape changes: one instance per share becomes one per disk; a value read once at start becomes reloadable; an owner-local decision becomes cluster-wide. Also: the transition policy from Step 0 #3, config keys, exporters for metrics and spans (and the RFC's alert rules), error mapping, API and CLI. **If it depends on an interface not yet agreed** (owned by another team or stage), add a synchronisation point for that agreement, keep in this epic only what is already decided, and put the rest in a following epic that is **planned after** that point and not estimated (Step 3). |
| **E2E green** | Burn down the known-failure list to empty. Also soak tests, benchmarks the RFC names, and CI registration. |
| **Dashboards** (optional) | Only if Step 0 #9 said yes: a small number of dashboards, each with an automated test and a consumer before release. |

### Middle epics

The middle epics implement the RFC. Cut them **along the RFC's own seams**: on-disk format,
index, data path, recovery, protocol state machine, API, controller loop, and so on. Each
one must be buildable and unit-testable on its own, under rule 1: against E1's public
surface, E2's test components, another epic's pure in-memory implementation, or the
implementation of an epic it depends on once that has landed.

**How many.** Start from the estimate `N = round(wc -l RFC / 400)`, clamped to 3..8, so
that each of the three tracks owns at least one. For RFC 1 (2747 lines) that gives 7 middle
epics. For RFC 30 (445 lines) it gives 3.

The estimate is a **sanity check, not a quota**:
1. First, list the RFC's cohesive work areas **without looking at N**. Map each one to the
   RFC sections it owns.
2. Merge areas that share most of their state, or split areas too big for one epic.
3. Only then compare the result with N. If they differ by more than one, write one line
   in §2 of the plan saying why. Never split a cohesive area just to reach N, and never
   merge unrelated areas just to stay under it.

The coverage table (§7 below) is the check. Two epics citing the same sections mean an
artificial split. A section with no epic means a gap.

**Three tracks.** The plan always splits the work into exactly three tracks, A, B and C,
named by letter only, with no owner. Every middle epic belongs to one track (a fixed epic may
be the team's). Assign epics so that each track is a coherent line of work — one area of
the RFC, its own packages, few hand-offs to the others — and busy from S2 to integration.
If the RFC's areas do not divide into three, split a track's area along a seam it already
has (for example the platform layer away from the codecs) rather than leave a track empty.

**Subtask size.** A subtask is one coherent piece of work for one engineer, landing with its
tests, ideally up to 5 man-days. It is **not** one pull request: the owner groups subtasks
into pull requests as they see fit, and the epic breakdown only suggests a grouping, with a
pull request ideally up to 20 man-days. Both figures are ceilings to aim under, not targets.
Cut subtasks, and group them, by what makes sense: one logical change, readable on its own,
following the dependencies, delivering a piece of functionality that can be named. There is
no target number of subtasks per epic; the count follows from the work. Give it as a
number; the detailed breakdown is written when the epic starts, with the `epic-breakdown`
skill.

**Man-days.** Estimate every epic in man-days of one engineer, in multiples of 0.25. This
is effort, not calendar time: no parallelism, no waiting on reviews or external
dependencies. It includes the tests, doc comments, review fixes and whatever the §1.5 rules
require. An epic that is "planned after Sx" gets no figure.

The plan estimates **epics, not subtasks**. Its subtasks are candidates, and the
per-subtask estimate belongs to the epic's breakdown, so this is a first, higher-level
figure. Reach it like this:

1. Place the epic's candidate subtasks roughly on the scale below; most land at 1–3
   man-days. Do not write per-subtask figures into the plan.
2. Add the work the candidate list does not name but the epic owns: its tests and fuzz
   targets, CI wiring, metrics, docs, the review or freeze step, per-platform variants.
   Size test-catalogue work by the number of checks the RFC names, not by the number of
   bullet families.
3. Weight the sum upward for crash or durability semantics, lock ordering, per-OS code,
   anything verified only with fault injection, and burn-down work whose size cannot be
   known in advance (estimate it, and name it in §6). Weight it downward for pure
   declaration work: signatures, stubs, types the RFC already states.

| Man-days | Typical work |
| --- | --- |
| 0.25 | a very small change: wording or a comment, a minor visual tweak, a one-line default |
| 0.5 | a type or constant set with tests; a doc table; a config key |
| 1 | a small codec, or one well-specified function with edge-case tests |
| 2 | a component with a few interacting rules, or about five conformance checks |
| 3 | concurrency, crash safety or on-disk ordering in one component |
| 5 | a subsystem's core algorithm under race or crash testing; the ceiling for one subtask |

The `epic-breakdown` skill uses the same scale, so a plan's figure and its epics' refined
figures can be compared.

## Step 3 — Write the plan

**The plan records decisions, not progress.** Nobody edits it to track how the work is
going. It is revised, through a pull request, only when the decisions change: an epic's
scope, a dependency, an estimate adjusted after an epic's breakdown, subtasks moving between
epics. So it has none of the following:

- status columns or progress markers;
- checkboxes;
- "to be updated when…" or "TBD" placeholders;
- tables of issue numbers that fill in later;
- decisions deferred to another thread ("location decided in #NNN"): settle them, or list
  them in §6 with the options.

The one exception is an epic that cannot be broken down yet because it waits on an
interface someone else owns: give it "planned after Sx" in place of an estimate (subtasks
and man-days alike), and name the synchronisation point and what it waits for.

§6 lists the open points as they stood when the plan was written. Each is resolved in an
epic or an issue. The plan is revised only if the answer changes the breakdown, not to
record that the point was settled.

Use this structure. Section numbers are fixed so plans can be compared.

```
# RFC <n> — <component> v2: implementation plan
Header: scope (the RFC + related RFCs with their in-scope level), tracks (A, B,
        C), the programme it belongs to, naming (v1/v2),
        defaults used from Step 0.

## 1. Introduction
### 1.1 What we are building    — v1 vs v2 table (if there is a v1); dependency limits
### 1.2 How the plan is organised
### 1.3 What epic 1 must deliver — concrete: which RFC sections' symbols, which seams
### 1.4 What epic 2 must deliver — the scenario catalogue by source, the known-failure
                                   mechanism, the test components
### 1.5 General rules for each epic — the rules below, adapted to this RFC

## 2. Epics at a glance          — table: # | Epic | Content | Subtasks | Man-days | Track | Blocks
                                   with a total row, then the plan estimate line (below),
                                   the line on N vs the estimate, and a legend
## 3. Synchronisation points, blockers and the split
### 3.1 Dependency graph        — Mermaid (see below)
### 3.2 Synchronisation points  — S1..Sk: what is true at each, and what it unblocks
### 3.3 Blockers                — epic → what it blocks, and why
### 3.4 Splitting the work      — per track (A, B, C): its path, plus where the team
                                   converges; no per-person subtask arithmetic, which goes
                                   stale at the first change
## 4. Directory layout proposed for epic 1 — a tree, each entry tagged with its epic
## 5. The epics in detail       — per epic, under `### Epic <n> — <title>`: the epic
                                   estimate line (below), a paragraph, the RFC sections
                                   owned, and the candidate subtasks with `<n>.<k>` ids
## 6. Risks and open points
## 7. RFC coverage              — table: RFC section → epic(s); related-RFC sections too
```

Mark blockers with ⛔ and synchronisation points with 🔀, in the headings and in the table.

### Estimate lines: one fixed form, so `grep` can read them

Each epic in §5 opens with exactly one line, on a line of its own, in one of these two forms:

```markdown
**Epic 4 estimate: 11 subtasks, 17.5 man-days.**
**Epic 11 estimate: planned after S4a.**
```

§2 carries the total, right under the table:

```markdown
**Plan estimate: 120 subtasks, 190 man-days.**
**Plan estimate: 108 subtasks, 172 man-days, not counting epic 11.**
```

Use the second form when some epic is "planned after", and list every such epic. Numbers
are plain: no `~`, no ranges, no `+ E11`, no unit inside the number. Compute the total
with this script, never by hand, and make the §2 table's total row agree with it:

```bash
grep -E '^\*\*Epic [0-9]+ estimate: [0-9]' <plan> |
  awk '{n+=$4; d+=$6} END{printf "%d subtasks, %g man-days\n", n, d}'
```

`grep -E '^\*\*(Epic [0-9]+|Plan) estimate:' <plan>` then lists the whole estimate.

### Names follow the transition policy

The `v2` suffix lives in Go package and import paths only. Every name a user sees —
metric and span names, log attribute keys, config keys, dashboard queries — says the
component's plain name. Whether a version is exposed at all follows Step 0 #3: when v1 and
v2 never run in the same binary (fresh installs only, or a cutover), there is no version
label, because it would always read `v2`.

### Graphs are Mermaid, never ASCII art

Every graph (dependencies, the per-track path, any state machine you need to explain)
is a fenced `mermaid` block. GitHub and Obsidian render these natively. In VS Code's
preview, use the *Markdown Preview Mermaid Support* extension. Conventions:

````markdown
```mermaid
flowchart TD
  E1["E1 Interfaces ⛔"]:::blocker --> S1(["S1 interfaces frozen"]):::sync
  S1 --> E2["E2 Red e2e suite ⛔"]:::blocker --> S2(["S2 red suite in CI"]):::sync
  subgraph A["Track A"]
    E3["E3 Wire format"] --> E6["E6 State machine"]
  end
  subgraph B["Track B"]
    E4["E4 Index"] --> E7["E7 Data path"]
  end
  subgraph C["Track C"]
    E5["E5 Platform layer"] --> E8["E8 Durability"]
  end
  S2 --> E3 & E4 & E5
  E6 -. "minimal stream" .-> E7
  E3 -. "codecs" .-> E5
  E6 & E7 & E8 --> E9["E9 Integration"]
  X(["S4a interface agreed with its owner"]):::sync --> E10["E10 Integration, rest 🔀"]:::sync
  E9 --> E10 --> E11["E11 E2E green"]
  classDef blocker stroke:#c0392b,stroke-width:3px
  classDef sync stroke:#2c5aa0,stroke-width:3px,stroke-dasharray:4 2
```
````

- Quote every label (`["..."]`). Parentheses, slashes and `§` otherwise break the parser.
- Style with stroke only, never `fill`. A light fill makes the text unreadable in a dark
  theme.
- The dependency graph in §3.1 has exactly **three** track `subgraph`s, A, B and C, labelled
  `Track A`, `Track B` and `Track C`, with no names. Epics that belong to the team (E1, E2, integration, E2E green)
  sit outside them. Show synchronisation points as stadium nodes `([...])`.
  Draw partial dependencies, such as "needs only the minimal X", as dotted edges with a
  label.
- The directory layout in §4 is a tree, not a graph, so it stays a `text` block.

### 1.5 General rules for each epic

Put these rules in §1.5. Make each one concrete for this RFC: name its parsers, its hot
paths, its counters, its v1 package. Drop a rule only by writing that it does not apply,
and why. Write each rule so that it stands alone once copied into an epic: no references
to sections of the plan ("§3.1", "§1.4"), only to the RFC, to `CLAUDE.md` or to other
rules by number.

Open §1.5 with this instruction, verbatim. It is written **into the plan itself**, so that
whoever later asks for an epic to be created from the plan, in any session, finds the rule to
use `epic-breakdown` in the document they are reading:

> **Instruction for whoever expands an epic into its detailed description and subtasks,
> human or AI:**
>
> - **Creating an epic from this plan: use the `epic-breakdown` skill.** Whenever the user
>   asks to create an epic from this plan, or to expand, detail or break one down, run the
>   `epic-breakdown` skill (`.claude/skills/epic-breakdown/SKILL.md`) on this plan and that
>   epic, rather than writing the epic document freehand. It extracts the RFC's MUST and
>   MUST NOT into numbered requirements, estimates each subtask in man-days and suggests
>   the pull requests. A human doing it by hand follows the same skill.
> - **Epic and subtask, nothing else.** The units of work are the epic and its subtasks,
>   with subtask ids `<epic>.<n>` (`4.3`). No other word names them: not task, ticket,
>   story, step or phase, in headings, tables, ids or prose.
> - **The epic stands alone.** Copy these rules into the epic document word for word,
>   under their own heading. The epic document must not refer to this plan, link to it,
>   or say "see the plan". Everything a reader needs to do the epic is in the epic
>   document itself: the RFC sections it implements, the interfaces it builds on, the
>   rules, and the subtasks.
> - **The epic document records decisions, not progress, like this plan.** It has no
>   status fields, no checkboxes to tick and no sections to keep up to date as the work
>   goes on. It is revised through a pull request only when a decision changes.
> - **Subtasks are not pull requests.** A subtask is ideally up to 5 man-days, a pull
>   request ideally up to 20. The owner groups subtasks into pull requests as they see
>   fit; the epic document only suggests a grouping. Cut and group by logic, readability,
>   dependencies and a deliverable that can be named, not to hit a number.
> - **Every subtask states how it meets each rule that applies to it.** A rule that does
>   not apply is named, with the reason.
> - **In the issue tracker, only when someone asks for issues:** they are not created by
>   default, because the plan and the breakdown are often revised and issues copied from
>   them go stale. When they are created, the epic is one issue labelled `epic`, whose body is the
>   epic document. Each subtask is its own issue, created as a sub-issue of the epic
>   issue rather than as a checklist in its body. Like the epic, a subtask issue stands
>   alone and does not refer to the plan.

1. **Independent unit tests.** Each epic carries its own unit tests. It may use another
   epic's pure in-memory implementation, the implementation of an epic it depends on once
   that has landed, and E2's test components. E1 declares no internal interface for it to
   fake (rule 12).
2. **Fuzz tests where relevant.** Use Go native fuzzing on every parser of input the
   component did not just write itself: on-disk records, wire messages, config, API
   payloads. Where E2 has a reference model, also fuzz operation sequences against it.
   The property: an input is either accepted as valid or rejected, never a panic.
3. **Metrics, and tracing where relevant.** Maintain every counter and gauge the RFC
   assigns to the epic's area, and expose it through `Stats`, served from maintained
   counters, never a walk. Spans go through E1's standard-library `Tracer` and logs
   through an injected `*slog.Logger`, never through a metrics or tracing library. There
   is no event callback: a condition worth an event is a `Stats` field, and a stream of
   them is a numbered ring a poller can tell it missed entries of. Add a callback only if
   the RFC asks for push delivery, and define what it carries. The exporters arrive in the
   integration epic. Name the operations worth a span.
4. **Profiling.** Tests and benchmarks run under E2's profiling harness, and fail on
   goroutine, descriptor or memory leaks and on anomalous resource growth. Name the hot
   paths that carry allocations-per-op assertions.
5. **Public interface and internals.** Tests cover both the internal package API and the
   public behaviour the epic enables, including concurrency under `-race`. Test at the
   layer that **consumes** a value (see `CLAUDE.md`, "Verifying a change").
6. **Known failures shrink.** An e2e test that turns green leaves the known-failure list
   in the same change. The runner fails if a listed test passes.
7. **v2 ignores v1.** No import of, copy from, or behavioural dependence on v1, enforced by
   an import test. The test checks every package on its own, so its allowlist names each
   package an exception applies to, test packages included. v1 is touched only as the
   transition policy says: at its callers in the integration epic, or not at all when v1
   is deleted at a cutover.
8. **Real dependencies for properties of those dependencies.** No fake stands in for a
   check about what the real thing does: a real disk for durability, a real S3 or
   Localstack for remote semantics, a real kernel client for protocol behaviour. Fakes are
   for pure logic only.
9. **Code comment conventions** (`CLAUDE.md`). No epic, issue or plan IDs in source. Use
   `ponytail:` and `decision:` markers. Where the RFC carries such a note, its marker goes
   at the code site that implements it.
10. **A test must be able to fail.** Keep a test only if a plausible change would make it
    fail and nothing else would catch that change: not the compiler, not the diff, not a
    later epic's tests. A review rule that no test can check, such as "stubs never panic",
    stays a review rule.
11. **A symbol is exported only if the RFC's interface uses it.** Everything exported is
    frozen at S1. Helpers the internal packages need stay internal, for example in an
    `internal/core` package.
12. **Each subtask creates only what it declares.** No empty package and no stub without a
    caller. A package, an option or a test helper lands with its first consumer.

## Step 4 — Check before handing over

- Every in-scope RFC section appears in §7, and every middle epic cites sections no other
  epic owns.
- Every ⛔ and 🔀 in §2 also appears in §3, and the Mermaid graph has the same edges as
  §3.3.
- There are exactly three tracks, A, B and C: three `subgraph`s in the §3.1 graph, three
  paths in §3.4, and every middle epic in §2 owned by one of them.
- Each track's path in §3.4 is busy from S2 to integration. Name any
  idle stretch, and say what fills it. Joint work is the team's, never "both engineers".
- E1 declares no internal interface with fewer than two consumers before implementation,
  and no package that a middle epic creates.
- No E2 test imports a package that does not exist at S2; each such test is listed in the
  epic that creates the package.
- The integration epic follows the programme's stages, and an epic waiting on someone
  else's interface says "planned after Sx" and names the point.
- There is a dashboards epic only if the RFC or the user asked for one.
- The §1.5 rules cite no section of the plan.
- Every Step 0 default and every divergence from an existing plan is listed in the header
  or in §6.
- No graph is in ASCII art.
- Units of work are called epics and subtasks everywhere, and subtask ids are `<epic>.<n>`:
  `rg -n -i -w 'tasks?|tickets?|stor(y|ies)|phases?|milestones?|work items?' <plan>` turns
  up nothing that names this plan's units.
- Every epic in §5 has exactly one estimate line in the fixed form, the script's total
  matches the `**Plan estimate: …**` line and the §2 total row, and every epic without a
  figure is "planned after Sx" and named in the plan estimate line.
- Nothing in the plan tracks progress: no status, no checkboxes, no placeholders.
- §1.5 opens with the verbatim instruction: epics are expanded with `epic-breakdown`, the
  rules are copied into each epic, the epic
  stands alone and records decisions rather than progress, subtasks are not pull requests,
  and issues, only when asked for, use an `epic` label with sub-issues.

Then report the output path, the epic count against the estimate, the plan estimate line
(subtasks and man-days), the epics whose man-days are least certain, and the open points,
in a few lines. End by saying that each epic is expanded with `epic-breakdown` when it
starts.
