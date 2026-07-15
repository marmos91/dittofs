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

	"github.com/marmos91/dittofs/pkg/block/chunker"
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
	// ChunkParams sets the per-share FastCDC sizing carve feeds the chunker
	// (#1569). The zero value (or any params that fail Validate) degrades to
	// chunker.DefaultParams — the historical 1M/4M/16M profile — so a
	// misconfiguration is never a hard error, matching the fs store.
	ChunkParams chunker.Params
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
	if c.ChunkParams.Validate() != nil {
		c.ChunkParams = chunker.DefaultParams()
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

	// gcMu serializes GC passes against each other: only one pass runs at a time,
	// so two passes never pick the same victim. It does NOT exclude Carve or Evict
	// — a running GC pass keeps them off its segments via the per-shard carveMu it
	// holds and the per-segment busy claim it CAS-sets on each victim, not gcMu.
	gcMu sync.Mutex

	nextSeg   atomic.Uint64 // global segment-ID allocator
	version   atomic.Uint64 // global monotonic LSN
	unsynced  atomic.Int64  // dirty bytes not yet carved to remote
	diskBytes atomic.Int64  // total on-disk segment bytes (headers + records), the eviction gate input

	writes    atomic.Int64
	reads     atomic.Int64
	coldReads atomic.Int64

	// evictionDisabled gates Evict (and thus the write-path ensureSpace that
	// drives it). Health-driven: while the remote is unhealthy, cold-marking a
	// segment would strand bytes that can't be refetched, so eviction is paused.
	// Zero value = enabled, the safe default.
	evictionDisabled atomic.Bool

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
	// Durability first: persist and fsync the tombstone BEFORE touching the
	// in-memory index or the counters. If the append fails, the file's ranges are
	// left intact — a failed Delete never makes data disappear, and a crash can
	// never resurrect a file whose tombstone is already durable. The returned
	// Version fences which intervals the delete buries: only those at or below it
	// (a concurrent rewrite that raced past it carries a higher Version and
	// survives, recreating the file), mirroring recovery.
	tombVer, err := s.appendTombstone(ctx, id)
	if err != nil {
		return err
	}

	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	var dirty int64
	if fi != nil {
		kept := fi.ivs[:0]
		for _, iv := range fi.ivs {
			if iv.version > tombVer {
				kept = append(kept, iv) // raced past the delete: survives
				continue
			}
			// Buried by the tombstone: its bytes become dead in their segment.
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
		if len(kept) == 0 {
			delete(sh.index, id)
		} else {
			fi.ivs = kept
		}
	}
	sh.mu.Unlock()
	if dirty != 0 {
		s.unsynced.Add(-dirty)
	}
	return nil
}

// FileSize reports a file's data high-water mark: the maximum end offset over
// all its live intervals (dirty or cold). The second result is false when the
// file has no index entry. It is the fileSize input DataExtents needs — the
// journal tracks data extents, not the logical size (a grow's trailing hole
// lives in the metadata store, not here).
func (s *Store) FileSize(_ context.Context, id FileID) (int64, bool) {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return 0, false
	}
	var size int64
	for _, iv := range fi.ivs {
		if e := iv.end(); e > size {
			size = e
		}
	}
	return size, true
}

// SetEvictionEnabled toggles whole-segment eviction. Disabling it pauses Evict
// (and the write-path ensureSpace that drives it) so a health monitor can stop
// the store shedding local bytes while the remote is unreachable — a cold-marked
// range would otherwise be unrecoverable until the remote returns.
func (s *Store) SetEvictionEnabled(enabled bool) {
	s.evictionDisabled.Store(!enabled)
}

// ListFiles returns every FileID with a live index entry across all shards, in
// no guaranteed order. It lets a caller drive a bulk reset (Delete every file)
// without tracking IDs itself.
func (s *Store) ListFiles(_ context.Context) []FileID {
	var out []FileID
	for _, sh := range s.shards {
		sh.mu.Lock()
		for id := range sh.index {
			out = append(out, id)
		}
		sh.mu.Unlock()
	}
	return out
}

// Truncate shrinks a file to newSize: every live interval past newSize is
// dropped and an interval straddling newSize is clipped to end there, the freed
// bytes becoming dead in their segments (GC reclaims them). Growing a file —
// newSize at or past the current high-water mark — is a no-op here; a grow's
// trailing hole lives in the metadata store, not the journal.
//
// Crash-safety mirrors Delete. A durable, fsynced truncate marker is persisted
// BEFORE the in-memory index is touched, so a failed marker write leaves the
// file intact and a crash after the marker can never resurrect the truncated
// bytes: the on-disk data records past newSize linger until GC repacks them
// away, and recovery re-applies the clip from the marker. The marker's Version
// fences the clip — only intervals at or below it are affected, so a write that
// raced past the truncate (a higher Version) survives, re-extending the file.
// An in-flight carve of a clipped range is harmless: its post-upload flip
// re-resolves the interval by (offset, version) and simply skips a fragment the
// truncate dropped or clipped, exactly as it skips a concurrent overwrite.
func (s *Store) Truncate(ctx context.Context, id FileID, newSize int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errClosed
	}
	if newSize < 0 {
		return fmt.Errorf("journal: negative truncate size %d", newSize)
	}

	sh := s.shardFor(id)
	// Peek: with nothing past newSize this is a grow or no-op, so skip the marker
	// fsync entirely. A write that lands past newSize after this peek carries a
	// higher Version than any marker we would issue and is meant to survive, so
	// not fencing it is correct.
	sh.mu.Lock()
	fi := sh.index[id]
	past := false
	if fi != nil {
		for _, iv := range fi.ivs {
			if iv.end() > newSize {
				past = true
				break
			}
		}
	}
	sh.mu.Unlock()
	if !past {
		return nil
	}

	// Durability first: the marker must be on disk before the index is clipped.
	truncVer, err := s.appendTruncateMarker(ctx, id, newSize)
	if err != nil {
		return err
	}

	sh.mu.Lock()
	fi = sh.index[id]
	var dirty int64
	if fi != nil {
		kept := fi.ivs[:0]
		for _, iv := range fi.ivs {
			if iv.version > truncVer || iv.end() <= newSize {
				kept = append(kept, iv) // raced past the truncate, or already within
				continue
			}
			if iv.fileOff < newSize {
				// Straddles newSize: clip to [fileOff, newSize); the tail dies.
				dead := iv.end() - newSize
				if !iv.cold {
					if seg := sh.segment(iv.loc.SegmentID); seg != nil {
						seg.deadBytes.Add(dead)
					}
				}
				if !iv.synced {
					dirty += dead
				}
				kept = append(kept, iv.clamp(iv.fileOff, newSize))
				continue
			}
			// Entirely past newSize: drop it; its bytes become dead.
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
		if len(kept) == 0 {
			delete(sh.index, id)
		} else {
			fi.ivs = kept
		}
	}
	sh.mu.Unlock()
	if dirty != 0 {
		s.unsynced.Add(-dirty)
	}
	return nil
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
