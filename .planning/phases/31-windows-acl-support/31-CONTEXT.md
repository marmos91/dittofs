# Phase 31: Windows ACL Support - Context

**Gathered:** 2026-02-27
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement Windows ACL support so Windows users see meaningful permissions in Explorer and icacls instead of "Everyone: Full Control." This includes NT Security Descriptors, Unix-to-SID mapping, POSIX-to-DACL synthesis, and cross-protocol ACL consistency between SMB and NFS. The abstract ACL model is the internal source of truth, with protocol-specific translators for SMB and NFS wire formats.

</domain>

<decisions>
## Implementation Decisions

### SID Mapping Scheme
- Unique machine SID generated per server instance, persisted in control plane database (SettingsStore)
- Samba-style RID mapping: users get RID = UID*2+1000, groups get RID = GID*2+1001 — safest for smbtorture ACL conformance tests in Phase 32
- SID mapper lives in shared package `pkg/auth/sid/` — reusable by both SMB and NFS
- UID 0 (root) maps to BUILTIN\Administrators (S-1-5-32-544)
- Anonymous/guest connections use Windows Anonymous SID (S-1-5-7)
- Basic LSA stub (LookupSids) so Explorer shows Unix usernames instead of raw SIDs
- Compute SID mappings on-the-fly (Samba-style is pure arithmetic, no cache needed)

### Abstract ACL Model
- Internal representation based on NFSv4 ACL semantics (access mask bits are nearly 1:1 with Windows ACEs)
- Abstract model captures the most complex scenario; SMB translates to ACE, NFS translates to POSIX
- ACLs are persisted per-file in the metadata store (not synthesized on-the-fly) for easier debugging and manual testing
- ACL source tracking: store flag indicating origin ('posix-derived', 'smb-explicit', 'nfs-explicit')
- Core model in `pkg/auth/acl/`
- Enforce 64KB DACL size limit (Windows default MAX_ACL_SIZE)

### DACL Synthesis
- Fine-grained POSIX-to-ACE mapping — each POSIX bit maps to full set of related Windows rights (READ_DATA, WRITE_DATA, APPEND_DATA, DELETE_CHILD for directories, etc.)
- Explicit DENY ACEs generated when POSIX mode restricts group/other below owner (e.g., mode 750 produces deny WRITE_DATA for group)
- Always canonical ACE ordering: explicit deny -> explicit allow -> inherited deny -> inherited allow
- CONTAINER_INHERIT_ACE + OBJECT_INHERIT_ACE flags set on ALL directory ACEs
- ACL inheritance from parent directories: new files/dirs inherit parent's inheritable ACEs; falls back to POSIX mode synthesis if parent has no inheritable ACEs
- Ignore setuid/setgid/sticky in ACL (no Windows equivalent; POSIX bits preserved internally but not exposed via SMB)
- Well-known SIDs in default DACLs: Claude's discretion on minimal vs extended set based on what smbtorture and Explorer expect

### Write-Back Behavior
- Accept all Windows ACL changes to abstract model (full ACL stored)
- Best-effort POSIX mode derivation using "most permissive" strategy (if ANY allow ACE grants a right, set that POSIX bit) — matches Samba behavior
- chmod (NFS) always overwrites ACL regardless of source — clear mental model: chmod = 'reset to POSIX'
- Allow ownership changes via SMB SET_INFO, restricted to callers with WRITE_OWNER rights
- Store unknown/domain SIDs but don't enforce — preserves round-trip fidelity, doesn't break domain-joined clients
- Support SE_DACL_PROTECTED flag (prevents inheritance from parent)
- Empty SACL stub returned for SACL_SECURITY_INFORMATION queries (valid but empty structure)
- ACL changes trigger immediate (synchronous) POSIX mode update in same metadata transaction
- Debug-level logging for ACL changes (old/new ACL summary)

### Cross-Protocol Consistency
- Both SMB and NFS read/write from the same abstract ACL in metadata
- SMB translator: `internal/adapter/smb/acl/` (ACL <-> NT Security Descriptor wire format)
- NFS translator: `internal/adapter/nfs/acl/` (ACL <-> NFSv4 ACE wire format)
- ACL enforcement (access checks) happens in the abstract layer (`pkg/auth/acl/`) — single enforcement point for both protocols
- NFSv4 clients can query FATTR4_ACL (full ACL in NFSv4 format) AND FATTR4_MODE (derived POSIX mode)
- Both SMB (SET_INFO) and NFSv4 (SETATTR ACL) can modify ACLs — full bidirectional support
- ACL changes visible immediately on any open handle (no re-open needed)
- Cross-protocol ACL changes do NOT trigger oplock/delegation breaks

### Code Structure & Testing
- Explicit `GetACL(handle)` and `SetACL(handle, acl)` methods added to MetadataStore interface
- Storage matches each store's natural model: PostgreSQL uses separate `file_acls` table, BadgerDB uses key-prefix scheme (`acl:{handle}`), Memory uses simple `map[FileHandle]*ACL`
- Shared ACL conformance test suite (`pkg/auth/acl/acltest/`) — all metadata store implementations must pass
- E2E tests using smbclient + smbcacls (runs in CI, no Windows VM)
- Basic `dfsctl acl show` command with table and JSON output (`-o json`) for debugging
- Build on existing SMB QUERY_INFO/SET_INFO handlers (`internal/adapter/smb/v2/handlers/security.go`), extend with new abstract ACL model — existing code already has SID types, SD building/parsing, and principal-to-SID mapping that should be refactored into the shared `pkg/auth/sid/` and `pkg/auth/acl/` packages
- New `docs/ACL.md` documenting abstract model, SID mapping, POSIX<->ACL translation, cross-protocol behavior
- Update CLAUDE.md with new package structure and interfaces

### Acceptance Testing & Conformance
- **WPTS BVT:** No ACL-specific tests exist in KNOWN_FAILURES.md — current BVT tests pass with the existing "Everyone: Full Access" fallback. Phase 31 must not regress any existing WPTS BVT passes.
- **smbcacls E2E tests (new):** Phase 31's primary acceptance tests. Validate: correct owner/group SIDs, DACL with deny+allow ACEs matching POSIX mode, canonical ACE ordering, inheritance flags on directories, SACL stub response, SID-to-UID round-trip fidelity.
- **smbtorture smb2.acls:** Full ACL conformance will be tested in Phase 32, but Phase 31 E2E tests should cover the core behaviors that smbtorture will exercise.
- **No KNOWN_FAILURES entries to remove:** The WPTS BVT suite doesn't have dedicated ACL test cases; ACL conformance is tested via smbtorture in Phase 32.

### Claude's Discretion
- Well-known SID set selection (minimal vs extended) based on smbtorture/Explorer expectations
- Cross-protocol oplock/delegation break policy for ACL changes
- LSA stub implementation scope (LookupSids minimum, LookupNames if needed)
- Exact fine-grained access right mapping table for each POSIX bit
- BadgerDB key-prefix format details

</decisions>

<specifics>
## Specific Ideas

- "The abstract permission should cover the most complex scenario, then SMB translate it to ACE and NFS to POSIX" — architecture: abstract ACL model is the canonical representation, protocol adapters translate to/from wire formats
- User wants ACLs persisted (not synthesized) for easier manual testing and debugging
- `dfsctl acl show` command specifically for debugging without needing a mounted client
- ACL source tracking for debuggability (knowing whether ACL was set via SMB, NFS, or derived from POSIX)
- Samba-style RID mapping chosen specifically for smbtorture conformance test compatibility in Phase 32
- Storage schema should match each metadata store's natural model (PG: table, Badger: key-prefix, Memory: map)

</specifics>

<deferred>
## Deferred Ideas

- Full LSA (LookupNames, TranslateName, etc.) — future phase if domain join support is added
- `dfsctl acl set` command for modifying ACLs via CLI — keep read-only for now
- SACL audit logging — empty stub for now, real audit ACEs in a future phase
- Domain SID resolution (mapping domain SIDs to actual usernames) — requires AD/LDAP integration

</deferred>

---

*Phase: 31-windows-acl-support*
*Context gathered: 2026-02-27*
