package journal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
)

// restoreFixture is a store whose fsyncs are tracked so a test can throw away
// every segment byte no completed fsync covered — precisely what a power cut
// takes. Reopening the same directory proves nothing on its own: the page cache
// serves buffered writes back exactly like fsynced ones, so a plain reopen
// cannot tell a durable record from one that only reached the page cache.
type restoreFixture struct {
	*Store
	t   *testing.T
	dir string
	cfg Config

	mu sync.Mutex
	// durable maps a segment id to the tail some completed fsync covered.
	durable map[uint64]int64
}

// newRestoreFixture opens a store over a fresh directory with shardCount shards
// and the dirty-age sync loop disabled, so the only fsyncs are the ones the code
// under test issues.
func newRestoreFixture(t *testing.T, shardCount int) *restoreFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{ShardCount: shardCount, DirtyExpiry: -1}
	s, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &restoreFixture{Store: s, t: t, dir: dir, cfg: cfg, durable: map[uint64]int64{}}
	for _, sh := range s.shards {
		inner := sh.segSync
		sh.segSync = func(seg *segmentMeta) error {
			// Sample the tail before the barrier: only bytes already written when
			// the fsync started are certainly covered by it.
			tail := seg.tail.Load()
			if err := inner(seg); err != nil {
				return err
			}
			f.mu.Lock()
			if tail > f.durable[seg.id] {
				f.durable[seg.id] = tail
			}
			f.mu.Unlock()
			return nil
		}
	}
	return f
}

// write appends data at off, the plain buffered client write.
func (f *restoreFixture) write(id FileID, off int64, data []byte) {
	f.t.Helper()
	if err := f.WriteAt(context.Background(), id, off, data); err != nil {
		f.t.Fatalf("WriteAt %s@%d: %v", id, off, err)
	}
}

// commitAll makes everything written so far durable and returns the watermark
// naming that point-in-time view, ready to hand to RestoreToVersion.
func (f *restoreFixture) commitAll() uint64 {
	f.t.Helper()
	for _, sh := range f.shards {
		if err := sh.groupCommit(); err != nil {
			f.t.Fatalf("groupCommit: %v", err)
		}
	}
	return f.JournalVersion()
}

// crashReopen models a power cut: every segment is cut back to the tail its last
// completed fsync covered and a fresh store is opened over the directory. Close
// is safe to call first because it issues no fsync of its own — it only stops the
// background loops and closes the fds.
func (f *restoreFixture) crashReopen() *Store {
	f.t.Helper()
	if err := f.Store.Close(); err != nil {
		f.t.Fatalf("Close: %v", err)
	}
	ids, err := scanSegmentIDs(f.dir)
	if err != nil {
		f.t.Fatalf("scanSegmentIDs: %v", err)
	}
	for _, id := range ids {
		path := f.segPath(id)
		fd, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			f.t.Fatalf("open segment %d: %v", id, err)
		}
		var hdr [segHeaderSize]byte
		if _, rerr := fd.ReadAt(hdr[:], 0); rerr != nil {
			_ = fd.Close()
			continue
		}
		_, _, flags, ok := decodeSegHeader(hdr[:])
		// A sealed segment fsyncs itself before it sets the sealed bit, so all of
		// it is durable and none of it is dropped.
		if ok && flags&segFlagSealed == 0 {
			f.mu.Lock()
			tail := f.durable[id]
			f.mu.Unlock()
			// The header is kept even when nothing fsynced this segment: it carries
			// no records, so keeping it costs the test nothing and spares recovery a
			// torn-create sweep that has no bearing on what is being asserted.
			if tail < segHeaderSize {
				tail = segHeaderSize
			}
			if err := fd.Truncate(tail); err != nil {
				_ = fd.Close()
				f.t.Fatalf("truncate segment %d to %d: %v", id, tail, err)
			}
		}
		if err := fd.Close(); err != nil {
			f.t.Fatalf("close segment %d: %v", id, err)
		}
	}
	r, err := Open(f.dir, f.cfg, newFakeRemote(), SystemClock())
	if err != nil {
		f.t.Fatalf("reopen after crash: %v", err)
	}
	f.t.Cleanup(func() { _ = r.Close() })
	return r
}

// idsOnDistinctShards returns one FileID per shard index in [0, n), so a test
// spanning them exercises a per-shard barrier rather than a single shard's.
func idsOnDistinctShards(t *testing.T, s *Store, n int) []FileID {
	t.Helper()
	ids := make([]FileID, n)
	found := 0
	for i := 0; found < n; i++ {
		if i > 10000 {
			t.Fatalf("could not find ids covering %d shards", n)
		}
		id := FileID(fmt.Sprintf("restore-%d", i))
		sh := s.shardIndex(id)
		if sh < uint64(n) && ids[sh] == "" {
			ids[sh] = id
			found++
		}
	}
	return ids
}

// TestRestoreToVersion_ReMaterializedViewSurvivesCrash pins the durability the
// method's doc promises: once RestoreToVersion returns, the re-materialized view
// is on stable storage. Each file's burial tombstone fsyncs itself, so without a
// barrier for the data that replaces it a crash keeps the burial and loses the
// replacement, and every restored file reads empty. The files span two shards,
// so a barrier that covered only the last shard written would still fail here.
func TestRestoreToVersion_ReMaterializedViewSurvivesCrash(t *testing.T) {
	const shards = 2
	f := newRestoreFixture(t, shards)
	ctx := context.Background()

	ids := idsOnDistinctShards(t, f.Store, shards)
	v1 := bytes.Repeat([]byte("first-version-"), 64)
	v2 := bytes.Repeat([]byte("second-version"), 64)

	// V1 across both shards, made durable so the restore's source bytes are never
	// themselves the thing at risk.
	for _, id := range ids {
		f.write(id, 0, v1)
	}
	target := f.commitAll()

	// V2 on top, also durable: the pre-restore state is what a crash must not
	// leave behind.
	for _, id := range ids {
		f.write(id, 0, v2)
	}
	f.commitAll()

	if err := f.RestoreToVersion(ctx, target); err != nil {
		t.Fatalf("RestoreToVersion: %v", err)
	}
	for _, id := range ids {
		if got := readAll(t, f.Store, id, len(v1)); !bytes.Equal(got, v1) {
			t.Fatalf("pre-crash %s: restore did not produce the V1 view", id)
		}
	}

	r := f.crashReopen()
	for _, id := range ids {
		got := make([]byte, len(v1))
		if _, _, err := r.ReadAt(ctx, id, 0, got); err != nil {
			t.Fatalf("post-crash ReadAt %s: %v", id, err)
		}
		if !bytes.Equal(got, v1) {
			empty := bytes.Equal(got, make([]byte, len(v1)))
			t.Fatalf("post-crash %s: restored view lost (reads-empty=%v, reads-V2=%v); "+
				"the burial tombstone was fsynced but its replacement data was not",
				id, empty, bytes.Equal(got, v2))
		}
	}
}
