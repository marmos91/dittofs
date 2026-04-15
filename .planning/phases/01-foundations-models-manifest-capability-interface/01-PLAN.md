---
phase: 01-foundations-models-manifest-capability-interface
plan: 01
type: execute
wave: 1
depends_on: []
files_modified:
  - go.mod
  - go.sum
  - pkg/controlplane/models/backup.go
  - pkg/controlplane/models/backup_test.go
  - pkg/controlplane/models/errors.go
  - pkg/controlplane/models/models.go
autonomous: true
requirements: [REPO-01, REPO-02, REPO-03, REPO-04]

must_haves:
  truths:
    - "BackupRepo, BackupRecord, BackupJob compile as GORM entities registered in AllModels()"
    - "Repo names are unique per metadata_store (not globally) — composite unique index on (metadata_store_id, name)"
    - "Retention is encoded as structured columns (keep_count, keep_age_days) on backup_repos — not JSON"
    - "BackupJob uses a single kind enum column (backup|restore) — not two tables"
    - "BackupRecord.pinned persists across reloads for retention safety (REPO-03)"
    - "oklog/ulid/v2 is added to go.mod and used for BackupRecord/BackupJob ID generation"
    - "Unit tests verify TableName(), Get/SetConfig round-trip, enum constants, and ErrBackupRepoInUse sentinel existence"
  artifacts:
    - path: "pkg/controlplane/models/backup.go"
      provides: "BackupRepo, BackupRecord, BackupJob GORM structs, BackupStatus + BackupJobKind enum types"
      contains: "type BackupRepo struct"
    - path: "pkg/controlplane/models/backup_test.go"
      provides: "Model unit tests (TableName, Get/SetConfig, enums)"
      exports: []
    - path: "pkg/controlplane/models/errors.go"
      provides: "New sentinels: ErrBackupRepoNotFound, ErrDuplicateBackupRepo, ErrBackupRepoInUse, ErrBackupRecordNotFound, ErrDuplicateBackupRecord, ErrBackupJobNotFound, ErrDuplicateBackupJob"
      contains: "ErrBackupRepoNotFound"
    - path: "pkg/controlplane/models/models.go"
      provides: "AllModels() includes BackupRepo{}, BackupRecord{}, BackupJob{}"
      contains: "BackupRepo"
    - path: "go.mod"
      provides: "github.com/oklog/ulid/v2 dependency"
      contains: "oklog/ulid/v2"
  key_links:
    - from: "pkg/controlplane/models/backup.go (BackupRepo)"
      to: "pkg/controlplane/models/stores.go (MetadataStoreConfig)"
      via: "FK MetadataStoreID + GORM foreignKey tag"
      pattern: "MetadataStoreConfig .*foreignKey:MetadataStoreID"
    - from: "pkg/controlplane/models/models.go (AllModels)"
      to: "pkg/controlplane/store/gorm.go (AutoMigrate)"
      via: "db.AutoMigrate(models.AllModels()...)"
      pattern: "BackupRepo\\{\\},"
---

<objective>
Create GORM entities for the backup subsystem (repos, records, jobs) along with their sentinel errors and migration registration. This is the schema foundation every other Phase 1 plan depends on.

Purpose: Establishes persistent schema for REPO-01..05. No store operations, no API — just the data shapes, typed enums, and error sentinels that plans 02–04 consume.

Output: `pkg/controlplane/models/backup.go` with 3 GORM entities, tests mirroring `stores_test.go`, sentinel errors added to `errors.go`, `AllModels()` updated, and `oklog/ulid/v2` added as a dependency.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-PATTERNS.md

# Direct source analogs — executor must mirror these
@pkg/controlplane/models/stores.go
@pkg/controlplane/models/stores_test.go
@pkg/controlplane/models/share.go
@pkg/controlplane/models/errors.go
@pkg/controlplane/models/models.go

<interfaces>
<!-- Key patterns from existing codebase. Mirror these exactly. -->

From pkg/controlplane/models/stores.go (BlockStoreConfig — exact analog for BackupRepo):
```go
type BlockStoreKind string
const (
    BlockStoreKindLocal  BlockStoreKind = "local"
    BlockStoreKindRemote BlockStoreKind = "remote"
)

type BlockStoreConfig struct {
    ID           string         `gorm:"primaryKey;size:36" json:"id"`
    Name         string         `gorm:"uniqueIndex;not null;size:255" json:"name"`
    Kind         BlockStoreKind `gorm:"not null;size:10;index" json:"kind"`
    Type         string         `gorm:"not null;size:50" json:"type"`
    Config       string         `gorm:"type:text" json:"-"`
    CreatedAt    time.Time      `gorm:"autoCreateTime" json:"created_at"`
    ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

func (BlockStoreConfig) TableName() string { return "block_store_configs" }

func (c *BlockStoreConfig) GetConfig() (map[string]any, error) { /* JSON unmarshal from Config */ }
func (c *BlockStoreConfig) SetConfig(v map[string]any) error    { /* JSON marshal to Config */ }
```

From pkg/controlplane/models/share.go (FK pattern):
```go
MetadataStoreID string              `gorm:"not null;size:36" json:"metadata_store_id"`
MetadataStore   MetadataStoreConfig `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitempty"`
```

From pkg/controlplane/models/errors.go (sentinel pattern):
```go
var ErrStoreNotFound = errors.New("store not found")
var ErrDuplicateStore = errors.New("store already exists")
var ErrStoreInUse = errors.New("store in use")
```

ULID library usage (per PATTERNS.md §Dependency Verification):
```go
import "github.com/oklog/ulid/v2"
id := ulid.Make().String() // canonical, monotonic, time-prefixed
```
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Add oklog/ulid/v2 dependency and define backup model types</name>
  <files>go.mod, go.sum, pkg/controlplane/models/backup.go, pkg/controlplane/models/errors.go</files>
  <behavior>
    - `github.com/oklog/ulid/v2` is listed in go.mod after `go get github.com/oklog/ulid/v2@latest`
    - `pkg/controlplane/models/backup.go` compiles and exports:
      * `type BackupStatus string` with constants: BackupStatusPending, BackupStatusRunning, BackupStatusSucceeded, BackupStatusFailed, BackupStatusInterrupted
      * `type BackupJobKind string` with constants: BackupJobKindBackup, BackupJobKindRestore
      * `type BackupRepoKind string` with constants: BackupRepoKindLocal = "local", BackupRepoKindS3 = "s3"
      * `BackupRepo` struct with fields: ID (string, UUID, primaryKey size:36), MetadataStoreID (string, not null, size:36, part of composite unique index `idx_backup_repo_store_name`), Name (string, not null, size:255, part of composite unique index `idx_backup_repo_store_name`), Kind (BackupRepoKind, not null, size:10, index), Config (string, type:text, json:"-"), Schedule (*string, nullable, size:255), KeepCount (*int, nullable), KeepAgeDays (*int, nullable), EncryptionEnabled (bool), EncryptionKeyRef (string, size:255), CreatedAt/UpdatedAt (time.Time autoCreateTime/autoUpdateTime), ParsedConfig (map[string]any, gorm:"-"), MetadataStore (MetadataStoreConfig, foreignKey:MetadataStoreID, json:"metadata_store,omitempty")
      * `func (BackupRepo) TableName() string` returns `"backup_repos"`
      * `func (r *BackupRepo) GetConfig() (map[string]any, error)` and `SetConfig(map[string]any) error` — copy verbatim from `stores.go` Get/SetConfig, reading/writing `BackupRepo.Config` and caching `ParsedConfig`
      * `BackupRecord` struct: ID (string, ULID, primaryKey size:36), RepoID (string, not null, size:36, index), CreatedAt (time.Time autoCreateTime), SizeBytes (int64), Status (BackupStatus, not null, size:20, index), Pinned (bool, not null, default:false, index), ManifestPath (string, size:512), SHA256 (string, size:64), StoreID (string, size:36) — FK snapshot for restore guard, Error (string, type:text), Repo (BackupRepo, foreignKey:RepoID, json:"repo,omitempty")
      * `func (BackupRecord) TableName() string` returns `"backup_records"`
      * `BackupJob` struct: ID (string, ULID, primaryKey size:36), Kind (BackupJobKind, not null, size:10, index), RepoID (string, not null, size:36, index), BackupRecordID (*string, nullable, size:36) — set only when Kind=restore, Status (BackupStatus, not null, size:20, index), StartedAt (*time.Time), FinishedAt (*time.Time), Error (string, type:text), Progress (int) — 0-100
      * `func (BackupJob) TableName() string` returns `"backup_jobs"`
    - `pkg/controlplane/models/errors.go` appended with sentinels: `ErrBackupRepoNotFound`, `ErrDuplicateBackupRepo`, `ErrBackupRepoInUse`, `ErrBackupRecordNotFound`, `ErrDuplicateBackupRecord`, `ErrBackupJobNotFound`, `ErrDuplicateBackupJob` — all via `errors.New(...)` pattern identical to `ErrStoreNotFound`
  </behavior>
  <action>
    1. Run `go get github.com/oklog/ulid/v2@latest` to add the dep. Verify it appears in go.mod.
    2. Create `pkg/controlplane/models/backup.go` mirroring `stores.go` structure exactly (same order: package/imports/enum-type-and-constants/struct/TableName/GetConfig/SetConfig). Copy Get/SetConfig from `stores.go:84-108` verbatim, rename receiver to `*BackupRepo` and ParsedConfig cache field. For BackupRecord and BackupJob no Get/SetConfig is needed (no opaque config blob).
    3. CRITICAL for composite unique constraint (REPO-04 invariant — "same repo name allowed on different stores"): use shared index name on BOTH columns. In GORM this is: `MetadataStoreID string \`gorm:"not null;size:36;uniqueIndex:idx_backup_repo_store_name" json:"metadata_store_id"\`` AND `Name string \`gorm:"not null;size:255;uniqueIndex:idx_backup_repo_store_name" json:"name"\``. Same index name on both columns produces a composite unique index. Do NOT use single-column `uniqueIndex` on Name alone.
    4. Retention columns must be pointers (`*int`) so they can be NULL (nullable = no policy set). Schedule is `*string` for the same reason.
    5. Do NOT store encryption keys — only `EncryptionKeyRef` (env var name or file path).
    6. Append sentinels to `pkg/controlplane/models/errors.go` in the backup group with a short comment header like `// Backup sentinels (v0.13.0)`.
    7. Do NOT modify `AllModels()` or `stores_test.go` in this task — that's Task 2.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go build ./pkg/controlplane/models/... &amp;&amp; go vet ./pkg/controlplane/models/...</automated>
  </verify>
  <done>go build succeeds, go vet clean, go.mod contains oklog/ulid/v2, backup.go exports all types listed above, errors.go contains all 7 new sentinels.</done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Register migrations and add model unit tests</name>
  <files>pkg/controlplane/models/models.go, pkg/controlplane/models/backup_test.go</files>
  <behavior>
    - `AllModels()` returns slice that includes `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` appended after `&NetgroupMember{}`, preserving existing order.
    - `pkg/controlplane/models/backup_test.go` is a plain Go test file (NO build tag — mirrors `stores_test.go:1` which has no tag) that tests:
      * `TestBackupRepoTableName` — asserts `(BackupRepo{}).TableName() == "backup_repos"`
      * `TestBackupRecordTableName` — asserts `"backup_records"`
      * `TestBackupJobTableName` — asserts `"backup_jobs"`
      * `TestBackupStatusConstants` — asserts all 5 BackupStatus values have expected string values (pending/running/succeeded/failed/interrupted)
      * `TestBackupJobKindConstants` — asserts both BackupJobKind values (backup/restore)
      * `TestBackupRepoKindConstants` — asserts both BackupRepoKind values (local/s3)
      * `TestBackupRepoGetSetConfig` — copy shape of `TestBlockStoreConfigGetSetConfig` (stores_test.go:23-41): SetConfig({"bucket":"my-bucket","region":"us-east-1"}) → GetConfig returns equivalent map → verify `repo.Config` string is non-empty JSON
      * `TestBackupRepoEmptyGetConfig` — empty Config string → GetConfig returns empty-or-nil map, no error (mirror stores_test.go:43-52)
      * `TestBackupRepoGetConfigCached` — call GetConfig twice, verify ParsedConfig cache is populated on second call (mirror stores_test.go pattern if present)
      * `TestBackupSentinelsDistinct` — assert all 7 new sentinels are non-nil and pairwise not `errors.Is` equal (e.g. `errors.Is(ErrBackupRepoNotFound, ErrBackupRecordNotFound)` is false)
      * `TestAllModelsIncludesBackup` — assert `AllModels()` contains BackupRepo, BackupRecord, BackupJob by type switch or reflection
  </behavior>
  <action>
    1. Modify `pkg/controlplane/models/models.go`: append `&BackupRepo{}`, `&BackupRecord{}`, `&BackupJob{}` to `AllModels()` in that order, immediately after `&NetgroupMember{}`. Preserve trailing comma style from existing entries.
    2. Create `pkg/controlplane/models/backup_test.go`. Use `package models` (not `_test`) to access unexported fields if needed, same as `stores_test.go`. Import `testing`, `errors`, and whatever else is needed.
    3. Tests must be self-contained — no DB, no filesystem. This file MUST NOT have `//go:build integration` tag.
    4. For `TestAllModelsIncludesBackup`: loop `AllModels()`, type-assert each element, count the three backup types, require all three present.
    5. Every test function starts with `func Test...(t *testing.T)` and uses `t.Run` / `require` / `assert` as matches project convention (check stores_test.go for which assertion library the project uses — use the same).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go test ./pkg/controlplane/models/... -run 'TestBackup|TestAllModelsIncludesBackup' -count=1 -v</automated>
  </verify>
  <done>All backup model tests pass, AllModels() includes the 3 new entities, existing models tests still pass (run full `go test ./pkg/controlplane/models/...` as a smoke check).</done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| operator → control plane DB | Operator creates BackupRepo rows via future API (Phase 6); for Phase 1, only migration-level concerns |
| backup record storage | Records carry SHA-256 and StoreID; consumed by future restore verification (Phase 5) |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-01-01 | Tampering | BackupRepo.EncryptionKeyRef field | mitigate | Field stores only a reference (env var name / path), never the key. Unit test verifies type is `string` not `[]byte`; code review gate for Phase 3 when consumer lands. |
| T-01-02 | Information disclosure | BackupRepo.Config JSON blob (S3 credentials) | accept | Config blob follows existing BlockStoreConfig pattern (stores.go) which already holds S3 credentials; existing control-plane DB auth is the trust boundary. No regression in Phase 1. |
| T-01-03 | Tampering | BackupRecord.SHA256 field | mitigate | Column stored as read-only after creation by convention. Field is immutable post-creation — future `UpdateBackupRecord` path (Plan 02) must not permit mutation; checker verifies in Phase 5 restore path. |
| T-01-04 | Repudiation | BackupJob state transitions lose history | accept | Phase 1 stores only current state; job history would require audit log infra (out of scope for milestone). Timestamps (StartedAt/FinishedAt) give minimal forensic signal. |
</threat_model>

<verification>
- `go build ./pkg/controlplane/models/...` passes
- `go vet ./pkg/controlplane/models/...` passes
- `go test ./pkg/controlplane/models/... -count=1` passes (all new and existing tests)
- `grep -c "oklog/ulid/v2" go.mod` ≥ 1
- `grep -c "BackupRepo{}" pkg/controlplane/models/models.go` == 1
- `grep -c "idx_backup_repo_store_name" pkg/controlplane/models/backup.go` ≥ 2 (same index name on both MetadataStoreID and Name)
</verification>

<success_criteria>
- BackupRepo, BackupRecord, BackupJob GORM entities exist with locked field set and constraints
- Composite unique index `(metadata_store_id, name)` encoded correctly (REPO-04)
- Retention encoded as structured `*int` columns, NOT JSON (locked decision)
- Single `backup_jobs` table with `kind` enum (locked decision)
- `Pinned` column persists (REPO-03)
- oklog/ulid/v2 in go.mod
- All 7 sentinel errors defined and distinct
- AllModels() registers the 3 new entities so AutoMigrate creates tables
- All model tests pass
</success_criteria>

<output>
After completion, create `.planning/phases/01-foundations-models-manifest-capability-interface/01-01-SUMMARY.md` following @$HOME/.claude/get-shit-done/templates/summary.md with:
- Files created/modified
- Field list for each of the 3 entities (final shape)
- Any deviations from the plan (e.g. renamed fields) with rationale
- Confirmation that composite unique index is expressed as shared-index-name pattern, not single-column uniqueIndex on Name
</output>
