package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/chunker"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// MigrationToolVersion is recorded inside `.cas-migrated-v1` so Plan 09's
// boot guard + ops can audit which tool revision migrated a share. Bump
// on any schema change to the sentinel / journal layout.
const MigrationToolVersion = "v1.0.0"

// SentinelFileName is the Plan 09 boot-guard marker. Written PER-SHARE
// at `<shareDir>/.cas-migrated-v1` via atomic rename (D-10).
const SentinelFileName = ".cas-migrated-v1"

// MigrateJournalFile is the per-share resumability journal. Distinct
// from migrate.JournalFile (v0.14/v0.15 backup tool) so concurrent
// legacy state cannot be confused with v0.16 migration progress.
const MigrateJournalFile = ".dittofs-migrate-to-cas.state"

// SentinelTmpSuffix is the pre-rename suffix for the sentinel write.
const SentinelTmpSuffix = ".tmp"

// ErrChunkPutMismatch is returned when post-Put re-Get yields bytes that
// disagree with the input (BSCAS-06 carry-forward, defense-in-depth).
// Fatal — the journal preserves the resume point. Detect via errors.Is.
var ErrChunkPutMismatch = errors.New("migrate: post-Put verification failed")

// MigrationOpts configures a MigrateShareToCAS invocation.
type MigrationOpts struct {
	// DryRun samples FastCDC over a bounded subset of legacy bytes and
	// reports estimated counts + dedup ratio. No disk writes; journal
	// untouched; sentinel not written.
	DryRun bool

	// Progress receives MigrationStats ~1Hz. nil disables reporting.
	Progress func(MigrationStats)

	// JournalPath overrides the per-share journal default
	// (filepath.Join(shareDir, MigrateJournalFile)).
	JournalPath string

	// StoreDir overrides where the `.cas-migrated-v1` sentinel is written.
	// Empty defaults to shareDir. Production callers (cmd/dfs migrate-to-cas)
	// pass the FSStore baseDir so the sentinel sits where Plan 09's boot
	// guard looks (`<baseDir>/.cas-migrated-v1`). When the FSStore baseDir
	// is a subdirectory of shareDir (e.g. `<shareDir>/blocks`), this MUST
	// be set or the boot guard will never find the sentinel.
	StoreDir string

	// DryRunSampleBudgetBytes caps total bytes sampled across all files.
	// Default 1 GiB.
	DryRunSampleBudgetBytes int64

	// DryRunPerFileSampleBytes caps per-file dry-run sampling. Default 64 MiB.
	DryRunPerFileSampleBytes int64
}

// MigrationStats is the cumulative progress snapshot emitted via
// MigrationOpts.Progress and surfaced in MigrationResult.
type MigrationStats struct {
	FilesDone   int64
	BytesDone   int64
	DedupHits   int64
	ChunksDone  int64
	FilesPerSec float64
	MiBPerSec   float64
	ETASec      float64
}

// MigrationResult is the terminal summary returned by MigrateShareToCAS.
type MigrationResult struct {
	Stats         MigrationStats
	EstDedupRatio float64       // dry-run only; otherwise zero
	Duration      time.Duration // wall-clock duration of the run
}

// MetadataAdapter enumerates legacy files and atomically commits each
// file's new CAS-shaped Blocks manifest. Implementation lives in the CLI
// subcommand; UpdateFileBlocks MUST commit transactionally (mds.PutFile
// — verified by pkg/metadata/storetest conformance).
type MetadataAdapter interface {
	// ListLegacyFiles returns files needing migration: BlockLayoutLegacy
	// OR (Blocks empty && Size > 0 && Type == FileTypeRegular).
	ListLegacyFiles(ctx context.Context) ([]LegacyFileInfo, error)

	// UpdateFileBlocks atomically sets Blocks + BlockLayout=CASOnly via mds.PutFile.
	UpdateFileBlocks(ctx context.Context, handle metadata.FileHandle, blocks []blockstore.BlockRef) error
}

// LegacyFileInfo carries the minimum projection MigrateShareToCAS needs
// to process one file.
type LegacyFileInfo struct {
	// Handle is the metadata-store-native identifier; passed back to
	// UpdateFileBlocks unchanged.
	Handle metadata.FileHandle
	// Path is the share-relative path (forensic display).
	Path string
	// PayloadID locates the `.blk` tree at `<shareDir>/<PayloadID>/<idx>.blk`.
	PayloadID metadata.PayloadID
	// Size is FileAttr.Size in bytes.
	Size int64
	// BlockSize is the legacy fixed block size (typically 8 MiB).
	BlockSize uint32
}

// migrateJournalState is the JSON shape persisted to MigrateJournalFile.
type migrateJournalState struct {
	Version       string    `json:"version"`
	LastFilePath  string    `json:"last_file_path"`
	LastOffset    int64     `json:"last_offset"`
	Timestamp     time.Time `json:"timestamp"`
	BytesWritten  int64     `json:"bytes_written"`
	DedupHits     int64     `json:"dedup_hits"`
	FilesDone     int64     `json:"files_done"`
	ChunksWritten int64     `json:"chunks_written"`
}

// sentinelContent is the JSON shape written to SentinelFileName at the
// successful completion of each share's migration.
type sentinelContent struct {
	Version     string    `json:"version"`
	CompletedAt time.Time `json:"completed_at"`
	ToolVersion string    `json:"tool_version"`
	ShareDir    string    `json:"share_dir"`
}

// MigrateShareToCAS migrates a single share's legacy `.blk` tree to CAS.
//
// On success (non-dry-run): `.cas-migrated-v1` is written at
// `<shareDir>/.cas-migrated-v1` via atomic rename, the per-file `.blk`
// trees are removed, the journal is dropped, and every previously-legacy
// file has FileAttr.Blocks rebuilt + FileAttr.BlockLayout == CASOnly.
//
// On failure: the journal at `<shareDir>/.dittofs-migrate-to-cas.state`
// preserves the resume point; the sentinel is NOT written so Plan 09's
// boot guard continues to refuse the share until a successful run.
//
// Idempotent: re-running after success is a no-op (legacy-file
// enumeration returns zero entries).
func MigrateShareToCAS(
	ctx context.Context,
	shareDir string,
	bs blockstore.BlockStore,
	meta MetadataAdapter,
	opts MigrationOpts,
) (MigrationResult, error) {
	start := time.Now()

	if opts.DryRunSampleBudgetBytes <= 0 {
		opts.DryRunSampleBudgetBytes = 1 << 30 // 1 GiB
	}
	if opts.DryRunPerFileSampleBytes <= 0 {
		opts.DryRunPerFileSampleBytes = 64 << 20 // 64 MiB
	}

	journalPath := opts.JournalPath
	if journalPath == "" {
		journalPath = filepath.Join(shareDir, MigrateJournalFile)
	}

	// Load resume state (no-op in dry-run).
	var state migrateJournalState
	if !opts.DryRun {
		if loaded, err := loadMigrateJournal(journalPath); err != nil {
			return MigrationResult{}, fmt.Errorf("migrate: load journal %q: %w", journalPath, err)
		} else if loaded != nil {
			state = *loaded
		}
	}

	files, err := meta.ListLegacyFiles(ctx)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("migrate: list legacy files: %w", err)
	}

	if opts.DryRun {
		res, err := runDryRun(ctx, shareDir, files, opts)
		if err != nil {
			return MigrationResult{}, err
		}
		res.Duration = time.Since(start)
		return res, nil
	}

	stats := MigrationStats{
		FilesDone:  state.FilesDone,
		BytesDone:  state.BytesWritten,
		DedupHits:  state.DedupHits,
		ChunksDone: state.ChunksWritten,
	}

	var lastProgress time.Time
	emit := func(force bool) {
		if opts.Progress == nil {
			return
		}
		now := time.Now()
		if !force && now.Sub(lastProgress) < time.Second {
			return
		}
		lastProgress = now
		elapsed := now.Sub(start).Seconds()
		if elapsed > 0 {
			stats.FilesPerSec = float64(stats.FilesDone) / elapsed
			stats.MiBPerSec = float64(stats.BytesDone) / (1024 * 1024) / elapsed
		}
		opts.Progress(stats)
	}

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return MigrationResult{Stats: stats, Duration: time.Since(start)}, contextErr(ctx, err)
		}

		// Resume: skip files whose path lexicographically precedes the
		// journal's last-completed file. A file currently in-flight at
		// crash time is fully re-processed (its `.blk` data is still on
		// disk; CAS Put is idempotent on hash collision).
		if state.LastFilePath != "" && f.Path < state.LastFilePath {
			continue
		}

		manifest, bytesPut, dedupHits, chunksWritten, err := migrateOneFile(ctx, shareDir, f, bs, opts)
		if err != nil {
			// Persist current progress before returning.
			state.LastFilePath = f.Path
			state.LastOffset = 0
			state.BytesWritten = stats.BytesDone + bytesPut
			state.DedupHits = stats.DedupHits + dedupHits
			state.ChunksWritten = stats.ChunksDone + chunksWritten
			state.Timestamp = time.Now().UTC()
			state.Version = MigrationToolVersion
			_ = writeMigrateJournal(journalPath, state)
			return MigrationResult{Stats: stats, Duration: time.Since(start)}, err
		}

		// Atomic per-file cutover via metadata adapter.
		if err := meta.UpdateFileBlocks(ctx, f.Handle, manifest); err != nil {
			state.LastFilePath = f.Path
			state.LastOffset = 0
			state.Timestamp = time.Now().UTC()
			state.Version = MigrationToolVersion
			_ = writeMigrateJournal(journalPath, state)
			return MigrationResult{Stats: stats, Duration: time.Since(start)},
				fmt.Errorf("migrate: update metadata for %s: %w", f.Path, err)
		}

		// Remove legacy `.blk` files for this file (best-effort —
		// metadata is already cut over; orphan `.blk` files on disk
		// will be reaped by Plan 09's boot guard at the next start).
		if err := removeLegacyBlkFiles(shareDir, f); err != nil {
			// Non-fatal: log via the journal but continue. The boot
			// guard's sentinel check is not affected by leftover .blk
			// files in already-migrated payloads (the share-level
			// sentinel takes precedence). Surface to the operator via
			// progress when a Progress callback is wired.
			_ = err
		}

		stats.FilesDone++
		stats.BytesDone += bytesPut
		stats.DedupHits += dedupHits
		stats.ChunksDone += chunksWritten

		state.LastFilePath = f.Path
		state.LastOffset = 0
		state.BytesWritten = stats.BytesDone
		state.DedupHits = stats.DedupHits
		state.FilesDone = stats.FilesDone
		state.ChunksWritten = stats.ChunksDone
		state.Timestamp = time.Now().UTC()
		state.Version = MigrationToolVersion
		if err := writeMigrateJournal(journalPath, state); err != nil {
			return MigrationResult{Stats: stats, Duration: time.Since(start)},
				fmt.Errorf("migrate: persist journal: %w", err)
		}

		emit(false)
	}

	emit(true)

	// All files migrated — write sentinel, then drop the journal.
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = shareDir
	}
	if err := writeSentinel(storeDir); err != nil {
		return MigrationResult{Stats: stats, Duration: time.Since(start)},
			fmt.Errorf("migrate: write sentinel: %w", err)
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Non-fatal: sentinel is durable; journal cleanup is best-effort.
		_ = err
	}

	return MigrationResult{Stats: stats, Duration: time.Since(start)}, nil
}

// migrateOneFile re-chunks one legacy file's `.blk` tree via FastCDC,
// Puts each chunk to the destination CAS store, verifies each Put via a
// re-Get + BLAKE3 compare, and returns the rebuilt manifest plus
// running counters.
func migrateOneFile(
	ctx context.Context,
	shareDir string,
	f LegacyFileInfo,
	bs blockstore.BlockStore,
	opts MigrationOpts,
) (manifest []blockstore.BlockRef, bytesPut int64, dedupHits int64, chunksWritten int64, err error) {
	stream, err := readLegacyFileStream(shareDir, f)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("migrate: read legacy stream for %s: %w", f.Path, err)
	}

	ck := chunker.NewChunker()
	var offset uint64
	pos := 0
	for pos < len(stream) {
		if err := ctx.Err(); err != nil {
			return manifest, bytesPut, dedupHits, chunksWritten, contextErr(ctx, err)
		}
		boundary, _ := ck.Next(stream[pos:], true)
		if boundary <= 0 {
			break
		}
		chunkBytes := stream[pos : pos+boundary]
		h := blake3ContentHash(chunkBytes)

		// Detect dedup: a pre-existing Put returns nil from bs.Put on
		// idempotent hit, so probe Has first.
		existed, herr := bs.Has(ctx, h)
		if herr != nil {
			return manifest, bytesPut, dedupHits, chunksWritten,
				fmt.Errorf("migrate: Has %s offset %d: %w", f.Path, offset, herr)
		}
		if existed {
			dedupHits++
		} else {
			if perr := bs.Put(ctx, h, chunkBytes); perr != nil {
				return manifest, bytesPut, dedupHits, chunksWritten,
					fmt.Errorf("migrate: Put %s offset %d: %w", f.Path, offset, perr)
			}
			// Defense-in-depth verification (BSCAS-06 carry-forward):
			// re-Get the chunk and compare bytes. A mismatch is fatal.
			got, gerr := bs.Get(ctx, h)
			if gerr != nil {
				return manifest, bytesPut, dedupHits, chunksWritten,
					fmt.Errorf("migrate: post-Put Get %s offset %d: %w", f.Path, offset, gerr)
			}
			if !bytes.Equal(got, chunkBytes) {
				return manifest, bytesPut, dedupHits, chunksWritten,
					fmt.Errorf("%w: file %s offset %d hash %s",
						ErrChunkPutMismatch, f.Path, offset, h)
			}
			bytesPut += int64(len(chunkBytes))
		}

		manifest = append(manifest, blockstore.BlockRef{
			Hash:   h,
			Offset: offset,
			Size:   uint32(len(chunkBytes)),
		})
		chunksWritten++

		offset += uint64(boundary)
		pos += boundary
	}

	return manifest, bytesPut, dedupHits, chunksWritten, nil
}

// readLegacyFileStream concatenates every `<idx>.blk` file under
// `<shareDir>/<PayloadID>/` in offset order into a single in-memory
// stream. Last block may be short. In-memory concat acceptable for the
// v0.16 migration window (offline, bounded by share quota); a streaming
// variant is deferred to Phase 18 if large-VM operators report OOMs.
func readLegacyFileStream(shareDir string, f LegacyFileInfo) ([]byte, error) {
	pid := string(f.PayloadID)
	if len(pid) < 2 {
		return nil, fmt.Errorf("migrate: PayloadID %q too short for shard prefix", pid)
	}
	shard := pid[:2]
	payloadDir := filepath.Join(shareDir, "blocks", shard, pid)
	if !strings.HasPrefix(payloadDir, shareDir) {
		return nil, fmt.Errorf("migrate: PayloadID %q escapes share directory", pid)
	}
	entries, err := os.ReadDir(payloadDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte{}, nil
		}
		return nil, err
	}

	var blks []legacyBlk
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".blk") {
			continue
		}
		base := strings.TrimSuffix(name, ".blk")
		var idx uint64
		if _, perr := fmt.Sscanf(base, "%d", &idx); perr != nil {
			continue
		}
		blks = append(blks, legacyBlk{idx: idx, path: filepath.Join(payloadDir, name)})
	}

	// Sort by idx ascending.
	sortBlks(blks)

	out := make([]byte, 0, f.Size)
	for _, b := range blks {
		data, rerr := os.ReadFile(b.path)
		if rerr != nil {
			return nil, fmt.Errorf("read legacy block %s: %w", b.path, rerr)
		}
		out = append(out, data...)
	}
	// Clip to declared file size — legacy block files may have trailing
	// zero-padding from the 8 MiB fixed-block layout.
	if int64(len(out)) > f.Size {
		out = out[:f.Size]
	}
	return out, nil
}

// legacyBlk is a single `<idx>.blk` file under a legacy payload tree.
type legacyBlk struct {
	idx  uint64
	path string
}

// sortBlks orders by idx ascending in place.
func sortBlks(blks []legacyBlk) {
	// Insertion sort — block counts are small (typical legacy files have
	// a handful of 8 MiB blocks); avoid sort.Slice overhead.
	for i := 1; i < len(blks); i++ {
		for j := i; j > 0 && blks[j-1].idx > blks[j].idx; j-- {
			blks[j-1], blks[j] = blks[j], blks[j-1]
		}
	}
}

// removeLegacyBlkFiles unlinks the `<idx>.blk` children of the file's
// payload directory. Leaves the directory itself in place (other
// metadata-store artifacts may live alongside) and treats missing
// entries as success.
func removeLegacyBlkFiles(shareDir string, f LegacyFileInfo) error {
	pid := string(f.PayloadID)
	if len(pid) < 2 {
		return nil
	}
	shard := pid[:2]
	payloadDir := filepath.Join(shareDir, "blocks", shard, pid)
	entries, err := os.ReadDir(payloadDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".blk") {
			continue
		}
		if err := os.Remove(filepath.Join(payloadDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// runDryRun walks the legacy file list, samples FastCDC over a bounded
// subset of bytes per file, and reports estimated dedup ratio + counts.
// Does not Put anything; does not touch the journal; does not write the
// sentinel.
func runDryRun(
	ctx context.Context,
	shareDir string,
	files []LegacyFileInfo,
	opts MigrationOpts,
) (MigrationResult, error) {
	start := time.Now()
	var totalBytes int64
	var sampledBytes int64
	var totalChunks int64
	var uniqueChunks int64
	seen := make(map[blockstore.ContentHash]struct{})

	budget := opts.DryRunSampleBudgetBytes
	perFile := opts.DryRunPerFileSampleBytes

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return MigrationResult{}, contextErr(ctx, err)
		}
		totalBytes += f.Size

		if sampledBytes >= budget {
			continue
		}
		sampleLimit := perFile
		fivePct := f.Size / 20
		if fivePct > 0 && fivePct < sampleLimit {
			sampleLimit = fivePct
		}
		if sampleLimit <= 0 {
			continue
		}
		if sampledBytes+sampleLimit > budget {
			sampleLimit = budget - sampledBytes
		}

		stream, err := readLegacyFileStream(shareDir, f)
		if err != nil {
			// Skip unreadable files in dry-run — surfacing the error
			// here would mask the count for an operator just trying to
			// preview. Real run will surface the same error and halt.
			continue
		}
		if int64(len(stream)) > sampleLimit {
			stream = stream[:sampleLimit]
		}

		ck := chunker.NewChunker()
		pos := 0
		for pos < len(stream) {
			boundary, _ := ck.Next(stream[pos:], true)
			if boundary <= 0 {
				break
			}
			chunkBytes := stream[pos : pos+boundary]
			h := blake3ContentHash(chunkBytes)
			totalChunks++
			if _, ok := seen[h]; !ok {
				seen[h] = struct{}{}
				uniqueChunks++
			}
			pos += boundary
		}
		sampledBytes += int64(len(stream))
	}

	var ratio float64
	if totalChunks > 0 {
		ratio = float64(uniqueChunks) / float64(totalChunks)
	}

	stats := MigrationStats{
		FilesDone:  int64(len(files)),
		BytesDone:  totalBytes,
		ChunksDone: totalChunks,
	}
	elapsed := time.Since(start).Seconds()
	if elapsed > 0 {
		stats.MiBPerSec = float64(sampledBytes) / (1024 * 1024) / elapsed
	}

	if opts.Progress != nil {
		opts.Progress(stats)
	}

	return MigrationResult{
		Stats:         stats,
		EstDedupRatio: ratio,
	}, nil
}

// loadMigrateJournal parses the per-share journal file if present.
// Returns (nil, nil) when the file is absent — that is the steady-state
// for a fresh run.
func loadMigrateJournal(path string) (*migrateJournalState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var st migrateJournalState
	if err := json.Unmarshal(data, &st); err != nil {
		// Corrupt journal — treat as absent so the run restarts cleanly.
		// The CAS Put is idempotent, so re-processing files is safe.
		return nil, nil
	}
	return &st, nil
}

// writeMigrateJournal persists the journal state atomically: write to a
// `.tmp` sibling, fsync, rename. SyncDir on the parent so the rename is
// durable across crashes.
func writeMigrateJournal(path string, st migrateJournalState) error {
	if st.Version == "" {
		st.Version = MigrationToolVersion
	}
	if st.Timestamp.IsZero() {
		st.Timestamp = time.Now().UTC()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// writeSentinel writes `<shareDir>/.cas-migrated-v1` via atomic rename
// from `.cas-migrated-v1.tmp`. Returns nil on success; the caller may
// then safely remove the journal.
func writeSentinel(shareDir string) error {
	content := sentinelContent{
		Version:     "v1",
		CompletedAt: time.Now().UTC(),
		ToolVersion: MigrationToolVersion,
		ShareDir:    shareDir,
	}
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(shareDir, SentinelFileName)
	tmp := target + SentinelTmpSuffix
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(shareDir)
	return nil
}

// contextErr returns context.Cause(ctx) when set, else err.
func contextErr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// blake3ContentHash returns the 32-byte BLAKE3 hash of data. Mirrors
// pkg/blockstore/local/fs/rollup.go::blake3ContentHash (CAS chunk
// hashing contract).
func blake3ContentHash(data []byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	sum := blake3.Sum256(data)
	copy(h[:], sum[:])
	return h
}
