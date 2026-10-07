package journal

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// TestDeleteDoesNotWaitForAFlushPass holds a flush pass open the way an upload
// to an unreachable remote does, then deletes another file in the same shard.
// The delete must return without waiting for the pass: the protocol request
// behind it (an SMB CLOSE, an NFS REMOVE) is held for as long as Delete is.
// The segment the delete emptied must still be reclaimed once the pass ends.
func TestDeleteDoesNotWaitForAFlushPass(t *testing.T) {
	const doomed, uploading = FileID("bb"), FileID("cc")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	// Segment 0 holds only the file to delete, filled to the brim so the next
	// write seals it, then marked synced: once the file is gone, the reclaim at
	// the end of Delete can retire it.
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
	sh := s.shardFor(doomed)
	sealed := func() int {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return len(sh.sealed)
	}
	sh.mu.Lock()
	for _, seg := range sh.sealed {
		seg.syncedRecords.Store(seg.records.Load())
	}
	sh.mu.Unlock()
	if n := sealed(); n != 1 {
		t.Fatalf("setup no longer seals the deleted file's segment on its own: %d sealed segments", n)
	}

	// A flush pass on the other file whose upload does not return until
	// released, as when the remote accepts no uploads.
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	flushed := make(chan error, 1)
	go func() {
		flushed <- s.Flush(ctx, uploading, FlushOptions{Force: true}, func(context.Context, Run) ([]Extent, error) {
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

	deleted := make(chan error, 1)
	go func() { deleted <- s.Delete(ctx, doomed) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("Delete %q: %v", doomed, err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Delete waited for a flush pass on another file in its shard: it is held as long as that pass's upload")
	}

	close(release)
	if err := <-flushed; err != nil {
		t.Fatalf("Flush %q: %v", uploading, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for sealed() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the segment the delete emptied was not reclaimed after the flush pass ended: %d sealed segments left", sealed())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
