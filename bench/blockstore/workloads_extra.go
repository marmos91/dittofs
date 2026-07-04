package blockstore

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Extra workloads that do not fit the engine.Store + RunWorkload
// shape: walk / delete run against a bare local FSStore (no engine,
// no syncer); raw-s3-put bypasses the engine and writes directly to a
// remote.RemoteStore. Each has its own RunX entry point and is
// dispatched from cmd/bench.
const (
	WorkloadWalk     = "walk"
	WorkloadDelete   = "delete"
	WorkloadRawS3Put = "raw-s3-put"
)

// NewLocalStore wires a bare FSStore (no engine, no syncer) for the
// walk + delete workloads, which exercise local CAS enumeration and
// chunk removal without engine overhead.
func NewLocalStore(baseDir string) (*fs.FSStore, func(), error) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(baseDir, 0, ms, fs.FSStoreOptions{
		LocalChunkIndex: ms,
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

// RunRawS3Put writes opts.Ops unique packed block objects directly via
// remoteStore.PutBlock, bypassing the engine. Used as a baseline for
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
		if err := remoteStore.PutBlock(ctx, h.String(), bytes.NewReader(buf)); err != nil {
			lat.Record(time.Since(opStart), false)
			return Result{}, fmt.Errorf("put %d: %w", i, err)
		}
		lat.Record(time.Since(opStart), true)
		total += int64(len(buf))
	}
	return Result{Duration: time.Since(start), Ops: opts.Ops, Bytes: total, Latency: lat}, nil
}
