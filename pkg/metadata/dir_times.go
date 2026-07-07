package metadata

import (
	"sync"
	"time"
)

// dirTimesFlushInterval bounds how often a directory's coalesced timestamps are
// persisted durably. Between flushes the bump lives only in memory (overlaid on
// reads), so a crash loses at most this window of directory-timestamp updates —
// the directory's children are always durable in their own transactions.
const dirTimesFlushInterval = 2 * time.Second

// dirTimes holds the coalesced, not-yet-durable timestamps for one directory.
type dirTimes struct {
	mtime, ctime, atime time.Time
	lastFlush           time.Time
}

// DirTimesTracker coalesces directory mtime/ctime/atime bumps that create and
// remove operations would otherwise write into the shared parent inode on every
// call. That per-op parent write is a BadgerDB SSI hot key: concurrent same-dir
// creates all read+write it and serialize on conflict-retry, so parallelism
// never helps (#1573). Recording the bump in memory instead lets the create/
// remove transactions touch only disjoint keys (so Badger group-commit batches
// them), while reads overlay the pending times to stay fresh, and a rate-limited
// flush persists them.
type DirTimesTracker struct {
	mu      sync.Mutex
	pending map[string]*dirTimes // dir handleKey -> coalesced times

	flushMu    sync.Mutex
	flushLocks map[string]*sync.Mutex // dir handleKey -> per-dir flush mutex

	interval time.Duration
}

// NewDirTimesTracker creates an empty tracker.
//
// ponytail: no background evictor. A directory that gets exactly one bump then
// goes idle keeps its pending entry until a later create/remove/setattr/rmdir
// flushes or clears it — bounded by the number of such dirs. If that footprint
// ever matters, add a periodic sweep that flushes+drops entries older than a
// few intervals (and a shutdown flush to make the last bumps durable).
func NewDirTimesTracker() *DirTimesTracker {
	return &DirTimesTracker{
		pending:    make(map[string]*dirTimes),
		flushLocks: make(map[string]*sync.Mutex),
		interval:   dirTimesFlushInterval,
	}
}

// RecordBump records a directory-timestamp bump to time t (latest wins) and
// reports whether a durable flush is now due (interval elapsed since the last
// persist for this directory).
func (d *DirTimesTracker) RecordBump(handle FileHandle, t time.Time) (flushDue bool) {
	key := handleKey(handle)

	d.mu.Lock()
	defer d.mu.Unlock()

	dt, ok := d.pending[key]
	if !ok {
		// First bump since the last flush: seed lastFlush at t so the flush
		// timer starts now (the durable record already holds an older time).
		dt = &dirTimes{lastFlush: t}
		d.pending[key] = dt
	}
	if t.After(dt.mtime) {
		dt.mtime, dt.ctime, dt.atime = t, t, t
	}
	return t.Sub(dt.lastFlush) >= d.interval
}

// GetPending returns the coalesced pending times for a directory, if any.
func (d *DirTimesTracker) GetPending(handle FileHandle) (mtime, ctime, atime time.Time, ok bool) {
	key := handleKey(handle)

	d.mu.Lock()
	defer d.mu.Unlock()

	dt, ok := d.pending[key]
	if !ok {
		return time.Time{}, time.Time{}, time.Time{}, false
	}
	return dt.mtime, dt.ctime, dt.atime, true
}

// FlushLock returns a per-directory mutex serializing durable flushes for that
// directory (mirrors PendingWritesTracker.GetFlushLock).
func (d *DirTimesTracker) FlushLock(handle FileHandle) *sync.Mutex {
	key := handleKey(handle)

	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	mu, ok := d.flushLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		d.flushLocks[key] = mu
	}
	return mu
}

// Clear drops any pending coalesced times for a directory. Called when an
// explicit SetAttr persists directory timestamps: the stored record is now
// authoritative (it may deliberately set OLDER times, e.g. a timestamp restore),
// so a leftover create/remove bump must not resurrect via the read overlay.
func (d *DirTimesTracker) Clear(handle FileHandle) {
	key := handleKey(handle)

	d.mu.Lock()
	delete(d.pending, key)
	d.mu.Unlock()
}

// ClearIfFlushed drops the pending entry once flushedTo has been persisted and
// no newer bump has arrived, bounding the map to directories with genuinely
// un-flushed timestamps. A later bump simply re-creates the entry. If a newer
// bump raced in, the entry is kept (with lastFlush advanced) so the next flush
// picks it up.
func (d *DirTimesTracker) ClearIfFlushed(handle FileHandle, flushedTo time.Time) {
	key := handleKey(handle)

	d.mu.Lock()
	defer d.mu.Unlock()
	dt, ok := d.pending[key]
	if !ok {
		return
	}
	if !dt.mtime.After(flushedTo) {
		delete(d.pending, key)
		return
	}
	dt.lastFlush = flushedTo
}
