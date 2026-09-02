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
	t *testing.T

	mu sync.Mutex
	// durable maps a segment id to the tail some completed fsync covered.
	durable map[uint64]int64
}

// newRestoreFixture opens a store over a fresh directory with shardCount shards.
// The dirty-age sync loop is disabled and the background repack is put out of
// reach (no segment's dead ratio can exceed 1), so the only fsyncs are the ones
// the code under test issues and no pass moves records between the segments the
// crash model tracks.
func newRestoreFixture(t *testing.T, shardCount int) *restoreFixture {
	t.Helper()
	s := testStore(t, Config{ShardCount: shardCount, DirtyExpiry: -1, GCDeadRatioForce: 2})
	f := &restoreFixture{Store: s, t: t, durable: map[uint64]int64{}}
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
			f.durable[seg.id] = max(f.durable[seg.id], tail)
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
// naming that point-in-time view, ready to hand to RestoreToVersion. It fsyncs
// the shards itself rather than calling the store's own dirty-shard sweep, so a
// test asserting that sweep runs cannot be set up by the very call it is pinning.
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
// is safe to call here because it issues no fsync of its own — it only stops the
// background loops and closes the fds — and it is idempotent, so the reopen
// helper's own Close is a no-op.
func (f *restoreFixture) crashReopen() *Store {
	f.t.Helper()
	if err := f.Close(); err != nil {
		f.t.Fatalf("Close: %v", err)
	}
	ids, err := scanSegmentIDs(f.dir)
	if err != nil {
		f.t.Fatalf("scanSegmentIDs: %v", err)
	}
	for _, id := range ids {
		fd, err := os.OpenFile(f.segPath(id), os.O_RDWR, 0o644)
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
			// The header is kept even when nothing fsynced this segment: it carries
			// no records, so keeping it costs the test nothing and spares recovery a
			// torn-create sweep that has no bearing on what is being asserted.
			tail := max(f.durable[id], int64(segHeaderSize))
			f.mu.Unlock()
			if err := fd.Truncate(tail); err != nil {
				_ = fd.Close()
				f.t.Fatalf("truncate segment %d to %d: %v", id, tail, err)
			}
		}
		if err := fd.Close(); err != nil {
			f.t.Fatalf("close segment %d: %v", id, err)
		}
	}
	return reopen(f.t, f.Store)
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
