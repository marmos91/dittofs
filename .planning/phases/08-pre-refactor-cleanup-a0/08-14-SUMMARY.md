---
phase: 08-pre-refactor-cleanup-a0
plan: 14
subsystem: blockstore
tags: [td-03, cow, backup, scaffolding-removal, engine, metadata]

requires:
  - phase: 08-13
    provides: "engine/ package with merged readbuffer/sync/gc (TD-01)"
provides:
  - "COW/backup scaffolding completely deleted from engine/ + metadata/ + adapter/ layers"
  - "BlockStore.Reader interface narrowed (ReadAtWithCOWSource removed)"
  - "FileAttr slimmed (ObjectID, Blocks []string, COWSourcePayloadID removed)"
  - "WriteOperation + PendingWriteState slimmed (IsCOW, COWSourcePayloadID, OldObjectID removed)"
  - "A3/A4 reintroduction breadcrumb in REQUIREMENTS.md (D-05)"
affects: [A1, A2, A3, A4, ADAPT]

tech-stack:
  added: []
  patterns:
    - "Rely on encoding/json tolerance for on-disk compat (D-07) — no schema migration"
    - "ContentHash type retention for BSCAS-06/A2 groundwork (W11)"

key-files:
  created:
    - .planning/phases/08-pre-refactor-cleanup-a0/08-14-SUMMARY.md
  modified:
    - pkg/blockstore/store.go
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/engine/syncer.go
    - pkg/blockstore/engine/gc.go
    - pkg/blockstore/engine/gc_test.go
    - pkg/metadata/file_types.go
    - pkg/metadata/pending_writes.go
    - pkg/metadata/io.go
    - internal/adapter/nfs/v3/handlers/read.go
    - internal/adapter/nfs/v3/handlers/read_payload.go
    - internal/adapter/nfs/v4/handlers/read.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/ioctl_copychunk.go
    - .planning/REQUIREMENTS.md

key-decisions:
  - "Single atomic commit — coordinated multi-file deletion kept together for bisect clarity"
  - "Adapter callers purged as part of TD-03 (safe deletion order: callers first, then interface + fields + helpers)"
  - "ContentHash type preserved — used by FindFileBlockByHash + backend objects indexes + BSCAS-06/A2"
  - "pkg/blockstore/local/localtest/ had ZERO COW references — Blocker 6 sub-step was a no-op"

patterns-established:
  - "Fix callers first, then types: avoids mid-deletion compiler breakage"
  - "Breadcrumb-in-REQUIREMENTS pattern for A3/A4 reintroduction signal (D-05)"

requirements-completed: [TD-03]

duration: ~15min
completed: 2026-04-23
---

# Phase 08 Plan 14: TD-03 Dead Scaffolding Removal Summary

**Deleted all COW/backup scaffolding (ReadAtWithCOWSource, readFromCOWSource, FinalizationCallback, BackupHoldProvider, StaticBackupHold, FileAttr.ObjectID/Blocks/COWSourcePayloadID, WriteOperation IsCOW/OldObjectID, applyCOWState) across engine/, metadata/, and NFS/SMB adapters in a single atomic signed commit; added A3/A4 reintroduction breadcrumb to REQUIREMENTS.md.**

## Performance

- **Duration:** ~15 min
- **Started:** 2026-04-23T20:50:00Z
- **Completed:** 2026-04-23T20:58:20Z
- **Tasks:** 1 (single atomic deletion per plan guidance)
- **Files modified:** 14

## Accomplishments

- **BlockStore interface narrowed:** `ReadAtWithCOWSource` removed from `pkg/blockstore/store.go` Reader interface — compiler enforced removal across all implementors.
- **Engine COW deletion:** `ReadAtWithCOWSource` + `readFromCOWSource` helpers + COW branch in `readAtInternal` (engine.go) deleted. `readAtInternal` signature simplified to 4 params (ctx, payloadID, data, offset).
- **Syncer finalization deletion:** `FinalizationCallback` type + `onFinalized` field + `SetFinalizationCallback` setter deleted from `pkg/blockstore/engine/syncer.go`.
- **GC hold deletion:** `BackupHoldProvider` interface + `StaticBackupHold` helper + `staticHold` type + `Options.BackupHold` field + `heldSet` consultation block in `CollectGarbage` deleted from `pkg/blockstore/engine/gc.go`. 4 `TestGC_BackupHold_*` tests + `fakeBackupHold` fake removed from `gc_test.go`.
- **Metadata type slim-down:** `FileAttr.ObjectID`, `FileAttr.Blocks []string`, `FileAttr.COWSourcePayloadID` deleted from `pkg/metadata/file_types.go`. `PendingWriteState.IsCOW`, `PendingWriteState.COWSourcePayloadID` deleted from `pkg/metadata/pending_writes.go`. Matching field assignments in `RecordWrite` cleaned up.
- **io.go COW surgery (W10):** `WriteOperation.IsCOW` + `COWSourcePayloadID` + `OldObjectID` fields deleted; `needsCOW` detection branch in `PrepareWrite` deleted; `applyCOWState` helper deleted; all `IsCOW`/`applyCOWState` call sites in `deferredCommitWrite` + `immediateCommitWrite` + `flushPendingWrite` + the `hasWriteData` predicate in `FlushPendingWriteForFile` cleaned up.
- **Adapter COW call-site purge:** `ReadAtWithCOWSource` calls + `file.COWSourcePayloadID` reads removed from 4 handler paths (NFSv3 `read.go` + `read_payload.go`, NFSv4 `read.go`, SMB2 `read.go` + `ioctl_copychunk.go`). `readFromBlockStore` helper signature in `pkg/adapter/nfs/v3/handlers/read_payload.go` lost the `cowSource metadata.PayloadID` parameter.
- **REQUIREMENTS.md breadcrumb (D-05):** A3 (Phase 12, META-01, `[]BlockRef`) and A4 (Phase 13, META-02, BLAKE3 Merkle root) reintroduction note added next to TD-03 line. ContentHash retention note included per W11.

## Task Commits

1. **Task 1: TD-03 remnants — coordinated multi-file deletion** — `a3b0f42f` (refactor)

_Single atomic commit as recommended by the plan for cross-file dependency clarity. Adapter callers + interface removal + type-field removal + dead-branch cleanup all landed together so every intermediate state was non-compilable — the committed state is the first compilable state after deletion._

## Files Created/Modified

- `pkg/blockstore/store.go` — Reader interface without `ReadAtWithCOWSource`.
- `pkg/blockstore/engine/engine.go` — `ReadAtWithCOWSource` + `readFromCOWSource` helpers deleted; `readAtInternal` simplified (no `cowSource` param, no COW fallback branch).
- `pkg/blockstore/engine/syncer.go` — `FinalizationCallback` type + `onFinalized` field + setter deleted.
- `pkg/blockstore/engine/gc.go` — `BackupHoldProvider` + `StaticBackupHold` + `Options.BackupHold` + `heldSet` consultation deleted.
- `pkg/blockstore/engine/gc_test.go` — 4 BackupHold tests + `fakeBackupHold` fake removed.
- `pkg/metadata/file_types.go` — `FileAttr` no longer has `ObjectID`, `Blocks []string`, `COWSourcePayloadID`.
- `pkg/metadata/pending_writes.go` — `PendingWriteState` slimmed (no `IsCOW`, no `COWSourcePayloadID`); `RecordWrite` no longer propagates these fields.
- `pkg/metadata/io.go` — W10 cleanup: `WriteOperation` slimmed (no `IsCOW`, `COWSourcePayloadID`, `OldObjectID`); `needsCOW` detection + `applyCOWState` helper + all call sites deleted; `hasWriteData` predicate simplified.
- `internal/adapter/nfs/v3/handlers/read.go` — `readFromBlockStore` called without `cowSource` param.
- `internal/adapter/nfs/v3/handlers/read_payload.go` — `readFromBlockStore` signature lost `cowSource`; single `ReadAt` call path.
- `internal/adapter/nfs/v4/handlers/read.go` — single `ReadAt` call path (no COW branch).
- `internal/adapter/smb/v2/handlers/read.go` — single `ReadAt` call path.
- `internal/adapter/smb/v2/handlers/ioctl_copychunk.go` — `srcCOWSource` variable + COW branch deleted.
- `.planning/REQUIREMENTS.md` — TD-03 line gets A3/A4/ContentHash breadcrumb.

## Decisions Made

- **Single atomic commit.** Plan explicitly allowed 2-3 commits if cross-file reliance made it hard; coordinated deletion was cohesive and easy to review in one go. Test suite confirmed no intermediate-state ambiguity.
- **Extended scope to adapter callers.** Plan's enumeration of "callers to delete" only mentioned abstract "places that call ReadAtWithCOWSource". Five concrete adapter call sites were found and cleaned up atomically to keep `go build ./...` green. Documented as part of the step-2a sub-step, not a deviation.
- **Kept `metadata.ObjectID = ContentHash` type alias in `pkg/metadata/object.go`.** This is a type alias re-export from `pkg/blockstore` — not the field — and serves as groundwork for BSCAS-04/A4 reintroduction per REQUIREMENTS.md META-02. Deletion would conflict with D-05 breadcrumb intent.

## Deviations from Plan

None — plan executed exactly as written. All audit greps clean, all acceptance criteria met.

- **Blocker 6 outcome (localtest path):** `pkg/blockstore/local/localtest/` contains only `doc.go` + `suite.go`; zero COW references. Sub-step was a no-op as the plan anticipated.
- **W10 outcome (io.go dead code):** All 7 enumerated dead-code lines deleted; 5 additional related cleanup sites (applyCOWState helper + 3 call sites + hasWriteData predicate) removed as natural extension of the core deletion.
- **W11 outcome (ContentHash retention):** Confirmed retained via `grep -c "type ContentHash" pkg/blockstore/types.go` → 1. Usage preserved at: `pkg/blockstore/store.go:33` (`FindFileBlockByHash`), `pkg/blockstore/types.go` (definition), `pkg/blockstore/local/local.go:23`, all `pkg/metadata/store/*/objects.go` (backend indexes), `pkg/metadata/object.go:27` (`ObjectID = ContentHash` alias).

## Issues Encountered

None. Test suite (`go test -count=1 -short -race ./...`) green on first attempt after the coordinated edit.

## Verification Results

All acceptance grep counts met:

| Grep | Expected | Actual |
|---|---|---|
| `ReadAtWithCOWSource\|readFromCOWSource` | 0 | 0 |
| `FinalizationCallback` | 0 | 0 |
| `BackupHoldProvider\|StaticBackupHold` | 0 | 0 |
| `COWSourcePayloadID` | 0 | 0 |
| `ObjectID` in `pkg/metadata/file_types.go` | 0 | 0 |
| `Blocks []string` in `pkg/metadata/file_types.go` | 0 | 0 |
| `needsCOW\|OldObjectID\|IsCOW` in `pkg/metadata/io.go` (W10) | 0 | 0 |
| `ObjectID\|COWSourcePayloadID` in `pkg/blockstore/local/localtest/*.go` (Blocker 6) | 0 | 0 |
| `type ContentHash` in `pkg/blockstore/types.go` (W11) | ≥1 | 1 |
| `reintroduce` in `.planning/REQUIREMENTS.md` (D-05) | ≥1 | 1 |

- `go build ./...` → exit 0
- `go vet ./pkg/blockstore/... ./pkg/metadata/...` → exit 0
- `go vet ./...` → exit 0
- `go test -count=1 -short -race ./...` → all ok, zero FAIL
- `git log -1 --show-signature` → Good signature
- `git log -1 --format='%B' | grep -iEq "claude code|co-authored-by"` → no match (hygiene OK)

## Self-Check: PASSED

**Files present on disk:**

- `.planning/phases/08-pre-refactor-cleanup-a0/08-14-SUMMARY.md` — FOUND
- `pkg/blockstore/store.go` — FOUND (modified)
- `pkg/blockstore/engine/engine.go` — FOUND (modified)
- `pkg/blockstore/engine/syncer.go` — FOUND (modified)
- `pkg/blockstore/engine/gc.go` — FOUND (modified)
- `pkg/blockstore/engine/gc_test.go` — FOUND (modified)
- `pkg/metadata/file_types.go` — FOUND (modified)
- `pkg/metadata/pending_writes.go` — FOUND (modified)
- `pkg/metadata/io.go` — FOUND (modified)
- Adapter files (5) — all FOUND (modified)
- `.planning/REQUIREMENTS.md` — FOUND (modified)

**Commit present in git log:**

- `a3b0f42f` `blockstore: remove COW/FinalizationCallback/BackupHoldProvider scaffolding (TD-03)` — FOUND, signed

## Next Phase Readiness

- TD-03 complete. D-31 step 2 done.
- PR-C remaining: block 3 (TD-04 parser collapse) per D-31.
- A1 (Phase 10) can now assume no COW-era scaffolding exists anywhere in engine/ or metadata/.
- A3 (Phase 12) / A4 (Phase 13) reintroduction is signaled via REQUIREMENTS.md breadcrumb (D-05).
- On-disk compat relies on `encoding/json` tolerance (D-07) — stale Badger/Postgres JSON rows with `cow_source`, `object_id`, `blocks` keys silently unmarshal on read; next save rewrites without them. All three fields had `omitempty` so stale rows rarely even carry these keys.

---
*Phase: 08-pre-refactor-cleanup-a0*
*Completed: 2026-04-23*
