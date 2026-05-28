# Phase 3: Stores - Research

**Researched:** 2026-02-02
**Domain:** Metadata and Payload Store CRUD via CLI, E2E test patterns for store management
**Confidence:** HIGH

## Summary

This phase implements E2E tests validating metadata store CRUD (memory, BadgerDB, PostgreSQL) and payload store CRUD (memory, filesystem, S3) operations via the `dittofsctl` CLI. The research confirms that all CLI commands, API endpoints, and store types are already implemented. The tests need to exercise these existing implementations through the CLI interface.

The existing codebase has mature test infrastructure from Phases 1-2 with `CLIRunner`, `TestEnvironment`, and functional options patterns. The store commands follow the same patterns as user/group commands (`dittofsctl store metadata add/list/edit/remove` and `dittofsctl store payload add/list/edit/remove`). The critical constraint is that stores referenced by shares cannot be deleted (returns `ErrStoreInUse`).

**Primary recommendation:** Extend CLIRunner with store CRUD methods following the Phase 2 functional options pattern. Each store type (memory, badger, postgres, filesystem, s3) needs type-specific options. Test the "store in use" constraint by creating a share referencing a store, then attempting deletion.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| os/exec | stdlib | Execute CLI commands (dittofsctl) | Standard subprocess execution |
| stretchr/testify | v1.11.1 | Assertions (require for hard failures) | Already in go.mod, project standard |
| test/e2e/helpers | internal | CLIRunner, TestEnvironment | Phase 1-2 infrastructure |
| encoding/json | stdlib | Parse CLI JSON output | Structured output verification |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/google/uuid | v1.6.0 | Generate unique test names | Test isolation |
| time | stdlib | Timeouts for container startup | Wait for Postgres/S3 readiness |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| CLI testing | Direct apiclient calls | CLI tests are the goal per project decisions |
| Testcontainers for Postgres/S3 | Real instances | Project decided Testcontainers (self-contained) |

**Installation:**
```bash
# No additional packages needed - all in go.mod
```

## Architecture Patterns

### Recommended Project Structure
```
test/e2e/
├── helpers/
│   └── cli.go            # Add MetadataStore, PayloadStore types and methods
├── metadata_stores_test.go    # NEW: Metadata store CRUD tests
└── payload_stores_test.go     # NEW: Payload store CRUD tests
```

### Pattern 1: Store Type Definitions
**What:** Define response types matching API JSON output for store operations
**When to use:** Parsing all store CLI responses
**Example:**
```go
// Source: Existing pkg/apiclient/stores.go types
type MetadataStore struct {
    Name   string          `json:"name"`
    Type   string          `json:"type"`
    Config json.RawMessage `json:"config,omitempty"`
}

type PayloadStore struct {
    Name   string          `json:"name"`
    Type   string          `json:"type"`
    Config json.RawMessage `json:"config,omitempty"`
}
```

### Pattern 2: Functional Options for Store Creation
**What:** Type-safe options for different store configurations
**When to use:** Creating metadata or payload stores with type-specific settings
**Example:**
```go
// Source: Phase 2 pattern from helpers/cli.go UserOption

// MetadataStoreOption configures metadata store creation
type MetadataStoreOption func(*metadataStoreOptions)

type metadataStoreOptions struct {
    // BadgerDB specific
    dbPath string
    // PostgreSQL specific
    pgHost     string
    pgPort     int
    pgDatabase string
    pgUser     string
    pgPassword string
    pgSSLMode  string
    // Generic JSON config
    rawConfig string
}

// WithDBPath sets the database path for BadgerDB stores
func WithDBPath(path string) MetadataStoreOption {
    return func(o *metadataStoreOptions) {
        o.dbPath = path
    }
}

// WithPostgresConfig sets PostgreSQL connection details
func WithPostgresConfig(host string, port int, database, user, password string) MetadataStoreOption {
    return func(o *metadataStoreOptions) {
        o.pgHost = host
        o.pgPort = port
        o.pgDatabase = database
        o.pgUser = user
        o.pgPassword = password
        o.pgSSLMode = "disable"
    }
}

// PayloadStoreOption configures payload store creation
type PayloadStoreOption func(*payloadStoreOptions)

type payloadStoreOptions struct {
    // Filesystem specific
    path string
    // S3 specific
    bucket    string
    region    string
    endpoint  string
    accessKey string
    secretKey string
    // Generic JSON config
    rawConfig string
}

// WithStoragePath sets the path for filesystem stores
func WithStoragePath(path string) PayloadStoreOption {
    return func(o *payloadStoreOptions) {
        o.path = path
    }
}

// WithS3Config sets S3 connection details
func WithS3Config(bucket, region, endpoint, accessKey, secretKey string) PayloadStoreOption {
    return func(o *payloadStoreOptions) {
        o.bucket = bucket
        o.region = region
        o.endpoint = endpoint
        o.accessKey = accessKey
        o.secretKey = secretKey
    }
}
```

### Pattern 3: Store CRUD Methods on CLIRunner
**What:** Methods that execute dittofsctl store commands and parse JSON responses
**When to use:** All store management tests
**Example:**
```go
// Source: Phase 2 pattern with existing CLI commands

// CreateMetadataStore creates a metadata store via CLI
func (r *CLIRunner) CreateMetadataStore(name, storeType string, opts ...MetadataStoreOption) (*MetadataStore, error) {
    o := &metadataStoreOptions{}
    for _, opt := range opts {
        opt(o)
    }

    args := []string{"store", "metadata", "add", "--name", name, "--type", storeType}

    // Add type-specific options
    switch storeType {
    case "badger":
        if o.dbPath != "" {
            args = append(args, "--db-path", o.dbPath)
        }
    case "postgres":
        // Use JSON config for postgres
        if o.pgHost != "" {
            config := fmt.Sprintf(`{"host":"%s","port":%d,"dbname":"%s","user":"%s","password":"%s","sslmode":"%s"}`,
                o.pgHost, o.pgPort, o.pgDatabase, o.pgUser, o.pgPassword, o.pgSSLMode)
            args = append(args, "--config", config)
        }
    }

    if o.rawConfig != "" {
        args = append(args, "--config", o.rawConfig)
    }

    output, err := r.Run(args...)
    if err != nil {
        return nil, fmt.Errorf("create metadata store failed: %w\noutput: %s", err, string(output))
    }

    var store MetadataStore
    if err := ParseJSONResponse(output, &store); err != nil {
        return nil, err
    }

    return &store, nil
}

// DeleteMetadataStore deletes a metadata store via CLI
func (r *CLIRunner) DeleteMetadataStore(name string) error {
    output, err := r.Run("store", "metadata", "remove", name, "--force")
    if err != nil {
        return fmt.Errorf("delete metadata store failed: %w\noutput: %s", err, string(output))
    }
    return nil
}
```

### Pattern 4: Testing Store In Use Constraint
**What:** Create share referencing store, verify deletion fails with clear error
**When to use:** MDS-07 and PLS-07 requirements
**Example:**
```go
// Source: Existing models.ErrStoreInUse error handling

func TestMetadataStoreInUse(t *testing.T) {
    t.Parallel()

    storeName := helpers.UniqueTestName("meta_inuse")
    shareName := "/" + helpers.UniqueTestName("share_inuse")

    // Create metadata store
    metaStore, err := cli.CreateMetadataStore(storeName, "memory")
    require.NoError(t, err)

    // Create payload store (needed for share)
    payloadStoreName := helpers.UniqueTestName("payload_inuse")
    _, err = cli.CreatePayloadStore(payloadStoreName, "memory")
    require.NoError(t, err)
    t.Cleanup(func() { _ = cli.DeletePayloadStore(payloadStoreName) })

    // Create share referencing the metadata store
    _, err = cli.CreateShare(shareName, storeName, payloadStoreName)
    require.NoError(t, err)
    t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

    // Try to delete metadata store - should fail
    err = cli.DeleteMetadataStore(storeName)
    require.Error(t, err, "Should reject deletion of store in use")

    // Error should indicate store is in use
    errStr := strings.ToLower(err.Error())
    assert.True(t,
        strings.Contains(errStr, "in use") ||
            strings.Contains(errStr, "referenced") ||
            strings.Contains(errStr, "shares"),
        "Error should indicate store is in use: %s", err.Error())

    // After deleting share, store deletion should succeed
    err = cli.DeleteShare(shareName)
    require.NoError(t, err)

    err = cli.DeleteMetadataStore(storeName)
    require.NoError(t, err, "Should delete store after share removed")
}
```

### Anti-Patterns to Avoid
- **Hardcoded store names:** Use `UniqueTestName("prefix")` for test isolation
- **Not cleaning up stores:** Register cleanup via `t.Cleanup()` even for expected failures
- **Testing interactive prompts:** Always use CLI flags, not interactive mode
- **Assuming container readiness:** Wait for Postgres/S3 endpoints before running tests
- **Testing with real AWS:** Use Localstack for S3 tests (Testcontainers)

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Unique store names | Manual counters | `UniqueTestName()` from helpers | Thread-safe, no collisions |
| JSON config building | String concatenation | `map[string]any` + `json.Marshal` | Type-safe, escaping handled |
| Postgres for tests | Manual Docker commands | Testcontainers | Automatic cleanup, port allocation |
| S3 for tests | Real AWS | Localstack via Testcontainers | Free, isolated, no credentials needed |
| Store type validation | Custom validation | CLI already validates types | Server rejects invalid types |

**Key insight:** The CLI already handles input validation, type checking, and interactive prompts. Tests should use flags to bypass prompts and let the CLI/server validate inputs.

## Common Pitfalls

### Pitfall 1: Store Names with Special Characters
**What goes wrong:** Store names with slashes or spaces break CLI parsing
**Why it happens:** Store names are used in URLs and file paths
**How to avoid:** Use alphanumeric names with underscores only: `UniqueTestName("store_type")`
**Warning signs:** "invalid argument" errors or unexpected URL encoding

### Pitfall 2: Attempting to Delete Store Before Share
**What goes wrong:** Store deletion fails with "store in use" error
**Why it happens:** Share references store by ID, foreign key constraint
**How to avoid:** Delete share first, then store. Use proper cleanup order in `t.Cleanup()`
**Warning signs:** `ErrStoreInUse` errors in cleanup

### Pitfall 3: BadgerDB Path Conflicts
**What goes wrong:** Multiple tests trying to use same BadgerDB path
**Why it happens:** BadgerDB locks its directory exclusively
**How to avoid:** Use unique temp directories: `filepath.Join(t.TempDir(), "badger")`
**Warning signs:** "LOCK" errors, "directory already in use"

### Pitfall 4: Postgres Not Ready
**What goes wrong:** Tests fail connecting to Postgres container
**Why it happens:** Container starts but Postgres takes time to initialize
**How to avoid:** Use Testcontainers wait strategies, poll for readiness
**Warning signs:** "connection refused", "database does not exist"

### Pitfall 5: S3 Endpoint Configuration
**What goes wrong:** S3 store creation works but operations fail
**Why it happens:** Localstack endpoint needs path-style URLs, special config
**How to avoid:** Use `--endpoint` flag with Localstack URL, ensure bucket exists
**Warning signs:** "bucket not found", "signature mismatch"

### Pitfall 6: Edit Without Changes
**What goes wrong:** Edit command returns "no update fields specified"
**Why it happens:** CLI requires at least one field to change
**How to avoid:** Always provide at least one changed field for edit tests
**Warning signs:** CLI exit code 1 with "no update" message

### Pitfall 7: Memory Store Persistence Expectations
**What goes wrong:** Tests expect data persistence in memory stores
**Why it happens:** Memory stores are ephemeral by design
**How to avoid:** Only test CRUD operations, not persistence
**Warning signs:** "store not found" after server restart

## Code Examples

Verified patterns from existing codebase:

### Metadata Store Test Structure
```go
//go:build e2e

package e2e

import (
    "path/filepath"
    "strings"
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestMetadataStoresCRUD(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping metadata stores tests in short mode")
    }

    // Start server with automatic cleanup
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    t.Run("create memory store", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("meta_mem")
        t.Cleanup(func() { _ = cli.DeleteMetadataStore(storeName) })

        store, err := cli.CreateMetadataStore(storeName, "memory")
        require.NoError(t, err, "Should create memory metadata store")

        assert.Equal(t, storeName, store.Name)
        assert.Equal(t, "memory", store.Type)
    })

    t.Run("create badger store", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("meta_bdg")
        dbPath := filepath.Join(t.TempDir(), "badger")
        t.Cleanup(func() { _ = cli.DeleteMetadataStore(storeName) })

        store, err := cli.CreateMetadataStore(storeName, "badger",
            helpers.WithDBPath(dbPath))
        require.NoError(t, err, "Should create BadgerDB metadata store")

        assert.Equal(t, storeName, store.Name)
        assert.Equal(t, "badger", store.Type)
    })

    t.Run("list stores", func(t *testing.T) {
        t.Parallel()

        storeName1 := helpers.UniqueTestName("meta_list1")
        storeName2 := helpers.UniqueTestName("meta_list2")
        t.Cleanup(func() {
            _ = cli.DeleteMetadataStore(storeName1)
            _ = cli.DeleteMetadataStore(storeName2)
        })

        _, err := cli.CreateMetadataStore(storeName1, "memory")
        require.NoError(t, err)
        _, err = cli.CreateMetadataStore(storeName2, "memory")
        require.NoError(t, err)

        stores, err := cli.ListMetadataStores()
        require.NoError(t, err)

        var found1, found2 bool
        for _, s := range stores {
            if s.Name == storeName1 {
                found1 = true
            }
            if s.Name == storeName2 {
                found2 = true
            }
        }

        assert.True(t, found1, "Should find store1")
        assert.True(t, found2, "Should find store2")
    })

    t.Run("duplicate name rejected", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("meta_dup")
        t.Cleanup(func() { _ = cli.DeleteMetadataStore(storeName) })

        _, err := cli.CreateMetadataStore(storeName, "memory")
        require.NoError(t, err)

        _, err = cli.CreateMetadataStore(storeName, "memory")
        require.Error(t, err)
        assert.True(t,
            strings.Contains(strings.ToLower(err.Error()), "already exists") ||
                strings.Contains(strings.ToLower(err.Error()), "duplicate"),
            "Error should indicate duplicate")
    })

    t.Run("cannot delete store in use", func(t *testing.T) {
        // Non-parallel - creates share that references store

        metaName := helpers.UniqueTestName("meta_inuse")
        payloadName := helpers.UniqueTestName("payload_inuse")
        shareName := "/" + helpers.UniqueTestName("share")

        // Create stores
        _, err := cli.CreateMetadataStore(metaName, "memory")
        require.NoError(t, err)

        _, err = cli.CreatePayloadStore(payloadName, "memory")
        require.NoError(t, err)

        // Create share using stores
        _, err = cli.CreateShare(shareName, metaName, payloadName)
        require.NoError(t, err)

        // Try to delete metadata store - should fail
        err = cli.DeleteMetadataStore(metaName)
        require.Error(t, err, "Should reject deletion of store in use")
        assert.True(t,
            strings.Contains(strings.ToLower(err.Error()), "in use") ||
                strings.Contains(strings.ToLower(err.Error()), "referenced"),
            "Error should indicate store is in use")

        // Cleanup in reverse order
        _ = cli.DeleteShare(shareName)
        _ = cli.DeletePayloadStore(payloadName)
        _ = cli.DeleteMetadataStore(metaName)
    })
}
```

### Payload Store Test Structure
```go
//go:build e2e

package e2e

import (
    "path/filepath"
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestPayloadStoresCRUD(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping payload stores tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    t.Run("create memory store", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("payload_mem")
        t.Cleanup(func() { _ = cli.DeletePayloadStore(storeName) })

        store, err := cli.CreatePayloadStore(storeName, "memory")
        require.NoError(t, err)

        assert.Equal(t, storeName, store.Name)
        assert.Equal(t, "memory", store.Type)
    })

    t.Run("create filesystem store", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("payload_fs")
        storagePath := filepath.Join(t.TempDir(), "content")
        t.Cleanup(func() { _ = cli.DeletePayloadStore(storeName) })

        store, err := cli.CreatePayloadStore(storeName, "filesystem",
            helpers.WithStoragePath(storagePath))
        require.NoError(t, err)

        assert.Equal(t, storeName, store.Name)
        assert.Equal(t, "filesystem", store.Type)
    })

    t.Run("edit store config", func(t *testing.T) {
        t.Parallel()

        storeName := helpers.UniqueTestName("payload_edit")
        initialPath := filepath.Join(t.TempDir(), "initial")
        newPath := filepath.Join(t.TempDir(), "updated")
        t.Cleanup(func() { _ = cli.DeletePayloadStore(storeName) })

        _, err := cli.CreatePayloadStore(storeName, "filesystem",
            helpers.WithStoragePath(initialPath))
        require.NoError(t, err)

        // Edit to change path
        updated, err := cli.EditPayloadStore(storeName,
            helpers.WithStoragePath(newPath))
        require.NoError(t, err)
        assert.Equal(t, storeName, updated.Name)
    })
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Direct API testing | CLI subprocess execution | Phase 2 decision | Tests actual user experience |
| Manual container setup | Testcontainers | Phase 1 decision | Self-contained, automatic cleanup |
| Mixed sync/async | Functional options pattern | Phase 2 implementation | Consistent, type-safe API |

**Deprecated/outdated:**
- None for this phase - building on Phase 1-2 patterns

## Open Questions

Things that couldn't be fully resolved:

1. **PostgreSQL Store Testing Scope**
   - What we know: CLI supports creating postgres metadata stores
   - What's unclear: Should E2E tests verify actual Postgres connectivity or just API acceptance?
   - Recommendation: Test creation via CLI; Testcontainers Postgres verifies connectivity if scope includes it

2. **S3 Store Bucket Creation**
   - What we know: CLI creates S3 store config, doesn't create bucket
   - What's unclear: Should tests create bucket in Localstack before creating store?
   - Recommendation: Create bucket via Testcontainers setup, test store creation against existing bucket

3. **Edit Memory Store Behavior**
   - What we know: Memory stores have no configurable settings per CLI code
   - What's unclear: What happens if you try to edit a memory store?
   - Recommendation: Test and document behavior (likely returns success with no changes)

## Sources

### Primary (HIGH confidence)
- Existing codebase: `cmd/dittofsctl/commands/store/metadata/*.go` - CLI command patterns
- Existing codebase: `cmd/dittofsctl/commands/store/payload/*.go` - CLI command patterns
- Existing codebase: `pkg/apiclient/stores.go` - API client and types
- Existing codebase: `pkg/controlplane/store/metadata_stores.go` - Store CRUD with constraints
- Existing codebase: `pkg/controlplane/store/payload_stores.go` - Store CRUD with constraints
- Existing codebase: `pkg/controlplane/models/errors.go` - ErrStoreInUse, ErrDuplicateStore
- Existing codebase: `test/e2e/helpers/cli.go` - CLIRunner patterns from Phase 2
- Existing codebase: `test/e2e/users_test.go` - Test structure patterns

### Secondary (MEDIUM confidence)
- Phase 2 research: `.planning/phases/02-server-identity/02-RESEARCH.md` - Test patterns
- STATE.md: `.planning/STATE.md` - Project decisions and patterns

### Tertiary (LOW confidence)
- None - all patterns verified from codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use in codebase
- Architecture: HIGH - Patterns derived from existing Phase 2 test infrastructure
- Pitfalls: MEDIUM - Some edge cases (Localstack config) need runtime verification

**Research date:** 2026-02-02
**Valid until:** 60 days (patterns are stable, CLI implementation unlikely to change)
