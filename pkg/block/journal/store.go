package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// FileID identifies a file's byte stream inside the cache. It is the same
// value space and hash keyspace as today's payloadID.
type FileID string

// BlockID is the opaque key of a packed block in the remote store.
type BlockID string

// errNotImplemented is returned by operations whose implementation lands in a
// later change (carve, evict, gc, delete, reopen recovery).
var errNotImplemented = errors.New("journal: not yet implemented")

// errClosed is returned by every operation attempted on a closed Store.
var errClosed = errors.New("journal: store closed")

// minSegmentSize is the floor for Config.SegmentSize. A segment must comfortably
// hold its header plus real records; below this a single write could exceed the
// cap. 1 MiB clears the largest protocol write plus framing with wide margin.
const minSegmentSize int64 = 1 << 20

// RemoteStore is the narrow remote contract journal carves to and hydrates
// from. It mirrors the shape of pkg/block/remote's RemoteBlockStore but is
// declared here so journal imports nothing from the block/remote package.
type RemoteStore interface {
	PutBlock(ctx context.Context, id BlockID, r io.Reader, size int64) error
	GetBlock(ctx context.Context, id BlockID) (io.ReadCloser, error)
	GetRange(ctx context.Context, id BlockID, off, length int64) (io.ReadCloser, error)
}

// Clock supplies the current time. Injected so tests can pin it.
type Clock interface{ Now() time.Time }

// systemClock is the production Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns a Clock backed by time.Now.
func SystemClock() Clock { return systemClock{} }

// Config tunes a Store. Zero values fall back to defaults via withDefaults.
type Config struct {
	SegmentSize      int64         // segment cap before rotation
	CarveBlockSize   int64         // fixed pack size handed to the remote store
	CarveMaxAge      time.Duration // age-based carve batching cap
	GCDeadRatioForce float64       // dead/total ratio that forces a repack
	ShardCount       int           // number of shards, power of two, immutable per store
}

const (
	defaultSegmentSize      int64 = 256 << 20
	defaultCarveBlockSize   int64 = 4 << 20
	defaultCarveMaxAge            = 5 * time.Second
	defaultGCDeadRatioForce       = 0.5
	defaultShardCount             = 16
)

func (c Config) withDefaults() Config {
	if c.SegmentSize <= 0 {
		c.SegmentSize = defaultSegmentSize
	}
	if c.CarveBlockSize <= 0 {
		c.CarveBlockSize = defaultCarveBlockSize
	}
	if c.CarveMaxAge <= 0 {
		c.CarveMaxAge = defaultCarveMaxAge
	}
	if c.GCDeadRatioForce <= 0 {
		c.GCDeadRatioForce = defaultGCDeadRatioForce
	}
	if c.ShardCount <= 0 {
		c.ShardCount = defaultShardCount
	}
	return c
}

// Stats is a coarse snapshot of store state, cheap to compute.
type Stats struct {
	Segments      int
	LiveBytes     int64
	DeadBytes     int64
	UnsyncedBytes int64
	Writes        int64
	Reads         int64
	ColdReads     int64
}

// Store is the per-share local cache. All exported methods are safe for
// concurrent use; per-shard mutexes serialize appends and index mutation while
// positioned reads run unlocked.
type Store struct {
	dir    string
	cfg    Config
	remote RemoteStore
	clock  Clock

	shards    []*shard
	shardMask uint64

	nextSeg  atomic.Uint64 // global segment-ID allocator
	version  atomic.Uint64 // global monotonic LSN
	unsynced atomic.Int64  // dirty bytes not yet carved to remote

	writes    atomic.Int64
	reads     atomic.Int64
	coldReads atomic.Int64

	closed atomic.Bool
}

// Open opens (or creates) a Store rooted at dir. A fresh directory gets one
// active segment per shard. Reopen of a populated directory (crash recovery
// replay) is not yet implemented; callers currently start from an empty cache.
func Open(dir string, cfg Config, remote RemoteStore, clock Clock) (*Store, error) {
	cfg = cfg.withDefaults()
	if cfg.ShardCount&(cfg.ShardCount-1) != 0 {
		return nil, fmt.Errorf("journal: ShardCount %d is not a power of two", cfg.ShardCount)
	}
	if cfg.SegmentSize < minSegmentSize {
		return nil, fmt.Errorf("journal: SegmentSize %d below floor %d (header+record framing)", cfg.SegmentSize, minSegmentSize)
	}
	if clock == nil {
		clock = SystemClock()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: mkdir %q: %w", dir, err)
	}
	if ids, err := scanSegmentIDs(dir); err != nil {
		return nil, err
	} else if len(ids) > 0 {
		// Crash-recovery replay is not yet implemented; refuse to start a fresh
		// cache over stale segments rather than silently lose their data.
		return nil, fmt.Errorf("journal: %w: reopen of populated dir %q", errNotImplemented, dir)
	}

	s := &Store{
		dir:       dir,
		cfg:       cfg,
		remote:    remote,
		clock:     clock,
		shardMask: uint64(cfg.ShardCount - 1),
	}
	s.shards = make([]*shard, cfg.ShardCount)
	for i := range s.shards {
		seg, err := s.createSegment()
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		s.shards[i] = newShard(seg)
	}
	return s, nil
}

// Close closes every open segment file descriptor. It is idempotent.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	var firstErr error
	for _, sh := range s.shards {
		if sh == nil {
			continue
		}
		sh.mu.Lock()
		if sh.active != nil {
			if err := sh.active.close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		for _, seg := range sh.sealed {
			if err := seg.close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		sh.mu.Unlock()
	}
	return firstErr
}

// WriteAt buffers a dirty client write. It never fsyncs; durability is a
// separate Commit.
func (s *Store) WriteAt(ctx context.Context, id FileID, offset int64, data []byte) error {
	return s.appendRecord(ctx, id, offset, data, false)
}

// Hydrate writes bytes fetched from the remote store during a cold read. Same
// append primitive as WriteAt, but the record is born clean (already durable
// remotely) so it is immediately evictable.
func (s *Store) Hydrate(ctx context.Context, id FileID, offset int64, data []byte) error {
	return s.appendRecord(ctx, id, offset, data, true)
}

// Commit fsyncs the file's shard so buffered writes become durable. NFS COMMIT
// and SMB Flush land here.
func (s *Store) Commit(ctx context.Context, id FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errClosed
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	fd := sh.active.fd
	sh.mu.Unlock()
	// ponytail: direct per-shard fsync. syncLeader batching (fs/sync_leader.go)
	// is ported in with the append primitive PR; wire it here then.
	if err := fd.Sync(); err != nil {
		return fmt.Errorf("journal: commit fsync: %w", err)
	}
	return nil
}

// UnsyncedBytes reports dirty bytes not yet carved to the remote store. The
// eviction backpressure path watches this.
func (s *Store) UnsyncedBytes() int64 { return s.unsynced.Load() }

// Stats returns a coarse snapshot of store state.
func (s *Store) Stats() Stats {
	st := Stats{
		UnsyncedBytes: s.unsynced.Load(),
		Writes:        s.writes.Load(),
		Reads:         s.reads.Load(),
		ColdReads:     s.coldReads.Load(),
	}
	for _, sh := range s.shards {
		sh.mu.Lock()
		if sh.active != nil {
			st.Segments++
			st.LiveBytes += sh.active.liveBytes.Load()
			st.DeadBytes += sh.active.deadBytes.Load()
		}
		for _, seg := range sh.sealed {
			st.Segments++
			st.LiveBytes += seg.liveBytes.Load()
			st.DeadBytes += seg.deadBytes.Load()
		}
		sh.mu.Unlock()
	}
	return st
}

// Carve packs dirty ranges into remote blocks and flips their records to
// synced. Not yet implemented.
func (s *Store) Carve(ctx context.Context, opts CarveOptions) (CarveResult, error) {
	return CarveResult{}, errNotImplemented
}

// Evict frees whole sealed segments under storage pressure. Not yet implemented.
func (s *Store) Evict(ctx context.Context, targetBytes int64) (EvictResult, error) {
	return EvictResult{}, errNotImplemented
}

// Delete drops all of a file's cached ranges. Not yet implemented.
func (s *Store) Delete(ctx context.Context, id FileID) error {
	return errNotImplemented
}

// segPath returns the on-disk path of a segment by ID.
func (s *Store) segPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf(segIDFmt+segSuffix, id))
}

// idxPath returns the on-disk path of a segment's .idx sidecar.
func (s *Store) idxPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf(segIDFmt+idxSuffix, id))
}

// nextVersion returns the next global LSN.
func (s *Store) nextVersion() uint64 { return s.version.Add(1) }
