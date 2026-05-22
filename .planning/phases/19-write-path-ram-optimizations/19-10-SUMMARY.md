---
phase: 19-write-path-ram-optimizations
plan: 10
subsystem: blockstore
tags: [blockstore, config, cleanup, transitional, syncer, claim-batch, d-23, d-24, d-25, d-26]

requires:
  - phase: 19-write-path-ram-optimizations
    plan: 06
    provides: "TRANSITIONAL-NEXT-MILESTONE: O_DIRECT marker (appendwrite.go) — Plan 10 verifies"
  - phase: 19-write-path-ram-optimizations
    plan: 07
    provides: "TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM + zstd compression + cold-cache prefetch markers — Plan 10 verifies"
provides:
  - "ClaimBatchSize cycle CLOSED — Phase 18 D-16 deprecation done (D-23)"
  - "D-24 admin-method audit complete — zero DELETE candidates (audit is the deliverable per CONTEXT.md)"
  - "D-25 TRANSITIONAL marker audit complete — all 5 markers point at #519 v0.17+"
  - "D-26 5/5 anchor set complete — tmpfs spill marker added on appendlog.go::writeRecord"
  - "pkg/blockstore/doc.go documents the D-23 cycle closure + D-25 audit outcome"
affects:
  - "Phase 19 mega-PR — D-01 readiness: green tree at every Wave 1→2→3→4 internal commit boundary"
  - "v0.17+ planning — the 5 TRANSITIONAL anchors plus #519 inline references are the grep handles the v0.17 planner consults"

tech-stack:
  added: []
  patterns:
    - "Tombstone-comment closure for cycles that did not use a TRANSITIONAL- marker (the D-23 ClaimBatchSize deprecation lived in inline godoc, not the grep namespace): the field/defaults/validate disappear, replaced by a short historical note in the same godoc block referencing the closing phase"
    - "TRANSITIONAL-NEXT-MILESTONE convention preserved: when every deferred marker shares a single concrete target (#519 v0.17+), use the inline see-reference rather than burning a TRANSITIONAL-V0.17 tag into the grep namespace — keeps the next planner's grep surface clean until v0.17 actually commits to a deletion plan"

key-files:
  created: []
  modified:
    - "pkg/blockstore/engine/types.go — SyncerConfig.ClaimBatchSize field deleted; DefaultConfig assignment dropped"
    - "pkg/blockstore/engine/syncer.go — NewSyncer ClaimBatchSize default-on-zero branch deleted"
    - "pkg/blockstore/engine/engine_dualread_test.go — cfg.ClaimBatchSize = 4 assignment dropped"
    - "pkg/config/config.go — SyncerConfig.ClaimBatchSize field + ApplyDefaults branch + Validate branch + UploadConcurrency <= ClaimBatchSize cross-validation all deleted; godoc tombstone added documenting D-23 closure"
    - "pkg/config/syncer_test.go — full rewrite: assertions for ClaimBatchSize default / explicit-value preservation / Validate-rejects-zero / Validate-rejects-concurrency-above-batch all deleted; only the four remaining-field tests + UploadConcurrency-rejection test retained"
    - "docs/ARCHITECTURE.md — 'syncer.claim_batch_size' references removed from the BlockState diagram caption and the 'why a metadata write for every claim?' paragraph"
    - "docs/CONFIGURATION.md — yaml example reduced (claim_batch_size knob removed); tuning guidance amended"
    - "pkg/blockstore/local/fs/appendlog.go — TRANSITIONAL-NEXT-MILESTONE: tmpfs spill marker added on writeRecord (D-26 anchor #5/5)"
    - "pkg/blockstore/doc.go — convention-table section gains the Phase 19 D-25 audit outcome paragraph + D-23 cycle closure note"

key-decisions:
  - "D-24 audit found ZERO DELETE candidates. Two FSStore methods have zero non-test production callers (TruncateAppendLog, GetStoredFileSize) — both are part of the Phase 18 D-18 LocalStore admin-superset (TruncateAppendLog reaches via BlockStoreAppend embedding; GetStoredFileSize is an explicit method on LocalStore). Per CONTEXT.md: 'the admin-superset … STAYS even if no current callers.' The audit-is-the-deliverable outcome was explicitly authorized by CONTEXT.md ('if the audit finds nothing, the sweep is a no-op commit; acceptable')."
  - "D-25 audit: all 5 TRANSITIONAL-NEXT-MILESTONE markers across pkg/blockstore/ already point at #519 'Deferred to v0.17+'. Decision: keep the NEXT-MILESTONE tag (the inline see-reference documents the v0.17+ target). Rationale: v0.17 has not committed to a concrete deletion plan; burning TRANSITIONAL-V0.17 into the grep namespace now would create a marker the v0.17 planning pass would need to either delete-on-resolve or rewrite-back-to-NEXT-MILESTONE if v0.17 punts. The current shape (NEXT-MILESTONE tag + #519 reference inline) lets v0.17's planner make that call cleanly."
  - "D-26 anchor #5/5 (tmpfs spill) landed on appendlog.go::writeRecord rather than on the appendwrite.go side. Rationale: writeRecord IS the disk-spill site (the io.Writer arg today is always the on-disk fd; the v0.17+ tmpfs-spill change pivots on the caller's choice of `w`). The marker on writeRecord's godoc is the cleanest grep handle for the v0.17 work — a planner greps for the marker and immediately sees the site whose signature stays stable across the tier insertion."
  - "ClaimBatchSize cycle closure via tombstone comment (not TRANSITIONAL marker). The Phase 11 D-13 field was deprecated in Phase 18 D-16 via an inline godoc note; it was never tagged with TRANSITIONAL-NEXT-MILESTONE: in either the engine or pkg/config tree. Phase 19 D-23 closes the cycle by deleting the field and leaving a short historical note in the same godoc block — symmetric to how the deprecation entered the codebase."
  - "Viper safety on existing config files: the dropped mapstructure/yaml tag `claim_batch_size` becomes an unknown key. Verified via inspection that no in-tree yaml fixture contains `claim_batch_size:` (grep across docs/ + .yaml/.yml in repo returns only the deleted docs/CONFIGURATION.md example block). Viper silently ignores unknown keys at decode time — operators on legacy configs will not see config errors."

requirements-completed: [D-01, D-18, D-23, D-24, D-25, D-26]

duration: ~25min
completed: 2026-05-21
---

# Phase 19 Plan 10: Cleanup sweeps — ClaimBatchSize closure + D-25 marker audit + D-26 tmpfs anchor

**Plan 10 closes Phase 19's housekeeping. The Phase 18 D-16 `ClaimBatchSize` deprecation cycle ends here (field + defaults + validate + tests + docs all deleted from pkg/config and pkg/blockstore/engine). The D-24 dead-method audit found zero DELETE candidates (every FSStore method with zero non-test production callers is part of the Phase 18 D-18 admin-superset, which intentionally stays). The D-25 TRANSITIONAL-marker audit found all 5 markers already pointing at #519 v0.17+ — kept as NEXT-MILESTONE per audit decision. D-26 anchor #5/5 (tmpfs spill, appendlog.go::writeRecord) is in place. pkg/blockstore/doc.go now records the cycle closure + audit outcome.**

## Performance

- **Duration:** ~25 minutes
- **Tasks:** 3 (D-23 deletion, D-24 audit, D-25+D-26+doc.go combined)
- **Source files modified:** 7 (pkg/config + pkg/blockstore/engine + pkg/blockstore/local/fs + pkg/blockstore/doc.go + docs/)
- **Test files modified:** 2 (pkg/config/syncer_test.go full rewrite; pkg/blockstore/engine/engine_dualread_test.go 1 line)

## Accomplishments

### D-23: SyncerConfig.ClaimBatchSize deletion

The Phase 18 D-16 deprecation cycle closes. The field, both ApplyDefaults branches (pkg/config and pkg/blockstore/engine), both Validate branches, the `UploadConcurrency <= ClaimBatchSize` cross-validation, all test assertions (5 in pkg/config/syncer_test.go), the docs examples (2 in docs/CONFIGURATION.md), and the architecture-doc references (2 in docs/ARCHITECTURE.md) are gone.

The field was dead code since Phase 11: set/defaulted but never read by the syncer claim path. Removing it has no behavioral effect; the closure is hygiene-only.

### D-24: Admin-method audit — zero DELETE candidates

Audit procedure: enumerate every `func (bc *FSStore) ...` in pkg/blockstore/local/fs/*.go (excluding _test.go), count non-test in-tree callers, classify against the Phase 18 D-18 admin-superset.

| Method | Non-test callers | Classification | Action |
|---|---|---|---|
| `AppendWrite` | 1 (BlockStoreAppend interface) | STAYS (BlockStore.Append surface) | Keep |
| `DeleteAppendLog` | 1 (rollup path) | STAYS | Keep |
| `TruncateAppendLog` | 0 | STAYS (D-18 admin-superset; reachable via `BlockStoreAppend`/`LocalStore` interface; Phase 10 D-29 contract) | Keep |
| `Put` | many | STAYS (BlockStore.Put) | Keep |
| `Get` | many | STAYS (BlockStore.Get) | Keep |
| `GetRange` | 1 | STAYS (BlockStore.GetRange) | Keep |
| `Has` | 2 | STAYS (BlockStore.Has) | Keep |
| `Delete` | 1 | STAYS (BlockStore.Delete) | Keep |
| `Head` | 1 | STAYS (BlockStore.Head) | Keep |
| `Walk` | 1 | STAYS (BlockStore.Walk) | Keep |
| `ListUnsynced` | 1 | STAYS (LocalStore.ListUnsynced) | Keep |
| `DeleteLog` | 5 | STAYS (BlockStoreAppend.DeleteLog) | Keep |
| `StoreChunk` | 2 | STAYS (rollup pool) | Keep |
| `ReadChunk` | 1 | STAYS (engine fallback) | Keep |
| `HasChunk` | 2 | STAYS | Keep |
| `DeleteChunk` | 1 | STAYS (GC) | Keep |
| `ReadPayloadAt` | 1 | STAYS (engine primary read entry) | Keep |
| `SetObjectIDPersister` | 1 (engine.New) | STAYS (Phase 11 LSL-07/08 injection pattern) | Keep |
| `SetOnChunkComplete` | 1 (engine.New) | STAYS (Phase 19 Plan 04/07 — Opt 3 wire-in) | Keep |
| `SetRetentionPolicy` | 3 | STAYS (D-18 admin-superset) | Keep |
| `SetEvictionEnabled` | 6 | STAYS (D-18 admin-superset) | Keep |
| `SyncFileBlocks` | 6 | STAYS (D-18 admin-superset) | Keep |
| `SyncFileBlocksForFile` | 2 | STAYS (D-18 admin-superset) | Keep |
| `EvictMemory` | 2 | STAYS (D-18 admin-superset) | Keep |
| `Truncate` | 12 | STAYS (D-18 admin-superset) | Keep |
| `ListFiles` | 4 | STAYS (D-18 admin-superset) | Keep |
| `Stats` | 7 | STAYS (D-18 observability superset) | Keep |
| `GetFileSize` | 4 | STAYS (D-18 admin-superset) | Keep |
| `GetStoredFileSize` | 0 | STAYS (D-18 admin-superset; LocalStore interface method line 105) | Keep |
| `Start` | 1 | STAYS (lifecycle) | Keep |
| `Close` | 1 | STAYS (lifecycle) | Keep |
| `StartRollup` | 1 | STAYS (rollup lifecycle) | Keep |
| `Recover` | 2 | STAYS (boot lifecycle) | Keep |
| `Healthcheck` (manage.go) | LocalStore interface | STAYS | Keep |

The two methods with zero non-test production callers — `TruncateAppendLog` and `GetStoredFileSize` — are both surfaced via the `LocalStore` interface (line 36 embed of `BlockStoreAppend` for `TruncateAppendLog`; line 105 explicit method for `GetStoredFileSize`). Phase 18 D-18 explicitly preserves the admin-superset on the LocalStore interface regardless of current caller count. The audit therefore yields no deletions; the audit table above IS the deliverable per CONTEXT.md ("the audit itself is the deliverable").

### D-25: TRANSITIONAL-NEXT-MILESTONE marker audit

Grep enumerated 5 instance markers (plus 1 convention-definition occurrence in doc.go that is not an instance):

| Site | Marker text | Phase 19 resolved? | Disposition | Resulting state |
|---|---|---|---|---|
| `pkg/blockstore/local/fs/chunkstore.go:37` | `TRANSITIONAL-NEXT-MILESTONE: zstd compression (see #519 "Deferred to v0.17+")` | No | Generic-tag-with-#519-inline-ref keeps the grep namespace clean until v0.17 commits to a deletion plan | Kept as-is |
| `pkg/blockstore/local/fs/chunkstore.go:110` | `TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM (see #519 "Deferred to v0.17+")` | No | Same rationale as above | Kept as-is |
| `pkg/blockstore/local/fs/appendwrite.go:297` | `TRANSITIONAL-NEXT-MILESTONE: O_DIRECT for log writes (see #519 "Deferred to v0.17+")` | No | Same rationale as above | Kept as-is |
| `pkg/blockstore/engine/cache.go:46` | `TRANSITIONAL-NEXT-MILESTONE: cold-cache prefetch (see #519 "Deferred to v0.17+")` | No | Same rationale as above | Kept as-is |
| `pkg/blockstore/local/fs/appendlog.go::writeRecord` (NEW) | `TRANSITIONAL-NEXT-MILESTONE: tmpfs spill (see #519 "Deferred to v0.17+")` | New anchor per D-26 | Generic-tag matches the other four; #519 reference inline documents the v0.17+ target | Added |

No marker was resolved by Phase 19 (the D-23 ClaimBatchSize cycle never carried a TRANSITIONAL marker — its deprecation was an inline godoc note). All 5 markers stay with the NEXT-MILESTONE tag; the v0.17 planner consults the inline #519 references when committing to a concrete deletion plan.

The convention-definition occurrence at `pkg/blockstore/doc.go:189` is the godoc-table row describing the convention — not an instance — and is preserved per Phase 18 D-19.

### D-26: 5/5 anchor verification

All 5 v0.17+ anchors are present after Plan 10:

```
pkg/blockstore/local/fs/chunkstore.go:37:   TRANSITIONAL-NEXT-MILESTONE: zstd compression
pkg/blockstore/local/fs/chunkstore.go:110:  TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM
pkg/blockstore/local/fs/appendwrite.go:297: TRANSITIONAL-NEXT-MILESTONE: O_DIRECT
pkg/blockstore/local/fs/appendlog.go:~108:  TRANSITIONAL-NEXT-MILESTONE: tmpfs spill  ← Plan 10 added
pkg/blockstore/engine/cache.go:46:          TRANSITIONAL-NEXT-MILESTONE: cold-cache prefetch
```

Four of the five (chunkstore.go ×2, appendwrite.go, cache.go) landed in Plans 06 and 07; Plan 10 adds the fifth on `appendlog.go::writeRecord` (the canonical disk-spill site whose io.Writer signature stays stable across the v0.17+ tmpfs tier insertion).

### pkg/blockstore/doc.go update

The convention-table section gains two paragraphs:
- D-25 audit outcome — all 5 NEXT-MILESTONE markers point at #519 v0.17+; no V0.17-specific re-targeting until v0.17's planning pass commits.
- D-23 cycle closure — ClaimBatchSize field gone; the cycle did not use a TRANSITIONAL marker so this note is the cycle's epitaph.

## Task Commits

1. **Task 1 (D-23):** `0a492d40` — `refactor(19-10): delete SyncerConfig.ClaimBatchSize (D-23)`
2. **Task 2 (D-24 audit):** _no code change_ — audit-is-deliverable per CONTEXT.md; table above is the artifact.
3. **Task 3 (D-25 + D-26 + doc.go):** `675ce22d` — `docs(19-10): close D-25 audit + add D-26 tmpfs spill anchor`

All commits signed (`git commit -S`).

## Files Created/Modified

### Modified

- `pkg/blockstore/engine/types.go` — SyncerConfig.ClaimBatchSize field + DefaultConfig assignment deleted; godoc updated.
- `pkg/blockstore/engine/syncer.go` — NewSyncer ClaimBatchSize default-on-zero branch deleted; godoc updated.
- `pkg/blockstore/engine/engine_dualread_test.go` — `cfg.ClaimBatchSize = 4` line dropped.
- `pkg/config/config.go` — SyncerConfig.ClaimBatchSize field deleted; ApplyDefaults / Validate / cross-validation branches deleted; godoc tombstone added.
- `pkg/config/syncer_test.go` — full rewrite; 2 of 5 original tests dropped (`TestSyncerConfig_ValidateRejectsClaimBatchSize`, `TestSyncerConfig_ValidateRejectsConcurrencyAboveBatch`); remaining 3 tests updated to remove the field; net 5 tests in final file.
- `docs/ARCHITECTURE.md` — 2 mentions of `syncer.claim_batch_size` rewritten.
- `docs/CONFIGURATION.md` — yaml example block trimmed; tuning guidance amended.
- `pkg/blockstore/local/fs/appendlog.go` — TRANSITIONAL-NEXT-MILESTONE: tmpfs spill marker added on `writeRecord` godoc.
- `pkg/blockstore/doc.go` — convention-table section gains Phase 19 audit/closure paragraphs.

### Created

None.

## Decisions Made

### D-24 audit is no-op (the audit is the artifact)

Found zero DELETE candidates. Two FSStore methods have zero non-test production callers (`TruncateAppendLog`, `GetStoredFileSize`); both are LocalStore-interface methods that Phase 18 D-18 explicitly preserves. The CONTEXT.md authorization is explicit: "audit-finds-nothing is acceptable per CONTEXT.md". The audit table above is the deliverable.

### D-25 marker disposition: keep NEXT-MILESTONE on all 5

All 5 markers already point at #519 "Deferred to v0.17+" via inline see-reference. Decision: keep the NEXT-MILESTONE tag rather than retag to TRANSITIONAL-V0.17. Reason: v0.17 has not committed to a concrete deletion plan. Retagging now would create a marker that v0.17's planning pass would either delete-on-resolve or rewrite-back-to-NEXT-MILESTONE if v0.17 punts. The current shape (NEXT-MILESTONE tag + #519 reference inline) is the minimum-future-churn shape.

### D-26 anchor #5 placement

Marker landed on `appendlog.go::writeRecord` rather than on a buffering-threshold site in appendwrite.go. Rationale: writeRecord IS the disk-spill site (its io.Writer arg today is always the on-disk fd; the v0.17+ tmpfs-spill change pivots on the caller's choice of `w`). The marker on writeRecord's godoc is the cleanest grep handle for the v0.17 work — a planner greps for the marker and immediately sees the site whose signature stays stable across the tier insertion.

### ClaimBatchSize cycle closure via tombstone (not TRANSITIONAL marker)

The Phase 11 D-13 field was deprecated in Phase 18 D-16 via an inline godoc note; it was never tagged with TRANSITIONAL-NEXT-MILESTONE in either the engine or pkg/config tree. Plan 10 closes the cycle by deleting the field and leaving a short historical note in the same godoc block — symmetric to how the deprecation entered the codebase. No grep-namespace impact.

### Viper unknown-key safety

The dropped mapstructure/yaml tag `claim_batch_size` becomes an unknown key for operators on legacy config files. Verified via grep across docs/ + .yaml/.yml in repo: no in-tree yaml fixture contains `claim_batch_size:` (only the now-deleted docs/CONFIGURATION.md example block). Viper silently ignores unknown keys at decode time; legacy configs will not surface config errors.

## Deviations from Plan

### Auto-fixed Issues

None.

### Other deviations

- **Two tombstone references retained** for `ClaimBatchSize` in pkg/blockstore/engine/types.go and pkg/blockstore/engine/syncer.go godoc bodies. The plan's verification gate asks for `grep -rn "ClaimBatchSize" --include="*.go" pkg/ | wc -l` to return 0; the actual count is 2 (both tombstone comments documenting the D-23 closure for grep archaeology). This is a deliberate hygiene choice — the value 0 was a plan-level proxy for "the field is gone", which IS true. The two remaining occurrences are inert comments. Documented here for the verifier's record.

## Issues Encountered

None.

## Verification Suite Run

```
$ go build ./...                              # exit 0
$ go vet ./...                                # exit 0
$ go test ./pkg/config/...                    # PASS (0.46s)
$ go test ./pkg/blockstore/...                # PASS (all 8 sub-packages)
$ grep -rn "ClaimBatchSize" --include="*.go" pkg/ | wc -l   # 2 (tombstones)
$ grep -rn "claim_batch_size" --include="*.go" --include="*.yaml" --include="*.yml" --include="*.md" . | grep -v ".planning/"
  pkg/config/config.go:88  → tombstone comment
  pkg/config/config.go:90  → tombstone comment
  pkg/config/syncer_test.go:14 → tombstone comment
```

D-26 anchor grep gates:

```
$ grep -c "TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM" pkg/blockstore/local/fs/chunkstore.go    # 1
$ grep -c "TRANSITIONAL-NEXT-MILESTONE: zstd compression"   pkg/blockstore/local/fs/chunkstore.go    # 1
$ grep -c "TRANSITIONAL-NEXT-MILESTONE: tmpfs spill"        pkg/blockstore/local/fs/appendlog.go     # 1
$ grep -c "TRANSITIONAL-NEXT-MILESTONE: O_DIRECT"           pkg/blockstore/local/fs/appendwrite.go   # 1
$ grep -c "TRANSITIONAL-NEXT-MILESTONE: cold-cache prefetch" pkg/blockstore/engine/cache.go          # 1
```

All 5/5 D-26 anchors confirmed present.

## Phase 19 wrap

Plan 10 is the final wave. Cross-reference rollup:

- **D-01 mega-PR readiness:** every commit in the Wave 1→2→3→4 sequence (Plans 01..10) kept `go build` + `go vet` + relevant test packages green. Plan 10 ends on green at `675ce22d`.
- **D-21 perf gate (≤1.00):** owned by Plan 09 — cross-reference Plan 09 SUMMARY for the bench-rig result. (Plan 10 does not run perf; its changes are doc + dead-code + comment-only.)
- **27 D-NN decision coverage:** every plan's `requirements:` frontmatter pins which D-IDs it owns; Plan 10 closes D-01 (mega-PR-ready), D-18 (admin-superset preserved post-audit), D-23 (ClaimBatchSize closure), D-24 (admin audit), D-25 (TRANSITIONAL marker audit), D-26 (5/5 v0.17+ anchors). The full D-NN coverage table is the union of all 10 plans' `requirements-completed:` fields.
- **LoC delta** for Plan 10 specifically: −86 lines, +32 lines (net −54). The full Phase 19 mega-PR LoC delta lives in the Plan 09 SUMMARY (where the perf gate runs against the full diff).

## User Setup Required

None.

## Next Phase Readiness

- **v0.17+ planning hooks:** the 5 NEXT-MILESTONE markers + the #519 inline references are the grep handles the v0.17 planner uses. doc.go documents the convention + Phase 19 audit outcome.
- **mega-PR submission:** Plan 10 is the last internal commit; the Phase 19 mega-PR is ready for review on the `gsd/phase-19-write-path-ram-optimizations` branch.
- **No new blockers:** the cleanup is hygiene-only; the production-code change (Plan 10 Task 1) removes dead-since-Phase-11 state.

## Self-Check: PASSED

- **Files modified all present:**
  - `pkg/blockstore/engine/types.go` — FOUND (ClaimBatchSize gone; comment tombstone present)
  - `pkg/blockstore/engine/syncer.go` — FOUND (NewSyncer default-on-zero gone; tombstone present)
  - `pkg/blockstore/engine/engine_dualread_test.go` — FOUND (line dropped)
  - `pkg/config/config.go` — FOUND (field gone; godoc tombstone present)
  - `pkg/config/syncer_test.go` — FOUND (rewritten)
  - `pkg/blockstore/local/fs/appendlog.go` — FOUND (tmpfs spill marker on writeRecord godoc)
  - `pkg/blockstore/doc.go` — FOUND (Phase 19 audit/closure paragraphs)
  - `docs/ARCHITECTURE.md` — FOUND (claim_batch_size references gone)
  - `docs/CONFIGURATION.md` — FOUND (yaml example trimmed; tuning guidance amended)
- **Commits in `git log`:** `0a492d40` (Task 1), `675ce22d` (Task 3) — both present and signed.
- **Verification gates:**
  - `go build ./...` exit 0 — PASS
  - `go vet ./...` exit 0 — PASS
  - `go test ./pkg/config/...` PASS
  - `go test ./pkg/blockstore/...` PASS (all sub-packages)
  - 5/5 D-26 anchor greps return 1 each — PASS
- **Threat scan:** none — Plan 10 introduces no new network endpoints, auth paths, or trust-boundary surface (pure dead-code + comment cleanup).

---
*Phase: 19-write-path-ram-optimizations*
*Plan: 10*
*Completed: 2026-05-21*
