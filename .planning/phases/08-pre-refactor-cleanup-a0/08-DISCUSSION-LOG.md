# Phase 08: Pre-refactor cleanup (A0) — Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in `08-CONTEXT.md` — this log preserves the alternatives considered.

**Date:** 2026-04-23
**Phase:** 08-pre-refactor-cleanup-a0
**Milestone:** v0.15.0
**GH issue:** #420
**Areas discussed:** Scaffolding deletion blast radius, Metadata field removal + on-disk compat, PR/commit granularity, Parser collapse strategy, Code structure/design/tests, Runtime/API/Docs follow-ups, Dep & observability follow-ups

---

## Scaffolding Deletion Blast Radius

| Option | Description | Selected |
|--------|-------------|----------|
| Surgical: just BackupHoldProvider | Delete the type, storebackups/backup_hold.go, NewBackupHold call in blockgc.go. Leave pkg/backup/ and rest of storebackups/ alone. | |
| Amputate backup hold chain only | Delete BackupHoldProvider chain + audit other references, keep broader backup package surface. | |
| **Full backup system removal** | Delete pkg/backup/, pkg/controlplane/runtime/storebackups/, backup API handlers, backup scheduler, backup CLI. All v0.13.0 surface. | ✓ |

**User's choice:** Full backup system removal
**Notes:** MEMORY.md recorded "v0.13.0 backup unreleased — breaking changes fine" so scope expansion is pre-cleared. Folds into TD-03.

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Delete both FileAttr and pending_writes COW fields** | Remove COWSourcePayloadID from FileAttr AND PendingWriteIntent. Drop field in all CopyPayload call sites. | ✓ |
| FileAttr only | Drop FileAttr.COWSourcePayloadID. Leave PendingWriteIntent as-is. | |
| Audit first | Grep every COWSource* reference during planning. | |

**User's choice:** Delete both FileAttr and pending_writes COW fields

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Verify zero consumers, hard delete** | Grep SetFinalizationCallback; if unused (as MEMORY.md implies), delete type + setter + field in one commit. | ✓ |
| Keep as deprecated, no-op | Leave types but mark Deprecated. | |
| You decide | Planner investigates during research. | |

**User's choice:** Verify zero consumers, hard delete

---

| Option | Description | Selected |
|--------|-------------|----------|
| Leave phase 05 context as historical | Don't retcon; add SUPERSEDED note in phase 08 CONTEXT. | |
| Update phase 05 with SUPERSEDED header | Banner atop 05-CONTEXT.md. | |
| Move phase 05 artifacts to archive | Relocate to .planning/milestones/v0.13.0-archive/. | |

**User's choice:** "You decide what's best for gsd workflow"
**Claude's decision:** Leave phase 05 historical, add SUPERSEDED note in phase 08 CONTEXT.md `<specifics>` section. Preserves GSD principle that past phase artifacts are immutable records.

---

## Metadata Field Removal + On-Disk Compat

| Option | Description | Selected |
|--------|-------------|----------|
| **Note in v0.15.0 REQUIREMENTS.md next to TD-03** | Inline comment that A3/A4 reintroduces with new types. No code breadcrumb. | ✓ |
| Comment in FileAttr struct | 2-line comment block listing deleted fields + reintro reference. | |
| Delete cleanly, no breadcrumb | Git log is the record. | |

**User's choice:** Note in v0.15.0 REQUIREMENTS.md next to TD-03

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Delete assignments + remove COW-specific tests** | Drop any test setting these fields; delete COW tests entirely. Conformance suites keep, drop COW assertions. | ✓ |
| Keep tests, mark skipped | t.Skip(...) with issue link. | |
| Audit during planning | Planner enumerates references before deleting. | |

**User's choice:** Delete assignments + remove COW-specific tests

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Rely on encoding/json tolerance** | Go's encoding/json ignores unknown keys; stale data keeps reading. Document behavior. | ✓ |
| Write one-shot SQL cleanup migration | UPDATE pending_writes SET pre_write_attr = pre_write_attr - 'cow_source' - 'object_id' - 'blocks'. | |
| You decide | Claude picks based on best-fit during planning. | |

**User's choice:** Rely on encoding/json tolerance
**Scout finding supporting this:** All three fields have `omitempty` JSON tags and ObjectID/Blocks were never populated in practice. Postgres `files` table has no columns for these fields — only Go struct + JSONB snapshot. Risk is near-zero.

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Update #420 at start of execution** | Before PR-A lands, update issue description to reflect expanded scope. | ✓ |
| Separate GH issue | File child issue of #419/#420 for backup removal. | |
| No GH update | Phase 08 CONTEXT.md captures expanded scope; GH unchanged. | |

**User's choice:** Update #420 at start of execution

---

## PR/Commit Granularity

| Option | Description | Selected |
|--------|-------------|----------|
| **Split by theme — 3 PRs** | PR-A: TD-02 bug fixes first. PR-B: backup system removal. PR-C: TD-01 merge + TD-03 remnants + TD-04 parsers. | ✓ |
| Single atomic PR | All work in one PR. | |
| Split by TD-id — 4 PRs | One PR per TD-id. | |
| Many small PRs — 7-10 | Per-bug, per-deletion, etc. | |

**User's choice:** Split by theme — 3 PRs

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Phase 08 lands first, then Phase 09 rebases** | A0 stabilizes engine surface; ADAPT rebases afterward. | ✓ |
| Truly parallel, both rebase daily | Concurrent work with daily conflict resolution. | |
| Phase 09 first, then Phase 08 | ADAPT lands before cleanup. | |

**User's choice:** Phase 08 lands first, then Phase 09 rebases

---

| Option | Description | Selected |
|--------|-------------|----------|
| **One commit per TD-id sub-item, all green** | Each commit compiles and passes `go test -race`. | ✓ |
| Per-file commits | One commit per file touched. | |
| Logical feature commits (fewer, larger) | One commit per logical unit. | |

**User's choice:** One commit per TD-id sub-item, all green

---

| Option | Description | Selected |
|--------|-------------|----------|
| **All work on develop, no tags** | Per MEMORY.md release-flow rule. | ✓ |
| Tag v0.14.3 cleanup release after backup removal | Signal 'scaffolding cleaned' before A1+. | |
| You decide | Claude picks per GSD conventions. | |

**User's choice:** All work on develop, no tags

---

## Parser Collapse Strategy

| Option | Description | Selected |
|--------|-------------|----------|
| **Two canonical parsers — ParseStoreKey + ParseBlockID** | Collapse 3 external-format parsers to ParseStoreKey; 2 internal-format to ParseBlockID. 5 → 2. | ✓ |
| Force single format — one parser only | Migrate recovery.go + manage.go to 'block-' prefix everywhere. Behavior change. | |
| Keep three-tier split — formal types | Typed StoreKey/BlockID wrappers. | |
| You decide during planning | Planner audits and recommends. | |

**User's choice:** Two canonical parsers — ParseStoreKey + ParseBlockID
**Scout finding:** The 5 parsers are NOT one format — they split 3+2 across `{payloadID}/block-{N}` (external) vs `{payloadID}/{blockIdx}` (internal). Roadmap TD-04 wording is imprecise; the CONTEXT corrects it.

---

| Option | Description | Selected |
|--------|-------------|----------|
| **No prep — A2 adds ParseCASKey as separate parser** | Phase 08 collapses legacy only. A2 adds ParseCASKey; dual-parser A2–A5 window. | ✓ |
| Introduce format-discriminating parser skeleton now | ParseKey dispatches on prefix; A2 fills CAS branch. | |
| Add enum/typed discriminator | type KeyKind int; ParseKey returns (kind, fields, ok). | |

**User's choice:** No prep — A2 adds ParseCASKey as separate parser

---

| Option | Description | Selected |
|--------|-------------|----------|
| **pkg/blockstore/types.go** | Existing convention; ParseStoreKey already there. | ✓ |
| pkg/blockstore/engine/keys.go | After TD-01 merge, place in new engine/keys.go. | |
| New pkg/blockstore/keys subpackage | Dedicated package. | |

**User's choice:** pkg/blockstore/types.go

---

| Option | Description | Selected |
|--------|-------------|----------|
| **ParseStoreKey stays usable through A5, deleted in A6** | Legacy parser exported + stable. TD-10/A6 removes it after migration. | ✓ |
| Move legacy parser into migration/ package in A2 | Symbolic separation once CAS lands. | |
| You decide | Planner picks based on A2/A5 plan shape. | |

**User's choice:** ParseStoreKey stays usable through A5, deleted in A6

---

## Code Structure / Design / Tests

| Option | Description | Selected |
|--------|-------------|----------|
| **Flat engine/ with file-name prefixes** | readbuffer/readbuffer.go → engine/cache.go; sync/syncer.go → engine/syncer.go; etc. Rename at move. | ✓ |
| Preserve subdirs inside engine/ | engine/readbuffer/, engine/sync/, engine/gc/ subpackages. | |
| Flat with original file names | No role renames; A3 does cache renaming later. | |

**User's choice:** Flat engine/ with file-name prefixes

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Move with their code** | sync/syncer_test.go → engine/syncer_test.go. No reorganization. | ✓ |
| Consolidate tests by concern | Merge overlapping tests. | |
| Move now, reorganize later | Deferred reorg idea. | |

**User's choice:** Move with their code

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Strict: no FileBlockStore references in write path** | write.go / eviction.go zero FileBlockStore imports on hot path. | ✓ |
| Read-only access OK, no writes | Allow GetFileBlock but not PutFileBlock/MarkBlockRemote. | |
| Audit in planning then decide | Planner enumerates call sites. | |

**User's choice:** Strict: no FileBlockStore references in write path

---

| Option | Description | Selected |
|--------|-------------|----------|
| **go vet + staticcheck on touched packages only** | ./pkg/blockstore/... scope; fix findings introduced by moves. | ✓ |
| Full repo lint sweep + fix | golangci-lint run on whole repo. | |
| Skip lint sweep | Rely on CI lint gates. | |

**User's choice:** go vet + staticcheck on touched packages only

---

## Follow-up: Runtime / API / Docs

| Option | Description | Selected |
|--------|-------------|----------|
| **Remove field, wiring, lifecycle hooks** | Delete storeBackupsSvc field, 5 builder calls, SetBumpBootVerifier, startup/shutdown hooks. | ✓ |
| Keep method stubs as no-ops for one release | SetBumpBootVerifier remains as no-op. | |
| Audit adapter call sites first | Planner enumerates before removal. | |

**User's choice:** Remove field, wiring, lifecycle hooks

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Delete all 4 files + any imports in dfsctl** | apiclient/backup*.go + dfsctl backup commands, all in same PR. | ✓ |
| Keep types exported as deprecated | Placeholder structs for external consumers. | |
| Two-stage — delete client/API, defer dfsctl cleanup | dfsctl commands deleted in later sub-PR. | |

**User's choice:** Delete all 4 files + any imports in dfsctl

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Delete docs/BACKUP.md + audit ARCHITECTURE.md/CLI.md/README.md** | Full delete of BACKUP.md; grep other docs for backup refs; add removal note to README/release notes. | ✓ |
| Move BACKUP.md to archive/ | docs/archive/v0.13.0-BACKUP.md. | |
| Delete BACKUP.md, no audit | Minimal scope; accept doc rot. | |

**User's choice:** Delete docs/BACKUP.md + audit other docs

---

| Option | Description | Selected |
|--------|-------------|----------|
| **v0.15.0 release notes when milestone ships** | Breaking-change entry in release notes at milestone completion. | ✓ |
| Per-phase CHANGELOG entry on each PR merge | CHANGELOG.md updated per PR. | |
| No changelog entry — unreleased code | Skip entirely. | |

**User's choice:** v0.15.0 release notes when milestone ships

---

## Follow-up: Tests / Deps / Observability

| Option | Description | Selected |
|--------|-------------|----------|
| **Delete all backup e2e tests + helpers** | test/e2e/backup_*.go (4) + helpers/backup*.go (2). | ✓ |
| Move to archive then delete later | Rename with _archive_ prefix (build tag skipped). | |
| Keep as skipped with issue link | t.Skip for each; preserve structure for v0.16.0. | |

**User's choice:** Delete all backup e2e tests + helpers

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Run `go mod tidy` in PR-B, remove any orphaned direct deps** | After deletion, tidy + audit require block for formerly-backup-only deps. | ✓ |
| tidy only, no direct-dep removal | Run tidy but don't audit. | |
| Defer to a later 'dep hygiene' pass | Don't tidy in Phase 08. | |

**User's choice:** Run `go mod tidy` in PR-B, remove any orphaned direct deps

---

| Option | Description | Selected |
|--------|-------------|----------|
| **Remove OTel backup spans + check for other consumers** | Delete RunBackup/RunRestore spans; if backup was sole OTel consumer, drop from go.mod. | ✓ |
| Keep OTel import, just delete backup spans | Preserve OTel as direct dep for future observability. | |
| Audit during planning | Planner decides based on findings. | |

**User's choice:** Remove OTel backup spans + check for other consumers

---

| Option | Description | Selected |
|--------|-------------|----------|
| **No explicit regression test** | Compile + existing router tests catch residual wiring. | ✓ |
| Add smoke test confirming routes absent | Test asserting /api/v1/backup/repos returns 404. | |
| You decide | Planner calls it based on API router surface. | |

**User's choice:** No explicit regression test

---

## Update — Session 2 (2026-04-23)

Re-ran `/gsd-discuss-phase 8 --chain` against the existing CONTEXT.md to lock two remaining ordering decisions before planning.

### PR-B commit staging

| Option | Description | Selected |
|--------|-------------|----------|
| **Reverse-import order** | 7 commits, dependency-leaf-first (tests → API/dfsctl/apiclient → runtime wiring → storebackups pkg → pkg/backup → docs → go mod tidy). Each commit compiles + passes race tests; full bisectability. | ✓ |
| 3 themed commits | Coarser: API surface + tests / packages / docs + tidy. Harder to bisect but faster review. | |
| Single delete + cleanup | One big atomic "remove backup" commit. Loses bisectability; violates PROJECT.md intent. | |

**User's choice:** Reverse-import order
**Notes:** Recorded as D-30. Seven-commit sequence spelled out with exact scopes per commit.

### PR-C internal ordering

| Option | Description | Selected |
|--------|-------------|----------|
| **Move → delete → collapse** | (1) `git mv` sync/readbuffer/gc → engine/ (blame preserved); (2) delete COW remnants + metadata fields in new locations; (3) collapse 5 parsers → 2 in engine/types.go. | ✓ |
| Collapse → move → delete | Parsers first, then move, then delete. `git mv` fights edited parsers; blame loss risk. | |
| Delete → move → collapse | Remove dead code at old locations, move survivors, then collapse. Smaller moves but blame loss on edited-then-moved code. | |

**User's choice:** Move → delete → collapse
**Notes:** Recorded as D-31. Three logical blocks; per-TD-id atomic-commit rule from D-11 applies within each block.

### Skipped (left as planner discretion)

- **TD-02 regression tests** — default: targeted regression test per fixed bug (goroutine leak / error swallow / .blk leak / write-path FileBlockStore). Matches PROJECT.md "each step compiles and passes tests independently."
- **PR landing sequence** — already answered by D-10: Phase 08 lands before Phase 09 rebases onto develop. No need to re-decide.

## Claude's Discretion

- D-04: SUPERSEDED note in phase 08 CONTEXT, leave phase 05 CONTEXT historical (user said "you decide what's best for gsd workflow").

## Deferred Ideas

- Reintroduced in v0.15.0: `FileAttr.Blocks` (A3), `FileAttr.ObjectID` (A4), `ParseCASKey` (A2), `ParseStoreKey` removal (A6).
- v0.16.0: New backup system atop CAS; `BackupHold` reinvented as retention mechanism.
- Out of phase: whole-repo lint sweep, test reorganization, regression-tests-for-deleted-routes, OTel retention decision.
