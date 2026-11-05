package openstate

import (
	"bytes"
	"context"
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
	id, _, err := f.Connect(ctx, c, &p)
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
	if _, _, err := f.Connect(ctx, nfs("host-a", 1, bob), nil); !errors.Is(err, ErrClientInUse) {
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

	if _, _, err := f.Connect(ctx, nfs("host-a", 2, entity.NewUser()), &p); !errors.Is(err, ErrClientInUse) {
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
		if _, _, err := f.Connect(ctx, tc.c, &tc.p); !errors.Is(err, ErrStaleClient) {
			t.Errorf("confirm with %s: %v, want ErrStaleClient", tc.name, err)
		}
	}
	f.clock.advance(leasePeriod)
	if _, _, err := f.Connect(ctx, reboot, &p); !errors.Is(err, ErrStaleClient) {
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
			if err := f.Destroy(ctx, Caller{Client: id, Principal: c.Owner}); err != nil {
				t.Fatal(err)
			}
		},
		"rebooted in one step": func(t *testing.T, f *fixture, c Client, _ entity.ClientID) {
			c.Boot = [8]byte{2}
			if _, _, err := f.Connect(ctx, c, nil); err != nil {
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
			if got, _, err := f.Connect(ctx, c, &p); !errors.Is(err, ErrStaleClient) {
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
	if _, _, err := f.Connect(ctx, b, &pb); !errors.Is(err, ErrStaleClient) {
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
		g := reopenServing(t, f.fs, f.clock, nil)
		file := entity.NewFileID()
		ref := mintIn(t, g.Table, file, old, 0)
		p, err := g.Propose(ctx, nfs(name, 2, owner), addr)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = g.Renew(ctx, old) })
		if _, _, err := g.Connect(ctx, nfs(name, 2, owner), &p); err != nil {
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
		if err := f.Destroy(ctx, Caller{Client: c, Principal: alice}); !errors.Is(err, ErrClientBusy) {
			t.Errorf("Destroy of client %d: %v, want ErrClientBusy", c, err)
		}
		if _, err := f.Record(ctx, c); err != nil {
			t.Errorf("client %d after a refused Destroy: %v", c, err)
		}
	}
	if _, err := f.resolve(withState, file, ref); err != nil {
		t.Errorf("state after a refused Destroy: %v", err)
	}
	if err := f.Destroy(ctx, Caller{Client: idle, Principal: entity.NewUser()}); !errors.Is(err, ErrNotYours) {
		t.Errorf("Destroy of a protected client by another principal: %v, want ErrNotYours", err)
	}
	if err := f.Destroy(ctx, Caller{Client: idle, Principal: alice}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Renew(ctx, idle); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a destroyed client: %v, want ErrStaleClient", err)
	}
}

// A Disconnect with Last takes its session's node out of the client's
// Reach.Sessions in the same call, and the client's Destroy, refused while
// the node was listed, then succeeds; one without Last leaves it listed.
func TestLastDisconnectLetsDestroyThrough(t *testing.T) {
	f := newFixture(t)
	alice := entity.NewUser()
	id := f.connect(t, nfs("laptop", 1, alice))
	rec, err := f.AddSession(ctx, id, nodeOf(s1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Disconnect(ctx, id, s1, SessionEnd{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Destroy(ctx, Caller{Client: id, Principal: alice}); !errors.Is(err, ErrClientBusy) {
		t.Fatalf("Destroy with a node listed: %v, want ErrClientBusy", err)
	}
	if _, err := f.Disconnect(ctx, id, s1, SessionEnd{Last: true, Gen: rec.Reach.GenOf(nodeOf(s1))}); err != nil {
		t.Fatal(err)
	}
	if rec, err := f.Record(ctx, id); err != nil || len(rec.Reach.Sessions) != 0 {
		t.Fatalf("after the last Disconnect: sessions %v, %v; want none", rec.Reach.Sessions, err)
	}
	if err := f.Destroy(ctx, Caller{Client: id, Principal: alice}); err != nil {
		t.Errorf("Destroy after the last Disconnect: %v", err)
	}
}

// A Reach.Sessions entry recorded under an earlier start's node epoch counts
// as absent: after a restart the client's Destroy goes through, though the
// entry is still written, and a session recorded again keeps it busy.
func TestAnEntryUnderAnOldEpochIsAbsent(t *testing.T) {
	f := newFixture(t)
	alice := entity.NewUser()
	id := f.connect(t, nfs("laptop", 1, alice))
	again := f.connect(t, nfs("desktop", 1, alice))
	for _, c := range []entity.ClientID{id, again} {
		if _, err := f.AddSession(ctx, c, entity.NodeID{}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.Stop()
	g := reopen(t, f.fs, f.clock)
	if rec, err := g.Record(ctx, id); err != nil || len(rec.Reach.Sessions) != 1 {
		t.Fatalf("after a restart: sessions %v, %v; want the entry still written", rec.Reach.Sessions, err)
	}
	if _, err := g.AddSession(ctx, again, entity.NodeID{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Destroy(ctx, Caller{Client: again, Principal: alice}); !errors.Is(err, ErrClientBusy) {
		t.Errorf("Destroy of a client listed again after the restart: %v, want ErrClientBusy", err)
	}
	if err := g.Destroy(ctx, Caller{Client: id, Principal: alice}); err != nil {
		t.Errorf("Destroy of a client listed only under the old epoch: %v, want it through", err)
	}
}

// A last Disconnect at a Gen a later listing raised removes nothing: the
// session set up meanwhile keeps the node listed, and the Disconnect of that
// session, at its own Gen, then unlists it.
func TestALastDisconnectAtAStaleGenKeepsTheEntry(t *testing.T) {
	f := newFixture(t)
	id, first, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil || second <= first {
		t.Fatalf("a second session's Connect: Gen %d after %d, %v; want it raised", second, first, err)
	}
	this := entity.SessionRef{ID: 1}
	if gen, err := f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: first}); err != nil || gen != second {
		t.Fatalf("a last Disconnect at the stale Gen: Gen %d, %v; want the entry's current Gen %d", gen, err, second)
	}
	if rec, err := f.Record(ctx, id); err != nil || rec.Reach.GenOf(nodeOf(this)) != second {
		t.Fatalf("after a last Disconnect at the stale Gen: sessions %v, %v; want the newer entry kept", rec.Reach.Sessions, err)
	}
	if gen, err := f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: second}); err != nil || gen != 0 {
		t.Fatalf("a last Disconnect at the current Gen: Gen %d, %v; want zero, the entry gone", gen, err)
	}
	if _, err := f.Record(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("after a last Disconnect at the current Gen: %v, want the record released, ErrStaleClient", err)
	}
	if _, err := f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: second}); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a repeat of the last Disconnect after the release: %v, want ErrStaleClient", err)
	}
}

// A second session's listing, which Connect writes outside the client step,
// landing between a last Disconnect's unlisting and its release keeps the
// record: the Disconnect returns the new entry's Gen.
func TestALastDisconnectRacingAListingKeepsTheRecord(t *testing.T) {
	f := newFixture(t)
	id, first, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var second uint64
	leaveReachHook = func() {
		leaveReachHook = nil
		rec, err := f.Record(ctx, id)
		if err == nil {
			rec, err = f.AddSession(ctx, id, entity.NodeID{}, rec.Adapter, rec.Adapter)
		}
		if err != nil {
			t.Fatalf("the racing listing: %v", err)
		}
		second = rec.Reach.GenOf(entity.NodeID{})
	}
	t.Cleanup(func() { leaveReachHook = nil })
	this := entity.SessionRef{ID: 1}
	if gen, err := f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: first}); err != nil || gen == 0 || gen != second {
		t.Fatalf("a last Disconnect racing a listing: Gen %d, %v; want the listed Gen %d", gen, err, second)
	}
	if rec, err := f.Record(ctx, id); err != nil || rec.Reach.GenOf(nodeOf(this)) != second {
		t.Errorf("after the race: sessions %v, %v; want the record kept with the new entry", rec.Reach.Sessions, err)
	}
}

// An entry removed and listed again takes a Gen above every one the record
// gave, so a repeat of the Disconnect that removed it cannot remove the new
// entry.
func TestAnEntryListedAgainTakesAHigherGen(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfs("laptop", 1, entity.NewUser()))
	rec, err := f.AddSession(ctx, id, nodeOf(s1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := rec.Reach.GenOf(nodeOf(s1))
	if _, err := f.Disconnect(ctx, id, s1, SessionEnd{Last: true, Gen: first}); err != nil {
		t.Fatal(err)
	}
	if rec, err = f.AddSession(ctx, id, nodeOf(s1), nil, nil); err != nil {
		t.Fatal(err)
	}
	if again := rec.Reach.GenOf(nodeOf(s1)); again <= first {
		t.Errorf("the entry listed again after its removal: Gen %d, want above %d", again, first)
	}
}

// An SMB Connect at a new node epoch lists the node again under that epoch,
// replacing its older-epoch entry rather than taking the node as listed
// already, at a raised Gen; the entry then counts, so Destroy is refused.
func TestAConnectAtANewEpochReplacesTheEntry(t *testing.T) {
	f := newFixture(t)
	id, gen, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.Record(ctx, id)
	if err != nil || len(old.Reach.Sessions) != 1 {
		t.Fatalf("after the first Connect: sessions %v, %v; want one", old.Reach.Sessions, err)
	}
	f.Stop()
	g := reopen(t, f.fs, f.clock)
	if _, again, err := g.Connect(ctx, smb("pc-a"), nil); err != nil || again <= gen {
		t.Fatalf("a Connect at the new epoch: Gen %d after %d, %v; want it raised", again, gen, err)
	}
	rec, err := g.Record(ctx, id)
	if err != nil || len(rec.Reach.Sessions) != 1 || rec.Reach.Sessions[0].Epoch <= old.Reach.Sessions[0].Epoch {
		t.Fatalf("after a Connect at the new epoch: sessions %v, %v; want one entry, above epoch %d", rec.Reach.Sessions, err, old.Reach.Sessions[0].Epoch)
	}
	if err := g.Destroy(ctx, Caller{Client: id}); !errors.Is(err, ErrClientBusy) {
		t.Errorf("Destroy of a client listed again at the new epoch: %v, want ErrClientBusy", err)
	}
}

// An SMB record holding no open is not released while a node is still
// listed in its Reach.Sessions: only the last node's last Disconnect
// releases it.
func TestAnSMBRecordStaysWhileANodeIsListed(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, smb("pc-a")) // lists this node, node zero
	on1, on2 := entity.SessionRef{ID: 1}, entity.SessionRef{Node: 2, ID: 1}
	rec, err := f.Record(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err = f.AddSession(ctx, id, nodeOf(on2), rec.Adapter, rec.Adapter); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Disconnect(ctx, id, on1, SessionEnd{Last: true, Gen: rec.Reach.GenOf(nodeOf(on1))}); err != nil {
		t.Fatal(err)
	}
	if rec, err := f.Record(ctx, id); err != nil || len(rec.Reach.Sessions) != 1 || rec.Reach.Sessions[0].Node != nodeOf(on2) {
		t.Fatalf("with node 2 still listed: sessions %v, %v; want the record kept, listing node 2", rec.Reach.Sessions, err)
	}
	if _, err := f.Disconnect(ctx, id, on2, SessionEnd{Last: true, Gen: rec.Reach.GenOf(nodeOf(on2))}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Record(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("with no node listed and no open: %v, want the record released, ErrStaleClient", err)
	}
}

// Every mutating call before Start and after Stop is ErrStopped, a table
// starts once, and Start's timer expires clients on the table's clock.
func TestStartAndStop(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfs("host-a", 1, entity.NewUser()))
	g := unstarted(t, f.fs, f.clock)
	file := anyFile()
	calls := map[string]func() error{
		"Propose":    func() error { _, err := g.Propose(ctx, nfs("host-b", 1, entity.NewUser()), addr); return err },
		"Connect":    func() error { _, _, err := g.Connect(ctx, nfs("host-b", 1, entity.Principal{}), nil); return err },
		"AddSession": func() error { _, err := g.AddSession(ctx, id, entity.NodeID{15: 1}, nil, []byte{1}); return err },
		"Renew":      func() error { _, err := g.Renew(ctx, id); return err },
		"Expire":     func() error { return g.Expire(ctx, id) },
		"Destroy":    func() error { return g.Destroy(ctx, Caller{Client: id}) },
		"Open":       func() error { _, err := g.Opens().Open(ctx, Caller{Client: id}, file, OpenRequest{}); return err },
		"Downgrade": func() error {
			_, err := g.Opens().Downgrade(ctx, Caller{Client: id}, OpenRef{Kind: RefOpened, Open: 1}, Seqs{}, 0, 0)
			return err
		},
		"Update": func() error { return g.Opens().Update(ctx, Caller{Client: id}, 1, OpenUpdate{}) },
		"Close": func() error {
			_, err := g.Opens().Close(ctx, Caller{Client: id}, OpenRef{Kind: RefOpened, Open: 1}, Seqs{})
			return err
		},
		"Durable":   func() error { _, _, err := g.Opens().Durable(ctx, file); return err },
		"Releasing": func() error { _, err := g.Opens().Releasing(ctx, file); return err },
		"ConfirmOpen": func() error {
			_, err := g.Opens().ConfirmOpen(ctx, Caller{Client: id}, OpenRef{Kind: RefOpened, Open: 1}, Seqs{})
			return err
		},
		"Lock": func() error {
			_, err := g.Locks().Lock(ctx, Caller{Client: id}, file, LockReq{Owner: LockOwner{Client: id}, Ranges: []LockRange{{}}})
			return err
		},
		"CancelLock": func() error {
			_, err := g.Locks().CancelLock(ctx, Caller{Client: id}, file, LockOwner{Client: id}, ByteRange{})
			return err
		},
		"WaitAnswered": func() error {
			return g.Locks().WaitAnswered(ctx, Caller{Client: id}, LockWaiter{File: file.ID, Owner: LockOwner{Client: id}}, true)
		},
		"Unlock": func() error {
			_, err := g.Locks().Unlock(ctx, Caller{Client: id}, file, OpenRef{}, LockOwner{Client: id}, nil, Seqs{}, nil)
			return err
		},
		"ReleaseLockOwner": func() error { return g.Locks().ReleaseLockOwner(ctx, Caller{Client: id}, LockOwner{Client: id}) },
		"AckRecall":        func() error { return g.Grants().AckRecall(ctx, Caller{Client: id}, GrantID{}, GrantNone) },
		"Revoke":           func() error { return g.Revoke(ctx, id) },
		"Rebind":           func() error { return g.Rebind(ctx, Caller{Client: id}, entity.NodeID{1}) },
		"FreeState":        func() error { return g.Recovery().FreeState(ctx, Caller{Client: id}, StateRef{}) },
		"CheckIO": func() error {
			return g.Checks().CheckIO(ctx, Caller{Client: id}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, ByteRange{}, true)
		},
		"CheckChange": func() error { return g.Checks().CheckChange(ctx, Caller{Client: id}, file, nil, ChangeAttrs) },
		"Watch":       func() error { _, err := g.Watches().Watch(ctx, Caller{Client: id}, file, false, names); return err },
		"Unwatch":     func() error { return g.Watches().Unwatch(ctx, Caller{Client: id}, WatchID{}) },
		"DropShare":   func() error { return g.Recovery().DropShare(ctx, f.fs.Share.ID, netip.Addr{}) },
		"CloneOpen":   func() error { _, err := g.Opens().CloneOpen(ctx, file, CloneSource{}); return err },
		"CloneClose":  func() error { return g.Opens().CloneClose(ctx, 1) },
		"Channel":     func() error { _, _, err := g.Opens().Channel(ctx, Caller{Client: id}, 1, 0, false); return err },
		"Disconnect":  func() error { _, err := g.Disconnect(ctx, id, entity.SessionRef{}, SessionEnd{}); return err },
		"ChannelDown": func() error { return g.ChannelDown(ctx, id) },
		"Reconnect": func() error {
			_, err := g.Opens().Reconnect(ctx, Caller{Client: id}, 1, ReconnectMatch{}, entity.SessionRef{})
			return err
		},
		"Flushed":         func() error { return g.Opens().Flushed(ctx, Caller{Client: id}, 1, 0) },
		"Renamed":         func() error { return g.Opens().Renamed(ctx, file, entity.FileID{}, nil, entity.FileID{}, nil) },
		"ServeShard":      func() error { return g.Recovery().ServeShard(ctx, f.fs.Share.ID, 0, 1) },
		"ReclaimComplete": func() error { return g.Recovery().ReclaimComplete(ctx, Caller{Client: id}, nil) },
	}
	check := func(when string) {
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrStopped) {
				t.Errorf("%s %s: %v, want ErrStopped", name, when, err)
			}
		}
	}
	check("before Start")
	if err := g.Start(ctx, g.sink, nil); err == nil {
		t.Error("a Start with no Order was accepted")
	}
	if err := g.Start(ctx, g.sink, g.order.order); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(ctx, g.sink, g.order.order); err == nil {
		t.Error("a second Start was accepted")
	}
	g.Stop()
	if err := g.Start(ctx, g.sink, g.order.order); err == nil {
		t.Error("a Start after Stop was accepted")
	}
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

// The lease timer outlives the ctx Start was given: once that is cancelled,
// a client silent for a lease period is still expired.
func TestStartContextDoesNotStopTheTimer(t *testing.T) {
	f0 := newFixture(t)
	f := unstarted(t, f0.fs, f0.clock)
	sctx, cancel := context.WithCancel(ctx)
	if err := f.Start(sctx, f.sink, f.order.order); err != nil {
		t.Fatal(err)
	}
	id := f.connect(t, nfs("host-a", 1, entity.NewUser()))
	cancel()
	for deadline := time.Now().Add(5 * time.Second); ; {
		f.clock.advance(sweepEvery)
		if len(f.sink.endedOf(id)) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client was not expired after Start's ctx was cancelled")
		}
		time.Sleep(time.Millisecond)
	}
}

// No Sink method runs under a table lock: a Sink that calls back into the
// table from ClientEnded deadlocks nothing, whether the sweep or a confirm
// that expires a lapsed client raised it.
func TestSinkMayCallBack(t *testing.T) {
	f0 := newFixture(t)
	f := unstarted(t, f0.fs, f0.clock)
	f.Table.sink = f.sink // no lease timer: this test's own sweep is the only one
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

	id = f.connect(t, nfs("host-a", 1, owner))
	f.clock.advance(leasePeriod)
	done = make(chan struct{})
	go func() {
		_, _, _ = f.Connect(ctx, nfs("host-a", 1, owner), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the confirm did not return: a Sink calling back into the table deadlocked")
	}
	if n := len(f.sink.endedOf(id)); n != 1 {
		t.Fatalf("ClientEnded raised %d times by the confirm, want once", n)
	}
}

// Expire is node-local: it releases the client's state here and drops its
// lease here, so it is granted nothing more here, but deletes no record and
// raises no ClientEnded, which are the lease timer's.
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
	if f.current(id) {
		t.Error("Expire left the client current here, so it could still be granted an open")
	}
}

// A reboot ends the old ID at once: it is granted no open, and the opens it
// held are released, each file a close left to unlink raised Closed.
func TestRebootEndsTheOldID(t *testing.T) {
	f := newFixture(t)
	c := nfs("host-a", 1, entity.NewUser())
	old := f.caller(t, c)
	marked, dir := anyFile(), entity.NewFileID()
	f.mustOpen(t, old, marked, OpenRequest{Via: dir, Name: []byte("m"), Access: del, DeleteOnClose: true})
	c.Boot = [8]byte{2}
	f.connect(t, c)
	if _, err := f.open(old, anyFile(), OpenRequest{Access: rd}); !errors.Is(err, ErrStaleClient) {
		t.Errorf("an open by the ID a reboot ended: %v, want ErrStaleClient", err)
	}
	var unlinked bool
	for _, cl := range f.sink.closed {
		unlinked = unlinked || cl.f == marked.ID && cl.r.Unlink != nil && cl.r.Unlink.Parent == dir
	}
	if !unlinked {
		t.Errorf("Closed after the reboot: %+v, want the marked file's unlink", f.sink.closed)
	}
}

// After Expire the client is in none of the indexes: no reference, count,
// lease key or grant budget names it, as the rebuild from the entries
// agrees.
func TestExpiredClientLeavesNoIndex(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("a", 1, entity.NewUser())), f.caller(t, nfs("b", 1, entity.NewUser()))
	file, held := in(1), in(1)
	o, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Via: entity.NewFileID(), Owner: owner(a, "oo"), Want: GrantRead})
	if g == nil {
		t.Fatal("no grant offered")
	}
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "lo"), Open: refOf(o), Ranges: []LockRange{sh(0, 10)}})
	f.mustLock(t, b, held, LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}})
	if _, err := f.lock(a, held, LockReq{Owner: owner(a, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitPoll}); lockedBy(err) != "lo" {
		t.Fatalf("waiter: %v", err)
	}
	f.watch(t, a, in(1), false, names)

	if err := f.Expire(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	x := f.kept()
	for k, h := range x.refs {
		if h.client == a.Client {
			t.Errorf("reference %+v still names the client", k)
		}
	}
	for k := range x.held {
		if k.client == a.Client {
			t.Errorf("count %+v still names the client", k)
		}
	}
	for k := range x.owners {
		if k.client == a.Client {
			t.Errorf("owner %+v still names the client", k)
		}
	}
	if _, ok := x.granted[a.Client]; ok || len(x.keys) > 0 || len(x.via) > 0 {
		t.Errorf("grant budget %v, lease keys %v, counts per Via %v after the expiry", x.granted, x.keys, x.via)
	}
}

// A confirm the store refuses leaves its proposal: another principal's
// confirm of it is refused, and the same confirm retried connects the client.
func TestRefusedConfirmMayBeRetried(t *testing.T) {
	fs, st := failingFS(t)
	f := reopen(t, fs, &fakeClock{})
	c := nfs("host-a", 1, entity.NewUser())
	p, err := f.Propose(ctx, c, addr)
	if err != nil {
		t.Fatal(err)
	}
	st.fail.Store(true)
	_, _, err = f.Connect(ctx, c, &p)
	st.fail.Store(false)
	if !errors.Is(err, errWrite) {
		t.Fatalf("a confirm over a refusing store: %v, want its error", err)
	}
	if _, _, err := f.Connect(ctx, nfs("host-a", 1, entity.NewUser()), &p); !errors.Is(err, ErrClientInUse) {
		t.Errorf("another principal's confirm of the name: %v, want ErrClientInUse", err)
	}
	if id, _, err := f.Connect(ctx, c, &p); err != nil || id != p.Client {
		t.Errorf("the same confirm retried: %d, %v; want %d", id, err, p.Client)
	}
}

// An SMB Connect that a node's last Disconnect lands inside, between its
// connect and its listing, is not refused: the listing is under the client
// step, so the Disconnect runs after it, finds the node listed again and
// keeps the record (RFC 14 §2.1, L52).
func TestAConnectRacingTheLastDisconnectIsListed(t *testing.T) {
	f := newFixture(t)
	id, first, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	this := entity.SessionRef{ID: 1}
	type result struct {
		gen uint64
		err error
	}
	done := make(chan result, 1)
	connectListHook = func() {
		connectListHook = nil
		go func() {
			gen, err := f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: first})
			done <- result{gen, err}
		}()
		select { // gives an unlocked listing the time to lose the race
		case r := <-done:
			done <- r
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() { connectListHook = nil })
	again, second, err := f.Connect(ctx, smb("pc-a"), nil)
	if err != nil {
		t.Fatalf("a Connect racing the last Disconnect: %v, want it listed", err)
	}
	r := <-done
	if r.err != nil || again != id || r.gen != second {
		t.Fatalf("the racing Disconnect: Gen %d, %v (client %d, Connect's %d); want the record kept at Gen %d", r.gen, r.err, id, again, second)
	}
	if rec, err := f.Record(ctx, id); err != nil || rec.Reach.GenOf(nodeOf(this)) != second {
		t.Errorf("after the race: sessions %v, %v; want the node listed at Gen %d", rec.Reach.Sessions, err, second)
	}
}

// Session setups racing a node's last logoff through the public calls never
// fail: each Connect lists its node whichever way the race goes (L52).
func TestConnectsRacingLastDisconnectsAreNeverRefused(t *testing.T) {
	f := newFixture(t)
	this := entity.SessionRef{ID: 1}
	for i := range 500 {
		id, gen, err := f.Connect(ctx, smb("pc-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Disconnect(ctx, id, this, SessionEnd{Last: true, Gen: gen})
		}()
		again, _, err := f.Connect(ctx, smb("pc-a"), nil)
		wg.Wait()
		if err != nil {
			t.Fatalf("round %d: a Connect racing the last Disconnect: %v", i, err)
		}
		rec, err := f.Record(ctx, again)
		if err != nil || len(rec.Reach.Sessions) == 0 {
			t.Fatalf("round %d: the racing Connect's client: sessions %v, %v; want its node listed", i, rec.Reach.Sessions, err)
		}
		if _, err := f.Disconnect(ctx, again, this, SessionEnd{Last: true, Gen: rec.Reach.GenOf(nodeOf(this))}); err != nil {
			t.Fatalf("round %d: the closing Disconnect: %v", i, err)
		}
	}
}

// A second Start is refused before it changes the store: the node epoch it
// would raise is left where the first Start put it.
func TestASecondStartRaisesNoEpoch(t *testing.T) {
	f := newFixture(t)
	if err := f.Start(ctx, f.sink, (&orderLog{}).order); err == nil {
		t.Fatal("a second Start succeeded")
	}
	next, err := f.d.Records.RaiseNodeEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next != f.epoch+1 {
		t.Errorf("the epoch after a refused second Start: next raise %d, want %d", next, f.epoch+1)
	}
}
