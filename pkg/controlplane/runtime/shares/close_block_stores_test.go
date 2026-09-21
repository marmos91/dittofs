package shares

import (
	"context"
	"fmt"
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

// newGatedShares registers n shares, each with a block store that cannot
// finish closing until its own release channel is closed. More than one share
// is the case the concurrent close exists for, so it is the case worth
// building: with a single share the fan-out never runs a second iteration.
func newGatedShares(t *testing.T, n int) (*Service, []*gatedLocal) {
	t.Helper()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	svc := New()
	gates := make([]*gatedLocal, 0, n)
	for i := range n {
		gl := &gatedLocal{LocalStore: localmemory.New(), release: make(chan struct{})}
		bs, err := engine.New(engine.BlockStoreConfig{
			Local:          gl,
			RemoteSync:     engine.NewRemoteSync(gl, nil, mds, engine.DefaultConfig()),
			FileChunkStore: mds,
		})
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		svc.InjectShareForTesting(&Share{Name: fmt.Sprintf("/share-%d", i), Enabled: true, BlockStore: bs})
		gates = append(gates, gl)
	}
	return svc, gates
}

// TestCloseBlockStores_ClosesEveryShareAndWaitsForThem exercises the fan-out
// with several shares at once and pins that the budget does not cut short a
// close that would have completed — otherwise every shutdown pays the expiry's
// cost rather than only a wedged one.
//
// Asserting the close FINISHED is the point: a store reports itself closed the
// moment teardown starts, so a return that merely happened after that proves
// nothing.
func TestCloseBlockStores_ClosesEveryShareAndWaitsForThem(t *testing.T) {
	svc, gates := newGatedShares(t, 3)

	// Staggered, so the goroutines finish at different times and the
	// bookkeeping is touched while the fan-out is still running.
	for i, gl := range gates {
		time.AfterFunc(time.Duration(i+1)*100*time.Millisecond, func() { close(gl.release) })
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc.CloseBlockStores(ctx)

	for i, gl := range gates {
		if !gl.finished.Load() {
			t.Errorf("share %d had not finished closing when CloseBlockStores returned", i)
		}
	}
}

// TestCloseBlockStores_ReturnsWhileSharesAreStillClosing pins the trade the
// bound exists to make. Shares that will not finish closing would otherwise
// hold shutdown until the process hits its own self-exit deadline and is
// killed, leaving EVERY share's metadata store unclosed rather than just
// theirs. The budget buys the rest a clean close at the cost of those.
func TestCloseBlockStores_ReturnsWhileSharesAreStillClosing(t *testing.T) {
	svc, gates := newGatedShares(t, 3)
	// One closes at once and two stay wedged, so the step has both something
	// to finish and something to give up on.
	close(gates[0].release)
	t.Cleanup(func() {
		close(gates[1].release)
		close(gates[2].release)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		svc.CloseBlockStores(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("CloseBlockStores did not return while shares were still closing; shutdown would never reach the metadata stores")
	}

	if !gates[0].finished.Load() {
		t.Error("the released share did not finish closing, so the fan-out did not run it")
	}
	if gates[1].finished.Load() || gates[2].finished.Load() {
		t.Fatal("every share finished closing, so this run never exercised the expiry branch")
	}
}
