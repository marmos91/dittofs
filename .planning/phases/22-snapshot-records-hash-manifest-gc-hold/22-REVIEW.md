# Phase 22 — Code Review

**Branch:** `gsd/phase-22-snapshot-records-hash-manifest-gc-hold` vs `develop`
**Date:** 2026-05-28
**Reviewer:** feature-dev:code-reviewer agent + orchestrator triage

## Summary

5 findings: 0 BLOCKING, 2 HIGH, 2 MEDIUM, 1 LOW. 4 fixed in-branch; 1 (H-2) deferred with rationale.

## Findings

### H-1 — GSD artifact reference in package doc — FIXED

**File:** `pkg/snapshot/doc.go:22-23`

Comment referenced `22-CONTEXT` (a GSD planning artifact name). CLAUDE.md rule: phase IDs stay in `.planning/`, never in godoc / comments / test names.

**Fix:** Replaced with canonical-path self-description pointing to `models.Snapshot.ManifestPath`.

### H-2 — `streamManifest` materializes full HashSet before forwarding — DEFERRED

**File:** `pkg/controlplane/runtime/snapshot_hold.go:streamManifest`

`ReadManifest` accumulates every parsed line into an in-memory `HashSet` before `ForEach` forwards to the engine callback. For a share with N blocks, this is a ~32×N byte allocation per ready snapshot per share per GC pass.

**Why deferred:**
- Required change: new `WalkManifest(io.Reader, fn)` function in `pkg/snapshot` that yields per-line, plus refactor of `streamManifest` to use it.
- Magnitude: bounded by 32×N bytes (hashes, not block bytes). For typical share sizes (≤1M blocks ≈ 32MB) it is acceptable.
- The data-plane streaming rule explicitly targets codecs over **block bytes**; hashes are metadata.
- The simplifier pass observed the same and judged it acceptable.
- Filed as follow-up — implement when first share exceeds ~5M blocks or when profiling shows GC heap spike.

### M-1 — `UpdateSnapshotState` accepts only `id`, not `(shareName, id)` — FIXED

**File:** `pkg/controlplane/store/{interface.go, snapshots.go}` + 4 test call sites.

Other snapshot ops scope to `(shareName, id)`. Update accepted only `id`. Not a correctness bug today (UUIDs), but the future REST handler will rely on store-level boundary for tenant safety.

**Fix:** Signature now `UpdateSnapshotState(ctx, shareName, id, state)`. WHERE clause filters both columns. All 7 call sites in `snapshot_hold_test.go`, `snapshot_lifecycle_test.go`, `snapshots_test.go` updated.

### M-2 — `HeldHashes` fail-closed on `ErrShareNotFound` — FIXED

**File:** `pkg/controlplane/runtime/snapshot_hold.go:HeldHashes`

A concurrent `RemoveShare` between GC entry and `LocalStoreDir` lookup made GC abort with a cryptic error.

**Fix:** Wrap with `errors.Is(err, shares.ErrShareNotFound)` continue. A just-removed share has no in-flight writes, so contributing no held hashes is safe.

### L-1 — Manifest tempfile mode `0o644` — FIXED

**File:** `pkg/snapshot/manifest.go:52`

World-readable manifests leak share shape (hash count, size distribution) to unprivileged OS users.

**Fix:** `0o644` → `0o600`.

## Non-issues (explicitly vetted)

- GC concurrency: `markPhase` invokes `HeldHashes` AFTER all `EnumerateFileBlocks` and BEFORE `FlushAdd` — sweep cannot see partial held set.
- `CreateSnapshot` unique-constraint short-circuit only triggers when `snap.State == StateCreating` — the only constrained shape.
- Idx fallback runs at store init before listener accepts traffic — no client race.
- Path traversal in `<shareDataDir>/snapshots/<id>/manifest.hashes` — `id` is UUID (hex+hyphen); `filepath.Join` throughout.
- `streamManifest` file descriptor leak — `defer f.Close()` per call; loop returns between manifests.

## Verification

All fixes verified with:
- `gofmt -s -w pkg/`
- `go vet ./...` — clean
- `go build ./...` — clean
- `go test ./pkg/snapshot/... ./pkg/controlplane/... ./pkg/blockstore/engine/... -count=1 -race` — all PASS
