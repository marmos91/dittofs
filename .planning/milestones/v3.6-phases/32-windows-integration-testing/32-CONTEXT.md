# Phase 32: Windows Integration Testing - Context

**Gathered:** 2026-02-27
**Status:** Ready for planning

<domain>
## Phase Boundary

Comprehensive conformance validation ensuring DittoFS works correctly with Windows 11 clients and passes industry-standard test suites (smbtorture + WPTS). Includes protocol-level fixes (MxAc, QFid, FileInfoClass, guest signing, capability flags), smbtorture integration, Unix path fixes for cross-platform server support, and manual Windows 11 validation. Does NOT include fixing smbtorture failures beyond what's already scoped in the success criteria -- failure fixing happens in v3.8 phases.

</domain>

<decisions>
## Implementation Decisions

### smbtorture Scope
- Run the **full SMB2 suite** (all smb2.* tests), not just basic+ACL
- **Docker isolation** for GPL compliance -- smbtorture runs in a separate container, no GPL code in DittoFS repo
- Use **official Samba Docker image** with a **pinned tag** (not `latest`)
- **Advisory CI** with known-failures model -- same approach as WPTS, no PR blocking on expected failures
- **Document baseline only** -- run full suite, record pass/fail, update KNOWN_FAILURES.md; fixing failures is deferred to v3.8
- **Same CI workflow as WPTS** (smb-conformance.yml), separate job -- both suites share triggers and run in parallel
- **Unified CI artifacts** -- both WPTS TRX and smbtorture output archived together in the same workflow run
- **Document baseline with no pass rate target** -- target pass rates set in future phases

### Windows Validation
- **Extended application testing** -- beyond Explorer/cmd/PowerShell, also test Office (Word/Excel save/open) and VS Code (project on share)
- **Both NFS and SMB from Windows** -- test Windows built-in NFS client (Services for NFS) alongside SMB
- **Formal versioned checklist** in repo -- reusable template that grows with each milestone (v3.8, v4.0)
- **Windows VM setup guide** in repo -- document how to set up Windows 11 VM for testing DittoFS
- **Test both mapped drives AND UNC paths** -- `net use Z: \\server\share` (persistent) and direct `\\server\share`
- **Explicit limitations section** -- document known limitations (no ADS, no Change Notify) alongside what works
- **Realistic file sizes** -- test with typical files: 1-50MB documents, 100MB+ Excel/PPT
- **Whatever Windows 11 version is available** -- test on available version, document which one
- **Office + VS Code** as must-test apps; WSL and full Visual Studio are out of scope

### Unix Path Strategy
- Fix paths in **SMB adapter + config/WAL/credentials** -- not just wire protocol, also internal paths
- **Full server on Windows** -- DittoFS should start and serve NFS/SMB from a Windows host
- **All storage backends** on Windows -- memory, filesystem, BadgerDB, S3
- **Platform-native config paths** -- XDG_CONFIG_HOME on Linux/macOS, %APPDATA% on Windows
- **Best-effort NFS on Windows** -- fix path issues, try to start NFS listener; if it works great, if not document the limitation
- **Platform-specific mmap** -- leverage existing mmap_windows.go, ensure WAL paths use filepath.Join
- **Windows CI = build + unit tests only** -- no actual server start in CI, just compile and run short tests

### Test Infrastructure
- **Mirror WPTS pattern** -- docker-compose.yml, bootstrap.sh, run.sh, parse results
- **Location**: `test/smb-conformance/smbtorture/` -- sibling directory to WPTS under smb-conformance
- **Makefile targets** for local dev -- developers can run smbtorture locally before pushing
- **Separate KNOWN_FAILURES.md** -- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md`
- **Same summary format as WPTS** -- pass/fail/skip counts, list of new failures, comparison against known failures

### Claude's Discretion
- Exact smbtorture Docker image and tag selection
- smbtorture output parsing implementation details
- Internal architecture of cross-platform path abstraction
- How to structure the Windows VM setup guide
- Compression/attribute tag info handler implementation details

</decisions>

<specifics>
## Specific Ideas

- smbtorture is different from WPTS: WPTS = wire protocol conformance ("does encoding match spec?"), smbtorture = client behavior ("does a real SMB client work?"). Both are valuable and complementary.
- The WPTS infrastructure from Phase 29.8 is the template to follow -- same patterns for docker-compose, bootstrap, result parsing, CI workflow
- Existing Windows build files (mmap_windows.go, daemon_windows.go, mount_windows.go, etc.) provide a starting point for cross-platform support
- Phase 29.8 baseline was 133/240 WPTS BVT tests passing; success criteria requires maintaining >= 150

</specifics>

<deferred>
## Deferred Ideas

None -- discussion stayed within phase scope

</deferred>

---

*Phase: 32-windows-integration-testing*
*Context gathered: 2026-02-27*
