package badgerkv

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"
)

// syncer is the store's group commit. Badger runs with its own sync off, and
// every commit that has finished writing waits here for the next sync, which
// covers all of them at once: concurrent Updates share one sync, and none is
// acknowledged before it.
//
// A sync covers what Badger writes between two syncs, which is not only its
// active files: when a memtable or a value log fills, Badger starts a new file
// and leaves the old one's tail unsynced, and db.Sync covers only the active
// ones. So a sync fsyncs every memtable WAL and value-log file at or after the
// newest each previous sync covered, and the directory whenever a file was
// created since, which is what makes a new file's entry durable.
//
// ponytail: this reads Badger's file names (%05d.mem, %06d.vlog) and relies on
// fsync of a file writing back the pages Badger dirtied through its mapping,
// which holds on Linux. Replace it with Badger's own grouping if a release
// shares one WAL sync among the transactions of a batch.
type syncer struct {
	db  *badger.DB
	dir string

	// synced is a version every commit at or below which is durable.
	synced atomic.Uint64
	syncs  atomic.Int64 // syncs run, for tests

	mu      sync.Mutex
	waiting []chan error
	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}

	// from is, per file kind, the newest file the last sync covered; a sync
	// covers it and every newer one.
	from map[string]int
}

var fileKinds = []string{".mem", ".vlog"}

func newSyncer(db *badger.DB, dir string) (*syncer, error) {
	y := &syncer{
		db:   db,
		dir:  dir,
		kick: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		from: map[string]int{},
	}
	newest, err := y.newest()
	if err != nil {
		return nil, err
	}
	y.from = newest
	go y.loop()
	return y, nil
}

// wait returns once a sync that started after the call has completed.
func (y *syncer) wait() error {
	ch := make(chan error, 1)
	y.mu.Lock()
	y.waiting = append(y.waiting, ch)
	y.mu.Unlock()
	select {
	case y.kick <- struct{}{}:
	default:
	}
	return <-ch
}

func (y *syncer) close() {
	close(y.stop)
	<-y.done
}

func (y *syncer) loop() {
	defer close(y.done)
	for {
		select {
		case <-y.kick:
			y.syncWaiting()
		case <-y.stop:
			y.syncWaiting()
			return
		}
	}
}

func (y *syncer) syncWaiting() {
	y.mu.Lock()
	batch := y.waiting
	y.waiting = nil
	y.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	// A snapshot opened now waits until every commit at or below its version
	// has been written, so the sync below covers them all, and among them every
	// waiter, which joined only once its own commit was written.
	r := y.db.NewTransaction(false)
	ts := r.ReadTs()
	r.Discard()
	err := y.syncFiles()
	y.syncs.Add(1)
	if err == nil {
		y.synced.Store(ts)
	}
	for _, ch := range batch {
		ch <- err
	}
}

func (y *syncer) syncFiles() error {
	ents, err := os.ReadDir(y.dir)
	if err != nil {
		return err
	}
	newest := map[string]int{}
	created := false
	for _, e := range ents {
		kind, fid, ok := parseFile(e.Name())
		if !ok {
			continue
		}
		newest[kind] = max(newest[kind], fid)
		if fid < y.from[kind] {
			continue
		}
		// A file created since the last sync gets a full fsync, which covers
		// the size Badger gave it; an older one only needs its data.
		isNew := fid > y.from[kind]
		created = created || isNew
		if err := syncFile(filepath.Join(y.dir, e.Name()), !isNew); err != nil {
			return err
		}
	}
	if created {
		if err := syncDir(y.dir); err != nil {
			return err
		}
	}
	for _, kind := range fileKinds {
		y.from[kind] = max(y.from[kind], newest[kind])
	}
	return nil
}

func (y *syncer) newest() (map[string]int, error) {
	ents, err := os.ReadDir(y.dir)
	if err != nil {
		return nil, err
	}
	newest := map[string]int{}
	for _, e := range ents {
		if kind, fid, ok := parseFile(e.Name()); ok {
			newest[kind] = max(newest[kind], fid)
		}
	}
	return newest, nil
}

func parseFile(name string) (kind string, fid int, ok bool) {
	for _, k := range fileKinds {
		if base, found := strings.CutSuffix(name, k); found {
			n, err := strconv.Atoi(base)
			return k, n, err == nil
		}
	}
	return "", 0, false
}

// syncFile syncs path, only its data when dataOnly. A file gone since the
// listing was a memtable WAL Badger deleted once its memtable reached a synced
// table, so it has nothing left to sync. It opens the file for writing, which
// Windows needs to flush it; it writes nothing.
func syncFile(path string, dataOnly bool) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// The sync below is what says whether the data is durable; a close
	// after it changes nothing on disk.
	defer func() { _ = f.Close() }()
	if dataOnly {
		return datasync(f)
	}
	return f.Sync()
}
