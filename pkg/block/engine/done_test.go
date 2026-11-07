package engine_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// A write a batch claimed whose window then fails leaves the batch: the size
// the batch commits excludes it, and its Done runs once, from the drop.
func TestAFailedWindowTakesAWriteOutOfItsBatch(t *testing.T) {
	u := newUnit(t)
	res := enginetest.NewReservations(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	for _, off := range []int64{0, 4} {
		o, answered := res.Opts()
		_, err := u.e.Write(t.Context(), u.share.ID, f, off, []byte("four"), o)
		answered(err)
	}
	var once sync.Once
	u.e.OnCapture(func() { once.Do(func() { u.failWindow(f, 4, 4) }) })
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h); err != nil || got.Size != 4 {
		t.Errorf("committed size %d, %v; want 4, the dropped write excluded", got.Size, err)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
}

// A batch whose commit fails and then succeeds calls each write's Done
// once, after the commit that succeeded.
func TestARetriedBatchCallsEachDoneOnce(t *testing.T) {
	c := &conflicting{}
	u := newUnit(t, func(d *engine.Deps) { c.Existence = d.Existence; d.Existence = c })
	res := enginetest.NewReservations(t)
	f := u.create(t, "a")
	o, answered := res.Opts()
	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"), o)
	answered(err)
	c.on.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = u.e.CommitNow(ctx, u.share.ID, f)
	if n := res.Outstanding(); n != 1 {
		t.Fatalf("after the failed commit %d writes await their Done, want 1", n)
	}
	c.on.Store(false)
	u.clock.Advance(time.Minute) // past the backoff
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("after the retry %d writes await their Done, want none", n)
	}
}

// Failed windows, corrupt losses and a device lost whole, interleaved with
// writes and batches under -race, leave no write without its Done and none
// with two.
func TestEveryAcceptedWriteGetsOneDone(t *testing.T) {
	u := newUnit(t)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Millisecond, GroupCommitMaxFiles: 4, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	res := enginetest.NewReservations(t)
	fs := make([]entity.FileID, 8)
	for i := range fs {
		fs[i] = u.create(t, string(rune('a'+i)))
	}
	var writes atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range fs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(0); ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				o, answered := res.Opts()
				_, err := u.e.Write(context.Background(), u.share.ID, f, n%16, []byte("x"), o)
				answered(err)
				writes.Add(1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			f := fs[rand.IntN(len(fs))]
			switch rand.IntN(4) {
			case 0:
				u.failWindow(f, rand.Int64N(16), 1)
			case 1:
				u.lose(f, rand.Int64N(16), 1)
			case 2:
				u.clock.Advance(time.Millisecond)
			default:
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				_ = u.e.CommitNow(ctx, u.share.ID, f)
				cancel()
			}
		}
	}()
	eventually(t, "the writers ran", func() bool { return writes.Load() > 2000 })
	u.e.LostWhole(u.rec)
	close(stop)
	wg.Wait()
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d accepted writes never got their Done", n)
	}
}

// A batch whose window drops one of its writes and whose commit then fails
// retries without it: the retry commits the size of the write left, and each
// Done runs once, the dropped write's from the drop.
func TestABatchRetriesAfterADrop(t *testing.T) {
	c := &conflicting{}
	u := newUnit(t, func(d *engine.Deps) { c.Existence = d.Existence; d.Existence = c })
	res := enginetest.NewReservations(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	for _, off := range []int64{0, 4} {
		o, answered := res.Opts()
		_, err := u.e.Write(t.Context(), u.share.ID, f, off, []byte("four"), o)
		answered(err)
	}
	var once sync.Once
	u.e.OnCapture(func() {
		once.Do(func() {
			u.failWindow(f, 4, 4)
			c.on.Store(true)
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = u.e.CommitNow(ctx, u.share.ID, f)
	if n := res.Outstanding(); n != 1 {
		t.Fatalf("after the failed commit %d writes await their Done, want the one left", n)
	}
	c.on.Store(false)
	u.clock.Advance(time.Minute) // past the backoff
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h); err != nil || got.Size != 4 {
		t.Errorf("committed size %d, %v; want 4, the dropped write left out", got.Size, err)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
}
