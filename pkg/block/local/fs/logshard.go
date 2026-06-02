package fs

import "sync"

// numLogShards is the stripe count for per-payload append-log state. A power
// of two so shardFor can mask instead of modulo. 16 cuts the create-path
// write-lock contention (#680 §5.1 C2) by ~16x while keeping the per-shard
// map overhead negligible.
const numLogShards = 16

// logShard holds the per-payload append-log maps for one stripe of the
// payloadID keyspace, guarded by its own RWMutex. Every map is keyed by
// payloadID, and a payloadID maps to exactly one shard (FSStore.shardFor), so
// all of a payload's state lives under a single shard lock. This preserves the
// #668 invariant that a payload's interval tree and logIndex are always
// created — and observed — under the same lock.
type logShard struct {
	mu             sync.RWMutex
	logFDs         map[string]*logFile      // open log fd wrapper (one fd per file, bypasses fdPool)
	logLocks       map[string]*sync.Mutex   // per-file append mutex
	rollupLocks    map[string]*sync.Mutex   // per-file rollup mutex (C1)
	dirtyIntervals map[string]*intervalTree // dirty-region tree
	logIndices     map[string]*logIndex     // log-position oracle (Direction-1)
	tombstones     map[string]struct{}      // payloads being deleted by DeleteAppendLog
	truncations    map[string]uint64        // truncation boundaries set by TruncateAppendLog
}

// newLogShard allocates an empty shard with all maps initialized.
func newLogShard() *logShard {
	return &logShard{
		logFDs:         make(map[string]*logFile),
		logLocks:       make(map[string]*sync.Mutex),
		rollupLocks:    make(map[string]*sync.Mutex),
		dirtyIntervals: make(map[string]*intervalTree),
		logIndices:     make(map[string]*logIndex),
		tombstones:     make(map[string]struct{}),
		truncations:    make(map[string]uint64),
	}
}

// shardFor returns the logShard that owns payloadID. FNV-1a over the
// payloadID bytes, masked to numLogShards (a power of two). The hash is
// deterministic, so a payloadID always resolves to the same shard for the
// FSStore's lifetime.
func (bc *FSStore) shardFor(payloadID string) *logShard {
	const (
		fnvOffset64 = 14695981039346656037
		fnvPrime64  = 1099511628211
	)
	h := uint64(fnvOffset64)
	for i := 0; i < len(payloadID); i++ {
		h ^= uint64(payloadID[i])
		h *= fnvPrime64
	}
	return bc.logShards[h&(numLogShards-1)]
}
