package vfs

import (
	"context"
	"net/netip"
	"strconv"

	"github.com/marmos91/dittofs/internal/vfs/admit"
	"github.com/marmos91/dittofs/internal/vfs/authz"
	"github.com/marmos91/dittofs/internal/vfs/pseudofs"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// admitted is one call admitted to one share: the caller it acts as there,
// and the share as admission read it.
type admitted struct {
	caller authz.Caller
	share  entity.Share
	policy entity.ExportPolicy
}

// root is the share's root handle.
func (a *admitted) root() vfs.Handle { return entity.NewHandle(a.share.ID, a.share.Root) }

// admitHandle admits a call naming h to h's share, before anything resolves
// h: a handle of no valid form is ErrStale, and a pseudo directory's handle
// names no share and is admitted to none, answering nil. The release, never
// nil, uncounts the call; it is called once the call has answered.
func (fs *Filesystem) admitHandle(ctx context.Context, c vfs.Call, h vfs.Handle) (*admitted, func(), error) {
	if pseudofs.IsHandle(h) {
		return nil, func() {}, nil
	}
	share, _, err := h.Parse()
	if err != nil {
		return nil, func() {}, vfs.ErrStale
	}
	return fs.admit(ctx, c, share, true)
}

// admit runs admission for the call c reaching share: it counts the call
// against the share, then runs the share's state, its security minimums and
// its client rules, then admit.Resolve resolves its Identity on the share.
// secure false skips the minimums alone, for the one call whose answer is the
// remedy for failing them. A share never created is ErrStale, as a handle naming it is.
//
// ponytail: admission's steps 1-4 run on every call, uncached; cache them
// per (share, identity) with every version §4.9 lists in the key once a
// profile shows them.
//
// decision: the share cap on the node's request buffers, admission's last
// step, is not run: no request-buffer pool exists to charge. Add it, refusing
// with DelayResources except for a call that only releases or reclaims
// state, with the pools.
func (fs *Filesystem) admit(ctx context.Context, c vfs.Call, share entity.ShareID, secure bool) (*admitted, func(), error) {
	release := fs.count(share)
	if _, err := fs.tree(ctx); err != nil {
		release()
		return nil, func() {}, err
	}
	a, err := fs.check(ctx, c, share, secure, false)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return a, release, nil
}

// check is admission's steps 1 to 4 for the call c on share, counting nothing:
// whether a call would be admitted. Once the share is read, the admission it
// answers holds it and its policy, refused or not. With paused, a quiesced
// share is admitted as an enabled one would be, and ErrDelay answered only
// once every later step admits the call.
func (fs *Filesystem) check(ctx context.Context, c vfs.Call, share entity.ShareID, secure, paused bool) (*admitted, error) {
	if fs.beforeShareRead != nil {
		fs.beforeShareRead()
	}
	sh, p, err := fs.meta.ShareIndex().Share(ctx, share)
	if merrs.IsNotFoundError(err) {
		return nil, vfs.ErrStale
	}
	if err != nil {
		return nil, storeErr(err)
	}
	a := &admitted{share: sh, policy: p}
	delay := paused && sh.State == entity.ShareQuiesced
	if err := sh.State.Gate(); err != nil && !delay {
		return a, storeErr(err)
	}
	in := p
	if !secure {
		in.MinProof, in.MinProtection = entity.ProofNone, entity.ProtectionNone
	}
	byClient, err := admit.Check(ctx, in, c.Identity, netgroups{fs.meta.ShareIndex()})
	if err != nil {
		return a, storeErr(err)
	}
	ro := entity.NotNarrowed
	switch {
	case p.ReadOnly:
		ro = entity.NarrowedByShare
	case byClient:
		ro = entity.NarrowedByClient
	}
	a.caller = authz.Caller{Share: sh.ID, Proved: c.Identity, Client: c.Client, Protocol: c.Protocol}
	a.caller.Identity, a.caller.Mapped, err = admit.Resolve(ctx, principals{fs.meta.Principals()}, c.Identity, sh.ID, p, ro)
	if err == nil && delay {
		err = &vfs.Delay{Cause: vfs.DelayFrozen}
	}
	return a, err
}

// count counts a call against share, before admission reads the share's
// state, and returns its uncount: so a removal whose mark has committed, and
// which then waits in JoinShare, waits for every call that read the state
// before the mark, and one counting after the mark reads it.
func (fs *Filesystem) count(share entity.ShareID) func() {
	fs.mu.Lock()
	j := fs.joins[share]
	if j == nil {
		j = &join{zero: make(chan struct{})}
		fs.joins[share] = j
	}
	j.n++
	fs.mu.Unlock()
	return func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if j.n--; j.n == 0 {
			close(j.zero)
			delete(fs.joins, share)
		}
	}
}

// join is the calls counted against one share.
type join struct {
	n    int
	zero chan struct{} // closed when n reaches zero
}

// JoinShare returns once no call is counted against share: every call
// admitted to it before JoinShare was called, and any counted since, has
// answered. A share's removal calls it once its mark has committed, and
// before it ends the share's open state; every call counted after the mark
// reads the mark and is refused at once. At ctx's end it answers ctx's
// error.
//
// ponytail: waits for the share's count to reach zero, not for the calls
// counted before it was called, so calls counted after the mark that keep
// overlapping one another hold it, each refused but never all at once.
// Count each share's calls per epoch, and wait for the epochs before the
// mark's, if a removal is seen to wait on them.
//
// ponytail: only removeShare calls it, on a share seen to leave the share
// list while serving; nothing runs it at Start, so a share marked before a
// restart keeps its open state. The node, not the control plane's
// delete, runs a removal's join, while serving and again at Start, so a
// delete answers at once and a crash between the mark and the join is
// finished at the next start. Replace removeShare with the node's removal
// driver, which runs both.
func (fs *Filesystem) JoinShare(ctx context.Context, share entity.ShareID) error {
	fs.mu.Lock()
	j := fs.joins[share]
	fs.mu.Unlock()
	if j == nil {
		return nil
	}
	select {
	case <-j.zero:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// removeShare is the node's side of a share's removal, run once for each
// share seen to leave the share list while serving: in the background, and
// counted as a call, so a drain waits for it and its deadline ends it, it
// joins the share's calls, then has open state drop the share's state,
// telling each holder.
//
// ponytail: joins and drops a share the store marked for removal, and
// nothing after; RFC 16 §2.3.2's full removal driver, on the node,
// replaces it. It is started from admission, by a share gone from the share
// list when its version moves, which costs every admission a read of that
// version; a share marked before the first call builds the list is never
// seen to leave it, and a run a drain or a stopped table fails is not run
// again. It drops every shard of the share, not only those this node is
// primary of, and keeps every share it ran for, one entry a removed share,
// for the filesystem's life.
func (fs *Filesystem) removeShare(share entity.ShareID) {
	fs.mu.Lock()
	seen := fs.removed[share]
	fs.removed[share] = true
	fs.mu.Unlock()
	if seen {
		return
	}
	ctx, done, err := fs.enter(context.Background())
	if err != nil {
		return
	}
	go func() {
		defer done()
		if fs.JoinShare(ctx, share) == nil {
			_ = fs.open.Recovery().DropShare(ctx, share, netip.Addr{})
		}
	}()
}

// flavors is what a share lists, as SECINFO and a share's info list it, in
// the policy's preference order: each flavour it names that its minimums
// would admit.
//
// decision: Kerberos lists krb5p, krb5i and krb5, strongest first, down to
// the protection floor; sys is listed only while the proof floor admits a
// claim, and none only while it admits no proof. A policy naming no flavour
// lists sys, the three Kerberos levels and none, as its minimums allow. NTLM
// is not listed: it is no NFS flavour, and an SMB client negotiates its
// mechanism apart from this list. List it if a caller of ShareInfo is found
// that needs it.
func flavors(p entity.ExportPolicy) []vfs.AuthFlavor {
	names := p.Flavors
	if len(names) == 0 {
		names = []entity.AuthFlavor{entity.FlavorSys, entity.FlavorKerberos, entity.FlavorNone}
	}
	var out []vfs.AuthFlavor
	for _, f := range names {
		switch {
		case f == entity.FlavorKerberos:
			out = append(out, kerberos(vfs.Protection(p.MinProtection))...)
		case f == entity.FlavorSys && p.MinProof <= entity.ProofClaimed:
			out = append(out, vfs.FlavorSys)
		case f == entity.FlavorNone && p.MinProof == entity.ProofNone:
			out = append(out, vfs.FlavorNone)
		}
	}
	return out
}

// kerberos is the Kerberos flavours protecting at least from, strongest
// first.
func kerberos(from vfs.Protection) []vfs.AuthFlavor {
	var out []vfs.AuthFlavor
	for _, k := range []struct {
		p vfs.Protection
		f vfs.AuthFlavor
	}{{vfs.ProtectionPrivacy, vfs.FlavorKrb5p}, {vfs.ProtectionIntegrity, vfs.FlavorKrb5i}, {vfs.ProtectionNone, vfs.FlavorKrb5}} {
		if k.p >= from {
			out = append(out, k.f)
		}
	}
	return out
}

// netgroups is admission's read of a netgroup.
type netgroups struct{ x kvstore.ShareIndex }

func (n netgroups) Members(ctx context.Context, name string) ([]string, bool, error) {
	g, found, err := n.x.Netgroup(ctx, name)
	return g.Members, found, err
}

// principals is the resolve step's reads of the store, its errors mapped.
type principals struct{ pr kvstore.Principals }

func (p principals) ByID(ctx context.Context, scheme entity.ProtocolScheme, n uint32) (entity.Principal, bool, error) {
	got, err := p.pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(n), 10)})
	if merrs.IsNotFoundError(err) {
		return entity.Principal{}, false, nil
	}
	return got, err == nil, storeErr(err)
}

func (p principals) User(ctx context.Context, of entity.Principal) (entity.User, bool, error) {
	u, err := p.pr.User(ctx, of)
	if merrs.IsNotFoundError(err) {
		return u, false, nil
	}
	return u, err == nil, storeErr(err)
}

func (p principals) Grant(ctx context.Context, share entity.ShareID, of entity.Principal) (entity.ShareAccess, error) {
	a, err := p.pr.Grant(ctx, share, of)
	return a, storeErr(err)
}

func (p principals) Groups(ctx context.Context, of entity.Principal) ([]entity.Principal, error) {
	var out []entity.Principal
	for g, err := range p.pr.Groups(ctx, of) {
		if err != nil {
			return nil, storeErr(err)
		}
		out = append(out, g)
	}
	return out, nil
}
