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
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// noStore is a store view any call of which panics, its views nil: a call
// that reaches the store fails the test.
type noStore struct {
	engine.Existence
	engine.Content
}

// A write waits for no store: with every existence and content call
// panicking, it still answers.
func TestAWriteMakesNoStoreCall(t *testing.T) {
	u := newUnit(t, func(d *engine.Deps) { d.Existence, d.Content = noStore{}, noStore{} })
	if _, err := u.e.Write(t.Context(), u.share.ID, entity.NewFileID(), 0, []byte("abc"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
}

// The journal records the clock's time with a write, in nanoseconds since
// the Unix epoch, and 0 when the write suspends the modification time; the
// write's operation is in the file's entry before Write answers.
func TestAWriteStagesTheClocksTime(t *testing.T) {
	u := newUnit(t)
	f := entity.NewFileID()
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.clock.Advance(time.Minute)
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 3, []byte("def"), engine.WriteOpts{Done: func() {}, Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	var mtimes []int64
	for c, err := range u.h.Since(t.Context(), journal.NewFileID(f), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}
		mtimes = append(mtimes, c.Mtime)
	}
	want := []int64{u.clock.Now().Add(-time.Minute).UnixNano(), 0}
	if len(mtimes) != 2 || mtimes[0] != want[0] || mtimes[1] != want[1] {
		t.Errorf("the journal's mtimes %v, want %v", mtimes, want)
	}
	ops := u.e.Pending(f)
	if len(ops) != 2 || ops[0].Off != 0 || ops[1].Off != 3 || !ops[0].SetsMtime || ops[1].SetsMtime || ops[0].Version.Compare(ops[1].Version) >= 0 {
		t.Errorf("pending operations %+v; want both writes in version order, the second setting no mtime", ops)
	}
}

// countingHandle counts the journal's WriteAt calls.
type countingHandle struct {
	engine.Handle
	n atomic.Int64
}

func (h *countingHandle) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.WriteAt(ctx, id, off, p, mtime)
}

// ctxHandle answers WriteAt with its context's error, as a journal does once
// the call's context ends.
type ctxHandle struct{ engine.Handle }

func (h ctxHandle) WriteAt(ctx context.Context, _ journal.FileID, _ int64, _ []byte, _ int64) (journal.Version, error) {
	return journal.Version{}, ctx.Err()
}

// A WriteAt that answers the context's error is answered with that error
// itself, not as an unavailable journal, and leaves nothing pending.
func TestAWriteCutShortAnswersTheContextsError(t *testing.T) {
	u := newUnit(t)
	u.e.BindForTest(u.share, u.j, ctxHandle{u.h})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := entity.NewFileID()
	_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("abc"), engine.WriteOpts{Done: func() {}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Write = %v, want context.Canceled and not ErrUnavailable", err)
	}
	if len(u.e.Pending(f)) != 0 {
		t.Errorf("a write cut short left %d pending", len(u.e.Pending(f)))
	}
}

// A write samples its verifier once, before it is staged, and answers it.
func TestAWriteSamplesItsVerifierFirst(t *testing.T) {
	u := newUnit(t)
	h := &countingHandle{Handle: u.h}
	u.e.BindForTest(u.share, u.j, h)
	var staged int64 = -1
	sampled := 0
	want := vfserr.Verifier{7, 1, 2}
	u.e.SetSample(func() vfserr.Verifier { staged = h.n.Load(); sampled++; return want })
	v, err := u.e.Write(t.Context(), u.share.ID, entity.NewFileID(), 0, []byte("abc"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	if sampled != 1 || staged != 0 || h.n.Load() != 1 {
		t.Errorf("the verifier was sampled %d times, the last after %d stagings, of %d; want once, before the one", sampled, staged, h.n.Load())
	}
	if v != want {
		t.Errorf("the write answered verifier %v, want the sampled %v", v, want)
	}
}

// A write the journal refuses for space is ErrDelay with DelayCapacity, no
// journal type: it calls no Done and leaves no operation. One of a file
// another share holds is ErrCorrupt.
func TestARefusedWriteLeavesNothing(t *testing.T) {
	u := newUnit(t)
	small := engine.Share{ID: entity.NewShareID(), Tag: 2, Limit: 2, State: engine.Serving}
	h, err := u.j.Share(small.Tag, small.Limit, journal.Version{}, small.SyncPolicy)
	if err != nil {
		t.Fatal(err)
	}
	u.e.BindForTest(small, u.j, h)
	f := entity.NewFileID()
	called := false
	_, err = u.e.Write(t.Context(), small.ID, f, 0, []byte("abc"), engine.WriteOpts{Done: func() { called = true }})
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
		t.Errorf("a write past the share's limit: %v, want ErrDelay(DelayCapacity)", err)
	}
	var noSpace *journal.NoSpaceError
	if errors.As(err, &noSpace) {
		t.Errorf("the refusal carries the journal's %T", noSpace)
	}
	if called || len(u.e.Pending(f)) != 0 || u.e.HasEntry(f) {
		t.Errorf("a refused write: Done called %v, operations %v", called, u.e.Pending(f))
	}

	g := entity.NewFileID()
	if _, err := u.e.Write(t.Context(), u.share.ID, g, 0, []byte("a"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), small.ID, g, 0, []byte("b"), engine.WriteOpts{Done: func() {}}); !errors.Is(err, vfserr.ErrCorrupt) {
		t.Errorf("a write of another share's file: %v, want ErrCorrupt", err)
	}
}

// A zero-length write stages nothing and calls Done once; a negative offset
// is ErrInvalid; a write of an unbound share is ErrDelay with DelayFrozen and
// calls no Done.
func TestAWriteOfNothing(t *testing.T) {
	u := newUnit(t)
	h := &countingHandle{Handle: u.h}
	u.e.BindForTest(u.share, u.j, h)
	f := entity.NewFileID()
	n := 0
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 7, nil, engine.WriteOpts{Done: func() { n++ }}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || h.n.Load() != 0 || u.e.HasEntry(f) {
		t.Errorf("a zero-length write: Done called %d times, %d stagings, entry %v; want once, none, none", n, h.n.Load(), u.e.HasEntry(f))
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, f, -1, []byte("a"), engine.WriteOpts{Done: func() { n++ }}); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("a negative offset: %v, want ErrInvalid", err)
	}
	var d *vfserr.Delay
	if _, err := u.e.Write(t.Context(), entity.NewShareID(), f, 0, []byte("a"), engine.WriteOpts{Done: func() { n++ }}); !errors.As(err, &d) || d.Cause != vfserr.DelayFrozen {
		t.Errorf("a write of an unbound share: %v, want ErrDelay(DelayFrozen)", err)
	}
	if n != 1 || h.n.Load() != 0 {
		t.Errorf("refused writes called Done %d times and staged %d", n-1, h.n.Load())
	}
}

// Writes of 32 files at once each leave their one operation.
func TestConcurrentWritesOfDistinctFiles(t *testing.T) {
	u := newUnit(t)
	fs := make([]entity.FileID, 32)
	var wg sync.WaitGroup
	for i := range fs {
		fs[i] = entity.NewFileID()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := u.e.Write(t.Context(), u.share.ID, fs[i], 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, f := range fs {
		if ops := u.e.Pending(f); len(ops) != 1 {
			t.Errorf("file %v holds %d operations, want 1", f, len(ops))
		}
	}
}

// Draw issues the journal's next version: write, draw, write strictly
// increase, and a draw creates no entry. At the version limit it is ErrDelay
// with DelayCommit, no journal type.
func TestDrawIsTheJournalsNextVersion(t *testing.T) {
	u := newUnit(t)
	f, g := entity.NewFileID(), entity.NewFileID()
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("a"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	d, err := u.e.Draw(t.Context(), u.share.ID, g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 1, []byte("b"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	ops := u.e.Pending(f)
	drawn := journal.VersionFromBytes(d)
	if len(ops) != 2 || ops[0].Version.Compare(drawn) >= 0 || drawn.Compare(ops[1].Version) >= 0 {
		t.Errorf("write %v, draw %v, write %v: want strictly increasing", ops[0].Version, drawn, ops[1].Version)
	}
	if u.e.HasEntry(g) {
		t.Error("a draw created an entry")
	}
	if err := u.j.SetVersionLimit(journal.Version{Counter: drawn.Counter + 2}); err != nil {
		t.Fatal(err)
	}
	_, err = u.e.Draw(t.Context(), u.share.ID, g)
	var delay *vfserr.Delay
	var noSpace *journal.NoSpaceError
	if !errors.As(err, &delay) || delay.Cause != vfserr.DelayCommit || errors.As(err, &noSpace) {
		t.Errorf("a draw at the version limit: %v, want ErrDelay(DelayCommit) with no journal type", err)
	}
}

// Overlay reads the file's entry alone: with every store call failing, it
// reflects an unstable write, its time and its size, and a file with nothing
// pending answers pending false and the zero overlay without allocating.
func TestOverlayReadsTheEntryAlone(t *testing.T) {
	u := newUnit(t, func(d *engine.Deps) { d.Existence, d.Content = noStore{}, noStore{} })
	f := entity.NewFileID()
	at := u.clock.Now()
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 10, []byte("abcd"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.clock.Advance(time.Second)
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}, Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	o, pending, err := u.e.Overlay(t.Context(), u.share.ID, f)
	if err != nil || !pending || o.Size != 14 || o.SizeSet || !o.Mtime.Equal(at) || !o.Ctime.Equal(at.Add(time.Second)) {
		t.Errorf("overlay %+v pending %v, %v; want size 14, mtime %v, ctime a second later", o, pending, err, at)
	}
	g := entity.NewFileID()
	if n := testing.AllocsPerRun(100, func() { _, _, _ = u.e.Overlay(t.Context(), u.share.ID, g) }); n != 0 {
		t.Errorf("an overlay of a file with nothing pending allocates %v times", n)
	}
	if o, pending, err := u.e.Overlay(t.Context(), u.share.ID, g); err != nil || pending || o != (engine.Overlay{}) || u.e.HasEntry(g) {
		t.Errorf("a file with nothing pending: %+v, %v, %v, entry %v; want the zero overlay, not pending, no entry", o, pending, err, u.e.HasEntry(g))
	}
}
