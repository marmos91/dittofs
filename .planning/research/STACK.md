# Technology Stack: SMB2 Conformance and Windows Compatibility (v3.6)

**Project:** DittoFS Windows Compatibility
**Researched:** 2026-02-26
**Scope:** Stack additions/changes for fixing SMB2 conformance failures, NT Security Descriptors (Owner SID, Group SID, DACL), Unix-to-Windows SID mapping, sparse file READ, renamed directory listing, and smbtorture/WPTS testing infrastructure.

## Recommended Stack

### No New Go Dependencies Required

**Confidence: HIGH** (verified by reading existing codebase)

DittoFS already has a complete, hand-rolled NT Security Descriptor implementation in `internal/adapter/smb/v2/handlers/security.go` that handles:

- SID encoding/decoding (MS-DTYP 2.4.2)
- Self-relative Security Descriptor building/parsing (MS-DTYP 2.4.6)
- ACL/ACE encoding with NFSv4-to-Windows type mapping
- Well-known SID table (Everyone, Creator Owner, Creator Group)
- DittoFS-specific SID scheme: `S-1-5-21-0-0-0-{UID/GID}`
- Bidirectional SID-to-Principal conversion
- Full round-trip tests (build -> parse -> verify)

This implementation uses only `encoding/binary`, `bytes`, `strconv`, and `strings` from the standard library. **No external library is needed.**

### Evaluated and Rejected: External SD Libraries

| Library | Version | Why Rejected |
|---------|---------|-------------|
| `github.com/cloudsoda/sddl` | v0.0.0-20250224 | LGPL-3.0 license (incompatible with DittoFS). Only adds SDDL string format we do not need. DittoFS only uses binary wire format over SMB. Also depends on `golang.org/x/sys` which pulls in Windows-specific code. |
| `github.com/huner2/go-sddlparse` | v0.1.0 | Focused on SDDL string parsing, not binary encoding. We encode binary SDs from NFSv4 ACLs, not from SDDL strings. |
| `golang.org/x/sys/windows` | N/A | Windows-only. DittoFS is cross-platform (Linux server, macOS dev). SID types in this package require Windows syscalls. |

**Rationale:** The existing 626-line `security.go` is purpose-built for DittoFS's use case (NFSv4 ACL <-> Windows SD translation). It covers all MS-DTYP encoding requirements. Adding an external library would:
1. Introduce license risk (LGPL-3.0)
2. Add complexity for features we do not use (SDDL strings, file security extraction)
3. Require mapping between external types and our ACL model anyway
4. Break the pure-Go cross-platform story

### Core Framework (Unchanged)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| Go | 1.25.0 | Language runtime | Already in go.mod |
| Cobra | v1.8.1 | CLI framework | Already used for dfs/dfsctl |
| GORM | v1.31.1 | Control plane store | Already used for user/group/share/permission persistence |
| BadgerDB | v4.5.2 | Metadata persistence | Already used for persistent metadata stores |

### Testing Infrastructure (New)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| Samba smbtorture | 4.21+ (system) | SMB2 protocol conformance | De facto standard for SMB implementation testing. Covers SMB2-GETINFO, SMB2-SETINFO, SMB2-ACLS, SMB2-CREATE, SMB2-READ, SMB2-WRITE, SMB2-LOCK, SMB2-OPLOCK, SMB2-LEASE. Install via system package manager, not Go dependency. |
| MS WindowsProtocolTestSuites | latest (external) | SMB2 BVT + feature tests | 101 BVT tests + 2,664 feature tests covering SMB2002 through SMB311. .NET-based, runs on Linux via dotnet CLI. Tests CreateClose, Oplock, Leasing, SessionMgmt, Signing, CreditMgmt. |
| `github.com/hirochachacha/go-smb2` | v1.1.0 (test only) | Go SMB2 client for integration tests | Pure Go SMB2 client for writing Go-native integration tests that exercise our server. Can test NEGOTIATE, SESSION_SETUP, TREE_CONNECT, CREATE, READ, WRITE, CLOSE, QUERY_INFO flows from Go test code. Add as test-only dependency. |
| Windows 11 VM | 23H2+ | Manual validation | Required for Explorer, cmd.exe, PowerShell, icacls.exe testing. Use Hyper-V or UTM on macOS. |

### Supporting Libraries (Unchanged, Already Present)

| Library | Version | Purpose | Relevant For v3.6 |
|---------|---------|---------|-------------------|
| `golang.org/x/crypto` | v0.45.0 | NTLM/crypto | Used by existing SMB auth |
| `github.com/jcmturner/gokrb5/v8` | v8.4.4 | Kerberos | Used by existing SPNEGO/Kerberos auth |
| `github.com/stretchr/testify` | v1.11.1 | Test assertions | Unit tests for SD encoding |
| `encoding/binary` | stdlib | Binary encoding | Core of all SMB2 wire encoding |

## What Needs to Change (Code, Not Dependencies)

### 1. Sparse File READ Fix (#180)

**Location:** `internal/adapter/smb/v2/handlers/read.go` (Read handler, Step 9)

**Problem:** When reading a sparse file (file with PayloadID but no data written at requested offset), the payload service returns an error or short read instead of zero-filled bytes.

**Fix:** The Read handler at line 243 already handles empty files (PayloadID == ""), but does not handle the case where PayloadID exists but blocks at the requested offset have not been written. The payload IO layer (`pkg/payload/io/read.go`) attempts to download blocks via `EnsureAvailable` and fails when they do not exist in the block store.

**Stack impact:** None. Fix is in handler logic -- detect short reads and zero-fill remaining bytes. May also need a change in `EnsureAvailable` to return a "not found" signal instead of an error for blocks that were never written.

### 2. Renamed Directory Listing Fix (#181)

**Location:** `pkg/metadata/file_modify.go` (Move method, line 299+) and the underlying metadata store `Rename` implementations.

**Problem:** After renaming a directory, the `Path` field of child files/directories is not updated. Subsequent `ListChildren` or path-based lookups return stale paths.

**Fix:** The metadata store `Rename` operation must recursively update the `Path` field of all descendants when a directory is renamed. This is a metadata store implementation fix (memory, BadgerDB, PostgreSQL stores).

**Stack impact:** None. Fix is in existing store implementations.

### 3. NT Security Descriptor Improvements (#182)

**Location:** `internal/adapter/smb/v2/handlers/security.go`

**Current state:** Already functional with SID encoding, DACL building, and round-trip parsing. Needs:

- **SACL support:** Add stub SACL (empty, with SE_SACL_PRESENT flag) for clients that request SACL_SECURITY_INFORMATION. Windows Explorer requests SACL even if empty.
- **SE_DACL_AUTO_INHERITED flag:** Set when DACL contains inherited ACEs (already tracked in NFSv4 ACL flags).
- **SE_DACL_PROTECTED flag:** Honor this in SET_INFO to prevent inheritance propagation.
- **Inheritance flags on ACEs:** Map OBJECT_INHERIT_ACE, CONTAINER_INHERIT_ACE, NO_PROPAGATE_INHERIT_ACE correctly (partially implemented -- ACE4 inheritance flags are stored but only low byte is used in encoding).
- **Generic rights mapping:** Map GENERIC_READ/WRITE/EXECUTE/ALL to specific file rights when received in SET_INFO (MS-SMB2 Section 2.2.13.1.2).
- **Canonical ACE ordering:** Already enforced by `pkg/metadata/acl/validate.go` (explicit deny before explicit allow before inherited deny before inherited allow).

**Stack impact:** None. All work is in existing security.go + minor additions.

### 4. SID Mapping Enhancements

**Location:** `internal/adapter/smb/v2/handlers/security.go` (PrincipalToSID, SIDToPrincipal)

**Current state:** Maps OWNER@/GROUP@/EVERYONE@ and numeric UIDs. Needs:

- **Well-known SID expansion:** Add BUILTIN\Administrators (S-1-5-32-544), BUILTIN\Users (S-1-5-32-545), NT AUTHORITY\SYSTEM (S-1-5-18), NT AUTHORITY\Authenticated Users (S-1-5-11). These are commonly queried by icacls and Windows Explorer.
- **Control plane user-to-SID mapping:** When a DittoFS user has a username, use deterministic SID generation (hash-based or control plane assigned) rather than UID-based.
- **SID caching:** For repeated lookups during directory listing, cache resolved SIDs per session.

**Stack impact:** None. Extension of existing mapping tables.

### 5. icacls Compatibility

**icacls.exe** reads and writes security descriptors via SMB2 QUERY_INFO/SET_INFO with InfoType=3 (security). Current implementation handles this path. Additional work:

- **DACL must use proper ACE ordering** (already enforced by acl.ValidateACL)
- **icacls expects well-known SID names** (BUILTIN\Administrators, etc.) -- this is client-side SID-to-name resolution via LSARPC, not server-side. DittoFS does not need to implement LSARPC for basic icacls support.
- **SET_INFO security** must accept icacls DACL modifications -- already functional via `setSecurityInfo`.

**Stack impact:** None.

## Alternatives Considered

| Category | Recommended | Alternative | Why Not |
|----------|-------------|-------------|---------|
| SD encoding | Hand-rolled (existing) | cloudsoda/sddl | LGPL license, SDDL format not needed, adds dep for what we already have |
| SD encoding | Hand-rolled (existing) | go-sddlparse | String format only, no binary encoding |
| SMB2 conformance | smbtorture (external) | Custom Go tests only | smbtorture is the industry standard, catches edge cases no one thinks to test |
| SMB2 BVT | MS WPTS (external) | smbtorture only | WPTS tests specific MS-SMB2 spec compliance, complements smbtorture |
| Go SMB2 client | hirochachacha/go-smb2 | cloudsoda/go-smb2 | hirochachacha is more widely used, simpler API, better documented |
| SID generation | UID-based (S-1-5-21-0-0-0-{UID}) | Domain SIDs (S-1-5-21-{machine}-{RID}) | Domain SIDs require machine SID persistence, over-engineering for current scope |

## Installation

### smbtorture (macOS)

```bash
# Install via Homebrew
brew install samba

# Verify
smbtorture --help

# Run specific SMB2 tests against DittoFS
smbtorture //localhost/export -U admin%password \
  smb2.getinfo smb2.setinfo smb2.acls smb2.create smb2.read
```

### smbtorture (Linux/Docker)

```bash
# Install samba test tools
apt-get install samba-testsuite  # Debian/Ubuntu

# Or use Docker
docker run --network=host samba-testsuite \
  smbtorture //host.docker.internal/export -U admin%password smb2.getinfo
```

### Microsoft WindowsProtocolTestSuites (Linux)

```bash
# Requires .NET SDK
dotnet tool install --global PTMCli

# Download latest release
wget https://github.com/microsoft/WindowsProtocolTestSuites/releases/latest/download/FileServer.zip
unzip FileServer.zip

# Run BVT tests
./RunTestCasesByFilter.sh "TestCategory=BVT&TestCategory=SMB2" "list"
./RunTestCasesByFilter.sh "TestCategory=BVT&TestCategory=SMB2" "run"
```

### Go SMB2 Client (test dependency)

```bash
# Add as test-only dependency
cd /Users/marmos91/Projects/dittofs
go get -t github.com/hirochachacha/go-smb2@v1.1.0
```

### Windows 11 VM (manual testing)

```bash
# macOS with UTM (Apple Silicon)
# Download Windows 11 ARM ISO from Microsoft
# Create VM with networking bridged to host
# From Windows CMD:
net use Z: \\<mac-ip>\export /user:admin password
dir Z:\
icacls Z:\testfile
```

## Testing Strategy by Tool

| Tool | What It Tests | When to Run | Confidence |
|------|---------------|-------------|------------|
| `go test ./...` | Unit tests for SD encoding, SID mapping, ACL validation | Every commit | HIGH |
| `go-smb2` integration tests | End-to-end NEGOTIATE through READ/WRITE/QUERY_INFO | Every PR | HIGH |
| smbtorture `smb2.*` | Protocol conformance (getinfo, setinfo, acls, create, read, lock, oplock) | Before release, after major changes | HIGH |
| MS WPTS BVT | Spec-level compliance for SMB2 operations | Milestone gate | MEDIUM (complex setup) |
| Windows 11 manual | Explorer, cmd, PowerShell, icacls real-world compatibility | Milestone gate | HIGH (irreplaceable) |

## What NOT to Add

| Temptation | Why Avoid |
|------------|-----------|
| SDDL string format support | No SMB client sends SDDL strings. Wire format is always binary. SDDL is a Windows admin display format only. |
| LSARPC/SAMR named pipe endpoints | icacls resolves SID-to-name locally. DittoFS does not need to implement LSA RPCs for basic ACL support. These are v3.8+ scope (full AD integration). |
| Domain SID with machine-SID persistence | Over-engineering. UID-based SIDs (S-1-5-21-0-0-0-{UID}) work for all current use cases. Domain SIDs are v3.8 scope with SMB3 + AD. |
| SACL enforcement | SACL is for auditing, not access control. Return empty SACL when requested but do not enforce audit events. |
| SMB3 encryption/signing | Out of scope for v3.6. Current SMB2 signing (HMAC-SHA256) is sufficient. SMB3 AES encryption is v3.8. |
| go-smb2 as a production dependency | Only use for testing. DittoFS is a server, not a client. |

## Sources

- [MS-DTYP Section 2.4.2 - SID](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/78eb9013-1c3a-4970-ad1f-2b1dad55a57d) -- SID binary encoding (HIGH confidence, official spec)
- [MS-DTYP Section 2.4.6 - SECURITY_DESCRIPTOR](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/7d4dac05-9cef-4563-a058-f108abecce1d) -- Self-relative SD layout (HIGH confidence, official spec)
- [MS-SMB2 Section 2.2.37-38 - QUERY_INFO/response](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) -- Security info query/set (HIGH confidence, official spec)
- [MS-FSCC - Sparse files](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/6a884fe5-3da1-4abb-84c4-f419d349d878) -- FSCTL_SET_SPARSE and zero-fill behavior (HIGH confidence, official spec)
- [Samba smbtorture source - smb2/acls.c](https://github.com/samba-team/samba/blob/master/source4/torture/smb2/acls.c) -- ACL test scenarios: creator_sid, generic_bits, owner_bits, inheritance, inheritance_flags (HIGH confidence, reference implementation)
- [CloudSoda/sddl](https://github.com/CloudSoda/sddl) -- Evaluated and rejected (LGPL-3.0, SDDL format, not binary encoding) (HIGH confidence, direct inspection)
- [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) -- SMB2 BVT and feature tests, runs on Linux (HIGH confidence, official Microsoft test suite)
- [hirochachacha/go-smb2](https://github.com/hirochachacha/go-smb2) -- Go SMB2 client for integration testing (MEDIUM confidence, community library)
- [MS-SMB2 Sparse file notes](https://learn.microsoft.com/en-us/archive/blogs/openspecification/notes-on-sparse-files-and-file-sharing) -- Zero-fill semantics for unallocated ranges (HIGH confidence, official blog)
