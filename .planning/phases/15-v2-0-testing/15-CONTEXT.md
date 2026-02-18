# Phase 15: v2.0 Testing - Context

**Gathered:** 2026-02-17
**Status:** Ready for planning

<domain>
## Phase Boundary

Comprehensive E2E testing for all NFSv4.0 functionality built in Phases 6-14. Validates NFSv4 basic operations, locking, delegations, Kerberos authentication, ACLs, control plane v2.0, and backward compatibility with NFSv3. Also extends POSIX compliance tests (pjdfstest) to NFSv4 per issue #122.

</domain>

<decisions>
## Implementation Decisions

### Test Environment
- Docker is a required dependency for E2E tests (KDC, Localstack, PostgreSQL containers)
- KDC already exists: Docker testcontainer with MIT Kerberos in `test/e2e/framework/kerberos.go` — reuse it
- Reuse existing Localstack setup from v1.0 E2E tests for S3 backend testing
- PostgreSQL testcontainer included for full persistent metadata path testing
- PostgreSQL uses GORM AutoMigrate (same as production) — tests validate the real migration path
- Full storage backend matrix: all combinations of metadata (memory/badger/postgres) x payload (memory/filesystem/s3)
- Both single-share and multi-share configurations tested
- Concurrent multi-share mounts tested (two shares mounted simultaneously)
- Always clean up containers and mounts on test failure — CI-friendly
- E2E tests run automatically in CI on every PR/push
- Global test timeout: 30 minutes
- Include server restart/recovery test (graceful shutdown + restart + state reclaim)
- Include client reconnection test after brief network disruption

### NFSv4 Mount Strategy
- Full v3 + v4.0 matrix: every test runs against both NFSv3 and NFSv4.0 mounts
- Use explicit `vers=4.0` (not `vers=4`) for NFSv4 mounts — no ambiguity
- Both macOS and Linux supported with graceful skipping (macOS skips NFSv4-specific features like delegations)
- Linux CI only — macOS tested manually by developers
- Kerberos mounts tested with all three security flavors: `sec=krb5`, `sec=krb5i`, `sec=krb5p`
- Pseudo-filesystem browsing verified (ls on NFSv4 root shows pseudo-fs tree and share junctions)
- 30-second mount timeout
- Test squash behavior (root_squash, all_squash) at mount level
- Test stale NFS file handles (access after server restart with memory backend → ESTALE)

### Coverage Depth
- Full lock matrix: read/write locks, overlapping ranges, lock upgrade, blocking locks, cross-client conflicts
- Full delegation cycle: grant delegation, open conflicting client, verify recall, verify data flush — multi-client test
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
- Golden path smoke test: server start → create user → create share → mount → write/read
- Moderate directory test: ~100 files to verify READDIR pagination
- Stress tests gated behind `-tags=stress` build tag (e.g., 100+ files with delegations under load)
- Skip metrics endpoint verification (tested at unit level)
- E2E coverage profile generated (`-coverprofile`) for merge with unit test coverage

### Test Organization
- NFSv4 E2E tests live in existing `test/e2e/` directory (same as v3 tests)
- Table-driven subtests with NFS version as parameter: `TestXxx/v3`, `TestXxx/v4`
- pjdfstest stays in separate `test/posix/` suite with `--nfs-version` parameter (per issue #122)
- Extend existing `test/e2e/helpers/` and `test/e2e/framework/` packages for NFSv4 helpers
- Standard `go test -v` output — no JUnit XML or custom reporting
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

</decisions>

<specifics>
## Specific Ideas

- Issue #122 tasks: add `--nfs-version` to `test/posix/setup-posix.sh`, create `known_failures_v4.txt`, update `run-posix.sh`, document initial pass/fail breakdown, update `test/posix/README.md`
- "Run full v1.0 suite" for backward compat — existing v1.0 E2E tests should still pass unchanged
- Stress tests use separate `-tags=stress` build tag so they can be skipped in CI when needed
- Backend matrix pragmatics: full version x backend cross-product for I/O tests; memory/memory for protocol feature tests

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 15-v2-0-testing*
*Context gathered: 2026-02-17*
