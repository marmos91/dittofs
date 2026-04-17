# Phase 6: CLI & REST API Surface - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-17
**Phase:** 06-cli-rest-api-surface
**Areas discussed:** Async CLI UX, Repo add/edit UX, Restore safety + share commands, Job API surface width

---

## Async CLI UX

### Q: Default mode for `dfsctl store metadata <name> backup` and `restore`?

| Option | Description | Selected |
|--------|-------------|----------|
| `--wait` by default | Command blocks until terminal state; recommended | ✓ |
| `--async` by default | Returns job ID immediately | |
| No default, require explicit flag | First invocation errors | |

**User's choice:** `--wait` by default.

### Q: Progress reporting during `--wait`?

| Option | Description | Selected |
|--------|-------------|----------|
| Spinner + status changes | Poll every ~1s, spinner + log transitions (recommended) | ✓ |
| Spinner + progress bar when available | Richer UX, more work | |
| Silent until done | No output until terminal state | |

**User's choice:** Spinner + status changes.

### Q: Ctrl-C behavior during `--wait`?

| Option | Description | Selected |
|--------|-------------|----------|
| Detach only, job continues (recommended per PITFALL #11) | First Ctrl-C prints 'detached' and exits | |
| Prompt: detach or cancel | `[d]etach / [c]ancel / [C]ontinue watching` | ✓ |
| Cancel by default | Ctrl-C sends DELETE | |

**User's choice:** Prompt — detach or cancel. **Notes:** Requires shipping a cancel endpoint, which commits Phase 6 to D-41. User accepted the added surface to get a better operator experience.

### Q: `--wait` timeout behavior?

| Option | Description | Selected |
|--------|-------------|----------|
| Indefinite with `--timeout` override (recommended) | No default deadline | ✓ |
| Default 30m + `--timeout` override | Sensible bound | |
| Require `--timeout` explicitly | Most explicit | |

**User's choice:** Indefinite with `--timeout` override.

### Q: Polling interval while `--wait` is active?

| Option | Description | Selected |
|--------|-------------|----------|
| Fixed 1s (recommended) | Simple, snappy | ✓ |
| Exponential backoff (250ms → 5s cap) | Adaptive | |
| Fixed 2s | Halves poll load | |

**User's choice:** Fixed 1s.

### Q: CLI exit codes for async command terminal states?

| Option | Description | Selected |
|--------|-------------|----------|
| 0 succeeded, 1 failed/interrupted/canceled (recommended) | Classic | ✓ |
| Distinct per state (0/2/3/4) | Granular | |
| You decide | | |

**User's choice:** 0/1 classic scheme.

### Q: When `-o json` is set with `--wait`, what's emitted?

| Option | Description | Selected |
|--------|-------------|----------|
| Final job record only on terminal state (recommended) | Silent during run | ✓ |
| Streaming JSON-lines per transition | Push-style | |
| Final record + exit 0/1 | Same as option 1 | |

**User's choice:** Final job record only on terminal state.

### Q: When `--wait` detaches via Ctrl-C, what does stdout print for machine consumers?

| Option | Description | Selected |
|--------|-------------|----------|
| Plain text to stderr, no stdout (recommended) | stdout stays clean | ✓ |
| JSON stub to stdout with job_id | Script-parseable detach | |
| You decide | | |

**User's choice:** Plain text to stderr, no stdout.

### Q: `--async` mode output format?

| Option | Description | Selected |
|--------|-------------|----------|
| Job record + 'Poll: dfsctl ...' hint to stderr (recommended) | Table + hint, JSON = record only | ✓ |
| Just the job ID on stdout | Terse | |
| Full table + no hint | No discoverability | |

**User's choice:** Job record + stderr hint.

### Q: Backup/restore command output when the job succeeds on `--wait`?

| Option | Description | Selected |
|--------|-------------|----------|
| Final job record + success msg (recommended) | Success banner + record | ✓ |
| Just job ID on stdout | Minimal | |
| Spinner cleanup only | Too quiet | |

**User's choice:** Final job record + success msg.

### Q: What happens when `--wait` hits its `--timeout` deadline?

| Option | Description | Selected |
|--------|-------------|----------|
| Detach like Ctrl-C, exit non-zero (recommended) | Job continues server-side | ✓ |
| Cancel server job + exit 1 | Safer against runaway | |
| Silent exit 2 | No message | |

**User's choice:** Detach like Ctrl-C, exit 2.

### Q: `dfsctl backup-job show <id>` — top-level or nested?

| Option | Description | Selected |
|--------|-------------|----------|
| Top-level (recommended) | `dfsctl backup-job {show,list,cancel}` | |
| Nested under store backup | `dfsctl store metadata <name> backup job show <id>` | ✓ (via follow-up) |
| Deeply nested under backup | Same as option 2 | |

**User's choice:** Option 3 / fully nested per-store. **Notes:** User picked "Other" with note "Why aren't we tracking backup jobs per metadata store?" — follow-up confirmed they want every command nested under store subtree for strict per-store symmetry. REST mirrors: `GET /api/stores/metadata/{name}/backup-jobs[/{id}]`.

### Q: When `backup --async` returns and another backup is already running for that repo, what happens?

| Option | Description | Selected |
|--------|-------------|----------|
| 409 with running job_id in response body (recommended) | Surfaces ULID | ✓ |
| 409 error only, no job_id | Generic | |
| Attach to existing job | Idempotent | |

**User's choice:** 409 with running job_id.

---

## Repo add/edit UX

### Q: How should `dfsctl store metadata <name> repo add --kind <local|s3>` collect destination config?

| Option | Description | Selected |
|--------|-------------|----------|
| Interactive prompts per kind (recommended) | Mirror `store metadata add` | ✓ |
| JSON-config only | Simpler | |
| Typed flags per kind | Flag soup | |

**User's choice:** Interactive prompts + `--config` shortcut.

### Q: How to collect the S3 access key / secret for a repo?

| Option | Description | Selected |
|--------|-------------|----------|
| Same as block store S3 config (recommended) | Unify convention | ✓ |
| Per-repo credential field in config | Isolate | |
| Defer credential plumbing | Ambient chain only | |

**User's choice:** Same as block store S3 config. **Notes:** Research flag — planner confirms whether `pkg/blockstore/remote/s3` has per-repo credential fields or relies on ambient AWS creds.

### Q: How does the operator express `encryption_key_ref`?

| Option | Description | Selected |
|--------|-------------|----------|
| Prefixed string `env:NAME` or `file:PATH` (recommended) | One flag, matches schema | ✓ |
| Two flags (`--encryption-key-env` / `--encryption-key-file`) | Explicit | |
| Interactive prompt when `--encryption` is set | Interactive only | |

**User's choice:** Prefixed-string single flag.

### Q: Retention policy flags — `--keep-count` / `--keep-age-days` as schema maps?

| Option | Description | Selected |
|--------|-------------|----------|
| Two flags (recommended) | One-to-one with schema | ✓ |
| Combined `--retention "count=7,age=14d"` | One flag | |
| Subcommand `repo set-retention` | Separate concern | |

**User's choice:** Two flags matching schema.

### Q: Should `repo add/edit` validate the cron expression client-side before the API call?

| Option | Description | Selected |
|--------|-------------|----------|
| Server-side only (recommended) | Reuse Phase 4 D-06 validator | ✓ |
| Client-side using robfig/cron/v3 | Faster feedback | |
| Both | Duplicated | |

**User's choice:** Server-side only.

### Q: `repo edit` semantics — full replace vs partial patch?

| Option | Description | Selected |
|--------|-------------|----------|
| Partial patch, nil flags preserved (recommended) | `kubectl patch` style | ✓ |
| Full replace | Must pass all flags | |
| Subcommands per field | Verbose | |

**User's choice:** Partial patch.

### Q: `repo list` default table columns?

| Option | Description | Selected |
|--------|-------------|----------|
| NAME \| KIND \| SCHEDULE \| RETENTION \| ENCRYPTED (recommended) | 5 cols, fits 80 | ✓ |
| Add UPDATED_AT column | 6 cols | |
| Minimal: NAME \| KIND \| SCHEDULE | Terse | |

**User's choice:** 5-column recommended layout.

### Q: `repo remove <name>` behavior re existing backup archives?

| Option | Description | Selected |
|--------|-------------|----------|
| Remove config only, leave archives (recommended) | Safer default | |
| Remove config AND destination archives | Cascade | |
| Prompt: config-only or with archives? | Interactive | |

**User's choice:** Flag-controlled, default keeps archives. **Notes:** User picked "Other" with note "We can add flags to control both behaviours. Default: config only". Decision captured as D-21: default config-only; `--purge-archives` flag cascades destination delete; confirmation always shown; `--force` skips.

### Q: Can `repo edit` toggle encryption on an existing repo?

| Option | Description | Selected |
|--------|-------------|----------|
| Allow both directions with a warning (recommended) | Flexible | ✓ |
| Only enable, never disable | Safer | |
| Lock at create time | Immutable | |

**User's choice:** Allow both with WARN; past archives preserve their manifest's encryption flag.

### Q: Backup record pinning CLI — where does it live?

| Option | Description | Selected |
|--------|-------------|----------|
| `dfsctl store metadata <n> backup pin/unpin <id>` (recommended) | Per-store subtree | ✓ |
| `dfsctl backup pin <id>` top-level | ID-only | |
| Flag on `backup show <id> --pin/--unpin` | Overloaded | |

**User's choice:** Per-store subtree.

### Q: What does `backup list` show when a repo has NO succeeded backups?

| Option | Description | Selected |
|--------|-------------|----------|
| Friendly empty state + hint (recommended) | Matches existing pattern | ✓ |
| Empty table with headers only | Terse | |
| 404 if repo has no records | Semantically wrong | |

**User's choice:** Friendly empty state + hint.

### Q: When multiple repos attached to a store, which is default for `backup`/`backup list`?

| Option | Description | Selected |
|--------|-------------|----------|
| Error: require `--repo` when >1 attached (recommended) | Matches REQ API-01 literal | ✓ |
| Use the first by CreatedAt | Deterministic but surprising | |
| Require `--repo` always | Worst ergonomics | |

**User's choice:** Error when >1 without `--repo`.

---

## Restore safety + share commands

### Q: Does Phase 6 ship `dfsctl share disable/enable` as first-class CLI commands?

| Option | Description | Selected |
|--------|-------------|----------|
| Yes — ship both plus REST (recommended) | CLI + REST endpoints | ✓ |
| Only REST, no CLI commands | Smaller CLI | |
| CLI only, reuse PATCH /api/v1/shares/{name} | Less REST surface | |

**User's choice:** Ship both CLI + dedicated REST endpoints.

### Q: Where does the `Enabled` status show in `dfsctl share list` / `show`?

| Option | Description | Selected |
|--------|-------------|----------|
| Add ENABLED column + show section (recommended) | Surface in default views | ✓ |
| Only in `share show` | Avoid widening list | |
| Via filtered `share list --disabled` | Opt-in | |

**User's choice:** Add ENABLED column + show section.

### Q: Behavior when `restore` runs and shares are still enabled?

| Option | Description | Selected |
|--------|-------------|----------|
| Hard 409 + list enabled shares (recommended, safety-first) | Explicit operator gate | ✓ |
| Offer `--force-disable-shares` flag | Smoother but hides side effect | |
| Interactive prompt | Interactive-heavy | |

**User's choice:** Hard 409 + list enabled shares.

### Q: `restore` confirmation prompt structure?

| Option | Description | Selected |
|--------|-------------|----------|
| Summary + plain Y/N, `--yes` to skip (recommended) | Matches existing UX | ✓ |
| Require typing the store name | Heavier confirmation | |
| Double prompt | Redundant | |

**User's choice:** Summary + Y/N + `--yes`.

### Q: Given the `share <name> <verb>` restructure extends across existing commands, how should it be staged?

| Option | Description | Selected |
|--------|-------------|----------|
| All-at-once breaking change (recommended) | CHANGELOG + migration note | ✓ |
| Dual-register with deprecation warning | Smoother migration | |
| Only new verbs use name-first; existing verbs stay | Inconsistent surface | |

**User's choice:** All-at-once breaking change. **Notes:** Follow-up messages confirmed: user wants "all share commands should have the same structure" and explicitly "we need to fix the delete as well" — this drove D-35 (flip every verb uniformly) rather than a partial flip.

### Q: Post-restore share re-enable — how ergonomic?

| Option | Description | Selected |
|--------|-------------|----------|
| Manual: operator runs `share <name> enable` per share (recommended) | Matches Phase 5 D-04 | ✓ |
| Flag on restore: `--reenable-shares-on-success` | Faster happy path | |
| Bulk command `dfsctl share enable-all --for-store <name>` | Helper | |

**User's choice:** Manual per-share only.

### Q: `restore --dry-run` scope — what does it actually validate?

| Option | Description | Selected |
|--------|-------------|----------|
| Pre-flight only (recommended) | Share check + record resolve + manifest validate | ✓ |
| Full manifest + SHA-256 verify | Adds payload download | |
| Skip dry-run | Not worth it | |

**User's choice:** Pre-flight only.

### Q: Authz for restore/backup endpoints — role boundary?

| Option | Description | Selected |
|--------|-------------|----------|
| `RequireAdmin()` for all (recommended) | Consistent | ✓ |
| operator triggers, admin for restore+repo | Split roles | |
| New `backup-operator` role | Dedicated | |

**User's choice:** RequireAdmin() everywhere.

### Q: What does a successful `restore` exit look like?

| Option | Description | Selected |
|--------|-------------|----------|
| Job record + next-step hint (recommended) | Reinforces Phase 5 D-04 | ✓ |
| Job record only | Clean | |
| Auto-re-enable prompt | Overlaps with earlier | |

**User's choice:** Job record + re-enable hint per disabled share.

### Q: When operator disables a share currently in use by live clients, behavior?

| Option | Description | Selected |
|--------|-------------|----------|
| Block synchronously until drained, no flag (recommended, Phase 5 D-03) | Synchronous contract | ✓ |
| Add `--wait-drained` flag (default true) | Toggle | |
| Warn if active mounts detected | Pre-check | |

**User's choice:** Synchronous block, no flag.

### Q: After a successful disable, should `share <name> disable` print a restore hint?

| Option | Description | Selected |
|--------|-------------|----------|
| Brief success + no hint (recommended) | Single-purpose | ✓ |
| Include restore-workflow hint | Noisy | |
| You decide | | |

**User's choice:** Brief success + no hint.

### Q: `share <name> enable` guard rails — anything to check before flipping the bit?

| Option | Description | Selected |
|--------|-------------|----------|
| No guard, just flip the bit (recommended, Phase 5 D-04) | DB authoritative | ✓ |
| Warn if store has running restore job | Defensive | |
| Refuse if store is mid-restore | Safest | |

**User's choice:** No guard.

### Q: Does `restore --dry-run` need the shares-disabled precondition?

| Option | Description | Selected |
|--------|-------------|----------|
| Skip share-disabled check for `--dry-run` (recommended) | Rehearsal on live system | ✓ |
| Enforce share-disabled even for `--dry-run` | Safer | |
| Opt-in `--dry-run --skip-share-check` | Verbose | |

**User's choice:** Skip share-disabled check for dry-run.

### Q: Does REST API expose a status/progress SSE/websocket, or only polling?

| Option | Description | Selected |
|--------|-------------|----------|
| Polling only (recommended) | Matches REQ API-06 | ✓ |
| Polling + SSE stream | Richer UI | |
| Polling + ETag long-poll | Middle ground | |

**User's choice:** Polling only.

### Q: Restore job's `Progress` field — keep at 0/100 only, or add milestone granularity?

| Option | Description | Selected |
|--------|-------------|----------|
| Milestones 0/10/30/60/95/100 (recommended) | Coarse but meaningful | ✓ |
| 0 → 100 only on terminal state | Simplest | |
| Byte-counted continuous progress | Most accurate, most invasive | |

**User's choice:** Milestones.

### Q: `share <name> disable` — does it take options controlling which adapters see the disable?

| Option | Description | Selected |
|--------|-------------|----------|
| All adapters atomically, no flag (recommended, Phase 5 D-01) | Single bool | ✓ |
| Add `--protocol nfs|smb` flag | Per-protocol | |
| You decide | | |

**User's choice:** All adapters atomically.

### Q: `restore` ergonomics when `--from <id>` uses a prefix?

| Option | Description | Selected |
|--------|-------------|----------|
| Exact ULID only (recommended) | Unambiguous | ✓ |
| Accept unambiguous prefix | Convenient | |
| Accept prefix only via `--from-prefix` | Opt-in | |

**User's choice:** Exact ULID only.

### Q: When restore finishes, do we create a BackupRecord for the pre-restore state?

| Option | Description | Selected |
|--------|-------------|----------|
| No implicit backup; document the workflow (recommended, Phase 5 deferred) | Explicit operator act | ✓ |
| Yes, auto-backup pre-restore (non-optional) | Safer | |
| Optional `--pre-restore-backup` flag | Opt-in | |

**User's choice:** No implicit backup — documented workflow.

---

## Job API surface width

### Q: Job list endpoint — scope and filters?

| Option | Description | Selected |
|--------|-------------|----------|
| Status + kind + limit filters (recommended) | Minimal useful filter set | ✓ |
| Only kind filter, no status | Smaller | |
| Full filter set + pagination | Richest | |

**User's choice:** Status + kind + limit (+ repo when multi-repo).

### Q: Cancel endpoint semantics?

| Option | Description | Selected |
|--------|-------------|----------|
| Cancel ctx, let executor unwind (recommended) | Reuse Phase 4 D-18 | ✓ |
| Add distinct `canceled` terminal state | Cleaner audit | |
| Don't ship cancel in v0.13.0 | Contradicts Ctrl-C decision | |

**User's choice:** Cancel ctx; reuse `interrupted` terminal state.

### Q: `dfsctl ... backup job cancel <id>` output?

| Option | Description | Selected |
|--------|-------------|----------|
| Return immediately, no wait (recommended) | Consistent trigger semantic | ✓ |
| Cancel + `--wait` transition to terminal | Know when done | |
| You decide | | |

**User's choice:** Return immediately.

### Q: If a job is already in a terminal state, what does cancel return?

| Option | Description | Selected |
|--------|-------------|----------|
| 200 OK, idempotent no-op (recommended) | Race-safe | ✓ |
| 409 Conflict with current status | Informative | |
| 204 No Content regardless | Minimalist | |

**User's choice:** 200 OK idempotent.

### Q: REST error body for backup/restore 4xx responses — what shape?

| Option | Description | Selected |
|--------|-------------|----------|
| Existing controlplane problem-details style (recommended) | Consistent | ✓ |
| Flat JSON `{error, code, extras}` | Simpler | |
| You decide | | |

**User's choice:** Existing problem-details shape.

### Q: Backup list default table columns?

| Option | Description | Selected |
|--------|-------------|----------|
| ID \| CREATED \| SIZE \| STATUS \| REPO \| PINNED (recommended) | Covers `--from` selection | ✓ |
| Minimal: ID \| CREATED \| SIZE \| REPO | Narrower | |
| Include ERROR column | Always empty on succeeded | |

**User's choice:** 6-column recommended layout.

### Q: `dfsctl ... backup job show <id>` table detail layout?

| Option | Description | Selected |
|--------|-------------|----------|
| Grouped sections + progress bar for running (recommended) | Matches `share show` | ✓ |
| Key-value flat table | Simpler | |
| Only JSON output | Breaks `-o table` | |

**User's choice:** Grouped sections.

### Q: Does `backup show <record-id>` also exist, or only `backup list`?

| Option | Description | Selected |
|--------|-------------|----------|
| Yes: ship `backup show <id>` (recommended) | Parallels other `show` cmds | ✓ |
| No, list only | Terser | |
| Expose full manifest.yaml | Too much internal detail | |

**User's choice:** Ship `backup show <id>`; keep it to BackupRecord fields (no manifest internals).

### Q: Cross-repo global job listing — is there a top-level `GET /api/backup-jobs`?

| Option | Description | Selected |
|--------|-------------|----------|
| No, only per-store (recommended) | Per-store symmetry | ✓ |
| Yes, admin-only global | Convenience for UI | |
| Defer to future phase | Minimal scope | |

**User's choice:** Per-store only.

### Q: How does the server persist 'job progress milestones'?

| Option | Description | Selected |
|--------|-------------|----------|
| Executor calls `BackupStore.UpdateBackupJobProgress` at each milestone (recommended) | Simple synchronous | ✓ |
| In-memory channel + writer goroutine | Batched | |
| Don't persist | Lossy | |

**User's choice:** Synchronous `UpdateBackupJobProgress` per milestone.

### Q: Where do old BackupJob rows (Phase 4 D-17 30-day pruner) get surfaced?

| Option | Description | Selected |
|--------|-------------|----------|
| Simply absent — pruner removes them (recommended) | D-17 invariant | ✓ |
| Soft-delete + filter | Forensic | |
| Operator-configurable pruner window | Scope creep | |

**User's choice:** Absent from lists.

---

## Claude's Discretion

Planner / researcher may refine without revisiting CONTEXT.md:

- Cobra subcommand file layout (one file per leaf vs. shared helpers).
- Spinner library or ASCII animation.
- HTTP handler composition (one `BackupHandler` vs. split resource handlers).
- `apiclient` method grouping (one `backups.go` vs. split files).
- Exact problem-details JSON field names.
- ULID prefix truncation length.
- Relative-time rendering helper.
- Whether `share create` moves under the new layout (user said "all same structure"; `create` doesn't yet have a target name).
- Whether `backup` parent command with no subcommand invokes trigger or prints help.

## Deferred Ideas

- Cross-repo global job list endpoint
- SSE / websocket / long-poll for job status
- Prefix matching on `--from <id>`
- Auto-re-enable shares post-restore
- Implicit pre-restore safety backup
- Operator-only trigger role
- Per-protocol share disable
- Distinct `canceled` terminal status
- Configurable BackupJob pruner window
- Client-side cron validation in dfsctl
- Byte-counted continuous progress
- Percent bar during `--wait`
- `backup verify <id>` / automatic test-restore
- `repo validate <name>` dry-run connectivity test
- K8s operator-driven backup triggers
- Bulk share re-enable helper
- `--dry-run` that also downloads payload
- Cursor-based pagination on job list

---

*Phase: 06-cli-rest-api-surface*
*Log written: 2026-04-17*
