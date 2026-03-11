# Phase 32: Windows Integration Testing - Research

**Researched:** 2026-02-27
**Domain:** SMB conformance testing (smbtorture + WPTS), Windows client validation, cross-platform Go server, protocol-level fixes
**Confidence:** HIGH

## Summary

Phase 32 covers four distinct workstreams: (1) protocol-level fixes to improve Windows 11 compatibility (MxAc/QFid create contexts, FileCompressionInformation/FileAttributeTagInformation handlers, guest signing, FileFsAttributeInformation flags), (2) smbtorture test suite integration using Docker-isolated Samba toolbox container, (3) cross-platform path fixes so DittoFS can build and run on Windows, and (4) manual Windows 11 validation with a formal checklist.

The WPTS infrastructure from Phase 29.8 provides a proven template for the smbtorture integration -- same Docker Compose patterns, result parsing scripts, KNOWN_FAILURES.md model, and CI workflow structure. The protocol fixes are well-scoped: MxAc is 8 bytes (QueryStatus + MaximalAccess), QFid is 32 bytes (opaque ID), and the two missing FileInfoClass handlers return fixed-size structures. The cross-platform path work is narrowly scoped to `getConfigDir()` in config.go and the SQLite default path in gorm.go -- the credential store and state directory already have Windows support.

**Primary recommendation:** Mirror the existing WPTS infrastructure exactly for smbtorture (same directory structure, same CI workflow with parallel jobs, same KNOWN_FAILURES.md pattern). Implement protocol fixes as focused handler modifications with unit tests. Cross-platform path fixes use `runtime.GOOS` guards matching the existing pattern in `credentials/store.go`.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Run the **full SMB2 suite** (all smb2.* tests), not just basic+ACL
- **Docker isolation** for GPL compliance -- smbtorture runs in a separate container, no GPL code in DittoFS repo
- Use **official Samba Docker image** with a **pinned tag** (not `latest`)
- **Advisory CI** with known-failures model -- same approach as WPTS, no PR blocking on expected failures
- **Document baseline only** -- run full suite, record pass/fail, update KNOWN_FAILURES.md; fixing failures is deferred to v3.8
- **Same CI workflow as WPTS** (smb-conformance.yml), separate job -- both suites share triggers and run in parallel
- **Unified CI artifacts** -- both WPTS TRX and smbtorture output archived together in the same workflow run
- **Document baseline with no pass rate target** -- target pass rates set in future phases
- **Extended application testing** -- beyond Explorer/cmd/PowerShell, also test Office (Word/Excel save/open) and VS Code (project on share)
- **Both NFS and SMB from Windows** -- test Windows built-in NFS client (Services for NFS) alongside SMB
- **Formal versioned checklist** in repo -- reusable template that grows with each milestone (v3.8, v4.0)
- **Windows VM setup guide** in repo -- document how to set up Windows 11 VM for testing DittoFS
- **Test both mapped drives AND UNC paths** -- `net use Z: \\server\share` (persistent) and direct `\\server\share`
- **Explicit limitations section** -- document known limitations (no ADS, no Change Notify) alongside what works
- **Realistic file sizes** -- test with typical files: 1-50MB documents, 100MB+ Excel/PPT
- **Whatever Windows 11 version is available** -- test on available version, document which one
- **Office + VS Code** as must-test apps; WSL and full Visual Studio are out of scope
- Fix paths in **SMB adapter + config/WAL/credentials** -- not just wire protocol, also internal paths
- **Full server on Windows** -- DittoFS should start and serve NFS/SMB from a Windows host
- **All storage backends** on Windows -- memory, filesystem, BadgerDB, S3
- **Platform-native config paths** -- XDG_CONFIG_HOME on Linux/macOS, %APPDATA% on Windows
- **Best-effort NFS on Windows** -- fix path issues, try to start NFS listener; if it works great, if not document the limitation
- **Platform-specific mmap** -- leverage existing mmap_windows.go, ensure WAL paths use filepath.Join
- **Windows CI = build + unit tests only** -- no actual server start in CI, just compile and run short tests
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

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| WIN-02 | MxAc (Maximal Access) create context response returned to clients | MS-SMB2 2.2.14.2.5 defines 8-byte response (QueryStatus + MaximalAccess). `FindCreateContext` helper already exists. Add to CREATE handler after lease context processing. |
| WIN-03 | QFid (Query on Disk ID) create context response returned to clients | MS-SMB2 2.2.14.2.9 defines 32-byte response (DiskFileId + VolumeId). Use file handle bytes padded to 32. Add alongside MxAc in CREATE handler. |
| WIN-05 | Remaining FileInfoClass handlers (FileCompressionInformation, FileAttributeTagInformation) | MS-FSCC 2.4.9 (28 bytes, all zeros for no compression) and MS-FSCC 2.4.6 (8 bytes, FileAttributes + ReparseTag). Add cases to `buildFileInfo()` switch. |
| WIN-06 | Guest access signing negotiation for Windows 11 24H2 | Guest sessions have no session key for signing. Samba approach: SESSION_SETUP response includes SESSION_FLAG_IS_GUEST and signing is NOT required for guest. Document that Win11 24H2 blocks insecure guest logons by default (requires GPO change). |
| WIN-07 | FileFsAttributeInformation capability flags updated | Current flags are 0x0000008F. Should add FILE_SUPPORTS_SPARSE_FILES (0x40), FILE_NAMED_STREAMS (0x40000) for stream stub. Review and document what each bit means. |
| WIN-08 | Windows CI build step in GitHub Actions | Already exists at `.github/workflows/windows-build.yml`. Verify it runs on PRs, add `-race` flag if feasible, ensure it stays green. |
| WIN-09 | NFS and SMB client compatibility validated from Windows | Manual testing with formal checklist. Test mapped drives + UNC paths, Explorer/cmd/PowerShell/Office/VS Code. |
| WIN-10 | Hardcoded Unix paths fixed for Windows compatibility | Two locations need fixes: `getConfigDir()` in `pkg/config/config.go` (line 501, no Windows check) and SQLite default path in `pkg/controlplane/store/gorm.go` (line 80, no Windows check). Pattern from `credentials/store.go` shows how. |
| TEST-01 | smbtorture SMB2 test suite run against DittoFS | Docker container `quay.io/samba.org/samba-toolbox:v0.8` (Samba 4.22.6, Fedora 42). Run full `smb2` suite. Mirror WPTS infrastructure pattern. |
| TEST-02 | Newly-revealed conformance failures fixed iteratively | Document baseline only per CONTEXT.md. Fixing deferred to v3.8. |
| TEST-03 | KNOWN_FAILURES.md updated with current pass/fail status | Two separate files: existing `test/smb-conformance/KNOWN_FAILURES.md` (WPTS) and new `test/smb-conformance/smbtorture/KNOWN_FAILURES.md`. |
</phase_requirements>

## Standard Stack

### Core

| Tool/Image | Version | Purpose | Why Standard |
|------------|---------|---------|--------------|
| quay.io/samba.org/samba-toolbox | v0.8 (Samba 4.22.6) | smbtorture test runner | Official Samba project image with smbtorture included. Pinned tag for reproducibility. |
| Docker Compose | 2.x | Test orchestration | Same pattern as existing WPTS infrastructure |
| GitHub Actions (ubuntu-latest + windows-latest) | - | CI execution | Already in use for WPTS and Windows build |
| xmlstarlet | - | TRX parsing (WPTS) | Already used in parse-results.sh |

### Supporting

| Tool | Version | Purpose | When to Use |
|------|---------|---------|-------------|
| bash/shell | - | smbtorture output parsing, run scripts | Parse subunit/TAP output from smbtorture |
| subunit2csv / python-subunit | - | smbtorture result format conversion | smbtorture uses subunit format, may need conversion |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| quay.io/samba.org/samba-toolbox | cmd.cat/smbtorture | cmd.cat is minimal but not officially maintained by Samba project |
| samba-toolbox:v0.8 | samba-toolbox:latest | Pinned tag is required by user decision for reproducibility |
| Full smb2 suite | Selected tests only | User decided full suite; document baseline, don't fix |

## Architecture Patterns

### smbtorture Test Infrastructure Layout

```
test/smb-conformance/
├── docker-compose.yml          # Updated: add smbtorture service/job
├── Makefile                    # Updated: add smbtorture targets
├── KNOWN_FAILURES.md           # WPTS known failures (existing)
├── run.sh                      # WPTS run script (existing)
├── parse-results.sh            # WPTS result parser (existing)
├── bootstrap.sh                # Shared bootstrap (existing)
├── configs/                    # Shared configs (existing)
└── smbtorture/
    ├── run.sh                  # smbtorture run script (NEW)
    ├── parse-results.sh        # smbtorture output parser (NEW)
    ├── KNOWN_FAILURES.md       # smbtorture known failures (NEW)
    └── Makefile                # smbtorture convenience targets (NEW)
```

### Pattern 1: smbtorture Docker Execution

**What:** Run smbtorture from Docker container against DittoFS running in separate container.
**When to use:** Every CI run and local development.
**Example:**
```bash
# smbtorture invocation inside Docker container
smbtorture //dittofs/smbbasic \
    -U "wpts-admin%TestPassword01!" \
    -p 445 \
    --option="client min protocol=SMB2_02" \
    --option="client max protocol=SMB3" \
    smb2
```

The output format is subunit (Samba's test protocol). Key fields:
- `success: smb2.connect.connect1` -- test passed
- `failure: smb2.lock.lock1 [reason]` -- test failed
- `skip: smb2.multichannel.interface_info [reason]` -- test skipped

### Pattern 2: MxAc Create Context Response

**What:** Return maximal access mask to clients requesting MxAc context.
**When to use:** In CREATE handler, after file/directory is opened.
**Example:**
```go
// In handleCreate(), after file is opened and permissions resolved:
if mxAcCtx := FindCreateContext(req.CreateContexts, "MxAc"); mxAcCtx != nil {
    // MxAc response is 8 bytes: QueryStatus (4) + MaximalAccess (4)
    // QueryStatus = STATUS_SUCCESS (0x00000000)
    // MaximalAccess = granted access rights bitmask
    mxAcResp := make([]byte, 8)
    binary.LittleEndian.PutUint32(mxAcResp[0:4], 0)              // QueryStatus = SUCCESS
    binary.LittleEndian.PutUint32(mxAcResp[4:8], maximalAccess)  // MaximalAccess mask
    resp.CreateContexts = append(resp.CreateContexts, CreateContext{
        Name: "MxAc",
        Data: mxAcResp,
    })
}
```

The maximalAccess value should reflect the user's actual permissions for the file. For owner: full access (0x001F01FF). For others: computed from POSIX mode or ACL.

### Pattern 3: QFid Create Context Response

**What:** Return on-disk file identifier to clients requesting QFid context.
**When to use:** In CREATE handler, alongside MxAc.
**Example:**
```go
// QFid response is 32 bytes: DiskFileId (16) + VolumeId (16)
if qfidCtx := FindCreateContext(req.CreateContexts, "QFid"); qfidCtx != nil {
    qfidResp := make([]byte, 32)
    // Use file handle bytes as DiskFileId (padded to 16 bytes)
    copy(qfidResp[0:16], fileHandleBytes)
    // Use ServerGUID as VolumeId
    copy(qfidResp[16:32], h.ServerGUID[:])
    resp.CreateContexts = append(resp.CreateContexts, CreateContext{
        Name: "QFid",
        Data: qfidResp,
    })
}
```

### Pattern 4: Cross-Platform Config Path

**What:** Platform-native config directory resolution.
**When to use:** In `getConfigDir()` and SQLite default path.
**Example:**
```go
func getConfigDir() string {
    if runtime.GOOS == "windows" {
        appData := os.Getenv("APPDATA")
        if appData != "" {
            return filepath.Join(appData, "dittofs")
        }
        home, err := os.UserHomeDir()
        if err == nil {
            return filepath.Join(home, "AppData", "Roaming", "dittofs")
        }
        return "."
    }

    // Unix: XDG_CONFIG_HOME or ~/.config
    if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
        return filepath.Join(xdgConfig, "dittofs")
    }
    home, err := os.UserHomeDir()
    if err != nil {
        return "."
    }
    return filepath.Join(home, ".config", "dittofs")
}
```

This mirrors the existing pattern in `internal/cli/credentials/store.go` lines 107-127.

### Pattern 5: Guest Signing Negotiation

**What:** Ensure guest sessions work when signing is enabled but not required.
**When to use:** SESSION_SETUP response for guest sessions.
**Key insight:** Per MS-SMB2 3.3.5.5, when the session is established as guest, the server MUST NOT set `SessionFlags` to indicate signing is required, even if the server normally requires signing. The current code already returns `SMB2SessionFlagIsGuest` and skips signing configuration for guests. The fix is primarily documentation: Windows 11 24H2 blocks insecure guest logons by default, requiring `AllowInsecureGuestAuth` GPO to be enabled. DittoFS should document this requirement in the Windows VM setup guide.

If additional signing behavior needs adjustment, verify that:
1. NEGOTIATE response keeps `SIGNING_ENABLED` but NOT `SIGNING_REQUIRED` when guest access is allowed
2. SESSION_SETUP response for guest sets `SESSION_FLAG_IS_GUEST` (0x0001)
3. No signing verification is attempted for guest session messages

### Anti-Patterns to Avoid
- **Mixing GPL code into DittoFS repo:** smbtorture is GPLv3. Never copy smbtorture source code, test scripts, or configuration files from the Samba repo into DittoFS. All interaction must be via the Docker container boundary.
- **Blocking CI on smbtorture failures:** The known-failures model means CI should only fail on NEW unexpected failures, not on documented expected failures.
- **Platform-specific path handling without build tags:** Use `runtime.GOOS` checks instead of build tags for simple path logic. Build tags (`_windows.go`) are for when the entire file is platform-specific or uses platform-specific imports.
- **Hardcoding smbtorture test names in CI:** The full smb2 suite may add/remove tests across Samba versions. Parse actual output dynamically.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SMB conformance testing | Custom SMB protocol tests | smbtorture (Samba project) | Industry-standard, tests real client behavior, thousands of test cases |
| WPTS result parsing | Custom TRX parser | xmlstarlet (existing) | Already working in parse-results.sh |
| smbtorture output parsing | Custom subunit parser | grep/awk on subunit output | Simple text format, parsing is straightforward |
| Windows config paths | Custom path resolution | `os.Getenv("APPDATA")` + `runtime.GOOS` | Standard Go pattern, already used in credentials/store.go |
| File info encoding | Manual byte packing | `encoding/binary` package | Already used throughout SMB handlers |

**Key insight:** The WPTS infrastructure already built for Phase 29.8 is the complete template. The smbtorture integration is a direct mirror -- same Compose orchestration, same parse/compare pattern, same CI approach.

## Common Pitfalls

### Pitfall 1: smbtorture Authentication Format
**What goes wrong:** smbtorture expects `DOMAIN\user` or `user%password` format. If the authentication format doesn't match what DittoFS expects, all tests fail with STATUS_LOGON_FAILURE.
**Why it happens:** smbtorture sends NTLM auth with domain prefix. DittoFS's NTLM handler may not strip the domain component.
**How to avoid:** Test authentication first with `smb2.connect` before running the full suite. Use `-U "user%password"` format. Verify DittoFS NTLM handler accepts domain-prefixed usernames.
**Warning signs:** 100% failure rate, all tests failing at session setup.

### Pitfall 2: smbtorture Port Configuration
**What goes wrong:** smbtorture defaults to port 445. DittoFS in Docker maps to 445 internally but the container needs correct networking.
**Why it happens:** Docker network topology must route smbtorture container to DittoFS container on the correct port.
**How to avoid:** Use `network_mode: "service:dittofs"` (same pattern as WPTS) so smbtorture sees DittoFS on localhost:445. Alternatively, use `--option="smb port=12445"` flag.
**Warning signs:** Connection timeouts, "NT_STATUS_IO_TIMEOUT" errors.

### Pitfall 3: MxAc Access Mask Computation
**What goes wrong:** Returning incorrect maximal access mask causes Windows Explorer to show wrong context menu options (e.g., no "Delete" option, or "Rename" greyed out).
**Why it happens:** The access mask must reflect actual permissions. If always returning full access, non-owner users see operations they can't perform. If too restrictive, owner can't use basic Explorer features.
**How to avoid:** Compute maximal access from the file's ACL/permissions for the authenticated user. For owner: `0x001F01FF` (GENERIC_ALL). For read-only: `0x00120089` (GENERIC_READ). Use the same permission check that `CheckAccess` uses, but requesting all access types.
**Warning signs:** Explorer showing limited context menu, "Access denied" on operations that should work.

### Pitfall 4: FileCompressionInformation Buffer Size
**What goes wrong:** Returning wrong-sized buffer for FileCompressionInformation causes STATUS_INFO_LENGTH_MISMATCH.
**Why it happens:** MS-FSCC 2.4.9 defines a 16-byte structure (CompressedFileSize + CompressionFormat + CompressionUnitShift + ChunkShift + ClusterShift + Reserved). Some implementations omit trailing padding.
**How to avoid:** Return exactly 16 bytes (actually 28 bytes per MS-FSCC: CompressedFileSize(8) + CompressionFormat(2) + CompressionUnitShift(1) + ChunkShift(1) + ClusterShift(1) + Reserved(3) = 16 bytes). Set CompressedFileSize = EndOfFile, CompressionFormat = 0 (COMPRESSION_FORMAT_NONE), all shifts = 0.
**Warning signs:** "Cannot retrieve properties" dialog in Windows Explorer.

### Pitfall 5: smbtorture Output Format Variations
**What goes wrong:** Different Samba versions use different output formats. Parsing assumes one format and breaks on another.
**Why it happens:** smbtorture can output in subunit v1, subunit v2, or plain text depending on version and flags.
**How to avoid:** Use `--format=subunit` flag explicitly if available, or parse the default output format. Test parsing with sample output before building CI. The v0.8 image (Samba 4.22.6) output should be verified locally first.
**Warning signs:** parse-results.sh producing empty or garbled output.

### Pitfall 6: Windows 11 24H2 Guest Login Block
**What goes wrong:** Windows 11 24H2 refuses to connect to DittoFS share with guest authentication.
**Why it happens:** Microsoft hardened SMB security in 24H2, making `AllowInsecureGuestAuth` default to 0 (disabled). Guest sessions cannot perform signing (no session key), so the connection is rejected.
**How to avoid:** Document in the Windows VM setup guide that `AllowInsecureGuestAuth` must be enabled via Group Policy or registry for guest access. Better: test with authenticated (NTLM) sessions which work with signing.
**Warning signs:** "The specified network password is not correct" or "System error 53" from Windows client.

### Pitfall 7: FileFsAttributeInformation Flag Overreach
**What goes wrong:** Advertising capability flags that aren't actually implemented causes clients to use features that fail.
**Why it happens:** Tempting to add many flags (FILE_SUPPORTS_ENCRYPTION, FILE_SUPPORTS_OBJECT_IDS) to make Windows happy, but clients then try to use those features.
**How to avoid:** Only set flags for capabilities that are actually functional: FILE_CASE_SENSITIVE_SEARCH (0x01), FILE_CASE_PRESERVED_NAMES (0x02), FILE_UNICODE_ON_DISK (0x04), FILE_PERSISTENT_ACLS (0x08), FILE_SUPPORTS_SPARSE_FILES (0x40), FILE_SUPPORTS_REPARSE_POINTS (0x80). Do NOT add FILE_SUPPORTS_ENCRYPTION, FILE_SUPPORTS_OBJECT_IDS, FILE_VOLUME_IS_COMPRESSED.
**Warning signs:** WPTS tests for unsupported features starting to fail in new ways.

## Code Examples

### FileCompressionInformation Handler (MS-FSCC 2.4.9)

```go
// In buildFileInfo() switch, add:
case 28: // FileCompressionInformation [MS-FSCC] 2.4.9
    // 16 bytes: CompressedFileSize(8) + CompressionFormat(2) +
    // CompressionUnitShift(1) + ChunkShift(1) + ClusterShift(1) + Reserved(3)
    info := make([]byte, 16)
    // CompressedFileSize = EndOfFile (no compression, so same as file size)
    binary.LittleEndian.PutUint64(info[0:8], attr.Size)
    // CompressionFormat = COMPRESSION_FORMAT_NONE (0)
    binary.LittleEndian.PutUint16(info[8:10], 0)
    // All shift values and reserved = 0
    return info, nil
```

### FileAttributeTagInformation Handler (MS-FSCC 2.4.6)

```go
// In buildFileInfo() switch, add:
case 35: // FileAttributeTagInformation [MS-FSCC] 2.4.6
    // 8 bytes: FileAttributes(4) + ReparseTag(4)
    info := make([]byte, 8)
    binary.LittleEndian.PutUint32(info[0:4], uint32(fileAttributes))
    // ReparseTag = 0 for non-reparse points
    // For symlinks: IO_REPARSE_TAG_SYMLINK (0xA000000C)
    var reparseTag uint32
    if fileAttributes&uint32(types.FileAttributeReparsePoint) != 0 {
        reparseTag = 0xA000000C // IO_REPARSE_TAG_SYMLINK
    }
    binary.LittleEndian.PutUint32(info[4:8], reparseTag)
    return info, nil
```

### smbtorture Output Parsing (Subunit-like)

```bash
#!/usr/bin/env bash
# parse-results.sh for smbtorture
# Parses plain text output from smbtorture

# smbtorture output format (plain text):
#   smb2.connect.connect1                         ok
#   smb2.read.read                                FAILED (STATUS_ACCESS_DENIED)
#   smb2.lock.lock1                               SKIP (not implemented)
#
# Alternative subunit-like format:
#   success: smb2.connect.connect1
#   failure: smb2.read.read [STATUS_ACCESS_DENIED]
#   skip: smb2.lock.lock1

# Count results
PASS=$(grep -c "^success:" "$RESULTS_FILE" || echo 0)
FAIL=$(grep -c "^failure:" "$RESULTS_FILE" || echo 0)
SKIP=$(grep -c "^skip:" "$RESULTS_FILE" || echo 0)
```

### Windows CI Enhancement (windows-build.yml)

The existing Windows CI at `.github/workflows/windows-build.yml` already covers building and unit testing. Verify it runs on the correct triggers and keeps passing.

### smbtorture Docker Compose Service

```yaml
# Addition to test/smb-conformance/docker-compose.yml
smbtorture:
    image: quay.io/samba.org/samba-toolbox:v0.8
    platform: linux/amd64
    depends_on:
      dittofs:
        condition: service_healthy
    network_mode: "service:dittofs"
    entrypoint: ["smbtorture"]
    command:
      - "//localhost/smbbasic"
      - "-U"
      - "wpts-admin%TestPassword01!"
      - "--option=client min protocol=SMB2_02"
      - "--option=client max protocol=SMB3"
      - "smb2"
    profiles:
      - smbtorture
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Manual SMB testing | smbtorture automated suite | Always existed in Samba | Industry-standard conformance validation |
| WPTS only | WPTS + smbtorture complementary | Phase 32 adds smbtorture | WPTS = wire conformance, smbtorture = client behavior |
| Unix-only server | Cross-platform Go with build tags | Ongoing since Phase 29 | Windows support via `_windows.go` files |
| No Windows CI | windows-build.yml | Phase 29 | Compile + unit test on Windows |

**Deprecated/outdated:**
- Docker image `cmd.cat/smbtorture`: Not officially maintained. Use `quay.io/samba.org/samba-toolbox:v0.8` instead.
- smbtorture `--format=subunit` flag: May not be available in all builds. Parse default output format.

## Open Questions

1. **smbtorture exact output format in samba-toolbox v0.8**
   - What we know: smbtorture uses subunit or plain text output
   - What's unclear: Exact format from the v0.8 samba-toolbox image (may vary by compile options)
   - Recommendation: Pull the image locally, run `smbtorture --list` to verify available tests, then run a single test (`smb2.connect`) to capture output format before building the parser. **LOW confidence** on exact output format until verified.

2. **MaximalAccess computation for MxAc**
   - What we know: Must reflect actual permissions for the authenticated user
   - What's unclear: Whether to use the ACL-based permission model or simplified POSIX mode bits
   - Recommendation: Use the same permission check already in `CheckAccess()` but query for all access types. For owner, grant `0x001F01FF` (GENERIC_ALL). For others, compute from effective permissions. **MEDIUM confidence**.

3. **FileFsAttributeInformation flags -- FILE_SUPPORTS_SPARSE_FILES**
   - What we know: DittoFS supports sparse files (zero-fill for unwritten blocks)
   - What's unclear: Whether advertising FILE_SUPPORTS_SPARSE_FILES (0x40) causes Windows to send FSCTL_SET_SPARSE or other operations we don't handle
   - Recommendation: Set the flag since sparse read/write works, but test with Windows first. Can always remove if it causes issues. **MEDIUM confidence**.

4. **smbtorture expected failure count**
   - What we know: WPTS baseline was ~107 known failures out of 240 BVT tests
   - What's unclear: How many of the ~80+ smb2.* tests will pass on first run
   - Recommendation: Expect high failure rate initially (30-50% pass) given no durable handles, no change notify, limited FSCTL support. Document baseline without concern for pass rate per user decision. **LOW confidence on count**.

## Codebase Analysis: Current State

### Files That Need Modification

| File | Change | Requirement |
|------|--------|-------------|
| `internal/adapter/smb/v2/handlers/create.go` | Add MxAc and QFid context processing in handleCreate | WIN-02, WIN-03 |
| `internal/adapter/smb/v2/handlers/query_info.go` | Add FileCompressionInformation (case 28) and FileAttributeTagInformation (case 35) | WIN-05 |
| `internal/adapter/smb/types/constants.go` | Add FileCompressionInformation and FileAttributeTagInformation to FileInfoClass enum | WIN-05 |
| `internal/adapter/smb/v2/handlers/query_info.go` | Update FileFsAttributeInformation flags (case 5) | WIN-07 |
| `pkg/config/config.go` | Add Windows %APPDATA% check in getConfigDir() | WIN-10 |
| `pkg/controlplane/store/gorm.go` | Add Windows %APPDATA% check in SQLite default path | WIN-10 |
| `internal/adapter/smb/v2/handlers/session_setup.go` | Verify guest signing behavior, add documentation | WIN-06 |
| `.github/workflows/smb-conformance.yml` | Add smbtorture job alongside WPTS job | TEST-01 |
| `.github/workflows/windows-build.yml` | Verify and potentially enhance | WIN-08 |

### Files That Already Work (No Changes Needed)

| File | Why It Works |
|------|-------------|
| `cmd/dfs/commands/util.go` | Already has Windows path support via runtime.GOOS check |
| `internal/cli/credentials/store.go` | Already uses %APPDATA% on Windows |
| `pkg/cache/wal/mmap_windows.go` | Already uses filepath.Join for WAL paths |
| `cmd/dfs/commands/daemon_windows.go` | Already handles Windows process management |
| `cmd/dfs/commands/stop_windows.go` | Already handles Windows process signaling |

### New Files to Create

| File | Purpose |
|------|---------|
| `test/smb-conformance/smbtorture/run.sh` | smbtorture run script |
| `test/smb-conformance/smbtorture/parse-results.sh` | smbtorture output parser |
| `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` | smbtorture known failures baseline |
| `test/smb-conformance/smbtorture/Makefile` | Local dev convenience targets |
| `docs/WINDOWS_TESTING.md` | Windows VM setup guide + validation checklist |

## Sources

### Primary (HIGH confidence)
- MS-SMB2 2.2.14.2.5 (MxAc create context) -- Microsoft protocol specification
- MS-SMB2 2.2.14.2.9 (QFid create context) -- Microsoft protocol specification
- MS-FSCC 2.4.9 (FileCompressionInformation) -- Microsoft protocol specification
- MS-FSCC 2.4.6 (FileAttributeTagInformation) -- Microsoft protocol specification
- MS-FSCC 2.5.1 (FileFsAttributeInformation) -- Microsoft protocol specification
- Existing WPTS infrastructure in `test/smb-conformance/` -- Codebase (verified via Read tool)
- Existing Windows build CI in `.github/workflows/windows-build.yml` -- Codebase (verified via Read tool)
- GitHub issues #141, #169, #172, #173 -- Project issue tracker (verified via gh CLI)

### Secondary (MEDIUM confidence)
- [samba-in-kubernetes/samba-container releases](https://github.com/samba-in-kubernetes/samba-container/releases) -- v0.8 includes Samba 4.22.6 with smbtorture
- [quay.io/samba.org/samba-toolbox](https://quay.io/organization/samba.org/) -- Docker registry for Samba toolbox images
- [smbtorture man page](https://www.mankier.com/1/smbtorture) -- Command-line options and test execution
- [Samba SMB2 torture tests](https://github.com/samba-team/samba/tree/master/source4/torture/smb2) -- Available test files

### Tertiary (LOW confidence)
- smbtorture exact output format from v0.8 image -- Needs local verification before building parser
- Expected pass/fail ratio for first smbtorture run -- No comparable data available

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Docker image tags verified from GitHub releases, WPTS pattern is proven in codebase
- Architecture: HIGH - Direct mirror of existing WPTS infrastructure, protocol fixes are well-specified by MS-SMB2/MS-FSCC
- Pitfalls: MEDIUM - Authentication format and output parsing need local verification. Guest signing behavior needs testing.
- Code examples: HIGH - Based on verified MS-SMB2/MS-FSCC specs and existing codebase patterns

**Research date:** 2026-02-27
**Valid until:** 2026-03-27 (stable domain, pinned versions)
