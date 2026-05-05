package blockstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"lukechampine.com/blake3"

	"golang.org/x/time/rate"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/chunker"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// migrateOptions captures the resolved CLI flags for the migration loop.
// Plan 14-04 wires parallel + bandwidthBPS into the worker pool +
// shared rate.Limiter. bandwidthRaw is preserved for diagnostics
// (logged at startup) — bandwidthBPS is the parsed integer that the
// limiter consumes.
type migrateOptions struct {
	share        string
	dryRun       bool
	parallel     int
	bandwidthRaw string
	// bandwidthBPS: parsed bytes-per-second cap on aggregate uploads
	// across all workers. 0 = unlimited (limiter is nil). D-A9 + D-A11.
	bandwidthBPS int64
	stateDir     string
}

// migrateResult is the loop-level summary. Returned to the caller
// (which formats it for stdout via printMigrateResult) and consumed by
// migrate_loop_test.go for behavioral assertions.
type migrateResult struct {
	FilesTotal        int
	FilesDone         int
	FilesSkipped      int
	BytesUploaded     uint64
	BytesDeduped      uint64
	StartedAt         time.Time
	DurationMS        int64
	LegacyKeysDeleted int // Populated by Plan 14-05 — zero for now.
}

// perFileResult is the migrateOneFile per-invocation summary.
type perFileResult struct {
	BytesUploaded uint64
	BytesDeduped  uint64
	Blocks        []blockstore.BlockRef
	ObjectID      blockstore.ObjectID
	Skipped       bool
}

// runMigrateLoop is the cobra command's dispatch hook. Production
// dispatches through openOfflineRuntime (which today returns
// ErrOfflineRuntimeNotWired — see migrate_runtime.go); tests bypass this
// path entirely by constructing an offlineRuntime via
// newTestOfflineRuntime and calling runMigrateLoopWithRuntime directly
// (see migrate_loop_test.go). The var indirection is preserved from
// Task 1 so tests can still swap the dispatch if a future suite needs
// to assert the cobra-side wiring.
var runMigrateLoop = func(ctx context.Context, opts migrateOptions) error {
	rt, err := openOfflineRuntime(ctx, opts.share)
	if err != nil {
		return err
	}
	defer rt.Close()
	return runMigrateLoopWithRuntime(ctx, rt, opts)
}

// runMigrateLoopWithRuntime is the testable core. It walks every file
// in the share, re-chunks legacy blocks via FastCDC, dedup-probes via
// GetByHash, uploads only new chunks, and commits per-file via PutFile.
// The journal records each commit; resume reads the journal head.
func runMigrateLoopWithRuntime(ctx context.Context, rt *offlineRuntime, opts migrateOptions) error {
	if rt == nil {
		return errors.New("blockstore migrate: nil offlineRuntime")
	}

	// Open the journal at the share's data dir (or the override).
	journalDir := opts.stateDir
	if journalDir == "" {
		journalDir = rt.DataDir()
	}
	if journalDir == "" {
		return errors.New("blockstore migrate: empty journalDir; pass --state-dir or wire offlineRuntime.DataDir")
	}
	journal, err := migrate.OpenJournal(journalDir)
	if err != nil {
		return fmt.Errorf("blockstore migrate: open journal: %w", err)
	}
	defer journal.Close()

	// Build the shared upload limiter once (D-A9 + D-A11). nil = no
	// metering. Legacy reads stay unmetered: bandwidthWait is only
	// invoked on the upload path inside rechunkAndUpload.
	limiter := newBandwidthLimiter(opts.bandwidthBPS)
	if limiter != nil {
		logger.Info("blockstore migrate: bandwidth limit",
			"bytes_per_sec", opts.bandwidthBPS,
			"raw", opts.bandwidthRaw,
			"burst", limiter.Burst(),
		)
	}

	result := migrateResult{StartedAt: time.Now()}

	// Phase 1: walk the share into a slice so the worker pool can
	// dispatch concurrently. For TB-scale shares with millions of
	// files the slice grows to a bounded sizeof(walkedFile) ~64B
	// per entry — comfortable for the offline tool's footprint.
	// A streaming dispatcher is a possible follow-up if 100M-file
	// shares prove out in production.
	var files []walkedFile
	walkErr := migrate.WalkShareFiles(ctx, rt.MetadataStore(), rt.Share(),
		func(handle metadata.FileHandle, file *metadata.File) error {
			files = append(files, walkedFile{Handle: handle, Attr: file})
			return nil
		})
	if walkErr != nil {
		return walkErr
	}
	result.FilesTotal = len(files)

	// Phase 2: dispatch through the worker pool. progress reports
	// per-commit slog + TTY bar; pool clamps parallel into [1,
	// maxWorkerSoftCap] and short-circuits IsFileDone before
	// goroutine spawn.
	progress := newProgressReporter(len(files))
	defer progress.Close()
	pool := newWorkerPool(opts.parallel, rt, journal, opts, limiter, progress)
	poolResult, runErr := pool.Run(ctx, files)
	result.FilesDone = poolResult.FilesDone
	result.FilesSkipped = poolResult.FilesSkipped
	result.BytesUploaded = poolResult.BytesUploaded
	result.BytesDeduped = poolResult.BytesDeduped
	if runErr != nil {
		return runErr
	}

	// Final snapshot — collapse the journal to a clean snapshot file
	// for the next invocation. Skipped on dry-run (no journal writes
	// happened, so nothing to compact).
	if !opts.dryRun {
		if err := journal.Snapshot(); err != nil {
			logger.Warn("blockstore migrate: final journal snapshot failed", "err", err)
		}
	}

	// Post-loop pipeline (Plan 14-05): integrity check → cutover →
	// legacy delete. Each stage gates the next; integrity failure is
	// fail-loud (D-A8) — leaves BlockLayout=legacy, leaves journal in
	// place, leaves any uploaded CAS chunks (orphans → GC reclaims),
	// leaves all legacy keys intact. Dry-run skips all three (no state
	// to verify).
	if !opts.dryRun {
		ir, ierr := verifyIntegrity(ctx, rt, opts)
		if ierr != nil {
			logger.Error("blockstore migrate: integrity check failed; aborting cutover and legacy delete",
				"share", opts.share,
				"unique_hashes", ir.UniqueHashes,
				"head_calls", ir.HEADCalls,
				"failures", len(ir.Failures),
			)
			result.DurationMS = time.Since(result.StartedAt).Milliseconds()
			return ierr
		}
		if cerr := performCutover(ctx, rt, opts.share); cerr != nil {
			result.DurationMS = time.Since(result.StartedAt).Milliseconds()
			return cerr
		}
		// Legacy GC is best-effort (D-A13). Partial failures are
		// reported via slog + the migrateResult, but do NOT fail the
		// command — the cutover txn already succeeded, the share is
		// authoritative cas-only, and orphaned legacy keys are
		// operator-recoverable via `dfsctl store block gc`.
		deletedCount, gcErr := deleteLegacyKeys(ctx, rt, opts)
		result.LegacyKeysDeleted = deletedCount
		if gcErr != nil {
			logger.Warn("blockstore migrate: legacy key deletion had partial failures",
				"share", opts.share,
				"deleted", deletedCount,
				"err", gcErr,
			)
		}
	} else {
		logger.Info("blockstore migrate: dry-run — would have run integrity check, cutover, and legacy delete",
			"share", opts.share)
	}

	result.DurationMS = time.Since(result.StartedAt).Milliseconds()
	return printMigrateResult(result, opts.dryRun)
}

// migrateOneFile re-chunks one legacy file and commits the new BlockRef
// list + ObjectID via PutFile in a single metadata txn. The journal is
// appended only after the txn commits — a crash between PutFile success
// and journal append re-migrates that file on resume (idempotent via
// GetByHash dedup, T-14-03-02 mitigation).
func migrateOneFile(
	ctx context.Context,
	rt *offlineRuntime,
	journal *migrate.Journal,
	opts migrateOptions,
	limiter *rate.Limiter,
	handle metadata.FileHandle,
	file *metadata.File,
) (perFileResult, error) {
	var res perFileResult

	// Skip empty files: zero bytes → zero chunks → no work. We still
	// commit a journal entry so resume sees them as done.
	if file.Size == 0 {
		if !opts.dryRun {
			entry := migrate.JournalEntry{
				Kind:       "file_skipped",
				FileHandle: string(handle),
				PayloadID:  string(file.PayloadID),
			}
			if err := journal.Append(entry); err != nil {
				return res, fmt.Errorf("journal append (skipped empty): %w", err)
			}
		}
		res.Skipped = true
		return res, nil
	}

	// Build a reader over the file's legacy {payloadID}/block-{idx} keys.
	legacyReader, err := newLegacyPayloadReader(ctx, rt, string(file.PayloadID))
	if err != nil {
		return res, fmt.Errorf("open legacy reader: %w", err)
	}

	// Re-chunk via FastCDC and upload (or dedup-probe) each chunk.
	blocks, bytesUploaded, bytesDeduped, err := rechunkAndUpload(ctx, rt, opts, limiter, legacyReader)
	if err != nil {
		return res, err
	}
	res.BytesUploaded = bytesUploaded
	res.BytesDeduped = bytesDeduped
	res.Blocks = blocks
	res.ObjectID = blockstore.ComputeObjectID(blocks)

	// Dry-run: do not touch metadata, do not journal. Report-only.
	if opts.dryRun {
		return res, nil
	}

	// Per-file metadata txn: PutFile with the new Blocks + ObjectID.
	// FileAttr is embedded in File; we set the new fields on a copy and
	// PutFile-write the whole record.
	updated := *file
	updated.Blocks = blocks
	updated.ObjectID = res.ObjectID
	if err := rt.MetadataStore().PutFile(ctx, &updated); err != nil {
		// Phase 13 D-14 first-committer-wins: another file in the share
		// already owns this ObjectID (identical content). The migration
		// tool reuses the same Blocks list — chunks already deduped via
		// GetByHash + IncrementRefCount — but yields ObjectID ownership
		// to the canonical first-committer. The second file's
		// FileAttr.ObjectID is left zero; a future quiesce can populate
		// it once Phase 15 removes the dual-read shim and re-runs the
		// Merkle hook.
		if mderrors.IsConflictError(err) {
			updated.ObjectID = blockstore.ObjectID{}
			if err2 := rt.MetadataStore().PutFile(ctx, &updated); err2 != nil {
				return res, fmt.Errorf("PutFile (post-objectid-conflict retry): %w", err2)
			}
			res.ObjectID = blockstore.ObjectID{}
		} else {
			return res, fmt.Errorf("PutFile: %w", err)
		}
	}

	// Journal append AFTER PutFile success — T-14-03-02 ordering rule:
	// the journal must never claim a file is done unless metadata has
	// the new BlockRefs persisted. A crash between PutFile and Append
	// re-migrates that file on resume; GetByHash makes the re-upload
	// path idempotent.
	entry := migrate.JournalEntry{
		Kind:          "file_done",
		FileHandle:    string(handle),
		PayloadID:     string(file.PayloadID),
		Blocks:        blocks,
		ObjectID:      res.ObjectID,
		BytesUploaded: bytesUploaded,
		BytesDeduped:  bytesDeduped,
	}
	if err := journal.Append(entry); err != nil {
		return res, fmt.Errorf("journal append: %w", err)
	}

	return res, nil
}

// rechunkAndUpload runs FastCDC over the legacy reader, dedup-probes
// each chunk via GetByHash, and uploads new chunks via WriteBlockWithHash
// + Put (FileBlock row).
//
// Returns the new BlockRef list (sorted by Offset), bytes uploaded, and
// bytes deduped. Empty / nil reader yields an empty slice.
func rechunkAndUpload(
	ctx context.Context,
	rt *offlineRuntime,
	opts migrateOptions,
	limiter *rate.Limiter,
	r io.Reader,
) ([]blockstore.BlockRef, uint64, uint64, error) {
	c := chunker.NewChunker()

	// Sliding buffer: data not yet emitted as a chunk. We append from
	// the reader and slice off the prefix as Next() returns boundaries.
	buf := make([]byte, 0, chunker.MaxChunkSize*2)
	tmp := make([]byte, 1<<20) // 1 MiB read window

	var (
		blocks        []blockstore.BlockRef
		offset        uint64
		bytesUploaded uint64
		bytesDeduped  uint64
		eof           bool
	)

	for {
		// Top up the buffer if not yet at EOF and we don't have enough
		// for a min-sized chunk.
		if !eof && len(buf) < chunker.MaxChunkSize {
			n, rerr := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if errors.Is(rerr, io.EOF) {
				eof = true
			} else if rerr != nil {
				return nil, 0, 0, fmt.Errorf("read legacy: %w", rerr)
			}
		}

		if len(buf) == 0 && eof {
			break
		}

		boundary, _ := c.Next(buf, eof)
		if boundary <= 0 {
			// Need more data — should only happen when !eof. If we are
			// at EOF and still got 0, exit defensively (chunker contract
			// guarantees boundary > 0 with eof=true and len(buf) > 0).
			if eof {
				break
			}
			continue
		}

		chunk := buf[:boundary]
		hash := blockstore.ContentHash(blake3.Sum256(chunk))
		size := uint32(len(chunk))

		ref := blockstore.BlockRef{
			Hash:   hash,
			Offset: offset,
			Size:   size,
		}
		blocks = append(blocks, ref)
		offset += uint64(size)

		// Dedup probe: if a FileBlock with this hash already exists,
		// bump its RefCount and skip the upload. On dry-run, we never
		// touch the FileBlockStore — just report the would-be win.
		if !opts.dryRun {
			existing, gerr := rt.FileBlockStore().GetByHash(ctx, hash)
			if gerr != nil {
				return nil, 0, 0, fmt.Errorf("GetByHash: %w", gerr)
			}
			if existing != nil {
				if err := rt.FileBlockStore().IncrementRefCount(ctx, existing.ID); err != nil {
					return nil, 0, 0, fmt.Errorf("IncrementRefCount: %w", err)
				}
				bytesDeduped += uint64(size)
			} else {
				// Upload new chunk to remote CAS, persist FileBlock row.
				// D-A9: meter the upload (and only the upload) through
				// the shared rate.Limiter. Legacy reads stay unmetered.
				if err := bandwidthWait(ctx, limiter, len(chunk)); err != nil {
					return nil, 0, 0, fmt.Errorf("bandwidth wait: %w", err)
				}
				casKey := blockstore.FormatCASKey(hash)
				if err := rt.RemoteStore().WriteBlockWithHash(ctx, casKey, hash, chunk); err != nil {
					return nil, 0, 0, fmt.Errorf("WriteBlockWithHash %s: %w", casKey, err)
				}
				fb := &blockstore.FileBlock{
					ID:            casKey,
					Hash:          hash,
					DataSize:      size,
					BlockStoreKey: casKey,
					RefCount:      1,
					State:         blockstore.BlockStateRemote,
					CreatedAt:     time.Now(),
					LastAccess:    time.Now(),
				}
				if err := rt.FileBlockStore().Put(ctx, fb); err != nil {
					return nil, 0, 0, fmt.Errorf("FileBlockStore.Put %s: %w", casKey, err)
				}
				bytesUploaded += uint64(size)
			}
		} else {
			// Dry-run still differentiates uploaded vs. deduped so the
			// estimate is realistic — but does NOT touch any store.
			existing, gerr := rt.FileBlockStore().GetByHash(ctx, hash)
			if gerr == nil && existing != nil {
				bytesDeduped += uint64(size)
			} else {
				bytesUploaded += uint64(size)
			}
		}

		buf = buf[boundary:]
	}

	return blocks, bytesUploaded, bytesDeduped, nil
}

// printMigrateResult writes a final summary to stdout. Today's format
// is a fixed table; Plan 14-06 will add `-o json` parity.
func printMigrateResult(r migrateResult, dryRun bool) error {
	mode := "applied"
	if dryRun {
		mode = "dry-run"
	}
	_, err := fmt.Fprintf(os.Stdout,
		"Migration %s: files_total=%d files_done=%d files_skipped=%d "+
			"bytes_uploaded=%d bytes_deduped=%d duration_ms=%d\n",
		mode, r.FilesTotal, r.FilesDone, r.FilesSkipped,
		r.BytesUploaded, r.BytesDeduped, r.DurationMS)
	return err
}
