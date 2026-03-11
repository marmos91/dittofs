---
phase: 32-windows-integration-testing
plan: 01
subsystem: smb
tags: [smb2, windows, create-context, mxac, qfid, file-info, cross-platform, ci]

# Dependency graph
requires:
  - phase: 31-windows-acl-support
    provides: SMB security descriptor support and NT ACL translation
  - phase: 30-smb-bug-fixes
    provides: Sparse file zero-fill fix and path traversal improvements
provides:
  - MxAc create context response encoding (maximal access from POSIX permissions)
  - QFid create context response encoding (on-disk file ID + volume ID)
  - FileCompressionInformation and FileAttributeTagInformation handlers
  - FILE_SUPPORTS_SPARSE_FILES capability flag
  - Windows %APPDATA% path support in config and control plane store
  - Windows CI with race detection
affects: [32-02, 32-03, windows-testing, smb-conformance]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "POSIX mode bits to SMB access mask translation"
    - "Cross-platform config directory resolution (APPDATA vs XDG)"

key-files:
  created:
    - pkg/controlplane/store/config_test.go
  modified:
    - internal/adapter/smb/v2/handlers/create.go
    - internal/adapter/smb/v2/handlers/create_test.go
    - internal/adapter/smb/v2/handlers/query_info.go
    - internal/adapter/smb/v2/handlers/query_info_test.go
    - internal/adapter/smb/types/constants.go
    - internal/adapter/smb/v2/handlers/session_setup.go
    - pkg/config/config.go
    - pkg/config/config_test.go
    - pkg/controlplane/store/gorm.go
    - .github/workflows/windows-build.yml

key-decisions:
  - "computeMaximalAccess checks owner UID first, then group membership, then other bits"
  - "QFid uses ServerGUID as VolumeId (consistent with FileFsObjectIdInformation)"
  - "Store config unit tests created in separate config_test.go (existing store_test.go has integration build tag)"

patterns-established:
  - "POSIX-to-SMB access mask: owner=GENERIC_ALL, group/other from rwx bits"
  - "Cross-platform path resolution: APPDATA on Windows, XDG_CONFIG_HOME on Unix"

requirements-completed: [WIN-02, WIN-03, WIN-05, WIN-06, WIN-07, WIN-08, WIN-10]

# Metrics
duration: 6min
completed: 2026-02-28
---

# Phase 32 Plan 01: Windows Protocol Compatibility Summary

**MxAc/QFid create contexts, FileCompressionInformation/FileAttributeTagInformation handlers, sparse files capability flag, and cross-platform path fixes for Windows server hosting**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-28T06:58:12Z
- **Completed:** 2026-02-28T07:04:30Z
- **Tasks:** 2
- **Files modified:** 11

## Accomplishments
- MxAc create context returns 8-byte response with maximal access mask computed from POSIX file permissions
- QFid create context returns 32-byte response with file UUID and ServerGUID as volume ID
- FileCompressionInformation (class 28) and FileAttributeTagInformation (class 35) return valid fixed-size buffers
- FileFsAttributeInformation flags updated from 0x8F to 0xCF (adds FILE_SUPPORTS_SPARSE_FILES)
- Guest signing behavior documented with Windows 11 24H2 GPO note
- Config and database paths resolve correctly on Windows via %APPDATA%
- Windows CI enhanced with -race flag and build summary

## Task Commits

Each task was committed atomically:

1. **Task 1: Add MxAc/QFid create contexts, FileInfoClass handlers, and capability flags** - `532ba9ac` (feat)
2. **Task 2: Cross-platform path fixes and Windows CI enhancement** - `7b6dc2ea` (feat)

## Files Created/Modified
- `internal/adapter/smb/types/constants.go` - Added FileCompressionInformation (28) and FileAttributeTagInformation (35) enum values
- `internal/adapter/smb/v2/handlers/create.go` - MxAc/QFid create context response encoding + computeMaximalAccess function
- `internal/adapter/smb/v2/handlers/create_test.go` - Tests for MxAc access computation and QFid response format
- `internal/adapter/smb/v2/handlers/query_info.go` - FileCompressionInformation and FileAttributeTagInformation handlers, updated FS attribute flags
- `internal/adapter/smb/v2/handlers/query_info_test.go` - Tests for compression and attribute tag info classes
- `internal/adapter/smb/v2/handlers/session_setup.go` - Windows 11 24H2 GPO documentation on guest sessions
- `pkg/config/config.go` - Windows APPDATA path support in getConfigDir()
- `pkg/config/config_test.go` - Platform-aware config directory tests
- `pkg/controlplane/store/gorm.go` - Windows APPDATA path support in SQLite default path
- `pkg/controlplane/store/config_test.go` - Unit tests for ApplyDefaults path resolution
- `.github/workflows/windows-build.yml` - Added -race flag and build summary step

## Decisions Made
- computeMaximalAccess checks owner UID first (grants GENERIC_ALL), then group membership, then other permission bits
- QFid uses ServerGUID as VolumeId (consistent with existing FileFsObjectIdInformation handler)
- Store config unit tests placed in separate config_test.go file since existing store_test.go has integration build tag

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Created separate unit test file for store config tests**
- **Found during:** Task 2
- **Issue:** Plan specified adding tests to store_test.go, but that file has `//go:build integration` tag which prevents running with `go test ./...`
- **Fix:** Created config_test.go without build tag for unit-testable ApplyDefaults path logic
- **Files modified:** pkg/controlplane/store/config_test.go
- **Verification:** Tests run successfully with `go test ./pkg/controlplane/store/ -run TestApplyDefaults`
- **Committed in:** 7b6dc2ea

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Necessary to make tests actually runnable in CI. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- SMB protocol compatibility fixes complete for Windows 11 Explorer and cmd.exe
- Cross-platform path fixes enable DittoFS server hosting on Windows
- Ready for Plan 02 (Windows integration testing infrastructure)

## Self-Check: PASSED

All 12 files verified present. Both task commits (532ba9ac, 7b6dc2ea) verified in git log.

---
*Phase: 32-windows-integration-testing*
*Completed: 2026-02-28*
