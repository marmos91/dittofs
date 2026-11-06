package vfs

import (
	"context"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The Locking area's watches, and the changes reported to them. Open state
// holds each watch and matches each change against it; the filesystem keeps,
// beside it, who set the watch, so a change is delivered only while the
// watch is held, and only where its holder may traverse to it.

// watcher is a watch set here: its client, its holder as admitted when it
// was set, the directory it watches and the changes it takes.
type watcher struct {
	client    entity.ClientID
	caller    authz.Caller
	dir       vfs.Handle
	recursive bool
	filter    openstate.ChangeMask
}

// watchers is every watch set here and not yet ended.
//
// ponytail: every change scans the watches set here for one on its share,
// O(watches) per change while any is set. Index them by share once a node
// holding many watches shows the scan in a profile.
//
// ponytail: a watch open state drops without telling the filesystem, as a
// shard's state lost to a new instance or a client released by a path that
// raises no ClientEnded or Revoked, stays here until its client ends: its
// share's changes keep walking to the root and reaching the table, which
// drops them, O(depth) store reads per change for no listener. Give the
// Sink a dropped-watch callback if a share is seen paying that after its
// watches went.
type watchers struct {
	mu  sync.Mutex
	set map[vfs.WatchID]watcher
}

// held is watch w, if it is still set for client c.
func (ws *watchers) held(c entity.ClientID, w vfs.WatchID) (watcher, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	h, ok := ws.set[w]
	return h, ok && h.client == c
}

// on reports whether a watch taking a change of what is set on share, and
// whether a recursive one is.
func (ws *watchers) on(share entity.ShareID, what openstate.ChangeMask) (set, recursive bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, h := range ws.set {
		s, _, _ := h.dir.Parse()
		if s == share && h.filter&what != 0 {
			set, recursive = true, recursive || h.recursive
		}
	}
	return set, recursive
}

// drop ends client c's watches on share, or every one of them for the zero
// share.
func (ws *watchers) drop(c entity.ClientID, share entity.ShareID) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for id, h := range ws.set {
		s, _, _ := h.dir.Parse()
		if h.client == c && (share.IsZero() || s == share) {
			delete(ws.set, id)
		}
	}
}

// dropShard ends client c's watches held in shard s.
func (ws *watchers) dropShard(c entity.ClientID, s openstate.ShardID) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for id, h := range ws.set {
		if h.client == c && id.Shard == s {
			delete(ws.set, id)
		}
	}
}

// Watch sets a watch of w.Dir for the call's client: of its entries'
// changes that w.Filter names, and with w.Recursive of those below it too.
// The caller must be granted listing the directory.
//
// decision: a pseudo directory is refused ErrInvalid: it holds no open
// state, and changes only with the share list. Overturned by a client seen
// watching the pseudo-filesystem for new shares.
func (fs *Filesystem) Watch(ctx context.Context, c vfs.Call, w vfs.WatchSpec) (vfs.WatchID, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.WatchID{}, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, w.Dir)
	if err == nil && a == nil {
		err = vfs.ErrInvalid
	}
	defer release()
	if err != nil {
		return vfs.WatchID{}, err
	}
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(fileOf(w.Dir))}})
	if err != nil {
		return vfs.WatchID{}, errContended
	}
	defer unlock()
	d, err := fs.meta.Files().Get(ctx, a.caller.Identity, w.Dir)
	switch {
	case err != nil:
		return vfs.WatchID{}, storeErr(err)
	case !d.IsDir():
		return vfs.WatchID{}, vfs.ErrNotDir
	}
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpWatch, w.Dir, nil, nil, entity.ListDirectory); err != nil {
		return vfs.WatchID{}, storeErr(err)
	}
	// The watch is recorded here before a change it matches can be
	// delivered: a delivery waits on this lock.
	fs.watchers.mu.Lock()
	defer fs.watchers.mu.Unlock()
	id, err := fs.watches.Watch(ctx, a.who(), keyOf(d), w.Recursive, w.Filter)
	if err != nil {
		return vfs.WatchID{}, openErr(err)
	}
	fs.watchers.set[id] = watcher{client: c.Client, caller: a.caller, dir: w.Dir, recursive: w.Recursive, filter: w.Filter}
	return id, nil
}

// Unwatch ends the call's client's watch w, with the changes it has not
// delivered. Another client's watch is StateError NotYours.
func (fs *Filesystem) Unwatch(ctx context.Context, c vfs.Call, w vfs.WatchID) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, c.Client)
	if err != nil {
		return err
	}
	if err := fs.watches.Unwatch(ctx, who, w); err != nil {
		return openErr(err)
	}
	fs.watchers.mu.Lock()
	delete(fs.watchers.set, w)
	fs.watchers.mu.Unlock()
	return nil
}

// Notify delivers the changes client c's watch w matched, those its holder
// may traverse to, decided at delivery. A watch no longer held here
// delivers nothing: one raised beside its end arrives after it.
//
// decision: the holder is decided as admitted when it set the watch, not
// admitted afresh, as a delivery comes on no call: a share grant withdrawn
// since stops nothing until the watch ends. Admit the holder again at each
// delivery if a revoked user is seen still hearing of a share's changes.
//
// decision: a Notify goes by the client's latest Callbacks, as every
// callback but a break does, naming no session. So a stranger whose Connect
// under a machine's ClientGuid registered them may be handed the names a
// machine's watch reports, though only those its holder may traverse to.
// Overturned if an adapter is seen delivering them to any request but the
// one that set the watch; then a Notify names the session that set it, as a
// break names one.
func (fs *Filesystem) Notify(c entity.ClientID, w openstate.WatchID, ch []openstate.Change) {
	fs.deliver(fs.latest("Notify", c), func(ctx context.Context, cb vfs.Callbacks) {
		h, ok := fs.watchers.held(c, w)
		if !ok {
			return
		}
		ch = slices.DeleteFunc(slices.Clone(ch), func(x openstate.Change) bool { return !fs.reaches(ctx, h, x) })
		if len(ch) > 0 && cb.Notify(ctx, w, ch) != nil {
			fs.callbackErrors.WithLabelValues("Notify").Inc()
		}
	})
}

// reaches reports whether h's holder may traverse the watched directory,
// and every directory below it down to the one ch was made in, as the
// chokepoint decides it now. The directories are those ch's path named when
// it committed, by identity, so a name renamed and made again since leads
// nowhere new; a directory gone since is not reached.
//
// decision: each directory is decided for traverse alone, as RFC 17 §3.2
// asks, not for listing, so a recursive watch hears a name made in a
// directory below it its holder may enter but not list, which ReadDir and
// Watch would refuse it. Decide listing on the change's own directory if
// the RFC settles that a recursive watch hears no more than a listing shows.
func (fs *Filesystem) reaches(ctx context.Context, h watcher, ch openstate.Change) bool {
	share, _, _ := h.dir.Parse()
	dir := h.dir
	for i := 0; ; i++ {
		if fs.authz.Decide(ctx, h.caller, vfs.OpWatch, dir, nil, nil, entity.Execute) != nil {
			return false
		}
		if i == len(ch.Dirs) {
			return true
		}
		dir = entity.NewHandle(share, ch.Dirs[i])
	}
}

// changed reports ch, a change committed in directory d, to d's watches,
// and to those of each directory above d up to its share's root, as a
// change below it named by its path from there, while a recursive watch is
// set on the share. With no watch on the share taking ch it reports
// nothing.
//
// ponytail: a directory's name in its parent is found by listing the
// parent, O(entries) a level, for each change below a recursively watched
// share. Keep each directory's name beside its parent in the store if a
// large directory under a recursive watch shows the listing in a profile.
func (fs *Filesystem) changed(ctx context.Context, d vfs.File, ch openstate.Change) {
	set, recursive := fs.watchers.on(d.Share, ch.What)
	if !set || d.ID.IsZero() {
		return
	}
	fs.watches.Changed(ctx, keyOf(d), ch)
	for recursive && !d.IsRoot() {
		up, err := fs.meta.Files().Get(ctx, system, entity.NewHandle(d.Share, d.Parent))
		if err != nil {
			return
		}
		name, err := fs.nameIn(ctx, up, d.ID)
		if err != nil {
			return
		}
		ch.Name = slices.Concat(name, []byte{'/'}, ch.Name)
		if ch.To != nil {
			ch.To = slices.Concat(name, []byte{'/'}, ch.To)
		}
		ch.Nested = true
		ch.Dirs = slices.Concat([]entity.FileID{d.ID}, ch.Dirs)
		fs.watches.Changed(ctx, keyOf(up), ch)
		d = up
	}
}

// nameIn is the name of child in directory d.
func (fs *Filesystem) nameIn(ctx context.Context, d vfs.File, child entity.FileID) ([]byte, error) {
	for l, err := range fs.meta.Namespace().Entries(ctx, system, entity.NewHandle(d.Share, d.ID), 0) {
		if err != nil {
			return nil, err
		}
		if l.Entry.Child == child {
			return l.Entry.Name, nil
		}
	}
	return nil, vfs.ErrNotFound
}

// entryChanged reports what, a change to file f through ref, in the
// directory the open was made through, named by its entry there as renames
// moved it. A directory changed through no such entry is reported under its
// one name, in the parent its record names. It runs on a context the
// client dropping does not end, as changedIn does.
//
// decision: a regular file changed through no open, an open made by handle,
// a delegation, or an open whose entry an unlink or a rename over it
// removed, is reported nowhere, though RFC 22 §8 expects a change made over
// NFS to reach an SMB watch. A file's record names no entry and the store
// keeps no index of a file's names (RFC 7 §13), so no name a listing shows
// is known for it, and a stale one would name a file as no listing does.
// So an NFSv3 WRITE or SETATTR, or I/O under an anonymous stateid, of a
// regular file changes no watch. Overturned by a client seen watching for
// those; then the store keeps a file's names, at one write per create, link
// and rename, and the file is reported under each.
func (fs *Filesystem) entryChanged(ctx context.Context, a *admitted, ref *vfs.OpenRef, f vfs.File, what openstate.ChangeMask) {
	if what == 0 {
		return
	}
	if set, _ := fs.watchers.on(f.Share, what); !set {
		return
	}
	ctx, cancel := fs.uncancelled(ctx)
	defer cancel()
	if ref != nil && (ref.Kind == openstate.RefOpened || ref.Kind == openstate.RefLocked) {
		dir, name, err := fs.opens.Entry(ctx, a.who(), keyOf(f), *ref)
		if err == nil && name != nil {
			fs.changedIn(ctx, entity.NewHandle(f.Share, dir), openstate.Change{What: what, Name: name})
			return
		}
	}
	if !f.IsDir() || f.IsRoot() {
		return
	}
	up, err := fs.meta.Files().Get(ctx, system, entity.NewHandle(f.Share, f.Parent))
	if err != nil {
		return
	}
	if name, err := fs.nameIn(ctx, up, f.ID); err == nil {
		fs.changed(ctx, up, openstate.Change{What: what, Name: name})
	}
}

// changedIn is changed for the directory dir names, once a watch taking ch
// is set on its share, reading dir as system on a context the client
// dropping does not end: a change that committed is reported though its
// client dropped right after. The call's deadline still ends the report, so
// a store that stops answering does not hold the call's locks past it.
func (fs *Filesystem) changedIn(ctx context.Context, dir vfs.Handle, ch openstate.Change) {
	share, _, _ := dir.Parse()
	if set, _ := fs.watchers.on(share, ch.What); !set {
		return
	}
	ctx, cancel := fs.uncancelled(ctx)
	defer cancel()
	if d, err := fs.meta.Files().Get(ctx, system, dir); err == nil {
		fs.changed(ctx, d, ch)
	}
}

// written is what a staged write or deallocate changed, from its WCC: the
// times, and the size when it moved.
func written(w vfs.WCC) openstate.ChangeMask {
	if w.Before != nil && w.Before.Size != w.After.Size {
		return openstate.ChangeTimes | openstate.ChangeSize
	}
	return openstate.ChangeTimes
}
