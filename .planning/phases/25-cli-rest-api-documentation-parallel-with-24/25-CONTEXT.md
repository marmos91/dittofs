# Phase 25: CLI + REST API + Documentation (parallel with 24) - Context

**Gathered:** 2026-05-28
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 6 of 6
**Depends on:** Phase 23 (Runtime.CreateSnapshot/WaitForSnapshot + sync-gate primitive), Phase 24 (Runtime.RestoreSnapshot + 7 error sentinels)

<domain>
## Phase Boundary

Ship the operator-facing surface for snapshot machinery built in Phases 20–24. **No new runtime business logic** — wraps existing `Runtime.{CreateSnapshot,WaitForSnapshot,RestoreSnapshot}` and `store.{Get,List,Delete}Snapshot`. Surfaces: 5 `dfsctl share snapshot` subcommands, 5 REST endpoints under `/api/v1/shares/{name}/snapshots`, `pkg/apiclient/snapshots.go` typed client, and 4 docs deliverables.

In addition, Phase 25 carries one cross-cutting cleanup: **rename "sync gate" → "verify"** across the Phase 23-shipped surface (Go fields, package name, log fields, docstrings) and **drop the `snapshot.sync_gate_concurrency` YAML knob** in favor of hardcoded `16` parallel HEAD probes. CLI-internal vocabulary (`--no-verify`) and operator-doc vocabulary ("the verify gate") then match.

**In scope:**
- `cmd/dfsctl/commands/share/snapshot/` (new nested cobra package) — `snapshot.go` (parent Cmd + init) + `create.go` / `list.go` / `show.go` / `delete.go` / `restore.go` (one file per leaf). Pattern from `cmd/dfsctl/commands/share/permission/`.
- `cmd/dfsctl/commands/share/share.go` — add `snapshot.Cmd` to `init()`.
- `pkg/apiclient/snapshots.go` (new) — typed `apiclient.Snapshot` DTO + 6 client methods (Create, List, Get, Delete, Restore, Wait); reuse `getResource[T]` / `createResource[T]` / `normalizeShareNameForAPI(name)` from existing apiclient package.
- `internal/controlplane/api/handlers/snapshot.go` (new) — 5 handler methods + `SnapshotRuntime` interface (testability seam) + `mapSnapshotError(w, err) bool` single error-to-HTTP table covering all 12 typed sentinels from Phase 23 D-23-12 + Phase 24 D-24-08.
- `pkg/controlplane/api/router.go` — wire `r.Route("/{name}/snapshots", ...)` inside the existing `/api/v1/shares` admin group (inherits `RequireAdmin`).
- `pkg/controlplane/runtime/snapshot.go` — extend with 4 wrapper methods: `GetSnapshot(ctx, share, snapID)`, `ListSnapshots(ctx, share)`, `DeleteSnapshot(ctx, share, snapID)` (acquire delete-lock → `store.DeleteSnapshot` → `os.RemoveAll(SnapshotDir)` → release lock), plus the rename pass.
- **Rename pass:** `pkg/snapshot/syncgate.go` → `pkg/snapshot/verify.go`; `CreateSnapshotOpts.NoSyncGate` → `NoVerify`; YAML `snapshot.sync_gate_concurrency` → DELETED; `Runtime.VerifyRemoteDurability` already named correctly; structured-log field renames (`verify_concurrency` etc. removed entirely since knob is gone); godoc + comments updated to "verify" terminology throughout `pkg/snapshot/`, `pkg/controlplane/runtime/snapshot*.go`.
- `docs/SNAPSHOTS.md` (new) — full operator guide, sections per Area-D decision (Overview, Snapshot model, CLI walkthrough per command, Creating, Listing & inspecting, Deleting, Restore runbook, Recovering from safety snapshot, GC hold semantics, Failure modes & recovery, Limitations, REST API reference brief). Depth mirrors `docs/BLOCKSTORE_MIGRATION.md`.
- `docs/ARCHITECTURE.md` — full "Share Snapshots" section + GC-section update (`SnapshotHoldProvider` + manifest-on-disk filter replaces `BackupHoldProvider` mention).
- `docs/CLI.md` — full `share snapshot` subtree section (not just tree-diagram update).
- `README.md` — replace deprecated "backup will ship in v0.16.0" / v0.13.0 backup paragraph with a "Share Snapshots" feature paragraph + 3-line example command + link to `docs/SNAPSHOTS.md`.
- E2E tests in `test/e2e/snapshot/`:
  - `snapshot_http_test.go` — in-process Runtime + chi router + memory metadata + memory `RemoteStore` + memory `LocalStore`; HTTP client exercises all 5 endpoints including async-create polling, sync-restore happy path, plus fault-injection coverage of all 9 D-24-13 failure modes
  - `snapshot_cli_test.go` — built `dfsctl` binary against a stubserver; asserts table/JSON/YAML output, confirmation-prompt + `--yes` behavior, `--no-wait` / `--no-verify` / `--retry` / `--force` flag wiring

**Out of scope:**
- New runtime/business logic — Phase 25 is plumbing + docs only.
- Streaming progress for long-running restore — sync 200 with simple body per Area-B decision; chunked-JSON-lines variant rejected.
- Async restore (202+poll) — explicitly violates Phase 24 D-24-02 ("no restore-tracking DB row").
- Cross-share restore / restore-into-new-share — Phase 24 REST-01 scoped to same-share.
- Snapshot encryption — orthogonal feature; not in v0.16.0.
- Prometheus / OTel metrics for snapshot ops — deferred per Phase 23 D-23-16; structured `slog` only.
- Verify-now action on `show` (`show --verify` flag re-running VerifyRemoteDurability) — listed as a possible add but cut for scope.
- Restore-progress streaming — kept simple (single 200 response).
- Operator/role split for read endpoints — admin-only inherited.
- Auto-disable / auto-enable around restore (CLI bookending) — Phase 24 D-24-01 spirit preserved; CLI fails on enabled share with hint and exits.
- Pagination (`--limit`, cursors) — snap counts per share stay in low-hundreds (Phase 23 D-23-04 comment) so no `--limit` needed.
- Sort flag — newest-first hardcoded.
- Refuse-to-delete `pre-restore-*` guardrail — rejected in favor of uniform Y/N-prompt + `--yes` policy.

</domain>

<decisions>
## Implementation Decisions

### Cross-cutting rename (sync_gate → verify)

- **D-25-01:** Rename "sync gate" terminology to "verify" across Phase 23-shipped code, so internal vocabulary aligns with the already-chosen `--no-verify` CLI flag and SNAPSHOTS.md operator docs.
  - **Go:** `pkg/snapshot/syncgate.go` → `pkg/snapshot/verify.go` (file rename + package symbol updates). `CreateSnapshotOpts.NoSyncGate bool` → `CreateSnapshotOpts.NoVerify bool`. `Runtime.VerifyRemoteDurability` already named correctly — no change. Logging fields containing "sync_gate" replaced with "verify" or removed where the per-knob context is gone.
  - **YAML:** `snapshot.sync_gate_concurrency` DELETED (per D-25-02). No replacement key.
  - **Docs / godoc / comments:** all "sync gate" prose → "verify" prose. SNAPSHOTS.md and ARCHITECTURE.md use "verify gate" / "the verify step" consistently.
  - **No backwards-compat shim** per memory `feedback_no_prod_users_delete_eagerly` — DittoFS pre-1.0, no production users. Deleting the YAML key is a hard break for any operator config that set it; release notes call it out.

- **D-25-02:** Drop the `snapshot.sync_gate_concurrency` YAML knob. Hardcode parallelism to 16 inside `VerifyRemoteDurability` callers (the Phase 23 default value). Rationale: nobody has tuned this knob; the rare operator with throttled-remote pain can either re-add the knob later (non-breaking) or live with 16 (still well under typical S3 rate limits). Aligns with memory `feedback_less_is_more`.

### CLI UX

- **D-25-03:** `dfsctl share snapshot create <share>` BLOCKS by default — runs `CreateSnapshot` then `WaitForSnapshot`, prints progress lines (creating → ready/failed), exits 0 on `ready` / non-zero on `failed`. `--no-wait` returns the new snapID immediately and exits 0. Rationale: matches operator expectation "the command finishes when the thing is done"; scripts that want fire-and-forget use `--no-wait`.

- **D-25-04:** `dfsctl share snapshot restore <share> <id>` pre-flight refuses on enabled share with hint `share /<name> is enabled; run 'dfsctl share disable /<name>' first`. On disabled share, interactive `Y/N` confirmation prompts (showing share + snap-id + safety-snap-will-be-created notice). `--yes` skips the prompt. Auto-disable / auto-re-enable REJECTED per D-24-01 spirit (explicit operator intent at each step). On success, prints `Safety snap: <id> (delete with 'dfsctl share snapshot delete <share> <id>' after verifying)`.

- **D-25-05:** `dfsctl share snapshot delete` uses same Y/N + `--yes` policy as restore. No special-casing of `pre-restore-*` safety snaps (rejected — uniform UX over guardrail).

- **D-25-06:** Flag naming (after deliberation):
  - **create:** `--no-verify` (= `CreateSnapshotOpts.NoVerify`, ex `NoSyncGate`); `--retry=<id>` (= `CreateSnapshotOpts.RetryOf`); `--no-wait` (CLI-local, no Runtime equivalent)
  - **restore:** `--force` (= `RestoreSnapshotOpts.AllowNonDurable`); `--yes` (CLI-local skip-prompt)
  - **delete:** `--yes`
  Rationale: shorter, colloquial; `--force` is conventional "I accept the risk" verb. Less direct mapping to struct-field names but better operator UX.

### REST API shape

- **D-25-07:** `POST /api/v1/shares/{name}/snapshots` (create) returns `202 Accepted` with `Location: /api/v1/shares/{name}/snapshots/{id}` and JSON body `{"snapshot_id":"<id>","share":"<name>"}`. GET on the Location URL returns the full snapshot record (state, durable, error, etc.). Matches Phase 23 D-23-13.

- **D-25-08:** `POST /api/v1/shares/{name}/snapshots/{id}/restore` is SYNC: handler blocks on `Runtime.RestoreSnapshot`, returns `200 OK` with body `{"snapshot_id":"<id>","safety_snapshot_id":"<id>","share":"<name>"}` on success. Handler wraps `r.Context()` with `context.WithTimeout(ctx, cfg.Snapshot.RestoreHTTPTimeout)` (D-25-10 below).

- **D-25-09:** Single error-to-HTTP mapping table `mapSnapshotError(w, err) bool` in `internal/controlplane/api/handlers/snapshot.go`. Covers all 12 typed sentinels (Phase 23 D-23-12: `ErrSnapshotBackupFailed/VerifyFailed/DrainTimeout/RetryTargetNotFound/RetryTargetNotFailed`; Phase 22: `ErrSnapshotNotFound`; Phase 24 D-24-08: `ErrShareEnabled/SnapshotNotDurable/SnapshotMetadataDumpMissing/MetadataStoreNotResetable/RestoreSafetySnapFailed/RestoreAborted/RestoreVerifyFailed`). Each handler error path calls `if mapSnapshotError(w, err) { return }` before falling through to `InternalServerError`. Single source of truth — one edit when a new sentinel lands.
  - Suggested mapping (planner finalizes against existing problem.go helpers):
    - `ErrSnapshotNotFound` → 404
    - `ErrShareEnabled` → 409 Conflict
    - `ErrSnapshotNotDurable` → 412 Precondition Failed
    - `ErrSnapshotMetadataDumpMissing` → 500
    - `ErrMetadataStoreNotResetable` → 500 (should never happen in prod)
    - `ErrSnapshotRetryTargetNotFound` → 404
    - `ErrSnapshotRetryTargetNotFailed` → 409 Conflict
    - `ErrSnapshotDrainTimeout` → 504 Gateway Timeout
    - `ErrSnapshotBackupFailed` / `ErrSnapshotVerifyFailed` / `ErrRestoreAborted` / `ErrRestoreVerifyFailed` / `ErrRestoreSafetySnapFailed` → 500 with sanitized message (no internal-error leak per IN-2-01 / IN-4-02 precedent in `block_gc.go`)

- **D-25-10:** New YAML knob `snapshot.restore_http_timeout: 30m` (server config) bounds the restore-handler's per-request context. `apiclient.RestoreSnapshot` uses a matching `http.Client.Timeout` (30m default, configurable on `apiclient.Client` builder). CLI `restore` inherits via apiclient. Rationale: operator visibility into long-running restore boundaries; protects server from runaway requests.

### REST DTO + routes

- **D-25-11:** Define `apiclient.Snapshot` DTO in `pkg/apiclient/snapshots.go`, decoupled from `pkg/controlplane/models/snapshot.go` GORM struct. Fields per snippet:
  ```go
  type Snapshot struct {
      ID            string    `json:"id"`
      Name          string    `json:"name,omitempty"`
      Share         string    `json:"share"`
      State         string    `json:"state"`           // creating | ready | failed
      RemoteDurable bool      `json:"remote_durable"`
      ManifestCount int       `json:"manifest_count,omitempty"`
      DumpBytes     int64     `json:"dump_bytes,omitempty"`
      RetryOf       string    `json:"retry_of,omitempty"`
      Error         string    `json:"error,omitempty"`
      CreatedAt     time.Time `json:"created_at"`
      UpdatedAt     time.Time `json:"updated_at"`
  }
  ```
  Handler converts `models.Snapshot` → `apiclient.Snapshot` before writing JSON. `ManifestCount` + `DumpBytes` are computed on demand from disk (stat + manifest hash count) for `show` and optionally `list`; nil/zero when stat fails (don't fail the list because one snap's disk vanished).

- **D-25-12:** Route layout under existing `/api/v1/shares` (which already mounts `RequireAdmin` middleware) — admin-only inherited, no extra middleware:
  - `POST   /api/v1/shares/{name}/snapshots` — create (202)
  - `GET    /api/v1/shares/{name}/snapshots` — list
  - `GET    /api/v1/shares/{name}/snapshots/{id}` — get/show
  - `DELETE /api/v1/shares/{name}/snapshots/{id}` — delete (204 or 200 with body)
  - `POST   /api/v1/shares/{name}/snapshots/{id}/restore` — restore (sync 200)

### List / show output

- **D-25-13:** `dfsctl share snapshot list <share>` default table columns: `ID(8-char short) NAME STATE DURABLE CREATED SIZE`. CREATED rendered relative ("2h ago") with `--no-relative` flag for ISO. SIZE = manifest hash count (`"1842 blocks"`) — the manifest is the user-meaningful unit of "what's protected" in CAS. SIZE is `-` when manifest absent (creating / failed-before-manifest).

- **D-25-14:** Newest-first default sort (no `--sort` flag). Filters: `--state=<state>` and `--name-prefix=<s>` (AND together). No pagination flags. JSON/YAML output via existing `cmdutil.GetOutputFormatParsed()` + `output.PrintJSON/PrintYAML` returns the full `[]apiclient.Snapshot` slice unfiltered (filtering happens server-side or in CLI before render).

- **D-25-15:** `dfsctl share snapshot show <share> <id>` table view includes full UUID + Name + Share + State + Durable + Manifest hash count + Dump bytes + Created + Updated + ManifestPath + DumpPath + RetryOf + Error (when state=failed). JSON/YAML modes return the apiclient.Snapshot DTO plus computed disk fields (paths, sizes).

### Code structure + Runtime API

- **D-25-16:** Nested cobra package `cmd/dfsctl/commands/share/snapshot/` with one file per leaf cmd. Mirrors `cmd/dfsctl/commands/share/permission/` exactly. Tests next to source (`list_test.go`, `restore_test.go` for prompt + flag wiring).

- **D-25-17:** Add 3 thin wrappers on `*Runtime` so handlers never reach into `r.store` directly:
  - `Runtime.GetSnapshot(ctx, share, snapID) (*models.Snapshot, error)` — delegates to `r.store.GetSnapshot`.
  - `Runtime.ListSnapshots(ctx, share) ([]*models.Snapshot, error)` — delegates to `r.store.ListSnapshots`.
  - `Runtime.DeleteSnapshot(ctx, share, snapID) error` — the full dance: acquire per-share delete lock via existing `snapshotDeleteLock(share)`, call `r.store.DeleteSnapshot`, `os.RemoveAll((&models.Snapshot{ID: snapID}).SnapshotDir(localStoreDir))`, release lock. Symmetric with `CreateSnapshot/RestoreSnapshot` already on Runtime.

  Handler `SnapshotRuntime` interface (testability seam, mirrors `BlockGCRuntime` in `block_gc.go`):
  ```go
  type SnapshotRuntime interface {
      CreateSnapshot(ctx context.Context, share string, opts CreateSnapshotOpts) (string, error)
      WaitForSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error)
      RestoreSnapshot(ctx context.Context, share, snapID string, opts RestoreSnapshotOpts) error
      GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error)
      ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error)
      DeleteSnapshot(ctx context.Context, share, snapID string) error
  }
  ```
  Handler unit tests substitute a fake `SnapshotRuntime` that records calls and returns canned responses.

### Tests + PR shape

- **D-25-18:** 3-layer test strategy:
  - **Handler unit tests:** fake `SnapshotRuntime` per `BlockGCRuntime` pattern; assert routing, DTO conversion, error mapping (every sentinel → expected HTTP code), 202+Location for create, 200 body for restore.
  - **apiclient tests:** against existing stubserver pattern; assert each client method's URL construction, body serialization, error decoding (problem+json or generic).
  - **CLI unit tests:** fake apiclient; assert table-renderer output, JSON/YAML modes, confirmation-prompt + `--yes`, flag wiring (`--no-wait`, `--no-verify`, `--retry`, `--force`).
  - **E2E (test/e2e/snapshot/):**
    - `snapshot_http_test.go`: in-process Runtime + chi router + memory metadata + memory `RemoteStore` + memory `LocalStore`. Exercises all 5 endpoints end-to-end. Polls async create. Covers all 9 D-24-13 restore failure modes via fault injection on the in-process stack.
    - `snapshot_cli_test.go`: built `dfsctl` binary against a stubserver; asserts CLI output formats, prompts, exit codes.

- **D-25-19:** PR shape — **4 plans / 2 waves / single PR against develop**, mirroring Phase 22/23/24 cadence:
  - **Wave 1 (1 plan, sequential, foundation):**
    - **P25-01** — Runtime wrappers (`GetSnapshot`, `ListSnapshots`, `DeleteSnapshot` per D-25-17) + sync_gate→verify rename pass across `pkg/snapshot/`, `pkg/controlplane/runtime/snapshot*.go`, config schema; delete `snapshot.sync_gate_concurrency` YAML knob; hardcode 16 in callers. All existing unit + integration tests updated to new names. **No behavior change** beyond the knob removal.
  - **Wave 2 (3 plans, parallel after P25-01 lands):**
    - **P25-02** — `internal/controlplane/api/handlers/snapshot.go` (5 handlers + `SnapshotRuntime` interface + `mapSnapshotError` + DTO conversion) + `pkg/controlplane/api/router.go` wiring under `/api/v1/shares/{name}/snapshots/*` + handler unit tests with fake runtime. Adds `snapshot.restore_http_timeout: 30m` YAML knob (D-25-10).
    - **P25-03** — `pkg/apiclient/snapshots.go` (DTO + 6 client methods) + `cmd/dfsctl/commands/share/snapshot/*` (nested cobra package per D-25-16) + `cmd/dfsctl/commands/share/share.go` registration + CLI unit tests + apiclient stubserver tests. Includes the `apiclient.Client` builder change for restore http timeout override.
    - **P25-04** — `docs/SNAPSHOTS.md` (new, full operator guide per D-25-20) + `docs/ARCHITECTURE.md` (full Snapshots section + GC section update) + `docs/CLI.md` (full snapshot subtree section) + `README.md` (replace deprecated backup paragraph with snapshots paragraph + 3-line example + link).
  - **E2E tests in `test/e2e/snapshot/`** land in P25-02 (HTTP-layer e2e) and P25-03 (CLI-binary e2e) per their respective surfaces — both reuse the in-process memory-stack fixture.
  - Branch: `gsd/phase-25-cli-rest-docs` (already created per current branch). Single PR against `develop`; staged commits per plan; reviewers walk commit-by-commit.

### Documentation

- **D-25-20:** `docs/SNAPSHOTS.md` is the new canonical operator guide; depth mirrors `docs/BLOCKSTORE_MIGRATION.md`. Sections:
  1. Overview (what a snapshot is, what guarantees it gives)
  2. Snapshot model (metadata dump + hash manifest + GC hold; manifest-on-disk = held)
  3. CLI walkthrough (create, list, show, delete) with worked transcripts
  4. Creating a snapshot (--no-wait, --no-verify, --retry semantics)
  5. Listing & inspecting (--state, --name-prefix filters; JSON/YAML modes)
  6. Deleting (Y/N + --yes; GC reclamation timing)
  7. Restore runbook (disable → restore → verify → enable; --force for non-durable)
  8. Recovering from safety snapshot (the second-restore path)
  9. The verify gate (what it drains, what it probes, when it fires)
  10. GC hold semantics (manifest-on-disk = held; failed-snap retention; delete vs GC race window)
  11. Failure modes & recovery (D-24-13 taxonomy in operator language)
  12. Limitations (no cross-share, no encryption, no auto-cleanup, sync restore, single-node v0.16.0)
  13. REST API reference (brief — endpoint table with method+path+description; full spec lives in OpenAPI if/when added)
  Add table-of-contents at top. Target 400–600 lines.

- **D-25-21:** `docs/ARCHITECTURE.md` gets a full "Share Snapshots" subsection (architecture overview, refs SNAPSHOTS.md for operator detail) **plus** GC section update (rename `BackupHoldProvider` mention → `SnapshotHoldProvider`, describe manifest-on-disk filter + per-share RWMutex from Phase 23 D-23-04). `docs/CLI.md` gets a full `dfsctl share snapshot` subtree section (not just tree-diagram update). `README.md` replaces the deprecated v0.13.0-backup / "backup will ship in v0.16.0" paragraph with a "Share Snapshots" feature paragraph + a 3-line example command (`dfsctl share snapshot create /share && dfsctl share snapshot list /share`) + link to `docs/SNAPSHOTS.md`.

### Claude's Discretion

- D-25-09 specific HTTP-code mappings — the table above is a suggestion; planner finalizes against existing `problem.go` helpers (e.g., does a `problem.Conflict` exist, or should it be `WriteJSON(w, 409, ...)` directly?).
- D-25-11 `ManifestCount` + `DumpBytes` computation — on-list (one stat per row, slower but uniform UX) vs on-show-only (faster list, asymmetric). Planner picks; default on-show-only with comment-line on `Snapshot` struct that these are optional.
- D-25-13 SIZE column semantics — `manifest hash count` vs `metadata.dump bytes` vs both. Planner picks whichever is cheaper + more meaningful to operators; either is defensible.
- D-25-17 `DeleteSnapshot` lock-acquisition path — direct call to `Runtime.snapshotDeleteLock(share).Lock()` vs going through `SnapshotHoldProvider.AcquireDeleteLock`. Both use the same underlying mutex per Phase 23 D-23-04; planner picks the call site that reads cleaner.
- D-25-18 stubserver pattern for apiclient tests — reuse existing `pkg/apiclient/stubserver_test.go` if it's extensible, else add snapshot-specific stub helpers.
- D-25-19 PR splitting — if reviewer load is too high in P25-02 (handler + router + interface + tests + new YAML knob), planner may split off the YAML knob + config plumbing as a tiny P25-02a.
- D-25-20 SNAPSHOTS.md exact section ordering + worked-transcript verbosity — operator-doc tone judgment.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and roadmap
- `.planning/REQUIREMENTS.md` lines 50–56 (API-01..07) + lines 65–68 (DOC-01..04) — Phase 25's eleven requirements
- `.planning/ROADMAP.md` §"Phase 25: CLI + REST API + Documentation (parallel with 24)" (lines 721–741) — goal + success criteria + files-to-touch list
- `.planning/REQUIREMENTS.md` lines 119–131 — requirement-to-phase coverage table (all pending → flip to done on PR merge)

### Phase 24 dependency (sync restore + 7 error sentinels + RestoreSnapshotOpts)
- `.planning/phases/24-restore-flow/24-CONTEXT.md` — all 19 Phase 24 decisions. Critical: D-24-01 (operator-bracketed restore — CLI refuses on enabled share), D-24-02 (sync restore — Phase 25 REST blocks on it, D-25-08), D-24-06 (`AllowNonDurable` → `--force` per D-25-06), D-24-08 (7 sentinels Phase 25 maps to HTTP per D-25-09), D-24-13 (failure-mode taxonomy for e2e + SNAPSHOTS.md ops doc)
- `.planning/phases/24-restore-flow/24-VERIFICATION.md` — Phase 24 verification report (read for what's actually shipped)
- `pkg/controlplane/runtime/snapshot.go::Runtime.RestoreSnapshot(ctx, share, snapID string, opts RestoreSnapshotOpts) error` — exact call the restore handler wraps
- `pkg/controlplane/runtime/snapshot.go::RestoreSnapshotOpts{AllowNonDurable bool}` — CLI/REST surface `--force` / `{"allow_non_durable": true}` body field
- `pkg/controlplane/models/errors.go` — 7 new sentinels from D-24-08: `ErrShareEnabled`, `ErrSnapshotNotDurable`, `ErrSnapshotMetadataDumpMissing`, `ErrMetadataStoreNotResetable`, `ErrRestoreSafetySnapFailed`, `ErrRestoreAborted`, `ErrRestoreVerifyFailed`

### Phase 23 dependency (async create + sync-gate primitive + 5 sentinels + WaitForSnapshot)
- `.planning/phases/23-snapshot-create-orchestration-sync-gate/23-CONTEXT.md` — all 23 Phase 23 decisions. Critical: D-23-11 (`NoSyncGate` → `NoVerify` per D-25-01; `--no-verify` per D-25-06), D-23-12 (5 sentinels), D-23-13 (async CreateSnapshot — Phase 25 REST returns 202 per D-25-07), D-23-15 (`CreateSnapshotOpts` plain struct — no functional opts), D-23-19 (`WaitForSnapshot` — Phase 25 CLI block-by-default per D-25-03), D-23-22 (`snapshot.sync_gate_concurrency` knob — DELETED per D-25-02)
- `pkg/controlplane/runtime/snapshot.go::CreateSnapshotOpts{NoSyncGate, RetryOf}` — Phase 25 renames `NoSyncGate → NoVerify` per D-25-01; CLI surface `--no-verify` / `--retry=<id>` per D-25-06
- `pkg/controlplane/runtime/snapshot.go::Runtime.CreateSnapshot`, `Runtime.WaitForSnapshot` — exact calls CLI create blocks on
- `pkg/snapshot/syncgate.go::VerifyRemoteDurability(ctx, remote, manifest *HashSet, concurrency int) error` — Phase 25 renames file `syncgate.go → verify.go` per D-25-01; callers pass literal 16 per D-25-02

### Phase 22 dependency (snapshot model + SnapshotStore CRUD + manifest I/O + delete-lock infra)
- `.planning/phases/22-snapshot-records-hash-manifest-gc-hold/22-CONTEXT.md` — Phase 22 decisions. Critical: D-22-04 (manifest = ground truth), D-22-19 (atomic manifest write via temp+rename)
- `pkg/controlplane/models/snapshot.go::Snapshot{ID, Name, Share, State, RemoteDurable, RetryOf, ...}` + path helpers `SnapshotDir(localStoreDir)`, `ManifestPath(...)`, `MetadataDumpPath(...)` — used by Phase 25 DTO mapping + Runtime.DeleteSnapshot dir wipe
- `pkg/controlplane/store/snapshots.go::GORMStore.{GetSnapshot,ListSnapshots,DeleteSnapshot}` — wrapped by new `Runtime.GetSnapshot/ListSnapshots/DeleteSnapshot` per D-25-17
- `pkg/snapshot/manifest.go::ReadManifest(path) (*HashSet, error)` — used by `show` to compute manifest count if not on the DB row
- `pkg/controlplane/runtime/snapshot_hold.go::SnapshotHoldProvider.AcquireDeleteLock(shareName) func()` + `Runtime.snapshotDeleteLock(shareName)` — Phase 25 Runtime.DeleteSnapshot uses the same per-share mutex per D-25-17 / D-23-04

### Reusable code patterns
- `cmd/dfsctl/commands/share/permission/` — exact template for the nested-cobra-package layout (D-25-16): `permission.go` (parent + init) + `grant.go` / `revoke.go` / `list.go` leaves. Each leaf uses `cmdutil.GetAuthenticatedClient()` + `client.XXX()` + `cmdutil.GetOutputFormatParsed()` switch over JSON/YAML/table via `output.PrintJSON/PrintYAML/PrintTable`.
- `cmd/dfsctl/commands/share/permission/list.go::PermissionList` — `TableRenderer` interface implementation pattern; Phase 25 mirrors with `SnapshotList` type (Headers/Rows methods).
- `pkg/apiclient/blockstore.go::BlockStoreStatsResponse` etc. — DTO pattern + generic `getResource[T]` / `createResource[T]` helpers + `url.PathEscape(normalizeShareNameForAPI(name))`. Phase 25 reuses verbatim.
- `internal/controlplane/api/handlers/block_gc.go::BlockGCRuntime` interface + `NewBlockStoreGCHandler(rt)` — exact template for `SnapshotRuntime` interface + `NewSnapshotHandler` per D-25-17. Also the pattern for `errors.Is(err, shares.ErrShareNotFound) → NotFound + log-at-Debug + sanitized 500 message` (D-25-09).
- `internal/controlplane/api/handlers/shares.go::ShareHandler.Disable / Enable` (lines 678 + 715) — pattern for "POST verb on a resource, get reloaded record back" (Phase 25 restore handler mirrors).
- `internal/controlplane/api/handlers/problem.go` — `BadRequest` / `NotFound` / `InternalServerError` / `WriteNoContent` helpers. Phase 25 also may add a `WriteJSONAccepted` (202) helper if not present (planner check).
- `pkg/controlplane/api/router.go` lines 190–215 — share routes group with `RequireAdmin` middleware; Phase 25 adds `r.Route("/{name}/snapshots", ...)` siblings inside this same group per D-25-12.

### Cobra leaf-cmd conventions
- `cmd/dfsctl/commands/share/permission/list.go` (full file) — confirmation prompt is NOT used here (read-only); for write/destructive ops with `--yes`, see how `cmd/dfsctl/commands/share/delete.go` handles confirmation (planner reads for exact prompt-helper if one exists; else add to `cmdutil`).
- `cmd/dfsctl/cmdutil/` — `GetAuthenticatedClient`, `GetOutputFormatParsed`. Phase 25 may add `ConfirmDestructive(prompt, yesFlag) (bool, error)` helper if no equivalent exists.

### Existing docs (reference for tone + depth + cross-link style)
- `docs/BLOCKSTORE_MIGRATION.md` — depth + tone template for `SNAPSHOTS.md` per D-25-20 (TOC at top, worked transcripts, "Recovery" / "Out of scope" / "Internals" sections)
- `docs/CLI.md` — current `dfsctl` tree to extend (D-25-21)
- `docs/ARCHITECTURE.md` — GC section that needs update + insertion point for new Snapshots section (D-25-21)
- `README.md` — deprecated v0.13.0 backup paragraph to replace (D-25-21)

### CLAUDE.md + standing rules
- `CLAUDE.md` §"Architecture invariants" — invariants 6 (return `metadata.ExportError` values + Debug/Error log split — applies to handler error paths) and 7 (metadata-store contract — `Resetable` already shipped by Phase 24)
- `CLAUDE.md` §"Commits & PRs" — no Claude/AI mentions; concise messages; sign commits
- Memory: `feedback_no_prod_users_delete_eagerly` — justifies hard-deleting `sync_gate_concurrency` YAML key without compat shim per D-25-02
- Memory: `feedback_less_is_more` — justifies hardcoding 16 over keeping the knob per D-25-02
- Memory: `feedback_run_lint_before_push`, `feedback_simplifier_reviewer_before_pr`, `feedback_sign_all_commits`, `feedback_assign_prs_to_marmos91` — pre-PR gates

### New files Phase 25 creates
- `cmd/dfsctl/commands/share/snapshot/snapshot.go` (parent cmd + init)
- `cmd/dfsctl/commands/share/snapshot/create.go`
- `cmd/dfsctl/commands/share/snapshot/list.go`
- `cmd/dfsctl/commands/share/snapshot/show.go`
- `cmd/dfsctl/commands/share/snapshot/delete.go`
- `cmd/dfsctl/commands/share/snapshot/restore.go`
- `cmd/dfsctl/commands/share/snapshot/list_test.go`, `restore_test.go`, etc. (CLI unit tests, planner picks coverage shape)
- `pkg/apiclient/snapshots.go` + sibling `snapshots_test.go`
- `internal/controlplane/api/handlers/snapshot.go` + sibling `snapshot_test.go`
- `pkg/snapshot/verify.go` (renamed from `syncgate.go`)
- `docs/SNAPSHOTS.md`
- `test/e2e/snapshot/snapshot_http_test.go`
- `test/e2e/snapshot/snapshot_cli_test.go`

### Files Phase 25 modifies
- `cmd/dfsctl/commands/share/share.go` — register `snapshot.Cmd` in `init()`
- `pkg/controlplane/api/router.go` — wire 5 snapshot routes under `/api/v1/shares/{name}/snapshots`
- `pkg/controlplane/runtime/snapshot.go` — add `GetSnapshot/ListSnapshots/DeleteSnapshot` wrappers + rename `NoSyncGate → NoVerify`
- `pkg/controlplane/runtime/snapshot*.go` (related files) — apply rename pass (sync gate → verify) in log fields, godoc, comments
- `pkg/controlplane/models/errors.go` — sanity-check 12 sentinels are exported (no change expected; Phase 23+24 already shipped them)
- `pkg/snapshot/syncgate.go` → renamed `pkg/snapshot/verify.go` (file move + symbol rename)
- Server YAML config schema + Go binding — delete `snapshot.sync_gate_concurrency` field; add `snapshot.restore_http_timeout` (default 30m)
- `pkg/apiclient/client.go` (or equivalent) — apiclient builder accepts optional per-call http timeout for restore
- `docs/ARCHITECTURE.md`, `docs/CLI.md`, `README.md` — per D-25-21

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `cmdutil.GetAuthenticatedClient()` + `cmdutil.GetOutputFormatParsed()` (existing CLI infra) — every leaf cmd reuses verbatim. No new auth/output plumbing.
- `output.PrintJSON / PrintYAML / PrintTable` + `TableRenderer` interface — Phase 25 implements `SnapshotList` (multi-row) and `SnapshotDetail` (single-record show) renderers.
- `apiclient.getResource[T](c, url)`, `apiclient.createResource[T](c, url, body)`, `apiclient.normalizeShareNameForAPI(name)`, `url.PathEscape(...)` — every snapshot client method reuses these. Generic-based decoding handles DTO unmarshalling automatically.
- `internal/controlplane/api/handlers/problem.go` — `BadRequest / NotFound / InternalServerError / WriteJSONOK / WriteNoContent` helpers. Likely missing: `WriteJSONAccepted(w, body)` for 202; planner adds if absent or writes inline.
- `Runtime.snapshotDeleteLock(shareName)` (Phase 23 D-23-04, in `snapshot_hold.go`) — per-share RWMutex; Phase 25 `Runtime.DeleteSnapshot` reuses for the lock + DB delete + dir wipe sequence.
- `models.Snapshot.SnapshotDir(localStoreDir)`, `ManifestPath`, `MetadataDumpPath` (Phase 22) — Phase 25 `show` reads via these for path/size display; `Runtime.DeleteSnapshot` wipes via `SnapshotDir`.
- `pkg/snapshot/manifest.go::ReadManifest` — Phase 25 `show` calls for manifest hash count when computing on demand.

### Established Patterns
- **Handler-interface-for-testability pattern** (`BlockGCRuntime` in `internal/controlplane/api/handlers/block_gc.go`): Phase 25 `SnapshotRuntime` mirrors. Handler file declares the narrow interface it needs from Runtime; tests substitute a fake.
- **Nested cobra subpackage pattern** (`cmd/dfsctl/commands/share/permission/`): Phase 25 `cmd/dfsctl/commands/share/snapshot/` mirrors exactly — parent `Cmd` + `init()` wires leaves, one file per leaf.
- **Single error-to-HTTP mapper helper** (no exact precedent — each existing handler does inline switches; Phase 25 introduces `mapSnapshotError` as the first canonical mapper for a typed-sentinel cluster, per D-25-09). Future phases with typed-error clusters may follow.
- **Async-create / sync-read REST pattern** (Phase 23 D-23-13 ratified; Phase 25 implements): `POST → 202 + Location + minimal body`; `GET → 200 + full record including state`. Familiar HTTP semantics.
- **Operator-bracketed destructive ops** (D-24-01; CLI `share disable / enable` cycle around `share snapshot restore`): preserved verbatim per D-25-04.
- **Hardcoded-after-tuning constants** (per `feedback_less_is_more`): Phase 25 hardcodes `verify` parallelism = 16, dropping a knob that nobody tunes.
- **In-memory full-stack fixture for orchestration tests** (Phase 22 D-21 / Phase 23 P23-06 / Phase 24 P24-04): Phase 25 `test/e2e/snapshot/snapshot_http_test.go` follows the same pattern with chi router on top.

### Integration Points
- **Router** (`pkg/controlplane/api/router.go` ~line 190): new `r.Route("/{name}/snapshots", ...)` inside existing `/shares` admin group — inherits `RequireAdmin`, no extra middleware (D-25-12).
- **CLI parent** (`cmd/dfsctl/commands/share/share.go::init()`): one line — `Cmd.AddCommand(snapshot.Cmd)` next to `permission.Cmd` (D-25-16).
- **Runtime** (`pkg/controlplane/runtime/snapshot.go`): 3 wrapper methods added per D-25-17 + rename pass.
- **Config** (server YAML + Go binding): delete `snapshot.sync_gate_concurrency`, add `snapshot.restore_http_timeout` (D-25-02 + D-25-10).
- **apiclient builder** (`pkg/apiclient/client.go` or constructor): optional per-call http timeout override for restore endpoint (D-25-10).

</code_context>

<specifics>
## Specific Ideas

- **User insistence on clear naming** (mid-discussion): "sync_gate is not very clear", "verify_concurrency is pretty confusing". Drove D-25-01 (rename everywhere) and D-25-02 (delete the knob entirely rather than rename it). Phase 25 takes the cleanup cost in exchange for one coherent vocabulary across docs/CLI/code.
- **Operator-as-primary-user lens** across CLI UX decisions: block-by-default create (D-25-03), interactive prompt + `--yes` for destructive ops (D-25-04 / D-25-05), pre-flight share-disable hint over auto-disable (D-25-04). Pattern: explicit operator intent at every step; scripts opt out via flags.
- **`--force` over `--allow-non-durable`** (D-25-06): user picked the shorter, conventional verb over the more-precise struct-mirror name. `--force` is operator-vocabulary; SNAPSHOTS.md explains what it actually does.
- **No streaming progress for restore** (D-25-08): user opted for sync 200 + simple body over chunked-JSON progress events. Keep the wire simple; CLI's interactive UX is already adequate since handler blocks.

</specifics>

<deferred>
## Deferred Ideas

- **Restore-progress streaming** (chunked-JSON-lines body or SSE) — kept simple per D-25-08; revisit if operators report long-restore UX pain.
- **`--verify` flag on `show`** (re-run `VerifyRemoteDurability` on demand) — useful diagnostic, but adds a new REST endpoint; defer to a follow-up if operators ask.
- **Pagination on `list`** (`--limit`, cursors) — snap counts per share stay in low-hundreds (Phase 23 D-23-04); revisit if a power-user share blows past that.
- **`--sort` flag on `list`** — newest-first is enough for now.
- **Refuse-to-delete `pre-restore-*` safety snaps** — rejected in favor of uniform UX; can re-add as `--no-safety-guard` if operators delete by mistake.
- **Operator-role read access** (split admin-only into admin-write + operator-read for list/show) — admin-only inherited per D-25-12; revisit if dashboard/observability tooling needs read-only role.
- **Auto-cleanup of safety snaps on restore success** — Phase 24 D-24-04 keeps operator-driven; could add `RestoreSnapshotOpts.AutoCleanupSafetySnap bool` later (non-breaking).
- **`apiclient.Snapshot.ManifestCount` / `DumpBytes` on every list row** — currently planner-discretion (on-list vs on-show-only per D-25-11); operator feedback decides default behavior.
- **OpenAPI spec for snapshot endpoints** — SNAPSHOTS.md ships a brief REST reference table; full OpenAPI is a future cross-cutting initiative.
- **Prometheus / OTel metrics** — deferred per Phase 23 D-23-16 stance; structured `slog` only.
- **Async restore (202 + poll)** — Phase 24 D-24-02 explicitly rejected for v0.16.0; revisit if HTTP-timeout pain emerges.
- **Cross-share restore** (snapshot of share A → share B) — REST-01 scope; orthogonal feature.
- **Snapshot encryption / per-snapshot keys** — orthogonal.

</deferred>

---

*Phase: 25-cli-rest-api-documentation-parallel-with-24*
*Context gathered: 2026-05-28*
