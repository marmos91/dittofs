# Phase 13: NFSv4 ACLs - Context

**Gathered:** 2026-02-16
**Status:** Ready for planning

<domain>
## Phase Boundary

Extend DittoFS with full NFSv4 ACL support (RFC 7530 Section 6) including identity mapping and SMB interoperability. ACLs stored as first-class metadata, evaluated for access decisions, inherited by new files/directories, and translated bidirectionally between NFSv4 user@domain principals and Windows SIDs. Includes identity mapping REST API (dittofsctl). Refactors existing identity code (Phase 12 StaticMapper) into new `pkg/identity/` package.

</domain>

<decisions>
## Implementation Decisions

### ACL Model Scope
- Full RFC 7530 ACL model: all four ACE types (ALLOW, DENY, AUDIT, ALARM)
- All 14 file + 2 directory permission mask bits per RFC 7530 Section 6.2.1
- Strict Windows canonical ordering enforced: explicit DENY -> explicit ALLOW -> inherited DENY -> inherited ALLOW. Mis-ordered ACLs rejected on SETATTR
- ACL overrides Unix permissions when present. Mode bits derived from ACL for display. No ACL = classic Unix permission check
- All three special identifiers supported: OWNER@, GROUP@, EVERYONE@
- ACL stored as first-class metadata field on File struct (not xattr). No Phase 25 dependency
- AUDIT and ALARM ACEs stored and returned only — no logging/alerting triggered
- chmod adjusts OWNER@, GROUP@, EVERYONE@ ACEs to match new mode bits per RFC 7530 Section 6.4.1. ACL stays authoritative
- Maximum 128 ACEs per file
- Full FATTR4_ACLSUPPORT bitmap in GETATTR (all 4 ACE types reported)

### Identity Mapping
- Layered strategy: convention-based default + explicit mapping table in control plane
- Convention-based: user@REALM maps to control plane user when domain matches configured Kerberos realm
- Case-insensitive domain matching (alice@EXAMPLE.COM = alice@example.com)
- Unknown principals stored as-is in ACEs, skipped during evaluation (preserves cross-domain ACLs)
- Numeric UID/GID accepted with @domain suffix (e.g., 1000@localdomain) for AUTH_SYS interop
- Pluggable IdentityMapper interface with Resolve(principal) method. Ships with ConventionMapper + TableMapper
- Group membership resolved via control plane groups (group@domain -> control plane group -> member lookup)
- Mapping table configurable via REST API (dittofsctl idmap add/list/remove). Stored in control plane DB
- Identity resolution results cached with TTL (e.g., 5 minutes) to reduce DB lookups
- Refactor Phase 12 StaticMapper into new `pkg/identity/` package

### ACL Inheritance
- All four RFC 7530 inheritance flags: FILE_INHERIT, DIRECTORY_INHERIT, NO_PROPAGATE_INHERIT, INHERIT_ONLY
- No ACL by default — derive from umask/mode until explicit ACL is set. Zero migration needed for existing files
- Inherited ACEs marked with ACE4_INHERITED_ACE flag (distinguishes explicit vs inherited)
- Snapshot inheritance at creation time — changing parent ACL does not retroactively affect children
- Recursive propagation supported: replaces inherited ACEs only on descendants, preserves explicit ACEs
- Recursive propagation sync/async mode: Claude's discretion based on implementation complexity

### SMB Interoperability
- Full bidirectional DACL translation between NFSv4 ACEs and Windows DACL ACEs
- DACL only — no SACL support
- Well-known SIDs mapped statically (Everyone=S-1-1-0, Administrators=S-1-5-32-544, etc.) + user SIDs resolved via identity mapper
- Direct bidirectional mask bit mapping table between SMB FILE_* permissions and NFSv4 mask bits
- Windows Owner/Group in security descriptor bidirectionally synced with NFSv4 file owner/group_owner
- Same validation rules for both protocols: canonical ordering, 128 ACE limit
- Single stored ACL (NFSv4 format internally), protocol-native view at boundary (NFS sees user@domain, SMB sees SIDs)
- SMB QUERY_INFO supports OWNER, GROUP, and DACL security information classes
- Security Descriptor binary encoding built from scratch in internal/protocol/smb/ (no external library)

### Testing & Code Structure
- Comprehensive unit tests for ACL evaluation engine + integration tests through NFS handlers
- Handler-level cross-protocol integration tests (set via NFSv4 SETATTR, query via SMB QUERY_INFO)
- Windows-compatibility reference test cases as unit tests (DENY before ALLOW, inheritance patterns, SID mapping)
- Manual Windows testing deferred to Phase 15 / checkpoint phase
- ACL package at `pkg/metadata/acl/` — types, evaluation, inheritance, validation
- Identity mapper package at `pkg/identity/` — IdentityMapper interface, ConventionMapper, TableMapper
- ACL XDR encoding in `internal/protocol/nfs/` (protocol layer handles wire format, pkg stays format-agnostic)
- ACL persistence extends existing metadata store interfaces (Memory, BadgerDB, PostgreSQL)
- PostgreSQL ACL storage as JSONB column on files table
- ACL-specific Prometheus metrics: evaluation duration histogram, cache hit/miss, DENY counters, inheritance computation
- Inline code docs (GoDoc, CLAUDE.md for new packages). User-facing docs deferred to Phase 27

### Claude's Discretion
- Recursive propagation sync/async mode and threshold
- Exact identity resolution cache TTL value
- Exact mask bit mapping table between SMB and NFSv4 permissions
- ACL evaluation algorithm implementation details
- PostgreSQL migration numbering and structure
- Exact well-known SID mapping table

</decisions>

<specifics>
## Specific Ideas

- Refactor Phase 12 StaticMapper (currently in Kerberos auth package) into `pkg/identity/` as the foundation for the pluggable identity mapper
- Identity mapper should be the same interface used by both Kerberos principal mapping and ACL principal resolution
- "Enterprise-grade" identity resolution: convention-based works immediately, mapping table for overrides, LDAP pluggable later
- Windows Explorer "Security" tab should work when accessing DittoFS via SMB

</specifics>

<deferred>
## Deferred Ideas

- **LDAP/AD integration for identity mapping** — Full LDAP directory lookup (connection pooling, schema, group sync, caching). Deserves its own phase. `pkg/identity/` interface designed to accommodate this.
- **AUDIT ACE logging** — When AUDIT ACEs should trigger actual log entries. Currently store-only.
- **Manual Windows testing** — Real Windows client ACL testing deferred to Phase 15 / checkpoint phase.
- **Full E2E mount-level ACL tests** — NFS mount + nfs4_setfacl testing deferred to Phase 15.

</deferred>

---

*Phase: 13-nfsv4-acls*
*Context gathered: 2026-02-16*
