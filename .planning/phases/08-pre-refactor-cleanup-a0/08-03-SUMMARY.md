---
phase: 08-pre-refactor-cleanup-a0
plan: 03
subsystem: blockstore/engine
tags: [bug-fix, TD-02, TD-02c, disk-cleanup, orphan-blocks]
dependency_graph:
  requires:
    - "08-02 (syncFileBlock error propagation) — independent file/function, no code coupling; satisfies the plan's depends_on ordering."
  provides:
    - "engine.BlockStore.Delete now removes on-disk .blk files alongside in-memory state, closing the TD-02c orphan-block leak."
  affects:
    - "Any caller of engine.BlockStore.Delete (runtime payload deletion, share removal, test cleanup). Semantics are strictly additive — the previous in-memory eviction is preserved via DeleteAllBlockFiles, which purges memBlocks as part of its cleanup (see fs/manage.go:111)."
tech_stack:
  added: []
  patterns:
    - "SyncFileBlocksForFile → DeleteAllBlockFiles sequencing to close the queueFileBlockUpdate race: flushBlock queues metadata asynchronously, so DeleteAllBlockFiles must force a sync before enumerating store-tracked blocks."
    - "FSStore-backed engine test helper (newFSTestEngine) + filepath.Walk .blk counter (countBlkFiles) — mirrors the on-disk assertion style in fs/manage_test.go but at the engine layer."
key_files:
  created: []
  modified:
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/engine/engine_test.go
decisions:
  - "Used DeleteAllBlockFiles (not a hand-rolled EvictMemory+DeleteBlockFile loop) — it already encapsulates memory purge + per-block disk delete + files-map cleanup + best-effort parent-dir removal (fs/manage.go:93-126), matching EvictLocal's pattern at engine.go:425-430."
  - "Inserted SyncFileBlocksForFile before DeleteAllBlockFiles so queued FileBlock metadata (populated asynchronously by flushBlock via queueFileBlockUpdate) is persisted to the store first — otherwise ListFileBlocks returns empty for a freshly-flushed payload and the .blk files leak anyway. Discovered during GREEN run: first fix (DeleteAllBlockFiles alone) still failed the regression test with 2 orphan .blk files. See Deviations → Rule 2."
  - "Retained syncer.Delete(ctx, payloadID) tail call so remote cleanup semantics are unchanged."
  - "Test uses real FSStore via fs.New(tmpDir, ...) rather than the memory LocalStore — the memory store has no .blk files so the regression would be untestable against it. MemoryMetadataStore serves as the FileBlockStore so SyncFileBlocksForFile actually has somewhere to persist."
metrics:
  duration: ~5 minutes
  completed: 2026-04-23
requirements: [TD-02]
commits:
  - "d35b4e53 (fix(blockstore): engine.Delete removes on-disk block files (TD-02c))"
---

# Phase 08 Plan 03: engine.Delete disk cleanup (TD-02c) Summary

HIGH-severity TD-02c bug fix: `engine.BlockStore.Delete` previously called `local.EvictMemory` — which per its own doc explicitly does NOT delete `.blk` files from disk — leaving orphan block files that grow unbounded across delete-and-recreate workloads. Replace with `SyncFileBlocksForFile` + `DeleteAllBlockFiles` so memory, disk, and metadata all get cleaned. Regression test locks the contract at the engine layer.

## What changed

### `pkg/blockstore/engine/engine.go`

`BlockStore.Delete` before:

```go
func (bs *BlockStore) Delete(ctx context.Context, payloadID string) error {
    if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
        return fmt.Errorf("local evict memory failed: %w", err)
    }
    bs.readBuffer.InvalidateAndReset(payloadID)
    return bs.syncer.Delete(ctx, payloadID)
}
```

`EvictMemory` (fs/fs.go:424-437) is documented as: *"Does not delete .blk files from disk — that is handled by eviction or explicit deletion via DeleteAllBlockFiles."* Delete called it anyway, so every call to Delete left every flushed block on disk permanently.

`BlockStore.Delete` after:

```go
func (bs *BlockStore) Delete(ctx context.Context, payloadID string) error {
    bs.local.SyncFileBlocksForFile(ctx, payloadID)
    if err := bs.local.DeleteAllBlockFiles(ctx, payloadID); err != nil {
        return fmt.Errorf("local delete all block files failed: %w", err)
    }
    bs.readBuffer.InvalidateAndReset(payloadID)
    return bs.syncer.Delete(ctx, payloadID)
}
```

`DeleteAllBlockFiles` (fs/manage.go:93-126) handles all three tiers — disk via per-block `DeleteBlockFile`, memory via `purgeMemBlocks`, and `files` map cleanup plus best-effort parent-dir removal. `SyncFileBlocksForFile` (fs/fs.go:251-263) flushes any pending FileBlock metadata (queued asynchronously by `flushBlock` via `queueFileBlockUpdate`) so `DeleteAllBlockFiles`'s `ListFileBlocks` enumeration actually sees the blocks — otherwise recently-flushed but not-yet-synced blocks would be missed.

Read-buffer invalidation and remote cleanup via `syncer.Delete` are unchanged.

### `pkg/blockstore/engine/engine_test.go`

Added:

- `newFSTestEngine(t)` — constructs an `engine.BlockStore` backed by an on-disk `fs.FSStore` under `t.TempDir()`, with a `metadatamemory.MemoryMetadataStore` as `FileBlockStore` and a local-only syncer (no remote). Returns `(*BlockStore, tmpDir)` so tests can observe `.blk` files on disk.
- `countBlkFiles(t, dir)` — `filepath.Walk` helper counting `.blk` files under a directory.
- `TestEngineDelete_RemovesBlockFiles` — writes `BlockSize + 4KiB` into a payload (forces 2 blocks), calls `Flush`, asserts `≥1` `.blk` file exists, calls `Delete`, asserts `0` `.blk` files remain. On pre-fix code the test reports: `expected 0 .blk files after Delete, got 2 (TD-02c regression)`.

New imports added to the file: `os`, `path/filepath`, `pkg/blockstore/local/fs`, `pkg/metadata/store/memory`.

## Verification

| Gate | Command | Result |
|------|---------|--------|
| RED (pre-fix) | `go test -race -run TestEngineDelete_RemovesBlockFiles ./pkg/blockstore/engine/...` | FAIL — `expected 0 .blk files after Delete, got 2` |
| GREEN (first attempt: DeleteAllBlockFiles alone) | same | FAIL — still `got 2`; root cause was queued FileBlock metadata not yet synced |
| GREEN (final: SyncFileBlocksForFile + DeleteAllBlockFiles) | same | PASS (1.5s) |
| Engine package | `go test -race ./pkg/blockstore/engine/...` | PASS (2.1s) |
| FS package | `go test -race ./pkg/blockstore/local/fs/...` | PASS (6.9s) |
| Whole-repo build | `go build ./...` | PASS |
| Lint | `go vet ./pkg/blockstore/...` | PASS (no output) |
| Format | `gofmt -l pkg/blockstore/engine/*.go` | clean (no diffs) |
| Signed commit | `git log -1 --show-signature` | Good RSA signature for m.marmos@gmail.com |
| Commit convention | `git log -1 --format=%s` | `fix(blockstore): engine.Delete removes on-disk block files (TD-02c)` |
| No AI mentions | grep `claude code\|co-authored-by` (case-insensitive) | none |
| Acceptance #1 | `grep -n "DeleteAllBlockFiles" pkg/blockstore/engine/engine.go \| wc -l` | `4` (≥ 1 ✓) |
| Acceptance #2 | `grep -n "TestEngineDelete_RemovesBlockFiles" pkg/blockstore/engine/engine_test.go` | 2 matches (comment + func def) |
| Acceptance #3 | `go test -race ./pkg/blockstore/engine/...` exits 0 | PASS |
| Acceptance #4 | `go build ./...` exits 0 | PASS |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 — Missing critical functionality] SyncFileBlocksForFile added before DeleteAllBlockFiles**

- **Found during:** GREEN step, first test run after the initial one-line `EvictMemory` → `DeleteAllBlockFiles` swap.
- **Issue:** `DeleteAllBlockFiles` enumerates blocks via `bc.blockStore.ListFileBlocks(ctx, payloadID)` (fs/manage.go:95). That list only contains blocks whose metadata has been persisted by `SyncFileBlocks` / `SyncFileBlocksForFile`. In a typical Write → Flush → Delete sequence, `flushBlock` writes the `.blk` file synchronously but only *queues* the FileBlock metadata via `queueFileBlockUpdate` (fs/flush.go:154) — the actual `PutFileBlock` runs asynchronously via the periodic `SyncFileBlocks` goroutine (fs/fs.go:219-232). Without an explicit flush in `Delete`, `ListFileBlocks` returns empty for freshly-flushed payloads and the `.blk` files leak anyway. Test failed with the same symptom (`got 2`) as pre-fix.
- **Fix:** Prepended `bs.local.SyncFileBlocksForFile(ctx, payloadID)` — the per-file variant — so only the target payload's pending metadata is flushed (not every in-flight file). No error return; matches the existing signature.
- **Files modified:** `pkg/blockstore/engine/engine.go` (one extra line inside `Delete`).
- **Commit:** `d35b4e53` (folded into the single atomic TD-02c commit — no separate commit).

**2. [Rule 1 — Correctness polish] Method doc expanded to cite TD-02c and the sync-before-delete rationale**

- **Found during:** GREEN step 3, after the fix worked.
- **Rationale:** The original doc "Invalidates all read buffer entries for the file and resets prefetcher state" said nothing about the disk tier. Added a paragraph explaining the TD-02c reason for `DeleteAllBlockFiles` and a paragraph explaining the ordering constraint. Reduces surprise for future readers; no behaviour change.
- **Files modified:** `pkg/blockstore/engine/engine.go` (comment only).
- **Commit:** `d35b4e53`.

### Deviations from `<action>` step text

None material. The plan suggested calling `e.local.DeleteAllBlockFiles` "after the metadata-store delete (or in the correct ordering consistent with CLAUDE.md invariant #5 for deletions)". CLAUDE.md invariant #5 governs the *write* coordination order (metadata first, then blockstore). Delete in this codebase is block-store-local — the metadata-store's `DeleteFile` lives in a separate code path (not in `BlockStore.Delete`) and already runs elsewhere in the runtime's delete sequence. `engine.BlockStore.Delete` only owns block-tier cleanup. Order chosen: sync-metadata → delete-local → invalidate-buffer → delete-remote. Symmetric with `EvictLocal` (engine.go:425-430, which uses `EvictMemory` + `DeleteAllBlockFiles` — now the pattern converges with `Delete`).

## Known Stubs

None.

## Threat Flags

None new. The plan's threat register lists T-08-03-01 (DoS: orphan `.blk` files fill disk) as `mitigate` — this commit is the mitigation. T-08-03-02 (Information disclosure of stale blocks) was `accept`ed; the fix also reduces residual exposure by cleaning up promptly. No new surface introduced.

## Deferred Issues

None within this task's scope. One adjacent observation (logged for awareness, not fix):

- `EvictLocal` (engine.go:425-430) still duplicates the `EvictMemory + DeleteAllBlockFiles` pattern that `Delete` now effectively inlines via `DeleteAllBlockFiles` alone (which does the memory purge). Converging `EvictLocal` to call `DeleteAllBlockFiles` only (dropping the redundant `EvictMemory`) would be a tiny simplification — out of scope for TD-02c (plan is strictly about `Delete`). Left for a future refactor pass; not a correctness issue.

## TDD Gate Compliance

`type=auto` task with `tdd=true`.

1. **RED** — `TestEngineDelete_RemovesBlockFiles` written first, confirmed FAIL on current (pre-fix) code with `got 2` orphan `.blk` files.
2. **GREEN** — `engine.Delete` swapped `EvictMemory` for `SyncFileBlocksForFile + DeleteAllBlockFiles`; test passes.
3. **REFACTOR** — none needed; one-line sync + one-line delete + updated doc comment; symmetric with `EvictLocal`.

Per D-11 and PROJECT.md "each step must compile and pass all tests independently", test + fix + doc landed as a *single* atomic commit `d35b4e53` so HEAD is never red.

## Self-Check: PASSED

- FOUND: `pkg/blockstore/engine/engine.go` (modified — `Delete` now calls `SyncFileBlocksForFile` + `DeleteAllBlockFiles`; doc updated)
- FOUND: `pkg/blockstore/engine/engine_test.go` (modified — `newFSTestEngine` + `countBlkFiles` + `TestEngineDelete_RemovesBlockFiles` appended; `os`, `path/filepath`, `pkg/blockstore/local/fs`, `pkg/metadata/store/memory` imports added)
- FOUND: commit `d35b4e53` in `git log` (signed RSA, conventional subject, no AI mentions in body)
- FOUND: `DeleteAllBlockFiles` grep returns 4 matches in `engine.go` (≥ 1 satisfied)
- FOUND: `TestEngineDelete_RemovesBlockFiles` in engine_test.go (line 719 comment, line 723 func)
- FOUND: `go test -race ./pkg/blockstore/engine/... ./pkg/blockstore/local/fs/...` exits 0
- FOUND: `go build ./...` exits 0
- FOUND: `go vet ./pkg/blockstore/...` exits 0 silently
