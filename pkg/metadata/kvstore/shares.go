package kvstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"iter"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Shares lists every share with its state, in folded-name order.
func (c ControlPlane) Shares(ctx context.Context) iter.Seq2[entity.Share, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXShare}, nil,
		func(r kv.Reader, row kv.KeyValue) (entity.Share, error) {
			id, err := decodeID(row.Value)
			if err != nil {
				return entity.Share{}, err
			}
			return shareIn(r, id)
		})
}

func shareIn(r kv.Reader, id entity.ShareID) (entity.Share, error) {
	b, err := r.Get(shareKey(id, keys.SInfo))
	if err != nil {
		return entity.Share{}, err
	}
	s, err := decodeShareInfo(id, b)
	if err != nil {
		return s, err
	}
	if b, err = r.Get(shareKey(id, keys.SSt)); err != nil {
		return s, err
	}
	st, err := decodeState(b)
	if err != nil {
		return s, err
	}
	s.State = st.State
	if b, ok, err := get(r, namespacesKey(id)); err != nil || !ok {
		return s, err
	} else if nss, err := decodeNamespaces(b); err != nil {
		return s, err
	} else {
		s.Namespaces, s.Write = nss.Namespaces, nss.Write
	}
	return s, nil
}

// PutShare creates a share or changes its name or path; its root never
// changes. It writes the share's info record and its name and path rows, and
// raises ShareList. State is not its to write: a new share starts enabled,
// and only SetShareState changes it after.
//
// No share's path is an ancestor of another's. An ancestor is found by a
// point read of each of the path's prefixes, a descendant by a guarded scan
// of the rows below it, so two shares created at once on nested paths cannot
// both commit, in either order.
//
// ponytail: a new share writes no shard list: every share has one shard,
// zero, which callers name directly. Write the list in the transaction that
// creates each shard once a share can have more than one.
func (c ControlPlane) PutShare(ctx context.Context, s entity.Share) error {
	if s.ID.IsZero() || s.Root.IsZero() {
		return merrs.NewInvalidArgumentError("a share needs an ID and a root")
	}
	if !entity.ValidShareName(s.Name) {
		return merrs.NewInvalidArgumentError("share name " + s.Name + " not valid")
	}
	if !entity.ValidSharePath(s.Path) {
		return merrs.NewInvalidArgumentError("share path " + s.Path + " not valid")
	}
	if s.Fold != 0 && !slices.Contains(entity.FoldRules, s.Fold) || s.Names != 0 && s.Names.String() == "" {
		return merrs.NewInvalidArgumentError("unknown fold or name rule")
	}
	key := s.DigestKey
	if key == ([entity.DigestKeyLen]byte{}) {
		rand.Read(key[:])
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		info := shareKey(s.ID, keys.SInfo)
		b, existed, err := get(tx, info)
		if err != nil {
			return err
		}
		var old entity.Share
		if existed {
			if old, err = decodeShareInfo(s.ID, b); err != nil {
				return err
			}
			if err := liveShare(tx, s.ID); err != nil {
				return err
			}
			if old.Root != s.Root {
				return merrs.NewInvalidArgumentError("a share's root never changes")
			}
			if err := keepBound(&s, old); err != nil {
				return err
			}
			if old.Name == s.Name && old.Path == s.Path {
				return nil
			}
		} else {
			bindNew(&s, key)
		}
		if err := c.claimShareName(tx, s, existed, old.Name); err != nil {
			return err
		}
		if err := c.claimSharePath(tx, s, existed, old.Path); err != nil {
			return err
		}
		if !existed {
			if err := createShareRecords(tx, &s); err != nil {
				return err
			}
		}
		if err := tx.Set(info, encodeShareInfo(s)); err != nil {
			return err
		}
		return raise(tx, shareListKey)
	})
}

// createShareRecords writes what a new share starts with besides its info:
// its state, enabled; its root directory; its numeric file id allocator,
// above the root's number; and a namespace of its own, generation 1, that its
// content is counted in.
//
// decision: every share gets a fresh namespace. Shares sharing one, so that
// their content deduplicates and clones across them, is a choice the
// configuration makes; take the namespace from the share's settings once
// they name one. The root's key must be free, so a root ID that
// already names a file is refused rather than overwritten.
func createShareRecords(tx kv.Txn, s *entity.Share) error {
	tag, err := nextTag(tx)
	if err != nil {
		return err
	}
	s.Tag = tag
	root := fileKey(s.ID, s.Root)
	rf := newRoot(tx, *s)
	if _, ok, err := get(tx, root); err != nil {
		return err
	} else if ok {
		return merrs.NewAlreadyExistsError("root " + s.Root.String())
	}
	for _, kvs := range [][2][]byte{
		{shareKey(s.ID, keys.SSt), encodeState(shareState{State: entity.ShareEnabled})},
		{root, encodeFile(rf)},
		{floorKey(s.ID, s.Root, rf.Version), encodeEmpty()},
		{numberKey(s.ID), encodeCounter(rootNumber + 1)},
		{namespacesKey(s.ID), encodeNamespaces(shareNamespaces{
			Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: entity.NewNamespaceID()}}, Write: 1})},
	} {
		if err := tx.Set(kvs[0], kvs[1]); err != nil {
			return err
		}
	}
	return chargeFile(tx, s.ID, nil, &rf)
}

// ErrTagsExhausted is an installation that has given every journal tag.
var ErrTagsExhausted = errors.New("kvstore: installation has no share tags left")

// nextTag mints a journal tag no share of the installation has had: the
// tag counter's next value, from 1, since a zero tag names no share.
func nextTag(tx kv.Txn) (uint32, error) {
	b, ok, err := get(tx, tagKey)
	if err != nil {
		return 0, err
	}
	var v uint64
	if ok {
		if v, err = decodeCounter(b); err != nil {
			return 0, err
		}
	}
	if v >= math.MaxUint32 {
		return 0, ErrTagsExhausted
	}
	return uint32(v + 1), tx.Set(tagKey, encodeCounter(v+1))
}

// liveShare refuses a share being removed or retired, which is stale to
// every caller. The read is tracked, so a write that passed it conflicts with
// a removal that marks the share before it commits.
func liveShare(r kv.Reader, share entity.ShareID) error {
	b, ok, err := get(r, shareKey(share, keys.SSt))
	if err != nil || !ok {
		return err
	}
	st, err := decodeState(b)
	if err != nil {
		return err
	}
	if st.State == entity.ShareRemoving || st.State == entity.ShareRetired {
		return entity.ErrStale
	}
	return nil
}

// bindNew gives a new share its bound settings: the identity fold unless
// one is named, the name rule that goes with its fold unless one is named,
// and a digest key drawn at random unless one is given.
func bindNew(s *entity.Share, key [entity.DigestKeyLen]byte) {
	if s.Fold == 0 {
		s.Fold = entity.FoldIdentity
	}
	if s.Names == 0 {
		s.Names = entity.NamesPOSIX
		if s.Fold == entity.FoldUpcase {
			s.Names = entity.NamesWindows
		}
	}
	s.DigestKey = key
}

// keepBound carries an existing share's bound settings into a put, a zero
// field meaning "unchanged", and refuses any other value.
func keepBound(s *entity.Share, old entity.Share) error {
	if s.Fold != 0 && s.Fold != old.Fold || s.Names != 0 && s.Names != old.Names ||
		s.DigestKey != ([entity.DigestKeyLen]byte{}) && s.DigestKey != old.DigestKey {
		return merrs.NewInvalidArgumentError("a share's fold rule, name rule and digest key never change")
	}
	s.Fold, s.Names, s.DigestKey, s.Tag = old.Fold, old.Names, old.DigestKey, old.Tag
	return nil
}

func (c ControlPlane) claimShareName(tx kv.Txn, s entity.Share, existed bool, oldName string) error {
	key := nameKey(keys.NXShare, s.Name)
	if existed && bytes.Equal(key, nameKey(keys.NXShare, oldName)) {
		return nil // a change of case only: the row stays
	}
	if err := claimShare(tx, key, s.ID, "share name "+s.Name+" taken"); err != nil {
		return err
	}
	if existed {
		return tx.Delete(nameKey(keys.NXShare, oldName))
	}
	return nil
}

func (c ControlPlane) claimSharePath(tx kv.Txn, s entity.Share, existed bool, oldPath string) error {
	if existed && s.Path == oldPath {
		return nil
	}
	if err := claimShare(tx, pathKey(s.Path), s.ID, "share path "+s.Path+" taken"); err != nil {
		return err
	}
	for _, a := range entity.PathAncestors(s.Path) {
		if b, ok, err := get(tx, pathKey(a)); err != nil {
			return err
		} else if ok && !holds(b, s.ID) {
			return merrs.NewAlreadyExistsError("share path " + s.Path + " is inside share path " + a)
		}
	}
	// Every share put also raises ShareList, which already orders two puts;
	// the guard keeps this rule from resting on that counter.
	below := pathsBelow(s.Path)
	if err := tx.GuardRange(below); err != nil {
		return err
	}
	for row, err := range tx.Scan(below, nil) {
		if err != nil {
			return err
		}
		if !holds(row.Value, s.ID) {
			return merrs.NewAlreadyExistsError("share path " + s.Path + " contains another share")
		}
	}
	if existed {
		return tx.Delete(pathKey(oldPath))
	}
	return nil
}

// holds reports whether an index row's value names id. A row that does not
// decode names nobody this caller can be.
func holds(b []byte, id entity.ShareID) bool {
	got, err := decodeID(b)
	return err == nil && got == [16]byte(id)
}

func claimShare(tx kv.Txn, key []byte, id entity.ShareID, taken string) error {
	b, ok, err := get(tx, key)
	if err != nil {
		return err
	}
	if ok {
		if !holds(b, id) {
			return merrs.NewAlreadyExistsError(taken)
		}
		return nil
	}
	return tx.Set(key, encodeID(id))
}

// SetShareState moves a share from one state to another, refusing when it is
// not in from: the read is tracked, so of two changes racing from one state,
// one conflicts and then finds the state moved. It raises ShareList.
//
// Removal is one-way and not this call's: only DeleteShare moves a share to
// being removed, and a share being removed or retired is stale to it.
func (c ControlPlane) SetShareState(ctx context.Context, share entity.ShareID, from, to entity.ShareState) error {
	switch {
	case to.String() == "":
		return merrs.NewInvalidArgumentError("unknown share state")
	case to == entity.ShareRemoving || to == entity.ShareRetired:
		return merrs.NewInvalidArgumentError("only DeleteShare removes a share")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		key := shareKey(share, keys.SSt)
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(share.String(), "share")
		}
		st, err := decodeState(b)
		if err != nil {
			return err
		}
		cur := st.State
		if cur == entity.ShareRemoving || cur == entity.ShareRetired {
			return entity.ErrStale
		}
		if cur != from {
			return merrs.NewConflictError("set share state", "share is "+cur.String()+", not "+from.String())
		}
		if from == to {
			return nil
		}
		if err := tx.Set(key, encodeState(shareState{State: to})); err != nil {
			return err
		}
		return raise(tx, shareListKey)
	})
}

// SetGrant sets a principal's access to a share, raising the grant's
// version. A grant of none removes it, with its reverse row; the principal's
// groups then decide its access. A grant names a user, a group, or the share's
// defaults, authenticated users or everyone. Share and principal must exist,
// read so that a deletion of either conflicts; a default always exists.
func (c ControlPlane) SetGrant(ctx context.Context, g entity.ShareGrant) error {
	isDefault := g.Principal == entity.AuthenticatedUsers || g.Principal == entity.Everyone
	if g.Access > entity.AccessAdmin || (g.Principal.Kind != entity.PrincipalUser && g.Principal.Kind != entity.PrincipalGroup && !isDefault) {
		return merrs.NewInvalidArgumentError("a grant names a user, a group, authenticated users or everyone, and a known access")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, ok, err := get(tx, shareKey(g.Share, keys.SInfo)); err != nil || !ok {
			if err == nil {
				err = merrs.NewNotFoundError(g.Share.String(), "share")
			}
			return err
		}
		if err := liveShare(tx, g.Share); err != nil {
			return err
		}
		if _, ok, err := get(tx, principalKey(g.Principal)); !isDefault && (err != nil || !ok) {
			return notFound(err, g.Principal)
		}
		key, rev := grantKey(g.Share, g.Principal.ID), grantRevKey(g.Principal.ID, g.Share)
		old, _, err := grantIn(tx, g.Share, g.Principal.ID)
		if err != nil {
			return err
		}
		if g.Access == entity.AccessNone {
			if err := tx.Delete(key); err != nil {
				return err
			}
			return tx.Delete(rev)
		}
		g.Version = nextVersion(tx, old.Version)
		if err := tx.Set(key, encodeGrant(g)); err != nil {
			return err
		}
		return tx.Set(rev, nil)
	})
}

// ExportPolicy reads a share's export policy; a share without one has
// defaultPolicy's.
func (c ControlPlane) ExportPolicy(ctx context.Context, share entity.ShareID) (entity.ExportPolicy, error) {
	p := defaultPolicy(share)
	err := c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, shareKey(share, keys.SXp))
		if err != nil || !ok {
			return err
		}
		p, err = decodePolicy(share, b)
		return err
	})
	return p, err
}

// defaultPolicy is the export policy of a share none is stored for: root
// squashed to the anonymous principal, every client admitted, no flavour
// named.
func defaultPolicy(share entity.ShareID) entity.ExportPolicy {
	return entity.ExportPolicy{Share: share, Squash: entity.SquashRoot, Anonymous: entity.AnonymousPrincipal}
}

// PutExportPolicy replaces a share's export policy, raising its version. A
// policy naming no Anonymous names the anonymous principal: it is stored as
// given and read back as naming it.
func (c ControlPlane) PutExportPolicy(ctx context.Context, p entity.ExportPolicy) error {
	for _, f := range p.Flavors {
		if f.String() == "" {
			return merrs.NewInvalidArgumentError("unknown auth flavor")
		}
	}
	for _, r := range p.Clients {
		if !r.Valid() {
			return merrs.NewInvalidArgumentError("a client rule names one CIDR or netgroup, and at most read-write")
		}
	}
	if !p.Valid() {
		return merrs.NewInvalidArgumentError("an export policy's protection floor needs a verified proof, and both are known values")
	}
	if p.Squash.String() == "" {
		return merrs.NewInvalidArgumentError("unknown squash")
	}
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		if _, ok, err := get(tx, shareKey(p.Share, keys.SInfo)); err != nil || !ok {
			if err == nil {
				err = merrs.NewNotFoundError(p.Share.String(), "share")
			}
			return err
		}
		if err := liveShare(tx, p.Share); err != nil {
			return err
		}
		key := shareKey(p.Share, keys.SXp)
		b, ok, err := get(tx, key)
		if err != nil {
			return err
		}
		var old entity.ExportPolicy
		if ok {
			if old, err = decodePolicy(p.Share, b); err != nil {
				return err
			}
		}
		p.Version = nextVersion(tx, old.Version)
		return tx.Set(key, encodePolicy(p))
	})
}

// ShareList is raised by every share create, rename, path change and state
// change: the pseudo-filesystem's change attribute.
func (c ControlPlane) ShareList(ctx context.Context) (entity.ShareList, error) {
	v, err := counter(ctx, c.s.kv, shareListKey)
	return entity.ShareList{Version: v}, err
}
