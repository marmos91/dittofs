# Project Research Summary

**Project:** DittoFS v3.6 - SMB2 Conformance and Windows Compatibility
**Domain:** SMB2 protocol conformance, NT Security Descriptors, Windows 11 client compatibility
**Researched:** 2026-02-26
**Confidence:** HIGH

## Executive Summary

DittoFS v3.6 is a **targeted bug-fix and conformance milestone** addressing three known issues (#180 sparse READ, #181 renamed directories, #182 NT Security Descriptors) plus Windows 11 24H2 compatibility. All changes are **extensions of existing modules** - no new packages, interfaces, or architectural patterns are required. The estimated work is 200-250 lines across 4-5 files.

The recommended approach is a three-phase execution: (A) fix the two critical bugs in parallel (sparse READ and renamed directory paths), (B) enhance the existing security.go NT Security Descriptor implementation with proper POSIX-to-DACL synthesis and well-known SIDs, (C) validate with smbtorture and Windows Protocol Test Suites. The existing 626-line security.go already has complete SD encoding - we extend it, not rebuild it.

Key risks are: (1) Security Descriptor field ordering must match Windows byte layout (not just MS-DTYP spec) to pass smbtorture conformance, (2) Windows 11 24H2 requires functional SMB signing for NTLM auth (guest mode blocked by default), (3) SID collision between user/group numeric IDs must be resolved to avoid icacls confusion. All three have clear mitigation strategies based on Samba reference implementation patterns.

## Key Findings

### Recommended Stack

**No new Go dependencies required.** DittoFS already has a complete, hand-rolled NT Security Descriptor implementation in `internal/adapter/smb/v2/handlers/security.go` that handles SID encoding, self-relative Security Descriptor building, ACL/ACE encoding with NFSv4-to-Windows type mapping, and bidirectional SID-to-Principal conversion. External libraries (cloudsoda/sddl, go-sddlparse, golang.org/x/sys/windows) were evaluated and rejected due to LGPL licensing, SDDL-only focus, or platform constraints.

**Core technologies (unchanged):**
- Go 1.25.0: Language runtime (already in go.mod)
- Cobra v1.8.1: CLI framework (already used for dfs/dfsctl)
- GORM v1.31.1: Control plane persistence (already used)
- BadgerDB v4.5.2: Metadata persistence (already used)

**Testing infrastructure (new):**
- Samba smbtorture 4.21+: De facto SMB2 protocol conformance standard (install via system package manager)
- Microsoft WindowsProtocolTestSuites: 101 BVT + 2,664 feature tests covering SMB2002-SMB311 (.NET-based, runs on Linux)
- github.com/hirochachacha/go-smb2 v1.1.0: Pure Go SMB2 client for integration tests (test-only dependency)
- Windows 11 VM 23H2+: Manual validation with Explorer, cmd.exe, PowerShell, icacls.exe

### Expected Features

**Must fix (bugs and broken behavior):**
- Sparse file READ zero-fill (#180) - READ at offsets beyond written data must return zeros, not error. Blocks basic Windows file operations.
- Renamed directory listing (#181) - After SET_INFO rename, child paths must reflect new location. Blocks Explorer navigation.
- CREATE response context wire encoding - Lease/MxAc responses silently dropped without proper context chain serialization.

**Must have (Windows compatibility):**
- Default DACL from POSIX permissions - Synthesize Windows DACL from Unix mode bits instead of "Everyone: full control"
- ACE ordering (deny before allow) - Windows canonical ACE ordering required for icacls
- Well-known SIDs (SYSTEM, Administrators, Everyone) - Expected in default DACLs
- MxAc create context response - Windows Explorer uses for UI decisions (rename/delete context menus)
- SMB signing enforcement - Windows 11 24H2 rejects unsigned sessions

**Should have (conformance):**
- Inheritance flags on ACEs (CONTAINER_INHERIT, OBJECT_INHERIT) - Required for directory ACL propagation
- Domain-aware SID construction - Stable SID generation (not S-1-5-21-0-0-0)
- SE_DACL_AUTO_INHERITED flag - Removes "inheritance disabled" warning
- Missing FileInfoClass handlers (FileCompressionInformation, FileAttributeTagInformation, FileModeInformation)

**Defer (v3.8+):**
- SMB 3.0/3.1.1 dialect negotiation - Full SMB3 is separate milestone with encryption
- AES encryption - Requires SMB3 session setup, key derivation
- Durable handles (DHnQ/DH2Q) - Complex reconnection semantics
- Server-side copy (FSCTL_SRV_COPYCHUNK) - Chunk-based copy coordination

### Architecture Approach

All v3.6 changes extend existing modules using established DittoFS patterns. The three-layer architecture (SMB2 handlers → metadata layer → payload layer) remains unchanged. Work concentrates in five files: `security.go` (DACL synthesis, SID mapping), `read.go` (sparse zero-fill), `create.go` (context encoding), `file_modify.go` (path update), and `pkg/payload/io/read.go` (block not found handling).

**Major components:**
1. **handlers/security.go** - Extend: POSIX-to-DACL synthesis, well-known SID table, SACL stub, ACE flag translation fix
2. **handlers/read.go** - Fix: zero-fill for sparse file reads when block not found
3. **handlers/create.go** - Fix: create context wire encoding, MxAc/QFid responses
4. **pkg/metadata/file_modify.go** - Fix: update Path field in Move() before PutFile
5. **pkg/payload/io/read.go** - Fix: treat missing blocks as zeros when offset < file size

### Critical Pitfalls

1. **Security Descriptor field ordering mismatch** - Windows emits SACL-DACL-Owner-Group, not the MS-DTYP spec's implied Owner-Group-DACL order. smbtorture byte-level comparisons will fail. Mitigation: match Windows-observed field ordering empirically via Wireshark.

2. **SID collision between users and groups** - Current code uses S-1-5-21-0-0-0-{id} for both UID and GID, causing icacls to show owner==group when UID==GID. Mitigation: use different RID offsets (users +1000, groups +10000) or Samba's S-1-22-1-{uid} / S-1-22-2-{gid} convention.

3. **Non-canonical ACE ordering** - NFSv4 ACL order preserved, but Windows requires deny-before-allow. icacls reorders silently, causing confusion. Mitigation: sort into Windows canonical order (explicit deny, explicit allow, inherited deny, inherited allow) before encoding DACL.

4. **Windows 11 24H2 mandatory signing breaks guest** - Win11 24H2 requires SMB signing by default; guest sessions have no session key for signing. Mitigation: ensure NTLM authenticated sessions produce valid signing keys; document that guest access requires AllowInsecureGuestAuth GPO change.

5. **Missing SE_DACL_AUTO_INHERITED flag** - Without this SD control flag, icacls shows all ACEs as "not inherited" even with INHERITED_ACE flag. Explorer shows inheritance as broken. Mitigation: set SE_DACL_AUTO_INHERITED (0x0400) when any DACL ACE has INHERITED_ACE flag.

## Implications for Roadmap

Based on research, suggested phase structure:

### Phase 1: Bug Fixes (Parallel Execution)
**Rationale:** Two independent bugs blocking basic Windows operations. No dependencies between them.
**Delivers:** Sparse file READ with zero-fill + renamed directory path consistency
**Addresses:** Issues #180 and #181
**Avoids:** Pitfall 12 (sparse READ errors) and Pitfall 13 (stale paths)
**Research needed:** No - fixes are localized, patterns clear

**Sub-phases:**
- 29.1: Sparse file READ zero-fill (payload/io/read.go)
- 29.2: Renamed directory path update (metadata/file_modify.go + store implementations)

### Phase 2: NT Security Descriptors Enhancement
**Rationale:** Depends on nothing from Phase 1. Can run in parallel but merge after Phase 1 for cleaner testing.
**Delivers:** Proper POSIX-to-DACL synthesis, well-known SIDs, machine SID generation, ACE flag translation
**Uses:** Existing security.go (626 lines) + acl/validate.go canonical ordering
**Implements:** Default DACL from mode bits (Samba pattern), SID mapping improvements
**Avoids:** Pitfalls 1-5 (SD ordering, SID collision, ACE ordering, auto-inherited flag)
**Research needed:** No - Samba reference implementation provides clear patterns

**Sub-phases:**
- 29.3: Machine SID generation + SID mapper enhancements
- 29.4: POSIX-to-DACL synthesis with canonical ACE ordering
- 29.5: SD control flags (SE_DACL_AUTO_INHERITED, SE_DACL_PROTECTED)

### Phase 3: Windows 11 Compatibility
**Rationale:** Depends on Phase 2 (security descriptors must work before testing signing interaction)
**Delivers:** Functional SMB signing for Windows 11 24H2, MxAc create context response, CREATE context wire encoding fix
**Addresses:** Windows 11 24H2 connection failures, Explorer context menu limitations
**Avoids:** Pitfall 8 (signing breaks guest), Pitfall 9 (compound CREATE+QUERY_INFO)
**Research needed:** LIGHT - verify signing key derivation for NTLM auth

**Sub-phases:**
- 29.6: CREATE response context wire encoding fix
- 29.7: MxAc and QFid create context responses
- 29.8: SMB signing validation for NTLM authenticated sessions

### Phase 4: Conformance Testing and Validation
**Rationale:** Depends on all previous phases. Validates complete v3.6 milestone.
**Delivers:** smbtorture test results, WPTS BVT results, Windows 11 manual validation report, updated KNOWN_FAILURES.md
**Uses:** smbtorture, Microsoft WindowsProtocolTestSuites, Windows 11 VM
**Research needed:** No - testing infrastructure documented

**Sub-phases:**
- 29.9: smbtorture SMB2 test suite execution (getinfo, setinfo, acls, create, read)
- 29.10: WPTS BVT execution (101 SMB2 BVT tests)
- 29.11: Windows 11 manual validation (Explorer, icacls, cmd.exe)

### Phase Ordering Rationale

- **Phases 1 and 2 can run in parallel** - no dependencies between bug fixes and security descriptor enhancements
- **Phase 3 depends on Phase 2** - MxAc context responses and signing both interact with security descriptors; testing requires working SD implementation
- **Phase 4 depends on all** - conformance testing validates the complete milestone
- **No new infrastructure needed** - all work extends existing handlers, no new packages/interfaces
- **Incremental testing** - each phase has clear pass/fail criteria (unit tests for P1/P2, manual Windows testing for P3, conformance suites for P4)

### Research Flags

Phases with standard patterns (skip research-phase):
- **Phase 1 (Bug Fixes):** Well-understood payload and metadata layer fixes, patterns documented
- **Phase 2 (Security Descriptors):** Samba reference implementation provides complete patterns, MS-DTYP spec definitive
- **Phase 4 (Testing):** Tool usage documented, test execution straightforward

Phase needing light research during execution:
- **Phase 3 (Windows 11 Compatibility):** May need empirical testing to verify signing key derivation produces correct HMAC-SHA256 signatures. If signing tests fail, use Wireshark to compare key derivation against Windows Server 2019 reference.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | No new dependencies; evaluated and rejected external SD libraries with clear rationale |
| Features | HIGH | Issues #180, #181, #182 well-defined; Windows 11 compatibility requirements verified from Microsoft docs |
| Architecture | HIGH | Direct codebase analysis confirms all fixes are localized extensions of existing handlers |
| Pitfalls | HIGH (1-7), MEDIUM (8-14) | Critical pitfalls verified against MS-DTYP/MS-SMB2 specs and Samba source; Windows 11 quirks from community reports |

**Overall confidence:** HIGH

### Gaps to Address

- **Signing key derivation validation:** Current code has signing infrastructure but Windows 11 24H2 enforcement is new. During Phase 3, verify NTLM session setup produces correct signing keys via empirical testing (Wireshark comparison against Windows Server).

- **smbtorture test coverage:** Unknown which specific smb2.* test cases will pass vs. fail until executed. Phase 4 execution will reveal conformance gaps beyond the 56 known WPTS BVT failures.

- **ACL round-trip fidelity:** Integration Pitfall A (SD caching for round-trip fidelity) is documented but not scoped for v3.6. If Windows clients exhibit unexpected SET_INFO behavior during testing, may need to add raw SD caching in Phase 2.

- **CREATE context encoding specifics:** MS-SMB2 2.2.14.2 defines the wire format, but exact padding and alignment may need empirical validation via Wireshark during Phase 3 implementation.

## Sources

### Primary (HIGH confidence)
- MS-DTYP Section 2.4.2 (SID), 2.4.6 (SECURITY_DESCRIPTOR), 2.4.4.1 (ACL canonical ordering) - Official spec
- MS-SMB2 Section 2.2.13.2 (CREATE contexts), 2.2.14.2 (CREATE response contexts), 2.2.37-38 (QUERY_INFO) - Official spec
- MS-FSCC (Sparse files, zero-fill semantics) - Official spec
- Windows 11 24H2 SMB Signing Changes (Microsoft TechCommunity) - Official documentation
- Samba source: smbd/posix_acls.c (POSIX-to-DACL synthesis), source4/torture/smb2/acls.c (test patterns)
- DittoFS codebase: internal/adapter/smb/v2/handlers/ (security.go, read.go, create.go, query_info.go) - Direct analysis

### Secondary (MEDIUM confidence)
- Microsoft WindowsProtocolTestSuites (GitHub) - Official test suite, complex setup
- Samba idmap architecture documentation - Community docs
- smbtorture test framework documentation (Samba wiki)
- Windows 11 24H2 third-party NAS compatibility (Microsoft TechCommunity)
- hirochachacha/go-smb2 library - Community library for testing

### Tertiary (LOW confidence)
- Windows 11 25H2 SMBv1 removal - Forward-looking, limited public documentation

---
*Research completed: 2026-02-26*
*Ready for roadmap: yes*
