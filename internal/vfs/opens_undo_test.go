package vfs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
// as a read behind a stalled journal does, or for free.
type stalling struct {
	Content
	entered, free chan struct{}
}

func (s stalling) Overlay(ctx context.Context, _ entity.ShareID, _ entity.FileID) (Overlay, bool, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return Overlay{}, false, ctx.Err()
	case <-s.free:
		return Overlay{}, false, errors.New("freed")
	}
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
	entered, free := make(chan struct{}, 1), make(chan struct{})
	n.Filesystem.engine = stalling{Content: n.engine, entered: entered, free: free}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = n.Open(ctx, pc, vfs.OpenRequest{H: h, Reconnect: &r.Ref, CreateID: [16]byte{1}})
	}()
	<-entered
	drain, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	drained := make(chan struct{})
	go func() { defer close(drained); _ = n.Drain(drain) }()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		close(free)
		t.Error("Drain still waited 5s past its deadline for a reconnect's reads")
	}
	<-done
	<-drained
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

// A reclaim in a shard that lost nothing is answered with the open its owner
// holds; when that answer fails, the open stays as it was, with its locks,
// and the owner's retried reclaim and its close still find it.
func TestAFailedReclaimKeepsTheHeldOpen(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	g, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	carol := n.user(t, "carol", "4321", n.share, entity.AccessReadWrite)
	c := n.nfs41(t, "v41", 4321, 1)
	owner := openstate.LockOwner{Client: c.Client, Name: []byte("open-owner")}
	req := vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Deny: entity.WriteData, Owner: owner}
	held, err := n.Open(ctx, c, req)
	n.must(t, err)
	who := openstate.Caller{Client: c.Client, Principal: carol.Principal, Protocol: entity.ProtocolNFSv4}
	rg := openstate.ByteRange{Offset: 0, Length: 10}
	lock := openstate.LockReq{Owner: openstate.LockOwner{Client: c.Client, Name: []byte("lock-owner")}, Open: held.Ref, Ranges: []openstate.LockRange{{ByteRange: rg, Exclusive: true}}}
	_, err = n.tab.Locks().Lock(ctx, who, keyOf(f), lock)
	n.must(t, err)
	n.engine.overlayErr = errors.New("journal read failed")
	req.Reclaim = true
	if _, err := n.Open(ctx, c, req); err == nil {
		t.Fatal("a reclaim whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	if _, err := n.Open(ctx, pc, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.WriteData}); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("another client's write open after the failed reclaim: %v, want ErrShareViolation", err)
	}
	other := openstate.Caller{Client: pc.Client, Principal: n.fs.Admin.Principal, Protocol: entity.ProtocolSMB}
	if l, err := n.tab.Locks().TestLock(ctx, other, keyOf(f), openstate.LockOwner{Client: pc.Client, Name: []byte("pc")}, rg, true); err != nil || l == nil {
		t.Errorf("the owner's lock after the failed reclaim: %v, %v; want held", l, err)
	}
	// The client's retry of the reclaim.
	if _, err := n.Open(ctx, c, req); err != nil {
		t.Errorf("the retried reclaim: %v, want the held open", err)
	}
	if _, err := n.Close(ctx, c, held.Ref, vfs.Seqs{}); err != nil {
		t.Errorf("the owner's close of its open after the failed reclaim: %v, want closed", err)
	}
}

// An NFSv4.0 owner's failed upgrade puts back the modes and the reference it
// widened: another client is judged against the open as the owner held it,
// and the owner's stateid and next number still close it.
func TestAFailedUpgradeLeavesTheOwnersModesAndReference(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	g, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	n.user(t, "carol", "4321", n.share, entity.AccessReadWrite)
	c := n.nfs41(t, "v40", 4321, 0) // on no session, as NFSv4.0
	owner := openstate.LockOwner{Client: c.Client, Name: []byte("open-owner")}
	seq := func(n uint32) vfs.Seqs { return vfs.Seqs{Owner: &n} }
	r, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.WriteData, Owner: owner, Seqs: seq(1)})
	n.must(t, err)
	ref := r.Ref
	ref.State.Seq, err = n.ConfirmOpen(ctx, c, ref, seq(2))
	n.must(t, err)
	n.engine.overlayErr = errors.New("journal read failed")
	up := vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Deny: entity.ReadData | entity.WriteData, Owner: owner, Seqs: seq(3)}
	if _, err := n.Open(ctx, c, up); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	if errs := n.TestState(ctx, c, c.Client, []vfs.StateRef{ref.State}); errs[0] != nil {
		t.Errorf("the owner's stateid after the failed upgrade: %v, want valid", errs[0])
	}
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	for _, tc := range []struct {
		name         string
		access, deny entity.AccessMask
	}{{"read open", entity.ReadData, 0}, {"read open denying write", entity.ReadData, entity.WriteData}} {
		o, err := n.Open(ctx, pc, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: tc.access, Deny: tc.deny})
		if err != nil {
			t.Errorf("another client's %s after the failed upgrade: %v, want granted", tc.name, err)
			continue
		}
		_, err = n.Close(ctx, pc, o.Ref, vfs.Seqs{})
		n.must(t, err)
	}
	if _, err := n.Close(ctx, c, ref, seq(4)); err != nil {
		t.Errorf("the owner's close by its stateid, numbered 4: %v, want closed", err)
	}
}

// stallingKV stalls each transaction once armed until its context ends, as
// a store that stops answering does, or until free closes.
type stallingKV struct {
	kv.KV
	armed *atomic.Bool
	free  chan struct{}
}

func (s stallingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if s.armed.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.free:
			return errors.New("freed")
		}
	}
	return s.KV.Update(ctx, fn)
}

// closeArming is open state's opens, arming the store's stall once a close
// has committed, so only what the close leaves meets it.
type closeArming struct {
	openstate.Opens
	armed *atomic.Bool
}

func (o *closeArming) Close(ctx context.Context, who openstate.Caller, ref openstate.OpenRef, q openstate.Seqs) (openstate.CloseResult, error) {
	r, err := o.Opens.Close(ctx, who, ref, q)
	o.armed.Store(true)
	return r, err
}

// RFC 17 §5.3: a delete on close's deferred unlink runs within its call's
// deadline. A close whose deadline passes while the unlink's store write
// stalls returns by its deadline, the unlink refused and counted, and the
// directory is free for another client's create.
func TestADeleteOnCloseEndsAtItsCallsDeadline(t *testing.T) {
	armed, free := new(atomic.Bool), make(chan struct{})
	stall := func(tb testing.TB) kv.KV { return stallingKV{KV: memKV(tb), armed: armed, free: free} }
	n := namesOver(t, startShares(t, newFixtureOver(t, Deps{}, kvstoretest.NewFS(t, stall)), nil))
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	doc := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	n.opens = &closeArming{Opens: n.opens, armed: armed}
	before := testutil.ToFloat64(n.refusals)
	call, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	closed := make(chan struct{})
	go func() { defer close(closed); _, _ = n.Close(call, c, r.Ref, vfs.Seqs{}) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		armed.Store(false)
		close(free)
		<-closed
		t.Fatal("a close still ran 5s past its deadline behind a stalled deferred unlink")
	}
	armed.Store(false)
	if refused := testutil.ToFloat64(n.refusals) - before; refused != 1 {
		t.Errorf("deferred unlinks refused at the close's deadline: %v, want 1", refused)
	}
	pc := n.smbClient(t, "pc-b", n.fs.Admin.Principal)
	create := byName(n.fs.Root, "g", vfs.OpenOrCreate, entity.ReadData)
	create.Backup = true
	if _, err := n.Open(ctx, pc, create); err != nil {
		t.Errorf("another client's create in the directory after the close: %v, want granted", err)
	}
}

// A file marked delete pending while a failed open was recorded is unlinked
// once that open is undone, when it was the last open, as after its Close
// (RFC 17 §5.3).
func TestAnUndoneOpensDeferredUnlinkRuns(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	// The unlink runs as the marker, an SMB caller without backup intent,
	// so the directory lets anyone remove a name.
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	h, _ := n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o666))
	m := n.smbClient(t, "marker", n.fs.Admin.Principal)
	marker := n.open(t, m, d, h, "f", entity.ReadData|entity.Delete)
	who := openstate.Caller{Client: m.Client, Principal: n.fs.Admin.Principal, Protocol: m.Protocol}
	// Between the failed open's record and its answer, the marker sets the
	// file delete pending and closes: the failed open is the file's last.
	// Only that open's read of the file fails.
	var hookErr error
	var once sync.Once
	n.engine.overlaying = func(entity.FileID) {
		n.engine.overlayErr = nil
		once.Do(func() {
			n.engine.overlayErr = errors.New("journal read failed")
			yes := true
			if hookErr = n.tab.Opens().Update(ctx, who, marker.ID, openstate.OpenUpdate{Delete: &yes}); hookErr != nil {
				return
			}
			var cr openstate.CloseResult
			cr, hookErr = n.tab.Opens().Close(ctx, who, openstate.OpenRef{Kind: openstate.RefOpened, Open: marker.ID}, openstate.Seqs{})
			if hookErr == nil && cr.Unlink != nil {
				hookErr = errors.New("the marker's close was the file's last")
			}
		})
	}
	if _, err := n.Open(ctx, n.smbClient(t, "pc", n.fs.Admin.Principal), vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr, n.engine.overlaying = nil, nil
	n.must(t, hookErr)
	if _, _, err := n.Lookup(ctx, n.admin, d, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the name of a delete-pending file after its last open was undone: %v, want ErrNotFound", err)
	}
}
