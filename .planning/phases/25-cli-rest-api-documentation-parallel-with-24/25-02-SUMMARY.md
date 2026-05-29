---
phase: 25-cli-rest-api-documentation-parallel-with-24
plan: 02
subsystem: api
tags: [snapshot, rest, handler, problem-json, http-timeout]

requires:
  - phase: 25-cli-rest-api-documentation-parallel-with-24
    plan: 01
    provides: pkg/controlplane/api/dto wire types; Runtime.GetSnapshot/ListSnapshots/DeleteSnapshot wrappers; RestoreSnapshot returning (safetyID, err)

provides:
  - "SnapshotHandler + SnapshotRuntime interface + mapSnapshotError single sentinel-to-HTTP table"
  - "5 REST routes under /api/v1/shares/{name}/snapshots inside the existing admin group"
  - "WriteJSONAccepted (202), PreconditionFailed (412), GatewayTimeout (504) problem helpers"
  - "snapshot.restore_http_timeout YAML knob (default 30m) bounding the restore handler context"
  - "api.NewServer / api.NewRouter extended with restoreHTTPTimeout argument"
  - "controlplane.Options.RestoreHTTPTimeout for library-mode callers; api.DefaultRestoreHTTPTimeout constant"

affects:
  - 25-03-PLAN (apiclient + CLI — has typed REST surface to call)
  - 25-04-PLAN (docs — REST reference section in SNAPSHOTS.md)

tech-stack:
  added: []
  patterns:
    - "Single error-to-HTTP mapper helper (mapSnapshotError) — first canonical mapper for a typed-sentinel cluster"
    - "Sanitized error responses: typed sentinels map to fixed operator-friendly strings; underlying err only reaches slog at Debug"
    - "Per-request context timeout for long-running synchronous handlers, plumbed through NewServer/NewRouter"

key-files:
  created:
    - internal/controlplane/api/handlers/snapshot.go
    - internal/controlplane/api/handlers/snapshot_test.go
    - internal/controlplane/api/handlers/problem_test.go
    - test/e2e/snapshot/main_test.go
    - test/e2e/snapshot/snapshot_http_test.go
  modified:
    - internal/controlplane/api/handlers/problem.go
    - pkg/controlplane/api/router.go
    - pkg/controlplane/api/server.go
    - pkg/controlplane/api/server_test.go
    - pkg/controlplane/controlplane.go
    - pkg/config/config.go
    - pkg/config/snapshot_test.go
    - cmd/dfs/commands/start.go

key-decisions:
  - "ErrSnapshotStateConflict added to the mapping table as 409 Conflict — the runtime returns it from RestoreSnapshot when the snapshot is not in StateReady, so the handler must classify it (plan listed 14 sentinels; 15th covers the not-ready precondition explicitly)"
  - "NewRouter/NewServer accept restoreHTTPTimeout as a positional argument rather than extending APIConfig — keeps snapshot config out of the HTTP-server config namespace and avoids a circular dep between pkg/config and pkg/controlplane"
  - "controlplane.Options.RestoreHTTPTimeout defaults to api.DefaultRestoreHTTPTimeout when zero so library-mode callers can omit it without breaking; the dfs binary always passes cfg.Snapshot.RestoreHTTPTimeout (which itself defaults to 30m in ApplyDefaults)"
  - "e2e suite uses a fault-injecting fake SnapshotRuntime mounted on a chi router that mirrors the production route layout; JWT + RequireAdmin middleware is deliberately omitted since dedicated middleware tests already cover that layer and the focus here is handler/router behavior under every D-24-13 failure-mode sentinel"
  - "RestoreSnapshotResponse.SafetySnapshotID is sourced DIRECTLY from Runtime.RestoreSnapshot's first return value — never via a ListSnapshots filter — and the e2e happy path asserts non-empty value"
  - "Handler imports dto types from pkg/controlplane/api/dto; no local DTO redeclaration (grep 'type Snapshot struct' on the handler file returns 0)"

requirements-completed:
  - API-01

# Metrics
duration: ~75min
completed: 2026-05-29
---

# Phase 25 Plan 02: REST handlers + SnapshotRuntime interface + mapSnapshotError + router wiring + restore-timeout YAML knob + HTTP e2e

Ships the snapshot REST API: five chi handlers wired under the existing `/api/v1/shares/{name}/snapshots` admin group, a single `mapSnapshotError` sentinel→HTTP table covering 15 typed errors, three new `problem.go` helpers (202/412/504), and an end-to-end HTTP test that exercises every endpoint plus all nine documented restore failure modes via fault injection.

## Final HTTP-mapping table (as shipped)

| Sentinel | Status | Sanitized message |
|---|---|---|
| `models.ErrSnapshotNotFound` | 404 | snapshot not found |
| `shares.ErrShareNotFound` | 404 | share not found |
| `models.ErrShareEnabled` | 409 Conflict | share is enabled; disable before restore |
| `models.ErrSnapshotNotDurable` | 412 Precondition Failed | snapshot not remotely durable; pass allow_non_durable=true to force |
| `models.ErrSnapshotRetryTargetNotFound` | 404 | retry target snapshot not found |
| `models.ErrSnapshotRetryTargetNotFailed` | 409 Conflict | retry target is not in failed state |
| `models.ErrSnapshotStateConflict` | 409 Conflict | snapshot is not in a state that allows this operation |
| `models.ErrSnapshotDrainTimeout` | 504 Gateway Timeout | upload drain timed out |
| `models.ErrSnapshotMetadataDumpMissing` | 500 | snapshot artifacts missing |
| `models.ErrMetadataStoreNotResetable` | 500 | backend does not support reset |
| `models.ErrSnapshotBackupFailed` | 500 | snapshot operation failed |
| `models.ErrSnapshotVerifyFailed` | 500 | snapshot operation failed |
| `models.ErrRestoreSafetySnapFailed` | 500 | snapshot operation failed |
| `models.ErrRestoreAborted` | 500 | snapshot operation failed |
| `models.ErrRestoreVerifyFailed` | 500 | snapshot operation failed |
| Unmapped / unknown error | 500 | (handler writes its own sanitized fallback, e.g. "snapshot restore failed") |

All sentinels match via `errors.Is`, so wrapped errors carrying `fmt.Errorf("...: %w", sentinel)` resolve correctly. Original `err` is never interpolated into the response body; verbose detail reaches `slog.Debug` for operator postmortems.

## NewRouter / NewServer signature changes

```go
// Before
func NewRouter(rt *runtime.Runtime, jwtService *auth.JWTService, cpStore store.Store, pprofEnabled bool) http.Handler
func NewServer(config APIConfig, rt *runtime.Runtime, cpStore store.Store) (*Server, error)

// After
func NewRouter(rt *runtime.Runtime, jwtService *auth.JWTService, cpStore store.Store, pprofEnabled bool, restoreHTTPTimeout time.Duration) http.Handler
func NewServer(config APIConfig, rt *runtime.Runtime, cpStore store.Store, restoreHTTPTimeout time.Duration) (*Server, error)
```

A new `api.DefaultRestoreHTTPTimeout = 30 * time.Minute` constant lives next to `NewServer` so library-mode callers in `pkg/controlplane` can resolve a sensible fallback when their `Options.RestoreHTTPTimeout` is zero without taking a dep on `pkg/config`.

## NewServer / NewRouter call sites updated

- `cmd/dfs/commands/start.go:222` — `api.NewServer(cfg.ControlPlane, rt, cpStore, cfg.Snapshot.RestoreHTTPTimeout)`
- `pkg/controlplane/controlplane.go:88` — `api.NewServer(*opts.API, rt, cpStore, timeout)` where `timeout = opts.RestoreHTTPTimeout` (or `api.DefaultRestoreHTTPTimeout` when zero)
- `pkg/controlplane/api/server.go:79` — internal `NewRouter(rt, jwtService, cpStore, config.Pprof, restoreHTTPTimeout)` consumer of the new arg
- `pkg/controlplane/api/server_test.go` — 6 `NewServer(cfg, nil, cpStore)` call sites flipped to `NewServer(cfg, nil, cpStore, 30*time.Minute)` so the package's existing server boot tests keep compiling

## Fault-injection technique per restore failure mode

The e2e suite in `test/e2e/snapshot/snapshot_http_test.go` drives a `fakeRuntime` test double that satisfies `handlers.SnapshotRuntime`. Each failure mode wires a hook on the fake that returns the expected sentinel; no mode uses `t.Skip`.

| # | Failure mode | Injection point | Expected HTTP |
|---|---|---|---|
| 1 | share enabled | `restore` hook returns `fmt.Errorf("... %w", models.ErrShareEnabled)` | 409 Conflict |
| 2 | snapshot not found | `restore` hook returns `models.ErrSnapshotNotFound` directly | 404 |
| 3 | snapshot not durable + no override | seeded snap with `RemoteDurable=false`; default `restore` (no hook) returns `ErrSnapshotNotDurable` when `opts.AllowNonDurable=false` | 412 |
| 4 | snapshot not durable + `allow_non_durable=true` | same seed as #3; request body sets `AllowNonDurable=true` so the default `restore` path emits a synthetic `safetyID` and 200 | 200 |
| 5 | metadata dump missing | `restore` hook returns `fmt.Errorf("open dump: %w", models.ErrSnapshotMetadataDumpMissing)` | 500 ("snapshot artifacts missing") |
| 6 | metadata store not resetable | `restore` hook returns `fmt.Errorf("reset: %w", models.ErrMetadataStoreNotResetable)` | 500 ("backend does not support reset") |
| 7 | safety-snap create failure | `restore` hook returns `fmt.Errorf("safety snap: %w", models.ErrRestoreSafetySnapFailed)` | 500 |
| 8 | restore aborted (mid-flight) | `restore` hook waits on `ctx.Done()` briefly then returns `fmt.Errorf("reset aborted: %w", models.ErrRestoreAborted)` — exercises the handler's `context.WithTimeout` wrap | 500 |
| 9 | post-verify failure | `restore` hook returns `fmt.Errorf("post-verify: %w", models.ErrRestoreVerifyFailed)` | 500 |

Each row of `TestSnapshotHTTP_RestoreFailureModes` further asserts a substring of the sanitized message survives the wrapping path, proving the typed sentinel propagates through `errors.Is` rather than being swallowed by an outer wrap.

## Task Commits

1. **Task 1: 412/504/202 problem helpers + `snapshot.restore_http_timeout` YAML knob** — `2c573f3a` (feat)
2. **Task 2: `SnapshotHandler` + `SnapshotRuntime` interface + `mapSnapshotError` + router wiring** — `d234882b` (feat)
3. **Task 3: HTTP e2e covering 5 endpoints + 9 restore failure modes** — `17823a3f` (test)

## Files Created/Modified

**Created:**
- `internal/controlplane/api/handlers/snapshot.go` — `SnapshotHandler`, `SnapshotRuntime`, `mapSnapshotError`, `toWire`. ~330 lines.
- `internal/controlplane/api/handlers/snapshot_test.go` — fake-runtime-driven unit tests covering every handler + every sentinel mapping + context-timeout propagation + body-sanitization. ~340 lines.
- `internal/controlplane/api/handlers/problem_test.go` — table tests for the four newly-added helpers (202/412/504 + a sanity check on the pre-existing `Conflict`).
- `test/e2e/snapshot/main_test.go` — `e2e` build tag + placeholder `TestMain`.
- `test/e2e/snapshot/snapshot_http_test.go` — happy path (create → list → get → restore → delete → 404) + 9 D-24-13 restore failure modes + create-failure sentinels.

**Modified:**
- `internal/controlplane/api/handlers/problem.go` — appended `WriteJSONAccepted`, `PreconditionFailed`, `GatewayTimeout`. `Conflict` was already present from earlier work.
- `pkg/controlplane/api/router.go` — `NewRouter` grew a `restoreHTTPTimeout time.Duration` arg; new `r.Route("/{name}/snapshots", ...)` block inside the existing `/shares` admin group after `/disable` + `/enable`.
- `pkg/controlplane/api/server.go` — `NewServer` grew the same arg; added `DefaultRestoreHTTPTimeout` constant.
- `pkg/controlplane/api/server_test.go` — six `NewServer(...)` call sites updated.
- `pkg/controlplane/controlplane.go` — `Options.RestoreHTTPTimeout` field; falls back to `api.DefaultRestoreHTTPTimeout` when zero.
- `pkg/config/config.go` — `SnapshotConfig.RestoreHTTPTimeout` with YAML key `restore_http_timeout`; `ApplyDefaults` writes 30m; `Validate` rejects negative.
- `pkg/config/snapshot_test.go` — added default-applied, YAML round-trip, and negative-rejected cases; kept the empty-config sanity check.
- `cmd/dfs/commands/start.go` — `api.NewServer` call passes `cfg.Snapshot.RestoreHTTPTimeout`.

## Decisions Made

See `key-decisions` in frontmatter. The notable ones:

- **`ErrSnapshotStateConflict` added to the mapping table (15th sentinel).** The runtime returns it when callers attempt to restore a snapshot whose `State` is not `StateReady` (e.g. `creating` or `failed`). The plan listed 14 sentinels; this 15th is a legitimate restore precondition failure and maps cleanly to 409 Conflict alongside `ErrShareEnabled`. Otherwise it would have fallen through to a generic 500, leaking the underlying state mismatch as an opaque error.
- **`restoreHTTPTimeout` plumbed as a positional argument, not via APIConfig.** Keeps the snapshot config inside `pkg/config` (which already imports `pkg/controlplane/api`) without forcing the `api` package to depend on `pkg/config` (which would be a circular dep). The library entrypoint `controlplane.Options.RestoreHTTPTimeout` documents the surface for non-`dfs` consumers.
- **e2e suite mounts SnapshotHandler directly (no JWT, no full middleware).** The plan's intent — "exercises all 5 endpoints + all 9 D-24-13 failure modes" — is about handler/router behavior under each sentinel. JWT + `RequireAdmin` are independently tested in `internal/controlplane/api/middleware/auth_test.go`; reproducing that wiring in the snapshot e2e would have added ~150 lines of fixture for zero additional handler coverage.

## Deviations from Plan

1. **[Rule 2 - Critical sentinel coverage] Added `ErrSnapshotStateConflict` to `mapSnapshotError`.** The plan listed 14 sentinels but `Runtime.RestoreSnapshot` returns `ErrSnapshotStateConflict` when the snapshot is not in `StateReady` (state precondition the planner overlooked). Without this, attempting to restore a `creating` or `failed` snapshot would 500 with a generic message instead of cleanly conveying the precondition. Mapped to 409 Conflict alongside the other "wrong state" sentinels. Files: `internal/controlplane/api/handlers/snapshot.go`, `_test.go`. Commit: `d234882b`.
2. **[Rule 3 - Blocking issue] Updated six `NewServer` call sites in `pkg/controlplane/api/server_test.go`** so the package tests still compile after the signature change. The plan said "the handful of tests" but the count was 6; identified and fixed in the Task 2 commit so `go test ./...` stays green. Commit: `d234882b`.
3. **[Rule 3 - Blocking issue] Extended `controlplane.Options` and `controlplane.New`** to plumb the restore timeout through the library entrypoint (`pkg/controlplane/controlplane.go`), not just the `cmd/dfs` binary path. The plan called this out implicitly via "update every existing `NewRouter` call site" but the controlplane.go path was not enumerated; without this change the library-mode boot would have failed to compile. Mitigated by introducing `api.DefaultRestoreHTTPTimeout` so the existing zero-value default keeps working. Commit: `d234882b`.

## Issues Encountered

None. All three commits landed cleanly; tests passed first time after each TDD GREEN step.

## User Setup Required

None.

## Verification

- `go test ./internal/controlplane/api/handlers/ -count=1` — passes (unit tests + table-driven sentinel coverage).
- `go test ./pkg/config/ -count=1` — passes (RestoreHTTPTimeout default + round-trip + negative reject).
- `go test -tags=e2e ./test/e2e/snapshot/ -count=1 -timeout=60s` — passes (happy path + 9 restore failure modes + 3 create-failure modes).
- `go build ./...` — exit 0.
- `go vet ./...` — exit 0.
- `gofmt -s -l .` — clean.
- `grep -c "mapSnapshotError" internal/controlplane/api/handlers/snapshot.go` — 7 (one per handler error path + the function definition; required ≥ 6).
- `grep -n 'r.Route("/{name}/snapshots"' pkg/controlplane/api/router.go` — single match inside the admin group at line 203.
- `grep -cE 'dto\.(Snapshot|CreateSnapshotResponse|RestoreSnapshotResponse|CreateSnapshotRequest|RestoreSnapshotRequest)' internal/controlplane/api/handlers/snapshot.go` — 9 (required ≥ 4).
- `grep -c "type Snapshot struct" internal/controlplane/api/handlers/snapshot.go` — 0 (no local DTO redeclaration).
- `grep -rEn 'D-[0-9]+-[0-9]+|Phase [0-9]+|per Phase'` on all new/edited files — 0 matches.

## Self-Check: PASSED

- `internal/controlplane/api/handlers/snapshot.go` — FOUND
- `internal/controlplane/api/handlers/snapshot_test.go` — FOUND
- `internal/controlplane/api/handlers/problem_test.go` — FOUND
- `test/e2e/snapshot/snapshot_http_test.go` — FOUND
- `test/e2e/snapshot/main_test.go` — FOUND
- Commit `2c573f3a` (Task 1) — FOUND in git log
- Commit `d234882b` (Task 2) — FOUND in git log
- Commit `17823a3f` (Task 3) — FOUND in git log
- `go build ./...` exit 0
- `go vet ./...` exit 0
- `go test ./internal/controlplane/api/handlers/... ./pkg/controlplane/api/... ./pkg/config/...` all pass
- `go test -tags=e2e ./test/e2e/snapshot/...` all pass
- `gofmt -s -l .` clean
- No GSD metadata in any prescribed file

---
*Phase: 25-cli-rest-api-documentation-parallel-with-24*
*Completed: 2026-05-29*
