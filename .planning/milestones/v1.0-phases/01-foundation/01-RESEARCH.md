# Phase 1: Foundation - Research

**Researched:** 2026-02-02
**Domain:** Test framework infrastructure (Testcontainers, Go testing) and CLI mount commands
**Confidence:** HIGH

## Summary

This phase establishes the foundation for CLI-driven E2E testing: mount/unmount commands for NFS and SMB protocols, plus the Testcontainers-based infrastructure for Postgres and S3. The research covers testcontainers-go patterns for shared container lifecycle, Go testing patterns for parallel execution with isolation, and platform-specific mount commands for both macOS and Linux.

The existing codebase already has a solid test framework in `test/e2e/framework/` with container helpers, mount functions, and TestContext patterns. This phase builds upon that foundation, adding CLI mount commands to `dittofsctl` and reorganizing tests by domain with improved namespace isolation for parallel execution.

**Primary recommendation:** Use testcontainers-go modules for Postgres and LocalStack (not raw GenericContainer), implement namespace isolation via unique Postgres schemas and S3 prefixes per test, and wrap platform-specific mount commands in the new `dittofsctl share mount` command.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| testcontainers-go | v0.40.0 | Container lifecycle for Postgres/S3 | Already in go.mod, official support for both Postgres and LocalStack modules |
| stretchr/testify | v1.11.1 | Assertions (assert for soft, require for hard) | Already in go.mod, project standard |
| spf13/cobra | v1.8.1 | CLI framework for mount commands | Already in go.mod, project standard |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| testcontainers-go/modules/postgres | v0.40.0 | PostgreSQL container with wait strategies | For Postgres-backed metadata store tests |
| testcontainers-go/modules/localstack | v0.40.0 | LocalStack container for S3 | For S3-backed payload store tests |
| jackc/pgx/v5 | v5.7.6 | PostgreSQL driver for schema isolation | Already in go.mod, for per-test schema creation |
| aws-sdk-go-v2/service/s3 | v1.90.2 | S3 client for bucket/prefix operations | Already in go.mod, for S3 namespace isolation |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Postgres module | GenericContainer | Module has built-in wait strategies, connection string helpers |
| LocalStack module | GenericContainer | Module handles AWS SDK configuration automatically |
| require.* | assert.* | require fails fast (test stops), assert continues (see all failures) |

**Installation:**
```bash
# Already in go.mod, no additional packages needed
go get github.com/testcontainers/testcontainers-go/modules/postgres@v0.40.0
go get github.com/testcontainers/testcontainers-go/modules/localstack@v0.40.0
```

## Architecture Patterns

### Recommended Project Structure
```
test/e2e/
├── main_test.go           # TestMain: container lifecycle, signal handling
├── helpers/               # Shared test utilities
│   ├── environment.go     # TestEnvironment struct (shared containers)
│   ├── scope.go          # TestScope struct (per-test isolation)
│   ├── must.go           # MustCreateUser, MustMount, etc.
│   └── assertions.go     # Custom assertions if needed
├── users_test.go          # User CRUD tests (Phase 2)
├── groups_test.go         # Group CRUD tests (Phase 2)
├── shares_test.go         # Share CRUD tests (Phase 4)
└── ...

cmd/dittofsctl/commands/share/
├── share.go               # Parent command (existing)
├── mount.go               # NEW: Mount subcommand
└── unmount.go             # NEW: Unmount subcommand
```

### Pattern 1: TestEnvironment in TestMain (Shared State)
**What:** Single struct holding shared containers and connection info
**When to use:** TestMain to start containers once for all tests
**Example:**
```go
// Source: testcontainers-go official docs + project pattern
package e2e

var env *helpers.TestEnvironment

func TestMain(m *testing.M) {
    ctx := context.Background()

    // Fail fast if containers don't start
    var err error
    env, err = helpers.NewTestEnvironment(ctx)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to start containers: %v\n", err)
        os.Exit(1)
    }

    // Signal handler for cleanup on interrupt
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-sigChan
        env.Cleanup(ctx)
        os.Exit(1)
    }()

    code := m.Run()

    env.Cleanup(ctx)
    os.Exit(code)
}
```

### Pattern 2: TestScope for Per-Test Isolation
**What:** Struct providing isolated namespace (Postgres schema, S3 prefix) per test
**When to use:** Every test that needs database or S3 access
**Example:**
```go
// Source: Project CONTEXT.md decisions
func TestUserCreate(t *testing.T) {
    t.Parallel()

    scope := env.NewScope(t) // Creates unique schema + S3 prefix
    defer scope.Cleanup()    // Drops schema, deletes S3 prefix

    // scope.DBConn - connection to test-specific schema
    // scope.S3Prefix - unique prefix like "test-abc123/"
    // scope.MustCreateUser("test-user-alice")
}
```

### Pattern 3: Must* Helpers for Fail-Fast
**What:** Helper functions that fail the test immediately on error
**When to use:** Setup operations where failure means test cannot proceed
**Example:**
```go
// Source: Project CONTEXT.md decisions
func (s *TestScope) MustCreateUser(t *testing.T, username string) *models.User {
    t.Helper()
    user, err := s.client.CreateUser(ctx, &models.CreateUserRequest{
        Username: username,
    })
    require.NoError(t, err, "failed to create user %s", username)
    return user
}

func (s *TestScope) MustMount(t *testing.T, protocol, share, mountPoint string) *Mount {
    t.Helper()
    cmd := exec.Command("dittofsctl", "share", "mount",
        "--protocol", protocol, share, mountPoint)
    output, err := cmd.CombinedOutput()
    require.NoError(t, err, "mount failed: %s", string(output))
    return &Mount{Path: mountPoint, Protocol: protocol}
}
```

### Pattern 4: Platform-Specific Mount Commands
**What:** Detect OS and use appropriate mount command
**When to use:** In CLI mount command implementation
**Example:**
```go
// Source: Existing test/e2e/framework/mount.go pattern
func mountNFS(share, mountPoint string, port int) error {
    var cmd *exec.Cmd
    switch runtime.GOOS {
    case "darwin":
        // macOS: mount -t nfs -o nfsvers=3,tcp,port=X,mountport=X,resvport
        opts := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,resvport,actimeo=0", port, port)
        cmd = exec.Command("mount", "-t", "nfs", "-o", opts,
            fmt.Sprintf("localhost:%s", share), mountPoint)
    case "linux":
        // Linux: mount -t nfs -o nfsvers=3,tcp,port=X,mountport=X,nolock
        opts := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,nolock,actimeo=0", port, port)
        cmd = exec.Command("mount", "-t", "nfs", "-o", opts,
            fmt.Sprintf("localhost:%s", share), mountPoint)
    default:
        return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
    }
    output, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("mount failed: %w\nOutput: %s", err, string(output))
    }
    return nil
}
```

### Anti-Patterns to Avoid
- **Global mutable state between tests:** Each test must get its own isolated namespace via TestScope
- **Skipping on container failure:** Fail fast in TestMain, don't skip tests if containers don't start
- **Parsing mount output for success:** Check error code, not output text
- **Hardcoded ports:** Use dynamic port allocation from testcontainers
- **Direct database access without schema isolation:** Always use per-test schema

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Container wait strategies | Custom polling loops | testcontainers wait.ForAll() | Handles timeouts, multiple conditions, restart detection |
| Postgres connection strings | String concatenation | postgres.ConnectionString() | Handles escaping, optional parameters |
| S3 endpoint configuration | Manual AWS config | localstack module helpers | Handles path style, credentials, endpoint resolution |
| Port availability checking | Custom TCP dial loops | testcontainers mapped ports | Containers handle port allocation automatically |
| Test cleanup | Manual defer chains | t.Cleanup() registration | Runs even on panic, proper ordering |

**Key insight:** Testcontainers modules encapsulate years of community learning about container startup edge cases, wait strategies, and cleanup patterns. The official modules handle many subtle issues (like Postgres restart detection needing 2 log occurrences) that custom GenericContainer setups miss.

## Common Pitfalls

### Pitfall 1: Postgres Wait Strategy Missing Restart Detection
**What goes wrong:** Tests flake because Postgres logs "ready to accept connections" twice (once during restart)
**Why it happens:** Postgres restarts during initialization; single log match triggers too early
**How to avoid:** Use `wait.ForLog("database system is ready to accept connections").WithOccurrence(2)`
**Warning signs:** Intermittent "connection refused" errors in CI

### Pitfall 2: Parallel Tests Sharing Database State
**What goes wrong:** Tests interfere with each other, causing flaky failures
**Why it happens:** t.Parallel() runs tests concurrently but they share the same database
**How to avoid:** Create unique Postgres schema per test: `test_${testName}_${uuid}`
**Warning signs:** Tests pass in isolation but fail when run together

### Pitfall 3: S3 Object Collisions in Parallel Tests
**What goes wrong:** Tests overwrite each other's S3 objects
**Why it happens:** Multiple parallel tests using same bucket without prefixes
**How to avoid:** Use unique S3 prefix per test: `test-${uuid}/`
**Warning signs:** Data corruption or unexpected file contents in test assertions

### Pitfall 4: Mount Commands Requiring sudo Without Proper Setup
**What goes wrong:** Mount commands fail with permission errors
**Why it happens:** NFS/SMB mounts typically require root privileges
**How to avoid:** Document sudo requirement, handle in CI with proper permissions, provide helpful error message
**Warning signs:** "Operation not permitted" errors, works locally but fails in CI

### Pitfall 5: Stale Mounts After Test Failures
**What goes wrong:** Subsequent test runs fail because mount points are still in use
**Why it happens:** Test panics or is interrupted before cleanup
**How to avoid:** Implement CleanupStaleMounts() in TestMain, use predictable mount path patterns
**Warning signs:** "mount point busy" errors, orphaned mount directories

### Pitfall 6: Container Reuse Race Conditions
**What goes wrong:** Parallel test packages try to create the same reusable container
**Why it happens:** testcontainers WithReuseByName has race conditions
**How to avoid:** Don't use WithReuseByName; use single TestMain with shared containers
**Warning signs:** "container already exists" errors in parallel test runs

## Code Examples

Verified patterns from official sources and existing codebase:

### Postgres Container with Module
```go
// Source: https://golang.testcontainers.org/modules/postgres/
import "github.com/testcontainers/testcontainers-go/modules/postgres"

func startPostgres(ctx context.Context) (*postgres.PostgresContainer, error) {
    return postgres.Run(ctx, "postgres:16-alpine",
        postgres.WithDatabase("dittofs_e2e"),
        postgres.WithUsername("dittofs_e2e"),
        postgres.WithPassword("dittofs_e2e"),
        testcontainers.WithWaitStrategy(
            wait.ForLog("database system is ready to accept connections").
                WithOccurrence(2).
                WithStartupTimeout(60*time.Second),
        ),
    )
}

// Get connection string
connStr, err := container.ConnectionString(ctx, "sslmode=disable")
```

### LocalStack Container with Module
```go
// Source: https://golang.testcontainers.org/modules/localstack/
import "github.com/testcontainers/testcontainers-go/modules/localstack"

func startLocalstack(ctx context.Context) (*localstack.LocalStackContainer, error) {
    return localstack.Run(ctx, "localstack/localstack:3.0",
        testcontainers.WithEnv(map[string]string{
            "SERVICES":              "s3",
            "DEFAULT_REGION":        "us-east-1",
            "EAGER_SERVICE_LOADING": "1",
        }),
        testcontainers.WithWaitStrategy(
            wait.ForAll(
                wait.ForListeningPort("4566/tcp"),
                wait.ForHTTP("/_localstack/health").
                    WithPort("4566/tcp").
                    WithStartupTimeout(60*time.Second),
            ),
        ),
    )
}
```

### Namespace Isolation - Postgres Schema
```go
// Source: Project CONTEXT.md decisions
func (e *TestEnvironment) NewScope(t *testing.T) *TestScope {
    t.Helper()

    // Generate unique schema name
    schemaName := fmt.Sprintf("test_%s_%s",
        sanitizeName(t.Name()),
        uuid.New().String()[:8])

    // Create schema
    _, err := e.db.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schemaName))
    require.NoError(t, err)

    // Create scoped connection
    scopedConnStr := fmt.Sprintf("%s&search_path=%s", e.connStr, schemaName)
    scopedDB, err := pgxpool.New(ctx, scopedConnStr)
    require.NoError(t, err)

    scope := &TestScope{
        t:          t,
        schemaName: schemaName,
        db:         scopedDB,
        s3Prefix:   fmt.Sprintf("test-%s/", uuid.New().String()[:8]),
        env:        e,
    }

    t.Cleanup(func() { scope.Cleanup() })
    return scope
}

func (s *TestScope) Cleanup() {
    s.db.Close()
    _, _ = s.env.db.Exec(context.Background(),
        fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", s.schemaName))
    // Clean S3 prefix
    s.env.cleanS3Prefix(s.s3Prefix)
}
```

### CLI Mount Command Structure
```go
// Source: Cobra best practices + existing dittofsctl pattern
// cmd/dittofsctl/commands/share/mount.go

var mountCmd = &cobra.Command{
    Use:   "mount [share] [mountpoint]",
    Short: "Mount a share via NFS or SMB",
    Long: `Mount a DittoFS share to a local directory.

The protocol must be specified via --protocol flag (nfs or smb).
Mount point must be an existing empty directory.

Examples:
  # Mount via NFS
  dittofsctl share mount --protocol nfs /myshare /mnt/myshare

  # Mount via SMB with credentials
  dittofsctl share mount --protocol smb --username alice /myshare /mnt/myshare`,
    Args: cobra.ExactArgs(2),
    RunE: runMount,
}

var (
    mountProtocol string
    mountUsername string
    mountPassword string
)

func init() {
    mountCmd.Flags().StringVarP(&mountProtocol, "protocol", "p", "",
        "Protocol to use (nfs or smb) [required]")
    mountCmd.MarkFlagRequired("protocol")

    mountCmd.Flags().StringVarP(&mountUsername, "username", "u", "",
        "Username for SMB authentication")
    mountCmd.Flags().StringVarP(&mountPassword, "password", "P", "",
        "Password for SMB authentication (prompted if not provided)")
}

func runMount(cmd *cobra.Command, args []string) error {
    share := args[0]
    mountPoint := args[1]

    // Validate mount point exists and is empty
    if err := validateMountPoint(mountPoint); err != nil {
        return fmt.Errorf("invalid mount point: %w\nHint: Create the directory first with 'mkdir %s'",
            err, mountPoint)
    }

    var err error
    switch mountProtocol {
    case "nfs":
        err = mountNFS(share, mountPoint)
    case "smb":
        err = mountSMB(share, mountPoint, mountUsername, mountPassword)
    default:
        return fmt.Errorf("unsupported protocol: %s\nHint: Use 'nfs' or 'smb'", mountProtocol)
    }

    if err != nil {
        return formatMountError(err, mountProtocol)
    }

    fmt.Printf("Mounted %s at %s\n", share, mountPoint)
    return nil
}

func formatMountError(err error, protocol string) error {
    // Provide actionable suggestions based on error type
    if strings.Contains(err.Error(), "connection refused") {
        return fmt.Errorf("%w\nHint: Is the %s adapter running? Check with 'dittofsctl adapter list'",
            err, strings.ToUpper(protocol))
    }
    if strings.Contains(err.Error(), "not found") {
        return fmt.Errorf("%w\nHint: Does the share exist? Check with 'dittofsctl share list'", err)
    }
    return err
}
```

### Build Tags for Test Organization
```go
// Source: Go testing documentation + project pattern
// test/e2e/users_test.go

//go:build e2e

package e2e

import (
    "testing"
    "github.com/stretchr/testify/require"
)

func TestUserCreate(t *testing.T) {
    t.Parallel()

    scope := env.NewScope(t)

    t.Run("creates user successfully", func(t *testing.T) {
        t.Parallel()
        user := scope.MustCreateUser(t, "test-user-alice")
        require.Equal(t, "test-user-alice", user.Username)
    })

    t.Run("rejects duplicate username", func(t *testing.T) {
        t.Parallel()
        scope.MustCreateUser(t, "test-user-bob")
        _, err := scope.CreateUser(t, "test-user-bob")
        require.Error(t, err)
        require.Contains(t, err.Error(), "already exists")
    })
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| GenericContainer for Postgres | postgres.Run() module | testcontainers-go v0.30+ | Built-in wait strategies, connection helpers |
| Manual AWS endpoint config | localstack module | testcontainers-go v0.30+ | Automatic SDK configuration |
| Shared database per test run | Per-test schema isolation | 2024 best practice | Enables t.Parallel() without flakes |
| testcontainers.WithReuse() | Shared containers in TestMain | 2024 (race fix) | Avoids race conditions with reusable containers |

**Deprecated/outdated:**
- `testcontainers.WithReuse()`: Has race conditions with parallel tests; prefer shared TestMain
- Manual `ContainerRequest` for Postgres: Use the postgres module for proper wait strategies
- Checking `m.Run()` return before cleanup: Always cleanup, even on failure (use defer or signal handlers)

## Open Questions

Things that couldn't be fully resolved:

1. **SMB mount on macOS Sequoia (15.x)**
   - What we know: macOS uses `mount_smbfs`, existing tests work on older versions
   - What's unclear: Recent macOS versions may have changed SMB behavior
   - Recommendation: Test on CI with latest macOS runner, add platform-specific notes

2. **Container startup timeout in CI**
   - What we know: LocalStack can take up to 60 seconds to initialize S3
   - What's unclear: Optimal timeout for GitHub Actions runners
   - Recommendation: Use 60-second timeout, add retry logic if needed

3. **Namespace cleanup on test timeout**
   - What we know: t.Cleanup() doesn't run if test times out
   - What's unclear: Best approach for orphaned schemas/prefixes
   - Recommendation: Implement periodic cleanup job or use naming convention with timestamps

## Sources

### Primary (HIGH confidence)
- [Testcontainers-go official docs](https://golang.testcontainers.org/) - Container creation, modules, wait strategies
- [Testcontainers Postgres module](https://golang.testcontainers.org/modules/postgres/) - Run function, options, connection string
- [Testcontainers LocalStack module](https://golang.testcontainers.org/modules/localstack/) - Run function, AWS SDK integration
- [stretchr/testify](https://pkg.go.dev/github.com/stretchr/testify) - assert vs require packages
- Existing codebase: `test/e2e/framework/` - Current patterns, mount implementations

### Secondary (MEDIUM confidence)
- [Cobra CLI testing patterns](https://github.com/spf13/cobra/issues/770) - SetArgs, SetOut patterns for testing
- [NFS mount options](https://www.cyberciti.biz/faq/apple-mac-osx-nfs-mount-command-tutorial/) - macOS mount -t nfs syntax

### Tertiary (LOW confidence)
- WebSearch results on parallel Testcontainers patterns - Community patterns, may vary

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in go.mod with proven usage
- Architecture: HIGH - Based on existing framework patterns and official docs
- Pitfalls: MEDIUM - Based on community patterns and project experience, some edge cases unverified

**Research date:** 2026-02-02
**Valid until:** 60 days (libraries are stable, testcontainers-go has slow release cycle)
