package vfs

import (
	"context"
	"sort"

	"github.com/marmos91/dittofs/internal/vfs/authz"
	"github.com/marmos91/dittofs/internal/vfs/pseudofs"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// tree is the pseudo-filesystem of the share list as it is now: the one
// built last, while the share list's version has not moved, else one built
// afresh from the list. Both are a function of the list alone, so keeping
// one decides nothing a rebuild would decide otherwise. One call rebuilds
// at a time, and a call that waited for it answers its tree when the list
// has not moved again; a call whose context ends while it waits answers the
// context's error. A rebuild starts the removal of each share the last
// tree held and the list no longer does, since only a removal's mark takes a
// share off the list.
//
// decision: the first call that needs the pseudo-filesystem builds it, not
// Start, as RFC 17 §2.5 has it: a share list moved since Start is rebuilt
// here anyway, so a build at Start would only move the first build's cost.
// Overturned if Start must refuse a share list it cannot read, or the first
// call's latency matters.
func (fs *Filesystem) tree(ctx context.Context) (*pseudofs.Tree, error) {
	ix := fs.meta.ShareIndex()
	v, err := ix.Version(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	if t := fs.pseudo.Load(); t != nil && t.Version() == v.Version {
		return t, nil
	}
	select {
	case fs.build <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-fs.build }()
	old := fs.pseudo.Load()
	if old != nil && old.Version() >= v.Version {
		return old, nil
	}
	if fs.beforeBuild != nil {
		fs.beforeBuild()
	}
	in, err := fs.meta.Installation(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	list, shares, err := ix.List(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	placed := make([]pseudofs.Share, len(shares))
	listed := make(map[[16]byte]bool, len(shares))
	for i, sh := range shares {
		placed[i] = pseudofs.Share{ID: sh.ID, Path: sh.Path}
		listed[sh.ID] = true
	}
	t := pseudofs.Build(in.ID, list.Version, placed)
	fs.pseudo.Store(t)
	if old != nil {
		for _, id := range old.Root().Shares {
			if !listed[id] {
				fs.removeShare(id)
			}
		}
	}
	return t, nil
}

// pseudoNode is the pseudo directory h names: ErrStale when its path leads
// to no share now.
func (fs *Filesystem) pseudoNode(ctx context.Context, h vfs.Handle) (*pseudofs.Node, error) {
	t, err := fs.tree(ctx)
	if err != nil {
		return nil, err
	}
	n, ok := t.Node(h)
	if !ok {
		return nil, vfs.ErrStale
	}
	return n, nil
}

// pseudoChild is the node name names in the pseudo directory dir, ".." its
// parent, and the tree it is in.
func (fs *Filesystem) pseudoChild(ctx context.Context, dir vfs.Handle, name []byte) (*pseudofs.Node, *pseudofs.Tree, error) {
	t, err := fs.tree(ctx)
	if err != nil {
		return nil, nil, err
	}
	n, ok := t.Node(dir)
	if !ok {
		return nil, nil, vfs.ErrStale
	}
	if string(name) == ".." {
		return n.Parent, t, nil
	}
	if c := n.Child(string(name)); c != nil {
		return c, t, nil
	}
	return nil, nil, vfs.ErrNotFound
}

// pseudoFile is a pseudo directory's attributes: a read-only directory of
// no share, its ids from its path, its change attribute the share list's
// version.
func pseudoFile(t *pseudofs.Tree, n *pseudofs.Node) vfs.File {
	return vfs.File{
		ID: n.ID(), Type: entity.Directory, Mode: 0o555, Parent: n.Parent.ID(),
		Number: n.Number(), Version: entity.VersionOf(t.Version()), Nlink: uint32(2 + len(n.Children)),
	}
}

// pseudoLookup is Lookup in a pseudo directory. A name where a share is
// mounted crosses into the share: the call is admitted to it, and answers
// the share's root as the caller may read it.
//
// decision: a lookup walks to any pseudo directory and is refused only at a
// share's mount, with the share's admission error, though a listing leaves
// out what leads to no share the caller would be admitted to. A client
// refused by a share's flavour must reach its mount to be told so and ask
// SECINFO there, and a path is all a walk learns. Hide a path from a lookup
// too if share paths are found to be secret.
func (fs *Filesystem) pseudoLookup(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) (vfs.Handle, vfs.File, error) {
	n, t, err := fs.pseudoChild(ctx, dir, name)
	if err != nil {
		return nil, vfs.File{}, err
	}
	if n.Share == nil {
		return n.Handle(), pseudoFile(t, n), nil
	}
	a, release, err := fs.admit(ctx, c, *n.Share, true)
	if err != nil {
		return nil, vfs.File{}, err
	}
	defer release()
	h, f, err := fs.cross(ctx, a, vfs.OpLookup, name, true)
	return h, f, storeErr(err)
}

// cross decides a caller admitted to a's share crossing into it at its
// mount, named name, for op: through authz, so the decision is audited as
// any other, then, with file, the root's attributes as the caller reads
// them. It answers the store's error, unmapped.
func (fs *Filesystem) cross(ctx context.Context, a *admitted, op vfs.Op, name []byte, file bool) (vfs.Handle, vfs.File, error) {
	h := a.root()
	if err := fs.authz.Decide(ctx, a.caller, op, h, nil, name, entity.ReadAttributes); err != nil {
		return nil, vfs.File{}, err
	}
	if !file {
		return h, vfs.File{}, nil
	}
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, h)
	if err != nil {
		return nil, vfs.File{}, err
	}
	return h, f, nil
}

// pseudoReadDir is ReadDir of a pseudo directory: its children after the
// cursor, in cursor order, each listed only when it leads to a share the
// caller would be admitted to. A share's mount lists as the share's root,
// its File as the caller may read it, and is left out where the crossing
// into it is refused, whatever the listing asks of its entries; a paused
// share's mount, which the caller reaches after a retry, is crossed the same
// way and lists with a pseudo directory's File. A failure of any share's
// reads fails the listing.
//
// decision: a pseudo directory above a share, such as /hr over /hr/payroll,
// is listed to a caller admission admits there though its share grant gives
// it nothing below: only the mount checks the grant, by crossing into the
// share, so a caller with no grant learns the path one level up and not the
// mount. A lookup reaches any pseudo directory anyway, so this hides nothing
// a walk does not learn. Check the grants below a pseudo directory too if
// share paths are found to be secret.
//
// ponytail: each listing admits the caller to every share below each child,
// O(shares) admissions a page, and reads each mounted share's root again.
// Keep the admitted set per caller with admission's cache once one exists.
func (fs *Filesystem) pseudoReadDir(ctx context.Context, c vfs.Call, dir vfs.Handle, after vfs.Cursor, max int, want vfs.EntryFields) (vfs.Page, error) {
	t, err := fs.tree(ctx)
	if err != nil {
		return vfs.Page{}, err
	}
	n, ok := t.Node(dir)
	if !ok {
		return vfs.Page{}, vfs.ErrStale
	}
	var page vfs.Page
	from := sort.Search(len(n.Children), func(i int) bool { return n.Children[i].Cursor() > uint64(after) })
	page.EOF = true
	for _, child := range n.Children[from:] {
		if max > 0 && len(page.Entries) == max {
			page.EOF = false
			break
		}
		e := vfs.DirEntry{Name: []byte(child.Name), Cursor: vfs.Cursor(child.Cursor())}
		var admitted *admitted
		for _, share := range child.Shares {
			a, ok, err := fs.wouldAdmit(ctx, c, share, true)
			if err != nil {
				return vfs.Page{}, err
			}
			if ok {
				admitted = a
				break
			}
		}
		if admitted == nil {
			continue
		}
		if child.Share == nil {
			e.Handle = child.Handle()
			if want&vfs.EntryFile != 0 {
				f := pseudoFile(t, child)
				e.File = &f
			}
		} else {
			f := pseudoFile(t, child)
			read := want&vfs.EntryFile != 0 && admitted.share.State != entity.ShareQuiesced
			h, root, err := fs.cross(ctx, admitted, vfs.OpReadDir, e.Name, read)
			if authz.Refused(err) {
				continue
			}
			if err != nil {
				return vfs.Page{}, storeErr(err)
			}
			if e.Handle = h; read {
				f = root
			}
			if want&vfs.EntryFile != 0 {
				e.File = &f
			}
		}
		if want&vfs.EntryHandle == 0 {
			e.Handle = nil
		}
		page.Entries = append(page.Entries, e)
		page.Next = e.Cursor
	}
	return page, nil
}
