# Phase 15: v2.0 Testing - Research

**Researched:** 2026-02-17
**Domain:** Go E2E testing for NFSv3/NFSv4 filesystem protocol, Kerberos authentication, NFSv4 locking/delegations/ACLs, POSIX compliance
**Confidence:** HIGH

## Summary

Phase 15 is a comprehensive E2E testing phase that validates all NFSv4.0 functionality built across Phases 6-14. The existing test infrastructure is mature and well-structured: `test/e2e/framework/` provides mount helpers, lock helpers, Kerberos KDC testcontainer, Localstack/PostgreSQL testcontainers, and file operation utilities; `test/e2e/helpers/` provides CLI runner (dfsctl), server process management, store/share/adapter CRUD, and per-test scoping (unique Postgres schemas and S3 prefixes). The existing v1.0 tests cover NFS file operations, store matrix (9 combinations), cross-protocol locking, Kerberos authentication (v3 and v4), control plane v2.0 settings, grace period recovery, and more.

The key technical challenge is extending the existing NFSv3-only `MountNFS()` helper to also support NFSv4.0 mounts with `vers=4.0`, then running the full test matrix across both protocol versions. For NFSv4-specific features (locking, delegations, ACLs), tests must use the Linux kernel NFS client which handles stateful NFSv4 OPEN/CLOSE/LOCK/DELEGRETURN operations transparently. The `nfs4-acl-tools` package (`nfs4_setfacl`/`nfs4_getfacl`) is required for ACL E2E tests. The POSIX compliance suite (`test/posix/`) needs `--nfs-version` parameter support and a separate `known_failures_v4.txt` file per issue #122.

**Primary recommendation:** Extend `framework.MountNFS()` with a version parameter (`MountNFSWithVersion`), then use table-driven tests with `{v3, v4}` as a test dimension for all I/O tests. Feature-specific tests (locking, delegations, ACLs) use default backend (memory/memory) with NFSv4 mounts only.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions

#### Test Environment
- Docker is a required dependency for E2E tests (KDC, Localstack, PostgreSQL containers)
- KDC already exists: Docker testcontainer with MIT Kerberos in `test/e2e/framework/kerberos.go` -- reuse it
- Reuse existing Localstack setup from v1.0 E2E tests for S3 backend testing
- PostgreSQL testcontainer included for full persistent metadata path testing
- PostgreSQL uses GORM AutoMigrate (same as production) -- tests validate the real migration path
- Full storage backend matrix: all combinations of metadata (memory/badger/postgres) x payload (memory/filesystem/s3)
- Both single-share and multi-share configurations tested
- Concurrent multi-share mounts tested (two shares mounted simultaneously)
- Always clean up containers and mounts on test failure -- CI-friendly
- E2E tests run automatically in CI on every PR/push
- Global test timeout: 30 minutes
- Include server restart/recovery test (graceful shutdown + restart + state reclaim)
- Include client reconnection test after brief network disruption

#### NFSv4 Mount Strategy
- Full v3 + v4.0 matrix: every test runs against both NFSv3 and NFSv4.0 mounts
- Use explicit `vers=4.0` (not `vers=4`) for NFSv4 mounts -- no ambiguity
- Both macOS and Linux supported with graceful skipping (macOS skips NFSv4-specific features like delegations)
- Linux CI only -- macOS tested manually by developers
- Kerberos mounts tested with all three security flavors: `sec=krb5`, `sec=krb5i`, `sec=krb5p`
- Pseudo-filesystem browsing verified (ls on NFSv4 root shows pseudo-fs tree and share junctions)
- 30-second mount timeout
- Test squash behavior (root_squash, all_squash) at mount level
- Test stale NFS file handles (access after server restart with memory backend -> ESTALE)

#### Coverage Depth
- Full lock matrix: read/write locks, overlapping ranges, lock upgrade, blocking locks, cross-client conflicts
- Full delegation cycle: grant delegation, open conflicting client, verify recall, verify data flush -- multi-client test
- Reuse v1.0 file size matrix: 500KB, 1MB, 10MB, 100MB
- Full ACL lifecycle: set ACL via nfs4_setfacl, read back, verify access enforcement, test inheritance on new files
- Cross-protocol ACL interop: set ACL via NFSv4, mount same share via SMB, verify Security Descriptor reflects it
- Re-run full v1.0 E2E test suite for NFSv3 backward compatibility regression
- Full pjdfstest POSIX compliance against NFSv4 (issue #122): add `--nfs-version` parameter, create `known_failures_v4.txt`
- Full symlink behavior: create symlink, readlink, follow symlink to read content
- Full hard link behavior: create link, verify link count, delete original, verify data survives via link
- Kerberos authorization denial: mount with valid krb5 but unauthorized user, verify EACCES/EPERM
- Multi-client concurrency: two NFS mounts to same share, concurrent reads/writes, verify no corruption
- Full SETATTR: chmod, chown, truncate, touch (update timestamps) via mounted filesystem
- Full RENAME: within directory, across directories, overwrite existing target
- All OPEN create modes: UNCHECKED (allow overwrite), GUARDED (fail if exists), EXCLUSIVE (atomic create)
- Control plane v2.0 mount-level testing: change adapter settings via API, verify NFS behavior changes (blocked ops fail, netgroup denies access)
- Golden path smoke test: server start -> create user -> create share -> mount -> write/read
- Moderate directory test: ~100 files to verify READDIR pagination
- Stress tests gated behind `-tags=stress` build tag (e.g., 100+ files with delegations under load)
- Skip metrics endpoint verification (tested at unit level)
- E2E coverage profile generated (`-coverprofile`) for merge with unit test coverage

#### Test Organization
- NFSv4 E2E tests live in existing `test/e2e/` directory (same as v3 tests)
- Table-driven subtests with NFS version as parameter: `TestXxx/v3`, `TestXxx/v4`
- pjdfstest stays in separate `test/posix/` suite with `--nfs-version` parameter (per issue #122)
- Extend existing `test/e2e/helpers/` and `test/e2e/framework/` packages for NFSv4 helpers
- Standard `go test -v` output -- no JUnit XML or custom reporting
- Test files organized by feature area (not matching plan boundaries)
- `t.Parallel()` used where safe for faster CI execution
- Backend matrix: full cross-product for I/O tests (read/write/create); default backend for feature tests (locking, delegations, ACLs, Kerberos)

### Claude's Discretion
- Suite parallelism strategy (which test groups can run in parallel vs sequential)
- TestMain vs per-test server lifecycle
- Port testing (non-standard ports)
- Cross-share EXDEV error testing
- Compound sequence testing approach (implicit via kernel client vs explicit RPC client)
- AUTH_NULL policy rejection testing
- Kerberos fixture strategy (dynamic vs pre-created principals/keytabs)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `testing` | stdlib | Test framework | Go built-in, used everywhere in codebase |
| `github.com/stretchr/testify` | v1.9+ | Assertions (assert/require) | Already used in all existing e2e tests |
| `github.com/testcontainers/testcontainers-go` | v0.35+ | Docker containers (KDC, Localstack, Postgres) | Already used in framework/containers.go |
| `github.com/google/uuid` | v1.6+ | Unique test names | Already used in helpers/cli.go |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/aws/aws-sdk-go-v2` | v1.x | S3 client for Localstack | Payload store S3 testing |
| `github.com/jackc/pgx/v5` | v5.x | PostgreSQL client | Schema isolation per test |
| `nfs4-acl-tools` (system) | any | nfs4_setfacl/nfs4_getfacl | ACL E2E tests (Linux only) |
| `pjdfstest` (system) | Rust version | POSIX compliance testing | Issue #122 NFSv4 support |

### Alternatives Considered
None -- all libraries are already in use. No new dependencies needed.

**Installation:**
```bash
# No new Go dependencies needed -- all already in go.mod
# System packages needed for CI (Linux):
apt-get install -y nfs-common krb5-user nfs4-acl-tools
```

## Architecture Patterns

### Existing Test Infrastructure (Reuse)

```
test/e2e/
├── main_test.go              # TestMain: signal handling, container lifecycle
├── framework/                # Low-level test primitives
│   ├── mount.go              # MountNFS, MountSMB, Cleanup, FindFreePort
│   ├── helpers.go            # WriteRandomFile, VerifyFileChecksum, FileExists
│   ├── containers.go         # Localstack, PostgreSQL testcontainers (singletons)
│   ├── kerberos.go           # KDCHelper: testcontainer, principals, kinit
│   └── lock_helpers.go       # LockFile, TryLockFileRange, WaitForLockRelease
├── helpers/                  # High-level test orchestration
│   ├── server.go             # StartServerProcess, WaitReady, ForceKill
│   ├── cli.go                # CLIRunner (dfsctl), LoginAsAdmin, UniqueTestName
│   ├── environment.go        # TestEnvironment (container coordination)
│   ├── scope.go              # TestScope (per-test Postgres schema, S3 prefix)
│   ├── stores.go             # CreateMetadataStore, CreatePayloadStore
│   ├── shares.go             # CreateShare, GrantUserPermission
│   ├── adapters.go           # EnableAdapter, WaitForAdapterStatus
│   └── controlplane.go       # GetAPIClient, PatchNFSSetting, CreateNetgroup
└── *_test.go                 # Test files by feature area
```

### Pattern 1: NFSv4 Mount Helper (New)

**What:** Extend `framework.Mount` with NFSv4 support.
**When to use:** Every test that mounts an NFS share.

The existing `MountNFS()` hardcodes `nfsvers=3`. Add a `MountNFSWithVersion()` that accepts a version parameter:

```go
// framework/mount.go

// NFSVersion represents the NFS protocol version for mount options.
type NFSVersion int

const (
    NFSv3 NFSVersion = 3
    NFSv4 NFSVersion = 4
)

// MountNFSWithVersion mounts an NFS share with the specified protocol version.
// For NFSv4, uses vers=4.0 (explicit minor version) and different export syntax.
func MountNFSWithVersion(t *testing.T, port int, export string, version NFSVersion) *Mount {
    t.Helper()
    time.Sleep(500 * time.Millisecond)

    mountPath, err := os.MkdirTemp("", "dittofs-e2e-nfs-*")
    if err != nil {
        t.Fatalf("Failed to create NFS mount directory: %v", err)
    }

    var mountOptions string
    var source string
    switch version {
    case NFSv4:
        // NFSv4: vers=4.0, single port, pseudo-filesystem root
        mountOptions = fmt.Sprintf("vers=4.0,port=%d,actimeo=0", port)
        source = fmt.Sprintf("localhost:%s", export)
    default:
        // NFSv3: nfsvers=3, separate mountport
        mountOptions = fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)
        source = fmt.Sprintf("localhost:%s", export)
    }

    // Platform adjustments...
    switch runtime.GOOS {
    case "darwin":
        if version == NFSv3 {
            mountOptions += ",resvport"
        }
    case "linux":
        if version == NFSv3 {
            mountOptions += ",nolock"
        }
    }

    // Mount with retries (same as existing pattern)
    // ...
}
```

**Key difference for NFSv4:**
- Uses `vers=4.0` (not `vers=4` which allows negotiation to 4.1/4.2)
- No separate `mountport` (NFSv4 uses single port)
- No `nolock` (NFSv4 has built-in locking, no NLM needed)
- Export path is relative to pseudo-filesystem root

### Pattern 2: Version-Parameterized Table Tests

**What:** Run the same test logic against both NFSv3 and NFSv4.0 mounts.
**When to use:** I/O tests, file operation tests, store matrix tests.

```go
func TestFileOperations(t *testing.T) {
    versions := []framework.NFSVersion{framework.NFSv3, framework.NFSv4}

    for _, version := range versions {
        version := version
        t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
            // Setup server, stores, share...
            mount := framework.MountNFSWithVersion(t, nfsPort, "/export", version)
            t.Cleanup(mount.Cleanup)

            t.Run("ReadWrite", func(t *testing.T) { testReadWrite(t, mount) })
            t.Run("CreateDelete", func(t *testing.T) { testCreateDelete(t, mount) })
        })
    }
}
```

### Pattern 3: Per-Test Server Lifecycle (Recommended for Isolation)

**What:** Each top-level test starts its own DittoFS server process.
**When to use:** Most tests -- provides complete isolation.

The existing tests already follow this pattern: `StartServerProcess(t, "")` in each test function, with `t.Cleanup(sp.ForceKill)`. This is the correct approach because:
1. Tests can run in parallel across different server instances (different ports)
2. One test's state doesn't leak to another
3. Server restart tests need independent servers

**Recommendation:** Keep per-test server lifecycle for most tests. For the store matrix tests that run many sub-combinations, a single server per matrix entry is fine (already the existing pattern).

### Pattern 4: Feature Tests on Default Backend

**What:** NFSv4-specific feature tests (locking, delegations, ACLs) use memory/memory backend only.
**When to use:** Protocol feature tests where backend is irrelevant.

```go
func TestNFSv4Locking(t *testing.T) {
    // memory/memory is sufficient -- we're testing the protocol, not the storage
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    runner := helpers.LoginAsAdmin(t, sp.APIURL())
    // Create memory stores, share, enable NFS adapter...
    mount := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)
    // Test locking operations...
}
```

### Pattern 5: Multi-Client Delegation Testing

**What:** Two separate NFS mounts to the same share from the same machine, simulating two clients.
**When to use:** Delegation recall tests, concurrent access tests.

```go
func TestDelegationRecall(t *testing.T) {
    // Mount 1: "client A" opens file, gets delegation
    mountA := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)
    // Mount 2: "client B" opens same file, triggers delegation recall
    mountB := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)

    // Client A writes a file
    framework.WriteFile(t, mountA.FilePath("delegtest"), []byte("data"))

    // Open on client A to get delegation
    fA, err := os.Open(mountA.FilePath("delegtest"))
    require.NoError(t, err)
    defer fA.Close()

    // Client B opens same file -- should trigger CB_RECALL to client A
    fB, err := os.Open(mountB.FilePath("delegtest"))
    require.NoError(t, err)
    defer fB.Close()

    // Both should be able to read
    // (delegation is transparently handled by kernel client)
}
```

**Note:** The Linux kernel NFS client handles delegation grant/recall/return transparently. E2E tests verify the behavior (data consistency, no errors) rather than inspecting protocol-level delegation state.

### Pattern 6: ACL Testing with nfs4-acl-tools

**What:** Use `nfs4_setfacl` and `nfs4_getfacl` commands via exec.
**When to use:** NFSv4 ACL E2E tests.

```go
func setNFS4ACL(t *testing.T, path, aceSpec string) {
    t.Helper()
    cmd := exec.Command("nfs4_setfacl", "-a", aceSpec, path)
    output, err := cmd.CombinedOutput()
    require.NoError(t, err, "nfs4_setfacl failed: %s", string(output))
}

func getNFS4ACL(t *testing.T, path string) string {
    t.Helper()
    cmd := exec.Command("nfs4_getfacl", path)
    output, err := cmd.CombinedOutput()
    require.NoError(t, err, "nfs4_getfacl failed: %s", string(output))
    return string(output)
}

// Skip helper
func SkipIfNoNFS4ACLTools(t *testing.T) {
    t.Helper()
    if _, err := exec.LookPath("nfs4_setfacl"); err != nil {
        t.Skip("Skipping: nfs4-acl-tools not installed (nfs4_setfacl not found)")
    }
}
```

### Anti-Patterns to Avoid

- **Shared mutable state between parallel tests:** Each test must get its own server, stores, and mount points. Never share a mount between parallel subtests.
- **Hardcoded ports:** Always use `FindFreePort(t)` -- already standard in the codebase.
- **Testing protocol internals from E2E:** E2E tests should verify observable behavior (files appear, data matches, errors returned), not protocol-level state (stateid values, sequence numbers).
- **Flaky time-dependent assertions:** Use polling with timeout rather than fixed sleeps for state transitions (lock release, delegation recall, settings propagation).
- **Missing cleanup on failure path:** Always use `t.Cleanup()` rather than `defer` inside helper functions that may call `t.Fatal`. The existing codebase does this correctly.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Docker container management | Custom Docker exec | testcontainers-go (already used) | Handles port mapping, health checks, cleanup |
| Server process lifecycle | Custom process management | helpers.StartServerProcess (already built) | Handles build, start, health check, cleanup |
| NFS mount/unmount | Manual mount commands | framework.MountNFS* (extend existing) | Platform-specific, retry logic, stale mount cleanup |
| KDC/Kerberos setup | Manual KDC configuration | framework.KDCHelper (already built) | keytab generation, principal management, kinit |
| Test isolation (Postgres) | Manual database cleanup | helpers.TestScope (already built) | Per-test schema, automatic drop |
| Test isolation (S3) | Manual bucket cleanup | helpers.TestScope (already built) | Per-test prefix, automatic cleanup |
| ACL operations | Custom XDR encoding | nfs4_setfacl/nfs4_getfacl CLI tools | Standard Linux tools, validates real client path |
| File locking | Custom NFS RPC client | syscall.Flock/FcntlFlock via kernel mount | Tests real kernel NFS client behavior |

**Key insight:** The existing test infrastructure is comprehensive. Phase 15 primarily extends existing patterns (add NFSv4 mount support, add version parameter to tests) rather than building new infrastructure.

## Common Pitfalls

### Pitfall 1: NFSv4 Mount Requires Different Syntax
**What goes wrong:** Using `nfsvers=3` syntax for NFSv4 mounts causes mount failures.
**Why it happens:** NFSv4 uses `vers=4.0` (not `nfsvers=4`), has no separate mount protocol (no `mountport`), and uses a pseudo-filesystem root rather than direct export paths.
**How to avoid:** Create `MountNFSWithVersion()` that builds correct options per version. For NFSv4: `vers=4.0,port=PORT,actimeo=0`. No `nolock` needed (NFSv4 has built-in locking).
**Warning signs:** "mount: wrong fs type" or "mount: connection timed out" errors.

### Pitfall 2: NFSv4 Attribute Caching Masks Test Failures
**What goes wrong:** Tests pass on v3 but fail on v4 because NFSv4 clients cache more aggressively.
**Why it happens:** NFSv4 uses OPEN stateids and delegations for caching, which can mask stale data in cross-client tests.
**How to avoid:** Use `actimeo=0` mount option (already standard in codebase). For delegation tests, force recall by opening from a second mount point.
**Warning signs:** Stale data reads, "file not found" after creation on different mount.

### Pitfall 3: macOS Does Not Support NFSv4 Kernel Features
**What goes wrong:** Delegation, locking, and ACL tests fail on macOS.
**Why it happens:** macOS NFS client has limited NFSv4 support (no delegations, limited locking).
**How to avoid:** Add `SkipIfDarwin()` for NFSv4-specific feature tests. The context decision already specifies "macOS skips NFSv4-specific features like delegations."
**Warning signs:** Tests hang or return unexpected errors on macOS.

### Pitfall 4: Two NFSv4 Mounts From Same Machine Share Client ID
**What goes wrong:** Two mounts intended to simulate "two clients" share the same NFSv4 client ID because they come from the same kernel NFS client.
**Why it happens:** The Linux kernel NFS client uses one client ID per server address.
**How to avoid:** For delegation recall tests, use two mount points but understand they share a client ID. The kernel still handles delegation recall correctly because it tracks per-open-owner state. For true multi-client testing, use separate network namespaces or containers (stress test territory).
**Warning signs:** Delegation never recalled despite conflicting opens.

### Pitfall 5: Stale Mounts Block CI
**What goes wrong:** A failed test leaves a stale NFS mount, blocking subsequent test runs.
**Why it happens:** Mount cleanup didn't run due to test panic or timeout.
**How to avoid:** The existing `CleanupStaleMounts()` in TestMain handles this. Ensure new mount temp directory patterns are added to the cleanup list. Use `t.Cleanup()` for all mounts.
**Warning signs:** "Device or resource busy" or hanging mount commands in CI.

### Pitfall 6: nfs4-acl-tools Not Available in CI
**What goes wrong:** ACL tests fail in CI because nfs4-acl-tools package isn't installed.
**Why it happens:** The package is not part of minimal Linux images.
**How to avoid:** Add `SkipIfNoNFS4ACLTools(t)` check at the start of ACL tests. Ensure CI Dockerfile installs `nfs4-acl-tools`.
**Warning signs:** "nfs4_setfacl: command not found".

### Pitfall 7: POSIX Tests Need Different known_failures for NFSv4
**What goes wrong:** POSIX tests pass on NFSv3 but fail on NFSv4 for different reasons (or vice versa).
**Why it happens:** NFSv4 supports features that NFSv3 doesn't (ACLs, extended attributes partially, different timestamp handling), so the expected failure set differs.
**How to avoid:** Create `known_failures_v4.txt` alongside existing `known_failures.txt`. The `--nfs-version` parameter selects which failure list to use.
**Warning signs:** CI fails on "unexpected test failures" that are actually expected for that NFS version.

### Pitfall 8: Server Restart Loses Memory State
**What goes wrong:** Server restart/recovery test fails because memory metadata store loses all files.
**Why it happens:** Memory store is ephemeral by design.
**How to avoid:** The stale handle test intentionally tests this (ESTALE expected). For recovery tests that need state persistence, use BadgerDB metadata store.
**Warning signs:** All file handles become stale after restart with memory backend (this is correct behavior to test).

## Code Examples

### Example 1: Version-Parameterized File Operations Test

```go
//go:build e2e

package e2e

func TestNFSv4BasicOperations(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping NFSv4 basic operations in short mode")
    }

    versions := []struct {
        name    string
        version framework.NFSVersion
    }{
        {"v3", framework.NFSv3},
        {"v4", framework.NFSv4},
    }

    for _, ver := range versions {
        ver := ver
        t.Run(ver.name, func(t *testing.T) {
            sp := helpers.StartServerProcess(t, "")
            t.Cleanup(sp.ForceKill)

            runner := helpers.LoginAsAdmin(t, sp.APIURL())

            // Create stores and share
            metaStore := helpers.UniqueTestName("meta")
            payloadStore := helpers.UniqueTestName("payload")
            _, err := runner.CreateMetadataStore(metaStore, "memory")
            require.NoError(t, err)
            _, err = runner.CreatePayloadStore(payloadStore, "memory")
            require.NoError(t, err)
            _, err = runner.CreateShare("/export", metaStore, payloadStore)
            require.NoError(t, err)

            // Enable NFS adapter
            nfsPort := helpers.FindFreePort(t)
            _, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
            require.NoError(t, err)
            err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
            require.NoError(t, err)
            framework.WaitForServer(t, nfsPort, 10*time.Second)

            // Mount with specified version
            mount := framework.MountNFSWithVersion(t, nfsPort, "/export", ver.version)
            t.Cleanup(mount.Cleanup)

            // Run operations tests
            t.Run("ReadWrite", func(t *testing.T) {
                content := []byte("Hello NFSv" + fmt.Sprint(ver.version))
                path := mount.FilePath("test.txt")
                framework.WriteFile(t, path, content)
                read := framework.ReadFile(t, path)
                assert.Equal(t, content, read)
            })
        })
    }
}
```

### Example 2: NFSv4 Locking E2E Test

```go
func TestNFSv4Locking(t *testing.T) {
    if runtime.GOOS != "linux" {
        t.Skip("NFSv4 locking tests require Linux")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    // ... setup stores, share, adapter ...

    mount := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)
    t.Cleanup(mount.Cleanup)

    t.Run("ExclusiveLock", func(t *testing.T) {
        path := mount.FilePath("locktest.txt")
        framework.WriteFile(t, path, []byte("lock test data"))

        // Acquire exclusive lock via fcntl (NFSv4 LOCK operation)
        f := framework.LockFileRange(t, path, 0, 0, true)
        t.Cleanup(func() { framework.UnlockFileRange(t, f) })

        // Verify lock is held (try non-blocking from same process fails for POSIX locks)
        // Cross-mount verification is the real test
    })

    t.Run("ReadWriteLockConflict", func(t *testing.T) {
        path := mount.FilePath("rwlock.txt")
        framework.WriteFile(t, path, []byte("read-write lock test"))

        // Acquire shared (read) lock
        readLock := framework.LockFileRange(t, path, 0, 0, false)

        // Try exclusive (write) lock - should block/fail
        _, err := framework.TryLockFileRange(t, path, 0, 0, true)
        assert.ErrorIs(t, err, framework.ErrLockWouldBlock)

        framework.UnlockFileRange(t, readLock)
    })
}
```

### Example 3: Delegation Recall (Multi-Mount)

```go
func TestDelegationRecall(t *testing.T) {
    if runtime.GOOS != "linux" {
        t.Skip("Delegation tests require Linux")
    }

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    // ... setup ...

    // Two mounts to same share
    mountA := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)
    t.Cleanup(mountA.Cleanup)
    mountB := framework.MountNFSWithVersion(t, nfsPort, "/export", framework.NFSv4)
    t.Cleanup(mountB.Cleanup)

    // Write initial data via mount A
    pathA := mountA.FilePath("deleg_test.txt")
    framework.WriteFile(t, pathA, []byte("initial data"))

    // Mount B reads -- triggers delegation grant to mount B (or A)
    pathB := mountB.FilePath("deleg_test.txt")
    content := framework.ReadFile(t, pathB)
    assert.Equal(t, []byte("initial data"), content)

    // Mount A writes -- may trigger delegation recall on B
    framework.WriteFile(t, pathA, []byte("updated data"))

    // Mount B should see updated data (after recall + re-read)
    time.Sleep(500 * time.Millisecond) // Allow delegation recall to propagate
    content = framework.ReadFile(t, pathB)
    assert.Equal(t, []byte("updated data"), content)
}
```

### Example 4: POSIX Test Script Extension for NFSv4

```bash
#!/usr/bin/env bash
# setup-posix.sh additions for --nfs-version parameter

NFS_VERSION="${NFS_VERSION:-3}"

mount_nfs() {
    mkdir -p "$MOUNT_POINT"

    case "$NFS_VERSION" in
        3)
            mount -t nfs -o nfsvers=3,tcp,port=$NFS_PORT,mountport=$NFS_PORT,nolock,noac,sync,lookupcache=none \
                localhost:/export "$MOUNT_POINT"
            ;;
        4|4.0)
            mount -t nfs -o vers=4.0,port=$NFS_PORT,noac,sync,lookupcache=none \
                localhost:/export "$MOUNT_POINT"
            ;;
    esac
}

# run-posix.sh selects known_failures file based on version
KNOWN_FAILURES_FILE="known_failures.txt"
if [[ "$NFS_VERSION" == "4" || "$NFS_VERSION" == "4.0" ]]; then
    KNOWN_FAILURES_FILE="known_failures_v4.txt"
fi
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Manual Docker management | testcontainers-go | Already in codebase | Automatic lifecycle |
| Hardcoded ports | `FindFreePort(t)` | Already in codebase | Parallel-safe |
| Shared server across tests | Per-test server lifecycle | Already in codebase | Full isolation |
| NFSv3 only | NFSv3 + NFSv4.0 dual testing | Phase 15 (this phase) | Full protocol coverage |
| known_failures.txt (v3 only) | Versioned known_failures files | Phase 15 (this phase) | Accurate POSIX tracking per version |

**Deprecated/outdated:**
- None -- the test infrastructure is modern and well-maintained.

## Parallelism Recommendations (Claude's Discretion)

### Safe to Parallelize
- Different NFS version subtests within the same feature test (v3 and v4 use separate servers/mounts)
- Store matrix entries (each gets its own server)
- Independent feature tests (e.g., file ops vs. permissions vs. metadata stores)

### Must Be Sequential
- Subtests within a single mount (share the mount point filesystem state)
- Grace period / restart tests (modify global server state)
- Kerberos tests (modify system-level /etc/krb5.conf, /etc/krb5.keytab)
- ACL inheritance tests (depend on directory creation order)
- Control plane settings tests that use WaitForSettingsReload (12s waits)

### Server Lifecycle Recommendation
Use per-test server lifecycle (existing pattern). The 5-second startup time is acceptable for E2E tests. Benefits:
1. Complete port isolation between parallel tests
2. No state leakage
3. Server restart tests are natural
4. Each test is self-contained and debuggable in isolation

### Kerberos Fixture Recommendation
Use dynamic principals (existing pattern in kerberos_test.go). The KDC container starts once per test run (singleton pattern via sharedLocalstackHelper-like approach). Principals are created dynamically per test. This is already implemented and working.

## Open Questions

1. **NFSv4 Delegation Observability**
   - What we know: The Linux kernel NFS client handles delegations transparently. E2E tests cannot directly inspect whether a delegation was granted or recalled.
   - What's unclear: How to definitively verify delegation recall happened (vs. regular cache invalidation).
   - Recommendation: Test observable behavior (data consistency under concurrent access). Use server logs at DEBUG level to verify delegation lifecycle if needed. The existing server already logs delegation grants and recalls.

2. **Two-Mount Multi-Client Simulation Limitation**
   - What we know: Two mount points from the same Linux kernel share the same NFSv4 client ID.
   - What's unclear: Whether this adequately tests delegation recall (the kernel may optimize internally).
   - Recommendation: Use two mount points for basic delegation tests. For stress tests (`-tags=stress`), consider using separate network namespaces or test containers to get truly independent clients.

3. **NFSv4 ACL + SMB Security Descriptor Interop**
   - What we know: The context requires verifying that ACLs set via NFSv4 are visible as SMB Security Descriptors.
   - What's unclear: Exact mapping between NFSv4 ACE types and SMB SD ACEs in DittoFS implementation.
   - Recommendation: Research the ACL-to-SD mapping in Phase 13 implementation code before writing tests. The test should set a known ACL via nfs4_setfacl, then verify the SMB SD contains equivalent permissions.

## Sources

### Primary (HIGH confidence)
- Existing codebase: `test/e2e/framework/` -- All test primitives inspected directly
- Existing codebase: `test/e2e/helpers/` -- All helper patterns inspected directly
- Existing codebase: `test/e2e/*_test.go` -- All 22 test files inspected
- Existing codebase: `test/posix/` -- POSIX test infrastructure and known_failures.txt
- Existing codebase: `internal/protocol/nfs/v4/` -- NFSv4 handler/state/types implementation
- Existing codebase: `pkg/adapter/nfs/nfs_adapter.go` -- Dual v3/v4 adapter confirmed

### Secondary (MEDIUM confidence)
- [nfs(5) Linux manual page](https://man7.org/linux/man-pages/man5/nfs.5.html) -- NFSv4 mount options (vers=4.0)
- [nfs4_setfacl(1) manual](https://www.man7.org/linux/man-pages/man1/nfs4_setfacl.1.html) -- ACL tool usage
- [nfs4_getfacl Debian manpage](https://manpages.debian.org/testing/nfs4-acl-tools/nfs4_getfacl.1.en.html) -- ACL tool availability
- [mount.nfs(8) Linux manual](https://www.man7.org/linux/man-pages/man8/mount.nfs4.8.html) -- NFSv4 mount syntax

### Tertiary (LOW confidence)
- None -- all critical findings verified against codebase and official documentation.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in codebase, no new dependencies
- Architecture: HIGH -- patterns directly observed in existing test files, extending proven patterns
- Pitfalls: HIGH -- pitfalls derived from actual codebase behavior and NFSv4 protocol knowledge
- NFSv4 mount options: HIGH -- verified against Linux nfs(5) manpage
- Delegation testing approach: MEDIUM -- kernel client behavior makes direct observability uncertain

**Research date:** 2026-02-17
**Valid until:** 2026-03-17 (stable domain -- Go testing and NFS protocol are mature)
