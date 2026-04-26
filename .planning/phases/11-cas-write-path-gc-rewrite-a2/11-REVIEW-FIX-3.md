---
phase: 11-cas-write-path-gc-rewrite-a2
fixed_at: 2026-04-25T00:00:00Z
review_path: .planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-3.md
iteration: 3
findings_in_scope: 7
fixed: 7
skipped: 0
status: all_fixed
---

# Phase 11: Code Review Fix Report (Pass 3)

**Fixed at:** 2026-04-25
**Source review:** .planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-3.md
**Iteration:** 3

**Summary:**
- Findings in scope: 7
- Fixed: 7
- Skipped: 0

## Fixed Issues

### WR-3-01: Concurrent CollectGarbage calls against the same GCStateRoot race in CleanStaleGCStateDirs

**Files modified:** `pkg/blockstore/engine/gc.go`, `pkg/blockstore/engine/gc_test.go`
**Commit:** `2256d338`
**Applied fix:** Added a per-process mutex registry keyed by `filepath.Clean`'d
GCStateRoot. `acquireGCRootLock(root)` returns the (lazy-allocated) mutex
already held; `CollectGarbage` defers the unlock at entry so the entire mark
+ sweep + MarkComplete + last-run.json persist runs under the lock. Empty
key (temp-root callers like `RunBlockGC`) still serialize against each
other to prevent accidental concurrent runs against one remote endpoint.
Cross-process safety is left as a TODO for the multi-process phase. Adds a
regression test that fires 8 parallel runs sharing one root and asserts
every run sweeps exactly its own orphan and leaves no `incomplete.flag`
behind.

### WR-3-02: gc.interval parsed/validated/documented but never wired

**Files modified:** `cmd/dfs/commands/start.go`, `pkg/config/config.go`,
`docs/CONFIGURATION.md`, `docs/ARCHITECTURE.md`, `docs/FAQ.md`
**Commit:** `cfb0ebec`
**Applied fix:** Picked the smaller-blast-radius option per orchestrator
guidance: keep the field for the future scheduler phase, but emit a loud
startup WARN whenever a non-zero `gc.interval` is configured. Updated
CONFIGURATION.md, ARCHITECTURE.md, FAQ.md, and the GCConfig.Interval doc
comment to call out the deferred status — operators who follow the docs
now schedule via cron instead of expecting a built-in scheduler. No
existing callers break (config validation, defaults, on-demand GC paths
are unchanged).

### IN-3-01: docs/CONFIGURATION.md grace_period<5m said "warned" but pass-2 made it rejected

**Files modified:** `docs/CONFIGURATION.md`
**Commit:** `35927db8`
**Applied fix:** One-line comment update reflecting the actual contract:
values in `(0, 5m)` are REJECTED at config load; values in `[5m, 10m)`
are accepted but emit a warning.

### IN-3-03: GC FirstErrors captured first 16 verbatim — homogeneous burst hides 17th distinct error

**Files modified:** `pkg/blockstore/engine/gc.go`,
`pkg/blockstore/engine/gc_test.go`
**Commit:** `b6e4f959`
**Applied fix:** Replaced the count-cap in `sweepPhase.addError` with a
class-cap. New `classifyGCError` strips the high-cardinality path/key
tail from the verb prefix (e.g. `delete cas/aa/bb/cc:` → `delete:`) and
truncates the body at the first ":" so `503 SlowDown` and
`AccessDenied` produce distinct keys but per-key noise does not.
Per-call `seenClasses` map under `statsMu` caps captured FirstErrors at
16 distinct classes; `ErrorCount` still reflects the true total. Adds
a 4-case unit test for the classifier and a sweep-level assertion that
20 identical delete failures collapse to one FirstErrors entry.

### IN-3-04: GC engine logs lack share/endpoint context

**Files modified:** `pkg/blockstore/engine/gc.go`,
`pkg/controlplane/runtime/blockgc.go`
**Commit:** `7bd0a2cb`
**Applied fix:** Added `RemoteEndpointID string` and `Shares []string` to
`engine.Options`. `Runtime.RunBlockGC` and `Runtime.RunBlockGCForShare`
populate both from `RemoteStoreEntry.ConfigID` and `entry.Shares`. The
engine includes both fields in the `mark phase starting`, `complete`,
and `mark failed` log lines so SREs no longer need to reach back to the
runtime caller's pre-call log to find which bucket/share a run was
touching.

### IN-3-05: dispatchRemoteFetch silent-zero on CAS ErrBlockNotFound masks live-data-loss

**Files modified:** `pkg/blockstore/engine/fetch.go`,
`pkg/blockstore/engine/engine_dualread_test.go`
**Commit:** `2bd08111`
**Applied fix:** Split routing in `fetchBlock` and `inlineFetchOrWait`:
legacy rows (zero hash) preserve the historical sparse-block silent-zero
semantics; CAS rows (non-zero hash) surface a wrapped
`blockstore.ErrBlockNotFound` and emit a structured `slog.Error` tagged
with `block_id`, `store_key`, `hash` so SREs can correlate with GC
activity. Adds two regression tests: one asserts the fail-closed CAS
miss returns wrapped `ErrBlockNotFound`; one asserts the legacy-path
sparse miss still returns `nil, nil` (no regression).

### IN-3-02: cross-backend dedup short-circuit on hash collision — backends disagree, no test pins behavior

**Files modified:** `pkg/blockstore/store.go`
**Commit:** `369350fd`
**Applied fix:** Documented the contract on the `FileBlockStore.PutFileBlock`
interface doc. Pinned semantics: upsert is by ID, Hash is NOT a
uniqueness constraint, any cross-row hash collision MUST return nil.
Memory + badger satisfy this today via silent overwrite of the hash→id
map; postgres does NOT (the partial UNIQUE on `(hash WHERE NOT NULL)`
rejects the second writer) and is flagged as a separate backend bug
that needs a follow-up `ON CONFLICT (hash) DO NOTHING` migration —
out of Phase 11 scope per orchestrator guidance ("document the actual
contract OR add a conformance test that pins the expected behavior").

## Skipped Issues

None — all in-scope findings were fixed.

---

_Fixed: 2026-04-25_
_Fixer: Claude (gsd-code-fixer)_
_Iteration: 3_
