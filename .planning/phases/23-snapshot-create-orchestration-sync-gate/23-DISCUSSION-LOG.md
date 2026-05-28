# Phase 23: Snapshot Create Orchestration + Sync Gate - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-28
**Phase:** 23-snapshot-create-orchestration-sync-gate
**Areas discussed:** Row lifecycle vs GC hold, Sync gate behavior, Failure + retry semantics, CreateSnapshot API shape, Code structure / design

---

## Row lifecycle vs GC hold

### Q1: Row insert timing relative to backup + manifest write

| Option | Description | Selected |
|--------|-------------|----------|
| Insert `creating` up front | Insert row before backup. Reserves ID, enforces D-08 unique partial index, supports D-06 retry path. | ✓ |
| Insert `ready` after all I/O | Run backup + manifest + verify in tmp dir, insert ready row at end. Loses concurrency guard. | |
| Insert `creating`, in `<id>.tmp/` | Row up front, on-disk work in tmp dir, rename after ready-flip. Manifest already atomic — redundant. | |

**User's choice:** Yes, `creating` up front
**Notes:** User asked for advisor recommendation. Claude argued: backup needs ID-keyed dir, D-08 concurrency guard only works with row present during create, D-06 retry semantics depend on failed row surviving. User confirmed.

### Q2: How blocks stay safe during the create window

| Option | Description | Selected |
|--------|-------------|----------|
| Extend hold to `creating` w/ manifest | Phase 22 D-05 revised — `creating`+manifest also held. | ✓ |
| Flip `ready` before sync gate | Ready as soon as manifest on disk; verify after; demote on fail. | |
| Rely on GC grace period | Trust Phase 22's "grace covers create window" remark. Brittle. | |

**User's choice:** Extend hold to `creating` w/ manifest
**Notes:** Later refined in Area 3 to "manifest-on-disk = held regardless of state" so `failed`+manifest also covered for retry.

### Q3: Ready flip relative to VerifyRemoteDurability

| Option | Description | Selected |
|--------|-------------|----------|
| After verify, RemoteDurable in same UPDATE | Clean invariant: `ready+RemoteDurable=true` always means durable. | ✓ |
| Before verify; RemoteDurable updated after | Hold activates early; observable `(ready, RemoteDurable=false)` window. | |

**User's choice:** After verify, RemoteDurable in same write

### Q4: Delete vs in-flight HeldHashes race (Phase 22 deferred concern)

| Option | Description | Selected |
|--------|-------------|----------|
| Per-snapshot RWMutex in HoldProvider | RLock during stream; Lock around delete. Minimal. | ✓ |
| Tombstone `deleting` state + delayed cleanup | Adds state to D-06 machine. | |
| Order: rm dir last, swallow ENOENT in reader | Lock-free; relies on mid-stream ENOENT being benign. | |

**User's choice:** Per-snapshot RWMutex in HoldProvider
**Notes:** Lock granularity (provider-wide vs per-ID `sync.Map`) left to planner discretion.

---

## Sync gate behavior

### Q1: When VerifyRemoteDurability finds a hash missing

| Option | Description | Selected |
|--------|-------------|----------|
| Trigger syncer drain, then re-verify | Call `Syncer.DrainAllUploads(ctx)` then one re-verify. | ✓ |
| Fail fast, return missing list | Verify is pure probe; caller retries after manual drain. | |
| Poll Head() until durable (timeout) | Brittle; no syncer signal. | |

**User's choice:** Trigger syncer drain, then re-verify
**Notes:** `engine.Syncer.DrainAllUploads(ctx)` already exists at `pkg/blockstore/engine/syncer.go:388` — no new engine surface.

### Q2: Error aggregation in VerifyRemoteDurability

| Option | Description | Selected |
|--------|-------------|----------|
| Fail-fast on first missing | Cancel siblings, return wrapped error with the hash. INV-04 pattern. | ✓ |
| Collect all missing, bounded sample | Higher latency; better UX for partial-durability diagnosis. | |

**User's choice:** Fail-fast on first missing
**Notes:** Drain+re-verify usually masks the diagnostic need.

### Q3: Bounded concurrency — fixed or configurable

| Option | Description | Selected |
|--------|-------------|----------|
| Fixed constant (e.g. 16) | Hardcoded in syncgate.go. Fewer knobs. | |
| Config knob on snapshot subsystem | YAML `snapshot.sync_gate_concurrency` (default 16). | ✓ |
| Match engine.Syncer's existing setting | Couples sync gate to syncer config. | |

**User's choice:** Config knob on snapshot subsystem

### Q4: Timeout — ctx only or internal default

| Option | Description | Selected |
|--------|-------------|----------|
| ctx only, no internal timeout | Cleanest Go contract. Caller sets deadline. | ✓ |
| Internal default + ctx | Belt-and-suspenders, conflicts with idiom. | |

**User's choice:** ctx only

---

## Failure + retry semantics

### Q1: Final state on failure (backup err, manifest err, verify fail after drain)

| Option | Description | Selected |
|--------|-------------|----------|
| `state=failed`, retain dump+manifest | Retry-friendly; combined with revised hold filter, blocks stay safe. | ✓ |
| `state=failed`, cleanup dump+manifest | Retry redoes all I/O. Simpler invariant. | |
| Auto-delete row + dir on failure | Loses D-06 retry-via-same-ID. | |

**User's choice:** `state=failed`, retain dump+manifest
**Notes:** Forced revisit of D-23-02 hold filter — extended to "manifest-on-disk = held regardless of state" so failed snapshots don't lose blocks before retry.

### Q2: Retry mechanism

| Option | Description | Selected |
|--------|-------------|----------|
| Caller-driven, new ID each time | Simple; failed row is independent garbage. | |
| Caller-driven, same ID via opt-in flag | `CreateSnapshotOpts.RetryOf = "<failed-id>"`. Matches D-06 verbatim. | ✓ |
| Auto-retry internally | Hides errors from caller; mixed responsibilities. | |

**User's choice:** Caller-driven, same ID via opt-in flag

### Q3: `--no-sync-gate` recorded state

| Option | Description | Selected |
|--------|-------------|----------|
| `state=ready, RemoteDurable=false` | Honors Phase 22 schema; restore can refuse; CLI gets signal. | ✓ |
| `state=ready, RemoteDurable=true` | Lies about durability. Unsafe at restore time. | |
| Drop RemoteDurable column entirely | Wastes Phase 22 design; restore re-verifies each time. | |

**User's choice:** Asked Claude to pick best option. Claude recommended A (`ready` + `RemoteDurable=false`) because Phase 22 already shipped the column for this exact case, Phase 24 restore can refuse without `--force`, and CLI list view gets a free signal. User confirmed.

### Q4: Error surface

| Option | Description | Selected |
|--------|-------------|----------|
| Typed sentinels | `ErrSnapshotBackupFailed`, `ErrSnapshotVerifyFailed`, `ErrSnapshotDrainTimeout`, `ErrSnapshotRetryTarget{NotFound,NotFailed}`. | ✓ |
| Wrapped errors from sub-layers | Just `fmt.Errorf("%w")`. Less surface. | |

**User's choice:** Typed sentinels
**Notes:** Phase 25 maps to HTTP status codes via `errors.Is`.

---

## CreateSnapshot API shape

### Q1: Sync vs async

| Option | Description | Selected |
|--------|-------------|----------|
| Sync: block until ready/failed | Caller blocks for backup+manifest+verify duration. | |
| Async: insert `creating`, run in goroutine | Returns `(snapID, error)` immediately; background goroutine does I/O. REST returns 202. | ✓ |
| Sync with progress channel | More API surface for live progress events. | |

**User's choice:** Async

### Q2: Entry point — Runtime method or sub-service

| Option | Description | Selected |
|--------|-------------|----------|
| Runtime method, `snapshot.go` file | File name already in ROADMAP. Mirrors `AddShare`/`RemoveShare`. | ✓ |
| New sub-service `runtime/snapshots/` | Heavier upfront; consistent with the 6 sub-services pattern. | |

**User's choice:** Runtime method, `snapshot.go`

### Q3: Opts shape

| Option | Description | Selected |
|--------|-------------|----------|
| Struct with fields | `type CreateSnapshotOpts struct { NoSyncGate bool; RetryOf string }`. Matches existing patterns. | ✓ |
| Functional options | Cleaner call sites but DittoFS doesn't use this pattern. | |

**User's choice:** Struct with fields

### Q4: Observability

| Option | Description | Selected |
|--------|-------------|----------|
| Structured logs at each step | `slog` debug/info per step. No metrics. | ✓ |
| Logs + metrics counters | Premature — no metrics layer exists. | |
| Logs only, basic level | Single info log start/end. Minimal. | |

**User's choice:** Structured logs at each step

### Q1b (follow-up): Goroutine lifecycle vs Runtime shutdown

| Option | Description | Selected |
|--------|-------------|----------|
| Runtime tracks in-flight, cancels on shutdown | Single registry; decoupled from share. | |
| Detached, `context.Background()` | In-flight rows left as `creating` on crash; relies on startup recovery. | |
| Tied to per-share lifecycle (RemoveShare cancels) | Share lifecycle drives cancellation; closest to ownership. | |
| **C with centralized registry on Runtime** | RemoveShare cancels + `WG.Wait` BEFORE Phase 22 D-15 tree wipe; Shutdown cancels all. | ✓ |

**User's choice:** Asked Claude for architect take. Claude argued: pure-A leaves a race window during RemoveShare (goroutine still writing into the tree being wiped); pure-C requires `shares.Service` plumbing for cancel registration; the right shape is C-in-spirit with centralized bookkeeping on Runtime — `map[shareName]*snapInFlight { wg, cancels[] }`. User confirmed.

### Q1c (follow-up): Startup recovery for orphaned `creating` rows

| Option | Description | Selected |
|--------|-------------|----------|
| On startup, flip orphaned `creating` → `failed` | Salvageable manifest retained; user can retry. | ✓ |
| No startup scan; user manages stale rows | D-08 partial index blocks new creates until manual cleanup. | |
| On startup, delete orphaned `creating` rows + dir | Auto-cleanup; loses salvageable work. | |

**User's choice:** Flip orphaned `creating` → `failed`

### Q1d (follow-up): Caller observation of completion

| Option | Description | Selected |
|--------|-------------|----------|
| Existing `GetSnapshot` is enough | CLI implements poll loop in Phase 25. No new API. | |
| Runtime exposes `WaitForSnapshot(ctx, id)` helper | Blocking helper via per-snap signal channel. | ✓ |

**User's choice:** `WaitForSnapshot` helper

---

## Code structure / design

### Q1: Plan/wave breakdown

| Option | Description | Selected |
|--------|-------------|----------|
| 6 plans / 3 waves (Phase 22 pattern) | Wave 1 parallel: syncgate, sentinels, hold filter. Wave 2: orchestration, RemoveShare integration. Wave 3: WaitForSnapshot + integration test. | ✓ |
| 4 plans / 2 waves | Tighter; some plans merged. | |
| Single mega-plan | Phase 19 pattern; harder to parallelize waves. | |

**User's choice:** 6 plans / 3 waves

### Q2: Helper location

| Option | Description | Selected |
|--------|-------------|----------|
| Private helpers in `runtime/snapshot.go` | Tests via internal `_test.go`. No API surface growth. | |
| Extract pure helpers into `pkg/snapshot/` | Natural home (manifest already there); unit-testable without Runtime. | ✓ |

**User's choice:** Extract pure helpers into `pkg/snapshot/`

### Q3: Config knob location

| Option | Description | Selected |
|--------|-------------|----------|
| Const default + Runtime override | No user-facing YAML this phase. | |
| YAML knob now (`snapshot.sync_gate_concurrency`) | Operators may want to tune for slow remotes before Phase 25. | ✓ |
| Fixed constant, no override | Reverses Area 2 Q3. | |

**User's choice:** YAML knob now

### Q4: Branch/PR strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Single PR vs `develop`, staged commits | Matches Phase 22 cadence; commit-by-commit review. | ✓ |
| One PR per plan | Parallel-mergeable; heavier coordination on shared files. | |

**User's choice:** Single PR vs `develop`

---

## Claude's Discretion

- D-23-02 implementation (DB-driven vs FS-walk for "manifest-exists" filter)
- D-23-04 RWMutex granularity (provider-level vs per-snapshot `sync.Map`)
- D-23-18 reason-marker storage for `abandoned at startup` (existing column vs structured log)
- D-23-20 placement of YAML config knob registration (P23-01 vs P23-04)
- D-23-21 exact pure-helper boundary in `pkg/snapshot/` (dump-writer signature, retry-validator signature)
- Sentinel naming variants (`ErrSnapshot*Failed` family vs Phase 22's `ErrSnapshotNotFound` style)

## Deferred Ideas

- Metrics surface for snapshot lifecycle (counters/histograms) — Phase 25 or v0.17 observability pass
- Streaming progress events from `CreateSnapshot` (chan<- Progress for CLI live UI) — Phase 25 if needed
- Collect-all-missing diagnostic mode on `VerifyRemoteDurability` — only if drain+re-verify masking proves insufficient
- Async GC + 202+poll REST for `RunBlockGC` (Phase 11 IN-4-03 deferred — independent)
- Per-snapshot RWMutex granularity upgrade to `sync.Map[snapID]*RWMutex` if head-of-line blocking emerges
- Auto-cleanup TTL of long-`failed` snapshots — operator-driven `DeleteSnapshot` sufficient for v0.16.0
- `WaitForSnapshot` event-stream API for many concurrent subscribers — `sync.Cond` upgrade if Phase 25 demands
