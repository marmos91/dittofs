---
phase: 06
plan: 04
subsystem: e2e-testing
tags: [permission-enforcement, smb, access-control, e2e]
dependency-graph:
  requires: ["06-02"]
  provides: ["permission-enforcement-tests"]
  affects: []
tech-stack:
  added: []
  patterns: ["MountSMBWithError-helper", "default-permission-none", "group-membership-tests"]
files:
  key-files:
    created:
      - "test/e2e/permission_enforcement_test.go"
    modified:
      - "test/e2e/framework/mount.go"
decisions:
  - id: ENF-SMB-ONLY
    choice: "Use SMB for permission enforcement tests, not NFS"
    rationale: "NFS AUTH_UNIX is UID-based, doesn't enforce DittoFS user permissions"
  - id: DEFAULT-PERMISSION-NONE
    choice: "Use default_permission: none on test share"
    rationale: "Ensures users must have explicit grants to access share"
  - id: REMOUNT-FOR-PERMISSION-CHANGE
    choice: "Remount SMB after permission changes"
    rationale: "OS may cache SMB authentication, remount ensures fresh session"
metrics:
  duration: "3 min"
  completed: "2026-02-02"
---

# Phase 6 Plan 04: Permission Enforcement Tests Summary

Permission enforcement E2E tests validating DittoFS access control via SMB protocol.

## One-Liner

MountSMBWithError helper and 4 permission enforcement tests (ENF-01 through ENF-04) using SMB with default_permission: none.

## What Changed

### Files Created
- `test/e2e/permission_enforcement_test.go` (342 lines)
  - TestPermissionEnforcement main function
  - ENF-01: testReadOnlyUserCannotWrite
  - ENF-02: testNoAccessUserCannotRead
  - ENF-03: testUserRemovedFromGroupLosesPermissions
  - ENF-04: testPermissionChangeEffectImmediate

### Files Modified
- `test/e2e/framework/mount.go`
  - Added MountSMBWithError function (78 lines)

## Implementation Details

### MountSMBWithError Helper

Added new function for permission testing scenarios where mount is expected to fail:

```go
func MountSMBWithError(t *testing.T, port int, creds SMBCredentials) (*Mount, error)
```

- Same implementation as MountSMB but returns error instead of calling t.Fatal
- Enables testing "mount should fail" scenarios (ENF-02, ENF-03)
- Follows same retry logic (3 attempts) as MountSMB

### Test Structure

TestPermissionEnforcement sets up shared infrastructure:
1. Server with memory stores
2. Share with `default_permission: "none"` (deny by default)
3. NFS adapter for admin file setup
4. SMB adapter for permission-enforced testing

### ENF-01: Read-Only User Cannot Write

- Creates user with `read` permission only
- Verifies reading files succeeds
- Verifies writing new files fails
- Verifies modifying existing files fails

### ENF-02: No-Access User Cannot Read

- Creates user with no explicit permission
- Default permission `none` applies
- Uses MountSMBWithError for expected failure
- Passes if mount fails OR operations fail

### ENF-03: User Removed From Group Loses Permissions

- Creates group with `read-write` permission
- User added to group can write files
- After removal from group, write fails
- Uses MountSMBWithError for graceful handling

### ENF-04: Permission Change Takes Effect

- User starts with `read` permission (write fails)
- Permission upgraded to `read-write`
- After remount, write succeeds
- Note: Requires remount due to OS session caching

## Decisions Made

| Decision | Choice | Rationale |
|----------|--------|-----------|
| ENF-SMB-ONLY | Use SMB for permission tests | NFS AUTH_UNIX is UID-based, doesn't enforce DittoFS user permissions |
| DEFAULT-PERMISSION-NONE | Use default_permission: none | Ensures explicit grants required for access |
| REMOUNT-FOR-PERMISSION-CHANGE | Remount after permission upgrade | OS may cache SMB session, remount ensures fresh session |

## Deviations from Plan

None - plan executed exactly as written.

## Verification Results

- Build: `go build -tags=e2e ./test/e2e/...` - PASS
- Vet: `go vet -tags=e2e ./test/e2e/permission_enforcement_test.go` - PASS
- Short mode: `go test -tags=e2e -run TestPermissionEnforcement ./test/e2e/ -short` - SKIP (as designed)
- Line count: 342 lines (exceeds 200 minimum)

**Note:** Full test execution requires sudo for mount operations. Tests compile and pass vet checks. Actual test execution should be performed in environment with sudo access.

## Test Execution Requirements

```bash
# Full test run (requires sudo for mount)
sudo go test -tags=e2e -v -run TestPermissionEnforcement ./test/e2e/
```

Tests require:
- Root/sudo access for mount operations
- SMB client (mount_smbfs on macOS, mount.cifs on Linux)
- NFS client (for admin file setup)

## Commits

| Hash | Type | Description |
|------|------|-------------|
| 0047fbf | feat | Add MountSMBWithError helper for permission testing |
| 6cd6b8c | test | Add permission enforcement E2E tests |

## Next Phase Readiness

Phase 6 permission enforcement tests are complete. This plan covers:
- ENF-01: Read-only user cannot write
- ENF-02: No-access user cannot read
- ENF-03: User removed from group loses permissions
- ENF-04: Permission change takes effect

Remaining Phase 6 plans:
- 06-05: Store matrix tests (MTX-01 through MTX-09)
