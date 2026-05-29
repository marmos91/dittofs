package blockstore

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Engine wiring constants. Sized for short-lived benchmark runs:
// 256 MiB rollup log budget, 64 MiB resident memory, 2 rollup workers.
const (
	LogBudget       = 256 * 1024 * 1024
	MemBudget       = 64 * 1024 * 1024
	RollupWorkers   = 2
	StabilizationMS = 5
)

// NewEngine wires production-equivalent FSStore + remote + memory
// metadata + Syncer for a single benchmark run. baseDir is the local
// fs root; remoteStore is the upload target (memory or s3). The
// returned cleanup closes the engine, which also closes the remote.
func NewEngine(baseDir string, remoteStore remote.RemoteStore) (*engine.Store, func(), error) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(baseDir, 0, MemBudget, ms, fs.FSStoreOptions{
		MaxLogBytes:     LogBudget,
		RollupWorkers:   RollupWorkers,
		StabilizationMS: StabilizationMS,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	if err := local.StartRollup(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("StartRollup: %w", err)
	}
	syncer := engine.NewSyncer(local, remoteStore, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          remoteStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("engine.New: %w", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("engine.Start: %w", err)
	}
	// engine.Store.Close also closes the remote — no double close here.
	return bs, func() { _ = bs.Close() }, nil
}
