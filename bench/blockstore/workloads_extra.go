package blockstore

import (
	"context"
	"fmt"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Extra workloads that do not fit the engine.Store + RunWorkload
// shape: walk / delete run against a bare local FSStore (no engine,
// no syncer); gc drives engine.CollectGarbage; raw-s3-put bypasses
// the engine and writes directly to a remote.RemoteStore. Each has
// its own RunX entry point and is dispatched from cmd/bench.
const (
	WorkloadWalk     = "walk"
	WorkloadDelete   = "delete"
	WorkloadGC       = "gc"
	WorkloadRawS3Put = "raw-s3-put"
	DefaultGCGarbage = 0.3
)

// NewLocalStore wires a bare FSStore (no engine, no syncer) for the
// walk + delete workloads, which exercise local CAS enumeration and
// chunk removal without engine overhead.
func NewLocalStore(baseDir string) (*fs.FSStore, func(), error) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(baseDir, 0, MemBudget, ms, fs.FSStoreOptions{
		MaxLogBytes:     LogBudget,
		RollupWorkers:   1,
		StabilizationMS: StabilizationMS,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	return local, func() { _ = local.Close() }, nil
}

// SeedLocalChunks Puts opts.Ops unique CAS chunks into local and
// returns their hashes. Used by RunWalk / RunDelete to set up a
// known working set before the timed region.
func SeedLocalChunks(ctx context.Context, local *fs.FSStore, opts Opts) ([]block.ContentHash, error) {
	hashes := make([]block.ContentHash, opts.Ops)
	data := make([]byte, opts.BlockSize)
	for i := 0; i < opts.Ops; i++ {
		// Per-iteration prefix makes every chunk unique even when
		// BlockSize is small — the hash is content-derived.
		copy(data, fmt.Sprintf("perf-seed-%016x", uint64(i)))
		h := block.ContentHash(blake3.Sum256(data))
		if err := local.Put(ctx, h, data); err != nil {
			return nil, fmt.Errorf("seed put %d: %w", i, err)
		}
		hashes[i] = h
	}
	return hashes, nil
}

// RunWalk seeds opts.Ops chunks and times a single Walk pass.
// Returns the count of visited chunks (ops) and 0 bytes.
func RunWalk(ctx context.Context, local *fs.FSStore, opts Opts) (Result, error) {
	if _, err := SeedLocalChunks(ctx, local, opts); err != nil {
		return Result{}, err
	}
	lat := NewLatencyRecorder(opts.Ops)
	start := time.Now()
	var visited int64
	if err := local.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error {
		opStart := time.Now()
		visited++
		lat.Record(time.Since(opStart), true)
		return nil
	}); err != nil {
		return Result{}, err
	}
	return Result{Duration: time.Since(start), Ops: int(visited), Latency: lat}, nil
}

// RunDelete seeds opts.Ops chunks and times deleting them all.
func RunDelete(ctx context.Context, local *fs.FSStore, opts Opts) (Result, error) {
	hashes, err := SeedLocalChunks(ctx, local, opts)
	if err != nil {
		return Result{}, err
	}
	lat := NewLatencyRecorder(len(hashes))
	start := time.Now()
	for i, h := range hashes {
		opStart := time.Now()
		if err := local.Delete(ctx, h); err != nil {
			lat.Record(time.Since(opStart), false)
			return Result{}, fmt.Errorf("delete %d: %w", i, err)
		}
		lat.Record(time.Since(opStart), true)
	}
	return Result{Duration: time.Since(start), Ops: len(hashes), Latency: lat}, nil
}

// gcReconciler is a single-share MultiShareReconciler over a memory
// metadata store, mirroring the production wiring used by Runtime
// when a remote points at exactly one share.
type gcReconciler struct {
	share string
	store metadata.Store
}

func (g *gcReconciler) GetMetadataStoreForShare(name string) (metadata.Store, error) {
	if name != g.share {
		return nil, fmt.Errorf("unknown share %q", name)
	}
	return g.store, nil
}

func (g *gcReconciler) SharesForGC() []string { return []string{g.share} }

// RunGC seeds opts.Ops CAS objects on remoteStore, references
// ceil(ops * (1-garbageRatio)) of them via FileBlock rows on a
// memory metadata store, then times opts.Ops CollectGarbage passes.
func RunGC(ctx context.Context, remoteStore remote.RemoteStore, opts Opts, garbageRatio float64) (Result, error) {
	if garbageRatio < 0 || garbageRatio > 1 {
		return Result{}, fmt.Errorf("gc-garbage-ratio must be in [0,1], got %f", garbageRatio)
	}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	totalRefs := int(float64(opts.Ops) * (1 - garbageRatio))
	// For the in-process memory remote, backdate Put-stamped
	// LastModified so freshly-seeded objects fall outside the GC
	// grace window. Real S3 timestamps are old enough that the
	// default grace is fine.
	if mem, ok := remoteStore.(*remotememory.Store); ok {
		backdate := time.Now().Add(-2 * time.Hour)
		mem.SetNowFnForTest(func() time.Time { return backdate })
	}
	data := make([]byte, opts.BlockSize)
	for i := 0; i < opts.Ops; i++ {
		copy(data, fmt.Sprintf("perf-gc-%016x", uint64(i)))
		h := block.ContentHash(blake3.Sum256(data))
		if err := remoteStore.Put(ctx, h, data); err != nil {
			return Result{}, fmt.Errorf("seed remote %d: %w", i, err)
		}
		if i < totalRefs {
			if err := ms.Put(ctx, &block.FileBlock{
				ID:            fmt.Sprintf("perf-gc/%d", i),
				Hash:          h,
				State:         block.BlockStateRemote,
				BlockStoreKey: block.FormatCASKey(h),
				DataSize:      uint32(len(data)),
				RefCount:      1,
				LastAccess:    time.Now(),
				CreatedAt:     time.Now(),
			}); err != nil {
				return Result{}, fmt.Errorf("seed metadata %d: %w", i, err)
			}
		}
	}
	rec := &gcReconciler{share: "perf-gc", store: ms}
	lat := NewLatencyRecorder(opts.Ops)
	start := time.Now()
	for i := 0; i < opts.Ops; i++ {
		opStart := time.Now()
		stats := engine.CollectGarbage(ctx, remoteStore, rec, &engine.Options{
			// 1 h grace; in-memory remote backdates LastModified by 2 h
			// at seed so synthetic objects fall outside the window.
			GracePeriod: time.Hour,
		})
		if stats.ErrorCount > 0 {
			lat.Record(time.Since(opStart), false)
			return Result{}, fmt.Errorf("gc op=%d errors=%d first=%v", i, stats.ErrorCount, stats.FirstErrors)
		}
		lat.Record(time.Since(opStart), true)
	}
	return Result{Duration: time.Since(start), Ops: opts.Ops, Latency: lat}, nil
}

// RunRawS3Put writes opts.Ops unique CAS chunks directly via
// remoteStore.Put, bypassing the engine. Used as a baseline for
// raw-S3 throughput vs the full engine write path.
func RunRawS3Put(ctx context.Context, remoteStore remote.RemoteStore, opts Opts) (Result, error) {
	if opts.Remote != RemoteS3 {
		return Result{}, fmt.Errorf("raw-s3-put requires remote=s3")
	}
	buf := make([]byte, opts.BlockSize)
	lat := NewLatencyRecorder(opts.Ops)
	start := time.Now()
	var total int64
	for i := 0; i < opts.Ops; i++ {
		copy(buf, fmt.Sprintf("perf-raws3-%016x", uint64(i)))
		h := block.ContentHash(blake3.Sum256(buf))
		opStart := time.Now()
		if err := remoteStore.Put(ctx, h, buf); err != nil {
			lat.Record(time.Since(opStart), false)
			return Result{}, fmt.Errorf("put %d: %w", i, err)
		}
		lat.Record(time.Since(opStart), true)
		total += int64(len(buf))
	}
	return Result{Duration: time.Since(start), Ops: opts.Ops, Bytes: total, Latency: lat}, nil
}
