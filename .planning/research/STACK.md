# Technology Stack for CLI-Driven E2E Testing

**Project:** DittoFS E2E Test Suite
**Researched:** 2026-02-02
**Overall Confidence:** HIGH (verified with official docs and existing codebase)

## Executive Summary

The existing DittoFS codebase already has a solid foundation with Go 1.25, testcontainers-go v0.40.0, and testify v1.11.1. The recommended stack builds on these proven choices while adding CLI-specific testing capabilities. The key insight: **keep it simple** - avoid BDD frameworks (Ginkgo) and exotic CLI testing tools; the standard library plus testify plus testcontainers is the battle-tested path.

---

## Recommended Stack

### Core Testing Framework

| Technology | Version | Purpose | Confidence |
|------------|---------|---------|------------|
| `testing` (stdlib) | Go 1.25 | Test runner, benchmarks, build tags | HIGH |
| `stretchr/testify` | v1.11.1 | Assertions, require, mock, suite | HIGH |

**Rationale:** DittoFS already uses testify v1.11.1 successfully. The `assert`/`require` packages provide readable assertions. The `suite` package enables shared setup/teardown for container lifecycle. Adding Ginkgo/Gomega would introduce unnecessary complexity and a different testing style.

**Why NOT Ginkgo/Gomega:**
- Different testing paradigm (BDD) would fragment the codebase
- Additional learning curve for contributors
- Kubernetes uses it, but DittoFS is not Kubernetes-scale complexity
- The `suite` package in testify provides adequate lifecycle management

### Container Management

| Technology | Version | Purpose | Confidence |
|------------|---------|---------|------------|
| `testcontainers-go` | v0.40.0 | Container lifecycle management | HIGH |
| `testcontainers-go/modules/postgres` | v0.40.0 | PostgreSQL containers | HIGH |
| `testcontainers-go/modules/localstack` | v0.40.0 | S3 (LocalStack) containers | HIGH |

**Rationale:** Already in use (verified in `go.mod`). Version v0.40.0 (released November 2024) is the latest stable release with improved `Run()` function patterns and wait strategies.

**Key Features to Use:**
- `postgres.Run()` with `BasicWaitStrategies()` for reliable startup detection
- `localstack.Run()` with health check wait strategy
- Shared container pattern via package-level variables (already implemented in `test/e2e/framework/containers.go`)

**Container Reuse Strategy:**
```go
// Use package-level singleton pattern (already in codebase)
var sharedPostgresHelper *PostgresHelper
var sharedLocalstackHelper *LocalstackHelper

// NOT recommended: WithReuseByName (experimental, API unstable)
```

**Why NOT use `WithReuseByName`:**
- Documented as "experimental and the API is subject to change"
- Name collision risks in parallel CI pipelines
- Package-level singletons provide equivalent reuse with better control

### CLI Testing Approach

| Technology | Version | Purpose | Confidence |
|------------|---------|---------|------------|
| `os/exec` (stdlib) | Go 1.25 | Execute CLI binary | HIGH |
| `bytes.Buffer` (stdlib) | Go 1.25 | Capture stdout/stderr | HIGH |
| `cobra.Command.Execute()` | v1.8.1 | Direct command invocation for unit tests | HIGH |

**Rationale:** For E2E tests, execute the actual CLI binary via `os/exec`. This tests the real user experience. For unit tests, use Cobra's `cmd.SetArgs()` and `cmd.SetOut()` pattern.

**NOT Recommended:**
- `rogpeppe/go-internal/testscript`: Overkill for this project, designed for testing the `go` command itself
- `google/go-cmdtest`: Unmaintained, last commit 2022
- `rendon/testcli`: Sparse maintenance, unnecessary abstraction

**CLI E2E Testing Pattern:**
```go
// E2E: Execute actual binary
func execCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
    cmd := exec.Command("./dittofsctl", args...)
    var outBuf, errBuf bytes.Buffer
    cmd.Stdout = &outBuf
    cmd.Stderr = &errBuf
    err = cmd.Run()
    return outBuf.String(), errBuf.String(), err
}

// Unit: Direct command invocation
func TestUserList(t *testing.T) {
    cmd := commands.NewUserListCmd()
    var buf bytes.Buffer
    cmd.SetOut(&buf)
    cmd.SetArgs([]string{"--output", "json"})
    err := cmd.Execute()
    // ...
}
```

### HTTP Testing (for API client tests)

| Technology | Version | Purpose | Confidence |
|------------|---------|---------|------------|
| `net/http/httptest` (stdlib) | Go 1.25 | Mock HTTP servers | HIGH |

**Rationale:** Already used effectively in `pkg/apiclient/client_test.go`. Lightweight, no external dependencies, perfect for testing the `apiclient` package.

### Assertions and Matchers

| Technology | Version | Purpose | Confidence |
|------------|---------|---------|------------|
| `testify/assert` | v1.11.1 | Non-fatal assertions | HIGH |
| `testify/require` | v1.11.1 | Fatal assertions | HIGH |

**Usage Guidelines:**
- Use `require` for setup validation (fail fast)
- Use `assert` for actual test assertions (see all failures)
- Use `require.Eventually` for async operations with timeout

```go
// Setup: fail fast if container isn't ready
require.NoError(t, err, "container startup failed")

// Test assertions: see all failures
assert.Equal(t, expected, actual, "user list mismatch")
assert.Contains(t, output, "alice", "should list alice")

// Async: wait for server ready
require.Eventually(t, func() bool {
    resp, err := http.Get(serverURL + "/health")
    return err == nil && resp.StatusCode == 200
}, 30*time.Second, 100*time.Millisecond)
```

---

## Supporting Libraries

### Already in Project (No Changes Needed)

| Library | Version | Purpose |
|---------|---------|---------|
| `spf13/cobra` | v1.8.1 | CLI framework |
| `aws-sdk-go-v2` | v1.39.6 | S3 client for LocalStack |
| `jackc/pgx/v5` | v5.7.6 | PostgreSQL driver |

### Recommended Additions

| Library | Version | Purpose | Confidence |
|---------|---------|---------|------------|
| None | - | - | - |

**Rationale:** The existing stack is complete. Adding more libraries increases maintenance burden without proportional benefit.

---

## Platform-Specific Considerations

### macOS vs Linux Mount Commands

| Platform | NFS Mount Command | SMB Mount Command |
|----------|-------------------|-------------------|
| macOS | `mount -t nfs -o tcp,port=N,mountport=N,resvport localhost:/export /mnt` | `mount -t smbfs //user:pass@localhost:N/export /mnt` |
| Linux | `mount -t nfs -o tcp,port=N,mountport=N,nfsvers=3,noacl localhost:/export /mnt` | `mount -t cifs //localhost/export /mnt -o port=N,user=...` |

**Already Handled:** The existing `test/e2e/framework/mount.go` abstracts platform differences. No changes needed.

### Sudo Requirements

Both platforms require `sudo` for mounting:
- Tests use build tag `//go:build e2e` to separate from regular tests
- CI runs with `sudo go test -tags=e2e`
- Local development: `sudo go test -tags=e2e -v ./test/e2e/...`

---

## Test Organization Pattern

### Recommended Structure (Aligns with Existing)

```
test/e2e/
├── framework/                    # Shared test infrastructure
│   ├── context.go               # TestContext (server + mounts)
│   ├── config.go                # Store configurations (9 combos)
│   ├── containers.go            # Postgres/LocalStack helpers
│   ├── mount.go                 # Platform-specific mount helpers
│   ├── cli.go                   # NEW: CLI execution helpers
│   └── helpers.go               # File operation helpers
│
├── cli/                          # NEW: CLI-specific E2E tests
│   ├── user_test.go             # dittofsctl user commands
│   ├── group_test.go            # dittofsctl group commands
│   ├── share_test.go            # dittofsctl share commands
│   ├── context_test.go          # Multi-server context management
│   └── auth_test.go             # Login/logout flows
│
├── functional_test.go           # Existing: CRUD via mounted FS
├── interop_v2_test.go           # Existing: NFS<->SMB interop
├── scale_test.go                # Existing: Large files
└── main_test.go                 # Test lifecycle
```

### Test Lifecycle Pattern

```go
// Use testify suite for shared container lifecycle
type CLISuite struct {
    suite.Suite
    postgres  *framework.PostgresHelper
    localstack *framework.LocalstackHelper
    serverURL string
}

func (s *CLISuite) SetupSuite() {
    // Start containers once for all tests
    s.postgres = framework.NewPostgresHelper(s.T())
    s.localstack = framework.NewLocalstackHelper(s.T())
    // Start DittoFS server
    // ...
}

func (s *CLISuite) TearDownSuite() {
    // Containers cleaned up by CleanupSharedContainers()
}

func (s *CLISuite) TestUserCreate() {
    stdout, _, err := s.execCLI("user", "create", "--username", "alice")
    s.Require().NoError(err)
    s.Assert().Contains(stdout, "User created")
}

func TestCLISuite(t *testing.T) {
    suite.Run(t, new(CLISuite))
}
```

---

## Configuration Matrix

### 9 Store Combinations to Test

| Config Name | Metadata | Payload | Docker Required |
|-------------|----------|---------|-----------------|
| `memory-memory` | Memory | Memory | No |
| `memory-filesystem` | Memory | Filesystem | No |
| `memory-s3` | Memory | S3 | Yes (LocalStack) |
| `badger-memory` | BadgerDB | Memory | No |
| `badger-filesystem` | BadgerDB | Filesystem | No |
| `badger-s3` | BadgerDB | S3 | Yes (LocalStack) |
| `postgres-memory` | PostgreSQL | Memory | Yes (Postgres) |
| `postgres-filesystem` | PostgreSQL | Filesystem | Yes (Postgres) |
| `postgres-s3` | PostgreSQL | S3 | Yes (Both) |

**Existing Implementation:** 8 of 9 configurations already exist in `test/e2e/framework/config.go`. Add the missing combinations as needed.

---

## Versions Summary

| Component | Version | Verified |
|-----------|---------|----------|
| Go | 1.25.0 | go.mod |
| testcontainers-go | v0.40.0 | go.mod, official releases |
| testify | v1.11.1 | go.mod, official releases |
| cobra | v1.8.1 | go.mod |
| aws-sdk-go-v2 | v1.39.6 | go.mod |
| pgx/v5 | v5.7.6 | go.mod |
| PostgreSQL (container) | 16-alpine | containers.go |
| LocalStack (container) | 3.0 | containers.go |

---

## What NOT to Use

| Technology | Reason |
|------------|--------|
| Ginkgo/Gomega | BDD style fragments codebase, overkill for this project |
| rogpeppe/testscript | Designed for go command testing, too complex |
| google/go-cmdtest | Unmaintained since 2022 |
| rendon/testcli | Sparse maintenance, unnecessary abstraction |
| `WithReuseByName` | Experimental API, name collision risks |
| Custom CLI test DSL | Maintenance burden, stdlib works fine |

---

## Sources

- [Testcontainers-go Official Docs](https://golang.testcontainers.org/)
- [Testcontainers-go Releases](https://github.com/testcontainers/testcontainers-go/releases)
- [Testcontainers Best Practices (Docker Blog)](https://www.docker.com/blog/testcontainers-best-practices/)
- [stretchr/testify Releases](https://github.com/stretchr/testify/releases)
- [testify/suite Package Docs](https://pkg.go.dev/github.com/stretchr/testify/suite)
- [Cobra CLI Testing (spf13/cobra#770)](https://github.com/spf13/cobra/issues/770)
- [Testing Cobra Commands (Medium)](https://nayaktapan37.medium.com/testing-cobra-commands-in-golang-ca1fe4ad6657)
- [Testcontainers Postgres Module](https://golang.testcontainers.org/modules/postgres/)
- [Testcontainers LocalStack Module](https://golang.testcontainers.org/modules/localstack/)
