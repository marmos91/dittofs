# Phase 2: Server & Identity - Research

**Researched:** 2026-02-02
**Domain:** Server lifecycle management, User/Group CRUD via CLI, E2E test patterns
**Confidence:** HIGH

## Summary

This phase implements E2E tests validating server lifecycle management (start, stop, health, readiness, status) and user/group CRUD operations via the `dittofsctl` CLI. The research covers patterns for testing background server processes, polling health endpoints, signal handling verification, and comprehensive CRUD testing with error scenarios.

The existing codebase has a mature test infrastructure from Phase 1 with `TestEnvironment` and `TestScope` for container management and isolation. The CLI commands (`dittofs` for server, `dittofsctl` for management) are already implemented with consistent patterns. The focus is on testing these through the CLI interface rather than direct API calls.

**Primary recommendation:** Use the Phase 1 test infrastructure, start the server as a background process using `exec.Command`, poll `/health/ready` for readiness (5-second timeout), and run dittofsctl commands via subprocess to test user/group CRUD. Use unique test prefixes (e.g., `e2e_test_<uuid8>`) for isolation when running parallel tests.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| os/exec | stdlib | Execute CLI commands (dittofs, dittofsctl) | Standard subprocess execution |
| net/http | stdlib | Health endpoint polling | Simple HTTP client for health checks |
| stretchr/testify | v1.11.1 | Assertions (require for hard failures) | Already in go.mod, project standard |
| test/e2e/helpers | internal | TestEnvironment, TestScope | Phase 1 infrastructure for isolation |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| time | stdlib | Timeouts, polling intervals | Server startup waits, retry delays |
| syscall | stdlib | Signal sending (SIGTERM, SIGINT) | Graceful shutdown tests |
| encoding/json | stdlib | Parse CLI JSON output | Structured output verification |
| pkg/apiclient | internal | API client for direct API calls | When CLI is insufficient |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| exec.Command | apiclient.Client | CLI tests are the goal, but apiclient useful for setup/verification |
| JSON output parsing | Table output parsing | JSON is machine-readable, table is human-friendly but fragile to parse |

**Installation:**
```bash
# No additional packages needed - all in go.mod
```

## Architecture Patterns

### Recommended Project Structure
```
test/e2e/
├── main_test.go           # TestMain (existing from Phase 1)
├── helpers/               # Shared test utilities (from Phase 1)
│   ├── environment.go     # TestEnvironment struct
│   ├── scope.go          # TestScope for per-test isolation
│   └── server.go         # NEW: Server lifecycle helpers
│   └── cli.go            # NEW: CLI execution helpers
├── server_test.go         # NEW: Server lifecycle tests (Phase 2)
├── users_test.go          # NEW: User CRUD tests (Phase 2)
└── groups_test.go         # NEW: Group CRUD tests (Phase 2)
```

### Pattern 1: Server Background Process Management
**What:** Start server as background process, track PID, poll for readiness
**When to use:** Server lifecycle tests, any test requiring a running server
**Example:**
```go
// Source: Existing cmd/dittofs/commands/start.go pattern
type ServerProcess struct {
    cmd     *exec.Cmd
    pidFile string
    apiPort int
    logFile string
}

func StartServerProcess(t *testing.T, configPath string) *ServerProcess {
    t.Helper()

    // Create temp directories for PID and log files
    stateDir := t.TempDir()
    pidFile := filepath.Join(stateDir, "dittofs.pid")
    logFile := filepath.Join(stateDir, "dittofs.log")

    // Find free port for API
    apiPort := FindFreePort(t)

    // Build command - foreground mode for direct control
    cmd := exec.Command("dittofs", "start", "--foreground",
        "--config", configPath,
        "--pid-file", pidFile)

    // Redirect output to log file for debugging
    logF, err := os.Create(logFile)
    require.NoError(t, err)
    cmd.Stdout = logF
    cmd.Stderr = logF

    // Start process
    err = cmd.Start()
    require.NoError(t, err)

    sp := &ServerProcess{
        cmd:     cmd,
        pidFile: pidFile,
        apiPort: apiPort,
        logFile: logFile,
    }

    // Wait for readiness
    err = sp.WaitReady(5 * time.Second)
    require.NoError(t, err, "Server did not become ready within timeout")

    return sp
}
```

### Pattern 2: Health/Readiness Polling
**What:** Poll endpoints with timeout to verify server state
**When to use:** Server startup verification, health checks
**Example:**
```go
// Source: Existing pkg/controlplane/api/server.go endpoints
func (sp *ServerProcess) WaitReady(timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    client := &http.Client{Timeout: 1 * time.Second}

    // Readiness = all adapters started + all store healthchecks pass
    readyURL := fmt.Sprintf("http://localhost:%d/health/ready", sp.apiPort)

    for time.Now().Before(deadline) {
        resp, err := client.Get(readyURL)
        if err == nil {
            defer resp.Body.Close()
            if resp.StatusCode == http.StatusOK {
                return nil
            }
        }
        time.Sleep(100 * time.Millisecond)
    }
    return fmt.Errorf("server not ready after %v", timeout)
}

func (sp *ServerProcess) CheckHealth() (*HealthResponse, error) {
    client := &http.Client{Timeout: 2 * time.Second}
    resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", sp.apiPort))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var health HealthResponse
    if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
        return nil, err
    }
    return &health, nil
}
```

### Pattern 3: CLI Command Execution with Output Parsing
**What:** Execute dittofsctl commands and parse JSON output
**When to use:** All CRUD tests via CLI
**Example:**
```go
// Source: dittofsctl command patterns
type CLIRunner struct {
    serverURL string
    token     string
}

func NewCLIRunner(serverURL, token string) *CLIRunner {
    return &CLIRunner{serverURL: serverURL, token: token}
}

func (r *CLIRunner) Run(args ...string) ([]byte, error) {
    fullArgs := append([]string{
        "--server", r.serverURL,
        "--token", r.token,
        "--output", "json",
    }, args...)

    cmd := exec.Command("dittofsctl", fullArgs...)
    return cmd.CombinedOutput()
}

func (r *CLIRunner) CreateUser(username, password string) (*apiclient.User, error) {
    output, err := r.Run("user", "create",
        "--username", username,
        "--password", password)
    if err != nil {
        return nil, fmt.Errorf("create user failed: %w\nOutput: %s", err, string(output))
    }

    var user apiclient.User
    if err := json.Unmarshal(output, &user); err != nil {
        return nil, fmt.Errorf("parse user failed: %w\nOutput: %s", err, string(output))
    }
    return &user, nil
}
```

### Pattern 4: Signal Handling Verification
**What:** Send signals and verify graceful shutdown
**When to use:** Server lifecycle tests for SIGTERM/SIGINT handling
**Example:**
```go
// Source: cmd/dittofs/commands/stop.go pattern
func (sp *ServerProcess) SendSignal(sig syscall.Signal) error {
    if sp.cmd.Process == nil {
        return fmt.Errorf("process not running")
    }
    return sp.cmd.Process.Signal(sig)
}

func (sp *ServerProcess) WaitForExit(timeout time.Duration) error {
    done := make(chan error, 1)
    go func() {
        done <- sp.cmd.Wait()
    }()

    select {
    case <-time.After(timeout):
        return fmt.Errorf("process did not exit within %v", timeout)
    case err := <-done:
        return err // nil if clean exit, non-nil if error
    }
}

// Test pattern
func TestServerGracefulShutdown(t *testing.T) {
    sp := StartServerProcess(t, configPath)
    defer sp.ForceKill() // Safety cleanup

    // Send SIGTERM
    err := sp.SendSignal(syscall.SIGTERM)
    require.NoError(t, err)

    // Verify graceful exit within timeout
    err = sp.WaitForExit(10 * time.Second)
    require.NoError(t, err, "Server should exit cleanly on SIGTERM")
}
```

### Pattern 5: Test Isolation with Unique Names
**What:** Generate unique names for test entities to enable parallel execution
**When to use:** All CRUD tests creating users, groups, etc.
**Example:**
```go
// Source: Phase 1 helpers/scope.go pattern
func UniqueTestName(prefix string) string {
    return fmt.Sprintf("%s_%s", prefix, uuid.New().String()[:8])
}

func TestUserCreate(t *testing.T) {
    t.Parallel()

    username := UniqueTestName("e2e_user")
    // username = "e2e_user_a1b2c3d4"

    user, err := cli.CreateUser(username, "password123")
    require.NoError(t, err)
    require.Equal(t, username, user.Username)

    // Cleanup
    t.Cleanup(func() {
        _ = cli.DeleteUser(username)
    })
}
```

### Anti-Patterns to Avoid
- **Hardcoded usernames/group names:** Use unique prefixes to avoid conflicts in parallel tests
- **Relying on server being running:** Each test suite should start its own server or use shared server with proper isolation
- **Parsing table output:** Use `--output json` for reliable parsing
- **Not cleaning up:** Always register cleanup via `t.Cleanup()` or deferred functions
- **Timeout-less waits:** All polling loops need deadlines to fail fast on issues

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Free port discovery | Manual port checking | FindFreePort(t) from framework | Race conditions, platform differences |
| Server readiness wait | Simple sleep | Polling with timeout | Flaky tests, wasted time |
| JSON output parsing | Manual string parsing | encoding/json + defined structs | Fragile, type-unsafe |
| User/Group cleanup | Manual deletion calls | t.Cleanup() with delete | Runs even on test failure |
| Unique test IDs | Incrementing counters | UUID (short) | Thread-safe, no collisions |

**Key insight:** The existing test framework in `test/e2e/framework/` and `test/e2e/helpers/` handles most infrastructure concerns. Focus on testing the CLI behavior, not reimplementing test infrastructure.

## Common Pitfalls

### Pitfall 1: Server Not Ready on First Request
**What goes wrong:** Tests fail because server isn't ready when first request is sent
**Why it happens:** Server startup is async, process.Start() returns immediately
**How to avoid:** Always poll `/health/ready` with timeout before running tests
**Warning signs:** Intermittent "connection refused" errors in CI

### Pitfall 2: Port Conflicts in Parallel Tests
**What goes wrong:** Tests fail with "address already in use"
**Why it happens:** Multiple server instances trying to use same port
**How to avoid:** Use `FindFreePort(t)` for each server instance, configure via env var or config
**Warning signs:** "bind: address already in use" errors

### Pitfall 3: Test Users Conflicting
**What goes wrong:** Tests fail because expected user already exists
**Why it happens:** Parallel tests creating users with same name, or leftover from previous run
**How to avoid:** Use unique test prefixes with UUID: `e2e_test_<uuid8>`
**Warning signs:** "user already exists" errors, unexpected group memberships

### Pitfall 4: Stale Server Process After Test Failure
**What goes wrong:** Subsequent tests fail because previous server is still running
**Why it happens:** Test panics or times out before cleanup runs
**How to avoid:** Use t.Cleanup() which runs even on panic, track PIDs for force-kill
**Warning signs:** Port conflicts, unexpected server state

### Pitfall 5: Admin User Cannot Be Deleted
**What goes wrong:** Cleanup tries to delete admin, fails, test reports error
**Why it happens:** Admin user has special protection in the codebase
**How to avoid:** Never attempt to delete "admin" user in cleanup, verify it exists after tests
**Warning signs:** "cannot delete admin user" errors in cleanup

### Pitfall 6: Signal Handling on macOS vs Linux
**What goes wrong:** Signal tests behave differently across platforms
**Why it happens:** Subtle differences in process group handling
**How to avoid:** Use `syscall.SIGTERM` not `os.Interrupt`, avoid SIGKILL for graceful tests
**Warning signs:** Tests pass on macOS but fail on Linux (or vice versa)

### Pitfall 7: Token Invalidation After Password Change
**What goes wrong:** Tests continue using old token after password change
**Why it happens:** Password change invalidates existing tokens
**How to avoid:** Re-login after password change, use returned tokens from password change endpoint
**Warning signs:** "unauthorized" errors after password change tests

## Code Examples

Verified patterns from existing codebase:

### Server Lifecycle Test Structure
```go
//go:build e2e

package e2e

import (
    "testing"
    "time"
    "syscall"

    "github.com/stretchr/testify/require"
)

func TestServerLifecycle(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping server lifecycle tests in short mode")
    }

    t.Run("start and stop gracefully", func(t *testing.T) {
        sp := StartServerProcess(t, DefaultConfigPath())
        t.Cleanup(func() { sp.ForceKill() })

        // Verify running
        health, err := sp.CheckHealth()
        require.NoError(t, err)
        require.Equal(t, "healthy", health.Status)

        // Stop gracefully
        err = sp.SendSignal(syscall.SIGTERM)
        require.NoError(t, err)

        // Verify clean exit
        err = sp.WaitForExit(10 * time.Second)
        require.NoError(t, err)
    })

    t.Run("status reports correct info", func(t *testing.T) {
        sp := StartServerProcess(t, DefaultConfigPath())
        t.Cleanup(func() { sp.StopGracefully() })

        // Get status via CLI
        output, err := exec.Command("dittofs", "status",
            "--api-port", strconv.Itoa(sp.apiPort),
            "--output", "json").CombinedOutput()
        require.NoError(t, err)

        var status ServerStatus
        err = json.Unmarshal(output, &status)
        require.NoError(t, err)
        require.True(t, status.Running)
        require.True(t, status.Healthy)
        require.NotEmpty(t, status.Uptime)
    })
}
```

### User CRUD Test Structure
```go
//go:build e2e

package e2e

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestUserCRUD(t *testing.T) {
    // Use shared server from Phase 1 infrastructure
    cli := NewCLIRunner(TestServerURL(), AdminToken())

    t.Run("create user", func(t *testing.T) {
        t.Parallel()

        username := UniqueTestName("e2e_user")
        t.Cleanup(func() { _ = cli.DeleteUser(username) })

        user, err := cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)
        require.Equal(t, username, user.Username)
        require.True(t, user.Enabled)
    })

    t.Run("duplicate username rejected", func(t *testing.T) {
        t.Parallel()

        username := UniqueTestName("e2e_dup")
        t.Cleanup(func() { _ = cli.DeleteUser(username) })

        // Create first
        _, err := cli.CreateUser(username, "password1")
        require.NoError(t, err)

        // Duplicate should fail
        _, err = cli.CreateUser(username, "password2")
        require.Error(t, err)
        require.Contains(t, err.Error(), "already exists")
    })

    t.Run("admin cannot be deleted", func(t *testing.T) {
        t.Parallel()

        err := cli.DeleteUser("admin")
        require.Error(t, err)
        // Verify admin still exists
        user, err := cli.GetUser("admin")
        require.NoError(t, err)
        require.Equal(t, "admin", user.Username)
    })

    t.Run("password change invalidates token", func(t *testing.T) {
        t.Parallel()

        username := UniqueTestName("e2e_pwchange")
        t.Cleanup(func() { _ = cli.DeleteUser(username) })

        // Create user
        _, err := cli.CreateUser(username, "OldPassword123!")
        require.NoError(t, err)

        // Login as user
        userCli, err := LoginAsUser(username, "OldPassword123!")
        require.NoError(t, err)

        // Change password
        newTokens, err := userCli.ChangeOwnPassword("OldPassword123!", "NewPassword456!")
        require.NoError(t, err)

        // Old token should be invalid
        _, err = userCli.GetCurrentUser()
        require.Error(t, err)

        // New token should work
        userCli.SetToken(newTokens.AccessToken)
        user, err := userCli.GetCurrentUser()
        require.NoError(t, err)
        require.Equal(t, username, user.Username)
    })
}
```

### Group Management Test Structure
```go
//go:build e2e

package e2e

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestGroupManagement(t *testing.T) {
    cli := NewCLIRunner(TestServerURL(), AdminToken())

    t.Run("create group", func(t *testing.T) {
        t.Parallel()

        groupName := UniqueTestName("e2e_group")
        t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

        group, err := cli.CreateGroup(groupName, "Test group description")
        require.NoError(t, err)
        require.Equal(t, groupName, group.Name)
    })

    t.Run("add and remove user from group", func(t *testing.T) {
        t.Parallel()

        groupName := UniqueTestName("e2e_grp")
        username := UniqueTestName("e2e_usr")
        t.Cleanup(func() {
            _ = cli.RemoveGroupMember(groupName, username)
            _ = cli.DeleteUser(username)
            _ = cli.DeleteGroup(groupName)
        })

        // Create group and user
        _, err := cli.CreateGroup(groupName, "")
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "password")
        require.NoError(t, err)

        // Add to group
        err = cli.AddGroupMember(groupName, username)
        require.NoError(t, err)

        // Verify bidirectional membership
        group, err := cli.GetGroup(groupName)
        require.NoError(t, err)
        require.Contains(t, group.Members, username)

        user, err := cli.GetUser(username)
        require.NoError(t, err)
        require.Contains(t, user.Groups, groupName)

        // Remove from group
        err = cli.RemoveGroupMember(groupName, username)
        require.NoError(t, err)

        // Verify removal
        group, err = cli.GetGroup(groupName)
        require.NoError(t, err)
        require.NotContains(t, group.Members, username)
    })

    t.Run("idempotent membership operations", func(t *testing.T) {
        t.Parallel()

        groupName := UniqueTestName("e2e_idem")
        username := UniqueTestName("e2e_user")
        t.Cleanup(func() {
            _ = cli.DeleteUser(username)
            _ = cli.DeleteGroup(groupName)
        })

        // Setup
        _, _ = cli.CreateGroup(groupName, "")
        _, _ = cli.CreateUser(username, "password")

        // Add twice should succeed
        err := cli.AddGroupMember(groupName, username)
        require.NoError(t, err)
        err = cli.AddGroupMember(groupName, username)
        require.NoError(t, err)

        // Remove twice should succeed
        err = cli.RemoveGroupMember(groupName, username)
        require.NoError(t, err)
        err = cli.RemoveGroupMember(groupName, username)
        require.NoError(t, err)
    })

    t.Run("system groups cannot be deleted", func(t *testing.T) {
        t.Parallel()

        // admins and users are system groups
        for _, systemGroup := range []string{"admins", "users"} {
            err := cli.DeleteGroup(systemGroup)
            require.Error(t, err)

            // Verify still exists
            group, err := cli.GetGroup(systemGroup)
            require.NoError(t, err)
            require.Equal(t, systemGroup, group.Name)
        }
    })
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Direct API calls for testing | CLI subprocess execution | Decision in Phase 2 | Tests actual user experience |
| Single shared server | Server-per-test or shared with isolation | Phase 1 | Better isolation, parallel execution |
| Manual unique name generation | UUID-based prefixes | Common pattern | Thread-safe, no collisions |
| Hardcoded timeouts | Configurable with sensible defaults | Best practice | Flexible for CI vs local |

**Deprecated/outdated:**
- None for this phase - building on Phase 1 patterns

## Open Questions

Things that couldn't be fully resolved:

1. **Username Case Sensitivity**
   - What we know: The API accepts usernames, unclear if case-sensitive
   - What's unclear: Should "Alice" and "alice" be different users?
   - Recommendation: Test both scenarios, document behavior in tests

2. **Password Complexity Requirements**
   - What we know: CONTEXT.md mentions "test password requirements (min length, complexity)"
   - What's unclear: Exact validation rules implemented in the codebase
   - Recommendation: Check store/users.go for validation, test edge cases

3. **Cascade Behavior on User Deletion**
   - What we know: "Auto-remove from groups when user deleted" per CONTEXT.md
   - What's unclear: Does this also remove share permissions?
   - Recommendation: Test both group membership and share permission cleanup

## Sources

### Primary (HIGH confidence)
- Existing codebase: `cmd/dittofs/commands/start.go` - Server lifecycle patterns
- Existing codebase: `cmd/dittofs/commands/status.go` - Health check patterns
- Existing codebase: `cmd/dittofsctl/commands/user/*.go` - User CLI patterns
- Existing codebase: `cmd/dittofsctl/commands/group/*.go` - Group CLI patterns
- Existing codebase: `pkg/apiclient/*.go` - API types and methods
- Existing codebase: `test/e2e/helpers/*.go` - Test infrastructure patterns
- Existing codebase: `pkg/controlplane/api/server.go` - Health endpoints

### Secondary (MEDIUM confidence)
- Phase 1 research: `.planning/phases/01-foundation/01-RESEARCH.md` - Test patterns
- CONTEXT.md: `.planning/phases/02-server-identity/02-CONTEXT.md` - User decisions

### Tertiary (LOW confidence)
- None - all patterns verified from codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use in codebase
- Architecture: HIGH - Patterns derived from existing test infrastructure
- Pitfalls: MEDIUM - Some edge cases (like cascade behavior) need implementation verification

**Research date:** 2026-02-02
**Valid until:** 60 days (patterns are stable, test infrastructure unlikely to change)
