# Phase 8: NFSv4 Advanced Operations - Context

**Gathered:** 2026-02-13
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement remaining NFSv4 operations that complete the operation set: LINK, RENAME, SETATTR, VERIFY/NVERIFY, SECINFO, OPENATTR (stub), OPEN_DOWNGRADE (stub), and RELEASE_LOCKOWNER (stub). These operations build on the foundation from Phases 6-7 (COMPOUND dispatcher, pseudo-fs, real-FS handlers, file I/O).

SAVEFH/RESTOREFH and OPEN_CONFIRM are already implemented from earlier phases and do NOT need reimplementation — only verification that they work correctly with real-FS handles.

</domain>

<decisions>
## Implementation Decisions

### LINK behavior
- Cross-directory hard links supported within the same share (standard NFSv4 behavior)
- No hard links to directories (return NFS4ERR_ISDIR)
- Uses two-filehandle pattern: SavedFH = source file, CurrentFH = target directory
- Link count (nlink) and ctime updated immediately and atomically with link creation
- change_info4 for target directory uses ctime before/after (consistent with Phase 7 CREATE/REMOVE)

### RENAME behavior
- Full cross-directory rename within the same share
- Cross-share (different FSID) rename returns NFS4ERR_XDEV
- Uses two-filehandle pattern: SavedFH = source directory, CurrentFH = target directory
- Atomic replace when target exists (POSIX and RFC 7530 required)
- RENAME of directories is allowed (mv semantics)
- Target directory must be empty for directory-to-directory overwrite (NFS4ERR_NOTEMPTY)
- change_info4 for both source and target directories uses ctime before/after

### SETATTR scope
- Core POSIX attribute set: mode, uid/gid (ownership), size (truncate/extend), atime/mtime
- ACL attributes deferred to Phase 13 (requires ACL storage infrastructure)
- sattrguard4 (guard) enforced: if client sends guard with ctime, verify ctime matches before applying
- Size changes delegate to existing MetadataService truncate/extend logic from NFSv3
- Ownership changes follow POSIX: owner can chgrp to own groups, only root can chown
- ctime auto-updates on ANY attribute modification (strict POSIX)
- Both SET_TO_SERVER_TIME and SET_TO_CLIENT_TIME supported for atime/mtime (time_how4 enum)
- Works on all file types: regular files, directories, symlinks
- Full mode support including setuid (04000), setgid (02000), sticky (01000) bits
- Clear setuid/setgid bits on ownership change (POSIX security requirement)
- Mode validation: reject values > 07777
- Symlink SETATTR: allowed for timestamps and ownership; mode changes stored in metadata (follows metadata layer support)
- Returns attrsset bitmap in response (RFC 7530 SETATTR4resok)
- Truncate accepts anonymous/special stateids (consistent with Phase 7, Phase 9 tightens)
- OPENATTR returns NFS4ERR_NOTSUPP (named attributes are Phase 25)
- OPEN_DOWNGRADE returns NFS4ERR_NOTSUPP (state management is Phase 9)

### VERIFY/NVERIFY semantics
- Support checking against ALL readable attributes (everything GETATTR can encode)
- VERIFY and NVERIFY have symmetric attribute support
- VERIFY failure returns NFS4ERR_NOT_SAME with no additional info (RFC 7530 spec)
- NVERIFY failure returns NFS4ERR_SAME
- Work on all filehandle types: files, directories, symlinks, pseudo-fs entries
- Change attribute uses ctime (consistent with GETATTR)
- Stale filehandle returns NFS4ERR_STALE

### SECINFO & security
- Advertise AUTH_SYS + AUTH_NONE security flavors
- Per-export security configuration: read from share config in control plane store
- Flavors returned in preference order (strongest first)
- Works on pseudo-fs and real-fs paths
- OPEN_CONFIRM: keep existing Phase 7 placeholder (clients need it for OPEN sequences)
- OPEN_DOWNGRADE: return NFS4ERR_NOTSUPP (Phase 9)
- RELEASE_LOCKOWNER: add as no-op stub returning success (prevents NOTSUPP errors from clients)

### Claude's Discretion
- SETATTR partial failure handling (all-or-nothing vs best-effort based on metadata layer transaction support)
- VERIFY/NVERIFY comparison method (byte-exact XDR vs per-attribute decode)
- SECINFO on pseudo-fs behavior details
- SECINFO flavor ordering specifics
- Plan count/structure (research determines optimal grouping)
- Compression algorithm for plan consolidation

</decisions>

<specifics>
## Specific Ideas

- Reuse as much existing code as possible — search for methods doing similar things and refactor rather than duplicate
- Run code-simplifier agent to find simplifications in newly written code
- change_info4 encoding already exists in Phase 7 CREATE/REMOVE — extend, don't rewrite
- MetadataService may need new methods (Link, SetAttr) — add to MetadataService and store implementations as needed
- Verify SAVEFH/RESTOREFH work correctly with real-FS handles in compound tests (already implemented in Phase 6)
- One handler file per operation (link.go, rename.go, setattr.go, verify.go, nverify.go, secinfo.go)
- Tests use real in-memory store implementations, no mocks/stubs
- All tests run with -race flag
- Include compound operation sequence tests (PUTFH + SAVEFH + RENAME + VERIFY patterns)
- Test both happy paths and error conditions (permission denied, stale FH, not found, etc.)

</specifics>

<deferred>
## Deferred Ideas

- ACL attribute support in SETATTR — Phase 13 (NFSv4 ACLs)
- Named attribute directory (OPENATTR full implementation) — Phase 25 (Extended Attributes)
- OPEN_DOWNGRADE full implementation — Phase 9 (State Management)
- Per-share Kerberos security (krb5/krb5i/krb5p flavors in SECINFO) — Phase 12 (Kerberos Authentication)
- Monotonic change counter (replacing ctime-based change attribute) — consider for Phase 9 or later

</deferred>

---

*Phase: 08-nfsv4-advanced-operations*
*Context gathered: 2026-02-13*
