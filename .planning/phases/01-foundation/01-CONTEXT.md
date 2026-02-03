# Phase 1: Foundation - Context

**Gathered:** 2026-02-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Mount/unmount CLI commands and test framework infrastructure. Users can mount shares via NFS or SMB using `dittofsctl share mount` and unmount with `dittofsctl share unmount`. Developers have a working test framework with Testcontainers for Postgres/S3 that supports parallel test execution.

</domain>

<decisions>
## Implementation Decisions

### Test Organization
- Tests organized by domain (e.g., `test/e2e/users_test.go`, `test/e2e/shares_test.go`)
- Hybrid tagging: build tags for major categories (`//go:build e2e`, `//go:build nfs`, `//go:build smb`), subtests for variations
- Tests run in parallel by default (`t.Parallel()`)
- Minimal output on success, verbose output opt-in via flag for debugging

### Container Lifecycle
- Shared containers (one Postgres, one S3/Localstack) started in TestMain
- Namespace isolation for parallel safety: unique Postgres schema and S3 prefix per test
- Fail fast if containers don't start — no skip, no retry
- Always cleanup containers after test run completes

### Mount CLI Interface
- Protocol specified via explicit flag: `dittofsctl share mount --protocol nfs /myshare /mnt/myshare`
- Mount point always user-specified (required argument)
- Minimal success output: "Mounted /myshare at /mnt/myshare"
- Error messages include actionable suggestions (e.g., "Is the NFS adapter running?")

### Test Helper Patterns
- Test fixture struct with per-test scopes (`TestEnvironment` in TestMain, `NewScope(t)` per test)
- `Must*` pattern for helpers — fail test immediately on error (e.g., `MustCreateUser`, `MustMount`)
- Descriptive test data names (e.g., `test-user-alice`, `test-share-archive`)
- Helpers live in `test/e2e/helpers/` package

### Claude's Discretion
- Exact Testcontainer configuration (ports, images, timeouts)
- Helper function signatures and return types
- Test assertion library usage (standard testify vs custom)
- Exact verbose logging format

</decisions>

<specifics>
## Specific Ideas

- Namespace isolation pattern: each test creates unique Postgres schema (`test_${testName}_${uuid}`) and S3 prefix (`test-${uuid}/`)
- TestEnvironment pattern: shared state in TestMain, scoped access per test for parallel execution
- Error suggestions should be contextual — "Is the NFS adapter running?" for connection refused, "Does the share exist?" for not found

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-foundation*
*Context gathered: 2026-02-02*
