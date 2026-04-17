---
phase: 06-cli-rest-api-surface
plan: 04
subsystem: cli/dfsctl
tags: [cli, dfsctl, repo, backup-repos, cobra, d-14, d-15, d-16, d-17, d-18, d-19, d-20, d-21, d-22]

requires:
  - phase: 06-cli-rest-api-surface
    provides: "pkg/apiclient BackupRepo CRUD helpers (Plan 02): CreateBackupRepo, ListBackupRepos, GetBackupRepo, UpdateBackupRepo, DeleteBackupRepo"
provides:
  - "cmd/dfsctl/commands/store/metadata/repo package: Cobra subtree for backup-repo management under a metadata store"
  - "repo.Cmd parent command (add/list/show/edit/remove verbs)"
  - "RepoList table renderer (D-20 columns: NAME|KIND|SCHEDULE|RETENTION|ENCRYPTED)"
  - "RepoDetail grouped-section renderer with S3 secret masking (T-06-04-01)"
  - "buildRepoConfig + buildS3RepoConfig for interactive + flag-driven per-kind config assembly"
  - "buildAddRequest / buildEditRequest factored for unit testability"
  - "doRemove(io.Writer, *Client, ...) testable core for the delete path"
affects: [phase-06-plan-06]

tech-stack:
  added: []
  patterns:
    - "Verb files self-register into Cmd via init() — parent stays agnostic to verb composition order"
    - "Testable core functions (buildAddRequest / buildEditRequest / doRemove) factored from Cobra RunE so unit tests can exercise business logic without TTY or credentials plumbing"
    - "httptest-backed apiclient.Client lets doRemove tests verify HTTP method + path + query without mocking the entire interface surface"
    - "Partial-patch detection via cobra.Command.Flags().Changed(name) — empty string / zero value are honored as explicit clears"
    - "S3 credential model in backup_repos.config mirrors pkg/blockstore/remote/s3 — credentials always prompted when flag empty (D-15 research)"

key-files:
  created:
    - cmd/dfsctl/commands/store/metadata/repo/repo.go
    - cmd/dfsctl/commands/store/metadata/repo/add.go
    - cmd/dfsctl/commands/store/metadata/repo/list.go
    - cmd/dfsctl/commands/store/metadata/repo/show.go
    - cmd/dfsctl/commands/store/metadata/repo/edit.go
    - cmd/dfsctl/commands/store/metadata/repo/remove.go
    - cmd/dfsctl/commands/store/metadata/repo/add_test.go
    - cmd/dfsctl/commands/store/metadata/repo/list_test.go
    - cmd/dfsctl/commands/store/metadata/repo/show_test.go
    - cmd/dfsctl/commands/store/metadata/repo/edit_test.go
    - cmd/dfsctl/commands/store/metadata/repo/remove_test.go
    - cmd/dfsctl/commands/store/metadata/repo/testdata/repo-s3.json
  modified: []

key-decisions:
  - "Verb files self-register into Cmd via their own init() — avoids cross-file ordering coupling and keeps repo.go a pure parent shell."
  - "Testable cores: buildAddRequest / buildEditRequest / doRemove take explicit arguments (not package globals) so tests can exercise validation / request-building without cobra runtime or TTY."
  - "S3 credentials live in backup_repos.config (D-15 research outcome) — the S3 block store at pkg/blockstore/remote/s3 REQUIRES explicit access_key_id + secret_access_key (no ambient AWS chain, see s3.NewFromConfig lines 85-88). Backup repos mirror this exactly for consistency."
  - "S3 prompt flow: 'bucket unset' is the interactive-mode trigger — when any bucket/region flag is passed we skip optional-field prompts (endpoint, prefix) to allow scripted runs to set only what they need. Credentials are always prompted when flag is empty, even in scripted mode, so CI secret material can stay out of shell history."
  - "Empty-string / zero-value values with explicit --flag are honored as 'clear' in edit. --schedule '' sends Schedule=ptr('') — the server is free to decide whether to treat an empty schedule as on-demand-only or a validation error."
  - "Table-mode credential masking is defense-in-depth on top of whatever server-side redaction policy exists (D-15 still open on the server side). JSON/YAML output echoes the server response verbatim — machine-readable pipelines are responsible for their own redaction (T-06-04-01 disposition)."
  - "AccessKeyID is masked too (not only SecretAccessKey) — the key ID is still PII that belongs in a secrets manager, not a terminal scrollback."
  - "titleize() helper replaces strings.Title (deprecated, locale-aware). Simple ASCII-only first-letter upper, rest as-is — all S3 config keys are ASCII so this is safe."

patterns-established:
  - "Cobra verb self-registration: each verb file owns its init() that adds itself to the package parent Cmd; repo.go's init is minimal"
  - "Testable-core pattern for stateful CLI commands: extract logic that consumes globals into a pure function taking those values as parameters"
  - "S3 secret masking in table mode, pass-through in JSON/YAML — consumer-appropriate redaction"

requirements-completed: [API-04]

duration: ~40min
completed: 2026-04-17T13:50:00Z
---

# Phase 6 Plan 4: dfsctl repo subtree Summary

**5-verb Cobra subtree under `dfsctl store metadata <name> repo` (add/list/show/edit/remove) that drives the Plan 02 BackupRepo apiclient with D-20 table columns, D-19 partial-patch edit, D-21 --purge-archives cascade, and D-15 S3 credentials stored in the repo config map.**

## Performance

- **Duration:** ~40 min
- **Started:** 2026-04-17T13:10:00Z
- **Completed:** 2026-04-17T13:50:00Z
- **Tasks:** 3
- **Files created:** 12 (6 source + 5 tests + 1 testdata)
- **Lines:** 824 source / 760 tests

## Accomplishments

- 5 verbs wired under `repo.Cmd`: add, list, show, edit, remove
- List table matches D-20 exactly: NAME | KIND | SCHEDULE | RETENTION | ENCRYPTED
- Show grouped-section detail with S3 credential masking in table mode (T-06-04-01 defense-in-depth)
- Add supports three input modes: flag-only, interactive per-kind prompts (local, s3), `--config @file.json`
- Edit is partial-patch via `cmd.Flags().Changed(name)` — 5 fields gated (schedule, keep-count, keep-age-days, encryption, encryption-key-ref)
- Remove default: config-only delete; `--purge-archives` cascades destination artifacts with a loud confirmation label
- Zero cron parser dependency in the dfsctl binary (D-18 verified: `go list -deps -test ./cmd/dfsctl/... | grep robfig` → 0 matches)
- 30 test cases covering retention-matrix, secret-masking, partial-patch, empty-state hint, error-surfacing

## Task Commits

Each task committed atomically (signed):

1. **Task 1: Parent + list + show** — `fc78e0cf` (feat)
2. **Task 2: add + edit verbs** — `ac66d559` (feat)
3. **Task 3: remove verb** — `15b4487d` (feat)

Plan 06 will add `metadata.Cmd.AddCommand(repo.Cmd)` — this plan deliberately does NOT modify metadata.go to avoid conflict with parallel Plan 03 + Plan 05 waves.

## Files Created

### Source (`cmd/dfsctl/commands/store/metadata/repo/`)

| File | LOC | Purpose |
|------|----:|---------|
| `repo.go` | 39 | Cobra parent command + minimal init wiring list/show |
| `add.go` | 277 | Flag set, interactive per-kind config builder, `--config @file` expansion, `buildAddRequest` testable core |
| `list.go` | 101 | D-20 RepoList table renderer + `renderRetention` / `renderEncrypted` helpers, empty-state hint |
| `show.go` | 131 | RepoDetail grouped-section renderer, S3 secret masking (AccessKeyID + SecretAccessKey → `***`), JSON/YAML pass-through |
| `edit.go` | 119 | Partial-patch via `Flags().Changed()`, explicit-clear semantics for empty-string / zero-int |
| `remove.go` | 76 | `--purge-archives` cascade, confirmation label suffix, `doRemove` testable core |

### Tests

| File | LOC | Coverage |
|------|----:|---------|
| `add_test.go` | 216 | Local + S3 flag flows, `@file` + inline JSON config, encryption-on-requires-key-ref gate, unknown kind rejection |
| `list_test.go` | 139 | Column rendering, retention matrix (5 permutations), empty-state hint guardrail, encrypted yes/no |
| `show_test.go` | 147 | S3 credential masking (positive + negative), local-kind config, JSON pass-through documentation |
| `edit_test.go` | 161 | Partial-patch (only changed flags set), no-flags error, explicit-clear (empty string, zero int), encryption toggle |
| `remove_test.go` | 137 | Default delete URL, `--purge-archives` query string, server error surfacing |
| `testdata/repo-s3.json` | 7 | Fixture for `--config @testdata/repo-s3.json` |

## Final Flag Matrices

### `repo add <store-name>`

| Flag | Type | Required | Purpose |
|------|------|----------|---------|
| `--name` | string | yes | Repo name |
| `--kind` | string | yes | `local` \| `s3` |
| `--config` | string | no | JSON or `@/path/to/file.json` — bypasses prompts |
| `--schedule` | string | no | Cron expression (server-validated, D-18) |
| `--keep-count` | int | no | D-17 retention by count |
| `--keep-age-days` | int | no | D-17 retention by age |
| `--encryption` | string | no | `on` \| `off` (D-16) |
| `--encryption-key-ref` | string | no | `env:VAR` \| `file:/path` — required when `--encryption on` |
| `--path` | string | no | `kind=local` path (prompted if empty) |
| `--bucket` | string | no | `kind=s3` bucket (prompted if empty) |
| `--region` | string | no | `kind=s3` region (defaults us-east-1) |
| `--endpoint` | string | no | `kind=s3` endpoint URL (optional) |
| `--prefix` | string | no | `kind=s3` key prefix (optional) |
| `--access-key-id` | string | no | `kind=s3` AWS key ID (prompted if empty) |
| `--secret-access-key` | string | no | `kind=s3` AWS secret (prompted hidden if empty) |

### `repo edit <store-name> <repo-name>` (D-19 partial patch)

| Flag | Type | Behaviour |
|------|------|-----------|
| `--schedule` | string | `Flags().Changed → Schedule=&v`; empty string is an explicit clear |
| `--keep-count` | int | zero value is an explicit clear |
| `--keep-age-days` | int | zero value is an explicit clear |
| `--encryption` | string | `on` → EncryptionEnabled=&true; anything else → &false |
| `--encryption-key-ref` | string | pointer-forwarded verbatim |

Passing zero flags → error: `No fields to update. Pass at least one of ...`.

### `repo remove <store-name> <repo-name>` (D-21)

| Flag | Type | Behaviour |
|------|------|-----------|
| `--purge-archives` | bool | adds `?purge_archives=true` to DELETE; confirmation label gains `(WILL ALSO DELETE ARCHIVE FILES)` |
| `-f, --force` | bool | skip confirmation prompt |

## D-15 Research Outcome

**Credentials are stored in `backup_repos.config` (per-repo), NOT sourced from ambient AWS chain.**

Evidence: `pkg/blockstore/remote/s3/store.go:85-88`:
```go
if config.AccessKey == "" || config.SecretKey == "" {
    return nil, errors.New("s3 block store: access_key_id and secret_access_key are required")
}
```

The S3 block store explicitly rejects configs without explicit credentials — there is no fallback to `LoadDefaultConfig` with the SDK's default provider chain. Backup repos mirror this convention exactly: the S3 prompt always prompts for `access_key_id` + `secret_access_key` when the flag is empty (even in scripted mode, so CI secret material stays out of shell history), and both land in the `config` map passed to the server.

The CLI emits the same key names the block store uses (`access_key_id`, `secret_access_key`). Future work to switch to ambient-chain support would change both stores in lockstep.

## Sample Output

### `repo list fast-meta`

```
NAME           KIND    SCHEDULE                 RETENTION        ENCRYPTED
nightly-local  local   CRON_TZ=UTC 0 2 * * *    count=7          no
weekly-s3      s3      -                        count=4 age=30d  yes
```

### `repo list empty-store`

```
No repos attached. Run: dfsctl store metadata empty-store repo add --name <name> --kind <local|s3>
```

### `repo show fast-meta weekly-s3` (table mode)

```
FIELD             VALUE
Name              weekly-s3
Kind              s3
Target            metadata_store/01J000000000000000000STORE
Schedule          CRON_TZ=UTC 0 3 * * 0
Retention         count=4 age=30d
Encrypted         yes
KeyRef            env:BACKUP_KEY
Created           2026-04-17 10:15:00
Updated           2026-04-17 10:15:00
Bucket            dittofs-backups
Region            us-east-1
Endpoint          https://s3.example.com
Prefix            meta/
AccessKeyID       ***
SecretAccessKey   ***
```

### `repo remove fast-meta daily-s3 --purge-archives --force`

```
Backup repo 'daily-s3' removed (archive files purged).
```

### `repo remove fast-meta daily-s3 --force` (default)

```
Backup repo 'daily-s3' removed (archive files retained).
```

## Test Outcomes

| Suite | Count | Result |
|-------|------:|:------:|
| TestRepoList_* | 4 (+5 subtests) | PASS |
| TestRepoShow_* | 3 | PASS |
| TestRepoAdd_* | 8 | PASS |
| TestRepoEdit_* | 6 | PASS |
| TestRepoRemove_* | 4 | PASS |
| **Total** | **30** | **PASS** |

Verification commands:

- `go build ./...` → clean
- `go vet ./cmd/dfsctl/commands/store/metadata/repo/...` → clean
- `go test ./cmd/dfsctl/commands/store/metadata/repo/... -count=1` → PASS (0.26s)
- `go list -deps -test ./cmd/dfsctl/... | grep -i robfig` → 0 matches (D-18 gate holds)

## Decisions Made

Documented inline at the top of each source file where relevant. Summarized:

1. **Verb self-registration** — each verb file owns its `init()` that adds itself to the parent; `repo.go` stays minimal.
2. **Testable-core refactor** — `buildAddRequest()`, `buildEditRequest(cmd)`, `doRemove(out, client, ...)` are pure functions the tests drive directly without spinning up a cobra harness or credentials store.
3. **D-15 finding codified** — S3 credentials go in `backup_repos.config`; implementation comment at `buildS3RepoConfig` explains.
4. **Scripted vs interactive S3 trigger** — `bucket == ""` is the discriminator. Passing `--bucket` alone implies scripted mode, and we skip prompts for `--endpoint`/`--prefix`. Credentials still prompt when their flag is empty to keep secrets out of shell history.
5. **Empty-string / zero is an explicit clear** in `edit` — relies on `Flags().Changed()`, not value inspection, so the operator intent is unambiguous.
6. **Table-mode-only credential masking** — JSON/YAML pass through the server response verbatim (machine-readable pipelines own their own redaction); table mode is the defense-in-depth layer for terminals.
7. **AccessKeyID masked too** — key IDs are still secrets-manager-grade PII.

## Deviations from Plan

### Rule 3 — Blocking issue (testability)

**1. [Rule 3] Factored testable cores from Cobra RunE**

- **Found during:** Task 2 + Task 3 test authoring.
- **Issue:** The plan prescribes tests that "assert req.X" or "fake client records DeleteBackupRepo(...)", but `apiclient.Client` is a concrete type (not an interface) and `runAdd`/`runRemove` go through `cmdutil.GetAuthenticatedClient()` which reads credentials from disk. Unit tests cannot drive the cobra RunE bodies directly without a credentials store + TTY.
- **Fix:** Extracted `buildAddRequest()` (no args, reads globals), `buildEditRequest(cmd *cobra.Command)`, and `doRemove(out io.Writer, client *apiclient.Client, storeName, repoName string, purgeArchives, force bool) error` from the RunE bodies. Tests drive these directly — `runAdd` / `runRemove` become thin wrappers that resolve the production client + stdout.
- **Files modified:** add.go, edit.go, remove.go — all three verbs.
- **Verification:** 30 tests pass; no regressions in production RunE paths (thin wrappers trivially forward to the cores).
- **Committed in:** ac66d559 (Task 2) + 15b4487d (Task 3).

### Rule 2 — Missing critical functionality

**2. [Rule 2] S3 interactive-vs-scripted discriminator**

- **Found during:** Task 2 test run — `TestRepoAdd_S3Kind_BuildsConfig` failed because the flag-driven path still hit the `--endpoint` optional prompt under `go test` (TTY unavailable → `^D` error).
- **Issue:** The original plan pseudocode prompted for every missing S3 field independently. Under `go test`, empty flags for optional fields (endpoint, prefix) caused the prompt to hang on TTY. The same failure would have hit any CI run that set some flags but not others.
- **Fix:** Adopted the `block/remote/add.go` convention: `bucket == ""` is the interactive-mode trigger. When bucket is set via flag we treat the run as scripted and leave optional fields as-is. Credentials (access_key_id, secret_access_key) still prompt when empty even in scripted mode so CI secret material stays out of shell history.
- **Files modified:** add.go:buildS3RepoConfig
- **Verification:** All 30 tests pass, including `TestRepoAdd_S3Kind_BuildsConfig` with `--bucket` / `--region` / `--access-key-id` / `--secret-access-key` flags but no `--endpoint` / `--prefix`.
- **Committed in:** ac66d559 (Task 2, original landing).

**3. [Rule 2] Region default when neither flag nor prompt set it**

- **Found during:** Task 2 while implementing the scripted-mode S3 path.
- **Issue:** When bucket is supplied by flag but region is not, the original pseudocode would silently pass `region=""` to the server. The existing `block/remote/add.go` cobra default is `"us-east-1"` — repo add should behave the same for consistency.
- **Fix:** Added an explicit default-region fallback after the prompt branch: `if region == "" { region = "us-east-1" }`.
- **Files modified:** add.go:buildS3RepoConfig.
- **Committed in:** ac66d559.

### Style fix

**4. [Style] Replaced `strings.Title` with local `titleize()` helper**

- **Found during:** Task 1 show.go drafting. `strings.Title` is deprecated (locale-aware, Unicode pitfalls).
- **Fix:** Added local `titleize(s string) string` that upper-cases the first byte. All S3 config keys are ASCII so this is safe.
- **Files modified:** show.go.
- **Committed in:** fc78e0cf.

---

**Total deviations:** 4 auto-fixed (1 Rule 3 blocking, 2 Rule 2 missing-critical, 1 style).
**Impact on plan:** All deviations preserve the plan's behavioural contract. The testable-core refactor is a net win — it lets us cover the validation logic without integration plumbing. The S3 scripted-mode discriminator and region default align this CLI with the existing `block/remote/add.go` precedent.

## Issues Encountered

Minor: initial TTY-hang on `TestRepoAdd_S3Kind_BuildsConfig` under `go test` — resolved by adopting the scripted-mode discriminator documented above.

## User Setup Required

None — no external service configuration required. The commands operate entirely against the existing REST endpoints wired in Plan 02.

## Next Phase Readiness

**Ready:** Plan 06 integrator task adds `metadata.Cmd.AddCommand(repo.Cmd)` to wire this subtree under `dfsctl store metadata`. The package exports `Cmd` at the expected path.

**Threat flags:** none new beyond the plan's `<threat_model>`:

- T-06-04-01 mitigated via `renderConfigRows` in show.go (AccessKeyID + SecretAccessKey → `***` in table mode).
- T-06-04-02 mitigated: S3 credential prompts use `prompt.PasswordWithValidation` (hidden entry) when `--secret-access-key` is empty. Long help text documents the history-leak trade-off.
- T-06-04-03 accepted: `--encryption-key-ref` pass-through, server-side format validation (Phase 3 D-08/D-09).
- T-06-04-04 mitigated: zero cron parser dependency leaked into dfsctl (`grep robfig` → 0 matches). Server-side only.
- T-06-04-05 mitigated: confirmation prompt unless `--force`; label explicitly calls out destructive intent.
- T-06-04-06 accepted: authz is server-side middleware (Plan 02).

## Self-Check: PASSED

Verification commands:

- `grep -n 'var Cmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/repo.go` → match
- `grep -n 'var addCmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/add.go` → match
- `grep -n 'var editCmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/edit.go` → match
- `grep -n 'var removeCmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/remove.go` → match
- `grep -n 'var listCmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/list.go` → match
- `grep -n 'var showCmd = &cobra.Command' cmd/dfsctl/commands/store/metadata/repo/show.go` → match
- `grep -n '"NAME", "KIND", "SCHEDULE", "RETENTION", "ENCRYPTED"' cmd/dfsctl/commands/store/metadata/repo/list.go` → match
- `grep -c 'cmd.Flags().Changed(' cmd/dfsctl/commands/store/metadata/repo/edit.go` → 5 (≥4 required)
- `grep -n '"purge-archives"' cmd/dfsctl/commands/store/metadata/repo/remove.go` → match
- `grep -n 'RunDeleteWithConfirmation' cmd/dfsctl/commands/store/metadata/repo/remove.go` → match
- `grep -rn 'robfig/cron' cmd/dfsctl/commands/store/metadata/repo/` → 0 matches
- `go list -deps -test ./cmd/dfsctl/... | grep robfig` → 0 matches
- `go build ./...` → clean
- `go vet ./cmd/dfsctl/commands/store/metadata/repo/...` → clean
- `go test ./cmd/dfsctl/commands/store/metadata/repo/... -count=1` → PASS (30 cases)
- All 3 commits (fc78e0cf, ac66d559, 15b4487d) present in `git log --oneline`.

---
*Phase: 06-cli-rest-api-surface*
*Completed: 2026-04-17*
