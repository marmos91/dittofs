package vfs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
)

// smb is an SMB machine's client: its ClientGuid, under one Boot.
func smb(guid string, boot byte) vfs.ClientInfo {
	return vfs.ClientInfo{Protocol: entity.ProtocolSMB, Name: []byte(guid), Boot: [8]byte{boot}}
}

// calls waits up to a second for cb to have seen n callbacks, and answers
// what it saw.
func calls(t *testing.T, cb *vfstest.RecordingCallbacks, n int) []vfstest.Callback {
	t.Helper()
	for range 100 {
		if got := cb.Calls(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("saw %d callbacks, want %d", len(cb.Calls()), n)
	return nil
}

// none fails if cb saw any callback within 50ms.
func none(t *testing.T, who string, cb *vfstest.RecordingCallbacks) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := cb.Calls(); len(got) != 0 {
		t.Errorf("%s saw %v, want nothing", who, got)
	}
}

// signedIn is a call from SMB session id, signed in as p.
func signedIn(p entity.Principal, id uint64) vfs.Call {
	c := proven(p, inLab)
	c.Protocol, c.Session = entity.ProtocolSMB, vfs.SessionRef{ID: id}
	return c
}

// beginShard begins shard 0 of the share in open state, where the tests' opens
// are held.
func beginShard(t *testing.T, s *shares) {
	t.Helper()
	s.must(t, s.tab.Recovery().ServeShard(t.Context(), s.share, 0, 1))
}

// leased opens file for p on session sess of client cl, under lease key k
// with read and handle caching, durable so a disconnect keeps it, and
// answers the lease.
func leased(t *testing.T, s *shares, cl vfs.ClientID, p entity.Principal, sess uint64, file entity.FileID, k byte) openstate.CachingGrant {
	t.Helper()
	who := openstate.Caller{Client: cl, Principal: p, Protocol: entity.ProtocolSMB}
	res, err := s.tab.Opens().Open(t.Context(), who, openstate.FileKey{Share: s.share, ID: file}, openstate.OpenRequest{
		Access: entity.ReadData, Lease: openstate.LeaseKey{k}, Want: openstate.GrantRead | openstate.GrantHandle,
		Session: vfs.SessionRef{ID: sess}, Durability: entity.DurabilityDurable, Durable: openstate.DurableKeys{Timeout: time.Minute},
	})
	if err != nil || res.Grant == nil {
		t.Fatalf("the leased open answered %+v, %v", res.Grant, err)
	}
	return *res.Grant
}

// breaks is the grants cb saw recalled.
func breaks(cb *vfstest.RecordingCallbacks) []vfs.GrantID {
	var gs []vfs.GrantID
	for _, c := range cb.Calls() {
		if c.Method == "RecallGrant" {
			gs = append(gs, c.Args[0].(vfs.CachingGrant).ID)
		}
	}
	return gs
}

func clientInUse(err error) bool {
	var se *vfs.StateError
	return errors.As(err, &se) && se.Kind == vfs.StateClientInUse
}

func notYours(err error) bool {
	var se *vfs.StateError
	return errors.As(err, &se) && se.Kind == vfs.StateNotYours
}

// V36: mallory presents alice-pc's ClientGuid and signs in as herself, the
// latest to connect under it. build01's conflicting open breaks alice's
// lease: the break reaches alice's session, holding the lease's open, and
// none of mallory's, and mallory's answer to it is NotYours while alice's is
// taken.
func TestAnSMBBreakGoesToTheSessionHoldingTheLeasesOpen(t *testing.T) {
	s := newShares(t, nil, nil)
	beginShard(t, s)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	alicePC := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), alicePC)
	if err != nil {
		t.Fatal(err)
	}
	thief := &vfstest.RecordingCallbacks{}
	if again, err := s.Connect(ctx, signedIn(mallory.Principal, 2), smb("alice-pc", 1), thief); err != nil || again != id {
		t.Fatalf("mallory's Connect under alice-pc's ClientGuid answered %v, %v; want alice-pc's %v", again, err, id)
	}
	file := entity.NewFileID()
	g := leased(t, s, id, alice.Principal, 1, file, 1)

	build01, err := s.Connect(ctx, signedIn(alice.Principal, 3), smb("build01", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	var delay *openstate.DelayError
	if _, err := s.tab.Opens().Open(ctx, openstate.Caller{Client: build01, Principal: alice.Principal, Protocol: entity.ProtocolSMB},
		openstate.FileKey{Share: s.share, ID: file}, openstate.OpenRequest{Access: entity.WriteData}); !errors.As(err, &delay) {
		t.Fatalf("build01's write open answered %v, want a delay behind the break", err)
	}
	calls(t, alicePC, 1)
	if got := breaks(alicePC); len(got) != 1 || got[0] != g.ID {
		t.Errorf("alice's session saw breaks %v, want the one of her lease", got)
	}
	none(t, "mallory", thief)

	asMallory, asAlice := signedIn(mallory.Principal, 2), signedIn(alice.Principal, 1)
	asMallory.Client, asAlice.Client = id, id
	if err := s.AckRecall(ctx, asMallory, g.ID, openstate.GrantNone); !notYours(err) {
		t.Errorf("mallory's answer to alice's break: %v, want NotYours", err)
	}
	if err := s.AckRecall(ctx, asAlice, g.ID, openstate.GrantNone); err != nil {
		t.Errorf("alice's answer to her break: %v", err)
	}
}

// A break goes to the session holding the lease's open before any other
// session of the client signed in as its opener: every one of ten does.
func TestABreakPrefersTheSessionHoldingTheOpen(t *testing.T) {
	s := newShares(t, nil, nil)
	beginShard(t, s)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	holding, other := &vfstest.RecordingCallbacks{}, &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), holding)
	if err != nil {
		t.Fatal(err)
	}
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	if _, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 1), other); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		s.RecallGrant(g)
	}
	calls(t, holding, 10)
	none(t, "alice's session holding no open of the lease", other)
}

// A session's disconnect ends the client's latest path when that session
// registered it: a callback raised after goes nowhere.
func TestADisconnectEndsTheLatestPathItRegistered(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	cb := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Disconnect(ctx, signedIn(alice.Principal, 1), id, vfs.SessionRef{ID: 1}); err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 0)
	s.LockWaitOver(openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}, false)
	none(t, "the disconnected session", cb)
}

// A destroyed client's path is dropped: a callback raised after goes
// nowhere.
func TestADestroyedClientsPathIsDropped(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(ctx, claimed(1000, inLab), id); err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 0)
	none(t, "the destroyed client", cb)
}

// With every open of the lease disconnected, its break goes only to a
// session of the client signed in as an opener; with none, it goes nowhere,
// not to the client's latest session.
func TestAnSMBBreakOfDisconnectedOpensGoesOnlyToAnOpenersSession(t *testing.T) {
	s := newShares(t, nil, nil)
	beginShard(t, s)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	first, second, thief := &vfstest.RecordingCallbacks{}, &vfstest.RecordingCallbacks{}, &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), first)
	if err != nil {
		t.Fatal(err)
	}
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	if _, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 1), second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connect(ctx, signedIn(mallory.Principal, 3), smb("alice-pc", 1), thief); err != nil {
		t.Fatal(err)
	}
	if err := s.Disconnect(ctx, signedIn(alice.Principal, 1), id, vfs.SessionRef{ID: 1}); err != nil {
		t.Fatal(err)
	}
	s.RecallGrant(g)
	calls(t, second, 1)
	none(t, "the disconnected session", first)
	none(t, "mallory", thief)

	if err := s.Disconnect(ctx, signedIn(alice.Principal, 2), id, vfs.SessionRef{ID: 2}); err != nil {
		t.Fatal(err)
	}
	s.RecallGrant(g)
	time.Sleep(50 * time.Millisecond)
	if n := len(second.Calls()); n != 1 {
		t.Errorf("alice's ended session saw %d breaks, want only the first", n)
	}
	none(t, "mallory, with no opener's session left", thief)
}

// A null or guest session, or a call naming no session, never joins an SMB
// client: its Connect is ErrInvalid and registers no path.
func TestAGuestOrNullSessionNeverJoinsAClient(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	guest := signedIn(entity.Principal{}, 1)
	guest.Identity = vfs.Identity{Proof: vfs.ProofNone, Client: inLab}
	cb := &vfstest.RecordingCallbacks{}
	for name, c := range map[string]vfs.Call{"guest": guest, "sessionless": proven(alice.Principal, inLab)} {
		if _, err := s.Connect(t.Context(), c, smb("alice-pc", 1), cb); !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("a %s Connect answered %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), nil); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a Connect with no callbacks answered %v, want ErrInvalid", err)
	}
	if n := len(s.reg.byID) + len(s.reg.smb); n != 0 {
		t.Errorf("refused Connects registered %d paths", n)
	}
}

// A delegation's recall goes by its client's path.
func TestADelegationsRecallGoesByItsClientsPath(t *testing.T) {
	s := newShares(t, nil, nil)
	beginShard(t, s)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.tab.Opens().Open(ctx, openstate.Caller{Client: id, Protocol: entity.ProtocolNFSv4},
		openstate.FileKey{Share: s.share, ID: entity.NewFileID()}, openstate.OpenRequest{Access: entity.ReadData, Want: openstate.GrantRead})
	if err != nil || res.Grant == nil {
		t.Fatalf("the delegated open answered %+v, %v", res.Grant, err)
	}
	s.RecallGrant(*res.Grant)
	calls(t, cb, 1)
	if got := breaks(cb); len(got) != 1 || got[0] != res.Grant.ID {
		t.Errorf("the client saw %v, want the delegation's recall", cb.Calls())
	}
}

// A client that rebooted is told its old ID ended, through the path it
// registered, and that path no longer receives the old ID's events.
func TestARebootTellsTheEndedClientAndDropsItsPath(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	old := &vfstest.RecordingCallbacks{}
	oldID, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), old)
	if err != nil {
		t.Fatal(err)
	}
	fresh := &vfstest.RecordingCallbacks{}
	newID, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 2), fresh)
	if err != nil || newID == oldID {
		t.Fatalf("the reboot's Connect answered %v, %v", newID, err)
	}
	got := calls(t, old, 1)
	if r, ok := got[0].Args[0].(vfs.Revocation); got[0].Method != "Revoked" || !ok || r.Client != oldID || r.Reason != vfs.RevokedLeaseExpired {
		t.Fatalf("the rebooted client saw %v, want its ID revoked", got)
	}
	s.RecallAny(oldID, 0)
	time.Sleep(50 * time.Millisecond)
	if n := len(old.Calls()); n != 1 {
		t.Errorf("the ended ID's path saw %d callbacks, want only its revocation", n)
	}
	none(t, "the new ID's path", fresh)
}

// A client whose lease expired is told so through its path, which then
// receives nothing more.
func TestALeaseExpiryTellsTheClientAndDropsItsPath(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
	if err != nil {
		t.Fatal(err)
	}
	for range 11 { // past the 90 s lease, one sweep at a time
		for s.clock.Pending() == 0 {
			time.Sleep(time.Millisecond)
		}
		s.clock.Advance(10 * time.Second)
	}
	got := calls(t, cb, 1)
	if r, ok := got[0].Args[0].(vfs.Revocation); !ok || r.Client != id || r.Reason != vfs.RevokedLeaseExpired {
		t.Fatalf("the expired client saw %v, want its lease's expiry", got)
	}
	s.RecallAny(id, 0)
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 1 {
		t.Errorf("the expired client's path saw %d callbacks, want only its expiry", n)
	}
}

// An administrator's revoke, raised by open state, reaches the client.
func TestARevokeReachesTheClient(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{}
	id, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.tab.Clients().Revoke(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	got := calls(t, cb, 1)
	if r, ok := got[0].Args[0].(vfs.Revocation); !ok || r.Client != id || r.Reason != vfs.RevokedByAdmin {
		t.Errorf("saw %v, want the administrator's revoke of %v", got, id)
	}
}

// An NFSv4 client's Owner is the principal its caller maps to, so another
// principal naming it is refused ClientInUse.
func TestAnNFSv4ClientIsOwnedByItsMappedPrincipal(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	s.user(t, "mallory", "1001", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("linux-host"), Boot: [8]byte{1}, Protect: true}

	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	id, err := s.Connect(ctx, claimed(1000, inLab), info, &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Record(ctx, claimed(1000, inLab), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Owner != alice.Principal || !rec.Protect {
		t.Errorf("the record's Owner is %v (protect %v), want alice's principal, protected", rec.Owner, rec.Protect)
	}
	info.Proposal = nil
	if _, err := s.Propose(ctx, claimed(1001, outside), info); !clientInUse(err) {
		t.Errorf("mallory's Propose of alice's name answered %v, want ClientInUse", err)
	}
}

// A client's Adapter bytes come back from Record as Propose and Connect gave
// them, so the NFSv4 adapter reads the minor version that confirmed the
// client from its record, and a confirm under the other one replaces them.
func TestAClientsAdapterBytesComeBackFromItsRecord(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	for _, minor := range []byte{0, 1} {
		info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("linux-host"), Boot: [8]byte{1 + minor}, Adapter: []byte{1, minor}}
		p, err := s.Propose(ctx, claimed(1000, inLab), info)
		if err != nil {
			t.Fatal(err)
		}
		info.Proposal = &p
		id, err := s.Connect(ctx, claimed(1000, inLab), info, &vfstest.RecordingCallbacks{})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := s.Record(ctx, claimed(1000, inLab), id)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rec.Adapter, info.Adapter) {
			t.Errorf("a client confirmed under NFSv4.%d has Adapter %v, want %v", minor, rec.Adapter, info.Adapter)
		}
	}
}

// A Connect open state refuses registers no path.
func TestNoPathWithoutAnAdmittedConnect(t *testing.T) {
	s := newShares(t, nil, nil)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{}
	if _, err := s.Connect(t.Context(), signedIn(mallory.Principal, 1), vfs.ClientInfo{Protocol: entity.ProtocolSMB}, cb); !errors.Is(err, vfs.ErrInvalid) {
		t.Fatalf("a Connect naming no client answered %v, want ErrInvalid", err)
	}
	if n := len(s.reg.byID) + len(s.reg.smb); n != 0 {
		t.Errorf("a refused Connect registered %d paths", n)
	}
}

// A lock grant the client gives no answer to is sent again; one it answers
// is not, and a wait that ended ungranted is sent once whatever the answer.
func TestALockGrantUnansweredIsSentAgain(t *testing.T) {
	defer func(d time.Duration) { grantRetry = d }(grantRetry)
	grantRetry = time.Millisecond
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	var silent atomic.Int32
	silent.Store(2)
	cb := &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error {
		if silent.Add(-1) >= 0 {
			return context.DeadlineExceeded
		}
		return nil
	}}
	id, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	w := openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}
	s.LockWaitOver(w, true)
	calls(t, cb, 3)
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 3 {
		t.Errorf("a grant unanswered twice, then answered, was sent %d times, want 3", n)
	}

	silent.Store(1)
	s.LockWaitOver(w, false)
	calls(t, cb, 4)
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 4 {
		t.Errorf("an ungranted wait's end was sent %d times, want once", n-3)
	}
}

// Stop ends a callback still being delivered, and returns once it has
// returned; a callback raised after Stop is not delivered.
func TestStopEndsTheCallbacksBeingDelivered(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice"}
	if err := f.fs.Store.ControlPlane().PutUser(t.Context(), alice); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	end := sync.OnceFunc(func() { close(ended) })
	var returned atomic.Bool
	cb := &vfstest.RecordingCallbacks{Answer: func(ctx context.Context, _ vfstest.Callback) error {
		<-ctx.Done()
		end()
		time.Sleep(20 * time.Millisecond)
		returned.Store(true)
		return ctx.Err()
	}}
	id, err := f.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	f.RecallAny(id, 0)
	calls(t, cb, 1)
	if err := f.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error)
	go func() { stopped <- f.Stop() }()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("Stop did not end the callback being delivered")
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Error("Stop returned before the callback it ended had returned")
	}
	f.RecallAny(id, 0)
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 1 {
		t.Errorf("a callback raised after Stop was delivered: %d calls, want 1", n)
	}
}

// Every error open state names maps to one of the service's own, never to
// the ErrUnavailable an unknown failure is, but open state stopped, which is
// the draining filesystem's ErrUnavailable itself.
func TestEveryOpenStateErrorIsMapped(t *testing.T) {
	for _, err := range []error{
		openstate.ErrBadState, openstate.ErrStaleState, openstate.ErrNotYours, openstate.ErrOldState,
		openstate.ErrStaleClient, openstate.ErrClientInUse, openstate.ErrClientBusy, openstate.ErrNoRecall,
		openstate.ErrInvalid, openstate.ErrGrace, openstate.ErrNoReclaim, openstate.ErrReclaimConflict,
		openstate.ErrShareViolation, openstate.ErrDeletePending, openstate.ErrAccess, openstate.ErrLocked,
		openstate.ErrRangeNotLocked, openstate.ErrBadSeqID, openstate.ErrLocksHeld, openstate.ErrCreateInUse,
		openstate.ErrRevoked, openstate.ErrInstanceVersion, openstate.ErrRecordChanged, openstate.ErrLost,
		&openstate.DelayError{Cause: openstate.DelayRecall}, &openstate.DelayError{Cause: openstate.DelayResources},
		&openstate.DelayError{Cause: openstate.DelayFrozen}, &openstate.DelayError{Cause: openstate.DelayCommit},
		&openstate.DelayError{Cause: openstate.DelayConflict},
		&openstate.LockedError{}, &openstate.RevokedError{Cause: openstate.RevokedGrant},
	} {
		got := openErr(err)
		var d *vfs.Delay
		if errors.Is(got, vfs.ErrUnavailable) || errors.As(got, &d) && d.Cause == 0 || !neutral(got) {
			t.Errorf("%v maps to %v", err, got)
		}
	}
	if got := openErr(openstate.ErrStopped); got != vfs.ErrUnavailable {
		t.Errorf("open state stopped maps to %v, want the draining answer, ErrUnavailable itself", got)
	}
	var se *vfs.StateError
	if got := openErr(&openstate.RevokedError{Cause: openstate.RevokedByAdmin}); !errors.As(got, &se) || se.Kind != vfs.StateRevoked || !se.Admin {
		t.Errorf("an administrator's revoke maps to %v", got)
	}
}
