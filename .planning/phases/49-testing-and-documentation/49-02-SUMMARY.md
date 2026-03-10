---
phase: 49-testing-and-documentation
plan: 02
subsystem: testing
tags: [refactoring, naming, block-store, payload-store, e2e, k8s, ci]

# Dependency graph
requires:
  - phase: 45-package-restructure
    provides: Block store package structure (pkg/blockstore/)
provides:
  - Consistent block store terminology across tests, scripts, K8s, CI
  - Legacy payload store detection in config validator
  - Legacy type aliases for incremental migration
affects: [49-05-documentation-update]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - Legacy type aliases for backward-compatible renames
    - Config validator legacy key detection pattern

key-files:
  created:
    - test/e2e/block_stores_test.go
  modified:
    - test/e2e/helpers/stores.go
    - test/e2e/helpers/server.go
    - test/e2e/helpers/smb3_helpers.go
    - scripts/run-bench.sh
    - test/posix/setup-posix.sh
    - k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go
    - k8s/dittofs-operator/chart/crds/dittoservers.yaml
    - k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml
    - k8s/dittofs-operator/docs/CRD_REFERENCE.md
    - .github/workflows/e2e-tests.yml
    - .github/instructions/copilot.instructions.md
    - pkg/controlplane/models/stores.go
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/gorm.go
    - internal/controlplane/api/handlers/shares.go
    - cmd/dfs/commands/config/validate.go

key-decisions:
  - "Keep migration SQL referencing old payload_stores table names (required for migration correctness)"
  - "Use legacy type aliases in stores.go for backward-compatible incremental migration"
  - "Add config validator legacy key detection rather than silently ignoring old YAML keys"

patterns-established:
  - "Legacy alias pattern: type PayloadStore = BlockStore for safe migration"
  - "Config validator warning pattern: detect deprecated keys and suggest replacements"

requirements-completed: [DOCS-04]

# Metrics
duration: 12min
completed: 2026-03-10
---

# Phase 49 Plan 02: Payload Store Rename Summary

**Renamed all payload store references to block store across Go code, E2E tests, scripts, K8s operator, CI, and copilot instructions with legacy backward compatibility**

## Performance

- **Duration:** 12 min
- **Started:** 2026-03-10T16:21:36Z
- **Completed:** 2026-03-10T16:34:14Z
- **Tasks:** 2
- **Files modified:** 47

## Accomplishments
- Renamed all payload store comments and variable names in Go packages (pkg/, internal/, cmd/)
- Replaced payload_stores_test.go with block_stores_test.go and renamed all helper methods
- Batch-renamed payload references across 21+ E2E test files
- Updated K8s operator types, CRD YAMLs, and documentation
- Updated CI workflow (suite list, trigger paths) and copilot instructions
- Added legacy config key detection in config validator

## Task Commits

Each task was committed atomically:

1. **Task 1: Go packages and config** - `f18d2345` (refactor)
2. **Task 2: Tests, scripts, K8s, CI** - `8e3d5663` (refactor)

## Files Created/Modified

### Task 1
- `pkg/controlplane/models/stores.go` - Updated comment from "PayloadStoreConfig" to "Kind discriminator"
- `internal/controlplane/api/handlers/shares.go` - Updated comment from "payload store" to "block store"
- `pkg/controlplane/store/interface.go` - Updated ShareStore comment
- `pkg/controlplane/store/gorm.go` - Updated 4 migration doc comments (SQL kept as-is)
- `pkg/controlplane/store/adapter_settings_test.go` - Updated test comment
- `cmd/dfs/commands/config/validate.go` - Added checkLegacyPayloadKey function

### Task 2
- `test/e2e/block_stores_test.go` - New file replacing payload_stores_test.go
- `test/e2e/helpers/stores.go` - Renamed PayloadStore->BlockStore types/methods, added legacy aliases
- `test/e2e/helpers/server.go` - Removed legacy config sections from test template
- `test/e2e/helpers/smb3_helpers.go` - payloadStoreName->localStoreName
- 21 E2E test files - Batch-renamed payload references
- 5 SMB conformance config YAMLs - Updated comments
- `test/posix/README.md`, `test/posix/setup-posix.sh` - Updated terminology
- `test/smb-conformance/README.md` - Updated table header
- `scripts/run-bench.sh` - Updated comments and variable names
- `k8s/dittofs-operator/api/v1alpha1/dittoserver_types.go` - Updated S3StoreConfig comments
- `k8s/dittofs-operator/chart/crds/dittoservers.yaml` - Updated CRD description
- `k8s/dittofs-operator/config/crd/bases/dittofs.dittofs.com_dittoservers.yaml` - Updated CRD description
- `k8s/dittofs-operator/docs/CRD_REFERENCE.md` - Updated S3 section heading and description
- `.github/workflows/e2e-tests.yml` - PayloadStores->BlockStores, pkg/payload->pkg/blockstore
- `.github/instructions/copilot.instructions.md` - Updated all payload store references

## Decisions Made
- **Migration SQL preserved**: The gorm.go migration code MUST reference old `payload_stores` table and `payload_store_id` column names since it's literally migrating FROM those names
- **Legacy aliases for backward compat**: Added type aliases (`PayloadStore = BlockStore`) and wrapper functions in stores.go to allow incremental migration while maintaining compilation
- **Config validator detection**: Added `checkLegacyPayloadKey` function to warn users about deprecated `payload:` YAML keys rather than silently ignoring them

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Added legacy payload config key detection**
- **Found during:** Task 1
- **Issue:** Plan specified cleaning Go code but config validator had no way to warn about deprecated YAML keys
- **Fix:** Added `checkLegacyPayloadKey` function scanning config files for `payload:` or `payload_store:` keys
- **Files modified:** cmd/dfs/commands/config/validate.go
- **Verification:** `go build ./cmd/dfs/...` passes, `go test ./pkg/config/...` passes
- **Committed in:** f18d2345 (Task 1 commit)

**2. [Rule 1 - Bug] Fixed stale pkg/payload trigger path in CI workflow**
- **Found during:** Task 2
- **Issue:** CI workflow still triggered on `pkg/payload/**` which no longer exists (now `pkg/blockstore/`)
- **Fix:** Changed trigger path from `pkg/payload/**` to `pkg/blockstore/**`
- **Files modified:** .github/workflows/e2e-tests.yml
- **Committed in:** 8e3d5663 (Task 2 commit)

---

**Total deviations:** 2 auto-fixed (1 missing critical, 1 bug)
**Impact on plan:** Both auto-fixes improve correctness. No scope creep.

## Issues Encountered
- Pre-existing build error in `pkg/blockstore/engine/engine.go` (`bs.syncer.Queue` undefined) prevented full `go build ./...` - worked around by building only modified packages. Logged as out-of-scope per deviation rules.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All payload store references cleaned from code, tests, scripts, K8s, and CI
- Remaining references in docs/ and README.md are handled by plan 05 (documentation update)
- E2E tests compile and use new block store terminology

---
*Phase: 49-testing-and-documentation*
*Completed: 2026-03-10*
