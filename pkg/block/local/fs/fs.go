// Package fs provides the journal-backed local block store. Since the Wall-A
// switchover (S2) FSStore is a thin adapter over *journal.Store: the journal is
// the live per-file byte cache (WriteAt/ReadAt keyed by payloadID+offset), owns
// its own segment layout, carve, eviction and GC, and writes the FileChunk
// manifest rows through the engine-supplied BlockSink. This file only bridges
// the string↔journal.FileID keyspace and the local.LocalStore admin surface.
package fs

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/health"
)

// ErrStoreClosed is returned by operations on a closed store.
var ErrStoreClosed = block.ErrStoreClosed

// FSStoreOptions tunes a journal-backed store. Only BackpressureMaxWait and
// ChunkParams are load-bearing; the rollup/append-log knobs are vestigial,
// retained so the shares config plumbing compiles.
//
// ponytail: RollupWorkers/StabilizationMS/SyncEveryWrite/OrphanLogMinAgeSeconds/
// MaxLogBytes no longer control anything — the journal carves on its own
// age/size gate. Drop them once the shares service stops reading them.
type FSStoreOptions struct {
	BackpressureMaxWait    time.Duration
	MaxLogBytes            int64
	ChunkParams            chunker.Params
	RollupWorkers          int
	StabilizationMS        int
	SyncEveryWrite         bool
	OrphanLogMinAgeSeconds int
}

// FSStore is the journal-backed local store. It embeds *journal.Store and
// shadows the FileID-typed data methods with payloadID(string) wrappers so it
// satisfies local.LocalStore.
type FSStore struct {
	*journal.Store

	dir            string
	maxDisk        int64
	maxLogBytes    int64
	fileChunkStore block.EngineFileChunkStore
	durable        atomic.Bool
}

var (
	_ local.LocalStore         = (*FSStore)(nil)
	_ block.DurabilityReporter = (*FSStore)(nil)
	_ local.MetricsAware       = (*FSStore)(nil)
)

// New builds a journal-backed FSStore with default options.
func New(dir string, maxDisk int64, fileChunkStore block.EngineFileChunkStore) (*FSStore, error) {
	return NewWithOptions(dir, maxDisk, fileChunkStore, FSStoreOptions{})
}

// NewWithOptions opens (or recovers) a journal rooted under dir and wraps it.
// The remote is nil: cold-read fetch and Hydrate are driven by the engine, so
// the journal never reaches the remote itself.
func NewWithOptions(dir string, maxDisk int64, fileChunkStore block.EngineFileChunkStore, opts FSStoreOptions) (*FSStore, error) {
	cfg := journal.Config{
		MaxLocalBytes: maxDisk,
		EvictMaxWait:  opts.BackpressureMaxWait,
		ChunkParams:   opts.ChunkParams,
	}
	js, err := journal.Open(filepath.Join(dir, "journal"), cfg, nil, journal.SystemClock())
	if err != nil {
		return nil, err
	}
	s := &FSStore{
		Store:          js,
		dir:            dir,
		maxDisk:        maxDisk,
		maxLogBytes:    opts.MaxLogBytes,
		fileChunkStore: fileChunkStore,
	}
	s.durable.Store(true) // fs-local survives a restart
	return s, nil
}

// --- string↔FileID data-plane shims (shadow the embedded FileID methods) ---

func (s *FSStore) WriteAt(ctx context.Context, payloadID string, offset int64, data []byte) error {
	return s.Store.WriteAt(ctx, journal.FileID(payloadID), offset, data)
}

func (s *FSStore) ReadAt(ctx context.Context, payloadID string, offset int64, dst []byte) (int, bool, error) {
	return s.Store.ReadAt(ctx, journal.FileID(payloadID), offset, dst)
}

func (s *FSStore) Hydrate(ctx context.Context, payloadID string, offset int64, data []byte) error {
	return s.Store.Hydrate(ctx, journal.FileID(payloadID), offset, data)
}

func (s *FSStore) Commit(ctx context.Context, payloadID string) error {
	return s.Store.Commit(ctx, journal.FileID(payloadID))
}

func (s *FSStore) Delete(ctx context.Context, payloadID string) error {
	return s.Store.Delete(ctx, journal.FileID(payloadID))
}

func (s *FSStore) Truncate(ctx context.Context, payloadID string, newSize int64) error {
	return s.Store.Truncate(ctx, journal.FileID(payloadID), newSize)
}

func (s *FSStore) FileSize(ctx context.Context, payloadID string) (int64, bool) {
	return s.Store.FileSize(ctx, journal.FileID(payloadID))
}

func (s *FSStore) DataExtents(ctx context.Context, payloadID string, fileSize int64) ([][2]uint64, error) {
	return s.Store.DataExtents(ctx, journal.FileID(payloadID), fileSize)
}

func (s *FSStore) ListFiles(ctx context.Context) []string {
	ids := s.Store.ListFiles(ctx)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// --- admin surface ---

// Start is a no-op: the journal has no background goroutines of its own (carve
// and eviction are driven by the engine's syncer).
func (s *FSStore) Start(context.Context) {}

// SetRetentionPolicy is a no-op: the journal evicts whole fully-synced segments
// approx-LRU and honors no pin/ttl/lru knob.
func (s *FSStore) SetRetentionPolicy(block.RetentionPolicy, time.Duration) {}

// SetMetrics is a no-op today — journal eviction/backpressure metrics are not
// wired through the local tier. ponytail: wire journal counters here if the
// eviction gauge regresses.
func (s *FSStore) SetMetrics(local.MetricsRecorder) {}

// Durable reports crash survival (block.DurabilityReporter). fs-local is durable.
func (s *FSStore) Durable() bool { return s.durable.Load() }

// SetDurable overrides the durability report (operator config).
func (s *FSStore) SetDurable(v bool) { s.durable.Store(v) }

// Stats maps the journal's coarse stats onto the local.Stats admin shape.
func (s *FSStore) Stats() local.Stats {
	js := s.Store.Stats()
	return local.Stats{
		DiskUsed:    js.LiveBytes + js.DeadBytes,
		MaxDisk:     s.maxDisk,
		MaxLogBytes: s.maxLogBytes,
		FileCount:   len(s.Store.ListFiles(context.Background())),
	}
}

// Healthcheck runs a cheap stat probe on the journal directory.
func (s *FSStore) Healthcheck(_ context.Context) health.Report {
	start := time.Now()
	if _, err := os.Stat(filepath.Join(s.dir, "journal")); err != nil {
		return health.NewUnhealthyReport(err.Error(), time.Since(start))
	}
	return health.NewHealthyReport(time.Since(start))
}
