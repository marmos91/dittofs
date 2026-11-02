package vfs

import (
	"context"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// The calls below are not built yet: each is refused while the filesystem is
// not serving, as every call is, and otherwise answered errUnbuilt. Each moves
// to its own file as it is built.

func (fs *Filesystem) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (vfs.Proposal, error) {
	return vfs.Proposal{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	return vfs.ClientRecord{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientRecord, error) {
	return vfs.ClientRecord{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientStatus, error) {
	return vfs.ClientStatus{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, sess vfs.SessionRef) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, ref vfs.StateRef) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, refs []vfs.StateRef) []error {
	return fs.unbuiltEach(ctx, len(refs))
}

func (fs *Filesystem) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	return nil, vfs.File{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	return vfs.Page{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, a vfs.CreateArgs) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	return vfs.CreateResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return vfs.UnlinkResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	return vfs.RenameResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	return vfs.File{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, a vfs.AttrChange) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	return vfs.ACL{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Access(ctx context.Context, c vfs.Call, h vfs.Handle, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	return vfs.OpenResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	return 0, false, fs.unbuilt(ctx)
}

func (fs *Filesystem) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, st vfs.Stability) (vfs.WriteResult, error) {
	return vfs.WriteResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.Verifier, error) {
	return vfs.Verifier{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, length int64) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	return vfs.CopyResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	return vfs.WCC{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	return vfs.StateRef{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange) (bool, error) {
	return false, fs.unbuilt(ctx)
}

func (fs *Filesystem) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, rs []vfs.ByteRange, q vfs.Seqs) (uint32, error) {
	return 0, fs.unbuilt(ctx)
}

func (fs *Filesystem) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	return vfs.WatchID{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	return fs.unbuilt(ctx)
}

func (fs *Filesystem) Root(ctx context.Context, c vfs.Call, share vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	return nil, vfs.ShareInfo{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) ListShares(ctx context.Context, c vfs.Call) ([]vfs.ShareInfo, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.FilesystemInfo, error) {
	return vfs.FilesystemInfo{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	return vfs.Usage{}, vfs.Quota{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	return nil, fs.unbuilt(ctx)
}

func (fs *Filesystem) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	return vfs.SignInResult{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	return vfs.Spelling{}, fs.unbuilt(ctx)
}

func (fs *Filesystem) Find(ctx context.Context, c vfs.Call, sp vfs.Spelling) (vfs.Principal, error) {
	return vfs.Principal{}, fs.unbuilt(ctx)
}
