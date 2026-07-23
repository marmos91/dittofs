package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metastore "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// blockingRemote models a remote whose GET has stalled after the pre-check
// health gate passed: ReadChunk parks until its context is cancelled, then
// reports the cancellation. A real S3 client eventually gives up only after its
// per-request timeout times its retry budget (minutes); this fake stands in for
// "the fetch does not return on any timescale a protocol client tolerates". A
// safety valve caps the park so a regression cannot wedge the whole suite.
type blockingRemote struct {
	*remotememory.Store
	started chan struct{}
	once    sync.Once
}

func newBlockingRemote() *blockingRemote {
	return &blockingRemote{Store: remotememory.New(), started: make(chan struct{})}
}

func (r *blockingRemote) ReadChunk(ctx context.Context, _ string, _, _ int64, _ block.ContentHash) ([]byte, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		return nil, errors.New("blockingRemote safety valve fired")
	}
}

var (
	_ remote.RemoteStore      = (*blockingRemote)(nil)
	_ remote.RemoteBlockStore = (*blockingRemote)(nil)
	_ remote.ChunkReader      = (*blockingRemote)(nil)
)

// TestNewSyncer_DefaultsDemandFetchTimeout guards against the bound being dead
// in production: a config that leaves DemandFetchTimeout unset (every path that
// does not thread the field, e.g. pkg/config) must still get the default, or
// EnsureAvailableAndRead would run unbounded and the hang would return.
func TestNewSyncer_DefaultsDemandFetchTimeout(t *testing.T) {
	fbs := newStubFileChunkStore()
	m := NewSyncer(memorylocal.New(), remotememory.New(), fbs, SyncerConfig{})
	if m.config.DemandFetchTimeout != DefaultDemandFetchTimeout {
		t.Fatalf("NewSyncer left DemandFetchTimeout = %v; want default %v",
			m.config.DemandFetchTimeout, DefaultDemandFetchTimeout)
	}
}

// TestColdRead_DemandFetchFailsFastWhenRemoteStalls pins the fix for the
// client-visible hang: a demand cold read must not block indefinitely on a
// stalled remote just because the caller's context carries no sub-deadline.
// EnsureAvailableAndRead bounds its own hydration to DemandFetchTimeout and
// surfaces ErrRemoteUnavailable when that budget is exceeded, so the protocol
// layer returns a fast error instead of the mount wedging. Before the fix the
// demand fan-out ran on the caller's unbounded context, so with a background
// context the read never returned.
func TestColdRead_DemandFetchFailsFastWhenRemoteStalls(t *testing.T) {
	ctx := context.Background() // a protocol read's context carries no sub-deadline

	loc := memorylocal.New()
	rs := newBlockingRemote()
	fbs := newStubFileChunkStore()
	mds := metastore.NewMemoryMetadataStoreWithDefaults()

	chunk := make([]byte, 4096)
	seedSyncedRemoteChunk(t, fbs, rs, mds, "p", 0, chunk)

	m := newFetchSyncer(loc, rs, fbs, mds)
	m.config.PrefetchBlocks = 0                          // isolate the demand loop
	m.config.DemandFetchTimeout = 200 * time.Millisecond // short fail-fast budget under test

	dest := make([]byte, 4096)
	done := make(chan error, 1)
	go func() {
		_, err := m.EnsureAvailableAndRead(ctx, "p", 0, uint32(len(dest)), dest)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, block.ErrRemoteUnavailable) {
			t.Fatalf("demand read returned %v; want ErrRemoteUnavailable (fail-fast on stalled remote)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demand read did not return within 5s while remote stalled — client-visible hang reproduced")
	}
}
