---
title: Rollup redesign — per-file logIndex (Direction 1)
date: 2026-05-22
status: queued (waits for PR #556 to merge)
author: marmos91 + code-architect
scope: pkg/blockstore/local/fs/
target: develop (no milestone phase — surgical bugfix)
---

# Rollup redesign — per-file `logIndex` (Direction 1)

## Context

v0.16.0 milestone shipped (Phases 16–19). Live smoke on macOS NFSv3 surfaced a structural bug in the rollup loop at `pkg/blockstore/local/fs/rollup.go`: records on the per-file append-log are stored in AppendWrite **arrival order**, not file-offset order. macOS NFSv3 issues parallel WRITE RPCs, so a 4 MiB random-write produces log records like:

```
rec#0 log_off=64    file_off=32768
rec#1 log_off=32848 file_off=458752
rec#2 log_off=65632 file_off=0
rec#3 log_off=98416 file_off=1540096
...
```

The rollup's linear scan loop assumes file-offset-sorted records and breaks on the first `off >= stableEnd`, never reaching the in-stable record at `log_off=65632`. Net effect: `recs=0` → silent no-op rollup pass → CAS blocks never produced for parallel-write workloads.

## Architect review

Full design review produced by `feature-dev:code-architect` on 2026-05-22. Verdict: **Direction 1 (interval-tree carries log positions, materialized as a separate per-file `logIndex`)** is strictly best. Architect notes archived in this proposal's commit history.

### Root cause — 5 sites, not 1

| Location | Ordering assumption |
|---|---|
| `rollup.go:196-256` | Log records for stable interval are contiguous at head of unread suffix |
| `rollup.go:181` | Single stable interval consumed per rollup pass |
| `interval_tree.go:80-99` | Unstable front interval blocks all later stable intervals |
| `recovery.go:345` | All records re-inserted with `time.Now()`; arrival order lost |
| `appendlog.go:48-52` | `rollup_offset` = "consumed up to this log byte offset" (not consumed record set) |

### Architecture chosen — per-file `logIndex`

Decouple the file-offset domain (interval tree, already exists) from the log-position domain (new `logIndex`). The interval tree answers *which file regions are dirty and stable*; the `logIndex` answers *where in the log are the records for those regions*.

New file `pkg/blockstore/local/fs/logindex.go`:

```go
type logEntry struct {
    logPos     uint64   // byte offset of frame start in log
    fileOff    uint64   // record's file_offset field
    payloadLen uint32
}

type logIndex struct {
    entries          []logEntry          // append-only, ordered by logPos (= arrival)
    consumed         map[uint64]struct{} // logPos → consumed
    compactionFence  uint64              // largest logPos s.t. all earlier entries are consumed
}
```

`lf.eofPos uint64` field added to `logFile` — tracks log EOF position, incremented under per-file `mu` after each successful `writeRecord + groupCommit.Sync`. No new syscall.

`rollup_offset` on-disk format **unchanged**. Semantic shift: it now records the compaction fence rather than a consumption marker. Recovery seeds `logIndex` from the existing record-scan loop; no on-disk migration.

### Invariants preserved

- Crash durability (AppendWrite + fsync = durable bytes)
- BLAKE3 content addressing
- FastCDC boundary stability (D-21) — `reconstructStream` FIX-3 anchor unchanged
- Per-share isolation (one FSStore per share)
- Lock ordering: per-file `mu` → `bc.logsMu` → `groupCommit.mu`
- All three metadata backends pass `pkg/metadata/storetest/file_block_ops.go` (no interface change)
- RFC1813 / SMB2 write semantics

### Invariants retired

| Retired | Why |
|---|---|
| `rollup_offset` = monotonic byte prefix of consumed log | A prefix cannot describe out-of-order consumption. Replaced by `compactionFence` (semantically same on-disk, conceptually different). |
| Interval tree as log-scan controller | Tree stays as file-offset oracle. Log-position lookup moves to `logIndex`. |
| Single stable interval per rollup pass | Unchanged in shape, but now identified by file-offset range, not by proximity to log head. |
| Linear log scan from `rollup_offset` | Replaced by `idx.EntriesForInterval` + per-entry `ReadAt`. |

### Phase 19 RAM-opt compatibility

All four opts survive **unchanged**:
- Opt 1 LRU dedup → still in `rollup.go` chunk emission loop
- Opt 2 group commit → unchanged `AppendWrite` shape (one new field increment under existing `mu`)
- Opt 3 OnChunkComplete → unchanged `StoreChunk`
- Opt 4 eager small-file dedup → fires before rollup; `DeleteAppendLog` step 5 gains one `delete(bc.logIndices, payloadID)` line

## Risk register

| ID | Risk | Mitigation |
|---|---|---|
| R-1 | `logPos` drifts if `writeRecord` partial-writes | `eofPos` advances only after `writeRecord` returns nil; existing FIX-2/FIX-20 cleanup path closes fd on error |
| R-2 | `consumed` map memory grows with in-flight unconsumed records | Bounded by `maxLogBytes / min_record_size` ≈ 67M entries at default 1 GiB; pressure machinery caps worst case |
| R-3 | Recovery rebuilds logIndex from full log scan | Same cost as today's interval-tree rebuild; no regression |
| R-4 | Race between AppendWrite logPos assignment and rollup pread | Per-file `mu` serializes both; rollup pread uses read-only fd (existing pattern) |
| R-5 | `rollup_offset` semantic shift breaks existing logs | None — on-disk format unchanged; semantic shift is in-process only |
| R-6 | Targeted preads slower than sequential scan on rotational media | NVMe is primary target per CLAUDE.md memory; pread is effectively free for cache-warm pages |
| R-7 | Compaction fence can stall if log_pos=0 record covers last-stabilized region | Same pathology as today's `rollup_offset` stall; existing pressure machinery handles it. v0.17+ improvement: track consumption by file-offset interval, not log position |
| R-8 | Hard to unit-test pread-based rollup | Existing temp-dir test infra works; new tests seed `logIndex` via real `AppendWrite` calls |

## Phased plan

Each phase = atomic commit, signed (`-S`). Branch: `fix/rollup-logindex-direction1` off `develop` **after** PR #556 (layout flatten) merges.

### P0 — pre-flight

- No code change. Pure setup: branch creation, plan link in commit message.
- (No band-aid revert needed — the band-aid was never committed.)

### P1 — `logFile.eofPos` plumbing (2 commits)

- `feat(fs): add logFile.eofPos cursor for log-position tracking`
- `test(fs): verify eofPos invariant against on-disk size`

### P2 — `logIndex` struct + unit tests (2 commits)

- `feat(fs): add logIndex data structure for log-position tracking`
- `test(fs): logIndex unit tests covering interval lookup + fence advancement`

### P3 — wire `logIndex` into FSStore (2 commits)

- `feat(fs): populate logIndex on every AppendWrite`
- `test(fs): verify logIndex tracks all AppendWrite records under concurrency`

System runs with both old (linear scan) + new (logIndex populated but unused) paths in parallel. Rollup still broken for parallel-write workload. Safe intermediate state.

### P4 — recovery populates `logIndex` (1 commit)

- `feat(fs): recovery seeds logIndex from on-disk records`

### P5 — switch rollup to `logIndex` (2 commits)

- `refactor(fs): rollup consumes records via logIndex, not sequential scan`
- `test(fs): out-of-order + stalled-fence rollup regression coverage`

Bug fixed. All `pkg/blockstore/local/fs/...` tests green.

### P6 — cleanup + documentation (1 commit)

- `docs(fs): document logIndex consumption model + rollup_offset semantic shift`
- Mark `TRANSITIONAL-V0.17+` for physical log compaction (rewriting log file dropping consumed records — out of scope here)

### P7 — integration verification (no commit)

- Full `go test ./... -race`
- Live smoke macOS NFSv3: 4 MiB random write → CAS blocks within 2s
- Live smoke macOS NFSv3: 18-byte file → single chunk within 1s
- Live smoke: 1 GiB sustained write → log pressure releases correctly
- Conformance suite memory/badger/postgres backends

### P8 — open PR

- 8 commits total against `develop`
- PR body references this proposal + architect review
- Test plan: P7 surface

## Sequencing

1. Wait for PR #556 (layout flatten) to merge.
2. Fresh branch off updated `develop`.
3. Execute P0–P6 in order.
4. P7 + P8.

## Rollback plan

Each commit is a coherent rollback point. Order of severity:
- Revert P6 → restore vestigial comments, keep structural fix
- Revert P5+P6 → keep `logIndex` populated but unused; rollup back to original (broken) scan
- Revert P3+P4+P5+P6 → drop `logIndex` entirely

If the structural fix introduces a regression, the safest rollback restores the original `pkg/blockstore/local/fs/rollup.go` behavior (broken for parallel writes, correct for sequential).

## Out of scope

- Physical log compaction (rewriting log file dropping consumed records) — `TRANSITIONAL-V0.17+`
- Tracking consumption by file-offset interval rather than log position (R-7 follow-up) — `TRANSITIONAL-V0.17+`
- Direction 2 (in-memory tree holds payload bytes) — rejected by architect; violates `maxLogBytes` RAM budget
- Direction 3 (per-record tombstone + compaction) — rejected by architect; strictly more I/O than Direction 1

## File-level checklist

- [ ] `pkg/blockstore/local/fs/logindex.go` — **NEW** (~150 LoC)
- [ ] `pkg/blockstore/local/fs/logindex_test.go` — **NEW** (~100 LoC)
- [ ] `pkg/blockstore/local/fs/appendwrite.go` — `eofPos` field, `idx.Append` after fsync, `DeleteAppendLog` cleanup
- [ ] `pkg/blockstore/local/fs/fs.go` — `bc.logIndices` map, init, teardown
- [ ] `pkg/blockstore/local/fs/rollup.go` — replace scan with `EntriesForInterval` + `ReadAt`
- [ ] `pkg/blockstore/local/fs/recovery.go` — populate `logIndex` in scan loop
- [ ] `pkg/blockstore/local/fs/rollup_test.go` — out-of-order + stalled-fence + recovery seeding + parallel-burst regression
- [ ] No change: `pkg/blockstore/store.go` (interfaces preserved)
- [ ] No change: `pkg/metadata/` (backends untouched)
- [ ] No change: `pkg/blockstore/engine/` (engine layer untouched)
- [ ] No change: `pkg/metadata/storetest/file_block_ops.go` (conformance unchanged)
