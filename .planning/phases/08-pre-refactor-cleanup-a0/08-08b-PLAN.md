---
phase: 08-pre-refactor-cleanup-a0
plan: 08b
type: execute
wave: 2
depends_on: [08-08a]
files_modified:
  - pkg/metadata/backup.go
  - pkg/metadata/backup_shim_test.go
  - pkg/metadata/storetest/backup_conformance.go
  - pkg/metadata/storetest/suite.go
  - pkg/metadata/store/badger/backup.go
  - pkg/metadata/store/badger/backup_test.go
  - pkg/metadata/store/memory/backup.go
  - pkg/metadata/store/memory/backup_test.go
  - pkg/metadata/store/postgres/backup.go
  - pkg/metadata/store/postgres/backup_test.go
autonomous: true
requirements: [TD-03]
must_haves:
  truths:
    - "`pkg/metadata/backup.go` (shim: `Backupable`, `PayloadIDSet`, `ErrBackupUnsupported` aliases) is deleted."
    - "`pkg/metadata/backup_shim_test.go` is deleted."
    - "`pkg/metadata/storetest/backup_conformance.go` is deleted; no conformance-suite registry still lists it."
    - "`pkg/metadata/store/{badger,memory,postgres}/backup.go` and their `backup_test.go` siblings are all deleted."
    - "`go build ./...` and `go test -count=1 -short -race ./...` pass."
    - "Nothing outside `pkg/backup/` still imports `pkg/backup/` — confirmed by grep — setting up plan 08-10 to delete `pkg/backup/` cleanly."
  artifacts: []
  key_links:
    - from: "(deleted) pkg/metadata/backup.go"
      to: pkg/backup (Backupable/PayloadIDSet source of truth)
      via: "type alias removal"
      pattern: "type Backupable\\|type PayloadIDSet\\|ErrBackupUnsupported"
    - from: "(deleted) pkg/metadata/store/{badger,memory,postgres}/backup.go"
      to: pkg/metadata/backup.go (alias surface being removed)
      via: "per-backend Backupable implementations gone"
      pattern: "Backup\\(ctx.*\\|Restore\\(ctx.*"
---

<objective>
PR-B commit 5 (D-30 step 5 — metadata shim + per-backend + conformance) — Delete the `pkg/metadata` backup shim (which aliases types from `pkg/backup`), its per-backend implementations (`badger`, `memory`, `postgres`), and the conformance suite that exercised them. After this commit, the only remaining backup code is `pkg/backup/` itself and `pkg/controlplane/runtime/storebackups/` — each of which is now unreferenced from outside itself and ready to be deleted in plans 08-09 and 08-10.

Purpose: Close the metadata layer. Per D-01 (live-code audit), the metadata surface has a shim layer + three per-backend impls + a conformance suite — all dependent on `pkg/backup/`. Must run BEFORE plan 08-10 (`pkg/backup/` deletion) or imports break.
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
@pkg/metadata/backup.go
@pkg/metadata/storetest/suite.go
</context>

<tasks>

<task type="auto">
  <name>Task 1: Remove metadata backup shim + per-backend impls + conformance (D-01, D-30 step 5)</name>
  <files>pkg/metadata/backup.go, pkg/metadata/backup_shim_test.go, pkg/metadata/storetest/backup_conformance.go, pkg/metadata/storetest/suite.go (only if it references backup), pkg/metadata/store/badger/backup.go, pkg/metadata/store/badger/backup_test.go, pkg/metadata/store/memory/backup.go, pkg/metadata/store/memory/backup_test.go, pkg/metadata/store/postgres/backup.go, pkg/metadata/store/postgres/backup_test.go</files>
  <read_first>
    - .planning/phases/08-pre-refactor-cleanup-a0/08-CONTEXT.md (D-01, D-30 step 5)
    - pkg/metadata/backup.go (confirm: shim exporting `Backupable`, `PayloadIDSet`, `ErrBackupUnsupported` — all type aliases / var aliases pointing at `pkg/backup`)
    - pkg/metadata/storetest/backup_conformance.go (contains `RunBackupConformanceSuite`, `RunBackupConformanceSuiteWithOptions`, `BackupTestStore`, `BackupStoreFactory`, `BackupSuiteOptions`)
    - pkg/metadata/storetest/suite.go (reality check: it contains `RunConformanceSuite` which runs FileOps/DirOps/Permissions/DurableHandles/FileBlockOps. Confirmed 2026-04-23: `RunConformanceSuite` does NOT call `RunBackupConformanceSuite`. Only the per-backend `backup_test.go` files do. So suite.go does NOT need editing unless a hidden reference is found — verify with `grep -n "Backup" pkg/metadata/storetest/suite.go`)
    - pkg/metadata/store/{badger,memory,postgres}/backup.go (each implements `Backup`/`Restore` on the backend's transaction/store type)
    - pkg/metadata/store/{badger,memory,postgres}/backup_test.go (each calls `storetest.RunBackupConformanceSuite(...)`)
  </read_first>
  <action>
    Step 1 (pre-audit grep) — Confirm no external importer uses these symbols:
      `grep -rn "metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported" . --include='*.go' | grep -v "^pkg/metadata/backup\|^pkg/metadata/backup_shim_test\|^pkg/metadata/store/\(badger\|memory\|postgres\)/backup" | wc -l` → 0 (anything else is an un-unwired caller; stop and fix upstream).
      `grep -rn "RunBackupConformanceSuite\|BackupTestStore\|BackupStoreFactory\|BackupSuiteOptions" . --include='*.go' | grep -v "^pkg/metadata/storetest/backup_conformance\|^pkg/metadata/store/\(badger\|memory\|postgres\)/backup_test" | wc -l` → 0.

    Step 2 (delete shim + per-backend + conformance files) —
      ```bash
      git rm pkg/metadata/backup.go pkg/metadata/backup_shim_test.go
      git rm pkg/metadata/storetest/backup_conformance.go
      git rm pkg/metadata/store/badger/backup.go pkg/metadata/store/badger/backup_test.go
      git rm pkg/metadata/store/memory/backup.go pkg/metadata/store/memory/backup_test.go
      git rm pkg/metadata/store/postgres/backup.go pkg/metadata/store/postgres/backup_test.go
      ```

    Step 3 (conditionally edit `pkg/metadata/storetest/suite.go`) —
      Run: `grep -n "Backup\|RunBackupConformance" pkg/metadata/storetest/suite.go` → if ANY match appears, remove each matching line/stanza (e.g., an unused import of backup types, or a hidden `t.Run("Backup", ...)` call). If the grep returns zero matches (expected per 2026-04-23 audit), skip this step entirely and do NOT modify suite.go — add a short note in the SUMMARY to that effect.

    Step 4 (verify) —
      - `go build ./...` exits 0. Any `undefined: metadata.Backupable` etc. is a missed un-unwire; follow and delete upward.
      - `go test -count=1 -short -race ./pkg/metadata/... ./pkg/controlplane/... ./pkg/blockstore/...` exits 0.
      - Grep sweeps (all must yield 0 lines):
        - `grep -rn "metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported" . --include='*.go'`
        - `grep -rn "RunBackupConformanceSuite\|BackupTestStore\|BackupStoreFactory\|BackupSuiteOptions" . --include='*.go'`
        - `grep -rn "\"github.com/marmos91/dittofs/pkg/backup\"" pkg/metadata --include='*.go'` → 0 (no metadata file imports pkg/backup anymore).
      - The check that pkg/backup is now un-imported outside itself + storebackups (both about to be deleted) is a strong signal plan 08-09 / 08-10 are clear to run:
        `grep -rn "\"github.com/marmos91/dittofs/pkg/backup\"\|\"github.com/marmos91/dittofs/pkg/backup/" . --include='*.go' | grep -v "^pkg/backup/\|^pkg/controlplane/runtime/storebackups/" | wc -l` → 0.

    Step 5 (Claude-Code hygiene) — `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by" && exit 1 || true`.

    Step 6 (commit) — signed:
      `git commit -S -m "metadata: remove Backupable shim + per-backend impls + conformance (TD-03)"`
  </action>
  <verify>
    <automated>test ! -f pkg/metadata/backup.go &amp;&amp; test ! -f pkg/metadata/storetest/backup_conformance.go &amp;&amp; test ! -f pkg/metadata/store/badger/backup.go &amp;&amp; test ! -f pkg/metadata/store/memory/backup.go &amp;&amp; test ! -f pkg/metadata/store/postgres/backup.go &amp;&amp; [ "$(grep -rn 'metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported' . --include='*.go' | wc -l)" = "0" ] &amp;&amp; [ "$(grep -rn 'RunBackupConformanceSuite' . --include='*.go' | wc -l)" = "0" ] &amp;&amp; go build ./... &amp;&amp; go test -count=1 -short -race ./pkg/metadata/... ./pkg/controlplane/... ./pkg/blockstore/...</automated>
  </verify>
  <acceptance_criteria>
    - All 10 files deleted (4 at pkg/metadata, 6 at pkg/metadata/store/{badger,memory,postgres}).
    - `grep -rn "metadata\.Backupable\|metadata\.PayloadIDSet\|metadata\.ErrBackupUnsupported" . --include='*.go' | wc -l` → 0.
    - `grep -rn "RunBackupConformanceSuite\|BackupTestStore\|BackupStoreFactory\|BackupSuiteOptions" . --include='*.go' | wc -l` → 0.
    - `grep -rn "\"github.com/marmos91/dittofs/pkg/backup\"\|\"github.com/marmos91/dittofs/pkg/backup/" . --include='*.go' | grep -v "^pkg/backup/\|^pkg/controlplane/runtime/storebackups/" | wc -l` → 0 (pkg/backup has no external importers).
    - `go build ./...` exits 0.
    - `go test -count=1 -short -race ./pkg/metadata/... ./pkg/controlplane/... ./pkg/blockstore/...` exits 0.
    - `git log -1 --format=%s` = `metadata: remove Backupable shim + per-backend impls + conformance (TD-03)`.
    - `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by" && exit 1 || true` passes (no offending strings).
    - `git log -1 --show-signature` reports Good signature.
  </acceptance_criteria>
  <done>
    Metadata backup surface gone (shim + 3 backends + conformance). `pkg/backup/` now has zero external importers outside `pkg/controlplane/runtime/storebackups/` (also about to die). Plans 08-09 and 08-10 are unblocked.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Metadata store API | `Backupable` facet removed from the store API. No downstream consumer remains. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-08-08b-01 | T (Tampering) | Per-backend Backup/Restore methods gone; a backend that someone forked externally might still implement `metadata.Backupable` | accept | Unreleased v0.13.0 surface; no external forks carry the interface. Compile + CI catches any in-tree residual. |
| T-08-08b-02 | I (Information disclosure) | Deletion of the conformance suite means future regressions go undetected | accept | Intended — backup being removed wholesale. v0.16.0 will introduce a new CAS-based suite. |
| T-08-08b-03 | E (EoP) | n/a | n/a | n/a |
</threat_model>

<verification>
- `go build ./...` green; metadata + controlplane + blockstore tests green.
- All greps yield zero matches.
- Commit signed; Claude-Code hygiene check passes.
</verification>

<success_criteria>
- D-01 (metadata slice) + D-30 step 5 complete; independently green.
- Stage is set for plan 08-09 (storebackups package deletion) and plan 08-10 (pkg/backup deletion).
</success_criteria>

<output>
`.planning/phases/08-pre-refactor-cleanup-a0/08-08b-SUMMARY.md` — commit SHA; list of 10 deleted files; note on whether `storetest/suite.go` required editing (expected: NO); final grep confirmations.
</output>
