package vfs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/internal/vfs/pseudofs"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// allRights is every right a caller can be granted, the fourteen entry
// rights, which a share's root reports as its maximal access.
const allRights = entity.EntryRights

// Root resolves a share by the path or the name the client sent, admits the
// call, authorises it on the share's root, and answers the root's handle
// with the share's info; the zero ShareRef answers the pseudo-root, which
// admits nothing. A ShareRef naming both a path and a name is ErrInvalid,
// one naming no share ErrNotFound.
func (fs *Filesystem) Root(ctx context.Context, c vfs.Call, ref vfs.ShareRef) (vfs.Handle, vfs.ShareInfo, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, vfs.ShareInfo{}, err
	}
	defer done()
	var share entity.ShareID
	ix := fs.meta.ShareIndex()
	switch {
	case ref == vfs.ShareRef{}:
		t, err := fs.tree(ctx)
		if err != nil {
			return nil, vfs.ShareInfo{}, err
		}
		return t.Root().Handle(), vfs.ShareInfo{}, nil
	case ref.Path != "" && ref.Name != "":
		return nil, vfs.ShareInfo{}, vfs.ErrInvalid
	case ref.Path != "":
		share, err = ix.ByPath(ctx, ref.Path)
	default:
		share, err = ix.ByName(ctx, ref.Name)
	}
	if err != nil {
		return nil, vfs.ShareInfo{}, storeErr(err)
	}
	a, release, err := fs.admit(ctx, c, share, true)
	if err != nil {
		return nil, vfs.ShareInfo{}, err
	}
	defer release()
	h := a.root()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpRoot, h, nil, nil, entity.ReadAttributes); err != nil {
		return nil, vfs.ShareInfo{}, storeErr(err)
	}
	info := shareInfo(a.share, a.policy)
	if info.MaximalAccess, err = fs.authz.Rights(ctx, a.caller, h, nil, allRights); err != nil {
		return nil, vfs.ShareInfo{}, storeErr(err)
	}
	return h, info, nil
}

// shareInfo is what a share tells a client about itself, its maximal access
// left zero.
func shareInfo(sh entity.Share, p entity.ExportPolicy) vfs.ShareInfo {
	return vfs.ShareInfo{
		Name:    sh.Name,
		Flavors: flavors(p),
		Hidden:  p.SMB&entity.SMBHidden != 0 || entity.HiddenShareName(sh.Name),
		SMB:     p.SMB &^ entity.SMBHidden,
	}
}

// ListShares is every share not hidden that the caller would be admitted to,
// each without its maximal access, which only Root computes.
//
// decision: a quiesced share is listed. Its refusal is a retry, not a
// refusal of the caller, and a listing that dropped every paused share would
// show it gone for the pause. Leave it out if a client is found that maps a
// listed share it cannot reach yet to an error it does not retry.
//
// decision: the listing runs admission alone and consults no grant, so a
// share is listed to a caller its grants refuse, as an SMB server's share
// enumeration lists a share whose ACL refuses the caller. Only a share's
// existence and name are shown; Root refuses the caller. Check the grant
// on the root, as the pseudo-filesystem's listing does, if share names are
// found to be secret.
func (fs *Filesystem) ListShares(ctx context.Context, c vfs.Call) ([]vfs.ShareInfo, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	_, shares, err := fs.meta.ShareIndex().List(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	var out []vfs.ShareInfo
	for _, sh := range shares {
		a, ok, err := fs.wouldAdmit(ctx, c, sh.ID, true)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if info := shareInfo(a.share, a.policy); !info.Hidden {
			out = append(out, info)
		}
	}
	return out, nil
}

// wouldAdmit reports whether the call c would be admitted to share, now
// or, for a paused share, once it resumes: its security minimums, client
// rules and resolution run as for an enabled share. It answers the share and
// its policy as read; secure false skips the minimums, as admit's does. It
// counts nothing, and errs wherever admission failed rather than refused.
func (fs *Filesystem) wouldAdmit(ctx context.Context, c vfs.Call, share entity.ShareID, secure bool) (*admitted, bool, error) {
	a, err := fs.check(ctx, c, share, secure, true)
	switch {
	case err == nil, errors.Is(err, vfs.ErrDelay):
		return a, true, nil
	case refused(err):
		return a, false, nil
	}
	return a, false, err
}

// refused reports an admission refusing the call, as against failing: a
// share gone, a security minimum or a client rule unmet, or a caller
// resolution refuses.
func refused(err error) bool {
	return errors.Is(err, vfs.ErrStale) || errors.Is(err, vfs.ErrWrongSecurity) || errors.Is(err, vfs.ErrAccess)
}

// SecInfo is the flavours a client may use for the object name names in dir:
// in a pseudo directory, a share's own for the place it is mounted, and
// every flavour admitted by a share below that admits the client for a
// directory above shares; in a share, the share's own. A share is admitted
// with its state and client rules but not its security minimums, since what
// SecInfo answers is how to meet them.
//
// ponytail: in a share, name is not looked up: the answer is the share's
// flavours whether or not name exists. Look it up, answering ErrNotFound,
// once Lookup reaches the store.
func (fs *Filesystem) SecInfo(ctx context.Context, c vfs.Call, dir vfs.Handle, name []byte) ([]vfs.AuthFlavor, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	var share *[16]byte
	var below [][16]byte
	if pseudofs.IsHandle(dir) {
		n, _, err := fs.pseudoChild(ctx, dir, name)
		if err != nil {
			return nil, err
		}
		share, below = n.Share, n.Shares
	} else {
		in, _, err := dir.Parse()
		if err != nil {
			return nil, vfs.ErrStale
		}
		share = (*[16]byte)(&in)
	}
	if share != nil {
		a, release, err := fs.admit(ctx, c, *share, false)
		if err != nil {
			return nil, err
		}
		defer release()
		return flavors(a.policy), nil
	}
	var out []vfs.AuthFlavor
	for _, share := range below {
		a, ok, err := fs.wouldAdmit(ctx, c, share, false)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, f := range flavors(a.policy) {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// StatFS is the share's usage, its share-wide quota and what its files can
// carry; a pseudo directory holds nothing and answers the zero info.
//
// ponytail: answers the share's usage and quota alone, never the tighter of
// it and h's project's, as the store has no read of a project's usage. Read
// the project's with that view, comparing their remaining room.
func (fs *Filesystem) StatFS(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.FilesystemInfo, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.FilesystemInfo{}, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil || a == nil {
		return vfs.FilesystemInfo{}, fs.pseudoOnly(ctx, h, err)
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpStatFS, h, nil, nil, entity.ReadAttributes); err != nil {
		return vfs.FilesystemInfo{}, storeErr(err)
	}
	capacity := fs.meta.Capacity()
	u, err := capacity.Usage(ctx, a.share.ID)
	if err != nil {
		return vfs.FilesystemInfo{}, storeErr(err)
	}
	q, _, err := capacity.Quota(ctx, a.share.ID, kvstore.QuotaTarget{})
	if err != nil {
		return vfs.FilesystemInfo{}, storeErr(err)
	}
	return vfs.FilesystemInfo{
		Share: a.share.ID,
		Capabilities: vfs.Capabilities{
			CaseSensitive: a.share.Fold == entity.FoldIdentity, CasePreserving: true, MaxNameBytes: 255,
			ACLs: true, Xattrs: true, Streams: true, Links: true, TimeGranularity: 1,
		},
		Usage: u,
		Quota: q,
	}, nil
}

// Quota is p's usage and quota in h's share, the zero quota when p has none;
// p is a user or a group, ErrInvalid otherwise.
//
// decision: a caller who may read h's attributes may read any principal's
// quota and usage in the share, as a share's StatFS shows its own to every
// caller and a quota names an amount, not a file. Overturned if quotas are
// found to disclose what a principal holds: then only the principal itself,
// its groups' members, or an admin may read one.
//
// decision: a principal with no quota of its own answers the zero quota, no
// limit, since the store keeps no per-kind default. Answer the default once
// one is kept.
func (fs *Filesystem) Quota(ctx context.Context, c vfs.Call, h vfs.Handle, p vfs.Principal) (vfs.Usage, vfs.Quota, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.Usage{}, vfs.Quota{}, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil || a == nil {
		return vfs.Usage{}, vfs.Quota{}, fs.pseudoOnly(ctx, h, err)
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpQuota, h, nil, nil, entity.ReadAttributes); err != nil {
		return vfs.Usage{}, vfs.Quota{}, storeErr(err)
	}
	if p == (vfs.Principal{}) {
		return vfs.Usage{}, vfs.Quota{}, vfs.ErrInvalid
	}
	capacity := fs.meta.Capacity()
	q, _, err := capacity.Quota(ctx, a.share.ID, kvstore.QuotaTarget{Principal: p})
	if err != nil {
		return vfs.Usage{}, vfs.Quota{}, storeErr(err)
	}
	u, err := capacity.PrincipalUsage(ctx, a.share.ID, p)
	return u, q, storeErr(err)
}

// Snapshots is the snapshots visible at h: none, as no snapshot is read yet
// (see vfs.SnapshotInfo). The call is admitted like any other.
func (fs *Filesystem) Snapshots(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.SnapshotInfo, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil || a == nil {
		return nil, fs.pseudoOnly(ctx, h, err)
	}
	release()
	return nil, nil
}

// pseudoOnly is a call's answer when admitHandle admitted it to no share:
// its error, or for a pseudo directory nil when it names a node, ErrStale
// otherwise.
func (fs *Filesystem) pseudoOnly(ctx context.Context, h vfs.Handle, err error) error {
	if err != nil {
		return err
	}
	_, err = fs.pseudoNode(ctx, h)
	return err
}

// errUnsigned is the Principals area's answer until an identity component
// signs callers in and spells principals.
//
// ponytail: SignIn, Spell and Find answer ErrAccess, as nothing under them
// exists: no authenticator, no principal mapping. Delegate each to RFC 18's
// identity component, held in Deps, once it lands.
var errUnsigned = fmt.Errorf("vfs: no identity component signs in or spells principals: %w", vfs.ErrAccess)

func (fs *Filesystem) SignIn(ctx context.Context, step vfs.SignInStep) (vfs.SignInResult, error) {
	return vfs.SignInResult{}, fs.unsigned(ctx)
}

func (fs *Filesystem) Spell(ctx context.Context, p vfs.Principal, as vfs.Scheme) (vfs.Spelling, error) {
	return vfs.Spelling{}, fs.unsigned(ctx)
}

func (fs *Filesystem) Find(ctx context.Context, c vfs.Call, sp vfs.Spelling) (vfs.Principal, error) {
	return vfs.Principal{}, fs.unsigned(ctx)
}

// unsigned is the body of a Principals call: not admitted, as it acts on no
// share, but refused outside serving like any call.
func (fs *Filesystem) unsigned(ctx context.Context) error {
	_, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	done()
	return errUnsigned
}
