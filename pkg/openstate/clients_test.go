package openstate

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

var addr = netip.MustParseAddr("192.0.2.1")

func nfs(name string, boot byte, owner entity.Principal) Client {
	return Client{ClientRecord: entity.ClientRecord{
		Protocol: entity.ProtocolNFSv4, Name: []byte(name), Boot: [8]byte{boot}, Owner: owner, Protect: !owner.IsZero(),
	}}
}

// connect proposes c and confirms the proposal, failing the test on an error.
func (f *fixture) connect(t *testing.T, c Client) entity.ClientID {
	t.Helper()
	p, err := f.Propose(ctx, c, addr)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	id, err := f.Connect(ctx, c, &p)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if id != p.Client {
		t.Fatalf("Connect answered %d, the proposal %d", id, p.Client)
	}
	return id
}

// A client is found by (protocol, name): the same Boot keeps its ID, after a
// restart too, with its Boot, Owner and Protect kept durably; a confirm
// writes the proposal's Adapter; another principal is refused; a new Boot is
// a reboot to a new ID, which ends the old one and releases its state.
func TestClientFoundByName(t *testing.T) {
	f := newFixture(t)
	alice, bob := entity.NewUser(), entity.NewUser()
	c := nfs("host-a", 1, alice)
	id := f.connect(t, c)

	c.Adapter = []byte{1, 2}
	if again := f.connect(t, c); again != id {
		t.Errorf("the same name, Boot and owner: ID %d, want %d", again, id)
	}
	if _, err := f.Propose(ctx, nfs("host-a", 1, bob), addr); !errors.Is(err, ErrClientInUse) {
		t.Errorf("a proposal from another principal: %v, want ErrClientInUse", err)
	}
	if _, err := f.Connect(ctx, nfs("host-a", 1, bob), nil); !errors.Is(err, ErrClientInUse) {
		t.Errorf("a one-step connect from another principal: %v, want ErrClientInUse", err)
	}
	if rec, err := f.Record(ctx, id); err != nil || rec.Pending != nil {
		t.Errorf("after the refusals: pending %+v, %v; want none", rec.Pending, err)
	}

	g := reopen(t, f.fs, f.clock)
	rec, err := g.Record(ctx, id)
	if err != nil || rec.Boot != c.Boot || rec.Owner != alice || !rec.Protect || !bytes.Equal(rec.Adapter, c.Adapter) {
		t.Fatalf("after a restart: %+v, %v; want Boot, Owner, Protect and the re-confirm's Adapter kept", rec.ClientRecord, err)
	}
	if again := g.connect(t, c); again != id {
		t.Errorf("after a restart: ID %d, want %d", again, id)
	}

	f2 := entity.NewFileID()
	ref := mintIn(t, g.Table, f2, id, 0)
	rebooted := g.connect(t, nfs("host-a", 2, alice))
	if rebooted == id {
		t.Fatalf("a new Boot kept ID %d", id)
	}
	if _, err := g.Record(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("the old ID's record after the reboot: %v, want ErrStaleClient", err)
	}
	if _, err := g.Renew(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("renewing the old ID after the reboot: %v, want ErrStaleClient", err)
	}
	if _, err := g.resolve(id, f2, ref); !errors.Is(err, ErrBadState) {
		t.Errorf("the old ID's state after the reboot: %v, want released", err)
	}
	if n := len(g.sink.endedOf(id)); n != 1 {
		t.Errorf("the reboot raised ClientEnded for the old ID %d times, want once", n)
	}
}

// A record without an owner is found by its name alone, whoever names it,
// and a zero owner protects nothing.
func TestOwnerlessRecordIsFoundByName(t *testing.T) {
	f := newFixture(t)
	anon := nfs("host-n", 1, entity.Principal{})
	anon.Protect = true
	id := f.connect(t, anon)
	if rec, err := f.Record(ctx, id); err != nil || rec.Protect {
		t.Fatalf("an ownerless record: Protect %v, %v; want false", rec.Protect, err)
	}
	if again := f.connect(t, nfs("host-n", 1, entity.NewUser())); again != id {
		t.Errorf("the ownerless name from a principal: ID %d, want %d", again, id)
	}
}

// A proposal changes no state until its confirm: a new Boot writes nothing,
// keeps the old ID current with its state, and is held at this node; only its
// own principal, naming its ID and confirm within a lease period, confirms it.
// Another principal's confirm is ErrClientInUse and leaves the proposal.
func TestProposalChangesNothingUntilConfirmed(t *testing.T) {
	f := newFixture(t)
	alice := entity.NewUser()
	id := f.connect(t, nfs("host-a", 1, alice))
	file := entity.NewFileID()
	ref := mintIn(t, f.Table, file, id, 0)

	reboot := nfs("host-a", 2, alice)
	f.fs.Counts.Take()
	p, err := f.Propose(ctx, reboot, addr)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.fs.Counts.Take(); n.Sets+n.Deletes != 0 {
		t.Errorf("a proposal wrote %d keys, want none", n.Sets+n.Deletes)
	}
	rec, err := f.Record(ctx, id)
	if err != nil || rec.Boot != [8]byte{1} || rec.Pending == nil || rec.Pending.Boot != reboot.Boot {
		t.Fatalf("after the proposal: Boot %v, pending %+v, %v; want the old Boot and the new one pending", rec.Boot, rec.Pending, err)
	}
	if _, err := f.Renew(ctx, id); err != nil {
		t.Errorf("the old ID after a proposal: %v", err)
	}
	if _, err := f.resolve(id, file, ref); err != nil {
		t.Errorf("the old ID's state after a proposal: %v", err)
	}

	if _, err := f.Connect(ctx, nfs("host-a", 2, entity.NewUser()), &p); !errors.Is(err, ErrClientInUse) {
		t.Errorf("confirm from another principal: %v, want ErrClientInUse", err)
	}
	if rec, err := f.Record(ctx, id); err != nil || rec.Pending == nil {
		t.Errorf("after another principal's confirm: pending %+v, %v; want the proposal kept", rec.Pending, err)
	}
	for _, tc := range []struct {
		name string
		c    Client
		p    Proposal
	}{
		{"another client ID", reboot, Proposal{Client: p.Client + 1, Confirm: p.Confirm}},
		{"another confirm", reboot, Proposal{Client: p.Client, Confirm: [8]byte{^p.Confirm[0]}}},
	} {
		if _, err := f.Connect(ctx, tc.c, &tc.p); !errors.Is(err, ErrStaleClient) {
			t.Errorf("confirm with %s: %v, want ErrStaleClient", tc.name, err)
		}
	}
	f.clock.advance(leasePeriod)
	if _, err := f.Connect(ctx, reboot, &p); !errors.Is(err, ErrStaleClient) {
		t.Errorf("confirm a lease period after the proposal: %v, want ErrStaleClient", err)
	}
}

// A confirm never brings back an ID that ended after its proposal: a
// proposal under the same Boot names the existing ID, and once that client
// has expired, been destroyed or been replaced by a one-step reboot, the
// confirm is ErrStaleClient and the ID stays ended.
func TestConfirmAfterExpiryIsStale(t *testing.T) {
	for name, end := range map[string]func(t *testing.T, f *fixture, c Client, id entity.ClientID){
		"expired": func(_ *testing.T, f *fixture, _ Client, _ entity.ClientID) {
			f.clock.advance(2 * time.Second) // past the lease, within the proposal's period
			f.sweep(ctx)
		},
		"destroyed": func(t *testing.T, f *fixture, c Client, id entity.ClientID) {
			if err := f.Destroy(ctx, Caller{id, c.Owner}); err != nil {
				t.Fatal(err)
			}
		},
		"rebooted in one step": func(t *testing.T, f *fixture, c Client, _ entity.ClientID) {
			c.Boot = [8]byte{2}
			if _, err := f.Connect(ctx, c, nil); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			c := nfs("host-a", 1, entity.NewUser())
			id := f.connect(t, c)
			f.clock.advance(leasePeriod - time.Second)
			p, err := f.Propose(ctx, c, addr)
			if err != nil || p.Client != id {
				t.Fatalf("a proposal under the same Boot: %d, %v; want %d", p.Client, err, id)
			}
			end(t, f, c, id)
			if got, err := f.Connect(ctx, c, &p); !errors.Is(err, ErrStaleClient) {
				t.Errorf("confirm after the client ended: %d, %v; want ErrStaleClient", got, err)
			}
			if _, err := f.Record(ctx, id); !errors.Is(err, ErrStaleClient) {
				t.Errorf("the ended ID after the confirm: %v, want still ended", err)
			}
		})
	}
}

// A client whose lease has lapsed is gone before the sweep too: proposing and
// confirming its name again, as a client told it is stale does at once, gets
// a new ID, and the old ID stays ended, its state released and ClientEnded
// raised for it once. A proposal made before the lapse confirms nothing.
func TestLapsedClientIsNotRevived(t *testing.T) {
	f0 := newFixture(t)
	f := unstarted(t, f0.fs, f0.clock)
	f.Table.sink = f.sink // no lease timer: this test's own sweep is the only one
	c, b := nfs("host-a", 1, entity.NewUser()), nfs("host-b", 1, entity.NewUser())
	id, bid := f.connect(t, c), f.connect(t, b)
	file := entity.NewFileID()
	ref := mintIn(t, f.Table, file, id, 0)
	f.clock.advance(leasePeriod - time.Second)
	pb, err := f.Propose(ctx, b, addr)
	if err != nil || pb.Client != bid {
		t.Fatalf("a proposal under the same Boot: %d, %v; want %d", pb.Client, err, bid)
	}
	f.clock.advance(2 * time.Second)
	if _, err := f.Connect(ctx, b, &pb); !errors.Is(err, ErrStaleClient) {
		t.Errorf("confirming a proposal for a client since lapsed: %v, want ErrStaleClient", err)
	}
	if _, err := f.Renew(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Fatalf("renewing a lapsed lease: %v, want ErrStaleClient", err)
	}
	if again := f.connect(t, c); again == id {
		t.Errorf("connecting the lapsed client's name again kept ID %d", id)
	}
	if _, err := f.Renew(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("the old ID after the name connected again: %v, want ErrStaleClient", err)
	}
	if _, err := f.resolve(id, file, ref); !errors.Is(err, ErrBadState) {
		t.Errorf("the old ID's state: %v, want released", err)
	}
	f.sweep(ctx)
	for _, c := range []entity.ClientID{id, bid} {
		if n := len(f.sink.endedOf(c)); n != 1 {
			t.Errorf("ClientEnded raised %d times for lapsed client %d, want once", n, c)
		}
	}
}

// Every bit of a confirm is read from Deps.Rand: a failed read proposes
// nothing.
func TestConfirmReadsDepsRand(t *testing.T) {
	f := newFixture(t)
	f.d.Rand = io.LimitReader(rand.Reader, 8)
	if _, err := f.Propose(ctx, nfs("host-a", 1, entity.NewUser()), addr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Propose(ctx, nfs("host-b", 1, entity.NewUser()), addr); err == nil {
		t.Fatal("a proposal past the end of Deps.Rand succeeded")
	}
	if len(f.pending) != 1 {
		t.Errorf("after a failed proposal: %d pending, want the first one", len(f.pending))
	}
}

// A node holds a bounded number of proposals, and so does each source, an
// IPv6 /64 counting as one; a replacement for a held name is not charged
// again, and a proposal a lease period old is dropped with its charge. A name
// is 1 to 1,024 bytes, an Adapter at most 4 KiB.
func TestProposalBounds(t *testing.T) {
	f := newFixture(t)
	owner := entity.NewUser()
	propose := func(name string, from string) error {
		_, err := f.Propose(ctx, nfs(name, 1, owner), netip.MustParseAddr(from))
		return err
	}
	delayed := func(err error) bool {
		var d *DelayError
		return errors.Is(err, ErrDelay) && errors.As(err, &d) && d.Cause == DelayResources
	}
	for i := range maxPerSource {
		if err := propose(fmt.Sprint("v6-", i), fmt.Sprintf("2001:db8::%x", i+1)); err != nil {
			t.Fatal(err)
		}
		if err := propose(fmt.Sprint("v4-", i), "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := propose("v6-over", "2001:db8::ffff:1"); !delayed(err) {
		t.Errorf("a proposal past the cap from one /64: %v, want ErrDelay(DelayResources)", err)
	}
	f.fs.Counts.Take()
	if err := propose("v4-over", "192.0.2.1"); !delayed(err) {
		t.Errorf("a proposal past the cap from one IPv4 address: %v, want ErrDelay(DelayResources)", err)
	}
	if n := f.fs.Counts.Take(); n != (kv.Counts{}) {
		t.Errorf("a proposal refused at the cap touched the store: %+v, want nothing", n)
	}
	if err := propose("v6-0", "2001:db8::1"); err != nil {
		t.Errorf("replacing a held proposal at its source's cap: %v", err)
	}
	if err := propose("v6-other", "2001:db8:0:1::1"); err != nil {
		t.Errorf("a proposal from another /64: %v", err)
	}
	if err := propose("v4-other", "192.0.2.2"); err != nil {
		t.Errorf("a proposal from another IPv4 address: %v", err)
	}

	for i := 0; len(f.pending) < maxPending; i++ {
		if err := propose(fmt.Sprint("fill-", i), fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)); err != nil {
			t.Fatal(err)
		}
	}
	if err := propose("node-over", "198.51.100.1"); !delayed(err) {
		t.Errorf("a proposal past the node's cap: %v, want ErrDelay(DelayResources)", err)
	}
	f.clock.advance(leasePeriod)
	f.sweep(ctx)
	if err := propose("v6-again", "2001:db8::1"); err != nil {
		t.Errorf("a proposal once the old ones lapsed: %v", err)
	}

	big := nfs("big", 1, owner)
	big.Adapter = make([]byte, kvstore.MaxAdapter+1)
	for name, c := range map[string]Client{
		"no name":                 nfs("", 1, owner),
		"a name past 1,024 bytes": nfs(string(make([]byte, maxName+1)), 1, owner),
		"an Adapter past 4 KiB":   big,
	} {
		if _, err := f.Propose(ctx, c, addr); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := f.Propose(ctx, nfs(string(make([]byte, maxName)), 1, owner), netip.MustParseAddr("203.0.113.1")); err != nil {
		t.Errorf("a name of %d bytes: %v", maxName, err)
	}
}

// AddSession writes its bytes only over the bytes its caller read, and
// otherwise answers ErrRecordChanged with the current record; what it wrote
// is the record after a restart.
func TestAddSessionWritesOverWhatItRead(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfs("host-a", 1, entity.NewUser()))
	n1, n2 := entity.NodeID{15: 1}, entity.NodeID{15: 2}
	if _, err := f.AddSession(ctx, id, n1, nil, []byte{1}); err != nil {
		t.Fatal(err)
	}
	cur, err := f.AddSession(ctx, id, n2, nil, []byte{2})
	if !errors.Is(err, ErrRecordChanged) || !bytes.Equal(cur.Adapter, []byte{1}) {
		t.Errorf("over bytes no longer stored: Adapter %v, %v; want ErrRecordChanged with the first's [1]", cur.Adapter, err)
	}
	if _, err := f.AddSession(ctx, id, n2, []byte{1}, make([]byte, kvstore.MaxAdapter+1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an Adapter past 4 KiB: %v, want ErrInvalid", err)
	}
	rec, err := reopen(t, f.fs, f.clock).Record(ctx, id)
	if err != nil || !bytes.Equal(rec.Adapter, []byte{1}) || len(rec.Reach.Sessions) != 1 {
		t.Errorf("after a restart: Adapter %v, sessions %v, %v; want [1] and one session", rec.Adapter, rec.Reach.Sessions, err)
	}
}

// A lease runs on Deps.Clock.Elapsed and a renewal writes nothing. A client
// silent for a lease period is expired by the lease timer: its state
// released, its record deleted, and ClientEnded raised with its shards. After
// a restart a client that renews is kept, its lease a full period from then.
func TestLeaseExpiresASilentClient(t *testing.T) {
	f := newFixture(t)
	quiet := f.connect(t, nfs("quiet", 1, entity.NewUser()))
	busy := f.connect(t, nfs("busy", 1, entity.NewUser()))
	if err := f.fs.Store.Clients().MarkShard(ctx, quiet, entity.ShardMark{Shard: 5}); err != nil {
		t.Fatal(err)
	}
	file := entity.NewFileID()
	ref := mintIn(t, f.Table, file, quiet, 5)

	f.fs.Counts.Take()
	for range 100 {
		if _, err := f.Renew(ctx, busy); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.fs.Counts.Take(); n.Sets+n.Deletes != 0 {
		t.Errorf("100 renewals wrote %d keys, want none", n.Sets+n.Deletes)
	}

	f.clock.advance(leasePeriod - time.Second)
	if _, err := f.Renew(ctx, busy); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Second)
	if _, err := f.Renew(ctx, quiet); !errors.Is(err, ErrStaleClient) {
		t.Errorf("renewing a lease that has ended: %v, want ErrStaleClient", err)
	}
	f.sweep(ctx)
	if _, err := f.Record(ctx, quiet); !errors.Is(err, ErrStaleClient) {
		t.Errorf("the expired client's record: %v, want gone", err)
	}
	if _, err := f.resolve(quiet, file, ref); !errors.Is(err, ErrBadState) {
		t.Errorf("the expired client's state: %v, want released", err)
	}
	if e := f.sink.endedOf(quiet); len(e) != 1 || len(e[0].shards) != 1 || e[0].shards[0] != 5 {
		t.Errorf("ClientEnded for the expired client: %+v, want once with shard 5", e)
	}
	if _, err := f.Renew(ctx, busy); err != nil {
		t.Errorf("a client that renewed within the lease: %v", err)
	}

	g := reopen(t, f.fs, f.clock)
	g.clock.advance(leasePeriod - time.Second)
	if _, err := g.Renew(ctx, busy); err != nil {
		t.Fatalf("renewing after a restart: %v", err)
	}
	g.clock.advance(leasePeriod - time.Second)
	g.sweep(ctx)
	if _, err := g.Renew(ctx, busy); err != nil {
		t.Errorf("within a lease period of its renewal after a restart: %v", err)
	}
	g.clock.advance(leasePeriod)
	g.sweep(ctx)
	if _, err := g.Record(ctx, busy); !errors.Is(err, ErrStaleClient) {
		t.Errorf("silent for a lease period after a restart: %v, want expired", err)
	}
}

// A reboot never leaves both IDs current: a renewal of the old ID racing
// the new Boot's confirm, on a node that has not yet seen the old one since
// its restart, does not keep it.
func TestRebootRaceLeavesOneID(t *testing.T) {
	f := newFixture(t)
	owner := entity.NewUser()
	for i := range 2000 {
		name := fmt.Sprint("host-", i)
		old := f.connect(t, nfs(name, 1, owner))
		g := reopen(t, f.fs, f.clock)
		file := entity.NewFileID()
		ref := mintIn(t, g.Table, file, old, 0)
		p, err := g.Propose(ctx, nfs(name, 2, owner), addr)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = g.Renew(ctx, old) })
		if _, err := g.Connect(ctx, nfs(name, 2, owner), &p); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if _, err := g.Renew(ctx, old); !errors.Is(err, ErrStaleClient) {
			t.Fatalf("round %d: the old ID after the reboot: %v, want ErrStaleClient", i, err)
		}
		if _, err := g.Renew(ctx, p.Client); err != nil {
			t.Fatalf("round %d: the new ID: %v", i, err)
		}
		if _, err := g.resolve(old, file, ref); !errors.Is(err, ErrBadState) {
			t.Fatalf("round %d: the old ID's state: %v, want released", i, err)
		}
		g.Stop()
	}
}

// Destroy deletes a client only while it holds no session and no state, and
// a protected one only for its owner; a refused Destroy releases nothing.
func TestDestroy(t *testing.T) {
	f := newFixture(t)
	alice := entity.NewUser()
	withSession := f.connect(t, nfs("session", 1, alice))
	if _, err := f.AddSession(ctx, withSession, entity.NodeID{15: 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	withState := f.connect(t, nfs("state", 1, alice))
	file := entity.NewFileID()
	ref := mintIn(t, f.Table, file, withState, 0)
	idle := f.connect(t, nfs("idle", 1, alice))

	for _, c := range []entity.ClientID{withSession, withState} {
		if err := f.Destroy(ctx, Caller{c, alice}); !errors.Is(err, ErrClientBusy) {
			t.Errorf("Destroy of client %d: %v, want ErrClientBusy", c, err)
		}
		if _, err := f.Record(ctx, c); err != nil {
			t.Errorf("client %d after a refused Destroy: %v", c, err)
		}
	}
	if _, err := f.resolve(withState, file, ref); err != nil {
		t.Errorf("state after a refused Destroy: %v", err)
	}
	if err := f.Destroy(ctx, Caller{idle, entity.NewUser()}); !errors.Is(err, ErrNotYours) {
		t.Errorf("Destroy of a protected client by another principal: %v, want ErrNotYours", err)
	}
	if err := f.Destroy(ctx, Caller{idle, alice}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Renew(ctx, idle); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a destroyed client: %v, want ErrStaleClient", err)
	}
}

// Every mutating call before Start and after Stop is ErrStopped, a table
// starts once, and Start's timer expires clients on the table's clock.
func TestStartAndStop(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfs("host-a", 1, entity.NewUser()))
	g := unstarted(t, f.fs, f.clock)
	calls := map[string]func() error{
		"Propose":    func() error { _, err := g.Propose(ctx, nfs("host-b", 1, entity.NewUser()), addr); return err },
		"Connect":    func() error { _, err := g.Connect(ctx, nfs("host-b", 1, entity.Principal{}), nil); return err },
		"AddSession": func() error { _, err := g.AddSession(ctx, id, entity.NodeID{15: 1}, nil, []byte{1}); return err },
		"Renew":      func() error { _, err := g.Renew(ctx, id); return err },
		"Expire":     func() error { return g.Expire(ctx, id) },
		"Destroy":    func() error { return g.Destroy(ctx, Caller{Client: id}) },
	}
	check := func(when string) {
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrStopped) {
				t.Errorf("%s %s: %v, want ErrStopped", name, when, err)
			}
		}
	}
	check("before Start")
	if err := g.Start(ctx, g.sink, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(ctx, g.sink, nil); err == nil {
		t.Error("a second Start was accepted")
	}
	g.Stop()
	check("after Stop")
	if _, err := f.Record(ctx, id); err != nil {
		t.Fatalf("calls on a stopped table changed the store: %v", err)
	}

	for deadline := time.Now().Add(5 * time.Second); ; {
		f.clock.advance(sweepEvery)
		if _, err := f.Record(ctx, id); errors.Is(err, ErrStaleClient) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client was not expired after the clock passed its lease")
		}
		time.Sleep(time.Millisecond)
	}
}

// No Sink method runs under a table lock: a Sink that calls back into the
// table from ClientEnded deadlocks nothing.
func TestSinkMayCallBack(t *testing.T) {
	f := newFixture(t)
	owner := entity.NewUser()
	id := f.connect(t, nfs("host-a", 1, owner))
	f.sink.onEnded = func(c entity.ClientID) {
		_ = f.Expire(ctx, c)
		_, _ = f.Record(ctx, c)
		_, _ = f.Renew(ctx, c)
		_, _ = f.Propose(ctx, nfs("host-b", 1, owner), addr)
		_ = f.step(entity.NewFileID(), func(*entry) error { return nil })
	}
	done := make(chan struct{})
	go func() {
		f.clock.advance(leasePeriod)
		f.sweep(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the expiry did not return: a Sink calling back into the table deadlocked")
	}
	if n := len(f.sink.endedOf(id)); n != 1 {
		t.Fatalf("ClientEnded raised %d times, want once", n)
	}
}

// Expire is node-local: it releases the client's state here and drops its
// lease here, but deletes no record and raises no ClientEnded, which are the
// lease timer's.
func TestExpireIsNodeLocal(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfs("host-a", 1, entity.NewUser()))
	file := entity.NewFileID()
	ref := mintIn(t, f.Table, file, id, 0)
	if err := f.Expire(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolve(id, file, ref); !errors.Is(err, ErrBadState) {
		t.Errorf("state after Expire: %v, want released", err)
	}
	if _, err := f.Record(ctx, id); err != nil {
		t.Errorf("record after Expire: %v, want kept for the lease owner", err)
	}
	if n := len(f.sink.endedOf(id)); n != 0 {
		t.Errorf("Expire raised ClientEnded %d times, want none", n)
	}
	f.cmu.Lock()
	_, leased := f.leases[id]
	f.cmu.Unlock()
	if leased {
		t.Error("Expire kept the lease")
	}
}
