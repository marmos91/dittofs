# Phase 08: Pre-refactor cleanup (A0) — Context

**Gathered:** 2026-04-23
**Status:** Ready for planning
**Milestone:** v0.15.0 Block Store + Core-Flow Refactor
**GH issue:** [#420](https://github.com/marmos91/dittofs/issues/420)
**Requirements:** TD-01, TD-02, TD-03, TD-04

<domain>
## Phase Boundary

Land low-risk, high-value cleanup that simplifies the A1 starting point.

**Core work (from ROADMAP.md):**
1. **TD-01**: Merge `pkg/blockstore/readbuffer/`, `pkg/blockstore/sync/`, `pkg/blockstore/gc/` into `pkg/blockstore/engine/`.
2. **TD-02**: Fix 4 HIGH-severity bugs — `FSStore.Start()` goroutine leak; `syncFileBlock` error swallowing; `engine.Delete` `.blk` leak; local tier calling `FileBlockStore` on write hot path.
3. **TD-03**: Delete dead scaffolding — `BackupHoldProvider`, `FinalizationCallback`, `ReadAtWithCOWSource`/`readFromCOWSource`, `COWSourcePayloadID`, unused `FileAttr.Blocks []string`, unset `FileAttr.ObjectID`.
4. **TD-04**: Collapse block-key parsers.

**Scope expansion (decided in discuss):** Full removal of the unreleased v0.13.0 backup system folded into TD-03 (see D-01).

**Out of scope:** Any new CAS/FastCDC/BLAKE3 behavior (A1+). Any feature additions.

</domain>

<decisions>
## Implementation Decisions

### Scaffolding & Backup Removal

- **D-01:** **Full v0.13.0 backup system removal.** Live-code audit (2026-04-23, plan-checker iteration 1) expanded the scope beyond the original D-01 enumeration. Complete inventory:

  **Packages (delete whole tree):**
  - `pkg/backup/` (39 Go files across manifest/scheduler/destination/executor/errors/restore + root-level `backupable.go`, `clock.go`, `backupable_test.go`, `concurrent_write_backup_restore_test.go`)
  - `pkg/controlplane/runtime/storebackups/` (15 files incl. `backup_hold.go`, `metrics.go` OTel wiring)

  **Metadata-store surface:**
  - `pkg/metadata/backup.go` (shim defining `Backupable` / `PayloadIDSet` / `ErrBackupUnsupported` aliases)
  - `pkg/metadata/backup_shim_test.go`
  - `pkg/metadata/storetest/backup_conformance.go` (conformance suite) + any reference in `storetest/suite.go`
  - `pkg/metadata/store/badger/backup.go` + `backup_test.go`
  - `pkg/metadata/store/memory/backup.go` + `backup_test.go`
  - `pkg/metadata/store/postgres/backup.go` + `backup_test.go`

  **Persistence / GORM models:**
  - `pkg/controlplane/store/backup.go` + `backup_test.go` (GORM `BackupStore` impl)
  - `pkg/controlplane/store/interface.go` — remove `BackupStore interface` (lines 369-503), remove embedded `BackupStore` on line 700, remove `ErrInvalidProgress` + `BackupJobFilter` (lines 23-42), remove `GORMStore.UpdateBackupRepo` + siblings
  - `pkg/controlplane/models/backup.go` + `backup_test.go`
  - `pkg/controlplane/models/models.go` — remove `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` from `AllModels()` (lines 22-24)

  **REST surface (TWO routers):**
  - `internal/controlplane/api/handlers/backup*.go` (6 files incl. tests) + `internal/controlplane/api/router.go` wiring
  - `pkg/controlplane/api/router.go` — remove `pkg/backup/destination` import (line 17), remove backup route block (lines 248-288: `backupHandler.TriggerBackup/ListRecords/ShowRecord/PatchRecord/ListJobs/GetJob/CancelJob/Restore/RestoreDryRun/CreateRepo/ListRepos/GetRepo/PatchRepo/DeleteRepo`)

  **CLI entrypoints (TWO binaries):**
  - `cmd/dfsctl/commands/store/metadata/backup/` (30+ files: list/run/show/pin/unpin/poll/job/restore/repo subtree)
  - `cmd/dfs/commands/backup/` (`backup.go`, `controlplane.go` — dfs-binary subcommand)
  - `cmd/dfs/commands/restore/` (`controlplane.go`, `restore.go` — entangled with cmd/dfs/commands/backup via imports)
  - `cmd/dfs/commands/root.go` — unregister backup subcommand

  **API client:**
  - `pkg/apiclient/backups.go`, `backup_jobs.go`, `backup_repos.go`, `backups_test.go`

  **E2E tests:**
  - `test/e2e/backup_matrix_test.go`, `backup_restore_mounted_test.go`, `backup_chaos_test.go`, `backup_test.go`
  - `test/e2e/helpers/backup.go`, `helpers/backup_metadata.go`

  **Docs:**
  - `docs/BACKUP.md`; audit `docs/{ARCHITECTURE,CLI,FAQ}.md`, `README.md` for backup references

  Safe because v0.13.0 was never released (see MEMORY.md). v0.16.0 will rebuild backup atop CAS.
- **D-02:** **Delete `COWSourcePayloadID` from both layers** — `FileAttr` (`pkg/metadata/file_types.go:72`) and `PendingWriteIntent` (`pkg/metadata/pending_writes.go:33-34,99`). Drop setter in all `CopyPayload` call sites.
- **D-03:** **`FinalizationCallback` hard delete.** Grep-verify zero non-test consumers first, then delete type + setter (`pkg/blockstore/sync/syncer.go:40,109-110`) + field from `Syncer` in a single commit.
- **D-04:** **Phase 05 CONTEXT.md retcon — Claude's discretion.** Leave `05-CONTEXT.md D-11/D-12` as historical artifact. Add a SUPERSEDED note in this document (see `<specifics>` below) so downstream readers can resolve the cross-reference.

### Metadata Field Removal & On-Disk Compat

- **D-05:** **A3/A4 reintroduction signal is documentation-only.** Add an inline note in `.planning/REQUIREMENTS.md` next to TD-03 stating A3/A4 will reintroduce `ObjectID` (BLAKE3 Merkle root) and `Blocks` (`[]BlockRef`) with new types. No code breadcrumb — git history is the record.
- **D-06:** **Delete COW-era test assignments outright.** Remove any test fixture that sets `ObjectID`, `Blocks []string`, or `COWSourcePayloadID`. Delete COW-specific tests entirely (e.g., `ReadAtWithCOWSource` coverage). `pkg/metadata/storetest/` conformance suite stays but drops COW assertions.
- **D-07:** **Rely on `encoding/json` tolerance for stale data.** No SQL migration. Stale `pending_writes.pre_write_attr` JSONB rows with `cow_source`/`object_id`/`blocks` keys silently unmarshal (Go drops unknown fields); next update re-marshals without them. All three omitempty fields means stale rows rarely even carry these keys.
- **D-08:** **Update GH issue #420 description before PR-A lands.** Reflect scope expansion: "Also removes v0.13.0 backup system (pkg/backup, storebackups, API handlers, scheduler, dfsctl backup commands)."

### PR Structure & Sequencing

- **D-09:** **3 themed PRs.**
  - **PR-A** — TD-02 bug fixes in-place (smallest, lands first, fast review).
  - **PR-B** — Full v0.13.0 backup system removal (largest, self-contained theme).
  - **PR-C** — TD-01 merge + TD-03 remnants (COW, FinalizationCallback) + TD-04 parser collapse — structural cleanup in new `engine/` layout.
- **D-10:** **Phase 08 lands before Phase 09 (ADAPT) rebases.** A0 stabilizes the `engine/` surface; ADAPT depends on that surface being stable. Phase 09 work starts rebasing onto `develop` only after all 3 Phase 08 PRs merge.
- **D-11:** **Commit granularity: one commit per TD-id sub-item.** Each commit independently compiles and passes `go test ./... -race`. Example: `fix(blockstore): join FSStore.Start goroutine on Close (TD-02a)`, `fix(blockstore): propagate syncFileBlock errors (TD-02b)`, etc. Preserves PROJECT.md constraint and enables bisect.
- **D-12:** **All work lands on `develop`.** Per MEMORY.md release-flow rule. No intermediate tag during v0.15.0; milestone tag is cut on `main` after all 8 phases complete.
- **D-30:** **PR-B commit staging — reverse-import order, 10 commits.** (Original 7-commit plan expanded in plan-checker iteration 1 once full scope was enumerated — see updated D-01. Reverse-import order still applies.) Delete dependency-leaf-first so each commit compiles + passes `go test -race`:
  1. `test: remove v0.13.0 backup e2e tests` — `test/e2e/backup_*.go` (4 files) + `test/e2e/helpers/backup*.go` (2 files).
  2. `api: remove v0.13.0 backup REST surface + CLI` — `internal/controlplane/api/handlers/backup*.go` (6 files) + `pkg/controlplane/api/router.go` backup route block + `pkg/backup/destination` import; `pkg/apiclient/backup*.go` (4 files); `cmd/dfsctl/commands/store/metadata/backup/` (30+ files); `cmd/dfs/commands/backup/` + `cmd/dfs/commands/restore/` + unregister from `cmd/dfs/commands/root.go`.
  3. `runtime: drop storebackups wiring` — `runtime.go` field/import/builder-calls/`SetRestoreBumpBootVerifier`/startup-ordering + 8 delegation methods (lines 417-498) + `blockgc.go` SAFETY-01 gate (lines 42-66) + `SetBackupHoldWiringForTest` + `blockgc_test.go` import/test cleanup.
  4. `store: remove BackupStore persistence + GORM surface` — `pkg/controlplane/store/backup.go` + `backup_test.go`; `pkg/controlplane/store/interface.go` drop `BackupStore interface` block + embedded interface + `ErrInvalidProgress` + `BackupJobFilter`; `pkg/controlplane/models/backup.go` + `backup_test.go`; `pkg/controlplane/models/models.go` remove 3 registration lines from `AllModels()`.
  5. `metadata: remove Backupable shim + per-backend impls + conformance` — `pkg/metadata/backup.go` + `backup_shim_test.go`; `pkg/metadata/storetest/backup_conformance.go` + update `storetest/suite.go` to drop conformance step; `pkg/metadata/store/{badger,memory,postgres}/backup.go` + `backup_test.go`.
  6. `runtime: remove storebackups package` — `pkg/controlplane/runtime/storebackups/` whole tree (includes `backup_hold.go`, `metrics.go` OTel wiring).
  7. `backup: remove pkg/backup` — whole package tree (manifest/scheduler/destination/executor/errors/restore + root-level `backupable.go`, `clock.go`, `backupable_test.go`, `concurrent_write_backup_restore_test.go`).
  8. `docs: remove v0.13.0 backup docs + audit` — delete `docs/BACKUP.md`; prune backup refs from `docs/{ARCHITECTURE,CLI,FAQ}.md`, `README.md`; add v0.15.0 release-note line.
  9. `build: go mod tidy + OTel audit` — tidy; drop orphaned direct deps; if backup was sole OTel consumer, drop `go.opentelemetry.io/otel` from `go.mod`.
  10. (Reserved — planner may subdivide commits 4 or 5 if size warrants for reviewability.)
  Each commit independently green → full bisect through PR-B.
- **D-31:** **PR-C internal ordering — move → delete → collapse.** Three logical blocks within PR-C, executed in order:
  1. **TD-01 (move):** `git mv` `pkg/blockstore/readbuffer/`, `pkg/blockstore/sync/`, `pkg/blockstore/gc/` contents into `pkg/blockstore/engine/` per D-17 layout. Rename at move (readbuffer → cache). Update all imports. Blame preserved via `git mv`. Delete three `doc.go` files, consolidate into `engine/doc.go`.
  2. **TD-03 (delete remnants):** In their new `engine/` home, delete `ReadAtWithCOWSource`, `readFromCOWSource`, `FinalizationCallback`, consumer wiring. Also delete `FileAttr.ObjectID`, `FileAttr.Blocks []string`, `FileAttr.COWSourcePayloadID`, `PendingWriteIntent.COWSourcePayloadID` + all setter call sites.
  3. **TD-04 (collapse):** In `pkg/blockstore/types.go`, collapse 5 parsers → 2 per D-13 (ParseStoreKey external + ParseBlockID internal). Delete `parseStoreKeyBlockIdx` (engine/syncer.go), `parsePayloadIDFromBlockKey` (engine/gc.go), `parseBlockID` (local/fs/recovery.go), `extractBlockIdx` (local/fs/manage.go).
  **Why this order:** Moving first preserves `git mv` blame; deletion happens once in final locations; parser collapse runs in one consolidated package rather than crossing package boundaries mid-refactor. Each of the three blocks is a commit group — the per-TD-id atomic-commit rule (D-11) still applies within each block.

### Parser Collapse (TD-04)

- **D-13:** **Two canonical parsers, not one.** Roadmap framed this as "4 → 1" but the 5 parsers actually split into two distinct formats:
  - External store key format `{payloadID}/block-{N}` — collapsed into `ParseStoreKey` (3 parsers → 1): `ParseStoreKey` (types.go) keeps, `parseStoreKeyBlockIdx` (sync/syncer.go) + `parsePayloadIDFromBlockKey` (gc/gc.go) delete.
  - Internal blockID format `{payloadID}/{blockIdx}` — collapsed into `ParseBlockID` (2 parsers → 1): `parseBlockID` (local/fs/recovery.go) + `extractBlockIdx` (local/fs/manage.go) fold into one canonical `ParseBlockID`.
  Net: **5 → 2**. Correct the REQUIREMENTS.md TD-04 wording during the same PR.
- **D-14:** **No CAS parser prep in Phase 08.** A2 (Phase 11) adds `ParseCASKey` for `cas/XX/YY/hash` format alongside `ParseStoreKey`. Dual-parser coexistence during A2–A5 migration window per MIG-03.
- **D-15:** **Both parsers live in `pkg/blockstore/types.go`.** Existing convention — `ParseStoreKey` already there; `ParseBlockID` joins it.
- **D-16:** **`ParseStoreKey` exported + stable through A5.** TD-10 in A6 removes it after the dual-read migration window closes.

### Code Structure / Merge Layout

- **D-17:** **Flat `engine/` layout with rename-at-move.**
  - `readbuffer/readbuffer.go` → `engine/cache.go` (role rename aligns with A3's CACHE-01).
  - `readbuffer/prefetch.go` → `engine/prefetch.go`.
  - `sync/syncer.go` → `engine/syncer.go`.
  - `sync/upload.go` → `engine/upload.go`.
  - `sync/queue.go` → `engine/sync_queue.go` (prefix to avoid future collisions).
  - `sync/health.go` → `engine/sync_health.go` (same reason).
  - `sync/fetch.go` → `engine/fetch.go`.
  - `sync/dedup.go` → `engine/dedup.go`.
  - `sync/entry.go` → `engine/sync_entry.go`.
  - `sync/types.go` → fold into `engine/engine.go` or a new `engine/types.go` (planner decides based on symbol collision).
  - `gc/gc.go` → `engine/gc.go`.
  - Consolidate three `doc.go` files into `engine/doc.go`.
- **D-18:** **Tests move with their code.** `sync/syncer_test.go` → `engine/syncer_test.go`, etc. Preserve `_integration_test.go` suffix. No reorganization in Phase 08 — matches git-blame preservation.
- **D-19:** **Strict write-path isolation.** Zero `FileBlockStore` imports on the local write hot path after this phase. Eviction driven from on-disk state only. Aligns with STATE-03 invariant that A2 will further enforce.
- **D-20:** **Lint sweep scoped to touched packages.** `go vet ./pkg/blockstore/...` + `staticcheck` on the same. Fix findings introduced by the moves/renames. Do NOT expand to whole-repo lint hygiene.

### Runtime Wiring Cleanup

- **D-21:** **Runtime.go fully drops backup integration.** Live-code audit expanded the removal scope. Remove:
  - `storeBackupsSvc *storebackups.Service` field (line 66).
  - Import at line 18.
  - All 5 builder calls in `storebackups.New(...)` block (lines 120-125): `WithShares`, `WithStores`, `WithMetadataConfigs`, and the resolver.
  - `SetRestoreBumpBootVerifier` method (lines 132-145; CONTEXT originally said `SetBumpBootVerifier`, actual symbol differs) + audit adapter call sites and remove them.
  - Startup ordering block (lines 395-400) that starts backup scheduler before API server.
  - **All 8 delegation methods at lines 417-498:** `RegisterBackupRepo`, `UnregisterBackupRepo`, `UpdateBackupRepo`, `RunBackup`, `ValidateBackupSchedule`, `BackupStore()`, `DestFactoryFn()`, `StoreBackupsService()` — discovered in plan-checker iteration 1.
  - **`pkg/controlplane/runtime/blockgc.go`:** delete SAFETY-01 gate (lines 42-66 — the entire resolved-hold logic using `r.BackupStore()` + `r.DestFactoryFn()` + `storebackups.NewBackupHold`), `NewBackupHold` call site (line 52), `SetBackupHoldWiringForTest` (lines 121-139 / 130-139), and remove `gc.Options.BackupHold` from `gc.CollectGarbage` call. Also update `blockgc_test.go` (lines 15, 129) to drop storebackups import + `SetBackupHoldWiringForTest` call.
  - Any shutdown path contribution to `Runtime.Serve` exit.

### API & CLI Client Cleanup

- **D-22:** **Full apiclient + dfsctl delete in PR-B.**
  - `pkg/apiclient/backups.go`, `backup_jobs.go`, `backup_repos.go`, `backups_test.go` — all gone.
  - `cmd/dfsctl/commands/store/metadata/backup/` subtree (list/run/show/pin/unpin/poll/job/restore/repo + tests) — all gone.
  - No shim, no deprecation warning — v0.13.0 was never released.
- **D-23:** **Docs cleanup.** Delete `docs/BACKUP.md` (377 lines). Audit `docs/ARCHITECTURE.md`, `docs/CLI.md`, `README.md`, `docs/FAQ.md` for backup references and prune sections. Add a line to `README.md` or v0.15.0 release notes: "v0.13.0 backup system removed, v0.16.0 will reintroduce on CAS foundation."
- **D-24:** **Changelog deferred to v0.15.0 release notes.** No per-phase CHANGELOG entry during v0.15.0. At milestone completion, release notes include: "BREAKING: Removed v0.13.0 backup system (pkg/backup, dfsctl backup commands, REST API). Never released to production — v0.16.0 will reintroduce atomically atop CAS."
- **D-25:** **K8s operator is unchanged.** The only "Backup" reference in CRDs is `PerconaBackupConfig` (`k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go:324-363`), which configures pgBackRest S3 backups of the Percona PostgreSQL metadata store — unrelated to DittoFS's own backup system. No operator work needed.

### Test, Dependency & Observability Cleanup

- **D-26:** **Delete all e2e backup tests + helpers in PR-B.** `test/e2e/backup_matrix_test.go`, `backup_restore_mounted_test.go`, `backup_chaos_test.go`, `backup_test.go`, `test/e2e/helpers/backup.go`, `helpers/backup_metadata.go` — all gone.
- **D-27:** **Run `go mod tidy` in PR-B; prune orphaned direct deps.** After `pkg/backup/` and `storebackups/` deletion, run `go mod tidy` and audit the `require` block. Any top-level dep that was exclusively backup-consumed gets removed from `go.mod`. Regenerate `go.sum`.
- **D-28:** **Remove backup-specific OTel spans; audit other OTel consumers.** `pkg/controlplane/runtime/storebackups/metrics.go` wires OTel spans for `RunBackup`/`RunRestore`. Delete those. Grep `go.opentelemetry.io/otel` imports across the repo — if backup is the only consumer, drop the dep from `go.mod` too; otherwise keep.
- **D-29:** **No explicit regression test for deleted routes.** The compile + existing API router tests naturally catch residual wiring. A dedicated "these routes should return 404" test is low-value for code that cannot be built.

### Claude's Discretion

- Exact shape of `ParseBlockID` signature (D-13) — planner decides return type based on existing callers.
- Where `sync/types.go` symbols land during merge (D-17) — planner picks based on collision audit.
- SUPERSEDED note wording in this document (D-04).
- The point at which `go mod tidy` runs within PR-B's commit sequence (D-27) — after deletions, before final lint.

### Folded Todos

None — no pending todos from the backlog matched Phase 08 scope.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner, executor) MUST read these before acting.**

### Roadmap & Requirements
- `.planning/ROADMAP.md` §"Phase 08: Pre-refactor cleanup (A0)" (lines 42-58) — success criteria, files to touch, key risks
- `.planning/REQUIREMENTS.md` — TD-01..TD-04 (lines 88-91); traceability table (lines 199-202)
- `.planning/PROJECT.md` §"Current Milestone: v0.15.0" — milestone core value
- `.planning/PROJECT.md` §"Constraints" — "Each step must compile and pass all tests independently"

### Predecessor Phase Context (historical; SUPERSEDED in D-04)
- `.planning/milestones/v2.0-phases/05-restore-orchestration-safety-rails/05-CONTEXT.md` D-11, D-12 — BackupHold/retention-hold design from v2.0 (superseded; backup system removed in this phase)

### Source Files To Touch (Phase 08 work)

**TD-01 merge targets:**
- `pkg/blockstore/engine/engine.go` — primary merge destination; current location of `ReadAtWithCOWSource` (line 189-191, 586, 643-644) to delete
- `pkg/blockstore/readbuffer/readbuffer.go`, `prefetch.go`, `doc.go` — source files to merge (and rename: readbuffer → cache)
- `pkg/blockstore/sync/syncer.go`, `upload.go`, `queue.go`, `fetch.go`, `health.go`, `dedup.go`, `entry.go`, `types.go`, `doc.go` — source files to merge
- `pkg/blockstore/gc/gc.go`, `doc.go` — source files to merge

**TD-02 bug sites:**
- `pkg/blockstore/local/fs/fs.go` — `FSStore.Start()` goroutine to join on `Close()` (TD-02a)
- `pkg/blockstore/sync/syncer.go` — `syncFileBlock` error propagation (TD-02b)
- `pkg/blockstore/engine/engine.go` — `Delete` to call `DeleteAllBlockFiles` (TD-02c)
- `pkg/blockstore/local/fs/fs.go`, `write.go`, `eviction.go` — write-path isolation from `FileBlockStore` (TD-02d)

**TD-03 scaffolding deletion sites:**
- `pkg/blockstore/gc/gc.go:48,58,68,72,76` — `BackupHoldProvider` type + `StaticBackupHold`
- `pkg/blockstore/sync/syncer.go:40-42,53,109-110` — `FinalizationCallback` type + setter + field
- `pkg/blockstore/engine/engine.go:189-191,586,643-644` — `ReadAtWithCOWSource`, `readFromCOWSource`
- `pkg/metadata/file_types.go:66,72,93` — `ObjectID`, `COWSourcePayloadID`, `Blocks []string`
- `pkg/metadata/pending_writes.go:33-34,99` — `PendingWriteIntent.COWSourcePayloadID`
- `pkg/controlplane/runtime/storebackups/backup_hold.go` — consumer to delete (also whole package per D-01)
- `pkg/controlplane/runtime/blockgc.go:52` — `NewBackupHold` call site
- `pkg/controlplane/runtime/runtime.go:18,66,120-125,133-144,395-400` — Runtime wiring to delete
- `pkg/blockstore/store.go:60-61` — `ReadAtWithCOWSource` method from `BlockStore` interface

**TD-04 parser collapse sites:**
- `pkg/blockstore/types.go:181` — canonical `ParseStoreKey` (keep)
- `pkg/blockstore/local/fs/recovery.go:124` — `parseBlockID` (fold into `ParseBlockID`)
- `pkg/blockstore/local/fs/manage.go:193` — `extractBlockIdx` (fold into `ParseBlockID`)
- `pkg/blockstore/sync/syncer.go:19` — `parseStoreKeyBlockIdx` (delete, use `ParseStoreKey`)
- `pkg/blockstore/gc/gc.go:263` — `parsePayloadIDFromBlockKey` (delete, use `ParseStoreKey`)

**Backup system removal (D-01, D-21, D-22, D-23, D-26):**
- `pkg/backup/` (full tree: manifest/scheduler/destination/executor/errors/restore subdirs) — **delete whole package**
- `pkg/controlplane/runtime/storebackups/` — **delete whole package**
- `internal/controlplane/api/handlers/backup*.go` (backup_jobs.go, backup_repos.go, backups.go + 3 tests) — **delete**
- `cmd/dfsctl/commands/store/metadata/backup/` (30+ files: list/run/show/pin/unpin/poll/job/restore/repo subtree) — **delete**
- `pkg/apiclient/backup*.go` (backups.go, backup_jobs.go, backup_repos.go, backups_test.go) — **delete**
- `test/e2e/backup_*.go` (4 files) + `test/e2e/helpers/backup*.go` (2 files) — **delete**
- `docs/BACKUP.md` — **delete**; audit `docs/ARCHITECTURE.md`, `docs/CLI.md`, `README.md`, `docs/FAQ.md` for backup references
- `pkg/controlplane/runtime/storebackups/metrics.go` — OTel span wiring; delete + audit cross-repo OTel consumers (D-28)

### Conformance Suites Touched
- `pkg/metadata/storetest/` — drop COW-related assertions
- `pkg/blockstore/local/localtest/` — drop any COW-specific cases

### Out of scope but referenced
- `pkg/blockstore/types.go` — `FormatCASKey`, `ContentHash`, `BlockRef` additions happen in A1/A2, NOT this phase
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go:324-363` — `PerconaBackupConfig` is pgBackRest, NOT DittoFS backup; untouched

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- **`ParseStoreKey` in `pkg/blockstore/types.go:181`** — already canonical for external format; `parseStoreKeyBlockIdx` at `sync/syncer.go:19` is already a thin wrapper delegating to it. Collapse is mostly mechanical.
- **Existing `engine/` package** — has `engine.go` + a few tests already (`engine_test.go`, `engine_health_test.go`, `engine_offline_test.go`, `healthcheck_test.go`, `stats_test.go`). Merge targets land here.

### Established Patterns
- **Error propagation via `metadata.ExportError`** — `CLAUDE.md` invariant #6. Any new error paths during bug fixes must return `metadata.ExportError` values, not raw errors.
- **Go encoding/json tolerance** — all three metadata backends (Badger, Postgres, Memory) use `encoding/json` for `FileAttr` serialization. Dropping fields is safe on read-back; `omitempty` means they rarely appear in existing JSON.
- **Test conformance suites** (`pkg/metadata/storetest/`, `pkg/blockstore/local/localtest/`) are source-of-truth for backend behavior. Any backend must pass them.
- **GSD atomic commits per TD-id** matches PROJECT.md constraint "Each step must compile and pass all tests independently."

### Integration Points
- **Runtime composition layer** (`pkg/controlplane/runtime/runtime.go`) — single entrypoint for all operations (see `CLAUDE.md`). After Phase 08, it loses the storebackups sub-service entirely. The remaining sub-services (`adapters/`, `stores/`, `shares/`, `mounts/`, `lifecycle/`, `identity/`) per CLAUDE.md are unaffected.
- **AuthContext threading** — every operation carries `*metadata.AuthContext` (CLAUDE.md invariant #2). Cleanup must preserve this — no bypassing the context in new error paths.
- **Per-share block stores** (CLAUDE.md invariant #4) — `rt.GetBlockStoreForHandle(ctx, handle)` remains the only way to resolve the block store. After backup removal, Runtime still owns block-store lifecycle via `AddShare`/`RemoveShare`.

### Format Split Discovered During Discuss
Phase 08 parser collapse splits into **two formats**, not one (correcting ROADMAP.md wording):
| Format | Example | Callers |
|---|---|---|
| `{payloadID}/block-{N}` | `export/docs/report.pdf/block-0` | `ParseStoreKey` (types.go) canonical; `parseStoreKeyBlockIdx` (sync/syncer.go) + `parsePayloadIDFromBlockKey` (gc/gc.go) duplicates |
| `{payloadID}/{blockIdx}` | `export/docs/report.pdf/0` | `parseBlockID` (local/fs/recovery.go) + `extractBlockIdx` (local/fs/manage.go) |

</code_context>

<specifics>
## Specific Ideas

### SUPERSEDED — Phase 5 BackupHold design

Phase 05 CONTEXT (D-11, D-12) designed a GC retention-hold mechanism (`BackupHoldProvider`) so that backed-up payloads survived garbage collection while their backup records were still valid. **That design is SUPERSEDED by Phase 08 of v0.15.0.** Rationale:

1. v0.13.0 backup (the consumer of `BackupHoldProvider`) was never released.
2. v0.16.0 will rebuild backup atop content-addressable storage (CAS), where block immutability obviates the need for GC holds — every block's key IS its content hash, so deletion requires provable absence from the live set (see GC-03 in REQUIREMENTS.md).
3. Keeping the retention-hold mechanism during v0.15.0 would carry dead complexity into the block store refactor.

`.planning/milestones/v2.0-phases/05-restore-orchestration-safety-rails/05-CONTEXT.md` is left as a historical record and NOT retconned — Phase 05's decisions were correct for their milestone.

### "Each step must compile and pass all tests independently"

PROJECT.md constraint. Concretely this phase:
- No commit may be made that leaves `go build ./...` broken.
- No commit may be made that leaves `go test -race ./...` failing.
- PR-A (bugs) commits are atomic per bug.
- PR-B (backup removal) is staged carefully so each commit passes: start by removing API routes, then handlers, then storebackups, then pkg/backup, then e2e tests, then Runtime wiring, then `go mod tidy`.
- PR-C (TD-01 merge + TD-03 remnants + TD-04 parsers) uses `git mv` for history preservation where possible.

### Sign commits

Per MEMORY.md feedback "Always sign commits (`git commit -S`)" — applies to every commit in this phase.

### Commit-message convention

Per MEMORY.md: no Claude Code mentions, no `Co-Authored-By` lines. Concise messages (e.g., `fix(blockstore): join FSStore.Start goroutine on Close (TD-02a)`).

</specifics>

<deferred>
## Deferred Ideas

### Reintroduced in v0.15.0 later phases
- `FileAttr.Blocks` as `[]BlockRef` — **A3 / Phase 12** (`META-01` in REQUIREMENTS.md).
- `FileAttr.ObjectID` as BLAKE3 Merkle root — **A4 / Phase 13** (`META-02`).
- `ParseCASKey` for `cas/XX/YY/hash` format — **A2 / Phase 11** (`BSCAS-01`).
- `ParseStoreKey` removal — **A6 / Phase 15** (`TD-10` dual-read shim cleanup).

### For v0.16.0
- New backup system atop CAS — per-share atomic backup primitives (see PROJECT.md "Upcoming Milestones").
- `BackupHold` re-invented as retention mechanism, not correctness — CAS handles correctness now.

### Deferred from this phase's discussion
- **Broader lint sweep of the whole repo** — out of scope; separate tech-debt pass (D-20).
- **Test consolidation / reorganization during merge** — out of scope; tests move verbatim with their code (D-18).
- **Regression tests asserting deleted routes return 404** — low value; CI compile + existing router tests cover this (D-29).
- **OTel tracing infrastructure retention** — depends on whether backup was sole OTel consumer (D-28); planner decides during audit.

</deferred>

---

*Phase: 08-pre-refactor-cleanup-a0*
*Context gathered: 2026-04-23*
