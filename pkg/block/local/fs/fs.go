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

// FSStoreOptions tunes a journal-backed store. BackpressureMaxWait and
// ChunkParams are load-bearing; MaxLogBytes is retained only as a Stats size
// hint (it does not gate writes — the journal carves and evicts on its own
// age/size/disk budget).
type FSStoreOptions struct {
	BackpressureMaxWait time.Duration
	MaxLogBytes         int64
	ChunkParams         chunker.Params
	// MigrateLegacyLayout, when set, makes NewWithOptions archive a detected
	// pre-journal blobs/+logs/ layout aside (instead of refusing to open) so the
	// journal opens clean. Only a remote-backed share should set this: the
	// authoritative bytes then live in the remote store and are re-materialized
	// from the surviving metadata manifest via cold seeding. A local-only share
	// must leave it false so the guardrail stays fatal.
	MigrateLegacyLayout bool
	// MigrateLegacyLocalOnly, when set, makes NewWithOptions attempt an async,
	// non-blocking migration of a pre-journal blobs/+logs/ layout on a LOCAL-ONLY
	// share (no remote): if the append logs are complete (none compacted) the
	// dirs are archived aside, the journal opens clean, and the bytes are
	// re-ingested from the logs in the background (with per-payload fault-in on
	// read). If any log was compacted the guardrail stays fatal (its rolled-up
	// bytes live only in unrecoverable blobs). Ignored when MigrateLegacyLayout
	// is set (remote-backed shares take that path instead).
	MigrateLegacyLocalOnly bool
}

// FSStore is the journal-backed local store. It embeds *journal.Store and
// shadows the FileID-typed data methods with payloadID(string) wrappers so it
// satisfies local.LocalStore.
type FSStore struct {
	*journal.Store

	dir                string
	maxDisk            int64
	maxLogBytes        int64
	fileChunkStore     block.EngineFileChunkStore
	durable            atomic.Bool
	migratedFromLegacy bool
	// legacyMig drives an async local-only migration off the pre-journal layout;
	// nil when there is nothing to migrate. ReadAt faults a payload in through it.
	legacyMig *legacyMigration
}

// MigratedFromLegacy reports whether NewWithOptions archived a pre-journal
// blobs/+logs/ layout aside while opening this store. The caller uses it to
// trigger a one-time cold-seed of the journal from the surviving metadata
// manifest so reads fault the bytes back in from the remote store.
func (s *FSStore) MigratedFromLegacy() bool {
	return s.migratedFromLegacy
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
	// A directory written by a pre-journal release (blobs/+logs/) cannot be read
	// by the journal — opening it as an empty journal would silently serve every
	// stored file as zeros. A remote-backed share (MigrateLegacyLayout) archives
	// the legacy dirs aside so the journal opens clean and reads later cold-fetch
	// from the remote via the surviving manifest; a local-only share fails loud
	// so its on-disk bytes are not stranded behind an empty journal.
	migrated := false
	var legacyMig *legacyMigration
	switch {
	case opts.MigrateLegacyLayout:
		legacy, err := hasLegacyLocalLayout(dir)
		if err != nil {
			return nil, err
		}
		if legacy {
			if err := archiveLegacyLayout(dir); err != nil {
				return nil, err
			}
			migrated = true
		}
	case opts.MigrateLegacyLocalOnly:
		// Gate + archive before opening the journal: a refusal must leave no
		// empty journal behind, and the journal is bound into the migration
		// once it is open (below).
		var err error
		if legacyMig, err = setupLegacyLocalOnlyMigration(dir); err != nil {
			return nil, err
		}
	default:
		if err := checkLegacyLayout(dir); err != nil {
			return nil, err
		}
	}
	cfg := journal.Config{
		MaxLocalBytes: maxDisk,
		EvictMaxWait:  opts.BackpressureMaxWait,
		ChunkParams:   opts.ChunkParams,
	}
	js, err := journal.Open(filepath.Join(dir, "journal"), cfg, nil, journal.SystemClock())
	if err != nil {
		return nil, err
	}
	if legacyMig != nil {
		legacyMig.store = js
	}
	s := &FSStore{
		Store:              js,
		dir:                dir,
		maxDisk:            maxDisk,
		maxLogBytes:        opts.MaxLogBytes,
		fileChunkStore:     fileChunkStore,
		migratedFromLegacy: migrated,
		legacyMig:          legacyMig,
	}
	s.durable.Store(true) // fs-local survives a restart
	return s, nil
}

// --- string↔FileID data-plane shims (shadow the embedded FileID methods) ---

// materializeLegacy drains payloadID's archived pre-journal log into the journal
// before an access touches it, so the access observes (and, for a mutation,
// lands after) the replayed legacy bytes rather than racing them: a later
// background drain replays the SAME records idempotently and cannot clobber a
// client write, resurrect a deleted/truncated payload, or serve a read zeros.
// A no-op once the migration is done, for any payload not under migration, or
// when no migration is in flight.
func (s *FSStore) materializeLegacy(payloadID string) error {
	if s.legacyMig == nil {
		return nil
	}
	return s.legacyMig.materialize(payloadID)
}

func (s *FSStore) WriteAt(ctx context.Context, payloadID string, offset int64, data []byte) error {
	if err := s.materializeLegacy(payloadID); err != nil {
		return err
	}
	return s.Store.WriteAt(ctx, journal.FileID(payloadID), offset, data)
}

func (s *FSStore) ReadAt(ctx context.Context, payloadID string, offset int64, dst []byte) (int, bool, error) {
	if err := s.materializeLegacy(payloadID); err != nil {
		return 0, false, err
	}
	return s.Store.ReadAt(ctx, journal.FileID(payloadID), offset, dst)
}

func (s *FSStore) Hydrate(ctx context.Context, payloadID string, offset int64, data []byte) error {
	return s.Store.Hydrate(ctx, journal.FileID(payloadID), offset, data)
}

func (s *FSStore) Commit(ctx context.Context, payloadID string) error {
	return s.Store.Commit(ctx, journal.FileID(payloadID))
}

func (s *FSStore) Delete(ctx context.Context, payloadID string) error {
	if err := s.materializeLegacy(payloadID); err != nil {
		return err
	}
	return s.Store.Delete(ctx, journal.FileID(payloadID))
}

func (s *FSStore) Truncate(ctx context.Context, payloadID string, newSize int64) error {
	if err := s.materializeLegacy(payloadID); err != nil {
		return err
	}
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
