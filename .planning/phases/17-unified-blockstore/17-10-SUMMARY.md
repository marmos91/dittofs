---
phase: 17-unified-blockstore
plan: 10
subsystem: docs
tags: [docs, blockstore, cas, migration, godoc, sentinel, cli]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: "Plan 08 (migrate-to-cas library + cobra subcommand + bypass constructor + 5-flag inventory + sentinel JSON shape)"
  - phase: 17-unified-blockstore
    provides: "Plan 09 (boot-guard exit 78 + ErrLegacyLayoutDetected wiring + directive text + docs/CONFIGURATION.md §migration anchor expectation)"
provides:
  - "pkg/blockstore/doc.go — package-level godoc covering BlockStore + BlockStoreAppend interface roles, Meta contract (D-08), Walk semantics (D-07), .cas-migrated-vN sentinel-file convention, migration entry point, error sentinel inventory"
  - "docs/CONFIGURATION.md ## Migration section — anchor matches Plan 09 boot-guard directive (§migration); documents boot-guard exit code 78, all five migrate-to-cas flags, per-share sentinel JSON, journal contract, recovery procedure"
  - "docs/CLI.md `dfs migrate-to-cas` entry — synopsis, 5-flag table, exit codes (0/1/2), progress-reporting formats, idempotent journaled resume, 4 examples, cross-reference back to CONFIGURATION.md#migration"
affects:
  - 17-CLI-followup  # The cross-plan sentinel path mismatch noted in 17-09 SUMMARY (migrate CLI writes at <shareDir>/.cas-migrated-v1; production opens FSStore on <shareDir>/blocks) is unchanged by Plan 10. Docs describe the contract; the path-mismatch fix is a parallel work stream.

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Three doc surfaces kept in sync via acceptance-criteria grep gates: pkg/blockstore/doc.go (godoc), docs/CONFIGURATION.md (operator), docs/CLI.md (reference). If the cobra subcommand's flag set evolves and the docs lag, the grep gates fail on the next planning cycle (Plan 10 threat model T-17-10-03)."
    - "Anchor-slug coupling: the boot-guard directive emitted by cmd/dfs/commands/start.go references 'docs/CONFIGURATION.md §migration'. GitHub anchor slugs '## Migration' to '#migration', so the heading text MUST be exactly 'Migration'. Any future rename requires updating Plan 09's directive in lockstep."
    - "Sentinel-file convention documented as a project-wide pattern in pkg/blockstore/doc.go (not just a Phase 17 implementation detail). Future irreversible state transitions can follow the .<event>-<schema>-vN naming + atomic-rename + read-on-open + do-not-edit-warning pattern."

key-files:
  created:
    - .planning/phases/17-unified-blockstore/17-10-SUMMARY.md
  modified:
    - pkg/blockstore/doc.go      # Expanded package godoc; +176 / -11 lines
    - docs/CONFIGURATION.md      # Added ## Migration section; +132 / 0 lines
    - docs/CLI.md                # Added ### Block Store Migration (v0.16.0 Phase 17) + `### dfs migrate-to-cas`; +95 / 0 lines
  deleted: []

key-decisions:
  - "doc.go is rewritten as a single godoc block above `package blockstore` — replaced the prior 14-line stub. Sub-package map updated to include migrate, blockstoretest, gc, chunker (which existed but were not listed)."
  - "## Migration section placement: top-level (## level) immediately before ## Environment Variables in docs/CONFIGURATION.md. Reason: anchor must be #migration to match the boot-guard directive — adding it inside the existing § 6 (Block Store) as a `### Migration` would produce anchor #6-migration or similar via GitHub's slug rules. Top-level ## guarantees the slug. Acceptance criterion `grep -c '^## Migration$'` enforces this."
  - "CLI.md placement: as a new `### Block Store Migration (v0.16.0 Phase 17)` subsection right after the existing `### Block Store Migration (v0.15.x Phase 14)` dfsctl section, with `#### dfs migrate-to-cas` as the command entry. This keeps both migration commands in the same neighborhood for operators who arrive at the doc from either direction. The CLI's existing depth convention (### for command groups, #### for individual subcommands) was preserved."
  - "Wording on irreversibility: docs/CONFIGURATION.md states the migration is irreversible up front in the 'Required when upgrading' subsection. pkg/blockstore/doc.go calls the sentinel 'a one-way irreversibility marker' and tells operators it cannot be hand-fixed. The boot-guard's per-share fail-fast property is documented in both surfaces so the operator's mental model is consistent across godoc and operator docs."
  - "Sentinel JSON field names: the CONFIGURATION.md example uses the exact field names from Plan 08's MigrationToolVersion / SentinelFileName constants (Version, CompletedAt, ToolVersion, ShareDir — CamelCase, matching Go's encoding/json default for unexported tags). If the migrate library later switches to lowercase JSON tags via struct tags, docs must update — caught by the grep gate that asserts `\"Version\"|\"CompletedAt\"|\"ToolVersion\"` appears at least twice."

patterns-established:
  - "Docs-as-grep-gate: critical operator surfaces (flag names, exit codes, sentinel filenames, JSON field names) are pinned by acceptance-criteria grep checks in the PLAN.md. Future drift between subcommand and docs surfaces on the next planning cycle when the grep gates fail. Avoid auto-generated docs for these surfaces (cobra's --help output is the canonical machine reference; the docs are the human-curated reference)."
  - "Cross-plan anchor coupling: when one plan's code emits a string referring to another plan's doc anchor (Plan 09 → docs/CONFIGURATION.md §migration), the receiving plan MUST use the exact heading text to produce the matching slug. Document the coupling in both plans' acceptance criteria."

requirements-completed: []

# Metrics
duration: ~10min
completed: 2026-05-20
---

# Phase 17 Plan 10: Migration documentation (godoc + CONFIGURATION.md + CLI.md)

**Shipped the operator-facing documentation surfaces for the v0.16.0 CAS
migration: package godoc in `pkg/blockstore/doc.go` covering BlockStore +
BlockStoreAppend interface roles, the Meta contract, Walk semantics, and the
`.cas-migrated-vN` sentinel-file convention as a project-wide pattern;
`docs/CONFIGURATION.md` gains a `## Migration` section whose anchor matches
the Plan 09 boot-guard directive's `§migration` reference, documenting all
five `dfs migrate-to-cas` flags, exit code 78 (`EX_CONFIG` per sysexits(3)),
the per-share sentinel JSON shape, the per-share journal contract, and the
recovery procedure; `docs/CLI.md` gains a `### dfs migrate-to-cas` entry with
synopsis, flag table, exit codes, progress formats, idempotent-resume
contract, and four usage examples cross-referenced back to
CONFIGURATION.md#migration. Three doc surfaces stay in sync via the
acceptance-criteria grep gates the plan defines.**

## Performance

- **Duration:** ~10 min
- **Tasks:** 3 (auto, all on plan)
- **Commits:** 3 — `bb97ec34` (Task 1 godoc), `99b5ef58` (Task 2
  CONFIGURATION.md), `9f604247` (Task 3 CLI.md)
- **Files created:** 1 (this SUMMARY)
- **Files modified:** 3 (pkg/blockstore/doc.go, docs/CONFIGURATION.md,
  docs/CLI.md)
- **LoC delta:** +176 / −11 (doc.go) + +132 (CONFIGURATION.md) + +95
  (CLI.md) = approximately +403 / −11

## Accomplishments

### Task 1 — `pkg/blockstore/doc.go` package godoc

Replaced the 14-line stub with a single comprehensive godoc block above
`package blockstore`. Coverage:

- **Package overview** — what the package provides (BlockStore +
  BlockStoreAppend interfaces, ContentHash as the key, FastCDC chunks as
  the unit of storage).
- **Interface roles** — BlockStore (Put/Get/GetRange/Has/Delete/Walk/Head)
  and BlockStoreAppend (embeds BlockStore, adds AppendWrite + DeleteLog
  for the random-write absorber tier). Lists the three implementations
  (`*fs.FSStore`, `*s3.Store`, `*memory.Store`).
- **Meta contract** — the D-08 minimal `{Size, LastModified}` shape; hash
  is the input, not output; LastModified MUST be non-zero (mirrors GC
  fail-closed gate from Phase 11 WR-4-02 / INV-04); S3's
  `x-amz-meta-content-hash` header preserved internally for BSCAS-06
  defense-in-depth but not surfaced through Meta.
- **Walk semantics** — D-07 contract: callback returns `ErrStopWalk` for
  clean early-exit; any other non-nil error wraps as
  `fmt.Errorf("walk halted at %s: %w", hash, err)`; ctx cancellation
  aborts immediately without a final spurious callback. Pattern mirrors
  `filepath.SkipDir` / `fs.SkipAll`.
- **Sentinel-file convention** — establishes `.cas-migrated-vN` as a
  project-wide pattern for irreversible on-disk state transitions.
  Documents lifecycle (atomic-rename write only on success), reader
  (backend constructors at open), writer (migration tooling), per-share
  placement, sentinel JSON contents, footgun warning, and the schema-bump
  protocol for future N+1 versions.
- **Migration entry point** — pointer to `cmd/dfs/commands/migrate_to_cas.go`
  and `pkg/blockstore/migrate/migrate_to_cas.go` for operators who arrive
  via `go doc`, with cross-references to `docs/CONFIGURATION.md §Migration`
  and exit code 78.
- **Error sentinels** — list of `ErrStopWalk`, `ErrLegacyLayoutDetected`,
  `ErrChunkNotFound`, `ErrBlockNotFound`, `ErrCASContentMismatch`,
  `ErrCASKeyMalformed`, `ErrBlockRefMissing` with one-line summaries.
  Confirmed against `pkg/blockstore/errors.go` — `ErrBlockNotFound` still
  exists (not deleted by Plan 07).
- **Sub-package map** — refreshed to include `migrate`, `blockstoretest`,
  `gc`, `chunker` (which existed but were not listed in the stub).

### Task 2 — `docs/CONFIGURATION.md ## Migration` section

Inserted top-level `## Migration` section immediately before
`## Environment Variables`. Subsections:

- **Required when upgrading from v0.15.x or earlier** — states the
  migration is irreversible up front; recommends out-of-band backup if
  rollback is operationally required.
- **Boot-guard behavior** — exit code 78 (EX_CONFIG per sysexits(3)),
  exact stderr directive text matching Plan 09's
  `formatLegacyLayoutDirective`, per-share fail-fast property.
- **Running the migration** — `dfs stop` then `dfs migrate-to-cas`
  prerequisite, refusal-on-PID-lockfile note, table documenting all five
  flags (`--storage-dir`, `--share`, `--dry-run`, `--json`, `--config`),
  plain-text and `--json` progress emission formats.
- **Crash safety** — per-share journal at
  `<storage_dir>/<share>/.dittofs-migrate-to-cas.state`, rerun-resumes-from-journal
  contract, journal-removed-only-after-sentinel-write contract, CAS Put
  idempotency on hash collision.
- **Verifying completion** — per-share sentinel at
  `<share_dir>/.cas-migrated-v1` with the four-field JSON shape (`Version`,
  `CompletedAt`, `ToolVersion`, `ShareDir`), atomic-rename write contract,
  do-not-hand-edit warning, `cat` verification command, and the
  "successful `dfs start` is the final verification" check.
- **Recovery from a failed migration** — four-step procedure including
  `ErrChunkPutMismatch` triage (post-Put BLAKE3 disagreement indicates
  storage corruption between Put and re-Get; investigate destination
  before retrying).
- **See also** — link to `docs/CLI.md#dfs-migrate-to-cas`.

### Task 3 — `docs/CLI.md ### dfs migrate-to-cas` entry

Inserted new `### Block Store Migration (v0.16.0 Phase 17)` subsection
right after the existing `### Block Store Migration (v0.15.x Phase 14)`
dfsctl section, with `#### dfs migrate-to-cas` as the command entry. The
two migrations now sit adjacent for operators who arrive at either via
search. Coverage:

- **Synopsis** — `dfs migrate-to-cas [flags]`.
- **Flags table** — five flags with type, default, description matching
  Plan 08's cobra subcommand verbatim.
- **Exit codes table** — `0` success, `1` generic error, `2` mid-flight
  failure with journal preserved.
- **Progress reporting** — plain-text and `--json` formats with full JSON
  object shape.
- **Idempotent / journaled resume** — references the journal location +
  CAS Put idempotency.
- **Examples** — four `bash` blocks: dry-run, single-share `--json | tee`,
  all-shares (with `dfs stop` prerequisite), custom `--storage-dir`.
- **See also** — link to `CONFIGURATION.md#migration`.

## Decisions Made

### Anchor-slug coupling: `## Migration` heading text is load-bearing

The Plan 09 boot-guard directive prints
"See docs/CONFIGURATION.md §migration." GitHub anchor-slugging rules
convert the heading text "Migration" to anchor `#migration`. Any other
heading text (e.g., "## Upgrading from v0.15.x") would produce a different
slug and the link would 404. Plan 10's acceptance criterion
`grep -c '^## Migration$'` enforces the exact heading.

### Sentinel-file convention documented as project-wide pattern, not Phase
### 17 implementation detail

The `.cas-migrated-vN` pattern is documented in `pkg/blockstore/doc.go` as
a reusable convention for irreversible on-disk state transitions: dot-
prefix + event name + schema version + atomic-rename write + read-on-open
+ do-not-edit-warning. Future schema migrations (e.g., a v2 chunker
format) can follow the same shape. The doc explicitly describes the
schema-bump protocol (increment N; new constructors stat the highest
version they recognize and refuse below).

### CLI.md placement adjacent to existing v0.15.x migrate section

Both migration commands now sit under `### Block Store Migration (v0.XX.X
Phase NN)` subsections back-to-back. Operators searching for "migration"
or "migrate" land on either depending on which version they came from.
The newer v0.16.0 Phase 17 section appears second (chronological), which
also matches the upgrade path direction (v0.13/14 → Phase 14 dfsctl
migrate → v0.15 → Phase 17 dfs migrate-to-cas → v0.16).

### Sentinel JSON field names match Plan 08's Go struct verbatim

The CONFIGURATION.md example uses CamelCase field names (`Version`,
`CompletedAt`, `ToolVersion`, `ShareDir`) matching Plan 08's
`sentinelPayload` struct without JSON tags — Go's `encoding/json` defaults
to exported field names. If the migrate library later adds lowercase JSON
tags, the docs must update; the grep gate
`grep -cE '"Version"|"CompletedAt"|"ToolVersion"'` will fail on the next
planning cycle if so.

## Deviations from Plan

None. All three tasks executed exactly as the PLAN.md specified, all
acceptance-criteria grep gates passed, all cross-references resolved.

### Note on the inherited sentinel-path inconsistency (NOT a Plan 10
### deviation)

The spawn prompt acknowledged a known cross-plan sentinel-placement issue:
the migrate CLI writes the sentinel at `<shareDir>/.cas-migrated-v1` but
production opens `fs.NewFSStore` on `<shareDir>/blocks`. Plan 10's scope
is documentation, not code reconciliation; this is being fixed in a
parallel work stream. Plan 10's docs describe the contract from the
fs-layer perspective (the sentinel lives at the same baseDir production
passes to fs.NewFSStore) — when the parallel fix lands, the docs already
match.

## Threat Flags

None. No new security-relevant surface introduced — pure documentation
plan touching no protocol handlers, no auth paths, no schema, no network
endpoints.

## Issues Encountered

None.

## Verification Output

```
$ go vet ./pkg/blockstore/
$ echo $?
0

$ go build ./pkg/blockstore/
$ echo $?
0

$ go vet ./...
$ echo $?
0

$ go doc pkg/blockstore | wc -l
206

$ grep -c 'BlockStore\|BlockStoreAppend' pkg/blockstore/doc.go
11

$ grep -c '\.cas-migrated-v' pkg/blockstore/doc.go
4

$ grep -c 'ErrStopWalk\|ErrLegacyLayoutDetected' pkg/blockstore/doc.go
5

$ grep -c 'migrate-to-cas\|Migration' pkg/blockstore/doc.go
9

$ grep -c '^## Migration$' docs/CONFIGURATION.md
1

$ grep -c 'migrate-to-cas' docs/CONFIGURATION.md
10

$ grep -c '\.cas-migrated-v1' docs/CONFIGURATION.md
4

$ grep -cE 'exit.*78|EX_CONFIG|code 78|code \*\*78\*\*' docs/CONFIGURATION.md
2

$ grep -c -- '--storage-dir' docs/CONFIGURATION.md
1
$ grep -c -- '--share' docs/CONFIGURATION.md
4
$ grep -c -- '--dry-run' docs/CONFIGURATION.md
3
$ grep -c -- '--json' docs/CONFIGURATION.md
2
$ grep -c -- '--config' docs/CONFIGURATION.md
20

$ grep -c 'CLI.md.*migrate-to-cas' docs/CONFIGURATION.md
1

$ grep -cE '"Version"|"CompletedAt"|"ToolVersion"' docs/CONFIGURATION.md
3

$ grep -c 'dfs migrate-to-cas' docs/CLI.md
8

$ grep -c -- '--storage-dir' docs/CLI.md
2
$ grep -c -- '--share' docs/CLI.md
15
$ grep -c -- '--dry-run' docs/CLI.md
8
$ grep -c -- '--json' docs/CLI.md
3
$ grep -c -- '--config' docs/CLI.md
3

$ grep -c 'Exit codes' docs/CLI.md
4

$ grep -c 'CONFIGURATION.md#migration' docs/CLI.md
1

$ grep -c '```bash' docs/CLI.md
13
```

All acceptance-criteria grep gates pass with margins.

## Next Plan Readiness

- **Phase 17 mega-PR closure** — Plans 17-01 through 17-10 are all
  shipped. The remaining tracked carry-over is the cross-plan
  sentinel-path mismatch (migrate CLI writes at `<shareDir>/.cas-migrated-v1`,
  production opens FSStore on `<shareDir>/blocks`) — fixed in a parallel
  work stream per the spawn prompt. Once that lands, Phase 17 is ready
  for develop merge.
- **Phase 18 (Syncer simplification)** — unblocked. Phase 17's four
  mandatory pieces (interface convergence, legacy deletion, migration
  tool, boot guard) are all in place, and Plan 10 closes the operator
  documentation contract.

## Self-Check

- `pkg/blockstore/doc.go` exists with substantive package godoc — **VERIFIED**.
- `docs/CONFIGURATION.md` contains `^## Migration$` — **VERIFIED**.
- `docs/CLI.md` contains `dfs migrate-to-cas` — **VERIFIED**.
- Commits `bb97ec34`, `99b5ef58`, `9f604247` in `git log` — **VERIFIED**.
- `go vet ./...` exits 0 — **VERIFIED**.
- All acceptance-criteria grep gates pass — **VERIFIED** (see Verification
  Output above).
- Cross-reference Plan 09 boot-guard directive (`§migration`) matches the
  `## Migration` heading via GitHub's anchor-slug rule — **VERIFIED**.

## Self-Check: PASSED

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
