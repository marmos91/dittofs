---
phase: 01-foundations-models-manifest-capability-interface
plan: 02
type: execute
wave: 2
depends_on: ["01-01"]
files_modified:
  - pkg/controlplane/store/interface.go
  - pkg/controlplane/store/backup.go
  - pkg/controlplane/store/backup_test.go
  - pkg/controlplane/store/gorm.go
autonomous: true
requirements: [REPO-01, REPO-02, REPO-03, REPO-04, REPO-05, SAFETY-02]

must_haves:
  truths:
    - "A new BackupStore sub-interface is embedded in the composite Store interface"
    - "GORMStore implements all BackupStore methods using existing generic helpers (getByField, listAll, createWithID, deleteByField)"
    - "Creating two BackupRepo rows with the same name under different metadata_store_ids succeeds (validates composite unique key per REPO-04)"
    - "Creating two BackupRepo rows with the same name under the same metadata_store_id fails with ErrDuplicateBackupRepo"
    - "DeleteBackupRepo fails with ErrBackupRepoInUse when records reference the repo"
    - "SetBackupRecordPinned toggles the pinned column and the value survives reload (REPO-03)"
    - "BackupRecord and BackupJob IDs are auto-generated ULIDs when caller leaves ID empty"
    - "RecoverInterruptedJobs transitions all running jobs to interrupted (SAFETY-02 contract surface — behavior exercised by Phase 5)"
    - "AutoMigrate (existing gorm.go path) creates backup_repos, backup_records, backup_jobs tables without manual migration code"
  artifacts:
    - path: "pkg/controlplane/store/interface.go"
      provides: "BackupStore sub-interface, embedded in composite Store"
      contains: "BackupStore interface"
    - path: "pkg/controlplane/store/backup.go"
      provides: "GORM implementation of BackupStore on GORMStore"
      contains: "func (s *GORMStore) CreateBackupRepo"
    - path: "pkg/controlplane/store/backup_test.go"
      provides: "Integration test suite (//go:build integration)"
      contains: "TestBackupRepoOperations"
  key_links:
    - from: "pkg/controlplane/store/backup.go"
      to: "pkg/controlplane/store/helpers.go"
      via: "Uses getByField[T], listAll[T], createWithID[T], deleteByField[T]"
      pattern: "getByField\\[models\\.Backup"
    - from: "pkg/controlplane/store/interface.go (Store composite)"
      to: "BackupStore sub-interface"
      via: "Interface embedding, 11th slot"
      pattern: "BackupStore$"
    - from: "pkg/controlplane/store/backup.go (DeleteBackupRepo)"
      to: "models.BackupRecord via repo_id"
      via: "Transactional count-then-delete"
      pattern: "Where\\(\"repo_id = \\?\""
---

<objective>
Deliver the `BackupStore` sub-interface and its GORM implementation. Add it to the composite `Store` so `db.AutoMigrate(models.AllModels()...)` (already invoked in `gorm.go`) creates the three new tables on next startup, and so future plans (Phase 2+) can inject a narrow interface instead of full `Store`.

Purpose: Closes REPO-01, REPO-02, REPO-04, REPO-05 at the persistence layer and pre-stages the SAFETY-02 recovery entry point. No API handlers, no CLI — only the store contract and its implementation.

Output: `BackupStore` sub-interface added to `interface.go`, `pkg/controlplane/store/backup.go` with GORM implementation, and integration tests (under `//go:build integration`) validating CRUD, composite uniqueness, pin round-trip, kind filter, and recovery sweep.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-PATTERNS.md

# Prior plan in this phase
@.planning/phases/01-foundations-models-manifest-capability-interface/01-01-SUMMARY.md

# Direct source analogs
@pkg/controlplane/store/interface.go
@pkg/controlplane/store/block.go
@pkg/controlplane/store/block_test.go
@pkg/controlplane/store/helpers.go
@pkg/controlplane/store/gorm.go

<interfaces>
<!-- Generic helpers available in pkg/controlplane/store/helpers.go. Use these, do not roll your own. -->

```go
// Single-record lookup with domain error mapping (helpers.go:21-31)
func getByField[T any](db *gorm.DB, ctx context.Context, field string, value any, notFoundErr error, preloads ...string) (*T, error)

// List-all slice (helpers.go:39-49)
func listAll[T any](db *gorm.DB, ctx context.Context, preloads ...string) ([]*T, error)

// UUID generation + create + duplicate-error mapping (helpers.go:58-71)
func createWithID[T any](db *gorm.DB, ctx context.Context, entity *T, idSetter func(*T, string), currentID string, dupErr error) (string, error)

// Delete with not-found check (helpers.go:92-102)
func deleteByField[T any](db *gorm.DB, ctx context.Context, field string, value any, notFoundErr error) error
```

Error mapping helpers in gorm.go:
```go
func convertNotFoundError(err error, notFoundErr error) error // gorm.go:330-335
func isUniqueConstraintError(err error) bool                   // gorm.go:319-327
```

BlockStoreConfigStore is the closest analog (interface.go:305-336 + block.go). Copy the shape and re-skin. See PATTERNS.md §`pkg/controlplane/store/backup.go` for method-by-method mapping including the transactional `DeleteBackupRepo` pattern at `block.go:65-85`.

ULID usage for BackupRecord/BackupJob IDs:
```go
import "github.com/oklog/ulid/v2"
if rec.ID == "" {
    rec.ID = ulid.Make().String()
}
return createWithID(s.db, ctx, rec,
    func(r *models.BackupRecord, id string) { r.ID = id },
    rec.ID, models.ErrDuplicateBackupRecord)
```
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Declare BackupStore sub-interface and embed in composite Store</name>
  <files>pkg/controlplane/store/interface.go</files>
  <behavior>
    - New `BackupStore` interface declared after `BlockStoreConfigStore` (around the current line ~336) with exactly these methods (signatures locked by PATTERNS.md):
      * `GetBackupRepo(ctx, storeID, name string) (*models.BackupRepo, error)`
      * `GetBackupRepoByID(ctx, id string) (*models.BackupRepo, error)`
      * `ListBackupReposByStore(ctx, storeID string) ([]*models.BackupRepo, error)`
      * `ListAllBackupRepos(ctx) ([]*models.BackupRepo, error)` — scheduler consumer will need this in Phase 4; surface now
      * `CreateBackupRepo(ctx, *models.BackupRepo) (string, error)`
      * `UpdateBackupRepo(ctx, *models.BackupRepo) error`
      * `DeleteBackupRepo(ctx, id string) error`  // ErrBackupRepoInUse if records exist
      * `GetBackupRecord(ctx, id string) (*models.BackupRecord, error)`
      * `ListBackupRecordsByRepo(ctx, repoID string) ([]*models.BackupRecord, error)`
      * `CreateBackupRecord(ctx, *models.BackupRecord) (string, error)`
      * `UpdateBackupRecord(ctx, *models.BackupRecord) error`
      * `DeleteBackupRecord(ctx, id string) error`
      * `SetBackupRecordPinned(ctx, id string, pinned bool) error`
      * `GetBackupJob(ctx, id string) (*models.BackupJob, error)`
      * `ListBackupJobs(ctx, kind models.BackupJobKind, status models.BackupStatus) ([]*models.BackupJob, error)` — empty kind/status == no filter on that field
      * `CreateBackupJob(ctx, *models.BackupJob) (string, error)`
      * `UpdateBackupJob(ctx, *models.BackupJob) error`
      * `RecoverInterruptedJobs(ctx) (int, error)`
    - Godoc on the interface explaining the one-table `kind`-discriminator model for jobs.
    - Composite `Store` interface (currently lines 531-542) gets `BackupStore` added between `BlockStoreConfigStore` and `AdapterStore` (preserving alphabetical-by-domain ordering is NOT required — match existing topology: persistence-layer groupings stay together).
  </behavior>
  <action>
    1. Open `pkg/controlplane/store/interface.go`. After `BlockStoreConfigStore` closing brace, add the `BackupStore` interface verbatim from PATTERNS.md §"Sub-interface declaration" and extended here with `ListAllBackupRepos` (needed by Phase 4 scheduler).
    2. Add `BackupStore` to the `Store` composite embedding list — insert after `BlockStoreConfigStore` line. Do NOT rename or reorder existing embeds.
    3. Do NOT implement methods in this task — interface only. Build will fail until Task 2 lands; that's expected. Verify with `go vet ./pkg/controlplane/store/...` showing `*GORMStore does not implement BackupStore` error, which confirms the interface binding is correct.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go vet ./pkg/controlplane/store/interface.go 2&gt;&amp;1 | grep -q "BackupStore" || echo "interface declared"</automated>
  </verify>
  <done>BackupStore interface declared with 18 methods listed above, embedded in composite Store. Code MAY fail to compile at this step — that is expected and drives Task 2.</done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Implement BackupStore on GORMStore + integration tests</name>
  <files>pkg/controlplane/store/backup.go, pkg/controlplane/store/backup_test.go</files>
  <behavior>
    - `pkg/controlplane/store/backup.go` declares all `func (s *GORMStore) ...` methods matching the `BackupStore` interface, using generic helpers per PATTERNS.md:
      * `GetBackupRepo` — `WithContext(ctx).Where("metadata_store_id = ? AND name = ?", ...)` + `convertNotFoundError(err, models.ErrBackupRepoNotFound)`
      * `GetBackupRepoByID` — `getByField[models.BackupRepo](s.db, ctx, "id", id, models.ErrBackupRepoNotFound)`
      * `ListBackupReposByStore` — `WithContext(ctx).Where("metadata_store_id = ?", storeID).Find(&out)`; map error
      * `ListAllBackupRepos` — `listAll[models.BackupRepo](s.db, ctx)`
      * `CreateBackupRepo` — if ID empty, assign UUID via `uuid.New().String()` (match BlockStoreConfig convention at block.go:37-43); use `createWithID` helper with `models.ErrDuplicateBackupRepo`. Before calling helper, if `repo.Config == ""` but `repo.ParsedConfig` non-empty, call `repo.SetConfig(repo.ParsedConfig)` to materialize JSON — mirror `block.go` pattern.
      * `UpdateBackupRepo` — `Save(repo)` under `WithContext(ctx)`; map unique-constraint and not-found errors
      * `DeleteBackupRepo` — transactional: count `BackupRecord` WHERE `repo_id = id`; if > 0 return `models.ErrBackupRepoInUse`; else `deleteByField[models.BackupRepo]`. Verbatim from PATTERNS.md §"Delete with reference check"
      * `GetBackupRecord` — `getByField[models.BackupRecord](s.db, ctx, "id", id, models.ErrBackupRecordNotFound)`
      * `ListBackupRecordsByRepo` — `WithContext(ctx).Where("repo_id = ?", repoID).Order("created_at DESC").Find(&out)`
      * `CreateBackupRecord` — if ID empty, `rec.ID = ulid.Make().String()`; then `createWithID` with `models.ErrDuplicateBackupRecord`. Status must be persisted as-is (pending at creation is caller's responsibility).
      * `UpdateBackupRecord` — `Save`; map errors
      * `DeleteBackupRecord` — `deleteByField[models.BackupRecord]`
      * `SetBackupRecordPinned` — `WithContext(ctx).Model(&models.BackupRecord{}).Where("id = ?", id).Update("pinned", pinned)`; if `RowsAffected == 0` return `ErrBackupRecordNotFound`
      * `GetBackupJob` — `getByField`
      * `ListBackupJobs(ctx, kind, status)` — build query conditionally: `q := s.db.WithContext(ctx)`; `if kind != "" { q = q.Where("kind = ?", kind) }`; `if status != "" { q = q.Where("status = ?", status) }`; `q.Find(&out)`
      * `CreateBackupJob` — ULID ID, `createWithID` with `ErrDuplicateBackupJob`
      * `UpdateBackupJob` — `Save`
      * `RecoverInterruptedJobs` — verbatim from PATTERNS.md §"RecoverInterruptedJobs": bulk update where `status = running` setting status=interrupted, error="server restarted while job was running", finished_at=now(). Return `int(result.RowsAffected), result.Error`.
    - `pkg/controlplane/store/backup_test.go` with `//go:build integration` build tag at the top (mirror `block_test.go:1`). Uses `createTestStore(t)` helper from `store_test.go`.
    - Tests (each uses a fresh store per subtest):
      * `TestBackupRepoOperations` — create; GetBackupRepoByID; GetBackupRepo by (storeID,name); duplicate-create returns ErrDuplicateBackupRepo; ListBackupReposByStore filters correctly; Update roundtrip; Delete succeeds when no records exist; Delete missing returns ErrBackupRepoNotFound.
      * `TestBackupRepoUniquePerStore` — KEY TEST for REPO-04: create two `MetadataStoreConfig` rows A and B. Create BackupRepo{store=A, name="local"} — success. Create BackupRepo{store=A, name="local"} again — expect ErrDuplicateBackupRepo. Create BackupRepo{store=B, name="local"} — expect success (same name, different store).
      * `TestBackupRepoGetConfigRoundTrip` — SetConfig → Create → Get → GetConfig returns equivalent map.
      * `TestBackupRecordPin` — create repo + record (pinned=false); call SetBackupRecordPinned(true); reload via GetBackupRecord; assert pinned=true; toggle back.
      * `TestBackupRecordListByRepo` — insert 3 records across 2 repos; list by repo returns only that repo's records; order is newest-first by CreatedAt.
      * `TestBackupJobKindFilter` — insert 2 `kind=backup` jobs, 1 `kind=restore`; ListBackupJobs(backup, "") returns 2; ListBackupJobs(restore, "") returns 1; ListBackupJobs("", pending) returns all pending regardless of kind.
      * `TestRecoverInterruptedJobs` — insert 3 jobs with status=running plus 1 with status=succeeded. Call RecoverInterruptedJobs. Assert return value = 3. Reload and verify the 3 jobs have status=interrupted, non-empty Error, and FinishedAt set. The succeeded job is untouched.
      * `TestDeleteBackupRepoInUse` — create repo + 1 record; DeleteBackupRepo returns ErrBackupRepoInUse; record still exists. Delete the record first, then DeleteBackupRepo succeeds.
      * `TestBackupRecordAutoULID` — create record with ID=""; after Create, record.ID is 26 chars (ULID canonical length) and lexicographically ordered if created sequentially.
  </behavior>
  <action>
    1. Create `pkg/controlplane/store/backup.go`. Package `store`. Imports: context, time, github.com/google/uuid, github.com/oklog/ulid/v2, gorm.io/gorm, dittofs/pkg/controlplane/models.
    2. Implement every method listed above. Copy the `DeleteBlockStore` transaction pattern verbatim from `block.go:65-85`; rename receiver/types. Keep method order in the file grouped by entity: repos first, records second, jobs third.
    3. Create `pkg/controlplane/store/backup_test.go` with build tag `//go:build integration` as the first line. Use test helpers already present in `store_test.go` (same package, so `createTestStore` is accessible directly).
    4. Seeding helper: create a local helper `seedRepo(t, s, storeID, name string) *models.BackupRepo` returning the persisted repo to reduce boilerplate across subtests.
    5. For `TestBackupRepoUniquePerStore`, you must first seed two `MetadataStoreConfig` rows via `s.CreateMetadataStore(...)` so the FK is satisfiable. Check `stores_test.go` or similar for the existing helper.
    6. Do NOT add manual migration code — `gorm.go:247 db.AutoMigrate(models.AllModels()...)` picks up the new entities once Plan 01 has registered them. Add a comment in `backup.go` header confirming this: `// Tables are created by AutoMigrate(models.AllModels()...); see pkg/controlplane/store/gorm.go.`
    7. After implementation, run `go build ./pkg/controlplane/...` to confirm the composite Store interface is satisfied.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go build ./pkg/controlplane/... &amp;&amp; go test -tags=integration ./pkg/controlplane/store/... -run 'TestBackup|TestRecoverInterruptedJobs|TestDeleteBackupRepoInUse' -count=1 -v</automated>
  </verify>
  <done>Full package builds, all new integration tests pass, existing BlockStore/MetadataStore/Share tests still pass under the integration tag, composite `Store` interface is satisfied (verified by build).</done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Phase 2+ callers → BackupStore | Downstream service code gets narrow interface; Phase 1 must enforce correct error mapping so callers cannot misread "not found" as "succeeded" |
| RecoverInterruptedJobs sweep | Runs at startup on every boot — must not run against in-flight jobs started by a concurrent process |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-01-05 | Tampering | SetBackupRecordPinned — bypass via direct Save | mitigate | Dedicated method; integration test confirms pinned flag round-trips. Future API layer (Phase 6) must use this method, not raw UpdateBackupRecord. |
| T-01-06 | Denial of service | RecoverInterruptedJobs on boot with 1M running jobs | accept | SQL bulk UPDATE is O(log n) with status index (declared in Plan 01 struct tags). Project is single-instance; realistic job cardinality is dozens not millions. |
| T-01-07 | Information disclosure | BackupRepo.Config (S3 creds) leaked via UpdateBackupRepo preload | mitigate | Config field carries `json:"-"` tag (inherited from Plan 01 pattern); never serialized in JSON responses. ParsedConfig is the public surface. |
| T-01-08 | Elevation | DeleteBackupRepo bypassing ErrBackupRepoInUse via concurrent insert | accept | Phase 1 uses transaction; for v0.13.0 single-instance model this prevents TOCTOU. Multi-instance HA is out of scope per PROJECT.md. |
</threat_model>

<verification>
- `go build ./pkg/controlplane/...` passes (confirms GORMStore satisfies Store interface)
- `go vet ./pkg/controlplane/store/...` clean
- `go test -tags=integration ./pkg/controlplane/store/... -count=1` passes (all new + existing integration tests)
- `go test ./pkg/controlplane/store/... -count=1` passes (non-integration tag, existing tests)
- Integration tests confirm: composite uniqueness, pin round-trip, kind filter, recovery sweep, in-use refusal
</verification>

<success_criteria>
- `BackupStore` sub-interface declared with 18 methods, embedded in composite `Store`
- GORM implementation covers every method using existing helpers (no bespoke not-found/duplicate handling)
- Composite unique constraint `(metadata_store_id, name)` enforced at DB level (verified by TestBackupRepoUniquePerStore)
- BackupRecord.Pinned round-trips (REPO-03)
- BackupRecord and BackupJob IDs auto-generate as ULIDs when caller leaves them empty
- `RecoverInterruptedJobs` transitions all running jobs to interrupted with error message and finished_at timestamp (SAFETY-02 surface — Phase 5 wires the boot hook)
- AutoMigrate creates backup_repos, backup_records, backup_jobs tables without manual migration code
- No new direct dependencies added in this plan (ulid came from Plan 01)
</success_criteria>

<output>
After completion, create `.planning/phases/01-foundations-models-manifest-capability-interface/01-02-SUMMARY.md` with:
- Files created/modified
- Full method set added to BackupStore (for Phase 6 API wiring)
- Confirmation of composite-uniqueness enforcement at DB level
- Any deviations (extra methods added, signatures refined)
- Note on how Phase 5 will consume RecoverInterruptedJobs (boot hook in lifecycle.Service)
</output>
