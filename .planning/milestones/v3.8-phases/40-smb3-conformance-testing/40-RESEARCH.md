# Phase 40: SMB3 Conformance Testing - Research

**Researched:** 2026-03-02
**Domain:** SMB3 protocol conformance validation, multi-OS client compatibility, cross-protocol coordination testing
**Confidence:** HIGH

## Summary

Phase 40 is a pure testing and fix-iteration phase. All SMB3 implementation (phases 33-39) is complete. The goal is to validate everything works against industry conformance suites (smbtorture, WPTS), native Go client (go-smb2), real OS clients (Windows/macOS/Linux), and cross-protocol coordination (SMB3 leases vs NFS delegations). The iterate-and-fix approach means: re-measure baseline, fix failures by impact cascade, re-run, repeat until all non-deferred tests pass.

The existing infrastructure is mature. smbtorture runner (`test/smb-conformance/smbtorture/`), WPTS BVT runner (`test/smb-conformance/`), known failures tracking, result parsing, Docker Compose orchestration, and tiered CI pipeline (`smb-conformance.yml`) are all in place. The work is primarily: (1) expand smbtorture to cover SMB3-specific suites, (2) remove wildcard known-failure entries and enumerate individual tests, (3) add go-smb2 E2E tests for SMB3 features, (4) add smbclient-based tests, (5) create multi-OS CI workflows, (6) add cross-protocol lease/delegation tests, and (7) refactor CI to keep PR checks fast.

**Primary recommendation:** Use the existing test infrastructure patterns exactly. The iterate-and-fix cycle should be organized as multiple small plans per batch: re-measure baseline, identify highest-impact failures, fix them, re-run, commit. The go-smb2 library (hirochachacha/go-smb2) supports SMB 3.0/3.0.2/3.1.1, AES-128-CCM/GCM encryption, CMAC signing, and preauth integrity -- making it ideal for native client validation.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Target ALL SMB3 suites: durable_v2, lease, replay, session, encryption -- plus all existing SMB2 suites
- Both SMB2 and SMB3 tests must pass -- no selective skipping
- Remove wildcard entries from KNOWN_FAILURES.md, enumerate individual test names instead
- Every failure must be investigated before being added to KNOWN_FAILURES -- only truly inapplicable tests (unimplemented features) are acceptable
- smbtorture runs in CI alongside existing WPTS BVT as separate CI jobs -- both must pass
- smbtorture must also run with Kerberos authentication (--use-kerberos), not just NTLM
- Pass rate: all tests pass except documented known failures for unimplemented features or interactive tests
- Same iterate-and-fix approach as smbtorture -- all BVT tests must pass
- Re-measure baseline before starting Phase 40 work (phases 30-35 improved pass rate since last measurement)
- Explore additional WPTS categories beyond FileServer BVT (Auth, RSVD) and enable any relevant ones
- Add test filtering capability (e.g., --filter encryption) for faster iteration during fix cycles
- Memory profile for iteration speed, all 5 profiles as final gate before phase completion
- Fix ordering: by impact -- fix failures that unblock the most other tests first (cascade approach)
- Multiple small plans per fix iteration batch, each with atomic commits and re-run verification
- Before committing any test to KNOWN_FAILURES, verify it's genuinely a known failure for an unimplemented feature -- otherwise fix it
- Use hirochachacha/go-smb2 library -- confirmed supports SMB 3.0, 3.0.2, 3.1.1, AES-128-CCM/GCM encryption, signing, SHA-512 preauth integrity
- Full SMB3 feature matrix: encryption, signing, dialect negotiation, session setup, file ops, directory ops
- Tests live in test/e2e/ alongside existing SMB tests (E2E with go-smb2 as client)
- Reuses existing E2E framework (server process, helpers)
- Automated smbclient tests (Go tests that exec smbclient) covering full SMB3 feature set
- Automated multi-OS native client testing in CI (public repo = free GitHub Actions minutes): Windows (windows-latest, net use), macOS (macos-latest, mount_smbfs), Linux (ubuntu-latest, mount.cifs)
- Standard file ops + dialect verification per platform (not full feature matrix per OS)
- Manual deep testing of Windows/macOS remains in Phase 40.5
- Full Kerberos matrix: encryption + Kerberos, signing + Kerberos, leases + Kerberos, durable handles + Kerberos
- Test NTLM fallback and guest sessions
- Cross-protocol Kerberos: same principal accesses same file via both NFS (RPCSEC_GSS) and SMB (SPNEGO)
- Kerberos testing against AD as KDC (not full AD domain join -- just AD as Kerberos realm)
- Both smbtorture (--use-kerberos) and Go E2E tests cover Kerberos scenarios
- Bidirectional break matrix: SMB3 file lease to NFS write triggers break, NFS delegation to SMB open triggers recall, SMB directory lease to NFS create/delete/rename triggers break
- Moderate concurrency: 5-10 goroutines doing simultaneous NFS + SMB operations
- Verify both mechanism (break/recall fires) AND data consistency (content matches after conflict resolution)
- Include Kerberos cross-protocol: same Kerberos principal accesses via both protocols, identity mapper resolves consistently
- Audit and refactor GitHub Actions workflows to avoid overcrowding, especially PR checks
- Organize workflows sensibly: what runs on PR (fast/essential), what runs on push (comprehensive), what runs weekly (full matrix)
- Document CI workflows in docs/CONTRIBUTING.md
- Update test/smb-conformance/README.md with SMB3 suite details
- One comprehensive testing guide covering all suites
- Add CI section to docs/CONTRIBUTING.md covering all workflows, triggers, result interpretation, manual dispatch

### Claude's Discretion
- WPTS Scenario tests: evaluate whether to add beyond BVT based on available categories and value
- Exact smbtorture suite list per Kerberos run
- Test infrastructure implementation details (how to set up cross-compiled binaries in CI)
- smbclient output parsing approach
- Fix ordering within impact-based batches

### Deferred Ideas (OUT OF SCOPE)
- Full Active Directory domain join (DittoFS joins AD as member server with machine account, SPN registration, group policy support) -- new capability, needs its own phase
- WPTS Scenario tests -- may be evaluated during Phase 40 (Claude's discretion) but full expansion is a separate effort if warranted
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| TEST-01 | smbtorture SMB3 tests pass (durable_v2, lease, replay, session, encryption suites) | Existing smbtorture runner supports `--filter` for individual suites. Run.sh already iterates sub-suites. Need to add SMB3-specific suites (durable-v2-open, lease, replay, session, encryption) and Kerberos runs. KNOWN_FAILURES.md needs wildcard-to-individual conversion. |
| TEST-02 | Microsoft WPTS FileServer SMB3 BVT tests pass | Existing WPTS infrastructure complete. Need baseline re-measurement, then iterate-and-fix. WPTS supports additional categories (SMB2 Feature Test, Auth) beyond BVT worth exploring. |
| TEST-03 | Go integration tests (go-smb2) validate native client-server SMB3 interop | Add hirochachacha/go-smb2 as test dependency. Library supports SMB 3.0/3.0.2/3.1.1, AES-128-CCM/GCM, CMAC signing, preauth integrity. Tests in test/e2e/ using existing framework. |
| TEST-04 | Cross-protocol integration tests validate SMB3 leases vs NFS delegations | Existing cross_protocol_lock_test.go pattern. Need new cross_protocol_lease_test.go for bidirectional lease/delegation breaks with concurrency. |
| TEST-05 | Windows 10/11, macOS, and Linux client compatibility validated | Multi-OS CI via GitHub Actions matrix: windows-latest (net use), macos-latest (mount_smbfs), ubuntu-latest (mount.cifs). DittoFS built natively on each platform (pure Go, no CGO needed). |
| TEST-06 | E2E tests for SMB3 encryption, signing, leases, Kerberos, and durable handle scenarios | go-smb2 tests + smbclient tests. go-smb2 exercises SMB3 crypto directly. smbclient tests via exec from Go tests. Kerberos tests reuse existing KDC container framework. |
</phase_requirements>

## Standard Stack

### Core

| Library / Tool | Version | Purpose | Why Standard |
|----------------|---------|---------|--------------|
| hirochachacha/go-smb2 | latest | Native Go SMB3 client for E2E tests | Only maintained pure-Go SMB2/3 client. Supports 3.0/3.0.2/3.1.1, CCM/GCM, CMAC signing, preauth integrity. |
| smbtorture (samba-toolbox) | v0.8 (Docker: quay.io/samba.org/samba-toolbox:v0.8) | Industry SMB2/3 conformance suite | Samba project's official torture test framework. Already in use. |
| WPTS FileServer | v8 (Docker: mcr.microsoft.com/windowsprotocoltestsuites:fileserver-v8) | Microsoft's official SMB2/3 BVT suite | Microsoft's own protocol test suites. Already in use. |
| smbclient | OS package (smbclient) | CLI SMB client for automated tests | Standard Samba CLI client, available on all Linux distros. Supports protocol negotiation, encryption. |
| testify | v1.9+ | Go test assertions | Already in use throughout E2E test suite (assert, require). |
| testcontainers-go | v0.40.0 | Docker container lifecycle for test infrastructure | Already in use for Localstack, PostgreSQL, KDC containers. |

### Supporting

| Library / Tool | Version | Purpose | When to Use |
|----------------|---------|---------|-------------|
| xmlstarlet | OS package | TRX result parsing for WPTS | Already used in parse-results.sh for WPTS output. |
| Docker Compose | v2.x | Service orchestration for conformance tests | Already used for WPTS + smbtorture + DittoFS coordination. |
| GitHub Actions | N/A | Multi-OS CI automation | Windows/macOS/Linux runners for client compatibility testing. |
| kinit/klist (krb5-user) | OS package | Kerberos ticket management for tests | Already used in smb_kerberos_test.go and cross_protocol_kerberos_test.go. |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| go-smb2 | smbclient only | smbclient cannot test programmatic SMB3 features (encryption negotiation, lease requests, durable handle reconnect). go-smb2 gives full API control. |
| Multi-OS CI runners | Docker multi-platform | Real OS clients (net use, mount_smbfs, mount.cifs) validate actual kernel SMB stack, not just protocol encoding. Docker cannot replicate kernel CIFS client. |
| Manual smbtorture filter | Automated sub-suite iteration | Already solved: run.sh iterates sub-suites individually with prefix normalization. |

**Installation:**
```bash
# Add go-smb2 as test dependency
go get -t github.com/hirochachacha/go-smb2@latest

# System packages for CI (ubuntu-latest)
sudo apt-get install -y smbclient cifs-utils krb5-user xmlstarlet
```

## Architecture Patterns

### Recommended Test Structure

```
test/
├── smb-conformance/                    # Conformance suites (WPTS + smbtorture)
│   ├── run.sh                         # WPTS BVT runner (existing)
│   ├── parse-results.sh               # TRX parser (existing)
│   ├── KNOWN_FAILURES.md              # WPTS known failures (update: verify each entry)
│   ├── smbtorture/
│   │   ├── run.sh                     # smbtorture runner (extend: SMB3 suites, Kerberos)
│   │   ├── KNOWN_FAILURES.md          # smbtorture known failures (update: remove wildcards)
│   │   └── parse-results.sh           # smbtorture parser (existing)
│   └── docker-compose.yml             # Service orchestration (existing)
├── e2e/
│   ├── smb3_gosmb2_test.go            # NEW: go-smb2 SMB3 feature tests
│   ├── smb3_smbclient_test.go         # NEW: smbclient exec tests
│   ├── smb3_encryption_test.go        # NEW: Encryption-specific E2E
│   ├── smb3_lease_test.go             # NEW: Lease V2 E2E
│   ├── smb3_durable_test.go           # NEW: Durable handle reconnect E2E
│   ├── cross_protocol_lease_test.go   # NEW: SMB3 lease vs NFS delegation
│   ├── cross_protocol_kerberos_test.go # EXISTING: Extend with SMB3 features
│   ├── file_operations_smb_test.go    # EXISTING: Basic SMB ops
│   ├── smb_kerberos_test.go           # EXISTING: SMB Kerberos auth
│   └── framework/                     # EXISTING: Test helpers
└── .github/workflows/
    ├── smb-conformance.yml            # UPDATE: Add smbtorture Kerberos job
    ├── smb-client-compat.yml          # NEW: Multi-OS client compatibility
    ├── e2e-tests.yml                  # UPDATE: Add go-smb2 test suites
    └── ...
```

### Pattern 1: go-smb2 E2E Test Pattern
**What:** Use go-smb2 as a native Go SMB3 client to connect directly to DittoFS via TCP, testing SMB3 features programmatically.
**When to use:** Testing SMB3 features that require client-side control (encryption negotiation, dialect selection, lease requests, durable handle reconnect).
**Example:**
```go
//go:build e2e

package e2e

import (
    "net"
    "testing"

    smb2 "github.com/hirochachacha/go-smb2"
    "github.com/stretchr/testify/require"
)

func TestSMB3Encryption(t *testing.T) {
    // Start DittoFS server (reuse existing pattern)
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    // Setup stores, share, user, SMB adapter (reuse existing helpers)
    smbPort := setupSMB3TestShare(t, cli)

    // Connect with go-smb2 using SMB3 encryption
    conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", smbPort))
    require.NoError(t, err)
    defer conn.Close()

    d := &smb2.Dialer{
        Initiator: &smb2.NTLMInitiator{
            User:     "testuser",
            Password: "testpass123",
        },
    }

    s, err := d.Dial(conn)
    require.NoError(t, err, "SMB3 session setup should succeed")
    defer s.Logoff()

    // Mount share and perform file operations
    fs, err := s.Mount("smbbasic")
    require.NoError(t, err, "Should mount share")
    defer fs.Umount()

    // Write and read back
    err = fs.WriteFile("test.txt", []byte("encrypted content"), 0644)
    require.NoError(t, err)

    data, err := fs.ReadFile("test.txt")
    require.NoError(t, err)
    require.Equal(t, "encrypted content", string(data))
}
```

### Pattern 2: smbclient Test Pattern
**What:** Execute smbclient commands from Go tests and parse output for validation.
**When to use:** Testing SMB features visible through standard CLI tooling, validating dialect negotiation, encryption negotiation status.
**Example:**
```go
func TestSMBClientDialectNegotiation(t *testing.T) {
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)
    cli := helpers.LoginAsAdmin(t, sp.APIURL())
    smbPort := setupSMB3TestShare(t, cli)

    // Run smbclient with max protocol
    cmd := exec.Command("smbclient",
        fmt.Sprintf("//localhost/smbbasic"),
        "-p", fmt.Sprintf("%d", smbPort),
        "-U", "testuser%testpass123",
        "--max-protocol=SMB3",
        "-c", "ls; exit",
    )
    output, err := cmd.CombinedOutput()
    require.NoError(t, err, "smbclient should connect: %s", string(output))

    // Verify output contains directory listing
    require.Contains(t, string(output), "blocks", "Should show share info")
}
```

### Pattern 3: Multi-OS CI Workflow Pattern
**What:** Cross-platform client compatibility testing via GitHub Actions matrix.
**When to use:** Validating that real OS SMB clients can connect and operate.
**Example:**
```yaml
jobs:
  client-compat:
    strategy:
      matrix:
        include:
          - os: ubuntu-latest
            mount_cmd: "sudo mount -t cifs //localhost/smbbasic /mnt/test -o port=12445,username=test,password=test123,vers=3.1.1"
          - os: macos-latest
            mount_cmd: "mount_smbfs //test:test123@localhost:12445/smbbasic /mnt/test"
          - os: windows-latest
            mount_cmd: "net use Z: \\\\localhost\\smbbasic /user:test test123"
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25.x"
      - name: Build DittoFS
        run: go build -o dfs cmd/dfs/main.go
      - name: Start server and test
        run: |
          # Start DittoFS, bootstrap, mount, test file ops, verify dialect
```

### Pattern 4: Iterate-and-Fix Cycle
**What:** Systematic approach to reducing conformance failures.
**When to use:** For each batch of smbtorture/WPTS failures.
**Workflow:**
1. Run full suite with memory profile (fastest)
2. Parse results, identify new failures NOT in KNOWN_FAILURES
3. Group failures by root cause (e.g., all failures caused by missing STATUS_X response)
4. Fix highest-impact root cause first (cascade: fix that unblocks most tests)
5. Re-run affected sub-suite to verify fix
6. Commit fix with atomic commit
7. Repeat until no new failures

### Anti-Patterns to Avoid
- **Wildcard known failures:** Using `smb2.session.*` hides real regressions. Enumerate each test individually after investigation.
- **Fixing tests in isolation:** Always re-run the full sub-suite after a fix to catch regressions.
- **Skipping test investigation:** Every failure MUST be investigated before being added to KNOWN_FAILURES. "It fails" is not a valid reason.
- **Running all 5 profiles during iteration:** Use memory profile for speed during fix cycles. Run all profiles only as final gate.
- **Monolithic fix plans:** Each fix batch should be a separate small plan with its own verification step.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SMB3 client for testing | Custom TCP SMB3 client | hirochachacha/go-smb2 | Full SMB3 stack with encryption, signing, preauth integrity. Reimplementing would be thousands of lines. |
| SMB conformance parsing | Custom result parser | Existing parse-results.sh (smbtorture) and parse-results.sh (WPTS/TRX) | Already handles known-failure matching, wildcard patterns, exit codes. |
| Docker service orchestration | Shell script service management | Existing docker-compose.yml | Health checks, network modes, profile support already configured. |
| Kerberos KDC setup | Manual KDC configuration | Existing framework.NewKDCHelper | Container lifecycle, principal management, keytab extraction all handled. |
| Multi-OS CI | Manual cross-compilation scripts | GitHub Actions matrix with native runners | Each runner has native SMB client stack. Pure Go = no CGO needed for cross-compilation. |

**Key insight:** The test infrastructure is already mature. Phase 40's value is in using it systematically, not building new frameworks.

## Common Pitfalls

### Pitfall 1: Wildcard Known Failures Masking Regressions
**What goes wrong:** Using `smb2.session.*` in KNOWN_FAILURES means a regression in a previously-passing test within that suite goes undetected.
**Why it happens:** During initial bring-up, entire suites fail so wildcards are convenient. As features are implemented, some tests start passing but the wildcard still covers them.
**How to avoid:** Replace every wildcard with individual test names. Run the suite, capture all individual test outcomes, then add only genuinely-failing tests.
**Warning signs:** A suite has a wildcard entry but you know some features within it are now implemented.

### Pitfall 2: smbtorture Output Format Variations
**What goes wrong:** smbtorture output format varies between versions and sub-suites. Some use `success: testname`, others use `testname ok`.
**Why it happens:** smbtorture has multiple output backends and the format depends on test runner configuration.
**How to avoid:** The existing parse-results.sh already handles both formats. When adding new suites, verify the output format matches what the parser expects. Use the `--verbose` flag during debugging.
**Warning signs:** CI reports "No test results found" even though smbtorture ran.

### Pitfall 3: ARM64 Emulation Timeouts
**What goes wrong:** smbtorture and WPTS Docker images are linux/amd64. On ARM64 (Apple Silicon), they run under Rosetta/QEMU, which is 3-5x slower, causing timeouts.
**Why it happens:** No native ARM64 builds of samba-toolbox or WPTS.
**How to avoid:** Use CI (ubuntu-latest x86_64) for conformance testing. Local development on ARM64 should use generous timeout values or `--filter` for individual tests.
**Warning signs:** `NT_STATUS_NO_MEMORY` errors in rapid connection tests under emulation.

### Pitfall 4: go-smb2 Dialect Negotiation
**What goes wrong:** go-smb2 negotiates the highest mutually-supported dialect by default. If you want to test a specific dialect (e.g., 3.0.2), you need to configure the Dialer explicitly.
**Why it happens:** The library auto-negotiates for convenience but tests need control over dialect selection.
**How to avoid:** Set `Dialer.MaxProtocol` or use protocol-specific configuration when testing specific dialect behaviors.
**Warning signs:** Test says "SMB 3.1.1 encryption" but actually negotiated 2.1.

### Pitfall 5: SMB Client Caching in Cross-Protocol Tests
**What goes wrong:** SMB clients aggressively cache file data and metadata. Cross-protocol tests see stale data when reading via SMB after NFS write.
**Why it happens:** CIFS kernel client (mount.cifs) has attribute caching (actimeo) and data caching (cache=strict) by default.
**How to avoid:** Use `cache=none` for mount.cifs mounts. Use `actimeo=0` for NFS mounts. Add `time.Sleep(200ms)` for metadata sync after cross-protocol operations (existing pattern).
**Warning signs:** Cross-protocol test passes on Linux but fails on macOS (different default caching behavior).

### Pitfall 6: WPTS Test Categories and Filters
**What goes wrong:** Running `TestCategory=Model` or `TestCategory=SMB311` instead of `TestCategory=BVT` pulls in 2600+ tests that take hours and many are irrelevant.
**Why it happens:** WPTS has multiple test categories with vastly different scope. BVT (101 tests) vs Feature Test (2664 tests).
**How to avoid:** Stick with BVT for CI. Use specific filter combinations like `TestCategory=BVT&TestCategory=Encryption` when iterating on specific features. Run broader categories only for one-off exploration.
**Warning signs:** CI job exceeds 30-minute timeout.

### Pitfall 7: smbclient Output Parsing Fragility
**What goes wrong:** smbclient output format changes between versions. Parsing specific fields breaks on version upgrades.
**Why it happens:** smbclient is designed for interactive use, not machine-readable output.
**How to avoid:** Parse minimally -- check for error indicators (`NT_STATUS_*`), success indicators (file listings), and dialect info (`Protocol negotiated: SMB3.x`). Don't parse column positions.
**Warning signs:** Tests fail after OS package update.

## Code Examples

### go-smb2 SMB3 Session with Encryption Verification
```go
// Source: go-smb2 library API + DittoFS E2E patterns
func connectSMB3WithEncryption(t *testing.T, port int, user, pass string) (*smb2.Session, *smb2.Share) {
    t.Helper()

    conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", port))
    require.NoError(t, err)

    d := &smb2.Dialer{
        Initiator: &smb2.NTLMInitiator{
            User:     user,
            Password: pass,
        },
    }

    s, err := d.Dial(conn)
    require.NoError(t, err, "SMB3 session with encryption should succeed")

    fs, err := s.Mount("smbbasic")
    require.NoError(t, err, "Should mount share")

    return s, fs
}
```

### smbclient Exec Helper
```go
// Source: DittoFS E2E pattern for executing external commands
func runSMBClient(t *testing.T, port int, user, pass, share, command string) (string, error) {
    t.Helper()

    cmd := exec.Command("smbclient",
        fmt.Sprintf("//localhost/%s", share),
        "-p", fmt.Sprintf("%d", port),
        "-U", fmt.Sprintf("%s%%%s", user, pass),
        "--max-protocol=SMB3",
        "-c", command,
    )

    output, err := cmd.CombinedOutput()
    return string(output), err
}
```

### Cross-Protocol Lease Break Test Pattern
```go
// Source: Derived from existing cross_protocol_lock_test.go pattern
func TestSMB3LeaseBreakOnNFSWrite(t *testing.T) {
    // Setup dual-protocol server (NFS + SMB) with shared stores
    sp, nfsMount, smbPort := setupDualProtocolServer(t)

    // Acquire SMB3 Read lease via go-smb2
    s, fs := connectSMB3WithEncryption(t, smbPort, "testuser", "testpass123")
    defer s.Logoff()
    defer fs.Umount()

    // Open file with lease (go-smb2 requests lease automatically)
    f, err := fs.OpenFile("shared-file.txt", os.O_RDWR|os.O_CREATE, 0644)
    require.NoError(t, err)
    defer f.Close()

    // Write via NFS -- should trigger SMB lease break
    nfsFile := nfsMount.FilePath("shared-file.txt")
    framework.WriteFile(t, nfsFile, []byte("written via NFS"))

    // Verify data consistency: SMB read should see NFS-written content
    time.Sleep(500 * time.Millisecond) // Allow lease break to propagate
    content, err := fs.ReadFile("shared-file.txt")
    require.NoError(t, err)
    require.Equal(t, "written via NFS", string(content))
}
```

### smbtorture Kerberos Run
```bash
# Source: Derived from existing smbtorture run.sh patterns
# Run smbtorture with Kerberos authentication
docker compose run --rm smbtorture \
    "//localhost/smbbasic" \
    "-U" "alice%alice123" \
    "--use-kerberos=required" \
    "--option=client min protocol=SMB3" \
    "--option=client max protocol=SMB3_11" \
    "smb2.session"
```

### Multi-OS CI Step (Windows)
```yaml
# Source: Derived from existing windows-build.yml pattern
- name: Start DittoFS and test SMB
  shell: powershell
  run: |
    # Start DittoFS in background
    Start-Process -FilePath .\dfs.exe -ArgumentList "start","--foreground" `
      -RedirectStandardOutput dfs.log -NoNewWindow
    Start-Sleep -Seconds 5

    # Map network drive
    net use Z: \\localhost\smbbasic /user:test testpass123

    # Create and read file
    echo "test content" > Z:\test.txt
    $content = Get-Content Z:\test.txt
    if ($content -ne "test content") { exit 1 }

    # Verify dialect (PowerShell)
    $sessions = Get-SmbSession
    Write-Host "SMB Dialect: $($sessions.Dialect)"

    # Cleanup
    net use Z: /delete
    Stop-Process -Name dfs -Force
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Wildcard known failures (`smb2.session.*`) | Individual test enumeration | Phase 40 (decision) | Every test name in KNOWN_FAILURES individually verified |
| NTLM-only smbtorture | NTLM + Kerberos (`--use-kerberos`) | Phase 40 (decision) | Full auth coverage for SMB3 suites |
| Memory-only iteration | Memory for iteration, all 5 profiles as gate | Phase 40 (decision) | Fast iteration + comprehensive final validation |
| Single monolithic test plan | Multiple small plans per fix batch | Phase 40 (decision) | Atomic commits, easier bisection, faster iteration |
| No go-smb2 integration | Full go-smb2 E2E suite | Phase 40 (new) | Native Go client validates SMB3 features directly |
| No multi-OS CI | Windows + macOS + Linux runners | Phase 40 (new) | Real kernel SMB clients validate compatibility |

## WPTS Categories Beyond BVT (Claude's Discretion Assessment)

Based on research, WPTS FileServer categories include:

| Category | Test Count | Relevance | Recommendation |
|----------|-----------|-----------|----------------|
| BVT | ~101 | Core basic verification | MUST run (already running) |
| SMB2 Feature Test | ~2,664 | Deep feature testing | TOO LARGE for CI. Cherry-pick: run Encryption, Leasing, Handle, Signing, Negotiate sub-categories individually during investigation. |
| Server Failover | ~48 | Clustering/HA | NOT APPLICABLE (DittoFS is single-node) |
| RSVD | ~29 | Virtual Hard Disk | NOT APPLICABLE (DittoFS has no VHD support) |
| DFSC | ~41 | Distributed File System | NOT APPLICABLE (DittoFS has no DFS) |
| Auth | Varies | Authentication | INVESTIGATE: May test NTLM/Kerberos/guest scenarios relevant to DittoFS |

**Recommendation:** Stick with BVT as the CI gate. During iteration, use specific Feature Test sub-categories (e.g., `TestCategory=BVT&TestCategory=Encryption`) to investigate specific failure areas. Do NOT add full Feature Test (2664 tests) to CI -- it would take hours and most tests cover features DittoFS does not implement.

## smbtorture SMB3 Suites for Kerberos (Claude's Discretion)

For `--use-kerberos` runs, recommend these suites that exercise session management and authentication:

| Suite | Why Include |
|-------|-------------|
| smb2.session | Session setup, binding, reconnect -- directly tests Kerberos session establishment |
| smb2.session-require-signing | Signing enforcement with Kerberos sessions |
| smb2.connect | Connection negotiation with Kerberos auth |
| smb2.durable-v2-open | Durable handle reconnect -- tests session key continuity |
| smb2.lease | Lease grants -- tests authenticated lease operations |

Full smb2 suite with `--use-kerberos` is ideal but may be slow. Start with the 5 above, expand if time permits.

## Cross-Compilation for Multi-OS CI (Claude's Discretion)

DittoFS is pure Go with no CGO dependencies. Cross-compilation is straightforward:

```bash
# Build for each target platform (no CGO, no special toolchain needed)
GOOS=linux   GOARCH=amd64 go build -o dfs-linux   cmd/dfs/main.go
GOOS=darwin  GOARCH=amd64 go build -o dfs-darwin  cmd/dfs/main.go
GOOS=windows GOARCH=amd64 go build -o dfs.exe     cmd/dfs/main.go
```

**Recommendation:** Build natively on each runner (no cross-compilation needed). Each GitHub Actions runner (ubuntu-latest, macos-latest, windows-latest) has Go installed via `actions/setup-go`. Build and test on the same platform for maximum fidelity.

## smbclient Output Parsing (Claude's Discretion)

**Recommendation:** Minimal parsing approach:
1. Check exit code (0 = success, non-zero = failure)
2. Check for `NT_STATUS_*` error strings in stderr/stdout
3. For dialect verification, use `smbclient --debuglevel=1` and grep for `Protocol negotiated:`
4. For file listings, check for presence of expected filenames
5. Do NOT parse column positions or fixed-width output

## Open Questions

1. **WPTS Auth Category Availability**
   - What we know: WPTS includes Auth as a category, but the Docker image may not include all test assemblies
   - What's unclear: Which specific Auth tests are available in the `fileserver-v8` Docker image
   - Recommendation: Try running `TestCategory=Auth` during baseline measurement. If tests exist, evaluate relevance. If not, skip.

2. **go-smb2 Lease and Durable Handle Support**
   - What we know: go-smb2 supports SMB 3.0/3.0.2/3.1.1 with encryption and signing
   - What's unclear: Whether go-smb2 exposes APIs for explicit lease requests or durable handle creation (these are typically transparent to the client)
   - Recommendation: Examine go-smb2 API surface during implementation. If no explicit lease/durable APIs, test these features via smbtorture instead, and use go-smb2 for basic SMB3 file operations.

3. **smbtorture Kerberos in Docker**
   - What we know: smbtorture supports `--use-kerberos`. The existing KDC container infrastructure works for E2E tests.
   - What's unclear: Whether the samba-toolbox Docker image has Kerberos client libraries pre-installed
   - Recommendation: Test with `docker run --rm quay.io/samba.org/samba-toolbox:v0.8 kinit --version` to verify. If not available, create a custom Dockerfile extending samba-toolbox with krb5-user.

4. **Windows Runner SMB Dialect Verification**
   - What we know: Windows has `Get-SmbSession` and `Get-SmbConnection` PowerShell cmdlets
   - What's unclear: Whether these cmdlets work with non-default ports and localhost connections
   - Recommendation: Test on windows-latest runner. If cmdlets don't work, use `net use` output which shows connection status.

## Sources

### Primary (HIGH confidence)
- Codebase: `test/smb-conformance/smbtorture/run.sh` -- existing smbtorture infrastructure
- Codebase: `test/smb-conformance/run.sh` -- existing WPTS BVT infrastructure
- Codebase: `test/e2e/file_operations_smb_test.go` -- existing SMB E2E pattern
- Codebase: `test/e2e/smb_kerberos_test.go` -- existing Kerberos SMB test pattern
- Codebase: `test/e2e/cross_protocol_test.go` -- existing cross-protocol pattern
- Codebase: `test/e2e/cross_protocol_lock_test.go` -- existing cross-protocol lock pattern
- Codebase: `.github/workflows/smb-conformance.yml` -- existing CI pipeline
- [go-smb2 const.go](https://github.com/hirochachacha/go-smb2/blob/master/internal/smb2/const.go) -- Dialect and cipher support verification
- [go-smb2 session.go](https://github.com/hirochachacha/go-smb2/blob/master/session.go) -- Key derivation and preauth hash verification

### Secondary (MEDIUM confidence)
- [Microsoft WPTS FileServer Test Design](https://github.com/Microsoft/WindowsProtocolTestSuites/blob/main/TestSuites/FileServer/docs/FileServerTestDesignSpecification.md) -- Test category breakdown
- [smbtorture man page](https://manpages.debian.org/testing/samba-testsuite/smbtorture.1.en.html) -- `--use-kerberos` flag documentation
- [SMB 3.0 Encryption Protocol Perspective](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective) -- SMB3 encryption reference
- [GitHub Actions Go Cross-Build](https://medium.com/@dedicatted/transforming-development-workflow-cross-building-go-binaries-with-github-actions-ea96d77e2cbf) -- Multi-platform build patterns

### Tertiary (LOW confidence)
- WPTS Auth category test availability -- needs empirical verification with the Docker image
- go-smb2 explicit lease/durable handle API surface -- needs code inspection during implementation

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- go-smb2 verified to support SMB 3.0/3.0.2/3.1.1 with CCM/GCM/CMAC. All existing infrastructure inspected.
- Architecture: HIGH -- Extending established patterns (E2E framework, smbtorture runner, CI pipeline). No new architectural decisions.
- Pitfalls: HIGH -- Based on direct codebase inspection (ARM64 emulation issues documented in KNOWN_FAILURES, client caching patterns in existing tests).

**Research date:** 2026-03-02
**Valid until:** 2026-04-01 (stable -- testing infrastructure, not fast-moving libraries)
