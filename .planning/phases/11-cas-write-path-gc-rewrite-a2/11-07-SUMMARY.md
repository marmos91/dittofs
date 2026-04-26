---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 07
subsystem: control-plane / cli
tags: [gc, rest-api, dfsctl, apiclient, phase-11, pr-c]
requires:
  - "Runtime.RunBlockGC + DistinctRemoteStores (plan 11-06)"
  - "engine.GCStats + engine.GCRunSummary + engine.PersistLastRunSummary (plan 11-06)"
  - "shares.Service.GetShare (existing)"
provides:
  - "Runtime.RunBlockGCForShare(ctx, share, dryRun)"
  - "Runtime.GCStateDirForShare(share)"
  - "shares.Service.GetGCStateDirForShare(share)"
  - "shares.Share.GCStateRoot()"
  - "handlers.BlockStoreGCHandler (RunGC + GCStatus)"
  - "handlers.BlockGCRuntime interface"
  - "apiclient.Client.BlockStoreGC(share, opts)"
  - "apiclient.Client.BlockStoreGCStatus(share)"
  - "dfsctl store block gc <share> [--dry-run]"
  - "dfsctl store block gc-status <share>"
  - "block.ErrNoGCRunYet sentinel"
affects:
  - "REST: POST /api/v1/shares/{name}/blockstore/gc"
  - "REST: GET /api/v1/shares/{name}/blockstore/gc-status"
  - "shares.Share gains gcStateRoot field (populated at share creation)"
tech-stack:
  added: []
  patterns:
    - "Narrow runtime interface in handler package for unit-testability (mirrors testBlockStoreHandler in blockstore_test.go)"
    - "Per-share REST mount at /api/v1/shares/{name}/blockstore/... (matches existing stats/evict endpoints)"
    - "Sentinel error (ErrNoGCRunYet) so callers and tests can branch on the 'first deploy' state without string matching"
key-files:
  created:
    - "internal/controlplane/api/handlers/block_gc.go"
    - "internal/controlplane/api/handlers/block_gc_test.go"
    - "pkg/apiclient/blockstore_gc_test.go"
    - "cmd/dfsctl/commands/store/block/gc.go"
    - "cmd/dfsctl/commands/store/block/gc_status.go"
    - "cmd/dfsctl/commands/store/block/gc_test.go"
  modified:
    - "pkg/apiclient/blockstore.go"
    - "pkg/controlplane/api/router.go"
    - "pkg/controlplane/runtime/blockgc.go"
    - "pkg/controlplane/runtime/shares/service.go"
    - "cmd/dfsctl/commands/store/block/block.go"
    - "internal/controlplane/api/handlers/block_gc.go"
decisions:
  - "Routes mount under /api/v1/shares/{name}/blockstore/{gc,gc-status} instead of /api/v1/store/block/{name}/{gc,gc-status} — see Deviations §1"
  - "shares.Share.gcStateRoot is populated only when the local store config carries a usable absolute path (fs backend); empty for in-memory backends — engine.PersistLastRunSummary already treats empty rootDir as a no-op"
  - "Runtime.RunBlockGC kept untouched (used by existing in-package tests). New Runtime.RunBlockGCForShare scopes last-run.json persistence to the named share's gc-state dir while delegating cross-share aggregation (D-03) to the existing DistinctRemoteStores plumbing"
  - "Handler depends on a narrow BlockGCRuntime interface (RunBlockGCForShare + GCStateDirForShare), not *runtime.Runtime — mirrors the testBlockStoreHandler pattern so unit tests substitute a recording fake"
  - "ErrNoGCRunYet sentinel in the cli package surfaces 'first deploy / no run yet' so cobra produces a non-zero exit and scripts can branch via errors.Is — replaces an earlier os.Exit(1) call that would have killed unit tests"
metrics:
  duration_seconds: 1015
  tasks_completed: 2
  commits: 3
  completed: 2026-04-25
---

# Phase 11 Plan 07: dfsctl + REST surface for the new mark-sweep GC engine

Wires the Phase 11 mark-sweep GC engine (shipped in plan 11-06) to the
dfsctl operator CLI and the REST API. Operators can now trigger a GC run
on demand, run it as a dry-run for first-deployment confidence, and
inspect the last-run summary without tailing logs. Both surfaces share
a single Runtime entry point so behavior cannot drift between CLI and
HTTP.

## What ships

### Runtime
- `Runtime.RunBlockGCForShare(ctx, shareName, dryRun) (*engine.GCStats, error)` — share-scoped GC dispatcher. Resolves the share's `gcStateRoot`, then iterates `DistinctRemoteStores()` so cross-share aggregation (D-03) is unchanged; only `last-run.json` persistence is narrowed to the named share. Returns `shares.ErrShareNotFound`-wrapped error if the share is unknown.
- `Runtime.GCStateDirForShare(name) (string, error)` — exposes the per-share gc-state root for the GCStatus handler. Empty when the share's local store has no persistent root (in-memory backend).

### Shares service
- `Share.gcStateRoot` field, populated at `createBlockStoreForShare` time via the new `deriveGCStateRoot` helper. The helper mirrors `CreateLocalStoreFromConfig`'s fs path layout: `<basePath>/shares/<sanitized>/gc-state`. Returns "" for in-memory backends or any config that doesn't yield a usable absolute path.
- `Share.GCStateRoot() string` accessor.
- `Service.GetGCStateDirForShare(name) (string, error)` — RLock'd lookup that returns `ErrShareNotFound`-wrapped error for unknown shares.

### REST handlers
- `handlers.BlockGCRuntime` — narrow interface (`RunBlockGCForShare`, `GCStateDirForShare`) so tests bind a fake without standing up `*runtime.Runtime`. Mirrors the `testBlockStoreHandler` pattern in `blockstore_test.go`.
- `handlers.BlockStoreGCHandler` with two methods:
  - **RunGC** — `POST` handler. Decodes optional `BlockStoreGCRequest{dry_run}`. 200 on success, 400 on empty share name or malformed body, 404 on `ErrShareNotFound`, 500 on nil runtime or unexpected runtime errors.
  - **GCStatus** — `GET` handler. Reads `<gcStateRoot>/last-run.json`, decodes into `engine.GCRunSummary`, returns it as JSON. 404 when the share is unknown OR when `gcStateRoot` is empty (in-memory backend) OR when no run has completed (file does not exist). 500 on filesystem or parse failures.
- Routes mounted in `pkg/controlplane/api/router.go` under the existing per-share `/api/v1/shares/{name}/blockstore/` prefix:
  - `POST /api/v1/shares/{name}/blockstore/gc`
  - `GET  /api/v1/shares/{name}/blockstore/gc-status`

### apiclient
- `apiclient.BlockStoreGCOptions{DryRun}` — request body type.
- `apiclient.BlockStoreGCResult{Stats *engine.GCStats}` — response body type.
- `Client.BlockStoreGC(share, opts) (*BlockStoreGCResult, error)` — nil opts maps to `dry_run=false`.
- `Client.BlockStoreGCStatus(share) (*engine.GCRunSummary, error)` — surfaces 404 as `*APIError` with `IsNotFound() == true` so callers can detect the "no run yet" state without string matching.

### dfsctl
- `dfsctl store block gc <share> [--dry-run] [-o json|yaml|table]` — triggers the run, prints a key/value summary table (Run ID, Hashes Marked, Objects Swept, Bytes Freed, Duration, Errors, Dry Run). With `--dry-run` and/or non-empty `DryRunCandidates`, also prints the candidate orphan-key listing.
- `dfsctl store block gc-status <share> [-o json|yaml|table]` — reads `last-run.json` and prints the parsed `GCRunSummary`. On 404 returns `ErrNoGCRunYet` so cobra emits a non-zero exit and scripts can branch via `errors.Is(err, block.ErrNoGCRunYet)`.

## Commits (signed, GitHub-verified)

- `91e4b9c0` — `feat(11-07): REST RunGC + GCStatus handlers, apiclient methods, per-share gc-state root`
- `fc1d8f85` — `feat(11-07): dfsctl store block gc + gc-status subcommands`
- `1c03d4b2` — `docs(11-07): align handler doc-comments with actual mounted route paths`

## Verification

```bash
$ go vet ./...                                                     # clean
$ go build ./...                                                   # clean
$ go test -short -count=1 \
    ./cmd/dfsctl/... \
    ./internal/controlplane/api/... \
    ./pkg/apiclient/... \
    ./pkg/controlplane/runtime/...
ok  ...                                                            # all pass
```

Spec-mandated manual smoke:

```bash
$ go build -o /tmp/dfsctl ./cmd/dfsctl
$ /tmp/dfsctl store block --help | grep -E 'gc|gc-status'
  gc          Run garbage collection for a block store share
  gc-status   Show the last block-store GC run summary for a share
$ /tmp/dfsctl store block gc --help | grep dry-run
      --dry-run   Run mark + sweep enumeration but skip deletes; print candidate keys
$ /tmp/dfsctl store block gc-status --help | grep -i 'share <share>'
Usage:
  dfsctl store block gc-status <share> [flags]
```

## Test inventory

10 handler tests (`internal/controlplane/api/handlers/block_gc_test.go`):
- `TestBlockStoreHandler_RunGC_Success_NotDryRun` — happy path; 200 + recorded call args
- `TestBlockStoreHandler_RunGC_DryRunPropagates` — `dry_run=true` reaches runtime; `DryRunCandidates` round-trip
- `TestBlockStoreHandler_RunGC_EmptyBody` — empty body → `dry_run=false`
- `TestBlockStoreHandler_RunGC_MalformedBody` — 400 on bad JSON; runtime never invoked
- `TestBlockStoreHandler_RunGC_ShareNotFound` — `shares.ErrShareNotFound`-wrapped error → 404
- `TestBlockStoreHandler_RunGC_EmptyShareName` — empty `{name}` → 400
- `TestBlockStoreHandler_RunGC_NilRuntime` — fail-closed 500
- `TestBlockStoreHandler_GCStatus_Success` — pre-seed real `last-run.json` via `engine.PersistLastRunSummary`, round-trip via handler
- `TestBlockStoreHandler_GCStatus_NoRunYet` — empty dir → 404
- `TestBlockStoreHandler_GCStatus_EmptyRoot` — empty `gcStateRoot` (in-memory backend) → 404
- `TestBlockStoreHandler_GCStatus_ShareNotFound` — wrapped error → 404
- `TestBlockStoreHandler_GCStatus_MalformedFile` — corrupt JSON → 500
- `TestBlockStoreHandler_GCStatus_NilRuntime` — fail-closed 500

(13 tests in total, listed condensed above.)

4 apiclient round-trip tests (`pkg/apiclient/blockstore_gc_test.go`):
- `TestBlockStoreGC_RoundTrip` — verifies URL, method, body decode, response decode
- `TestBlockStoreGC_NilOpts` — nil opts → `dry_run=false`
- `TestBlockStoreGCStatus_RoundTrip` — verifies URL, method, summary round-trip
- `TestBlockStoreGCStatus_NotFound` — 404 surfaces as `APIError.IsNotFound() == true`

7 CLI tests (`cmd/dfsctl/commands/store/block/gc_test.go`):
- `TestGCCmd_CallsClient_AndPrintsSummary` — happy path
- `TestGCCmd_DryRunFlag` — `--dry-run` propagates; candidates print
- `TestGCCmd_NoArg_Errors` — `cobra.ExactArgs(1)` gate
- `TestGCStatusCmd_PrintsSummary` — happy path
- `TestGCStatusCmd_NoRunYet` — `errors.Is(err, ErrNoGCRunYet)` branch
- `TestGCStatusCmd_NoArg_Errors` — `cobra.ExactArgs(1)` gate
- `TestGCCmd_HelpListsDryRun` — regression guard against losing the flag

## Deviations from Plan

### 1. [Rule 3 - Blocking issue] REST routes mounted under /shares/{name}/blockstore/, not /store/block/{name}/

- **Found during:** Task 1, attempting to register `r.Post("/block/{name}/gc", ...)` inside the existing `r.Route("/store", ...)` group.
- **Issue:** The plan specified `POST /api/v1/store/block/{name}/gc` and `GET /api/v1/store/block/{name}/gc-status`. The pre-existing `r.Route("/block/{kind}", ...)` sub-router consumes the same first segment after `/block/`, where `kind ∈ {local, remote}`. Adding `/block/{name}/gc` at the same level produces a chi v5 routing conflict — chi cannot disambiguate two differently-named wildcards (`{kind}` vs `{name}`) on the same path segment. A request to `/block/local/gc` would be ambiguous (kind=local with sub-route gc, or share=local with gc subcommand?). chi v5 panics at registration time.
- **Fix:** Mount the GC routes under the existing per-share prefix `/api/v1/shares/{name}/blockstore/` — the same convention used by `/api/v1/shares/{name}/blockstore/stats` and `/api/v1/shares/{name}/blockstore/evict` (plan 11-06 / earlier phases). Updated the apiclient URLs to match. The semantics are unchanged — `{name}` still scopes the run summary to the named share, cross-share aggregation still spans every share that shares the remote.
- **Files modified:** `pkg/controlplane/api/router.go`, `pkg/apiclient/blockstore.go`, `internal/controlplane/api/handlers/block_gc.go` (doc comments).
- **Commit:** Bundled into `91e4b9c0`; doc-comment alignment in `1c03d4b2`.

### 2. [Rule 2 - Auto-add missing critical functionality] Per-share gc-state root tracking

- **Found during:** Task 1, designing the `GCStatus` handler.
- **Issue:** The plan's pseudocode assumed `Runtime.GetLocalStoreDir(ctx, share)` exists, but no such method or per-share local-store-dir cache existed. `Runtime.RunBlockGC` previously called `engine.CollectGarbage` with `Options.GCStateRoot` unset, which made the GC use a temp dir and skip `last-run.json` persistence entirely (`engine.PersistLastRunSummary` is a no-op on empty rootDir). Without persistence, the GCStatus endpoint has nothing to read.
- **Fix:** Added a `gcStateRoot` field to `shares.Share`, populated at share creation by a new `deriveGCStateRoot` helper that mirrors the fs path layout used in `CreateLocalStoreFromConfig` (`<basePath>/shares/<sanitized>/gc-state`). Added `Service.GetGCStateDirForShare`, `Runtime.GCStateDirForShare`, and a new `Runtime.RunBlockGCForShare` that wires `gcStateRoot` into `engine.Options.GCStateRoot` so each run writes its summary under the share's persistent root.
- **Files added:** `pkg/controlplane/runtime/shares/service.go` (Share field, GetGCStateDirForShare, deriveGCStateRoot), `pkg/controlplane/runtime/blockgc.go` (RunBlockGCForShare, GCStateDirForShare).

### 3. [Rule 3 - Blocking issue] CLI status command uses sentinel error, not os.Exit

- **Found during:** Task 2, writing CLI tests.
- **Issue:** The plan suggested `dfsctl store block gc-status <share>` should return exit 1 with a friendly message when no run has occurred. An initial implementation called `os.Exit(1)` directly inside `RunE` — but unit tests invoking `runBlockStoreGCStatus` would terminate the test binary.
- **Fix:** Defined `block.ErrNoGCRunYet` sentinel, returned from `RunE` when the apiclient surfaces a 404. Cobra's RunE-error path already produces a non-zero exit, and tests can branch on `errors.Is(err, ErrNoGCRunYet)`.
- **Files modified:** `cmd/dfsctl/commands/store/block/gc_status.go`.

## Honors CONTEXT.md

- D-08 (CLI + REST shipped, both backed by Runtime entry point) — ✓
- D-09 (`--dry-run` flag and `dry_run` REST body field, both honored end-to-end) — ✓
- D-10 (`gc-status` reads persisted `last-run.json`; `gcStateRoot` wired through Runtime) — ✓
- D-03 (cross-share aggregation preserved via existing `DistinctRemoteStores`) — ✓ (RunBlockGCForShare delegates to the same plumbing)

## Carries Forward

- Plan 11-08 (canonical E2E `TestBlockStoreImmutableOverwrites`): can drive `dfsctl store block gc` directly to assert the OLD CAS key is reaped.
- Future docs/CLI.md update (deferred per plan 11-CONTEXT D-34) should reflect the actual mount path `/api/v1/shares/{name}/blockstore/{gc,gc-status}`.

## Self-Check: PASSED

- All commits exist:
  - `91e4b9c0` (handlers + apiclient + runtime + shares)
  - `fc1d8f85` (CLI commands)
  - `1c03d4b2` (doc-comment alignment)
- All created files exist:
  - `internal/controlplane/api/handlers/block_gc.go`
  - `internal/controlplane/api/handlers/block_gc_test.go`
  - `pkg/apiclient/blockstore_gc_test.go`
  - `cmd/dfsctl/commands/store/block/gc.go`
  - `cmd/dfsctl/commands/store/block/gc_status.go`
  - `cmd/dfsctl/commands/store/block/gc_test.go`
- All modified files compile and tests pass under `-race -count=1`.
- `go vet ./...` clean.
- `go build ./...` clean.
