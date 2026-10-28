// Package vfstest holds the fakes an adapter is tested against: Record, a
// Service that records each call and answers as scripted; Null, one that
// answers every call at once with zero values; and RecordingCallbacks, the
// client side of the callbacks. Each implements every method explicitly, so a
// method added to the interface fails to compile here until it is handled.
package vfstest

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// Any matches any one argument in Expectation.Args.
var Any any = anyArg{}

type anyArg struct{}

// Recorder is the Service Record returns.
type Recorder struct {
	t      testing.TB
	mu     sync.Mutex
	expect []*Expectation
	calls  []vfs.Op
}

var _ vfs.Service = (*Recorder)(nil)

// Record is a Service that records every call. A call answers the first unmet
// Expectation of its method, and fails t if its arguments differ from those
// the Expectation pinned. A call nothing expected, or expected with no
// Return, answers as Null does, and the former fails t if it may change
// state, so a handler that calls where it should not
// is caught as surely as one that answers from its own state.
// An Expectation still unmet when t ends fails t, so a test that never calls
// AssertDone does not pass for nothing.
func Record(t testing.TB) *Recorder {
	rec := &Recorder{t: t}
	t.Cleanup(func() {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.reportUnmet()
	})
	return rec
}

// Expectation is one scripted call. Its Args and Return are set before the
// call it scripts is made.
type Expectation struct {
	op   vfs.Op
	args []any // nil: any arguments
	ret  []any
	met  bool
}

// Expect scripts one call of op.
func (rec *Recorder) Expect(op vfs.Op) *Expectation {
	e := &Expectation{op: op}
	rec.mu.Lock()
	rec.expect = append(rec.expect, e)
	rec.mu.Unlock()
	return e
}

// Args pins the call's arguments, the context first, each compared deeply;
// Any matches any one.
func (e *Expectation) Args(args ...any) *Expectation {
	e.args = args
	return e
}

// Return is what the call answers, in the method's result order; a result
// left out or nil is its zero value.
func (e *Expectation) Return(ret ...any) *Expectation {
	e.ret = ret
	return e
}

// AssertDone fails t unless every Expectation was met and at most one call
// that may change state was made, then forgets both, so the recorder serves
// the next request afresh.
func (rec *Recorder) AssertDone() {
	rec.t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reportUnmet()
	var muts []vfs.Op
	for _, op := range rec.calls {
		if vfs.Mutates(op) {
			muts = append(muts, op)
		}
	}
	if len(muts) > 1 {
		rec.t.Errorf("vfstest: %d calls that may change state, at most one allowed: %v", len(muts), muts)
	}
	rec.expect, rec.calls = nil, nil
}

func (rec *Recorder) reportUnmet() {
	rec.t.Helper()
	for _, e := range rec.expect {
		if !e.met {
			rec.t.Errorf("vfstest: %v expected, never called", e.op)
		}
	}
}

func (rec *Recorder) call(op vfs.Op, args ...any) []any {
	rec.t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls = append(rec.calls, op)
	for _, e := range rec.expect {
		if e.met || e.op != op {
			continue
		}
		e.met = true
		if e.args != nil && !match(e.args, args) {
			rec.t.Errorf("vfstest: %v called with %v, want %v", op, args[1:], e.args[1:])
		}
		return e.ret
	}
	if vfs.Mutates(op) {
		rec.t.Errorf("vfstest: unexpected %v, which may change state", op)
	}
	return nil
}

func match(want, got []any) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != Any && !reflect.DeepEqual(want[i], got[i]) {
			return false
		}
	}
	return true
}

// result is the i-th scripted result as a T, its zero value when absent.
func result[T any](rec *Recorder, ret []any, i int) T {
	var v T
	if i < len(ret) && ret[i] != nil {
		var ok bool
		if v, ok = ret[i].(T); !ok {
			rec.t.Errorf("vfstest: result %d is %T, want %T", i, ret[i], v)
		}
	}
	return v
}

func (rec *Recorder) Propose(ctx context.Context, id vfs.Identity, c vfs.ClientInfo) (vfs.Proposal, error) {
	ret := rec.call(vfs.OpPropose, ctx, id, c)
	return result[vfs.Proposal](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Connect(ctx context.Context, id vfs.Identity, c vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, error) {
	ret := rec.call(vfs.OpConnect, ctx, id, c, cb)
	return result[vfs.ClientID](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) AddSession(ctx context.Context, id vfs.Identity, c vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	ret := rec.call(vfs.OpAddSession, ctx, id, c, prev, next)
	return result[vfs.ClientRecord](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Record(ctx context.Context, id vfs.Identity, c vfs.ClientID) (vfs.ClientRecord, error) {
	ret := rec.call(vfs.OpRecord, ctx, id, c)
	return result[vfs.ClientRecord](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Renew(ctx context.Context, id vfs.Identity, c vfs.ClientID) (vfs.ClientStatus, error) {
	ret := rec.call(vfs.OpRenew, ctx, id, c)
	return result[vfs.ClientStatus](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Disconnect(ctx context.Context, id vfs.Identity, c vfs.ClientID, s vfs.SessionRef) error {
	ret := rec.call(vfs.OpDisconnect, ctx, id, c, s)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) Destroy(ctx context.Context, id vfs.Identity, c vfs.ClientID) error {
	ret := rec.call(vfs.OpDestroy, ctx, id, c)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) ReclaimComplete(ctx context.Context, id vfs.Identity, c vfs.ClientID, shown []vfs.GraceRef) error {
	ret := rec.call(vfs.OpReclaimComplete, ctx, id, c, shown)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) FreeState(ctx context.Context, id vfs.Identity, c vfs.ClientID, s vfs.StateRef) error {
	ret := rec.call(vfs.OpFreeState, ctx, id, c, s)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) TestState(ctx context.Context, id vfs.Identity, c vfs.ClientID, s []vfs.StateRef) []error {
	ret := rec.call(vfs.OpTestState, ctx, id, c, s)
	if ret == nil {
		return Null{}.TestState(ctx, id, c, s)
	}
	return result[[]error](rec, ret, 0)
}

func (rec *Recorder) ReleaseLockOwner(ctx context.Context, id vfs.Identity, c vfs.ClientID, o vfs.LockOwner) error {
	ret := rec.call(vfs.OpReleaseLockOwner, ctx, id, c, o)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) HostRestarted(ctx context.Context, id vfs.Identity, host []byte, state uint32) error {
	ret := rec.call(vfs.OpHostRestarted, ctx, id, host, state)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) Lookup(ctx context.Context, id vfs.Identity, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	ret := rec.call(vfs.OpLookup, ctx, id, dir, name)
	return result[vfs.Handle](rec, ret, 0), result[vfs.File](rec, ret, 1), result[error](rec, ret, 2)
}

func (rec *Recorder) ReadDir(ctx context.Context, id vfs.Identity, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	ret := rec.call(vfs.OpReadDir, ctx, id, dir, after, max, want)
	if ret == nil {
		return Null{}.ReadDir(ctx, id, dir, after, max, want)
	}
	return result[vfs.Page](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Create(ctx context.Context, id vfs.Identity, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	ret := rec.call(vfs.OpCreate, ctx, id, dir, name, a)
	return result[vfs.CreateResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Link(ctx context.Context, id vfs.Identity, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	ret := rec.call(vfs.OpLink, ctx, id, dir, name, target)
	return result[vfs.CreateResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Unlink(ctx context.Context, id vfs.Identity, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	ret := rec.call(vfs.OpUnlink, ctx, id, dir, name)
	return result[vfs.UnlinkResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Rmdir(ctx context.Context, id vfs.Identity, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	ret := rec.call(vfs.OpRmdir, ctx, id, dir, name)
	return result[vfs.UnlinkResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Rename(ctx context.Context, id vfs.Identity, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	ret := rec.call(vfs.OpRename, ctx, id, from, fromName, to, toName, noReplace)
	return result[vfs.RenameResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) GetAttr(ctx context.Context, id vfs.Identity, h vfs.Handle) (vfs.File, error) {
	ret := rec.call(vfs.OpGetAttr, ctx, id, h)
	return result[vfs.File](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) SetAttr(ctx context.Context, id vfs.Identity, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	ret := rec.call(vfs.OpSetAttr, ctx, id, t, a)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) GetACL(ctx context.Context, id vfs.Identity, h vfs.Handle) (vfs.ACL, error) {
	ret := rec.call(vfs.OpGetACL, ctx, id, h)
	return result[vfs.ACL](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) SetACL(ctx context.Context, id vfs.Identity, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	ret := rec.call(vfs.OpSetACL, ctx, id, t, acl)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Xattrs(ctx context.Context, id vfs.Identity, h vfs.Handle) ([]vfs.Xattr, error) {
	ret := rec.call(vfs.OpXattrs, ctx, id, h)
	return result[[]vfs.Xattr](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) SetXattr(ctx context.Context, id vfs.Identity, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	ret := rec.call(vfs.OpSetXattr, ctx, id, t, x, m)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) RemoveXattr(ctx context.Context, id vfs.Identity, t vfs.Target, name []byte) (vfs.WCC, error) {
	ret := rec.call(vfs.OpRemoveXattr, ctx, id, t, name)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Streams(ctx context.Context, id vfs.Identity, h vfs.Handle) ([]vfs.File, error) {
	ret := rec.call(vfs.OpStreams, ctx, id, h)
	return result[[]vfs.File](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Access(ctx context.Context, id vfs.Identity, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	ret := rec.call(vfs.OpAccess, ctx, id, h, parent, want)
	return result[vfs.Access](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Open(ctx context.Context, id vfs.Identity, req vfs.OpenRequest) (vfs.OpenResult, error) {
	ret := rec.call(vfs.OpOpen, ctx, id, req)
	return result[vfs.OpenResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Downgrade(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	ret := rec.call(vfs.OpDowngrade, ctx, id, ref, q, access, deny)
	return result[uint32](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) ConfirmOpen(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	ret := rec.call(vfs.OpConfirmOpen, ctx, id, ref, q)
	return result[uint32](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Close(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	ret := rec.call(vfs.OpClose, ctx, id, ref, q)
	return result[*vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

// ponytail: Read answers the scripted n and eof but writes nothing into dst.
// Let Return carry bytes to copy into dst once a handler test asserts the
// bytes of a read's reply.
func (rec *Recorder) Read(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	ret := rec.call(vfs.OpRead, ctx, id, ref, off, dst)
	if ret == nil {
		return Null{}.Read(ctx, id, ref, off, dst)
	}
	return result[int](rec, ret, 0), result[bool](rec, ret, 1), result[error](rec, ret, 2)
}

func (rec *Recorder) Write(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, off int64, src []byte, s vfs.Stability) (vfs.WriteResult, error) {
	ret := rec.call(vfs.OpWrite, ctx, id, ref, off, src, s)
	if ret == nil {
		return Null{}.Write(ctx, id, ref, off, src, s)
	}
	return result[vfs.WriteResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Commit(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	ret := rec.call(vfs.OpCommit, ctx, id, ref, off, length)
	return result[vfs.Verifier](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Deallocate(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	ret := rec.call(vfs.OpDeallocate, ctx, id, ref, off, length)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Seek(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	ret := rec.call(vfs.OpSeek, ctx, id, ref, off, what)
	return result[int64](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Copy(ctx context.Context, id vfs.Identity, a vfs.CopyArgs) (vfs.CopyResult, error) {
	ret := rec.call(vfs.OpCopy, ctx, id, a)
	return result[vfs.CopyResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Clone(ctx context.Context, id vfs.Identity, a vfs.CopyArgs) (vfs.WCC, error) {
	ret := rec.call(vfs.OpClone, ctx, id, a)
	return result[vfs.WCC](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Lock(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	ret := rec.call(vfs.OpLock, ctx, id, ref, r)
	return result[vfs.StateRef](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) CancelLock(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, r vfs.ByteRange) (bool, error) {
	ret := rec.call(vfs.OpCancelLock, ctx, id, ref, r)
	return result[bool](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) TestLock(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	ret := rec.call(vfs.OpTestLock, ctx, id, ref, r, exclusive)
	return result[*vfs.Lock](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Unlock(ctx context.Context, id vfs.Identity, ref vfs.OpenRef, rs []vfs.ByteRange, q vfs.Seqs) (uint32, error) {
	ret := rec.call(vfs.OpUnlock, ctx, id, ref, rs, q)
	return result[uint32](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) AckRecall(ctx context.Context, id vfs.Identity, c vfs.ClientID, g vfs.GrantID, to vfs.GrantKind) error {
	ret := rec.call(vfs.OpAckRecall, ctx, id, c, g, to)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) Watch(ctx context.Context, id vfs.Identity, c vfs.ClientID, w vfs.WatchSpec) (vfs.WatchID, error) {
	ret := rec.call(vfs.OpWatch, ctx, id, c, w)
	return result[vfs.WatchID](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Unwatch(ctx context.Context, id vfs.Identity, c vfs.ClientID, w vfs.WatchID) error {
	ret := rec.call(vfs.OpUnwatch, ctx, id, c, w)
	return result[error](rec, ret, 0)
}

func (rec *Recorder) Root(ctx context.Context, id vfs.Identity, s vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	ret := rec.call(vfs.OpRoot, ctx, id, s)
	return result[vfs.Handle](rec, ret, 0), result[vfs.ShareInfo](rec, ret, 1), result[error](rec, ret, 2)
}

func (rec *Recorder) SecInfo(ctx context.Context, id vfs.Identity, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	ret := rec.call(vfs.OpSecInfo, ctx, id, dir, name)
	return result[[]vfs.AuthFlavor](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) ListShares(ctx context.Context, id vfs.Identity) ([]vfs.ShareInfo, error) {
	ret := rec.call(vfs.OpListShares, ctx, id)
	return result[[]vfs.ShareInfo](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) StatFS(ctx context.Context, id vfs.Identity, h vfs.Handle) (vfs.FilesystemInfo, error) {
	ret := rec.call(vfs.OpStatFS, ctx, id, h)
	return result[vfs.FilesystemInfo](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Quota(ctx context.Context, id vfs.Identity, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	ret := rec.call(vfs.OpQuota, ctx, id, h, p)
	return result[vfs.Usage](rec, ret, 0), result[vfs.Quota](rec, ret, 1), result[error](rec, ret, 2)
}

func (rec *Recorder) Snapshots(ctx context.Context, id vfs.Identity, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	ret := rec.call(vfs.OpSnapshots, ctx, id, h)
	return result[[]vfs.SnapshotInfo](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	ret := rec.call(vfs.OpSignIn, ctx, step)
	return result[vfs.SignInResult](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	ret := rec.call(vfs.OpSpell, ctx, p, as)
	return result[vfs.Spelling](rec, ret, 0), result[error](rec, ret, 1)
}

func (rec *Recorder) Find(ctx context.Context, id vfs.Identity, s vfs.Spelling) (vfs.Principal, error) {
	ret := rec.call(vfs.OpFind, ctx, id, s)
	return result[vfs.Principal](rec, ret, 0), result[error](rec, ret, 1)
}
