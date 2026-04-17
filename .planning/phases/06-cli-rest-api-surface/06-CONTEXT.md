# Phase 6: CLI & REST API Surface - Context

**Gathered:** 2026-04-17
**Status:** Ready for planning
**Requirements covered:** API-01, API-02, API-03, API-04, API-05, API-06

<domain>
## Phase Boundary

Deliver the operator-facing CLI (`dfsctl`) and REST API surface that drives
backup, restore, list, repo management, and share enable/disable for metadata
stores. Phase 6 is a thin adapter layer over existing runtime primitives —
**no new business logic**.

Specifically:

1. **Backup CLI tree** under `dfsctl store metadata <name>`: `backup` (on-demand),
   `backup list`, `backup show <id>`, `backup pin/unpin <id>`, `backup job
   {list,show,cancel}` — scoped per-store per user directive.

2. **Restore CLI**: `dfsctl store metadata <name> restore [--from <id>]` with
   `--wait` default, confirmation prompt, `--yes` skip, `--dry-run` pre-flight,
   `--timeout` bound.

3. **Repo management** under `dfsctl store metadata <name> repo {add,list,show,
   edit,remove}` — interactive prompts per destination kind (local / s3),
   encryption key-ref, retention flags, partial-patch edit semantics.

4. **Share enable/disable CLI + REST** — Phase 5 built the runtime primitives
   (`shares.Service.DisableShare/EnableShare`); Phase 6 exposes them via both
   transports: `dfsctl share <name> disable|enable` and `POST /api/v1/shares/
   {name}/{disable|enable}`.

5. **Breaking CLI restructure for all share commands** — all share verbs move
   to the `dfsctl share <name> <verb>` shape (matching the store-scoped
   pattern). Single-commit breaking change documented in CHANGELOG; existing
   verbs affected: `delete`, `edit`, `show`, `mount`, `unmount`. Only
   `share list` and `share create` stay root-level (no target name).

6. **REST endpoints** under `/api/v1/stores/metadata/{name}/`:
   `POST backups` (trigger), `GET backups` (list records), `GET backups/{id}`
   (show record), `PATCH backups/{id}` (pin/unpin), `POST restore`, `GET
   backup-jobs?status=&kind=&limit=` (list jobs), `GET backup-jobs/{id}`
   (poll), `POST backup-jobs/{id}/cancel`. Everything per-store; no
   cross-store global job list endpoint.

7. **apiclient methods** in `pkg/apiclient/` to match the new REST surface
   (typed client for the dittofs-pro UI + Cobra handlers).

8. **Async job polling** — `--wait` polls 1s, spinner + status transitions.
   Server persists progress milestones `0/10/30/60/95/100` at D-05 / backup
   executor stage boundaries. Client renders spinner; no percent bar in v0.13.0.

**Out of scope for this phase:**
- Any new runtime business logic (backup drivers, restore orchestration,
  GC hold) — all lives in Phases 2–5.
- SSE / websocket / long-poll streaming — polling only.
- Cross-repo global job list endpoint — per-store scope only.
- Prefix matching for `--from <id>` — exact ULIDs only.
- Implicit pre-restore safety backup — documented workflow, not machine-enforced.
- Per-protocol share disable granularity (e.g., disable NFS but not SMB) — all-adapters-atomic only.
- Global (cross-store) `GET /api/backup-jobs` — per-store only.
- K8s operator integration — future milestone.
- `--pre-restore-backup` flag — deferred.
- Custom `canceled` terminal status — cancel reuses Phase 4 D-18 `interrupted`.
- Configurable BackupJob retention window (Phase 4 D-17 30-day pruner hardcoded).

</domain>

<decisions>
## Implementation Decisions

### Async CLI UX (applies to `backup` and `restore`)

- **D-01 — `--wait` is the default; `--async` opt-in.**
  Command blocks until the job reaches a terminal state, mirrors
  `kubectl rollout status` / `gh run watch`. `--async` returns immediately
  with the job record for scripts/automation.

- **D-02 — Progress rendering: spinner + status transitions.**
  Poll at fixed 1s, render a spinner while the job is `running`, log each
  status transition (`pending → running → succeeded`). **No percent bar in
  the CLI in v0.13.0** even though the server persists progress milestones
  (D-48). Table mode: spinner + transitions. JSON mode: nothing emitted
  during the wait (final record only on completion — D-07).

- **D-03 — Ctrl-C during `--wait`: interactive three-way prompt.**
  First Ctrl-C prints `[d]etach (default) / [c]ancel / [C]ontinue watching`.
  On `d`: detach-and-exit per D-08. On `c`: POST cancel (D-41) and continue
  polling until terminal. On `C`: resume the spinner. Requires the cancel
  endpoint (D-41).

- **D-04 — `--wait` timeout: indefinite by default; `--timeout <dur>` override.**
  Metadata-store backups of large Postgres systems can legitimately run for
  hours. No default deadline.

- **D-05 — Poll rate: fixed 1s.**
  Bounded cost (3600/hour worst case), snappy UX. No exponential backoff.

- **D-06 — Exit codes: 0 = succeeded; 1 = failed | interrupted | canceled.**
  Classic two-value scheme; CI-friendly.

- **D-07 — `-o json` with `--wait`: emit the final BackupJob record only on
  terminal state.**
  Nothing during the run. Matches `kubectl wait -o json`. Scripts parse a
  single object.

- **D-08 — Ctrl-C detach output: stderr only, stdout stays clean.**
  stderr gets `Detached — job still running. Poll: dfsctl store metadata
  <name> backup job show <id>`. stdout remains reserved for the final result
  (so piping `-o json | jq` stays correct even if the user bails).

- **D-09 — `--async` output: job record to stdout + stderr poll hint.**
  Table mode: renders the BackupJob + stderr hint `Poll: dfsctl store
  metadata <name> backup job show <id>`. JSON mode: record only; no stderr
  hint (stderr is fine but absent when running piped).

- **D-10 — `--wait` success output: final job record + summary banner.**
  Table mode: success banner (`✓ Backup completed`), duration, size, and
  — for backup kind — the resulting `BackupRecord.ID` so operators can
  immediately `pin`, `show`, or `restore --from`. JSON mode: just the
  BackupJob (callers look up the record via the linked ID).

- **D-11 — `--timeout` deadline behavior: exit 2 + detach.**
  On deadline, print the same detach message to stderr as Ctrl-C, but exit
  code 2 (distinct from terminal-failure exit 1). Job continues
  server-side — no implicit cancel.

### Command Layout (everything per-store)

- **D-12 — All backup/restore/job/repo commands nest under
  `dfsctl store metadata <name>`.**
  ```
  dfsctl store metadata <name> backup                 # run (on-demand)
  dfsctl store metadata <name> backup list            # list records
  dfsctl store metadata <name> backup show <id>       # record detail (D-46)
  dfsctl store metadata <name> backup pin <id>        # flip Pinned=true
  dfsctl store metadata <name> backup unpin <id>      # flip Pinned=false
  dfsctl store metadata <name> backup job list        # BackupJob attempts
  dfsctl store metadata <name> backup job show <id>   # single job detail
  dfsctl store metadata <name> backup job cancel <id> # cancel running job
  dfsctl store metadata <name> restore                # restore
  dfsctl store metadata <name> repo add/list/show/edit/remove
  ```
  REST mirror: everything under `/api/v1/stores/metadata/{name}/`. No
  top-level `GET /api/backup-jobs` or `dfsctl backup-job` — strict per-store
  symmetry per user directive.

- **D-13 — On-demand backup conflict: 409 with running `job_id` in body.**
  When `RunBackup` hits Phase 4 D-07 overlap guard, API returns 409 with
  `{detail: "backup already running", running_job_id: "<ULID>"}`. CLI
  prints `Backup already running: <id>. Show: dfsctl store metadata <name>
  backup job show <id>`. Does NOT attach to the existing job.

### Repo Management

- **D-14 — `repo add` uses interactive prompts per kind; `--config
  <json|@file>` bypasses for scripts.**
  Mirrors the existing `dfsctl store metadata add` pattern. Local kind
  prompts for path; S3 kind prompts for bucket / region / endpoint / prefix.
  Interactive flow uses `internal/cli/prompt` helpers.

- **D-15 — S3 credentials follow the existing block-store S3 convention.**
  Reuse whatever `pkg/blockstore/remote/s3` does today (ambient AWS
  credential chain + any existing per-config fields). Research agent
  confirms the exact shape and whether per-repo credential fields are
  needed in `backup_repos.config`. **Research flag.**

- **D-16 — Encryption key reference: single prefixed-string flag.**
  `--encryption-key-ref env:BACKUP_KEY` or `--encryption-key-ref
  file:/etc/dittofs/backup.key`. One flag, matches the
  `backup_repos.encryption_key_ref` column verbatim. Extensible (future
  `vault:path/to/key`). Paired with `--encryption on/off` for the boolean.

- **D-17 — Retention flags: `--keep-count N` + `--keep-age-days D`.**
  One-to-one with schema (`KeepCount *int`, `KeepAgeDays *int`). Unset flag
  = nil = no policy on that axis. Phase 4 D-09 union semantics (keep if
  either matches) are handled server-side.

- **D-18 — Cron schedule validation: server-side only.**
  CLI posts the raw schedule; server calls `storebackups.Service.
  ValidateSchedule(expr)` (Phase 4 D-06). 400 + parse error on invalid.
  CLI has **zero cron parser dependencies** — single source of truth.

- **D-19 — `repo edit` is partial patch.**
  Only flags the operator passes are updated; the rest preserve DB values.
  Runtime D-22 treats every edit as Unregister + Register. REST:
  `PATCH /api/v1/stores/metadata/{name}/repos/{repo_name}` with nullable
  fields in body.

- **D-20 — `repo list` default table columns: `NAME | KIND | SCHEDULE |
  RETENTION | ENCRYPTED`.**
  RETENTION renders as `count=7 age=14d` / `count=7` / `-`. ENCRYPTED
  yes/no. `-o json` emits full `BackupRepo` including `config` map +
  `updated_at`.

- **D-21 — `repo remove <name>` default: config-only delete; `--purge-archives`
  flag cascades.**
  Default: delete the `backup_repos` row + dependent `backup_records` rows;
  destination artifacts (manifest.yaml + payload) remain. `--purge-archives`
  additionally iterates `Destination.Delete` for every record in the repo
  before destroying the DB rows. Confirmation prompt always shown
  (`--force`/`-f` skips). Error message when `--purge-archives` partially
  fails lists the record IDs that failed to delete.

- **D-22 — `repo edit` can toggle encryption in either direction with a
  WARN.**
  `--encryption on/off` + `--encryption-key-ref <ref>` valid post-creation.
  Server logs WARN `repo {name}: encryption setting changed, prior archives
  keep their original encryption status; restore honors per-manifest
  encryption flag`. Past archives are **not** re-encrypted. Restore reads
  each manifest's own `encryption.enabled` field.

- **D-23 — `backup pin/unpin <id>` lives under the per-store subtree.**
  `dfsctl store metadata <name> backup pin <id>` / `unpin <id>`. REST:
  `PATCH /api/v1/stores/metadata/{name}/backups/{id}` with
  `{"pinned": true}` / `false`.

### Backup List UX

- **D-24 — Multi-repo default: error without `--repo` if >1 repo attached.**
  Matches REQ API-01 literal ("if multiple repos, `--repo <name>`").
  `backup`, `backup list`, `backup job list` all require `--repo` when the
  store has more than one attached repo. Error body lists available repo
  names. Single-repo store: implicit `--repo` = the only attached repo.

- **D-25 — Empty `backup list` shows friendly hint (table mode only).**
  Table: `No backups yet. Run: dfsctl store metadata <name> backup [--repo
  <name>]`. JSON mode: `[]`. Matches existing `cmdutil.PrintOutput` empty
  handling for shares / stores.

- **D-26 — Backup list table columns: `ID | CREATED | SIZE | STATUS |
  REPO | PINNED`.**
  ID: short prefix (first 8 chars of ULID + `…`). CREATED: relative time
  (`3h ago`) in table mode; RFC3339 in `-o json` / `-o yaml`. SIZE:
  human-readable (`12.3MB`). PINNED: `yes` / `-`. Default sort: newest
  first. No `ERROR` column — list defaults to `status=succeeded` records
  (retention invariant D-12: records are restorable only).

### Restore Safety + Share Commands

- **D-27 — Phase 6 ships `dfsctl share <name> disable|enable` CLI + matching
  REST.**
  CLI: `dfsctl share <name> disable` / `dfsctl share <name> enable`. REST:
  `POST /api/v1/shares/{name}/disable` + `POST /api/v1/shares/{name}/enable`.
  Both admin-only.

- **D-28 — `share list` + `share show` surface the `Enabled` field.**
  `share list` adds an `ENABLED` column (yes/no). `share show <name>` adds
  `Enabled: yes/no` to its detail section. `-o json` exposes `enabled bool`
  directly in the response. This touches existing share handlers + models.

- **D-29 — Restore gate failure: hard 409, no auto-disable flag.**
  API response body (problem-details D-44): `{type, title: "Restore
  precondition failed", status: 409, detail: "N share(s) still enabled",
  enabled_shares: ["a", "b"]}`. CLI renders:
  ```
  Cannot restore: 2 share(s) enabled — disable them first.
    dfsctl share a disable
    dfsctl share b disable
  ```
  No `--force-disable-shares` flag. Operator's explicit act.

- **D-30 — Restore confirmation prompt: summary + plain Y/N; `--yes` skips.**
  Prompt body:
  ```
  Restore <store-name>
    From backup: <ULID> (created 2026-04-17 12:34 UTC, size 42.1 MB)
    Repo: <repo-name>
    Shares disabled: a, b

  This will REPLACE all metadata in <store-name>. Continue? [y/N]
  ```
  `--yes` (`-y`) skips the prompt. Matches the `RunDeleteWithConfirmation`
  UX convention.

- **D-31 — `restore --dry-run` runs pre-flight only and SKIPS the
  shares-enabled gate.**
  Pre-flight scope (no state mutation, no payload download):
  1. Record resolution (`--from <id>` or latest succeeded in repo).
  2. `Destination.GetManifestOnly` (cheap; KBs not GBs).
  3. Validate `manifest_version == 1`, `store_id`, `store_kind`, `sha256`
     non-empty.
  Dry-run does NOT require shares disabled — the point is rehearsal on a
  live system. Output reports: selected record, manifest validation
  status, **and** a note `Shares enabled on <store>: a, b (must be
  disabled before real restore)`.

- **D-32 — Authz: `RequireAdmin()` for all backup/restore/repo/share-disable
  endpoints.**
  No operator-only trigger role in v0.13.0. Matches every other
  store-config-mutating endpoint. Consistent model.

- **D-33 — Successful restore output: job record + next-step hint.**
  Table mode ends with:
  ```
  ✓ Restore succeeded. Shares remain disabled.
  Re-enable:
    dfsctl share a enable
    dfsctl share b enable
  ```
  Reinforces the Phase 5 D-04 "operator re-enables explicitly" contract.

- **D-34 — Post-restore share re-enable is manual per-share.**
  No bulk `enable-all` helper in v0.13.0; no `--reenable-shares-on-success`
  flag on restore. Matches Phase 5 D-04 (deliberate audit gate). The
  hint in D-33 enumerates the commands verbatim.

### Share Command Restructure

- **D-35 — All `share` commands adopt the `dfsctl share <name> <verb>`
  layout (breaking change).**
  One-commit flip. Affects existing `delete`, `edit`, `show`, `mount`,
  `unmount`. Adds new `disable`, `enable`. Root-level (no target name):
  `share list`, `share create`.

  Final shape:
  ```
  dfsctl share list
  dfsctl share create ...
  dfsctl share <name> show
  dfsctl share <name> edit ...
  dfsctl share <name> delete [-f]
  dfsctl share <name> mount [...]
  dfsctl share <name> unmount
  dfsctl share <name> disable
  dfsctl share <name> enable
  ```

  Documented in CHANGELOG + migration note in release notes. No
  dual-registration with deprecation warnings — single clean flip.

- **D-36 — `share <name> disable` blocks synchronously until drained.**
  Per Phase 5 D-03. No `--wait-drained` flag. By the time the command
  returns, clients are disconnected (or `lifecycle.ShutdownTimeout` fired
  and the disable succeeded anyway).

- **D-37 — `share <name> disable` success output: brief, no restore hint.**
  Table mode: `Share <name> disabled.` JSON mode: the updated share
  record. Exit 0. No verbose hint about "run restore next" — that's a
  separate operator decision.

- **D-38 — `share <name> enable`: no guard rails, flip the bit.**
  No mid-restore check. Phase 5 D-04 says re-enable is a deliberate
  operator act; the operator owns the timing. Runtime reconciles via
  `notifyShareChange`.

- **D-39 — `share disable` is all-adapters-atomic (no per-protocol flag).**
  Enabled is a single boolean (Phase 5 D-01). Disable affects NFS + SMB
  together. No `--protocol` flag.

### Record Selection

- **D-40 — `--from <id>` requires the full 26-char ULID.**
  No prefix matching. Operator copies from `backup list` output. Phase 5
  D-16 validates record existence + repo match + status=succeeded.

- **D-41 — No implicit pre-restore "safety backup"; documented workflow only.**
  If the operator wants a rollback path, they run `backup` first. Not
  machine-enforced. Phase 5 deferred idea (Phase 5 §Deferred).

### Job API Surface

- **D-42 — Job list endpoint: `GET /api/v1/stores/metadata/{name}/backup-jobs?
  status=&kind=&limit=`.**
  Filters: `status ∈ {pending, running, succeeded, failed, interrupted}`,
  `kind ∈ {backup, restore}`, `limit` capped at 200 (default 50).
  Default sort: newest first by `StartedAt`. When a repo filter is needed
  (multi-repo store), add `?repo=<name>`. No `since` / cursor pagination
  in v0.13.0 — D-17 30-day pruner bounds the row count.

- **D-43 — Cancel endpoint: `POST /api/v1/stores/metadata/{name}/backup-jobs/
  {id}/cancel`.**
  Server cancels the executor run ctx (Phase 4 D-18 path); job transitions
  to `status=interrupted` with `error="canceled by operator"`. No new
  `canceled` terminal status in v0.13.0 — reuse existing `interrupted`.
  Returns 202 Accepted + current BackupJob; client polls for final state.

- **D-44 — `backup job cancel <id>` CLI: POST + exit 0.**
  CLI calls the endpoint, prints:
  ```
  Cancel requested for job <id>.
  Poll: dfsctl store metadata <name> backup job show <id>
  ```
  No implicit wait for terminal state. The Ctrl-C-on-`--wait` cancel
  path (D-03 option `c`) DOES keep polling — that's the ONE place where
  cancel + wait compose.

- **D-45 — Cancel on a terminal job: 200 OK, idempotent no-op.**
  `succeeded` / `failed` / `interrupted` job + cancel = return the current
  BackupJob unchanged, status 200. Clients (especially the Ctrl-C race in
  `--wait`) don't need to special-case "job finished between poll and
  cancel". Avoids noisy error logs.

- **D-46 — REST error body: existing controlplane problem-details shape.**
  Reuse whatever `internal/controlplane/api/handlers/problem.go` defines
  today (RFC 7807-ish or project-specific). Typed fields for
  `ErrRestorePreconditionFailed` carry `enabled_shares: []string`; for
  `ErrBackupAlreadyRunning` carry `running_job_id: string`; etc.

- **D-47 — `backup job show <id>` table layout: grouped sections.**
  ```
  Job <ULID>
    Kind:        backup | restore
    Repo:        <name>
    Started:     2026-04-17 12:34:56 UTC
    Finished:    2026-04-17 12:36:02 UTC     (or - if running)
    Duration:    1m6s                        (computed)

  Status:        running | succeeded | failed | interrupted
  Progress:      60%  [▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░]   (bar only for running)
  Error:         <text>                      (only if non-empty)
  ```
  `-o json` / `-o yaml`: flat `BackupJob` model (no derived Duration).

- **D-48 — `backup show <id>` exposes the BackupRecord.**
  REST: `GET /api/v1/stores/metadata/{name}/backups/{id}`. Response =
  full `BackupRecord` (id, created_at, size_bytes, status, pinned,
  sha256, store_id, manifest_path, repo_id). CLI table mode: grouped
  sections mirroring Job show. **Does not** parse/render
  `manifest.yaml` engine-metadata fields — those stay internal; operators
  who need them read the manifest directly. Keeps the operator-visible
  surface minimal.

### Progress & Streaming

- **D-49 — No SSE / websocket / long-poll in v0.13.0: polling only.**
  `GET backup-jobs/{id}` returns current state. CLI polls at 1s (D-05).
  UI mirrors the same endpoint. Future enhancement deferred.

- **D-50 — Job progress milestones: persisted by the executor at 6 stages;
  CLI does NOT render a percent bar during `--wait`.**
  Server persists `Progress` column via a new
  `BackupStore.UpdateBackupJobProgress(ctx, jobID, pct int)` method.
  Phase 6 adds the method to the interface + GORM impl + calls at stage
  boundaries:
  - **Backup executor**: 0 (pending → running), 10 (destination opened),
    50 (streaming payload), 95 (manifest written), 100 (committed /
    succeeded).
  - **Restore executor** (Phase 5 D-05 sequence): 0, 10 (manifest
    fetched + validated), 30 (fresh engine opened), 60 (payload
    streamed + SHA verified), 95 (atomic swap committed), 100 (done).
  Update failures are logged WARN and do NOT fail the parent op.
  The CLI uses the field only in `backup job show` (D-47), not in the
  `--wait` spinner — keeps the spinner simple in v0.13.0.

- **D-51 — BackupJob pruner rows are simply absent from lists.**
  Phase 4 D-17 30-day DELETE pruner removes rows; list endpoint reflects
  the surviving set. No soft-delete, no `?include-pruned=true`. Operators
  who need longer history revisit the pruner window in a future milestone.

### Claude's Discretion

Planner / researcher may refine without revisiting CONTEXT.md:

- Exact Cobra subcommand file layout (one file per leaf vs. shared helpers)
  — follow the `cmd/dfsctl/commands/store/metadata/` convention.
- Spinner library or ASCII animation frames — match whatever dfsctl
  already uses (or a stdlib timer + `\r` if no helper exists).
- HTTP handler struct composition (one `BackupHandler` vs. split
  `BackupRecordHandler` + `BackupJobHandler` + `BackupRepoHandler`) —
  whichever keeps each file < 400 lines.
- `apiclient` method grouping (one `backups.go` vs. `backups.go` +
  `backup_jobs.go` + `backup_repos.go`) — planner picks based on Cobra
  call-site clarity.
- Exact proton-details JSON field names (`enabled_shares`, `running_job_id`,
  etc.) — match whatever naming convention the rest of the API uses.
- ULID prefix truncation length in `backup list` table (D-26) — 8 chars
  vs. 10 vs. 12; planner picks based on how many records typically
  share a prefix in practice.
- Relative-time rendering helper (`3h ago`) — use existing helper if
  present, otherwise vendor a minimal one.
- Whether cron-schedule parsing dependency should be added to `dfsctl` in
  case we later want client-side pre-check (D-18 says no for v0.13.0).
- Whether `backup run` (implicit via `dfsctl store metadata <name>
  backup`) needs an alias or help text clarifying the trigger semantic.
- Whether the `backup` parent command with no subcommand invokes the
  trigger (shortcut) or prints help — conventional Cobra pick.
- Whether `share` create should also move under `<name>` subtree
  retroactively (user said "all the same structure"; `create` doesn't
  have a name yet). Conservative: keep `share create <name>` root-level
  since the `<name>` is the argument being introduced. Planner decides.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner) MUST read these before planning or
implementing.**

### Phase 1–5 lock-ins (binding contracts)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — Phase 1 models (BackupRepo / BackupRecord / BackupJob schemas, manifest v1, `BackupStore` sub-interface)
- `.planning/phases/02-per-engine-backup-drivers/02-CONTEXT.md` — Phase 2 Backupable invariants (Phase 6 does not touch drivers; consumed for manifest_version / store_kind semantics in restore validation)
- `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` — Phase 3 destination two-phase commit, SHA-256 streaming, `GetManifestOnly` (Phase 6 exposes via REST)
- `.planning/phases/04-scheduler-retention/04-CONTEXT.md` — Phase 4 `RunBackup` entrypoint (D-23), overlap guard (D-07), `RegisterRepo/UnregisterRepo/UpdateRepo` (D-22), `BackupJob` pruner 30-day (D-17), `Progress int 0–100` schema (Phase 1)
- `.planning/phases/05-restore-orchestration-safety-rails/05-CONTEXT.md` — Phase 5 `RunRestore` entrypoint, `ErrRestorePreconditionFailed` 409 mapping (D-26), `shares.Service.Disable/Enable/IsShareEnabled/ListEnabledSharesForStore` (D-22), `share.Enabled` column (D-01, D-25), Phase-5 sentinels taxonomy (D-26)

### Project-level
- `.planning/REQUIREMENTS.md` §API — API-01, API-02, API-03, API-04, API-05, API-06 (Phase 6 requirements)
- `.planning/REQUIREMENTS.md` §Out of Scope — confirms no external KMS, no multi-node, no incremental; Phase 6 surface stays v0.13.0-scoped
- `.planning/research/SUMMARY.md` §"Phase 04: Restore Orchestration + CLI/REST API" — async job + polling pattern, `--wait` default, `dfsctl store metadata` subtree, ULID IDs
- `.planning/research/SUMMARY.md` §"Phase 06: Test Matrix" — reminds Phase 7 tests the Phase 6 surface; CLI docs mapping between `dfs backup controlplane` and `dfsctl store metadata backup`
- `.planning/research/FEATURES.md` §Async REST + CLI `--wait` — industry precedent (`aws s3 cp`, `kubectl rollout status`, `gh run watch`)
- `.planning/research/ARCHITECTURE.md` §"Pattern 3: Async Job + Polling (for Restore)" — 202 Accepted + `GET /api/backup-jobs/{id}`, CLI hides polling behind `--wait`
- `.planning/research/PITFALLS.md` §Pitfall 11 — CLI UX (ULIDs, `--dry-run`, confirmation prompts, Ctrl-C ghost-job prevention)
- `.planning/research/PITFALLS.md` §Pitfall 2 — Restore while mounted (feeds D-29 hard-409, D-33 re-enable hint)
- `.planning/PROJECT.md` — single-instance; jobs are in-process, no distributed leader election; admin-only authz convention

### Runtime primitives Phase 6 consumes (do not modify)
- `pkg/controlplane/runtime/storebackups/service.go` — `Service.RunBackup(ctx, repoID)` (Phase 4 D-23), `Service.RunRestore(ctx, repoID, recordID *string)` (Phase 5), `RegisterRepo/UnregisterRepo/UpdateRepo`, overlap guard integration, `ValidateSchedule(expr)`
- `pkg/controlplane/runtime/storebackups/errors.go` — `ErrBackupAlreadyRunning` (409), `ErrRepoNotFound` (404), `ErrRestorePreconditionFailed` (409), `ErrNoRestoreCandidate` (409), `ErrStoreIdMismatch` (400), `ErrStoreKindMismatch` (400), `ErrRecordNotRestorable` (409), `ErrRecordRepoMismatch` (400), `ErrScheduleInvalid` (400), `ErrInvalidTargetKind` (400)
- `pkg/controlplane/runtime/shares/service.go` — `shares.Service.DisableShare`, `EnableShare`, `IsShareEnabled`, `ListEnabledSharesForStore` (Phase 5 D-22)
- `pkg/controlplane/store/backup.go` — existing `BackupStore` sub-interface: `GetBackupRepo`, `GetBackupRepoByID`, `ListReposByTarget`, `ListAllBackupRepos`, `CreateBackupRepo`, `UpdateBackupRepo`, `DeleteBackupRepo` (Phase 1); `ListSucceededRecordsByRepo`, `GetBackupRecordByID`, `CreateBackupRecord`, `CreateBackupJob`, `UpdateBackupJob`, `GetBackupJobByID`, `RecoverInterruptedJobs` (Phase 1/4/5). **Phase 6 adds**: `ListBackupRecords(ctx, repoID, filter)`, `ListBackupJobs(ctx, filter)`, `UpdateBackupRecordPinned(ctx, id, pinned bool)`, `UpdateBackupJobProgress(ctx, jobID, pct int)`.
- `pkg/controlplane/models/backup.go` — `BackupRepo`, `BackupRecord`, `BackupJob` models (Phase 1); `BackupStatus` enum incl. `interrupted`; `BackupJobKind` enum
- `pkg/controlplane/models/share.go` — `Share.Enabled` field (Phase 5 D-01); existing update handler needs to surface `Enabled` in JSON (D-28)

### Files Phase 6 will create / modify
- **New CLI commands**:
  - `cmd/dfsctl/commands/store/metadata/backup/` — `backup.go` (parent), `run.go` (on-demand), `list.go`, `show.go`, `pin.go`, `unpin.go`
  - `cmd/dfsctl/commands/store/metadata/backup/job/` — `job.go`, `list.go`, `show.go`, `cancel.go`
  - `cmd/dfsctl/commands/store/metadata/restore/` — `restore.go`
  - `cmd/dfsctl/commands/store/metadata/repo/` — `repo.go`, `add.go`, `list.go`, `show.go`, `edit.go`, `remove.go`
  - `cmd/dfsctl/commands/share/` — **restructure**: every verb flipped to `<name> <verb>` layout; add `disable.go`, `enable.go`
- **Modified**: `cmd/dfsctl/commands/store/metadata/metadata.go` (AddCommand for backup/restore/repo)
- **Modified**: `cmd/dfsctl/commands/share/share.go` (restructure command tree)
- **New REST handlers**: `internal/controlplane/api/handlers/backups.go` (records + pin/unpin), `backup_jobs.go` (list + show + cancel), `backup_repos.go` (CRUD), `shares.go` (add Disable / Enable handlers) — new routes wired in `pkg/controlplane/api/router.go`
- **New apiclient methods**: `pkg/apiclient/backups.go` (typed methods for every new endpoint)
- **Modified models / store** for the new methods and `Share.Enabled` surfaced in responses
- **Modified `internal/controlplane/api/handlers/problem.go`** (or equivalent) — new typed error fields `enabled_shares` and `running_job_id` where applicable (D-46)

### Progress instrumentation (D-50)
- **Backup executor** (`pkg/backup/executor/executor.go`): stage markers at destination-open / streaming / manifest-write / commit
- **Restore executor** (`pkg/backup/restore/restore.go`): stage markers at manifest-fetch / fresh-engine-open / payload-verify / swap-commit / done
- **New store method**: `BackupStore.UpdateBackupJobProgress(ctx, jobID, pct int)` — simple UPDATE statement; best-effort

### External (read at plan/execute time)
- Cobra docs — command composition, subcommand patterns: https://github.com/spf13/cobra
- chi docs — route composition: https://github.com/go-chi/chi
- RFC 7807 Problem Details for HTTP APIs — D-46 reference (verify the project already uses a compatible shape via `problem.go`)
- `internal/cli/prompt` + `internal/cli/output` — existing helpers for interactive + table/JSON/YAML rendering (mirror every new command through these)

### Reference CLI/API shapes
- Existing `dfsctl store metadata add/list/edit/remove/health` and handlers — the template for new commands (D-14 / D-20 interactive + table conventions)
- Existing `dfsctl share delete <name>` confirmation pattern via `cmdutil.RunDeleteWithConfirmation` (D-30 / D-21 reuse it)
- Existing `/api/v1/store/metadata/{name}/health` / `/status` routes — show how per-store sub-routes are composed in `pkg/controlplane/api/router.go` (Phase 6 adds `/backups`, `/backup-jobs`, `/restore`, `/repos` at the same level)

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets

- **`cmd/dfsctl/cmdutil/util.go`** — `GetAuthenticatedClient()`, `PrintOutput`,
  `PrintResourceWithSuccess`, `PrintResource`, `RunDeleteWithConfirmation`,
  `HandleAbort` — every new Cobra command builds on these. D-30 confirmation,
  D-07/D-09/D-10 output format handling, all already solved.
- **`internal/cli/prompt/`** (`confirm.go`, `input.go`, `password.go`,
  `select.go`) — interactive repo add / S3 credential prompts (D-14) reuse
  these directly.
- **`internal/cli/output/`** (`format.go`, `json.go`, `yaml.go`, `table.go`)
  — `-o table|json|yaml` rendering. Every new list/show command binds a
  `TableRenderer` implementation.
- **`pkg/apiclient/client.go`** — `Client` with `get`, `post`, `put`, `delete`;
  `listResources[T]`, `getResource[T]` generic helpers. New `backups.go` /
  `backup_jobs.go` methods follow the established pattern.
- **`internal/controlplane/api/handlers/problem.go`** (referenced by
  `BadRequest`, `Conflict`, `InternalServerError` helpers across existing
  handlers) — D-46 error shape already in place; Phase 6 just emits typed
  extra fields like `enabled_shares` and `running_job_id`.
- **`pkg/controlplane/api/router.go`** — per-store sub-routes under
  `/api/v1/store/metadata/{name}/` — template for adding `/backups`,
  `/backup-jobs`, `/restore`, `/repos`. Middleware stack (`RequireAdmin`,
  JWT auth, `RequirePasswordChange`) is wired.
- **`pkg/controlplane/runtime/storebackups/`** — every runtime entrypoint
  Phase 6 consumes is already implemented (Phase 4 / 5). Phase 6 adds zero
  new runtime code.

### Established Patterns

- **Cobra sub-trees per domain** — `cmd/dfsctl/commands/store/metadata/`,
  `cmd/dfsctl/commands/store/block/` with `*.go` per verb + a parent
  `metadata.go` / `block.go`. Phase 6 mirrors for
  `store/metadata/backup/`, `store/metadata/restore/`, `store/metadata/repo/`.
- **REST handlers per resource** — `internal/controlplane/api/handlers/
  shares.go`, `metadata_stores.go`, `block_stores.go`. Each file owns CRUD
  + helpers. Phase 6 adds `backups.go`, `backup_jobs.go`, `backup_repos.go`.
- **Typed interface composition** — handlers embed narrow store interfaces
  (e.g., `ShareHandlerStore = ShareStore + PermissionStore + ...`).
  Phase 6 handlers similarly compose `BackupStore` + `ShareStore` +
  `MetadataStoreConfigStore` where needed.
- **`-o` output format** — global flag `cmdutil.Flags.Output`; every list/show
  command rehydrates to `table|json|yaml`. Zero new plumbing required.
- **Confirmation prompts** — `prompt.ConfirmWithForce` + `cmdutil.HandleAbort`
  for every destructive op. `--force`/`-f` flag convention.
- **Admin-only writes** — `apiMiddleware.RequireAdmin()` wrapped around every
  mutation route. Phase 6 adds routes under the same middleware group.

### Integration Points

- **Phase 4/5 runtime entrypoints** (`storebackups.Service.RunBackup`,
  `RunRestore`, `RegisterRepo`, `UnregisterRepo`, `UpdateRepo`,
  `ValidateSchedule`) are all the business logic Phase 6 needs. Handler
  methods just resolve the repo, parse flags, and delegate.
- **`shares.Service.DisableShare` / `EnableShare`** — Phase 6 REST handlers
  call these directly after DB commit confirms the Enabled flip. Phase 5
  D-03 synchronous-drain behavior flows through the handler response.
- **`BackupStore.UpdateBackupJobProgress`** — Phase 6 adds this method to
  the `BackupStore` sub-interface; executor call sites in
  `pkg/backup/executor/` + `pkg/backup/restore/` are modified to invoke it
  at D-50 stage boundaries. Small, additive changes to Phase 4/5 packages.
- **Phase 7 testing** consumes the Phase 6 CLI + REST surface for E2E
  happy-path and chaos tests. Phase 6 must ship enough CLI output
  determinism (JSON mode) for Phase 7 assertions to be robust.
- **dittofs-pro UI** consumes the REST endpoints directly. The `-o json`
  output of every CLI command mirrors the REST response 1:1 so UI + CLI
  can share type definitions.

</code_context>

<specifics>
## Specific Ideas

- **"Everything per-store" (user, session 2026-04-17).** No top-level `dfsctl
  backup-job` or `GET /api/backup-jobs` endpoints in v0.13.0 — every command
  and every REST path is nested under the store. Job lookups by ULID go
  through `GET /api/stores/metadata/{name}/backup-jobs/{id}`. Explicitly
  chosen for symmetry with `backup`, `restore`, `backup list`, `repo *`
  — one subtree per store.

- **"All share commands should have the same structure" (user, session
  2026-04-17).** Flipped every verb from `share <verb> <name>` to `share
  <name> <verb>` in one commit. User explicitly vetoed a partial flip where
  only new verbs (disable/enable) used the new shape and existing verbs
  stayed. CHANGELOG entry mandatory.

- **"Ctrl-C offers cancel, not just detach" (user, session 2026-04-17).**
  Commits Phase 6 to shipping the cancel endpoint (D-03 requires it).
  Research PITFALL #11 defaulted to "detach only" for ghost-job prevention;
  user chose the richer three-way prompt. Cancel goes through the
  existing Phase 4 D-18 cancel-immediately path (maps to `interrupted`
  terminal state — no new `canceled` status in v0.13.0).

- **Safety-first extends into Phase 6's operator-facing surface.** Hard 409
  on shares-enabled (no `--force-disable-shares`), explicit per-share
  re-enable after restore, exact ULID for `--from` (no prefix), no
  implicit pre-restore backup, RequireAdmin() for all endpoints — every
  ambiguity collapsed to the conservative option, matching Phases 2–5.

- **Breaking CLI changes are acceptable.** User authorised a one-shot flip
  of all share verbs, with the understanding that this is a v0.13.0
  breaking change. This is Phase 6's opportunity to also modernise
  surrounding CLI UX that has drifted — but planner should keep the
  scope tight to what's directly on the Phase 6 path.

- **Per-repo credential shape (D-15) is the research flag.** Planner needs
  to confirm whether `backup_repos.config` should hold per-repo S3
  credentials or whether the existing ambient AWS credential chain via
  `pkg/blockstore/remote/s3` is sufficient. If per-repo fields are
  required, this may cascade into a small schema change — flag for the
  researcher to settle early.

- **Progress milestones are a modest behavioural change in Phase 4/5
  executor code.** D-50 adds 5-6 `BackupStore.UpdateBackupJobProgress`
  calls per run. Phase 4/5 tests may need minor updates to tolerate the
  new method signature; researcher should confirm which Phase 4/5
  integration tests mock `BackupStore` and whether those mocks need the
  new method.

</specifics>

<deferred>
## Deferred Ideas

- **Cross-repo global job list endpoint** (`GET /api/backup-jobs`) —
  rejected for v0.13.0 (per-store symmetry). UI iterates stores for an
  all-jobs dashboard.
- **SSE / websocket / long-poll for job status** — polling only in
  v0.13.0.
- **Prefix matching on `--from <id>`** — exact ULID only.
- **Auto-re-enable shares post-restore** (flag or implicit) — manual
  per-share only.
- **Implicit "safety backup before restore"** — documented workflow, not
  machine-enforced.
- **Operator-only "trigger backup" role** — all admin only in v0.13.0;
  revisit when RBAC expands.
- **Per-protocol share disable** — all-adapters-atomic only (Enabled is
  a single bool).
- **Distinct `canceled` terminal status** — reuse `interrupted` in
  v0.13.0; new status = schema migration + UI work for marginal benefit.
- **Configurable BackupJob 30-day pruner window** — hardcoded per Phase 4
  D-17; future setting if operators need it.
- **Client-side cron validation in dfsctl** — server-side authoritative
  (D-18); no robfig/cron dep in the CLI binary.
- **Byte-counted continuous progress** — milestones only in v0.13.0;
  continuous progress requires reader-hook plumbing across destination +
  backupable + blockstore.
- **Percent bar during `--wait`** — server emits milestones but the CLI
  renders only spinner + status (D-02). Revisit after v0.13.0 ships.
- **`backup verify <id>`** / automatic test-restore (`AUTO-01`) —
  deferred.
- **`repo validate <name>`** (dry-run destination connectivity test) —
  deferred; `repo add` already fails fast on validation.
- **K8s operator-driven backup triggers** (`DittoServer.spec.backupRepos`)
  — future operator milestone.
- **Global `GET /api/backups`** listing across stores — per-store only.
- **Bulk share re-enable helper** (`dfsctl share enable-all --for-store
  <name>`) — deferred.
- **`--dry-run` that also downloads + SHA-verifies the payload** — v0.13.0
  dry-run is pre-flight only (manifest fetch + validate, no payload
  download).
- **Cursor-based pagination on job list** — bounded by Phase 4 D-17
  pruner; `limit` (default 50, cap 200) is enough for v0.13.0.

### Reviewed Todos (not folded)

None — no pending todos match Phase 6 scope.

</deferred>

---

*Phase: 06-cli-rest-api-surface*
*Context gathered: 2026-04-17*
