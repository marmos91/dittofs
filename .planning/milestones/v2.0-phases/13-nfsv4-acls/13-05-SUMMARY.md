---
phase: 13-nfsv4-acls
plan: 05
subsystem: protocol, api, cli
tags: [smb, security-descriptor, sid, dacl, identity-mapping, rest-api, dittofsctl, prometheus, acl-metrics]

# Dependency graph
requires:
  - phase: 13-nfsv4-acls
    plan: 01
    provides: "ACE/ACL types and constants (pkg/metadata/acl)"
  - phase: 13-nfsv4-acls
    plan: 03
    provides: "ACL metadata integration, IdentityMapping GORM model and store CRUD"
  - phase: 13-nfsv4-acls
    plan: 04
    provides: "NFSv4 ACL wire format, FATTR4_ACL encoding/decoding"
provides:
  - "SMB Security Descriptor encoding/decoding with SID mapping and DACL"
  - "QUERY_INFO returns real Security Descriptor with Owner/Group/DACL from file ACL"
  - "SET_INFO parses Security Descriptor and applies ACL changes via MetadataService"
  - "Well-known SID mapping (EVERYONE@=S-1-1-0, OWNER@=S-1-3-0, GROUP@=S-1-3-1)"
  - "Identity mapping REST API with admin-auth (GET/POST/DELETE /identity-mappings)"
  - "dittofsctl idmap add/list/remove CLI commands"
  - "ACL Prometheus metrics (evaluation duration, deny count, inheritance, validation errors)"
affects: [smb-acl-interop, identity-management, acl-monitoring]

# Tech tracking
tech-stack:
  added: []
  patterns: [ms-dtyp-security-descriptor, sid-encoding, dacl-ace-translation, nil-safe-metrics]

key-files:
  created:
    - internal/protocol/smb/v2/handlers/security.go
    - internal/protocol/smb/v2/handlers/security_test.go
    - internal/controlplane/api/handlers/identity_mappings.go
    - pkg/apiclient/identity_mappings.go
    - cmd/dittofsctl/commands/idmap/idmap.go
    - cmd/dittofsctl/commands/idmap/add.go
    - cmd/dittofsctl/commands/idmap/list.go
    - cmd/dittofsctl/commands/idmap/remove.go
    - pkg/metadata/acl/metrics.go
  modified:
    - internal/protocol/smb/v2/handlers/query_info.go
    - internal/protocol/smb/v2/handlers/set_info.go
    - pkg/controlplane/api/router.go
    - cmd/dittofsctl/commands/root.go

key-decisions:
  - "DittoFS user SID format: S-1-5-21-0-0-0-{UID/GID} for local identity mapping"
  - "Well-known SID bidirectional mapping: EVERYONE@ <-> S-1-1-0, OWNER@ <-> S-1-3-0 (CREATOR_OWNER), GROUP@ <-> S-1-3-1 (CREATOR_GROUP)"
  - "NFSv4 ACE mask bits identical to Windows ACCESS_MASK (no translation needed per RFC 7530)"
  - "Security Descriptor uses self-relative format with 4-byte alignment per MS-DTYP"
  - "Identity mapping API routes under /identity-mappings with RequireAdmin middleware"
  - "ACL metrics use nil-safe methods matching GSSMetrics pattern from Phase 12"
  - "sync.Once singleton for ACLMetrics registration (same as GSSMetrics)"

patterns-established:
  - "MS-DTYP Security Descriptor: 20-byte header + Owner SID + Group SID + DACL with 4-byte alignment"
  - "SID encode/decode: Revision + SubAuthorityCount + Authority[6] + SubAuthorities[] little-endian"
  - "PrincipalToSID/SIDToPrincipal for bidirectional NFSv4-Windows identity translation"
  - "Identity mapping CLI pattern: dittofsctl idmap add/list/remove following existing user/group pattern"

# Metrics
duration: ~12min
completed: 2026-02-16
---

# Phase 13 Plan 05: SMB Security Descriptor and Control Plane Integration Summary

**SMB Security Descriptor encoding with SID mapping for Windows ACL interop, identity mapping REST API + dittofsctl CLI, and ACL Prometheus metrics**

## Performance

- **Duration:** ~12 min
- **Started:** 2026-02-16T08:44:07Z
- **Completed:** 2026-02-16T08:52:22Z
- **Tasks:** 2
- **Files modified:** 13

## Accomplishments
- SMB QUERY_INFO returns real Security Descriptors with Owner SID, Group SID, and DACL translated from NFSv4 ACLs
- SMB SET_INFO parses client-provided Security Descriptors and applies ACL changes via MetadataService
- Bidirectional SID mapping with well-known SID support (EVERYONE@, OWNER@, GROUP@)
- Identity mapping REST API with admin-auth CRUD endpoints
- dittofsctl idmap add/list/remove commands for managing principal-to-user mappings
- ACL Prometheus metrics with nil-safe methods for zero-overhead when disabled

## Task Commits

Each task was committed atomically:

1. **Task 1: SMB Security Descriptor Encoding and QUERY_INFO/SET_INFO Integration** - `33297d5` (feat)
2. **Task 2: Identity Mapping REST API, dittofsctl Commands, and ACL Metrics** - `2cc2997` (feat)

## Files Created/Modified
- `internal/protocol/smb/v2/handlers/security.go` - SID types, encode/decode, Security Descriptor build/parse, well-known SID mapping (635 lines)
- `internal/protocol/smb/v2/handlers/security_test.go` - 10 test cases covering SID round-trip, SD build/parse, alignment, error cases (435 lines)
- `internal/protocol/smb/v2/handlers/query_info.go` - Updated buildSecurityInfo to call BuildSecurityDescriptor with file and secInfo
- `internal/protocol/smb/v2/handlers/set_info.go` - Added setSecurityInfo method parsing SD and applying ACL changes
- `internal/controlplane/api/handlers/identity_mappings.go` - REST handlers for List, Create, Delete identity mappings
- `pkg/controlplane/api/router.go` - Added /identity-mappings route group with RequireAdmin middleware
- `pkg/apiclient/identity_mappings.go` - API client methods for identity mapping CRUD
- `cmd/dittofsctl/commands/idmap/idmap.go` - Parent "idmap" command group
- `cmd/dittofsctl/commands/idmap/add.go` - Add identity mapping command
- `cmd/dittofsctl/commands/idmap/list.go` - List identity mappings with table rendering
- `cmd/dittofsctl/commands/idmap/remove.go` - Remove identity mapping with confirmation
- `cmd/dittofsctl/commands/root.go` - Registered idmap command in root
- `pkg/metadata/acl/metrics.go` - ACL Prometheus metrics (5 metrics, nil-safe, sync.Once singleton)

## Decisions Made
- DittoFS user SID format S-1-5-21-0-0-0-{UID/GID} chosen for simplicity (local identity domain)
- NFSv4 ACE mask bits map directly to Windows ACCESS_MASK (no translation needed -- same bit positions by design per RFC 7530)
- Well-known SID mapping is bidirectional: build and parse both use the same tables
- Security Descriptor always uses self-relative format (SE_SELF_RELATIVE flag) per MS-DTYP
- additionalSecInfo bitmask controls which SD sections are included (OWNER/GROUP/DACL)
- Identity mapping API routes placed at /identity-mappings (not nested under /idmap)
- ACL metrics follow the nil-safe singleton pattern established by GSSMetrics in Phase 12

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 13 (NFSv4 ACLs) is now COMPLETE with all 5 plans delivered
- Full ACL pipeline functional: types -> evaluation -> metadata integration -> NFS wire format -> SMB interop
- Identity mapping manageable via REST API and dittofsctl CLI
- ACL metrics ready for Prometheus instrumentation wiring
- Ready to proceed to Phase 14

---
*Phase: 13-nfsv4-acls*
*Completed: 2026-02-16*

## Self-Check: PASSED

All 13 files verified present. Both task commits (33297d5, 2cc2997) verified in git history.
