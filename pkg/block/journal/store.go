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

	"github.com/marmos91/dittofs/internal/logger"
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
	// MaxLocalBytes is the local on-disk cap that triggers eviction. 0 leaves
	// it unset here: Open derives a free-space-based default (a
	// defaultMaxLocalBytesFreeFraction share of the store volume's free space)
	// so a caller only lands on a genuinely uncapped store when that probe
	// itself fails.
	MaxLocalBytes int64
	EvictMaxWait  time.Duration // write-path backpressure budget before ErrLocalStoreFull
	// CarveUploadConcurrency bounds how many of one file's packed blocks may be
	// committed (uploaded + committed) at once within a single carve run. Packing
	// stays sequential; only the block commits overlap, so a single large file's
	// carve is no longer one PutBlock at a time. Peak carve RAM scales with this
	// window (window x CarveBlockSize), so keep it modest. Zero falls back to the
	// default via withDefaults.
	CarveUploadConcurrency int
	// ChunkParams sets the per-share FastCDC sizing carve feeds the chunker
	// (#1569). The zero value (or any params that fail Validate) degrades to
	// chunker.DefaultParams — the historical 1M/4M/16M profile — so a
	// misconfiguration is never a hard error, matching the fs store.
	ChunkParams chunker.Params
}

const (
	defaultSegmentSize            int64 = 256 << 20
	defaultCarveBlockSize         int64 = 4 << 20
	defaultCarveMaxAge                  = 5 * time.Second
	defaultGCDeadRatioForce             = 0.5
	defaultShardCount                   = 16
	defaultEvictMaxWait                 = 30 * time.Second
	defaultCarveUploadConcurrency       = 8

	// defaultMaxLocalBytesFreeFraction is the share of a store dir's free disk
	// space Open claims for an unset MaxLocalBytes. Conservative (not the
	// whole disk) since the volume is typically shared with other shares and
	// the host OS; it is a soft pressure threshold, not a hard reservation
	// (see ensureSpace), so leaving headroom below 100% just makes eviction
	// engage before the disk is bone dry.
	defaultMaxLocalBytesFreeFraction = 0.8
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
	if c.CarveUploadConcurrency <= 0 {
		c.CarveUploadConcurrency = defaultCarveUploadConcurrency
	}
	if c.ChunkParams.Validate() != nil {
		c.ChunkParams = chunker.DefaultParams()
	}
	// MaxLocalBytes is left untouched here (0 = unset): withDefaults has no dir
	// to size a free-space-based cap from. Open fills it in once dir is known.
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

	nextSeg atomic.Uint64 // global segment-ID allocator
	version atomic.Uint64 // global monotonic LSN
	// pinVersion is the highest snapshot watermark still held by a live snapshot
	// (0 = none). A segment whose minVersion is at or below it is kept off the
	// eviction/GC path so a local-only snapshot's bytes — the only durable copy —
	// survive until the snapshot is deleted. DERIVED: the runtime recomputes it as
	// max(JournalVersion) over live snapshots and calls SetPinVersion; the journal
	// only reads it (reclaim.go). See #1718.
	pinVersion atomic.Uint64
	unsynced   atomic.Int64 // dirty bytes not yet carved to remote
	diskBytes  atomic.Int64 // total on-disk segment bytes (headers + records), the eviction gate input

	writes    atomic.Int64
	reads     atomic.Int64
	coldReads atomic.Int64

	// evictionDisabled gates Evict (and thus the write-path ensureSpace that
	// drives it). Health-driven: while the remote is unhealthy, cold-marking a
	// segment would strand bytes that can't be refetched, so eviction is paused.
	// Zero value = enabled, the safe default.
	evictionDisabled atomic.Bool

	// verifyReads turns on per-read record-CRC verification of warm reads (opt-in
	// for durable tiers; off for the fast writeback path). When off, ReadAt serves
	// a warm piece with a single raw pread and does no extra work. Set once before
	// the store serves reads.
	verifyReads atomic.Bool

	closed atomic.Bool

	// gcCancel/gcDone govern the background dead-ratio GC loop started by
	// Open: cancel stops the loop, and Close waits on gcDone so no repack is
	// still in flight when segment files are closed underneath it.
	gcCancel context.CancelFunc
	gcDone   chan struct{}
}

// SetVerifyReads enables or disables per-read record-CRC verification of warm
// reads. Durable tiers turn it on so on-disk corruption between recovery and a
// warm read is caught (and healed/failed-closed by the caller) instead of
// returning silently-wrong bytes; the writeback tier leaves it off to keep the
// raw fast read. Called once at share construction, before the store serves reads.
func (s *Store) SetVerifyReads(v bool) { s.verifyReads.Store(v) }

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

	// An unset cap left the write-path gate (ensureSpace) a permanent no-op,
	// so dead append-log records were never reclaimed and disk usage grew
	// without bound under overwrites. Size a soft default off the volume's
	// free space at open time; a probe failure (unsupported platform, statfs
	// error) degrades to the old unbounded posture rather than failing Open.
	if cfg.MaxLocalBytes <= 0 {
		if free, ferr := diskFreeBytes(dir); ferr == nil && free > 0 {
			cfg.MaxLocalBytes = int64(float64(free) * defaultMaxLocalBytesFreeFraction)
		} else if ferr != nil {
			logger.Warn("journal: could not determine free disk space; local store cap left unset (unbounded growth risk)",
				"dir", dir, "error", ferr)
		}
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
	} else {
		s.shards = make([]*shard, cfg.ShardCount)
		for i := range s.shards {
			seg, err := s.createSegment()
			if err != nil {
				_ = s.Close()
				return nil, err
			}
			s.shards[i] = newShard(seg)
		}
	}

	s.startBackgroundGC()
	return s, nil
}

// Close closes every open segment file descriptor. It is idempotent.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.gcCancel != nil {
		s.gcCancel()
		<-s.gcDone
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

// defaultGCInterval is how often the background loop repacks high-dead-ratio
// segments. A pass is a near-noop when nothing is at or above GCDeadRatioForce,
// so a short interval keeps on-disk growth tracking live bytes under
// overwrite-heavy load without costing an idle store anything meaningful.
const defaultGCInterval = 30 * time.Second

// startBackgroundGC launches the periodic dead-ratio repack loop. Overwrites
// leave dead records behind; without proactive repacking they are only
// reclaimed on the write-path eviction gate, so a store whose writes outpace
// carve grows until the cap forces backpressure. The loop keeps local bytes
// bounded relative to live bytes regardless of whether a cap is set. Close
// cancels it and waits on gcDone before closing segment files.
func (s *Store) startBackgroundGC() {
	ctx, cancel := context.WithCancel(context.Background())
	s.gcCancel = cancel
	s.gcDone = make(chan struct{})
	go func() {
		defer close(s.gcDone)
		t := time.NewTicker(defaultGCInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// errClosed races Close (which sets s.closed before cancelling
				// this loop's context); both it and context.Canceled are the
				// normal shutdown signal, not a failure worth logging.
				if _, err := s.GC(ctx, GCOptions{}); err != nil &&
					!errors.Is(err, context.Canceled) && !errors.Is(err, errClosed) {
					logger.Warn("journal: background GC pass failed", "error", err)
				}
			}
		}
	}()
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

// SeedCold registers a byte range as remote-durable-but-not-local: a read of it
// reports cold so the engine hydrates it from the remote store instead of
// zero-filling. Snapshot restore seeds the restored FileChunk manifest's extents
// this way after ResetLocalState wiped the local tier — the bytes live in remote,
// addressed by the restored manifest. The caller (restore, remote-backed shares
// only) guarantees the range is remotely backed; a hydrate replaces the seeded
// cold interval with the fetched warm bytes on first read.
func (s *Store) SeedCold(_ context.Context, id FileID, offset, length int64) error {
	if s.closed.Load() {
		return errClosed
	}
	if length <= 0 {
		return nil
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.indexFor(id)
	fi.insert(interval{
		fileOff: offset,
		length:  length,
		version: s.nextVersion(),
		synced:  true,
		cold:    true,
	})
	return nil
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
	return sh.groupCommit()
}

// groupCommit fsyncs the shard so every write buffered so far is durable, but
// coalesces concurrent callers: the first to arrive leads and issues one fsync
// of the active fd (which flushes all its dirty bytes); everyone who enqueued
// before that fsync started piggybacks on its result instead of issuing their
// own barrier. Correctness rests on two durability points — a completed active-fd
// fsync, and segment rotation (sealInPlace fsyncs the segment it seals) — so a
// caller whose bytes are on a since-sealed segment is durable no matter which fd
// the leader synced. This is the per-shard sync-leader the fio rand-write-4k
// burst (iodepth=32 × numjobs=4) needs; without it every one of ~128 in-flight
// commits pays a full disk barrier (#1736).
func (sh *shard) groupCommit() error {
	sh.commitMu.Lock()
	myGen := sh.reqSeq
	sh.reqSeq++
	for {
		// A fsync that started after this caller enqueued (so after its write
		// completed) has finished and covered it. Return that batch's outcome:
		// the error is sticky per errSeq, so a covered waiter can never read a
		// later batch's nil in place of its own batch's failure (fsyncgate).
		if sh.doneSeq > myGen {
			var err error
			if myGen < sh.errSeq {
				err = sh.syncErr
			}
			sh.commitMu.Unlock()
			return err
		}
		if !sh.syncing {
			break // become the leader for this batch
		}
		sh.commitCond.Wait()
	}
	sh.syncing = true
	batchUpTo := sh.reqSeq // every commit enqueued so far rides this fsync
	sh.commitMu.Unlock()

	// Grab the current active segment fresh: if it rotated while we waited, the old
	// fd was already fsynced by sealInPlace, so fsyncing the new one still leaves
	// the whole batch durable.
	sh.mu.Lock()
	seg := sh.active
	sh.mu.Unlock()
	err := sh.segSync(seg)

	sh.commitMu.Lock()
	if err != nil {
		err = fmt.Errorf("journal: commit fsync: %w", err)
		// Sticky: mark every commit in this batch as failed so covered waiters
		// (gen < batchUpTo) read the failure even after a later batch succeeds.
		sh.errSeq = batchUpTo
		sh.syncErr = err
	}
	sh.doneSeq = batchUpTo
	sh.syncing = false
	sh.commitCond.Broadcast()
	sh.commitMu.Unlock()
	return err
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
	// A tombstone can leave a segment holding no live bytes — most visibly the
	// active segment after a cold read hydrated the just-removed file's bytes
	// locally. Reclaim those now-dead segments so the unlink frees the local tier
	// immediately instead of stranding it until the next rotation or force-evict.
	// Best-effort: a reclaim failure never wedges the delete (the file is already
	// tombstoned and the recovery sweep reclaims any orphan).
	_ = s.reclaimEmptied(sh)
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

// JournalVersion returns the current global LSN watermark: every record written
// so far carries a Version at or below it. Snapshot create captures this after
// draining rollups so the snapshot pins exactly the records that make up its
// point-in-time view (#1718).
func (s *Store) JournalVersion() uint64 { return s.version.Load() }

// SetPinVersion sets the highest live-snapshot watermark. The runtime derives it
// as max(JournalVersion) over live snapshots and raises it before a snapshot is
// marked ready / lowers it only after a delete commits, so GC only ever grows
// more conservative. Reads are a single atomic load on the reclaim path.
func (s *Store) SetPinVersion(v uint64) { s.pinVersion.Store(v) }

// PinVersion reports the current pin watermark (0 = no live snapshot).
func (s *Store) PinVersion() uint64 { return s.pinVersion.Load() }

// RestoreToVersion rewinds every file to its point-in-time view as of the global
// LSN watermark V and re-materializes that view durably at the log head, so a
// crash-reopen reconstructs V and the pre-restore records (which a safety snapshot
// still pins for rollback) stay intact. It is the local-only snapshot-restore
// primitive: the journal is the only durable copy of the bytes, so a plain rewind
// is not restart-safe (recover() would resurrect the >V head) and the >V records
// cannot be deleted. See #1718.
//
// Two phases:
//
//  1. Ceiling replay: scan every on-disk record and rebuild each file's coverage
//     as of V — data records with Version<=V resolved newest-wins, tombstones and
//     truncate markers with Version<=V honored, everything above V ignored. The
//     pre-overwrite records survive because a live snapshot pinned their segments.
//  2. Re-materialize: for each file, read the V-view bytes from their pinned
//     source records and re-append them as fresh dirty records at the head (a
//     tombstone first to bury the current head, then the V-view data). Fresh
//     versions exceed everything, so recover() rebuilds V on reopen; a file
//     present at head but absent at V is tombstoned away.
//
// The caller (restore orchestration) drains rollups afterward and holds the share
// disabled, so no concurrent writer races this.
func (s *Store) RestoreToVersion(ctx context.Context, v uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errClosed
	}

	// --- phase 1: ceiling replay over the on-disk records ---
	vIndex := map[FileID]*fileIndex{}
	tombstones := map[FileID]uint64{}
	truncations := map[FileID]truncMark{}
	for _, sh := range s.shards {
		sh.mu.Lock()
		segs := make([]*segmentMeta, 0, len(sh.sealed)+1)
		if sh.active != nil {
			segs = append(segs, sh.active)
		}
		for _, seg := range sh.sealed {
			segs = append(segs, seg)
		}
		sh.mu.Unlock()

		for _, seg := range segs {
			recs, _ := scanValidRecords(seg.fd, s.cfg.SegmentSize, s.cfg.SegmentSize)
			for _, rec := range recs {
				if rec.header.Version > v {
					continue // above the watermark: belongs to a post-snapshot state
				}
				fid := FileID(rec.fileID)
				switch {
				case rec.header.Flags&flagTombstone != 0:
					if rec.header.Version > tombstones[fid] {
						tombstones[fid] = rec.header.Version
					}
				case rec.header.Flags&flagTruncate != 0:
					if cur, ok := truncations[fid]; !ok || rec.header.Version > cur.version {
						truncations[fid] = truncMark{version: rec.header.Version, newSize: int64(rec.header.FileOffset)}
					}
				default:
					fi := vIndex[fid]
					if fi == nil {
						fi = &fileIndex{}
						vIndex[fid] = fi
					}
					fi.insert(interval{
						fileOff: int64(rec.header.FileOffset),
						length:  int64(rec.header.PayloadLen),
						version: rec.header.Version,
						synced:  rec.header.Flags&flagSynced != 0,
						loc: SegmentLocation{
							SegmentID: seg.id,
							Offset:    rec.segOff + recordHeaderSize + int64(len(rec.fileID)),
							Length:    int64(rec.header.PayloadLen),
						},
					})
				}
			}
		}
	}
	// Honor tombstones and truncate markers at or below V, mirroring recover().
	for fid, fi := range vIndex {
		if tv, ok := tombstones[fid]; ok {
			kept := fi.ivs[:0]
			for _, iv := range fi.ivs {
				if iv.version > tv {
					kept = append(kept, iv)
				}
			}
			fi.ivs = kept
		}
		if tm, ok := truncations[fid]; ok {
			kept := fi.ivs[:0]
			for _, iv := range fi.ivs {
				if iv.version > tm.version || iv.end() <= tm.newSize {
					kept = append(kept, iv)
					continue
				}
				if iv.fileOff < tm.newSize {
					kept = append(kept, iv.clamp(iv.fileOff, tm.newSize))
				}
			}
			fi.ivs = kept
		}
		if len(fi.ivs) == 0 {
			delete(vIndex, fid)
		}
	}

	// --- phase 2: re-materialize the V-view at the head ---
	head := map[FileID]struct{}{}
	for _, id := range s.ListFiles(ctx) {
		head[id] = struct{}{}
	}
	for id, fi := range vIndex {
		type extent struct {
			off  int64
			data []byte
		}
		exts := make([]extent, 0, len(fi.ivs))
		sh := s.shardFor(id)
		for _, iv := range fi.ivs {
			if iv.length <= 0 {
				continue
			}
			sh.mu.Lock()
			seg := sh.segment(iv.loc.SegmentID)
			sh.mu.Unlock()
			if seg == nil {
				return fmt.Errorf("journal: restore: source segment %d gone for %q@%d", iv.loc.SegmentID, id, iv.fileOff)
			}
			buf := make([]byte, iv.length)
			if _, err := seg.fd.ReadAt(buf, iv.loc.Offset); err != nil {
				return fmt.Errorf("journal: restore: read %q@%d from segment %d: %w", id, iv.fileOff, iv.loc.SegmentID, err)
			}
			exts = append(exts, extent{off: iv.fileOff, data: buf})
		}
		// Bury the current head (tombstone Version > head), then re-assert the
		// V-view as fresh dirty records on top of it.
		if err := s.Delete(ctx, id); err != nil {
			return fmt.Errorf("journal: restore: bury head for %q: %w", id, err)
		}
		for _, e := range exts {
			if err := s.WriteAt(ctx, id, e.off, e.data); err != nil {
				return fmt.Errorf("journal: restore: re-materialize %q@%d: %w", id, e.off, err)
			}
		}
		delete(head, id)
	}
	// Files present at head but not in the V-view were created after V: tombstone
	// them so recover() and reads agree they are gone.
	for id := range head {
		if err := s.Delete(ctx, id); err != nil {
			return fmt.Errorf("journal: restore: tombstone post-V file %q: %w", id, err)
		}
	}
	return nil
}
