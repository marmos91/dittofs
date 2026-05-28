---
phase: 08-pre-refactor-cleanup-a0
plan: 11
subsystem: docs
tags: [docs, backup, v0.15.0, v0.16.0, TD-03, release-notes]

# Dependency graph
requires:
  - phase: 08-pre-refactor-cleanup-a0
    provides: "Plans 08-01..08-10 removed pkg/backup, storebackups, REST/CLI/apiclient backup surfaces, and e2e tests (PR-B commits 1-7 per D-30)."
provides:
  - "docs/BACKUP.md deleted (377 lines)."
  - "Stray backup references pruned from docs/ARCHITECTURE.md (Cobra command list), docs/CLI.md (dfs command tree), README.md (CLI purposes table + `dfs backup controlplane` example)."
  - "README.md Changelog section introduced with v0.15.0 release-note documenting backup removal + v0.16.0 CAS-backed reintroduction plan."
affects: [08-12-go-mod-tidy, v0.15.0-release-notes, v0.16.0-backup-cas]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "README-level Changelog section (###-style list) for milestone release notes."

key-files:
  created:
    - ".planning/phases/08-pre-refactor-cleanup-a0/08-11-SUMMARY.md"
  modified:
    - "docs/ARCHITECTURE.md"
    - "docs/CLI.md"
    - "README.md"
  deleted:
    - "docs/BACKUP.md"

key-decisions:
  - "Added a top-level `## Changelog` section in README.md (between `## License` and `## Disclaimer`) rather than creating a separate CHANGELOG.md — keeps the v0.13.0-was-never-released note discoverable where users already land."
  - "Did not edit docs/FAQ.md: audit found no v0.13.0 backup references there (only generic file-sharing language; the word 'backup' does not appear)."
  - "Trimmed CLI.md dfs command tree: rewrote `config` as the last child (└──) now that `backup` is gone — preserves valid ASCII tree rendering."
  - "Kept PerconaBackupConfig (pgBackRest) references untouched per D-25 — none appear in README/docs/ARCHITECTURE/CLI/FAQ anyway."

patterns-established:
  - "Release-note text wording (quoted verbatim from D-23 + 08-11 plan) — reusable template for v0.16.0 entry when backup reintroduction ships."

requirements-completed: [TD-03]

# Metrics
duration: 3min
completed: 2026-04-23
---

# Phase 08 Plan 11: Remove v0.13.0 backup docs + prune references Summary

**Deleted `docs/BACKUP.md`, pruned residual `dfs backup` references from ARCHITECTURE/CLI/README, and added a README Changelog entry documenting v0.13.0 backup removal with v0.16.0 CAS reintroduction plan.**

## Performance

- **Duration:** ~3 min
- **Started:** 2026-04-23T20:20:00Z
- **Completed:** 2026-04-23T20:21:00Z
- **Tasks:** 1
- **Files modified:** 3 (ARCHITECTURE.md, CLI.md, README.md)
- **Files deleted:** 1 (docs/BACKUP.md, 377 lines)

## Accomplishments

- Removed the public-facing `docs/BACKUP.md` design/usage doc (377 LOC) — no user-visible path to v0.13.0 backup remains.
- Pruned the last three surface-level references to `dfs backup` / `backup` subcommand:
  - `docs/ARCHITECTURE.md:376` — dropped `backup` from the Cobra commands list in the directory-structure tree.
  - `docs/CLI.md:31-32` — removed the `backup` subtree (`└── backup / └── controlplane`) from the `dfs` command hierarchy; reshaped `config` as the final child node.
  - `README.md:223` — dropped `backup` from the `dfs` purposes column in the two-binary CLI table.
  - `README.md:247-248` — removed the `# Backup` comment and `./dfs backup controlplane --output /tmp/backup.json` example from the Server Management snippet.
- Added a `## Changelog` section to README.md with the v0.15.0 release-note line: removal of v0.13.0 backup (REST API, `dfsctl backup`, `pkg/backup`, scheduler) + v0.16.0 CAS-foundation reintroduction + no-backcompat callout.

## Task Commits

1. **Task 1: Remove docs/BACKUP.md + audit backup references** — `fac29ef8` (docs)
   - `docs: remove v0.13.0 backup docs + prune references (TD-03)`
   - Good GPG signature (m.marmos@gmail.com, RSA key SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8).
   - 4 files changed, 11 insertions(+), 389 deletions(-).

## Files Created/Modified

- `docs/BACKUP.md` — **deleted** (377-line design/usage doc for the removed v0.13.0 backup system).
- `docs/ARCHITECTURE.md` — removed `backup` from the `dfs` Cobra commands list (line 376).
- `docs/CLI.md` — removed the `backup / controlplane` subtree from the `dfs` command tree (lines 31-32); restructured the tree so `config` is the final branch.
- `README.md` —
  - Removed `backup` from the dfs binary's purpose column in the two-binary CLI table (line 223).
  - Removed the `# Backup` comment + `./dfs backup controlplane --output /tmp/backup.json` example from the Server Management bash block (lines 247-248).
  - Added a `## Changelog` section between `## License` and `## Disclaimer` with a single v0.15.0 bullet documenting the backup removal and v0.16.0 CAS plan.

## Sections Removed (per-file audit)

| File | Section / Line | Before | After |
|------|----------------|--------|-------|
| `docs/BACKUP.md` | whole file | 377 lines of v0.13.0 backup design, REST/CLI surface, destination driver reference | deleted via `git rm` |
| `docs/ARCHITECTURE.md` | line 376 | `# Cobra commands (start, stop, config, logs, backup)` | `# Cobra commands (start, stop, config, logs)` |
| `docs/CLI.md` | lines 31-32 | `└── backup        Backup operations` and `    └── controlplane  Backup control plane database` (attached after a `└── config` child) | `config` promoted to the `└──` terminal branch; backup subtree removed |
| `README.md` | line 223 | `start, stop, status, config, logs, backup` | `start, stop, status, config, logs` |
| `README.md` | lines 247-248 | `# Backup` + `./dfs backup controlplane --output /tmp/backup.json` | removed |
| `README.md` | between `## License` and `## Disclaimer` | — | new `## Changelog` section with v0.15.0 release-note line |

## Verification Results

- `test ! -f docs/BACKUP.md` → **OK**.
- `grep -rni "BACKUP\.md" docs/ README.md` → 0 matches.
- `grep -rniE "dfsctl backup|api/v1/backups" docs/ README.md` → only the README changelog line (expected — self-referential within the v0.15.0 release note). No functional references.
- `grep -rn "pkg/backup" docs/ README.md` → only the README changelog line (same).
- `grep -niE "v0\\.(15|16)\\.0" README.md | grep -c "backup"` → **1** (release-note line present).
- `go build ./...` → exit 0.
- `git log -1 --show-signature` → **Good** signature.
- `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by"` → no match (W14 OK).

## Decisions Made

- Placed the release-note line inline in README.md under a new `## Changelog` section rather than in a separate `CHANGELOG.md` — matches the plan's "README.md … or an equivalent changelog" option and keeps the note where users already look. Future milestones can promote this section to a standalone CHANGELOG.md when the list grows.
- No edits to `docs/FAQ.md`: pre-edit audit (`grep -ni "backup" docs/FAQ.md`) returned zero matches. The file did not reference v0.13.0 backup features, pgBackRest, or any backup concept — nothing to prune.
- In `docs/CLI.md`, restructured the tree branch markers so `config` uses `└──` and its children use 4-space indentation (instead of `│` continuation), preserving a syntactically-valid ASCII tree once `backup` was removed.

## Deviations from Plan

None — plan executed exactly as written. Acceptance criteria, verification steps, and commit message conform to the plan spec; no Rule 1/2/3/4 triggers occurred during execution.

## Issues Encountered

None. Three read-before-edit reminder hooks fired during editing, but the edits themselves succeeded because the relevant file regions had been read prior to editing (the reminders are precautionary, not blocking).

## User Setup Required

None — docs-only change.

## Next Phase Readiness

- PR-B commit 6 of D-30 is complete. Plan 08-12 (`build: go mod tidy + OTel audit`, PR-B commit 7) is unblocked — it only requires the prior deletion commits (plans 08-04..08-10) to be in place, which they are.
- Public docs now match the post-backup-removal state; no broken links remain in README/ARCHITECTURE/CLI/FAQ.
- The `## Changelog` section exists and can be extended during the v0.15.0 release and when v0.16.0 ships its CAS-backed backup.

## Self-Check: PASSED

- `.planning/phases/08-pre-refactor-cleanup-a0/08-11-SUMMARY.md` — FOUND.
- `docs/BACKUP.md` — absent (deletion verified).
- `docs/ARCHITECTURE.md`, `docs/CLI.md`, `README.md` — FOUND (modified).
- Commit `fac29ef8` — FOUND in git log.

---
*Phase: 08-pre-refactor-cleanup-a0*
*Plan: 11*
*Completed: 2026-04-23*
