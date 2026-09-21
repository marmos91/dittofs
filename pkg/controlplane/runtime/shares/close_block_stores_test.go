package shares

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gatedLocal holds a block store's close open until release is closed, and
// records when the close actually finished. Nothing can interrupt a close, so
// this is the only way a share is slow to shut down.
type gatedLocal struct {
	local.LocalStore
	release  chan struct{}
	finished atomic.Bool
}

func (g *gatedLocal) Close() error {
	<-g.release
	err := g.LocalStore.Close()
	g.finished.Store(true)
	return err
}

// newGatedShare registers one share whose block store cannot finish closing
// until the returned channel is closed.
func newGatedShare(t *testing.T) (*Service, *gatedLocal, chan struct{}) {
	t.Helper()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	release := make(chan struct{})
	gl := &gatedLocal{LocalStore: localmemory.New(), release: release}
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          gl,
		RemoteSync:     engine.NewRemoteSync(gl, nil, mds, engine.DefaultConfig()),
		FileChunkStore: mds,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	svc := New()
	svc.InjectShareForTesting(&Share{Name: "/gated", Enabled: true, BlockStore: bs})
	return svc, gl, release
}

// TestCloseBlockStores_ReturnsWhileAShareIsStillClosing pins the trade the
// bound exists to make. A share that will not finish closing would otherwise
// hold shutdown until the process hits its own self-exit deadline and is
// killed, leaving EVERY share's metadata store unclosed rather than just the
// wedged one's. The budget buys the others a clean close at the cost of that
// one's.
func TestCloseBlockStores_ReturnsWhileAShareIsStillClosing(t *testing.T) {
	svc, gl, release := newGatedShare(t)
	// Let the close finish once the assertions are done, so the store is not
	// left wedged for the rest of the package's run.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		svc.CloseBlockStores(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("CloseBlockStores did not return while a share was still closing; shutdown would never reach the metadata stores")
	}
	if gl.finished.Load() {
		t.Fatal("the share finished closing, so this run never exercised the expiry branch")
	}
}

// TestCloseBlockStores_WaitsForACloseThatFinishes is the other half: the bound
// must not cut short a close that would have completed, or every shutdown pays
// the expiry's cost instead of only a wedged one. Asserting the close FINISHED
// is the point — the store reports itself closed the moment teardown starts, so
// a return that merely happened after that proves nothing.
func TestCloseBlockStores_WaitsForACloseThatFinishes(t *testing.T) {
	svc, gl, release := newGatedShare(t)
	time.AfterFunc(300*time.Millisecond, func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc.CloseBlockStores(ctx)

	if !gl.finished.Load() {
		t.Fatal("CloseBlockStores returned before the share's close finished")
	}
}
