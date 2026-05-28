# Phase 17: Unified BlockStore — Pattern Map

**Mapped:** 2026-05-20
**Files to touch (from CONTEXT.md + ROADMAP.md):** 18 (5 new, 9 modified, 4 deleted)
**Analogs found:** 18 / 18

> **NOTE on prompt drift:** the spawn prompt claimed `pkg/blockstore/migrate/` holds 7 files (`migrate_offline.go`, `migrate_progress.go`, `migrate_status.go`, `migrate_workers.go`, `migrate_loop.go`, `migrate_runtime.go`, `migrate_legacy_reader.go`). It does NOT — the directory currently has only `journal.go`, `walk.go`, plus the platform `syncdir_*.go` pair. The Phase 14 A5 migration framework lives in `cmd/dfsctl/commands/blockstore/migrate.go` (not in this tree) and was deliberately kept thin. Phase 17 should follow the same lean shape: one `migrate_to_cas.go` library + one `dfs migrate-to-cas` cobra command, reusing the existing `migrate.Journal` + `migrate.WalkShareFiles`.

## File Classification

| File | New / Modified / Deleted | Role | Data Flow | Closest Analog | Match |
|------|--------------------------|------|-----------|----------------|-------|
| `pkg/blockstore/blockstore.go` | NEW or extended | interface-definition | request-response (CAS CRUD) | `pkg/blockstore/local/local.go:52` + `pkg/blockstore/remote/remote.go:63` | exact |
| `pkg/blockstore/errors.go` | MODIFIED (+2 sentinels) | error-sentinel | n/a | existing `ErrChunkNotFound`, `ErrCASContentMismatch`, `engine/errors.go::ErrLegacyReadOnCASOnly` | exact |
| `pkg/blockstore/types.go` | MODIFIED (delete `FormatStoreKey`/`ParseStoreKey`) | type-helper | n/a | self (lines 240–260) | n/a — pure deletion |
| `pkg/blockstore/local/local.go` | MODIFIED (narrow to ~12 methods, embed `BlockStoreAppend`) | interface-definition | mixed | self (lines 52–175) — Phase 16 already wired `Get(ctx, hash)` at line 85 | exact (the signature lock that Phase 17 closes) |
| `pkg/blockstore/local/fs/fs.go` | MODIFIED (sentinel-file check in `NewFSStore`) | constructor / boot-guard | startup-validation | self (lines 253–308 — existing `New`); pattern from Phase 14's `BlockLayout` gate in `engine/fetch.go:69` | role-match |
| `pkg/blockstore/local/fs/write.go` | **DELETED** | legacy writer | n/a | n/a | pure deletion |
| `pkg/blockstore/local/fs/chunkstore.go` | reused as-is | CAS reader/writer | request-response | self (lines 36–169) — the Phase 17 `BlockStore.Get` IS `*FSStore.Get` at line 147 | exact |
| `pkg/blockstore/remote/remote.go` | MODIFIED (rename + collapse) | interface-definition | request-response | self (lines 63–149) | n/a — in-place rename |
| `pkg/blockstore/remote/s3/store.go` | MODIFIED (method renames + Walk) | backend impl | streaming | self (existing `WriteBlock`/`WriteBlockWithHash` at 223/246; `ListByPrefixWithMeta` at 545 → `Walk`) | exact |
| `pkg/blockstore/engine/engine.go` | MODIFIED (drop `BlockLayout()` reads at l.771; type narrowing) | engine consumer | composition | self (lines 210–250) | exact |
| `pkg/blockstore/engine/fetch.go` | MODIFIED (delete legacy branch lines 58–82) | engine consumer | request-response | self (lines 42–82) | pure deletion |
| `pkg/blockstore/engine/errors.go` | **DELETED** (`ErrLegacyReadOnCASOnly` no longer reachable) | error-sentinel | n/a | self | pure deletion |
| `pkg/blockstore/engine/syncer.go` + `upload.go` | MODIFIED (`WriteBlock`→`Put`, `ReadBlock`→`Get`, `WriteBlockWithHash`→`Put`) | engine consumer | streaming | grep results show callers at engine/sync_entry.go:43, engine/fetch.go:231,431,491 — mechanical renames | exact |
| `pkg/blockstore/blockstoretest/conformance.go` | NEW | test-conformance | n/a | `pkg/blockstore/local/localtest/suite.go` + `pkg/blockstore/remote/remotetest/suite.go` | structural collapse |
| `pkg/blockstore/blockstoretest/appendlog.go` | NEW | test-conformance | n/a | `pkg/blockstore/local/localtest/appendlog_suite.go` (lines 17–45) | exact |
| `pkg/blockstore/local/localtest/` | **DELETED** (collapsed) | test-conformance | n/a | n/a | structural collapse |
| `pkg/blockstore/remote/remotetest/` | **DELETED** (collapsed) | test-conformance | n/a | n/a | structural collapse |
| `cmd/dfs/commands/migrate_to_cas.go` | NEW | cobra-subcommand | offline-batch | `cmd/dfs/commands/migrate.go` (cobra shape) + `cmd/dfs/commands/status.go:96–112` (PID-file refusal) | exact |
| `pkg/blockstore/migrate/migrate_to_cas.go` | NEW | migration library | offline-batch | `pkg/blockstore/migrate/journal.go` (resume), `walk.go` (share traversal), `pkg/blockstore/chunker/chunker.go` (FastCDC), `pkg/blockstore/local/fs/rollup.go:325` (`blake3ContentHash`), `pkg/blockstore/objectid.go::ComputeObjectID` | exact |
| `cmd/dfs/commands/start.go` | MODIFIED (`errors.As` + exit 78) | boot-guard | request-response | self (lines 63–127 — existing `runStart` fail-fast pattern) | exact |
| `pkg/blockstore/doc.go` | MODIFIED (document `.cas-migrated-v1` convention) | docs | n/a | self (lines 1–14) | exact |
| `docs/CONFIGURATION.md` | MODIFIED (add migration section) | docs | n/a | existing CONFIGURATION.md (not read in this pass; planner cross-refs) | role-match |

## Pattern Assignments

### `pkg/blockstore/blockstore.go` (interface-definition, request-response)

**Goal:** Declare `BlockStore`, `BlockStoreAppend`, and minimal `Meta{Size, LastModified}` as the single contract for fs / s3 / memory backends. `BlockStoreAppend` MUST embed `BlockStore`.

**Analog A — Phase 16 `local.Get` signature LOCK** (`pkg/blockstore/local/local.go:71–85`):

```go
// Get returns the chunk bytes addressed by the given content hash.
// The returned []byte is freshly allocated and owned by the caller
// — matches the prior mmap-then-copy semantics ...
//
// Returns blockstore.ErrChunkNotFound if the chunk is absent from
// the local store. Implementations MUST NOT return a slice that
// aliases internal storage; no read-buffer pool is used.
//
// Signature is forward-compatible with the unified BlockStore.Get
// interface — engine call sites can narrow the receiver type without
// renaming.
Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)
```

**Phase 17's `BlockStore.Get` MUST be byte-identical** to this signature. Engine consumer at `pkg/blockstore/engine/engine.go:230–250` continues to compile with zero call-site churn.

**Analog B — existing RemoteStore method docstrings** (`pkg/blockstore/remote/remote.go:114–128` for `HeadObject`, `pkg/blockstore/remote/remote.go:95–96` for `ReadBlockRange`):

```go
// HeadObject returns object metadata (ContentLength + lowercased
// user-metadata headers) without transferring the body. Returns
// blockstore.ErrBlockNotFound (or a wrapping error) when the key is
// missing.
HeadObject(ctx context.Context, key string) (HeadResult, error)

// ReadBlockRange reads a byte range from a block. Returns error if missing.
ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error)
```

After Phase 17 rename: `HeadObject` → `Head`, returning `Meta`; `ReadBlockRange` → `GetRange(ctx, hash ContentHash, offset, length int64) ([]byte, error)` (keyed by hash, not opaque `blockKey`).

**`Meta` struct** (per D-08, minimal — hash is the key, not echoed):

```go
// Meta is the minimal per-object metadata returned by BlockStore.Head.
// ContentHash is the lookup key — never echoed inside Meta (D-08).
// S3's x-amz-meta-content-hash header is preserved inside the s3
// backend as defense-in-depth (BSCAS-06) but not exposed here.
type Meta struct {
    Size         int64
    LastModified time.Time
}
```

**Walk-callback contract** (per D-07, mirror `filepath.SkipDir`):

```go
// Walk enumerates every object in the store. The callback receives the
// content hash and Meta for each object; ordering is unspecified.
//
// Returning blockstore.ErrStopWalk exits cleanly (Walk returns nil).
// Any other non-nil error halts the walk and Walk returns it wrapped
// with fmt.Errorf("walk halted at %s: %w", hash, err).
// Context cancellation aborts immediately; callback is NOT re-invoked
// after ctx.Err() != nil.
Walk(ctx context.Context, fn func(hash ContentHash, meta Meta) error) error
```

---

### `pkg/blockstore/errors.go` (error-sentinel, additive)

**Pattern source:** lines 121–134 of self show the established sentinel idiom — `var ErrFoo = errors.New("...")`, doc paragraph above each sentinel, wrapping via `fmt.Errorf("...: %w", err)`.

**Add at line ~170 (after `ErrBlockRefMissing`):**

```go
// ErrStopWalk is the sentinel a Walk callback returns to request a
// clean early exit (e.g., GC found its target). Walk returns nil to
// the outer caller. Any non-ErrStopWalk error halts and propagates
// wrapped with file/offset context. Mirrors filepath.SkipDir /
// fs.SkipAll. See BlockStore.Walk (Phase 17 D-07).
ErrStopWalk = errors.New("blockstore: stop walk")

// ErrLegacyLayoutDetected is returned by *fs.FSStore.NewFSStore when
// the share directory contains legacy `.blk` files but no
// `.cas-migrated-v1` sentinel. cmd/dfs/start unwraps via errors.As,
// prints an operator directive, and exits 78 (EX_CONFIG). The
// wrapped target carries the offending share path:
//
//   return nil, fmt.Errorf("%w: share path %s", ErrLegacyLayoutDetected, baseDir)
//
// Operator action: run `dfs migrate-to-cas --share <name>` (or
// `dfs migrate-to-cas` for all shares) and retry. See
// docs/CONFIGURATION.md §migration. Phase 17 D-10/D-11.
ErrLegacyLayoutDetected = errors.New("blockstore: legacy .blk layout detected (run `dfs migrate-to-cas`)")
```

The wrapping pattern matches existing `engine/errors.go:24` (`ErrLegacyReadOnCASOnly`) which is similarly wrapped via `fmt.Errorf("%w: block_id=%s", ...)` at `engine/fetch.go:76`.

---

### `pkg/blockstore/local/fs/fs.go` (boot-guard add in `NewFSStore`)

**Analog A — existing `New` constructor** (`pkg/blockstore/local/fs/fs.go:253–276`):

```go
func New(baseDir string, maxDisk int64, maxMemory int64, fileBlockStore blockstore.EngineFileBlockStore) (*FSStore, error) {
    if err := os.MkdirAll(baseDir, 0755); err != nil {
        return nil, fmt.Errorf("local store: create base dir: %w", err)
    }
    if maxMemory <= 0 {
        maxMemory = 256 * 1024 * 1024 // 256MB default
    }
    // ... seed maps + LRU + start rollup ...
    return bc, nil
}
```

**Pattern to add** (insert after `os.MkdirAll`, before any other I/O — cheap O(1) stat check per D-10):

```go
// Phase 17 D-10/D-11: refuse to open a share that still holds the
// legacy {payloadID}/block-{idx}.blk layout. The sentinel file is
// atomically renamed into place ONLY at successful completion of
// `dfs migrate-to-cas`; its presence is the canonical proof. The
// stat call is O(1); we do not enumerate.
sentinel := filepath.Join(baseDir, ".cas-migrated-v1")
if _, err := os.Stat(sentinel); errors.Is(err, os.ErrNotExist) {
    // Sentinel absent — check whether any legacy .blk files exist.
    // An empty share dir (greenfield bring-up) passes through.
    hasLegacy, scanErr := containsLegacyBlkFiles(baseDir)
    if scanErr != nil {
        return nil, fmt.Errorf("local store: scan for legacy layout: %w", scanErr)
    }
    if hasLegacy {
        return nil, fmt.Errorf("%w: share path %s", blockstore.ErrLegacyLayoutDetected, baseDir)
    }
} else if err != nil {
    return nil, fmt.Errorf("local store: stat .cas-migrated-v1: %w", err)
}
```

**Sentinel write pattern** (in `pkg/blockstore/migrate/migrate_to_cas.go` at end of successful run — mirror the atomic-rename pattern from `journal.go:346–369`):

```go
// Atomic-rename so a crash never leaves a partial-state sentinel
// (D-10). Records timestamp + tool version for the audit trail.
type sentinelMeta struct {
    Version       string    `json:"version"`
    Timestamp     time.Time `json:"timestamp"`
    ToolVersion   string    `json:"tool_version"`
}
data, _ := json.MarshalIndent(sentinelMeta{
    Version: "1", Timestamp: time.Now().UTC(), ToolVersion: dfsVersion,
}, "", "  ")
tmp := filepath.Join(baseDir, ".cas-migrated-v1.tmp")
if err := os.WriteFile(tmp, data, 0o644); err != nil { return err }
if err := os.Rename(tmp, filepath.Join(baseDir, ".cas-migrated-v1")); err != nil { return err }
```

---

### `cmd/dfs/start.go` (boot-guard consumer)

**Analog — existing error-wrap-and-fail pattern in `runStart`** (`cmd/dfs/commands/start.go:88–96`):

```go
cpStore, err := store.New(&cfg.Database)
if err != nil {
    return fmt.Errorf("failed to initialize control plane store: %w", err)
}
// ...
adminPassword, err := cpStore.EnsureAdminUser(ctx)
if err != nil {
    return fmt.Errorf("failed to ensure admin user: %w", err)
}
```

**Pattern to add** after the `runtime.LoadSharesFromStore` call at line 186 — share load is where `NewFSStore` runs per-share:

```go
// Phase 17 D-11: legacy layout halts boot with explicit operator
// directive + EX_CONFIG (78) exit. Per-share fail-fast: first
// un-migrated share trips the gate.
if err := runtime.LoadSharesFromStore(ctx, rt, cpStore); err != nil {
    var legacy *blockstore.ErrLegacyLayoutDetected  // sentinel wrapped via fmt.Errorf("%w: ...")
    if errors.As(err, &legacy) {
        fmt.Fprintln(os.Stderr, "Detected legacy `.blk` layout. v0.16+ requires CAS migration.")
        fmt.Fprintln(os.Stderr, "Run `dfs migrate-to-cas --share <name>` (or `dfs migrate-to-cas` for all shares) before starting.")
        fmt.Fprintln(os.Stderr, "See docs/CONFIGURATION.md §migration.")
        fmt.Fprintf(os.Stderr, "Offending path: %v\n", err)
        os.Exit(78) // EX_CONFIG per sysexits(3)
    }
    logger.Warn("Failed to load some shares", "error", err)
}
```

**Note:** `ErrLegacyLayoutDetected` is `errors.New(...)`, not a struct — `errors.As` requires either `errors.Is` (preferred) or a struct type. Use:

```go
if errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
    // unwrap path from %w wrap: err.Error() already carries "...: share path /...".
    fmt.Fprintf(os.Stderr, "%v\n", err)
    os.Exit(78)
}
```

---

### `cmd/dfs/commands/migrate_to_cas.go` (NEW cobra subcommand)

**Analog A — existing cobra subcommand shape** (`cmd/dfs/commands/migrate.go:1–60`):

```go
package commands

import (
    "context"
    "fmt"
    // ...
    "github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
    Use:   "migrate",
    Short: "Run database migrations",
    Long:  `...`,
    RunE:  runMigrate,
}

func runMigrate(cmd *cobra.Command, args []string) error {
    cfg, err := config.MustLoad(GetConfigFile())
    if err != nil { return err }
    if err := InitLogger(cfg); err != nil { return err }
    // ... actual work ...
    return nil
}
```

**Wire into `root.go`** (mirror line 53 of `cmd/dfs/commands/root.go`):

```go
rootCmd.AddCommand(migrateToCasCmd)
```

**Analog B — PID-file refusal** (`cmd/dfs/commands/status.go:96–112`):

```go
pidData, err := os.ReadFile(pidPath)
if err == nil {
    pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
    if err == nil {
        process, err := os.FindProcess(pid)
        if err == nil {
            // On Unix, FindProcess always succeeds, we need to send signal 0 to check
            err = process.Signal(syscall.Signal(0))
            if err == nil {
                status.Running = true
                status.PID = pid
            }
        }
    }
}
```

**Pattern to use** at top of `runMigrateToCas` (per D-02 — refuse if server live):

```go
// Refuse to run if a live dfs server holds the PID file. The migration
// rewrites .blk files in place; a concurrent server would race the
// rename and corrupt the store.
pidPath := GetDefaultPidFile()
if pidData, err := os.ReadFile(pidPath); err == nil {
    pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
    if pid > 0 {
        if proc, perr := os.FindProcess(pid); perr == nil {
            if proc.Signal(syscall.Signal(0)) == nil {
                return fmt.Errorf("dfs server is running (pid %d) — stop it before running migrate-to-cas", pid)
            }
        }
    }
}
```

**Flag wiring** (per D-04/D-05/D-06):

```go
func init() {
    migrateToCasCmd.Flags().BoolVar(&mtcDryRun, "dry-run", false, "Walk legacy .blk tree and report file count, total bytes, estimated dedup ratio, estimated duration. Writes nothing.")
    migrateToCasCmd.Flags().StringVar(&mtcShare, "share", "", "Scope migration to one share (default: all shares)")
    migrateToCasCmd.Flags().BoolVar(&mtcJSON, "json", false, "Emit one JSON object per line on stdout (machine-parseable progress)")
}
```

---

### `pkg/blockstore/migrate/migrate_to_cas.go` (NEW library)

**Analog A — journal resume idiom** (`pkg/blockstore/migrate/journal.go:257–294` — Append + auto-Snapshot):

```go
func (j *Journal) Append(e JournalEntry) error {
    if j.readOnly { return ErrJournalReadOnly }
    if e.Version == 0 { e.Version = JournalEntryVersion }
    if e.Timestamp.IsZero() { e.Timestamp = time.Now().UTC() }
    j.mu.Lock()
    defer j.mu.Unlock()
    // ... write JSON line + newline, fsync ...
    j.done[e.FileHandle] = e
    j.appended++
    if j.appended >= j.snapshotEvery {
        if err := j.snapshotLocked(); err != nil { return err }
    }
    return nil
}
```

**Pattern reuse:** Phase 17 `migrate_to_cas.go` calls `migrate.OpenJournal(<shareDir>)` and `j.IsFileDone(handle)` to skip already-migrated files. Per D-03 the per-share journal location is `<storage_dir>/<share>/.dittofs-migrate-to-cas.state` — choose **either** the existing `migrate.JournalFile` (`.migration-state.jsonl`) constant **or** a new constant scoped to this command. Plan should pick one and document; aliasing risks confusing operators who ran Phase 14's `dfsctl blockstore migrate` previously.

**Analog B — share walk idiom** (`pkg/blockstore/migrate/walk.go:31–97`):

```go
func WalkShareFiles(ctx context.Context, mds metadata.MetadataStore, shareName string, fn WalkCallback) error {
    root, err := mds.GetRootHandle(ctx, shareName)
    if err != nil {
        return fmt.Errorf("migrate: get root handle for share %q: %w", shareName, err)
    }
    return walkDir(ctx, mds, root, fn)
}
```

**Reuse directly.** The Phase 17 migration loop is:

```go
err := migrate.WalkShareFiles(ctx, mds, shareName, func(h metadata.FileHandle, f *metadata.File) error {
    if journal.IsFileDone(h.String()) {
        return nil // resume skip
    }
    if err := ctx.Err(); err != nil { return err }
    // 1. Read legacy .blk files for this payload from disk
    // 2. Chunk via chunker.NewChunker().Next(stream, true)
    // 3. For each chunk: hash = blake3ContentHash(chunk); store.Put(ctx, hash, chunk)
    // 4. Build []BlockRef list, recompute ObjectID via blockstore.ComputeObjectID
    // 5. Persist FileAttr.Blocks + ObjectID in metadata txn
    // 6. Delete .blk files
    // 7. journal.Append(JournalEntry{FileHandle: h.String(), Kind: "file_done", ...})
    return nil
})
```

**Analog C — FastCDC chunking loop** (`pkg/blockstore/local/fs/rollup.go:314–330`):

```go
minOff := minRecOffset(recs)
ck := chunker.NewChunker()
pos := minOff
for pos < uint64(len(stream)) {
    b, _ := ck.Next(stream[pos:], true)
    if b <= 0 { break }
    chunkBytes := stream[pos : pos+uint64(b)]
    h := blake3ContentHash(chunkBytes)
    if err := bc.StoreChunk(ctx, h, chunkBytes); err != nil {
        return fmt.Errorf("rollup: StoreChunk: %w", err)
    }
    pos += uint64(b)
}
```

**Reuse verbatim** inside the migration loop. `bc.StoreChunk` → `bs.Put` post-Phase-17 rename (same on-disk path; just the interface method name changes).

**Analog D — ObjectID recompute** (`pkg/blockstore/objectid.go:41–50`):

```go
func ComputeObjectID(blocks []BlockRef) ObjectID {
    h := blake3.New(32, nil)
    _, _ = h.Write([]byte(objectIDDomainPrefix))
    for i := range blocks {
        _, _ = h.Write(blocks[i].Hash[:])
    }
    var out ObjectID
    h.Sum(out[:0])
    return out
}
```

**Reuse directly.** Per CONTEXT §canonical_refs — "recomputed during migration to rebuild `FileAttr.Blocks` manifest with correct ObjectIDs."

---

### `pkg/blockstore/blockstoretest/conformance.go` (NEW)

**Analog A — `localtest/suite.go::RunSuite`** (`pkg/blockstore/local/localtest/suite.go:33–49`):

```go
func RunSuite(t *testing.T, factory Factory) {
    t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
    t.Run("ReadMiss", func(t *testing.T) { testReadMiss(t, factory) })
    t.Run("WriteMultiBlock", func(t *testing.T) { testWriteMultiBlock(t, factory) })
    // ... 12 more scenarios ...
}

func testWriteAndRead(t *testing.T, factory Factory) {
    store := factory(t)
    ctx := context.Background()
    data := bytes.Repeat([]byte("hello"), 100)
    if err := store.WriteAt(ctx, "file1", data, 0); err != nil { t.Fatalf("WriteAt failed: %v", err) }
    // ...
}
```

**Analog B — `remotetest/suite.go::RunSuite`** (`pkg/blockstore/remote/remotetest/suite.go:39–58`):

```go
func RunSuite(t *testing.T, factory Factory) {
    t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
    t.Run("ReadNotFound", func(t *testing.T) { testReadNotFound(t, factory) })
    t.Run("ReadBlockRange", func(t *testing.T) { testReadBlockRange(t, factory) })
    t.Run("DeleteBlock", func(t *testing.T) { testDeleteBlock(t, factory) })
    // ...
    t.Run("HeadObjectRoundTrip", func(t *testing.T) { TestHeadObjectRoundTrip(t, factory) })
}
```

**Pattern for new file** (per D-09 — two top-level entrypoints):

```go
// Factory creates a fresh BlockStore for a single test.
type Factory func(t *testing.T) blockstore.BlockStore

// AppendFactory creates a fresh BlockStoreAppend for a single test.
type AppendFactory func(t *testing.T) blockstore.BlockStoreAppend

// BlockStoreConformance runs the unified contract suite against any
// BlockStore impl. fs / s3 / memory backends call this. The append-
// log scenarios live in BlockStoreAppendConformance — backends that
// implement BlockStoreAppend call both (e.g., fs); s3 / memory call
// only this entrypoint.
func BlockStoreConformance(t *testing.T, factory Factory) {
    t.Run("Put_Get_Roundtrip", func(t *testing.T) { testPutGetRoundtrip(t, factory) })
    t.Run("Get_NotFound", func(t *testing.T) { testGetNotFound(t, factory) })
    t.Run("GetRange", func(t *testing.T) { testGetRange(t, factory) })
    t.Run("Delete", func(t *testing.T) { testDelete(t, factory) })
    t.Run("Walk", func(t *testing.T) { testWalk(t, factory) })
    t.Run("Walk_ErrStopWalk", func(t *testing.T) { testWalkStopSentinel(t, factory) })
    t.Run("Head", func(t *testing.T) { testHead(t, factory) })
    t.Run("Put_Idempotent_SameHash", func(t *testing.T) { testPutIdempotent(t, factory) })
    t.Run("Put_Concurrent_SameHash", func(t *testing.T) { testPutConcurrent(t, factory) })
}

func BlockStoreAppendConformance(t *testing.T, factory AppendFactory) {
    t.Run("AppendWrite_Rollup_Chunks", func(t *testing.T) { testAppendRollup(t, factory) })
    t.Run("DeleteLog", func(t *testing.T) { testDeleteLog(t, factory) })
}
```

**Walk-sentinel test** (per D-07 — pin the contract):

```go
func testWalkStopSentinel(t *testing.T, factory Factory) {
    store := factory(t)
    ctx := context.Background()
    // Seed 3 objects
    for _, b := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
        h := blake3Sum(b)
        if err := store.Put(ctx, h, b); err != nil { t.Fatalf("Put: %v", err) }
    }
    seen := 0
    err := store.Walk(ctx, func(h blockstore.ContentHash, m blockstore.Meta) error {
        seen++
        if seen == 1 {
            return blockstore.ErrStopWalk
        }
        return nil
    })
    if err != nil { t.Fatalf("Walk should return nil on ErrStopWalk, got %v", err) }
    if seen != 1 { t.Fatalf("Walk should stop after first ErrStopWalk, saw %d objects", seen) }
}
```

---

### `pkg/blockstore/blockstoretest/appendlog.go` (NEW)

**Analog — `localtest/appendlog_suite.go::RunAppendLogSuite`** (`pkg/blockstore/local/localtest/appendlog_suite.go:39–45`):

```go
func RunAppendLogSuite(t *testing.T, factory AppendLogFactory) {
    t.Run("AppendLogRoundTrip", func(t *testing.T) { testAppendLogRoundTrip(t, factory) })
    t.Run("PressureChannel_INV05", func(t *testing.T) { testPressureChannelINV05(t, factory) })
    t.Run("TornWriteRecovery_LSL06", func(t *testing.T) { testTornWriteRecovery(t, factory) })
    t.Run("ConcurrentStorm", func(t *testing.T) { testConcurrentStorm(t, factory) })
    t.Run("RollupOffsetMonotone_INV03", func(t *testing.T) { testRollupOffsetMonotoneINV03(t, factory) })
}
```

**Pattern:** keep the 5 existing scenarios; rename factory type from `func(t *testing.T) *fs.FSStore` to `func(t *testing.T) blockstore.BlockStoreAppend`. The fs backend's factory is the only caller that supplies a concrete type implementing `BlockStoreAppend`.

---

### `pkg/blockstore/engine/fetch.go` (delete legacy branch)

**Lines to delete** (`pkg/blockstore/engine/fetch.go:58–82`):

```go
    // Legacy path: pre-Phase-11 row, no hash, unverifiable. The legacy
    // key was persisted at upload time. Removed in Phase 15 (A6).
    //
    // Plan 14-02 (MIG-03 / D-A8): per-share gate. If this share's
    // BlockLayout has been flipped to cas-only, the legacy fallback is
    // disabled — ...
    if m.blockLayout == metadata.BlockLayoutCASOnly {
        logger.Error("legacy FileBlock encountered on cas-only share — possible migration drift", ...)
        return "", nil, fmt.Errorf("%w: block_id=%s", ErrLegacyReadOnCASOnly, fb.ID)
    }
    if fb.BlockStoreKey == "" {
        return "", nil, nil
    }
    data, err := m.remoteStore.ReadBlock(ctx, fb.BlockStoreKey)
    return fb.BlockStoreKey, data, err
```

**Replace with** a single fail-loud line (Phase 17 keeps no shim):

```go
// Legacy path deleted Phase 17 (subsumes Phase 15 A6). Any FileBlock
// surfacing here without a CAS hash is migration drift — refuse the
// read instead of returning silent zeros.
return "", nil, fmt.Errorf("blockstore: legacy zero-hash FileBlock encountered post-migration: block_id=%s", fb.ID)
```

The `metadata.BlockLayoutCASOnly` import disappears from `fetch.go` (Phase 18 cleans up the metadata enum itself).

---

### `pkg/blockstore/remote/s3/store.go` (method renames + Walk)

**Analog — existing WriteBlockWithHash** (`pkg/blockstore/remote/s3/store.go:246–265`):

```go
func (s *Store) WriteBlockWithHash(ctx context.Context, blockKey string, hash blockstore.ContentHash, data []byte) error {
    if err := s.checkClosed(); err != nil { return err }
    key := s.fullKey(blockKey)
    _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(key),
        Body:   bytes.NewReader(data),
        Metadata: map[string]string{
            "content-hash": hash.CASKey(),
        },
    })
    if err != nil { return fmt.Errorf("s3 put object with hash: %w", err) }
    return nil
}
```

**Rename pattern** (the `BlockStore.Put` collapses both `WriteBlock` and `WriteBlockWithHash` — hash is always passed because hash is the key):

```go
// Put writes data under the CAS-shaped key derived from hash. The
// x-amz-meta-content-hash header is stamped as defense-in-depth
// (BSCAS-06); the s3 backend recomputes BLAKE3 on read and fails
// closed on header mismatch via ReadBlockVerified (now unified
// inside Get for the verified-read case).
func (s *Store) Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error {
    if err := s.checkClosed(); err != nil { return err }
    key := s.fullKey(blockstore.FormatCASKey(hash))
    _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket:   aws.String(s.bucket),
        Key:      aws.String(key),
        Body:     bytes.NewReader(data),
        Metadata: map[string]string{"content-hash": hash.CASKey()},
    })
    if err != nil { return fmt.Errorf("s3 put: %w", err) }
    return nil
}
```

**Walk collapse** — existing `ListByPrefixWithMeta` at `pkg/blockstore/remote/s3/store.go:545` provides the page-by-page S3 iteration. Wrap it into `Walk`:

```go
func (s *Store) Walk(ctx context.Context, fn func(blockstore.ContentHash, blockstore.Meta) error) error {
    objs, err := s.ListByPrefixWithMeta(ctx, "cas/")
    if err != nil { return fmt.Errorf("s3 walk: %w", err) }
    for _, o := range objs {
        hash, err := blockstore.ParseCASKey(o.Key)
        if err != nil { continue } // ignore non-CAS keys
        if cberr := fn(hash, blockstore.Meta{Size: o.Size, LastModified: o.LastModified}); cberr != nil {
            if errors.Is(cberr, blockstore.ErrStopWalk) { return nil }
            return fmt.Errorf("walk halted at %s: %w", hash, cberr)
        }
        if err := ctx.Err(); err != nil { return err }
    }
    return nil
}
```

Phase 18 may convert this to a true cursor-based stream; Phase 17 keeps it list-then-iterate.

---

## Shared Patterns (apply to multiple files)

### Sentinel error idiom
**Source:** `pkg/blockstore/errors.go:121–134` + `pkg/blockstore/engine/errors.go:5–24`
**Apply to:** all new sentinels in this phase

```go
// ErrFoo is returned when [...]. [Doc paragraph explaining when,
// who returns it, who unwraps it, what operator action recovers.]
//
// Wrapping pattern at call site:
//   return fmt.Errorf("%w: <context>", ErrFoo)
//
// Detection pattern at caller:
//   if errors.Is(err, ErrFoo) { ... }
var ErrFoo = errors.New("blockstore: foo failed")
```

### Atomic-rename for irreversible state files
**Source:** `pkg/blockstore/migrate/journal.go:346–369` (snapshot rotation); `pkg/blockstore/local/fs/chunkstore.go:61–98` (CAS chunk write)
**Apply to:** `.cas-migrated-v1` sentinel writer in `migrate_to_cas.go`; any future sentinel files

Sequence: `os.CreateTemp` (or `os.OpenFile` with `.tmp` suffix) → write → `f.Sync()` → `f.Close()` → `os.Rename(tmp, dest)` → `syncDir(parent)`. Never write the canonical name first.

### Context cancellation in long-running loops
**Source:** `pkg/blockstore/migrate/walk.go:53–67`
**Apply to:** migration loop, Walk callback dispatch

```go
for {
    if err := ctx.Err(); err != nil { return err }
    // ... one iteration ...
}
```

Always check `ctx.Err()` at the top of each iteration before any I/O.

### Cobra subcommand wiring
**Source:** `cmd/dfs/commands/migrate.go` (entire file) + `cmd/dfs/commands/root.go:50–58`
**Apply to:** `migrate_to_cas.go`

1. Declare `var migrateToCasCmd = &cobra.Command{Use: "migrate-to-cas", Short: ..., Long: ..., RunE: runMigrateToCas}`
2. Flags wired in `init()` via `migrateToCasCmd.Flags().BoolVar(...)`
3. Register in `cmd/dfs/commands/root.go:init()` via `rootCmd.AddCommand(migrateToCasCmd)`
4. `RunE` returns `error`; root prints `Error: %v\n` and exits 1 — but for Phase 17 boot guard, `cmd/dfs/start.go` exits 78 directly on `ErrLegacyLayoutDetected`.

### Project conventions (CLAUDE.md)
**Apply to:** every file in this phase

- **Protocol-handler boundary:** none of these files live in `internal/adapter/`. No risk; planner can ignore.
- **AuthContext threading:** migration command is offline (server stopped); no AuthContext required. Library functions take `ctx context.Context` but no auth.
- **Error codes:** sentinel + wrap; `metadata.ExportError` does not apply (this is block-store layer, not protocol).
- **Sign commits:** every commit `git commit -S`. No `--no-gpg-sign` fallback.
- **No Claude Code mentions:** in commits, PRs, code comments, or docs.

## No Analog Found

| File | Reason |
|------|--------|
| (none) | Every file has either an exact, role-match, or structural-collapse analog. The Phase 14 A5 + Phase 16 work pre-staged all the patterns Phase 17 needs. |

## Metadata

**Analog search scope:** `pkg/blockstore/`, `cmd/dfs/commands/`, `pkg/blockstore/migrate/`, `pkg/blockstore/local/`, `pkg/blockstore/remote/`, `pkg/blockstore/engine/`, `pkg/blockstore/chunker/`.
**Files scanned:** ~70 (full blockstore tree + cmd/dfs).
**Pattern extraction date:** 2026-05-20.
**Phase 16 carry-forward confirmed:** `*FSStore.Get` at `pkg/blockstore/local/fs/chunkstore.go:147` and `LocalStore.Get` at `pkg/blockstore/local/local.go:85` are byte-identical to the signature Phase 17's `BlockStore.Get` MUST adopt — zero call-site rename burden at the engine boundary.
