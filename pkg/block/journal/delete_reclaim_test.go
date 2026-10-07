package journal

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

const doomed, uploading = FileID("bb"), FileID("cc")

// heldFlush is a store with one shard in the state the reclaim tests need:
// segment 0 holds only doomed, sealed and fully synced, so deleting doomed
// empties a segment a reclaim can retire; and a flush pass on uploading is
// held inside its upload callback, holding the shard's flushMu, the way an
// upload to an unreachable remote does. release ends the pass; it is also
// called at cleanup, so a test that fails early never leaves Close waiting
// for flushMu behind a pass nobody releases.
type heldFlush struct {
	s       *Store
	sh      *shard
	release func()
	flushed chan error
}

func newHeldFlush(t *testing.T) *heldFlush {
	t.Helper()
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	// Filled to the brim so the next write seals it.
	full := bytes.Repeat([]byte("d"), int(s.cfg.SegmentSize-segHeaderSize-recordLen(len(doomed), 0)))
	if err := s.WriteAt(ctx, doomed, 0, full); err != nil {
		t.Fatalf("WriteAt %q: %v", doomed, err)
	}
	if err := s.Commit(ctx, doomed); err != nil {
		t.Fatalf("Commit %q: %v", doomed, err)
	}
	if err := s.WriteAt(ctx, uploading, 0, []byte("dirty bytes the pass is uploading")); err != nil {
		t.Fatalf("WriteAt %q: %v", uploading, err)
	}
	h := &heldFlush{s: s, sh: s.shardFor(doomed), flushed: make(chan error, 1)}
	h.sh.mu.Lock()
	for _, seg := range h.sh.sealed {
		seg.syncedRecords.Store(seg.records.Load())
	}
	h.sh.mu.Unlock()
	if n := h.sealed(); n != 1 {
		t.Fatalf("setup no longer seals the deleted file's segment on its own: %d sealed segments", n)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	h.release = sync.OnceFunc(func() { close(release) })
	t.Cleanup(h.release)
	go func() {
		h.flushed <- s.Flush(ctx, uploading, FlushOptions{Force: true}, func(context.Context, Run) ([]Extent, error) {
			enterOnce.Do(func() { close(entered) })
			<-release
			return nil, nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the flush pass never reached its upload")
	}
	return h
}

func (h *heldFlush) sealed() int {
	h.sh.mu.Lock()
	defer h.sh.mu.Unlock()
	return len(h.sh.sealed)
}

// deleteDoomed deletes doomed and fails if Delete does not return while the
// flush pass is still held.
func (h *heldFlush) deleteDoomed(t *testing.T) {
	t.Helper()
	deleted := make(chan error, 1)
	go func() { deleted <- h.s.Delete(context.Background(), doomed) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("Delete %q: %v", doomed, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete waited for a flush pass on another file in its shard: it is held as long as that pass's upload")
	}
}

// TestDeleteDoesNotWaitForAFlushPass deletes a file while a flush pass on
// another file in the same shard is held in its upload. The delete must return
// without waiting for the pass: the protocol request behind it (an SMB CLOSE,
// an NFS REMOVE) is held for as long as Delete is. The segment the delete
// emptied must still be reclaimed once the pass ends.
func TestDeleteDoesNotWaitForAFlushPass(t *testing.T) {
	h := newHeldFlush(t)
	h.deleteDoomed(t)

	h.release()
	if err := <-h.flushed; err != nil {
		t.Fatalf("Flush %q: %v", uploading, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.sealed() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the segment the delete emptied was not reclaimed after the flush pass ended: %d sealed segments left", h.sealed())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCloseWaitsForAQueuedReclaim closes the store while the reclaim a Delete
// handed off is still queued behind a held flush pass. Close must not return
// while the pass holds flushMu, and once the pass ends it must return without
// the queued reclaim retiring anything: the store is closing, so the reclaim
// stops before touching a segment whose file Close is about to close.
func TestCloseWaitsForAQueuedReclaim(t *testing.T) {
	h := newHeldFlush(t)
	h.deleteDoomed(t)
	if !h.sh.reclaimQueued.Load() {
		t.Fatal("Delete during a held flush pass queued no reclaim")
	}

	closed := make(chan error, 1)
	go func() { closed <- h.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a flush pass held flushMu and a reclaim was queued behind it", err)
	case <-time.After(200 * time.Millisecond):
	}

	h.release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the flush pass ended")
	}
	if h.sh.reclaimQueued.Load() {
		t.Error("the queued reclaim never ran: reclaimQueued is still set after Close")
	}
	if n := h.sealed(); n != 1 {
		t.Errorf("the queued reclaim retired segments while the store closed: %d sealed segments, want 1", n)
	}
}
