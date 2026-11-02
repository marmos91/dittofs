package vfs

import (
	"bytes"
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/internal/vfs/pseudofs"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// The Names area: one name per call, resolved, listed, added or removed
// through the store's Namespace, each change checked against open state
// first and made under the lock order.
//
// ponytail: a File answered here is the store's record alone, not joined with
// the engine's overlay of writes not yet committed. Join it, as GetAttr will,
// once writes stage in the engine.
//
// ponytail: a change in a share's shard not yet served answers ErrDelay, as
// open state refuses every change there; the filesystem serves no shard
// itself yet. Start serves the shards it is primary of once release recovery
// is built.

// Lookup resolves one name in dir: "." is dir itself, ".." its parent, and a
// share's root its own parent. A pseudo directory's names are the
// pseudo-filesystem's. The caller needs traverse on dir.
func (fs *Filesystem) Lookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, vfs.File{}, err
	}
	defer done()
	if pseudofs.IsHandle(dir) {
		return fs.pseudoLookup(ctx, c, dir, name)
	}
	a, release, err := fs.admitHandle(ctx, c, dir)
	if err != nil {
		return nil, vfs.File{}, err
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpLookup, dir, nil, name, entity.Execute); err != nil {
		return nil, vfs.File{}, storeErr(err)
	}
	files := fs.meta.Files()
	if n := string(name); n == "." || n == ".." {
		d, err := files.Get(ctx, a.caller.Identity, dir)
		switch {
		case err != nil:
			return nil, vfs.File{}, storeErr(err)
		case !d.IsDir():
			return nil, vfs.File{}, vfs.ErrNotDir
		case n == "." || d.IsRoot():
			return dir, d, nil
		}
		h := entity.NewHandle(a.share.ID, d.Parent)
		f, err := files.Get(ctx, a.caller.Identity, h)
		return h, f, storeErr(err)
	}
	h, f, err := fs.meta.Namespace().Lookup(ctx, a.caller.Identity, dir, name)
	return h, f, storeErr(err)
}

// ReadDir lists dir after the cursor, at most max entries, every one when max
// is not positive, in the store's digest order, past max only to finish the
// collision chain the last entry is in. A cursor is the digest of the
// entry it follows, so it resumes in dir whatever it was taken from: an entry
// present throughout is listed once, before or after it. The caller needs
// list on dir; an entry's handle and File are filled only for a caller who
// may also traverse it, as Lookup would refuse them the same, and a caller
// who may not gets names alone.
func (fs *Filesystem) ReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.Page{}, err
	}
	defer done()
	if pseudofs.IsHandle(dir) {
		return fs.pseudoReadDir(ctx, c, dir, after, max, want)
	}
	a, release, err := fs.admitHandle(ctx, c, dir)
	if err != nil {
		return vfs.Page{}, err
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpReadDir, dir, nil, nil, entity.ListDirectory); err != nil {
		return vfs.Page{}, storeErr(err)
	}
	if want&(vfs.EntryHandle|vfs.EntryFile) != 0 {
		got, err := fs.authz.Rights(ctx, a.caller, dir, nil, entity.Execute)
		if err != nil || got&entity.Execute == 0 {
			want &^= vfs.EntryHandle | vfs.EntryFile
		}
	}
	ns := fs.meta.Namespace()
	list := ns.Entries
	if want&vfs.EntryFile != 0 {
		list = ns.EntriesPlus
	}
	page := vfs.Page{EOF: true}
	for l, err := range list(ctx, a.caller.Identity, dir, uint64(after)) {
		if err != nil {
			return vfs.Page{}, storeErr(err)
		}
		// A page ends only between two digests: the next call resumes after
		// the last one, so a page cut inside a collision chain would skip the
		// rest of the chain.
		if max > 0 && len(page.Entries) >= max && l.Cookie != uint64(page.Next) {
			page.EOF = false
			break
		}
		e := vfs.DirEntry{Name: l.Entry.Name, Cursor: vfs.Cursor(l.Cookie)}
		if want&vfs.EntryHandle != 0 {
			e.Handle = entity.NewHandle(a.share.ID, l.Entry.Child)
		}
		if want&vfs.EntryFile != 0 {
			f := l.File
			e.File = &f
		}
		page.Entries = append(page.Entries, e)
		page.Next = e.Cursor
	}
	return page, nil
}

// Create makes a new file named name in dir: the add right on dir decided
// first, then open state's refusal of a delete-pending dir, then the store's
// create, which validates the name, sets the owner and group, inherits the
// ACL and answers an exclusive create's retry with the file it made.
func (fs *Filesystem) Create(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, ca vfs.CreateArgs) (vfs.CreateResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.CreateResult{}, err
	}
	defer done()
	a, release, err := fs.admitChange(ctx, c, dir)
	if err != nil {
		return vfs.CreateResult{}, err
	}
	defer release()
	nf, err := newFile(ca)
	if err != nil {
		return vfs.CreateResult{}, err
	}
	d := lockorder.Key(fileOf(dir))
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{d}, Enter: []lockorder.Key{d}, Accept: []lockorder.Key{d}})
	if err != nil {
		return vfs.CreateResult{}, errContended
	}
	defer unlock()
	var r vfs.CreateResult
	if err := fs.addable(ctx, a, c, vfs.OpCreate, dir, name, addRight(nf.Type), &r.Dir); err != nil {
		return r, err
	}
	r.Handle, r.File, err = fs.meta.Namespace().Create(ctx, a.caller.Identity, dir, name, nf)
	if err != nil {
		return vfs.CreateResult{}, storeErr(err)
	}
	r.Dir.After = fs.attrs(ctx, a, dir)
	return r, nil
}

// Link adds name in dir for target, a file of the same share that is not a
// directory.
//
// decision: Link asks nothing of target beyond a handle to it: the add right
// and traverse on dir are decided, and the store refuses a directory, a
// released file, an immutable or append-only one and another project's. A
// handle is a bearer token, as an NFS handle is, so a caller holding one may
// name the file, as POSIX's link(2) asks nothing of the file either. That
// holds only while every handle the filesystem answers reaches a caller who
// may traverse to the file, as Lookup and ReadDir decide. Ask more of target,
// such as ownership or read and write as Linux's protected_hardlinks does, if
// a handle is found to reach a caller who could not traverse to its file.
func (fs *Filesystem) Link(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte, target vfs.Handle) (vfs.CreateResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.CreateResult{}, err
	}
	defer done()
	a, release, err := fs.admitChange(ctx, c, dir)
	if err != nil {
		return vfs.CreateResult{}, err
	}
	defer release()
	if err := a.same(target); err != nil {
		return vfs.CreateResult{}, err
	}
	d, f := lockorder.Key(fileOf(dir)), lockorder.Key(fileOf(target))
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{d, f}, Enter: []lockorder.Key{d}, Accept: []lockorder.Key{d, f}})
	if err != nil {
		return vfs.CreateResult{}, errContended
	}
	defer unlock()
	r := vfs.CreateResult{Handle: target}
	if err := fs.addable(ctx, a, c, vfs.OpLink, dir, name, entity.AddFile, &r.Dir); err != nil {
		return r, err
	}
	if err := fs.meta.Namespace().Link(ctx, a.caller.Identity, dir, name, target); err != nil {
		return vfs.CreateResult{}, storeErr(err)
	}
	r.File = fs.attrs(ctx, a, target)
	r.Dir.After = fs.attrs(ctx, a, dir)
	return r, nil
}

// Unlink removes the entry name in dir, a directory only when empty.
func (fs *Filesystem) Unlink(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return fs.unlinkAt(ctx, c, dir, name, vfs.OpUnlink)
}

// Rmdir removes the entry name in dir, which must name an empty directory.
func (fs *Filesystem) Rmdir(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte) (vfs.UnlinkResult, error) {
	return fs.unlinkAt(ctx, c, dir, name, vfs.OpRmdir)
}

// unlinkAt serves Unlink and Rmdir. It looks name up unlocked to learn
// which file's steps to take, takes them, and looks again: a name that moved
// in between starts it over. Under the steps it decides delete through dir,
// the sticky rule included, then asks open state, then unlinks, writing the
// file's opens durable when the unlink takes its last name, and releases a
// file left with no name and no open.
func (fs *Filesystem) unlinkAt(ctx context.Context, c vfs.Call, dir vfs.Target, name []byte, op vfs.Op) (vfs.UnlinkResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.UnlinkResult{}, err
	}
	defer done()
	a, release, err := fs.admitChange(ctx, c, dir.H)
	if err != nil {
		return vfs.UnlinkResult{}, err
	}
	defer release()
	for {
		seen, err := fs.entry(ctx, a, dir.H, name)
		if err != nil {
			return vfs.UnlinkResult{}, err
		}
		r, again, err := fs.unlinkLocked(ctx, a, c, dir, name, seen, op)
		if !again {
			return r, err
		}
	}
}

// unlinkLocked is one attempt of unlinkAt, holding the steps of dir and of
// seen, the file name named when looked up; again is set when name no
// longer names it.
func (fs *Filesystem) unlinkLocked(ctx context.Context, a *admitted, c vfs.Call, dir vfs.Target, name []byte, seen vfs.File, op vfs.Op) (_ vfs.UnlinkResult, again bool, _ error) {
	d, k := lockorder.Key(fileOf(dir.H)), lockorder.Key(seen.ID)
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{d, k}, Enter: []lockorder.Key{d}, Accept: []lockorder.Key{d, k}})
	if err != nil {
		return vfs.UnlinkResult{}, false, errContended
	}
	defer unlock()
	f, err := fs.entry(ctx, a, dir.H, name)
	if err != nil || f.ID != seen.ID {
		return vfs.UnlinkResult{}, err == nil, err
	}
	if op == vfs.OpRmdir && !f.IsDir() {
		return vfs.UnlinkResult{}, false, vfs.ErrNotDir
	}
	h := entity.NewHandle(a.share.ID, f.ID)
	// decision: a change through an open is authorised by the caller's own
	// rights, not the open's grant, and the open is passed to open state
	// alone, which checks it is the caller's open of the file. So a delete
	// through a handle opened with DELETE by a caller without delete-child
	// on the directory is refused. Decide through the open's grant once
	// opens are built and record it.
	if err := fs.authz.Decide(ctx, a.caller, op, h, dir.H, name, entity.Delete); err != nil {
		return vfs.UnlinkResult{}, false, storeErr(err)
	}
	if err := fs.checks.CheckChange(ctx, a.who(), keyOf(f), dir.Open, openstate.ChangeRemove); err != nil {
		return vfs.UnlinkResult{}, false, openErr(err)
	}
	var r vfs.UnlinkResult
	r.Dir.Before = fs.before(ctx, a, c, dir.H)
	orphan, err := fs.unlinkDurable(ctx, f, func(open []entity.OpenRecord) (kvstore.Orphaned, error) {
		return fs.meta.Namespace().Unlink(ctx, a.caller.Identity, dir.H, name, open...)
	})
	if err != nil {
		return vfs.UnlinkResult{}, false, err
	}
	r.Dir.After = fs.attrs(ctx, a, dir.H)
	return r, false, fs.release(ctx, orphan)
}

// Rename moves the entry fromName in from to toName in to, both of one
// share, replacing an entry there unless noReplace. It takes the steps of
// both directories, the moved file and any file replaced, looks both names
// up again under them, and starts over if either moved. Then it decides
// delete through from and through to on the file replaced, answers a rename
// between two names of one file as done without changing anything, decides
// the add right on to, asks open state about to, the moved file and the
// replaced one, renames, and moves the opens made through the old name to
// the new. Every decision is made before open state is asked anything, so a
// refused rename recalls nothing.
func (fs *Filesystem) Rename(ctx context.Context, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool) (vfs.RenameResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.RenameResult{}, err
	}
	defer done()
	a, release, err := fs.admitChange(ctx, c, from.H)
	if err != nil {
		return vfs.RenameResult{}, err
	}
	defer release()
	if err := a.same(to); err != nil {
		return vfs.RenameResult{}, err
	}
	for {
		src, err := fs.entry(ctx, a, from.H, fromName)
		if err != nil {
			return vfs.RenameResult{}, err
		}
		dst, err := fs.entryIf(ctx, a, to, toName)
		if err != nil {
			return vfs.RenameResult{}, err
		}
		r, again, err := fs.renameLocked(ctx, a, c, from, fromName, to, toName, noReplace, src, dst)
		if !again {
			return r, err
		}
	}
}

// renameLocked is one attempt of Rename, holding the steps of both
// directories, of src and of dst, the files the names named when looked up,
// dst nil for none; again is set when either name has moved since.
func (fs *Filesystem) renameLocked(ctx context.Context, a *admitted, c vfs.Call, from vfs.Target, fromName []byte, to vfs.Handle, toName []byte, noReplace bool, src vfs.File, dst *vfs.File) (_ vfs.RenameResult, again bool, _ error) {
	fd, td := lockorder.Key(fileOf(from.H)), lockorder.Key(fileOf(to))
	files := []lockorder.Key{fd, td, lockorder.Key(src.ID)}
	if dst != nil {
		files = append(files, lockorder.Key(dst.ID))
	}
	set := lockorder.Set{Admit: files, Enter: []lockorder.Key{fd, td}, Accept: files}
	moves := src.IsDir() && fd != td
	if moves {
		set.Drain = []lockorder.Key{lockorder.Key(src.ID)}
	}
	unlock, err := fs.locks.Take(ctx, set)
	if err != nil {
		return vfs.RenameResult{}, false, errContended
	}
	defer unlock()
	s, err := fs.entry(ctx, a, from.H, fromName)
	if err != nil || s.ID != src.ID {
		return vfs.RenameResult{}, err == nil, err
	}
	t, err := fs.entryIf(ctx, a, to, toName)
	if err != nil || (t == nil) != (dst == nil) || t != nil && t.ID != dst.ID {
		return vfs.RenameResult{}, err == nil, err
	}
	if dst != nil && noReplace {
		return vfs.RenameResult{}, false, vfs.ErrExist
	}
	srcH := entity.NewHandle(a.share.ID, src.ID)
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpRename, srcH, from.H, fromName, entity.Delete); err != nil {
		return vfs.RenameResult{}, false, storeErr(err)
	}
	replaces := dst != nil && dst.ID != src.ID
	if replaces {
		if err := fs.authz.Decide(ctx, a.caller, vfs.OpRename, entity.NewHandle(a.share.ID, dst.ID), to, toName, entity.Delete); err != nil {
			return vfs.RenameResult{}, false, storeErr(err)
		}
	}
	var r vfs.RenameResult
	if dst != nil && !replaces && (fd != td || !bytes.Equal(a.share.Fold.Fold(fromName), a.share.Fold.Fold(toName))) {
		// Two names of one file: the store changes nothing, as POSIX has it,
		// so open state is neither asked nor told. A respelling of one entry
		// goes on, as the store renames it.
		r.From.After, r.To.After = fs.attrs(ctx, a, from.H), fs.attrs(ctx, a, to)
		return r, false, nil
	}
	if err := fs.addable(ctx, a, c, vfs.OpRename, to, toName, addRight(src.Type), &r.To); err != nil {
		return vfs.RenameResult{}, false, err
	}
	if err := fs.checks.CheckChange(ctx, a.who(), keyOf(src), from.Open, openstate.ChangeRename); err != nil {
		return vfs.RenameResult{}, false, openErr(err)
	}
	if replaces {
		if err := fs.checks.CheckChange(ctx, a.who(), keyOf(*dst), nil, openstate.ChangeRemove|openstate.ChangeReplace); err != nil {
			return vfs.RenameResult{}, false, openErr(err)
		}
	}
	r.From.Before = fs.before(ctx, a, c, from.H)
	rename := func(open []entity.OpenRecord) (kvstore.Orphaned, error) {
		return fs.meta.Namespace().Rename(ctx, a.caller.Identity, from.H, fromName, to, toName, open...)
	}
	var orphan kvstore.Orphaned
	if replaces {
		orphan, err = fs.unlinkDurable(ctx, *dst, rename)
	} else {
		_, err = rename(nil)
		err = storeErr(err)
	}
	if err != nil {
		return vfs.RenameResult{}, false, err
	}
	r.From.After, r.To.After = fs.attrs(ctx, a, from.H), fs.attrs(ctx, a, to)
	if moves {
		r.Moved = &vfs.WCC{After: fs.attrs(ctx, a, srcH)}
	}
	if err := fs.opens.Renamed(ctx, keyOf(src), fileOf(from.H), fromName, fileOf(to), toName); err != nil {
		// The rename committed; an open's stale name only defers a delete
		// on close to a name that is gone, which then removes nothing.
		fs.log.Warn("vfs: open names not moved with a rename", "file", src.ID, "err", err)
	}
	return r, false, fs.release(ctx, orphan)
}

// admitChange admits a change to h, a directory or a file: ErrReadOnly for a
// pseudo directory, which no client may change.
//
// decision: a change in a pseudo directory is ErrReadOnly, not ErrAccess,
// since no caller may make it and an NFS client answers it read-only
// filesystem, as today's pseudo-filesystem does. Overturned if a client is
// found that retries a read-only refusal elsewhere.
func (fs *Filesystem) admitChange(ctx context.Context, c vfs.Call, h vfs.Handle) (*admitted, func(), error) {
	a, release, err := fs.admitHandle(ctx, c, h)
	if err == nil && a == nil {
		if _, err = fs.pseudoNode(ctx, h); err == nil {
			err = vfs.ErrReadOnly
		}
	}
	return a, release, err
}

// addable is the checks before a name is added in dir, in their order: the
// add right on dir, so a caller without it is refused that whatever else
// holds, then open state, which refuses a delete-pending dir. It reads dir
// under its accept lock, its attributes kept as w's Before.
func (fs *Filesystem) addable(ctx context.Context, a *admitted, c vfs.Call, op vfs.Op, dir vfs.Handle, name []byte, want vfs.Access, w *vfs.WCC) error {
	if err := fs.authz.Decide(ctx, a.caller, op, dir, nil, name, want|entity.Execute); err != nil {
		return storeErr(err)
	}
	d, err := fs.meta.Files().Get(ctx, a.caller.Identity, dir)
	if err != nil {
		return storeErr(err)
	}
	if err := fs.checks.CheckChange(ctx, a.who(), keyOf(d), nil, openstate.ChangeAddEntry); err != nil {
		return openErr(err)
	}
	if c.Protocol != entity.ProtocolNFSv3 {
		w.Before = &d
	}
	return nil
}

// unlinkDurable runs remove, the store's unlink or rename taking a name of
// f, with f's opens as the records it writes durable should it take f's
// last name, and tells open state whether it did.
func (fs *Filesystem) unlinkDurable(ctx context.Context, f vfs.File, remove func([]entity.OpenRecord) (kvstore.Orphaned, error)) (kvstore.Orphaned, error) {
	recs, commit, err := fs.opens.Durable(ctx, keyOf(f))
	if err != nil {
		return kvstore.Orphaned{}, openErr(err)
	}
	orphan, err := remove(recs)
	commit(err == nil && !orphan.File.IsZero())
	if err != nil {
		return kvstore.Orphaned{}, storeErr(err)
	}
	if len(recs) > 0 {
		// The last close releases it.
		orphan = kvstore.Orphaned{}
	}
	return orphan, nil
}

// release runs the release of a file a change left with no name and no
// open: the engine's delete, whose transaction re-checks the holders.
func (fs *Filesystem) release(ctx context.Context, orphan kvstore.Orphaned) error {
	if orphan.File.IsZero() {
		return nil
	}
	return storeErr(fs.engine.Delete(ctx, orphan.File))
}

// before is dir's attributes as a WCC's Before, read under its accept lock:
// nil over NFSv3, which never reports a directory's, and where the caller
// may not read them.
func (fs *Filesystem) before(ctx context.Context, a *admitted, c vfs.Call, dir vfs.Handle) *vfs.File {
	if c.Protocol == entity.ProtocolNFSv3 {
		return nil
	}
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, dir)
	if err != nil {
		return nil
	}
	return &f
}

// attrs is h's attributes once a change has committed, as a WCC's After.
//
// decision: an After the caller may not read, or the store fails to read, is
// left zero and the change answered as done, since it has committed and a
// retry of a name change does not do it again. Overturned if a reply is
// found that must refuse rather than omit its attributes.
func (fs *Filesystem) attrs(ctx context.Context, a *admitted, h vfs.Handle) vfs.File {
	f, _ := fs.meta.Files().Get(ctx, a.caller.Identity, h)
	return f
}

// entry is the file name names in dir.
func (fs *Filesystem) entry(ctx context.Context, a *admitted, dir vfs.Handle, name []byte) (vfs.File, error) {
	_, f, err := fs.meta.Namespace().Lookup(ctx, a.caller.Identity, dir, name)
	return f, storeErr(err)
}

// entryIf is entry, nil where name names nothing.
func (fs *Filesystem) entryIf(ctx context.Context, a *admitted, dir vfs.Handle, name []byte) (*vfs.File, error) {
	f, err := fs.entry(ctx, a, dir, name)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// same refuses h unless it names a file of a's share: CrossShare for another
// share's or a pseudo directory's, ErrStale for no valid handle.
func (a *admitted) same(h vfs.Handle) error {
	if pseudofs.IsHandle(h) {
		return &vfs.CrossError{Kind: vfs.CrossShare}
	}
	share, _, err := h.Parse()
	switch {
	case err != nil:
		return vfs.ErrStale
	case share != a.share.ID:
		return &vfs.CrossError{Kind: vfs.CrossShare}
	}
	return nil
}

// who is the caller as open state takes it: the call's client and protocol,
// and the principal it maps to, never squashed.
func (a *admitted) who() openstate.Caller {
	return openstate.Caller{Client: a.caller.Client, Principal: a.caller.Mapped, Protocol: a.caller.Protocol}
}

// keyOf is f as open state names it.
func keyOf(f vfs.File) openstate.FileKey {
	return openstate.FileKey{Share: f.Share, Shard: openstate.ShardID(f.Shard), ID: f.ID}
}

// fileOf is the file a handle already admitted names.
func fileOf(h vfs.Handle) entity.FileID {
	_, f, _ := h.Parse()
	return f
}

// errContended is the answer of a call whose locks were not all taken by
// its deadline: a retry, so no two calls wait on each other past it.
var errContended = &vfs.Delay{Cause: vfs.DelayConflict}

// addRight is the right on a directory that adding an entry of type t needs.
func addRight(t entity.FileType) vfs.Access {
	if t == entity.Directory {
		return entity.AddSubdirectory
	}
	return entity.AddFile
}

// createTypes is the store's file type for each kind Create makes.
var createTypes = map[vfs.CreateKind]entity.FileType{
	vfs.CreateRegular: entity.Regular, vfs.CreateDir: entity.Directory, vfs.CreateSymlink: entity.Symlink,
	vfs.CreateFifo: entity.FIFO, vfs.CreateSocket: entity.Socket, vfs.CreateChar: entity.CharDevice,
	vfs.CreateBlock: entity.BlockDevice,
}

// newFile is what the store creates for ca: its mode from ca's attributes, a
// symlink's 0777 where they name none, and a device number's major in its
// high 32 bits, as NFS's specdata pairs them.
func newFile(ca vfs.CreateArgs) (kvstore.NewFile, error) {
	t, ok := createTypes[ca.Kind]
	if !ok {
		return kvstore.NewFile{}, vfs.ErrInvalid
	}
	nf := kvstore.NewFile{
		Type: t, Mode: ca.Attrs.Mode, Attrs: ca.Attrs, Target: ca.Target,
		Device: entity.DeviceID{Major: uint32(ca.Dev >> 32), Minor: uint32(ca.Dev)},
	}
	if t == entity.Symlink && !ca.Attrs.Changes(entity.AttrMode) {
		nf.Mode = 0o777
	}
	if ca.Verifier != nil {
		nf.Verifier = ca.Verifier[:]
	}
	return nf, nil
}
