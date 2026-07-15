package segstore

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
}

func newShard(active *segmentMeta) *shard {
	return &shard{
		active: active,
		sealed: make(map[uint64]*segmentMeta),
		index:  make(map[FileID]*fileIndex),
	}
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

// shardFor returns the shard owning id: FNV-1a masked to the power-of-two
// shard count.
func (s *Store) shardFor(id FileID) *shard {
	return s.shards[fnv1a(string(id))&s.shardMask]
}
