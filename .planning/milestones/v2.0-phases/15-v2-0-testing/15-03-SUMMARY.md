---
phase: 15-v2-0-testing
plan: 03
subsystem: testing
tags: [nfsv4, acl, kerberos, e2e, nfs4-acl-tools, vers4.0, rpcsec_gss]

# Dependency graph
requires:
  - phase: 15-01
    provides: "NFSv4 E2E framework (MountNFSWithVersion, SkipIfDarwin, SkipIfNoNFS4ACLTools, setupNFSv4TestServer)"
provides:
  - "NFSv4 ACL E2E tests (set/read/enforce/inherit via nfs4_setfacl/nfs4_getfacl)"
  - "NFSv4 Kerberos extended E2E tests with explicit vers=4.0 mounts"
  - "Cross-protocol ACL interop test (NFSv4 -> SMB, skips when unavailable)"
  - "Multi-flavor Kerberos test (krb5/krb5i/krb5p with vers=4.0)"
affects: [15-v2-0-testing]

# Tech tracking
tech-stack:
  added: [nfs4-acl-tools (nfs4_setfacl, nfs4_getfacl)]
  patterns: [local mount wrapper for unexported framework fields, vers=4.0 explicit mount options]

key-files:
  created:
    - test/e2e/nfsv4_acl_test.go
    - test/e2e/nfsv4_kerberos_test.go
  modified: []

key-decisions:
  - "Local krbV4Mount type instead of framework.KerberosMount to avoid unexported field access"
  - "All Kerberos v4 mounts use vers=4.0 explicitly (never vers=4) per locked decision #5"
  - "Cross-protocol ACL test uses MountSMBWithError for graceful skip on SMB unavailability"

patterns-established:
  - "nfs4SetACL/nfs4AddACE/nfs4GetACL helpers for CLI-based ACL manipulation in E2E tests"
  - "krbV4Mount local type for vers=4.0 Kerberos mounts with proper cleanup"

requirements-completed: [TEST2-04, TEST2-05]

# Metrics
duration: 5min
completed: 2026-02-17
---

# Phase 15 Plan 03: NFSv4 ACL and Kerberos E2E Tests Summary

**NFSv4 ACL lifecycle tests via nfs4_setfacl/nfs4_getfacl and Kerberos E2E tests with explicit vers=4.0 covering all three security flavors (krb5/krb5i/krb5p)**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-17T17:13:37Z
- **Completed:** 2026-02-17T17:19:32Z
- **Tasks:** 2
- **Files created:** 2

## Accomplishments
- NFSv4 ACL lifecycle E2E tests: set/read ACLs via nfs4_setfacl/nfs4_getfacl, deny ACE enforcement, ACL inheritance with FILE_INHERIT/DIRECTORY_INHERIT, chmod interop, and cross-protocol ACL interop (NFSv4 -> SMB)
- NFSv4 Kerberos extended E2E tests with explicit vers=4.0: authorization denial for unmapped users, file ownership mapping, all three Kerberos flavors (krb5/krb5i/krb5p), AUTH_SYS fallback, and concurrent users
- Helper functions (nfs4SetACL, nfs4AddACE, nfs4GetACL) for CLI-based ACL manipulation
- Local krbV4Mount type with proper cleanup for vers=4.0 Kerberos mounts

## Task Commits

Each task was committed atomically:

1. **Task 1: NFSv4 ACL E2E tests** - `dbb39f5` (feat)
2. **Task 2: NFSv4 Kerberos extended E2E tests with explicit vers=4.0** - `c50575a` (feat)

## Files Created/Modified
- `test/e2e/nfsv4_acl_test.go` - NFSv4 ACL E2E tests: lifecycle (set/read/deny), inheritance (file/dir), access enforcement (restrictive ACL, chmod interop), cross-protocol interop (NFSv4 -> SMB)
- `test/e2e/nfsv4_kerberos_test.go` - NFSv4 Kerberos extended E2E tests: authorization denial, file ownership mapping, multi-flavor v4 (krb5/krb5i/krb5p with vers=4.0), AUTH_SYS fallback, concurrent users

## Decisions Made
- Used local `krbV4Mount` type instead of `framework.KerberosMount` because the framework types have unexported fields (`mounted`, `secFlavor`) that cannot be set from outside the framework package. The local type includes proper cleanup via direct `umount` exec calls.
- All Kerberos vers=4.0 mounts use `vers=4.0` in mount options (verified via grep -- no `vers=4` without `.0` in any mount option string). This satisfies locked decision #5.
- Cross-protocol ACL test (TestNFSv4ACLCrossProtocol) uses `framework.MountSMBWithError` to gracefully handle SMB mount failure with explicit skip message, rather than `t.Fatal`.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- NFSv4 ACL and Kerberos E2E test coverage complete
- All tests skip gracefully on unsupported platforms (macOS, missing nfs4-acl-tools, missing Kerberos prereqs)
- Ready for Plan 15-04 (next wave of testing)

## Self-Check: PASSED

All files verified present, all commits verified in git log.

---
*Phase: 15-v2-0-testing*
*Completed: 2026-02-17*
