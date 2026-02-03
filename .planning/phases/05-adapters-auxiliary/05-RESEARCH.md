# Phase 5: Adapters & Auxiliary - Research

**Researched:** 2026-02-02
**Domain:** Protocol adapter lifecycle, backup/restore, multi-context management
**Confidence:** HIGH

## Summary

This phase covers E2E testing for three distinct subsystems: protocol adapter lifecycle management (NFS/SMB enable/disable with hot reload), control plane backup/restore operations, and multi-context server management for dittofsctl. All three areas are well-implemented in the existing codebase with clear interfaces.

The adapter lifecycle is managed through the Runtime with graceful drain support already built into the NFS adapter. Backup functionality exists via `dittofs backup controlplane` with support for SQLite VACUUM INTO, pg_dump, and JSON export formats. Multi-context credentials are stored in `~/.config/dittofsctl/config.json` with full CRUD operations already implemented.

**Primary recommendation:** Test through CLI commands (dittofsctl/dittofs) following the established pattern, extending CLIRunner with Adapter, Backup, and Context helper methods.

## Standard Stack

### Core (Already in Codebase)

| Component | Location | Purpose | Status |
|-----------|----------|---------|--------|
| Runtime adapter management | `pkg/controlplane/runtime/runtime.go` | CreateAdapter, EnableAdapter, DisableAdapter | Complete |
| NFS Adapter | `pkg/adapter/nfs/nfs_adapter.go` | Graceful shutdown, connection draining | Complete |
| API client adapters | `pkg/apiclient/adapters.go` | ListAdapters, UpdateAdapter, CreateAdapter | Complete |
| Backup command | `cmd/dittofs/commands/backup/` | backup controlplane with multiple formats | Complete |
| Credentials store | `internal/cli/credentials/store.go` | Multi-context storage | Complete |
| Context CLI commands | `cmd/dittofsctl/commands/context/` | list, use, delete, rename, current | Complete |

### Supporting

| Component | Location | Purpose | Status |
|-----------|----------|---------|--------|
| CLIRunner | `test/e2e/helpers/cli.go` | CLI test helper | Needs extension |
| ServerProcess | `test/e2e/helpers/server.go` | Server lifecycle for tests | Complete |
| GORM Store | `pkg/controlplane/store/gorm.go` | SQLite/PostgreSQL backend | Complete |

## Architecture Patterns

### Existing Adapter CLI Structure

```
dittofsctl adapter
  list                    # List all adapters with status
  enable <type>           # Enable and start adapter
  disable <type>          # Stop and disable adapter
  edit <type>             # Edit adapter configuration
```

### Existing Backup CLI Structure

```
dittofs backup
  controlplane            # Backup control plane DB
    --output <path>       # Output file (required)
    --format <fmt>        # native, native-cli, json
    --config <path>       # Config file path
```

### Existing Context CLI Structure

```
dittofsctl context
  list                    # List all contexts
  use <name>              # Switch to context
  current                 # Show current context
  rename <old> <new>      # Rename context
  delete <name>           # Delete context
```

### Pattern: CLI-Driven E2E Testing (from prior phases)

Tests use CLIRunner to execute commands and parse JSON output:

```go
// Test adapter enable/disable cycle
func TestAdapterLifecycle(t *testing.T) {
    runner := helpers.LoginAsAdmin(t, serverURL)

    // Disable NFS adapter
    err := runner.DisableAdapter("nfs")
    require.NoError(t, err)

    // Verify disabled
    adapter, err := runner.GetAdapter("nfs")
    require.NoError(t, err)
    require.False(t, adapter.Enabled)

    // Re-enable
    err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(12049))
    require.NoError(t, err)
}
```

### Pattern: Adapter Status Response

Based on `pkg/apiclient/adapters.go`:

```go
type Adapter struct {
    Type    string          `json:"type"`
    Port    int             `json:"port"`
    Enabled bool            `json:"enabled"`
    Config  json.RawMessage `json:"config,omitempty"`
}
```

### Pattern: Context Data Structure

Based on `internal/cli/credentials/store.go`:

```go
type Context struct {
    ServerURL    string    `json:"server_url"`
    Username     string    `json:"username,omitempty"`
    AccessToken  string    `json:"access_token,omitempty"`
    RefreshToken string    `json:"refresh_token,omitempty"`
    ExpiresAt    time.Time `json:"expires_at,omitempty"`
}
```

## Don't Hand-Roll

| Problem | Use Instead | Why |
|---------|-------------|-----|
| Adapter enable/disable | `runtime.EnableAdapter()`, `runtime.DisableAdapter()` | Handles store + live state sync |
| Connection draining | NFSAdapter.Stop() with context timeout | Already implements graceful drain |
| Backup format detection | Existing `backupSQLiteNative`, `backupJSON` | Handles all edge cases |
| Context switching | `credentials.Store.UseContext()` | Handles file I/O, validation |
| Token extraction | `extractTokenFromCredentialsFile()` | Existing helper in cli.go |

## Common Pitfalls

### Pitfall 1: Port Conflicts in Parallel Tests

**What goes wrong:** Multiple tests try to use the same adapter port (e.g., 12049 for NFS)
**Why it happens:** Tests run in parallel, each test instance wants to enable adapters
**How to avoid:**
- Use `FindFreePort()` helper when enabling adapters in tests
- Or run adapter lifecycle tests sequentially (`t.Parallel()` NOT called)
- Tests should use unique ports: `runner.EnableAdapter("nfs", WithAdapterPort(FindFreePort(t)))`
**Warning signs:** "address already in use" errors in CI

### Pitfall 2: Restore Requires Server Stopped

**What goes wrong:** Attempting to restore backup while server is running
**Why it happens:** Decision was "restore requires server stopped" but tests might forget this
**How to avoid:**
- Stop server before restore operation
- Verify server is not running before restore
- Start fresh server after restore
**Warning signs:** Database lock errors, "database is locked"

### Pitfall 3: Context Token Expiry During Tests

**What goes wrong:** Test fails because token expired mid-test
**Why it happens:** Long-running tests with short token expiry
**How to avoid:**
- Use fresh login before each test that needs auth
- Set longer token expiry in test config
- Handle token refresh in CLIRunner
**Warning signs:** 401 Unauthorized errors mid-test

### Pitfall 4: Graceful Drain Timing

**What goes wrong:** Tests don't wait for connections to drain during disable
**Why it happens:** Adapter disable is async, test proceeds too fast
**How to avoid:**
- Poll adapter status until fully stopped
- Check connection count via API/metrics
- Use timeout-based waiting with retries
**Warning signs:** Race conditions, "adapter still running" errors

### Pitfall 5: Config File State Leakage Between Tests

**What goes wrong:** Context tests affect each other via shared config file
**Why it happens:** `~/.config/dittofsctl/config.json` is shared across tests
**How to avoid:**
- Set `XDG_CONFIG_HOME` to test-specific temp directory
- Clean up contexts after each test
- Use unique context names per test
**Warning signs:** Tests pass individually but fail when run together

## Code Examples

### CLIRunner Extension: Adapter Methods

```go
// Adapter represents an adapter from the API.
type Adapter struct {
    Type    string `json:"type"`
    Port    int    `json:"port"`
    Enabled bool   `json:"enabled"`
}

// AdapterOption is a functional option for adapter operations.
type AdapterOption func(*adapterOptions)

type adapterOptions struct {
    port *int
}

// WithAdapterPort sets the port for adapter enable.
func WithAdapterPort(port int) AdapterOption {
    return func(o *adapterOptions) {
        o.port = &port
    }
}

// ListAdapters lists all adapters via the CLI.
func (r *CLIRunner) ListAdapters() ([]*Adapter, error) {
    output, err := r.Run("adapter", "list")
    if err != nil {
        return nil, err
    }

    var adapters []*Adapter
    if err := ParseJSONResponse(output, &adapters); err != nil {
        return nil, err
    }

    return adapters, nil
}

// GetAdapter retrieves an adapter by type.
func (r *CLIRunner) GetAdapter(adapterType string) (*Adapter, error) {
    adapters, err := r.ListAdapters()
    if err != nil {
        return nil, err
    }

    for _, a := range adapters {
        if a.Type == adapterType {
            return a, nil
        }
    }

    return nil, fmt.Errorf("adapter not found: %s", adapterType)
}

// EnableAdapter enables an adapter via the CLI.
func (r *CLIRunner) EnableAdapter(adapterType string, opts ...AdapterOption) error {
    options := &adapterOptions{}
    for _, opt := range opts {
        opt(options)
    }

    args := []string{"adapter", "enable", adapterType}
    if options.port != nil {
        args = append(args, "--port", fmt.Sprintf("%d", *options.port))
    }

    _, err := r.Run(args...)
    return err
}

// DisableAdapter disables an adapter via the CLI.
func (r *CLIRunner) DisableAdapter(adapterType string) error {
    _, err := r.Run("adapter", "disable", adapterType)
    return err
}
```

### CLIRunner Extension: Context Methods

```go
// ContextInfo represents context information for tests.
type ContextInfo struct {
    Name      string `json:"name"`
    Current   bool   `json:"current"`
    ServerURL string `json:"server_url"`
    Username  string `json:"username,omitempty"`
    LoggedIn  bool   `json:"logged_in"`
}

// ListContexts lists all contexts via the CLI.
func (r *CLIRunner) ListContexts() ([]*ContextInfo, error) {
    output, err := r.Run("context", "list")
    if err != nil {
        return nil, err
    }

    var contexts []*ContextInfo
    if err := ParseJSONResponse(output, &contexts); err != nil {
        return nil, err
    }

    return contexts, nil
}

// UseContext switches to a context via the CLI.
func (r *CLIRunner) UseContext(name string) error {
    _, err := r.RunRaw("context", "use", name)
    return err
}

// DeleteContext deletes a context via the CLI.
func (r *CLIRunner) DeleteContext(name string) error {
    _, err := r.RunRaw("context", "delete", name)
    return err
}

// GetCurrentContext returns the current context name.
func (r *CLIRunner) GetCurrentContext() (string, error) {
    output, err := r.RunRaw("context", "current")
    if err != nil {
        return "", err
    }
    return strings.TrimSpace(string(output)), nil
}
```

### Backup Test Pattern

```go
func TestBackupRestore(t *testing.T) {
    // Setup: Start server, create test data
    server := helpers.StartServerProcess(t, configPath)
    defer server.ForceKill()

    runner := helpers.LoginAsAdmin(t, server.APIURL())

    // Create test data
    _, err := runner.CreateUser("backuptest", "password123")
    require.NoError(t, err)

    // Stop server for backup (online backup is OK, but restore requires stopped)
    err = server.StopGracefully()
    require.NoError(t, err)

    // Run backup via dittofs command
    backupFile := filepath.Join(t.TempDir(), "backup.db")
    output, err := helpers.RunDittofs(t, "backup", "controlplane",
        "--config", server.ConfigFile(),
        "--output", backupFile,
        "--format", "native")
    require.NoError(t, err, "backup failed: %s", output)

    // Verify backup file exists
    _, err = os.Stat(backupFile)
    require.NoError(t, err)

    // Restore test: Would need fresh DB, then restore
    // (Restore requires empty state per CONTEXT.md)
}
```

### Adapter Status Polling Pattern

```go
// WaitForAdapterStatus polls until adapter reaches expected state.
func WaitForAdapterStatus(t *testing.T, runner *CLIRunner, adapterType string, enabled bool, timeout time.Duration) error {
    t.Helper()

    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        adapter, err := runner.GetAdapter(adapterType)
        if err != nil {
            time.Sleep(100 * time.Millisecond)
            continue
        }

        if adapter.Enabled == enabled {
            return nil
        }

        time.Sleep(100 * time.Millisecond)
    }

    return fmt.Errorf("adapter %s did not reach enabled=%v within %v", adapterType, enabled, timeout)
}
```

## State of the Art

| Old Approach | Current Approach | Impact |
|--------------|------------------|--------|
| Direct API calls | CLI-driven tests | Per STATE.md, all tests use dittofsctl |
| Manual adapter control | Runtime.EnableAdapter/DisableAdapter | Coordinated store + live state |
| Raw file backup | VACUUM INTO / pg_dump | Atomic, safe for hot backup |

## Open Questions

1. **Protocol verification after enable**
   - CONTEXT.md mentions "verify basic protocol check after enable"
   - What constitutes a valid check? Mount success? Health endpoint?
   - **Recommendation:** Test that health endpoint shows adapter running + correct port

2. **Hot reload scope**
   - CONTEXT.md says "all adapter settings can change without full restart"
   - Does this include port changes? (Likely requires listener restart)
   - **Recommendation:** Test port change triggers proper listener restart

3. **Progress feedback testing**
   - CONTEXT.md requests "Progress steps shown during operations"
   - How to test spinner/progress output in automated tests?
   - **Recommendation:** Test verbose mode (-v) output contains expected steps

4. **Restore preview implementation**
   - CONTEXT.md requests "Show summary of what will be restored"
   - No restore command found in codebase yet
   - **Recommendation:** This may be new implementation work, not just testing

## Sources

### Primary (HIGH confidence)
- `/Users/marmos91/Projects/dittofs/pkg/controlplane/runtime/runtime.go` - Adapter lifecycle management
- `/Users/marmos91/Projects/dittofs/pkg/adapter/nfs/nfs_adapter.go` - Graceful shutdown implementation
- `/Users/marmos91/Projects/dittofs/cmd/dittofs/commands/backup/controlplane.go` - Backup implementation
- `/Users/marmos91/Projects/dittofs/internal/cli/credentials/store.go` - Context storage
- `/Users/marmos91/Projects/dittofs/test/e2e/helpers/cli.go` - Existing CLIRunner pattern
- `/Users/marmos91/Projects/dittofs/pkg/apiclient/adapters.go` - Adapter API types

### Secondary (MEDIUM confidence)
- `.planning/phases/05-adapters-auxiliary/05-CONTEXT.md` - User decisions for this phase

## Metadata

**Confidence breakdown:**
- Adapter lifecycle: HIGH - Complete implementation exists in codebase
- Backup/restore: HIGH - Backup exists, restore needs implementation check
- Multi-context: HIGH - Full implementation exists
- Test patterns: HIGH - Established patterns from prior phases

**Research date:** 2026-02-02
**Valid until:** 30 days (stable domain, implementation complete)
