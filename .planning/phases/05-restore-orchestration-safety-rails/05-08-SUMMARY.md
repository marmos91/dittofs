---
phase: 05-restore-orchestration-safety-rails
plan: 08
subsystem: infra
tags: [safety, gc, blockstore, backup, retention, storebackups, phase5]

requires:
  - phase: 03-destination-drivers-encryption
    provides: "Destination.GetManifestOnly for cheap manifest-only fetches (D-12)"
  - phase: 04-scheduler-retention
    provides: "BackupStore.ListSucceededRecordsByRepo + DestinationFactoryFn wiring pattern"
  - phase: 01-foundations
    provides: "Manifest.PayloadIDSet field + metadata.PayloadID type"
provides:
  - "gc.BackupHoldProvider interface exported from pkg/blockstore/gc"
  - "gc.Options.BackupHold nullable field gating the hold check (pre-Phase-5 compatible)"
  - "gc.CollectGarbage consults hold set at the orphan-detection point"
  - "storebackups.BackupHold implementation unioning manifest PayloadIDSets across every succeeded record"
  - "Continue-on-error semantics (D-13) for per-repo + per-record manifest fetch failures"
affects: [phase-06-cli-rest, phase-07-testing, blockstore-gc-integration]

tech-stack:
  added: []
  patterns:
    - "Capability interface + optional wiring: nil BackupHoldProvider preserves legacy behavior"
    - "At-GC-time manifest union (not persisted hold table) — self-healing on retention deletes"
    - "Fail-open on infrastructure errors (under-hold over abort)"

key-files:
  created:
    - pkg/controlplane/runtime/storebackups/backup_hold.go
    - pkg/controlplane/runtime/storebackups/backup_hold_test.go
  modified:
    - pkg/blockstore/gc/gc.go
    - pkg/blockstore/gc/gc_test.go

key-decisions:
  - "Held payloadID check inserted immediately after the metadata-reference check and before stats.OrphanFiles++ accounting"
  - "heldSet computed exactly once per CollectGarbage run and cached in a local var; passed by reference through loop closure"
  - "Provider errors log WARN (`GC: backup hold provider failed, proceeding without hold`) and proceed with heldSet=nil (fail-open)"
  - "Only ListAllBackupRepos errors are returned to the caller; per-repo and per-record errors are swallowed with WARN log (continue-on-error, matching D-13)"
  - "Service wiring into the actual runtime GC invocation is deferred — this plan produces primitives only (per plan action step 3)"

patterns-established:
  - "Optional capability provider via nullable Options field: Options.BackupHold==nil preserves pre-Phase-5 behavior"
  - "Compile-time interface satisfaction assertion (`var _ gc.BackupHoldProvider = (*BackupHold)(nil)`) at the bottom of the implementation file"
  - "Per-repo resource hygiene: Close() called after each repo inside the loop; error swallowed with underscore"

requirements-completed: [SAFETY-01]

duration: 7min
completed: 2026-04-16
---

# Phase 5 Plan 8: SAFETY-01 Block-GC Hold Provider + GC Integration Summary

**`gc.BackupHoldProvider` interface + `storebackups.BackupHold` implementation unions `PayloadIDSet` from every retained backup manifest so block-GC never reclaims blocks a backup still holds.**

## Performance

- **Duration:** ~7 min
- **Started:** 2026-04-16T22:49:00Z (approx)
- **Completed:** 2026-04-16T22:56:08Z
- **Tasks:** 3
- **Files modified:** 4 (2 created, 2 modified)
- **New tests:** 9 (4 in gc, 5 in storebackups)

## Accomplishments

- `gc.BackupHoldProvider` interface exported; `gc.Options.BackupHold` field added (nullable).
- Orphan-detection path in `CollectGarbage` now consults the hold set exactly once per run via `heldSet := options.BackupHold.HeldPayloadIDs(ctx)` computed at the start of the function.
- Hold check inserted between the metadata-reference check and the `stats.OrphanFiles++` accounting line — retains held payloads with an `INFO` log (`GC: holding orphan for backup`).
- Fail-open semantics: provider errors log `WARN` (`GC: backup hold provider failed, proceeding without hold`) and GC proceeds with no hold rather than aborting.
- `storebackups.BackupHold` iterates every repo via `ListAllBackupRepos`, every succeeded record via `ListSucceededRecordsByRepo`, fetches each manifest via `Destination.GetManifestOnly`, and unions `manifest.PayloadIDSet` into a `map[metadata.PayloadID]struct{}`.
- Continue-on-error at two layers: `destFactory` failure skips the repo; `GetManifestOnly` failure skips the record. Only `ListAllBackupRepos` failure propagates to the caller (infrastructure-level, nothing to iterate otherwise).
- Compile-time assertion `var _ gc.BackupHoldProvider = (*BackupHold)(nil)` guards against future signature drift.

## Task Commits

1. **Task 1: Add BackupHoldProvider interface + Options.BackupHold + hold check in CollectGarbage** — `f30daa6f` (feat)
2. **Task 2: GC hold tests — held payload preserved, errors fail open** — `68a1edf8` (test)
3. **Task 3: Create storebackups.BackupHold implementing gc.BackupHoldProvider** — `01ad8de3` (feat)

## Files Created/Modified

- `pkg/blockstore/gc/gc.go` — `BackupHoldProvider` interface (line 68), `Options.BackupHold` field (line 48), `heldSet` computation (line 122-134), hold check at orphan-detection point (line 188-196).
- `pkg/blockstore/gc/gc_test.go` — fake provider + 4 new tests: `TestGC_BackupHold_PreservesHeldPayload`, `TestGC_BackupHold_OrphanStillDeletedWhenNotHeld`, `TestGC_BackupHold_ProviderError_FailsOpen`, `TestGC_NilBackupHold_PreservesLegacyBehavior`.
- `pkg/controlplane/runtime/storebackups/backup_hold.go` — `BackupHold` struct, `NewBackupHold` constructor, `HeldPayloadIDs` method, compile-time interface check.
- `pkg/controlplane/runtime/storebackups/backup_hold_test.go` — fake `BackupStore` + fake `Destination` + 5 tests: `TestBackupHold_UnionAcrossRepos`, `TestBackupHold_ListReposFails`, `TestBackupHold_DestFactoryFails_SkipsRepo`, `TestBackupHold_GetManifestOnlyFails_SkipsRecord`, `TestBackupHold_EmptyWhenNoSucceededRecords`.

## Decisions Made

### Exact line in gc.go where the hold check was inserted

The hold check is inserted at `pkg/blockstore/gc/gc.go:188-196`, immediately after the metadata lookup block (line 169-183 — the one that ends with `continue` when `metaStore.GetFileByPayloadID` succeeds) and immediately before the `stats.OrphanFiles++` line. This gives the ordering:

```
GetFileByPayloadID succeeds  -> continue (not orphan)
GetFileByPayloadID fails AND held[payloadID] -> log "GC: holding orphan for backup" + continue
otherwise                    -> stats.OrphanFiles++ (orphan path)
```

`heldSet` itself is computed at `pkg/blockstore/gc/gc.go:122-134`, once per `CollectGarbage` call, hoisted above both the per-payload loop AND the block-grouping pass so that an early return from a `ctx.Err()` or bad-payloadID-format path doesn't leak a half-populated hold set into later work.

### Fail-open on provider errors

When `options.BackupHold.HeldPayloadIDs(ctx)` returns an error, GC logs `WARN` with message `"GC: backup hold provider failed, proceeding without hold"` and the `error` kv pair. `heldSet` stays nil, and the per-payload hold check short-circuits on the `heldSet != nil` gate. Orphans are still reclaimed as in pre-Phase-5 behavior. Preferring under-hold over GC-abort matches D-13 and avoids a "manifest-fetch infrastructure outage halts block reclamation" failure mode.

### Service wiring status

**Deferred.** This plan intentionally produces primitives only — the `Options.BackupHold` field and the `storebackups.BackupHold` struct — without wiring them into the actual `storebackups.Service` GC invocation site. The plan action step 3 says:

> Do NOT modify `service.go` to wire the provider into the GC run in this plan — wiring into the runtime lives in Plan 09 (or a follow-up) since it touches the existing GC invocation site and overlaps with the adapter changes.

Callers that need the hold today can construct `BackupHold` manually and pass it through `gc.Options{BackupHold: backupHold}` at the call site. The compile-time interface assertion ensures the constructed value satisfies the interface.

## Deviations from Plan

None — plan executed exactly as written. All 9 tests (4 gc + 5 storebackups) pass with `-race`. Full-project `go build ./...` and `go vet ./...` remain clean.

**Total deviations:** 0
**Impact on plan:** None.

## Issues Encountered

None — TDD paths RED → GREEN cleanly; no regressions in adjacent packages.

## Test Outcomes (9 new tests)

| Test | Result |
|------|--------|
| `TestGC_BackupHold_PreservesHeldPayload` | PASS — held payloadID retained; orphan counter 0; block still readable |
| `TestGC_BackupHold_OrphanStillDeletedWhenNotHeld` | PASS — non-held orphan reclaimed; stats.OrphanFiles=1 |
| `TestGC_BackupHold_ProviderError_FailsOpen` | PASS — WARN logged; orphan still deleted (fail-open) |
| `TestGC_NilBackupHold_PreservesLegacyBehavior` | PASS — regression guard for nil provider path |
| `TestBackupHold_UnionAcrossRepos` | PASS — 5 unique payloadIDs unioned across 2 repos / 4 records; Close called twice (once per repo) |
| `TestBackupHold_ListReposFails` | PASS — sentinel wraps through; destFactory never invoked |
| `TestBackupHold_DestFactoryFails_SkipsRepo` | PASS — survivor repo's payload present; failed repo skipped with WARN |
| `TestBackupHold_GetManifestOnlyFails_SkipsRecord` | PASS — good record's 2 payloads present; bad record silently excluded |
| `TestBackupHold_EmptyWhenNoSucceededRecords` | PASS — non-nil empty map returned |

## User Setup Required

None — no external service configuration required.

## Next Phase Readiness

- `gc.BackupHoldProvider` and `storebackups.BackupHold` primitives are production-ready.
- Runtime wiring (constructing a `BackupHold` inside the `storebackups.Service` and passing `Options.BackupHold` at the GC invocation site) is deferred to a follow-up plan; no blockers.
- Phase 7 chaos tests can now verify "block retained across backup-day + GC-run + restore-day" by running `CollectGarbage` with a non-nil `BackupHold` after a successful backup and before a simulated restore.

## Self-Check: PASSED

- `pkg/blockstore/gc/gc.go`: FOUND
- `pkg/blockstore/gc/gc_test.go`: FOUND
- `pkg/controlplane/runtime/storebackups/backup_hold.go`: FOUND
- `pkg/controlplane/runtime/storebackups/backup_hold_test.go`: FOUND
- Commit `f30daa6f`: FOUND
- Commit `68a1edf8`: FOUND
- Commit `01ad8de3`: FOUND

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
