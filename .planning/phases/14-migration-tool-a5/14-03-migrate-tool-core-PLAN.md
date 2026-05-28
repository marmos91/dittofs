---
phase: 14-migration-tool-a5
plan: 03
type: execute
wave: 3
depends_on: [14-01-share-blocklayout, 14-02-engine-blocklayout-routing]
files_modified:
  - pkg/blockstore/migrate/journal.go
  - pkg/blockstore/migrate/journal_test.go
  - pkg/blockstore/migrate/walk.go
  - pkg/blockstore/migrate/walk_test.go
  - cmd/dfsctl/commands/blockstore/blockstore.go
  - cmd/dfsctl/commands/blockstore/migrate.go
  - cmd/dfsctl/commands/blockstore/migrate_loop.go
  - cmd/dfsctl/commands/blockstore/migrate_offline.go
  - cmd/dfsctl/commands/blockstore/migrate_runtime.go
  - cmd/dfsctl/commands/blockstore/migrate_test.go
  - cmd/dfsctl/commands/blockstore/migrate_loop_test.go
  - cmd/dfsctl/commands/root.go
autonomous: true
requirements: [MIG-01, MIG-02]
tags: [migration, dfsctl, journal, fastcdc, dedup, objectid_backfill]
must_haves:
  truths:
    - "`dfsctl blockstore migrate --share <name>` reads each legacy {payloadID}/block-{idx} key, runs FastCDC over the concatenated bytes, uploads CAS chunks via the engine's existing Put path, and updates FileAttr.Blocks with []BlockRef in a single metadata txn per file (D-A1)"
    - "FileAttr.ObjectID is populated in the same per-file metadata txn (Phase 13 BLAKE3 Merkle root over sorted block hashes — D-A14)"
    - "Per-file commits append a JSON line to {share-data-dir}/.migration-state.jsonl; every 1000 commits the tool fsyncs and atomic-renames a compacted snapshot {share-data-dir}/.migration-state.snapshot.json (D-A2, D-A3)"
    - "Resume reads snapshot first, replays journal tail, restores done-set, and skips already-migrated files (D-A4 — trust journal head; no per-file re-verification)"
    - "Tool refuses to start if the daemon owning the share is active — probes daemon status via `dfs status` socket lockfile (D-A5)"
    - "`--dry-run` walks the file list, runs FastCDC in memory, computes BlockRef hashes, and reports estimated upload bytes WITHOUT touching the metadata store or remote store (D-A2)"
    - "Per-chunk dedup probe: tool calls MetadataCoordinator.GetByHash before each PUT; on hit, IncrementRefCount only — no S3 PUT (D-A1 idempotency safety net)"
    - "The journal type lives in the importable package `pkg/blockstore/migrate/` so downstream callers (REST handler in Plan 14-06) can read it without importing cmd/. (BLOCKER 3 fix.)"
    - "The offline migration tool composes its own metadata + local + remote stores from `pkg/config` factories and does NOT depend on `pkg/controlplane/runtime.Runtime` — Runtime is daemon-internal and the migration tool runs offline. (BLOCKER 2 fix.)"
    - "A concrete `walkShareFiles(ctx, mds, shareName, fn)` helper lives at `pkg/blockstore/migrate/walk.go` — built from `MetadataStore.GetRootHandle` + `ListChildren` recursion. Empty share = zero callbacks. (BLOCKER 2 fix.)"
  artifacts:
    - path: pkg/blockstore/migrate/journal.go
      provides: "appendOnlyJournal type + Append / Snapshot / Replay methods. Importable package — REST handler (Plan 14-06) and CLI loop both consume from here."
      contains: "appendOnlyJournal"
    - path: pkg/blockstore/migrate/walk.go
      provides: "walkShareFiles helper — recurses share directory tree via metadata.MetadataStore primitives (GetRootHandle + ListChildren)"
      contains: "walkShareFiles"
    - path: cmd/dfsctl/commands/blockstore/blockstore.go
      provides: "New top-level `dfsctl blockstore` command group registered under root.go"
      contains: "Use:   \"blockstore\""
    - path: cmd/dfsctl/commands/blockstore/migrate.go
      provides: "`migrate` cobra command — flag wiring, runE entrypoint, calls migrate_loop"
      contains: "migrate"
    - path: cmd/dfsctl/commands/blockstore/migrate_runtime.go
      provides: "openOfflineRuntime — composes metadata + local + remote stores directly from pkg/config factories without touching the daemon Runtime"
      contains: "openOfflineRuntime"
    - path: cmd/dfsctl/commands/blockstore/migrate_loop.go
      provides: "per-file re-chunk loop — reads legacy, FastCDC, dedup probe, PUT, txn commit"
      contains: "migrateOneFile"
    - path: cmd/dfsctl/commands/blockstore/migrate_offline.go
      provides: "daemon-active probe — refuses to run on hot share"
      contains: "ensureDaemonOffline"
    - path: cmd/dfsctl/commands/root.go
      provides: "Registers blockstore.Cmd with root command"
      contains: "blockstorecmd"
  key_links:
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: pkg/blockstore/chunker (FastCDC)
      via: "chunker.Chunk(reader) yields content-defined boundaries"
      pattern: "chunker\\."
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: pkg/blockstore/engine (Put + GetByHash)
      via: "engine.MetadataCoordinator (Phase 12 D-37)"
      pattern: "GetByHash|IncrementRefCount"
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: pkg/blockstore/engine.ComputeObjectID (Phase 13)
      via: "BLAKE3 Merkle root over sorted BlockRef hashes"
      pattern: "ComputeObjectID"
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: pkg/blockstore/migrate.walkShareFiles
      via: "share-tree walk helper used by both migration loop and verifyIntegrity (Plan 14-05)"
      pattern: "walkShareFiles"
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: pkg/blockstore/migrate.appendOnlyJournal
      via: "OpenJournal + Append + Snapshot + IsFileDone + Replay"
      pattern: "OpenJournal"
    - from: cmd/dfsctl/commands/blockstore/migrate.go
      to: cmd/dfsctl/commands/root.go
      via: "AddCommand"
      pattern: "blockstorecmd.Cmd"
---

<objective>
Ship the core of `dfsctl blockstore migrate --share <name>`: re-chunk legacy blocks via FastCDC, upload as CAS chunks, populate `FileAttr.Blocks` and `FileAttr.ObjectID` per file in a single metadata txn, and persist progress in an append-only journal that supports crash-resume. Offline-only — refuses to run on a hot share. (MIG-01, MIG-02 partial; D-A1..D-A5, D-A14.)

Purpose: This is the central re-chunk loop. Plans 04 (bandwidth/parallel) and 05 (integrity check + cutover) wrap around the loop introduced here. Without this plan, the tool does not exist.

**Architecture note (BLOCKER fixes from review iteration 1):**

- The journal lives at `pkg/blockstore/migrate/journal.go`, NOT `cmd/dfsctl/commands/blockstore/migrate_journal.go`. Go forbids `internal/` and `pkg/` from importing `cmd/`, and the REST handler in Plan 14-06 (under `internal/controlplane/api/handlers/`) needs to import the journal type. So the journal MUST live in an importable package from day one.
- The offline migration tool does NOT use `pkg/controlplane/runtime.Runtime`. Runtime is the daemon's composition root and was never designed for offline-utility use. Instead, `openOfflineRuntime` composes the metadata store + remote store + local store directly from `pkg/config`-derived factories. This keeps the offline tool genuinely offline (no daemon dependencies, no shared lifecycle).
- The share-tree walk is a concrete helper `walkShareFiles(ctx, mds, shareName, fn)` at `pkg/blockstore/migrate/walk.go`, built from existing `MetadataStore` primitives (`GetRootHandle` + `ListChildren`). It is consumed by both the migration loop and Plan 14-05's `verifyIntegrity`.

Output: Working `dfsctl blockstore migrate --share NAME` command — single-threaded, no bandwidth limit yet (Plan 04 adds those), no auto-cutover yet (Plan 05). What it does: walks every file in the share, re-chunks via FastCDC, uploads CAS chunks (dedup-aware), updates metadata, journals progress, supports `--dry-run` and `--resume` (resume is automatic on re-invocation if a journal exists).
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-01-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-02-SUMMARY.md
@.planning/phases/12-cdc-read-path-metadata-engine-api-a3/12-CONTEXT.md
@.planning/phases/13-merkle-root-file-level-dedup-a4/13-CONTEXT.md
@.planning/codebase/CONVENTIONS.md
@.planning/codebase/STRUCTURE.md

<interfaces>
<!-- Engine + chunker contracts the migration tool consumes. -->

From pkg/blockstore/chunker/chunker.go (Phase 10):
```go
// Chunker yields content-defined boundaries from a byte stream.
// Params: min=1MB / avg=4MB / max=16MB / normalization level 2 (locked).
type Chunker interface {
    Next() (chunk []byte, eof bool, err error)
}
func NewChunker(r io.Reader) Chunker
```

From pkg/blockstore/engine/coordinator.go (Phase 12 D-37):
```go
type MetadataCoordinator interface {
    GetByHash(ctx context.Context, hash blockstore.ContentHash) (*FileBlock, bool, error)
    IncrementRefCount(ctx context.Context, hash blockstore.ContentHash) error
    Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error // Through engine; uploads to remote and persists FileBlock
}
```

From pkg/blockstore/engine/objectid.go (Phase 13):
```go
// ComputeObjectID returns the BLAKE3 Merkle root over sorted BlockRef
// hashes. Stable across rename, reproducible across restarts.
func ComputeObjectID(blocks []blockstore.BlockRef) blockstore.ContentHash
```

From pkg/metadata/store.go (verified — these are the EXISTING primitives the walk helper uses; no new methods needed):
```go
// GetRootHandle is the share-tree entry point.
GetRootHandle(ctx context.Context, shareName string) (FileHandle, error)

// ListChildren returns directory entries with pagination support.
ListChildren(ctx context.Context, dirHandle FileHandle, cursor string, limit int) ([]DirEntry, string, error)

// GetFile retrieves file metadata by handle.
GetFile(ctx context.Context, handle FileHandle) (*File, error)

// PutFile is the per-file metadata txn point — updates Blocks + ObjectID
// (and optionally Size/Mtime) atomically.
PutFile(ctx context.Context, file *File) error

// EnumerateFileBlocks (Phase 12 D-08) yields every FileBlock's ContentHash;
// used elsewhere — migration uses ListFileBlocks(payloadID) to find a single
// file's legacy keys in order.
ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error)
```

NOTE: `MetadataStore.WalkFiles` does NOT exist. The migration package builds its own walk helper from `GetRootHandle` + `ListChildren` (recursion + pagination). That helper is `walkShareFiles` in `pkg/blockstore/migrate/walk.go`.

From pkg/blockstore/types.go:
```go
type BlockRef struct {
    Hash   ContentHash `json:"hash"`
    Offset uint64      `json:"offset"`
    Size   uint32      `json:"size"`
}
func FormatStoreKey(payloadID string, blockIdx uint64) string  // legacy key
func FormatCASKey(h ContentHash) string                        // new CAS key
```

From cmd/dfsctl/commands/grace/status.go (existing pattern for output formatting):
```go
format, err := cmdutil.GetOutputFormatParsed()
switch format {
case output.FormatJSON: return output.PrintJSON(os.Stdout, resp)
case output.FormatYAML: return output.PrintYAML(os.Stdout, resp)
default:               return output.PrintTable(os.Stdout, renderer)
}
```

Cobra command-tree pattern (existing — see cmd/dfsctl/commands/store/store.go and store/block/block.go):
```go
package blockstore
var Cmd = &cobra.Command{Use: "blockstore", Short: "..."}
func init() { Cmd.AddCommand(migrateCmd) /* ...status added in Plan 06... */ }
```

Config-derived factories (used by openOfflineRuntime to avoid Runtime dependency):

- `pkg/config/config.go` — load `~/.config/dfs/config.yaml`.
- `pkg/blockstore/remote/s3.New(client, config)` and `pkg/blockstore/remote/memory.New()` — direct constructors used to wire a remote store from a share's persisted block-store config.
- `pkg/metadata/store/{memory,badger,postgres}.New*` — direct constructors used to open a metadata store from the share's persisted metadata-store config.

The offline tool reads the daemon's persisted ShareConfig from the controlplane store (read-only), then uses these constructors directly. It never calls Runtime.AddShare.
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: blockstore command group skeleton + offline probe + flag wiring</name>
  <files>
    cmd/dfsctl/commands/blockstore/blockstore.go,
    cmd/dfsctl/commands/blockstore/migrate.go,
    cmd/dfsctl/commands/blockstore/migrate_offline.go,
    cmd/dfsctl/commands/blockstore/migrate_test.go,
    cmd/dfsctl/commands/root.go
  </files>
  <read_first>
    - cmd/dfsctl/commands/store/store.go (Cmd group pattern — copy verbatim, just rename)
    - cmd/dfsctl/commands/store/block/gc.go (existing per-share command with `<share>` positional arg, `--dry-run` flag, `-o json` output — use as the migrate command's structural template)
    - cmd/dfsctl/commands/grace/status.go (output-format dispatch pattern)
    - cmd/dfsctl/commands/root.go (top-level AddCommand registration)
    - cmd/dfs/commands/status.go (or `cmd/dfs/commands/start.go` — find the daemon socket lockfile / status-probe mechanism; grep for "lockfile" or "PIDFile" or "socket" in cmd/dfs/)
  </read_first>
  <behavior>
    - Test 1: `dfsctl blockstore --help` lists `migrate` (and later `status` from Plan 06).
    - Test 2: `dfsctl blockstore migrate --share foo --help` lists flags: `--share`, `--dry-run`, `--parallel` (default 4 — wired in Plan 04), `--bandwidth-limit` (Plan 04), `--state-dir` (default empty = derive from share local store path).
    - Test 3 — ensureDaemonOffline (unit): when a stub probe returns "daemon active" → returns ErrDaemonActive; when it returns "daemon offline" → returns nil.
    - Test 4 — runMigrate exits non-zero with a clear message ("daemon for share %q is active — stop it before migration") when the offline probe fails.
  </behavior>
  <action>
    1. Create `cmd/dfsctl/commands/blockstore/blockstore.go`:
       ```go
       // Package blockstore implements offline block-store migration commands
       // for dfsctl. The migration tool re-chunks legacy {payloadID}/block-{idx}
       // keys into the v0.15 CAS layout per Phase 14 (D-A1..D-A20).
       package blockstore

       import "github.com/spf13/cobra"

       var Cmd = &cobra.Command{
           Use:   "blockstore",
           Short: "Block-store migration and inspection",
           Long:  "...", // include reference to BLOCKSTORE_MIGRATION.md (added by Plan 07)
       }

       func init() {
           Cmd.AddCommand(migrateCmd)
           // statusCmd is added by Plan 06.
       }
       ```

    2. Create `cmd/dfsctl/commands/blockstore/migrate.go` with the `migrateCmd` Cobra command. Mirror `store/block/gc.go` shape:
       ```go
       var migrateCmd = &cobra.Command{
           Use:   "migrate",
           Short: "Migrate a share's blocks from legacy {payloadID}/block-{idx} keys to v0.15 CAS layout",
           Long:  `...`, // include rich examples
           Args:  cobra.NoArgs, // share is via --share flag
           RunE:  runMigrate,
       }

       func init() {
           migrateCmd.Flags().String("share", "", "Share name to migrate (required)")
           migrateCmd.Flags().Bool("dry-run", false, "Walk file list and report estimated upload bytes WITHOUT writing any data")
           migrateCmd.Flags().Int("parallel", 4, "Number of parallel migration workers (D-A10 default 4); honored by Plan 14-04")
           migrateCmd.Flags().String("bandwidth-limit", "", "Aggregate upload bandwidth ceiling (e.g., 50MB, 100MiB); honored by Plan 14-04")
           migrateCmd.Flags().String("state-dir", "", "Override journal/snapshot directory; defaults to {share-data-dir}/.migration-state")
           _ = migrateCmd.MarkFlagRequired("share")
       }

       func runMigrate(cmd *cobra.Command, args []string) error {
           share, _ := cmd.Flags().GetString("share")
           dryRun, _ := cmd.Flags().GetBool("dry-run")

           if err := ensureDaemonOffline(cmd.Context(), share); err != nil {
               return fmt.Errorf("refusing to migrate share %q: %w", share, err)
           }

           // Plan 14-04 fills in parallel + bandwidth wiring; today single-thread.
           // Plan 14-05 fills in auto-cutover after integrity check.
           opts := migrateOptions{
               share:  share,
               dryRun: dryRun,
           }
           return runMigrateLoop(cmd.Context(), opts)
       }
       ```

    3. Create `cmd/dfsctl/commands/blockstore/migrate_offline.go`:
       ```go
       var ErrDaemonActive = errors.New("daemon is active for share — stop it before migration (D-A5)")

       // ensureDaemonOffline returns nil if the dfs daemon owning shareName is
       // not running, or ErrDaemonActive if it is. Detection mechanism: probe
       // the daemon's socket lockfile / `dfs status` endpoint.
       //
       // The daemon-status probe re-uses the mechanism in cmd/dfs/commands/status.go
       // (referenced via internal/cli/health or wherever the existing probe lives —
       // grep for it during implementation; the goal is one-line reuse, not a new
       // probe).
       func ensureDaemonOffline(ctx context.Context, shareName string) error { ... }
       ```
       Implementation detail: locate the existing daemon-status probe (`cmd/dfs/commands/status.go` or `internal/cli/health/`). If a packaged probe exists (e.g., `health.IsDaemonRunning(ctx)`), call it. If not, check for the lockfile / pidfile: the existing `cmd/dfs start` writes a pidfile somewhere (grep `pidfile|PIDFile|lockfile`). Use whatever mechanism is already there. **Do not invent a new health-check protocol** — read what's there.

       If the probe cannot reliably determine daemon state on this OS, fall back to checking whether anyone is bound to the configured listen ports — that's the original tell. The exact mechanism matters less than the *existence* of a fail-closed check.

    4. Register in `cmd/dfsctl/commands/root.go`:
       ```go
       blockstorecmd "github.com/marmos91/dittofs/cmd/dfsctl/commands/blockstore"
       // ...
       rootCmd.AddCommand(blockstorecmd.Cmd)
       ```

    5. Add `cmd/dfsctl/commands/blockstore/migrate_test.go` with the four behaviors above. Use `cobra.Command.SetArgs` + `.Execute()` for the `--help` assertions; for ensureDaemonOffline tests, refactor it to take an injectable probe interface so the test can stub it. Use the project's existing `internal/cli/health` package shape if it has one.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestMigrate' -count=1 &amp;&amp; ./dfsctl blockstore --help 2&gt;&amp;1 | grep -q migrate || (go run ./cmd/dfsctl blockstore --help 2&gt;&amp;1 | grep -q migrate)</automated>
  </verify>
  <acceptance_criteria>
    - `cmd/dfsctl/commands/blockstore/blockstore.go` exists with `var Cmd = &cobra.Command{Use: "blockstore"...}`.
    - `cmd/dfsctl/commands/blockstore/migrate.go` exists with `migrateCmd` and a `runMigrate` function.
    - `grep -c '\-\-share' cmd/dfsctl/commands/blockstore/migrate.go` >= 1 and the flag is marked required.
    - `grep -c 'rootCmd.AddCommand(blockstorecmd.Cmd)' cmd/dfsctl/commands/root.go` == 1.
    - `grep -c 'ensureDaemonOffline' cmd/dfsctl/commands/blockstore/migrate_offline.go` >= 1.
    - `grep -c 'ErrDaemonActive' cmd/dfsctl/commands/blockstore/migrate_offline.go` >= 1.
    - `go run ./cmd/dfsctl blockstore --help` (or built binary) prints `migrate` in the Available Commands list.
    - `go run ./cmd/dfsctl blockstore migrate --help` lists all five flags above.
    - The four unit tests pass.
  </acceptance_criteria>
  <done>
    Command tree wired; daemon-active probe present; all flags declared (full behavior of `--parallel` and `--bandwidth-limit` lands in Plan 04, but the flags exist now so help text is stable for docs).
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Append-only journal + walk helper — both in pkg/blockstore/migrate/ (importable package)</name>
  <files>
    pkg/blockstore/migrate/journal.go,
    pkg/blockstore/migrate/journal_test.go,
    pkg/blockstore/migrate/walk.go,
    pkg/blockstore/migrate/walk_test.go
  </files>
  <read_first>
    - .planning/phases/14-migration-tool-a5/14-CONTEXT.md (D-A1..D-A4 — atomic unit = one file, journal location, snapshot interval, trust-journal-head on resume)
    - pkg/blockstore/local/fs/chunkstore.go (existing pattern for atomic-rename writes — `write to .tmp, fsync, rename`; mirror it)
    - pkg/blockstore/engine/audit_state.go (existing pattern for persisted JSON state under share data dir; if it uses a snapshot+journal pattern, copy that exactly)
    - pkg/metadata/store.go (verified: GetRootHandle + ListChildren + GetFile are the primitives the walk helper composes)
  </read_first>
  <behavior>
    Journal:
    - Test J1 — Append + Replay: append entries A, B, C; reopen journal; Replay returns {A, B, C} in order.
    - Test J2 — Snapshot at threshold: append 1000 entries; observe `.migration-state.snapshot.json` written + journal truncated to a fresh empty `.migration-state.jsonl`; Replay still returns {1..1000}.
    - Test J3 — Resume after crash mid-snapshot: simulate the snapshot file being half-written (corrupt JSON); Replay falls back to the journal (last good snapshot + everything since); no data loss for entries that were committed before the snapshot.
    - Test J4 — Atomic rename invariant: snapshot writes go to `.snapshot.json.tmp` first, then `os.Rename`. If a fault is injected between fsync and rename, replay still works.
    - Test J5 — IsFileDone(handle) — returns true after a CommitFile entry has been appended and replayed; useful for resume.
    - Test J6 — Compaction floor: after Snapshot, the .jsonl file size is 0 (or contains only entries appended *after* the snapshot moment).

    Walk:
    - Test W1 — Empty share: walkShareFiles on a freshly created share invokes the callback zero times.
    - Test W2 — Single file: one file at the root → callback invoked exactly once with that file's handle + attr.
    - Test W3 — Nested directories: tree of depth 3 with 5 files total → callback invoked 5 times; directory handles NOT delivered to the callback.
    - Test W4 — Pagination: a directory with 200 children (default ListChildren limit may be smaller) → callback invoked 200 times; cursor pagination handled internally.
    - Test W5 — Context cancel: cancel ctx mid-walk → walk returns ctx.Err(), callback invocation stops.
    - Test W6 — Callback error: callback returns a sentinel error → walk returns the wrapped error and stops.
  </behavior>
  <action>
    Create `pkg/blockstore/migrate/journal.go`:

    ```go
    // Package migrate provides the offline block-store migration journal
    // and share-tree walk helper. Lives in pkg/ (not cmd/) because the
    // controlplane REST handler (Plan 14-06) needs to import the journal
    // type — Go forbids pkg/ and internal/ from importing cmd/. (Phase 14
    // BLOCKER 3 fix.)
    package migrate

    import (
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "io"
        "os"
        "path/filepath"
        "sync"
        "time"

        "github.com/marmos91/dittofs/pkg/blockstore"
    )

    const (
        JournalFile  = ".migration-state.jsonl"
        SnapshotFile = ".migration-state.snapshot.json"
        // D-A3 default snapshot interval. Tunable via env DITTOFS_MIGRATE_SNAPSHOT_INTERVAL
        // for test injection only — not a documented operator knob.
        DefaultSnapshotInterval = 1000
    )

    // JournalEntry is one line in the append-only log. Each represents a
    // file-level commit (D-A1 — atomic unit = one file). Schema is forward-
    // compat by virtue of `omitempty` and a `version` field.
    type JournalEntry struct {
        Version    int                    `json:"v"`
        Kind       string                 `json:"kind"` // "file_done" | "file_skipped"
        Timestamp  time.Time              `json:"ts"`
        FileHandle string                 `json:"handle"`
        PayloadID  string                 `json:"payload_id"`
        Blocks     []blockstore.BlockRef  `json:"blocks,omitempty"`
        ObjectID   blockstore.ContentHash `json:"object_id,omitempty"`
        BytesUploaded uint64              `json:"bytes_uploaded,omitempty"`
        BytesDeduped  uint64              `json:"bytes_deduped,omitempty"`
    }

    // Journal is the resumability state file. One per share.
    // D-A2 — lives at {share-data-dir}/.migration-state.jsonl with a
    // companion {share-data-dir}/.migration-state.snapshot.json.
    type Journal struct {
        mu              sync.Mutex
        dir             string
        jf              *os.File
        snapshotEvery   int
        appended        int
        done            map[string]JournalEntry
    }

    // OpenJournal opens (or creates) the journal at dir. On open, it loads
    // any existing snapshot, then replays the journal tail; the in-memory
    // done-set is the union.
    func OpenJournal(dir string) (*Journal, error) { /* ... */ }

    // OpenJournalReadOnly opens the journal in read-only mode (no Append, no
    // Snapshot rotation). Used by the REST handler in Plan 14-06 — it needs
    // to peek at the journal while a migration may or may not be running.
    // Concurrent reads against an active writer are well-defined by POSIX
    // semantics as long as we never truncate or rotate from this handle.
    func OpenJournalReadOnly(dir string) (*Journal, error) { /* ... */ }

    func (j *Journal) Append(e JournalEntry) error            { /* ... */ }
    func (j *Journal) IsFileDone(handle string) bool           { /* ... */ }
    func (j *Journal) Snapshot() error                         { /* ... */ }
    func (j *Journal) Replay() ([]JournalEntry, error)         { /* ... */ }
    func (j *Journal) Close() error                            { /* ... */ }

    // Aggregate convenience for the REST status handler (Plan 14-06).
    // Returns (entries, journal_present, snapshot_present, last_commit_at).
    func (j *Journal) Aggregate() (entries []JournalEntry, journalPresent, snapshotPresent bool, lastCommitAt time.Time) { /* ... */ }
    ```

    Implementation rules:

    - **Open**: ensure `dir` exists (mkdir -p). If `SnapshotFile` exists, parse it as `[]JournalEntry` and rebuild `done` map. Then open `JournalFile` (`O_RDWR|O_CREATE|O_APPEND`); read its contents line by line, json.Unmarshal each, and overlay onto `done` map. Lines that fail to parse: log a warning and stop replay there (truncated tail = last incomplete write; treat the surviving prefix as authoritative — D-A4).
    - **Append**: serialize entry as JSON + newline; `Write` to journal file; `Sync()` to fsync. Update `done` map under mu.
    - **Snapshot**: under mu, marshal sorted-by-handle slice of `done` to a tmp file (`SnapshotFile + ".tmp"`); fsync; `os.Rename` to `SnapshotFile`; truncate journal file to length 0; reset counter.
    - **Snapshot trigger**: every `snapshotEvery` Append calls (default `DefaultSnapshotInterval`), Snapshot is auto-fired.
    - **Test injection**: `OpenJournalWithInterval(dir string, interval int)` for tests.
    - **JSON forward-compat**: include `Version: 1` in every entry.
    - **OpenJournalReadOnly**: opens both files in O_RDONLY; never writes; Snapshot/Append on a read-only journal return an error sentinel.

    Add `pkg/blockstore/migrate/journal_test.go` covering J1-J6.

    ---

    Create `pkg/blockstore/migrate/walk.go`:

    ```go
    package migrate

    import (
        "context"

        "github.com/marmos91/dittofs/pkg/metadata"
    )

    // WalkCallback is invoked for every regular FILE in the share tree.
    // Directories are NOT delivered (caller almost always wants files).
    type WalkCallback func(handle metadata.FileHandle, file *metadata.File) error

    // WalkShareFiles walks every regular file in the share, recursing
    // into directories via metadata.MetadataStore primitives
    // (GetRootHandle + ListChildren). Pagination is handled internally
    // (cursor loop). Context cancellation aborts the walk and returns
    // ctx.Err(). Callback errors abort the walk and are returned wrapped.
    //
    // Note: This is a deliberate self-contained helper. The metadata store
    // does not expose a single WalkFiles method (Plan 14 reviewer flagged
    // this) — the existing primitives compose into the walk we need
    // without modifying the MetadataStore interface.
    func WalkShareFiles(
        ctx context.Context,
        mds metadata.MetadataStore,
        shareName string,
        fn WalkCallback,
    ) error {
        root, err := mds.GetRootHandle(ctx, shareName)
        if err != nil { return err }
        return walkDir(ctx, mds, root, fn)
    }

    func walkDir(ctx context.Context, mds metadata.MetadataStore, dir metadata.FileHandle, fn WalkCallback) error {
        const pageSize = 256
        cursor := ""
        for {
            if err := ctx.Err(); err != nil { return err }
            entries, next, err := mds.ListChildren(ctx, dir, cursor, pageSize)
            if err != nil { return err }
            for _, e := range entries {
                child, err := mds.GetFile(ctx, e.Handle)
                if err != nil { return err }
                if child.IsDir() { // mirror existing convention; child.Attr.Mode etc.
                    if err := walkDir(ctx, mds, e.Handle, fn); err != nil { return err }
                    continue
                }
                if err := fn(e.Handle, child); err != nil {
                    return err
                }
            }
            if next == "" { break }
            cursor = next
        }
        return nil
    }
    ```

    Add `pkg/blockstore/migrate/walk_test.go` covering W1-W6 using the in-memory metadata store fixture (`pkg/metadata/store/memory`).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./pkg/blockstore/migrate/ -count=1 -v &amp;&amp; go vet ./pkg/blockstore/migrate/ &amp;&amp; go build ./... &amp;&amp; grep -q 'os.Rename' pkg/blockstore/migrate/journal.go &amp;&amp; grep -q '.Sync()' pkg/blockstore/migrate/journal.go &amp;&amp; grep -q 'WalkShareFiles' pkg/blockstore/migrate/walk.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'package migrate' pkg/blockstore/migrate/journal.go` == 1 (importable package, NOT cmd/).
    - `grep -c 'package migrate' pkg/blockstore/migrate/walk.go` == 1.
    - `grep -c 'WalkShareFiles' pkg/blockstore/migrate/walk.go` >= 1.
    - `grep -c 'OpenJournalReadOnly' pkg/blockstore/migrate/journal.go` >= 1 (REST handler in Plan 14-06 uses this).
    - `grep -c '\.Sync()' pkg/blockstore/migrate/journal.go` >= 1 (fsync after Append).
    - `grep -c 'os.Rename' pkg/blockstore/migrate/journal.go` >= 1 (atomic snapshot rename).
    - `grep -c '"v"' pkg/blockstore/migrate/journal.go` >= 1 (versioned entries).
    - All 6 journal + 6 walk unit tests pass.
    - `go vet ./pkg/blockstore/migrate/` clean.
    - `go build ./...` clean (cross-package compile check confirms the new package is importable).
    - No file at `cmd/dfsctl/commands/blockstore/migrate_journal.go` is created (was the wrong location pre-revision).
  </acceptance_criteria>
  <done>
    Journal + walk helper land in `pkg/blockstore/migrate/` (importable). Journal supports append + fsync + atomic snapshot + read-only open. Walk recurses share tree via existing MetadataStore primitives. All tests green.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 3: openOfflineRuntime + per-file migration loop — read legacy → FastCDC → dedup probe → PutBlock → metadata txn → ObjectID backfill → journal commit</name>
  <files>
    cmd/dfsctl/commands/blockstore/migrate_runtime.go,
    cmd/dfsctl/commands/blockstore/migrate_loop.go,
    cmd/dfsctl/commands/blockstore/migrate_loop_test.go
  </files>
  <read_first>
    - pkg/blockstore/chunker/chunker.go (FastCDC API — `NewChunker(io.Reader)` + `Next()` boundary)
    - pkg/blockstore/engine/coordinator.go (MetadataCoordinator interface — GetByHash + IncrementRefCount + Put)
    - pkg/blockstore/engine/objectid.go (ComputeObjectID — Phase 13 BLAKE3 Merkle root)
    - pkg/blockstore/engine/dedup.go (existing speculative-dedup short-circuit pattern)
    - pkg/metadata/store.go (lines around `PutFile`, `ListFileBlocks`, `GetFile` — the txn surface; verified WalkFiles does NOT exist)
    - pkg/blockstore/types.go (BlockRef + ContentHash + FormatStoreKey + FormatCASKey)
    - pkg/blockstore/engine/syncer.go (existing per-payload drain logic)
    - pkg/blockstore/migrate/journal.go (Task 2 output — OpenJournal + Append)
    - pkg/blockstore/migrate/walk.go (Task 2 output — WalkShareFiles)
    - pkg/config/config.go (load daemon config; openOfflineRuntime needs this)
    - pkg/blockstore/remote/s3/store.go and pkg/blockstore/remote/memory/store.go (constructors `New` / `NewFromConfig` — used directly by openOfflineRuntime)
    - pkg/metadata/store/memory and pkg/metadata/store/badger (constructors used directly)
    - pkg/controlplane/runtime/blockstore_init.go (read-only — to understand HOW the daemon validates block-store configs; mirror that validation in openOfflineRuntime)
  </read_first>
  <behavior>
    - Test 1 — Empty share: no files; runMigrateLoop returns nil; journal is empty; no S3 calls made.
    - Test 2 — Single small file (4 MB legacy block) is re-chunked into one CAS chunk; PutFile updates Blocks=[{Hash,Offset:0,Size:4MB}]; ObjectID = ComputeObjectID(Blocks); journal has one CommitFile entry.
    - Test 3 — Two files with identical content: second file's chunks all dedup-hit on GetByHash; second file's PutFile commits the same Blocks list with all-zero new uploads; bytes_deduped reflects the win.
    - Test 4 — Resume: pre-populate journal with one file's CommitFile entry; run loop on a share with two files; only the second file is migrated (the first is skipped via journal.IsFileDone).
    - Test 5 — Dry-run: walks files, runs FastCDC, computes hashes; calls metadata store ZERO times for writes; remoteStore.Put called ZERO times; reports estimated upload bytes via stdout.
    - Test 6 — Mid-file crash: simulate Put failing on the 3rd of 5 chunks; the per-file metadata txn must NOT commit; journal does NOT get a CommitFile entry; on resume, the file is retried from scratch and any already-uploaded chunks dedup-hit.
    - Test 7 — Legacy-source read failure: simulate the legacy ReadBlock returning ErrBlockNotFound for a sparse block; chunker treats that region as zeros; the resulting BlockRef list is sparse-aware (or the chunk is omitted, depending on existing chunker semantics for zero regions — match what Phase 10/11 already do).
  </behavior>
  <action>
    1. Create `cmd/dfsctl/commands/blockstore/migrate_runtime.go` — the offline runtime composition root. Crucially, this does NOT touch `pkg/controlplane/runtime.Runtime`. (BLOCKER 2 fix.)

       ```go
       package blockstore

       import (
           "context"
           "fmt"

           "github.com/marmos91/dittofs/pkg/blockstore"
           "github.com/marmos91/dittofs/pkg/blockstore/engine"
           "github.com/marmos91/dittofs/pkg/blockstore/local"
           "github.com/marmos91/dittofs/pkg/blockstore/remote"
           remotemem "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
           remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
           "github.com/marmos91/dittofs/pkg/config"
           "github.com/marmos91/dittofs/pkg/metadata"
       )

       // offlineRuntime is the migration tool's self-contained composition
       // root. It deliberately avoids pkg/controlplane/runtime.Runtime —
       // Runtime is the daemon's composition root and was never designed
       // for offline-utility use. This struct opens just the stores the
       // migration loop needs (metadata, remote, local-data dir for the
       // journal) and tears them down on Close.
       //
       // Phase 14 BLOCKER 2 fix: the original plan called Runtime.WalkFiles,
       // Runtime.JournalDir, Runtime.PutBlock, etc. — none exist. Self-
       // contained composition keeps the offline tool genuinely offline.
       type offlineRuntime struct {
           share         string
           cfg           *config.Config
           shareCfg      *config.ShareEntry  // verified during read; pkg/config representation
           mds           metadata.MetadataStore
           rs            remote.RemoteStore
           // The migration loop talks to the engine's MetadataCoordinator
           // for GetByHash / IncrementRefCount / Put. The coordinator is
           // wired around the same metadata store + remote store the
           // daemon uses, but in a single-process, no-syncer config.
           coord         engine.MetadataCoordinator
           dataDir       string  // {share-data-dir} — journal lives at {dataDir}/.migration-state*
       }

       // openOfflineRuntime reads ~/.config/dfs/config.yaml (or the path
       // overridden by --config), looks up the share entry by name,
       // verifies block_layout=legacy (refuse on cas-only), and opens:
       //   1. The metadata store (memory/badger/postgres) directly via
       //      pkg/metadata/store/<kind>.New — same signature the daemon uses.
       //   2. The remote store (memory/s3) directly via the constructors
       //      in pkg/blockstore/remote/<kind>/store.go.
       //   3. The on-disk data dir for the share (used for the journal).
       //   4. A minimal MetadataCoordinator (no syncer; uploads are
       //      synchronous in offline mode — see existing engine setup
       //      in pkg/blockstore/engine for the no-syncer constructor or
       //      add a dedicated NewOfflineCoordinator helper if absent).
       //
       // Returns ErrShareNotFound if the share is not in config.
       // Returns ErrShareAlreadyCAS if BlockLayout == cas-only (Phase 14
       // is a one-way migration; running it twice is a noop refusal).
       func openOfflineRuntime(ctx context.Context, shareName string) (*offlineRuntime, error) { /* ... */ }

       func (r *offlineRuntime) MetadataStore() metadata.MetadataStore { return r.mds }
       func (r *offlineRuntime) RemoteStore() remote.RemoteStore       { return r.rs }
       func (r *offlineRuntime) Coordinator() engine.MetadataCoordinator { return r.coord }
       func (r *offlineRuntime) DataDir() string                      { return r.dataDir }

       // RemoteHeadObject is the wrapper Plan 14-05 uses for the integrity
       // check. Defined here so the loop and integrity check share one
       // remote-store façade. Implementation: r.rs.HeadObject(ctx, key) once
       // BLOCKER 1 lands HeadObject on the RemoteStore interface (Plan 14-05).
       func (r *offlineRuntime) RemoteHeadObject(ctx context.Context, key string) (remote.HeadResult, error) {
           return r.rs.HeadObject(ctx, key)
       }

       func (r *offlineRuntime) Close() error { /* close mds, rs */ }
       ```

       **Concrete factory wiring** (the executor follows the existing daemon code):
       - The daemon reads `config.Config` and constructs metadata + remote + local stores via pkg/config plus the per-store-type constructors. Mirror that wiring; do NOT add new factory functions in pkg/config — reuse what exists.
       - For metadata: switch on `cfg.MetadataStore.Type` and call the right constructor (`memory.New()`, `badger.NewBadgerStore(path)`, `postgres.NewPostgresStore(...)` — exact names match what's in pkg/metadata/store/*).
       - For remote: switch on the share's block-store config kind; call `s3.NewFromConfig` or `memory.New`.
       - For coordinator: instantiate the engine in offline mode (no background syncer). If there is no existing offline-mode constructor in `pkg/blockstore/engine`, the executor adds a minimal one (`NewOfflineCoordinator(mds, rs) MetadataCoordinator`) in this task's scope and adds it to `files_modified` of this plan when implementing.

    2. Create `cmd/dfsctl/commands/blockstore/migrate_loop.go`:
       ```go
       package blockstore

       import (
           "context"
           "errors"
           "fmt"
           "io"
           "os"
           "time"

           "github.com/marmos91/dittofs/internal/logger"
           "github.com/marmos91/dittofs/pkg/blockstore"
           "github.com/marmos91/dittofs/pkg/blockstore/chunker"
           "github.com/marmos91/dittofs/pkg/blockstore/engine"
           "github.com/marmos91/dittofs/pkg/blockstore/migrate"  // journal + walk live here now
           "github.com/marmos91/dittofs/pkg/metadata"
       )

       type migrateOptions struct {
           share         string
           dryRun        bool
           parallel      int
           bandwidthBPS  int64
           stateDir      string
       }

       type migrateResult struct {
           FilesTotal     int
           FilesDone      int
           FilesSkipped   int
           BytesUploaded  uint64
           BytesDeduped   uint64
           StartedAt      time.Time
           DurationMS     int64
           LegacyKeysDeleted int  // populated by Plan 14-05
       }

       func runMigrateLoop(ctx context.Context, opts migrateOptions) error {
           // 1. Open offline runtime — composes stores directly from config.
           //    Does NOT touch Runtime.
           svc, err := openOfflineRuntime(ctx, opts.share)
           if err != nil { return err }
           defer svc.Close()

           // 2. Open journal at the share's data dir.
           journalDir := opts.stateDir
           if journalDir == "" { journalDir = svc.DataDir() }
           journal, err := migrate.OpenJournal(journalDir)
           if err != nil { return err }
           defer journal.Close()

           // 3. Walk every file in the share. Uses the migrate package's
           //    walk helper (built from MetadataStore.GetRootHandle +
           //    ListChildren — no new metadata interface methods).
           result := migrateResult{StartedAt: time.Now()}
           err = migrate.WalkShareFiles(ctx, svc.MetadataStore(), opts.share,
               func(handle metadata.FileHandle, file *metadata.File) error {
                   result.FilesTotal++
                   if journal.IsFileDone(string(handle)) {
                       result.FilesSkipped++
                       return nil
                   }
                   r, err := migrateOneFile(ctx, svc, journal, opts, handle, file)
                   if err != nil { return err }
                   result.FilesDone++
                   result.BytesUploaded += r.BytesUploaded
                   result.BytesDeduped += r.BytesDeduped
                   return nil
               })
           if err != nil { return err }

           // 4. Final snapshot — clean state for next run.
           if !opts.dryRun {
               if err := journal.Snapshot(); err != nil {
                   logger.Warn("final journal snapshot failed", "err", err)
               }
           }

           result.DurationMS = time.Since(result.StartedAt).Milliseconds()
           return printMigrateResult(result, opts.dryRun)
       }

       func migrateOneFile(
           ctx context.Context,
           svc *offlineRuntime,
           journal *migrate.Journal,
           opts migrateOptions,
           handle metadata.FileHandle,
           file *metadata.File,
       ) (perFileResult, error) {
           // a. Build a legacyReader over all FileBlock rows for this payloadID
           //    via svc.MetadataStore().ListFileBlocks(ctx, file.Attr.PayloadID).
           //    Wrap them as an io.Reader that fetches each
           //    legacy {payloadID}/block-{N} from svc.RemoteStore().ReadBlock
           //    and concatenates. Sparse blocks (ErrBlockNotFound) yield zeros.
           //
           // b. Run chunker.NewChunker(legacyReader); accumulate BlockRef list.
           //
           // c. For each chunk:
           //    i.  hash = blake3(chunk)
           //    ii. existing, hit, err := svc.Coordinator().GetByHash(ctx, hash)
           //    iii. if hit: bytesDeduped += len(chunk); IncrementRefCount(hash); skip PUT.
           //    iv. else if !opts.dryRun: svc.Coordinator().Put(ctx, hash, chunk)
           //         (Put = remote PUT cas/.../h + persist FileBlock row).
           //    v.  Append BlockRef{Hash:hash, Offset:offset, Size:len(chunk)}.
           //
           // d. ObjectID = engine.ComputeObjectID(blocks).
           //
           // e. If !opts.dryRun:
           //      newAttr := file.Attr; newAttr.Blocks = blocks; newAttr.ObjectID = ObjectID
           //      newFile := *file; newFile.Attr = newAttr
           //      svc.MetadataStore().PutFile(ctx, &newFile)  // single per-file txn
           //
           // f. Append journal entry (kind=file_done, blocks, ObjectID, byte counts).
           //    On dryRun, journal Append is skipped — dry runs leave no trace.
       }
       ```

    3. `printMigrateResult` writes a final summary to stdout (table or JSON via `cmdutil.GetOutputFormatParsed()` — match `cmd/dfsctl/commands/grace/status.go`).

    4. Add `migrate_loop_test.go` covering all 7 behaviors. Use a memory metadata store + memory remote store as the runtime (avoid touching real S3). Inject the offline runtime via a test-only constructor `newTestOfflineRuntime(mds, rs, dataDir)` so the production `openOfflineRuntime` path stays untested at the unit level — it is exercised by Plan 07's e2e/runbook transcripts.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestMigrateLoop' -count=1 -timeout 60s &amp;&amp; go vet ./cmd/dfsctl/commands/blockstore/ &amp;&amp; grep -q 'ComputeObjectID' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; grep -q 'GetByHash' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; grep -q 'PutFile' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; grep -q 'migrate.OpenJournal\|migrate\.OpenJournal' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; grep -q 'migrate.WalkShareFiles\|migrate\.WalkShareFiles' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; ! grep -q 'controlplane/runtime' cmd/dfsctl/commands/blockstore/migrate_runtime.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'ComputeObjectID' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'GetByHash' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'PutFile' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'IncrementRefCount' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'chunker\.' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'migrate\.OpenJournal' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1 (journal lives in pkg/blockstore/migrate, not cmd/).
    - `grep -c 'migrate\.WalkShareFiles' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'controlplane/runtime' cmd/dfsctl/commands/blockstore/migrate_runtime.go` == 0 (BLOCKER 2 — offline tool MUST NOT depend on Runtime).
    - `grep -c 'openOfflineRuntime' cmd/dfsctl/commands/blockstore/migrate_runtime.go` >= 1.
    - All 7 unit tests pass.
    - Dry-run test asserts `len(remoteStorePutCalls) == 0` and `len(metadataPutFileCalls) == 0`.
    - Resume test asserts that pre-populated journal entries cause `FilesSkipped == 1` while `FilesDone == 1`.
    - `go build ./...` succeeds (cross-package compile).
    - `go vet ./cmd/dfsctl/commands/blockstore/` clean.
  </acceptance_criteria>
  <done>
    Per-file migration loop reads legacy → re-chunks via FastCDC → dedup-probes via GetByHash → uploads only new chunks → updates Blocks + ObjectID in single PutFile txn → journals success. Offline runtime is self-contained (no Runtime dependency). Dry-run, resume, dedup, mid-file crash recovery all green-tested.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| dfsctl (operator) → daemon socket / pidfile | Probe is read-only; if probe is bypassed, the offline invariant breaks but the worst case is concurrent writers landing on a half-migrated share — the per-file metadata txn is the consistency guarantee. |
| Migration tool → metadata store | Each per-file commit is one metadata txn (PutFile). Mid-file crash leaves Blocks unchanged, but possibly creates orphan CAS chunks that GC reclaims later. |
| Migration tool → remote store | PUT is verified by content hash on read (Phase 11 INV-06). A torn upload cannot poison the read path because the CAS key encodes the hash. |
| Journal file → next invocation | An attacker with local fs write access can falsify journal entries and skip files. Out of scope per D-A4 (operator runs the tool; trust boundary is at the operator account). |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-03-01 | Tampering | Mid-file crash leaves stale BlockRefs | mitigate | Per-file PutFile is a single txn; crash before commit leaves attr.Blocks unchanged (still legacy). Re-run is idempotent. |
| T-14-03-02 | Repudiation | Journal claims a file is migrated when metadata still has legacy Blocks | mitigate | Journal Append happens AFTER PutFile returns success; the order is (PutFile, fsync) → journal Append. A crash between them re-migrates that file (idempotent via GetByHash dedup). Test 6 covers this. |
| T-14-03-03 | Information disclosure | Dry-run accidentally writes to remote/metadata | mitigate | Tests assert zero metadata write calls and zero remote PUT calls during dry-run. The dryRun bool is a hard gate — both code paths inspect it. |
| T-14-03-04 | Denial of service | Hot daemon serves the same share during migration → torn writes | mitigate | ensureDaemonOffline (Task 1) refuses to start when the daemon is up. D-A5. |
| T-14-03-05 | Elevation of privilege | dfsctl invoking the migration loop with operator CLI privileges | accept | Migration is a maintenance action requiring root-level access to the share's local-store dir + remote credentials. The same trust model used for `dfsctl store block gc`. |
</threat_model>

<verification>
- All 4 + 6 + 6 + 7 = 23 unit tests pass (cli help/probe + journal + walk + loop).
- `go run ./cmd/dfsctl blockstore migrate --share NAME` end-to-end against a memory-only fixture produces a journal with one CommitFile entry per file, matching ObjectID via independent BLAKE3 Merkle-root computation.
- Dry-run mode writes nothing.
- Resume skips already-done files via journal.
- `go vet ./...` and `go build ./...` clean.
- `grep -r 'controlplane/runtime' cmd/dfsctl/commands/blockstore/` returns no results (BLOCKER 2 invariant).
- `ls pkg/blockstore/migrate/journal.go pkg/blockstore/migrate/walk.go` succeeds (BLOCKER 3 invariant — journal is in pkg/, not cmd/).
</verification>

<success_criteria>
- `dfsctl blockstore migrate --share NAME` runs to completion on a small share (memory-store fixture).
- Per-file commits land Blocks + ObjectID via PutFile.
- GetByHash dedup probe before every PUT.
- Journal supports resume after crash.
- Dry-run reports estimated upload bytes without writing.
- Daemon-active probe blocks the run.
- Journal + walk helper live in `pkg/blockstore/migrate/` (importable by Plan 14-06).
- Offline runtime composes stores directly from config — no Runtime dependency.
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-03-SUMMARY.md` describing the new package layout, the journal contract, the walk-helper contract, the offline-runtime composition, and the per-file migration semantics. Document the deferred items (parallel + bandwidth Plan 04, integrity check + cutover Plan 05, status/REST Plan 06, docs Plan 07).
</output>
