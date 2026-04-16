---
phase: 03-destination-drivers-encryption
plan: 05
subsystem: backup
tags: [backup, destination, registry, factory, fs, s3, aes-256-gcm]

# Dependency graph
requires:
  - phase: 03-destination-drivers-encryption
    provides: "Destination interface, typed-kind Registry (Register/Lookup), fs.New, s3.New, ErrIncompatibleConfig sentinel"
provides:
  - "DestinationFactoryFromRepo(ctx, repo) — single dispatch entrypoint used by Phase 4/5/6 (no fs/s3 import required)"
  - "Kinds() []models.BackupRepoKind — deterministic sorted introspection of registered drivers"
  - "builtins.RegisterBuiltins() — explicit wiring of the two builtin drivers (local, s3) to be called once at cmd/dfs/main.go startup"
affects:
  - "04-scheduler-retention — scheduler calls DestinationFactoryFromRepo per repo tick"
  - "05-restore-orchestration — restore orchestrator uses DestinationFactoryFromRepo for GetBackup"
  - "06-cli-api — cmd/dfs/main.go wires builtins.RegisterBuiltins(); REST handlers dispatch via DestinationFactoryFromRepo"

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Factory + Registry keyed on typed models.BackupRepoKind (not bare string) for compile-time lookup safety"
    - "Explicit RegisterBuiltins() in a separate builtins/ sub-package — NO init() side-effects, called once from main"
    - "Deterministic Kinds() listing surfaced in unknown-kind error messages (operator diagnostics)"

key-files:
  created:
    - "pkg/backup/destination/registry.go — DestinationFactoryFromRepo + Kinds"
    - "pkg/backup/destination/registry_test.go — 7 tests (happy/unknown/nil/empty/kinds-sorted/kinds-empty/typed-constants)"
    - "pkg/backup/destination/builtins/builtins.go — RegisterBuiltins wiring"
    - "pkg/backup/destination/builtins/builtins_test.go — 2 tests (both-registered / duplicate-panics)"
  modified: []

key-decisions:
  - "s3Factory does not accept additional Options — Phase 6 callers that need blockStoreLister (D-13 collision check) will wrap the factory at the API handler layer. Keeps the Factory type uniform."
  - "Kinds() listing is embedded in the unknown-kind error message so operators see exactly which drivers are wired at the moment of failure (addresses threat T-03-32 repudiation)."
  - "No init() in builtins/ — PATTERNS.md explicitly called out init-order surprises, and the existing code culture (runtime/shares/service.go switch dispatch) favors explicit wiring."

patterns-established:
  - "Dispatch pattern: callers of DestinationFactoryFromRepo pass repo.Kind verbatim; the function wraps Lookup with ErrIncompatibleConfig-wrapped diagnostics"
  - "Builtins wiring pattern: a tiny sub-package that only exposes a single RegisterBuiltins() function, imported once by main — future new drivers slot in via Register() calls alongside local+s3"

requirements-completed: [DRV-01, DRV-02]

# Metrics
duration: 2min
completed: 2026-04-16
---

# Phase 3 Plan 05: Registry Wiring + DestinationFactoryFromRepo Summary

**Registry-level dispatcher `DestinationFactoryFromRepo(ctx, repo)` and `builtins.RegisterBuiltins()` that wire fs+s3 drivers under the typed `models.BackupRepoKind` constants — no string conversion, no init() magic.**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-04-16T12:46:31Z
- **Completed:** 2026-04-16T12:48:39Z
- **Tasks:** 2
- **Files created:** 4 (2 impl + 2 test)
- **Files modified:** 0

## Accomplishments

- `DestinationFactoryFromRepo(ctx, repo)` dispatches via `Lookup(repo.Kind)` directly — Go's typed-string guarantee prevents any caller from accidentally passing a bare `string` and getting silent lookup miss.
- `Kinds() []models.BackupRepoKind` returns a deterministic sorted list; embedded in unknown-kind error messages so operators see the registered set at the moment of failure.
- `builtins.RegisterBuiltins()` wires `models.BackupRepoKindLocal → fs.New` and `models.BackupRepoKindS3 → s3.New` via thin adapters — the single place downstream callers flip to when wiring new builtin drivers.
- Seven registry tests + two builtins tests; TDD RED-first for both tasks (build failures confirmed before implementations landed).

## Task Commits

Each task was committed atomically with the TDD RED → GREEN gate:

1. **Task 1 RED: failing tests for DestinationFactoryFromRepo + Kinds** — `2080b7b5` (test)
2. **Task 1 GREEN: add DestinationFactoryFromRepo + Kinds helpers** — `940b1420` (feat)
3. **Task 2 RED: failing tests for RegisterBuiltins helper** — `f85cf68d` (test)
4. **Task 2 GREEN: add builtins.RegisterBuiltins wiring fs+s3 drivers** — `2ee281a3` (feat)

## Files Created/Modified

- `pkg/backup/destination/registry.go` — `DestinationFactoryFromRepo` (single dispatch entrypoint) + `Kinds` (deterministic introspection)
- `pkg/backup/destination/registry_test.go` — 7 tests covering happy path, unknown kind (with Kinds listing check), nil repo, empty kind, Kinds determinism, Kinds empty, typed-constants round-trip via `models.BackupRepoKindLocal` / `BackupRepoKindS3`
- `pkg/backup/destination/builtins/builtins.go` — `RegisterBuiltins` + thin `localFactory` / `s3Factory` adapters (no init())
- `pkg/backup/destination/builtins/builtins_test.go` — 2 tests covering both-kinds-registered and duplicate-panics

## Decisions Made

- **s3Factory accepts no extra Options.** Phase 6 API handlers that need a `blockStoreLister` (for the D-13 bucket/prefix collision check at `ValidateConfig` time) will wrap `s3Factory` at a higher layer rather than changing the `destination.Factory` signature. Keeps the Factory type uniform across fs and s3.
- **Kinds() listed in unknown-kind error message.** Not just "unknown kind X" but "unknown kind X (registered: [local s3])". Directly addresses STRIDE threat T-03-32 (repudiation): operators can tell whether the driver wasn't registered vs. whether the repo row has a typo.
- **No init() in builtins/.** PATTERNS.md ("No Analog Found" section) warned against init-order coupling; the existing code culture uses explicit switch dispatch (`runtime/shares/service.go:954-1038`). `RegisterBuiltins` is called exactly once from `cmd/dfs/main.go` in Phase 6 — still unwired at the end of this plan.

## Deviations from Plan

None — plan executed exactly as written.

Every plan step (import block, stubDest shape, compile-time check `var _ Destination = (*stubDest)(nil)`, no-conversion `Lookup(repo.Kind)`, Kinds-embedded error message, duplicate-panic contract) landed verbatim.

## Issues Encountered

None.

## Verification

Ran:
```bash
go build ./pkg/backup/destination/...
go vet ./pkg/backup/destination/...
go test ./pkg/backup/destination/... -count=1
```

Results (clean):
```
ok  	github.com/marmos91/dittofs/pkg/backup/destination        0.218s
ok  	github.com/marmos91/dittofs/pkg/backup/destination/builtins 0.308s
ok  	github.com/marmos91/dittofs/pkg/backup/destination/fs     0.645s
ok  	github.com/marmos91/dittofs/pkg/backup/destination/s3     0.637s
```

Task 1 narrow run:
```
go test ./pkg/backup/destination/ -run 'TestDestinationFactoryFromRepo|TestKinds' -count=1 -v
```
7/7 pass: `TestDestinationFactoryFromRepo_HappyPath`, `_UnknownKind`, `_NilRepo`, `_EmptyKind`, `_TypedConstants`, `TestKinds_Deterministic`, `TestKinds_Empty`.

Task 2 narrow run:
```
go test ./pkg/backup/destination/builtins/... -count=1 -v
```
2/2 pass: `TestRegisterBuiltins_BothKindsRegistered`, `TestRegisterBuiltins_DuplicatePanics`.

Plan-level self-checks:
- `grep -c 'func init()' pkg/backup/destination/builtins/builtins.go` → 0 (no init, as required)
- `grep -c 'RegisterBuiltins' pkg/backup/destination/builtins/builtins.go` → 3 (godoc + declaration + impl body)
- `grep -c 'models.BackupRepoKindLocal' pkg/backup/destination/builtins/builtins.go` → 1
- `grep -c 'models.BackupRepoKindS3' pkg/backup/destination/builtins/builtins.go` → 1
- `grep 'Lookup(repo.Kind)' pkg/backup/destination/registry.go` → present (godoc ref + dispatch call)
- `grep 'Lookup(string(repo.Kind))' pkg/backup/destination/registry.go` → absent (no string conversion anywhere)

## Output Report (from plan)

- **s3Factory additional Options needed?** No. `dests3.New(ctx, repo)` is called with zero extra options. Phase 6 API handlers that need `WithBlockStoreLister` / `WithClock` will either (a) register a custom factory replacing `s3Factory` or (b) wrap the factory inline at the call site. This keeps the `destination.Factory` type uniform across fs and s3.
- **Total registry-based tests:** 9 (`registry_test.go` = 7, `builtins_test.go` = 2). Pre-existing `destination_test.go` (6 tests for Register/Lookup/ResetRegistryForTest) brings package total to 15.
- **RegisterBuiltins call sites:** 0 production call sites yet. Phase 6 (`cmd/dfs/main.go` wiring) adds the single startup call. Tests call it directly via `builtins.RegisterBuiltins()` with `destination.ResetRegistryForTest` sandwich.
- **`Lookup(repo.Kind)` confirmation:** Confirmed by source inspection (`grep -n 'Lookup(repo.Kind)' pkg/backup/destination/registry.go`). There are zero `string(repo.Kind)` / `string(repo.Kind)` casts anywhere in the new code.

## Threat Flags

None — this plan only adds a dispatcher and builtin wiring. No new network endpoints, no new auth paths, no new file access patterns, no schema changes. The typed-kind registry is itself a mitigation for T-03-30 (attacker-supplied kind triggers wrong driver) as documented in the plan's threat model.

## Next Plan Readiness

- Phase 3 plan 06 (API surface wiring) can import `pkg/backup/destination/builtins` and call `RegisterBuiltins()` at process startup. Callers dispatch to drivers via `destination.DestinationFactoryFromRepo(ctx, repo)` without touching fs/ or s3/ directly.
- Phase 4 scheduler and Phase 5 restore orchestrator can use the same helper; the typed-kind guarantee means passing a stale or mis-typed BackupRepoKind fails compilation at the call site rather than at runtime.

## Self-Check: PASSED

- `pkg/backup/destination/registry.go` — FOUND
- `pkg/backup/destination/registry_test.go` — FOUND
- `pkg/backup/destination/builtins/builtins.go` — FOUND
- `pkg/backup/destination/builtins/builtins_test.go` — FOUND
- Commit `2080b7b5` — FOUND (test RED Task 1)
- Commit `940b1420` — FOUND (feat GREEN Task 1)
- Commit `f85cf68d` — FOUND (test RED Task 2)
- Commit `2ee281a3` — FOUND (feat GREEN Task 2)

## TDD Gate Compliance

Both tasks followed the strict RED → GREEN sequence:
- Task 1: `test(03-05)` commit (`2080b7b5`) precedes `feat(03-05)` commit (`940b1420`)
- Task 2: `test(03-05)` commit (`f85cf68d`) precedes `feat(03-05)` commit (`2ee281a3`)

No REFACTOR commit needed — both implementations were minimal and clean on first pass.

---
*Phase: 03-destination-drivers-encryption*
*Plan: 05*
*Completed: 2026-04-16*
