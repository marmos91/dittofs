package journal

import "sync"

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
	// which fd the leader synced. See Store.Commit (#1736).
	commitMu   sync.Mutex
	commitCond *sync.Cond
	reqSeq     uint64 // commits enqueued so far (monotonic)
	doneSeq    uint64 // commits made durable by a completed fsync (monotonic)
	syncing    bool   // a leader is mid-fsync
	syncErr    error  // error from the most recent completed fsync batch
}

func newShard(active *segmentMeta) *shard {
	sh := &shard{
		active: active,
		sealed: make(map[uint64]*segmentMeta),
		index:  make(map[FileID]*fileIndex),
	}
	sh.commitCond = sync.NewCond(&sh.commitMu)
	return sh
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
