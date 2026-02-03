# Architecture Patterns for Go E2E Test Suites

**Domain:** CLI-driven system E2E testing for DittoFS
**Researched:** 2026-02-02
**Confidence:** HIGH (based on existing codebase patterns + Go ecosystem best practices)

## Executive Summary

DittoFS already has a well-structured E2E test framework in `test/e2e/`. This research documents the architecture patterns used and identifies components needed for comprehensive dittofsctl-based testing. The existing framework demonstrates Go E2E best practices: build tag isolation, TestMain lifecycle, configuration matrix testing, and protocol mount abstractions.

## Recommended Architecture

The E2E test suite should follow a **layered component architecture** with clear boundaries between infrastructure, abstractions, and test implementations.

```
┌─────────────────────────────────────────────────────────────────┐
│                         TEST LAYER                              │
│  functional_test.go, advanced_test.go, scale_test.go, etc.     │
│  (Test implementations using framework abstractions)            │
└────────────────────────────┬────────────────────────────────────┘
                             │ uses
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                      FRAMEWORK LAYER                            │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌───────────┐ │
│  │ TestContext │ │   Helpers   │ │ CLI Wrapper │ │  Runners  │ │
│  │ (context.go)│ │(helpers.go) │ │  (NEW)      │ │(helpers.go│ │
│  └─────────────┘ └─────────────┘ └─────────────┘ └───────────┘ │
└────────────────────────────┬────────────────────────────────────┘
                             │ orchestrates
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                    INFRASTRUCTURE LAYER                         │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌───────────┐ │
│  │   Mount     │ │ Containers  │ │   Config    │ │  Server   │ │
│  │ (mount.go)  │ │(containers. │ │ (config.go) │ │ Lifecycle │ │
│  │ NFS/SMB     │ │    go)      │ │             │ │           │ │
│  └─────────────┘ └─────────────┘ └─────────────┘ └───────────┘ │
└────────────────────────────┬────────────────────────────────────┘
                             │ manages
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                   EXTERNAL RESOURCES                            │
│  DittoFS Server  │  NFS Mounts  │  SMB Mounts  │  PostgreSQL   │
│  dittofsctl CLI  │  Localstack  │  Temp Dirs   │  S3 Buckets   │
└─────────────────────────────────────────────────────────────────┘
```

### Component Boundaries

| Component | Responsibility | Communicates With |
|-----------|---------------|-------------------|
| **TestContext** | Unified test environment: server, stores, mounts, cleanup | Config, Mount, Server, Tests |
| **CLI Wrapper** | Execute dittofsctl commands, parse output, provide typed results | dittofsctl binary, Tests |
| **Mount Helpers** | NFS/SMB mount/unmount operations, path resolution | OS mount commands, TestContext |
| **Config Matrix** | Define test configurations (memory, badger, postgres, S3) | Container helpers, TestContext |
| **Container Helpers** | Manage PostgreSQL/Localstack containers via testcontainers | Docker daemon, Config |
| **Test Runners** | RunOnAllConfigs, RunOnLocalConfigs orchestration | TestContext, Tests |
| **Helpers** | File operations, checksums, assertions | Standard library, Tests |

### Data Flow

```
1. TestMain initializes (signal handlers, stale mount cleanup)
                     │
2. Test function calls RunOnAllConfigs()
                     │
3. For each config, create TestContext:
   │
   ├─► Setup dependencies (PostgreSQL/S3 containers if needed)
   ├─► Create metadata store
   ├─► Start DittoFS server (Runtime with adapters)
   ├─► Mount NFS and/or SMB filesystems
   └─► Return TestContext
                     │
4. Test executes using:
   ├─► tc.Path() / tc.NFSPath() / tc.SMBPath() for file paths
   ├─► framework.WriteFile(), ReadFile(), etc. for file ops
   ├─► CLI wrapper (NEW) for dittofsctl commands
   └─► Standard Go assertions
                     │
5. defer tc.Cleanup():
   ├─► Unmount filesystems
   ├─► Stop adapters
   ├─► Close stores
   └─► Remove temp directories
```

## Patterns to Follow

### Pattern 1: Build Tag Isolation

**What:** Use `//go:build e2e` tags to exclude E2E tests from regular `go test ./...`
**When:** Always for E2E tests that require sudo, external resources, or long execution
**Why:** Prevents accidental execution, enables selective CI pipeline stages

```go
//go:build e2e

package e2e

import "testing"

func TestSomething(t *testing.T) {
    // Only runs with: go test -tags=e2e ./test/e2e/...
}
```

**DittoFS Status:** Already implemented correctly.

### Pattern 2: TestMain Lifecycle Management

**What:** Use TestMain for global setup/teardown (signal handlers, container cleanup, stale mount cleanup)
**When:** Test suite requires shared resources or cleanup across all tests
**Why:** Ensures consistent environment and resource cleanup on interrupts

```go
func TestMain(m *testing.M) {
    // Setup signal handler for CTRL+C
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-sigChan
        framework.CleanupAllContexts()
        framework.CleanupSharedContainers()
        os.Exit(1)
    }()

    // Cleanup stale mounts from failed runs
    framework.CleanupStaleMounts()

    code := m.Run()

    // Final cleanup
    framework.CleanupAllContexts()
    framework.CleanupSharedContainers()
    os.Exit(code)
}
```

**DittoFS Status:** Already implemented in `main_test.go`.

### Pattern 3: Configuration Matrix Testing

**What:** Run same test across multiple backend configurations
**When:** System has pluggable backends (memory, BadgerDB, PostgreSQL, S3)
**Why:** Ensures consistent behavior across all supported configurations

```go
func TestCRUD(t *testing.T) {
    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        // Test runs once per config: memory, badger, postgres
        filePath := tc.Path("test.txt")
        framework.WriteFile(t, filePath, []byte("content"))
        // ...
    })
}
```

**DittoFS Status:** Already implemented. Consider adding `RunOnCLIConfigs` for dittofsctl-specific tests.

### Pattern 4: Protocol Abstraction (Mount Helpers)

**What:** Abstract protocol-specific mount operations behind common interface
**When:** Testing multiple protocols (NFS, SMB) that expose same filesystem
**Why:** Tests remain protocol-agnostic while testing protocol-specific behavior when needed

```go
type Mount struct {
    Path     string
    Protocol string // "nfs" or "smb"
    Port     int
}

// Protocol-agnostic
filePath := tc.Path("file.txt")  // Uses default (NFS)

// Protocol-specific
nfsPath := tc.NFSPath("file.txt")
smbPath := tc.SMBPath("file.txt")
```

**DittoFS Status:** Already implemented in `mount.go` and `context.go`.

### Pattern 5: CLI Wrapper Pattern (NEW - To Implement)

**What:** Wrap CLI binary execution with typed Go functions
**When:** Testing CLI-driven systems where CLI is primary interface
**Why:** Type-safe CLI testing, structured error handling, output parsing

```go
// cli_wrapper.go (NEW)
type CLIWrapper struct {
    BinaryPath string
    ServerURL  string
    Token      string // For authenticated commands
}

type UserListResult struct {
    Users []User
    Err   error
}

func (c *CLIWrapper) UserList(ctx context.Context) UserListResult {
    args := []string{"user", "list", "-o", "json", "--server", c.ServerURL}
    output, err := c.exec(ctx, args...)
    if err != nil {
        return UserListResult{Err: err}
    }
    var users []User
    if err := json.Unmarshal(output, &users); err != nil {
        return UserListResult{Err: err}
    }
    return UserListResult{Users: users}
}

// Usage in tests
func TestUserManagement(t *testing.T) {
    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        cli := tc.CLI() // Get pre-configured CLI wrapper

        result := cli.UserCreate(ctx, "alice", "password123")
        if result.Err != nil {
            t.Fatalf("Failed to create user: %v", result.Err)
        }

        list := cli.UserList(ctx)
        if len(list.Users) != 1 {
            t.Errorf("Expected 1 user, got %d", len(list.Users))
        }
    })
}
```

**DittoFS Status:** NOT YET IMPLEMENTED. This is a key new component needed.

### Pattern 6: Table-Driven Sub-tests

**What:** Use table-driven tests with t.Run for multiple scenarios
**When:** Testing variations of same operation (different file sizes, permission levels, etc.)
**Why:** DRY test code, clear failure isolation, parallel execution potential

```go
func TestLargeFiles(t *testing.T) {
    sizes := []struct {
        name string
        size int64
    }{
        {"1MB", 1 * 1024 * 1024},
        {"10MB", 10 * 1024 * 1024},
        {"100MB", 100 * 1024 * 1024},
    }

    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        for _, sz := range sizes {
            t.Run(sz.name, func(t *testing.T) {
                if testing.Short() && sz.size > 10*1024*1024 {
                    t.Skip("Skipping large file test in short mode")
                }
                checksum := framework.WriteRandomFile(t, tc.Path("large.bin"), sz.size)
                framework.VerifyFileChecksum(t, tc.Path("large.bin"), checksum)
            })
        }
    })
}
```

**DittoFS Status:** Already implemented in `scale_test.go`.

## Anti-Patterns to Avoid

### Anti-Pattern 1: Global State Between Tests

**What:** Sharing mutable state (files, database records) between test functions
**Why bad:** Creates flaky tests, order-dependent failures, difficult debugging
**Instead:** Each test creates its own TestContext with fresh server/mounts

```go
// BAD: Shared state
var globalPath string

func TestCreate(t *testing.T) {
    globalPath = tc.Path("shared.txt")
    framework.WriteFile(t, globalPath, []byte("data"))
}

func TestRead(t *testing.T) {
    content := framework.ReadFile(t, globalPath) // Depends on TestCreate running first!
}

// GOOD: Isolated state per test
func TestCRUD(t *testing.T) {
    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        t.Run("Create", func(t *testing.T) { /* fresh tc */ })
        t.Run("Read", func(t *testing.T) { /* fresh tc */ })
    })
}
```

### Anti-Pattern 2: Sleeping Instead of Waiting

**What:** Using `time.Sleep` to wait for async operations
**Why bad:** Flaky (timing-dependent), slow (over-waiting), unreliable in CI
**Instead:** Use polling with timeout (WaitForServer pattern)

```go
// BAD: Fixed sleep
time.Sleep(5 * time.Second)
// Then assume server is ready...

// GOOD: Polling with timeout
func WaitForServer(t *testing.T, port int, timeout time.Duration) {
    deadline := time.After(timeout)
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-deadline:
            t.Fatalf("Timeout waiting for server on port %d", port)
        case <-ticker.C:
            conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
            if err == nil {
                conn.Close()
                return
            }
        }
    }
}
```

**DittoFS Status:** Correctly implemented in `mount.go`.

### Anti-Pattern 3: Testing Implementation Details

**What:** Tests that rely on internal file paths, internal API responses, internal data structures
**Why bad:** Breaks when implementation changes, doesn't validate user-facing behavior
**Instead:** Test through public interfaces (CLI, mounted filesystem, API endpoints)

```go
// BAD: Testing internal cache state
if tc.Runtime.cache.pendingBlocks > 0 { ... }

// GOOD: Test observable behavior
framework.WriteFile(t, tc.Path("file.txt"), data)
// Wait for commit
tc.CLI().Sync() // or explicit COMMIT operation
// Verify data persisted
readData := framework.ReadFile(t, tc.Path("file.txt"))
if !bytes.Equal(data, readData) { ... }
```

### Anti-Pattern 4: Hardcoded Ports

**What:** Using fixed ports for test servers
**Why bad:** Port conflicts in CI, parallel test failures, flaky tests
**Instead:** Use dynamic port allocation

```go
// BAD: Fixed port
server := StartServer(12049)

// GOOD: Dynamic port
port := framework.FindFreePort(t)
server := StartServer(port)
```

**DittoFS Status:** Correctly implemented using `FindFreePort`.

## Suggested Build Order (Phase Structure)

Based on component dependencies, implement in this order:

### Phase 1: CLI Wrapper Foundation
**Components:** CLI wrapper base, binary path resolution, execution helpers
**Dependencies:** None (uses existing binaries)
**Build:**
1. `cli_wrapper.go` - Base wrapper with exec, output capture
2. Context integration - `tc.CLI()` accessor
3. Basic commands - `version`, `status`

### Phase 2: Authentication Flow
**Components:** Login/logout, token management, context persistence
**Dependencies:** Phase 1
**Build:**
1. `login.go` tests - Login command, token storage
2. `logout.go` tests - Token cleanup
3. Context persistence tests

### Phase 3: User/Group Management
**Components:** User CRUD, Group CRUD, membership operations
**Dependencies:** Phase 2 (requires authentication)
**Build:**
1. `user_test.go` - Create, list, get, update, delete
2. `group_test.go` - Create, list, add/remove members
3. Permission inheritance tests

### Phase 4: Share Management
**Components:** Share CRUD, permissions, metadata/payload store assignment
**Dependencies:** Phase 3 (shares have permissions)
**Build:**
1. `share_test.go` - Create, list, get, delete
2. `permission_test.go` - Grant, revoke, list
3. Store assignment tests

### Phase 5: Store Management
**Components:** Metadata store CRUD, payload store CRUD
**Dependencies:** Phase 1
**Build:**
1. `metadata_store_test.go` - Add, list, remove stores
2. `payload_store_test.go` - Add, list, remove stores
3. Store configuration validation

### Phase 6: Adapter Management
**Components:** Adapter lifecycle via CLI
**Dependencies:** Phase 1
**Build:**
1. `adapter_test.go` - List, status
2. Adapter configuration tests

### Phase 7: Integration Scenarios
**Components:** End-to-end workflows combining multiple operations
**Dependencies:** Phases 1-6
**Build:**
1. Full user workflow (create user -> create group -> add to group -> create share -> grant permission -> mount -> file ops)
2. Multi-protocol tests (CLI + NFS + SMB interop)
3. Failover/recovery scenarios

## Existing Framework Components (Reference)

DittoFS already has these well-implemented components:

| File | Component | Status |
|------|-----------|--------|
| `framework/context.go` | TestContext | Complete |
| `framework/config.go` | Configuration matrix | Complete |
| `framework/mount.go` | NFS/SMB mount helpers | Complete |
| `framework/containers.go` | PostgreSQL/Localstack | Complete |
| `framework/helpers.go` | File ops, assertions, runners | Complete |
| `main_test.go` | TestMain lifecycle | Complete |

## New Components Needed

| File | Component | Priority |
|------|-----------|----------|
| `framework/cli.go` | CLI wrapper base | HIGH |
| `framework/cli_auth.go` | Authentication helpers | HIGH |
| `framework/cli_user.go` | User command wrappers | MEDIUM |
| `framework/cli_group.go` | Group command wrappers | MEDIUM |
| `framework/cli_share.go` | Share command wrappers | MEDIUM |
| `framework/cli_store.go` | Store command wrappers | MEDIUM |
| `framework/cli_adapter.go` | Adapter command wrappers | LOW |

## Sources

- [efficientgo/e2e Framework](https://github.com/efficientgo/e2e) - Go E2E testing patterns
- [Kubernetes E2E Package](https://pkg.go.dev/k8s.io/kubernetes/test/e2e) - Large-scale E2E architecture
- [Testcontainers for Go](https://golang.testcontainers.org/) - Container management patterns
- [Go Testing Package](https://pkg.go.dev/testing) - TestMain, t.Run patterns
- [Cobra Testing Patterns](https://cobra.dev/docs/explanations/enterprise-guide/) - CLI testing best practices
- [Writing Integration Tests for a Go CLI Application](https://lucapette.me/writing/writing-integration-tests-for-a-go-cli-application/) - CLI testing patterns
- [Go Blog: Integration Test Coverage](https://go.dev/blog/integration-test-coverage) - Go 1.20+ coverage for binaries
- Existing DittoFS `test/e2e/` framework - Production-proven patterns
