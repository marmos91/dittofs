# DittoFS CLI Architecture

This document describes the architecture of the DittoFS CLI tools following the Phase 1 implementation.

## Overview

The CLI is split into two separate binaries following Go best practices:

1. **`dfs`** - Server daemon management (local operations)
2. **`dfsctl`** - REST API client for remote control plane operations

## Binary Structure

### dfs (Server CLI)

Located in `cmd/dfs/`, this binary handles local server management:

```
dfs
├── start         Start the DittoFS server
├── stop          Stop the DittoFS server
├── status        Show server status
├── init          Initialize configuration file
├── migrate       Run database migrations
├── version       Show version information
└── config        Configuration management
    ├── init      Initialize config file
    ├── edit      Open config in editor
    ├── validate  Validate configuration
    └── show      Display current config
```

### dfsctl (Client CLI)

Located in `cmd/dfsctl/`, this binary handles remote server management via REST API:

```
dfsctl
├── login         Authenticate with DittoFS server
├── logout        Clear stored credentials
├── version       Show version information
└── context       Manage server contexts (multi-server)
    ├── list      List all contexts
    ├── use       Switch to a different context
    ├── current   Show current context
    ├── rename    Rename a context
    └── delete    Delete a context
├── grace         Grace period management
│   ├── end       End the current grace period
│   └── status    Show grace period status
├── client        Client management
│   ├── evict     Evict a client
│   ├── list      List connected clients
│   └── sessions  List client sessions
├── idmap         Identity mapping management
│   ├── add       Add identity mapping
│   ├── list      List identity mappings
│   └── remove    Remove identity mapping
└── netgroup      Netgroup management
    ├── create    Create a netgroup
    ├── delete    Delete a netgroup
    ├── list      List netgroups
    ├── show      Show netgroup details
    └── members   Manage netgroup members
```

## Package Structure

### Internal Packages

Located in `internal/cli/`:

#### output/

Output formatting utilities:

- `format.go` - Format types and Printer for colored output
- `table.go` - Table rendering using tablewriter
- `json.go` - JSON output formatting
- `yaml.go` - YAML output formatting

Usage:
```go
printer := output.NewPrinter(os.Stdout, output.FormatTable, true)
printer.Print(data)
printer.Success("Operation completed")
printer.Error("Something went wrong")
```

#### prompt/

Interactive terminal prompts using promptui:

- `confirm.go` - Yes/no confirmation prompts
- `password.go` - Password input with masking
- `select.go` - Selection menus
- `input.go` - Text input prompts

Usage:
```go
confirmed, err := prompt.Confirm("Delete this item?", false)
password, err := prompt.NewPassword()
selection, err := prompt.SelectString("Choose option", []string{"a", "b", "c"})
```

#### credentials/

Credential and context management for dfsctl:

- `store.go` - Context storage and management

Credentials are stored in `~/.config/dfsctl/config.json` with mode 0600.

### Public Packages

Located in `pkg/`:

#### apiclient/

REST API client for dfsctl:

- `client.go` - HTTP client wrapper
- `auth.go` - Authentication (login, token refresh)
- `errors.go` - API error types

Usage:
```go
client := apiclient.New("http://localhost:8080")
tokens, err := client.Login(username, password)
client = client.WithToken(tokens.AccessToken)
```

## Dependencies

New dependencies added:

- `github.com/spf13/cobra` - CLI framework (industry standard)
- `github.com/manifoldco/promptui` - Interactive prompts
- `github.com/olekukonko/tablewriter` - Table output formatting

## Configuration

### dfs

Uses the same configuration as before, located at `$XDG_CONFIG_HOME/dittofs/config.yaml`.

### dfsctl

Stores credentials and preferences in `$XDG_CONFIG_HOME/dfsctl/config.json`:

```json
{
  "current_context": "default",
  "contexts": {
    "default": {
      "server_url": "http://localhost:8080",
      "username": "admin",
      "access_token": "eyJ...",
      "refresh_token": "eyJ...",
      "expires_at": "2025-01-21T12:00:00Z"
    }
  },
  "preferences": {
    "default_output": "table",
    "color": "auto"
  }
}
```

## Building

Build both binaries:

```bash
# Build dfs
go build -o dfs ./cmd/dfs

# Build dfsctl
go build -o dfsctl ./cmd/dfsctl

# Build with version info
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o dfs ./cmd/dfs
```

## Testing

Run CLI package tests:

```bash
# Run all CLI tests
go test ./internal/cli/... ./pkg/apiclient/...

# Run with verbose output
go test -v ./internal/cli/...

# Run specific package tests
go test ./internal/cli/credentials/
```

## Usage Examples

### Server Management (dfs)

```bash
# Initialize configuration
dfs init

# Validate configuration
dfs config validate

# Start server
dfs start

# Start with custom config
dfs start --config /etc/dittofs/config.yaml

# Check status
dfs status --pid-file /var/run/dittofs.pid

# Stop server
dfs stop --pid-file /var/run/dittofs.pid
```

### Remote Management (dfsctl)

```bash
# Login to server
dfsctl login --server http://localhost:8080 --username admin

# List contexts
dfsctl context list

# Switch context
dfsctl context use production

# Get current context
dfsctl context current

# Logout
dfsctl logout
```

### Block Store Garbage Collection (v0.15.0 Phase 11)

v0.15.0 (Phase 11 / A2) replaces the previous path-prefix GC with a
fail-closed mark-sweep over the union of every live block's
`ContentHash`. Two `dfsctl` subcommands drive and inspect it:

#### `dfsctl store block gc <share> [--dry-run]`

Run garbage collection for the named share. The mark phase enumerates
every live `ContentHash` across all shares pointing at the same remote
(cross-share aggregation by `bucket+endpoint+prefix`). The sweep phase
deletes any `cas/.../` object that is absent from the live set AND
whose `LastModified` is older than the configured grace period
(default `gc.grace_period=1h`).

```bash
# Run mark-sweep against the default remote configured for share /archive
dfsctl store block gc /archive

# Dry-run: skip DELETEs; print up to gc.dry_run_sample_size candidate keys
dfsctl store block gc /archive --dry-run
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--dry-run` | `false` | Skip DELETEs; print up to `gc.dry_run_sample_size` candidate keys (default 1000). Critical for first-time deployment confidence. |

**Output:** A `GCStats` record with `hashes_marked` (live blocks
referenced), `objects_scanned` (total CAS objects found in the store),
`objects_swept` (orphans deleted, or would-be-deleted under
`--dry-run`), `bytes_freed`, `duration_ms`, and `error_count` plus a
sample of the first errors when `error_count > 0`.

**Fail-closed posture:** any mark-phase error aborts the sweep entirely
(no objects are deleted). Sweep-side per-prefix DELETE errors are
captured and the sweep continues; surviving garbage is reclaimed on
the next run.

#### `dfsctl store block gc-status <share>`

Print the most recent `GCRunSummary` for the named share, read from
`<localStore>/gc-state/last-run.json`. Useful for inspecting a
periodic run launched via `gc.interval` without having to grep slog
output.

```bash
dfsctl store block gc-status /archive
```

**Output:** the `GCRunSummary` JSON: `run_id`, `started_at`,
`finished_at`, `hashes_marked`, `objects_scanned`, `objects_swept`,
`bytes_freed`, `duration_ms`, `error_count`, `error_samples`, plus the
configuration snapshot (`grace_period`, `dry_run`) used for the run.

See [ARCHITECTURE.md](ARCHITECTURE.md#garbage-collection-mark-sweep-v0150-phase-11)
for the full mark-sweep design and [CONFIGURATION.md](CONFIGURATION.md)
for every `gc.*` knob.

### Block Store INV-02 Refcount Audit (v0.15.0 Phase 12)

v0.15.0 (Phase 12 / A3) ships an operator-facing audit for INV-02
(`∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)`). The audit runs the
same metadata enumeration as the GC mark phase (D-36) and emits
aggregate counts; a non-zero `delta` indicates refcount drift between
the metadata and block stores.

#### `dfsctl blockstore audit-refcounts <share>`

```
dfsctl blockstore audit-refcounts <share> [--output table|json|yaml]
```

Run the INV-02 reconciliation audit for the named share. Emits
aggregate counts and persists the last-run summary at
`<localStore>/audit-state/last-inv02.json` (mirrors Phase 11 GC's
`last-run.json`).

```bash
# Default text-table output
dfsctl blockstore audit-refcounts /archive

# Structured JSON for log aggregation / alerting
dfsctl blockstore audit-refcounts /archive --output json

# YAML for tooling that prefers it
dfsctl blockstore audit-refcounts /archive --output yaml
```

**Output fields:**

| Field | Description |
|-------|-------------|
| `share` | Share name (matches the CLI argument) |
| `started_at` | RFC3339 timestamp when the audit started |
| `duration_ms` | Wall-clock duration of the audit, in milliseconds |
| `total_files` | Number of files enumerated from `MetadataStore` |
| `total_refs` | Sum of `len(FileAttr.Blocks)` across all files |
| `total_refcount` | Sum of `FileBlock.RefCount` across the FileBlockStore |
| `delta` | `total_refs - total_refcount`. Zero ⇒ INV-02 holds. Non-zero ⇒ drift; investigate. |

**Exit codes:**

- `0` — audit completed successfully **regardless of `delta` value**.
  A non-zero `delta` is informational, not an error: the CLI reports
  drift so operators can triage; it does not fail the command.
- non-zero — infrastructure failure (auth, network, share not found,
  metadata-store error, etc.).

**Cross-reference:** [FAQ.md](FAQ.md#how-do-i-run-the-inv-02-audit) for
operator guidance and CI-friendly invocation patterns.

The audit is **operator-invoked**, not periodic. Schedule via cron at
the cadence that matches your operational risk tolerance until a
periodic-scheduler phase ships. For belt-and-braces protection, the
property-based fuzzer at `pkg/metadata/storetest/inv02_fuzz_test.go`
runs against all 3 built-in backends in CI on every PR, asserting
INV-02 at every quiescent point under concurrent load.

### Block Store Migration (v0.15.x Phase 14)

Phase 14 (v0.15.x A5) ships the offline migration tool that converts
a v0.13/v0.14 share's block layout from the legacy
`{payloadID}/block-{idx}` keyspace to the v0.15 content-addressable
(CAS) `cas/{hh}/{hh}/{hex}` keyspace. The tool re-chunks every file
via FastCDC, uploads CAS chunks (dedup-aware via `GetByHash`), runs
a HEAD-per-ref integrity check, and atomically flips the per-share
`block_layout` flag to `cas-only` before deleting legacy keys.

> **`docs/CLI.md` is hand-maintained**, not auto-generated. The
> sections below mirror the cobra command tree and flag set; they
> are kept in sync with `cmd/dfsctl/commands/blockstore/migrate.go`
> on every change. Run `dfsctl blockstore migrate --help` and
> `dfsctl blockstore migrate status --help` for the canonical
> on-the-wire reference. The full operator runbook with worked
> transcripts lives at
> [BLOCKSTORE_MIGRATION.md](BLOCKSTORE_MIGRATION.md#phase-14-v015x-a5--dfsctl-blockstore-migrate-runbook).

#### `dfsctl blockstore migrate`

```text
dfsctl blockstore migrate --share <name> [flags]
```

Migrate a share's blocks from the legacy v0.13/v0.14 path-indexed
layout to the v0.15 content-addressed (CAS) layout.

**Offline-only.** The tool refuses to start while a daemon is
serving the target share (D-A5). Stop the daemon (or the
target-share's adapter) before invoking.

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--share NAME` | (required) | Share name to migrate. |
| `--dry-run` | `false` | Walk the file list and report estimated upload bytes WITHOUT writing any data (no metadata-store, FileBlockStore, or RemoteStore mutation). |
| `--parallel N` | `4` | Number of concurrent per-file workers. Clamped to `[1, 64]` with a warning log on out-of-range values (D-A10). |
| `--bandwidth-limit STR` | empty | Aggregate upload-byte ceiling. Accepts SI (`KB`/`MB`/`GB`/`TB`/`PB`, 1000-base) and IEC (`KiB`/`MiB`/`GiB`/`TiB`/`PiB`, 1024-base) suffixes. Empty / `0` = unlimited (D-A11). The limit applies to PUT bytes only; legacy reads stay unmetered (D-A9). |
| `--state-dir DIR` | `{share-data-dir}/.migration-state` | Override journal/snapshot directory. The journal is `.migration-state.jsonl`; the rolling snapshot is `.migration-state.snapshot.json`. |

**Examples:**

```bash
# Migrate a share with default 4 workers, no bandwidth cap
dfsctl blockstore migrate --share myshare

# Estimate upload size before committing to a maintenance window
dfsctl blockstore migrate --share myshare --dry-run

# Cap aggregate upload at 50 MB/s across all workers
dfsctl blockstore migrate --share myshare --bandwidth-limit 50MB

# TB-scale share: 16 workers, 200 MB/s aggregate cap
dfsctl blockstore migrate --share vm-images --parallel 16 --bandwidth-limit 200MB

# Override journal directory (e.g., for shared-FS daemons)
dfsctl blockstore migrate --share myshare --state-dir /var/lib/dittofs/migrate-state
```

**Stdout summary** (single-line, machine-parseable):

```text
Migration applied: files_total=N files_done=N files_skipped=N \
    bytes_uploaded=N bytes_deduped=N duration_ms=N
```

The `applied` token is replaced by `dry-run` when `--dry-run` is
passed.

**Progress reporting:**

- **TTY stdout:** 10 fps `\r`-rewriting progress bar overlaid on
  stdout (`Migrating: D/T (PCT%) ETA E`). Bar is silenced when
  stdout is piped.
- **Structured slog** (always on, machine-parseable): every per-file
  commit emits a `migrate.file.committed` event with
  `blocks_count`, `bytes_uploaded`, `bytes_deduped`, `files_done`,
  `files_total`. Pipe stdout to a logfile to capture the stream.

**Exit codes:**

- `0` — migration completed successfully (all files committed,
  integrity check passed, `block_layout` flipped to `cas-only`,
  legacy keys deleted).
- non-zero — daemon-active probe failed, integrity check failed,
  or any infrastructure error (auth, network, metadata-store,
  remote-store). The journal stays in place and a re-run resumes
  from the last committed file.

**Resume / recovery:** the journal at `{share-data-dir}/.migration-state.jsonl`
captures every successful per-file commit. Re-running the same
command resumes from the journal head. See
[BLOCKSTORE_MIGRATION.md — Recovery](BLOCKSTORE_MIGRATION.md#recovery)
for the full crash-recovery and integrity-check failure procedures.

**Cross-references:**

- [BLOCKSTORE_MIGRATION.md](BLOCKSTORE_MIGRATION.md#phase-14-v015x-a5--dfsctl-blockstore-migrate-runbook) — full operator runbook with worked transcripts.
- [ARCHITECTURE.md](ARCHITECTURE.md#migration--block-layout-routing-v015x-a5) — design rationale + dual-read shim + per-share `block_layout` flag.
- [FAQ.md](FAQ.md#how-do-i-migrate-from-v013--v014-to-v015) — operator quick-start.

#### `dfsctl blockstore migrate status`

```text
dfsctl blockstore migrate status --share <name> [--output table|json|yaml]
```

Show migration progress for a share. Combines the per-share
`.migration-state.jsonl` journal (when a migration ran or is running)
with the share's `BlockLayout` flag from the metadata store, returning
a unified view of progress and current state.

The command is **online-friendly** — it queries the daemon's REST
endpoint, so it works against a running daemon (unlike `migrate`
itself which requires the daemon to be offline for the target share).
Authentication is via the operator's existing `dfsctl login`
credentials.

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--share NAME` | (required) | Share name to query. |
| `--output, -o` | `table` | Output format: `table` (FIELD/VALUE key-value table), `json`, or `yaml`. |

**Output fields:**

| Field | Description |
|-------|-------------|
| `Share` | Share name (matches the CLI argument). |
| `BlockLayout` | `legacy` or `cas-only`. Authoritative state from the metadata store. |
| `FilesTotal` | Total regular files in the share. `-1` if the server-side walk hit its 30s timeout (TB-scale shares can opt out by querying the REST endpoint with `?with_total=false`). |
| `FilesDone` | Files committed by the migration journal. |
| `FilesSkipped` | Files skipped (already CAS-laid-out before the migration started, or zero-byte). |
| `BytesUploaded` | Sum of bytes PUT to remote across done files. |
| `BytesDeduped` | Sum of bytes hit by `GetByHash` (skipped at upload). |
| `JournalPresent` | `true` if `.migration-state.jsonl` exists and is non-empty. |
| `SnapshotPresent` | `true` if the rolling snapshot exists. |
| `LastCommitAt` | RFC3339 timestamp (UTC) of the last journal commit. Empty if no journal. |

**Examples:**

```bash
# Default human-readable table
dfsctl blockstore migrate status --share myshare

# JSON for log aggregation / dashboard scraping
dfsctl blockstore migrate status --share myshare -o json

# YAML for tooling that prefers it
dfsctl blockstore migrate status --share myshare -o yaml

# Pre-flight check before scheduling a maintenance window
dfsctl blockstore migrate status --share myshare -o json | \
    jq '{layout: .block_layout, files: .files_total}'
```

**REST equivalent:** `GET /api/v1/blockstore/migrate/status?share=NAME`
returns the same JSON shape and is admin-only (JWTAuth +
RequireAdmin). The `?with_total=false` query parameter skips the
file-count walk on pathologically large shares.

**Exit codes:**

- `0` — status retrieved successfully (regardless of migration
  state).
- non-zero — share unknown (404 maps to a friendly `share %q not
  found` message), auth failure, network error, or other
  infrastructure failure.

**Cross-reference:** [BLOCKSTORE_MIGRATION.md](BLOCKSTORE_MIGRATION.md#phase-14-v015x-a5--dfsctl-blockstore-migrate-runbook)
for the full operator runbook with worked transcripts.

### Share Snapshots

Point-in-time, reference-based protection for a share's content.
Five `dfsctl share snapshot` subcommands: `create`, `list`, `show`,
`delete`, `restore`. All subcommands require admin auth (JWT) and
operate against the daemon's snapshot store.

For workflows, recovery procedures, and the verify-gate explanation
see [SNAPSHOTS.md](SNAPSHOTS.md#7-restore-runbook).

#### `dfsctl share snapshot create`

Create a point-in-time snapshot of a share. By default the command
blocks until the snapshot reaches `ready` or `failed`.

**Synopsis:**

```
dfsctl share snapshot create <share> [flags]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--name` | `""` | Human-friendly name for the snapshot. Not required to be unique. |
| `--no-verify` | `false` | Skip the verify gate (upload drain + remote HEAD probes). The snapshot completes with `remote_durable=false`. |
| `--retry` | `""` | Resume orchestration from a prior `failed` snapshot ID. The target row must exist and be in `failed` state. |
| `--no-wait` | `false` | Return immediately with the new snapshot ID; do not block on `ready`. |

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | Snapshot reached `ready` (or `--no-wait` returned the new ID). |
| `1` | Snapshot reached `failed`, authentication failure, network error, retry target invalid (404 / 409), or a verify / drain failure (504). |

**Examples:**

```bash
# Create a snapshot and block until it is ready.
dfsctl share snapshot create /photos

# Create with a name, return immediately.
dfsctl share snapshot create /photos --name weekly-2026-05 --no-wait

# Resume a failed snapshot.
dfsctl share snapshot create /photos --retry 4c19fbe0
```

#### `dfsctl share snapshot list`

List snapshots for a share. Newest-first. Filters AND together; no
pagination flags.

**Synopsis:**

```
dfsctl share snapshot list <share> [flags]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--state` | `""` | Filter by state: `creating`, `ready`, or `failed`. |
| `--name-prefix` | `""` | Filter by name prefix (case-sensitive). |
| `--no-relative` | `false` | Render `CREATED` as ISO 8601 instead of relative ("2h ago"). |

**Table columns:** `ID` (8-char short of UUID), `NAME`, `STATE`,
`DURABLE`, `CREATED`, `SIZE`. The `SIZE` column is always `-` in
list mode; use `show` to resolve the manifest hash count.

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | List retrieved (an empty list is still success). |
| `1` | Share not found (404), authentication failure, or network error. |

**Examples:**

```bash
dfsctl share snapshot list /photos
dfsctl share snapshot list /photos --state=failed
dfsctl share snapshot list /photos --name-prefix=pre-restore-
dfsctl share snapshot list /photos -o json
```

#### `dfsctl share snapshot show`

Detail view for a single snapshot. Resolves the manifest hash count
and the metadata-dump size from disk.

**Synopsis:**

```
dfsctl share snapshot show <share> <id>
```

This subcommand has no flags beyond the global `-o` output format.

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | Record retrieved. |
| `1` | Snapshot not found (404), share not found (404), authentication failure, or network error. |

**Example:**

```bash
dfsctl share snapshot show /photos 7a3ec1b2
```

#### `dfsctl share snapshot delete`

Delete a snapshot. Removes the database row, wipes the on-disk
artifacts, and releases the GC hold on the snapshot's referenced
blocks.

**Synopsis:**

```
dfsctl share snapshot delete <share> <id> [--yes]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--yes` | `false` | Skip the interactive Y/N confirmation. |

Safety snapshots (`pre-restore-*` names) are not treated specially —
the same prompt + `--yes` policy applies.

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | Snapshot deleted, or the operator aborted at the prompt. |
| `1` | Snapshot not found (404), authentication failure, or network error. |

**Examples:**

```bash
dfsctl share snapshot delete /photos 9f2dab17
dfsctl share snapshot delete /photos 9f2dab17 --yes
```

#### `dfsctl share snapshot restore`

Restore a share from a snapshot. The restore is synchronous: the
command blocks until the server reports success or failure.

**Pre-flight requirements:**

- The share must be disabled (`dfsctl share disable /<share>`).
  The command refuses on an enabled share with a hint to disable
  first.
- The snapshot must be `remote_durable=true`, OR `--force` must be
  passed.

**Synopsis:**

```
dfsctl share snapshot restore <share> <id> [flags]
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--yes` | `false` | Skip the interactive Y/N confirmation. |
| `--force` | `false` | Restore a snapshot whose `remote_durable=false`. Maps to `allow_non_durable=true` on the REST request. |

On success, prints the safety snapshot ID returned by the server.
The safety snap captures the share's state immediately before
restore; delete it explicitly once the restored share is validated.

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | Restore completed successfully. |
| `1` | Share enabled (pre-flight refusal), snapshot not found (404), not remotely durable without `--force` (412), drain timeout (504), other restore failure (500), authentication failure, or network error. |

**Examples:**

```bash
# Standard restore flow.
dfsctl share disable /photos
dfsctl share snapshot restore /photos 7a3ec1b2
dfsctl share enable /photos

# Restore from a non-durable snapshot.
dfsctl share snapshot restore /photos 9f2dab17 --force --yes
```

**Cross-reference:**
[SNAPSHOTS.md — Restore runbook](SNAPSHOTS.md#7-restore-runbook)
for the complete step-by-step procedure, safety-snap recovery, and
the failure-mode taxonomy.

### Block Store Migration

v0.15.0 replaces the legacy `{payloadID}/block-{idx}` block layout with the
unified content-addressed (CAS) layout and ships the offline one-shot
conversion as a server-side `dfs` subcommand (NOT a `dfsctl` REST round-trip
— the daemon must be stopped because the migration rewrites blocks in place).
The boot guard refuses to start on un-migrated shares; the recovery path is
to run `dfs migrate-to-cas` and retry.

#### `dfs migrate-to-cas`

Migrate a v0.14.x storage directory's legacy `{payloadID}/block-{idx}` block
layout to the v0.15+ content-addressed (CAS) layout. Required before
`dfs start` will succeed on a pre-v0.15 install.

**Offline operation** — stop the server first (`dfs stop`). The command
refuses to run while a live `dfs` PID lockfile is detected.

**Synopsis:**

```
dfs migrate-to-cas [flags]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--storage-dir` | path | **required** | Storage root directory. Shares are discovered under `<storage-dir>/shares/`. There is no config-derived default; pass the path explicitly. |
| `--metadata-dir` | path | **required** | Path to the badger metadata database directory (the `path` value from the metadata store config). Required so the migration tool can enumerate legacy files and commit CAS block manifests. |
| `--share` | string | (all shares) | Scope migration to one share. Default migrates every share under the storage root. |
| `--dry-run` | bool | `false` | Walk the legacy `{payloadID}/block-{idx}` tree and report file count, total bytes, estimated dedup ratio, and ETA. Writes nothing. Does not touch the journal; does not write the sentinel. |
| `--json` | bool | `false` | Emit one JSON object per line on stdout (machine-parseable progress). |
| `--config` | path | (default) | Override config file location. Inherited from the root `dfs` command. |

**Exit codes:**

| Code | Meaning |
|------|---------|
| `0` | Success — all targeted shares migrated; per-share `.cas-migrated-v1` sentinels written. |
| `1` | Generic error (PID guard tripped, config load failure, IO error, share discovery failure). |
| `2` | Migration failed mid-flight. The per-share journal is preserved at `<storage_dir>/shares/<share>/.dittofs-migrate-to-cas.state`; stderr describes the resume point. Rerun the same command to resume. |

**Progress reporting:**

- Plain text (default): `[<share>] N files, X.X MiB/s, dedup_hits=K`,
  emitted approximately once per second per share.
- JSON (`--json`): one object per line per share per second:
  ```json
  {"ts":"<RFC3339>","share":"<name>","files_done":N,"bytes_done":N,"files_per_sec":F,"mib_per_sec":F,"dedup_hits":N,"eta_seconds":F}
  ```

**Idempotent / journaled resume:** the per-share journal at
`<storage_dir>/shares/<share>/.dittofs-migrate-to-cas.state` records the
last-completed file path and byte offset. If interrupted, rerunning
`dfs migrate-to-cas` resumes from the journaled position. The CAS Put
surface is idempotent on hash collision, so re-processing an in-flight file
at the resume point is safe (chunks already uploaded are treated as dedup
hits on the second pass). The journal is removed (best-effort) only AFTER
the per-share sentinel write succeeds.

**Examples:**

Dry-run a migration to see what would happen:

```bash
dfs migrate-to-cas --storage-dir /var/lib/dittofs/storage --metadata-dir /var/lib/dittofs/metadata --dry-run
```

Migrate one large share off-hours and capture machine-parseable progress
for log aggregation:

```bash
dfs stop
dfs migrate-to-cas --storage-dir /var/lib/dittofs/storage --metadata-dir /var/lib/dittofs/metadata --share data --json | tee migration.log
```

Migrate every share under the storage root:

```bash
dfs stop
dfs migrate-to-cas --storage-dir /var/lib/dittofs/storage --metadata-dir /var/lib/dittofs/metadata
```

Migrate a single share off-hours:

```bash
dfs stop
dfs migrate-to-cas --storage-dir /var/lib/dittofs/storage --metadata-dir /var/lib/dittofs/metadata --share data
```

**See also:**

- [docs/CONFIGURATION.md §Migration](CONFIGURATION.md#migration) for the
  full upgrade procedure, boot-guard exit code 78, sentinel file format,
  crash-safety guarantees, and recovery from a failed migration.

## Global Flags

### dfs

- `--config` - Path to configuration file

### dfsctl

- `--server` - Override server URL
- `--token` - Override authentication token
- `--output, -o` - Output format (table|json|yaml)
- `--no-color` - Disable colored output
- `--verbose, -v` - Enable verbose output

