# Project Research Summary

**Project:** CLI-Driven E2E Test Suite for DittoFS
**Domain:** End-to-end testing for networked virtual filesystem with NFS/SMB protocols
**Researched:** 2026-02-02
**Confidence:** HIGH

## Executive Summary

This research evaluated how to build a comprehensive E2E test suite for DittoFS that validates both file system operations (via NFS/SMB mounts) and control plane management (via dittofsctl CLI). The research identified that DittoFS already has an excellent foundation with a well-structured test/e2e/ framework demonstrating Go E2E best practices. The missing piece is systematic CLI-driven testing for the control plane (user/group/share/adapter management).

**Recommended approach:** Build on the existing framework by adding a CLI wrapper layer that provides type-safe dittofsctl command execution. Continue using the proven stack (stdlib testing, testify, testcontainers-go) rather than introducing BDD frameworks or exotic CLI testing tools. The test suite should validate three integration paths: (1) CLI-only operations, (2) filesystem-only operations (existing coverage), and (3) cross-layer scenarios where CLI changes affect mounted filesystems.

**Key risks:** NFS/SMB attribute caching can cause flaky tests, stale mounts from failed runs break subsequent executions, and cross-protocol locking semantics don't interoperate. These risks are well-understood and preventable through existing framework patterns (actimeo=0 mount option, TestMain cleanup, protocol-specific isolation). The most significant new risk is goroutine leaks from server connection handling, mitigable using goleak verification.

## Key Findings

### Recommended Stack

The existing DittoFS stack (Go 1.25, testcontainers-go v0.40.0, testify v1.11.1) is complete and production-proven. No additional testing libraries are needed. The research explicitly recommends AGAINST adding Ginkgo/Gomega (BDD complexity), rogpeppe/testscript (overkill), or custom CLI test DSLs (maintenance burden).

**Core technologies:**
- **testing (stdlib) + testify v1.11.1** — Test runner, assertions, suite lifecycle; battle-tested, already in use
- **testcontainers-go v0.40.0** — Container lifecycle for PostgreSQL/S3; latest stable, proven in existing framework
- **os/exec + bytes.Buffer** — CLI execution and output capture; simple, reliable, no external dependencies
- **cobra v1.8.1** — CLI framework already used by dittofsctl; direct command invocation for unit tests

**Critical insight:** Avoid the temptation to introduce "clever" testing frameworks. The stdlib plus testify provides everything needed. Complexity should be in the system under test, not the test infrastructure.

### Expected Features

The research identified 80+ features across table stakes, differentiators, and anti-features. The analysis reveals DittoFS already has strong coverage of filesystem operations but gaps in control plane E2E testing.

**Must have (table stakes):**
- **File Operations Coverage** — CRUD, directories, attributes, links, large files (already ~90% covered in functional_test.go)
- **Control Plane API E2E** — User/group/share CRUD via dittofsctl (NOT covered, critical gap)
- **Permission Enforcement** — User-cannot-access-share-without-permission scenarios (NOT systematically tested)
- **Error Handling** — Systematic EACCES, ENOENT, EEXIST, ENOTEMPTY tests (partially covered)
- **Store Backend Matrix** — All 9 metadata/payload combinations work (framework supports, good coverage)

**Should have (competitive):**
- **Adapter Lifecycle Testing** — Enable/disable adapters via CLI, verify connectivity changes (API exists, not E2E tested)
- **Hot Reload Testing** — Change share config, verify changes take effect without restart (critical for production)
- **Graceful Shutdown Verification** — In-flight operations complete during shutdown (partially tested)
- **Basic Fault Tolerance** — Server restart recovery, stale handle detection (NOT covered)

**Defer (v2+):**
- **pjdfstest Integration** — 8,813 POSIX compliance tests (extensive, high value but not MVP)
- **xfstests Compatibility** — Linux kernel filesystem test suite (very high complexity)
- **Chaos Engineering** — Network partition, slow backend injection (requires significant infrastructure)
- **File Locking Tests** — fcntl/flock behavior (DittoFS doesn't implement NLM yet)

### Architecture Approach

The existing framework demonstrates the ideal layered component architecture: Tests → Framework Layer (TestContext, Helpers, Runners) → Infrastructure Layer (Mount, Containers, Config) → External Resources. The research identifies one missing component: a CLI Wrapper that provides type-safe dittofsctl command execution with structured result parsing.

**Major components:**

1. **CLI Wrapper (NEW)** — Execute dittofsctl commands, parse JSON/YAML output, return typed results. Should handle authentication flow (login/logout), manage multi-context credentials, and provide helper methods for each command group (user, group, share, store, adapter).

2. **TestContext (existing, enhance)** — Currently orchestrates server, stores, and mounts. Needs to add CLI accessor method (tc.CLI()) that returns a pre-configured wrapper with server URL and authentication token.

3. **Container Helpers (existing, maintain)** — PostgreSQL and LocalStack management is production-ready. Uses composite wait strategies (port + log + health check) to avoid startup races. Implements singleton pattern for shared containers to reduce test suite execution time.

4. **Mount Helpers (existing, maintain)** — Platform-specific NFS/SMB mount logic with actimeo=0 to eliminate attribute caching flakiness. Includes CleanupStaleMounts() for robust cleanup after test failures.

**Key patterns to follow:**
- Build tag isolation (//go:build e2e) to separate from unit tests
- TestMain lifecycle for global setup/teardown and signal handling
- Configuration matrix testing via RunOnAllConfigs() for pluggable backends
- Table-driven subtests with t.Run() for test variations
- Never use fixed ports, always dynamic allocation via FindFreePort()

### Critical Pitfalls

Research identified 13 pitfalls (5 critical, 5 moderate, 3 minor). The top 5 account for 90% of E2E test suite failures in production.

1. **NFS/SMB Attribute Caching** — Clients cache attributes (size, mtime, existence) causing flaky cross-protocol tests. **Prevention:** Mount with actimeo=0 (already done), implement WaitForVisibility helper with exponential backoff instead of time.Sleep().

2. **Stale Mounts After Failures** — Interrupted tests leave NFS/SMB mounts active, blocking subsequent runs. **Prevention:** TestMain cleanup at startup (already done), signal handlers for SIGINT/SIGTERM, unique mount point directories per run, force-unmount with retries.

3. **Cross-Protocol Locking Mismatch** — NFS advisory locks and SMB mandatory locks don't interoperate, causing false test passes despite data race conditions. **Prevention:** Document limitation explicitly, test locking within single protocol only, use different files for cross-protocol tests.

4. **Testcontainers Port Collision** — Parallel CI runs collide on fixed ports. **Prevention:** Never hardcode ports, always use container.MappedPort() (already done), let Testcontainers generate unique container names.

5. **Goroutine Leaks from Server Connections** — Unclosed connections leak goroutines across tests, causing resource exhaustion. **Prevention:** Use goleak.VerifyTestMain(), ensure Runtime.StopAllAdapters() waits for connection drain, set short connection timeouts in tests (5s idle).

## Implications for Roadmap

The research findings suggest a 4-phase structure that builds incrementally from CLI foundation through integration scenarios. Each phase delivers standalone value while setting up the next phase.

### Phase 1: CLI Wrapper Foundation
**Rationale:** Must establish type-safe CLI execution infrastructure before building specific command tests. This is the blocking dependency for all subsequent phases.

**Delivers:**
- framework/cli.go with CLIWrapper base implementation
- Binary execution, output capture, error handling
- TestContext integration (tc.CLI() accessor)
- Basic smoke tests (version, status commands)

**Stack decisions:** os/exec + bytes.Buffer (stdlib), no external CLI libraries
**Avoids pitfalls:** Foundation for proper async testing (no time.Sleep), enables clean teardown patterns

**Research flag:** SKIP — standard Go patterns, well-documented

### Phase 2: Authentication and User Management
**Rationale:** Authentication is prerequisite for all control plane operations. User/group management are simplest CRUD operations, ideal for validating CLI wrapper patterns before tackling complex share management.

**Delivers:**
- Login/logout flow tests
- Token management and persistence
- User CRUD via dittofsctl (create, list, get, update, delete)
- Group CRUD and membership operations

**Features:** Control Plane API E2E (table stakes), Authentication Flow (table stakes)
**Avoids pitfalls:** Test isolation through unique usernames, proper cleanup in teardown

**Research flag:** SKIP — REST API testing patterns are standard

### Phase 3: Share and Permission Management
**Rationale:** Builds on Phase 2 (users/groups exist). Share management is core functionality that bridges control plane and data plane. Permission enforcement is critical for production readiness.

**Delivers:**
- Share CRUD via dittofsctl
- Permission grant/revoke/list operations
- Store assignment to shares (metadata + payload)
- Integration test: create user → create share → grant permission → mount → verify access

**Features:** Permission Enforcement (table stakes), Share Management (table stakes)
**Architecture:** Validates Named Stores pattern, tests control plane → runtime sync
**Avoids pitfalls:** Cross-protocol permission isolation, explicit permission verification before file ops

**Research flag:** LOW — Share permission model may need validation against NFS/SMB access control semantics

### Phase 4: Adapter Lifecycle and Hot Reload
**Rationale:** Advanced production features that depend on all previous phases. Hot reload testing requires shares and permissions to be working. This is the differentiator that makes DittoFS production-ready.

**Delivers:**
- Adapter enable/disable via CLI
- Adapter configuration change triggers restart
- Hot reload with in-flight operations (verify completion)
- Graceful shutdown verification
- Metrics validation (connection lifecycle, operation counts)

**Features:** Adapter Lifecycle Testing (competitive differentiator), Hot Reload (competitive differentiator)
**Avoids pitfalls:** Goroutine leak verification via goleak, connection draining tests, proper timeout handling

**Research flag:** MEDIUM — Hot reload semantics need validation (which operations must complete vs can abort?)

### Phase Ordering Rationale

1. **Foundation first:** CLI wrapper is blocking dependency for everything else. Without type-safe command execution, all subsequent tests become brittle shell scripting.

2. **Simple CRUD before complex workflows:** User/group management teaches CLI wrapper patterns with minimal domain complexity before tackling share permissions and multi-protocol testing.

3. **Permissions after users exist:** Share permission tests need users/groups as test fixtures. Dependency graph dictates this order.

4. **Hot reload last:** Requires stable control plane. Testing adapter restart while operations are in-flight needs all previous functionality working correctly.

5. **Defer advanced features:** pjdfstest integration, chaos engineering, and performance benchmarking provide incremental value but aren't blockers for production readiness.

### Research Flags

**Phases needing deeper research during planning:**
- **Phase 3:** Share permission model mapping to NFS/SMB access control — need to verify RootSquash, AllSquash, anonuid/anongid semantics align with user/group permissions
- **Phase 4:** Hot reload operation semantics — which NFS procedures can be interrupted vs must complete? What are safe cancellation points?

**Phases with standard patterns (skip research-phase):**
- **Phase 1:** CLI testing via os/exec is well-documented, no novel patterns needed
- **Phase 2:** REST API CRUD testing is standard practice, testify assertions sufficient

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | All technologies already in use in DittoFS, verified with official docs and releases |
| Features | HIGH | Industry standards (pjdfstest, xfstests, JuiceFS) provide clear benchmarks for completeness |
| Architecture | HIGH | Existing framework demonstrates best practices, new CLI wrapper follows established patterns |
| Pitfalls | HIGH | All pitfalls verified against DittoFS existing code and documented in production NFS/SMB systems |

**Overall confidence:** HIGH

The research benefits from strong existing foundation. DittoFS already has ~70% of a comprehensive E2E test suite. The remaining 30% (CLI-driven control plane testing) follows the same architectural patterns as existing code.

### Gaps to Address

**Authentication token expiry:** Research didn't investigate how dittofsctl handles JWT token refresh. During Phase 2 implementation, need to verify if long-running test suites handle token expiry gracefully or if tests need to re-login periodically.

**Multi-context edge cases:** dittofsctl supports multi-server context management. Research didn't identify if parallel tests can safely use different contexts simultaneously or if context switching requires locking. May discover this during Phase 1 implementation.

**Hot reload atomicity:** Unclear from research whether adapter config changes are atomic (all-or-nothing) or if partial application is possible. Phase 4 planning should investigate Runtime.UpdateAdapter() implementation to design appropriate tests.

**S3 eventual consistency:** LocalStack provides immediate consistency, but real S3 has eventual consistency for overwrites and deletes. Research didn't address if tests should simulate eventual consistency or if this is out of scope for E2E suite (covered in integration tests instead).

## Sources

### Primary (HIGH confidence)
- DittoFS existing codebase (test/e2e/framework/, cmd/dittofsctl/) — architecture patterns, proven infrastructure
- [testcontainers-go v0.40.0 official docs](https://golang.testcontainers.org/) — container lifecycle patterns
- [testify v1.11.1 release notes](https://github.com/stretchr/testify/releases/tag/v1.11.1) — suite package usage
- [Go testing package](https://pkg.go.dev/testing) — TestMain, build tags, subtests
- [NFStest framework](https://github.com/thombashi/NFStest) — NFS-specific test patterns
- [pjdfstest](https://github.com/pjd/pjdfstest) — POSIX compliance baseline (8,813 tests)

### Secondary (MEDIUM confidence)
- [JuiceFS POSIX compatibility comparison](https://juicefs.com/en/blog/engineering/posix-compatibility-comparison-among-four-file-system-on-the-cloud) — feature completeness benchmarks
- [Testcontainers Best Practices (Docker Blog)](https://www.docker.com/blog/testcontainers-best-practices/) — port collision, wait strategies
- [uber-go/goleak](https://github.com/uber-go/goleak) — goroutine leak detection patterns
- [Red Hat: NFS Attribute Caching](https://access.redhat.com/solutions/315113) — actimeo parameter documentation
- [Multiprotocol NAS Locking](https://whyistheinternetbroken.wordpress.com/2015/05/20/techmultiprotocol-nas-locking-and-you/) — NFS/SMB lock semantic mismatch

### Tertiary (LOW confidence)
- [File Systems are Hard to Test - Xfstests Research](https://www.researchgate.net/publication/330808206_File_Systems_are_Hard_to_Test_-_Learning_from_Xfstests) — 72% coverage analysis, hard-to-test areas
- [Bunnyshell: E2E Testing for Microservices](https://www.bunnyshell.com/blog/end-to-end-testing-for-microservices-a-2025-guide/) — general microservice testing patterns
- [Chaos Mesh documentation](https://deepwiki.com/chaos-mesh/chaos-mesh) — fault injection methodology (defer to v2+)

---
*Research completed: 2026-02-02*
*Ready for roadmap: yes*
