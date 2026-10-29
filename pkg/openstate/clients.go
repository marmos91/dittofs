package openstate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

const (
	// leasePeriod is the client lease, and every period derived from it.
	//
	// decision: 90 s, long enough that a client is not expired across a brief
	// network or suspend gap, short enough that a failover's grace stays near a
	// minute and a half. It is not measured. Change the one value, not a
	// setting, if failover time or clients expired by survivable gaps say so.
	leasePeriod = 90 * time.Second

	// maxName is the longest name a client may give itself, the protocols'
	// own opaque limit.
	maxName = 1024

	// ponytail: a node holds at most maxPending proposals, and maxPerSource
	// from one source; past either a proposal is answered ErrDelay. Raise
	// them if mounts are refused below them.
	maxPending   = 4096
	maxPerSource = 16
)

// Client is a client's record and what the table holds for it in memory.
type Client struct {
	entity.ClientRecord
	Expires time.Duration // the Deps.Clock.Elapsed reading its lease ends at, at this node; zero: no lease
	Pending *Pending      // a proposed instance awaiting its confirm
}

// Pending is a proposed client instance awaiting its confirm. It is held only
// by the node that answered, and never written.
type Pending struct {
	ID      entity.ClientID  // the ID a confirm makes current: this client's, or a new one for a new Boot
	Boot    [8]byte          // the instance proposed
	Owner   entity.Principal // who proposed it; only it may confirm
	Confirm [8]byte          // what the confirm must present: 64 bits from Deps.Rand
	Expires time.Duration    // Deps.Clock.Elapsed one lease period after the proposal; then it is gone
	Adapter []byte           // becomes the record's Adapter at the confirm; never read here
}

// Proposal is what Propose answers and Connect confirms.
type Proposal struct {
	Client  entity.ClientID // Pending.ID
	Confirm [8]byte         // Pending.Confirm
}

// ClientStatus is what a renewal reports beside the lease.
type ClientStatus struct {
	AdminRevoked      bool       // a reference an administrator revoked is in the client's revoked set
	RecallableRevoked bool       // a grant revoked at its recall deadline is
	ReclaimNeeded     bool       // a shard of its state is in a grace instance it has not finished
	Reclaim           []GraceRef // those instances, which the adapter keeps per session for ReclaimComplete
}

// Caller is the client a call acts for, as the adapter proved it, the
// principal it runs as, mapped and never squashed, and the request's
// protocol, which a rule that differs by protocol reads. The protocol is the
// caller's to state on every call, never looked up from a client record:
// NFSv3 has no client.
type Caller struct {
	Client    entity.ClientID
	Principal entity.Principal
	Protocol  entity.Protocol // zero: NFSv3, which holds no client
}

// Clients is the view of client records and leases; it runs at the client's
// lease owner, which on one node is this one.
type Clients interface {
	// Propose answers a protocol's first client call: it changes no state,
	// and holds the proposal at this node until Connect confirms it, a later
	// proposal for the name replaces it, or a lease period passes. A name a
	// record holds under another owner is refused ErrClientInUse, and a
	// proposal past this node's or its source's cap ErrDelay.
	Propose(ctx context.Context, c Client, from netip.Addr) (Proposal, error)
	// Connect makes a client current: a confirm of p, or with a nil p a
	// client that confirms in one step. A new Boot under a known name is a
	// reboot: the old ID ends as the new one appears, and a known name whose
	// lease has lapsed ends its old ID first. The waiters the ended ID's
	// release frees are served under each file's admission lock, which the
	// table takes itself: the service must not hold one when calling.
	Connect(ctx context.Context, c Client, p *Proposal) (entity.ClientID, error)
	// Record is the record as written, Adapter included; it changes nothing.
	Record(ctx context.Context, c entity.ClientID) (Client, error)
	// AddSession records node's session and writes next as Adapter if it
	// still equals prev; otherwise it writes nothing and returns
	// ErrRecordChanged with the current record.
	AddSession(ctx context.Context, c entity.ClientID, node entity.NodeID, prev, next []byte) (Client, error)
	// Renew renews a client's lease for another period, writing nothing.
	Renew(ctx context.Context, c entity.ClientID) (ClientStatus, error)
	// Expire ends the client's state at this node: its state here is
	// released, closing its opens, and its lease here dropped. It deletes no
	// record and raises no ClientEnded: the lease timer's expiry does both,
	// and the service then calls Expire at each node holding the client's
	// state. Each file a close left orphaned or to unlink is raised Closed.
	// The waiters the releases free are served under each file's admission
	// lock, which the table takes itself: the service must not hold one when
	// calling.
	Expire(ctx context.Context, c entity.ClientID) error
	// Destroy deletes a client that holds no session and no state, refusing
	// ErrClientBusy otherwise; it releases nothing. A revoked reference is
	// not state the client holds: its revoked set goes with the record.
	Destroy(ctx context.Context, who Caller) error
	// Revoke is an administrator's revoke of c at this node: every open,
	// lock and grant it holds here is released, as Expire releases them, but
	// each reference stays in its revoked set, answered ErrRevoked until
	// freed, its record is kept and its Shards emptied. A client with no
	// Owner has no revoked set to be told by: its revoke deletes its record.
	// Either is raised Revoked. The waiters the releases free are served
	// under each file's admission lock, which the table takes itself: the
	// service must not hold one when calling.
	Revoke(ctx context.Context, c entity.ClientID) error
	// Rebind records a new back channel at node: it sets Reach.Callback and
	// clears Reach.Down, so the client is offered grants again. Only its
	// Owner may, where it protects its state.
	Rebind(ctx context.Context, who Caller, node entity.NodeID) error
	// ChannelDown records that c's back channel is lost: it sets
	// Reach.Down, and c is offered no grant until it rebinds.
	ChannelDown(ctx context.Context, c entity.ClientID) error
	// Disconnect is the loss of session s of client c's last connection:
	// each volatile open s made or last reconnected closes, as does a
	// durable one no longer under handle caching, and each other durable
	// or persistent one is kept, with every lock, deny mode and lease it
	// holds, until its Timeout passes without a reconnect; the timer then
	// closes it. Its waiters go, answered not granted. Another session's
	// opens are left alone, and no open is attached to s again. A logoff is
	// not a disconnect: it closes every open of its session, durable ones
	// included, which the service does by Close. The waiters the closes free
	// are served under each file's admission lock, which the table takes
	// itself: the service must not hold one when calling.
	Disconnect(ctx context.Context, c entity.ClientID, s entity.SessionRef) error
}

type clients struct{ t *Table }

type nameKey struct {
	protocol entity.Protocol
	name     string
}

// pending is a proposal and the source it is charged to.
type pending struct {
	Pending
	source netip.Prefix
	known  bool // ID was an existing record's when proposed
}

func keyOf(rec entity.ClientRecord) nameKey { return nameKey{rec.Protocol, string(rec.Name)} }

// admit checks a client's own fields and returns the record they make.
//
// decision: the service records an owner that maps to the anonymous principal
// as zero, since only it knows the installation's anonymous principal; a zero
// owner protects nothing, so Protect is cleared with it. Record it if a
// protocol is served whose anonymous callers the table can tell apart.
func admit(c Client) (entity.ClientRecord, error) {
	rec := c.ClientRecord
	if len(rec.Name) == 0 || len(rec.Name) > maxName || len(rec.Adapter) > kvstore.MaxAdapter {
		return entity.ClientRecord{}, ErrInvalid
	}
	if rec.Owner.IsZero() {
		rec.Protect = false
	}
	return rec, nil
}

// refuses reports whether a record with an owner refuses principal p: one
// without, an SMB machine's or an NLM host's, is found by its name alone.
func refuses(rec entity.ClientRecord, p entity.Principal) bool {
	return !rec.Owner.IsZero() && rec.Owner != p
}

// source is the source a proposal is charged to: an IPv6 /64 is one, since
// one host holds a whole prefix.
func source(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	bits := 32
	if a.Is6() {
		bits = 64
	}
	p, _ := a.Prefix(bits) // an invalid address is one source of its own
	return p
}

func (v clients) Propose(ctx context.Context, c Client, from netip.Addr) (Proposal, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return Proposal{}, err
	}
	rec, err := admit(c)
	if err != nil {
		return Proposal{}, err
	}
	src, key := source(from), keyOf(rec)
	t.cmu.Lock()
	full := t.capped(key, src)
	t.cmu.Unlock()
	if full {
		return Proposal{}, &DelayError{DelayResources}
	}
	p := Pending{Boot: rec.Boot, Owner: rec.Owner, Adapter: rec.Adapter}
	known := false
	cur, found, err := t.byName(ctx, rec)
	found = found && !t.lapsed(cur.ID) // its confirm expires it first
	switch {
	case err != nil:
		return Proposal{}, err
	case found && refuses(cur, rec.Owner):
		return Proposal{}, ErrClientInUse
	case found && cur.Boot == rec.Boot:
		p.ID, known = cur.ID, true
	default:
		if p.ID, err = t.nextClientID(ctx); err != nil {
			return Proposal{}, err
		}
	}
	if _, err := io.ReadFull(t.d.Rand, p.Confirm[:]); err != nil {
		return Proposal{}, fmt.Errorf("openstate: drawing a confirm: %w", err)
	}

	t.cmu.Lock()
	defer t.cmu.Unlock()
	old, replaces := t.pending[key]
	if t.capped(key, src) {
		return Proposal{}, &DelayError{DelayResources}
	}
	if replaces {
		t.uncharge(old.source)
	}
	t.sources[src]++
	p.Expires = t.d.Clock.Elapsed() + leasePeriod
	t.pending[key] = pending{p, src, known}
	return Proposal{p.ID, p.Confirm}, nil
}

func (v clients) Connect(ctx context.Context, c Client, p *Proposal) (entity.ClientID, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return 0, err
	}
	rec, err := admit(c)
	if err != nil {
		return 0, err
	}
	out := outbox{timer: true}
	t.cstep.Lock()
	ended, err := t.connect(ctx, &rec, p, &out)
	t.cstep.Unlock()
	if ended != nil {
		sink.ClientEnded(ended.ID, shardsOf(*ended), RevokedLeaseExpired)
	}
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	if err != nil {
		return 0, err
	}
	return rec.ID, nil
}

// connect makes rec current, under the client step, and returns the record a
// reboot replaced or a lapsed lease ended, the latter on an error too; what
// releasing its state raised goes to out.
func (t *Table) connect(ctx context.Context, rec *entity.ClientRecord, p *Proposal, out *outbox) (*entity.ClientRecord, error) {
	var id entity.ClientID
	known := false
	if p != nil {
		key := keyOf(*rec)
		t.cmu.Lock()
		pd, ok := t.pending[key]
		ok = ok && pd.ID == p.Client && pd.Confirm == p.Confirm && t.d.Clock.Elapsed() < pd.Expires
		if ok && pd.Owner != rec.Owner {
			t.cmu.Unlock()
			return nil, ErrClientInUse // the proposal stays for its own principal
		}
		if ok {
			delete(t.pending, key)
			t.uncharge(pd.source)
		}
		t.cmu.Unlock()
		if !ok {
			return nil, ErrStaleClient
		}
		id, known, rec.Boot, rec.Adapter = pd.ID, pd.known, pd.Boot, pd.Adapter
	}

	cur, found, err := t.byName(ctx, *rec)
	var ended *entity.ClientRecord
	if found && t.lapsed(cur.ID) {
		// A lapsed lease is expired here as the timer would, so a confirm
		// before the sweep never revives it: the name then has no record.
		gone, expired, xerr := t.expireLocked(ctx, cur.ID, out)
		if err, found = xerr, false; expired {
			ended = &gone
		}
	}
	switch {
	case err != nil:
		return nil, err
	case found && refuses(cur, rec.Owner):
		return nil, ErrClientInUse
	case known && (!found || cur.ID != id):
		return ended, ErrStaleClient // the proposed client ended since the proposal
	case found && cur.Boot == rec.Boot:
		if p != nil && cur.ID != id {
			return nil, ErrStaleClient // the name was connected under another ID since the proposal
		}
		if p != nil {
			if cur, err = t.putAdapter(ctx, cur, rec.Adapter); err != nil {
				return nil, err
			}
			if cur.Recovery == entity.RecoverRefusal {
				if cur.Shards, err = t.markDone(ctx, cur); err != nil {
					return nil, err
				}
			}
		}
		*rec = cur
	default:
		if id == 0 {
			if id, err = t.nextClientID(ctx); err != nil {
				return ended, err
			}
		}
		rec.ID = id
		if !found {
			err = t.d.Records.Clients().Create(ctx, *rec)
		} else if err = t.d.Records.Clients().Reboot(ctx, cur.ID, *rec); err == nil {
			ended = &cur
		}
		if err != nil {
			return ended, err
		}
	}

	// One critical section ends the old ID and makes the new one current, so
	// no renewal finds both.
	t.cmu.Lock()
	if ended != nil {
		delete(t.leases, ended.ID)
		delete(t.known, ended.ID)
	}
	t.leases[rec.ID] = t.leaseEnd(rec.Protocol)
	t.know(*rec)
	t.cmu.Unlock()
	if ended != nil {
		t.unreclaim(ended.ID)
		t.releaseClient(ctx, ended.ID, out)
	}
	return ended, nil
}

// markDone sets rec's Done to each served shard's grace instance it is
// below, as a client that recovers by connecting again does when it confirms
// again, and returns the marks as written: it then reclaims.
func (t *Table) markDone(ctx context.Context, rec entity.ClientRecord) ([]entity.ShardMark, error) {
	marks := slices.Clone(rec.Shards)
	for i, m := range marks {
		t.cmu.Lock()
		sh := t.shards[m.Shard]
		t.cmu.Unlock()
		if sh == nil || m.Done >= sh.instance {
			continue
		}
		marks[i].Done = sh.instance
		if err := t.d.Records.Clients().MarkShard(ctx, rec.ID, marks[i]); err != nil {
			return nil, staleIfGone(err)
		}
	}
	return marks, nil
}

// putAdapter writes a confirmed proposal's Adapter over the record's.
//
// decision: the confirm wins: it overwrites bytes a racing AddSession
// committed after the proposal was composed, as a confirm racing an
// AddSession makes the latter retry. The AddSession that lost has already
// been answered, so its bytes are gone. On ErrRecordChanged it re-reads and
// writes again, so each retry follows a racing writer's commit. Write once
// over the record read in this step, and answer ErrRecordChanged, if an
// adapter is seen losing a session reply this way.
func (t *Table) putAdapter(ctx context.Context, cur entity.ClientRecord, adapter []byte) (entity.ClientRecord, error) {
	for {
		next := cur
		next.Adapter = adapter
		got, err := t.d.Records.Clients().Put(ctx, cur, next)
		if !errors.Is(err, ErrRecordChanged) {
			return got, err
		}
		cur = got
	}
}

// leaseEnd is the Elapsed reading a lease armed now ends at, or zero for a
// protocol whose clients hold none.
//
// decision: only an NFSv4 client holds a lease. An SMB client's opens are
// kept by their durable Timeout and an NLM host's locks until its restart
// notice, so neither is expired for silence. Give a protocol a lease if one
// is served whose clients renew.
func (t *Table) leaseEnd(p entity.Protocol) time.Duration {
	if p != entity.ProtocolNFSv4 {
		return 0
	}
	return t.d.Clock.Elapsed() + leasePeriod
}

func (v clients) Record(ctx context.Context, c entity.ClientID) (Client, error) {
	t := v.t
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return Client{}, staleIfGone(err)
	}
	cl := Client{ClientRecord: rec}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	cl.Expires = t.leases[c]
	if pd, ok := t.pending[keyOf(rec)]; ok {
		cl.Pending = &pd.Pending
	}
	return cl, nil
}

func (v clients) AddSession(ctx context.Context, c entity.ClientID, node entity.NodeID, prev, next []byte) (Client, error) {
	if _, err := v.t.running(); err != nil {
		return Client{}, err
	}
	if len(next) > kvstore.MaxAdapter {
		return Client{}, ErrInvalid
	}
	rec, err := v.t.d.Records.Clients().AddSession(ctx, c, node, prev, next)
	if err != nil && !errors.Is(err, ErrRecordChanged) {
		return Client{}, staleIfGone(err)
	}
	return Client{ClientRecord: rec}, err
}

func (v clients) Renew(ctx context.Context, c entity.ClientID) (ClientStatus, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return ClientStatus{}, err
	}
	t.cmu.Lock()
	end, ok := t.leases[c]
	if !ok {
		t.cmu.Unlock()
		if err := t.adopt(ctx, c); err != nil {
			return ClientStatus{}, err
		}
		t.cmu.Lock()
		stale := t.mustConnectLocked(c)
		t.cmu.Unlock()
		if stale {
			return ClientStatus{}, ErrStaleClient
		}
		return t.status(c), nil
	}
	switch now := t.d.Clock.Elapsed(); {
	case end != 0 && now >= end, t.mustConnectLocked(c):
		t.cmu.Unlock()
		return ClientStatus{}, ErrStaleClient
	case end != 0:
		t.leases[c] = now + leasePeriod
	}
	delete(t.adopted, c) // it came back: its installed state is its own again
	t.cmu.Unlock()
	return t.status(c), nil
}

// status is what c's revoked set and the grace instances it owes report.
//
// ponytail: a scan of every revoked reference, O(revoked), per renewal; the
// set is empty unless something was revoked and not yet freed. Count each
// client's entries by cause if a renewal shows the scan in a profile.
func (t *Table) status(c entity.ClientID) ClientStatus {
	var s ClientStatus
	t.cmu.Lock()
	s.Reclaim = t.owedLocked(c)
	t.cmu.Unlock()
	s.ReclaimNeeded = len(s.Reclaim) > 0
	t.idx.Lock()
	defer t.idx.Unlock()
	for _, r := range t.revoked {
		if r.client == c {
			s.AdminRevoked = s.AdminRevoked || r.cause == RevokedByAdmin
			s.RecallableRevoked = s.RecallableRevoked || r.cause == RevokedGrant
		}
	}
	return s
}

// adopt serves a client this node holds no lease for, as after a restart: its
// record is the proof it is current, and its lease starts a full period from
// now. It reads the record and writes nothing, under the client step, so a
// reboot or an expiry committed meanwhile is never undone.
func (t *Table) adopt(ctx context.Context, c entity.ClientID) error {
	t.cstep.Lock()
	defer t.cstep.Unlock()
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return staleIfGone(err)
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if _, ok := t.leases[c]; !ok {
		t.leases[c] = t.leaseEnd(rec.Protocol)
		t.know(rec)
	}
	return nil
}

// know records what the table reads of rec while its client is current; the
// caller holds cmu.
func (t *Table) know(rec entity.ClientRecord) {
	delete(t.adopted, rec.ID) // it came back: its state is its own again
	k := known{recovery: rec.Recovery, down: rec.Reach.Down, shards: rec.Shards}
	if rec.Protect {
		k.owner = rec.Owner
	}
	t.known[rec.ID] = k
}

func (v clients) Expire(ctx context.Context, c entity.ClientID) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	out := outbox{timer: true}
	t.cstep.Lock()
	t.releaseClient(ctx, c, &out)
	t.forget(ctx, c, &out)
	t.cstep.Unlock()
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	return nil
}

// expireLapsed ends c if its lease has ended: the lease owner's expiry. Its
// state is released, then its record deleted, then its lease dropped, and
// ClientEnded raised once the client step is released. A renewal or a
// confirm that landed meanwhile keeps it.
//
// It runs on the timer, so the waiters the release frees are not served
// here: their files are returned, to be served under each file's admission
// lock.
func (t *Table) expireLapsed(ctx context.Context, sink Sink, c entity.ClientID) ([]entity.FileID, error) {
	out := outbox{timer: true}
	t.cstep.Lock()
	rec, ended, err := t.expireLocked(ctx, c, &out)
	t.cstep.Unlock()
	if ended {
		sink.ClientEnded(c, shardsOf(rec), RevokedLeaseExpired)
	}
	out.raise(t, sink)
	return out.serve, err
}

func (t *Table) expireLocked(ctx context.Context, c entity.ClientID, out *outbox) (entity.ClientRecord, bool, error) {
	if !t.lapsed(c) {
		return entity.ClientRecord{}, false, nil
	}
	rec, err := t.d.Records.Clients().Get(ctx, c)
	found := err == nil
	if err != nil && !merrs.IsNotFoundError(err) {
		return entity.ClientRecord{}, false, err
	}
	t.releaseClient(ctx, c, out)
	if found {
		if err := t.d.Records.Clients().Delete(ctx, c); err != nil {
			return entity.ClientRecord{}, false, err
		}
	}
	t.forget(ctx, c, out)
	return rec, found, nil
}

// forget drops c's lease, its fence and its place in the known set here, then
// releases any state granted between the last release and c's leaving the
// set, none being granted after it, and its revoked set: its released
// references are bad alike.
func (t *Table) forget(ctx context.Context, c entity.ClientID, out *outbox) {
	t.cmu.Lock()
	delete(t.leases, c)
	delete(t.known, c)
	delete(t.fenced, c)
	t.cmu.Unlock()
	t.unreclaim(c)
	t.releaseClient(ctx, c, out)
	t.dropRevoked(c)
}

// dropRevoked drops c's revoked set.
//
// ponytail: it scans every revoked reference of every client. Index the
// revoked set by client when revoked sets are seen large at a client's end.
func (t *Table) dropRevoked(c entity.ClientID) {
	t.idx.Lock()
	defer t.idx.Unlock()
	for k, r := range t.revoked {
		if r.client == c {
			delete(t.revoked, k)
		}
	}
}

func (v clients) Revoke(ctx context.Context, c entity.ClientID) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	out := outbox{timer: true}
	t.cstep.Lock()
	err = t.revoke(ctx, c, &out)
	t.cstep.Unlock()
	if err == nil {
		sink.Revoked(Revocation{Client: c, Reason: RevokedByAdmin})
	}
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	return err
}

// revoke is Revoke, under the client step. Each reference enters the revoked
// set before its state is released, so none is ever answered bad meanwhile.
//
// decision: a client with no Owner, an SMB machine or an NLM host, is revoked
// as an expiry ends it: its record is deleted, since without an owner it has
// no revoked set to be told by. Keep its record if such a protocol is found to
// report revoked state.
func (t *Table) revoke(ctx context.Context, c entity.ClientID, out *outbox) error {
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return staleIfGone(err)
	}
	if rec.Owner.IsZero() {
		t.releaseClient(ctx, c, out)
		if err := t.d.Records.Clients().Delete(ctx, c); err != nil {
			return err
		}
		t.forget(ctx, c, out)
		return nil
	}
	// ponytail: it scans every reference the table holds, of every client.
	// Index references by client when a revoke is seen slow on a busy primary.
	t.idx.Lock()
	for k, h := range t.refs {
		if h.client == c {
			t.revoked[k] = revokedRef{c, h.typ, RevokedByAdmin}
		}
	}
	t.idx.Unlock()
	t.releaseClient(ctx, c, out)
	if err := t.d.Records.Clients().DropShards(ctx, c); err != nil {
		return err
	}
	t.unreclaim(c)
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if k, ok := t.known[c]; ok {
		k.shards = nil
		t.known[c] = k
	}
	return nil
}

func (v clients) ChannelDown(ctx context.Context, c entity.ClientID) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	t.cstep.Lock()
	defer t.cstep.Unlock()
	return t.setReach(ctx, c, func(r *entity.Reach) { r.Down = true })
}

// setReach applies set to c's record's Reach, and to what the table reads of
// it, under the client step. A racing AddSession's bytes are kept: the write
// is retried over the record it committed.
func (t *Table) setReach(ctx context.Context, c entity.ClientID, set func(*entity.Reach)) error {
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return staleIfGone(err)
	}
	for {
		next := rec
		set(&next.Reach)
		got, err := t.d.Records.Clients().Put(ctx, rec, next)
		if errors.Is(err, ErrRecordChanged) {
			rec = got
			continue
		}
		if err != nil {
			return staleIfGone(err)
		}
		t.cmu.Lock()
		defer t.cmu.Unlock()
		if k, ok := t.known[c]; ok {
			k.down = next.Reach.Down
			t.known[c] = k
		}
		return nil
	}
}

func (v clients) Rebind(ctx context.Context, who Caller, node entity.NodeID) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	t.cstep.Lock()
	defer t.cstep.Unlock()
	rec, err := t.d.Records.Clients().Get(ctx, who.Client)
	switch {
	case err != nil:
		return staleIfGone(err)
	case rec.Protect && who.Principal != rec.Owner:
		return ErrNotYours
	}
	return t.setReach(ctx, who.Client, func(r *entity.Reach) { r.Callback, r.Down = node, false })
}

func (v clients) Destroy(ctx context.Context, who Caller) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	t.cstep.Lock()
	defer t.cstep.Unlock()
	rec, err := t.d.Records.Clients().Get(ctx, who.Client)
	switch {
	case err != nil:
		return staleIfGone(err)
	case rec.Protect && who.Principal != rec.Owner:
		return ErrNotYours
	case len(rec.Reach.Sessions) > 0 || t.holdsAny(who.Client):
		return ErrClientBusy
	}
	if err := t.d.Records.Clients().Delete(ctx, who.Client); err != nil {
		return err
	}
	t.cmu.Lock()
	delete(t.leases, who.Client)
	delete(t.known, who.Client)
	t.cmu.Unlock()
	t.unreclaim(who.Client)
	t.dropRevoked(who.Client)
	return nil
}

// sweep expires every client whose lease has ended, drops every proposal a
// lease period old and every fence ended, and serves the files holding a poll
// waiter, those holding a recall handed to the Sink and those whose waiters an
// expiry freed.
func (t *Table) sweep(ctx context.Context) {
	sink, err := t.running()
	if err != nil {
		return
	}
	var lapsed []entity.ClientID
	t.cmu.Lock()
	now := t.d.Clock.Elapsed()
	for c, end := range t.leases {
		if end != 0 && now >= end {
			lapsed = append(lapsed, c)
		}
	}
	for k, pd := range t.pending {
		if now >= pd.Expires {
			delete(t.pending, k)
			t.uncharge(pd.source)
		}
	}
	maps.DeleteFunc(t.fenced, func(_ entity.ClientID, until time.Duration) bool { return now >= until })
	t.cmu.Unlock()
	t.idx.Lock()
	due := maps.Clone(t.polls)
	maps.Copy(due, t.parked)
	maps.Copy(due, t.recalling)
	serve := slices.Collect(maps.Keys(due))
	maps.DeleteFunc(t.recalled, func(_ entity.FileID, until time.Duration) bool { return now >= until })
	maps.DeleteFunc(t.ended, func(_ sessionKey, at time.Duration) bool { return now >= at+leasePeriod })
	t.idx.Unlock()
	for _, c := range lapsed {
		// A failed expiry is tried again at the next sweep: the lease stays
		// lapsed, so the client is already refused.
		files, err := t.expireLapsed(ctx, sink, c)
		if err != nil {
			logger.Error("openstate: lease expiry failed, retried at the next sweep", "client", c, "error", err)
		}
		for _, f := range files {
			if !slices.Contains(serve, f) {
				serve = append(serve, f)
			}
		}
	}
	// On a goroutine of its own, so the sweep never waits on an admission
	// lock.
	t.timer.Go(func() {
		t.serveFiles(ctx, sink, serve)
		t.closeUnreclaimed(ctx, sink)
	})
}

// held is one piece of state a client holds.
type held struct {
	ref  StateRef
	file entity.FileID
}

// heldBy snapshots the state client c holds.
//
// ponytail: a scan of every reference, O(state), per expiry, reboot and
// destroy, with no ended mark: state minted for c after the snapshot
// survives its release, and a request during the walk sees part of it.
// Replace it with the per-client index and its ended mark before anything
// mints for a client concurrently with its expiry.
func (t *Table) heldBy(c entity.ClientID) []held {
	t.idx.Lock()
	defer t.idx.Unlock()
	var hs []held
	for k, h := range t.refs {
		if h.client == c {
			hs = append(hs, held{StateRef{Type: h.typ, Shard: k.shard, Token: k.token}, h.file})
		}
	}
	return hs
}

// holdsAny reports whether client c holds any state, by the same scan as
// heldBy.
func (t *Table) holdsAny(c entity.ClientID) bool {
	t.idx.Lock()
	defer t.idx.Unlock()
	return t.holdsAnyLocked(c)
}

// holdsAnyLocked is holdsAny, with idx held. A revoked reference is not
// state the client holds.
func (t *Table) holdsAnyLocked(c entity.ClientID) bool {
	for _, h := range t.refs {
		if h.client == c {
			return true
		}
	}
	return false
}

// releaseClient releases everything client c holds, file by file, each under
// that file's step, with its owners' sequences, and adds to out what its
// closes left the service to run and the waiters its releases granted.
//
// ponytail: the owners' sequences are found by a scan of every slot,
// O(owners), per release. Key the book by client when a profile shows it.
//
// decision: an open whose durable record's delete fails is released here
// unless it is persistent; an expiry that kept it would hold its deny modes
// for a client that is gone. Its record stays a holder of its file until the
// next start, which closes it once that start's grace ends. A persistent open
// is kept instead, parked as timed out, holding its deny modes and locks until
// a sweep deletes its record: released, it would be installed at the next
// start and reconnect with them. Keep the other opens too if a store outage
// is shown to strand files.
func (t *Table) releaseClient(ctx context.Context, c entity.ClientID, out *outbox) {
	for _, h := range t.heldBy(c) {
		_ = t.step(h.file, func(e *entry) error {
			if ls := e.lstates[h.ref.Token]; h.ref.Type == StateLock && ls != nil {
				t.dropLockState(e, ls)
				t.serve(ctx, e, out)
				return nil
			}
			if g := e.grants[h.ref.Token]; h.ref.Type == StateGrant && g != nil {
				t.dropGrant(e, g)
				t.serve(ctx, e, out)
				return nil
			}
			o := e.opens[h.ref.Token]
			if h.ref.Type != StateOpen || o == nil {
				t.release(e, h.ref)
				return nil
			}
			t.closeByTable(ctx, e, o, out)
			return nil
		})
	}
	t.smu.Lock()
	defer t.smu.Unlock()
	for o := range t.seqs {
		if o.Client == c {
			delete(t.seqs, o)
		}
	}
}

// lapsed reports whether c's lease has ended.
func (t *Table) lapsed(c entity.ClientID) bool {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	end := t.leases[c]
	return end != 0 && t.d.Clock.Elapsed() >= end
}

// capped reports whether a proposal for key from src is past a cap, a
// replacement from the same source being charged nothing; the caller holds
// cmu.
func (t *Table) capped(key nameKey, src netip.Prefix) bool {
	old, replaces := t.pending[key]
	return !replaces && len(t.pending) >= maxPending ||
		(!replaces || old.source != src) && t.sources[src] >= maxPerSource
}

// uncharge drops one proposal's charge to src; the caller holds cmu.
func (t *Table) uncharge(src netip.Prefix) {
	if t.sources[src]--; t.sources[src] <= 0 {
		delete(t.sources, src)
	}
}

func (t *Table) nextClientID(ctx context.Context) (entity.ClientID, error) {
	n, err := t.d.Records.NextID(ctx, kvstore.IDClient)
	return entity.ClientID(n), err
}

// byName finds the record rec's protocol and name, if there is one.
func (t *Table) byName(ctx context.Context, rec entity.ClientRecord) (entity.ClientRecord, bool, error) {
	cur, err := t.d.Records.Clients().ByName(ctx, rec.Protocol, rec.Name)
	if merrs.IsNotFoundError(err) {
		return entity.ClientRecord{}, false, nil
	}
	return cur, err == nil, err
}

// staleIfGone answers a client whose record is gone ErrStaleClient: it
// expired, was replaced by a reboot, or never confirmed.
func staleIfGone(err error) error {
	if merrs.IsNotFoundError(err) {
		return ErrStaleClient
	}
	return err
}

func shardsOf(rec entity.ClientRecord) []ShardID {
	shards := make([]ShardID, len(rec.Shards))
	for i, m := range rec.Shards {
		shards[i] = m.Shard
	}
	return shards
}
