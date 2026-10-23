package openstate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

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

// ClientStatus is what a renewal reports beside the lease. Its revoked and
// reclaim flags arrive with the pieces that set them.
type ClientStatus struct{}

// Caller is the client a call acts for, as the adapter proved it, and the
// principal it runs as, mapped and never squashed.
type Caller struct {
	Client    entity.ClientID
	Principal entity.Principal
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
	// reboot: the old ID ends as the new one appears.
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
	// released and its lease here dropped. It deletes no record and raises
	// nothing: the lease timer's expiry does both, and the service then
	// calls Expire at each node holding the client's state.
	Expire(ctx context.Context, c entity.ClientID) error
	// Destroy deletes a client that holds no session and no state, refusing
	// ErrClientBusy otherwise; it releases nothing.
	Destroy(ctx context.Context, who Caller) error
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
	p := Pending{Boot: rec.Boot, Owner: rec.Owner, Adapter: rec.Adapter}
	known := false
	cur, found, err := t.byName(ctx, rec)
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

	src, key := source(from), keyOf(rec)
	t.cmu.Lock()
	defer t.cmu.Unlock()
	old, replaces := t.pending[key]
	if !replaces && len(t.pending) >= maxPending ||
		(!replaces || old.source != src) && t.sources[src] >= maxPerSource {
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
	t.cstep.Lock()
	ended, err := t.connect(ctx, &rec, p)
	t.cstep.Unlock()
	if err != nil {
		return 0, err
	}
	if ended != nil {
		sink.ClientEnded(ended.ID, shardsOf(*ended), RevokedLeaseExpired)
	}
	return rec.ID, nil
}

// connect makes rec current, under the client step, and returns the record a
// reboot replaced.
func (t *Table) connect(ctx context.Context, rec *entity.ClientRecord, p *Proposal) (*entity.ClientRecord, error) {
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
	switch {
	case err != nil:
		return nil, err
	case found && refuses(cur, rec.Owner):
		return nil, ErrClientInUse
	case known && (!found || cur.ID != id):
		return nil, ErrStaleClient // the proposed client ended since the proposal
	case found && cur.Boot == rec.Boot:
		if p != nil && cur.ID != id {
			return nil, ErrStaleClient // the name was connected under another ID since the proposal
		}
		if p != nil {
			if cur, err = t.putAdapter(ctx, cur, rec.Adapter); err != nil {
				return nil, err
			}
		}
		*rec = cur
	default:
		if id == 0 {
			if id, err = t.nextClientID(ctx); err != nil {
				return nil, err
			}
		}
		rec.ID = id
		if !found {
			err = t.d.Records.Clients().Create(ctx, *rec)
		} else if err = t.d.Records.Clients().Reboot(ctx, cur.ID, *rec); err == nil {
			ended = &cur
		}
		if err != nil {
			return nil, err
		}
	}

	// One critical section ends the old ID and makes the new one current, so
	// no renewal finds both.
	t.cmu.Lock()
	if ended != nil {
		delete(t.leases, ended.ID)
	}
	t.leases[rec.ID] = t.leaseEnd(rec.Protocol)
	t.cmu.Unlock()
	if ended != nil {
		t.releaseClient(ended.ID)
	}
	return ended, nil
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
		return t.adopt(ctx, c)
	}
	defer t.cmu.Unlock()
	switch now := t.d.Clock.Elapsed(); {
	case end == 0:
	case now >= end:
		return ClientStatus{}, ErrStaleClient
	default:
		t.leases[c] = now + leasePeriod
	}
	return ClientStatus{}, nil
}

// adopt serves a client this node holds no lease for, as after a restart: its
// record is the proof it is current, and its lease starts a full period from
// now. It reads the record and writes nothing, under the client step, so a
// reboot or an expiry committed meanwhile is never undone.
func (t *Table) adopt(ctx context.Context, c entity.ClientID) (ClientStatus, error) {
	t.cstep.Lock()
	defer t.cstep.Unlock()
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return ClientStatus{}, staleIfGone(err)
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if _, ok := t.leases[c]; !ok {
		t.leases[c] = t.leaseEnd(rec.Protocol)
	}
	return ClientStatus{}, nil
}

func (v clients) Expire(ctx context.Context, c entity.ClientID) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	t.cstep.Lock()
	defer t.cstep.Unlock()
	t.releaseClient(c)
	t.cmu.Lock()
	delete(t.leases, c)
	t.cmu.Unlock()
	return nil
}

// expireLapsed ends c if its lease has ended: the lease owner's expiry. Its
// state is released, then its record deleted, then its lease dropped, and
// ClientEnded raised once the client step is released. A renewal or a
// confirm that landed meanwhile keeps it.
func (t *Table) expireLapsed(ctx context.Context, sink Sink, c entity.ClientID) error {
	t.cstep.Lock()
	rec, ended, err := t.expireLocked(ctx, c)
	t.cstep.Unlock()
	if ended {
		sink.ClientEnded(c, shardsOf(rec), RevokedLeaseExpired)
	}
	return err
}

func (t *Table) expireLocked(ctx context.Context, c entity.ClientID) (entity.ClientRecord, bool, error) {
	t.cmu.Lock()
	end := t.leases[c]
	t.cmu.Unlock()
	if end == 0 || t.d.Clock.Elapsed() < end {
		return entity.ClientRecord{}, false, nil
	}
	rec, err := t.d.Records.Clients().Get(ctx, c)
	found := err == nil
	if err != nil && !merrs.IsNotFoundError(err) {
		return entity.ClientRecord{}, false, err
	}
	t.releaseClient(c)
	if found {
		if err := t.d.Records.Clients().Delete(ctx, c); err != nil {
			return entity.ClientRecord{}, false, err
		}
	}
	t.cmu.Lock()
	delete(t.leases, c)
	t.cmu.Unlock()
	return rec, found, nil
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
	t.cmu.Unlock()
	return nil
}

// sweep expires every client whose lease has ended and drops every proposal
// a lease period old.
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
	t.cmu.Unlock()
	for _, c := range lapsed {
		// A failed expiry is tried again at the next sweep: the lease stays
		// lapsed, so the client is already refused.
		_ = t.expireLapsed(ctx, sink, c)
	}
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
	for _, h := range t.refs {
		if h.client == c {
			return true
		}
	}
	return false
}

// releaseClient releases everything client c holds, file by file, each under
// that file's step.
func (t *Table) releaseClient(c entity.ClientID) {
	for _, h := range t.heldBy(c) {
		_ = t.step(h.file, func(e *entry) error { t.release(e, h.ref); return nil })
	}
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
