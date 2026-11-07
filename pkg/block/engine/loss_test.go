package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A corrupt loss of one file's write raises that file's loss sequence alone,
// and LossSeq reads it with no Commit; an offloaded loss raises nothing; a
// gap in the ring raises every file, one with no event of its own included.
func TestTheLossSequence(t *testing.T) {
	u := newUnit(t)
	f, g := entity.NewFileID(), entity.NewFileID()
	u.write(t, f, 0, []byte("held"))
	u.write(t, g, 0, []byte("held"))
	u.lose(f, 0, 4)
	if s := u.e.LossSeq(u.share.ID, f); s != 1 {
		t.Errorf("f's sequence after its loss is %d, want 1", s)
	}
	if s := u.e.LossSeq(u.share.ID, g); s != 0 {
		t.Errorf("g's sequence after f's loss is %d, want 0", s)
	}

	u.j.Report(journal.LossEvent{File: journal.NewFileID(g), Extent: journal.Extent{Len: 4}, Reason: journal.LossCorrupt, Offloaded: true})
	if s := u.e.LossSeq(u.share.ID, g); s != 0 {
		t.Errorf("g's sequence after an offloaded loss is %d, want 0", s)
	}

	h := entity.NewFileID()
	u.j.Report(journal.LossEvent{SeqNum: 10, File: journal.NewFileID(f), Reason: journal.LossStale})
	if s := u.e.LossSeq(u.share.ID, h); s == 0 {
		t.Error("after a gap in the ring, a file with no event of its own is not raised")
	}
	if s := u.e.LossSeq(u.share.ID, f); s <= 1 {
		t.Errorf("after a gap in the ring, f's sequence is %d, want above its own 1", s)
	}
}

// A loss the journal reports between a batch's capture and its sync is
// drained before the batch commits: the write commits lost, charging
// nothing, and a read of it answers ErrLost.
func TestALossBeforeTheBatchsSyncCommitsTheWriteLost(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	u.write(t, f, 0, []byte("lost"))
	var once sync.Once
	u.e.OnCapture(func() { once.Do(func() { u.lose(f, 0, 4) }) })
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 4 || got.Charged != 0 {
		t.Errorf("size %d, charged %d; want size 4 charging nothing", got.Size, got.Charged)
	}
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4)); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("a read of the lost write: %v, want ErrLost", err)
	}
}

// A journal's device lost whole, with writers running: every write accepted
// gets its Done exactly once, none is left pending, a write after the loss is
// answered unavailable with no Done, and every file's sequence is raised.
func TestADeviceLostWhole(t *testing.T) {
	u := newUnit(t)
	fs := make([]entity.FileID, 16)
	for i := range fs {
		fs[i] = entity.NewFileID()
	}
	var accepted, dones atomic.Int64
	var perWrite sync.Map // the count of each write's Done calls
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range fs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				key := [2]int{i, n}
				calls := new(atomic.Int32)
				perWrite.Store(key, calls)
				_, err := u.e.Write(t.Context(), u.share.ID, fs[i], int64(n), []byte("x"), engine.WriteOpts{Done: func() { calls.Add(1); dones.Add(1) }})
				if errors.Is(err, vfserr.ErrUnavailable) {
					return // the device is lost: refused, with no Done
				}
				if err != nil {
					t.Error(err)
					return
				}
				accepted.Add(1)
			}
		}()
	}
	eventually(t, "the writers are running", func() bool { return accepted.Load() >= 200 })
	u.e.LostWhole(u.rec)
	close(stop)
	wg.Wait()
	if a, d := accepted.Load(), dones.Load(); a != d {
		t.Errorf("%d writes accepted, %d Dones; want one each", a, d)
	}
	perWrite.Range(func(k, v any) bool {
		if n := v.(*atomic.Int32).Load(); n > 1 {
			t.Errorf("write %v's Done ran %d times", k, n)
		}
		return true
	})
	if n := u.e.FileEntries(); n != 0 {
		t.Errorf("%d files still hold pending writes", n)
	}
	for _, f := range fs {
		if s := u.e.LossSeq(u.share.ID, f); s == 0 {
			t.Errorf("file %v's sequence is not raised", f)
		}
	}
	// The lost journal is synced no more: a sync after the loss is refused.
	if err := u.e.SyncFile(t.Context(), u.share.ID, u.create(t, "after")); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("a sync after the device was lost: %v, want the journal closed", err)
	}
	// So no Commit answers a verifier past the loss.
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, u.create(t, "commit after"), 0, 0); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a Commit after the device was lost: %v, want ErrUnavailable", err)
	}
}

// A drain that finds the generation already applied by a drain still
// marking its writes waits for it: the batch's own drain cannot run ahead
// and commit a write that drain marks lost as written.
func TestAConcurrentDrainWaitsForTheMarks(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	u.write(t, f, 0, []byte("lost"))
	u.lose(f, 0, 4)
	committed := make(chan error, 1)
	var once sync.Once
	u.e.OnDrain(func() {
		once.Do(func() {
			go func() { committed <- u.e.CommitNow(context.Background(), u.share.ID, f) }()
			time.Sleep(50 * time.Millisecond) // the batch's sync and drain run meanwhile
		})
	})
	_ = u.e.LossSeq(u.share.ID, f) // the first drain, held after it applied the generation
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Charged != 0 {
		t.Errorf("charged %d: the lost write was committed as written", got.Charged)
	}
}

// A loss marks lost only the write whose record it dropped: an older write
// under the lost extent is still committed as written, and charged.
func TestALossMarksOnlyItsRecordsWrite(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	u.write(t, f, 0, make([]byte, 8))
	u.write(t, f, 0, []byte("newe"))
	u.lose(f, 0, 4)
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 8 || got.Charged != 8 {
		t.Errorf("size %d, charged %d; want the older write committed as written, size 8 charging 8", got.Size, got.Charged)
	}
}

// A corrupt loss drained between a write's WriteAt and its insert finds no
// pending write to mark, so the insert applies it: the batch commits the
// write lost, charging nothing, a read answers ErrLost, and Done runs once.
func TestALossBeforeTheWritesInsertCommitsItLost(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	var once sync.Once
	u.e.OnWritten(func() {
		once.Do(func() {
			u.lose(f, 0, 4)
			u.e.LossSeq(u.share.ID, f) // drains the loss
		})
	})
	var dones atomic.Int32
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("lost"), engine.WriteOpts{Done: func() { dones.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 4 || got.Charged != 0 {
		t.Errorf("size %d, charged %d; want size 4 charging nothing", got.Size, got.Charged)
	}
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4)); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("a read of the lost write: %v, want ErrLost", err)
	}
	if n := dones.Load(); n != 1 {
		t.Errorf("Done ran %d times, want once", n)
	}
}
