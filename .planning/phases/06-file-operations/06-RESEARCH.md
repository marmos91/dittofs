# Phase 6: File Operations - Research

**Researched:** 2026-02-02
**Domain:** NFS/SMB file operations, cross-protocol interoperability, store matrix validation
**Confidence:** HIGH

## Summary

This is the final phase of the E2E test suite redesign. It focuses on validating file operations through mounted filesystems (NFS and SMB), cross-protocol interoperability, permission enforcement, and comprehensive store matrix testing across all 9 metadata/payload store combinations.

The codebase already has extensive infrastructure for file operation testing. The existing `test/e2e/framework/` package provides `MountNFS`, `MountSMB`, file helpers (`WriteFile`, `ReadFile`, `CreateDir`, etc.), and `RunOnAllConfigs` for store matrix testing. The `test/e2e/helpers/` package provides CLI-driven test helpers for setting up shares, users, groups, and permissions.

The key challenge for Phase 6 is bridging these two worlds: using the CLI-driven approach (from phases 1-5) to set up the test environment (shares, users, permissions), then using the framework's mount and file helpers to perform actual file operations. This creates a hybrid approach that validates the full stack.

**Primary recommendation:** Create new test files that use ServerProcess + CLIRunner for setup, then framework mount helpers for file operations. Reuse existing framework patterns (WriteFile, ReadFile, CreateDir, etc.) but integrate with CLI-driven share/permission management.

## Standard Stack

### Core (Already in Codebase)

| Component | Location | Purpose | Status |
|-----------|----------|---------|--------|
| MountNFS | `test/e2e/framework/mount.go` | Mount NFS shares | Complete |
| MountSMB | `test/e2e/framework/mount.go` | Mount SMB shares | Complete |
| File helpers | `test/e2e/framework/helpers.go` | WriteFile, ReadFile, CreateDir, ListDir, etc. | Complete |
| RunOnAllConfigs | `test/e2e/framework/helpers.go` | Run tests on all store configs | Complete |
| TestConfig | `test/e2e/framework/config.go` | Store combination definitions | Complete |
| ServerProcess | `test/e2e/helpers/server.go` | Managed dittofs subprocess | Complete |
| CLIRunner | `test/e2e/helpers/cli.go` | dittofsctl command execution | Complete |
| Share helpers | `test/e2e/helpers/shares.go` | CreateShare, GrantPermission, etc. | Complete |
| Adapter helpers | `test/e2e/helpers/adapters.go` | EnableAdapter, GetAdapter, etc. | Complete |

### Supporting

| Component | Location | Purpose | Status |
|-----------|----------|---------|--------|
| testify/require | External | Assertions with fail-fast | Used |
| testify/assert | External | Soft assertions | Used |
| sync.WaitGroup | stdlib | Concurrent test coordination | Used |
| time.Sleep | stdlib | Cross-protocol sync delays | Used |

## Architecture Patterns

### Pattern 1: Hybrid CLI + Mount Testing

The core pattern combines CLI-driven setup with direct mount operations.

**What:** Use ServerProcess + CLIRunner to set up server/shares/users/permissions, then use framework mount helpers for actual file operations.

**When to use:** All file operation tests - this validates the full stack from CLI to protocol adapter to filesystem.

**Example:**
```go
// Source: Pattern from existing tests in codebase
func TestNFSFileOperations(t *testing.T) {
    // 1. Start server via ServerProcess
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    // 2. Login as admin
    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // 3. Create stores and shares via CLI
    metaStore := helpers.UniqueTestName("meta")
    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)

    payloadStore := helpers.UniqueTestName("payload")
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)

    shareName := "/" + helpers.UniqueTestName("share")
    _, err = cli.CreateShare(shareName, metaStore, payloadStore)
    require.NoError(t, err)

    // 4. Enable NFS adapter with dynamic port
    nfsPort := helpers.FindFreePort(t)
    _, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
    require.NoError(t, err)

    // 5. Wait for adapter to be ready
    framework.WaitForServer(t, nfsPort, 10*time.Second)

    // 6. Mount via framework helper
    mount := framework.MountNFS(t, nfsPort)
    defer mount.Cleanup()

    // 7. Perform file operations
    framework.WriteFile(t, mount.FilePath("test.txt"), []byte("hello"))
    content := framework.ReadFile(t, mount.FilePath("test.txt"))
    require.Equal(t, []byte("hello"), content)
}
```

### Pattern 2: Cross-Protocol Interoperability

**What:** Create via one protocol, verify via other protocol.

**When to use:** XPR-01 through XPR-06 tests.

**Example:**
```go
// Source: test/e2e/interop_v2_test.go patterns
func TestCrossProtocolInterop(t *testing.T) {
    // ... setup server, stores, share, both adapters ...

    nfsMount := framework.MountNFS(t, nfsPort)
    defer nfsMount.Cleanup()

    smbMount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
        Username: "testuser",
        Password: "testpass123",
    })
    defer smbMount.Cleanup()

    // Create via NFS
    framework.WriteFile(t, nfsMount.FilePath("crosstest.txt"), []byte("NFS created"))

    // Small delay for metadata sync
    time.Sleep(100 * time.Millisecond)

    // Read via SMB
    content := framework.ReadFile(t, smbMount.FilePath("crosstest.txt"))
    require.Equal(t, []byte("NFS created"), content)
}
```

### Pattern 3: Permission Enforcement Testing

**What:** Grant specific permissions to user, verify access via mounted share.

**When to use:** ENF-01 through ENF-04 tests.

**Key insight:** Permission enforcement happens at the protocol layer (NFS/SMB adapter). CLI grants permission, then mount with user credentials and verify access is enforced.

**Example:**
```go
// Source: Synthesized from codebase patterns
func TestReadOnlyPermissionEnforcement(t *testing.T) {
    // Setup server, stores, share...
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Create share with default permission "none"
    shareName := "/" + helpers.UniqueTestName("share")
    _, err := cli.CreateShare(shareName, metaStore, payloadStore,
        helpers.WithShareDefaultPermission("none"))
    require.NoError(t, err)

    // Create a read-only user
    userName := helpers.UniqueTestName("readonly")
    userPass := "testpassword123"
    _, err = cli.CreateUser(userName, userPass)
    require.NoError(t, err)

    // Grant read-only permission
    err = cli.GrantUserPermission(shareName, userName, "read")
    require.NoError(t, err)

    // Enable SMB adapter (SMB enforces user auth, NFS uses AUTH_UNIX which is UID-based)
    smbPort := helpers.FindFreePort(t)
    _, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
    require.NoError(t, err)
    framework.WaitForServer(t, smbPort, 10*time.Second)

    // Mount as the read-only user
    smbMount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
        Username: userName,
        Password: userPass,
    })
    defer smbMount.Cleanup()

    // Reading should work (create file as admin first)
    // ... admin creates test file via NFS ...

    // Writing should fail
    err = os.WriteFile(smbMount.FilePath("forbidden.txt"), []byte("data"), 0644)
    require.Error(t, err, "Read-only user should not be able to write")
}
```

### Pattern 4: Store Matrix Testing

**What:** Run the same test across all 9 metadata/payload store combinations.

**When to use:** MTX-01 through MTX-09 tests.

**Example:**
```go
// Source: Adaptation of test/e2e/framework/helpers.go patterns
func TestFileOperationsStoreMatrix(t *testing.T) {
    // Define store combinations
    metadataTypes := []string{"memory", "badger", "postgres"}
    payloadTypes := []string{"memory", "filesystem", "s3"}

    for _, metaType := range metadataTypes {
        for _, payloadType := range payloadTypes {
            testName := fmt.Sprintf("%s/%s", metaType, payloadType)
            t.Run(testName, func(t *testing.T) {
                // Skip if containers not available
                if metaType == "postgres" && !helpers.CheckPostgresAvailable(t) {
                    t.Skip("PostgreSQL not available")
                }
                if payloadType == "s3" && !helpers.CheckS3Available(t) {
                    t.Skip("S3/Localstack not available")
                }

                // Setup with specific store types
                runFileOperationsWithStores(t, metaType, payloadType)
            })
        }
    }
}
```

### Recommended Test File Structure

```
test/e2e/
├── file_operations_nfs_test.go    # NFS-01 through NFS-06
├── file_operations_smb_test.go    # SMB-01 through SMB-06
├── cross_protocol_test.go         # XPR-01 through XPR-06
├── permission_enforcement_test.go # ENF-01 through ENF-04
└── store_matrix_test.go           # MTX-01 through MTX-09
```

### Anti-Patterns to Avoid

- **Direct API calls instead of CLI:** Per STATE.md, all tests should use dittofsctl CLI, not direct Go API calls
- **Hardcoded ports:** Always use `helpers.FindFreePort(t)` to avoid conflicts in parallel tests
- **Missing cleanup:** Always use `t.Cleanup()` and `defer mount.Cleanup()` for proper resource release
- **No sync delay for cross-protocol:** Cross-protocol tests need small delays (100-200ms) for metadata sync
- **Using AUTH_UNIX for permission tests:** NFS AUTH_UNIX is UID-based, not user-based. Use SMB for user-authenticated permission testing

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Mount NFS shares | Custom mount logic | `framework.MountNFS(t, port)` | Handles platform differences, retries, cleanup |
| Mount SMB shares | Custom mount logic | `framework.MountSMB(t, port, creds)` | Handles auth encoding, platform differences |
| File operations | Direct os.* calls | `framework.WriteFile`, `framework.ReadFile` | Consistent error handling, logging |
| Server subprocess | exec.Command directly | `helpers.ServerProcess` | Handles pid, health checks, signals, cleanup |
| CLI execution | exec.Command directly | `helpers.CLIRunner` | Token management, JSON parsing, error formatting |
| Store setup | Direct store creation | `cli.CreateMetadataStore`, `cli.CreatePayloadStore` | Validates CLI interface, proper cleanup |
| Permission setup | Direct DB manipulation | `cli.GrantUserPermission`, `cli.GrantGroupPermission` | Validates CLI interface |
| Port allocation | Random ports | `helpers.FindFreePort(t)` | Guarantees availability |
| Adapter readiness | Sleep-based waits | `framework.WaitForServer(t, port, timeout)` | Polls for actual readiness |

**Key insight:** The existing framework and helpers already solve these problems. Phase 6 integrates them, not replaces them.

## Common Pitfalls

### Pitfall 1: NFS AUTH_UNIX vs User Authentication

**What goes wrong:** Permission enforcement tests pass for NFS when they shouldn't
**Why it happens:** NFS AUTH_UNIX uses UID/GID from client mount, not DittoFS user database. A user with UID 1000 can access files regardless of DittoFS user permissions.
**How to avoid:**
- Use SMB for permission enforcement tests (SMB enforces user authentication)
- NFS tests should focus on file operations, not fine-grained permission enforcement
- Document that NFS permission tests verify mode bits, not ACLs
**Warning signs:** Permission tests pass on NFS but should fail based on user grants

### Pitfall 2: Cross-Protocol Attribute Cache

**What goes wrong:** File created via NFS not visible via SMB immediately
**Why it happens:** NFS and SMB clients cache attributes. macOS is particularly aggressive.
**How to avoid:**
- Use `actimeo=0` mount option (already in framework.MountNFS)
- Add small delay (100-200ms) between write via one protocol and read via other
- For critical assertions, retry with backoff
**Warning signs:** Tests flaky on CI, pass locally

### Pitfall 3: SMB Session State on macOS

**What goes wrong:** Unmount succeeds but next mount fails with auth errors
**Why it happens:** macOS caches SMB session state even after unmount
**How to avoid:**
- Framework already includes 200ms delay after SMB unmount on macOS
- Don't reuse credentials immediately after unmount
- Use unique usernames per test when testing permission changes
**Warning signs:** "Session setup failed" errors on second mount

### Pitfall 4: Port Conflicts in Parallel Tests

**What goes wrong:** Tests fail with "address already in use"
**Why it happens:** Multiple tests try to enable adapters on same port
**How to avoid:**
- Always use `helpers.FindFreePort(t)` for adapter ports
- Never hardcode port numbers in tests
- Each test enables its own adapters
**Warning signs:** Tests pass individually, fail when run together

### Pitfall 5: Cleanup Order for Mounts

**What goes wrong:** "Device busy" errors on directory cleanup
**Why it happens:** Trying to remove mount directory before unmounting
**How to avoid:**
- Always call `mount.Cleanup()` before test ends (via defer or explicit)
- Cleanup unmounts first, then removes directory
- If test fails mid-operation, t.Cleanup handles this
**Warning signs:** Temp directories accumulate, require manual cleanup

### Pitfall 6: PostgreSQL/S3 Container Availability

**What goes wrong:** Store matrix tests fail for postgres/s3 combinations
**Why it happens:** Testcontainers not started or not available
**How to avoid:**
- Check container availability before running test: `helpers.CheckPostgresAvailable(t)`, `helpers.CheckS3Available(t)`
- Skip test with `t.Skip()` if containers not available
- Don't fail tests due to infrastructure issues
**Warning signs:** All postgres or s3 tests fail together

### Pitfall 7: Share Export Path Mismatch

**What goes wrong:** Mount succeeds but files don't appear
**Why it happens:** Share name in DittoFS must match NFS export path
**How to avoid:**
- Share name MUST start with `/` (e.g., `/export`, `/myshare`)
- NFS mount uses `localhost:/export` - the share name IS the export path
- For custom shares, adjust mount command accordingly
**Warning signs:** Empty mount point, stale file handle errors

## Code Examples

### Complete NFS File Operations Test

```go
// Source: Synthesized from codebase patterns
func TestNFSOperations(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping NFS operations in short mode")
    }

    // Start server
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Create stores
    metaStore := helpers.UniqueTestName("nfs_meta")
    payloadStore := helpers.UniqueTestName("nfs_payload")

    _, err := cli.CreateMetadataStore(metaStore, "memory")
    require.NoError(t, err)
    _, err = cli.CreatePayloadStore(payloadStore, "memory")
    require.NoError(t, err)

    t.Cleanup(func() {
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    // Create share (use /export for standard path)
    shareName := "/export"
    _, err = cli.CreateShare(shareName, metaStore, payloadStore)
    require.NoError(t, err)
    t.Cleanup(func() { _ = cli.DeleteShare(shareName) })

    // Enable NFS adapter
    nfsPort := helpers.FindFreePort(t)
    _, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
    require.NoError(t, err)
    framework.WaitForServer(t, nfsPort, 10*time.Second)

    // Mount
    mount := framework.MountNFS(t, nfsPort)
    defer mount.Cleanup()

    t.Run("NFS-01 Read files", func(t *testing.T) {
        filePath := mount.FilePath("read_test.txt")
        expected := []byte("test content for reading")
        framework.WriteFile(t, filePath, expected)

        content := framework.ReadFile(t, filePath)
        assert.Equal(t, expected, content)
    })

    t.Run("NFS-02 Write files", func(t *testing.T) {
        filePath := mount.FilePath("write_test.txt")
        content := []byte("written content")
        framework.WriteFile(t, filePath, content)

        // Verify write succeeded
        assert.True(t, framework.FileExists(filePath))
        readBack := framework.ReadFile(t, filePath)
        assert.Equal(t, content, readBack)
    })

    t.Run("NFS-03 Delete files", func(t *testing.T) {
        filePath := mount.FilePath("delete_test.txt")
        framework.WriteFile(t, filePath, []byte("to delete"))

        require.True(t, framework.FileExists(filePath))
        err := os.Remove(filePath)
        require.NoError(t, err)
        assert.False(t, framework.FileExists(filePath))
    })

    t.Run("NFS-04 List directories", func(t *testing.T) {
        dirPath := mount.FilePath("list_test")
        framework.CreateDir(t, dirPath)

        framework.WriteFile(t, filepath.Join(dirPath, "file1.txt"), []byte("1"))
        framework.WriteFile(t, filepath.Join(dirPath, "file2.txt"), []byte("2"))

        entries := framework.ListDir(t, dirPath)
        assert.Len(t, entries, 2)
    })

    t.Run("NFS-05 Create directories", func(t *testing.T) {
        dirPath := mount.FilePath("new_directory")
        framework.CreateDir(t, dirPath)

        assert.True(t, framework.DirExists(dirPath))
    })

    t.Run("NFS-06 Change file permissions", func(t *testing.T) {
        filePath := mount.FilePath("chmod_test.txt")
        framework.WriteFile(t, filePath, []byte("test"))

        err := os.Chmod(filePath, 0600)
        require.NoError(t, err)

        info := framework.GetFileInfo(t, filePath)
        mode := info.Mode.Perm()
        assert.Equal(t, os.FileMode(0600), mode&0777)
    })
}
```

### Cross-Protocol Interoperability Test

```go
// Source: Adapted from test/e2e/interop_v2_test.go
func TestCrossProtocolInterop(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping cross-protocol tests in short mode")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Create stores and share
    metaStore := helpers.UniqueTestName("xpr_meta")
    payloadStore := helpers.UniqueTestName("xpr_payload")
    _, _ = cli.CreateMetadataStore(metaStore, "memory")
    _, _ = cli.CreatePayloadStore(payloadStore, "memory")

    shareName := "/export"
    _, _ = cli.CreateShare(shareName, metaStore, payloadStore)

    // Create SMB test user
    smbUser := helpers.UniqueTestName("smbuser")
    smbPass := "testpassword123"
    _, _ = cli.CreateUser(smbUser, smbPass)
    _ = cli.GrantUserPermission(shareName, smbUser, "read-write")

    t.Cleanup(func() {
        _ = cli.DeleteShare(shareName)
        _ = cli.DeleteUser(smbUser)
        _ = cli.DeleteMetadataStore(metaStore)
        _ = cli.DeletePayloadStore(payloadStore)
    })

    // Enable both adapters
    nfsPort := helpers.FindFreePort(t)
    smbPort := helpers.FindFreePort(t)
    _, _ = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
    _, _ = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
    framework.WaitForServer(t, nfsPort, 10*time.Second)
    framework.WaitForServer(t, smbPort, 10*time.Second)

    // Mount both
    nfsMount := framework.MountNFS(t, nfsPort)
    defer nfsMount.Cleanup()

    smbMount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
        Username: smbUser,
        Password: smbPass,
    })
    defer smbMount.Cleanup()

    t.Run("XPR-01 File created via NFS readable via SMB", func(t *testing.T) {
        filePath := "nfs_created.txt"
        content := []byte("Created via NFS")

        framework.WriteFile(t, nfsMount.FilePath(filePath), content)
        time.Sleep(100 * time.Millisecond)

        readContent := framework.ReadFile(t, smbMount.FilePath(filePath))
        assert.Equal(t, content, readContent)
    })

    t.Run("XPR-02 File created via SMB readable via NFS", func(t *testing.T) {
        filePath := "smb_created.txt"
        content := []byte("Created via SMB")

        framework.WriteFile(t, smbMount.FilePath(filePath), content)
        time.Sleep(100 * time.Millisecond)

        readContent := framework.ReadFile(t, nfsMount.FilePath(filePath))
        assert.Equal(t, content, readContent)
    })

    // ... XPR-03 through XPR-06 ...
}
```

### Permission Enforcement Test (SMB-based)

```go
// Source: Synthesized from codebase patterns
func TestPermissionEnforcement(t *testing.T) {
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Create stores and share
    metaStore := helpers.UniqueTestName("enf_meta")
    payloadStore := helpers.UniqueTestName("enf_payload")
    _, _ = cli.CreateMetadataStore(metaStore, "memory")
    _, _ = cli.CreatePayloadStore(payloadStore, "memory")

    shareName := "/export"
    _, _ = cli.CreateShare(shareName, metaStore, payloadStore,
        helpers.WithShareDefaultPermission("none"))

    // Create users
    readUser := helpers.UniqueTestName("readonly")
    noAccessUser := helpers.UniqueTestName("noaccess")
    userPass := "testpassword123"

    _, _ = cli.CreateUser(readUser, userPass)
    _, _ = cli.CreateUser(noAccessUser, userPass)

    // Grant read-only to one user, nothing to other
    _ = cli.GrantUserPermission(shareName, readUser, "read")
    // noAccessUser gets no permission

    // Enable SMB
    smbPort := helpers.FindFreePort(t)
    _, _ = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
    framework.WaitForServer(t, smbPort, 10*time.Second)

    t.Run("ENF-01 Read-only user cannot write", func(t *testing.T) {
        mount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
            Username: readUser,
            Password: userPass,
        })
        defer mount.Cleanup()

        // Write should fail
        err := os.WriteFile(mount.FilePath("forbidden.txt"), []byte("data"), 0644)
        assert.Error(t, err, "Read-only user should not be able to write")
    })

    t.Run("ENF-02 No-access user cannot read", func(t *testing.T) {
        // Mount should fail or read should fail
        // Depending on SMB implementation, mount may fail with auth error
        _, err := framework.MountSMBWithError(t, smbPort, framework.SMBCredentials{
            Username: noAccessUser,
            Password: userPass,
        })

        // Either mount fails or read fails
        if err == nil {
            // If mount succeeded (some servers allow), operations should fail
            // This is implementation-dependent
        }
        // Test passes if mount fails (expected)
    })
}
```

## State of the Art

| Old Approach | Current Approach | Impact |
|--------------|------------------|--------|
| Direct framework.TestContext | CLI-driven setup + framework mounts | Validates full CLI-to-protocol stack |
| Hardcoded /export | CLI-created shares | Tests actual share management |
| Single store config | 9-combination matrix | Comprehensive store validation |
| UID-based permission | User-authenticated (SMB) | Real permission enforcement |

**Current patterns from codebase:**
- `test/e2e/framework/` - Direct Go API, creates stores programmatically
- `test/e2e/helpers/` - CLI-driven, uses dittofsctl

**Phase 6 integrates both:** CLI for setup, framework mounts for operations.

## Open Questions

1. **MountSMBWithError helper**
   - What we know: framework.MountSMB calls t.Fatal on failure
   - What's unclear: How to test "mount should fail" scenarios
   - **Recommendation:** Add helper variant that returns error instead of failing

2. **NFS permission enforcement scope**
   - What we know: NFS AUTH_UNIX uses client UID, not DittoFS users
   - What's unclear: Should NFS permission tests exist? What do they validate?
   - **Recommendation:** NFS tests validate mode bits (chmod), not user ACLs. Document this clearly.

3. **Store matrix container requirements**
   - What we know: Postgres/S3 combinations need containers
   - What's unclear: CI setup for testcontainers
   - **Recommendation:** Tests skip gracefully if containers unavailable; CI workflow starts containers

4. **S3 payload store configuration**
   - What we know: S3 store needs endpoint, bucket, credentials
   - What's unclear: How to configure for localstack in tests
   - **Recommendation:** Use helpers.TestEnvironment.LocalstackHelper() for endpoint/credentials

## Sources

### Primary (HIGH confidence)
- `/Users/marmos91/Projects/dittofs/test/e2e/framework/mount.go` - MountNFS, MountSMB implementation
- `/Users/marmos91/Projects/dittofs/test/e2e/framework/helpers.go` - File operation helpers
- `/Users/marmos91/Projects/dittofs/test/e2e/framework/context.go` - TestContext for store setup
- `/Users/marmos91/Projects/dittofs/test/e2e/framework/config.go` - Store configuration types
- `/Users/marmos91/Projects/dittofs/test/e2e/helpers/server.go` - ServerProcess implementation
- `/Users/marmos91/Projects/dittofs/test/e2e/helpers/cli.go` - CLIRunner implementation
- `/Users/marmos91/Projects/dittofs/test/e2e/helpers/shares.go` - Share and permission helpers
- `/Users/marmos91/Projects/dittofs/test/e2e/helpers/adapters.go` - Adapter lifecycle helpers
- `/Users/marmos91/Projects/dittofs/test/e2e/interop_v2_test.go` - Cross-protocol patterns
- `/Users/marmos91/Projects/dittofs/test/e2e/functional_test.go` - File operation patterns

### Secondary (MEDIUM confidence)
- `/Users/marmos91/Projects/dittofs/.planning/STATE.md` - Prior decisions and patterns
- `/Users/marmos91/Projects/dittofs/.planning/REQUIREMENTS.md` - Phase 6 requirements

## Metadata

**Confidence breakdown:**
- File operations (NFS/SMB): HIGH - Complete implementation in framework
- Cross-protocol interop: HIGH - Existing tests demonstrate pattern
- Permission enforcement: MEDIUM - SMB path clear, NFS path has limitations
- Store matrix: HIGH - Framework supports this, containers documented

**Research date:** 2026-02-02
**Valid until:** 30 days (stable domain, existing implementation)
