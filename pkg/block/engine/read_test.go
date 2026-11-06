package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// stubContent answers each covering lookup with cover's spans, numbering the
// calls from 1, and call fail, when set, with an error alone: failWith, or
// "store down". Every other content call panics.
type stubContent struct {
	engine.Content
	cover    func(call int, off, n int64) []entity.Span
	fail     int
	failWith error
	mu       sync.Mutex
	calls    int
}

// dep makes c the engine's content view, the real one behind it answering
// every call c does not stub.
func (c *stubContent) dep(d *engine.Deps) {
	c.Content = d.Content
	d.Content = c
}

func (c *stubContent) Covering(_ context.Context, _ entity.ShareID, _ entity.FileID, off, n int64) iter.Seq2[entity.Span, error] {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == c.fail {
		err := c.failWith
		if err == nil {
			err = errors.New("store down")
		}
		return func(yield func(entity.Span, error) bool) { yield(entity.Span{}, err) }
	}
	spans := c.cover(call, off, n)
	return func(yield func(entity.Span, error) bool) {
		for _, s := range spans {
			if !yield(s, nil) {
				return
			}
		}
	}
}

// write writes p at off of f through the engine.
func (u *unit) write(t *testing.T, f entity.FileID, off int64, p []byte) {
	t.Helper()
	if _, err := u.e.Write(t.Context(), u.share.ID, f, off, p, engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
}

// commit commits the existence of every pending write of f to the store,
// at the versions the journal gave them, as the group commit will.
func (u *unit) commit(t *testing.T, f entity.FileID) {
	t.Helper()
	u.commitOps(t, f, u.e.Pending(f))
}

// commitOps commits the existence of pending writes ps of f to the store.
func (u *unit) commitOps(t *testing.T, f entity.FileID, ps []engine.PendingView) {
	t.Helper()
	var ops []kvstore.PendingOp
	for _, op := range ps {
		ops = append(ops, kvstore.PendingOp{Version: entity.JournalVersion(op.Version.Bytes()), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: op.Off, Length: op.Len}, Mtime: op.At, Ctime: op.At})
	}
	failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: ops}}, nil)
	if err != nil || failed[f] != nil {
		t.Fatalf("commit: %v, %v", err, failed[f])
	}
}

// create creates an empty file in the unit's share.
func (u *unit) create(t *testing.T, name string) entity.FileID {
	t.Helper()
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, name, kvstore.Regular(0o644))
	return made.ID
}

func (u *unit) lose(f entity.FileID, off, n int64) {
	u.j.Lose(journal.NewFileID(f), journal.Extent{Off: off, Len: n}, journal.LossCorrupt)
}

func fill(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func zeros(p []byte) bool { return bytes.Count(p, []byte{0}) == len(p) }

// A range below zero or past the largest offset is ErrInvalid, asking
// neither the journal nor block metadata.
func TestAReadOutOfRangeIsInvalid(t *testing.T) {
	st := &stubContent{cover: func(_ int, off, n int64) []entity.Span { return nil }}
	var log []string
	u := newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){st.dep},
		wrap: func(h engine.Handle) engine.Handle { return recordingHandle{h, &log} },
	})
	for _, off := range []int64{-1, math.MaxInt64 - 100} {
		if n, err := u.e.Read(t.Context(), u.share.ID, entity.NewFileID(), off, make([]byte, 4*kib)); n != 0 || !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("Read at %d = %d, %v; want ErrInvalid", off, n, err)
		}
	}
	if len(log) != 0 || st.calls != 0 {
		t.Errorf("journal calls %v and %d lookups; want none", log, st.calls)
	}
}

// Every row of the resolution table, over a stub covering lookup of a file
// the journal holds nothing of, at asOf zero: only a hole or a zero ref reads
// as zeros, an uncarved run the journal answered for is lost, a carved one
// unavailable, past the end a short read, and an uncarved run whose applied
// or overwrite version is above asOf is asked again. Each read is counted
// once, by its outcome.
func TestReadResolutionTable(t *testing.T) {
	above := entity.VersionOf(5)
	span := func(off, n int64, c entity.Class) entity.Span {
		return entity.Span{Extent: entity.Extent{Offset: off, Length: n}, Class: c}
	}
	ref := entity.ChunkRef{Chunk: entity.ChunkHash{1}}
	for _, row := range []struct {
		name  string
		cover func(call int, off, n int64) []entity.Span
		n     int
		err   error
		calls int
		class string
	}{
		{"hole", func(_ int, off, n int64) []entity.Span { return []entity.Span{span(off, n, entity.Hole)} }, 4 * kib, nil, 1, "hole"},
		{"zero ref", func(_ int, off, n int64) []entity.Span { return []entity.Span{span(off, n, entity.Carved)} }, 4 * kib, nil, 1, "hole"},
		{"uncarved", func(_ int, off, n int64) []entity.Span { return []entity.Span{span(off, n, entity.Uncarved)} }, 0, vfserr.ErrLost, 1, "lost"},
		{"carved", func(_ int, off, n int64) []entity.Span {
			s := span(off, n, entity.Carved)
			s.Ref = ref
			return []entity.Span{s}
		}, 0, vfserr.ErrUnavailable, 1, "unavailable"},
		{"past the end", func(_ int, off, n int64) []entity.Span { return []entity.Span{span(off, 1*kib, entity.Hole)} }, 1 * kib, nil, 1, "hole"},
		{"applied above asOf, then a hole", func(call int, off, n int64) []entity.Span {
			s := span(off, n, entity.Hole)
			if call == 1 {
				s.Class, s.Applied = entity.Uncarved, above
			}
			return []entity.Span{s}
		}, 4 * kib, nil, 2, "hole"},
		{"overwrite above asOf, then a hole", func(call int, off, n int64) []entity.Span {
			s := span(off, n, entity.Hole)
			if call == 1 {
				s.Class, s.Overwrite = entity.Uncarved, above
			}
			return []entity.Span{s}
		}, 4 * kib, nil, 2, "hole"},
		{"applied above asOf, then still uncarved", func(call int, off, n int64) []entity.Span {
			s := span(off, n, entity.Uncarved)
			if call == 1 {
				s.Applied = above
			}
			return []entity.Span{s}
		}, 0, vfserr.ErrLost, 2, "lost"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := &stubContent{cover: row.cover}
			u := newUnit(t, c.dep)
			dst := fill(4*kib, 0xFF)
			n, err := u.e.Read(t.Context(), u.share.ID, entity.NewFileID(), 0, dst)
			if n != row.n || !errors.Is(err, row.err) || (row.err == nil) != (err == nil) {
				t.Errorf("Read = %d, %v; want %d, %v", n, err, row.n, row.err)
			}
			if errors.Is(err, vfserr.ErrLost) && row.err != vfserr.ErrLost {
				t.Errorf("a %s run reads as lost", row.name)
			}
			if !zeros(dst[:n]) {
				t.Errorf("the %d bytes read are not zeros", n)
			}
			if c.calls != row.calls {
				t.Errorf("%d covering lookups; want %d", c.calls, row.calls)
			}
			if got := u.e.Reads(row.class); got != 1 {
				t.Errorf("reads of class %s: %d; want 1", row.class, got)
			}
		})
	}
}

// recordingHandle logs the journal's reads into a shared call log.
type recordingHandle struct {
	engine.Handle
	log *[]string
}

func (h recordingHandle) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	*h.log = append(*h.log, fmt.Sprintf("read %d+%d", off, len(p)))
	return h.Handle.ReadAt(ctx, id, off, p)
}

// The journal is asked first, and block metadata once per extent the journal
// does not hold, with its exact bounds; a read the journal answers whole asks
// metadata nothing. The read's time is recorded by the clock.
func TestAReadAsksTheJournalFirst(t *testing.T) {
	var log []string
	var u *unit
	c := &stubContent{cover: func(_ int, off, n int64) []entity.Span {
		log = append(log, fmt.Sprintf("covering %d+%d", off, n))
		u.clock.Advance(time.Second)
		return []entity.Span{{Extent: entity.Extent{Offset: off, Length: n}, Class: entity.Hole}}
	}}
	u = newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){c.dep},
		wrap: func(h engine.Handle) engine.Handle { return recordingHandle{h, &log} },
	})
	f := entity.NewFileID()
	u.write(t, f, 4*kib, fill(4*kib, 'a'))

	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 12*kib)); n != 12*kib || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	want := fmt.Sprint([]string{"read 0+12288", "covering 0+4096", "covering 8192+4096"})
	if fmt.Sprint(log) != want {
		t.Errorf("calls %v; want %v", log, want)
	}
	log = nil
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 4*kib, make([]byte, 4*kib)); n != 4*kib || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if fmt.Sprint(log) != fmt.Sprint([]string{"read 4096+4096"}) {
		t.Errorf("a read the journal holds whole: calls %v", log)
	}
	if n, total := u.e.ReadSeconds(); n != 2 || total != 2*time.Second {
		t.Errorf("engine_read_seconds: %d reads totalling %v; want 2 totalling 2s", n, total)
	}
	if u.e.Reads("journal") != 1 || u.e.Reads("hole") != 1 {
		t.Errorf("reads: journal %d, hole %d; want 1 each", u.e.Reads("journal"), u.e.Reads("hole"))
	}
}

// hookContent runs hook once, before the first covering lookup it serves.
type hookContent struct {
	engine.Content
	once sync.Once
	hook func()
}

func (c *hookContent) Covering(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) iter.Seq2[entity.Span, error] {
	c.once.Do(c.hook)
	return c.Content.Covering(ctx, s, f, off, n)
}

// endingCtx is a context the test ends with a chosen error, a deadline's
// included, at the moment it chooses.
type endingCtx struct {
	context.Context
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func (c *endingCtx) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	close(c.done)
}

func (c *endingCtx) Done() <-chan struct{} { return c.done }

func (c *endingCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Existence staged and committed between a read's journal step and its
// metadata step sends the read back to the journal: an overwrite, a fill of
// a hole the read saw and a write growing the file past the end it saw each
// read back, never lost. A context ended before the read is asked again,
// cancelled or past its deadline, is answered its own error.
func TestExistenceAboveAsOfIsAskedAgain(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(u *unit, f entity.FileID) // before the read
		landing func(u *unit, f entity.FileID) // between its two steps
		off, n  int64
		want    func(u *unit, f entity.FileID) []byte // what the read answers
	}{
		{"overwrite",
			func(u *unit, f entity.FileID) {
				u.write(t, f, 0, fill(8*kib, 'a'))
				u.commit(t, f)
				u.lose(f, 0, 8*kib)
			},
			func(u *unit, f entity.FileID) { u.write(t, f, 0, fill(8*kib, 'b')); u.commit(t, f) },
			0, 8 * kib,
			func(*unit, entity.FileID) []byte { return fill(8*kib, 'b') }},
		{"hole fill",
			func(u *unit, f entity.FileID) { u.write(t, f, 12*kib, fill(4*kib, 'a')); u.commit(t, f) },
			func(u *unit, f entity.FileID) { u.write(t, f, 0, fill(4*kib, 'b')); u.commit(t, f) },
			0, 4 * kib,
			func(*unit, entity.FileID) []byte { return fill(4*kib, 'b') }},
		{"growth",
			func(u *unit, f entity.FileID) { u.write(t, f, 0, fill(8*kib, 'a')); u.commit(t, f) },
			func(u *unit, f entity.FileID) { u.write(t, f, 8*kib, fill(8*kib, 'b')); u.commit(t, f) },
			0, 16 * kib,
			func(*unit, entity.FileID) []byte { return append(fill(8*kib, 'a'), fill(8*kib, 'b')...) }},
	} {
		for _, ended := range []error{nil, context.Canceled, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("%s ctx ended %v", c.name, ended), func(t *testing.T) {
				ctx := &endingCtx{Context: t.Context(), done: make(chan struct{})}
				var u *unit
				var f entity.FileID
				hc := &hookContent{hook: func() {
					c.landing(u, f)
					if ended != nil {
						ctx.end(ended)
					}
				}}
				u = newUnit(t, func(d *engine.Deps) { hc.Content = d.Content; d.Content = hc })
				f = u.create(t, "f")
				c.setup(u, f)
				dst := make([]byte, c.n)
				n, err := u.e.Read(ctx, u.share.ID, f, c.off, dst)
				if ended != nil {
					if !errors.Is(err, ended) {
						t.Errorf("Read = %d, %v; want the context's %v", n, err, ended)
					}
					return
				}
				if err != nil || n != int(c.n) || !bytes.Equal(dst, c.want(u, f)) {
					t.Errorf("Read = %d, %v, bytes %q...; want the landed write's", n, err, dst[:min(n, 4)])
				}
			})
		}
	}
}

// A read keeps nothing of its resolution: a lost extent read, then made a
// hole, reads as zeros.
func TestAReadKeepsNoResolution(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")
	u.write(t, f, 0, fill(4*kib, 'a'))
	u.lose(f, 0, 4*kib)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4*kib)); !errors.Is(err, vfserr.ErrLost) {
		t.Fatalf("a lost extent: %v, want ErrLost", err)
	}
	v, err := u.h.Deallocate(t.Context(), journal.NewFileID(f), journal.Extent{Off: 0, Len: 4 * kib})
	if err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(f, v.Counter, false, 0, 4*kib)
	dst := fill(4*kib, 0xFF)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst); n != 4*kib || err != nil || !zeros(dst) {
		t.Errorf("the extent made a hole: Read = %d, %v, zeros %v; want all zeros", n, err, zeros(dst))
	}
}

// The end of file is judged with the pending operations applied: a gap below
// a pending write past the committed size reads as zeros, and a pending
// truncate below the committed size ends the read there.
func TestPendingOperationsJudgeTheEnd(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")
	u.write(t, f, 8*kib, fill(4*kib, 'a'))
	dst := fill(16*kib, 0xFF)
	n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst)
	if n != 12*kib || err != nil || !zeros(dst[:8*kib]) || !bytes.Equal(dst[8*kib:12*kib], fill(4*kib, 'a')) {
		t.Errorf("a gap below a pending write: Read = %d, %v; want 12 KiB, zeros then the write", n, err)
	}

	g := u.create(t, "g")
	u.write(t, g, 0, fill(8*kib, 'a'))
	u.commit(t, g)
	v, err := u.h.Truncate(t.Context(), journal.NewFileID(g), 4*kib)
	if err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(g, v.Counter, true, 4*kib, 0)
	if n, err := u.e.Read(t.Context(), u.share.ID, g, 0, make([]byte, 8*kib)); n != 4*kib || err != nil {
		t.Errorf("past a pending truncate: Read = %d, %v; want a short read of 4 KiB", n, err)
	}
}

// A read that fails part-way answers the bytes before the failing part: a
// hole, then a pending write the journal lost, then held bytes answer the
// hole's count and ErrLost.
func TestAPartFailedReadAnswersTheBytesBefore(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")
	u.write(t, f, 8*kib, fill(4*kib, 'a'))
	u.write(t, f, 12*kib, fill(4*kib, 'b'))
	u.lose(f, 8*kib, 4*kib)
	dst := fill(16*kib, 0xFF)
	n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst)
	if n != 8*kib || !errors.Is(err, vfserr.ErrLost) || !zeros(dst[:n]) {
		t.Errorf("Read = %d, %v; want 8 KiB of zeros and ErrLost", n, err)
	}
}

// countHandler counts the records logged at Error.
type countHandler struct {
	slog.Handler
	errors *atomic.Int64
}

func (h countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelError {
		h.errors.Add(1)
	}
	return nil
}
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// A lost extent read twice is logged at Error once.
func TestALostExtentLogsOnce(t *testing.T) {
	var errs atomic.Int64
	u := newUnit(t, func(d *engine.Deps) { d.Logger = slog.New(countHandler{errors: &errs}) })
	f := u.create(t, "f")
	u.write(t, f, 0, fill(4*kib, 'a'))
	u.lose(f, 0, 4*kib)
	for range 2 {
		if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4*kib)); !errors.Is(err, vfserr.ErrLost) {
			t.Fatalf("Read: %v, want ErrLost", err)
		}
	}
	if n := errs.Load(); n != 1 {
		t.Errorf("%d Error lines for one lost extent read twice; want 1", n)
	}
}

// A read of a file another share's handle holds is corrupt, counted so,
// logged at Error once, and carries no journal type.
func TestAReadOfAnotherSharesFileIsCorrupt(t *testing.T) {
	var errs atomic.Int64
	u := newUnit(t, func(d *engine.Deps) { d.Logger = slog.New(countHandler{errors: &errs}) })
	other := enginetest.AddShare(t, u.fs.Store, "other", 1<<20)
	if err := u.e.Bind(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	f := entity.NewFileID()
	u.write(t, f, 0, []byte("a"))
	_, err := u.e.Read(t.Context(), other.ID, f, 0, make([]byte, 1))
	var foreign *journal.OtherShareError
	if !errors.Is(err, vfserr.ErrCorrupt) || errors.As(err, &foreign) || u.e.Reads("corrupt") != 1 {
		t.Errorf("Read = %v, counted corrupt %d; want ErrCorrupt, counted once", err, u.e.Reads("corrupt"))
	}
	if n := errs.Load(); n != 1 {
		t.Errorf("%d Error lines for a corrupt read; want 1", n)
	}
}

// A read of one file completes while another file in the same shard has its
// entry held and a read of it waits.
func TestAReadWaitsForNoOtherFile(t *testing.T) {
	u := newUnit(t)
	a := u.create(t, "a")
	b := u.create(t, "b")
	for !u.e.SameShard(a, b) {
		b = u.create(t, fmt.Sprintf("b%v", b))
	}
	u.write(t, a, 4*kib, fill(kib, 'a'))
	u.write(t, b, 4*kib, fill(kib, 'b'))
	release := u.e.HoldEntry(a)
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_, _ = u.e.Read(context.Background(), u.share.ID, a, 0, make([]byte, 5*kib))
	}()
	deadline := time.Now().Add(time.Second)
	for u.e.FileStateWaiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the read of the held file never waited")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		_, err := u.e.Read(context.Background(), u.share.ID, b, 0, make([]byte, 5*kib))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("a read of a second file waited on the held one")
		defer func() {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
	}
	release()
	<-waited
}

// A pending write the journal lost, under an overwrite record above asOf
// that its file's applied has not reached, is asked again, not lost at once:
// the overwrite version the store holds over it is kept.
func TestAPendingWriteKeepsTheOverwriteOverIt(t *testing.T) {
	c := &stubContent{cover: func(call int, off, n int64) []entity.Span {
		s := entity.Span{Extent: entity.Extent{Offset: off, Length: n}, Class: entity.Uncarved}
		if call == 1 {
			s.Overwrite = entity.VersionOf(unitLimit - 1)
		}
		return []entity.Span{s}
	}}
	u := newUnit(t, c.dep)
	f := entity.NewFileID()
	u.write(t, f, 0, fill(4*kib, 'a'))
	u.lose(f, 0, 4*kib)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4*kib)); !errors.Is(err, vfserr.ErrLost) || c.calls != 2 {
		t.Errorf("Read: %v after %d covering lookups; want ErrLost after 2", err, c.calls)
	}
}

// writeAfterRead is a journal handle that runs hook once, after its first
// ReadAt answers.
type writeAfterRead struct {
	engine.Handle
	once *sync.Once
	hook func()
}

func (h writeAfterRead) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	n, m, held, v, err := h.Handle.ReadAt(ctx, id, off, p)
	h.once.Do(h.hook)
	return n, m, held, v, err
}

// afterFirstRead is the unit config whose journal handle runs *hook once,
// after its first ReadAt answers: the test sets *hook before that read.
func afterFirstRead(hook *func()) unitConfig {
	return unitConfig{wrap: func(h engine.Handle) engine.Handle {
		return writeAfterRead{h, &sync.Once{}, func() { (*hook)() }}
	}}
}

// A write staged after the journal answered a read is not applied to it: a
// hole the read saw stays zeros, not data the journal does not hold.
func TestAWriteAfterTheJournalAnsweredIsNotApplied(t *testing.T) {
	var hook func()
	u := newUnitWith(t, afterFirstRead(&hook))
	f := u.create(t, "f")
	u.write(t, f, 12*kib, fill(4*kib, 'a'))
	u.commit(t, f)
	hook = func() { u.write(t, f, 0, fill(4*kib, 'b')) }
	dst := fill(4*kib, 0xFF)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst); n != 4*kib || err != nil || !zeros(dst) {
		t.Errorf("Read = %d, %v, zeros %v; want 4 KiB of zeros", n, err, zeros(dst))
	}
}

// A window asked again is read whole, and answers the count of its last
// pass alone: a hole, then a part whose applied is above asOf, and on the
// second ask, of the whole window, 2 KiB of hole and then the end of file or
// a lost run.
func TestAWindowAskedAgainCountsItsLastPass(t *testing.T) {
	for _, c := range []struct {
		name string
		tail entity.Class
		err  error
	}{{"short", 0, nil}, {"failed", entity.Uncarved, vfserr.ErrLost}} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			st := &stubContent{cover: func(call int, off, n int64) []entity.Span {
				asked = append(asked, fmt.Sprintf("%d+%d", off, n))
				if call == 1 {
					return []entity.Span{
						{Extent: entity.Extent{Offset: 0, Length: 4 * kib}, Class: entity.Hole},
						{Extent: entity.Extent{Offset: 4 * kib, Length: 4 * kib}, Class: entity.Uncarved, Applied: entity.VersionOf(5)},
					}
				}
				spans := []entity.Span{{Extent: entity.Extent{Offset: off, Length: 2 * kib}, Class: entity.Hole}}
				if c.tail != 0 {
					spans = append(spans, entity.Span{Extent: entity.Extent{Offset: off + 2*kib, Length: n - 2*kib}, Class: c.tail})
				}
				return spans
			}}
			u := newUnit(t, st.dep)
			n, err := u.e.Read(t.Context(), u.share.ID, entity.NewFileID(), 0, make([]byte, 8*kib))
			if n != 2*kib || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
				t.Errorf("Read = %d, %v; want 2 KiB, %v", n, err, c.err)
			}
			if fmt.Sprint(asked) != "[0+8192 0+8192]" {
				t.Errorf("covering lookups %v; want the whole window twice", asked)
			}
		})
	}
}

// A window asked again is counted by its last pass alone: a first pass that
// met a hole and then a part whose applied is above asOf, and a second the
// journal answers whole, once a write landed between them, is counted
// journal, not hole.
func TestAReaskCountsTheClassOfItsLastPass(t *testing.T) {
	st := &stubContent{cover: func(call int, off, n int64) []entity.Span {
		return []entity.Span{
			{Extent: entity.Extent{Offset: 0, Length: 4 * kib}, Class: entity.Hole},
			{Extent: entity.Extent{Offset: 4 * kib, Length: 4 * kib}, Class: entity.Uncarved, Applied: entity.VersionOf(unitLimit - 1)},
		}
	}}
	var hook func()
	c := afterFirstRead(&hook)
	c.deps = []func(*engine.Deps){st.dep}
	u := newUnitWith(t, c)
	f := entity.NewFileID()
	hook = func() { u.write(t, f, 0, fill(8*kib, 'b')) }
	dst := make([]byte, 8*kib)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst); n != 8*kib || err != nil || !bytes.Equal(dst, fill(8*kib, 'b')) {
		t.Fatalf("Read = %d, %v; want the write's 8 KiB", n, err)
	}
	if st.calls != 1 || u.e.Reads("journal") != 1 || u.e.Reads("hole") != 0 {
		t.Errorf("%d covering lookups, counted journal %d, hole %d; want 1 lookup, counted journal once", st.calls, u.e.Reads("journal"), u.e.Reads("hole"))
	}
}

// A read asked again returns a state the file held, never bytes of two: the
// journal holds b at [4k, 8k) over a hole at [0, 4k), and between the read's
// journal step and its lookup d is written over b, then c over the hole, and
// both commit. The read answers 0…b or c…d, never c…b.
func TestAReadAskedAgainIsNeverTorn(t *testing.T) {
	var hook func()
	u := newUnitWith(t, afterFirstRead(&hook))
	f := u.create(t, "f")
	u.write(t, f, 4*kib, fill(4*kib, 'b'))
	u.commit(t, f)
	u.e.ClearOps(f)
	hook = func() {
		u.write(t, f, 4*kib, fill(4*kib, 'd'))
		u.commit(t, f)
		u.e.ClearOps(f)
		u.write(t, f, 0, fill(4*kib, 'c'))
		u.commit(t, f)
		u.e.ClearOps(f)
	}
	dst := make([]byte, 8*kib)
	n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst)
	if n != 8*kib || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	held := false
	for _, s := range [][]byte{
		append(fill(4*kib, 0), fill(4*kib, 'b')...),
		append(fill(4*kib, 0), fill(4*kib, 'd')...),
		append(fill(4*kib, 'c'), fill(4*kib, 'd')...),
	} {
		held = held || bytes.Equal(dst, s)
	}
	if !held {
		t.Errorf("Read answered %q…%q, a state the file never held", dst[:1], dst[4*kib:4*kib+1])
	}
}

// deadlineCtx is a context with a deadline on the test's clock, which its
// Err never reports.
type deadlineCtx struct {
	context.Context
	at time.Time
}

func (c deadlineCtx) Deadline() (time.Time, bool) { return c.at, true }

// A read whose lookups keep answering above asOf is asked again until its
// deadline by the engine's clock, then told to retry; a read its caller
// cancels is answered that. Either is counted under no class, its time
// recorded once.
func TestAReadAskedAgainEndsAtItsDeadline(t *testing.T) {
	for _, c := range []struct {
		name string
		want func(error) bool
	}{
		{"deadline", func(err error) bool {
			var d *vfserr.Delay
			return errors.As(err, &d) && d.Cause == vfserr.DelayConflict
		}},
		{"cancelled", func(err error) bool { return errors.Is(err, context.Canceled) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var u *unit
			cctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			st := &stubContent{cover: func(call int, off, n int64) []entity.Span {
				s := entity.Span{Extent: entity.Extent{Offset: off, Length: n}, Class: entity.Uncarved, Applied: entity.VersionOf(5)}
				switch {
				case call > 100:
					s = entity.Span{Extent: s.Extent, Class: entity.Hole} // ends a read nothing else ends
				case c.name == "cancelled" && call == 3:
					cancel()
				}
				u.clock.Advance(10 * time.Millisecond)
				return []entity.Span{s}
			}}
			u = newUnit(t, st.dep)
			ctx := cctx
			if c.name == "deadline" {
				ctx = deadlineCtx{cctx, u.clock.Now().Add(50 * time.Millisecond)}
			}
			n, err := u.e.Read(ctx, u.share.ID, entity.NewFileID(), 0, make([]byte, 4*kib))
			if !c.want(err) || n != 0 || st.calls > 5 {
				t.Errorf("Read = %d, %v after %d lookups", n, err, st.calls)
			}
			for _, class := range []string{"journal", "hole", "lost", "corrupt", "unavailable"} {
				if got := u.e.Reads(class); got != 0 {
					t.Errorf("counted under %s: %d", class, got)
				}
			}
			if k, _ := u.e.ReadSeconds(); k != 1 {
				t.Errorf("engine_read_seconds counted %d reads; want 1", k)
			}
		})
	}
}

// failOnSecondRead is a journal handle whose second ReadAt ends the read's
// context with its deadline and answers that.
type failOnSecondRead struct {
	engine.Handle
	ctx   *endingCtx
	calls int
}

func (h *failOnSecondRead) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	if h.calls++; h.calls == 2 {
		h.ctx.end(context.DeadlineExceeded)
		return 0, nil, nil, journal.Version{}, context.DeadlineExceeded
	}
	return h.Handle.ReadAt(ctx, id, off, p)
}

// A deadline that passes inside the journal step of a window asked again is
// answered as the context's own error, not a retry.
func TestADeadlineInsideAReaskIsTheContextsOwn(t *testing.T) {
	st := &stubContent{cover: func(_ int, off, n int64) []entity.Span {
		return []entity.Span{{Extent: entity.Extent{Offset: off, Length: n}, Class: entity.Uncarved, Applied: entity.VersionOf(5)}}
	}}
	ctx := &endingCtx{Context: t.Context(), done: make(chan struct{})}
	u := newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){st.dep},
		wrap: func(h engine.Handle) engine.Handle { return &failOnSecondRead{Handle: h, ctx: ctx} },
	})
	_, err := u.e.Read(ctx, u.share.ID, entity.NewFileID(), 0, make([]byte, 4*kib))
	var d *vfserr.Delay
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &d) {
		t.Errorf("Read: %v; want context.DeadlineExceeded itself", err)
	}
}

// A covering lookup failing part-way through a read answers the bytes before
// the extent it was asked for: a hole, then bytes the journal holds, then a
// lookup that fails.
func TestAStoreFailureAnswersTheBytesBefore(t *testing.T) {
	st := &stubContent{fail: 2, cover: func(_ int, off, n int64) []entity.Span {
		return []entity.Span{{Extent: entity.Extent{Offset: off, Length: n}, Class: entity.Hole}}
	}}
	u := newUnit(t, st.dep)
	f := entity.NewFileID()
	u.write(t, f, 4*kib, fill(4*kib, 'a'))
	dst := make([]byte, 12*kib)
	n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst)
	if n != 8*kib || !errors.Is(err, vfserr.ErrUnavailable) || !bytes.Equal(dst[4*kib:8*kib], fill(4*kib, 'a')) {
		t.Errorf("Read = %d, %v; want 8 KiB, the journal's bytes last, and ErrUnavailable", n, err)
	}
}

// Pending operations make holes inside committed data: a pending deallocate
// inside it reads as zeros, not lost, and so does the gap a pending truncate
// leaves below a pending write past it.
func TestPendingOperationsMakeHolesInCommittedData(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")
	u.write(t, f, 0, fill(12*kib, 'a'))
	u.commit(t, f)
	u.lose(f, 0, 12*kib)
	v, err := u.h.Deallocate(t.Context(), journal.NewFileID(f), journal.Extent{Off: 4 * kib, Len: 4 * kib})
	if err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(f, v.Counter, false, 4*kib, 4*kib)
	dst := fill(4*kib, 0xFF)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 4*kib, dst); n != 4*kib || err != nil || !zeros(dst) || u.e.Reads("hole") != 1 {
		t.Errorf("a pending deallocate: Read = %d, %v, zeros %v, counted hole %d; want 4 KiB of zeros, counted hole", n, err, zeros(dst), u.e.Reads("hole"))
	}

	g := u.create(t, "g")
	u.write(t, g, 0, fill(8*kib, 'a'))
	u.commit(t, g)
	u.lose(g, 0, 8*kib)
	v, err = u.h.Truncate(t.Context(), journal.NewFileID(g), 4*kib)
	if err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(g, v.Counter, true, 4*kib, 0)
	u.write(t, g, 50*kib, fill(4*kib, 'b'))
	dst = fill(46*kib, 0xFF)
	if n, err := u.e.Read(t.Context(), u.share.ID, g, 4*kib, dst); n != 46*kib || err != nil || !zeros(dst) {
		t.Errorf("below a write past a pending truncate: Read = %d, %v, zeros %v; want 46 KiB of zeros", n, err, zeros(dst))
	}
}

// A pending write the journal lost, overwritten by a write committed between
// the read's two steps, reads as the new bytes, not lost: the file's applied
// the store answers is kept on the pending write's run.
func TestALostPendingWriteOverwrittenReadsTheNewBytes(t *testing.T) {
	var hook func()
	u := newUnitWith(t, afterFirstRead(&hook))
	f := u.create(t, "f")
	u.write(t, f, 0, fill(4*kib, 'a'))
	u.lose(f, 0, 4*kib)
	hook = func() {
		u.write(t, f, 0, fill(8*kib, 'n'))
		p := u.e.Pending(f)
		u.commitOps(t, f, p[len(p)-1:])
	}
	dst := make([]byte, 8*kib)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst); n != 8*kib || err != nil || !bytes.Equal(dst, fill(8*kib, 'n')) {
		t.Errorf("Read = %d, %v, bytes %q…; want the new write's 8 KiB", n, err, dst[:min(n, 1)])
	}
}

// Runs of one lookup's answer, each read up to the first that fails or is
// missing: a hole then a carved run answers the hole's bytes and
// unavailable; two holes with a gap between answer the first hole's bytes;
// a pending write the journal lost over a hole, beside an uncarved run
// overwritten above asOf, is lost at once, as that overwrite is not over it.
func TestAReadAnswersTheRunsBeforeTheFirstItCannotRead(t *testing.T) {
	sp := func(off, n int64, c entity.Class) entity.Span {
		return entity.Span{Extent: entity.Extent{Offset: off, Length: n}, Class: c}
	}
	carved := sp(4*kib, 4*kib, entity.Carved)
	carved.Ref = entity.ChunkRef{Chunk: entity.ChunkHash{1}}
	for _, c := range []struct {
		name  string
		write bool // a pending write of [0, 4k) the journal lost
		cover func(call int) []entity.Span
		n     int
		err   error
		calls int
	}{
		{"carved after a hole", false, func(int) []entity.Span { return []entity.Span{sp(0, 4*kib, entity.Hole), carved} }, 4 * kib, vfserr.ErrUnavailable, 1},
		{"a gap between holes", false, func(int) []entity.Span { return []entity.Span{sp(0, kib, entity.Hole), sp(2*kib, 2*kib, entity.Hole)} }, kib, nil, 1},
		{"a lost write beside an overwrite", true, func(call int) []entity.Span {
			s := sp(4*kib, 4*kib, entity.Uncarved)
			if call == 1 {
				s.Overwrite = entity.VersionOf(unitLimit - 1)
			}
			return []entity.Span{sp(0, 4*kib, entity.Hole), s}
		}, 0, vfserr.ErrLost, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &stubContent{cover: func(call int, _, _ int64) []entity.Span { return c.cover(call) }}
			u := newUnit(t, st.dep)
			f := entity.NewFileID()
			size := 8 * kib
			if c.write {
				u.write(t, f, 0, fill(4*kib, 'a'))
				u.lose(f, 0, 4*kib)
				size = 4 * kib
			}
			dst := fill(size, 0xFF)
			n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst)
			if n != c.n || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) || st.calls != c.calls {
				t.Errorf("Read = %d, %v after %d lookups; want %d, %v after %d", n, err, st.calls, c.n, c.err, c.calls)
			}
			if !zeros(dst[:n]) {
				t.Errorf("the %d bytes read are not zeros", n)
			}
		})
	}
}

// A pending deallocate inside a carved zero-ref run splits it, and both
// pieces keep their class: the whole run reads as zeros.
func TestAZeroRefSplitByADeallocateReadsZeros(t *testing.T) {
	st := &stubContent{cover: func(int, int64, int64) []entity.Span {
		return []entity.Span{{Extent: entity.Extent{Offset: 0, Length: 12 * kib}, Class: entity.Carved}}
	}}
	u := newUnit(t, st.dep)
	f := entity.NewFileID()
	v, err := u.h.Deallocate(t.Context(), journal.NewFileID(f), journal.Extent{Off: 4 * kib, Len: 4 * kib})
	if err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(f, v.Counter, false, 4*kib, 4*kib)
	dst := fill(12*kib, 0xFF)
	if n, err := u.e.Read(t.Context(), u.share.ID, f, 0, dst); n != 12*kib || err != nil || !zeros(dst) {
		t.Errorf("Read = %d, %v, zeros %v; want 12 KiB of zeros", n, err, zeros(dst))
	}
}

// A store refusal crosses a read as the filesystem's: the file gone as
// ErrStale, a clone's extent as a retry with DelayClone, anything else
// unavailable.
func TestAStoreRefusalCrossesARead(t *testing.T) {
	for _, c := range []struct {
		err  error
		want func(error) bool
	}{
		{entity.ErrStale, func(err error) bool { return errors.Is(err, vfserr.ErrStale) }},
		{entity.ErrDelay, func(err error) bool {
			var d *vfserr.Delay
			return errors.As(err, &d) && d.Cause == vfserr.DelayClone
		}},
		{errors.New("store down"), func(err error) bool { return errors.Is(err, vfserr.ErrUnavailable) }},
	} {
		t.Run(c.err.Error(), func(t *testing.T) {
			st := &stubContent{fail: 1, failWith: c.err}
			u := newUnit(t, st.dep)
			if _, err := u.e.Read(t.Context(), u.share.ID, entity.NewFileID(), 0, make([]byte, kib)); !c.want(err) {
				t.Errorf("a store answering %v: Read: %v", c.err, err)
			}
		})
	}
}
