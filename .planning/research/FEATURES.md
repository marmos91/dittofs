# Feature Landscape: E2E Test Suite for Networked File Systems

**Domain:** E2E testing for virtual filesystem with NFS/SMB protocols
**Researched:** 2026-02-02
**Confidence:** HIGH (based on industry standards, established test suites, and DittoFS-specific context)

## Table Stakes

Features that comprehensive E2E test suites for networked file systems MUST have. Missing any of these makes the test suite incomplete and unreliable.

### File Operations Coverage

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **CRUD Operations** | Core functionality - every filesystem must create, read, update, delete files | Low | Already covered in `functional_test.go` |
| **Directory Operations** | mkdir, rmdir, readdir - fundamental metadata ops | Low | Already covered |
| **File Attributes** | mode, uid, gid, timestamps (atime, mtime, ctime) | Medium | Partially covered (permissions tested, timestamps less so) |
| **Rename/Move** | Cross-directory moves, in-place renames, overwrites | Medium | Already covered in `advanced_test.go` |
| **Hard Links** | Link counting, shared inode, delete-original-survives | Medium | Already covered |
| **Symbolic Links** | Create, readlink, follow, dangling | Medium | Already covered |
| **Large File Support** | Files > 4GB, sparse files, truncation | Medium | Already covered up to 1GB |
| **Empty Files** | Zero-byte files (edge case for many bugs) | Low | Already covered |
| **Binary Data Integrity** | Non-text data, null bytes, all byte values | Low | Already covered via checksums |

### Concurrency and Consistency

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Concurrent Reads** | Multiple clients reading same file | Low | Already covered |
| **Concurrent Writes to Different Files** | Parallel writes to different paths | Low | Already covered |
| **Sequential Consistency** | Write-then-read returns written data | Low | Implicitly tested everywhere |
| **Metadata Consistency** | Size, mtime updates reflect after write | Medium | Partially covered |
| **Directory Enumeration Under Mutation** | Readdir while files added/removed | High | NOT covered - needed |

### Protocol-Specific Testing

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **NFS Mount/Unmount** | Basic connectivity | Low | Already covered (framework) |
| **SMB Mount/Unmount** | Basic connectivity | Low | Already covered (framework) |
| **Cross-Protocol Consistency** | NFS write visible via SMB and vice versa | Medium | Already covered in `interop_v2_test.go` |
| **Protocol-Specific Error Codes** | ENOENT, EACCES, EEXIST map correctly | Medium | Partially tested implicitly |
| **Attribute Caching Behavior** | stale data within acregmin/acdirmin | High | NOT covered - needed |

### Error Handling

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Permission Denied** | Access to file without rights returns error | Low | NOT systematically covered |
| **File Not Found** | Access to nonexistent file returns ENOENT | Low | Implicitly tested |
| **Directory Not Empty** | rmdir on non-empty dir fails | Low | NOT explicitly tested |
| **File Exists** | Create with O_EXCL on existing file fails | Low | NOT explicitly tested |
| **Path Too Long** | Exceeding NAME_MAX or PATH_MAX | Low | NOT tested |
| **No Space Left** | ENOSPC handling | High | NOT tested (hard to simulate) |

### Scale Testing

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Many Files in Directory** | 1K, 10K, 100K files | Medium | Already covered up to 10K |
| **Deep Directory Nesting** | 50+ levels deep | Low | Already covered (50 levels) |
| **Large File Sizes** | 100MB, 1GB, 10GB | High | Already covered up to 1GB (configurable) |
| **Many Simultaneous Connections** | Multiple mount points, parallel ops | High | Partially covered |

### Control Plane API Testing (DittoFS-Specific)

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **User CRUD** | Create, read, update, delete users | Low | NOT covered in E2E (only handler unit tests) |
| **Group CRUD** | Create, read, update, delete groups | Low | NOT covered in E2E |
| **Share CRUD** | Create, read, update, delete shares | Low | NOT covered in E2E |
| **Permission Enforcement** | User X cannot access share Y without permission | Medium | NOT covered |
| **Authentication Flow** | Login, token refresh, logout | Low | NOT covered in E2E |

### Store Backend Matrix (DittoFS-Specific)

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Memory Metadata Store** | Fast, volatile baseline | Low | Already covered |
| **BadgerDB Metadata Store** | Persistent, embedded | Low | Already covered |
| **PostgreSQL Metadata Store** | Distributed, HA | Medium | Already covered |
| **All Combinations Work** | Each config passes basic CRUD | Medium | Framework supports this |

## Differentiators

Features that make a test suite excellent rather than adequate. Not strictly required but significantly improve test value.

### Advanced POSIX Compliance

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **pjdfstest Integration** | Industry-standard POSIX test suite (8,813 tests) | High | Industry standard, JuiceFS uses this |
| **xfstests Compatibility** | Linux kernel filesystem test suite | Very High | Used by ext4, XFS, btrfs teams |
| **Connectathon Tests** | Traditional NFS compliance suite | Medium | Older but still valuable |
| **fcntl/flock Testing** | File locking behavior | High | DittoFS doesn't support NLM yet |

### Performance Benchmarking

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Throughput Metrics** | MB/s for sequential read/write | Medium | Valuable for regression detection |
| **Latency Metrics** | p50/p95/p99 for operations | Medium | Identifies tail latency issues |
| **IOPS Measurement** | Small file random I/O | Medium | Critical for many workloads |
| **Comparison Baselines** | Compare against local FS, other NFS | High | Requires reference implementations |

### Fault Tolerance Testing

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Server Restart Recovery** | Client reconnects after server restart | High | Critical for production |
| **Network Partition Simulation** | Client behavior during network drop | Very High | Requires iptables/tc manipulation |
| **Graceful Shutdown Verification** | In-flight ops complete cleanly | Medium | Important for data integrity |
| **Stale Handle Recovery** | Client recovery from stale NFS handles | High | Common production issue |

### Control Plane Hot Reload (DittoFS-Specific)

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Adapter Enable/Disable** | Turn off NFS without restart | Medium | API exists, not E2E tested |
| **Adapter Port Change** | Change port, adapter restarts | Medium | API exists, not E2E tested |
| **Share Hot Add** | Add new share while running | Medium | Critical for production |
| **Store Hot Swap** | Change metadata store for share | High | May require downtime |
| **User Permission Changes** | Take effect immediately | Medium | Security-critical |

### Chaos Engineering

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Random Operation Stress** | fsstress/fsx style randomized ops | High | Finds edge cases |
| **Slow Backend Injection** | Simulate S3 latency spikes | High | Tests timeout handling |
| **Partial Write Injection** | Simulate incomplete I/O | Very High | Tests data corruption handling |

### Observability Integration

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **Metrics Validation** | Prometheus metrics increment correctly | Medium | Ensures observability works |
| **Trace Correlation** | OpenTelemetry spans appear | Medium | Debugging aid |
| **Log Assertions** | Specific log lines appear/don't appear | Low | Catches regressions |

### Multi-Tenancy (DittoFS-Specific)

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **User Isolation** | User A cannot see User B's files | Medium | Security requirement |
| **Group Permissions** | Group members share access | Medium | Unix model |
| **Admin Override** | Admin can access everything | Low | Operational need |
| **Quota Enforcement** | Per-user/share space limits | High | Not implemented in DittoFS yet |

## Anti-Features

Things to deliberately NOT test in E2E tests. These belong in unit tests, integration tests, or are simply out of scope.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| **XDR Encoding Details** | Implementation detail, not user-visible | Unit tests in `internal/protocol/nfs/xdr/` |
| **RPC Framing** | Wire protocol internals | Unit tests, wireshark validation |
| **Buffer Pool Internals** | Performance optimization, not correctness | Unit tests, benchmarks |
| **GORM Query Optimization** | Database layer detail | Integration tests in `pkg/controlplane/store/` |
| **JWT Token Parsing** | Authentication internals | Unit tests in `internal/controlplane/api/auth/` |
| **Individual Handler Validation** | Already unit tested | Handler tests exist in `internal/controlplane/api/handlers/*_test.go` |
| **Specific Error Message Text** | Brittle, locale-dependent | Test error codes/types instead |
| **Timing-Dependent Behavior** | Flaky, non-deterministic | Use explicit synchronization |
| **Network Byte Order Details** | Protocol internals | Unit tests |
| **S3 Multipart Upload Internals** | Backend implementation detail | Integration tests |
| **Cache Eviction Timing** | Non-deterministic | Test invariants, not timing |

## Feature Dependencies

```
Authentication ─────┬───> User CRUD
                    ├───> Group CRUD
                    └───> Permission Enforcement

Permission Enforcement ───> Share Access Control
                       ───> Cross-Protocol Consistency

Store Backend Matrix ───> All File Operations
                     ───> Metadata Consistency

Adapter Lifecycle ───> Protocol-Specific Testing
                  ───> Hot Reload Testing
                  ───> Graceful Shutdown

File Operations ───> Large File Support
               ───> Concurrency Testing
               ───> Scale Testing
```

## MVP Recommendation

For a comprehensive E2E test suite that validates DittoFS for production use:

### Phase 1: Foundation (Table Stakes)
1. **File Operations Coverage** - Already largely complete
2. **Permission Enforcement** - Add tests for user-cannot-access-share-without-permission
3. **Error Handling** - Add systematic error code tests (EACCES, ENOENT, EEXIST, ENOTEMPTY)
4. **Control Plane API E2E** - Add tests that use REST API to create users/shares, then verify via protocol

### Phase 2: Production Readiness (Key Differentiators)
1. **Adapter Lifecycle Testing** - Enable/disable adapters via API, verify connectivity
2. **Graceful Shutdown Verification** - Start write, trigger shutdown, verify completion
3. **Hot Reload Testing** - Change share config via API, verify changes take effect
4. **Basic Chaos** - Server restart during operations

### Phase 3: Excellence (Advanced Differentiators)
1. **pjdfstest Integration** - Run industry-standard POSIX compliance suite
2. **Performance Benchmarking** - Establish baselines, detect regressions
3. **Fault Injection** - Network partition, slow backend simulation
4. **Observability Validation** - Verify metrics and traces

### Defer to Post-MVP:
- xfstests full suite (very extensive, complex setup)
- Quota enforcement testing (feature not implemented)
- Multi-tenant isolation (depends on quota/ACL implementation)
- Connectathon lock tests (NLM not implemented)

## Sources

### Industry Standards
- [NFStest](https://github.com/thombashi/NFStest) - Linux NFS testing framework with packet-level verification
- [pjdfstest](https://github.com/pjd/pjdfstest) - POSIX filesystem compliance test suite (8,813 tests)
- [xfstests](https://github.com/kdave/xfstests) - Linux kernel filesystem test suite (72% code coverage on ext4)
- [Connectathon NFS tests](https://github.com/dkruchinin/cthon-nfs-tests) - Traditional NFS compliance tests
- [smbtorture](https://wiki.samba.org/index.php/Writing_Torture_Tests) - Samba's SMB protocol test suite

### Research and Best Practices
- [File Systems are Hard to Test - Learning from Xfstests](https://www.researchgate.net/publication/330808206_File_Systems_are_Hard_to_Test_-_Learning_from_Xfstests) - Coverage analysis showing 72% coverage, identifies hard-to-test areas
- [POSIX Compatibility Comparison - JuiceFS](https://juicefs.com/en/blog/engineering/posix-compatibility-comparison-among-four-file-system-on-the-cloud) - Shows cloud filesystems failing 0-21% of pjdfstest
- [End-to-End Testing for Microservices Guide](https://www.bunnyshell.com/blog/end-to-end-testing-for-microservices-a-2025-guide/) - API testing best practices
- [Fault Injection Testing - Microsoft](https://microsoft.github.io/code-with-engineering-playbook/automated-testing/fault-injection-testing/) - Chaos engineering methodology

### Control Plane Testing
- [Chaos Mesh](https://deepwiki.com/chaos-mesh/chaos-mesh) - Kubernetes chaos engineering platform
- [Samba Integration Tests](https://github.com/samba-in-kubernetes/sit-test-cases) - Automated SMB testing in Kubernetes

### Confidence Assessment

| Category | Confidence | Rationale |
|----------|------------|-----------|
| File Operations | HIGH | Well-established industry patterns, existing coverage good |
| Control Plane API | HIGH | Standard REST API testing patterns apply |
| Protocol Testing | MEDIUM | NFS/SMB specifics vary by implementation |
| Fault Tolerance | MEDIUM | Complex to implement correctly, tooling varies |
| Chaos Engineering | LOW | Requires significant infrastructure investment |
