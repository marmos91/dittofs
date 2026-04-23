---
phase: 08-pre-refactor-cleanup-a0
plan: 08a
type: execute
wave: 2
depends_on: [08-08]
files_modified:
  - pkg/controlplane/store/backup.go
  - pkg/controlplane/store/backup_test.go
  - pkg/controlplane/store/interface.go
  - pkg/controlplane/models/backup.go
  - pkg/controlplane/models/backup_test.go
  - pkg/controlplane/models/models.go
autonomous: true
requirements: [TD-03]
must_haves:
  truths:
    - "`pkg/controlplane/store/backup.go` and `backup_test.go` are deleted."
    - "`pkg/controlplane/store/interface.go` no longer defines `BackupStore` interface, `ErrInvalidProgress`, `BackupJobFilter`, or the embedded `BackupStore` in `GORMStore`; no `GORMStore.UpdateBackupRepo` (or siblings)."
    - "`pkg/controlplane/models/backup.go` and `backup_test.go` are deleted."
    - "`pkg/controlplane/models/models.go`'s `AllModels()` no longer registers `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}`."
    - "`go build ./...` and `go test -count=1 -short -race ./...` pass."
  artifacts:
    - path: pkg/controlplane/store/interface.go
      provides: "Store interface without BackupStore facet."
    - path: pkg/controlplane/models/models.go
      provides: "AllModels() without backup GORM registrations."
  key_links:
    - from: pkg/controlplane/store/interface.go
      to: "(deleted BackupStore interface + embedded)"
      via: "interface cleanup"
      pattern: "BackupStore|ErrInvalidProgress|BackupJobFilter|UpdateBackupRepo"
    - from: pkg/controlplane/models/models.go
      to: "(deleted AllModels() registrations)"
      via: "GORM auto-migrate surface shrinks"
      pattern: "BackupRepo\\{\\}|BackupRecord\\{\\}|BackupJob\\{\\}"
---

<objective>
PR-B commit 4 (D-30 step 4 — store + GORM persistence layer) — Delete the GORM-backed `BackupStore` persistence and the `BackupRepo`/`BackupRecord`/`BackupJob` GORM models. After plan 08-07 unwired REST/CLI/apiclient callers and plan 08-08 unwired Runtime, nothing else imports the BackupStore interface or the models. This commit removes those types cleanly.

Purpose: Close the persistence layer. Prerequisite for deleting `pkg/backup/` (plan 08-10) which is still imported by the metadata shim (plan 08-08b) — this plan strictly predates 08-08b in the reverse-import chain for the store/models surface.
Output: One atomic commit.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/phases/08-pre-refactor-cleanup-a0/08-CONTEXT.md
@CLAUDE.md
@pkg/controlplane/store/interface.go
@pkg/controlplane/models/models.go
</context>

<tasks>

<task type="auto">
  <name>Task 1: Remove BackupStore persistence + GORM models (D-01, D-30 step 4)</name>
  <files>pkg/controlplane/store/backup.go, pkg/controlplane/store/backup_test.go, pkg/controlplane/store/interface.go, pkg/controlplane/models/backup.go, pkg/controlplane/models/backup_test.go, pkg/controlplane/models/models.go</files>
  <read_first>
    - .planning/phases/08-pre-refactor-cleanup-a0/08-CONTEXT.md (D-01, D-30 step 4)
    - pkg/controlplane/store/interface.go (verify lines before editing — CONTEXT says: `BackupStore interface` lines 369-503; embedded `BackupStore` in `GORMStore` around line 700; `ErrInvalidProgress` + `BackupJobFilter` lines 23-42. Actual line numbers may drift; trust the source — use grep to anchor before surgical delete)
    - pkg/controlplane/store/backup.go (confirm it's the GORM-backed BackupStore implementation — contains `GORMStore.UpdateBackupRepo` and siblings like CreateBackupRepo, DeleteBackupRepo, ListBackupRepos, etc.)
    - pkg/controlplane/models/models.go (CONTEXT says `AllModels()` registers `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` on lines 22-24; verify before editing)
    - pkg/controlplane/models/backup.go (contains `BackupRepo`, `BackupRecord`, `BackupJob` GORM model structs)
  </read_first>
  <action>
    Step 1 (pre-audit grep) — Confirm no external importers still reference the symbols about to die:
      `grep -rn "store\.BackupStore\|store\.ErrInvalidProgress\|store\.BackupJobFilter\|UpdateBackupRepo\|CreateBackupRepo\|DeleteBackupRepo\|ListBackupRepos" . --include='*.go' | grep -v "^pkg/controlplane/store/backup"` → MUST be empty. Any hit is an un-unwired caller — stop and fix upstream (plan 08-07 or 08-08 missed something).
      `grep -rn "models\.BackupRepo\|models\.BackupRecord\|models\.BackupJob" . --include='*.go' | grep -v "^pkg/controlplane/models/backup"` → MUST be empty.

    Step 2 (delete files) —
      ```bash
      git rm pkg/controlplane/store/backup.go pkg/controlplane/store/backup_test.go
      git rm pkg/controlplane/models/backup.go pkg/controlplane/models/backup_test.go
      ```

    Step 3 (surgical edit: `pkg/controlplane/store/interface.go`) —
      Use `grep -n` to anchor each removal. Delete, in source order:
      (a) `ErrInvalidProgress` sentinel and `BackupJobFilter` struct (CONTEXT says lines 23-42 — verify by `grep -n "ErrInvalidProgress\|BackupJobFilter" pkg/controlplane/store/interface.go`). Also remove any sibling types or var decls that only serve the backup facet.
      (b) `BackupStore interface { ... }` block in full (CONTEXT says lines 369-503 — verify with `grep -n "BackupStore interface\|^}" pkg/controlplane/store/interface.go` and delete the full brace-balanced block).
      (c) The embedded `BackupStore` inside `GORMStore` struct (CONTEXT says line 700 — verify with `grep -nA2 "GORMStore struct" pkg/controlplane/store/interface.go`).
      (d) If the `Store` umbrella interface embeds `BackupStore`, remove that embedding too (grep for `BackupStore` in the file — any remaining match outside deletions above is a structural embed to remove).
      (e) Strip any now-unused imports (e.g., time imports only used by deleted filter types).

    Step 4 (surgical edit: `pkg/controlplane/models/models.go`) —
      In `AllModels()`, remove the three lines registering `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` (CONTEXT says lines 22-24 — verify with `grep -n "BackupRepo{}\|BackupRecord{}\|BackupJob{}" pkg/controlplane/models/models.go`). Preserve surrounding entries.

    Step 5 (verify — compilation + grep sweep) —
      - `go build ./...` exits 0. Any "undefined: BackupStore / BackupRepo / ..." is an un-unwired caller; follow upward and delete.
      - `go test -count=1 -short -race ./pkg/controlplane/... ./pkg/metadata/... ./pkg/blockstore/...` exits 0.
      - `grep -rn "BackupStore\|ErrInvalidProgress\|BackupJobFilter" pkg/controlplane/store/ --include='*.go'` → 0 matches (file deleted, interface excised).
      - `grep -rn "BackupRepo{}\|BackupRecord{}\|BackupJob{}" pkg/controlplane/models/ --include='*.go'` → 0 matches (file deleted, registrations excised).
      - `grep -rn "models\.BackupRepo\|models\.BackupRecord\|models\.BackupJob" . --include='*.go'` → 0 matches.

    Step 6 (Claude-Code hygiene) — `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by" && exit 1 || true`.

    Step 7 (commit) — signed:
      `git commit -S -m "store: remove BackupStore persistence + GORM models (TD-03)"`
  </action>
  <verify>
    <automated>test ! -f pkg/controlplane/store/backup.go &amp;&amp; test ! -f pkg/controlplane/models/backup.go &amp;&amp; ! grep -q "BackupStore interface\|ErrInvalidProgress\|BackupJobFilter" pkg/controlplane/store/interface.go &amp;&amp; ! grep -qE "BackupRepo\{\}|BackupRecord\{\}|BackupJob\{\}" pkg/controlplane/models/models.go &amp;&amp; go build ./... &amp;&amp; go test -count=1 -short -race ./pkg/controlplane/... ./pkg/metadata/... ./pkg/blockstore/...</automated>
  </verify>
  <acceptance_criteria>
    - `test ! -f pkg/controlplane/store/backup.go && test ! -f pkg/controlplane/store/backup_test.go` passes.
    - `test ! -f pkg/controlplane/models/backup.go && test ! -f pkg/controlplane/models/backup_test.go` passes.
    - `grep -c "BackupStore\|ErrInvalidProgress\|BackupJobFilter" pkg/controlplane/store/interface.go` → 0.
    - `grep -cE "BackupRepo\{\}|BackupRecord\{\}|BackupJob\{\}" pkg/controlplane/models/models.go` → 0.
    - `grep -rn "models\.BackupRepo\|models\.BackupRecord\|models\.BackupJob\|store\.BackupStore\|store\.BackupJobFilter" . --include='*.go' | wc -l` → 0.
    - `go build ./...` exits 0.
    - `go test -count=1 -short -race ./pkg/controlplane/... ./pkg/metadata/... ./pkg/blockstore/...` exits 0.
    - `git log -1 --format=%s` = `store: remove BackupStore persistence + GORM models (TD-03)`.
    - `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by" && exit 1 || true` passes (no offending strings).
    - `git log -1 --show-signature` reports Good signature.
  </acceptance_criteria>
  <done>
    Persistence layer for backup is gone. Interface shrunk. GORM models unregistered. Build + tests green.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| SQL persistence (GORM → Postgres) | Dropping three model registrations means GORM no longer auto-migrates those tables. Existing tables on running databases become orphaned (not dropped). |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-08-08a-01 | I (Information disclosure) | Orphaned `backup_repos` / `backup_records` / `backup_jobs` tables may linger in Postgres after upgrade | accept | v0.13.0 was never released (MEMORY.md); no live deployments carry these tables. A fresh install creates none. Operators running test DBs can manually `DROP TABLE`. |
| T-08-08a-02 | T (Tampering) | Removing `BackupStore interface` could orphan an unknown implementor | mitigate | Step 1 pre-audit grep ensures no external references. `go build ./...` catches any residual implementor. |
| T-08-08a-03 | R (Repudiation) | Audit-log records referencing now-missing GORM models | accept | No production audit log depends on this; internal only. |
</threat_model>

<verification>
- `go build ./...` green; control-plane + metadata + blockstore tests green.
- Greps confirm removals.
- Commit signed; Claude-Code hygiene check passes.
</verification>

<success_criteria>
- D-01 (persistence slice) + D-30 step 4 complete; independently green.
</success_criteria>

<output>
`.planning/phases/08-pre-refactor-cleanup-a0/08-08a-SUMMARY.md` — commit SHA; list of deleted types/functions from `store/interface.go`; list of removed GORM registrations from `models/models.go`; pre-audit grep confirmation.
</output>
