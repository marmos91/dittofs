---
phase: 23-snapshot-create-orchestration-sync-gate
plan: 05
subsystem: controlplane/runtime
tags: [snapshot, lifecycle, shutdown, recovery, registry-integration]
requires:
  - .planning/phases/23-snapshot-create-orchestration-sync-gate/23-04-SUMMARY.md
  - .planning/phases/22-snapshot-store-foundation/22-SUMMARY.md
provides:
  - "Runtime.cancelAndWaitInFlightSnaps(shareName) — drain hook for RemoveShare"
  - "Runtime.shutdownSnapshots(ctx) — first step of Runtime.Shutdown"
  - "Runtime.recoverOrphanedSnapshots(ctx) — startup reconcile of state=creating rows"
  - "Runtime.Shutdown(ctx) error — composed lifecycle entrypoint (snapshot drain → adapters → metadata stores)"
  - "Runtime.RemoveShare now drains snap goroutines BEFORE delegating to sharesSvc.RemoveShare"
  - "Runtime.Serve now invokes recoverOrphanedSnapshots before lifecycleSvc.Serve"
affects:
  - "Phase 23 plan 23-06 (integration test: RemoveShare-cancels-in-flight + startup-recovery-after-crash)"
  - "cmd/dfs shutdown sequence (callers should migrate to Runtime.Shutdown(ctx))"
tech_stack:
  added: []
  patterns:
    - "Composed lifecycle entrypoint over piecewise helpers (StopAllAdapters + CloseMetadataStores kept public for tests)"
    - "Snapshot-drain-first ordering enforced by Shutdown body; documented as load-bearing in godoc"
    - "Structured-log recovery marker (slog.Warn reason=abandoned_at_startup) — no schema column"
    - "Lock-protected registry: snapInFlightMu held only to snapshot entries map, released before wg.Wait"
key_files:
  created: []
  modified:
    - pkg/controlplane/runtime/snapshot.go
    - pkg/controlplane/runtime/runtime.go
decisions:
  - "D-23-17 honored: RemoveShare cancels+waits BEFORE delegating; Runtime.Shutdown orders snap drain → adapters → meta stores"
  - "D-23-18 honored: Serve-time recovery scan flips state=creating rows to state=failed before adapters start serving"
  - "D-23-09 preserved: failed-state rows retain metadata.dump + manifest.hashes on disk for retry"
  - "D-23-16 honored: slog Info on cancel/drain/recovery, slog.Warn on recovery flip, slog.Error on per-share scan failures"
  - "Planner discretion: structured log over schema column for recovery marker — keeps plan additive at the store layer"
  - "shares.Service.RemoveShare body UNCHANGED — integration sits at *Runtime per PATTERNS.md option (2)"
  - "Phase 22 invariant preserved: snapshot DB rows are NOT cascade-deleted by RemoveShare; orphan rows are harmless after on-disk wipe (hold filter D-23-02 returns false)"
metrics:
  duration: ~30 minutes (continuation/rescue agent: task 3 only ~10 min)
  completed: 2026-05-28
  tasks: 3/3
  commits: 3
---

# Phase 23 Plan 05: Snapshot lifecycle integration + Runtime.Shutdown + startup recovery Summary

Wired the snap-in-flight registry from plan 23-04 into Runtime's lifecycle so RemoveShare drains in-flight snapshots before the Phase 22 D-15 tree wipe, Shutdown drains snapshots before adapters and metadata stores close, and Serve reconciles state=creating rows abandoned by a prior crash before adapters start serving.

## Methods landed

### `pkg/controlplane/runtime/snapshot.go` (3 new + 1 from prior task)

| Method                                              | Role                                                                                                  | Commit     |
| --------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | ---------- |
| `Runtime.cancelAndWaitInFlightSnaps(shareName)`     | Cancels every in-flight snap goroutine for the share, waits for drain. Called from `RemoveShare`.     | `03fc5be7` |
| `Runtime.shutdownSnapshots(ctx)`                    | Cancels `runtimeCtx`, drains all per-share entries, races against `ctx.Done()`.                       | `03fc5be7` |
| `Runtime.recoverOrphanedSnapshots(ctx)`             | Scans all shares × snapshots, flips `state=creating` → `state=failed`, emits `slog.Warn` marker.      | `47978f4c` |

### `pkg/controlplane/runtime/runtime.go` (1 new + 2 modifications)

| Change                                              | Role                                                                                                                | Commit     |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- | ---------- |
| `Runtime.RemoveShare` calls `cancelAndWaitInFlightSnaps(name)` BEFORE `sharesSvc.RemoveShare(name)` | D-23-17 drain-before-wipe ordering.                          | `82e47124` |
| New `Runtime.Shutdown(ctx context.Context) error`   | Composed lifecycle entrypoint: `shutdownSnapshots → StopAllAdapters → CloseMetadataStores`. Idempotent.             | `47978f4c` |
| `Runtime.Serve` invokes `recoverOrphanedSnapshots(r.runtimeCtx)` before delegating to `lifecycleSvc.Serve` | D-23-18 startup reconcile after metadata-store registration, before adapters serve.            | `47978f4c` |

## Shutdown ordering rationale (load-bearing)

The order documented in `Runtime.Shutdown` godoc:

1. **`shutdownSnapshots(ctx)` first** — snapshot orchestration goroutines call into `metadata.Backupable.Backup` (on metadata stores) and `r.store.UpdateSnapshotState` (on the control-plane DB). If metadata stores or the control plane were torn down first, every in-flight snap goroutine would panic on use-after-close. Cancelling `runtimeCtx` propagates to every child ctx derived in `registerSnapInFlight`; goroutines notice at their next ctx-aware call. `failSnap` uses `context.Background`, so the final `state=failed` flip still completes even with `runtimeCtx` cancelled.
2. **`StopAllAdapters()` second** — adapters refuse new RPCs. In-flight RPCs fail naturally because no waiter is left to receive their reply.
3. **`CloseMetadataStores()` last** — now safe; nothing holds open references.

If `ctx` fires before snap drain completes, `shutdownSnapshots` logs a warning and returns; the rest of the sequence proceeds because `runtimeCancel` has already fired and orphan goroutines will exit on their own. Callers wanting a hard deadline pass `context.WithTimeout(...)`; callers passing `context.Background` block until full snapshot drain.

The composed `Shutdown(ctx)` is the dedicated entrypoint going forward, but the piecewise helpers (`StopAllAdapters`, `CloseMetadataStores`) remain public so tests can drive individual steps. Per CLAUDE.md "less is more / no compat shims," no deprecation flag was added.

## Integration call sites

- **`Runtime.RemoveShare`** (pkg/controlplane/runtime/runtime.go) — drain-first ordering: `r.cancelAndWaitInFlightSnaps(name)` then `r.sharesSvc.RemoveShare(name)`. The cancelled snap goroutine flips its own row to `state=failed` per D-23-09; that orphan row is harmless because the manifest-on-disk filter (D-23-02) returns false once the per-share `snapshots/` tree is wiped (Phase 22 D-15 inside `sharesSvc.RemoveShare`).
- **`Runtime.Shutdown`** (pkg/controlplane/runtime/runtime.go) — calls `r.shutdownSnapshots(ctx)` first; `StopAllAdapters` errors are logged-and-continue so meta-store close always runs.
- **`Runtime.Serve`** (pkg/controlplane/runtime/runtime.go:447) — calls `r.recoverOrphanedSnapshots(r.runtimeCtx)` after `clientRegistry.StartSweeper` and before `lifecycleSvc.Serve`. By that point, metadata stores and shares were already registered by the `cmd/dfs` boot sequence and adapters have not yet been loaded from the store, so the recovery scan completes before any external client can issue a new `CreateSnapshot` RPC.

## Recovery marker strategy: slog vs schema column

Per the planner-discretion note in CONTEXT D-23-18 and the iteration-1 revision applied to plan 23-04's orchestration-failure path, **the recovery marker is structured-log only** — no `Error` column added to `models.Snapshot`. Each recovered row emits:

```text
WARN snapshot recovery: abandoned creating snapshot flipped to failed
    snapshot_id=<id> share=<name> reason=abandoned_at_startup
```

This keeps the plan additive at the store layer (no migration, no schema bump) while still giving operators a grep-able post-crash signal that distinguishes pre-restart failures from in-run failures. The operator can retry via `CreateSnapshot(opts.RetryOf=...)` because D-23-09 preserves the on-disk `metadata.dump` and `manifest.hashes`, and the hold filter (D-23-02) continues to protect their blocks.

The cross-share scan iterates `r.sharesSvc.ListShares()` × `r.store.ListSnapshots(ctx, shareName)` rather than adding a `ListSnapshotsByState` method — PATTERNS.md confirmed this simpler option avoids new store-interface surface while remaining O(shares × snapshots-per-share), which is bounded by operator intent.

## Phase 22 invariant preserved

`shares.Service.RemoveShare` body remains untouched (PATTERNS.md option (2)). The integration sits at `*Runtime` level only. Per Phase 22 `shares/service.go:776` (`DB row is the source of truth`), snapshot DB rows are not cascade-deleted by `RemoveShare`. After integration:

- Cancelled goroutine flips its row to `state=failed` (D-23-09) before the Phase 22 D-15 wipe runs.
- The on-disk `snapshots/` tree is wiped by the Phase 22 D-15 hook inside `sharesSvc.RemoveShare`.
- The orphan `state=failed` row in the DB is harmless: the manifest-on-disk filter (D-23-02) returns false because the FS tree is gone, so GC will not be blocked.

## Threat model coverage

| Threat ID       | Mitigation in this plan                                                                                                                                          |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| T-23-05-RACE    | `cancelAndWaitInFlightSnaps` runs BEFORE `sharesSvc.RemoveShare` — to be verified by plan 23-06 sub-test 6.                                                      |
| T-23-05-USEAFTER| `Runtime.Shutdown` ordering: `shutdownSnapshots → StopAllAdapters → CloseMetadataStores`; godoc documents load-bearing reason.                                   |
| T-23-05-DOS     | `shutdownSnapshots` races `ctx.Done()`; caller controls deadline.                                                                                                |
| T-23-05-RECOVERY| `recoverOrphanedSnapshots` flips orphan `state=creating` rows before adapters serve, freeing the Phase 22 D-08 partial unique index slot for new CreateSnapshot. |

## Verification

```text
$ go build ./...                                              # OK
$ go vet ./pkg/controlplane/runtime/...                       # OK
$ gofmt -s -l pkg/controlplane/runtime/                       # (empty)
$ go test ./pkg/controlplane/runtime/... -race -count=1 -short
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime              4.847s
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/blockstoreprobe 1.423s
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/clients      2.000s
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/shares       3.254s
ok  github.com/marmos91/dittofs/pkg/controlplane/runtime/stores       2.724s
```

Symbol presence (per plan verification block):

- `func (r *Runtime) cancelAndWaitInFlightSnaps` → snapshot.go:449
- `func (r *Runtime) shutdownSnapshots`          → snapshot.go:503
- `func (r *Runtime) recoverOrphanedSnapshots`   → snapshot.go:568
- `func (r *Runtime) Shutdown(ctx`               → runtime.go:171
- Serve-time invocation `recoverOrphanedSnapshots(r.runtimeCtx)` → runtime.go:455
- Shutdown body composes `shutdownSnapshots` + `StopAllAdapters` + `CloseMetadataStores` in order.

## Deviations from Plan

None — plan executed exactly as written. Tasks 1 + 2 were completed by a prior agent (commits `03fc5be7` + `82e47124`); this continuation agent executed only Task 3 (commit `47978f4c`) and wrote this SUMMARY.

## Commits

| Hash       | Message                                                                  | Task |
| ---------- | ------------------------------------------------------------------------ | ---- |
| `03fc5be7` | `feat(23-05): add cancelAndWaitInFlightSnaps + shutdownSnapshots`        | 1    |
| `82e47124` | `feat(23-05): wire RemoveShare to drain in-flight snapshots`             | 2    |
| `47978f4c` | `feat(23-05): recoverOrphanedSnapshots + Runtime.Shutdown + Serve wiring`| 3    |

## Self-Check: PASSED

- `pkg/controlplane/runtime/snapshot.go` contains all three new methods (cancelAndWaitInFlightSnaps, shutdownSnapshots, recoverOrphanedSnapshots): FOUND
- `pkg/controlplane/runtime/runtime.go` contains `Runtime.Shutdown` + Serve-time `recoverOrphanedSnapshots` invocation + `RemoveShare` drain-before-delegate: FOUND
- Commits `03fc5be7`, `82e47124`, `47978f4c` present in `git log`: FOUND
- `go build ./...` exits 0: PASS
- `go vet ./pkg/controlplane/runtime/...` exits 0: PASS
- `gofmt -s -l pkg/controlplane/runtime/` empty: PASS
- `go test ./pkg/controlplane/runtime/... -race -count=1 -short`: PASS
