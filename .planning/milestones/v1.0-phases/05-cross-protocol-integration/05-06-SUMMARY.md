# Plan 05-06 Summary: E2E SMB Mount Support

## Status: COMPLETE (Simplified)

**Duration:** ~5 min
**Gap Closure:** Yes (closes VERIFICATION.md gap)
**Scope Change:** Docker fallback removed per user feedback (proven unreliable)

## Objective

Enable cross-protocol E2E tests to handle missing SMB mount capability gracefully.

**Original goal:** Docker-based CIFS mount fallback for systems without native SMB support.
**Actual delivery:** Graceful test skipping when native SMB mount unavailable.

## Scope Change Rationale

User feedback indicated Docker-based CIFS mount was previously tested and proved unreliable:
> "We tried to test SMB CIFS with Docker in the past. And we failed. We may also skip this part, we can continue to test SMB with native mount on macos and linux..."

The simplified approach:
- Tests run on systems with native SMB mount (macOS, Linux with cifs-utils, Windows)
- Tests skip gracefully on systems without SMB mount capability
- No Docker dependency needed

## Completed Tasks

### Task 1: Add SkipIfNoSMBMount helper
**Commit:** `87e385a`

Added graceful skip helper to `test/e2e/framework/mount.go`:
- `IsNativeSMBAvailable()` - checks for native SMB mount capability per platform
- `SkipIfNoSMBMount(t)` - skips test with informative message
- Platform support: Windows (always), macOS (mount_smbfs), Linux (mount.cifs)

### Task 2: Update cross-protocol tests
**Commit:** `4174238`

Added skip conditions to cross-protocol locking tests:
- `TestCrossProtocolLocking` - skips if no SMB mount
- `TestCrossProtocolLockingByteRange` - skips if no SMB mount

## Files Modified

| File | Changes |
|------|---------|
| `test/e2e/framework/mount.go` | Added `IsNativeSMBAvailable`, `SkipIfNoSMBMount`, `fileExists` |
| `test/e2e/cross_protocol_lock_test.go` | Added `framework.SkipIfNoSMBMount(t)` calls |

## Reverted Changes

| Commit | Description |
|--------|-------------|
| `1156937` | Docker-based CIFS mount fallback (reverted in `43770cb`) |

Files removed by revert:
- `test/e2e/framework/docker_cifs.go` (250+ lines Docker mount code)

## Verification

1. Build verification: `go build -tags=e2e ./test/e2e/...` succeeds
2. Tests skip gracefully when SMB mount unavailable
3. Tests run normally when native SMB mount available
4. No Docker dependency

## Key Decisions

- **No Docker fallback**: Docker CIFS mount proved unreliable in testing
- **Graceful skip over failure**: Tests skip with informative message rather than failing
- **Platform-specific checks**: Each OS checked for its native SMB mount command

## Platform Support

| Platform | SMB Mount Tool | Availability |
|----------|----------------|--------------|
| Windows | net use (built-in) | Always available |
| macOS | mount_smbfs | Always available |
| Linux | mount.cifs (cifs-utils) | Requires package install |

## Notes

The original plan was to provide Docker-based CIFS mount as a fallback. After implementing and user feedback about past failures, the approach was simplified to graceful skipping. Cross-protocol tests will run on developer machines with native SMB support and skip in CI environments without it.

For full cross-protocol testing, run on:
- macOS (has mount_smbfs built-in)
- Windows (has SMB built-in)
- Linux with cifs-utils installed (`apt install cifs-utils` or equivalent)
