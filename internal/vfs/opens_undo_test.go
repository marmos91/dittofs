package vfs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// An owner's open of a file it already holds open, which widens that open,
// leaves it as it was when its answer fails: its deny mode still refuses
// another client, and its retried upgrade takes the next sequence.
func TestAFailedUpgradeKeepsTheOwnersOpen(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	g, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	n.user(t, "carol", "4321", n.share, entity.AccessReadWrite)
	c := n.nfs41(t, "v41", 4321, 1)
	owner := openstate.LockOwner{Client: c.Client, Name: []byte("open-owner")}
	first, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.WriteData, Owner: owner})
	n.must(t, err)
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Owner: owner}); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	if _, err := n.Open(ctx, n.smbClient(t, "pc", n.fs.Admin.Principal), vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.WriteData}); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("another client's write open after the failed upgrade: %v, want ErrShareViolation", err)
	}
	// The owner's retry is the upgrade its stateid's next sequence names,
	// as the failed one never reached it.
	up, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Owner: owner})
	n.must(t, err)
	if want := first.Ref.State.Seq + 1; up.Ref.State.Seq != want {
		t.Errorf("the retried upgrade's sequence: %d, want %d", up.Ref.State.Seq, want)
	}
	if _, err := n.Close(ctx, c, up.Ref, vfs.Seqs{}); err != nil {
		t.Errorf("the owner's close of its upgraded open: %v, want closed", err)
	}
}

// A delete-on-close open whose answer fails once open state recorded it
// leaves the file as a refused open does: not delete pending, and kept at
// the other open's close.
func TestAFailedDeleteOnCloseOpenMarksNothing(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	a := n.smbClient(t, "pc-a", n.fs.Admin.Principal)
	ra, err := n.Open(ctx, a, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	n.must(t, err)
	doc := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, n.smbClient(t, "pc-b", n.fs.Admin.Principal), doc); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	pc := n.smbClient(t, "pc-c", n.fs.Admin.Principal)
	if r, err := n.Open(ctx, pc, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); err != nil {
		t.Errorf("an open after the failed one: %v, want granted", err)
	} else if _, err := n.Close(ctx, pc, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Close(ctx, a, ra.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil {
		t.Errorf("f after the other open's last close: %v, want kept", err)
	}
}

// stalling is the engine whose overlay reads wait for their context to end,
// as a read behind a stalled journal does.
type stalling struct {
	Content
	entered chan struct{}
}

func (s stalling) Overlay(ctx context.Context, f entity.FileID) (Overlay, bool, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return Overlay{}, false, ctx.Err()
}

// A reconnect whose reads Drain's deadline ends leaves its persistent open
// attached, as Drain leaves every open to the next start.
func TestADrainedReconnectKeepsItsOpen(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	n.must(t, err)
	n.must(t, n.Disconnect(ctx, pc, pc.Client, pc.Session))
	pc.Session = vfs.SessionRef{ID: 2}
	entered := make(chan struct{}, 1)
	n.Filesystem.engine = stalling{Content: n.engine, entered: entered}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = n.Open(ctx, pc, vfs.OpenRequest{H: h, Reconnect: &r.Ref, CreateID: [16]byte{1}})
	}()
	<-entered
	drain, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_ = n.Drain(drain)
	<-done
	if _, err := n.fs.Store.Opens().Get(ctx, n.fs.Share.ID, fileOf(h), r.Ref.Open); err != nil {
		t.Errorf("the persistent open's record once Drain's deadline ended its reconnect: %v, want kept", err)
	}
}

// ctxKV fails every transaction on a done context, as a store whose
// transactions are bound to their context does.
type ctxKV struct{ kv.KV }

func (c ctxKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.KV.Update(ctx, fn)
}

func (c ctxKV) View(ctx context.Context, fn func(kv.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.KV.View(ctx, fn)
}

// closeEnding is open state's opens, ending the call's context once a close
// has committed, as a client dropping then does.
type closeEnding struct {
	openstate.Opens
	cancel context.CancelFunc
}

func (o *closeEnding) Close(ctx context.Context, who openstate.Caller, ref openstate.OpenRef, q openstate.Seqs) (openstate.CloseResult, error) {
	r, err := o.Opens.Close(ctx, who, ref, q)
	o.cancel()
	return r, err
}

// A delete on close whose client drops once its last close committed still
// removes the name, over a store bound by its context: what the close left
// runs past the call's end.
func TestADeleteOnCloseOutlivesItsCall(t *testing.T) {
	bound := func(tb testing.TB) kv.KV { return ctxKV{memKV(tb)} }
	n := namesOver(t, startShares(t, newFixtureOver(t, Deps{}, kvstoretest.NewFS(t, bound)), nil))
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	doc := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.opens = &closeEnding{Opens: n.opens, cancel: cancel}
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(call, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = n.Lookup(ctx, n.admin, n.fs.Root, []byte("f"))
	if refused := testutil.ToFloat64(n.refusals) - before; !errors.Is(err, vfs.ErrNotFound) || refused != 0 {
		t.Errorf("f once its client dropped after the close: %v, %v refusals; want gone and none", err, refused)
	}
}

// stuck is a releaser whose release waits for its context to end, as one
// over an unreachable store does, or for free; entered closes as it begins.
type stuck struct {
	releaser
	entered, free chan struct{}
	once          sync.Once
}

func (m *stuck) Releasing(ctx context.Context, f openstate.FileKey) (func(bool), bool, error) {
	m.once.Do(func() { close(m.entered) })
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-m.free:
		return m.releaser.Releasing(ctx, f)
	}
}

// RFC 17 §2.5: at Drain's deadline a last close's release is cancelled as
// its call is, so Drain answers and the release is left to the next start.
func TestDrainsDeadlineEndsALastClosesRelease(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: home}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	m := &stuck{releaser: n.marks, entered: make(chan struct{}), free: make(chan struct{})}
	n.marks = m
	closed := make(chan error, 1)
	go func() { _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); closed <- err }()
	<-m.entered
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- n.Drain(deadline) }()
	select {
	case err := <-drained:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Drain past its deadline: %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		close(m.free)
		t.Error("Drain still waited 5s past its deadline for a last close's release")
	}
	<-closed
}

// probingClose is open state's opens, trying each close's file lock as open
// state closes the open: free reports, per close, whether another call could
// take it then.
type probingClose struct {
	openstate.Opens
	locks *lockorder.Locks
	free  []bool
}

func (p *probingClose) Close(ctx context.Context, who openstate.Caller, o openstate.OpenRef, q openstate.Seqs) (openstate.CloseResult, error) {
	try, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	unlock, err := p.locks.Take(try, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(fileOf(o.Handle))}})
	cancel()
	if err == nil {
		unlock()
	}
	p.free = append(p.free, err == nil)
	return p.Opens.Close(ctx, who, o, q)
}

// A failed reconnect closes the open it attached under the file's admission
// lock, as Close does, and runs what that close left: the last open of an
// unlinked file gone, the file is released.
func TestAFailedReconnectClosesAsCloseDoes(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	n.stored(t, f.ID)
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	n.must(t, err)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	n.must(t, n.Disconnect(ctx, pc, pc.Client, pc.Session))
	pc.Session = vfs.SessionRef{ID: 2}
	p := &probingClose{Opens: n.opens, locks: &n.locks}
	n.opens = p
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, pc, vfs.OpenRequest{H: h, Reconnect: &r.Ref, CreateID: [16]byte{1}}); err == nil {
		t.Fatal("a reconnect whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	if len(p.free) != 1 || p.free[0] {
		t.Errorf("the failed reconnect's closes, lock free at each: %v; want one, with the lock held", p.free)
	}
	if n.held(f.ID) {
		t.Error("an unlinked file whose last open's reconnect failed is still held")
	}
}
