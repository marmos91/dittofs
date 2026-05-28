---
phase: 24-restore-flow
plan: 02
subsystem: api
tags: [errors, sentinels, restore, snapshot, types, go-stdlib]

requires:
  - phase: 22-snapshot-records-hash-manifest-gc-hold
    provides: ErrSnapshotNotFound / ErrSnapshotStateConflict sentinels (Phase 24 collision-tested against these)
provides:
  - 7 typed error sentinels for Phase 24 restore orchestration (D-24-08)
  - RestoreSnapshotOpts struct with AllowNonDurable bool (D-24-06)
affects:
  - 24-03-runtime-restoresnapshot-orchestration
  - 24-04-restore-e2e-integration-test
  - 25-restore-cli-rest-handler (HTTP-status mapping via errors.Is)

tech-stack:
  added: []
  patterns:
    - "Phase-tagged sentinel comment block (mirror of Phase 22/23 pattern)"
    - "TDD-first declaration: failing test → declare sentinel → wrap round-trip"
    - "Zero-value-is-safe-default invariant for orchestration opts structs"

key-files:
  created:
    - pkg/controlplane/models/errors_test.go
    - pkg/controlplane/runtime/restore_opts.go
  modified:
    - pkg/controlplane/models/errors.go

key-decisions:
  - "Phase 23 sentinels are not on develop yet, so the non-collision test slice covers only Phase 22 (ErrSnapshotNotFound, ErrSnapshotStateConflict) plus other long-standing snapshot/share sentinels. When Phase 23 lands, P24-03 (or a follow-up) should widen the collision slice to include Phase 23 sentinels."
  - "SkipPreVerify field deliberately OMITTED from RestoreSnapshotOpts per CONTEXT planner-discretion note. Can be added later non-breakingly since zero-value preserves current behavior; pre-verify is cheap so skipping is rarely justified."
  - "RestoreSnapshotOpts lives in its own file restore_opts.go rather than inside snapshot.go. Keeps the Wave 1 surface free of merge conflicts with P24-03 which extends snapshot.go itself."

patterns-established:
  - "Phase 24 sentinels surfaced to REST in Phase 25 via errors.Is, mapped to HTTP status codes (ErrShareEnabled -> 400, ErrSnapshotNotDurable -> 412, others -> 500)"
  - "Round-trip + non-collision sentinel test triad: nil/uniqueness, fmt.Errorf %w wrap, no errors.Is overlap with prior phases"

requirements-completed: []

duration: 11min
completed: 2026-05-28
---

# Phase 24 Plan 02: Restore Flow Type Vocabulary Summary

**Seven typed error sentinels (D-24-08) + RestoreSnapshotOpts struct (D-24-06) landed in Wave 1 with TDD test coverage for wrap/unwrap and non-collision with prior-phase sentinels.**

## Performance

- **Duration:** ~11 min
- **Started:** 2026-05-28T15:08Z (approx)
- **Completed:** 2026-05-28T15:19Z (approx)
- **Tasks:** 2
- **Files modified:** 1
- **Files created:** 2

## Accomplishments

- Seven `errors.New` sentinels declared in `pkg/controlplane/models/errors.go` under the canonical `// Phase 24 (D-24-08)` comment block, with exact wording from PLAN/CONTEXT (Phase 25 may surface these in HTTP responses, so messages are normative).
- `errors_test.go` covers three orthogonal correctness invariants: non-nil + message-unique, wrap/unwrap round-trip through `fmt.Errorf("restore %q: %w: %v", ...)`, and no errors.Is collision with Phase 22 sentinels.
- `pkg/controlplane/runtime/restore_opts.go` declares `RestoreSnapshotOpts{ AllowNonDurable bool }` with doc-comments mirroring the structure described in the PATTERNS analog, referencing D-24-06 explicitly.
- Phase 22 sentinel block left literally untouched (git diff shows zero `-` lines on prior sentinel declarations).

## Sentinels Declared

| Sentinel | Message | Phase 25 mapping |
|---|---|---|
| `ErrShareEnabled` | `share must be disabled before restore` | 400 |
| `ErrSnapshotNotDurable` | `snapshot is not remote-durable; pass AllowNonDurable to override` | 412 |
| `ErrSnapshotMetadataDumpMissing` | `snapshot metadata dump file is missing` | 500 |
| `ErrMetadataStoreNotResetable` | `metadata engine does not implement Resetable` | 500 |
| `ErrRestoreSafetySnapFailed` | `restore safety snapshot creation or wait failed` | 500 |
| `ErrRestoreAborted` | `restore aborted; safety snapshot retained for rollback` | 500 |
| `ErrRestoreVerifyFailed` | `restore verify failed: missing hashes on remote` | 500 |

Wrap/unwrap round-trip confirmed for all seven via `errors.Is(fmt.Errorf("restore %q: %w: %v", "snap-1", sentinel, inner), sentinel) == true`.

## Task Commits

1. **Task 1 RED — failing sentinel tests** — `f28550b8` (test): non-nil/unique, wrap round-trip, non-collision tests fail to compile.
2. **Task 1 GREEN — sentinel declarations** — `0097e7f3` (feat): 7 sentinels added; all three tests pass; `go vet ./pkg/controlplane/...` clean.
3. **Task 2 — RestoreSnapshotOpts struct** — `2c8138f2` (feat): `restore_opts.go` with `AllowNonDurable bool` + doc-comments referencing D-24-06; `go build ./pkg/controlplane/runtime/...` succeeds.

## Files Created/Modified

- `pkg/controlplane/models/errors.go` — appended `// Phase 24 (D-24-08)` block with 7 sentinels. Phase 22 lines untouched.
- `pkg/controlplane/models/errors_test.go` — created. Package `models_test`. Three tests + a `phase24Sentinels()` helper.
- `pkg/controlplane/runtime/restore_opts.go` — created. Package `runtime`. Single struct declaration; no imports needed.

## Decisions Made

- **Phase 23 collision slice deferred** — Phase 23's sentinels (`ErrSnapshotBackupFailed`, `ErrSnapshotVerifyFailed`, …) aren't on develop yet. The non-collision test covers only sentinels that actually exist in this tree (Phase 22 snapshot + share errors). P24-03 or a Phase 23 merge follow-up should widen the slice when Phase 23 lands. This is a known limitation, not a deviation — the plan body acknowledged it by saying "if it exists in the file" for `ErrSnapshotStateConflict`.
- **SkipPreVerify deferred** per CONTEXT planner-discretion: pre-verify is cheap on durable snapshots (manifest is sorted hex hashes with bounded `Head()` concurrency), so an escape hatch is rarely justified. Adding it later is non-breaking because zero-value preserves the current behavior (pre-verify always runs). P24-03 / Phase 25 see this in the doc-comments and can add the field if observability surfaces a real need.
- **Opts struct in its own file** — keeps the Wave 1 surface clean of merge friction with P24-03's `snapshot.go` extension. The CONTEXT `<canonical_refs>` "Files Phase 24 modifies" line says "extend `snapshot.go`", which P24-03 honors for the orchestration function; the opts type sits in a sibling file so the merge graph is simple.

## Deviations from Plan

None — plan executed exactly as written. Non-collision slice scope was already acknowledged in the plan body as "if it exists in the file" for Phase 23 sentinels (they don't yet), so the narrower slice is plan-conformant rather than a deviation.

## Issues Encountered

- **CreateSnapshotOpts not yet in tree.** Confirmed via `grep -rn "CreateSnapshotOpts"`: Phase 23 surface isn't merged. Doesn't block P24-02 because the plan explicitly scopes to type declarations only — `RestoreSnapshotOpts` doesn't reference `CreateSnapshotOpts`. P24-03 will need to compose against Phase 23 once it lands (per Wave 2 dependency order).

## Threat Model Compliance

- T-24-02-01 (info disclosure): All 7 sentinels are static `errors.New(...)` with no interpolation. Messages contain only failure category — no share names, snapshot IDs, or paths. Operator-readable detail will be added by P24-03 via `fmt.Errorf("%w: %v")` wrapping. ✓
- T-24-02-02 (sentinel identity drift): Non-collision test + uniqueness test guard. ✓
- T-24-02-03 (opts struct evolution): Doc-comment documents the zero-value-is-safe invariant. ✓
- T-24-02-SC (supply-chain): Zero new dependencies. ✓

## Next Phase Readiness

- **P24-03 can now reference** `models.ErrShareEnabled`, `models.ErrSnapshotNotDurable`, `models.ErrSnapshotMetadataDumpMissing`, `models.ErrMetadataStoreNotResetable`, `models.ErrRestoreSafetySnapFailed`, `models.ErrRestoreAborted`, `models.ErrRestoreVerifyFailed`, and `runtime.RestoreSnapshotOpts` immediately. Zero churn expected on these surfaces during Wave 2.
- **P24-04** (integration test) has stable sentinel identifiers to `errors.Is` against in failure-mode subtests.
- **Phase 25** has the canonical surface to map to HTTP status codes via `errors.Is`.

## Self-Check: PASSED

- `pkg/controlplane/models/errors.go` — FOUND, `Phase 24 (D-24-08)` block present (1 match).
- `pkg/controlplane/models/errors_test.go` — FOUND.
- `pkg/controlplane/runtime/restore_opts.go` — FOUND; `type RestoreSnapshotOpts struct` + `AllowNonDurable bool` + `D-24-06` all grep-positive.
- Commits `f28550b8`, `0097e7f3`, `2c8138f2` all on branch `worktree-agent-a5bff223cad5556cb`.
- `go test ./pkg/controlplane/models/... -run TestPhase24Sentinels -count=1` → PASS (3 tests, 7 subtests).
- `go vet ./pkg/controlplane/...` → exit 0.
- `go build ./pkg/controlplane/runtime/...` → exit 0.

---
*Phase: 24-restore-flow*
*Completed: 2026-05-28*
