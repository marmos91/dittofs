---
phase: 13-nfsv4-acls
plan: 02
subsystem: auth
tags: [identity, nfsv4, kerberos, acl, mapper, cache]

# Dependency graph
requires:
  - phase: 12-kerberos-authentication
    provides: "StaticMapper in pkg/auth/kerberos, IdentityMapper interface"
provides:
  - "pkg/identity/ package with IdentityMapper interface"
  - "ConventionMapper for user@REALM resolution"
  - "TableMapper for explicit mapping table resolution"
  - "CachedMapper for TTL-based identity caching"
  - "StaticMapper migrated from pkg/auth/kerberos"
  - "GroupResolver interface for group membership queries"
  - "MappingStore interface for explicit mapping CRUD"
affects: [13-nfsv4-acls, nfsv4-handlers, smb-security-descriptor]

# Tech tracking
tech-stack:
  added: []
  patterns: [pluggable-mapper, cached-wrapper, double-check-locking, backward-compat-delegation]

key-files:
  created:
    - pkg/identity/mapper.go
    - pkg/identity/convention.go
    - pkg/identity/table.go
    - pkg/identity/cache.go
    - pkg/identity/static.go
    - pkg/identity/CLAUDE.md
  modified:
    - pkg/auth/kerberos/identity.go

key-decisions:
  - "StaticMapper always returns Found=true (falls back to defaults)"
  - "CachedMapper caches errors to prevent thundering herd"
  - "pkg/identity has zero external imports (stdlib only)"
  - "ParsePrincipal splits on last @ for user@host@REALM safety"
  - "ConventionMapper numeric UID uses same value for default GID"
  - "Backward compat via wrapper delegation, not type alias"

patterns-established:
  - "UserLookupFunc callback to avoid circular imports with controlplane"
  - "Found=false vs error: not-found is normal, error is infrastructure failure"
  - "CachedMapper wrapping pattern: NewCachedMapper(inner, ttl)"

# Metrics
duration: 7min
completed: 2026-02-16
---

# Phase 13 Plan 02: Identity Mapper Package Summary

**Pluggable identity mapper package (pkg/identity/) with ConventionMapper, TableMapper, CachedMapper, and migrated StaticMapper for NFSv4 principal resolution**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-16T08:15:55Z
- **Completed:** 2026-02-16T08:22:54Z
- **Tasks:** 2
- **Files modified:** 12 (7 created + 5 created in task 2 + 1 modified)

## Accomplishments
- IdentityMapper interface defined with Resolve(ctx, principal) method and ResolvedIdentity result type
- ConventionMapper resolves user@REALM with case-insensitive domain matching and numeric UID support for AUTH_SYS interop
- TableMapper resolves explicit mappings from MappingStore interface with userLookup callback
- CachedMapper provides TTL-based caching with double-check locking, error caching, invalidation, and stats
- StaticMapper migrated from pkg/auth/kerberos with full backward compatibility via delegation wrapper
- GroupResolver interface defined for group membership queries in ACL evaluation
- Package has zero external imports (stdlib only) -- no circular dependency risk

## Task Commits

Each task was committed atomically:

1. **Task 1: Identity Mapper Interface, ConventionMapper, and StaticMapper Migration** - `91dbad9` (feat)
2. **Task 2: TableMapper, CachedMapper, and Package Documentation** - `9dae339` (feat)

## Files Created/Modified
- `pkg/identity/mapper.go` - IdentityMapper interface, ResolvedIdentity, GroupResolver, ParsePrincipal, NobodyIdentity
- `pkg/identity/mapper_test.go` - ParsePrincipal and NobodyIdentity tests
- `pkg/identity/convention.go` - ConventionMapper with case-insensitive realm matching and numeric UID support
- `pkg/identity/convention_test.go` - 8 tests covering all ConventionMapper paths
- `pkg/identity/static.go` - StaticMapper migrated from kerberos package with MapPrincipal backward compat
- `pkg/identity/static_test.go` - 6 tests including GIDs deep copy and MapPrincipal compat
- `pkg/identity/table.go` - TableMapper with MappingStore and userLookup callback
- `pkg/identity/table_test.go` - 5 tests with mock MappingStore
- `pkg/identity/cache.go` - CachedMapper with TTL, double-check locking, error caching, invalidation, stats
- `pkg/identity/cache_test.go` - 9 tests including concurrent access and thundering herd prevention
- `pkg/identity/CLAUDE.md` - Package documentation with patterns and gotchas
- `pkg/auth/kerberos/identity.go` - Updated to delegate to pkg/identity.StaticMapper

## Decisions Made
- StaticMapper always returns Found=true (falls back to defaults for unknown principals), matching Phase 12 behavior
- CachedMapper caches errors too, preventing thundering herd when database is down
- pkg/identity uses stdlib only (context, sync, strings, strconv, time, fmt) -- zero external imports
- ParsePrincipal splits on last @ to handle "user@host@REALM" edge case safely
- ConventionMapper uses numeric UID value as default GID for AUTH_SYS interop (same-value convention)
- Backward compatibility achieved via wrapper delegation (kerberos.StaticMapper embeds identity.StaticMapper) rather than type alias

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- pkg/identity/ ready for use by ACL evaluation engine (Plan 03)
- MappingStore interface ready for control plane implementation (Plan 05)
- GroupResolver interface ready for ACL group@domain evaluation (Plan 03)
- All existing Kerberos code continues to work unchanged

## Self-Check: PASSED

All 12 created/modified files verified on disk. Both task commits (91dbad9, 9dae339) verified in git log. 34 tests passing with -race. No forbidden imports.

---
*Phase: 13-nfsv4-acls*
*Completed: 2026-02-16*
