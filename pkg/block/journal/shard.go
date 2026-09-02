package journal

import (
	"sync"
	"sync/atomic"
)

// FNV-1a constants (64-bit), matching fs/logshard.go so a FileID resolves to
// the same partition as today's payloadID.
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnv1a hashes s with FNV-1a. Used both to pick a shard and to fill the .idx
// FileIDHash column.
func fnv1a(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// shard owns one stripe of the FileID keyspace: its single append-target
// segment, the sealed segments still readable, and the per-file interval index.
// One mutex serializes appends and index mutation; positioned reads snapshot
// under it then pread unlocked.
type shard struct {
	mu     sync.Mutex
	active *segmentMeta
	sealed map[uint64]*segmentMeta
	index  map[FileID]*fileIndex
	// hydrateFence is, per file, the store LSN as it stood when that file's most
	// recent truncate or delete began. A hydrate whose caller sampled its bound
	// at or below it is refused: both mutations empty intervals away rather than
	// recording over them, so the range they cleared leaves nothing for
	// hydratable to weigh a stale write-back against — a delete leaves not even
	// an index entry, and hydratable's nil receiver offers the whole range.
	//
	// A delete's entry therefore has to outlive the file's index entry, and
	// deleteFences is what keeps that from growing without bound.
	hydrateFence map[FileID]uint64
	// deleteFences records the fences stamped by Delete, oldest first. Nothing
	// else ever takes a delete fence back out of hydrateFence, so without this
	// the map would retain one entry per FileID the store has ever deleted.
	//
	// ponytail: a flat FIFO capped at maxDeleteFences per shard, evicting the
	// oldest fence rather than the one that has outlived every hydrate that
	// could still cite it. A fence only has to survive one in-flight remote
	// fetch, so overflowing the cap takes maxDeleteFences deletes into a single
	// shard inside that window; make it an age-based sweep only if a workload
	// is measured doing that.
	deleteFences []fenceEntry
	// lastVersion is the highest record Version appended to this shard, stamped
	// under mu once the record's write() returned. syncedVersion is the highest
	// Version a completed fsync has covered — records above it exist only in the
	// page cache and vanish on device loss; it is atomic because groupCommit
	// raises it after releasing mu. DurableExtent reads both.
	lastVersion   uint64
	syncedVersion atomic.Uint64
	// syncFailed goes true the first time an fsync on this shard fails and never
	// clears, freezing syncedVersion where it stood. It mirrors the stickiness of
	// errSeq/syncErr and for the same reason: once the kernel has reported a
	// write-back failure it drops those pages, and the next fsync can return
	// success without them ever having reached the device.
	syncFailed atomic.Bool
	// carveMu serializes a shard's carve passes: the background flush and an
	// explicit Carve() never build a block from the same records twice. It is
	// distinct from mu, which serializes appends and index mutation — carve holds
	// carveMu across its whole pass but only grabs mu briefly to snapshot and flip.
	carveMu sync.Mutex

	// Group-commit state (all under commitMu). Coalesces the burst of concurrent
	// Commits a high-iodepth durable-write workload issues (fio rand-write-4k runs
	// iodepth=32 × numjobs=4) into a single fsync: one leader fsyncs the shard's
	// active fd — which flushes every byte written to it so far — and satisfies
	// every commit that enqueued before the leader started. Segment rotation is
	// itself a durability point (sealInPlace fsyncs the sealed segment), so a
	// commit whose bytes moved to a now-sealed segment is durable regardless of
	// which fd the leader synced. See Store.Commit.
	commitMu   sync.Mutex
	commitCond *sync.Cond
	reqSeq     uint64 // commits enqueued so far (monotonic)
	doneSeq    uint64 // commits released by a completed fsync, any outcome (monotonic)
	syncing    bool   // a leader is mid-fsync
	// errSeq is the highest batchUpTo whose fsync ERRORED; syncErr is that error.
	// Both are sticky — never cleared by a later successful batch — so a waiter
	// covered by an errored batch (its gen < errSeq) always reads the failure
	// instead of a newer batch's nil. Under Linux fsync-error semantics a post-error
	// fsync can report success for pages the kernel already dropped, so a false
	// "success" would be silent data loss; that is the direction we must never take.
	// The cost is a rare, benign spurious error to a waiter a later success actually
	// covered (safe direction). See groupCommit.
	errSeq  uint64
	syncErr error
	// segSync fsyncs a segment's backing file. Per-shard indirection so durability
	// tests can substitute a spy that counts syncs and can be forced to fail — the
	// group-commit's whole guarantee rests on this call, so it needs a seam a test
	// can neutralize. Production uses the real (*os.File).Sync via seg.fd.
	segSync func(*segmentMeta) error
}

// maxDeleteFences caps how many delete fences one shard retains. See
// shard.deleteFences.
const maxDeleteFences = 4096

// fenceEntry is one delete fence as it was stamped. The version is kept so an
// eviction leaves alone a fence a later truncate has since re-stamped.
type fenceEntry struct {
	id  FileID
	ver uint64
}

func newShard(active *segmentMeta) *shard {
	sh := &shard{
		active:       active,
		sealed:       make(map[uint64]*segmentMeta),
		index:        make(map[FileID]*fileIndex),
		hydrateFence: make(map[FileID]uint64),
		segSync:      func(seg *segmentMeta) error { return seg.fd.Sync() },
	}
	sh.commitCond = sync.NewCond(&sh.commitMu)
	return sh
}

// fenceDelete publishes id's delete fence at ver and drops the oldest fence
// once the shard holds more than maxDeleteFences of them. Caller holds sh.mu.
func (sh *shard) fenceDelete(id FileID, ver uint64) {
	if ver > sh.hydrateFence[id] {
		sh.hydrateFence[id] = ver // never lower a fence a racing Truncate stamped higher
	}
	sh.deleteFences = append(sh.deleteFences, fenceEntry{id: id, ver: ver})
	if len(sh.deleteFences) > maxDeleteFences {
		oldest := sh.deleteFences[0]
		sh.deleteFences = sh.deleteFences[1:]
		// Only if nothing has raised the fence since. A Truncate re-stamp belongs
		// to a file that is live again and still needs its fence; that entry is
		// then bounded by the live file set, as every truncate fence always was.
		if sh.hydrateFence[oldest.id] <= oldest.ver {
			delete(sh.hydrateFence, oldest.id)
		}
	}
}

// markSynced raises the shard's durable watermark to v, which must be a Version
// an already-completed fsync covered. Monotonic: a seal and an in-flight commit
// finishing out of order can never walk the watermark back over durable records.
func (sh *shard) markSynced(v uint64) {
	if sh.syncFailed.Load() {
		return
	}
	for {
		cur := sh.syncedVersion.Load()
		if v <= cur || sh.syncedVersion.CompareAndSwap(cur, v) {
			return
		}
	}
}

// dirty reports whether the shard holds appended records that no completed
// fsync has covered. A shard whose fsync already failed is never dirty:
// syncFailed freezes syncedVersion for good, so retrying would fsync every
// pass without the watermark ever advancing.
func (sh *shard) dirty() bool {
	if sh.syncFailed.Load() {
		return false
	}
	sh.mu.Lock()
	last := sh.lastVersion
	sh.mu.Unlock()
	return last > sh.syncedVersion.Load()
}

// segment returns the segment with the given ID, active or sealed, or nil.
// Caller must hold sh.mu.
func (sh *shard) segment(id uint64) *segmentMeta {
	if sh.active != nil && sh.active.id == id {
		return sh.active
	}
	return sh.sealed[id]
}

// indexFor returns the file's interval index, creating it if absent.
// Caller must hold sh.mu.
func (sh *shard) indexFor(id FileID) *fileIndex {
	fi := sh.index[id]
	if fi == nil {
		fi = &fileIndex{}
		sh.index[id] = fi
	}
	return fi
}

// shardIndex returns the shard slot owning id. Recovery needs the slot before
// s.shards is populated, so it is factored out of shardFor.
func (s *Store) shardIndex(id FileID) uint64 { return fnv1a(string(id)) & s.shardMask }

// shardFor returns the shard owning id: FNV-1a masked to the power-of-two
// shard count.
func (s *Store) shardFor(id FileID) *shard {
	return s.shards[s.shardIndex(id)]
}
