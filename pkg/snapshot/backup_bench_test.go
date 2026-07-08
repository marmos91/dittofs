package snapshot_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// ---------------------------------------------------------------------------
// Harness API (was bench/snapshots/workloads.go)
// ---------------------------------------------------------------------------

// VerifyConcurrency mirrors the hardcoded production verify fan-out
// (runtime/snapshot.go). The benchmarks probe at the same width so the
// measured per-block latency budget transfers directly.
const VerifyConcurrency = 16

// BackupResult reports the cost of one Backup pass.
type BackupResult struct {
	// DumpBytes is the number of bytes streamed to the dump writer.
	DumpBytes int64

	// ManifestHashes is the number of unique block hashes the engine
	// returned in the HashSet (= manifest line count).
	ManifestHashes int

	// HashSet is the engine-returned set, reused by the manifest + verify
	// workloads so a benchmark can chain them without re-running Backup.
	HashSet *block.HashSet
}

// countingDiscard counts bytes written to it and drops them. Used so the
// dump stream is fully serialized (exercising the engine's per-KV write
// path) without allocating an O(dump) buffer — the dump is meant to be
// streamed, and the benchmark's RAM ceiling should reflect that.
type countingDiscard struct{ n int64 }

func (c *countingDiscard) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// RunBackup runs a metadata Backup against a counting discard writer and
// returns the dump size + the returned HashSet. The metadata engine must
// implement Snapshotable (memory + badger both do). The harness itself does
// not buffer the dump — the writer discards. Whether the dump is resident
// depends on the engine: badger streams KV-by-KV (only the returned
// HashSet is resident, the quantity the create-path ceiling is built
// around), whereas the memory engine gob-encodes its whole snapshot into
// one internal buffer before writing (so its B/op reflects that buffer,
// not a stream).
func RunBackup(ctx context.Context, store metadata.Store) (BackupResult, error) {
	snapshotable, ok := store.(metadata.Snapshotable)
	if !ok {
		return BackupResult{}, fmt.Errorf("snapshots bench: store is not Snapshotable")
	}
	cd := &countingDiscard{}
	hs, err := snapshotable.WriteSnapshot(ctx, cd)
	if err != nil {
		return BackupResult{}, fmt.Errorf("snapshots bench: backup: %w", err)
	}
	count := 0
	if hs != nil {
		count = hs.Len()
	} else {
		hs = block.NewHashSet(0)
	}
	return BackupResult{DumpBytes: cd.n, ManifestHashes: count, HashSet: hs}, nil
}

// RunWriteManifest serializes hs to a counting discard writer and returns
// the manifest's on-disk byte size. WriteManifest streams sorted hex lines,
// so this measures the sort + encode cost, not a buffer allocation.
func RunWriteManifest(hs *block.HashSet) (int64, error) {
	cd := &countingDiscard{}
	if err := snapshot.WriteManifest(cd, hs); err != nil {
		return 0, fmt.Errorf("snapshots bench: write manifest: %w", err)
	}
	return cd.n, nil
}

// benchLocators maps every hash to a distinct packed-block locator so
// RunVerify's block-only durability probe (#1493) resolves each hash to a
// blocks/<id> object seeded by SeedRemote.
type benchLocators map[block.ContentHash]block.ChunkLocator

func (b benchLocators) GetLocator(_ context.Context, h block.ContentHash) (block.ChunkLocator, bool, error) {
	loc, ok := b[h]
	return loc, ok, nil
}

// blockIDForHash derives a deterministic block ID from a content hash so
// SeedRemote and RunVerify agree on the packed-block key.
func blockIDForHash(h block.ContentHash) string { return "bench-" + h.String() }

// SeedRemote PUTs a one-byte packed block for every hash in hs into rs so a
// subsequent RunVerify finds every block-durability probe present (the
// all-durable happy path, which is also the most expensive: it never
// short-circuits on a miss). Returns the number of objects seeded.
func SeedRemote(ctx context.Context, rs remote.RemoteStore, hs *block.HashSet) (int, error) {
	n := 0
	err := hs.ForEach(func(h block.ContentHash) error {
		if err := rs.PutBlock(ctx, blockIDForHash(h), bytes.NewReader([]byte{0x01})); err != nil {
			return fmt.Errorf("snapshots bench: seed remote put block: %w", err)
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunVerify block-probes every hash in hs against rs at VerifyConcurrency
// and returns the number of hashes probed. The caller times the call. With
// an all-present remote (see SeedRemote) every probe runs, giving the
// worst-case verify wall time for the manifest size.
func RunVerify(ctx context.Context, rs remote.RemoteStore, hs *block.HashSet) (int, error) {
	locators := make(benchLocators, hs.Len())
	_ = hs.ForEach(func(h block.ContentHash) error {
		locators[h] = block.ChunkLocator{BlockID: blockIDForHash(h)}
		return nil
	})
	if err := snapshot.VerifyRemoteDurability(ctx, locators, rs, hs, VerifyConcurrency); err != nil {
		return 0, fmt.Errorf("snapshots bench: verify: %w", err)
	}
	return hs.Len(), nil
}

// SerializeManifest renders hs to its on-disk byte form via the production
// WriteManifest encoder. Benchmarks call it once, outside the timed loop,
// then feed the bytes to RunReadManifest so only the parse is measured.
func SerializeManifest(hs *block.HashSet) ([]byte, error) {
	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, hs); err != nil {
		return nil, fmt.Errorf("snapshots bench: serialize manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// RunReadManifest parses a serialized manifest (see SerializeManifest) back
// into a HashSet, measuring the ReadManifest cost — the dominant allocation
// on the restore pre-verify path, where the parsed set is resident in RAM.
// Returns the parsed hash count.
func RunReadManifest(manifest []byte) (int, error) {
	parsed, err := snapshot.ReadManifest(bytes.NewReader(manifest))
	if err != nil {
		return 0, fmt.Errorf("snapshots bench: read manifest: %w", err)
	}
	return parsed.Len(), nil
}

// ---------------------------------------------------------------------------
// Fixture / seeding (was bench/snapshots/fixture.go)
// ---------------------------------------------------------------------------

// Engine selects which metadata backend a workload seeds and backs up.
const (
	EngineMemory = "memory"
	EngineBadger = "badger"
)

// shareName is the single share every workload seeds under.
const shareName = "/bench"

// SeedOpts parameterizes the synthetic share a workload builds before
// timing Backup / manifest / verify.
type SeedOpts struct {
	// Engine is EngineMemory or EngineBadger.
	Engine string

	// Files is the number of regular files to seed (e.g. 1e5, 1e6).
	Files int

	// BlocksPerFile is the number of ChunkRef entries per file. Total
	// referenced blocks = Files * BlocksPerFile; with Dedup=1 every block
	// hash is unique so the returned HashSet has Files*BlocksPerFile
	// entries — the worst case for create-path RAM.
	BlocksPerFile int

	// BlockSize is the byte size recorded on each ChunkRef (drives the
	// reported logical-bytes metric only; no real bytes are written).
	BlockSize uint32

	// Dedup, when > 1, makes every Dedup-th block share a hash so the
	// unique-hash count (and thus HashSet + manifest size) shrinks by
	// roughly that factor. Dedup<=1 means all-unique.
	Dedup int

	// DBPath is the on-disk directory for the badger engine. Ignored for
	// memory. Must be a fresh empty dir per run.
	DBPath string
}

// NewStore constructs the metadata engine named by opts.Engine and seeds
// it with opts.Files synthetic regular files, each carrying
// opts.BlocksPerFile ChunkRefs. Returns the store, the number of unique
// block hashes seeded, and a cleanup func.
//
// Files are attached directly under the share root via PutFile + SetParent
// + SetChild — the same surface the metadata conformance suite uses — so
// the seed exercises the real Backup hash-extraction path (File.Blocks on
// the f: prefix) without routing through CreateFile permission checks.
func NewStore(ctx context.Context, opts SeedOpts) (metadata.Store, int, func(), error) {
	store, cleanup, err := newEngine(ctx, opts)
	if err != nil {
		return nil, 0, nil, err
	}

	uniqueHashes, err := seed(ctx, store, opts)
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return store, uniqueHashes, cleanup, nil
}

func newEngine(ctx context.Context, opts SeedOpts) (metadata.Store, func(), error) {
	switch opts.Engine {
	case "", EngineMemory:
		s := metadatamemory.NewMemoryMetadataStoreWithDefaults()
		return s, func() {}, nil
	case EngineBadger:
		if opts.DBPath == "" {
			return nil, nil, fmt.Errorf("snapshots bench: badger engine needs a DBPath")
		}
		s, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, opts.DBPath)
		if err != nil {
			return nil, nil, fmt.Errorf("snapshots bench: open badger: %w", err)
		}
		return s, func() { _ = s.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("snapshots bench: unknown engine %q (want %q or %q)",
			opts.Engine, EngineMemory, EngineBadger)
	}
}

func seed(ctx context.Context, store metadata.Store, opts SeedOpts) (int, error) {
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		return 0, fmt.Errorf("snapshots bench: create share: %w", err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		return 0, fmt.Errorf("snapshots bench: create root: %w", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return 0, fmt.Errorf("snapshots bench: encode root handle: %w", err)
	}

	blocksPerFile := opts.BlocksPerFile
	if blocksPerFile < 1 {
		blocksPerFile = 1
	}
	dedup := opts.Dedup
	if dedup < 1 {
		dedup = 1
	}
	blockSize := opts.BlockSize
	if blockSize == 0 {
		blockSize = 1 << 20
	}

	var blockSeq uint64

	for i := 0; i < opts.Files; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		name := fmt.Sprintf("f%08d.bin", i)
		path := "/" + name

		handle, err := store.GenerateHandle(ctx, shareName, path)
		if err != nil {
			return 0, fmt.Errorf("snapshots bench: generate handle: %w", err)
		}
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			return 0, fmt.Errorf("snapshots bench: decode handle: %w", err)
		}

		refs := make([]block.ChunkRef, blocksPerFile)
		for b := 0; b < blocksPerFile; b++ {
			// One distinct hash per (dedup-bucketed) block. blockSeq/dedup
			// collapses every dedup-th block onto a shared hash.
			h := hashFromSeq(blockSeq / uint64(dedup))
			refs[b] = block.ChunkRef{
				Hash:   h,
				Offset: uint64(b) * uint64(blockSize),
				Size:   blockSize,
			}
			blockSeq++
		}

		f := &metadata.File{
			ShareName: shareName,
			Path:      path,
			FileAttr: metadata.FileAttr{
				Type:   metadata.FileTypeRegular,
				Mode:   0o644,
				Nlink:  1,
				Size:   uint64(blocksPerFile) * uint64(blockSize),
				Blocks: refs,
			},
		}
		f.ID = id
		if err := store.PutFile(ctx, f); err != nil {
			return 0, fmt.Errorf("snapshots bench: put file: %w", err)
		}
		if err := store.SetParent(ctx, handle, rootHandle); err != nil {
			return 0, fmt.Errorf("snapshots bench: set parent: %w", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
			return 0, fmt.Errorf("snapshots bench: set child: %w", err)
		}
	}

	// Unique-hash count is derived, not tracked: hashes are
	// hashFromSeq(blockSeq/dedup) for blockSeq in [0, totalRefs), so the
	// distinct count is ceil(totalRefs/dedup). Computing it avoids a
	// multi-GB scratch map at the 1e6×8 scale that would otherwise pollute
	// the benchmark's reported memory ceiling.
	totalRefs := uint64(opts.Files) * uint64(blocksPerFile)
	unique := int((totalRefs + uint64(dedup) - 1) / uint64(dedup))
	return unique, nil
}

// hashFromSeq derives a deterministic unique ContentHash from a sequence
// number. The first 8 bytes carry the big-endian counter; the rest stay
// zero. Distinct counters yield distinct hashes, which is all the snapshot
// pipeline (set membership + sort + HEAD-probe by key) depends on.
func hashFromSeq(seq uint64) block.ContentHash {
	var h block.ContentHash
	binary.BigEndian.PutUint64(h[:8], seq)
	return h
}

// ---------------------------------------------------------------------------
// Benchmarks (was bench/snapshots/workloads_test.go)
// ---------------------------------------------------------------------------

// scaleCase is one (files, blocks-per-file) point on the sweep.
type scaleCase struct {
	name          string
	files         int
	blocksPerFile int
	// large gates the case behind -short: 1e6-file points allocate
	// hundreds of MB and take seconds, too heavy for the default CI run.
	large bool
}

// scales is the shared sweep used by every benchmark below. The small
// points run in CI; the large (1e6) points run only without -short.
var scales = []scaleCase{
	{name: "1e4files_1blk", files: 1e4, blocksPerFile: 1},
	{name: "1e5files_1blk", files: 1e5, blocksPerFile: 1},
	{name: "1e5files_8blk", files: 1e5, blocksPerFile: 8},
	{name: "1e6files_1blk", files: 1e6, blocksPerFile: 1, large: true},
	{name: "1e6files_8blk", files: 1e6, blocksPerFile: 8, large: true},
}

func seedOpts(b *testing.B, sc scaleCase) SeedOpts {
	b.Helper()
	return SeedOpts{
		Engine:        EngineMemory,
		Files:         sc.files,
		BlocksPerFile: sc.blocksPerFile,
		BlockSize:     1 << 20, // 1 MiB logical blocks
		Dedup:         1,       // all-unique: worst case for HashSet + manifest
	}
}

// BenchmarkBackup measures the metadata Backup cost (streamed dump +
// resident HashSet) per scale. Custom metrics: dump bytes and manifest
// hash count, so dump-size and HashSet growth read directly off the row.
func BenchmarkBackup(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			store, _, cleanup, err := NewStore(ctx, seedOpts(b, sc))
			if err != nil {
				b.Fatalf("seed: %v", err)
			}
			defer cleanup()

			var last BackupResult
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				last, err = RunBackup(ctx, store)
				if err != nil {
					b.Fatalf("backup: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(last.DumpBytes), "dump_bytes")
			b.ReportMetric(float64(last.ManifestHashes), "manifest_hashes")
		})
	}
}

// BenchmarkWriteManifest measures WriteManifest (sort + hex encode, fully
// streamed). Custom metric: manifest_bytes (on-disk manifest size).
func BenchmarkWriteManifest(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)

			var bytesOut int64
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				bytesOut, err = RunWriteManifest(hs)
				if err != nil {
					b.Fatalf("write manifest: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(bytesOut), "manifest_bytes")
		})
	}
}

// BenchmarkReadManifest measures ReadManifest (parse into a resident
// HashSet — the restore pre-verify allocation).
func BenchmarkReadManifest(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)
			manifest, err := SerializeManifest(hs)
			if err != nil {
				b.Fatalf("serialize manifest: %v", err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := RunReadManifest(manifest); err != nil {
					b.Fatalf("read manifest: %v", err)
				}
			}
		})
	}
}

// BenchmarkVerify measures VerifyRemoteDurability against an all-present
// in-memory remote at the production concurrency (16). This is the
// HEAD-probe cost; the in-memory remote has no network latency, so the
// number is a floor — multiply by real per-HEAD RTT for an S3 budget.
// Custom metric: probes (= hashes HEAD-probed).
func BenchmarkVerify(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)
			rs := remotememory.New()
			defer func() { _ = rs.Close() }()
			if _, err := SeedRemote(ctx, rs, hs); err != nil {
				b.Fatalf("seed remote: %v", err)
			}

			var probes int
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				probes, err = RunVerify(ctx, rs, hs)
				if err != nil {
					b.Fatalf("verify: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(probes), "probes")
		})
	}
}

// buildHashSet seeds a store, runs one Backup, and returns the resulting
// HashSet for the manifest / verify benchmarks (which time a stage
// downstream of Backup and so need its output, not the timed Backup
// itself).
func buildHashSet(b *testing.B, ctx context.Context, sc scaleCase) *block.HashSet {
	b.Helper()
	store, _, cleanup, err := NewStore(ctx, seedOpts(b, sc))
	if err != nil {
		b.Fatalf("seed: %v", err)
	}
	defer cleanup()
	res, err := RunBackup(ctx, store)
	if err != nil {
		b.Fatalf("backup: %v", err)
	}
	return res.HashSet
}

// BenchmarkBackupBadger keeps the on-disk badger engine exercised (the
// sweep above uses the memory engine). Badger streams the dump KV-by-KV, so
// this is the streamed-create reference point.
func BenchmarkBackupBadger(b *testing.B) {
	if testing.Short() {
		b.Skip("badger backup skipped under -short")
	}
	ctx := context.Background()
	opts := SeedOpts{
		Engine:        EngineBadger,
		Files:         1e4,
		BlocksPerFile: 4,
		BlockSize:     1 << 20,
		Dedup:         1,
		DBPath:        b.TempDir(),
	}
	store, _, cleanup, err := NewStore(ctx, opts)
	if err != nil {
		b.Fatalf("seed badger: %v", err)
	}
	defer cleanup()

	var last BackupResult
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		last, err = RunBackup(ctx, store)
		if err != nil {
			b.Fatalf("backup: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(last.DumpBytes), "dump_bytes")
	b.ReportMetric(float64(last.ManifestHashes), "manifest_hashes")
}
