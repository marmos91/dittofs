# Phase 29: Core Layer Decomposition - Research

**Researched:** 2026-02-26
**Domain:** Go codebase refactoring — interface decomposition, error unification, file splitting
**Confidence:** HIGH

## Summary

Phase 29 is a pure refactoring phase: decompose god objects into focused sub-services, unify errors with structured types, reduce CRUD boilerplate with generics, and reorganize large files into coherent sub-packages. The codebase is Go 1.25 with full generics support. All changes are behavioral no-ops — the existing E2E test suite is the primary validation mechanism.

The key technical challenge is the **dependency graph**: the Runtime holds references to MetadataService and PayloadService, which hold stores, which have their own interfaces. Decomposition must proceed bottom-up (leaf packages first, then services, then Runtime) to ensure each layer compiles before moving up. The CONTEXT.md decisions are comprehensive and well-considered — they lock the sub-package structure, naming conventions, and migration approach.

**Primary recommendation:** Execute layer-by-layer bottom-up. Start with error types and generic helpers (zero-dependency additions), then decompose the ControlPlane Store interface, then MetadataService and PayloadService, then Runtime. Each layer must compile and pass unit tests before proceeding.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **Interface Composition Pattern**: narrowest interfaces everywhere, composite embeds sub-interfaces, interfaces in sub-packages, entity-grouped
- **ControlPlane Store**: flat package, renamed files (adapter_configs.go absorbed into shares.go, etc.), ~9 named sub-interfaces + composite Store, API handlers narrowed, router passes full Store
- **Runtime Decomposition**: 6 sub-services (adapters, shares, stores, mounts, lifecycle, identity) as sub-packages under `pkg/controlplane/runtime/`
- **MetadataService Decomposition**: `pkg/metadata/file/` and `pkg/metadata/auth/` sub-packages, FileService and AuthService, composite in parent
- **PayloadService Decomposition**: 3 sub-services (io, offloader, gc), `pkg/payload/io/`, `pkg/payload/offloader/`, `pkg/payload/gc/`
- **Offloader Rename/Split**: TransferManager -> Offloader, `pkg/payload/transfer/` -> `pkg/payload/offloader/`, file split into offloader.go, upload.go, download.go, dedup.go, queue.go, entry.go, types.go; keep TransferQueue/TransferRequest names; GC extracted to `pkg/payload/gc/`
- **Error Unification**: PayloadError (structured, in `pkg/payload/`), ProtocolError (generic interface in `pkg/adapter/`), unified Response/Request for API, error middleware, centralized API error mapping
- **Auth Centralization**: `pkg/auth/` with auth.go, identity.go, kerberos/ sub-package; provider chain; IdentityMapper in adapter interface; TranslateAuth method; generic Identity type
- **Testing**: in-memory implementations (not mocks), conformance suite in `pkg/metadata/storetest/`, tests follow code into sub-packages
- **Naming**: Service for logic, Store for data, no underscore files, service.go + operation files pattern
- **Migration**: layer by layer bottom-up, one PR per layer, clean break (no aliases), unit tests per layer, E2E at the end
- **Documentation**: full docs update with each PR, godoc pass, per-package doc.go

### Claude's Discretion
- Package location for ControlPlane Store sub-interfaces (flat vs sub-packages — evaluate based on actual interface sizes)
- WAL replay file naming (wal_replay.go vs startup_replay.go)
- Composite interface naming (runtime.Runtime vs runtime.Service)
- Exact sub-interface groupings if methods don't cleanly separate
- Godoc wording and examples
- Generic helper function signatures

### Deferred Ideas (OUT OF SCOPE)
None — discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| REF-05.1 | ControlPlane Store interface decomposed into 9 sub-interfaces | Interface analysis (514-line interface.go) identifies natural groupings; see Architecture Patterns section |
| REF-05.2 | API handlers narrowed to accept specific sub-interfaces | 12 handler files currently accept `store.Store`; narrowing analysis complete |
| REF-05.3 | AdapterManager extracted from Runtime (~300 lines) | Runtime function analysis identifies adapter methods (lines 857-1203) |
| REF-05.4 | MetadataStoreManager extracted from Runtime (~70 lines) | Runtime function analysis identifies metadata store methods (lines 350-415) |
| REF-05.5 | TransferManager renamed to Offloader, package to `pkg/payload/offloader/` | 1361-line manager.go mapped; function groupings identified |
| REF-05.6 | Upload orchestrator extracted to upload.go (~450 lines) | Upload functions identified in manager.go |
| REF-05.7 | Download coordinator extracted to download.go (~250 lines) | Download functions identified in manager.go |
| REF-05.8 | Dedup handler extracted to dedup.go (~150 lines) | Dedup functions identified in manager.go |
| REF-06.1 | Structured PayloadError type wrapping sentinels | Current errors.go has 12 sentinel errors; PayloadError wraps these |
| REF-06.2 | Shared error-to-status mapping for NFS/SMB | ProtocolError interface pattern documented |
| REF-06.3 | Generic GORM helpers | GORM boilerplate patterns identified across 6 store files |
| REF-06.4 | Centralized API error mapping | Current per-handler error mapping analyzed |
| REF-06.5 | Generic API client helpers | API client boilerplate patterns identified across 9 files |
| REF-06.6 | Common transaction helpers in txutil | Not directly in success criteria but supports REF-06.3 |
| REF-06.7 | Shared transaction test suite in storetest | Conformance test pattern for metadata stores |
| REF-06.8 | file.go split into file_create.go, file_modify.go, file_remove.go, file_helpers.go | 1217-line file.go function mapping complete |
| REF-06.9 | authentication.go split into identity.go, permissions.go | 796-line authentication.go function mapping complete |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.25 | Language runtime | Full generics, range-over-func, type aliases for generics |
| GORM | v2 (glebarez/sqlite + gorm.io/driver/postgres) | ORM for ControlPlane store | Already in use; generic helpers leverage GORM's type system |
| go-chi/chi/v5 | v5 | HTTP router | Already in use; middleware composition for error/request handling |
| errors (stdlib) | 1.25 | Error wrapping and matching | `errors.Is()`, `errors.As()` for PayloadError compatibility |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| testing (stdlib) | 1.25 | Test framework | All unit tests |
| io (stdlib) | 1.25 | Import alias `payloadio` for pkg/payload/io | Avoid stdlib conflict |

### Alternatives Considered
None — this is a refactoring phase that uses the existing technology stack. No new libraries are introduced.

## Architecture Patterns

### Recommended Target Structure (after decomposition)

```
pkg/
├── adapter/
│   ├── adapter.go          # Adapter interface (existing)
│   ├── auth.go             # Authenticator + AuthResult (existing)
│   ├── errors.go           # ProtocolError interface (NEW)
│   ├── base.go             # BaseAdapter (existing)
│   ├── nfs/                # NFS adapter (existing)
│   └── smb/                # SMB adapter (existing)
│
├── auth/                   # Centralized auth (EXPANDED)
│   ├── auth.go             # DittoFS Authenticator interface, AuthResult, errors
│   ├── identity.go         # Identity model, IdentityMapper interface
│   └── kerberos/           # Kerberos provider (existing)
│       ├── provider.go     # (was kerberos.go)
│       ├── keytab.go       # (existing)
│       └── config.go       # (existing)
│
├── controlplane/
│   ├── store/              # FLAT package, restructured files
│   │   ├── interface.go    # Sub-interfaces + composite Store
│   │   ├── helpers.go      # Generic GORM helpers (NEW)
│   │   ├── gorm.go         # GORMStore constructor + db init
│   │   ├── users.go        # UserStore impl
│   │   ├── groups.go       # GroupStore impl
│   │   ├── shares.go       # ShareStore impl (+ShareAdapterConfig methods)
│   │   ├── permissions.go  # PermissionStore impl
│   │   ├── adapters.go     # AdapterStore impl (+global settings)
│   │   ├── metadata.go     # MetadataStoreConfigStore impl (was metadata_stores.go)
│   │   ├── payload.go      # PayloadStoreConfigStore impl (was payload_stores.go)
│   │   ├── identity.go     # IdentityMappingStore impl (was identity_mappings.go)
│   │   ├── settings.go     # SettingsStore impl
│   │   ├── health.go       # HealthStore impl
│   │   ├── netgroups.go    # NetgroupStore impl (separate interface, existing pattern)
│   │   └── store_test.go   # (existing, integration build tag)
│   │
│   ├── runtime/            # Runtime with sub-services
│   │   ├── runtime.go      # Reduced Runtime (~500 lines), composes sub-services
│   │   ├── adapters/       # AdapterManager sub-service (NEW)
│   │   │   └── service.go
│   │   ├── shares/         # ShareManager sub-service (NEW)
│   │   │   └── service.go
│   │   ├── stores/         # MetadataStoreManager sub-service (NEW)
│   │   │   └── service.go
│   │   ├── mounts/         # MountManager sub-service (NEW — from existing mounts.go)
│   │   │   └── service.go
│   │   ├── lifecycle/      # Lifecycle sub-service (NEW — Serve/shutdown)
│   │   │   └── service.go
│   │   ├── identity/       # IdentityMapper sub-service (NEW)
│   │   │   └── service.go
│   │   ├── init.go         # (existing, startup logic)
│   │   ├── share.go        # Share type (existing)
│   │   ├── mounts.go       # MountTracker (existing, absorbed into mounts/)
│   │   ├── settings_watcher.go  # (existing)
│   │   └── netgroups.go    # (existing)
│   │
│   └── api/
│       ├── router.go       # Router passes narrow interfaces (UPDATED)
│       ├── server.go       # (existing)
│       └── config.go       # (existing)
│
├── metadata/
│   ├── service.go          # Composite Service embedding file.Service + auth.Service (UPDATED)
│   ├── file/               # FileService sub-package (NEW)
│   │   ├── service.go      # Interface + struct + constructor
│   │   ├── create.go       # CreateFile, CreateSymlink, CreateSpecialFile, CreateHardLink
│   │   ├── modify.go       # SetFileAttributes, Move, MarkFileAsOrphaned, ReadSymlink
│   │   ├── remove.go       # RemoveFile, RemoveDirectory
│   │   └── helpers.go      # buildPath, buildPayloadID, MakeRdev, etc.
│   ├── auth/               # AuthService sub-package (NEW)
│   │   ├── service.go      # Interface + struct + constructor
│   │   ├── identity.go     # AuthContext, Identity types, ApplyIdentityMapping
│   │   └── permissions.go  # CheckShareAccess, checkFilePermissions, calculatePermissions
│   ├── errors.go           # (existing)
│   ├── types.go            # (existing)
│   └── ...                 # Other existing files
│
├── payload/
│   ├── service.go          # Composite Service embedding io.Service + offloader (UPDATED)
│   ├── errors.go           # PayloadError structured type (UPDATED)
│   ├── io/                 # PayloadIO sub-package (NEW)
│   │   ├── read.go         # ReadAt, ReadAtWithCOWSource
│   │   └── write.go        # WriteAt, Truncate, Delete, writeBlockWithRetry
│   ├── offloader/          # Offloader (RENAMED from transfer/)
│   │   ├── offloader.go    # Struct, constructor, Flush(), Close(), orchestration
│   │   ├── upload.go       # Upload methods (tryEagerUpload, startBlockUpload, uploadBlock, etc.)
│   │   ├── download.go     # Download methods (downloadBlock, EnsureAvailable, enqueueDownload, etc.)
│   │   ├── dedup.go        # Dedup handler (handleUploadSuccess, content-addressed dedup)
│   │   ├── queue.go        # TransferQueue (existing queue.go)
│   │   ├── entry.go        # TransferQueueEntry (existing entry.go)
│   │   ├── types.go        # Config, FlushResult, etc. (existing types.go)
│   │   ├── wal_replay.go   # WAL recovery (was recovery.go)
│   │   └── *_test.go       # Tests follow code
│   ├── gc/                 # GC sub-package (EXTRACTED from transfer/)
│   │   ├── gc.go           # CollectGarbage function
│   │   └── gc_test.go
│   └── ...
│
└── apiclient/
    ├── helpers.go           # Generic helpers (NEW)
    └── ...                  # Existing files
```

### Pattern 1: Interface Composition (Locked Decision)

**What:** Break a large interface into entity-grouped sub-interfaces; composite embeds all of them.
**When to use:** Every decomposition target (ControlPlane Store, MetadataService, PayloadService, Runtime).

```go
// pkg/controlplane/store/interface.go

// Sub-interfaces (entity-grouped)
type UserStore interface {
    GetUser(ctx context.Context, username string) (*models.User, error)
    GetUserByID(ctx context.Context, id string) (*models.User, error)
    GetUserByUID(ctx context.Context, uid uint32) (*models.User, error)
    ListUsers(ctx context.Context) ([]*models.User, error)
    CreateUser(ctx context.Context, user *models.User) (string, error)
    UpdateUser(ctx context.Context, user *models.User) error
    DeleteUser(ctx context.Context, username string) error
    UpdatePassword(ctx context.Context, username, passwordHash, ntHash string) error
    UpdateLastLogin(ctx context.Context, username string, timestamp time.Time) error
    ValidateCredentials(ctx context.Context, username, password string) (*models.User, error)
}

type GroupStore interface { /* ... */ }
type ShareStore interface { /* ... includes ShareAdapterConfig ops */ }
type PermissionStore interface { /* ... user + group share perms + resolution */ }
type MetadataStoreConfigStore interface { /* ... */ }
type PayloadStoreConfigStore interface { /* ... */ }
type AdapterStore interface { /* ... + adapter settings */ }
type SettingsStore interface { /* ... */ }
type AdminStore interface { /* EnsureAdminUser, IsAdminInitialized, EnsureDefaultGroups */ }

// Health and lifecycle
type HealthStore interface {
    Healthcheck(ctx context.Context) error
    Close() error
}

// Composite — embeds all sub-interfaces
type Store interface {
    UserStore
    GroupStore
    ShareStore
    PermissionStore
    MetadataStoreConfigStore
    PayloadStoreConfigStore
    AdapterStore
    SettingsStore
    AdminStore
    HealthStore
}
```

### Pattern 2: Handler Narrowing

**What:** Each API handler constructor accepts only the sub-interface it needs.
**When to use:** All API handlers.

```go
// Before
type UserHandler struct {
    store store.Store  // uses only ~5 of ~70 methods
}

// After
type UserHandler struct {
    store store.UserStore  // narrowed to exactly what it needs
}

// Router passes narrow interfaces from full Store
func NewRouter(rt *runtime.Runtime, jwtService *auth.JWTService, cpStore store.Store) http.Handler {
    userHandler := handlers.NewUserHandler(cpStore, jwtService)   // cpStore satisfies store.UserStore
    groupHandler := handlers.NewGroupHandler(cpStore)              // cpStore satisfies store.GroupStore
    // ...
}
```

### Pattern 3: Generic GORM Helpers

**What:** Reduce CRUD boilerplate in GORM store implementations.
**When to use:** ControlPlane store methods with repetitive get/list/create patterns.

```go
// pkg/controlplane/store/helpers.go

func getByField[T any](db *gorm.DB, ctx context.Context, field string, value any, notFoundErr error, preloads ...string) (*T, error) {
    var result T
    q := db.WithContext(ctx)
    for _, p := range preloads {
        q = q.Preload(p)
    }
    if err := q.Where(field+" = ?", value).First(&result).Error; err != nil {
        return nil, convertNotFoundError(err, notFoundErr)
    }
    return &result, nil
}

func listAll[T any](db *gorm.DB, ctx context.Context, preloads ...string) ([]*T, error) {
    var results []*T
    q := db.WithContext(ctx)
    for _, p := range preloads {
        q = q.Preload(p)
    }
    if err := q.Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}

func createWithID[T interface{ SetID(string) }](db *gorm.DB, ctx context.Context, entity T, dupErr error) (string, error) {
    // ... generate UUID if empty, create, handle unique constraint
}
```

### Pattern 4: Structured PayloadError

**What:** Wrap sentinel errors with performance debugging context.
**When to use:** All payload/offloader operations that produce errors.

```go
// pkg/payload/errors.go

type PayloadError struct {
    Op        string        // "upload", "download", "dedup", "gc"
    Share     string        // Share name for routing context
    PayloadID string        // File content identifier
    BlockIdx  uint32        // Block index within chunk
    Size      int64         // Data size involved
    Duration  time.Duration // How long the operation took
    Retries   int           // Number of retry attempts
    Backend   string        // "s3", "memory", "filesystem"
    Err       error         // Wrapped sentinel (ErrContentNotFound, etc.)
}

func (e *PayloadError) Error() string {
    return fmt.Sprintf("payload %s: %s (share=%s, payload=%s, block=%d, backend=%s): %v",
        e.Op, e.Err, e.Share, e.PayloadID, e.BlockIdx, e.Backend, e.Err)
}

func (e *PayloadError) Unwrap() error { return e.Err }
// errors.Is(payloadErr, ErrContentNotFound) works via Unwrap chain
```

### Pattern 5: ProtocolError Interface

**What:** Generic error interface for protocol adapters (NFS, SMB).
**When to use:** Protocol-level error translation in adapters.

```go
// pkg/adapter/errors.go

type ProtocolError interface {
    error
    Code() uint32
    Message() string
    Unwrap() error
}

// Each adapter implements:
// NFS: nfsError{code: NFS3ERR_NOENT, msg: "...", err: metadata.ErrNoEntity}
// SMB: smbError{code: STATUS_OBJECT_NAME_NOT_FOUND, msg: "...", err: metadata.ErrNoEntity}
```

### Pattern 6: API Error Middleware

**What:** Centralized error mapping via middleware, handlers return `*Response` or `error`.
**When to use:** All API handlers.

```go
// Middleware wraps handlers, handles encoding
func ErrorMiddleware(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Call handler — handler focuses on logic only
        next(w, r)
        // Error responses already written via WriteProblem helpers
    }
}

// Request validation middleware
func ValidateRequest[T any](next func(w http.ResponseWriter, r *http.Request, req T)) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var req T
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            BadRequest(w, "Invalid request body")
            return
        }
        next(w, r, req)
    }
}
```

### Pattern 7: Sub-Service Composition in Runtime

**What:** Runtime composes independent sub-services, each with its own interface.
**When to use:** Runtime decomposition.

```go
// pkg/controlplane/runtime/runtime.go (after decomposition)

type Runtime struct {
    adapters  *adapters.Service
    shares    *shares.Service
    stores    *stores.Service
    mounts    *mounts.Service
    lifecycle *lifecycle.Service
    identity  *identity.Service

    // Cross-cutting
    store           store.Store
    metadataService *metadata.MetadataService
    payloadService  *payload.PayloadService
    cacheInstance   *cache.Cache
}
```

### Anti-Patterns to Avoid
- **Circular imports between sub-packages:** Each sub-package defines its own interface. Never import sibling sub-packages directly. The parent package composes them.
- **Leaking implementation details through interfaces:** Sub-interfaces should only expose methods the consumer actually calls. Don't add methods "just in case."
- **Temporary aliases during migration:** The decision is "clean break" — all importers update in the same PR. No deprecated re-exports.
- **Over-splitting files:** Don't create a file for every single function. Group related operations (e.g., all create operations in create.go).

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| GORM boilerplate | Copy-paste CRUD per entity | Generic helpers `getByField[T]`, `listAll[T]` | 6 files have identical patterns; Go 1.25 generics reduce this cleanly |
| Error wrapping | Manual `fmt.Errorf` with context | PayloadError struct with `Unwrap()` | Ensures `errors.Is()` works; structured fields for debugging |
| API client boilerplate | Per-resource GET/POST/DELETE | Generic `getResource[T]`, `listResources[T]` | 9 API client files with near-identical HTTP patterns |
| Request validation | Per-handler decode+validate | Validation middleware | Handlers trust their input; 400 errors handled before reaching logic |

**Key insight:** The boilerplate in this codebase follows extremely consistent patterns (GORM CRUD, API client HTTP calls, handler request parsing). Generics eliminate this cleanly without sacrificing type safety.

## Common Pitfalls

### Pitfall 1: Circular Import Chains
**What goes wrong:** Moving code into sub-packages creates import cycles (e.g., `metadata/file` imports `metadata` which imports `metadata/file`).
**Why it happens:** Sub-packages need types defined in the parent, and the parent needs to reference sub-package services.
**How to avoid:** Sub-packages define their own interfaces. Types shared across sub-packages (FileHandle, File, AuthContext) stay in the parent package. The parent composes sub-services via their interfaces, not concrete types.
**Warning signs:** `import cycle not allowed` compiler error after moving first file.

### Pitfall 2: Breaking the Interface Satisfaction Contract
**What goes wrong:** After splitting `store.Store` into sub-interfaces, GORMStore no longer satisfies the composite because a method was missed.
**Why it happens:** Manual interface decomposition with ~70 methods is error-prone.
**How to avoid:** Use compile-time assertions (`var _ Store = (*GORMStore)(nil)`) for every interface. Add them early and let the compiler catch misses.
**Warning signs:** Missing methods in sub-interface cause compile error in test files.

### Pitfall 3: Forgetting to Update Import Paths
**What goes wrong:** Code compiles but tests fail because imports still point to old locations.
**Why it happens:** Renaming `pkg/payload/transfer/` to `pkg/payload/offloader/` has 15+ importers.
**How to avoid:** Use `goimports` or IDE rename. After every package move, run `go build ./...` and `go vet ./...` before committing.
**Warning signs:** "package not found" errors from files that weren't updated.

### Pitfall 4: Over-Narrowing Sub-Interfaces
**What goes wrong:** A handler needs method X but its sub-interface doesn't include it, requiring the handler to accept a wider interface anyway.
**Why it happens:** Not carefully auditing which store methods each handler actually calls.
**How to avoid:** Before defining sub-interfaces, grep each handler for every `h.store.XXX` call. Group methods by actual usage, not by entity name alone.
**Warning signs:** Handler receives `UserStore` but also calls `ResolveSharePermission` (which is in `PermissionStore`).

### Pitfall 5: Test Build Tag Isolation
**What goes wrong:** Store tests have `//go:build integration` tag and don't run in normal `go test ./...`.
**Why it happens:** The existing store_test.go uses integration build tag (requires SQLite).
**How to avoid:** When splitting tests, preserve the build tag. For new sub-package tests that only need in-memory implementations, consider dropping the integration tag.
**Warning signs:** "0 tests run" when expecting test validation.

### Pitfall 6: Renamed Package Breaks Type Aliases
**What goes wrong:** `FlushResult = transfer.FlushResult` breaks when transfer is renamed to offloader.
**Why it happens:** Type aliases reference the old package path.
**How to avoid:** Update all type aliases in the same PR as the package rename. Search for the old package name across the entire codebase.
**Warning signs:** Compile errors in `pkg/payload/types.go` after renaming transfer.

## Code Examples

### ControlPlane Store Sub-Interface Groupings (based on actual interface.go analysis)

The current 514-line `interface.go` has ~70 methods. Based on analysis, here are the natural groupings:

```
UserStore (10 methods):
  GetUser, GetUserByID, GetUserByUID, ListUsers, CreateUser, UpdateUser,
  DeleteUser, UpdatePassword, UpdateLastLogin, ValidateCredentials

GroupStore (9 methods):
  GetGroup, GetGroupByID, ListGroups, CreateGroup, UpdateGroup, DeleteGroup,
  GetUserGroups, AddUserToGroup, RemoveUserFromGroup, GetGroupMembers,
  EnsureDefaultGroups

MetadataStoreConfigStore (7 methods):
  GetMetadataStore, GetMetadataStoreByID, ListMetadataStores,
  CreateMetadataStore, UpdateMetadataStore, DeleteMetadataStore,
  GetSharesByMetadataStore

PayloadStoreConfigStore (7 methods):
  GetPayloadStore, GetPayloadStoreByID, ListPayloadStores,
  CreatePayloadStore, UpdatePayloadStore, DeletePayloadStore,
  GetSharesByPayloadStore

ShareStore (11 methods):
  GetShare, GetShareByID, ListShares, CreateShare, UpdateShare, DeleteShare,
  GetUserAccessibleShares, GetShareAccessRules, SetShareAccessRules,
  AddShareAccessRule, RemoveShareAccessRule
  + GetShareAdapterConfig, SetShareAdapterConfig, DeleteShareAdapterConfig,
    ListShareAdapterConfigs (from adapter_configs.go — share-scoped)

PermissionStore (9 methods):
  GetUserSharePermission, SetUserSharePermission, DeleteUserSharePermission,
  GetUserSharePermissions, GetGroupSharePermission, SetGroupSharePermission,
  DeleteGroupSharePermission, GetGroupSharePermissions,
  ResolveSharePermission

GuestStore (2 methods):
  GetGuestUser, IsGuestEnabled

AdapterStore (10 methods):
  GetAdapter, ListAdapters, CreateAdapter, UpdateAdapter, DeleteAdapter,
  EnsureDefaultAdapters
  + GetNFSAdapterSettings, UpdateNFSAdapterSettings, ResetNFSAdapterSettings,
    GetSMBAdapterSettings, UpdateSMBAdapterSettings, ResetSMBAdapterSettings,
    EnsureAdapterSettings (from adapter_settings.go — merged)

SettingsStore (4 methods):
  GetSetting, SetSetting, DeleteSetting, ListSettings

AdminStore (2 methods):
  EnsureAdminUser, IsAdminInitialized

HealthStore (2 methods):
  Healthcheck, Close
```

**Note:** GuestStore could fold into UserStore or ShareStore (Claude's discretion). The total is ~9 named sub-interfaces + Health/lifecycle. This aligns with the "~9 sub-interfaces" target from CONTEXT.md.

### TransferManager -> Offloader File Split Mapping

Based on function analysis of the 1361-line manager.go:

```
offloader.go (~200 lines): TransferManager struct renamed to Offloader,
  New(), SetFinalizationCallback(), Flush(), flushSmallFileSync(),
  WaitForEagerUploads(), WaitForAllUploads(), canProcess(), Start(), Close(),
  HealthCheck()

upload.go (~450 lines): OnWriteComplete(), tryEagerUpload(),
  startBlockUpload(), uploadRemainingBlocks(), uploadBlock()

download.go (~300 lines): downloadBlock(), EnsureAvailable(),
  enqueueDownload(), enqueuePrefetch(), isBlockInCache(), isRangeInCache(),
  waitForDownloads()

dedup.go (~200 lines): handleUploadSuccess(), getOrCreateUploadState(),
  getUploadState(), getOrderedBlockHashes(), invokeFinalizationCallback(),
  DeleteWithRefCount()

types.go (existing): Config, FlushResult, TransferType, etc.
queue.go (existing): TransferQueue
entry.go (existing): TransferQueueEntry
wal_replay.go (renamed from recovery.go): WAL recovery functions
```

Remaining functions: GetFileSize(), Exists(), Truncate(), Delete() -> stay in offloader.go (core lifecycle).

### file.go Split Mapping (1217 lines)

```
file/create.go: CreateFile (line 218), CreateSymlink (223), CreateSpecialFile (233),
  CreateHardLink (250), createEntry (865)

file/modify.go: SetFileAttributes (365), Move (596), MarkFileAsOrphaned (835),
  ReadSymlink (338), Lookup (168)

file/remove.go: RemoveFile (32) — the biggest single function (~130 lines)

file/helpers.go: buildPath (1057), buildPayloadID (1065),
  MakeRdev (1197), RdevMajor (1202), RdevMinor (1207), GetInitialLinkCount (1212)
```

### authentication.go Split Mapping (796 lines)

```
auth/identity.go: AuthContext struct (line 22), Identity struct (65),
  HasGID (127), ApplyIdentityMapping (371), IsAdministratorSID (434),
  MatchesIPPattern (450), CopyFileAttr (506)

auth/permissions.go: CheckShareAccess (207), checkFilePermissions (550),
  calculatePermissions (597), evaluateACLPermissions (664),
  evaluateWithACL (730), checkWritePermission (748), checkReadPermission (765),
  checkExecutePermission (782), CalculatePermissionsFromBits (475),
  CheckOtherPermissions (495)
```

### Runtime Decomposition Method Mapping

```
adapters/ (~350 lines):
  CreateAdapter, DeleteAdapter, UpdateAdapter, EnableAdapter, DisableAdapter,
  startAdapter, stopAdapter, StopAllAdapters, LoadAdaptersFromStore,
  ListRunningAdapters, IsAdapterRunning, AddAdapter, registerAndRunAdapterLocked,
  SetAdapterFactory

stores/ (~70 lines):
  RegisterMetadataStore, GetMetadataStore, GetMetadataStoreForShare,
  ListMetadataStores, CountMetadataStores, CloseMetadataStores

shares/ (~200 lines):
  AddShare, RemoveShare, UpdateShare, GetShare, GetRootHandle, ListShares,
  ShareExists, OnShareChange, notifyShareChange, GetShareNameForHandle,
  CountShares

mounts/ (~50 lines):
  Mounts, RecordMount, RemoveMount, RemoveAllMounts, ListMounts

lifecycle/ (~100 lines):
  Serve, serve, shutdown, SetShutdownTimeout, SetAPIServer

identity/ (~70 lines):
  ApplyIdentityMapping, applyAnonymousIdentity, applyRootIdentity

Remaining in runtime.go (~500 lines):
  Runtime struct, New(), Store(), GetMetadataService(), GetPayloadService(),
  SetPayloadService(), SetCacheConfig(), GetCacheConfig(), SetCache(), GetCache(),
  GetUserStore(), GetIdentityStore(), GetBlockService(), GetSettingsWatcher(),
  GetNFSSettings(), GetSMBSettings(), SetAdapterProvider(), GetAdapterProvider(),
  SetNFSClientProvider(), NFSClientProvider()
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Single large interface for all store ops | Interface composition with embedding | Go best practice (Rob Pike, Dave Cheney) | Enables testability, reduces coupling |
| `fmt.Errorf` with string context | Structured error types with `Unwrap()` | Go 1.13+ (2019) | `errors.Is()` and `errors.As()` work correctly |
| Copy-paste CRUD per entity | Generic helpers `func getByField[T any]` | Go 1.18+ (2022) | Type-safe boilerplate reduction |
| Per-handler error mapping | Middleware-based centralized error handling | Standard Go HTTP pattern | Single point of change for error format |

## Open Questions

1. **GuestStore placement**
   - What we know: GetGuestUser and IsGuestEnabled are 2 methods, could be UserStore or ShareStore
   - What's unclear: Whether callers needing guest access also need other user/share methods
   - Recommendation: Fold into UserStore since it returns *User. Claude's discretion item.

2. **AdapterSettingsHandler interface needs**
   - What we know: AdapterSettingsHandler uses both adapter CRUD and adapter settings methods from store.Store + runtime
   - What's unclear: Whether to give it AdapterStore directly (which includes settings) or a separate AdapterSettingsStore
   - Recommendation: Include adapter settings in AdapterStore (single entity, single interface). The handler accepts AdapterStore.

3. **ShareHandler complexity**
   - What we know: ShareHandler calls store methods for shares, permissions, access rules, AND runtime methods for share lifecycle
   - What's unclear: Whether ShareStore + PermissionStore should be combined or the handler accepts both
   - Recommendation: Handler accepts both ShareStore and PermissionStore. This keeps the interfaces focused while the handler composes as needed.

## Sources

### Primary (HIGH confidence)
- **Codebase analysis**: Direct reading of all 30+ source files involved in decomposition
- `pkg/controlplane/store/interface.go` (514 lines, ~70 methods) — complete interface mapping
- `pkg/controlplane/runtime/runtime.go` (1203 lines, 50+ methods) — complete function mapping
- `pkg/payload/transfer/manager.go` (1361 lines, 30+ methods) — complete function mapping
- `pkg/metadata/file.go` (1217 lines) — complete function mapping
- `pkg/metadata/authentication.go` (796 lines) — complete function mapping
- `pkg/metadata/service.go` (497 lines) — service structure analysis
- `pkg/payload/service.go` (407 lines) — service structure analysis
- All 12 API handler files — store.Store usage audit
- `go.mod` — confirmed Go 1.25 with full generics support

### Secondary (MEDIUM confidence)
- Go interface composition patterns (standard community practice, well-documented)
- GORM generic helper patterns (Go 1.18+ capability, widely used)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — no new libraries, pure refactoring of existing Go code
- Architecture: HIGH — all patterns verified against actual codebase structure and import graph
- Pitfalls: HIGH — based on concrete analysis of import dependencies and interface contracts

**Research date:** 2026-02-26
**Valid until:** 2026-04-26 (90 days — stable codebase, no external dependency changes)
