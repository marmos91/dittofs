package engine

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// opKind is what a pending operation does to its file.
type opKind uint8

const (
	opWrite opKind = iota + 1
	opTruncate
	opDeallocate
)

// pendingOp is one operation the journal holds and block metadata has not
// recorded: its version, its kind, the extent it covers and the clock's time
// when it was staged. It never holds the operation's bytes, which the journal
// alone keeps.
type pendingOp struct {
	version   journal.Version
	kind      opKind
	extent    journal.Extent // a truncate's Off is the size it sets, its Len zero
	at        time.Time      // the clock's time when staged: the file's change time
	setsMtime bool           // the operation moves the file's modification time
	done      func()         // the write's WriteOpts.Done, called once its existence commits or it is dropped
}

// fileEntry is one file's state: its pending operations in version order.
// It exists only while it holds something.
type fileEntry struct {
	mu   sync.Mutex
	dead bool // removed from its shard: a caller that finds it so looks again
	ops  []pendingOp
}

// empty reports the entry holds nothing. The caller holds mu.
func (fe *fileEntry) empty() bool { return len(fe.ops) == 0 }

// fileShards is the count of shards the entry map is split into, each with
// its own lock held only to look up, insert or remove.
const fileShards = 64

type fileShard struct {
	mu sync.Mutex
	m  map[entity.FileID]*fileEntry
}

// files is the per-file state of every file with something pending, keyed by
// file: one entry per file, never a stripe two files share. A shard's lock is
// held to look up, insert or remove an entry only, never while an entry's lock
// is waited on or across a call out of the package.
//
// ponytail: per-file state lives while the file has something pending and is
// dropped after; its count is bounded by the files active in the last
// existence age plus those open. This bound is a proposal, not a measurement.
// Bound it explicitly, with eviction of idle entries, if a count of per-file
// states grows with the namespace rather than the working set.
type files struct {
	shards [fileShards]fileShard
	clock  Clock
	waits  lockWaits // area file_state
	// removals is bumped under an entry's lock whenever committed operations
	// leave it, so a reader that read block metadata and an entry apart can
	// tell a commit landed between them.
	//
	// ponytail: one counter for every file, so a commit to any file makes an
	// Allocation pass of another read again. Keep one per file, beside the
	// entry and outliving it, once those retries show in a profile.
	removals atomic.Uint64
}

func newFiles(c Clock) *files {
	fs := &files{clock: c}
	for i := range fs.shards {
		fs.shards[i].m = map[entity.FileID]*fileEntry{}
	}
	return fs
}

// shardOf is f's shard. A FileID is random, so its first bytes spread files
// evenly.
func (fs *files) shardOf(f entity.FileID) *fileShard {
	return &fs.shards[binary.LittleEndian.Uint64(f[:8])%fileShards]
}

// entryOf is f's entry, or nil when f has none. It never inserts one, so a
// lookup of a file with nothing pending allocates nothing.
func (fs *files) entryOf(f entity.FileID) *fileEntry {
	sh := fs.shardOf(f)
	sh.mu.Lock()
	fe := sh.m[f]
	sh.mu.Unlock()
	return fe
}

// entryFor is f's entry, inserted when f has none.
func (fs *files) entryFor(f entity.FileID) *fileEntry {
	sh := fs.shardOf(f)
	sh.mu.Lock()
	fe := sh.m[f]
	if fe == nil {
		fe = &fileEntry{}
		sh.m[f] = fe
	}
	sh.mu.Unlock()
	return fe
}

// lockOf is f's entry, locked, or nil when f has none: for a call that only
// reads.
func (fs *files) lockOf(f entity.FileID) *fileEntry {
	for {
		fe := fs.entryOf(f)
		if fe == nil {
			return nil
		}
		fs.lock(fe)
		if !fe.dead {
			return fe
		}
		fe.mu.Unlock()
	}
}

// lockFor is f's entry, locked, inserted when f has none: for a call that
// adds to it.
func (fs *files) lockFor(f entity.FileID) *fileEntry {
	for {
		fe := fs.entryFor(f)
		fs.lock(fe)
		if !fe.dead {
			return fe
		}
		fe.mu.Unlock()
	}
}

// unlock releases fe, f's entry, removing it from its shard once it holds
// nothing. The shard's lock is taken while fe's is held, never the other way.
func (fs *files) unlock(f entity.FileID, fe *fileEntry) {
	if fe.empty() {
		sh := fs.shardOf(f)
		sh.mu.Lock()
		if sh.m[f] == fe {
			delete(sh.m, f)
		}
		sh.mu.Unlock()
		fe.dead = true
	}
	fe.mu.Unlock()
}

// lock takes fe's lock, recording any wait for it in the file_state series.
func (fs *files) lock(fe *fileEntry) {
	if fe.mu.TryLock() {
		return
	}
	start := fs.clock.Elapsed()
	fs.waits.waiting.Add(1)
	fe.mu.Lock()
	fs.waits.waiting.Add(-1)
	fs.waits.record(fs.clock.Elapsed() - start)
}

// lockWaits is one area's engine_lock_wait_seconds series: the waits for its
// locks, counted and summed. A lock taken without waiting records nothing, so
// the count is of contended takes.
//
// ponytail: a count and a sum, not RFC 8 §14's histogram, so a scrape gives
// the mean contended wait but no tail. Add the buckets when the engine's
// metrics are exported, which is where an exporter first reads this.
type lockWaits struct {
	count   atomic.Int64
	total   atomic.Int64 // nanoseconds
	waiting atomic.Int64 // callers waiting now
}

func (w *lockWaits) record(d time.Duration) {
	w.count.Add(1)
	w.total.Add(int64(d))
}
