package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
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

// stubContent answers each covering lookup with cover's spans, numbering the
// calls from 1. Every other content call panics.
type stubContent struct {
	engine.Content
	cover func(call int, off, n int64) []entity.Span
	mu    sync.Mutex
	calls int
}

func (c *stubContent) Covering(_ context.Context, _ entity.ShareID, _ entity.FileID, off, n int64) iter.Seq2[entity.Span, error] {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
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
	var ops []kvstore.PendingOp
	for _, op := range u.e.Pending(f) {
		ops = append(ops, kvstore.PendingOp{Version: entity.JournalVersion(op.Version.Bytes()), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: op.Off, Length: op.Len}, Time: op.At})
	}
	failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: ops}})
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
			u := newUnit(t, func(d *engine.Deps) { d.Content = c })
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
	u = newUnit(t, func(d *engine.Deps) { d.Content = c })
	u.e.BindForTest(u.share, u.j, recordingHandle{u.h, &log})
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
// read back, never lost. With the read's deadline passed before it is asked
// again, the read is to be retried; with the caller's cancellation, it is
// answered that.
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
				switch ended {
				case context.Canceled:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("Read = %d, %v; want the caller's cancellation", n, err)
					}
					return
				case context.DeadlineExceeded:
					var d *vfserr.Delay
					if !errors.As(err, &d) || d.Cause != vfserr.DelayConflict {
						t.Errorf("Read = %d, %v; want ErrDelay(DelayConflict)", n, err)
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

// A read of a file another share's handle holds is corrupt, counted so, and
// carries no journal type.
func TestAReadOfAnotherSharesFileIsCorrupt(t *testing.T) {
	u := newUnit(t)
	other := engine.Share{ID: entity.NewShareID(), Tag: 2, Limit: 1 << 20, State: engine.Serving}
	h, err := u.j.Share(other.Tag, other.Limit, journal.Version{}, other.SyncPolicy)
	if err != nil {
		t.Fatal(err)
	}
	u.e.BindForTest(other, u.j, h)
	f := entity.NewFileID()
	u.write(t, f, 0, []byte("a"))
	_, err = u.e.Read(t.Context(), other.ID, f, 0, make([]byte, 1))
	var foreign *journal.OtherShareError
	if !errors.Is(err, vfserr.ErrCorrupt) || errors.As(err, &foreign) || u.e.Reads("corrupt") != 1 {
		t.Errorf("Read = %v, counted corrupt %d; want ErrCorrupt, counted once", err, u.e.Reads("corrupt"))
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
		defer func() { <-done }()
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
			s.Overwrite = entity.VersionOf(1 << 30)
		}
		return []entity.Span{s}
	}}
	u := newUnit(t, func(d *engine.Deps) { d.Content = c })
	f := entity.NewFileID()
	u.write(t, f, 0, fill(4*kib, 'a'))
	u.lose(f, 0, 4*kib)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 0, make([]byte, 4*kib)); !errors.Is(err, vfserr.ErrLost) || c.calls != 2 {
		t.Errorf("Read: %v after %d covering lookups; want ErrLost after 2", err, c.calls)
	}
}
