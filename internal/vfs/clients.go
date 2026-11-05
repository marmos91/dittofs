package vfs

import (
	"context"
	"errors"
	"slices"

	"github.com/marmos91/dittofs/internal/vfs/admit"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// The Clients area: client records and leases over open state's Clients,
// Recovery, Locks and Grants views, and the Callbacks Connect registers. A call
// here names no share, so its caller is mapped under no share's policy and
// never squashed: the principal a client record's Owner names.
//
// decision: this node's sessions are recorded under the zero node ID, and a
// session ref names node zero, as a single node has no other to tell apart;
// each entry carries the node epoch open state raised at its start. No
// Connect calls Rebind, since
// on one node the back channel never moves to another and nothing calls
// ChannelDown. Give the filesystem the node's ID, and rebind at a Connect
// that moves the path here, with the cluster.

// who is the openstate caller c acts as for client cl: the principal its
// Identity maps to, never squashed.
func (fs *Filesystem) who(ctx context.Context, c vfs.Call, cl vfs.ClientID) (openstate.Caller, error) {
	p, err := admit.Mapped(ctx, principals{fs.meta.Principals()}, c.Identity)
	if err != nil {
		return openstate.Caller{}, err
	}
	return openstate.Caller{Client: cl, Principal: p, Protocol: c.Protocol}, nil
}

// record is the client record info proposes or connects, owned by p. An SMB
// record names a machine and an NLM one a host, so neither holds an Owner:
// many users' sessions share one machine's record, and NLM proves no one.
//
// decision: so a stranger who presents a machine's ClientGuid joins its
// record, and shares its grant budget and the caching fence after a missed
// break: by holding grants, or missing a break of its own, it can deny the
// machine caching. It cannot read the machine's breaks, which go by open,
// nor answer them. Keying the record by principal would refuse every user
// but the first on a shared host. Overturned if a ClientGuid shared this way
// is seen in the wild.
func record(info vfs.ClientInfo, p entity.Principal) openstate.Client {
	if info.Protocol != entity.ProtocolNFSv4 {
		p = entity.Principal{}
	}
	return openstate.Client{ClientRecord: entity.ClientRecord{
		Protocol: info.Protocol, Name: info.Name, Boot: info.Boot,
		Owner: p, Protect: info.Protect, Adapter: info.Adapter, Recovery: info.Recovery,
	}}
}

func (fs *Filesystem) Propose(ctx context.Context, c vfs.Call, info vfs.ClientInfo) (vfs.Proposal, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.Proposal{}, err
	}
	defer done()
	who, err := fs.who(ctx, c, 0)
	if err != nil {
		return vfs.Proposal{}, err
	}
	p, err := fs.clients.Propose(ctx, record(info, who.Principal), c.Identity.Client)
	return p, openErr(err)
}

// Connect makes the client current in open state and registers cb as its
// Callbacks, replacing any this node held for it: who may connect a client
// is open state's to refuse, by the principal its record names. An SMB
// client is connected at its first authenticated session on the node, which
// lists the node among its sessions, under the node's current epoch, until
// the Disconnect of its last session there. A null or guest call never
// connects an SMB client, and is ErrInvalid. So is a zero info.Recovery: the
// kind sticks for the record's life, and a client of none is never told of a
// lost shard, so it holds the shard's grace open for the whole period.
// It answers the entry's Gen, zero for any other client.
//
// The adapter decides which session is a client's first and last on the
// node under a per-client step held only around that decision; the Gen
// orders the calls, so a session set up while the last one ends either lists
// the node again before the last Disconnect, which then removes nothing, or
// after it, listing the node anew. A re-list with no session behind it that
// lands after the last Disconnect lists the node too, at a Gen above the
// removed one, and the adapter, finding no session on its return, unlists
// the node with that Gen.
func (fs *Filesystem) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, uint64, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer done()
	if cb == nil || info.Recovery == 0 || info.Protocol == entity.ProtocolSMB && c.Identity.Proof < vfs.ProofSecret {
		return 0, 0, vfs.ErrInvalid
	}
	who, err := fs.who(ctx, c, 0)
	if err != nil {
		return 0, 0, err
	}
	id, gen, err := fs.clients.Connect(ctx, record(info, who.Principal), info.Proposal)
	if err != nil {
		return 0, 0, openErr(err)
	}
	var host string
	if info.Protocol == entity.ProtocolNLM {
		host = string(info.Name)
	}
	fs.reg.register(id, host, cb)
	return id, gen, nil
}

// AddSession records a session of the client here.
func (fs *Filesystem) AddSession(ctx context.Context, c vfs.Call, cl vfs.ClientID, prev []byte, next []byte) (vfs.ClientRecord, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.ClientRecord{}, err
	}
	defer done()
	rec, err := fs.clients.AddSession(ctx, cl, entity.NodeID{}, prev, next)
	return rec, openErr(err)
}

func (fs *Filesystem) Record(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientRecord, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.ClientRecord{}, err
	}
	defer done()
	rec, err := fs.clients.Record(ctx, cl)
	return rec, openErr(err)
}

func (fs *Filesystem) Renew(ctx context.Context, c vfs.Call, cl vfs.ClientID) (vfs.ClientStatus, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.ClientStatus{}, err
	}
	defer done()
	st, err := fs.clients.Renew(ctx, cl)
	return st, openErr(err)
}

// Disconnect ends session sess of the client in open state as end says, and
// answers the newer Gen of an entry a later listing kept. A last session's
// end that leaves an SMB record holding nothing releases it there, and its
// Callbacks are then dropped here.
func (fs *Filesystem) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, sess vfs.SessionRef, end vfs.SessionEnd) (uint64, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	gen, err := fs.clients.Disconnect(ctx, cl, sess, end)
	if errors.Is(err, openstate.ErrStaleClient) && end.Last {
		// A repeat after the release, which the adapter counts as success: the
		// Callbacks a failed first call left go too.
		fs.reg.unregister(cl)
		return 0, openErr(err)
	}
	if err != nil || !end.Last || gen != 0 {
		return gen, openErr(err)
	}
	if _, err := fs.clients.Record(ctx, cl); errors.Is(err, openstate.ErrStaleClient) {
		fs.reg.unregister(cl)
	}
	return 0, nil
}

// Destroy deletes the client in open state, then its Callbacks here.
func (fs *Filesystem) Destroy(ctx context.Context, c vfs.Call, cl vfs.ClientID) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, cl)
	if err != nil {
		return err
	}
	if err := fs.clients.Destroy(ctx, who); err != nil {
		return openErr(err)
	}
	fs.reg.unregister(cl)
	return nil
}

func (fs *Filesystem) ReclaimComplete(ctx context.Context, c vfs.Call, cl vfs.ClientID, shown []vfs.GraceRef) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, cl)
	if err != nil {
		return err
	}
	return openErr(fs.recovery.ReclaimComplete(ctx, who, shown))
}

func (fs *Filesystem) FreeState(ctx context.Context, c vfs.Call, cl vfs.ClientID, ref vfs.StateRef) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, cl)
	if err != nil {
		return err
	}
	return openErr(fs.recovery.FreeState(ctx, who, ref))
}

func (fs *Filesystem) TestState(ctx context.Context, c vfs.Call, cl vfs.ClientID, refs []vfs.StateRef) []error {
	errs := make([]error, len(refs))
	ctx, done, err := fs.enter(ctx)
	if err == nil {
		defer done()
		var who openstate.Caller
		if who, err = fs.who(ctx, c, cl); err == nil {
			for i, err := range fs.recovery.TestState(ctx, who, refs) {
				errs[i] = openErr(err)
			}
			return errs
		}
	}
	for i := range errs {
		errs[i] = err
	}
	return errs
}

func (fs *Filesystem) ReleaseLockOwner(ctx context.Context, c vfs.Call, cl vfs.ClientID, o vfs.LockOwner) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, cl)
	if err != nil {
		return err
	}
	return openErr(fs.lockState.ReleaseLockOwner(ctx, who, o))
}

// HostRestarted connects the host under its new NSM state, which ends the
// client of its old one as a reboot does. It registers no Callbacks:
// the host's next call connects with them.
func (fs *Filesystem) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	rec := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolNLM, Name: host, Boot: entity.NSMBoot(state), Recovery: entity.RecoverNotice}}
	_, _, err = fs.clients.Connect(ctx, rec, nil)
	return openErr(err)
}

// AckRecall answers a recall of g for the caller's client. A lease or an
// oplock is answered only by one of its openers' principals, never on the
// client alone: anyone else, a stranger presenting the machine's ClientGuid
// included, is StateError NotYours, and the recall stays in flight.
func (fs *Filesystem) AckRecall(ctx context.Context, c vfs.Call, g vfs.GrantID, to vfs.GrantKind) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	who, err := fs.who(ctx, c, c.Client)
	if err != nil {
		return err
	}
	opens, err := fs.grants.Openers(ctx, g)
	if err != nil {
		return openErr(err)
	}
	if len(opens) > 0 && !slices.ContainsFunc(opens, func(o openstate.Open) bool { return o.Principal == who.Principal }) {
		return &vfs.StateError{Kind: vfs.StateNotYours}
	}
	return openErr(fs.grants.AckRecall(ctx, who, g, to))
}
