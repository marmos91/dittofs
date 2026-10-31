package vfs

import (
	"context"
	"slices"

	"github.com/marmos91/dittofs/internal/vfs/admit"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// The Clients area: client records and leases over open state's Clients,
// Recovery, Locks and Grants views, and the callback paths Connect fills. A call
// here names no share, so its caller is mapped under no share's policy and
// never squashed: the principal a client record's Owner names.
//
// decision: this node's sessions are recorded under the zero node ID, as a
// single node has no other to tell apart, and no Connect calls Rebind, since
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
		Owner: p, Protect: info.Protect, Adapter: info.Adapter,
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
// callback path, replacing any this node held for it: who may connect a
// client is open state's to refuse, by the principal its record names. An
// SMB client is connected by each authenticated session, which also
// registers cb as that session's path, signed in as its caller: its breaks
// go by those (Sink). A null or guest session, or a call naming no session,
// never joins an SMB client, and is ErrInvalid.
func (fs *Filesystem) Connect(ctx context.Context, c vfs.Call, info vfs.ClientInfo, cb vfs.Callbacks) (vfs.ClientID, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	smb := info.Protocol == entity.ProtocolSMB
	if cb == nil || smb && (c.Identity.Proof < vfs.ProofSecret || c.Session == (vfs.SessionRef{})) {
		return 0, vfs.ErrInvalid
	}
	who, err := fs.who(ctx, c, 0)
	if err != nil {
		return 0, err
	}
	id, err := fs.clients.Connect(ctx, record(info, who.Principal), info.Proposal)
	if err != nil {
		return 0, openErr(err)
	}
	var sess vfs.SessionRef
	if smb {
		sess = c.Session
	}
	fs.reg.register(id, sess, who.Principal, cb)
	return id, nil
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

// Disconnect ends session sess of the client in open state, and its path
// here: a break of the opens it held goes to another session signed in as
// their opener, or nowhere.
func (fs *Filesystem) Disconnect(ctx context.Context, c vfs.Call, cl vfs.ClientID, sess vfs.SessionRef) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	fs.reg.dropSession(cl, sess)
	return openErr(fs.clients.Disconnect(ctx, cl, sess))
}

// Destroy deletes the client in open state, then its callback paths here.
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
// client of its old one as a reboot does. It registers no callback path:
// the host's next call connects with one.
func (fs *Filesystem) HostRestarted(ctx context.Context, c vfs.Call, host []byte, state uint32) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	rec := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolNLM, Name: host, Boot: entity.NSMBoot(state)}}
	_, err = fs.clients.Connect(ctx, rec, nil)
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
	opens, err := fs.grants.Openers(ctx, who, g)
	if err != nil {
		return openErr(err)
	}
	if len(opens) > 0 && !slices.ContainsFunc(opens, func(o openstate.Open) bool { return o.Principal == who.Principal }) {
		return &vfs.StateError{Kind: vfs.StateNotYours}
	}
	return openErr(fs.grants.AckRecall(ctx, who, g, to))
}
