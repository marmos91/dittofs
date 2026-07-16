package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// gatedChunkStore wraps a stubFileChunkStore so a download worker can be pinned
// mid-lookup inside GetFileChunkAtOffset (the exact call that panicked with
// "DB Closed" in #1722 when the badger metadata store was closed out from under
// a live prefetch worker). It also records any access that arrives AFTER the
// store is closed, which is what the real backend would turn into a panic.
type gatedChunkStore struct {
	*stubFileChunkStore

	entered   chan struct{} // signalled once a worker enters GetFileChunkAtOffset
	release   chan struct{} // closed by the test to let the pinned worker proceed
	enterOnce sync.Once

	closed         atomic.Bool // set by Close()
	accessedClosed atomic.Bool // set if the store is touched after Close()
}

func newGatedChunkStore() *gatedChunkStore {
	return &gatedChunkStore{
		stubFileChunkStore: newStubFileChunkStore(),
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
}

// GetFileChunkAtOffset makes gatedChunkStore satisfy chunkAtOffsetResolver, so
// resolveCovering routes the download worker through here. It gates the first
// caller until the test releases it, and flags any post-Close access.
func (g *gatedChunkStore) GetFileChunkAtOffset(_ context.Context, _ string, _ uint64) (*block.FileChunk, error) {
	if g.closed.Load() {
		// A real badger store panics ("DB Closed") here; record it so the test
		// fails deterministically instead of crashing the process.
		g.accessedClosed.Store(true)
		return nil, errors.New("gatedChunkStore: accessed after Close")
	}
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.release
	return nil, nil // resolve as a hole -> fetchBlock returns cleanly, no remote GET
}

func (g *gatedChunkStore) Close() error {
	g.closed.Store(true)
	return nil
}

// TestSyncerClose_JoinsInFlightDownloadWorker pins the shutdown ordering that
// #1722 violated: Syncer.Close() (via SyncQueue.Stop) must block until every
// in-flight download worker has left the metadata store, so the store can then
// be closed with no worker still reading it.
//
// It is fully deterministic (no reliance on CI slowness): a download worker is
// forced to block inside GetFileChunkAtOffset, and the test asserts Close() does
// not return while the worker is pinned, then returns once it is released, and
// that no store access occurs after the store is finally closed.
func TestSyncerClose_JoinsInFlightDownloadWorker(t *testing.T) {
	gs := newGatedChunkStore()

	cfg := DefaultConfig()
	cfg.ParallelDownloads = 2
	// A non-nil remote is required for fetchBlock to reach resolveFileChunk;
	// a nil remote short-circuits before the store lookup. No HealthMonitor is
	// started (we never call Syncer.Start), so IsRemoteHealthy() is true.
	syncer := NewSyncer(memorylocal.New(), remotememory.New(), gs, cfg)

	syncer.Queue().Start(context.Background())

	if !syncer.Queue().EnqueueDownload(TransferRequest{PayloadID: "payload", BlockIndex: 0}) {
		t.Fatal("EnqueueDownload returned false (queue full)")
	}

	// Wait until a worker is pinned inside GetFileChunkAtOffset.
	select {
	case <-gs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download worker never reached GetFileChunkAtOffset")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = syncer.Close()
		close(closeDone)
	}()

	// Close() must NOT return while the worker is still inside the store. If it
	// did, the metadata store could be closed under a live worker -> #1722.
	select {
	case <-closeDone:
		t.Fatal("Syncer.Close returned before the in-flight download worker finished — SyncQueue.Stop did not join workers (regression of #1722)")
	case <-time.After(150 * time.Millisecond):
	}

	// Let the pinned worker finish; the store is still open, so this access is
	// legal and must not set the post-Close flag.
	close(gs.release)

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Syncer.Close did not return after the worker was released")
	}

	// Only now — after Close() has joined all workers — is it safe to close the
	// metadata store, matching the corrected engine/test teardown ordering.
	_ = gs.Close()

	if gs.accessedClosed.Load() {
		t.Fatal("metadata store was accessed after Close — download worker outlived Syncer.Close (#1722)")
	}
}
