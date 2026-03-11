# Phase 4: Shares & Permissions - Research

**Researched:** 2026-02-02
**Domain:** Share CRUD with store assignments, Permission management (users/groups) via CLI, E2E test patterns
**Confidence:** HIGH

## Summary

This phase implements E2E tests validating share creation with metadata/payload store assignments, share editing/deletion with soft delete behavior, and permission management (grant/revoke for users and groups) via the `dittofsctl` CLI. The research confirms that share and permission CLI commands are already implemented, but the current deletion is a hard delete while requirements specify soft delete with deferred cleanup.

The existing codebase has mature test infrastructure from Phases 1-3 with `CLIRunner`, `StartServerProcess`, and functional options patterns. Phase 3 already added minimal `CreateShare`/`DeleteShare` helpers that need expansion to support full CRUD operations. Permission commands follow standard patterns: `dittofsctl share permission grant/revoke/list`.

**Critical finding:** SHR-05 and SHR-06 require soft delete behavior that is NOT currently implemented. Current `DeleteShare` does hard delete (removes from database). Tests for SHR-05/SHR-06 will need to either:
1. Test the current (hard delete) behavior and defer soft delete to implementation phase
2. Skip SHR-05/SHR-06 tests until soft delete is implemented

**Primary recommendation:** Extend CLIRunner with full Share CRUD (expand existing minimal helpers) and Permission management methods. Use functional options pattern for share creation. Test permission grant/revoke with both users and groups. For SHR-05/SHR-06, document the gap and test what's currently implemented.

## Standard Stack

The established libraries/tools for this domain:

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| os/exec | stdlib | Execute CLI commands (dittofsctl) | Standard subprocess execution |
| stretchr/testify | v1.11.1 | Assertions (require for hard failures) | Already in go.mod, project standard |
| test/e2e/helpers | internal | CLIRunner, TestEnvironment | Phase 1-3 infrastructure |
| encoding/json | stdlib | Parse CLI JSON output | Structured output verification |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/google/uuid | v1.6.0 | Generate unique test names | Test isolation |
| strings | stdlib | Error message matching | Verify error responses |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| CLI testing | Direct apiclient calls | CLI tests are the goal per project decisions |
| Hard-coded share names | UniqueTestName() | Isolation for parallel tests |

**Installation:**
```bash
# No additional packages needed - all in go.mod
```

## Architecture Patterns

### Recommended Project Structure
```
test/e2e/
├── helpers/
│   └── cli.go            # Expand Share types/methods, add Permission types/methods
├── shares_test.go        # NEW: Share CRUD tests (SHR-01 through SHR-06)
└── permissions_test.go   # NEW: Permission tests (PRM-01 through PRM-07)
```

### Pattern 1: Share Type Definition
**What:** Define response types matching API JSON output for share operations
**When to use:** Parsing all share CLI responses
**Example:**
```go
// Source: Existing pkg/apiclient/shares.go types

// Share represents a share returned from the API.
type Share struct {
    ID                string `json:"id"`
    Name              string `json:"name"`
    MetadataStoreID   string `json:"metadata_store_id"`
    PayloadStoreID    string `json:"payload_store_id"`
    ReadOnly          bool   `json:"read_only"`
    DefaultPermission string `json:"default_permission"`
    CreatedAt         string `json:"created_at,omitempty"`
    UpdatedAt         string `json:"updated_at,omitempty"`
}
```

### Pattern 2: Functional Options for Share Creation
**What:** Type-safe options for share configuration
**When to use:** Creating shares with optional parameters
**Example:**
```go
// Source: Phase 2-3 patterns from helpers/cli.go

// ShareOption is a functional option for share operations.
type ShareOption func(*shareOptions)

type shareOptions struct {
    readOnly          bool
    hasReadOnly       bool
    defaultPermission string
    description       string
}

// WithShareReadOnly sets the read-only flag.
func WithShareReadOnly(readOnly bool) ShareOption {
    return func(o *shareOptions) {
        o.readOnly = readOnly
        o.hasReadOnly = true
    }
}

// WithShareDefaultPermission sets the default permission level.
// Valid values: "none", "read", "read-write", "admin"
func WithShareDefaultPermission(perm string) ShareOption {
    return func(o *shareOptions) {
        o.defaultPermission = perm
    }
}

// WithShareDescription sets the share description.
func WithShareDescription(desc string) ShareOption {
    return func(o *shareOptions) {
        o.description = desc
    }
}
```

### Pattern 3: Expanded Share CRUD Methods on CLIRunner
**What:** Replace minimal CreateShare with full CRUD supporting options
**When to use:** All share management tests
**Example:**
```go
// Source: Phase 3 pattern with expanded functionality

// CreateShare creates a share with metadata and payload stores.
// This expands the minimal Phase 3 helper with full options support.
func (r *CLIRunner) CreateShare(name, metadataStore, payloadStore string, opts ...ShareOption) (*Share, error) {
    o := &shareOptions{}
    for _, opt := range opts {
        opt(o)
    }

    args := []string{"share", "create", "--name", name, "--metadata", metadataStore, "--payload", payloadStore}

    if o.hasReadOnly {
        args = append(args, "--read-only", fmt.Sprintf("%t", o.readOnly))
    }
    if o.defaultPermission != "" {
        args = append(args, "--default-permission", o.defaultPermission)
    }
    if o.description != "" {
        args = append(args, "--description", o.description)
    }

    output, err := r.Run(args...)
    if err != nil {
        return nil, fmt.Errorf("create share failed: %w\noutput: %s", err, string(output))
    }

    var share Share
    if err := ParseJSONResponse(output, &share); err != nil {
        return nil, err
    }

    return &share, nil
}

// ListShares lists all shares.
func (r *CLIRunner) ListShares() ([]*Share, error) {
    output, err := r.Run("share", "list")
    if err != nil {
        return nil, fmt.Errorf("list shares failed: %w\noutput: %s", err, string(output))
    }

    var shares []*Share
    if err := ParseJSONResponse(output, &shares); err != nil {
        return nil, err
    }

    return shares, nil
}

// GetShare retrieves a share by name.
// Since there's no dedicated 'share get' command, this lists all shares and filters.
func (r *CLIRunner) GetShare(name string) (*Share, error) {
    shares, err := r.ListShares()
    if err != nil {
        return nil, err
    }

    for _, s := range shares {
        if s.Name == name {
            return s, nil
        }
    }

    return nil, fmt.Errorf("share not found: %s", name)
}

// EditShare updates an existing share.
func (r *CLIRunner) EditShare(name string, opts ...ShareOption) (*Share, error) {
    o := &shareOptions{}
    for _, opt := range opts {
        opt(o)
    }

    args := []string{"share", "edit", name}
    hasUpdate := false

    if o.hasReadOnly {
        args = append(args, "--read-only", fmt.Sprintf("%t", o.readOnly))
        hasUpdate = true
    }
    if o.defaultPermission != "" {
        args = append(args, "--default-permission", o.defaultPermission)
        hasUpdate = true
    }
    if o.description != "" {
        args = append(args, "--description", o.description)
        hasUpdate = true
    }

    if !hasUpdate {
        return nil, fmt.Errorf("at least one option is required for EditShare")
    }

    output, err := r.Run(args...)
    if err != nil {
        return nil, fmt.Errorf("edit share failed: %w\noutput: %s", err, string(output))
    }

    var share Share
    if err := ParseJSONResponse(output, &share); err != nil {
        return nil, err
    }

    return &share, nil
}

// DeleteShare deletes a share.
// Uses --force to skip confirmation prompt.
// Note: Current implementation is hard delete; SHR-05/SHR-06 require soft delete.
func (r *CLIRunner) DeleteShare(name string) error {
    _, err := r.Run("share", "delete", name, "--force")
    if err != nil {
        return fmt.Errorf("delete share failed: %w", err)
    }
    return nil
}
```

### Pattern 4: Permission Types and Methods
**What:** Types and methods for permission management via CLI
**When to use:** All permission tests (PRM-01 through PRM-07)
**Example:**
```go
// Source: pkg/apiclient/shares.go SharePermission type

// SharePermission represents a permission on a share.
type SharePermission struct {
    Type  string `json:"type"`  // "user" or "group"
    Name  string `json:"name"`  // username or group name
    Level string `json:"level"` // "none", "read", "read-write", "admin"
}

// GrantUserPermission grants a permission level to a user on a share.
func (r *CLIRunner) GrantUserPermission(shareName, username, level string) error {
    _, err := r.Run("share", "permission", "grant", shareName, "--user", username, "--level", level)
    if err != nil {
        return fmt.Errorf("grant user permission failed: %w", err)
    }
    return nil
}

// GrantGroupPermission grants a permission level to a group on a share.
func (r *CLIRunner) GrantGroupPermission(shareName, groupName, level string) error {
    _, err := r.Run("share", "permission", "grant", shareName, "--group", groupName, "--level", level)
    if err != nil {
        return fmt.Errorf("grant group permission failed: %w", err)
    }
    return nil
}

// RevokeUserPermission revokes a user's permission on a share.
func (r *CLIRunner) RevokeUserPermission(shareName, username string) error {
    _, err := r.Run("share", "permission", "revoke", shareName, "--user", username)
    if err != nil {
        return fmt.Errorf("revoke user permission failed: %w", err)
    }
    return nil
}

// RevokeGroupPermission revokes a group's permission on a share.
func (r *CLIRunner) RevokeGroupPermission(shareName, groupName string) error {
    _, err := r.Run("share", "permission", "revoke", shareName, "--group", groupName)
    if err != nil {
        return fmt.Errorf("revoke group permission failed: %w", err)
    }
    return nil
}

// ListSharePermissions lists all permissions on a share.
func (r *CLIRunner) ListSharePermissions(shareName string) ([]*SharePermission, error) {
    output, err := r.Run("share", "permission", "list", shareName)
    if err != nil {
        return nil, fmt.Errorf("list share permissions failed: %w\noutput: %s", err, string(output))
    }

    var perms []*SharePermission
    if err := ParseJSONResponse(output, &perms); err != nil {
        return nil, err
    }

    return perms, nil
}
```

### Pattern 5: Test Structure for Shares
**What:** Test structure following Phase 2-3 patterns
**When to use:** All share CRUD tests
**Example:**
```go
//go:build e2e

package e2e

import (
    "strings"
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestSharesCRUD(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping shares tests in short mode")
    }

    // Start server with automatic cleanup
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    // Create stores for share tests (shared across subtests)
    metaStore := helpers.UniqueTestName("meta_share")
    payloadStore := helpers.UniqueTestName("payload_share")
    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)
    t.Cleanup(func() {
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    t.Run("create share with stores", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_basic")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        share, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err, "Should create share")

        assert.Equal(t, shareName, share.Name)
        assert.Equal(t, metaStore, share.MetadataStoreID)
        assert.Equal(t, payloadStore, share.PayloadStoreID)
        assert.Equal(t, "read-write", share.DefaultPermission) // Default
    })

    t.Run("create share with options", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_opts")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        share, err := cli.CreateShare(shareName, metaStore, payloadStore,
            helpers.WithShareReadOnly(true),
            helpers.WithShareDefaultPermission("read"),
        )
        require.NoError(t, err, "Should create share with options")

        assert.True(t, share.ReadOnly)
        assert.Equal(t, "read", share.DefaultPermission)
    })

    t.Run("list shares", func(t *testing.T) {
        t.Parallel()

        shareName1 := "/" + helpers.UniqueTestName("share_list1")
        shareName2 := "/" + helpers.UniqueTestName("share_list2")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName1)
            _ = cli.DeleteShare(shareName2)
        })

        _, err := cli.CreateShare(shareName1, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateShare(shareName2, metaStore, payloadStore)
        require.NoError(t, err)

        shares, err := cli.ListShares()
        require.NoError(t, err)

        var found1, found2 bool
        for _, s := range shares {
            if s.Name == shareName1 {
                found1 = true
            }
            if s.Name == shareName2 {
                found2 = true
            }
        }

        assert.True(t, found1, "Should find share1")
        assert.True(t, found2, "Should find share2")
    })

    t.Run("edit share", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_edit")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        updated, err := cli.EditShare(shareName,
            helpers.WithShareReadOnly(true),
            helpers.WithShareDefaultPermission("admin"),
        )
        require.NoError(t, err)

        assert.True(t, updated.ReadOnly)
        assert.Equal(t, "admin", updated.DefaultPermission)
    })

    t.Run("delete share", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_del")

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        err = cli.DeleteShare(shareName)
        require.NoError(t, err, "Should delete share")

        // Verify share is gone
        _, err = cli.GetShare(shareName)
        require.Error(t, err)
        assert.Contains(t, err.Error(), "not found")
    })

    t.Run("duplicate share name rejected", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_dup")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        _, err = cli.CreateShare(shareName, metaStore, payloadStore)
        require.Error(t, err)
        errStr := strings.ToLower(err.Error())
        assert.True(t,
            strings.Contains(errStr, "already exists") ||
                strings.Contains(errStr, "conflict") ||
                strings.Contains(errStr, "duplicate"),
            "Error should indicate share already exists: %s", err.Error())
    })
}
```

### Pattern 6: Test Structure for Permissions
**What:** Test structure for permission grant/revoke/list operations
**When to use:** All permission tests
**Example:**
```go
//go:build e2e

package e2e

import (
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestSharePermissions(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping permission tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    // Setup: create stores and share for permission tests
    metaStore := helpers.UniqueTestName("meta_perm")
    payloadStore := helpers.UniqueTestName("payload_perm")
    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)
    t.Cleanup(func() {
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    t.Run("grant read permission to user", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_perm_r")
        username := helpers.UniqueTestName("user_perm_r")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        // Grant read permission
        err = cli.GrantUserPermission(shareName, username, "read")
        require.NoError(t, err)

        // Verify permission in list
        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        var found bool
        for _, p := range perms {
            if p.Type == "user" && p.Name == username && p.Level == "read" {
                found = true
                break
            }
        }
        assert.True(t, found, "Should find user permission")
    })

    t.Run("grant read-write permission to user", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_perm_rw")
        username := helpers.UniqueTestName("user_perm_rw")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        err = cli.GrantUserPermission(shareName, username, "read-write")
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        var found bool
        for _, p := range perms {
            if p.Type == "user" && p.Name == username && p.Level == "read-write" {
                found = true
                break
            }
        }
        assert.True(t, found, "Should find read-write permission")
    })

    t.Run("grant read permission to group", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_grp_r")
        groupName := helpers.UniqueTestName("group_perm_r")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read")
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        var found bool
        for _, p := range perms {
            if p.Type == "group" && p.Name == groupName && p.Level == "read" {
                found = true
                break
            }
        }
        assert.True(t, found, "Should find group permission")
    })

    t.Run("grant read-write permission to group", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_grp_rw")
        groupName := helpers.UniqueTestName("group_perm_rw")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read-write")
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        var found bool
        for _, p := range perms {
            if p.Type == "group" && p.Name == groupName && p.Level == "read-write" {
                found = true
                break
            }
        }
        assert.True(t, found, "Should find group read-write permission")
    })

    t.Run("revoke user permission", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_revoke_u")
        username := helpers.UniqueTestName("user_revoke")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        // Grant then revoke
        err = cli.GrantUserPermission(shareName, username, "read-write")
        require.NoError(t, err)

        err = cli.RevokeUserPermission(shareName, username)
        require.NoError(t, err)

        // Verify permission is gone
        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        for _, p := range perms {
            if p.Type == "user" && p.Name == username {
                t.Errorf("User permission should be revoked")
            }
        }
    })

    t.Run("revoke group permission", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_revoke_g")
        groupName := helpers.UniqueTestName("group_revoke")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read")
        require.NoError(t, err)

        err = cli.RevokeGroupPermission(shareName, groupName)
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        for _, p := range perms {
            if p.Type == "group" && p.Name == groupName {
                t.Errorf("Group permission should be revoked")
            }
        }
    })

    t.Run("list permissions shows empty for new share", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_empty_perm")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)
        assert.Empty(t, perms, "New share should have no explicit permissions")
    })
}
```

### Anti-Patterns to Avoid
- **Hardcoded share names:** Use `"/" + UniqueTestName("share_prefix")` for isolation
- **Not cleaning up in correct order:** Delete share before deleting stores
- **Testing permission on non-existent user/group:** Create user/group first
- **Assuming share names don't need leading slash:** Always prefix with `/`
- **Testing soft delete when it's not implemented:** Document the gap, test current behavior

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Unique share names | Manual counters | `"/" + UniqueTestName()` | Thread-safe, no collisions |
| Permission level validation | Custom checks | CLI validates levels | Server rejects invalid levels |
| Share name normalization | String manipulation | CLI normalizes names | Consistent with server behavior |
| User/group existence check | Separate API call | Let grant fail with clear error | Server validates relationships |
| Store existence for shares | Pre-validation | Let create fail with clear error | Server validates store references |

**Key insight:** The CLI and server already validate inputs (share names, permission levels, store references, user/group existence). Tests should verify these validations work correctly through the CLI interface.

## Common Pitfalls

### Pitfall 1: Share Names Without Leading Slash
**What goes wrong:** Share name stored differently than expected
**Why it happens:** CLI normalizes names to include leading slash
**How to avoid:** Always prefix share names with `/`: `"/" + UniqueTestName("share")`
**Warning signs:** Share not found errors, name mismatch in assertions

### Pitfall 2: Cleanup Order - Shares Before Stores
**What goes wrong:** Store deletion fails because share still references it
**Why it happens:** Foreign key constraint from share to store
**How to avoid:** Delete shares first, then stores. Register cleanup in correct order:
```go
t.Cleanup(func() {
    _ = cli.DeleteShare(shareName)      // First
    _ = cli.DeleteMetadataStore(meta)   // Second
    _ = cli.DeletePayloadStore(payload) // Third
})
```
**Warning signs:** "store in use" errors during cleanup

### Pitfall 3: Permission Grant on Non-Existent User/Group
**What goes wrong:** Grant fails with "not found" error
**Why it happens:** User or group must exist before granting permission
**How to avoid:** Always create user/group before granting permissions
**Warning signs:** "User not found" or "Group not found" errors

### Pitfall 4: Expecting Soft Delete Behavior (SHR-05/SHR-06)
**What goes wrong:** Tests fail because soft delete isn't implemented
**Why it happens:** Current implementation does hard delete (removes from DB)
**How to avoid:** Test current behavior (hard delete) and document gap for implementation phase
**Warning signs:** Share truly deleted from database, no "deleted" flag

### Pitfall 5: Permission Level Case Sensitivity
**What goes wrong:** Permission level not recognized
**Why it happens:** Levels are case-sensitive ("read-write" not "Read-Write")
**How to avoid:** Use lowercase: "none", "read", "read-write", "admin"
**Warning signs:** "Invalid permission level" errors

### Pitfall 6: Multiple Stores Reused Across Parallel Tests
**What goes wrong:** Tests conflict on shared store names
**Why it happens:** Store names are global, parallel tests may collide
**How to avoid:** Create stores per test or use test-suite level shared stores with unique names
**Warning signs:** "store already exists" errors, unexpected store state

### Pitfall 7: Testing Permission Override Behavior
**What goes wrong:** Unexpected permission level after multiple grants
**Why it happens:** Each grant overwrites previous permission (not additive)
**How to avoid:** Understand that grant replaces, not augments
**Warning signs:** User has only last-granted permission level

## Code Examples

Verified patterns from existing codebase:

### Full Share CRUD Test Suite Structure
```go
//go:build e2e

package e2e

import (
    "strings"
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// TestSharesCRUD tests comprehensive share management via CLI.
// Covers SHR-01 through SHR-04. SHR-05/SHR-06 (soft delete) deferred.
func TestSharesCRUD(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping shares tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    // Shared stores for all share tests
    metaStore := helpers.UniqueTestName("meta_shares")
    payloadStore := helpers.UniqueTestName("payload_shares")
    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)
    t.Cleanup(func() {
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    // SHR-01: Create share with assigned stores via CLI
    t.Run("SHR-01 create share with assigned stores", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_shr01")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        share, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err, "Should create share with stores")

        assert.Equal(t, shareName, share.Name)
        assert.Equal(t, metaStore, share.MetadataStoreID)
        assert.Equal(t, payloadStore, share.PayloadStoreID)
    })

    // SHR-02: List shares via CLI
    t.Run("SHR-02 list shares", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_shr02")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        shares, err := cli.ListShares()
        require.NoError(t, err, "Should list shares")

        var found bool
        for _, s := range shares {
            if s.Name == shareName {
                found = true
                break
            }
        }
        assert.True(t, found, "Should find created share in list")
    })

    // SHR-03: Edit share configuration via CLI
    t.Run("SHR-03 edit share configuration", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_shr03")
        t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        updated, err := cli.EditShare(shareName,
            helpers.WithShareReadOnly(true),
            helpers.WithShareDefaultPermission("admin"),
        )
        require.NoError(t, err, "Should edit share")

        assert.True(t, updated.ReadOnly)
        assert.Equal(t, "admin", updated.DefaultPermission)
    })

    // SHR-04: Delete share via CLI
    t.Run("SHR-04 delete share", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_shr04")

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)

        err = cli.DeleteShare(shareName)
        require.NoError(t, err, "Should delete share")

        _, err = cli.GetShare(shareName)
        require.Error(t, err, "Share should not exist after deletion")
    })

    // SHR-05: Soft delete - DEFERRED (not implemented)
    // SHR-06: Deferred cleanup - DEFERRED (not implemented)
}
```

### Full Permissions Test Suite Structure
```go
//go:build e2e

package e2e

import (
    "testing"

    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// TestSharePermissions tests permission management via CLI.
// Covers PRM-01 through PRM-07.
func TestSharePermissions(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping permission tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    serverURL := sp.APIURL()
    cli := helpers.LoginAsAdmin(t, serverURL)

    // Shared stores for all permission tests
    metaStore := helpers.UniqueTestName("meta_perms")
    payloadStore := helpers.UniqueTestName("payload_perms")
    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)
    t.Cleanup(func() {
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    // PRM-01: Grant read permission to user via CLI
    t.Run("PRM-01 grant read to user", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm01")
        username := helpers.UniqueTestName("user_prm01")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        err = cli.GrantUserPermission(shareName, username, "read")
        require.NoError(t, err, "Should grant read permission to user")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "user", username, "read")
        assert.True(t, found, "Should find user read permission")
    })

    // PRM-02: Grant read-write permission to user via CLI
    t.Run("PRM-02 grant read-write to user", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm02")
        username := helpers.UniqueTestName("user_prm02")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        err = cli.GrantUserPermission(shareName, username, "read-write")
        require.NoError(t, err, "Should grant read-write permission to user")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "user", username, "read-write")
        assert.True(t, found, "Should find user read-write permission")
    })

    // PRM-03: Grant read permission to group via CLI
    t.Run("PRM-03 grant read to group", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm03")
        groupName := helpers.UniqueTestName("group_prm03")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read")
        require.NoError(t, err, "Should grant read permission to group")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "group", groupName, "read")
        assert.True(t, found, "Should find group read permission")
    })

    // PRM-04: Grant read-write permission to group via CLI
    t.Run("PRM-04 grant read-write to group", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm04")
        groupName := helpers.UniqueTestName("group_prm04")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read-write")
        require.NoError(t, err, "Should grant read-write permission to group")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "group", groupName, "read-write")
        assert.True(t, found, "Should find group read-write permission")
    })

    // PRM-05: Revoke permission from user via CLI
    t.Run("PRM-05 revoke from user", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm05")
        username := helpers.UniqueTestName("user_prm05")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)

        // Grant then revoke
        err = cli.GrantUserPermission(shareName, username, "read-write")
        require.NoError(t, err)

        err = cli.RevokeUserPermission(shareName, username)
        require.NoError(t, err, "Should revoke user permission")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "user", username, "")
        assert.False(t, found, "User permission should be revoked")
    })

    // PRM-06: Revoke permission from group via CLI
    t.Run("PRM-06 revoke from group", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm06")
        groupName := helpers.UniqueTestName("group_prm06")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantGroupPermission(shareName, groupName, "read")
        require.NoError(t, err)

        err = cli.RevokeGroupPermission(shareName, groupName)
        require.NoError(t, err, "Should revoke group permission")

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err)

        found := findPermission(perms, "group", groupName, "")
        assert.False(t, found, "Group permission should be revoked")
    })

    // PRM-07: List permissions for share via CLI
    t.Run("PRM-07 list permissions", func(t *testing.T) {
        t.Parallel()

        shareName := "/" + helpers.UniqueTestName("share_prm07")
        username := helpers.UniqueTestName("user_prm07")
        groupName := helpers.UniqueTestName("group_prm07")
        t.Cleanup(func() {
            _ = cli.DeleteShare(shareName)
            _ = cli.DeleteUser(username)
            _ = cli.DeleteGroup(groupName)
        })

        _, err := cli.CreateShare(shareName, metaStore, payloadStore)
        require.NoError(t, err)
        _, err = cli.CreateUser(username, "TestPassword123!")
        require.NoError(t, err)
        _, err = cli.CreateGroup(groupName)
        require.NoError(t, err)

        err = cli.GrantUserPermission(shareName, username, "read-write")
        require.NoError(t, err)
        err = cli.GrantGroupPermission(shareName, groupName, "read")
        require.NoError(t, err)

        perms, err := cli.ListSharePermissions(shareName)
        require.NoError(t, err, "Should list permissions")

        assert.Len(t, perms, 2, "Should have 2 permissions")

        foundUser := findPermission(perms, "user", username, "read-write")
        foundGroup := findPermission(perms, "group", groupName, "read")
        assert.True(t, foundUser, "Should find user permission")
        assert.True(t, foundGroup, "Should find group permission")
    })
}

// findPermission is a helper to search for a permission in the list.
// If level is empty, it matches any level for that type/name combination.
func findPermission(perms []*helpers.SharePermission, permType, name, level string) bool {
    for _, p := range perms {
        if p.Type == permType && p.Name == name {
            if level == "" || p.Level == level {
                return true
            }
        }
    }
    return false
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Direct API testing | CLI subprocess execution | Phase 2 decision | Tests actual user experience |
| Minimal share helpers | Full CRUD with options | Phase 4 expansion | Complete coverage |
| Hard delete only | Soft delete (future) | SHR-05/SHR-06 requirement | Deferred implementation |

**Deprecated/outdated:**
- Phase 3 minimal `CreateShare(name, meta, payload)` - Replace with full `CreateShare(...opts)` pattern

## Open Questions

Things that couldn't be fully resolved:

1. **Soft Delete Implementation (SHR-05/SHR-06)**
   - What we know: Requirements specify soft delete with deferred cleanup
   - What's unclear: Current implementation does hard delete; soft delete not yet implemented
   - Recommendation: Test current (hard delete) behavior; defer SHR-05/SHR-06 tests until implementation exists

2. **Permission Override vs. Augment**
   - What we know: CLI grant command sets permission level
   - What's unclear: Does granting "read-write" to user who has "read" replace or augment?
   - Recommendation: Verify with test; likely replaces (based on SetUserSharePermission implementation)

3. **Share Get Command**
   - What we know: No dedicated `share get <name>` command in CLI
   - What's unclear: Should one be added, or is list+filter acceptable?
   - Recommendation: Use list+filter pattern (consistent with Phase 3 store get)

4. **Permission Level "none" vs. Revoke**
   - What we know: "none" is a valid permission level
   - What's unclear: Is grant --level none equivalent to revoke?
   - Recommendation: Test both approaches; document behavior

## Sources

### Primary (HIGH confidence)
- Existing codebase: `cmd/dittofsctl/commands/share/*.go` - CLI command patterns
- Existing codebase: `cmd/dittofsctl/commands/share/permission/*.go` - Permission CLI
- Existing codebase: `pkg/apiclient/shares.go` - API client types and methods
- Existing codebase: `internal/controlplane/api/handlers/shares.go` - API handlers
- Existing codebase: `pkg/controlplane/store/shares.go` - Share CRUD operations
- Existing codebase: `pkg/controlplane/store/permissions.go` - Permission operations
- Existing codebase: `pkg/controlplane/models/permission.go` - Permission types
- Existing codebase: `test/e2e/helpers/cli.go` - Existing CLIRunner with minimal share helpers
- Existing codebase: `test/e2e/metadata_stores_test.go` - Phase 3 test patterns

### Secondary (MEDIUM confidence)
- Phase 2-3 research: `.planning/phases/02-server-identity/02-RESEARCH.md` - Test patterns
- Phase 3 research: `.planning/phases/03-stores/03-RESEARCH.md` - Store test patterns
- STATE.md: `.planning/STATE.md` - Project decisions and patterns

### Tertiary (LOW confidence)
- Requirements: `.planning/REQUIREMENTS.md` - SHR-05/SHR-06 soft delete requirements
- Requirements specify soft delete but implementation is hard delete

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in use in codebase
- Architecture: HIGH - Patterns derived from existing Phase 2-3 test infrastructure
- Share CRUD: HIGH - CLI commands and API handlers exist and are tested
- Permission management: HIGH - CLI commands and API handlers fully implemented
- Soft delete (SHR-05/SHR-06): LOW - Not implemented, tests will fail or need to be deferred

**Research date:** 2026-02-02
**Valid until:** 60 days (patterns are stable; soft delete implementation may change this)
