---
phase: 05-restore-orchestration-safety-rails
plan: 09
subsystem: controlplane
tags: [nfs, nfsv4, smb, observability, prometheus, otel, rest-02, d-02, d-19, d-20]

requires:
  - phase: 05-restore-orchestration-safety-rails
    provides: Plan 05-01 Share.Enabled column + shares.Service.DisableShare/EnableShare; Plan 05-07 RunRestore REST-02 pre-flight gate (control-plane side)
provides:
  - REST-02 adapter-side enforcement at NFS MOUNT (MNT3ERR_ACCES/MountErrAccess)
  - REST-02 adapter-side enforcement at NFSv4 PUTFH (NFS4ERR_STALE)
  - REST-02 adapter-side enforcement at SMB TREE_CONNECT (STATUS_NETWORK_NAME_DELETED)
  - storebackups.MetricsCollector interface + NoopMetrics + WithMetricsCollector option
  - storebackups.Tracer interface + NoopTracer + OTelTracer concrete + WithTracer option
  - backup_operations_total{kind,outcome} counter on every RunBackup / RunRestore exit
  - backup_last_success_timestamp_seconds{repo_id,kind} gauge update on success
  - classifyOutcome helper mapping ctx.Canceled/DeadlineExceeded → interrupted
  - Propagation of share.Enabled from DB model → runtime ShareConfig at init (Rule 3 auto-fix)
affects: [05-10, 06-cli-rest-api, 07-testing]

tech-stack:
  added: []
  patterns:
    - "Adapter gate pattern: load share → check Enabled → protocol-specific refusal code with WARN log"
    - "Noop observability collectors as zero-overhead default; Options inject real collectors"
    - "Single-shot spans per long-running operation (backup.run / restore.run) not per-step"
    - "Outcome classifier derived from final error (nil → succeeded, ctx.Cancel/Deadline → interrupted, else failed)"

key-files:
  created:
    - pkg/controlplane/runtime/storebackups/metrics.go
    - pkg/controlplane/runtime/storebackups/metrics_test.go
    - internal/adapter/nfs/mount/handlers/mount_test.go
    - internal/adapter/nfs/v4/handlers/putfh_test.go
  modified:
    - internal/adapter/nfs/mount/handlers/mount.go
    - internal/adapter/nfs/v4/handlers/putfh.go
    - internal/adapter/smb/v2/handlers/tree_connect.go
    - internal/adapter/smb/v2/handlers/tree_connect_test.go
    - pkg/controlplane/runtime/storebackups/service.go
    - pkg/controlplane/runtime/storebackups/restore.go
    - pkg/controlplane/runtime/init.go

key-decisions:
  - "PUTFH gate resolves share via Registry.GetShareNameForHandle; handles that don't decode to a known share fall through (pseudo-fs / boot-verifier flows stay permissive). Refused handles return NFS4ERR_STALE with WARN log per MS-FSA advisory."
  - "SMB TREE_CONNECT gate returns StatusNetworkNameDeleted (MS-SMB2 2.2.9) — the spec error for a share that existed but is no longer available. Matches the REST-02 'disconnects and refuses new connections' contract."
  - "Observability collectors are set once at construction via Options (WithMetricsCollector / WithTracer) and accessed without a mutex on the hot path — the fields are immutable after New returns. Defaults are NoopMetrics + NoopTracer so callers that never wire observability pay zero overhead."
  - "PromMetrics concrete Prometheus implementation is deliberately omitted. `prometheus/client_golang` is not in go.mod and Plan 05-09 guardrails prohibit adding new top-level dependencies. The MetricsCollector interface is the shipped contract; Phase 7 (or whichever plan promotes Prometheus) adds the concrete type."
  - "OTelTracer concrete implementation uses go.opentelemetry.io/otel/trace which is already present in go.mod (as indirect). Using it in storebackups promotes to direct but adds no new module — still honors the no-new-deps rule."
  - "classifyOutcome helper lives in service.go rather than metrics.go because the classification reflects the Service's error contract, not the collector's concerns."

patterns-established:
  - "Adapter gate placement: AFTER share existence + permission checks, BEFORE any handle issuance or state mutation. Refusal must log WARN with share name + client identifier."
  - "Observability field access: set-once via Options, read direct on hot path, default to Noop in New(). No mutex overhead on every RunBackup/RunRestore."
  - "Named return values on RunBackup/RunRestore so the deferred metrics hook observes the final err (succeeded/failed/interrupted)."

requirements-completed: [REST-02]

duration: ~20 min
completed: 2026-04-17
---

# Phase 5 Plan 9: REST-02 Adapter Gates + Minimal Observability Summary

**Three protocol-layer gates (NFS MOUNT → MNT3ERR_ACCES, NFSv4 PUTFH → NFS4ERR_STALE, SMB TREE_CONNECT → STATUS_NETWORK_NAME_DELETED) refuse disabled shares; Service.RunBackup + RunRestore emit backup_operations_total counter + backup_last_success_timestamp_seconds gauge + OTel span per terminal state via a Noop-by-default MetricsCollector/Tracer.**

## Performance

- **Duration:** ~20 minutes
- **Started:** 2026-04-17T01:02:00Z
- **Completed:** 2026-04-17T01:22:00Z
- **Tasks:** 3 (TDD: RED → GREEN per task)
- **Files created:** 4
- **Files modified:** 7

## Accomplishments

- **NFS MOUNT gate** at `internal/adapter/nfs/mount/handlers/mount.go:115` — consults `share.Enabled` after existing share-lookup/auth flow, returns `MountErrAccess` (13 = MNT3ERR_ACCES) with WARN log `"NFS MOUNT refused: share disabled"`. Existing tests unaffected (first mount_test.go in the package).
- **NFSv4 PUTFH gate** at `internal/adapter/nfs/v4/handlers/putfh.go:51-64` — resolves share via `Registry.GetShareNameForHandle`; if the handle decodes to a known share AND that share is disabled, returns `NFS4ERR_STALE` with WARN log. Handles that don't decode to known shares fall through (pseudo-fs / legacy paths stay permissive). Clients reacquire fresh handles after restore + explicit re-enable.
- **SMB TREE_CONNECT gate** at `internal/adapter/smb/v2/handlers/tree_connect.go:97-106` — inserted after existing share-lookup and before permission resolution. Returns `StatusNetworkNameDeleted` (MS-SMB2 2.2.9) with WARN log.
- **init.go propagation (Rule 3 auto-fix)** at `pkg/controlplane/runtime/init.go:191` — added `Enabled: share.Enabled` to the `ShareConfig` struct literal. Without this, production shares loaded from the DB would hit the runtime Share struct with `Enabled=false` (Go zero value) and every MOUNT/PUTFH/TREE_CONNECT would refuse. The plan did not call this out; it became apparent when reasoning about the end-to-end REST-02 path.
- **MetricsCollector + Tracer interfaces** at `pkg/controlplane/runtime/storebackups/metrics.go` — minimal surface: `RecordOutcome(kind, outcome string)`, `RecordLastSuccess(repoID, kind string, at time.Time)`, `Start(ctx, operation) (context.Context, func(err error))`. Exported metric name constants (`backup_operations_total`, `backup_last_success_timestamp_seconds`), outcome taxonomy (`succeeded`, `failed`, `interrupted`), and span names (`backup.run`, `restore.run`).
- **Noop implementations** (default via `New()`, zero overhead when observability isn't wired) and `OTelTracer` concrete (wraps `go.opentelemetry.io/otel/trace.Tracer` — already indirect-present in go.mod).
- **Terminal-state hooks** wired into `Service.RunBackup` (`service.go:387-412`) and `Service.RunRestore` (`restore.go:72-103`): both now use named returns and a single deferred closure to record the counter + conditionally the last-success gauge + close the span. Single span per operation (no per-step fan-out) per D-19.
- **classifyOutcome helper** (`service.go:528`) maps `nil → succeeded`, `ctx.Canceled/DeadlineExceeded (errors.Is) → interrupted`, anything else → `failed`. Tested across all four branches plus wrapped-canceled.
- **7 new tests added** across 4 files, all passing. Details below.

## Task Commits

Each task was committed atomically (TDD cycles):

1. **Task 1 RED: mount disabled-share tests** — `2588a844` (test)
2. **Task 1 GREEN: NFS MOUNT gate** — `9cede0b7` (feat)
3. **Task 2 RED: PUTFH + TREE_CONNECT disabled-share tests** — `e5720cf2` (test)
4. **Task 2 GREEN: PUTFH + TREE_CONNECT gates + init.go propagation** — `6e4aba9b` (feat)
5. **Task 3: observability hooks + MetricsCollector + Tracer + OTelTracer** — `d597905a` (feat)

**Plan metadata:** pending (final `docs(05-09):` commit covers SUMMARY + STATE + ROADMAP).

## Files Created/Modified

### Created

- `internal/adapter/nfs/mount/handlers/mount_test.go` (108 lines) — first test file in the mount handlers package. `TestMount_DisabledShare_ReturnsAccess` + `TestMount_EnabledShare_AllowsMount` regression guard.
- `internal/adapter/nfs/v4/handlers/putfh_test.go` (107 lines) — `TestPUTFH_DisabledShare_ReturnsStale` + `TestPUTFH_EnabledShare_Succeeds` regression guard.
- `pkg/controlplane/runtime/storebackups/metrics.go` (122 lines) — MetricsCollector + NoopMetrics + Tracer + NoopTracer + OTelTracer + metric name + outcome taxonomy constants.
- `pkg/controlplane/runtime/storebackups/metrics_test.go` (232 lines) — covers terminal-state classification under RunBackup/RunRestore with a fake collector + fake tracer, noop paths, nil-safety.

### Modified

- `internal/adapter/nfs/mount/handlers/mount.go` — 10-line Enabled gate after GetShare (line 115).
- `internal/adapter/nfs/v4/handlers/putfh.go` — resolves share from handle via `GetShareNameForHandle`, refuses with NFS4ERR_STALE if !Enabled. Adds imports for `internal/logger` and `pkg/metadata`.
- `internal/adapter/smb/v2/handlers/tree_connect.go` — 10-line Enabled gate after GetShare (line 97).
- `internal/adapter/smb/v2/handlers/tree_connect_test.go` — added imports for metadata + memorymeta, new test block with `newTreeConnectGateHandler` helper + 2 gate tests.
- `pkg/controlplane/runtime/storebackups/service.go` — added `metrics MetricsCollector` + `tracer Tracer` fields; `WithMetricsCollector` + `WithTracer` options; Noop defaults in `New`; span + counter + gauge hooks in `RunBackup`; `classifyOutcome` + `s.now()` helpers.
- `pkg/controlplane/runtime/storebackups/restore.go` — named-return `err`; span + counter + gauge hooks in `RunRestore`.
- `pkg/controlplane/runtime/init.go` — single-line addition `Enabled: share.Enabled` in the DB-share → runtime `ShareConfig` translation (Rule 3 auto-fix — see Deviations).

## Test Outcomes

All new tests pass; full adapter + storebackups suites continue to pass.

| Test | Outcome |
|------|---------|
| TestMount_DisabledShare_ReturnsAccess | PASS — Status=MountErrAccess, empty FileHandle |
| TestMount_EnabledShare_AllowsMount | PASS — Status=MountOK, non-empty handle |
| TestPUTFH_DisabledShare_ReturnsStale | PASS — Status=NFS4ERR_STALE, CurrentFH untouched |
| TestPUTFH_EnabledShare_Succeeds | PASS — Status=NFS4_OK, CurrentFH set to handle |
| TestTreeConnect_DisabledShare_ReturnsNameDeleted | PASS — Status=StatusNetworkNameDeleted, no tree created |
| TestTreeConnect_EnabledShare_Succeeds | PASS — Status=StatusSuccess |
| TestRunRestore_Observability_FailureClassified | PASS — outcome=restore\|failed, no last_success, span opened+closed with err |
| TestRunRestore_Observability_InterruptedClassified | PASS — outcome=restore\|interrupted |
| TestRunBackup_Observability_FailureClassified | PASS — outcome=backup\|failed, span backup.run opened+closed |
| TestClassifyOutcome (5 sub-tests) | PASS — nil, Canceled, DeadlineExceeded, wrapped Canceled, other |
| TestNoopCollectors | PASS — no panic on nil/non-nil err |
| TestOTelTracer_NilSafe | PASS — NewOTelTracer(nil) returns noop-behaving tracer |

Full suite `go test ./internal/adapter/nfs/mount/... ./internal/adapter/nfs/v4/... ./internal/adapter/smb/v2/... ./pkg/controlplane/runtime/storebackups/...` → all green.

## Decisions Made

- **PUTFH gate is share-scoped, not per-path.** The handler resolves the owning share via `GetShareNameForHandle` (which decodes the opaque share prefix from the handle) and checks Enabled. Any handle that doesn't decode to a known share (pseudo-fs pseudo-handles, orphaned boot-verifier reuse) falls through — we do not turn PUTFH into a stricter gate than the plan requires.
- **SMB gate uses `NewErrorResult`.** Matches the existing error-response idiom in `tree_connect.go`. Server emits the SMB2 ERROR body via the shared helper — no new encoding path.
- **Observability accessors are lockless.** Default Noop field values are set inside `New()` before the Service reference leaves construction; subsequent writes via Options happen synchronously before `New` returns. Consumers of the Service never observe a zero value. The hot path skips `s.mu` overhead for every RunBackup/RunRestore.
- **Prometheus concrete implementation is NOT shipped this plan.** Plan 05-09's guardrail ("do NOT add new top-level deps in Phase 5") precludes adding `github.com/prometheus/client_golang` to `go.mod`. The MetricsCollector interface is the operator-visible contract; any deployment that wants Prometheus wires a concrete implementation that uses their preferred registry. This is documented in `metrics.go`'s package doc.
- **OTelTracer is shipped because otel/trace is already present** (as an indirect dep). Using it in source code promotes to direct without adding a new module.
- **Named returns on RunBackup/RunRestore.** Required so the deferred hook can read the final `err` after all return paths.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Propagate `share.Enabled` from DB model to runtime `ShareConfig` at init.**

- **Found during:** Task 2 analysis — after wiring the PUTFH/TREE_CONNECT/MOUNT gates, I traced end-to-end how production shares load their `Enabled` flag.
- **Issue:** `pkg/controlplane/runtime/init.go` builds a `ShareConfig` from the `models.Share` DB row, but did NOT copy the `Enabled` field. Plan 05-01 added `Enabled` to both `models.Share` and `runtime.ShareConfig` but the translation site at init was never updated. Without the fix, all production shares would load with `Enabled=false` (Go zero value) and every MOUNT/PUTFH/TREE_CONNECT would immediately refuse — a catastrophic adapter-level DOS for anyone upgrading.
- **Fix:** One-line addition `Enabled: share.Enabled,` in the `ShareConfig` struct literal at `pkg/controlplane/runtime/init.go:192`.
- **Files modified:** `pkg/controlplane/runtime/init.go`
- **Verification:** Full `go test ./pkg/controlplane/runtime/...` green. Existing share-service tests continue to pass. The new adapter gate tests (which set `Enabled=true` explicitly in test fixtures) are unaffected.
- **Committed in:** `6e4aba9b` (Task 2 GREEN commit — bundled with PUTFH + TREE_CONNECT gates since it's the same REST-02 logical change).

**2. [Scope-compliance] Prometheus concrete `PromMetrics` NOT shipped.**

- **Found during:** Task 3 (metrics.go authoring).
- **Issue:** Plan 05-09's acceptance criteria include `"PromMetrics exported"`, but the same plan states `"Both should already be in go.mod (inspect to confirm; if not, leave a note in the SUMMARY — do NOT add new top-level deps in Phase 5)"`. `github.com/prometheus/client_golang` is NOT in `go.mod`. The stronger instruction wins: no concrete Prometheus impl this plan.
- **Fix:** Ship only the MetricsCollector interface + NoopMetrics + Tracer interface + NoopTracer + OTelTracer (OTel already present). Document the deferral in `metrics.go`'s package comment AND in this SUMMARY. A follow-up plan (Phase 7 or explicit observability phase) is the right place to promote prometheus to a direct dep.
- **Files modified:** `pkg/controlplane/runtime/storebackups/metrics.go` (package comment documents the deferral).
- **Verification:** `go build ./...` + `go vet ./...` clean. No callers of `PromMetrics` in production — the `MetricsCollector` interface is the shipped contract.
- **Committed in:** `d597905a` (Task 3 commit).

---

**Total deviations:** 2 (1 Rule-3 blocking auto-fix, 1 scope-compliance documentation).

**Impact on plan:** Both deviations are risk-neutral — the init.go fix prevents a production outage the plan would have caused; the Prometheus deferral honors the plan's explicit no-new-deps constraint. No scope creep.

## Issues Encountered

- **SMB `tree_connect_test.go` required new imports** for `pkg/metadata` and `pkg/metadata/store/memory` to build the Registry-backed test harness. Added them to the existing import block.
- **Existing NFSv4 handler tests use `nil` registry** (via `NewHandler(nil, pfs)`). The PUTFH gate's `if h.Registry != nil` guard keeps them green; no existing test needed modification.

## Observability Scope

**What we ship now:**
- Noop-by-default MetricsCollector + Tracer; zero overhead unless operators wire concrete implementations.
- `backup_operations_total{kind,outcome}` counter — fires exactly once per terminal state (success/failed/interrupted).
- `backup_last_success_timestamp_seconds{repo_id,kind}` gauge — updated only on successful outcomes so operators can alert on "no successful backup in 2× scheduled period" (Pitfall #10).
- `backup.run` and `restore.run` OTel spans — single top-level span per operation (not per step).
- `OTelTracer` concrete implementation wrapping `go.opentelemetry.io/otel/trace.Tracer`.

**What is deferred:**
- `PromMetrics` concrete — requires adding `prometheus/client_golang` to go.mod, which Plan 05-09 explicitly forbids. Ships in a follow-up observability plan (Phase 7 candidate).
- Duration histograms, in-flight gauges, retention counters, byte-throughput metrics (Plan 05-09 D-19 explicitly scoped them out; Phase 7 may extend).
- Telemetry/metrics config wiring (`server.metrics.enabled`, `telemetry.enabled`) into the runtime composition site — Plan 05-09 says "gated by existing flags" but those flags are documented in CLAUDE.md, not yet implemented in `pkg/config`. Concrete gate wiring happens in whichever plan lands the runtime composition site.

## Block-Store GC Wiring

**Plan 10 boundary honored.** This plan does NOT modify `pkg/blockstore/gc/` and does NOT construct any `gc.Options.BackupHold`. Verified by `grep -rn 'gc\.Options\{|BackupHold:' pkg/ cmd/` — the only references are inside `pkg/blockstore/gc/gc_test.go` (Plan 08's test harness) and `pkg/blockstore/gc/doc.go` (doc comment). No production caller exists. SAFETY-01 end-to-end wiring (provider → GC invocation) lands in Plan 05-10.

## Verification Results

- `go build ./...` → clean
- `go vet ./internal/adapter/... ./pkg/controlplane/runtime/storebackups/...` → clean
- `go test ./internal/adapter/nfs/mount/... ./internal/adapter/nfs/v4/... ./internal/adapter/smb/v2/... -count=1` → PASS
- `go test ./pkg/controlplane/runtime/storebackups/... -count=1` → PASS (existing + 7 new metrics tests)
- `go test ./pkg/controlplane/runtime/... -count=1` → PASS (init.go propagation verified indirectly)

## Self-Check: PASSED

All claimed files exist and contain the claimed contents:
- `internal/adapter/nfs/mount/handlers/mount.go` → contains `!share.Enabled` gate + `MountErrAccess` return + WARN log.
- `internal/adapter/nfs/mount/handlers/mount_test.go` → created with `TestMount_DisabledShare_ReturnsAccess`.
- `internal/adapter/nfs/v4/handlers/putfh.go` → contains `NFS4ERR_STALE` + `share disabled` log + imports for logger + metadata.
- `internal/adapter/nfs/v4/handlers/putfh_test.go` → created with `TestPUTFH_DisabledShare_ReturnsStale`.
- `internal/adapter/smb/v2/handlers/tree_connect.go` → contains `StatusNetworkNameDeleted` + `share disabled` log.
- `internal/adapter/smb/v2/handlers/tree_connect_test.go` → contains `TestTreeConnect_DisabledShare_ReturnsNameDeleted`.
- `pkg/controlplane/runtime/storebackups/metrics.go` → exists, contains `backup_operations_total` + `backup_last_success_timestamp_seconds` + MetricsCollector + NoopMetrics + Tracer + NoopTracer + OTelTracer.
- `pkg/controlplane/runtime/storebackups/service.go` → contains `s.tracer.Start` + `s.metrics.RecordOutcome` + classifyOutcome.
- `pkg/controlplane/runtime/storebackups/restore.go` → contains `s.tracer.Start` + `s.metrics.RecordOutcome`.
- `pkg/controlplane/runtime/init.go` → contains `Enabled: share.Enabled` in ShareConfig literal.

All commits present in `git log`:
- `2588a844` test(05-09): add failing MNT disabled-share gate tests
- `9cede0b7` feat(05-09): gate NFS MOUNT on share.Enabled (REST-02)
- `e5720cf2` test(05-09): add failing PUTFH + TREE_CONNECT disabled-share gate tests
- `6e4aba9b` feat(05-09): gate NFSv4 PUTFH + SMB TREE_CONNECT on share.Enabled
- `d597905a` feat(05-09): observability hooks for RunBackup + RunRestore (D-19/D-20)

## Next Plan Readiness

- **Plan 05-10 (SAFETY-01 end-to-end):** ready. BackupHold provider (Plan 08) + adapter gates (this plan) provide the remaining pieces; Plan 10 introduces the production GC invocation site and wires `gc.Options.BackupHold`.
- **Phase 6 (CLI/REST):** can surface share disable/enable + restore endpoints knowing adapters will refuse traffic to disabled shares at the protocol layer.

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-17*
