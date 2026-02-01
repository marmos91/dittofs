# pkg/controlplane

Control plane for DittoFS - manages users, groups, shares, adapters, and settings.

## Architecture

```
pkg/controlplane/
├── models/          # Domain models (User, Group, Share, Adapter)
├── store/           # GORM-based persistent storage (SQLite/PostgreSQL)
├── runtime/         # Ephemeral state (metadata stores, mounts, shares)
└── api/             # REST API server (router, config)

internal/controlplane/
└── api/
    ├── auth/        # JWT service and claims
    ├── handlers/    # HTTP handlers for each resource
    └── middleware/  # Auth middleware (JWTAuth, RequireAdmin)
```

## Key Components

### Store (`pkg/controlplane/store/`)
- GORM-based persistent storage
- Supports SQLite (single-node) and PostgreSQL (HA)
- CRUD operations for all resources
- User authentication with bcrypt + NT hash for SMB

### Runtime (`pkg/controlplane/runtime/`)
- Ephemeral state manager
- Metadata store registry
- Active share management with root handles
- Mount tracking for NFS/SMB
- Identity resolution for protocol operations

### Models (`pkg/controlplane/models/`)
- Domain objects: User, Group, Share, Adapter, Setting
- Permission types: SharePermission (none, read, read-write, admin)
- Validation methods and helpers

## Critical Conventions

### GORM Zero-Value Handling
Boolean fields with `gorm:"default:true"` require special handling:
```go
// Creating with Enabled=false won't work directly
user := &models.User{Enabled: false}  // GORM applies default:true

// Workaround: Create then update
user := &models.User{Enabled: true}
store.CreateUser(ctx, user)
user.Enabled = false
store.UpdateUser(ctx, user)
```

### Security
- `ValidateCredentials` returns `ErrInvalidCredentials` for both wrong password
  AND non-existent user (prevents user enumeration)
- Never expose `ErrUserNotFound` through login endpoints

### Transaction Rules
- All stores support `WithTransaction(ctx, fn)` for atomic ops
- Nested transactions NOT supported
- Transaction isolation is store-specific

## Testing

### Store Tests (Integration)
```go
//go:build integration

func createTestStore(t *testing.T) *GORMStore {
    store, _ := New(&Config{
        Type: DatabaseTypeSQLite,
        SQLite: SQLiteConfig{Path: ":memory:"},
    })
    return store
}
```

### Handler Tests (Integration)
Use real SQLite store + httptest:
```go
//go:build integration

func setupTest(t *testing.T) (store.Store, *Handler) {
    cpStore, _ := store.New(&store.Config{...})
    handler := NewHandler(cpStore)
    return cpStore, handler
}
```
