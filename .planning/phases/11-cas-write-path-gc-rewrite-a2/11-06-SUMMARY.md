# Plan 11-06 — Mark-Sweep GC + EnumerateFileBlocks Cursor + GCState

**Phase:** 11-cas-write-path-gc-rewrite-a2 (PR-C)
**Requirements:** GC-01, GC-02, GC-03, GC-04, INV-01, INV-04
**Status:** Complete
**Date:** 2026-04-25

## Summary

Replaces the legacy path-prefix GC scan with a fail-closed mark-sweep over the union of `FileAttr.Blocks[*].Hash` across all shares with the same remote-store identity. Adds the `MetadataStore.EnumerateFileBlocks` cursor (D-02) with conformance for memory + badger + postgres backends. Persists the live set on disk under `<localStore>/gc-state/<runID>/` (D-01) so memory is bounded regardless of file count. Sweeps with a bounded worker pool over 256 top prefixes (D-04) honoring snapshot + grace TTL (D-05). Confirms no `BackupHoldProvider` coupling (GC-04).

## Commits (signed)

- `d7c80258` — `feat(11-06): add EnumerateFileBlocks cursor on FileBlockStore`
- `8c3190bd` — `feat(11-06): add GCState disk-backed live set + last-run.json (D-01/D-10)`
- `8d8935c4` — `feat(11-06): mark-sweep CollectGarbage + RemoteStore.ListByPrefixWithMeta + gc.* config`

## Files

### New
- `pkg/blockstore/engine/gcstate.go` — `GCState` exporting `Add(h ContentHash)`, `Has(h)`, `MarkComplete()`, `Close()`, `IsStale(dir)`, plus `CleanStaleGCStateDirs`, `NewGCState`, `PersistLastRunSummary`
- `pkg/blockstore/engine/gcstate_test.go` — unit tests for the on-disk live set + stale-dir cleanup + last-run.json round-trip

### Modified
- `pkg/blockstore/engine/gc.go` — full rewrite from path-prefix scan to mark-sweep
- `pkg/blockstore/engine/gc_test.go` — 10 mark-sweep behavior tests (mark dedup, sweep happy path, grace TTL, fail-closed, sweep continue+capture, dry-run, GC-04 regression guard, last-run.json, stale-dir cleanup, concurrency cap)
- `pkg/blockstore/store.go` — `FileBlockStore.EnumerateFileBlocks(ctx, fn)` cursor + `RemoteStore.ListByPrefixWithMeta(ctx, prefix)` + `Delete(ctx, key)` audit
- `pkg/blockstore/remote/remote.go` — `ObjectInfo{Key,Size,LastModified}`
- `pkg/blockstore/remote/s3/store.go` — `ListByPrefixWithMeta` impl
- `pkg/blockstore/remote/memory/store.go` — `ListByPrefixWithMeta` mirror
- `pkg/metadata/store.go` — re-export contract
- `pkg/metadata/store/{memory,badger,postgres}/objects.go` — `EnumerateFileBlocks` impl per backend
- `pkg/metadata/storetest/file_block_ops.go` — extended conformance for the cursor (4 scenarios: empty store, single file, large fanout, error mid-iteration)
- `pkg/config/config.go`, `pkg/config/defaults.go` — `GCConfig` with `interval/sweep_concurrency/grace_period/dry_run_sample_size`; `ApplyDefaults` + `Validate`
- `pkg/controlplane/runtime/blockgc.go` — construct per-remote `MultiShareReconciler` for D-03 cross-share aggregation
- `pkg/controlplane/runtime/blockgc_test.go` — adapt to mark-sweep

## Verification

- `go vet ./...` — clean
- `go build ./...` — clean
- `go test -short -count=1 ./pkg/blockstore/engine/... ./pkg/metadata/...` — all pass
- `grep -E "^\s+Delete\(ctx" pkg/blockstore/store.go` — non-empty (Warning 2)
- `grep -E "^\s+ListByPrefixWithMeta\(ctx" pkg/blockstore/store.go` — non-empty (Warning 2)
- `grep BackupHoldProvider pkg/blockstore/engine/gc.go` — empty (GC-04 reconfirmed)

## Decisions Made

- **D-01 backing impl**: BadgerDB temp store under `<localStore>/gc-state/<runID>/`. `incomplete.flag` marker; on next run, stale dirs are detected via `CleanStaleGCStateDirs` and deleted before starting fresh.
- **D-04 default concurrency**: 16 (configurable up to 32 via `gc.sweep_concurrency`).
- **D-05 default grace**: 1h (`gc.grace_period`); warn-log if set <5min.
- **D-06 fail-closed**: any mark error aborts the sweep entirely; sweep workers do not start.
- **D-07 sweep error handling**: per-prefix capture; first N samples reported.
- **D-10 observability**: slog INFO at start/end with run_id/hashes_marked/objects_swept/bytes_freed/duration_ms/error_count; `last-run.json` persisted under `gc-state/`.

## Honors CONTEXT.md

- D-01 ✓ disk-backed live set
- D-02 ✓ MetadataStore.EnumerateFileBlocks cursor + storetest conformance
- D-03 ✓ cross-share grouping by remote identity
- D-04 ✓ bounded worker pool default 16
- D-05 ✓ grace TTL 1h default
- D-06 ✓ fail-closed mark phase
- D-07 ✓ sweep continue+capture
- D-10 ✓ slog + last-run.json
- GC-04 ✓ no BackupHoldProvider coupling

## Carries Forward

PR-C consumers:
- Plan 11-07 (dfsctl gc + REST) wires the public surface to `Runtime.RunBlockGC` (already updated here).
- Plan 11-08 (canonical E2E) drives the full lifecycle: write→overwrite→GC and asserts the OLD CAS key is reaped.
