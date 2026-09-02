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
	// CarveUploadConcurrency bounds how many of one file's packed blocks are
	// committed (uploaded + committed) at once. Packing itself is sequential
	// across the whole file, one block at a time, and so are the manifest row-end
	// lookups that widen each run; only the commits overlap, so a single large
	// file's carve is not one PutBlock at a time.
	// Peak carve RAM per file is window x (CarveBlockSize + one overhang chunk)
	// for the block arenas, plus the single chunker scratch buffer of
	// chunker.MaxChunkSize the pass holds — so keep it modest.
	// Zero falls back to the default via withDefaults.
	CarveUploadConcurrency int
	// DirtyExpiry bounds how long an appended record may sit unfsynced. A
	// background loop commits every shard still holding uncovered records once
	// per interval, so a client that never asks for durability (no NFS COMMIT,
	// no SMB FLUSH/CLOSE) still has its writes reach the device within roughly
	// this window instead of waiting for the shard's next 256 MiB rotation. It
	// is a ceiling on the loss window, not a guarantee: fsync remains the only
	// synchronous durability point. Zero falls back to the default via
	// withDefaults; negative disables the loop entirely.
	DirtyExpiry time.Duration
	// ChunkParams sets the per-share FastCDC sizing carve feeds the chunker.
	// The zero value (or any params that fail Validate) degrades to
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
	// defaultDirtyExpiry mirrors Linux writeback's dirty_expire_centisecs
	// default: an unfsynced write is pushed to the device once it is about
	// this old.
	defaultDirtyExpiry = 30 * time.Second

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
	if c.DirtyExpiry == 0 {
		c.DirtyExpiry = defaultDirtyExpiry
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
	Segments int
	// DiskBytes is the physical footprint of the segment files: segment headers
	// plus record framing plus payload, seeded at recovery from the segments
	// already on disk and maintained by every append and retire. It is the
	// figure the eviction gate compares against MaxLocalBytes.
	DiskBytes int64
	// LiveBytes and DeadBytes are payload-only accounting, both derived from
	// record PayloadLen. LiveBytes counts every payload byte a segment has ever
	// been charged for and DeadBytes the share of those since superseded, so
	// they overlap and must not be summed into a footprint.
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
	// only reads it (reclaim.go).
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

	// coldMu guards the cold log's append handle (see cold.go). The log records
	// every cold interval so a restart still knows those ranges are
	// remote-resident rather than POSIX holes.
	coldMu sync.Mutex
	coldFD *os.File

	// bgCancel stops the background loops started by Open — the dead-ratio
	// repack and the dirty-age commit. Close cancels it and waits on bgWG so
	// neither loop is still touching a segment when its file is closed
	// underneath it.
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup

	// failTombstone/failTruncate are test seams: when either equals the FileID
	// of a Delete or Truncate, the corresponding marker append returns an error
	// before persisting anything, modeling a durability failure. Always empty in
	// production.
	failTombstone FileID
	failTruncate  FileID
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

	s.startBackground()
	return s, nil
}

// Close closes every open segment file descriptor. It is idempotent.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.bgCancel != nil {
		s.bgCancel()
	}
	s.bgWG.Wait()
	firstErr := s.closeCold()
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

// startBackground launches the store's periodic loops. Close cancels them and
// waits for each to return before closing segment files. A negative DirtyExpiry
// leaves the dirty-age loop unstarted.
func (s *Store) startBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	s.bgCancel = cancel
	s.bgWG.Add(1)
	go func() { defer s.bgWG.Done(); s.gcLoop(ctx) }()
	if s.cfg.DirtyExpiry > 0 {
		s.bgWG.Add(1)
		go func() { defer s.bgWG.Done(); s.syncLoop(ctx) }()
	}
}

// gcLoop is the periodic dead-ratio repack. Overwrites leave dead records
// behind; without proactive repacking they are only reclaimed on the write-path
// eviction gate, so a store whose writes outpace carve grows until the cap
// forces backpressure. The loop keeps local bytes bounded relative to live
// bytes regardless of whether a cap is set.
func (s *Store) gcLoop(ctx context.Context) {
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
}

// syncLoop commits the shards holding unfsynced records once per DirtyExpiry,
// bounding how long an acknowledged write can sit in the page cache. It is
// dirty-driven, so an idle store never fsyncs, and it runs off the ack path, so
// it adds no write latency. See Config.DirtyExpiry.
func (s *Store) syncLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.DirtyExpiry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.commitDirtyShards(); err != nil {
				logger.Warn("journal: dirty-age commit failed", "error", err)
			}
		}
	}
}

// commitDirtyShards fsyncs each shard holding records above its durable
// watermark and reports the first failure, having still attempted every other
// dirty shard. groupCommit coalesces with any concurrent client commit, so
// overlapping with a carve pass or an explicit Commit costs at most one extra
// fsync and never duplicates work.
func (s *Store) commitDirtyShards() error {
	var firstErr error
	for _, sh := range s.shards {
		if !sh.dirty() {
			continue
		}
		if err := sh.groupCommit(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WriteAt buffers a dirty client write. It never fsyncs; durability is a
// separate Commit.
func (s *Store) WriteAt(ctx context.Context, id FileID, offset int64, data []byte) error {
	return s.appendRecord(ctx, id, offset, data, false, 0)
}

// Hydrate writes bytes fetched from the remote store during a cold read. Same
// append primitive as WriteAt, but the record is born clean (already durable
// remotely) so it is immediately evictable.
//
// It fills rather than overwrites: only the parts of the range no live interval
// holds, plus the parts a cold interval holds no later than notAfter, are
// written. A read window resolves every manifest row covering it once any byte
// of it is cold, so a fetch routinely offers bytes the journal already holds —
// and the journal's are the newer copy, since the remote holds what a carve
// uploaded and the journal holds that plus everything written since. Writing
// them back would put a client's just-written data underneath the content it
// replaced.
//
// notAfter is the WriteVersion the caller sampled before it resolved which
// remote bytes to fetch; a cold range recorded after it was superseded and
// evicted while the fetch ran, so it is stale too. Zero applies no bound.
//
// Dropping is always safe: the write-back is a cache fill, never a read's
// answer, and costs at most a re-fetch.
func (s *Store) Hydrate(ctx context.Context, id FileID, offset int64, data []byte, notAfter uint64) error {
	sh := s.shardFor(id)
	sh.mu.Lock()
	var ranges [][2]int64
	if !hydrateFenced(sh, id, notAfter) {
		ranges = sh.index[id].hydratable(offset, int64(len(data)), notAfter)
	}
	sh.mu.Unlock()
	for _, r := range ranges {
		if err := s.appendRecord(ctx, id, r[0], data[r[0]-offset:r[1]-offset], true, notAfter); err != nil {
			return err
		}
	}
	return nil
}

// WriteVersion reports the store's current global LSN. Sampled before a caller
// resolves what to fetch, it bounds what that fetch is allowed to write back
// (see Hydrate).
func (s *Store) WriteVersion() uint64 { return s.version.Load() }

// hydrateFenced reports whether a hydrate's bound predates the file's most
// recent truncate or delete — the two mutations that leave no interval behind
// to compare against. A delete's fence may have been evicted from the shard's
// FIFO, in which case evictedFenceFloor still stands in for it. Callers hold
// sh.mu.
func hydrateFenced(sh *shard, id FileID, notAfter uint64) bool {
	return notAfter > 0 && (notAfter <= sh.hydrateFence[id] || notAfter <= sh.evictedFenceFloor)
}

// SeedCold registers a byte range as remote-durable-but-not-local: a read of it
// reports cold so the engine hydrates it from the remote store instead of
// zero-filling. Snapshot restore seeds the restored FileChunk manifest's extents
// this way after the local tier was wiped, and an upgrade that archives a
// pre-journal layout aside seeds the surviving manifest the same way — the bytes
// live in remote, addressed by that manifest. The caller (remote-backed shares
// only) guarantees the range is remotely backed; a hydrate replaces the seeded
// cold interval with the fetched warm bytes on first read.
//
// The markers are persisted before they are indexed: an in-memory-only seed would
// be lost on the next restart, turning the ranges back into holes that read as
// zeros with no fetch. Extents are taken a whole file at a time so seeding a
// manifest costs one fsync per file rather than one per extent; SeedColdBatch
// takes many files at a time and costs one for the batch.
//
// Only the parts of each extent the file index does not already cover are
// seeded. A cold interval carries a fresh version, so seeding over a range the
// journal holds locally would shadow those bytes — including bytes not yet on
// the remote, which a cold read cannot fetch back. Skipping covered ranges makes
// the call idempotent and safe to repeat against a store that is only partly
// missing its markers, at the cost of nothing on a store that has none.
//
// The whole call runs under the file's shard lock, so a Delete or Truncate of
// the same file either lands entirely before the seed — and the seed then plans
// against the index it left — or entirely after, where its version fences the
// seeded intervals away like any other write. Neither can land in the middle and
// have the insert undo it, which is what makes this the variant a live file can
// be seeded through: a server-side copy seeds its destination while other
// clients are free to unlink it.
//
// What the lock does not do is make the caller's extents current. They were
// chosen before the call, so a truncate that lands first leaves the seed
// describing a range the file no longer has — a hole is a hole whether it was
// never written or just clipped away. That range is past the size every reader
// clamps to, so it serves nothing and only ever inflates the count of bytes the
// tier calls remote-only. Erring toward "more remote-only than there is" is the
// safe direction for every caller of that count.
//
// ponytail: the cold-log fsync runs under the shard lock, so it stalls that
// shard for its duration — the same trade Invalidate takes, and for the same
// reason: one fsync per server-side copy is too rare to show. Take
// SeedColdBatch's plan/append/insert split instead if a workload ever seeds live
// files often enough to matter, and pay for the delete watermark it needs.
func (s *Store) SeedCold(_ context.Context, id FileID, extents [][2]int64) error {
	if s.closed.Load() {
		return errClosed
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	entries := s.planColdSeed(sh, id, extents)
	if len(entries) == 0 {
		return nil
	}
	if err := s.appendCold(entries); err != nil {
		return err
	}
	fi := sh.indexFor(id)
	for _, e := range entries {
		fi.insert(interval{
			fileOff: e.fileOff,
			length:  e.length,
			version: e.version,
			synced:  true,
			cold:    true,
		})
	}
	return nil
}

// planColdSeed turns one file's extents into the cold entries that would cover
// the parts of them the index does not describe yet. The caller holds the file's
// shard lock, and the entries carry the versions taken under it.
func (s *Store) planColdSeed(sh *shard, id FileID, extents [][2]int64) []coldEntry {
	var entries []coldEntry
	fi := sh.index[id]
	for _, e := range extents {
		if e[1] <= 0 {
			continue
		}
		if fi == nil { // unknown file: the whole extent is a hole
			entries = append(entries, coldEntry{id: id, fileOff: e[0], length: e[1], version: s.nextVersion()})
			continue
		}
		for _, p := range fi.plan(e[0], e[1]) {
			if !p.hole {
				continue
			}
			entries = append(entries, coldEntry{
				id:      id,
				fileOff: e[0] + p.dstStart,
				length:  p.dstEnd - p.dstStart,
				version: s.nextVersion(),
			})
		}
	}
	return entries
}

// ColdSeed is one file's worth of work for SeedColdBatch: the file's ID and the
// {offset, length} extents to mark cold, exactly as SeedCold takes them.
type ColdSeed struct {
	ID      FileID
	Extents [][2]int64
}

// SeedColdBatch is SeedCold over many files, with one durable append for the
// whole batch instead of one per file. Seeding a manifest is otherwise an fsync
// per file, which is nearly all of its wall clock on a share with many small
// files.
//
// Batching is safe here and only here: seeding takes nothing away, so an
// interrupted batch leaves the store where it started — the ranges stay holes,
// no ColdSeeded marker is written, and the next start seeds them again. Eviction
// cannot borrow this, because it unlinks the bytes its entries describe.
//
// Callers bound their own batches: every entry is held in memory until the
// append, so a whole manifest at once trades the fsyncs for a proportional heap.
//
// ponytail: an entry's version is taken while planning and the shard lock is
// dropped for the append, so a Delete landing in that gap sweeps the index at a
// version above the pending entry and the insert below recreates the range it
// buried. The gap is not new — a single-file seed has it too — but a batch holds
// it open for the whole batch rather than one fsync. Both callers seed a share
// that is not serving yet (share add) or one whose local tier was just reset
// (snapshot restore), so nothing deletes through it today. Closing it means
// either holding shard locks across the fsync or a per-file delete watermark to
// revalidate against; build the watermark if a batch ever runs against a live
// share. A caller seeding one live file takes SeedCold instead, which holds the
// lock across its own fsync and has no gap to close.
func (s *Store) SeedColdBatch(_ context.Context, seeds []ColdSeed) error {
	if s.closed.Load() {
		return errClosed
	}
	var entries []coldEntry
	for _, sd := range seeds {
		sh := s.shardFor(sd.ID)
		sh.mu.Lock()
		entries = append(entries, s.planColdSeed(sh, sd.ID, sd.Extents)...)
		sh.mu.Unlock()
	}
	if len(entries) == 0 {
		return nil
	}
	// Unlike eviction's append, this one may be batched across files: no local
	// copy is being unlinked, and an interrupted seed leaves no ColdSeeded marker
	// so it simply repeats. That batching belongs on this side of the call, by
	// accumulating entries before it — never inside appendCold.
	if err := s.appendCold(entries); err != nil {
		return err
	}
	for _, e := range entries {
		sh := s.shardFor(e.id)
		sh.mu.Lock()
		sh.indexFor(e.id).insert(interval{
			fileOff: e.fileOff,
			length:  e.length,
			version: e.version,
			synced:  true,
			cold:    true,
		})
		sh.mu.Unlock()
	}
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
// commits pays a full disk barrier.
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
	// Ceiling for this fsync: every record at or below it finished its write under
	// the same lock, so the fsync covers all of them. Records appended after this
	// read are simply not claimed — conservative in the only safe direction.
	upTo := sh.lastVersion
	sh.mu.Unlock()
	err := sh.segSync(seg)
	if err != nil {
		sh.syncFailed.Store(true)
	} else {
		sh.markSynced(upTo)
	}

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
		DiskBytes:     s.diskBytes.Load(),
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

// DurableExtent reports how far a file's bytes survive device loss: the maximum
// end offset over the intervals whose bytes are already on stable storage. An
// interval qualifies when a completed fsync covered its record (Version at or
// below the shard's durable watermark), when it is cold (the bytes live in the
// remote store and the marker was fsynced before it was indexed), or when it is
// synced (carved and uploaded). Everything else was only buffered —
// acknowledged to the client, but gone after a crash.
//
// It is the counterpart of FileSize, which reports every interval whether or not
// its bytes are durable. Callers that publish a size derived from written bytes
// use this one so the published size can never describe bytes a crash would take
// away, which would leave the range reading as a hole full of zeros.
func (s *Store) DurableExtent(_ context.Context, id FileID) (int64, bool) {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return 0, false
	}
	synced := sh.syncedVersion.Load()
	var size int64
	for _, iv := range fi.ivs {
		// Intervals are sorted by offset and non-overlapping, but durability is
		// keyed on Version, which is append order — a later write to an earlier
		// offset is common. So the scan stops at the first interval whose bytes
		// could vanish rather than skipping over it: anything beyond it may be
		// durable on its own, yet reporting it would put the lost range inside
		// the committed size, which is the hole-of-zeros this exists to prevent.
		// Gaps between durable intervals are a different thing entirely — never
		// written, correctly read as zeros — so they do not stop the scan.
		if !iv.cold && !iv.synced && iv.version > synced {
			break
		}
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

// FileCount reports how many files the journal indexes. It is what a caller that
// only wants the count should use: ListFiles builds and returns a slice of every
// FileID to answer the same question, which on a large store allocates (and
// grows) a slice under every shard lock the read path also takes.
func (s *Store) FileCount() int {
	n := 0
	for _, sh := range s.shards {
		sh.mu.Lock()
		n += len(sh.index)
		sh.mu.Unlock()
	}
	return n
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
	if past {
		// Published before the marker is stamped, so a hydrate that samples its
		// bound after this point is never mistaken for one that predates the clip.
		sh.hydrateFence[id] = s.version.Load()
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
// point-in-time view.
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
// cannot be deleted.
//
// Two phases:
//
//  1. Ceiling replay: scan every on-disk record and rebuild each file's coverage
//     as of V — data records with Version<=V resolved newest-wins, tombstones and
//     truncate markers with Version<=V honored, everything above V ignored. The
//     pre-overwrite records survive because a live snapshot pinned their segments.
//  2. Re-materialize: for each file, read the V-view bytes from their pinned
//     source records and re-append them as fresh dirty records at the head (a
//     tombstone first to bury the current head, then the V-view data), then fsync
//     every shard still holding unsynced records — a superset of the shards this
//     pass wrote. Fresh versions exceed everything, so recover() rebuilds V on
//     reopen; a file present at head but absent at V is tombstoned away.
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
						recOff:  rec.segOff,
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
			// Re-materializing rewrites these bytes as fresh records under a fresh
			// CRC, so they are verified against the source record first — restoring
			// a snapshot must not turn on-disk bit rot into trusted content.
			rec, rerr := readVerifiedRecord(seg.fd, iv.recOff, s.cfg.SegmentSize, id, nil)
			if rerr != nil {
				return fmt.Errorf("journal: restore: read %q@%d from segment %d: %w", id, iv.fileOff, iv.loc.SegmentID, rerr)
			}
			src, ok := rec.payloadRange(iv.loc.Offset, iv.length)
			if !ok {
				return fmt.Errorf("journal: restore: segment %d record %d does not frame %q@%d+%d: %w",
					iv.loc.SegmentID, iv.recOff, id, iv.fileOff, iv.length, errTornRecord)
			}
			exts = append(exts, extent{off: iv.fileOff, data: append([]byte(nil), src...)})
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

	// Every tombstone fsynced itself, but the V-view data went in through WriteAt,
	// which only buffers. A crash before the next commit would leave each burial
	// durable and its replacement not, and the restored files would read empty.
	// The sweep covers every shard still holding unsynced records rather than the
	// last one written: a restore's files hash across shards, and a shard already
	// at its durable watermark is skipped, so the superset costs nothing.
	if err := s.commitDirtyShards(); err != nil {
		return fmt.Errorf("journal: restore: commit re-materialized view: %w", err)
	}
	return nil
}

// Invalidate marks the live synced intervals overlapping [off, off+length) cold:
// their local bytes are unusable, but the range is still durable remotely, so a
// read of it fetches instead of serving what is there. A whole interval is
// demoted even when the range covers only part of it, because the record it
// points into is the unit that failed.
//
// A dirty interval is left alone. Nothing has uploaded it, so there is no copy
// to fetch back, and demoting it would turn unwritten-back data into zeros; a
// read of it fails closed instead.
//
// This is what lets a hydrate stay a fill rather than an overwrite: a caller
// that has proven the local bytes bad demotes them first, and the re-fetch then
// lands in a range the journal no longer claims to hold.
//
// The markers are persisted to the cold log before the flip. This is the one
// path that marks an interval cold while its record is still live in a segment,
// and every path that reclaims a segment — eviction, the emptied-segment sweep,
// GC repack — treats a cold interval as owning nothing there, so without a
// durable marker the reclaim unlinks the only copy and a restart finds the range
// a hole that reads zeros. Persisting first can at worst leave a marker for
// bytes that are still local, which costs a needless remote fetch.
//
// A failed append leaves the interval warm and returns the error, so the caller's
// read fails closed rather than proceeding on a demotion the store cannot keep.
func (s *Store) Invalidate(_ context.Context, id FileID, off, length int64) error {
	if s.closed.Load() {
		return errClosed
	}
	if length <= 0 {
		return nil
	}
	end := off + length
	sh := s.shardFor(id)
	// ponytail: the cold-log fsync runs under the shard lock, so it stalls the
	// shard for its duration. This path only fires on a record that failed its
	// checksum, so the contention is bounded by how often the local tier rots;
	// split it into scan/append/flip like evictSegment if that stops being rare.
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return nil
	}
	var (
		entries []coldEntry
		hits    []int
	)
	for k := range fi.ivs {
		iv := &fi.ivs[k]
		if iv.end() <= off || iv.fileOff >= end || iv.cold || !iv.synced {
			continue
		}
		hits = append(hits, k)
		entries = append(entries, coldEntry{
			id:      id,
			fileOff: iv.fileOff,
			length:  iv.length,
			version: iv.version,
		})
	}
	if len(entries) == 0 {
		return nil
	}
	if err := s.appendCold(entries); err != nil {
		return err
	}
	for _, k := range hits {
		fi.ivs[k].cold = true
	}
	return nil
}
