---
status: resolved
trigger: "Migrate GSS framework from kerberos.IdentityMapper to identity.IdentityMapper"
created: 2026-02-18T12:00:00Z
updated: 2026-02-18T12:15:00Z
---

## Current Focus

hypothesis: Migration complete - all code uses identity.IdentityMapper directly
test: Full test suite run
expecting: All tests pass
next_action: Archive and commit

## Symptoms

expected: GSS framework should use identity.IdentityMapper directly
actual: GSS framework uses kerberos.IdentityMapper with backward-compat wrapper
errors: None - code cleanliness issue
reproduction: Look at framework.go mapper field type
started: Phase 13 introduced identity.IdentityMapper but kept backward compat

## Eliminated

(none - this is a straightforward migration, not a bug investigation)

## Evidence

- timestamp: 2026-02-18T12:00:00Z
  checked: All affected files
  found: |
    1. framework.go: mapper field is kerberos.IdentityMapper, calls MapPrincipal(principal, realm) -> *metadata.Identity
    2. framework_test.go: uses kerberos.NewStaticMapper to create test mapper
    3. nfs_adapter.go: calls kerberos.NewStaticMapper(&s.kerberosConfig.IdentityMapping)
    4. kerberos/identity.go: IdentityMapper interface + StaticMapper wrapper -> delegates to identity.StaticMapper
    5. identity/static.go: StaticMapper.MapPrincipal() backward-compat method returns (uid, gid, gids, err)
    6. identity/mapper.go: IdentityMapper interface with Resolve(ctx, principal) -> *ResolvedIdentity
    7. test/integration/kerberos/: Two call sites using kerberos.NewStaticMapper
  implication: |
    The GSSProcessor.mapper calls MapPrincipal(principal, realm) -> *metadata.Identity.
    We need to change it to use identity.IdentityMapper.Resolve(ctx, "principal@realm") -> *ResolvedIdentity.
    Then convert ResolvedIdentity to *metadata.Identity at the call sites.

- timestamp: 2026-02-18T12:15:00Z
  checked: Full test suite after migration
  found: All tests pass (89 GSS tests, 35 identity tests, 12 kerberos tests, full suite green)
  implication: Migration is complete and verified

## Resolution

root_cause: Backward-compat wrapper from Phase 13 was never cleaned up
fix: |
  1. Changed GSSProcessor.mapper field from kerberos.IdentityMapper to identity.IdentityMapper
  2. Updated NewGSSProcessor and SetMapper signatures to accept identity.IdentityMapper
  3. Changed handleInit and handleData to use Resolve(ctx, "principal@realm") instead of MapPrincipal(principal, realm)
  4. handleData now converts ResolvedIdentity to *metadata.Identity inline
  5. Updated framework_test.go to use identity.NewStaticMapper directly
  6. Updated nfs_adapter.go to create identity.StaticMapper directly (with config conversion)
  7. Updated integration test to use identity.NewStaticMapper directly
  8. Removed pkg/auth/kerberos/identity.go (wrapper file)
  9. Removed StaticMapper.MapPrincipal() backward-compat method from identity/static.go
  10. Removed MapPrincipal backward-compat tests from identity/static_test.go
  11. Updated pkg/identity/CLAUDE.md documentation
verification: Full test suite passes - go test ./... with zero failures
files_changed:
  - internal/protocol/nfs/rpc/gss/framework.go
  - internal/protocol/nfs/rpc/gss/framework_test.go
  - pkg/adapter/nfs/nfs_adapter.go
  - pkg/auth/kerberos/identity.go (REMOVED)
  - pkg/identity/static.go
  - pkg/identity/static_test.go
  - pkg/identity/CLAUDE.md
  - test/integration/kerberos/kerberos_integration_test.go
