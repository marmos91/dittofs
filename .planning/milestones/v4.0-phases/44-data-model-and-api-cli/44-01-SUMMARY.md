---
phase: 44-data-model-and-api-cli
plan: 01
subsystem: database
tags: [gorm, sqlite, postgresql, migration, models, block-store]

# Dependency graph
requires: []
provides:
  - BlockStoreConfig model with Kind discriminator (local/remote)
  - Share model with LocalBlockStoreID + RemoteBlockStoreID
  - BlockStoreConfigStore interface with kind-filtered CRUD
  - GORM migration from payload_stores to block_store_configs
affects: [44-02, 44-03, api-handlers, cli-commands, runtime-init]

# Tech tracking
tech-stack:
  added: []
  patterns: [kind-discriminator single-table, nullable foreign key with pointer type, pre/post AutoMigrate migration]

key-files:
  created:
    - pkg/controlplane/store/block.go
    - pkg/controlplane/store/block_test.go
    - internal/controlplane/api/handlers/block_stores.go
  modified:
    - pkg/controlplane/models/stores.go
    - pkg/controlplane/models/share.go
    - pkg/controlplane/models/models.go
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/shares.go
    - pkg/controlplane/store/gorm.go
    - pkg/controlplane/store/health.go
    - pkg/controlplane/store/metadata.go
    - pkg/controlplane/store/netgroups.go
    - pkg/controlplane/runtime/init.go
    - internal/controlplane/api/handlers/shares.go
    - pkg/controlplane/api/router.go
    - pkg/apiclient/shares.go
    - pkg/apiclient/stores.go
    - cmd/dfs/commands/backup/controlplane.go
    - cmd/dfs/commands/restore/controlplane.go
    - cmd/dfsctl/commands/share/create.go
    - cmd/dfsctl/commands/share/list.go
    - cmd/dfsctl/commands/store/payload/list.go
    - cmd/dfsctl/commands/store/payload/add.go
    - cmd/dfsctl/commands/store/payload/edit.go
    - cmd/dfsctl/commands/store/payload/remove.go

key-decisions:
  - "Single table with Kind discriminator for local/remote block stores (not separate tables)"
  - "RemoteBlockStoreID is *string (nullable pointer) for GORM nullable FK support"
  - "Migration runs in two phases: pre-AutoMigrate (table rename + add kind) and post-AutoMigrate (share column split)"
  - "API route changed from /payload-stores to /store/block/{kind} for kind-aware CRUD"

patterns-established:
  - "Kind-discriminator pattern: single table with kind column, all queries filter by kind"
  - "Nullable FK pattern: *string ID + *Model relationship for optional references"
  - "Two-phase migration: structural changes before AutoMigrate, data migration after"

requirements-completed: [MODEL-01, MODEL-02, MODEL-03, MODEL-04, MODEL-05]

# Metrics
duration: 18min
completed: 2026-03-09
---

# Phase 44 Plan 01: Block Store Data Model Summary

**BlockStoreConfig model with Kind discriminator replacing PayloadStoreConfig, dual local/remote FKs on Share, GORM migration, and full-stack type propagation across 26 files**

## Performance

- **Duration:** ~18 min
- **Started:** 2026-03-09T16:55:18Z
- **Completed:** 2026-03-09T17:13:22Z
- **Tasks:** 2
- **Files modified:** 26

## Accomplishments
- Replaced PayloadStoreConfig with BlockStoreConfig featuring Kind discriminator (local/remote) across models, store interface, and GORM implementation
- Updated Share model from single PayloadStoreID to dual LocalBlockStoreID (mandatory) + RemoteBlockStoreID (nullable) with proper GORM FK relationships
- Implemented complete GORM BlockStoreConfigStore with kind-filtered CRUD, in-use protection on delete, and GetSharesByBlockStore cross-reference query
- Added two-phase migration in gorm.go: pre-AutoMigrate table rename + kind column, post-AutoMigrate share column split with default-local block store creation
- Propagated type changes across entire codebase: API handlers, router, CLI commands, apiclient, backup/restore, runtime init
- Created 28 integration tests covering CRUD, kind filtering, share relationships, and in-use protection

## Task Commits

Each task was committed atomically:

1. **Task 1: BlockStoreConfig model, updated Share model, BlockStoreConfigStore interface** - `9db1ba71` (feat, TDD)
2. **Task 2: GORM store implementation with migration and updated share queries** - `5b329b93` (feat)

## Files Created/Modified
- `pkg/controlplane/models/stores.go` - BlockStoreConfig model with Kind discriminator, replaces PayloadStoreConfig
- `pkg/controlplane/models/stores_test.go` - Unit tests for BlockStoreConfig model (kind constants, TableName, GetConfig/SetConfig)
- `pkg/controlplane/models/share.go` - Share with LocalBlockStoreID + RemoteBlockStoreID replacing PayloadStoreID
- `pkg/controlplane/models/models.go` - AllModels() updated with BlockStoreConfig
- `pkg/controlplane/store/interface.go` - BlockStoreConfigStore interface replacing PayloadStoreConfigStore
- `pkg/controlplane/store/block.go` - NEW: Full GORM implementation of BlockStoreConfigStore
- `pkg/controlplane/store/block_test.go` - NEW: 28 integration tests for block store CRUD, filtering, share refs
- `pkg/controlplane/store/payload.go` - DELETED: Replaced by block.go
- `pkg/controlplane/store/shares.go` - Updated preloads for LocalBlockStore/RemoteBlockStore
- `pkg/controlplane/store/gorm.go` - Two-phase migration logic (pre/post AutoMigrate)
- `pkg/controlplane/store/health.go` - Compile-time assertion updated
- `pkg/controlplane/store/metadata.go` - Preload updates
- `pkg/controlplane/store/netgroups.go` - Preload updates
- `pkg/controlplane/store/store_test.go` - Updated test fixtures for BlockStoreConfig
- `pkg/controlplane/store/adapter_settings_test.go` - Updated test fixtures for BlockStoreConfig
- `internal/controlplane/api/handlers/block_stores.go` - NEW: BlockStoreHandler with kind from URL path
- `internal/controlplane/api/handlers/shares.go` - Updated for local/remote block store references
- `internal/controlplane/api/handlers/netgroups_test.go` - Updated test fixtures
- `pkg/controlplane/api/router.go` - Route changed to /store/block/{kind}
- `pkg/controlplane/runtime/init.go` - ListBlockStores with BlockStoreKindRemote
- `pkg/apiclient/shares.go` - Share/CreateShareRequest DTOs updated
- `pkg/apiclient/stores.go` - BlockStore type replaces PayloadStore, kind-aware API methods
- `cmd/dfs/commands/backup/controlplane.go` - Backup struct and logic for BlockStoreConfig
- `cmd/dfs/commands/restore/controlplane.go` - Restore logic for BlockStoreConfig
- `cmd/dfsctl/commands/share/create.go` - --local/--remote flags replace --payload
- `cmd/dfsctl/commands/share/list.go` - Shows LOCAL STORE and REMOTE STORE columns
- `cmd/dfsctl/commands/store/payload/list.go` - Uses BlockStore type
- `cmd/dfsctl/commands/store/payload/add.go` - Uses CreateBlockStore
- `cmd/dfsctl/commands/store/payload/edit.go` - Uses GetBlockStore/UpdateBlockStore
- `cmd/dfsctl/commands/store/payload/remove.go` - Uses DeleteBlockStore

## Decisions Made
- Used single table with Kind discriminator (not separate tables) for local/remote block stores -- simpler queries, matches existing MetadataStoreConfig pattern
- RemoteBlockStoreID as *string (nullable pointer) rather than string with empty value -- GORM correctly handles nullable FK with pointer types
- Migration runs in two phases: pre-AutoMigrate handles table rename + kind column addition (so AutoMigrate sees correct table), post-AutoMigrate handles share column split (so new columns exist from model definition)
- API route changed from `/api/v1/payload-stores` to `/api/v1/store/block/{kind}` for kind-aware CRUD operations
- Existing CLI `dfsctl store payload` subcommands updated to use "remote" kind by default (backward compatible for payload store users)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Fixed compilation errors in runtime/init.go**
- **Found during:** Task 2
- **Issue:** `ListPayloadStores` undefined after interface rename
- **Fix:** Changed to `ListBlockStores(ctx, models.BlockStoreKindRemote)`, renamed variables
- **Files modified:** `pkg/controlplane/runtime/init.go`
- **Verification:** `go build ./...` passes
- **Committed in:** 5b329b93

**2. [Rule 3 - Blocking] Replaced payload_stores.go handler with block_stores.go**
- **Found during:** Task 2
- **Issue:** API handlers referenced removed PayloadStoreConfig types; handler file needed complete rewrite
- **Fix:** Created new `block_stores.go` with BlockStoreHandler extracting kind from URL path, deleted old `payload_stores.go`
- **Files modified:** `internal/controlplane/api/handlers/block_stores.go`, deleted `internal/controlplane/api/handlers/payload_stores.go`
- **Verification:** `go build ./...` passes
- **Committed in:** 5b329b93

**3. [Rule 3 - Blocking] Updated shares handler for dual block store references**
- **Found during:** Task 2
- **Issue:** ShareHandlerStore interface, request/response types, and Create/Update handlers all referenced old PayloadStore types
- **Fix:** Updated ShareHandlerStore interface, request types, response mapping, and Create/Update logic for local/remote block stores
- **Files modified:** `internal/controlplane/api/handlers/shares.go`
- **Verification:** `go build ./...` passes
- **Committed in:** 5b329b93

**4. [Rule 3 - Blocking] Updated router for new block store handler**
- **Found during:** Task 2
- **Issue:** Router referenced `NewPayloadStoreHandler` which no longer exists
- **Fix:** Changed to `NewBlockStoreHandler` with `/store/block/{kind}` route
- **Files modified:** `pkg/controlplane/api/router.go`
- **Verification:** `go build ./...` passes
- **Committed in:** 5b329b93

**5. [Rule 3 - Blocking] Updated backup/restore commands**
- **Found during:** Task 2
- **Issue:** Backup/restore referenced PayloadStoreConfig type and methods
- **Fix:** Updated backup struct and export/import logic to use BlockStoreConfig
- **Files modified:** `cmd/dfs/commands/backup/controlplane.go`, `cmd/dfs/commands/restore/controlplane.go`
- **Verification:** `go build ./...` passes
- **Committed in:** 5b329b93

**6. [Rule 3 - Blocking] Updated apiclient, CLI commands, and test fixtures**
- **Found during:** Task 2
- **Issue:** Multiple CLI and client files referenced old PayloadStore types
- **Fix:** Updated all apiclient DTOs, CLI share/store commands, and handler test fixtures
- **Files modified:** `pkg/apiclient/shares.go`, `pkg/apiclient/stores.go`, `cmd/dfsctl/commands/share/create.go`, `cmd/dfsctl/commands/share/list.go`, `cmd/dfsctl/commands/store/payload/*.go`, `internal/controlplane/api/handlers/netgroups_test.go`
- **Verification:** `go build ./...` passes, all tests pass
- **Committed in:** 5b329b93

---

**Total deviations:** 6 auto-fixed (all Rule 3 - blocking)
**Impact on plan:** All auto-fixes necessary to maintain compilation after the type rename. The plan acknowledged Task 1 would "temporarily break compilation" -- these fixes completed the propagation across the full codebase. No scope creep.

## Issues Encountered
None beyond the expected compilation fixes from the type rename.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- BlockStoreConfig model and store are ready for API handler plans (44-02)
- CLI commands updated and ready for CLI plan (44-03)
- All existing tests pass, codebase builds cleanly
- Migration code handles both fresh installs and upgrades from payload_stores

## Self-Check: PASSED

- All 10 key files verified present
- 2 deleted files confirmed removed (payload.go, payload_stores.go)
- Commit 9db1ba71 (Task 1) verified
- Commit 5b329b93 (Task 2) verified
- `go build ./...` passes
- `go vet ./...` passes
- 28 integration tests pass

---
*Phase: 44-data-model-and-api-cli*
*Completed: 2026-03-09*
