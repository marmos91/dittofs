package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/metadata"
	badgerstore "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// Offline `dfs migrate-to-cas` subcommand.
//
// Walks the configured storage tree, opens each share's destination CAS
// BlockStore via fs.NewFSStoreForMigration (sentinel-bypass), invokes
// migrate.MigrateShareToCAS per share, and writes the per-share
// `.cas-migrated-v1` sentinel that the boot guard requires.
//
// Refuses to run while the dfs server holds the PID lockfile (D-02
// OFFLINE constraint).
//
// The MetadataAdapter opens the badger metadata store directly (offline
// — the server must be stopped). Production shares normally go through
// the controlplane runtime, but the migration tool short-circuits that
// to avoid booting the full services graph.

var (
	migrateStorageDir  string
	migrateMetadataDir string
	migrateShare       string
	migrateDryRun      bool
	migrateJSON        bool
	migrateMaxDisk     int64
	migrateMaxMemory   int64
)

var migrateToCASCmd = &cobra.Command{
	Use:   "migrate-to-cas",
	Short: "Migrate legacy .blk block layout to CAS (offline; required for v0.16+ servers)",
	Long: `Migrate a stopped DittoFS server's legacy .blk block layout to the
content-addressed (CAS) layout required by v0.16+.

The dfs server MUST be stopped before running this command — the
migration rewrites the on-disk layout in place and a concurrent server
would race the rename and corrupt the store.

Required flag: --storage-dir <root>. The storage root is expected to
contain a shares/<name>/blocks/ subtree per share (legacy v0.13 layout);
the command refuses to start if the path is missing or empty.

The command is idempotent: a per-share journal at
<storage-dir>/shares/<name>/.dittofs-migrate-to-cas.state lets you
resume after a crash or Ctrl-C without re-uploading already-migrated
chunks. Successful completion writes
<storage-dir>/shares/<name>/.cas-migrated-v1 via atomic rename;
the boot guard refuses to start dfs until this sentinel exists (exit
code 78).

Use --dry-run for a non-destructive preview (file count, estimated
dedup ratio, sampled bytes-per-second).
Use --share to scope the run to one share.
Use --json to emit one JSON object per second of progress on stdout.

See docs/CONFIGURATION.md §migration for the full operator runbook.`,
	Args: cobra.NoArgs,
	RunE: runMigrateToCAS,
}

func init() {
	migrateToCASCmd.Flags().StringVar(&migrateStorageDir, "storage-dir", "",
		"Storage root (REQUIRED; expects <root>/shares/<name>/blocks layout)")
	migrateToCASCmd.Flags().StringVar(&migrateShare, "share", "",
		"Scope migration to one share (default: all shares discovered under <storage-dir>/shares/)")
	migrateToCASCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false,
		"Walk + sample only; report file count, bytes, estimated dedup ratio, ETA. Writes nothing.")
	migrateToCASCmd.Flags().BoolVar(&migrateJSON, "json", false,
		"Emit one JSON object per second of progress to stdout (machine-parseable)")
	migrateToCASCmd.Flags().Int64Var(&migrateMaxDisk, "max-disk", 0,
		"Per-share max-disk budget for the destination FSStore (0 = unlimited)")
	migrateToCASCmd.Flags().Int64Var(&migrateMaxMemory, "max-memory", 0,
		"Per-share max-memory budget for the destination FSStore (0 = 256 MiB default)")
	migrateToCASCmd.Flags().StringVar(&migrateMetadataDir, "metadata-dir", "",
		"Path to the badger metadata database directory (REQUIRED; the directory passed to the metadata store's 'path' config)")
}

func runMigrateToCAS(cmd *cobra.Command, args []string) error {
	// PID guard: refuse to run while a live dfs server holds the
	// lockfile. The migration rewrites .blk files in place; a concurrent
	// server would race the rename and corrupt the store. (D-02)
	if err := guardPidFile(); err != nil {
		return err
	}

	if migrateStorageDir == "" {
		return errors.New("--storage-dir is required (path to the dfs storage root, expected to contain shares/<name>/blocks/)")
	}
	abs, err := filepath.Abs(migrateStorageDir)
	if err != nil {
		return fmt.Errorf("resolve --storage-dir: %w", err)
	}
	migrateStorageDir = abs

	if migrateMetadataDir == "" {
		return errors.New("--metadata-dir is required (path to the badger metadata database directory)")
	}
	metaAbs, err := filepath.Abs(migrateMetadataDir)
	if err != nil {
		return fmt.Errorf("resolve --metadata-dir: %w", err)
	}
	migrateMetadataDir = metaAbs

	sharesRoot := filepath.Join(migrateStorageDir, "shares")
	entries, err := os.ReadDir(sharesRoot)
	if err != nil {
		return fmt.Errorf("read shares root %q: %w", sharesRoot, err)
	}

	var shareNames []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if migrateShare != "" && name != migrateShare {
			continue
		}
		shareNames = append(shareNames, name)
	}
	if len(shareNames) == 0 {
		if migrateShare != "" {
			return fmt.Errorf("share %q not found under %s", migrateShare, sharesRoot)
		}
		return fmt.Errorf("no shares found under %s", sharesRoot)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mds, err := badgerstore.NewBadgerMetadataStoreWithDefaults(ctx, migrateMetadataDir)
	if err != nil {
		return fmt.Errorf("open metadata store at %q: %w", migrateMetadataDir, err)
	}
	defer func() { _ = mds.Close() }()

	var aggregate migrate.MigrationStats
	for _, name := range shareNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		shareDir := filepath.Join(sharesRoot, name)

		// Destination BlockStore for this share. Use the migration-bypass
		// constructor so Plan 09's sentinel-detection gate (added later
		// inside fs.New / fs.NewWithOptions) does not refuse the share
		// — that gate is precisely what the migration is establishing.
		//
		// Phase 19 follow-up: baseDir is the share root (not the redundant
		// `<shareDir>/blocks/` sub-path). FSStore internally creates
		// `blocks/` (CAS) + `logs/` (append log) as siblings under baseDir.
		bs, openErr := fs.NewFSStoreForMigration(shareDir,
			migrateMaxDisk, migrateMaxMemory, nopFileBlockStore{},
			fs.FSStoreOptions{})
		if openErr != nil {
			return fmt.Errorf("open destination store for share %q: %w", name, openErr)
		}

		opts := migrate.MigrationOpts{
			DryRun:   migrateDryRun,
			Progress: makeProgressFn(name),
			// Sentinel lives at the FSStore baseDir (Plan 09 boot guard
			// probes `<baseDir>/.cas-migrated-v1`), which post-follow-up
			// is shareDir.
			StoreDir: shareDir,
		}

		adapter := &cliMetadataAdapter{shareName: "/" + name, store: mds}
		res, runErr := migrate.MigrateShareToCAS(ctx, shareDir, bs, adapter, opts)
		_ = bs.Close()
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "share %q failed: %v\n", name, runErr)
			fmt.Fprintf(os.Stderr, "Journal preserved at %s; rerun to resume.\n",
				filepath.Join(shareDir, migrate.MigrateJournalFile))
			return runErr
		}

		aggregate.FilesDone += res.Stats.FilesDone
		aggregate.BytesDone += res.Stats.BytesDone
		aggregate.DedupHits += res.Stats.DedupHits
		aggregate.ChunksDone += res.Stats.ChunksDone

		if migrateDryRun {
			fmt.Printf("[%s] DRY-RUN: files=%d bytes=%d chunks_sampled=%d est_dedup_ratio=%.3f duration=%s\n",
				name, res.Stats.FilesDone, res.Stats.BytesDone, res.Stats.ChunksDone,
				res.EstDedupRatio, res.Duration)
		} else {
			fmt.Printf("[%s] DONE: files=%d bytes=%d chunks=%d dedup_hits=%d duration=%s\n",
				name, res.Stats.FilesDone, res.Stats.BytesDone, res.Stats.ChunksDone,
				res.Stats.DedupHits, res.Duration)
		}
	}

	fmt.Printf("ALL SHARES OK: files=%d bytes=%d chunks=%d dedup_hits=%d\n",
		aggregate.FilesDone, aggregate.BytesDone, aggregate.ChunksDone, aggregate.DedupHits)
	return nil
}

// guardPidFile returns an error when a live dfs server holds the PID
// lockfile. Mirrors cmd/dfs/commands/status.go's process probe.
func guardPidFile() error {
	pidPath := GetDefaultPidFile()
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read PID file %q: %w", pidPath, err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if perr != nil || pid <= 0 {
		return nil
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		return nil
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return fmt.Errorf("dfs server is running (pid %d; pid file at %s); stop with `dfs stop` before migrating",
			pid, pidPath)
	}
	return nil
}

// makeProgressFn returns a Progress callback that emits plain-text or
// JSON lines on stdout depending on the --json flag.
func makeProgressFn(shareName string) func(migrate.MigrationStats) {
	return func(s migrate.MigrationStats) {
		if migrateJSON {
			obj := map[string]any{
				"ts":            time.Now().UTC().Format(time.RFC3339),
				"share":         shareName,
				"files_done":    s.FilesDone,
				"bytes_done":    s.BytesDone,
				"files_per_sec": s.FilesPerSec,
				"mib_per_sec":   s.MiBPerSec,
				"dedup_hits":    s.DedupHits,
				"eta_seconds":   s.ETASec,
			}
			b, _ := json.Marshal(obj)
			fmt.Println(string(b))
			return
		}
		fmt.Printf("[%s] %d files, %.1f MiB/s, dedup_hits=%d\n",
			shareName, s.FilesDone, s.MiBPerSec, s.DedupHits)
	}
}

// cliMetadataAdapter bridges the migration library to a live badger
// metadata store opened directly by the CLI (offline — server must be
// stopped). ListLegacyFiles walks the share tree and returns every
// regular file with Size > 0 and empty Blocks (unmigrated). UpdateFileBlocks
// commits the new CAS BlockRef manifest transactionally via PutFile.
type cliMetadataAdapter struct {
	shareName string
	store     *badgerstore.BadgerMetadataStore
}

func (a *cliMetadataAdapter) ListLegacyFiles(ctx context.Context) ([]migrate.LegacyFileInfo, error) {
	rootHandle, err := a.store.GetRootHandle(ctx, a.shareName)
	if err != nil {
		return nil, fmt.Errorf("get root handle for %s: %w", a.shareName, err)
	}
	var files []migrate.LegacyFileInfo
	if err := a.walkDir(ctx, rootHandle, "", &files); err != nil {
		return nil, err
	}
	return files, nil
}

func (a *cliMetadataAdapter) walkDir(ctx context.Context, dirHandle metadata.FileHandle, prefix string, out *[]migrate.LegacyFileInfo) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, nextCursor, err := a.store.ListChildren(ctx, dirHandle, cursor, 500)
		if err != nil {
			return fmt.Errorf("list children at %q: %w", prefix, err)
		}
		for _, e := range entries {
			file, err := a.store.GetFile(ctx, e.Handle)
			if err != nil {
				return fmt.Errorf("get file %s%s: %w", prefix, e.Name, err)
			}
			if file.Type == metadata.FileTypeDirectory {
				if err := a.walkDir(ctx, e.Handle, prefix+e.Name+"/", out); err != nil {
					return err
				}
				continue
			}
			if file.Type != metadata.FileTypeRegular || file.Size == 0 {
				continue
			}
			if len(file.Blocks) > 0 {
				continue
			}
			handle, err := metadata.EncodeFileHandle(file)
			if err != nil {
				return fmt.Errorf("encode handle for %s%s: %w", prefix, e.Name, err)
			}
			*out = append(*out, migrate.LegacyFileInfo{
				Handle:    handle,
				Path:      prefix + e.Name,
				PayloadID: file.PayloadID,
				Size:      int64(file.Size),
				BlockSize: blockstore.BlockSize,
			})
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return nil
}

func (a *cliMetadataAdapter) UpdateFileBlocks(ctx context.Context, handle metadata.FileHandle, blocks []blockstore.BlockRef) error {
	file, err := a.store.GetFile(ctx, handle)
	if err != nil {
		return fmt.Errorf("get file for update: %w", err)
	}
	file.Blocks = blocks
	pid := string(file.PayloadID)
	return a.store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if err := tx.PutFile(ctx, file); err != nil {
			return err
		}
		for _, br := range blocks {
			fb := &blockstore.FileBlock{
				ID:       fmt.Sprintf("%s/%d", pid, br.Offset),
				Hash:     br.Hash,
				DataSize: br.Size,
				State:    blockstore.BlockStateRemote,
				RefCount: 1,
			}
			if err := tx.Put(ctx, fb); err != nil {
				return fmt.Errorf("create FileBlock row %s: %w", fb.ID, err)
			}
		}
		return nil
	})
}

// nopFileBlockStore satisfies blockstore.EngineFileBlockStore without
// touching any persistent store. The migration path only writes CAS
// chunks (which fs.FSStore handles through its own chunk store on
// blocks/{hh}/{hh}/{hex}), so the FileBlock metadata surface is unused
// during migration.
type nopFileBlockStore struct{}

func (nopFileBlockStore) GetByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFileBlockStore) Put(_ context.Context, _ *blockstore.FileBlock) error { return nil }
func (nopFileBlockStore) Delete(_ context.Context, _ string) error {
	return blockstore.ErrFileBlockNotFound
}
func (nopFileBlockStore) IncrementRefCount(_ context.Context, _ string) error { return nil }
func (nopFileBlockStore) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (nopFileBlockStore) AddRef(_ context.Context, _ blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
	// Phase 19 D-04: migration path doesn't exercise the LRU hit path,
	// so always returning ErrUnknownHash is safe — production callers
	// fall back to the full Put path on this sentinel.
	return blockstore.ErrUnknownHash
}
func (nopFileBlockStore) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (nopFileBlockStore) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (nopFileBlockStore) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
