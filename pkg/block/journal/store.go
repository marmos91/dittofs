package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// FileID identifies a file's byte stream inside the cache. It is the same
// value space and hash keyspace as today's payloadID.
type FileID string

// BlockID is the opaque key of a packed block in the remote store.
type BlockID string

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
	MaxLocalBytes    int64         // local on-disk cap that triggers eviction; 0 = unlimited
	EvictMaxWait     time.Duration // write-path backpressure budget before ErrLocalStoreFull
}

const (
	defaultSegmentSize      int64 = 256 << 20
	defaultCarveBlockSize   int64 = 4 << 20
	defaultCarveMaxAge            = 5 * time.Second
	defaultGCDeadRatioForce       = 0.5
	defaultShardCount             = 16
	defaultEvictMaxWait           = 30 * time.Second
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
	if c.EvictMaxWait <= 0 {
		c.EvictMaxWait = defaultEvictMaxWait
	}
	// MaxLocalBytes intentionally has no default: 0 means "no local cap" (eviction
	// off), the safe posture until the FSStore adapter wires --local-store-size.
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

	// deduper and sink are the carve collaborators, injected via SetCarveTargets
	// at wiring time. They own every step that touches pkg/block, blockcodec and
	// the metadata store, so journal imports none of them. Set once before the
	// first Carve; nil until wired (Carve reports the substrate is unwired).
	deduper Deduper
	sink    BlockSink

	shards    []*shard
	shardMask uint64

	// gcMu serializes GC passes: only one repack runs at a time so two passes
	// never pick the same victim.
	gcMu sync.Mutex

	nextSeg   atomic.Uint64 // global segment-ID allocator
	version   atomic.Uint64 // global monotonic LSN
	unsynced  atomic.Int64  // dirty bytes not yet carved to remote
	diskBytes atomic.Int64  // total on-disk segment bytes (headers + records), the eviction gate input

	writes    atomic.Int64
	reads     atomic.Int64
	coldReads atomic.Int64

	closed atomic.Bool
}

// Open opens (or creates) a Store rooted at dir. A fresh directory gets one
// active segment per shard. A populated directory is recovered: the active
// segment of each shard is tail-scanned and its torn tail truncated, every
// valid record is replayed into a fresh interval index, and the global Version
// LSN is resumed past the highest observed record. See recover.
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

	s := &Store{
		dir:       dir,
		cfg:       cfg,
		remote:    remote,
		clock:     clock,
		shardMask: uint64(cfg.ShardCount - 1),
	}

	ids, err := scanSegmentIDs(dir)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := s.recover(); err != nil {
			_ = s.Close()
			return nil, err
		}
		return s, nil
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
	// ponytail: direct per-shard fsync. syncLeader coalescing (fs/sync_leader.go)
	// is a throughput optimization for concurrent commits on one shard, not a
	// correctness requirement; port it here if commit contention shows up.
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

// Delete drops all of a file's cached ranges and persists a tombstone so
// recovery does not resurrect the file from its still-on-disk records (they
// linger in their segments until GC repacks them away). The tombstone's Version
// exceeds every prior write to the file, so a rewrite after the delete — with a
// higher Version — survives, recreating the file (correct create-after-unlink).
func (s *Store) Delete(ctx context.Context, id FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errClosed
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	var dirty int64
	if fi != nil {
		// Every live, non-cold interval's bytes become dead in their segment.
		for _, iv := range fi.ivs {
			if iv.cold {
				continue
			}
			if seg := sh.segment(iv.loc.SegmentID); seg != nil {
				seg.deadBytes.Add(iv.length)
			}
			if !iv.synced {
				dirty += iv.length
			}
		}
		delete(sh.index, id)
	}
	sh.mu.Unlock()
	if dirty != 0 {
		s.unsynced.Add(-dirty)
	}
	return s.appendTombstone(ctx, id)
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
