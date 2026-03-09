# Phase 44: Data Model and API/CLI - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Create the BlockStoreConfig DB model (replacing PayloadStoreConfig), REST endpoints for local/remote block stores, dfsctl CLI commands, and updated share model with LocalBlockStoreID + RemoteBlockStoreID. This is the control plane layer for the new two-tier block store model. Refactor existing API routes to use consistent `/api/v1/store/` prefix.

</domain>

<decisions>
## Implementation Decisions

### Migration Strategy
- Single GORM migration: rename `payload_stores` table to `block_store_configs`, add `kind` column
- Classify existing rows by type: `memory` and `s3` become `kind=remote` (memory stores serve as lightweight test substitutes for remote storage)
- Assume Phase 42 already removed filesystem payload stores — migration only handles `memory` and `s3` types
- Share table migration: split `PayloadStoreID` into `LocalBlockStoreID` + `RemoteBlockStoreID`
- Existing shares get `RemoteBlockStoreID` populated from old `PayloadStoreID` (since all existing payload stores classify as remote)
- Migration auto-creates a `default-local` block store (kind=local, type=fs, default path) and assigns it to all existing shares as their `LocalBlockStoreID`
- Old `PayloadStoreConfigStore` interface removed entirely — clean break, no deprecation shims
- Old `/api/v1/payload-stores` endpoints removed entirely

### BlockStoreConfig Model
- Fields: ID, Name, Kind (local/remote), Type, Config (JSON), CreatedAt
- Kind is a discriminator column — single table for both local and remote configs
- Local store types: `fs` (disk-backed blocks), `memory` (testing)
- Remote store types: `memory` (testing), `s3` (production)
- Single `BlockStoreConfigStore` interface with kind-filtered methods (e.g., `ListBlockStores(ctx, kind)`)
- Replaces `PayloadStoreConfigStore` in the composite `Store` interface

### Share Model
- `LocalBlockStoreID` (mandatory, NOT NULL) + `RemoteBlockStoreID` (nullable)
- Share creation requires local block store — API returns validation error if not specified
- Remote block store is optional — local-only shares are valid
- Both references are changeable via share update (useful for migrating shares)
- Delete block store blocked if any share references it (ErrStoreInUse) — check both local and remote FKs
- API response: `remote_block_store: null` when no remote configured (explicit null, not omitted)
- CLI table: remote column shows `-` when null

### API Route Design
- New consistent route prefix: `/api/v1/store/`
- Block stores: `/api/v1/store/block/{kind}` (kind = local or remote)
- Metadata stores: `/api/v1/store/metadata` (refactored from `/api/v1/metadata-stores`)
- Old `/api/v1/payload-stores` removed entirely
- Single `BlockStoreHandler` struct with kind extracted from URL path parameter — reduces code duplication
- Share endpoints use inline fields: `local_block_store` (required) and `remote_block_store` (nullable) in request body
- API client methods refactored for all store types to use new `/api/v1/store/` prefix

### CLI Command Structure
- `dfsctl store block local add/list/edit/remove`
- `dfsctl store block remote add/list/edit/remove`
- `dfsctl store payload` subcommand removed entirely
- Share creation: `dfsctl share create --name /data --metadata default --local fs-cache --remote s3-store`
- `--local` is required, `--remote` is optional
- Interactive mode: prompt for both "Local block store name" (required) and "Remote block store name (optional, Enter to skip)"
- Local store type `fs`: prompt for block directory path only (other settings auto-deduced in Phase 48)
- CLI code: `cmd/dfsctl/commands/store/block/local/` and `block/remote/` subdirectories

### Code Structure
- API handler: rename `payload_stores.go` to `block_stores.go` in handlers directory
- CLI: `cmd/dfsctl/commands/store/block/` with `local/` and `remote/` subdirectories, each with `add.go`, `list.go`, `edit.go`, `remove.go`
- GORM store: rename `payload.go` to `block.go` in controlplane store package
- Model: `BlockStoreConfig` replaces `PayloadStoreConfig` in `pkg/controlplane/models/stores.go`

### Testing
- Unit tests for GORM `BlockStoreConfigStore` CRUD with kind filtering
- API handler tests for block store endpoints
- CLI command tests
- Share create/update tests with new local/remote fields
- Migration test verifying payload_stores → block_store_configs conversion
- No E2E tests (deferred to Phase 49)

### Documentation
- Update docs inline with code changes (Phase 29 pattern: "docs with each PR")
- CLAUDE.md updated for new API routes and CLI commands
- ARCHITECTURE.md updated for block store model

### Claude's Discretion
- Exact GORM migration SQL for table rename + column add
- Default path for auto-created `default-local` block store
- API client helper method signatures
- Test fixture design and helper patterns
- Exact validation error messages and HTTP status codes
- Router registration order for new routes

</decisions>

<specifics>
## Specific Ideas

- Memory stores exist both for local and remote — they let you test the remote flow without external dependencies
- User explicitly wants no dashes in API paths (e.g., `/store/block/local` not `/block-stores/local`)
- Route refactoring includes metadata stores (`/api/v1/metadata-stores` → `/api/v1/store/metadata`) for consistency
- All old payload-related interfaces, endpoints, and CLI commands removed — clean break, no deprecation shims

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `PayloadStoreConfig` model in `pkg/controlplane/models/stores.go` — transform into `BlockStoreConfig` with kind column
- `PayloadStoreConfigStore` interface in `pkg/controlplane/store/interface.go` — transform into `BlockStoreConfigStore`
- GORM helpers (`getByField[T]`, `listAll[T]`, `createWithID[T]`) in store package — reuse for block store CRUD
- `cmd/dfsctl/commands/store/payload/` — CLI pattern to replicate for block store commands
- `internal/controlplane/api/handlers/payload_stores.go` — handler pattern to transform
- `pkg/apiclient/` — API client helpers to extend with block store methods

### Established Patterns
- Phase 14: API handler pattern (single handler struct, narrowest interface, CRUD methods)
- Phase 29: Interface composition (sub-interface + composite Store)
- GORM auto-migrate for schema changes
- `cmd/dfsctl/commands/store/` directory structure with per-resource subdirectories
- Share model has foreign key references to store configs with GORM Preload
- `convertNotFoundError()` helper for consistent error mapping

### Integration Points
- `pkg/controlplane/store/interface.go` — composite `Store` interface needs `BlockStoreConfigStore` replacing `PayloadStoreConfigStore`
- `pkg/controlplane/models/share.go` — Share struct needs `LocalBlockStoreID` + `RemoteBlockStoreID` fields
- `internal/controlplane/api/handlers/shares.go` — share create/update handlers need new field handling
- `pkg/controlplane/api/router.go` — route registration for new endpoints
- `pkg/apiclient/` — needs block store client methods, share request structs updated
- `cmd/dfsctl/commands/store/store.go` — parent command needs block subcommand registered
- `cmd/dfsctl/commands/share/create.go` — `--payload` flag replaced with `--local` + `--remote`

</code_context>

<deferred>
## Deferred Ideas

- Metadata store route refactoring: Already included in this phase (moved from deferred to in-scope after discussion)
- General API route cleanup (e.g., `/api/v1/adapter/` prefix) — future cleanup phase
- `--payload` backward compatibility shim — Phase 49 (Testing and Documentation)

</deferred>

---

*Phase: 44-data-model-and-api-cli*
*Context gathered: 2026-03-09*
