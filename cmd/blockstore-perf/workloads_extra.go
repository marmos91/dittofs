package main

import (
	"context"
	"fmt"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedChunks Puts cfg.ops unique CAS chunks into local, returning their
// hashes for downstream Walk/Delete/GC reference shaping.
func seedChunks(ctx context.Context, local *fs.FSStore, cfg config) ([]blockstore.ContentHash, error) {
	hashes := make([]blockstore.ContentHash, cfg.ops)
	data := make([]byte, cfg.blockSize)
	for i := 0; i < cfg.ops; i++ {
		// Per-iteration prefix makes every chunk unique even when
		// blockSize is small (no synthetic randomness needed — the
		// hash is content-derived).
		copy(data, fmt.Sprintf("perf-seed-%016x", uint64(i)))
		h := blockstore.ContentHash(blake3.Sum256(data))
		if err := local.Put(ctx, h, data); err != nil {
			return nil, fmt.Errorf("seed put %d: %w", i, err)
		}
		hashes[i] = h
	}
	return hashes, nil
}

// gcReconciler is a single-share MultiShareReconciler over a memory
// metadata store, matching the production wiring shape used by Runtime
// when a remote points at exactly one share.
type gcReconciler struct {
	share string
	store metadata.MetadataStore
}

func (g *gcReconciler) GetMetadataStoreForShare(name string) (metadata.MetadataStore, error) {
	if name != g.share {
		return nil, fmt.Errorf("unknown share %q", name)
	}
	return g.store, nil
}

func (g *gcReconciler) SharesForGC() []string { return []string{g.share} }

// prepareGC seeds (ops) CAS objects on the remote, references
// ceil(ops * (1-garbageRatio)) of them via FileBlock rows on a memory
// metadata store, and returns a timed step that runs one CollectGarbage
// pass per op.
func prepareGC(ctx context.Context, remoteStore remote.RemoteStore, ms metadata.MetadataStore, cfg config) (func(i int) (int, error), error) {
	if cfg.gcGarbageRatio < 0 || cfg.gcGarbageRatio > 1 {
		return nil, fmt.Errorf("--gc-garbage-ratio must be in [0,1], got %f", cfg.gcGarbageRatio)
	}
	totalRefs := int(float64(cfg.ops) * (1 - cfg.gcGarbageRatio))
	// For the in-process memory remote, backdate Put-stamped LastModified
	// so freshly-seeded objects fall outside the GC grace window. Real
	// S3 timestamps are old enough that the default grace is fine.
	if mem, ok := remoteStore.(*remotememory.Store); ok {
		backdate := time.Now().Add(-2 * time.Hour)
		mem.SetNowFnForTest(func() time.Time { return backdate })
	}
	data := make([]byte, cfg.blockSize)
	for i := 0; i < cfg.ops; i++ {
		copy(data, fmt.Sprintf("perf-gc-%016x", uint64(i)))
		h := blockstore.ContentHash(blake3.Sum256(data))
		if err := remoteStore.Put(ctx, h, data); err != nil {
			return nil, fmt.Errorf("seed remote %d: %w", i, err)
		}
		if i < totalRefs {
			if err := ms.Put(ctx, &blockstore.FileBlock{
				ID:            fmt.Sprintf("perf-gc/%d", i),
				Hash:          h,
				State:         blockstore.BlockStateRemote,
				BlockStoreKey: blockstore.FormatCASKey(h),
				DataSize:      uint32(len(data)),
				RefCount:      1,
				LastAccess:    time.Now(),
				CreatedAt:     time.Now(),
			}); err != nil {
				return nil, fmt.Errorf("seed metadata %d: %w", i, err)
			}
		}
	}
	rec := &gcReconciler{share: "perf-gc", store: ms}
	// One CollectGarbage call per timed op so the step closure
	// matches runLoop's per-call accounting; ops=1 is the canonical
	// usage but --ops>1 lets a caller average over multiple sweeps.
	return func(_ int) (int, error) {
		stats := engine.CollectGarbage(ctx, remoteStore, rec, &engine.Options{
			// 1h grace; in-memory remote backdates LastModified by 2h
			// at seed so synthetic objects fall outside the window.
			GracePeriod: time.Hour,
		})
		if stats.ErrorCount > 0 {
			return 0, fmt.Errorf("gc errors=%d first=%v", stats.ErrorCount, stats.FirstErrors)
		}
		return 0, nil
	}, nil
}

// stepRawS3Put writes one unique CAS chunk per op directly via
// remoteStore.Put, bypassing the engine. Used as a baseline for
// raw-S3 throughput vs the full --remote=s3 write path.
func stepRawS3Put(ctx context.Context, remoteStore remote.RemoteStore, cfg config) func(i int) (int, error) {
	buf := make([]byte, cfg.blockSize)
	return func(i int) (int, error) {
		copy(buf, fmt.Sprintf("perf-raws3-%016x", uint64(i)))
		h := blockstore.ContentHash(blake3.Sum256(buf))
		return len(buf), remoteStore.Put(ctx, h, buf)
	}
}

// runLocalOnly wires a bare FSStore (no engine / no syncer) for the
// walk + delete workloads, which exercise local CAS enumeration and
// chunk removal without the engine's write-path overhead.
func runLocalOnly(cfg config, tmpDir string) error {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(tmpDir, 0, memBudget, ms, fs.FSStoreOptions{
		MaxLogBytes:     logBudget,
		RollupWorkers:   1,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	defer func() { _ = local.Close() }()

	ctx := context.Background()
	hashes, err := seedChunks(ctx, local, cfg)
	if err != nil {
		return err
	}
	return timedRun(cfg, cfg.workload, func() (int64, int64, error) {
		var visited int64
		switch cfg.workload {
		case "walk":
			err := local.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
				visited++
				return nil
			})
			return visited, 0, err
		case "delete":
			for _, h := range hashes {
				if err := local.Delete(ctx, h); err != nil {
					return visited, 0, err
				}
				visited++
			}
			return visited, 0, nil
		default:
			return 0, 0, fmt.Errorf("runLocalOnly: unsupported workload %q", cfg.workload)
		}
	})
}

// runGC seeds remote CAS objects + a referenced subset on a memory
// metadata store, then times engine.CollectGarbage(...) calls.
func runGC(cfg config, remoteStore remote.RemoteStore) error {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	step, err := prepareGC(ctx, remoteStore, ms, cfg)
	if err != nil {
		return err
	}
	return timedRun(cfg, "gc", func() (int64, int64, error) {
		for i := 0; i < cfg.ops; i++ {
			if _, err := step(i); err != nil {
				return int64(i), 0, err
			}
		}
		return int64(cfg.ops), 0, nil
	})
}

// runRawS3 bypasses the engine and writes directly to the remote
// store. Provides a raw-S3 baseline against the full --remote=s3
// write path. Only valid when --remote=s3.
func runRawS3(cfg config, remoteStore remote.RemoteStore) error {
	if cfg.remote != "s3" {
		return fmt.Errorf("raw-s3-put requires --remote=s3")
	}
	ctx := context.Background()
	step := stepRawS3Put(ctx, remoteStore, cfg)
	return timedRun(cfg, "raw-s3-put", func() (int64, int64, error) {
		bytes, err := runLoop(cfg, step)
		return int64(cfg.ops), bytes, err
	})
}
