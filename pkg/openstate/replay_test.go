package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// A replayed create naming an open of its client by its create ID answers
// that open, making none: Replayed, the same open and its lease. Another
// client's replay under the same ID, or one with no ID, is a create of its
// own, and another principal's replay is refused.
func TestCreateReplay(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file := anyFile()
	r := keep(OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 7)
	first, err := f.Opens().Open(ctx, a, file, r)
	if err != nil || first.Grant == nil {
		t.Fatalf("Open %+v, %v; want it leased", first, err)
	}
	r.Replay = true
	again, err := f.Opens().Open(ctx, a, file, r)
	if err != nil || !again.Replayed || again.Open.ID != first.Open.ID || again.Grant == nil || again.Grant.ID != first.Grant.ID {
		t.Errorf("the replay %+v, %v; want the first open and its lease, Replayed", again, err)
	}
	if _, err := f.Opens().Open(ctx, Caller{Client: a.Client, Principal: entity.NewUser(), Protocol: a.Protocol}, file, r); !errors.Is(err, ErrNotYours) {
		t.Errorf("another principal's replay: %v, want ErrNotYours", err)
	}
	r.Lease = LeaseKey{}
	if res, err := f.Opens().Open(ctx, b, file, r); err != nil || res.Replayed {
		t.Errorf("another client's replay under the ID: %+v, %v; want an open of its own", res, err)
	}
	if n := len(f.entryOf(file.ID).opens); n != 2 {
		t.Errorf("%d opens, want the first and the other client's", n)
	}

	// An open named by no create ID, or disconnected, is no replay's.
	v1 := keep(OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rh}, entity.DurabilityDurable, s1, time.Minute, 0)
	f.mustOpen(t, a, file, v1)
	v1.Replay = true
	if res, err := f.Opens().Open(ctx, a, file, v1); err != nil || res.Replayed {
		t.Errorf("a replay naming no create ID: %+v, %v; want an open of its own", res, err)
	}
	f.disconnect(t, a, s1)
	r.Lease, r.Session = LeaseKey{1}, s2
	if res, err := f.Opens().Open(ctx, a, file, r); err != nil || res.Replayed {
		t.Errorf("a replay of a disconnected open: %+v, %v; want an open of its own", res, err)
	}
}

// A lock naming its open's lock sequence is stored there once granted, for
// every range of it at once: its retry is answered and not applied, after a
// failover too, while a retry after a refusal is applied.
func TestLockReplay(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smb("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access = rd | wr
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ob := f.mustOpen(t, b, file, OpenRequest{Access: rd})
	lockAs := func(g *fixture, seq uint8, rs ...LockRange) error {
		_, err := g.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: rs, Replay: &LockSequence{Index: 3, Seq: seq}})
		return err
	}
	if err := lockAs(f, 1, x(0, 10), x(20, 10)); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(f, 1, x(0, 10), x(20, 10)); err != nil {
		t.Errorf("a replay of the lock: %v, want it answered", err)
	}
	f.mustLock(t, b, file, LockReq{Owner: smbOwner(b, ob), Open: byID(ob), Ranges: []LockRange{x(100, 10)}})
	if err := lockAs(f, 2, x(100, 10)); lockedBy(err) == "" {
		t.Fatalf("a conflicting lock: %v", err)
	}
	if _, err := f.Locks().Unlock(ctx, b, file, byID(ob), smbOwner(b, ob), []ByteRange{br(100, 10)}, Seqs{}); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(f, 2, x(100, 10)); err != nil {
		t.Fatalf("the retry after a refusal: %v", err)
	}
	if _, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{sh(200, 1)}, Replay: &LockSequence{Index: 0, Seq: 1}}); err != nil {
		t.Errorf("a lock naming no index: %v", err)
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err) // a later write of the open's record keeps its lock sequences
	}
	if h := f.holder(t, file, br(100, 10)); h == nil || h.Owner.Client != a.Client {
		t.Errorf("the retry after a refusal holds %+v, want it applied", h)
	}

	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if err := lockAs(g, 2, x(100, 10)); err != nil {
		t.Errorf("a replay after a failover: %v, want it answered", err)
	}
	if got := g.storedLocks(t, file, o); len(got) != 4 {
		t.Errorf("stored %+v, want each lock once", got)
	}
}

// A create naming an app instance another client's open of the file holds
// closes that open first, a persistent one installed by a restart included,
// unless the caller may not read the file, the open is the caller's own
// client's, or both are versioned and the create's version is not higher:
// then the create meets the open's deny mode as any other, or, for the
// version, is refused.
func TestAppInstanceReplacesItsPredecessor(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Deny = rd|wr, wr
	as := func(who Caller, version uint64, read bool) OpenRequest {
		q := keep(r, entity.DurabilityPersistent, s1, time.Hour, byte(who.Client))
		q.Durable.Instance, q.MaxRead = AppInstance{ID: [16]byte{9}, Version: [2]uint64{1, version}}, read
		return q
	}
	oa := f.mustOpen(t, a, file, as(a, 5, true))
	for what, q := range map[string]struct {
		who Caller
		r   OpenRequest
	}{
		"a caller that may not read":   {b, as(b, 6, false)},
		"the open's own client":        {a, as(a, 6, true)},
		"a version not above the open": {b, as(b, 5, true)},
	} {
		if _, err := f.open(q.who, file, q.r); !errors.Is(err, ErrShareViolation) {
			t.Errorf("%s: %v, want ErrShareViolation", what, err)
		}
	}
	if n := len(f.entryOf(file.ID).opens); n != 1 {
		t.Fatalf("%d opens, want only the first", n)
	}

	g := f.restart(t)
	g.renew(t, b)
	g.idle(leasePeriod) // past grace, inside the installed open's Timeout
	ob := g.mustOpen(t, b, file, as(b, 6, true))
	if got := g.stored(t, file); len(got) != 1 || got[0].ID != ob.ID {
		t.Errorf("stored %+v, want only the open that replaced the installed one", got)
	}
	if _, err := g.reconnect(a, oa, ReconnectMatch{Share: file.Share, Create: [16]byte{byte(a.Client)}}, s2); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reconnect of the replaced open: %v, want ErrNoReclaim", err)
	}
}

// A request through an open is judged by its channel sequence: an older one
// is stale, a newer one raises the open's, written before it runs for a
// persistent open so a failover keeps it, and a replay is stale while a
// request under an earlier sequence is in flight.
func TestChannelSequence(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smbMachine("pc-a"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Channel = rd|wr, 5
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
	ch := func(g *fixture, seq uint16, replay bool) (bool, func()) {
		t.Helper()
		stale, done, err := g.Opens().Channel(ctx, a, o.ID, seq, replay)
		if err != nil {
			t.Fatalf("Channel %d: %v", seq, err)
		}
		return stale, done
	}
	if stale, _ := ch(f, 4, false); !stale {
		t.Error("a request on a sequence older than the create's is not stale")
	}
	stale, first := ch(f, 5, false)
	if stale {
		t.Fatal("a request on the open's own sequence is stale")
	}
	if stale, _ := ch(f, 6, false); stale {
		t.Error("a request on a newer sequence is stale")
	}
	if stale, _ := ch(f, 6, true); !stale {
		t.Error("a replay while a request under an earlier sequence is in flight is not stale")
	}
	first()
	if stale, _ := ch(f, 6, true); stale {
		t.Error("a replay once the earlier request left is stale")
	}
	if stale, _ := ch(f, 6+0x8000, false); !stale {
		t.Error("a sequence half the space away is not older")
	}
	if err := f.Opens().Update(ctx, a, o.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err) // a later write of the open's record keeps its sequence
	}

	g := f.restart(t)
	if _, err := g.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}}, s2); err != nil {
		t.Fatal(err)
	}
	if stale, _ := ch(g, 5, false); !stale {
		t.Error("after a failover, the sequence a request raised is lost")
	}
}
