# pkg/identity

Pluggable identity resolution for NFSv4 principals and Kerberos authentication.

## Architecture

```
IdentityMapper (mapper.go) - core interface
         |
         +-- ConventionMapper (convention.go) - user@REALM -> control plane user
         +-- TableMapper (table.go) - explicit mapping table from MappingStore
         +-- StaticMapper (static.go) - static config map (migrated from pkg/auth/kerberos)
         +-- CachedMapper (cache.go) - TTL-based caching wrapper for any IdentityMapper
```

## IdentityMapper Interface

- `Resolve(ctx, principal)` -> `(*ResolvedIdentity, error)`
- `ResolvedIdentity.Found=false` means "unknown principal" (NOT an error)
- Errors mean infrastructure failure (DB down, network timeout)
- All mappers are goroutine-safe

## MapperChain Pattern

Expected usage in production:
1. **TableMapper** checked first (explicit overrides from control plane DB)
2. **ConventionMapper** as fallback (convention-based user@REALM resolution)
3. Wrap the final result with **CachedMapper** for TTL-based caching

```go
table := identity.NewTableMapper(store, userLookup)
convention := identity.NewConventionMapper(realm, userLookup)
chain := identity.NewCachedMapper(chainMapper, 5*time.Minute)
```

## GroupResolver

Separate interface for group membership queries during ACL evaluation:
- `GetGroupMembers(ctx, groupName)` -> member list
- `IsGroupMember(ctx, username, groupName)` -> bool
- Used for evaluating group@domain ACE principals

## MappingStore

Interface for explicit mapping CRUD (GetMapping, ListMappings, CreateMapping, DeleteMapping).
Implementations provided by the control plane (Plan 05).

## StaticMapper

Canonical static identity mapper (originally from `pkg/auth/kerberos`, fully migrated):
- Always returns `Found=true` (falls back to defaults for unknown principals)
- Used directly by GSS framework via `identity.IdentityMapper` interface
- The old `kerberos.IdentityMapper` wrapper has been removed

## Common Gotchas

1. **Found=false is NOT an error** - Unknown principals are stored as-is in ACEs, skipped during evaluation
2. **CachedMapper caches errors too** - Prevents thundering herd when DB is down (TTL expiry clears)
3. **Domain matching is case-insensitive** - `alice@EXAMPLE.COM` == `alice@example.com` in ConventionMapper
4. **ParsePrincipal splits on last @** - Handles "user@host@REALM" correctly
5. **Special identifiers** - OWNER@, GROUP@, EVERYONE@ are returned as-is by ParsePrincipal (empty domain)
6. **Numeric UID support** - ConventionMapper accepts "1000@EXAMPLE.COM" for AUTH_SYS interop
