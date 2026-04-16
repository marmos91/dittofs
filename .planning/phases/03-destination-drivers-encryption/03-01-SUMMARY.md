---
phase: 03-destination-drivers-encryption
plan: 01
subsystem: backup
tags: [backup, destination, driver, registry, errors, factory]

# Dependency graph
requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: manifest v1 struct (pkg/backup/manifest), BackupRepo+BackupRepoKind models
provides:
  - Destination interface (7 methods) as driver contract for Phase 4/5 orchestrators
  - 11 D-07 error sentinels (errors.New, split transient/permanent)
  - Factory type keyed on models.BackupRepoKind
  - Registry surface: Register, Lookup, ResetRegistryForTest
  - Compile-time contract the rest of Phase 3 (plans 02-06) targets
affects: [03-02, 03-03, 03-04, 03-05, 03-06, 04-scheduler, 05-restore, 06-cli-api]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Factory + typed-key Registry (map[models.BackupRepoKind]Factory) — first registry in the repo, replaces the switch-dispatch idiom used in pkg/controlplane/runtime/shares/service.go"
    - "errors.New sentinels with doc-per-error + wrap-at-callsite via fmt.Errorf(\"%w: ...: %v\", sentinel, cause)"
    - "Two var-block error taxonomy splitting transient/retryable from permanent/do-not-retry"
    - "Register-panics-on-programmer-error (duplicate, empty kind, nil factory) — startup-only mutation, no mutex"

key-files:
  created:
    - pkg/backup/destination/destination.go
    - pkg/backup/destination/destination_test.go
    - pkg/backup/destination/errors.go
    - pkg/backup/destination/errors_test.go
  modified: []

key-decisions:
  - "Registry keyed on models.BackupRepoKind (typed enum) — not bare string — so callers pass repo.Kind verbatim without conversion (Go does not auto-convert typed strings on map lookup)"
  - "Register panics on duplicate/empty/nil — these are programmer errors (startup-only mutation), not operator errors"
  - "11 sentinels defined via errors.New (no fmt.Errorf for identity); wrapping happens at call sites via %w"
  - "Per-sentinel doc comments describe trigger conditions so downstream drivers have a style precedent"
  - "Interface docs enumerate which sentinels each method may return (contract for orchestrator errors.Is branching)"

patterns-established:
  - "Destination interface doc enumerates return-error sentinels — downstream drivers (plans 03/04) document actual returns at implementation, but the interface contract is now binding"
  - "ResetRegistryForTest naming convention for test-only registry mutation (first in this codebase, drivers in plans 03/04 will reuse)"
  - "Compile-time Factory signature check via package-level `var _ Factory = factoryStub` in tests"

requirements-completed: [DRV-01, DRV-02]

# Metrics
duration: 3min
completed: 2026-04-16
---

# Phase 03 Plan 01: Destination Package Skeleton Summary

**Stable `Destination` interface, 11 D-07 error sentinels, and a `models.BackupRepoKind`-keyed Factory registry — the compile-time contract Phase 3 plans 02-06 target.**

## Performance

- **Duration:** 3 min
- **Started:** 2026-04-16T12:04:43Z
- **Completed:** 2026-04-16T12:07:59Z
- **Tasks:** 2
- **Files created:** 4 (2 production + 2 tests)

## Accomplishments

- `pkg/backup/destination/errors.go` — 11 sentinels defined via `errors.New`, split
  into transient/retryable (2) and permanent/do-not-retry (9) `var` blocks. Each
  sentinel carries a doc comment describing its trigger condition. Package doc
  comment references Phase 3 CONTEXT.md D-01..D-14.
- `pkg/backup/destination/destination.go` — 7-method `Destination` interface
  (PutBackup, GetBackup, List, Stat, Delete, ValidateConfig, Close), `BackupDescriptor`
  struct, `Factory` type accepting `*models.BackupRepo`, and the registry surface
  (`Register`, `Lookup`, `ResetRegistryForTest`) keyed on `models.BackupRepoKind`.
- Method-level doc comments enumerate which D-07 sentinels each method may return —
  binding contract for orchestrator `errors.Is` branching (Phase 4/5).
- Full unit coverage: 10 tests total (3 sentinel-behavior + 7 registry-behavior),
  all green under `go test -count=1`.

## Task Commits

Each task was committed atomically with a TDD RED/GREEN split:

1. **Task 1 RED — D-07 sentinel tests** — `3c2fa15d` (test)
2. **Task 1 GREEN — D-07 sentinel implementation** — `1ee981a7` (feat)
3. **Task 2 RED — Destination interface + registry tests** — `61430a1e` (test)
4. **Task 2 GREEN — Destination interface + registry implementation** — `a53d1d39` (feat)

_Task 2 GREEN also contains the `TestFactorySignature_Compiles` auto-fix (replaced
an always-false `factoryStub == nil` check that failed `go vet`) — see Deviations._

## Files Created/Modified

- `pkg/backup/destination/errors.go` — 11 D-07 sentinels (errors.New only), package doc
- `pkg/backup/destination/errors_test.go` — distinct-identity + wrap-preserves-identity + stable-message tests
- `pkg/backup/destination/destination.go` — Destination interface, BackupDescriptor, Factory, typed-key registry
- `pkg/backup/destination/destination_test.go` — Register/Lookup happy path + 3 panic tests + typed-key test

## Decisions Made

- **Registry key type is `models.BackupRepoKind` (not string).** Go does not
  auto-convert typed strings to string on map lookup, so keying on the bare
  string type would force every caller to write `string(repo.Kind)`. The
  `TestRegister_TypedKindKey` test encodes this so future refactors will
  break loudly instead of silently forcing conversion at call sites.
- **No mutex on the registry.** Registration is process-startup-only (once,
  from `cmd/dfs/main.go` or driver `init()` — plan 05 decides); runtime
  mutation is a programmer error. Documented in the registry's doc comment.
- **Panic-on-programmer-error for Register.** Duplicate, empty-kind, or
  nil-factory registration fail loudly at startup rather than silently
  shadowing a driver. Operator-facing errors (unknown repo kind at repo-create
  time) remain the caller's responsibility via `Lookup` (returns `(nil, false)`).
- **`ResetRegistryForTest` is exported.** Tests in plans 03 (fs) and 04 (s3)
  will each need to reset between test runs; exposing one reset helper avoids
  duplication across driver packages.
- **Interface method docs enumerate returned sentinels.** E.g. `PutBackup`
  doc lists `ErrDestinationUnavailable`, `ErrPermissionDenied`, etc. Downstream
  drivers have a precedent for documenting actual returns.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Corrected `TestFactorySignature_Compiles` body**
- **Found during:** Task 2 GREEN (running `go test` after adding `destination.go`)
- **Issue:** The original test body `if factoryStub == nil { t.Fatal(...) }` is
  always false for a named package function. `go vet` treats this as an error
  (`comparison of function factoryStub == nil is always false`) and the build
  failed before any test could run.
- **Fix:** Replaced the check with an explicit `var f Factory = factoryStub`
  assignment and a runtime invocation that asserts the stub returns `(nil, nil)`.
  This still exercises the compile-time signature match (same named var assignment
  used at package scope) and additionally covers the call-site convention that
  `Factory` is safe to invoke.
- **Files modified:** `pkg/backup/destination/destination_test.go`
- **Verification:** `go vet` and `go test -run TestFactorySignature_Compiles` both
  exit 0.
- **Committed in:** `a53d1d39` (part of Task 2 GREEN commit)

---

**Total deviations:** 1 auto-fixed (1 Rule-1 bug in the RED test body)
**Impact on plan:** The fix is cosmetic — the compile-time assertion (package-level
`var _ Factory = factoryStub`) already carried the test's intent; the body now
carries runtime coverage too. No scope creep.

## Issues Encountered

None beyond the auto-fix above. Both tasks followed their plan-specified TDD
cycle (RED — failing build/tests → GREEN — passing implementation). No
architectural decisions required.

## Notes for Downstream Plans (03-02 through 03-06)

- **Plan 02 (envelope/crypto):** Import sentinels from this package for
  `ErrEncryptionKeyMissing`, `ErrInvalidKeyMaterial`, `ErrDecryptFailed`. Wrap via
  `fmt.Errorf("%w: ...: %v", sentinel, cause)` at call sites.
- **Plan 03 (fs driver):** Implement `destination.Destination` on the Store type;
  add `var _ destination.Destination = (*Store)(nil)` at the top of the driver file.
  Factory signature is fixed — take `*models.BackupRepo`, call `repo.GetConfig()`.
- **Plan 04 (s3 driver):** Same shape as plan 03. The narrow `blockStoreLister`
  interface for D-13 prefix-collision check stays in the s3 package (not added here).
- **Plan 05 (registry wiring):** Call `destination.Register(models.BackupRepoKindLocal, fs.New)`
  and `destination.Register(models.BackupRepoKindS3, s3.New)` from one central place
  (recommend `cmd/dfs/main.go` per PATTERNS.md "no-magic" preference — but driver
  `init()` also works). Registry key is `models.BackupRepoKind`, not bare string.
- **Plan 06 (CLI/REST):** Use `destination.Lookup(repo.Kind)` directly; wrap the
  `(nil, false)` case with `fmt.Errorf("%w: unknown destination kind %q", destination.ErrIncompatibleConfig, repo.Kind)`.

## User Setup Required

None — this plan ships a Go package skeleton with unit tests only. No external
services or environment variables.

## Next Phase Readiness

- Downstream plans 03-02 through 03-06 can begin immediately; the `Destination`
  interface, Factory signature, Registry surface, and D-07 sentinels are stable.
- Compile-time satisfaction checks (`var _ destination.Destination = (*Store)(nil)`)
  will enforce the contract when plans 03 and 04 implement the concrete drivers.
- No blockers; no architectural decisions outstanding.

## Self-Check: PASSED

- `pkg/backup/destination/errors.go` — FOUND
- `pkg/backup/destination/errors_test.go` — FOUND
- `pkg/backup/destination/destination.go` — FOUND
- `pkg/backup/destination/destination_test.go` — FOUND
- Commit `3c2fa15d` (test: sentinels) — FOUND
- Commit `1ee981a7` (feat: sentinels) — FOUND
- Commit `61430a1e` (test: interface+registry) — FOUND
- Commit `a53d1d39` (feat: interface+registry) — FOUND
- `go build ./pkg/backup/destination/...` — OK
- `go vet ./pkg/backup/destination/...` — OK
- `go test ./pkg/backup/destination/... -count=1` — OK (10 tests pass)
- `grep -c 'errors.New(' pkg/backup/destination/errors.go` — 11
- `grep -c 'fmt.Errorf' pkg/backup/destination/errors.go` — 0
- `Destination` interface method count — 7
- Registry key type — `models.BackupRepoKind` (typed enum)

---
*Phase: 03-destination-drivers-encryption*
*Completed: 2026-04-16*
