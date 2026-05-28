# Phase 25: v3.0 Integration Testing - Research

**Researched:** 2026-02-23
**Domain:** End-to-end testing of NFSv4.1 sessions, EOS, backchannel, directory delegations, coexistence, and SMB Kerberos
**Confidence:** HIGH

## Summary

Phase 25 is the final integration testing phase for v3.0, verifying all NFSv4.1 functionality built in Phases 16-24 works end-to-end with real kernel NFS clients. The test infrastructure is mature -- the existing e2e framework already supports versioned NFS mounts (v3, v4.0), SMB mounts (NTLM auth), Kerberos via KDC testcontainer, multiple storage backends (memory, badger, postgres x memory, filesystem, s3), and cross-protocol interop tests.

The core work is extending the existing `MountNFSWithVersion()` helper to accept `"4.1"`, adding v4.1 to the existing version-parametrized test loops, and writing new tests for v4.1-specific features (EOS replay, backchannel CB_RECALL, directory delegation CB_NOTIFY, coexistence). SMB Kerberos (SMBKRB-01/02) requires implementing SPNEGO/Kerberos authentication in the SMB SESSION_SETUP handler -- the SPNEGO parser and NFS Kerberos layer already exist, but the SMB adapter currently only handles NTLM.

**Primary recommendation:** Extend the existing e2e framework incrementally -- add v4.1 mount support, parametrize existing tests to include v4.1, then add protocol-specific tests for EOS/backchannel/directory-delegation/coexistence as separate test functions.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Run on both macOS and Linux (CI via extended existing GitHub Actions workflow)
- macOS: attempt all mounts, gracefully skip on failure (OS check -- macOS skips, Linux treats mount failure as real failure)
- Use existing `e2e` build tag for all tests (no new tags)
- Server runs in-process (existing e2e pattern), with logs piped to t.Log()
- Protocol-specific tests (EOS, delegations, backchannel) use memory-only backends
- File I/O storage matrix runs against ALL existing backend combinations for NFS v3, v4.0, v4.1, and SMB
- Small file sizes for I/O matrix (1MB, 10MB) -- benchmarks are a separate suite later
- No test retries -- flakiness must be fixed, not masked
- Functional tests only -- no metrics/telemetry assertions
- Reuse existing KDC helper from framework/kerberos.go for SMB Kerberos tests, started once per test suite
- Extend existing nfsv4_*.go test files (add v4.1 test functions alongside v4.0 tests)
- Parametrize existing file operation tests by protocol version: v3, v4.0, v4.1, and SMB all run the same core file ops suite
- Include SMB in the file operations matrix (both Kerberos and NTLM/simpler auth modes)
- Shared helpers go into existing framework/ and helpers/ directories
- Build custom test assertion helpers (e.g., for EOS replay verification, delegation recall checks)
- No requirement traceability comments in test code
- Use real Linux NFS client only for EOS/session tests (no custom RPC client)
- Verify replay through server-side observability (internal counters or log signals)
- Test basic replay scenario + 1-2 edge cases (e.g., stale seqid rejected)
- For backchannel delegation recall (CB_RECALL): Claude picks the triggering approach
- Directory delegation notifications: test ALL notification types (entry added, removed, renamed, attributes changed)
- Verify delegation state is properly cleaned up after recall/revocation
- v4.0 and v4.1 coexistence test: both versions mounted simultaneously on the SAME share
- Core file ops suite is version-parametrized -- same tests run on v3, v4.0, v4.1, and SMB
- SMB full cross-protocol test: SPNEGO/Kerberos auth + basic file ops + bidirectional visibility
- SMB file ops tested with both Kerberos and NTLM/simpler auth modes
- Identity mapping verification covers: UID/GID match, permission enforcement, cross-protocol identity consistency, group membership resolution
- macOS: native SMB mount via `mount_smbfs` for basic ops; advanced features on Linux only
- Unified Mount interface using Go `os` package on mount paths (protocol-transparent)
- Separate interface levels: base interface (CRUD, attrs, lifecycle) + extended interface (links, ACLs, protocol-specific ops)
- Protocol-specific helpers available alongside unified interface
- Shared mountV41() helper for v4.1 mount setup with OS-aware skip logic
- One server per entire test suite (started in TestMain)
- Automatic cleanup of stale mounts and temp directories from failed test runs
- Server log dump to test output on test failure only
- Disconnect/robustness tests: force-close client during large file write, directory listing, and session establishment

### Claude's Discretion
- Test timeout values per test case
- Mount reuse strategy (per-suite vs per-test)
- Test function naming convention for v4.1 tests within shared files
- EOS replay trigger mechanism (delay injection vs connection disruption)
- EOS replay detection mechanism (log parsing vs internal counters)
- CB_RECALL test approach (multi-client conflict vs server-side force recall)
- Event capture approach for protocol-level test assertions
- Coexistence test scope (pairwise vs all-protocols simultaneous)
- CI runner NFS package installation approach
- SMB client approach for tests (smbclient vs mount.cifs vs Go library)
- Parallel vs sequential execution of protocol version subtests
- Additional disconnect test scenarios beyond the three specified

### Deferred Ideas (OUT OF SCOPE)
- Performance benchmarks for v4.1 vs v4.0 vs v3 -- separate benchmark suite later
- Metrics/telemetry assertion tests -- out of scope for this phase
- Test profiles (quick/full) -- just use go test -run patterns instead
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| TEST-01 | E2E tests with Linux NFS client using vers=4.1 mount option | Mount helper extension in framework/mount.go; Linux `mount -t nfs -o vers=4.1` is the standard mount syntax |
| TEST-02 | EOS replay verification (retry same slot+seqid, confirm cached response) | Server log scraping (existing pattern from delegation tests) or internal counters via REST API; Linux NFS client retries automatically on timeout |
| TEST-03 | Backchannel delegation recall test (CB_RECALL over fore-channel) | Multi-client conflict pattern (existing in nfsv4_delegation_test.go); v4.1 uses backchannel instead of dial-out |
| TEST-04 | v4.0/v4.1 coexistence test (both versions mounted simultaneously) | Mount two versions on same share using existing MountNFSWithVersion with "4.0" and "4.1" |
| TEST-05 | Directory delegation notification test (CB_NOTIFY on directory mutation) | New test: grant dir delegation via READDIR on v4.1 mount, mutate from second mount, verify notification via log scraping |
| SMBKRB-01 | SMB adapter authenticates via SPNEGO/Kerberos using shared Kerberos layer | Requires implementing Kerberos auth path in SESSION_SETUP handler; SPNEGO parser exists, gokrb5 library available |
| SMBKRB-02 | Kerberos principal maps to control plane identity for SMB sessions | Map Kerberos principal to UserStore lookup; same identity mapping pattern as NFS RPCSEC_GSS |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go testing | 1.21+ | Test framework | Built-in, used by all existing e2e tests |
| testify | v1.9+ | Assertions (require/assert) | Already used in every e2e test file |
| testcontainers-go | v0.28+ | KDC container for Kerberos tests | Already used for KDC, Postgres, Localstack |
| gokrb5/v8 | v8.4+ | Kerberos 5 library for SPNEGO/Kerberos SMB auth | Already a dependency (used by NFS RPCSEC_GSS) |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| jcmturner/gofork/encoding/asn1 | latest | ASN.1 encoding for SPNEGO tokens | Already imported by internal/auth/spnego |
| os/exec | stdlib | Mount/unmount commands | Already used by framework/mount.go |
| strings/bytes | stdlib | Log parsing for server-side observability | EOS replay and delegation verification |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Log scraping for EOS | REST API internal counters | Log scraping is simpler and already proven in delegation tests; counters would require new API endpoints |
| Multi-client conflict for CB_RECALL | Server-side force recall API | Conflict is more realistic; force recall tests internal mechanism but not real client behavior |
| mount.cifs for SMB Kerberos | smbclient CLI | mount.cifs provides real kernel-level SMB mount; smbclient is user-space only |

## Architecture Patterns

### Recommended Test File Organization
```
test/e2e/
├── framework/
│   ├── mount.go             # EXTEND: add "4.1" case to MountNFSWithVersion
│   ├── helpers.go           # EXTEND: add SkipIfNFSv41Unsupported()
│   ├── kerberos.go          # EXISTING: reuse KDCHelper for SMB Kerberos
│   └── containers.go        # EXISTING: no changes needed
├── helpers/
│   ├── server.go            # EXISTING: no changes needed
│   ├── adapters.go          # EXISTING: no changes needed
│   └── ...                  # EXISTING: no changes needed
├── nfsv4_basic_test.go      # EXTEND: add "4.1" to versions slice
├── nfsv4_store_matrix_test.go # EXTEND: add "4.1" to versions slice
├── nfsv4_delegation_test.go # EXTEND: add v4.1 backchannel delegation tests
├── nfsv41_session_test.go   # NEW: EOS replay, session establishment tests
├── nfsv41_coexistence_test.go # NEW: v4.0+v4.1 simultaneous mount tests
├── nfsv41_dirdeleg_test.go  # NEW: directory delegation notification tests
├── nfsv41_disconnect_test.go # NEW: robustness/disconnect tests
├── smb_kerberos_test.go     # NEW: SMB SPNEGO/Kerberos tests
└── cross_protocol_v41_test.go # NEW: v4.1+SMB cross-protocol visibility
```

### Pattern 1: Version-Parametrized Tests (extend existing)
**What:** Add "4.1" to existing version slices in parameterized tests
**When to use:** Any test that currently iterates over `[]string{"3", "4.0"}`
**Example:**
```go
// Before (existing in nfsv4_basic_test.go)
versions := []string{"3", "4.0"}

// After
versions := []string{"3", "4.0", "4.1"}

// Each test function already handles version-specific skip logic
for _, ver := range versions {
    ver := ver
    t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
        if ver == "4.0" || ver == "4.1" {
            framework.SkipIfNFSv4Unsupported(t)
        }
        mount := framework.MountNFSWithVersion(t, nfsPort, ver)
        // ... test body ...
    })
}
```

### Pattern 2: Server Log Scraping for Observability
**What:** Read server log file before and after test operation to detect protocol events
**When to use:** EOS replay detection, delegation grant/recall confirmation, CB_NOTIFY verification
**Example:**
```go
// Already exists in nfsv4_delegation_test.go
logBefore := readLogFile(t, sp)
// ... perform test operations ...
logAfter := readLogFile(t, sp)
newLogs := extractNewLogs(logBefore, logAfter)

// Check for specific log messages
if strings.Contains(newLogs, "replay cache hit") {
    t.Log("EOS replay confirmed via server logs")
}
```

### Pattern 3: NFSv4.1 Mount Helper
**What:** Extend MountNFSExportWithVersion to handle version "4.1"
**When to use:** All v4.1 mount operations
**Example:**
```go
// In framework/mount.go, extend the switch in MountNFSExportWithVersion
case "4.1":
    // NFSv4.1: uses vers=4.1 (or vers=4,minorversion=1)
    // No mountport needed (NFSv4.1 is stateful, session-based)
    mountOptions = fmt.Sprintf("vers=4.1,port=%d,actimeo=0", port)
    switch runtime.GOOS {
    case "darwin":
        // macOS NFSv4.1 support is unreliable -- skip
        t.Skip("NFSv4.1 not supported on macOS")
    case "linux":
        // No additional options needed
    }
```

### Pattern 4: SMB Kerberos Authentication (new implementation)
**What:** Add Kerberos auth path to SMB SESSION_SETUP handler alongside existing NTLM
**When to use:** When SPNEGO token contains Kerberos OID instead of NTLM
**Example flow:**
```go
// In session_setup.go extractNTLMToken(), add Kerberos detection:
if parsed.HasKerberos() && len(parsed.MechToken) > 0 {
    // Route to Kerberos validation via gokrb5
    return h.handleKerberosAuth(ctx, parsed.MechToken)
}

// Kerberos validation:
// 1. Decode AP-REQ from MechToken using gokrb5
// 2. Validate ticket against service keytab
// 3. Extract client principal name
// 4. Map principal to UserStore identity (strip realm, lookup username)
// 5. Create authenticated session
```

### Anti-Patterns to Avoid
- **Custom RPC client for EOS testing:** User decided on real Linux NFS client only. The NFS kernel client will naturally trigger EOS via retries on delayed responses or reconnection scenarios. Do NOT write a custom RPC client to inject specific seqid values.
- **Per-test server startup for protocol tests:** User decided one server per entire test suite (started in TestMain). Protocol-specific tests should share the server instance, not start their own.
- **Metrics assertions:** User explicitly deferred metrics/telemetry assertions. Tests should use log scraping or data consistency checks, not Prometheus endpoint scraping.
- **Testing NTLM and Kerberos SMB in the same mount:** They are separate auth modes requiring different mount options. Use separate subtests with separate mounts.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Kerberos ticket validation | Custom KRB5 parser | `gokrb5/v8/service` AcceptSecContext | Complex crypto, timing attacks, edge cases |
| SPNEGO token wrapping | Custom ASN.1 encoder | `internal/auth/spnego` package | Already implemented, tested, handles both NTLM and Kerberos OIDs |
| NFS mount command construction | Inline exec.Command | Extend `framework.MountNFSWithVersion` | Centralized retry logic, OS-aware options, cleanup tracking |
| KDC setup for Kerberos tests | Manual Kerberos container | `framework.NewKDCHelper` | Already handles keytab export, krb5.conf generation, principal creation |
| Log parsing | Complex regex engine | Simple `strings.Contains` | Server logs are structured, simple string matching is sufficient |

**Key insight:** The existing e2e framework is comprehensive and well-tested. The main work is extending it incrementally, not building new infrastructure.

## Common Pitfalls

### Pitfall 1: macOS NFSv4.1 Mount Failure
**What goes wrong:** macOS NFS client does not support NFSv4.1 (vers=4.1 mount option), causing test failures.
**Why it happens:** macOS NFSv4 client has known limitations; it only supports NFSv4.0 reliably, and even that is unreliable for many operations.
**How to avoid:** Check `runtime.GOOS == "darwin"` before attempting v4.1 mounts. Use `t.Skip()` instead of `t.Fatal()` for v4.1 on macOS. The existing `SkipIfNFSv4Unsupported()` helper already handles this pattern.
**Warning signs:** Mount command returns "Protocol not supported" or "Operation not supported" on macOS.

### Pitfall 2: NFS Attribute Caching Masking Cross-Client Visibility
**What goes wrong:** File written by mount1 is not immediately visible to mount2 because the NFS kernel client caches attributes.
**Why it happens:** Even with `actimeo=0`, some NFS clients may cache briefly. Between different protocol versions (v3 vs v4.1), caching behavior differs.
**How to avoid:** Always use `actimeo=0` in mount options (already done). Add `time.Sleep(500 * time.Millisecond)` between write and cross-mount read. For cross-protocol tests (NFS->SMB), use longer delays (1-2 seconds).
**Warning signs:** Intermittent test failures where cross-mount reads return stale data.

### Pitfall 3: EOS Replay Not Triggerable via Normal Operations
**What goes wrong:** The Linux NFS client is smart about retransmission -- it rarely triggers EOS replay in normal operation, making it hard to verify replay behavior.
**Why it happens:** The Linux NFS client uses its own seqid tracking and only retries when it detects network-level issues (timeout, connection reset).
**How to avoid:** To trigger replay: (a) Use server log scraping to detect if any replay cache hits occurred during normal test operations, (b) induce network delay by pausing the server briefly during a write, causing the client to retransmit, (c) force-close the TCP connection mid-operation to trigger client reconnection with the same slot+seqid.
**Warning signs:** Test passes but logs show zero replay cache hits -- the test may not actually be testing EOS.

### Pitfall 4: SMB Kerberos Requires System-Level Configuration
**What goes wrong:** SMB Kerberos mount (mount.cifs sec=krb5) requires system-level /etc/krb5.conf and cifs.upcall configuration that may not exist on CI runners.
**Why it happens:** Unlike NFS Kerberos (which uses rpc.gssd and /etc/krb5.keytab), SMB Kerberos on Linux uses request-key and cifs.upcall for ticket management.
**How to avoid:** Install cifs-utils and krb5-user on CI runner. Copy KDC-generated krb5.conf to /etc/krb5.conf for the test duration (restore on cleanup). Use `kinit` to obtain a ticket before mounting. The existing kerberos_test.go pattern with `installSystemKeytab()` shows how to manage system files.
**Warning signs:** "Key has expired" or "No key available" errors during mount.cifs with sec=krb5.

### Pitfall 5: Directory Delegation Notification Timing
**What goes wrong:** CB_NOTIFY is batched (default 50ms window, batch at 100 entries). Tests that check for notifications immediately after mutation may miss them.
**Why it happens:** Phase 24 implemented count-based flush at maxBatchSize=100 + timer-based flush at configurable window (default 50ms). Notifications are asynchronous.
**How to avoid:** After directory mutations, wait at least 200ms before checking for CB_NOTIFY in server logs. The batching window plus network delivery time means immediate checks will fail.
**Warning signs:** Tests pass on fast machines but fail on slower CI runners.

### Pitfall 6: Port Conflicts in Parallel Test Runs
**What goes wrong:** Multiple test functions start servers on conflicting ports.
**Why it happens:** `FindFreePort()` finds a free port, but between finding and binding, another test may claim it.
**How to avoid:** The existing pattern uses `FindFreePort(t)` which binds-and-releases. The server immediately binds the port. Race window is small but possible with heavy parallelism. For Phase 25, the user decided one server per entire suite, which eliminates this issue for the main test suite.
**Warning signs:** "address already in use" errors in server startup.

## Code Examples

### Extending MountNFSWithVersion for v4.1

```go
// In framework/mount.go, add case "4.1" to MountNFSExportWithVersion
case "4.1":
    // NFSv4.1: session-based protocol
    mountOptions = fmt.Sprintf("vers=4.1,port=%d,actimeo=0", port)
    switch runtime.GOOS {
    case "darwin":
        _ = os.RemoveAll(mountPath)
        t.Skip("NFSv4.1 not supported on macOS (kernel NFS client limitation)")
    case "linux":
        // Linux supports NFSv4.1 natively since kernel 2.6.38
    default:
        _ = os.RemoveAll(mountPath)
        t.Fatalf("Unsupported platform for NFS: %s", runtime.GOOS)
    }
```

### EOS Replay Verification via Log Scraping

```go
func TestNFSv41EOSReplay(t *testing.T) {
    framework.SkipIfDarwin(t)
    framework.SkipIfNFSv4Unsupported(t)

    sp, _, nfsPort := setupNFSv4TestServer(t)

    mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
    t.Cleanup(mount.Cleanup)

    logBefore := readLogFile(t, sp)

    // Perform operations that may trigger replay
    // (create, write, sync -- NFS client may retry on slow response)
    filePath := mount.FilePath("eos_test.txt")
    framework.WriteFile(t, filePath, []byte("test data"))

    // Force close the TCP connection to trigger client reconnection
    // The NFS client will retry pending operations with same slot+seqid
    // ... (implementation detail: connection disruption mechanism)

    logAfter := readLogFile(t, sp)
    newLogs := extractNewLogs(logBefore, logAfter)

    // Verify EOS behavior via server logs
    if strings.Contains(newLogs, "replay cache hit") ||
       strings.Contains(newLogs, "SEQUENCE replay") {
        t.Log("EOS replay detected and handled correctly")
    }
}
```

### SMB Kerberos Session Setup (server-side implementation)

```go
// In internal/protocol/smb/v2/handlers/session_setup.go
// Add Kerberos path alongside existing NTLM path

func (h *Handler) handleKerberosAuth(ctx *SMBHandlerContext, apReqBytes []byte) (*HandlerResult, error) {
    // 1. Get the Kerberos keytab from config
    keytab := h.Registry.GetKeytab() // shared with NFS Kerberos layer

    // 2. Validate the AP-REQ using gokrb5
    // This verifies the Kerberos ticket is valid for our service principal
    settings := service.NewSettings(keytab, service.Logger(logger))
    identity, valid, err := service.VerifyAPREQ(apReqBytes, settings)
    if err != nil || !valid {
        return NewErrorResult(types.StatusLogonFailure), nil
    }

    // 3. Extract client principal and map to UserStore identity
    // Convention: principal "alice@REALM" maps to username "alice"
    username := extractUsername(identity.UserName())

    // 4. Look up user in control plane
    user, err := h.Registry.GetUserStore().GetUser(ctx.Context, username)
    if err != nil || user == nil || !user.Enabled {
        return NewErrorResult(types.StatusLogonFailure), nil
    }

    // 5. Create authenticated session
    sess := h.CreateSessionWithUser(sessionID, ctx.ClientAddr, user, identity.Realm())
    // ... build response ...
}
```

### Coexistence Test Pattern

```go
func TestNFSv41v40Coexistence(t *testing.T) {
    framework.SkipIfDarwin(t)
    framework.SkipIfNFSv4Unsupported(t)

    _, _, nfsPort := setupNFSv4TestServer(t)

    // Mount BOTH v4.0 and v4.1 on the SAME share simultaneously
    mountV40 := framework.MountNFSWithVersion(t, nfsPort, "4.0")
    t.Cleanup(mountV40.Cleanup)

    mountV41 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
    t.Cleanup(mountV41.Cleanup)

    // Write from v4.0, read from v4.1
    content := []byte("written from v4.0")
    framework.WriteFile(t, mountV40.FilePath("coexist.txt"), content)
    time.Sleep(500 * time.Millisecond)
    readBack := framework.ReadFile(t, mountV41.FilePath("coexist.txt"))
    assert.Equal(t, content, readBack, "v4.1 should see v4.0 writes")

    // Write from v4.1, read from v4.0
    content2 := []byte("written from v4.1")
    framework.WriteFile(t, mountV41.FilePath("coexist2.txt"), content2)
    time.Sleep(500 * time.Millisecond)
    readBack2 := framework.ReadFile(t, mountV40.FilePath("coexist2.txt"))
    assert.Equal(t, content2, readBack2, "v4.0 should see v4.1 writes")
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| NFSv3 only tests | v3 + v4.0 parametrized | Phase 15 (v2.0 testing) | Store matrix tests cover both versions |
| Separate mount per test | Shared mount within version subtest | Phase 15 | Faster tests, less mount churn |
| Manual NFS testing | Automated e2e with real kernel mounts | Phase 15 | Reproducible CI |
| NFS-only SMB guest auth | NTLMv2 validated auth with signing | Phase 14 | Real credential verification |
| v4.0 dial-out callbacks | v4.1 backchannel over fore-channel | Phase 22 | NAT-friendly, no separate TCP |

**Current state of SMB auth:** The SMB adapter supports NTLM authentication (including NTLMv2 with proper session key derivation and signing). SPNEGO parsing exists in `internal/auth/spnego/` and correctly detects Kerberos OIDs. However, the Kerberos authentication path in SESSION_SETUP is **not yet implemented** -- the handler falls through to NTLM or guest when it sees a Kerberos token. SMBKRB-01 and SMBKRB-02 require implementing this path.

## Open Questions

1. **EOS Replay Trigger Mechanism**
   - What we know: Linux NFS client retries on timeout/disconnect; server logs EOS events
   - What's unclear: Best way to reliably trigger a replay in automated tests without custom RPC client
   - Recommendation: Use TCP connection disruption (close server socket mid-operation) to force client retransmission. If unreliable, accept that EOS testing via log scraping during normal heavy I/O operations is sufficient (the client may naturally retry). Mark test as skip-if-no-replay-detected rather than fail.

2. **SMB Kerberos Implementation Scope**
   - What we know: SMBKRB-01/02 are in Phase 25 requirements; NFS Kerberos layer and SPNEGO parser exist
   - What's unclear: Whether implementing SMB Kerberos auth is "implementation" or "testing" work
   - Recommendation: SMB Kerberos auth implementation (the SESSION_SETUP handler extension) is a prerequisite for the e2e tests. Plan it as the first task in the phase, before the SMB Kerberos tests.

3. **Directory Delegation Grant Reliability**
   - What we know: Linux NFS client requests dir delegations on READDIR for v4.1
   - What's unclear: Whether the Linux NFS client consistently requests GET_DIR_DELEGATION, or if it depends on client version/configuration
   - Recommendation: Test should attempt to trigger dir delegation but not fail if the client doesn't request one. Use log scraping to detect if delegation was granted, and only test notification if it was.

4. **macOS SMB Kerberos Mount**
   - What we know: macOS has `mount_smbfs` and supports Kerberos via system keychain
   - What's unclear: Whether macOS SMB Kerberos mount will work with a containerized KDC in automated tests
   - Recommendation: Attempt mount on macOS, skip on failure. Focus Kerberos SMB validation on Linux.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: `test/e2e/framework/mount.go` -- existing mount infrastructure supports v3/v4.0/SMB
- Codebase analysis: `test/e2e/nfsv4_basic_test.go` -- existing version-parametrized test pattern
- Codebase analysis: `test/e2e/nfsv4_delegation_test.go` -- existing log scraping pattern for delegation observability
- Codebase analysis: `internal/auth/spnego/spnego.go` -- SPNEGO parser with Kerberos OID detection
- Codebase analysis: `internal/protocol/smb/v2/handlers/session_setup.go` -- current NTLM-only auth flow
- Codebase analysis: `test/e2e/framework/kerberos.go` -- KDC testcontainer helper

### Secondary (MEDIUM confidence)
- [nfs(5) Linux man page](https://man7.org/linux/man-pages/man5/nfs.5.html) -- `vers=4.1` mount option syntax
- [mount.cifs Kerberos documentation](https://std.rocks/gnulinux_cifs_kerberos.html) -- SMB Kerberos mount requires `sec=krb5` mount option and system-level krb5 config

### Tertiary (LOW confidence)
- Linux NFS client behavior for GET_DIR_DELEGATION -- unclear if all kernel versions request this consistently

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - using existing framework, no new dependencies needed
- Architecture: HIGH - extending proven patterns, codebase thoroughly analyzed
- Pitfalls: HIGH - identified from existing test code patterns and protocol knowledge
- SMB Kerberos implementation: MEDIUM - SPNEGO parser exists but Kerberos auth path untested

**Research date:** 2026-02-23
**Valid until:** 2026-03-23 (stable -- test infrastructure and NFS protocol specs don't change rapidly)
