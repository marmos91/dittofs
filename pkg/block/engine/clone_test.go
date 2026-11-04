package engine_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const mib = 1 << 20

// cloneUnit is a unit with settings applied, clone_max_len 16 MiB.
func cloneUnit(t *testing.T, opts ...func(*engine.Deps)) *unit {
	t.Helper()
	return cloneUnitWith(t, unitConfig{deps: opts})
}

// cloneUnitWith is cloneUnit as c asks.
func cloneUnitWith(t *testing.T, c unitConfig) *unit {
	t.Helper()
	u := newUnitWith(t, c)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Minute, GroupCommitMaxFiles: 1, CloneMaxLen: 16 * mib}); err != nil {
		t.Fatal(err)
	}
	return u
}

// bindShare binds another share on the unit's journal, in namespace ns.
func (u *unit) bindShare(t *testing.T, name string, ns entity.NamespaceID) entity.ShareID {
	t.Helper()
	s := enginetest.AddShare(t, u.fs.Store, name, 1<<30)
	s.Namespace = ns
	if err := u.e.Bind(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// Admission refuses before registering anything, and admits up to its
// bounds: an admitted clone reaches the steps not built yet.
//
// No journal call is made and the store is never reached, admitted or not:
// every journal call is counted and every store call panics.
func TestCloneAdmission(t *testing.T) {
	ns := &noStore{}
	var calls atomic.Int64
	u := cloneUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){func(d *engine.Deps) {
			ns.Existence, ns.Content = d.Existence, d.Content
			d.Existence, d.Content = ns, ns
		}},
		unbound: true,
		wrap:    func(h engine.Handle) engine.Handle { return everyCallHandle{h, &calls} },
	})
	u.e.SetJournalsForTest(everyCallJournal{u.rec, &calls})
	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}
	f, g := entity.NewFileID(), entity.NewFileID()
	same := u.bindShare(t, "same", entity.NamespaceID{})
	other := u.bindShare(t, "other", entity.NewNamespaceID())
	ns.arm()
	calls.Store(0) // the binds' own calls
	s := u.share.ID
	for _, c := range []struct {
		name     string
		ss, ds   entity.ShareID
		src, dst entity.FileID
		x        vfserr.Extent
		check    func(error) bool
		want     string
	}{
		{"overlap in one file", s, s, f, f, vfserr.Extent{SrcOff: 0, DstOff: 1 * mib, Len: 8 * mib}, isInvalid, "ErrInvalid"},
		{"one byte over clone_max_len", s, s, f, g, vfserr.Extent{Len: 16*mib + 1}, isInvalid, "ErrInvalid"},
		{"zero length", s, s, f, g, vfserr.Extent{}, isInvalid, "ErrInvalid"},
		{"negative offset", s, s, f, g, vfserr.Extent{SrcOff: -1, Len: 1}, isInvalid, "ErrInvalid"},
		{"two namespaces", s, other, f, g, vfserr.Extent{Len: mib}, isCrossNamespace, "ErrCross(CrossNamespace)"},
		{"unbound source share", entity.NewShareID(), s, f, g, vfserr.Extent{Len: mib}, frozen, "DelayFrozen"},
		{"unbound destination share", s, entity.NewShareID(), f, g, vfserr.Extent{Len: mib}, frozen, "DelayFrozen"},
		{"disjoint extents of one file", s, s, f, f, vfserr.Extent{SrcOff: 0, DstOff: 8 * mib, Len: 4 * mib}, admitted, "admitted"},
		{"exactly clone_max_len", s, s, f, g, vfserr.Extent{Len: 16 * mib}, admitted, "admitted"},
		{"two shares of one namespace", s, same, f, g, vfserr.Extent{Len: mib}, admitted, "admitted"},
	} {
		if err := u.e.Clone(t.Context(), c.ss, c.src, c.ds, c.dst, c.x); !c.check(err) {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	if n := u.e.FileEntries(); n != 0 {
		t.Errorf("%d entries left after the clones, want none: a registration outlived its clone", n)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d journal calls during admission, want none", n)
	}
}

// everyCallJournal and everyCallHandle count every call before passing it on.
type everyCallJournal struct {
	engine.Journal
	n *atomic.Int64
}

func (j everyCallJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version, p journal.SyncPolicy) (engine.Handle, error) {
	j.n.Add(1)
	return j.Journal.Share(tag, limit, floor, p)
}
func (j everyCallJournal) Forget(ctx context.Context, tag journal.ShareTag) error {
	j.n.Add(1)
	return j.Journal.Forget(ctx, tag)
}
func (j everyCallJournal) Strand(tag journal.ShareTag) error {
	j.n.Add(1)
	return j.Journal.Strand(tag)
}
func (j everyCallJournal) Stats() journal.Stats           { j.n.Add(1); return j.Journal.Stats() }
func (j everyCallJournal) Draw() (journal.Version, error) { j.n.Add(1); return j.Journal.Draw() }
func (j everyCallJournal) SetVersionLimit(m journal.Version) error {
	j.n.Add(1)
	return j.Journal.SetVersionLimit(m)
}
func (j everyCallJournal) Repack(ctx context.Context, b int64, s journal.RepackScope) (int64, error) {
	j.n.Add(1)
	return j.Journal.Repack(ctx, b, s)
}
func (j everyCallJournal) Close() error { j.n.Add(1); return j.Journal.Close() }

type everyCallHandle struct {
	engine.Handle
	n *atomic.Int64
}

func (h everyCallHandle) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.WriteAt(ctx, id, off, p, mtime)
}
func (h everyCallHandle) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	h.n.Add(1)
	return h.Handle.ReadAt(ctx, id, off, p)
}
func (h everyCallHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	h.n.Add(1)
	return h.Handle.Sync(ctx, ids...)
}
func (h everyCallHandle) Truncate(ctx context.Context, id journal.FileID, size int64) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.Truncate(ctx, id, size)
}
func (h everyCallHandle) Deallocate(ctx context.Context, id journal.FileID, e journal.Extent) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.Deallocate(ctx, id, e)
}
func (h everyCallHandle) Delete(ctx context.Context, id journal.FileID) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.Delete(ctx, id)
}
func (h everyCallHandle) CloneTarget(ctx context.Context, id journal.FileID, spec journal.CloneSpec) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.CloneTarget(ctx, id, spec)
}
func (h everyCallHandle) Reserve(n int64, writes int) (engine.Reservation, error) {
	h.n.Add(1)
	return h.Handle.Reserve(n, writes)
}
func (h everyCallHandle) Since(ctx context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error] {
	h.n.Add(1)
	return h.Handle.Since(ctx, id, from)
}
func (h everyCallHandle) Settle(id journal.FileID, v journal.Version) {
	h.n.Add(1)
	h.Handle.Settle(id, v)
}
func (h everyCallHandle) Files() iter.Seq[journal.FileID] { h.n.Add(1); return h.Handle.Files() }
func (h everyCallHandle) Close() error                    { h.n.Add(1); return h.Handle.Close() }

func isInvalid(err error) bool { return errors.Is(err, vfserr.ErrInvalid) }
func frozen(err error) bool    { return isDelay(err, vfserr.DelayFrozen) }
func admitted(err error) bool  { return errors.Is(err, engine.ErrUnbuilt) }
func isCrossNamespace(err error) bool {
	var c *vfserr.CrossError
	return errors.As(err, &c) && c.Kind == vfserr.CrossNamespace
}

// heldClone runs a clone of src[0, 4 MiB) onto dst[8, 12 MiB) and holds it
// once registered, until the returned release is called.
func heldClone(t *testing.T, u *unit, src, dst entity.FileID) (release func()) {
	t.Helper()
	registered, hold := make(chan struct{}), make(chan struct{})
	var first atomic.Bool // only the first clone is held: a later one goes on
	u.e.SetAfterRegister(func() {
		if first.CompareAndSwap(false, true) {
			close(registered)
			<-hold
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{SrcOff: 0, DstOff: 8 * mib, Len: 4 * mib})
	}()
	select {
	case <-registered:
	case err := <-done:
		t.Fatalf("the clone answered %v before registering", err)
	}
	return func() {
		close(hold)
		if err := <-done; !admitted(err) {
			t.Errorf("the held clone: %v", err)
		}
		u.e.SetAfterRegister(nil)
	}
}

// While a clone is registered, a write into either extent, and a clone into
// the source extent, answer DelayClone at once, staging nothing and calling
// no Done; a write one byte past either extent is served. A read or an
// allocation of the destination extent answers DelayClone; a read of the
// source extent is served, and an allocation before the destination extent
// stops at it.
func TestARegistrationHoldsBothExtents(t *testing.T) {
	u := cloneUnit(t)
	src, dst := u.create(t, "src"), u.create(t, "dst")
	u.write(t, dst, 0, fill(16*mib, 'd'))
	release := heldClone(t, u, src, dst)
	defer release()
	ctx := t.Context()
	for _, w := range []struct {
		name string
		f    entity.FileID
		off  int64
		held bool
	}{
		{"into the source extent", src, 4*mib - 1, true},
		{"into the destination extent", dst, 8 * mib, true},
		{"one byte past the source extent", src, 4 * mib, false},
		{"one byte past the destination extent", dst, 12 * mib, false},
		{"one byte before the destination extent", dst, 8*mib - 1, false},
	} {
		calls := 0
		_, err := u.e.Write(ctx, u.share.ID, w.f, w.off, []byte("x"), engine.WriteOpts{Done: func() { calls++ }})
		switch {
		case w.held && (!isDelay(err, vfserr.DelayClone) || calls != 0):
			t.Errorf("a write %s: %v, %d Done calls; want DelayClone and none", w.name, err, calls)
		case !w.held && err != nil:
			t.Errorf("a write %s: %v, want served", w.name, err)
		}
	}
	other := u.create(t, "other")
	if err := u.e.Clone(ctx, u.share.ID, other, u.share.ID, src, vfserr.Extent{DstOff: 2 * mib, Len: mib}); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a clone into the source extent: %v, want DelayClone", err)
	}
	if err := u.e.Clone(ctx, u.share.ID, dst, u.share.ID, other, vfserr.Extent{SrcOff: 9 * mib, Len: mib}); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a clone from the destination extent: %v, want DelayClone", err)
	}
	if _, err := u.e.Read(ctx, u.share.ID, dst, 11*mib, make([]byte, 2*mib)); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a read into the destination extent: %v, want DelayClone", err)
	}
	if _, err := u.e.Read(ctx, u.share.ID, dst, 7*mib, make([]byte, 2*mib)); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a read starting before the destination extent and running into it: %v, want DelayClone", err)
	}
	if _, err := u.e.Read(ctx, u.share.ID, src, 0, make([]byte, 4)); isDelay(err, vfserr.DelayClone) {
		t.Errorf("a read of the source extent: %v, want served", err)
	}
	if _, err := u.e.Allocation(ctx, u.share.ID, dst, 9*mib); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("an allocation in the destination extent: %v, want DelayClone", err)
	}
	if sp, err := u.e.Allocation(ctx, u.share.ID, dst, 0); err != nil || sp.End() != 8*mib {
		t.Errorf("an allocation before the destination extent: %+v, %v; want it to stop at 8 MiB", sp, err)
	}
}

// A clone refused at its destination undoes its source's registration: a
// write into that source extent is served after.
func TestARefusedDestinationUndoesTheSource(t *testing.T) {
	u := cloneUnit(t)
	src, dst, third := u.create(t, "src"), u.create(t, "dst"), u.create(t, "third")
	release := heldClone(t, u, src, dst)
	defer release()
	if err := u.e.Clone(t.Context(), u.share.ID, third, u.share.ID, src, vfserr.Extent{Len: mib}); !isDelay(err, vfserr.DelayClone) {
		t.Fatalf("a clone into a registered source extent: %v, want DelayClone", err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, third, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the refused clone's source extent: %v, want served", err)
	}
}

// writeGate holds the first WriteAt after it is armed until open closes; later
// ones go on.
type writeGate struct {
	armed, first atomic.Bool
	entered      chan struct{}
	open         chan struct{}
}

// gatedWrite is a journal handle whose WriteAt passes its writeGate.
type gatedWrite struct {
	engine.Handle
	g *writeGate
}

func (h gatedWrite) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	if h.g.armed.Load() && h.g.first.CompareAndSwap(false, true) {
		close(h.g.entered)
		<-h.g.open
	}
	return h.Handle.WriteAt(ctx, id, off, p, mtime)
}

// gatedCloneUnit is cloneUnit over handles whose WriteAt passes the writeGate it
// returns, for gateFirstWrite.
func gatedCloneUnit(t *testing.T) (*unit, *writeGate) {
	t.Helper()
	g := &writeGate{entered: make(chan struct{}), open: make(chan struct{})}
	u := cloneUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return gatedWrite{h, g} }})
	return u, g
}

// gateFirstWrite arms g and writes f at off: it returns once that write is
// held at g, and the function that lets it go on.
func (u *unit) gateFirstWrite(t *testing.T, g *writeGate, f entity.FileID, off int64) (open func(), staged <-chan error) {
	t.Helper()
	g.armed.Store(true)
	errc := make(chan error, 1)
	go func() {
		_, err := u.e.Write(context.Background(), u.share.ID, f, off, fill(3, 'w'), engine.WriteOpts{Done: func() {}})
		errc <- err
	}()
	<-g.entered
	return func() { close(g.open) }, errc
}

// A registration waits for a write admitted into its extent before it to
// have its operation in the entry: when the registration returns, the
// source's overlay holds the write.
func TestARegistrationWaitsForAnAdmittedWrite(t *testing.T) {
	u, g := gatedCloneUnit(t)
	src, dst := u.create(t, "src"), u.create(t, "dst")
	open, staged := u.gateFirstWrite(t, g, src, 1*mib)
	sizes := make(chan int64, 1)
	u.e.SetAfterRegister(func() {
		o, _, err := u.e.Overlay(context.Background(), u.share.ID, src)
		if err != nil {
			t.Error(err)
		}
		sizes <- o.Size
	})
	done := make(chan error, 1)
	go func() {
		done <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 4 * mib})
	}()
	select {
	case s := <-sizes:
		t.Fatalf("the registration returned with the admitted write unstaged (overlay size %d)", s)
	case <-time.After(100 * time.Millisecond):
	}
	open()
	if err := <-staged; err != nil {
		t.Fatal(err)
	}
	if s := <-sizes; s != 1*mib+3 {
		t.Errorf("the source's overlay size at registration: %d, want %d", s, 1*mib+3)
	}
	if err := <-done; !admitted(err) {
		t.Errorf("Clone: %v", err)
	}
}

// A clone whose context ends while it waits for an admitted write answers
// the context's error and leaves no registration: once the write stages, a
// write into the source extent is served.
func TestARegistrationCutShortLeavesNone(t *testing.T) {
	u, g := gatedCloneUnit(t)
	src, dst := u.create(t, "src"), u.create(t, "dst")
	open, staged := u.gateFirstWrite(t, g, src, 0)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var opened sync.Once
	defer opened.Do(open)
	go func() {
		done <- u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 4 * mib})
	}()
	// Once a write into the source extent is refused, the registration is
	// installed and waiting for the held write: only the cancel wakes it.
	for {
		if _, err := u.e.Write(t.Context(), u.share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); isDelay(err, vfserr.DelayClone) {
			break
		}
		runtime.Gosched()
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Clone cut short: %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancel did not wake a registration waiting for an admitted write")
	}
	opened.Do(open)
	if err := <-staged; err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the cut-short clone's source extent: %v, want served", err)
	}
}

// Writers of the source file racing a clone's registration: each write is
// served, or answered DelayClone only when it is into the source extent.
// When a registration returns, no write into the source extent is still in
// the journal and every one the journal took is in the source's entry.
func TestRacingWritersAndARegistration(t *testing.T) {
	var tr *trackedWrites
	u := cloneUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		tr = &trackedWrites{Handle: h, end: 4 * mib}
		return tr
	}})
	src, dst := u.create(t, "src"), u.create(t, "dst")
	tr.f = journal.NewFileID(src)
	u.e.SetAfterRegister(func() {
		for range 32 {
			runtime.Gosched()
		}
		in, fin := tr.inFlight.Load(), tr.finished.Load()
		if in != 0 {
			t.Errorf("a registration returned with %d writes into its extent in the journal", in)
		}
		if p := u.e.PendingWrites(src, 0, 4*mib); int64(p) != fin {
			t.Errorf("a registration returned with %d writes into its extent in the entry, %d taken by the journal", p, fin)
		}
	})
	var wg sync.WaitGroup
	errs := make(chan error, 32*50)
	stop := make(chan struct{})
	for w := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				off := int64((w+i)%8) * mib
				_, err := u.e.Write(context.Background(), u.share.ID, src, off, []byte("x"), engine.WriteOpts{Done: func() {}})
				if err != nil && !(isDelay(err, vfserr.DelayClone) && off < 4*mib) {
					errs <- fmt.Errorf("a write at %d MiB: %w", off/mib, err)
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(stop) }()
	for clones := 0; ; clones++ {
		select {
		case <-stop:
			if clones == 0 {
				t.Error("the writers finished before any clone ran")
			}
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			return
		default:
		}
		if err := u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 4 * mib}); !admitted(err) {
			t.Errorf("Clone: %v", err)
		}
	}
}

// trackedWrites counts the WriteAt calls into [0, end) of file f: in the
// journal now, and returned from it.
type trackedWrites struct {
	engine.Handle
	f                  journal.FileID
	end                int64
	inFlight, finished atomic.Int64
}

func (h *trackedWrites) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	if id != h.f || off >= h.end {
		return h.Handle.WriteAt(ctx, id, off, p, mtime)
	}
	h.inFlight.Add(1)
	runtime.Gosched()
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	if err == nil {
		h.finished.Add(1)
	}
	h.inFlight.Add(-1)
	return v, err
}

// A same-file clone registers its source extent once and its destination
// once; an allocation of the source extent is served, and one before the
// destination extent stops at it.
func TestASameFileCloneRegistersOnce(t *testing.T) {
	u := cloneUnit(t)
	f := u.create(t, "f")
	u.write(t, f, 0, fill(16*mib, 'f'))
	release := heldClone(t, u, f, f)
	defer release()
	if s, d := u.e.Registrations(f); s != 1 || d != 1 {
		t.Errorf("registrations on the file: %d source, %d destination; want 1 and 1", s, d)
	}
	if sp, err := u.e.Allocation(t.Context(), u.share.ID, f, 0); err != nil || sp.End() != 8*mib {
		t.Errorf("an allocation of the source extent: %+v, %v; want served, stopping at 8 MiB", sp, err)
	}
}

// Two clones may share a source extent: a second clone from a registered
// source extent is admitted.
func TestTwoClonesShareASource(t *testing.T) {
	u := cloneUnit(t)
	src, dst, other := u.create(t, "src"), u.create(t, "dst"), u.create(t, "other")
	release := heldClone(t, u, src, dst)
	defer release()
	if err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, other, vfserr.Extent{SrcOff: mib, Len: mib}); !admitted(err) {
		t.Errorf("a second clone from the registered source extent: %v, want admitted", err)
	}
}
