# Domain Pitfalls: E2E Testing for Networked File Systems

**Domain:** E2E testing for NFS/SMB virtual filesystem (DittoFS)
**Researched:** 2026-02-02
**Confidence:** HIGH (based on existing codebase analysis + verified community patterns)

---

## Critical Pitfalls

Mistakes that cause test suite unreliability, CI breakage, or major rework.

---

### Pitfall 1: NFS/SMB Attribute Caching Causes Test Flakiness

**What goes wrong:** Tests intermittently fail because NFS/SMB clients cache file attributes (size, mtime, existence). A file written via one protocol may not be immediately visible via another protocol, or even via the same mount point after certain operations.

**Why it happens:** NFS clients cache attributes by default (acregmin=3s, acdirmin=30s). SMB has similar caching. Cross-protocol tests that write-then-read without accounting for cache propagation fail randomly.

**Consequences:**
- Tests pass locally but fail in CI (timing differences)
- Intermittent failures that are hard to reproduce
- Developers add arbitrary `time.Sleep()` calls that slow down the suite

**Warning signs:**
- Tests that pass when run individually but fail in parallel
- `time.Sleep()` calls scattered throughout test code
- Tests that fail more often on faster machines

**Prevention:**
1. Mount NFS with `actimeo=0` to disable attribute caching (already done in DittoFS framework)
2. For cross-protocol tests, use explicit sync/flush operations rather than time delays
3. Implement a `WaitForVisibility` helper that polls with exponential backoff rather than fixed sleep
4. Design tests to be cache-aware: verify operations through the same protocol path when possible

**Detection:**
- Run tests 10x in a loop; flaky tests will fail at least once
- Compare test timing between local and CI environments

**Phase:** Address in Phase 1 (Foundation) - mount options and helper patterns

**Sources:**
- [Red Hat: Configure age of attribute cache in NFS client](https://access.redhat.com/solutions/315113)
- [nfstest_cache documentation](https://www.mankier.com/1/nfstest_cache)
- DittoFS existing implementation in `framework/mount.go` (line 41: `actimeo=0`)

---

### Pitfall 2: Stale Mounts After Test Failures Break Subsequent Runs

**What goes wrong:** When a test panics, times out, or is interrupted (Ctrl+C), NFS/SMB mounts may remain active. Subsequent test runs fail because mount points are busy, ports are occupied, or the server cannot bind.

**Why it happens:** Go's `defer` cleanup doesn't run on panic or process termination. NFS/SMB kernel clients hold connections open even after server process dies.

**Consequences:**
- "mount point busy" errors on next test run
- Port binding failures requiring manual cleanup
- CI jobs that hang waiting for resources
- Developers running `sudo umount -f` manually between test runs

**Warning signs:**
- `mount | grep dittofs` shows stale entries after test failure
- Tests work after reboot but fail after interrupted run
- CI requires a "cleanup" step before tests

**Prevention:**
1. Implement `TestMain` cleanup that runs before and after all tests (already done in DittoFS)
2. Use `CleanupStaleMounts()` at test suite startup to clear orphaned mounts
3. Register signal handlers (SIGINT, SIGTERM) that trigger cleanup
4. Use unique mount point directories per test run (timestamp or random suffix)
5. Implement force-unmount with retries and timeouts
6. In CI, add a pre-test step: `sudo umount -f /tmp/dittofs-* 2>/dev/null || true`

**Detection:**
- Run `mount | grep -E "(nfs|smb|cifs)"` before and after tests
- Check for `/tmp/dittofs-*` directories that shouldn't exist

**Phase:** Address in Phase 1 (Foundation) - cleanup infrastructure

**Sources:**
- DittoFS existing implementation in `framework/mount.go` (`CleanupStaleMounts`)
- [Virtuozzo: Zombie Disks from stale mounts](https://iaas-onapp-support.virtuozzo.com/hc/en-us/articles/360005673214-Integrated-Storage-Disk-Snapshots-Zombie-Disks)

---

### Pitfall 3: Cross-Protocol Locking Semantic Mismatch

**What goes wrong:** Tests assume NFS and SMB file locks are compatible. They are not. A file locked via SMB may still be writable via NFS, leading to data corruption in concurrent access tests.

**Why it happens:** NFS uses advisory locks (POSIX semantics); SMB uses mandatory share-mode locks (Windows semantics). The two systems do not interoperate unless the server explicitly bridges them.

**Consequences:**
- Concurrent access tests that "pass" but actually have data races
- False confidence in multiprotocol safety
- Production data corruption when both protocols access same files

**Warning signs:**
- Concurrent write tests never fail even with aggressive timing
- No lock-related errors in test output
- Tests don't actually verify lock acquisition

**Prevention:**
1. Document that DittoFS does not implement cross-protocol lock bridging
2. Test locking within single protocol only (NFS-to-NFS, SMB-to-SMB)
3. For cross-protocol tests, use different files (e.g., `nfs_*.txt`, `smb_*.txt`) to avoid lock contention
4. Add explicit warnings in test documentation about this limitation
5. Consider implementing unified lock manager if cross-protocol locking is required

**Detection:**
- Audit tests for concurrent write operations across protocols
- Check if tests actually verify lock acquisition vs just assuming it works

**Phase:** Address in Phase 2 (Cross-Protocol Tests) with explicit documentation

**Sources:**
- [NetApp: Multiprotocol NAS Locking](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/)
- [Dell OneFS File Locking](http://www.unstructureddatatips.com/onefs-file-locking-and-concurrent-access/)
- [Red Hat: Concurrent NFS Clients writing causes data corruption](https://access.redhat.com/solutions/320503)

---

### Pitfall 4: Testcontainers Port Collision in Parallel Test Runs

**What goes wrong:** Multiple CI pipelines or parallel test processes try to use the same fixed port for PostgreSQL/S3 containers, causing binding failures.

**Why it happens:** Developers hardcode ports (e.g., `5432`, `4566`) instead of using Testcontainers' dynamic port allocation. Or they reuse container names across parallel runs.

**Consequences:**
- "Port already in use" errors in CI
- Flaky tests that depend on execution order
- CI jobs that work in isolation but fail when multiple PRs run simultaneously

**Warning signs:**
- Tests pass locally but fail in CI with port errors
- Tests pass in single-PR CI but fail in merge queues
- Container names like `postgres-test` without unique suffixes

**Prevention:**
1. Never use fixed ports - always use `container.MappedPort()` to get the dynamically assigned port
2. Never use fixed container names - let Testcontainers generate unique names
3. Use `FindFreePort()` helper for non-container services (already in DittoFS)
4. For shared containers (performance optimization), use test-level locking or unique database/bucket names per test

**Detection:**
- Search codebase for hardcoded port numbers in test code
- Check if container names include unique identifiers

**Phase:** Already addressed in DittoFS - maintain this pattern

**Sources:**
- [Why you should never use fixed ports in Testcontainers](https://bsideup.github.io/posts/testcontainers_fixed_ports/)
- [Docker: Testcontainers Best Practices](https://www.docker.com/blog/testcontainers-best-practices/)
- [Testcontainers: Reuse in Parallel Integration Tests](https://peshrus.medium.com/reuse-test-containers-in-parallel-integration-tests-ccb8ffbd889)

---

### Pitfall 5: Goroutine Leaks from Unclosed Server Connections

**What goes wrong:** Each test creates a server, but connections/goroutines from previous tests leak into subsequent tests. This causes resource exhaustion, false test failures, and makes debugging impossible.

**Why it happens:** NFS/SMB servers spawn goroutines per connection. If cleanup doesn't wait for all connections to close, goroutines survive. Tests share global state and interfere with each other.

**Consequences:**
- Tests that pass individually but fail in sequence
- Memory/goroutine count grows throughout test run
- Mysterious errors like "too many open files"
- Stack traces show goroutines from previous tests

**Warning signs:**
- `runtime.NumGoroutine()` increases over test run
- Tests fail more often later in the suite
- `goleak` reports leaked goroutines

**Prevention:**
1. Use `goleak.VerifyTestMain(m)` to detect leaks after all tests complete
2. Ensure `Runtime.StopAllAdapters()` waits for all connections to drain
3. Set connection timeouts short in tests (e.g., 5s idle timeout)
4. Track active connections and fail cleanup if count > 0 after timeout
5. Use `t.Cleanup()` instead of `defer` for test-specific resources

**Detection:**
- Add `goleak.VerifyNone(t)` to individual tests during development
- Monitor goroutine count before and after each test
- Use `pprof` to identify stuck goroutines

**Phase:** Address in Phase 1 (Foundation) - server lifecycle management

**Sources:**
- [Uber: goleak - Goroutine leak detector](https://github.com/uber-go/goleak)
- [Storj: Finding Goroutine Leaks in Tests](https://storj.dev/blog/finding-goroutine-leaks-in-tests)
- [Go Blog: Testing concurrent code with synctest](https://go.dev/blog/synctest)

---

## Moderate Pitfalls

Mistakes that cause delays, maintenance burden, or technical debt.

---

### Pitfall 6: Test Isolation Through Shared Server State

**What goes wrong:** Tests share a single DittoFS server instance for performance. However, files/directories created by one test pollute subsequent tests. Tests become order-dependent.

**Why it happens:** Creating a new server per test is slow (mount/unmount overhead). Teams share servers but forget to clean up test artifacts.

**Prevention:**
1. Each test should create files in a unique subdirectory (e.g., `tc.Path("testname-uuid/file.txt")`)
2. Implement automatic cleanup in test teardown that removes test-specific directories
3. Consider using unique share/export per test rather than unique subdirectory
4. Truncate database tables between tests for metadata stores (already in DittoFS: `TruncateTables()`)

**Phase:** Address in Phase 1 (Foundation) - test context isolation

---

### Pitfall 7: Sudo Requirements Break Developer Workflow and CI

**What goes wrong:** NFS/SMB mount operations require `sudo`. Developers forget to run tests with sudo, or CI environments don't have sudo access configured.

**Why it happens:** Mounting filesystems is a privileged kernel operation. There's no way around this for real NFS/SMB mounts.

**Prevention:**
1. Document sudo requirement prominently in README and test output
2. Add early check in test framework that fails fast with clear message if not root
3. For CI (GitHub Actions), use `sudo go test` in workflow
4. Consider rootless alternatives for quick feedback:
   - Unit tests that mock the mount layer
   - Container-based testing where the container runs as root

**Phase:** Address in Phase 1 (Foundation) - CI configuration

**Sources:**
- [GitHub Community: Running with sudo in Actions](https://github.com/orgs/community/discussions/52343)

---

### Pitfall 8: macOS vs Linux Mount Option Differences

**What goes wrong:** Mount commands that work on Linux fail on macOS (or vice versa). Tests become platform-specific.

**Why it happens:** macOS requires `resvport` option for NFS, uses `mount_smbfs` instead of `mount -t cifs`, and has different default behaviors.

**Prevention:**
1. Use platform-specific mount logic (already in DittoFS: `runtime.GOOS` switch)
2. Test on both platforms in CI (macOS + Linux matrix)
3. Document known platform differences
4. Use `diskutil unmount` on macOS instead of `umount` for cleaner unmount

**Phase:** Address in Phase 1 (Foundation) - platform abstraction

---

### Pitfall 9: Hot Reload Tests Without Connection Draining

**What goes wrong:** Testing protocol adapter hot reload without properly draining existing connections. Clients get disconnected mid-operation, but tests don't verify graceful handling.

**Why it happens:** Hot reload tests focus on "adapter restarts successfully" but not on "existing operations complete safely."

**Prevention:**
1. Start long-running operations before triggering reload
2. Verify operations complete (not just "no error")
3. Test that new connections after reload work correctly
4. Implement and test graceful shutdown timeout in adapter
5. Verify metrics show proper connection lifecycle (accepted -> closed, not force-closed)

**Phase:** Address in Phase 3 (Hot Reload Tests) - specific test scenarios

---

### Pitfall 10: Container Startup Race in Testcontainers

**What goes wrong:** Tests start before container is fully ready. Database connection fails, S3 returns errors.

**Why it happens:** `WaitingFor` strategies are too simplistic (e.g., just waiting for port) when the service needs more initialization time.

**Prevention:**
1. Use composite wait strategies (already in DittoFS):
   - `wait.ForListeningPort()` AND
   - `wait.ForLog()` with specific ready message AND
   - `wait.ForHTTP()` health endpoint
2. Add application-level readiness check after container reports ready
3. Use exponential backoff for first connection attempt

**Phase:** Already addressed in DittoFS - maintain this pattern

---

## Minor Pitfalls

Annoyances that are easily fixable but worth knowing about.

---

### Pitfall 11: Large File Tests Timeout in CI

**What goes wrong:** 100MB+ file tests pass locally but timeout in CI due to slower I/O or network.

**Prevention:**
1. Use `-short` flag to skip large file tests in quick feedback loops
2. Set generous timeouts for large file tests (e.g., `-timeout 30m`)
3. Run large file tests in separate CI job with appropriate resources

**Phase:** Address in Phase 2 (Scale Tests) - test organization

---

### Pitfall 12: Flaky Directory Listing Tests

**What goes wrong:** Tests that count directory entries get unexpected results due to hidden files, metadata files, or timing issues.

**Prevention:**
1. Filter out hidden files (`.DS_Store`, etc.) in helpers
2. Use unique directory per test
3. Verify specific file names, not just count

**Phase:** Address in Phase 1 (Foundation) - helper improvements

---

### Pitfall 13: Test Output Noise Hides Real Failures

**What goes wrong:** Server logs at INFO/DEBUG level flood test output. Real errors are buried.

**Prevention:**
1. Set server log level to ERROR during tests (already in DittoFS)
2. Use `DITTOFS_LOGGING_LEVEL` env var to enable debug only when needed
3. Capture server logs to separate file for debugging

**Phase:** Already addressed in DittoFS - maintain this pattern

---

## Phase-Specific Warnings

| Phase | Topic | Likely Pitfall | Mitigation |
|-------|-------|---------------|------------|
| Phase 1 | Foundation | Stale mounts, goroutine leaks | Comprehensive cleanup in TestMain |
| Phase 1 | Foundation | Sudo requirement not documented | Early fail-fast check |
| Phase 2 | Cross-Protocol | Attribute caching flakiness | Use `actimeo=0`, avoid timing assumptions |
| Phase 2 | Cross-Protocol | Lock semantic mismatch | Document limitation, test within protocol |
| Phase 3 | Hot Reload | Connection draining not tested | Long-running ops during reload |
| Phase 4 | Scale Tests | CI timeouts | Separate job, generous timeouts |
| All | Testcontainers | Port collision | Never use fixed ports |
| All | Testcontainers | Container startup race | Composite wait strategies |

---

## Summary: Top 5 Actions to Prevent E2E Test Suite Failure

1. **Use `actimeo=0` for NFS mounts** - Eliminates attribute caching flakiness
2. **Implement robust cleanup in TestMain** - Handles panics, interrupts, and stale state
3. **Never use fixed ports** - Let Testcontainers allocate dynamically
4. **Use goleak to detect goroutine leaks** - Catches connection/resource leaks early
5. **Isolate tests by directory/bucket** - Prevents test pollution when sharing servers

---

## Sources

- [Testcontainers Best Practices - Docker](https://www.docker.com/blog/testcontainers-best-practices/)
- [Why you should never use fixed ports - Testcontainers](https://bsideup.github.io/posts/testcontainers_fixed_ports/)
- [goleak - Uber](https://github.com/uber-go/goleak)
- [NFS Attribute Caching - Red Hat](https://access.redhat.com/solutions/315113)
- [Multiprotocol NAS Locking](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/)
- [FOSDEM 2026: Multiprotocol Stack Challenges](https://fosdem.org/2026/schedule/event/DMKKYH-open-source-multiprotocol/)
- [Go Blog: Testing concurrent code](https://go.dev/blog/synctest)
- [Concurrent NFS write corruption - Red Hat](https://access.redhat.com/solutions/320503)
