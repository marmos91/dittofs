---
phase: 14-migration-tool-a5
plan: 07
type: execute
wave: 7
depends_on: [14-06-status-rest]
files_modified:
  - docs/BLOCKSTORE_MIGRATION.md
  - docs/ARCHITECTURE.md
  - docs/IMPLEMENTING_STORES.md
  - docs/FAQ.md
  - docs/CLI.md
autonomous: false
requirements: [MIG-01, MIG-02, MIG-03, MIG-04]
tags: [docs, runbook, transcripts, operator_guide]
must_haves:
  truths:
    - "docs/BLOCKSTORE_MIGRATION.md is the primary operator runbook with pre-flight checklist, step-by-step procedure, bandwidth tuning advice, common failure modes, and recovery procedures (D-A17)"
    - "BLOCKSTORE_MIGRATION.md includes four worked transcripts: (1) ~10 GB share happy path, (2) TB-scale share with --parallel + --bandwidth-limit tuning, (3) crash mid-migration + auto-resume, (4) integrity-check failure + manual diagnosis + re-run (D-A19)"
    - "docs/ARCHITECTURE.md gains a new section explaining the dual-read shim + per-share block_layout flag + migration tool boundary (D-A18)"
    - "docs/IMPLEMENTING_STORES.md notes that new metadata backends MUST persist block_layout on the share record + lists the storetest conformance scenario (D-A18)"
    - "docs/FAQ.md gains a 'How do I migrate from v0.13/v0.14 to v0.15?' entry pointing at BLOCKSTORE_MIGRATION.md (D-A18)"
    - "docs/CLI.md is regenerated to include `dfsctl blockstore migrate` and `dfsctl blockstore migrate status` reference, OR an explicit note that CLI.md is auto-generated and the regeneration command was run (D-A17)"
    - "Human checkpoint at the end of this plan — reviewer reads BLOCKSTORE_MIGRATION.md end-to-end and confirms the four transcripts are copy-pasteable and the procedure is clear"
  artifacts:
    - path: docs/BLOCKSTORE_MIGRATION.md
      provides: "Operator runbook"
      min_lines: 400
    - path: docs/ARCHITECTURE.md
      provides: "Updated architecture doc with dual-read + block_layout section"
      contains: "block_layout"
    - path: docs/IMPLEMENTING_STORES.md
      provides: "Schema note for block_layout"
      contains: "block_layout"
    - path: docs/FAQ.md
      provides: "v0.13→v0.15 migration entry"
      contains: "v0.15"
  key_links:
    - from: docs/BLOCKSTORE_MIGRATION.md
      to: docs/ARCHITECTURE.md
      via: "cross-link explaining the dual-read shim invariant"
      pattern: "ARCHITECTURE.md"
---

<objective>
Land the operator-facing documentation: BLOCKSTORE_MIGRATION.md runbook (with four worked transcripts), updates to ARCHITECTURE.md / IMPLEMENTING_STORES.md / FAQ.md / CLI.md. (D-A17, D-A18, D-A19, MIG-01..04 closure.)

Purpose: Phase 14's stated duration is 4 weeks because the long tail is production-rollout documentation. Operators will read BLOCKSTORE_MIGRATION.md before they ever invoke `dfsctl blockstore migrate`; the runbook must be self-sufficient.

Output: All five docs files updated; one human-verify checkpoint at the end.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-03-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-04-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-05-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-06-SUMMARY.md

<interfaces>
<!-- Existing docs styling — keep consistent. -->

From docs/ARCHITECTURE.md (existing — see lines 1–80; section structure uses `## Pattern Overview` then `## Layers`; the new section follows this voice).

From docs/FAQ.md (existing — Q/A pairs; the new entry follows the existing Q/A markup).

From docs/IMPLEMENTING_STORES.md (existing — backend implementer's guide; new schema note slots into the existing "Required Methods" or "Conformance" section).

CLI.md regeneration command (search README/CONTRIBUTING/Makefile for the exact target; if there's a `make docs-cli` target use it; if not, it's typically `go run ./cmd/dfsctl docs --output docs/CLI.md` via Cobra's GenMarkdownTree pattern — grep `GenMarkdown` in the codebase to confirm).
</interfaces>
</context>

<tasks>

<task type="auto">
  <name>Task 1: docs/BLOCKSTORE_MIGRATION.md operator runbook with 4 worked transcripts</name>
  <files>docs/BLOCKSTORE_MIGRATION.md</files>
  <read_first>
    - .planning/phases/14-migration-tool-a5/14-CONTEXT.md (D-A17, D-A19 — runbook scope + four transcripts)
    - docs/CONFIGURATION.md (existing operator-doc voice; mirror tone)
    - docs/ARCHITECTURE.md (lines 1–80 — for cross-reference style)
    - .planning/phases/14-migration-tool-a5/14-03-SUMMARY.md, 14-04, 14-05, 14-06 (the actual command flags + output shapes)
  </read_first>
  <action>
    Create `docs/BLOCKSTORE_MIGRATION.md` with the following structure (target ≥ 400 lines including transcripts):

    ```markdown
    # Block-Store Migration Runbook (v0.13/v0.14 → v0.15)

    > **Audience:** Operators running DittoFS v0.13.x or v0.14.x who need to upgrade to v0.15+ with the new content-addressable (CAS) block layout.

    ## Why migrate

    - One-paragraph context: v0.15 introduces immutable CAS keys (`cas/{hh}/{hh}/{hex}`) replacing mutable path-indexed keys (`{payloadID}/block-{idx}`).
    - Benefits: 40–80% cross-VM dedup, atomic per-share backups, simplified GC.
    - Cost: One offline maintenance window per share.

    ## Prerequisites

    - DittoFS server upgraded to v0.15+ binary BEFORE migration. The dual-read shim in v0.15 reads both legacy and CAS keys, so the running server can serve unmigrated shares while you migrate them one at a time.
    - Operator account with shell access to the daemon host AND admin credentials for `dfsctl`.
    - Stop the daemon for the share you're about to migrate (offline-only invariant — D-A5). Other shares on the same daemon can keep serving if they're independently configured; if not, a global outage window is required.

    ## Pre-flight checklist

    - [ ] Confirm v0.15+ binary: `dfs --version`.
    - [ ] Confirm share's BlockLayout: `dfsctl blockstore migrate status --share NAME` reports `BlockLayout: legacy`.
    - [ ] Confirm S3 credentials are valid: a successful `aws s3 head-bucket --bucket NAME` (or equivalent) on the share's remote.
    - [ ] Estimate migration size: `dfsctl blockstore migrate --share NAME --dry-run` reports estimated upload bytes. Multiply by your S3 throughput to estimate wall time.
    - [ ] Choose a maintenance window long enough for the migration plus 50% headroom.

    ## Procedure

    1. **Stop the daemon** (or, more precisely, ensure it's not serving the target share):
       ```bash
       sudo systemctl stop dfs    # or: pkill -INT dfs
       ```

    2. **Run the migration**:
       ```bash
       dfsctl blockstore migrate --share myshare --parallel 4 --bandwidth-limit 50MB
       ```

       - `--parallel` = number of concurrent file workers (default 4).
       - `--bandwidth-limit` = aggregate S3 PUT byte rate. Suffixes: `KB/MB/GB` (1000-base) or `KiB/MiB/GiB` (1024-base). Empty / `0` = unlimited.
       - On a TTY, a progress bar overlays. On a pipe (e.g., `>migrate.log`), structured slog events stream instead.

    3. **Watch progress** in another terminal (read-only):
       ```bash
       dfsctl blockstore migrate status --share myshare    # human table
       dfsctl blockstore migrate status --share myshare -o json | jq '.files_done, .files_total'
       ```

    4. **On completion**, the tool runs the integrity check, flips the share to `cas-only`, and deletes legacy keys automatically. Final state:
       ```bash
       dfsctl blockstore migrate status --share myshare
       # FIELD             VALUE
       # BlockLayout       cas-only
       # FilesDone         12345
       # FilesSkipped      0
       # ...
       ```

    5. **Restart the daemon**:
       ```bash
       sudo systemctl start dfs
       ```

       The engine reads `BlockLayout: cas-only` from the share's metadata at open and skips the legacy fallback in the dual-read shim.

    6. **Mount and smoke-test** at least one file from a client:
       ```bash
       mount -t nfs -o nolock localhost:/myshare /mnt/myshare
       md5sum /mnt/myshare/known-file
       umount /mnt/myshare
       ```

    ## Bandwidth tuning

    - Default `--parallel 4` saturates ~4 concurrent S3 connections; tune up for high-bandwidth links, down for slow links.
    - `--bandwidth-limit` is *aggregate*, not per-worker. `--parallel 8 --bandwidth-limit 50MB` total ≤ 50 MB/s.
    - Burst behavior: the rate limiter allows a 1 MB burst (or one second's worth, whichever is larger) — chunks are 1–16 MB so the limit is enforced strictly across multiple WaitN calls.
    - For TB-scale shares, expect 4–24 hours per TB depending on `--parallel`, S3 region latency, and dedup hit rate.

    ## Recovery

    ### Crash mid-migration

    The tool is resumable. Per-file commits are atomic (D-A1). Re-running picks up where the last successful per-file commit left off:
    ```bash
    dfsctl blockstore migrate --share myshare    # automatic resume
    ```

    The journal is at `{share-data-dir}/.migration-state.jsonl`. Don't delete it.

    ### Integrity-check failure

    On HEAD-per-ref failure (any CAS key missing or with a tampered content-hash header), the tool exits non-zero and leaves:
    - `BlockLayout: legacy` (unchanged)
    - Legacy keys intact
    - Journal in place
    - Any new CAS chunks orphaned in S3 (GC reclaims them on the next cycle)

    Diagnose:
    ```bash
    # Inspect the failure log:
    journalctl -u dfs --since "10 minutes ago" | grep -i integrity
    # Check the specific missing key directly:
    aws s3api head-object --bucket BUCKET --key cas/AB/CD/AB CD...
    ```

    Common causes:
    1. **S3 outage during migration** — re-run after S3 recovers.
    2. **Object lifecycle deleting recent uploads** — adjust S3 lifecycle policy.
    3. **Misconfigured remote credentials** — check the daemon's view of S3 differs from `dfsctl`'s.

    Re-run the migration:
    ```bash
    dfsctl blockstore migrate --share myshare    # picks up from journal
    ```

    ### Forced abort

    Sigint-ing `dfsctl blockstore migrate` mid-loop is safe; the in-flight per-file commit may roll back, but the journal won't be corrupted. Re-run the command to resume.

    ## Worked transcripts

    ### Transcript 1 — happy path, ~10 GB share

    ```text
    $ sudo systemctl stop dfs

    $ dfsctl blockstore migrate status --share photos
    FIELD            VALUE
    BlockLayout      legacy
    FilesTotal       2543
    FilesDone        0
    JournalPresent   false

    $ dfsctl blockstore migrate --share photos --parallel 4
    Migrating: 2543/2543 (100.0%) ETA 0s
    Files Total      2543
    Files Done       2543
    Files Skipped    0
    Bytes Uploaded   8.2 GB
    Bytes Deduped    1.8 GB
    Duration         18m04s

    $ dfsctl blockstore migrate status --share photos
    FIELD            VALUE
    BlockLayout      cas-only
    FilesTotal       2543
    FilesDone        2543

    $ sudo systemctl start dfs
    ```

    ### Transcript 2 — TB-scale share with parallel + bandwidth tuning

    [...similar shape, ~50 lines, showing --parallel 16 --bandwidth-limit 200MB on a 1.2 TB share, includes the slog stream output excerpt and final summary]

    ### Transcript 3 — crash mid-migration + auto-resume

    [...shows the operator killing the migration ~30% through, then re-running and observing FilesSkipped=N where N is the prior progress; demonstrates D-A4 trust-journal-head]

    ### Transcript 4 — integrity-check failure + diagnosis + re-run

    [...shows a deliberately corrupted S3 object (e.g., one CAS key deleted out-of-band), the tool reporting ErrIntegrityCheckFailed, the operator running `aws s3api head-object` to confirm the missing key, restoring the upload (manual S3 PUT or trigger a re-PUT by truncating the journal entry for the affected file), and re-running successfully]

    ## Internals (for the curious)

    See `docs/ARCHITECTURE.md`, section "Dual-read shim and per-share `block_layout`", for the design rationale. Key invariants:
    - Atomic unit = one file (D-A1)
    - Journal lives at `{share-data-dir}/.migration-state.jsonl` with periodic snapshot at `.migration-state.snapshot.json`
    - Integrity check = HEAD per unique BlockRef.Hash + x-amz-meta-content-hash parity
    - Cutover = single metadata txn flipping `block_layout`, runs ONLY after integrity passes
    - Legacy delete = best-effort batch after cutover

    ## Out of scope

    - **Online migration** — rejected; offline only.
    - **Reverse migration (CAS → legacy)** — rejected; failures are fail-loud, manual fix, re-run forward.
    - **Cross-bucket dedup during migration** — non-goal.
    - **Per-chunk checkpoint granularity** — rejected; per-file is the unit.

    ## See also

    - `docs/ARCHITECTURE.md` — dual-read shim + block_layout section
    - `docs/IMPLEMENTING_STORES.md` — block_layout schema requirement
    - `docs/CLI.md` — auto-generated reference for `dfsctl blockstore migrate`
    - `docs/FAQ.md` — "How do I migrate from v0.13 to v0.15?"
    ```

    All four transcripts must be **complete, copy-pasteable, and accurate** — match the actual stdout the tool produces (Plan 03 + 04 + 05 + 06 outputs).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; test -f docs/BLOCKSTORE_MIGRATION.md &amp;&amp; wc -l docs/BLOCKSTORE_MIGRATION.md | awk '$1 &gt;= 400 {print "ok"; exit 0} {exit 1}' &amp;&amp; grep -c 'Transcript ' docs/BLOCKSTORE_MIGRATION.md | awk '$1 &gt;= 4 {exit 0} {exit 1}' &amp;&amp; grep -q 'dfsctl blockstore migrate' docs/BLOCKSTORE_MIGRATION.md &amp;&amp; grep -q 'BlockLayout' docs/BLOCKSTORE_MIGRATION.md &amp;&amp; grep -q '\.migration-state\.jsonl' docs/BLOCKSTORE_MIGRATION.md</automated>
  </verify>
  <acceptance_criteria>
    - File `docs/BLOCKSTORE_MIGRATION.md` exists with ≥ 400 lines.
    - Contains all four worked transcripts (search "Transcript 1", "Transcript 2", "Transcript 3", "Transcript 4").
    - Contains the exact CLI command names: `dfsctl blockstore migrate`, `dfsctl blockstore migrate status`.
    - Contains references to `--parallel`, `--bandwidth-limit`, `--dry-run` flags.
    - Cross-links to ARCHITECTURE.md / IMPLEMENTING_STORES.md / CLI.md / FAQ.md.
    - `markdownlint docs/BLOCKSTORE_MIGRATION.md` clean (project's existing markdownlint config — see `.markdownlint.yaml`).
  </acceptance_criteria>
  <done>
    Operator runbook exists, four worked transcripts cover happy path / TB-scale tuning / crash-resume / integrity failure, all CLI commands match the actual implementation.
  </done>
</task>

<task type="auto">
  <name>Task 2: ARCHITECTURE.md + IMPLEMENTING_STORES.md + FAQ.md + CLI.md regeneration</name>
  <files>docs/ARCHITECTURE.md, docs/IMPLEMENTING_STORES.md, docs/FAQ.md, docs/CLI.md</files>
  <read_first>
    - docs/ARCHITECTURE.md (existing — find a section header like `## Block Store` or similar; the new content goes there)
    - docs/IMPLEMENTING_STORES.md (find the "Required schema" or "Conformance suite" section)
    - docs/FAQ.md (full file — see Q/A markup convention)
    - cmd/dfsctl/main.go or commands/root.go (look for any GenMarkdown / docs-gen wiring)
    - Makefile (or scripts/ directory; grep for "docs" or "gen-cli")
  </read_first>
  <action>
    1. **`docs/ARCHITECTURE.md`** — add a new section (typical placement: near the existing "Block Store Implementations" subsection in the Layers list, OR as a new top-level `## Migration & Dual-Read Shim` section before the closing). Length target: 60–120 lines. Content:
       - Why CAS keys + dual-read shim
       - The `block_layout` per-share flag — values, where it's stored, when it's read
       - The migration tool's role and Phase 15's removal of the shim
       - One-paragraph data-flow diagram (text) showing engine.ReadAt → BlockLayout check → CAS path or legacy path

    2. **`docs/IMPLEMENTING_STORES.md`** — locate the "Conformance suite" or "Required methods" section for metadata stores; add (~20 lines):
       ```markdown
       ### Block layout flag (v0.15+)

       Metadata backends MUST persist a `block_layout` field on the share record. The field is a string enum with values `legacy` or `cas-only` (Plan 14-01 introduced this; Phase 14 [#425]).

       The conformance suite scenario `RunBlockLayoutSuite` exercises round-trip and update semantics. New backends MUST invoke it from their per-backend test file:

       \`\`\`go
       func TestBlockLayoutConformance(t *testing.T) {
           storetest.RunBlockLayoutSuite(t, factoryFunc)
       }
       \`\`\`

       Empty / missing values MUST coerce to `legacy` on read (forward-compat for pre-Phase-14 rows). Use the `metadata.ParseBlockLayout("")` helper which returns `BlockLayoutLegacy`.
       ```

    3. **`docs/FAQ.md`** — add a new entry (~30 lines):
       ```markdown
       ### How do I migrate from v0.13 / v0.14 to v0.15?

       Use `dfsctl blockstore migrate --share <name>` per share. The migration is offline (the daemon must be stopped for the share). See `docs/BLOCKSTORE_MIGRATION.md` for the full runbook with worked examples.

       Quick version:
       1. Stop the daemon: `sudo systemctl stop dfs`
       2. Migrate: `dfsctl blockstore migrate --share myshare --parallel 4`
       3. Verify: `dfsctl blockstore migrate status --share myshare` shows `BlockLayout: cas-only`.
       4. Restart: `sudo systemctl start dfs`

       The migration is resumable, dry-run-able (`--dry-run`), and bandwidth-cappable (`--bandwidth-limit 50MB`).
       ```

    4. **`docs/CLI.md`** — regenerate from cobra. Two paths:
       - If `make docs-cli` (or similar) exists: run it, commit the result.
       - If not: add a small `cmd/dfsctl/docs/main.go` (or extend an existing one — grep for `GenMarkdownTree` or `GenMarkdown`) that calls `cobra.Command.GenMarkdownTree(rootCmd, "docs/CLI/")` or `GenMarkdownTreeCustom`. Run it; copy the relevant sections into CLI.md, OR (preferred) restructure CLI.md to be the auto-generated tree's index file.

       If CLI.md doesn't follow a regenerated pattern in the existing codebase, just hand-edit:
       - Add `### dfsctl blockstore migrate` section (~40 lines) listing all flags from Plan 03 + Plan 04 with descriptions.
       - Add `### dfsctl blockstore migrate status` section (~20 lines).

       The verify step asserts the section headers exist regardless of which path was taken.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; grep -q 'block_layout' docs/ARCHITECTURE.md &amp;&amp; grep -q 'dual-read' docs/ARCHITECTURE.md &amp;&amp; grep -q 'block_layout' docs/IMPLEMENTING_STORES.md &amp;&amp; grep -q 'RunBlockLayoutSuite' docs/IMPLEMENTING_STORES.md &amp;&amp; grep -q 'v0.13\|v0.14' docs/FAQ.md &amp;&amp; grep -q 'BLOCKSTORE_MIGRATION' docs/FAQ.md &amp;&amp; grep -q 'dfsctl blockstore migrate' docs/CLI.md</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'block_layout' docs/ARCHITECTURE.md` >= 2.
    - `grep -c 'dual-read' docs/ARCHITECTURE.md` >= 1.
    - `grep -c 'RunBlockLayoutSuite' docs/IMPLEMENTING_STORES.md` >= 1.
    - `grep -c 'BLOCKSTORE_MIGRATION' docs/FAQ.md` >= 1.
    - `grep -c 'dfsctl blockstore migrate' docs/CLI.md` >= 1.
    - `markdownlint docs/ARCHITECTURE.md docs/IMPLEMENTING_STORES.md docs/FAQ.md docs/CLI.md` clean.
  </acceptance_criteria>
  <done>
    All four supporting docs files updated. CLI.md regenerated (or hand-updated) with the new `migrate` command tree.
  </done>
</task>

<task type="checkpoint:human-verify" gate="blocking">
  <name>Task 3: Operator-runbook walkthrough checkpoint</name>
  <files>docs/BLOCKSTORE_MIGRATION.md, docs/ARCHITECTURE.md, docs/IMPLEMENTING_STORES.md, docs/FAQ.md, docs/CLI.md</files>
  <action>
    Pause for human verification. The Claude executor MUST stop here and surface the <how-to-verify> steps to the user. Do NOT proceed past this task without an explicit "approved" resume-signal. This checkpoint exists because automated grep gates cannot judge "is this runbook clear and operator-ready?"
  </action>
  <what-built>
    docs/BLOCKSTORE_MIGRATION.md (Plan 14-07 Task 1) is the primary user-facing artifact for this phase. The four worked transcripts and the procedure are operator-facing — automated verification cannot judge "is this clear and copy-pasteable?"
  </what-built>
  <how-to-verify>
    1. Open `docs/BLOCKSTORE_MIGRATION.md` in a markdown viewer.
    2. Read the Pre-flight checklist; confirm every check is something an operator would actually run pre-flight.
    3. Read the Procedure section; confirm steps 1–6 are unambiguous.
    4. Read all four transcripts; confirm:
       - Commands match what the actual tool produces (`go run ./cmd/dfsctl blockstore migrate --help` for sanity).
       - stdout output matches what Plans 03 + 04 + 05 + 06 ship.
       - Transcript 4 (integrity failure) has actionable diagnosis steps.
    5. Read the Recovery section; confirm both "crash mid-migration" and "integrity-check failure" subsections give the operator a concrete next step.
    6. Click through cross-links to ARCHITECTURE.md, IMPLEMENTING_STORES.md, FAQ.md, CLI.md and confirm each landing-page paragraph is consistent with the runbook.
    7. Run `markdownlint docs/BLOCKSTORE_MIGRATION.md` (project's existing config) and confirm clean.
  </how-to-verify>
  <resume-signal>
    Type "approved" or describe specific transcript / procedure issues to fix.
  </resume-signal>
  <verify>
    <automated>echo "Human checkpoint — see how-to-verify steps above; Claude executor pauses here for resume-signal"</automated>
  </verify>
  <done>
    Reviewer typed "approved" after walking through BLOCKSTORE_MIGRATION.md end-to-end and confirming the four transcripts are accurate, the procedure is clear, and cross-links land on consistent content.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Documentation → operator decisions | Inaccurate or stale docs lead to operator error, which is the primary risk for a migration tool. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-07-01 | Repudiation | Stale CLI flags in runbook diverge from actual tool | mitigate | Verify step requires running `dfsctl blockstore migrate --help` and matching against Procedure step 2's flags. The human-verify checkpoint is the final guard. |
| T-14-07-02 | Information disclosure | Transcript 4 leaks sensitive S3 key paths | accept | Worked example uses synthetic share names ("photos", "myshare"). Operator-facing runbook in our public-ish docs is the right venue. |
</threat_model>

<verification>
- BLOCKSTORE_MIGRATION.md ≥ 400 lines, four transcripts, cross-linked to other docs.
- ARCHITECTURE.md has the new dual-read + block_layout section.
- IMPLEMENTING_STORES.md notes the schema requirement + RunBlockLayoutSuite.
- FAQ.md has the v0.13→v0.15 migration entry.
- CLI.md updated.
- markdownlint clean across all five files.
- Human checkpoint approved.
</verification>

<success_criteria>
- All five docs files updated with the content specified above.
- Human reviewer confirms the runbook is operator-ready.
- markdownlint clean.
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-07-SUMMARY.md` summarizing the docs landing.
</output>
