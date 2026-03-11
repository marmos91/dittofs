# Phase 44: Data Model and API/CLI - Research

**Researched:** 2026-03-09
**Domain:** Go control plane refactoring (GORM models, chi REST API, Cobra CLI, apiclient)
**Confidence:** HIGH

## Summary

Phase 44 transforms the DittoFS control plane from a single-kind payload store model to a two-tier block store model with local and remote kinds. This involves renaming the `PayloadStoreConfig` model to `BlockStoreConfig` with a `Kind` discriminator column, splitting the `Share.PayloadStoreID` into `LocalBlockStoreID` (mandatory) and `RemoteBlockStoreID` (nullable), and updating all layers: GORM store, REST API, API client, and CLI commands. The phase also refactors API routes from `/api/v1/payload-stores` and `/api/v1/metadata-stores` to a consistent `/api/v1/store/` prefix.

All code is pure Go with no external framework changes. The patterns are well-established in the existing codebase: GORM AutoMigrate for schema evolution, chi router for HTTP, Cobra for CLI, and generic helpers for GORM and API client operations. The primary risk is ensuring the GORM migration correctly handles table rename + column addition + data classification in a single migration pass.

**Primary recommendation:** Follow the existing codebase patterns exactly -- transform `PayloadStoreConfig` to `BlockStoreConfig`, reuse generic GORM helpers, and use a single `BlockStoreHandler` with kind extracted from URL path to avoid code duplication.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Single GORM migration: rename `payload_stores` table to `block_store_configs`, add `kind` column
- Classify existing rows by type: `memory` and `s3` become `kind=remote`
- Migration auto-creates `default-local` block store (kind=local, type=fs, default path) and assigns to all existing shares
- Share table migration: split `PayloadStoreID` into `LocalBlockStoreID` + `RemoteBlockStoreID`
- Old `PayloadStoreConfigStore` interface removed entirely -- clean break, no deprecation shims
- Old `/api/v1/payload-stores` endpoints removed entirely
- New consistent route prefix: `/api/v1/store/`
- Block stores: `/api/v1/store/block/{kind}` (kind = local or remote)
- Metadata stores refactored from `/api/v1/metadata-stores` to `/api/v1/store/metadata`
- Single `BlockStoreHandler` struct with kind extracted from URL path parameter
- CLI: `dfsctl store block local add/list/edit/remove` and `dfsctl store block remote add/list/edit/remove`
- `dfsctl store payload` subcommand removed entirely
- Share creation: `--local` required, `--remote` optional
- API handler: rename `payload_stores.go` to `block_stores.go`
- GORM store: rename `payload.go` to `block.go`
- Model: `BlockStoreConfig` replaces `PayloadStoreConfig` in `pkg/controlplane/models/stores.go`
- Unit tests for GORM CRUD, API handlers, CLI commands, share create/update, migration
- No E2E tests (deferred to Phase 49)

### Claude's Discretion
- Exact GORM migration SQL for table rename + column add
- Default path for auto-created `default-local` block store
- API client helper method signatures
- Test fixture design and helper patterns
- Exact validation error messages and HTTP status codes
- Router registration order for new routes

### Deferred Ideas (OUT OF SCOPE)
- General API route cleanup (e.g., `/api/v1/adapter/` prefix) -- future cleanup phase
- `--payload` backward compatibility shim -- Phase 49 (Testing and Documentation)
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| MODEL-01 | BlockStoreConfig model with ID, Name, Kind (local/remote), Type, Config, CreatedAt | Transform existing `PayloadStoreConfig` struct in `pkg/controlplane/models/stores.go`, add `Kind` field with GORM tag |
| MODEL-02 | Share model updated with LocalBlockStoreID (mandatory) + RemoteBlockStoreID (nullable) | Modify `Share` struct in `pkg/controlplane/models/share.go`, update GORM foreign key relationships |
| MODEL-03 | Migration renames payload_store_configs -> block_store_configs with kind column | Post-AutoMigrate raw SQL in `pkg/controlplane/store/gorm.go` New() function |
| MODEL-04 | Migration splits Share.PayloadStoreID into LocalBlockStoreID + RemoteBlockStoreID | Post-AutoMigrate raw SQL to populate new columns from old PayloadStoreID |
| MODEL-05 | BlockStoreConfigStore interface with CRUD filtered by kind replaces PayloadStoreConfigStore | New interface in `pkg/controlplane/store/interface.go` replacing PayloadStoreConfigStore |
| API-01 | REST endpoints for local block store CRUD at `/api/v1/store/block/local` | Single `BlockStoreHandler` with kind from chi URL param |
| API-02 | REST endpoints for remote block store CRUD at `/api/v1/store/block/remote` | Same handler, different kind value from URL |
| API-03 | Share endpoints accept `local_block_store` (required) and `remote_block_store` (nullable) | Update `CreateShareRequest`/`UpdateShareRequest` in handlers/shares.go |
| CLI-01 | `dfsctl store block local add/list/edit/remove` commands | New `cmd/dfsctl/commands/store/block/local/` directory following payload/ pattern |
| CLI-02 | `dfsctl store block remote add/list/edit/remove` commands | New `cmd/dfsctl/commands/store/block/remote/` directory following payload/ pattern |
| CLI-03 | `dfsctl share create --local X --remote Y` replacing `--payload` | Update `cmd/dfsctl/commands/share/create.go` flags |
| CLI-04 | API client methods for block store operations replacing payload store methods | Transform methods in `pkg/apiclient/stores.go` |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| gorm.io/gorm | existing | ORM for database models and migrations | Already in use, AutoMigrate handles schema evolution |
| github.com/go-chi/chi/v5 | existing | HTTP router with URL parameter extraction | Already in use, `{kind}` path param for block store routes |
| github.com/spf13/cobra | existing | CLI framework | Already in use for all dfsctl commands |
| github.com/google/uuid | existing | UUID generation for model IDs | Already in use in all model creation |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| gorm.io/gorm/logger | existing | GORM query logging | Debug migration issues |
| encoding/json | stdlib | JSON serialization for Config field | Already used in existing store models |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| GORM AutoMigrate + raw SQL | golang-migrate | More control but adds dependency; AutoMigrate is established pattern here |
| Single BlockStoreHandler | Separate Local/RemoteHandler | Duplication vs. single handler with kind param; single is cleaner |

## Architecture Patterns

### Recommended File Changes

```
pkg/controlplane/models/
    stores.go               # PayloadStoreConfig -> BlockStoreConfig + Kind field
    share.go                # PayloadStoreID -> LocalBlockStoreID + RemoteBlockStoreID
    models.go               # Update AllModels() list

pkg/controlplane/store/
    interface.go            # PayloadStoreConfigStore -> BlockStoreConfigStore in composite Store
    payload.go -> block.go  # GORM methods with kind filtering
    shares.go               # Update Preload names, FK references
    gorm.go                 # Migration logic in New()

internal/controlplane/api/handlers/
    payload_stores.go -> block_stores.go  # Single handler with kind param
    shares.go               # New request/response structs with local/remote fields

pkg/controlplane/api/
    router.go               # New route registration under /api/v1/store/

pkg/apiclient/
    stores.go               # New block store client methods, updated share structs

cmd/dfsctl/commands/store/
    store.go                # Register block subcommand, remove payload
    payload/                # DELETE entire directory
    block/                  # NEW directory
        block.go            # Parent command
        local/              # add.go, list.go, edit.go, remove.go
        remote/             # add.go, list.go, edit.go, remove.go

cmd/dfsctl/commands/share/
    create.go               # --local + --remote flags replacing --payload
    list.go                 # Update table columns and store name resolution
    edit.go                 # Update store reference fields
```

### Pattern 1: BlockStoreConfig Model with Kind Discriminator

**What:** Single database table with a `kind` column discriminating local vs. remote block stores.
**When to use:** When two closely related entities share the same schema.
**Example:**
```go
// Source: Existing PayloadStoreConfig pattern in pkg/controlplane/models/stores.go
type BlockStoreKind string

const (
    BlockStoreKindLocal  BlockStoreKind = "local"
    BlockStoreKindRemote BlockStoreKind = "remote"
)

type BlockStoreConfig struct {
    ID        string         `gorm:"primaryKey;size:36" json:"id"`
    Name      string         `gorm:"uniqueIndex;not null;size:255" json:"name"`
    Kind      BlockStoreKind `gorm:"not null;size:10;index" json:"kind"`
    Type      string         `gorm:"not null;size:50" json:"type"`
    Config    string         `gorm:"type:text" json:"-"`
    CreatedAt time.Time      `gorm:"autoCreateTime" json:"created_at"`

    ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

func (BlockStoreConfig) TableName() string {
    return "block_store_configs"
}
```

### Pattern 2: Share Model with Nullable Remote FK

**What:** Share has mandatory LocalBlockStoreID and nullable RemoteBlockStoreID.
**When to use:** When one relationship is required and another is optional.
**Example:**
```go
// Source: Existing Share pattern in pkg/controlplane/models/share.go
type Share struct {
    // ... existing fields ...
    MetadataStoreID     string  `gorm:"not null;size:36" json:"metadata_store_id"`
    LocalBlockStoreID   string  `gorm:"not null;size:36" json:"local_block_store_id"`
    RemoteBlockStoreID  *string `gorm:"size:36" json:"remote_block_store_id"`
    // ... rest of fields ...

    // Relationships
    MetadataStore     MetadataStoreConfig `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitempty"`
    LocalBlockStore   BlockStoreConfig    `gorm:"foreignKey:LocalBlockStoreID" json:"local_block_store,omitempty"`
    RemoteBlockStore  *BlockStoreConfig   `gorm:"foreignKey:RemoteBlockStoreID" json:"remote_block_store"`
}
```

**Key detail:** `RemoteBlockStoreID` is `*string` (pointer) for GORM nullable FK. The JSON response uses `json:"remote_block_store"` (no omitempty) so it renders as `null` when not set, matching the CONTEXT.md requirement.

### Pattern 3: Kind-Filtered GORM Store Methods

**What:** Single `BlockStoreConfigStore` interface with kind parameter for filtering.
**When to use:** When CRUD operations are identical except for a discriminator.
**Example:**
```go
// Source: Existing PayloadStoreConfigStore pattern in pkg/controlplane/store/interface.go
type BlockStoreConfigStore interface {
    GetBlockStore(ctx context.Context, name string, kind models.BlockStoreKind) (*models.BlockStoreConfig, error)
    GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
    ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error)
    CreateBlockStore(ctx context.Context, store *models.BlockStoreConfig) (string, error)
    UpdateBlockStore(ctx context.Context, store *models.BlockStoreConfig) error
    DeleteBlockStore(ctx context.Context, name string, kind models.BlockStoreKind) error
    GetSharesByBlockStore(ctx context.Context, storeName string) ([]*models.Share, error)
}
```

### Pattern 4: Single Handler with Kind from URL

**What:** One `BlockStoreHandler` struct serving both `/api/v1/store/block/local` and `/api/v1/store/block/remote`.
**When to use:** When handlers are identical except for a discriminator value.
**Example:**
```go
// Source: Existing handler patterns in internal/controlplane/api/handlers/
type BlockStoreHandler struct {
    store store.BlockStoreConfigStore
}

func (h *BlockStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
    kind := models.BlockStoreKind(chi.URLParam(r, "kind"))
    if kind != models.BlockStoreKindLocal && kind != models.BlockStoreKindRemote {
        BadRequest(w, "Invalid block store kind: must be 'local' or 'remote'")
        return
    }
    // ... rest follows PayloadStoreHandler.Create pattern ...
}
```

Router registration:
```go
// Source: Existing router pattern in pkg/controlplane/api/router.go
r.Route("/store", func(r chi.Router) {
    r.Use(apiMiddleware.RequireAdmin())

    // Block stores (local and remote)
    r.Route("/block/{kind}", func(r chi.Router) {
        blockStoreHandler := handlers.NewBlockStoreHandler(cpStore)
        r.Post("/", blockStoreHandler.Create)
        r.Get("/", blockStoreHandler.List)
        r.Get("/{name}", blockStoreHandler.Get)
        r.Put("/{name}", blockStoreHandler.Update)
        r.Delete("/{name}", blockStoreHandler.Delete)
    })

    // Metadata stores (refactored from /metadata-stores)
    r.Route("/metadata", func(r chi.Router) {
        metadataStoreHandler := handlers.NewMetadataStoreHandler(cpStore, rt)
        r.Post("/", metadataStoreHandler.Create)
        r.Get("/", metadataStoreHandler.List)
        r.Get("/{name}", metadataStoreHandler.Get)
        r.Put("/{name}", metadataStoreHandler.Update)
        r.Delete("/{name}", metadataStoreHandler.Delete)
    })
})
```

### Pattern 5: GORM Migration Strategy

**What:** Post-AutoMigrate raw SQL for table rename and data migration.
**When to use:** When AutoMigrate cannot handle rename operations.
**Example:**
```go
// Source: Existing migration pattern in pkg/controlplane/store/gorm.go New()
// After AutoMigrate for new models, run migration for existing data:

migrator := db.Migrator()

// Step 1: If old table exists, rename it and add kind column
if migrator.HasTable("payload_stores") && !migrator.HasTable("block_store_configs") {
    // Rename table
    db.Exec("ALTER TABLE payload_stores RENAME TO block_store_configs")
    // Add kind column with default
    db.Exec("ALTER TABLE block_store_configs ADD COLUMN kind VARCHAR(10) NOT NULL DEFAULT 'remote'")
    // Classify all existing stores as remote
    db.Exec("UPDATE block_store_configs SET kind = 'remote'")
}

// Step 2: If share still has old column, migrate
if migrator.HasColumn(&models.Share{}, "payload_store_id") {
    // Add new columns if not exist
    if !migrator.HasColumn(&models.Share{}, "remote_block_store_id") {
        db.Exec("ALTER TABLE shares ADD COLUMN remote_block_store_id VARCHAR(36)")
    }
    if !migrator.HasColumn(&models.Share{}, "local_block_store_id") {
        db.Exec("ALTER TABLE shares ADD COLUMN local_block_store_id VARCHAR(36)")
    }
    // Copy payload_store_id to remote_block_store_id
    db.Exec("UPDATE shares SET remote_block_store_id = payload_store_id WHERE payload_store_id IS NOT NULL AND payload_store_id != ''")
    // Create default-local block store
    // ... insert default-local block store and assign to shares ...
    // Drop old column
    migrator.DropColumn(&models.Share{}, "payload_store_id")
}
```

### Anti-Patterns to Avoid
- **Keeping PayloadStoreConfigStore alongside BlockStoreConfigStore:** Context explicitly says clean break, no deprecation shims.
- **Separate handlers for local and remote block stores:** Code duplication; use single handler with kind from URL.
- **Using GORM AutoMigrate for table rename:** AutoMigrate cannot rename tables or columns; use raw SQL.
- **Omitting `remote_block_store` from JSON when null:** Context says `"remote_block_store": null` must be explicit, not omitted. Use `json:"remote_block_store"` without `omitempty`.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| UUID generation | Custom ID generation | `github.com/google/uuid` | Already used everywhere, consistent format |
| JSON decode/validation | Manual JSON parsing | `decodeJSONBody()` helper | Existing pattern, auto-writes 400 on failure |
| HTTP error responses | Custom error formatting | `BadRequest()`, `NotFound()`, `Conflict()` etc. | RFC 7807 Problem Details already implemented |
| GORM CRUD boilerplate | Manual SQL | `getByField[T]`, `listAll[T]`, `createWithID[T]` | Generic helpers reduce repetition |
| API client HTTP calls | Manual http.Client | `getResource[T]`, `listResources[T]`, `deleteResource` | Generic helpers in `pkg/apiclient/helpers.go` |
| CLI output formatting | Manual table rendering | `cmdutil.PrintOutput()`, `cmdutil.PrintResourceWithSuccess()` | Handles table/JSON/YAML output modes |
| CLI authentication | Manual token handling | `cmdutil.GetAuthenticatedClient()` | Handles context, token refresh, flag overrides |
| CLI confirmation prompts | Manual stdin reading | `cmdutil.RunDeleteWithConfirmation()` | Already handles --force flag |

**Key insight:** Every layer has established helpers. The task is transformation of existing code, not creation from scratch.

## Common Pitfalls

### Pitfall 1: GORM AutoMigrate vs. Table Rename
**What goes wrong:** GORM AutoMigrate creates a new `block_store_configs` table instead of renaming `payload_stores`, resulting in data loss.
**Why it happens:** AutoMigrate only adds columns/tables, never renames. If `BlockStoreConfig` with `TableName() = "block_store_configs"` is in `AllModels()`, AutoMigrate creates a new empty table.
**How to avoid:** Run migration SQL BEFORE AutoMigrate for new models, or handle the rename in post-AutoMigrate when old table still exists. Check `migrator.HasTable("payload_stores")` before proceeding.
**Warning signs:** Empty block_store_configs table after migration with old payload_stores still present.

### Pitfall 2: GORM Foreign Key Constraints During Migration
**What goes wrong:** Adding `LocalBlockStoreID` as NOT NULL with a foreign key fails because existing rows have no value.
**Why it happens:** GORM AutoMigrate adds FK constraints that reference block_store_configs. If existing shares don't have a valid LocalBlockStoreID, the constraint fails.
**How to avoid:** Create the default-local block store FIRST, then add the column with that ID as default, then add the NOT NULL constraint.
**Warning signs:** "FOREIGN KEY constraint failed" errors during migration.

### Pitfall 3: Nullable Pointer Field JSON Serialization
**What goes wrong:** `RemoteBlockStoreID *string` serializes as `null` in JSON, but the GORM Preloaded `RemoteBlockStore *BlockStoreConfig` relationship may cause issues.
**Why it happens:** GORM Preload on a nil FK doesn't set the relationship to nil; it may leave the zero value.
**How to avoid:** Use `*BlockStoreConfig` for the relationship field and test nil behavior explicitly. Ensure JSON tag is `json:"remote_block_store"` (no omitempty).
**Warning signs:** `remote_block_store: {}` instead of `remote_block_store: null` in API responses.

### Pitfall 4: Old Route Removal Breaking API Client
**What goes wrong:** Removing `/api/v1/payload-stores` and `/api/v1/metadata-stores` routes breaks existing API client methods.
**Why it happens:** API client methods reference old paths. If client is updated but server route removal happens in different order, tests fail.
**How to avoid:** Update routes, handlers, API client, and CLI in a coordinated way. Keep old routes temporarily during development if needed, but remove in final commit.
**Warning signs:** 404 responses from API client during testing.

### Pitfall 5: Share Preload Names After Model Change
**What goes wrong:** `Preload("PayloadStore")` in share GORM queries fails because field is renamed to `LocalBlockStore`/`RemoteBlockStore`.
**Why it happens:** GORM Preload uses the struct field name, not the table/column name.
**How to avoid:** Update ALL Preload calls in shares.go: `"PayloadStore"` -> `"LocalBlockStore"`, `"RemoteBlockStore"`.
**Warning signs:** Missing store data in share list/get responses.

### Pitfall 6: CLI Block Store Type Validation
**What goes wrong:** Local block store accepts `s3` type, or remote accepts `fs` type.
**Why it happens:** No type validation based on kind.
**How to avoid:** Validate type against kind: local accepts `fs`, `memory`; remote accepts `s3`, `memory`.
**Warning signs:** Invalid block stores that can't be instantiated at runtime.

### Pitfall 7: Delete Block Store FK Check
**What goes wrong:** Deleting a block store succeeds even though a share references it via `LocalBlockStoreID` or `RemoteBlockStoreID`.
**Why it happens:** Old delete logic only checks `payload_store_id`. New logic must check both FK columns.
**How to avoid:** In `DeleteBlockStore`, check both `local_block_store_id` and `remote_block_store_id` columns of shares table.
**Warning signs:** Orphaned foreign keys causing runtime panics when loading shares.

## Code Examples

### BlockStoreConfig Model

```go
// Source: Transform of existing PayloadStoreConfig in pkg/controlplane/models/stores.go
type BlockStoreKind string

const (
    BlockStoreKindLocal  BlockStoreKind = "local"
    BlockStoreKindRemote BlockStoreKind = "remote"
)

type BlockStoreConfig struct {
    ID        string         `gorm:"primaryKey;size:36" json:"id"`
    Name      string         `gorm:"uniqueIndex;not null;size:255" json:"name"`
    Kind      BlockStoreKind `gorm:"not null;size:10;index" json:"kind"`
    Type      string         `gorm:"not null;size:50" json:"type"` // fs, memory, s3
    Config    string         `gorm:"type:text" json:"-"`
    CreatedAt time.Time      `gorm:"autoCreateTime" json:"created_at"`

    ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

func (BlockStoreConfig) TableName() string {
    return "block_store_configs"
}

// GetConfig / SetConfig methods identical to existing PayloadStoreConfig pattern
```

### Updated Share Model

```go
// Source: Transform of existing Share in pkg/controlplane/models/share.go
type Share struct {
    ID                  string    `gorm:"primaryKey;size:36" json:"id"`
    Name                string    `gorm:"uniqueIndex;not null;size:255" json:"name"`
    MetadataStoreID     string    `gorm:"not null;size:36" json:"metadata_store_id"`
    LocalBlockStoreID   string    `gorm:"not null;size:36" json:"local_block_store_id"`
    RemoteBlockStoreID  *string   `gorm:"size:36" json:"remote_block_store_id"`
    ReadOnly            bool      `gorm:"default:false" json:"read_only"`
    // ... other fields same ...

    MetadataStore    MetadataStoreConfig  `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitempty"`
    LocalBlockStore  BlockStoreConfig     `gorm:"foreignKey:LocalBlockStoreID" json:"local_block_store,omitempty"`
    RemoteBlockStore *BlockStoreConfig    `gorm:"foreignKey:RemoteBlockStoreID" json:"remote_block_store"`
}
```

### Kind-Filtered GORM List

```go
// Source: Transform of existing listAll helper usage
func (s *GORMStore) ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error) {
    var results []*models.BlockStoreConfig
    if err := s.db.WithContext(ctx).Where("kind = ?", kind).Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}
```

### API Client Block Store Methods

```go
// Source: Transform of existing PayloadStore client methods in pkg/apiclient/stores.go
type BlockStore struct {
    ID     string          `json:"id"`
    Name   string          `json:"name"`
    Kind   string          `json:"kind"`
    Type   string          `json:"type"`
    Config json.RawMessage `json:"config,omitempty"`
}

func (c *Client) ListBlockStores(kind string) ([]BlockStore, error) {
    return listResources[BlockStore](c, fmt.Sprintf("/api/v1/store/block/%s", kind))
}

func (c *Client) CreateBlockStore(kind string, req *CreateStoreRequest) (*BlockStore, error) {
    configStr, err := serializeConfig(req.Config)
    if err != nil {
        return nil, err
    }
    apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
    var store BlockStore
    if err := c.post(fmt.Sprintf("/api/v1/store/block/%s", kind), apiReq, &store); err != nil {
        return nil, err
    }
    return &store, nil
}
```

### Share Create Request (Updated)

```go
// Source: Transform of existing CreateShareRequest in handlers/shares.go
type CreateShareRequest struct {
    Name              string    `json:"name"`
    MetadataStoreID   string    `json:"metadata_store_id"`
    LocalBlockStore   string    `json:"local_block_store"`       // required, name or ID
    RemoteBlockStore  *string   `json:"remote_block_store"`      // optional, name or ID
    ReadOnly          bool      `json:"read_only,omitempty"`
    DefaultPermission string    `json:"default_permission,omitempty"`
    BlockedOperations *[]string `json:"blocked_operations,omitempty"`
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| PayloadStoreConfig (single kind) | BlockStoreConfig with Kind discriminator | Phase 44 | Two-tier storage model |
| Share.PayloadStoreID (single FK) | LocalBlockStoreID + RemoteBlockStoreID | Phase 44 | Per-share storage isolation |
| /api/v1/payload-stores | /api/v1/store/block/{kind} | Phase 44 | Consistent API prefix |
| /api/v1/metadata-stores | /api/v1/store/metadata | Phase 44 | Consistent API prefix |
| dfsctl store payload | dfsctl store block local/remote | Phase 44 | Clearer CLI semantics |

**Deprecated/outdated:**
- `PayloadStoreConfig` model: Replaced by `BlockStoreConfig` with Kind field
- `PayloadStoreConfigStore` interface: Replaced by `BlockStoreConfigStore` with kind-filtered methods
- `/api/v1/payload-stores` routes: Removed entirely
- `/api/v1/metadata-stores` routes: Moved to `/api/v1/store/metadata`
- `dfsctl store payload` commands: Removed entirely
- `--payload` flag on `dfsctl share create`: Replaced by `--local` + `--remote`

## Open Questions

1. **Default path for `default-local` block store**
   - What we know: It needs a filesystem path for the local block store
   - What's unclear: Exact default path. Should it be relative to config dir or an absolute path?
   - Recommendation: Use `$XDG_CONFIG_HOME/dittofs/blocks/` or `~/.config/dittofs/blocks/` (matches existing config dir pattern from `gorm.go` line 97). Can also use `$XDG_DATA_HOME/dittofs/blocks/` since it's data, not config.

2. **Unique constraint scope for block store names**
   - What we know: Current `PayloadStoreConfig.Name` has a global uniqueIndex
   - What's unclear: Should uniqueness be per-kind or global? A local store named "default" and remote store named "default" -- conflict?
   - Recommendation: Keep global uniqueness (simpler, avoids ambiguity in CLI). The unique index on `Name` already prevents duplicates regardless of kind.

3. **Migration ordering: AutoMigrate vs. raw SQL**
   - What we know: AutoMigrate runs on `AllModels()` which will include new `BlockStoreConfig`
   - What's unclear: If `PayloadStoreConfig` is removed from `AllModels()`, AutoMigrate won't touch `payload_stores`. But if `BlockStoreConfig` is added, it creates `block_store_configs` as empty.
   - Recommendation: Run raw SQL rename BEFORE AutoMigrate. Remove `PayloadStoreConfig` from `AllModels()`, add `BlockStoreConfig`. If `payload_stores` table exists, rename it first. Then AutoMigrate adds the `kind` column to the renamed table.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing + `go test` |
| Config file | None (Go convention) |
| Quick run command | `go test ./pkg/controlplane/... -run TestBlockStore -count=1` |
| Full suite command | `go test ./pkg/controlplane/... -count=1 -tags=integration` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| MODEL-01 | BlockStoreConfig model CRUD | unit | `go test ./pkg/controlplane/store/ -run TestBlockStoreOperations -tags=integration -count=1` | Wave 0 |
| MODEL-02 | Share with local/remote block store IDs | unit | `go test ./pkg/controlplane/store/ -run TestShareBlockStore -tags=integration -count=1` | Wave 0 |
| MODEL-03 | Migration: payload_stores -> block_store_configs | unit | `go test ./pkg/controlplane/store/ -run TestMigrationBlockStore -tags=integration -count=1` | Wave 0 |
| MODEL-04 | Migration: Share PayloadStoreID split | unit | `go test ./pkg/controlplane/store/ -run TestMigrationShareBlockStore -tags=integration -count=1` | Wave 0 |
| MODEL-05 | BlockStoreConfigStore interface filtered by kind | unit | `go test ./pkg/controlplane/store/ -run TestBlockStoreKindFilter -tags=integration -count=1` | Wave 0 |
| API-01 | Local block store REST CRUD | unit | `go test ./internal/controlplane/api/handlers/ -run TestBlockStoreHandler -count=1` | Wave 0 |
| API-02 | Remote block store REST CRUD | unit | `go test ./internal/controlplane/api/handlers/ -run TestBlockStoreHandler -count=1` | Wave 0 |
| API-03 | Share endpoints with local/remote | unit | `go test ./internal/controlplane/api/handlers/ -run TestShareBlockStore -count=1` | Wave 0 |
| CLI-01 | dfsctl store block local commands | manual-only | Build + manual CLI test | N/A |
| CLI-02 | dfsctl store block remote commands | manual-only | Build + manual CLI test | N/A |
| CLI-03 | dfsctl share create --local --remote | manual-only | Build + manual CLI test | N/A |
| CLI-04 | API client block store methods | unit | `go test ./pkg/apiclient/ -run TestBlockStore -count=1` | Wave 0 |

### Sampling Rate
- **Per task commit:** `go build ./... && go vet ./...`
- **Per wave merge:** `go test ./pkg/controlplane/... -count=1 -tags=integration`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `pkg/controlplane/store/store_test.go` -- add `TestBlockStoreOperations`, `TestBlockStoreKindFilter`, `TestShareBlockStore` tests (existing file, add test functions)
- [ ] Migration tests can use existing `createTestStore` pattern with pre-populated data

## Sources

### Primary (HIGH confidence)
- Direct codebase analysis of all affected files (models, store, handlers, CLI, API client)
- `pkg/controlplane/models/stores.go` -- existing PayloadStoreConfig pattern
- `pkg/controlplane/store/interface.go` -- existing interface composition pattern
- `pkg/controlplane/store/payload.go` -- existing GORM method patterns
- `internal/controlplane/api/handlers/payload_stores.go` -- existing handler pattern
- `pkg/controlplane/api/router.go` -- existing chi routing pattern
- `cmd/dfsctl/commands/store/payload/` -- existing CLI command pattern
- `pkg/apiclient/stores.go` -- existing API client pattern
- `pkg/controlplane/store/gorm.go` -- existing migration pattern in New()

### Secondary (MEDIUM confidence)
- GORM documentation on AutoMigrate limitations (cannot rename tables/columns)
- chi router documentation on URL parameters (`{kind}` pattern)

### Tertiary (LOW confidence)
- None -- all findings directly verified from codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use, no new dependencies
- Architecture: HIGH - Transforming existing patterns, not inventing new ones
- Pitfalls: HIGH - Identified from direct code analysis of GORM migration behavior and FK constraints
- Migration strategy: MEDIUM - GORM table rename via raw SQL is well-known but migration ordering needs careful implementation

**Research date:** 2026-03-09
**Valid until:** 2026-04-09 (stable -- internal refactoring, no external dependency changes)
