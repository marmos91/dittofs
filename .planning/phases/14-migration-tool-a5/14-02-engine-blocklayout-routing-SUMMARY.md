---
phase: 14-migration-tool-a5
plan: 02
subsystem: blockstore
tags: [engine, dual-read-shim, block_layout, mig-03, fail-loud, syncer]

# Dependency graph
requires:
  - phase: 14-migration-tool-a5
    provides: Per-share metadata.BlockLayout enum + ShareOptions field threaded through every metadata backend (Plan 14-01)
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: Dual-read shim seam inside Syncer.dispatchRemoteFetch
provides:
  - "engine.ErrLegacyReadOnCASOnly sentinel — fail-loud signal when a legacy-shaped FileBlock is encountered on a cas-only share"
  - "engine.SyncerConfig.BlockLayout field + Syncer.blockLayout backing field with coerce-empty-to-legacy semantics in NewSyncer"
  - "Syncer.BlockLayout() / engine.BlockStore.BlockLayout() getters for cutover (Plan 14-05) and dfsctl introspection"
  - "Per-share gate inside dispatchRemoteFetch — `cas-only` shares refuse the legacy ReadBlock fallback and surface ErrLegacyReadOnCASOnly with structured logger.Error context"
  - "shares.Service.createBlockStoreForShare reads BlockLayout from metadata.ShareOptions (D-A6 source-of-truth) and threads it into SyncerConfig"
affects: [14-03-migrate-tool-core, 14-05-integrity-cutover]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Coerce-empty-to-default at construction time (engine NewSyncer mirrors metadata.ParseBlockLayout) — defense-in-depth so a zero-valued SyncerConfig never reaches the gate"
    - "API-02 justification preamble on every new pkg/metadata import inside engine production files (matches the audit_state.go / gc.go precedent)"
    - "Cast EngineFileBlockStore -> metadata.MetadataStore for the option-read path (mirrors the existing coordinator-wiring cast)"

key-files:
  created:
    - pkg/blockstore/engine/errors.go
    - pkg/controlplane/runtime/shares/blocklayout_wiring_test.go
  modified:
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/engine/engine_dualread_test.go
    - pkg/blockstore/engine/fetch.go
    - pkg/blockstore/engine/syncer.go
    - pkg/blockstore/engine/types.go
    - pkg/controlplane/runtime/shares/service.go

key-decisions:
  - "Field lives on Syncer (not BlockStore) because dispatchRemoteFetch is a Syncer method and Plan 11's dual-read shim already binds the routing decision there. BlockStore.BlockLayout() delegates to Syncer.BlockLayout() so callers (cutover, dfsctl, tests) have one obvious accessor."
  - "Empty / unknown SyncerConfig.BlockLayout coerces to BlockLayoutLegacy at NewSyncer time. Engine never trusts that the metadata layer's coercion was applied — defense-in-depth keeps a forgotten wire-up from misrouting a share onto cas-only."
  - "GetShareOptions failure on createBlockStoreForShare logs Warn and defaults to legacy, rather than failing share creation. Failing share creation on a metadata-store hiccup (transient Postgres connection error, eventual-consistency race) was deemed too aggressive; the legacy fallback is the safe direction."
  - "Three new dual-read tests live alongside the existing TestDualRead_* family and share the same dualReadEnv fixture (extended with a per-test BlockLayout setter). Keeps the entire dual-read regression matrix in one file."

patterns-established:
  - "Per-share enum gates in the engine — when a share-record field needs to influence the engine's hot path, plumb it through SyncerConfig (not BlockStore.Config) and expose an engine.BlockStore getter that delegates. Future per-share gates (e.g. retention overrides for the GC) can follow this exact pattern."
  - "Fail-loud post-migration drift — the gate's sentinel + Error log is the template Plan 14-04 / 14-05 / 14-06 should use for every other migration-related drift signal."

requirements-completed: [MIG-03]

# Metrics
duration: ~12min
completed: 2026-05-05
---

# Phase 14 Plan 02: Engine BlockLayout Routing Summary

**Per-share `BlockLayout` flag now gates the engine's dual-read shim — `cas-only` shares refuse the legacy `{payloadID}/block-{idx}` fallback and surface `ErrLegacyReadOnCASOnly`, closing the live-data-loss gap MIG-03 / D-A8 calls out.**

## What Shipped

Two commits, two tasks, every existing dual-read test still green plus six new tests asserting the gate.

- **Task 1 — `feat(14-02): gate legacy fallback per-share via BlockLayout in dispatchRemoteFetch` (`501bc008`).** New `engine.ErrLegacyReadOnCASOnly` sentinel in `pkg/blockstore/engine/errors.go` (its own file — engine sentinel hygiene, the `errors.go` file did not exist before; existing sentinels lived inline in their owning files). New `metadata.BlockLayout` field on `SyncerConfig`; new `blockLayout metadata.BlockLayout` field on `Syncer`. `NewSyncer` coerces empty/unknown to `BlockLayoutLegacy` at construction time (defense-in-depth — Plan 14-01 already coerces on read at the metadata layer, but the engine should not trust callers). `Syncer.BlockLayout()` getter returns the frozen value. `dispatchRemoteFetch` now consults `m.blockLayout` before the legacy `ReadBlock` fallback: if the share is `cas-only`, the function logs at Error with `block_id` + `store_key` and returns `fmt.Errorf("%w: block_id=%s", ErrLegacyReadOnCASOnly, fb.ID)`. CAS-path routing is untouched — `cas-only` shares still serve verified reads via `ReadBlockVerified` exactly as before. Three new `TestDualRead_CASOnly_RefusesLegacyFallback` / `TestDualRead_Legacy_AllowsBothPaths` / `TestDualRead_CASOnly_AllowsCASPath` tests + `TestDualRead_BlockLayoutGetterRoundTrips` cover the gate matrix; the existing `TestDualRead_*` family stays green (T-14-02-03 non-regression assertion).

- **Task 2 — `feat(14-02): wire share BlockLayout into engine via createBlockStoreForShare` (`531fd20b`).** `shares.Service.createBlockStoreForShare` reads the share's `metadata.ShareOptions.BlockLayout` (cast from `EngineFileBlockStore` to `metadata.MetadataStore`, mirroring the same cast used a few lines below for the metadata coordinator wiring). Threads it into `syncerCfg.BlockLayout` before `engine.NewSyncer`. New `engine.BlockStore.BlockLayout()` getter delegates to the syncer for test/dfsctl introspection and the future Plan 14-05 cutover reload path. Three wiring tests in a new `pkg/controlplane/runtime/shares/blocklayout_wiring_test.go` exercise the full `createBlockStoreForShare` path against a memory metadata store + tmp-dir fs block store, asserting `share.BlockStore.BlockLayout()` returns `cas-only`, `legacy`, and `legacy` (zero-value) respectively.

## Verification Results

| Check                                                                                              | Result            |
| -------------------------------------------------------------------------------------------------- | ----------------- |
| `go test ./pkg/blockstore/engine/ -run 'TestDualRead' -count=1`                                    | PASS (10/10)      |
| `go test ./pkg/blockstore/engine/ -count=1`                                                         | PASS (full suite) |
| `go test ./pkg/controlplane/runtime/shares/ -run 'TestCreateBlockStoreForShare_BlockLayout'`        | PASS (3/3)        |
| `go test ./pkg/controlplane/runtime/shares/ -count=1`                                              | PASS (full suite) |
| `go test ./... -count=1`                                                                           | PASS module-wide  |
| `go vet ./pkg/blockstore/engine/ ./pkg/controlplane/runtime/shares/`                               | clean             |
| `go build ./...`                                                                                   | clean             |
| `grep -rln 'ErrLegacyReadOnCASOnly' pkg/blockstore/engine/` (excluding test files)                | 4 files (sentinel + fetch.go + errors.go) |
| `grep -c 'blockLayout' pkg/blockstore/engine/syncer.go pkg/blockstore/engine/fetch.go`            | 4 + 1 = 5 (≥3 ✓)  |
| `grep -c 'BlockLayout' pkg/controlplane/runtime/shares/service.go`                                | 9 (≥2 ✓)          |
| `grep -c 'func.*BlockStore.*BlockLayout' pkg/blockstore/engine/engine.go`                         | 1 (≥1 ✓)          |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] FileBlock has no `PayloadID` / `BlockIndex` fields**

- **Found during:** Task 1 implementation, when wiring the structured log line per the plan's action snippet.
- **Issue:** The plan's `<action>` block calls for `logger.Error(... "block_id", fb.ID, "payload_id", fb.PayloadID, "block_idx", fb.BlockIndex)`. Inspecting `pkg/blockstore/types.go` shows `FileBlock` only carries `ID` (the `"{payloadID}/{blockIdx}"` composite). Compiling the snippet verbatim would have failed.
- **Fix:** Log the composite `block_id` and the legacy `store_key` instead. Operators can derive payloadID/blockIdx from `block_id` via the existing `blockstore.ParseStoreKey` (or just reading the prefix). The forensic value is preserved without inventing accessors.
- **Files modified:** `pkg/blockstore/engine/fetch.go` (gate snippet only).
- **Commit:** `501bc008` (rolled into Task 1).

**2. [Rule 2 - Missing critical functionality] BlockStore-level getter required even though Syncer owns the field**

- **Found during:** Task 2, when writing the wiring test — the test only has access to `share.BlockStore` (a `*engine.BlockStore`), not the underlying `*Syncer`.
- **Issue:** The plan's `<acceptance_criteria>` for Task 2 calls for `grep -c 'func.*BlockStore.*BlockLayout' pkg/blockstore/engine/engine.go >= 1`. The plan's `<action>` step 2 leaves the placement somewhat open ("on the BlockStore" or "on the Syncer"). Putting the field on Syncer (where `dispatchRemoteFetch` lives) is the cleanest decomposition, but the public getter has to live on `BlockStore` for callers that don't reach into the syncer.
- **Fix:** Added `BlockStore.BlockLayout()` that delegates to `Syncer.BlockLayout()`. Both getters exist. The `bs.syncer == nil` guard returns `metadata.BlockLayoutLegacy` so a partially-constructed BlockStore (theoretical only — `engine.New` errors out if `cfg.Syncer == nil`) still returns a safe value.
- **Files modified:** `pkg/blockstore/engine/engine.go`.
- **Commit:** `531fd20b`.

**3. [Rule 3 - Blocking issue] Postgres test timing & metadata.MetadataStore access pattern**

- **Found during:** Task 2 wiring test design.
- **Issue:** `createBlockStoreForShare` accepts a `blockstore.EngineFileBlockStore` (the narrowed engine seam from Plan 12-04 / META-03). But the BlockLayout read must go through `metadata.MetadataStore.GetShareOptions` — that wider interface is not on `EngineFileBlockStore`. The plan calls for "fetch the share's BlockLayout from `metadataStore.GetShareOptions(...)` ... do NOT re-fetch from the controlplane DB".
- **Fix:** Cast `fileBlockStore.(metadata.MetadataStore)` exactly the same way the existing coordinator-wiring code does a few lines below. Production paths always pass a real metadata store; in-test fakes that don't implement `metadata.MetadataStore` cleanly fall back to the legacy default (logged at Warn). The cast is the established pattern in this file — no new abstraction needed.
- **Files modified:** `pkg/controlplane/runtime/shares/service.go`.
- **Commit:** `531fd20b`.

### Plan Output Path

The plan's `<output>` block specifies `.planning/phases/14-migration-tool-a5/14-02-SUMMARY.md`, but the executor convention (and this agent's spawning instructions) use the longer `14-02-engine-blocklayout-routing-SUMMARY.md`. Wrote to the conventional path — same as Plan 14-01 did with `14-01-share-blocklayout-SUMMARY.md`.

### Commit Granularity vs. Plan-Body TDD Order

The plan tasks are tagged `tdd="true"`, which conventionally implies a RED commit (failing test alone) followed by a GREEN commit. I rolled both into a single `feat` commit per task because the new tests reference symbols (`ErrLegacyReadOnCASOnly`, `cfg.BlockLayout`, `BlockLayout()`) that don't exist yet — splitting into a RED commit would leave the build broken between commits, breaking `git bisect`. This matches the precedent set by Plan 14-01's commits (`67af6a8b`, `7eff1c34`, `5b30ff05`), which all combined test + impl. Each task's commit message documents both the test additions and the implementation changes.

## Threat Surface Notes

The plan's `<threat_model>` covered three threats. All three now have code-level mitigations:

- **T-14-02-01 (Tampering — direct DB UPDATE flipping BlockLayout to cas-only on an unmigrated share):** Engine reads `BlockLayout` once at share open. Direct DB tampering surfaces as `ErrLegacyReadOnCASOnly` on the FIRST legacy-keyed read with a structured Error log line. Operators get a clear forensic trail (block_id + store_key) instead of stale bytes. Plan 14-05 controls the production-flip path; this gate is the safety net for everything else.
- **T-14-02-02 (Information disclosure — silent legacy fallback returning stale post-migration bytes):** The per-share gate refuses the fallback on `cas-only` AND logs at Error so the failure is observable in production logs. The error wraps the sentinel via `fmt.Errorf("%w: block_id=%s", ...)` so callers can `errors.Is` and surface a meaningful end-user error.
- **T-14-02-03 (Denial of service — existing legacy share unable to read after this change):** The `TestDualRead_Legacy_AllowsBothPaths` test asserts a legacy-shaped FileBlock on a `BlockLayoutLegacy` share routes through `ReadBlock` exactly as before. The empty-string coercion in `NewSyncer` ensures pre-Phase-14 shares (whose metadata rows pre-date the column) behave identically to today.

## Key Files Touched

### Created

- `pkg/blockstore/engine/errors.go` — new file housing `ErrLegacyReadOnCASOnly`. Engine sentinel hygiene: existing sentinels lived inline in their owning files (`coordinator.go`, `audit_state.go`, `types.go`); this is the first one with broad cross-file relevance, so it gets its own file.
- `pkg/controlplane/runtime/shares/blocklayout_wiring_test.go` — three end-to-end wiring tests against a memory metadata store + tmp-dir fs block store. Uses `createBlockStoreForShare` directly (unexported, same package) to keep the test scope narrow.

### Modified

- `pkg/blockstore/engine/types.go` — `metadata` import added under an API-02 justification preamble; `SyncerConfig.BlockLayout` field with godoc tying it to MIG-03 / D-A6 / D-A8.
- `pkg/blockstore/engine/syncer.go` — `metadata` import added under an API-02 justification preamble; `Syncer.blockLayout` field with godoc; `NewSyncer` coerces empty/unknown to `BlockLayoutLegacy` and stores the value; `Syncer.BlockLayout()` getter.
- `pkg/blockstore/engine/fetch.go` — `metadata` import added under an API-02 justification preamble; `dispatchRemoteFetch` gates the legacy fallback behind `m.blockLayout == metadata.BlockLayoutCASOnly` with a structured Error log line and wrapped-sentinel return. CAS path untouched.
- `pkg/blockstore/engine/engine.go` — `metadata` import added under an API-02 justification preamble; `BlockStore.BlockLayout()` getter delegating to the Syncer with a defensive nil-guard.
- `pkg/blockstore/engine/engine_dualread_test.go` — `metadata` test import; new `newDualReadEnvWithLayout` helper; three new `TestDualRead_*` tests + getter round-trip test.
- `pkg/controlplane/runtime/shares/service.go` — `createBlockStoreForShare` reads `metadata.ShareOptions.BlockLayout` via the same cast pattern used for coordinator wiring; threads into `syncerCfg.BlockLayout`. Failure to read share options is non-fatal (Warn log, fall back to legacy).

## Decisions Made

- **Field on Syncer, not BlockStore.** `dispatchRemoteFetch` is a Syncer method and the dual-read shim already binds the routing decision there. Putting the field on the Syncer keeps the gate logic tight and avoids passing a per-share config through every fetch call. `BlockStore.BlockLayout()` delegates so external callers have one obvious accessor.

- **Coerce at construction, not at every read.** `NewSyncer` normalizes empty/unknown values to `BlockLayoutLegacy` once. The hot-path comparison in `dispatchRemoteFetch` is then a simple `==` check against `BlockLayoutCASOnly`, no string parsing or error handling per fetch.

- **Wider tests in their own file vs. extending existing test files.** The wiring tests live in a new `blocklayout_wiring_test.go` (not appended to `service_test.go`) because the existing file is dedicated to Disable/Enable share testing with a different fixture style (`fakeShareStore`). Mixing a heavier `createBlockStoreForShare` fixture with the lightweight DB-row tests would have muddied the file's purpose.

- **GetShareOptions failure → Warn + legacy default.** The alternative (fail share creation on a metadata-store read error) was rejected because it makes share creation a metadata-store-dependent operation under a flag that has a perfectly safe default. Operators can still see drift via the Warn log; production paths exercise this read on every share creation.

## Self-Check: PASSED

- [x] `pkg/blockstore/engine/errors.go` exists and contains `ErrLegacyReadOnCASOnly` — verified.
- [x] `pkg/blockstore/engine/fetch.go` references `BlockLayoutCASOnly` and `ErrLegacyReadOnCASOnly` — verified.
- [x] `pkg/blockstore/engine/syncer.go` declares `Syncer.blockLayout` field, copies from config in `NewSyncer`, exposes `BlockLayout()` getter — verified.
- [x] `pkg/blockstore/engine/types.go` declares `SyncerConfig.BlockLayout` field — verified.
- [x] `pkg/blockstore/engine/engine.go` exposes `BlockStore.BlockLayout()` getter — verified.
- [x] `pkg/controlplane/runtime/shares/service.go` reads `ShareOptions.BlockLayout` and threads into `SyncerConfig.BlockLayout` — verified (9 references).
- [x] Commit `501bc008` (Task 1) reachable via `git log` — verified.
- [x] Commit `531fd20b` (Task 2) reachable via `git log` — verified.
- [x] All new tests green; existing dual-read tests + full module test suite green.
