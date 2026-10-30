package vfstest

import (
	"context"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// Null is a Service that answers every call at once with zero values and no
// error, and keeps nothing: what an adapter's own cost is measured against.
// Where the zero value would break the call's contract it answers the
// nearest thing to it: TestState one nil per reference, ReadDir and Read
// the end of what they read, so a client paging through them stops, and
// Write every byte written, so a client sees no short write to retry.
type Null struct{}

var _ vfs.Service = Null{}

func (Null) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (vfs.Proposal, error) {
	return vfs.Proposal{}, nil
}

func (Null) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, error) {
	return 0, nil
}

func (Null) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	return vfs.ClientRecord{}, nil
}

func (Null) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientRecord, error) {
	return vfs.ClientRecord{}, nil
}

func (Null) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientStatus, error) {
	return vfs.ClientStatus{}, nil
}

func (Null) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.SessionRef) error {
	return nil
}

func (Null) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) error {
	return nil
}

func (Null) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) error {
	return nil
}

func (Null) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s vfs.StateRef) error {
	return nil
}

func (Null) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, s []vfs.StateRef) []error {
	return make([]error, len(s))
}

func (Null) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) error {
	return nil
}

func (Null) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	return nil
}

func (Null) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	return nil, vfs.File{}, nil
}

func (Null) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	return vfs.Page{EOF: true}, nil
}

func (Null) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, nil
}

func (Null) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, nil
}

func (Null) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, nil
}

func (Null) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, nil
}

func (Null) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	return vfs.RenameResult{}, nil
}

func (Null) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	return vfs.File{}, nil
}

func (Null) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	return vfs.ACL{}, nil
}

func (Null) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	return nil, nil
}

func (Null) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	return nil, nil
}

func (Null) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	return 0, nil
}

func (Null) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	return vfs.OpenResult{}, nil
}

func (Null) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	return 0, nil
}

func (Null) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	return 0, nil
}

func (Null) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	return nil, nil
}

func (Null) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	return 0, true, nil
}

func (Null) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, s vfs.Stability) (vfs.WriteResult, error) {
	return vfs.WriteResult{N: len(src)}, nil
}

func (Null) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	return vfs.Verifier{}, nil
}

func (Null) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	return 0, nil
}

func (Null) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	return vfs.CopyResult{}, nil
}

func (Null) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	return vfs.WCC{}, nil
}

func (Null) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	return vfs.StateRef{}, nil
}

func (Null) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange) (bool, error) {
	return false, nil
}

func (Null) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	return nil, nil
}

func (Null) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, rs []vfs.ByteRange, q vfs.Seqs) (uint32, error) {
	return 0, nil
}

func (Null) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) error {
	return nil
}

func (Null) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	return vfs.WatchID{}, nil
}

func (Null) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	return nil
}

func (Null) Root(ctx context.Context, c vfs.Call, s vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	return nil, vfs.ShareInfo{}, nil
}

func (Null) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	return nil, nil
}

func (Null) ListShares(ctx context.Context, c vfs.Call) ([]vfs.ShareInfo, error) {
	return nil, nil
}

func (Null) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.FilesystemInfo, error) {
	return vfs.FilesystemInfo{}, nil
}

func (Null) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	return vfs.Usage{}, vfs.Quota{}, nil
}

func (Null) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	return nil, nil
}

func (Null) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	return vfs.SignInResult{}, nil
}

func (Null) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	return vfs.Spelling{}, nil
}

func (Null) Find(ctx context.Context, c vfs.Call, s vfs.Spelling) (vfs.Principal, error) {
	return vfs.Principal{}, nil
}
