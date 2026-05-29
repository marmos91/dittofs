package snapshots

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

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

	// BlocksPerFile is the number of BlockRef entries per file. Total
	// referenced blocks = Files * BlocksPerFile; with Dedup=1 every block
	// hash is unique so the returned HashSet has Files*BlocksPerFile
	// entries — the worst case for create-path RAM.
	BlocksPerFile int

	// BlockSize is the byte size recorded on each BlockRef (drives the
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
// opts.BlocksPerFile BlockRefs. Returns the store, the number of unique
// block hashes seeded, and a cleanup func.
//
// Files are attached directly under the share root via PutFile + SetParent
// + SetChild — the same surface the metadata conformance suite uses — so
// the seed exercises the real Backup hash-extraction path (File.Blocks on
// the f: prefix) without routing through CreateFile permission checks.
func NewStore(ctx context.Context, opts SeedOpts) (metadata.MetadataStore, int, func(), error) {
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

func newEngine(ctx context.Context, opts SeedOpts) (metadata.MetadataStore, func(), error) {
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

func seed(ctx context.Context, store metadata.MetadataStore, opts SeedOpts) (int, error) {
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

		refs := make([]blockstore.BlockRef, blocksPerFile)
		for b := 0; b < blocksPerFile; b++ {
			// One distinct hash per (dedup-bucketed) block. blockSeq/dedup
			// collapses every dedup-th block onto a shared hash.
			h := hashFromSeq(blockSeq / uint64(dedup))
			refs[b] = blockstore.BlockRef{
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
func hashFromSeq(seq uint64) blockstore.ContentHash {
	var h blockstore.ContentHash
	binary.BigEndian.PutUint64(h[:8], seq)
	return h
}
