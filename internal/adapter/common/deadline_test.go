package common

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// shortRequestDeadline shortens requestDeadline for one test.
func shortRequestDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	old := requestDeadline
	requestDeadline = d
	t.Cleanup(func() { requestDeadline = old })
}

// stallingRemote is an in-memory remote whose chunk reads, once stall is set,
// wait until their context ends: a remote that accepts the request and never
// answers.
type stallingRemote struct {
	*remotememory.Store
	stall atomic.Bool
}

var _ remote.ChunkReader = (*stallingRemote)(nil)

func (r *stallingRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, h block.ContentHash) ([]byte, error) {
	if !r.stall.Load() {
		return r.Store.ReadChunk(ctx, blockID, offset, length, h)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// newDeadlineTestEngine is an engine on a journal configured by cfg, a Badger
// metadata store and rs as its remote (nil for none).
func newDeadlineTestEngine(t *testing.T, cfg journal.Config, rs remote.RemoteStore) *engine.Store {
	t.Helper()
	ms, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open badger metadata store: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	local, err := journal.Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	syncer := engine.NewRemoteSync(local, rs, ms, engine.DefaultConfig())
	if rb, ok := rs.(remote.RemoteBlockStore); ok {
		syncer.SetSyncedHashStore(ms)
		syncer.SetRemoteBlockStore(rb)
	}
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          local,
		Remote:         rs,
		RemoteSync:     syncer,
		FileChunkStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestWriteToBlockStore_CapacityWaitEndsAtRequestDeadline writes past a full
// local store whose own wait budget is an hour, with a request that carries no
// deadline, as protocol requests do. The write must end at the request
// deadline the choke point gives it, and its refusal must still be for space,
// so a protocol answers "no space" and not a generic failure.
func TestWriteToBlockStore_CapacityWaitEndsAtRequestDeadline(t *testing.T) {
	// Long enough that the writes below the cap never meet it under -race on a
	// loaded machine, short against the store's own hour-long budget.
	shortRequestDeadline(t, 2*time.Second)
	bs := newDeadlineTestEngine(t, journal.Config{
		MaxLocalBytes: 2 << 20,
		SegmentSize:   1 << 20,
		ShardCount:    1,
		EvictMaxWait:  time.Hour,
	}, nil)
	// No deadline, as a protocol request has none, but cancelled at cleanup,
	// which runs before the engine's Close: a write this test fails to bound
	// is released instead of holding Close.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	buf := bytes.Repeat([]byte{0xCD}, 256<<10)
	done := make(chan error, 1)
	go func() {
		for i := range 32 {
			if err := WriteToBlockStore(ctx, bs, metadata.PayloadID("full"), buf, uint64(i)*uint64(len(buf))); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writing 8 MiB under a 2 MiB cap succeeded")
		}
		if !errors.Is(err, journal.ErrLocalStoreFull) {
			t.Fatalf("the write held at capacity ended with %v, want a refusal for space (journal.ErrLocalStoreFull)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write held at capacity did not end at the request deadline: it waits out the store's own budget")
	}
}

// TestReadFromBlockStore_ColdReadEndsAtRequestDeadline reads a file whose only
// copy is on a remote that has stopped answering, with a request that carries
// no deadline and the engine's demand-fetch budget at its default. The read
// must fail at the request deadline the choke point gives it, not at the
// longer demand-fetch budget.
func TestReadFromBlockStore_ColdReadEndsAtRequestDeadline(t *testing.T) {
	rs := &stallingRemote{Store: remotememory.New()}
	bs := newDeadlineTestEngine(t, journal.Config{}, rs)
	// Cancelled at cleanup, before the engine's Close, for the same reason as
	// in the write test.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	data := bytes.Repeat([]byte("cold"), 256<<10)
	if err := WriteToBlockStore(ctx, bs, metadata.PayloadID("cold"), data, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	rs.stall.Store(true)
	// Shortened only now, so the setup above never runs into it.
	shortRequestDeadline(t, 300*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := ReadFromBlockStore(ctx, bs, metadata.PayloadID("cold"), 0, 64<<10)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cold read from a remote that does not answer succeeded: the setup no longer leaves the file remote-only")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cold read from a remote that does not answer did not end at the request deadline")
	}
}

// TestWithRequestDeadline_KeepsAnExistingDeadline pins what lets a handler that
// calls the choke points in a loop give the whole loop one budget: a context
// that already has a deadline comes back unchanged, so the choke points below
// it never start a new one.
func TestWithRequestDeadline_KeepsAnExistingDeadline(t *testing.T) {
	outer, cancel := WithRequestDeadline(context.Background())
	defer cancel()
	want, ok := outer.Deadline()
	if !ok {
		t.Fatal("a context without a deadline got none")
	}
	if left := time.Until(want); left <= 0 || left > requestDeadline {
		t.Fatalf("the default deadline is %v away, want within %v", left, requestDeadline)
	}

	time.Sleep(10 * time.Millisecond)
	inner, cancelInner := WithRequestDeadline(outer)
	defer cancelInner()
	if got, _ := inner.Deadline(); !got.Equal(want) {
		t.Fatalf("a nested call moved the deadline from %v to %v: each call in a loop would start a new budget", want, got)
	}
}
