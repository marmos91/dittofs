package vfs

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// smb is an SMB machine's client: its ClientGuid, under one Boot.
func smb(guid string, boot byte) vfs.ClientInfo {
	return vfs.ClientInfo{Protocol: entity.ProtocolSMB, Name: []byte(guid), Boot: [8]byte{boot}, Recovery: entity.RecoverReconnect}
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

// fastRetry makes grantRetry a millisecond until the test's cleanups, those
// registered after it included, have run: a filesystem's Stop then waits for
// every delivery reading it.
func fastRetry(t *testing.T) {
	d := grantRetry
	t.Cleanup(func() { grantRetry = d })
	grantRetry = time.Millisecond
}

// signedIn is a call from SMB session id, signed in as p.
func signedIn(p entity.Principal, id uint64) vfs.Call {
	c := proven(p, inLab)
	c.Protocol, c.Session = entity.ProtocolSMB, vfs.SessionRef{ID: id}
	return c
}

// disconnect ends session sess of client id's last connection: its durable
// opens are kept, disconnected.
func disconnect(t *testing.T, s *shares, id vfs.ClientID, sess uint64) {
	t.Helper()
	if _, err := s.Disconnect(t.Context(), vfs.Call{}, id, vfs.SessionRef{ID: sess}, vfs.SessionEnd{}); err != nil {
		t.Fatal(err)
	}
}

// sentOn waits up to a second for cb to have sent n breaks, and answers
// the session each went on.
func sentOn(t *testing.T, cb *vfstest.RecordingCallbacks, n int) []vfs.SessionRef {
	t.Helper()
	for range 100 {
		if got := breakSessions(cb); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sent %d breaks, want %d", len(breakSessions(cb)), n)
	return nil
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
			gs = append(gs, c.Args[2].(vfs.CachingGrant).ID)
		}
	}
	return gs
}

// breakSessions is the session each break cb sent went on, in order: the
// one the filesystem named, or the one cb picked; a break sent nowhere is
// left out.
func breakSessions(cb *vfstest.RecordingCallbacks) []vfs.SessionRef {
	var ss []vfs.SessionRef
	for _, c := range cb.Calls() {
		if c.Method == "RecallGrant" && c.Session != (vfs.SessionRef{}) {
			ss = append(ss, c.Session)
		}
	}
	return ss
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
// latest to connect under it, registering the client's Callbacks. build01's
// conflicting open breaks alice's lease: the break names alice's session,
// holding the lease's open, never mallory's, and mallory's answer to it is
// NotYours while alice's is taken.
func TestAnSMBBreakGoesToTheSessionHoldingTheLeasesOpen(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	alicePC := &vfstest.RecordingCallbacks{}
	if again, _, err := s.Connect(ctx, signedIn(mallory.Principal, 2), smb("alice-pc", 1), alicePC); err != nil || again != id {
		t.Fatalf("mallory's Connect under alice-pc's ClientGuid answered %v, %v; want alice-pc's %v", again, err, id)
	}
	alicePC.SignIn(vfs.SessionRef{ID: 2}, mallory.Principal)
	file := entity.NewFileID()
	g := leased(t, s, id, alice.Principal, 1, file, 1)

	build01, _, err := s.Connect(ctx, signedIn(alice.Principal, 3), smb("build01", 1), &vfstest.RecordingCallbacks{})
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
		t.Errorf("alice-pc saw breaks %v, want the one of her lease", got)
	}
	if got := breakSessions(alicePC); len(got) != 1 || got[0] != (vfs.SessionRef{ID: 1}) {
		t.Errorf("the break went to sessions %v, want alice's, 1", got)
	}

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
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	pc := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	pc.SignIn(vfs.SessionRef{ID: 2}, alice.Principal)
	pc.SignIn(vfs.SessionRef{ID: 1}, alice.Principal)
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	for range 10 {
		s.RecallGrant(g)
	}
	calls(t, pc, 10)
	for _, sess := range breakSessions(pc) {
		if sess != (vfs.SessionRef{ID: 1}) {
			t.Errorf("a break went to session %v, want the one holding the open, 1", sess)
		}
	}
}

// The Disconnect of the node's last session of an SMB client still holding
// a disconnected durable open keeps the client and its Callbacks; one
// holding nothing is released in open state, and its Callbacks go with it.
func TestLastDisconnectDropsOnlyAReleasedClientsCallbacks(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx, as := t.Context(), signedIn(alice.Principal, 1)
	pc := &vfstest.RecordingCallbacks{}
	id, gen, err := s.Connect(ctx, as, smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	if _, err := s.Disconnect(ctx, as, id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen}); err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 0)
	if got := calls(t, pc, 1); got[0].Method != "RecallAny" {
		t.Errorf("a client holding a durable open saw %v after its last Disconnect, want RecallAny", got)
	}

	idle, gen, err := s.Connect(ctx, as, smb("idle-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, as, idle, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, as, idle); !stateKind(err, vfs.StateStaleClient) {
		t.Errorf("an idle client after its last Disconnect: %v, want StaleClient", err)
	}
	if _, ok := s.reg.cb[idle]; ok {
		t.Error("a released client keeps its Callbacks")
	}

	// A logoff closes the session's durable open in the same call, so the
	// client is left holding nothing and is released.
	off, gen, err := s.Connect(ctx, as, smb("off-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	leased(t, s, off, alice.Principal, 1, entity.NewFileID(), 2)
	if _, err := s.Disconnect(ctx, as, off, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Logoff: true, Last: true, Gen: gen}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, as, off); !stateKind(err, vfs.StateStaleClient) {
		t.Errorf("a client whose durable open's session logged off: %v, want it released, StaleClient", err)
	}
}

// A last Disconnect at a Gen a second Connect raised answers that newer Gen
// and keeps the client, its Callbacks included; one at that Gen answers zero
// and releases it.
func TestALastDisconnectAtAStaleGenAnswersTheNewer(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx, as := t.Context(), signedIn(alice.Principal, 1)
	id, first, err := s.Connect(ctx, as, smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if gen, err := s.Disconnect(ctx, as, id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: first}); err != nil || gen != second {
		t.Fatalf("a last Disconnect at the stale Gen %d: Gen %d, %v; want %d", first, gen, err, second)
	}
	if _, ok := s.reg.cb[id]; !ok {
		t.Error("a client still listed lost its Callbacks")
	}
	if gen, err := s.Disconnect(ctx, as, id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: second}); err != nil || gen != 0 {
		t.Fatalf("a last Disconnect at the current Gen: Gen %d, %v; want zero", gen, err)
	}
	if _, ok := s.reg.cb[id]; ok {
		t.Error("a released client keeps its Callbacks")
	}
}

// A re-list's Connect landing after the last session's Disconnect removed
// the node's entry lists it again at a Gen above the removed one; the
// adapter, holding no session on that Connect's return, sends Disconnect
// with Last and that Gen, which unlists the node.
func TestARelistAfterTheLastDisconnectIsUnlistedByItsOwnGen(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx, as := t.Context(), signedIn(alice.Principal, 1)
	id, first, err := s.Connect(ctx, as, smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	// A durable open keeps the record once the node is unlisted, so the
	// re-list finds it.
	leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	if gen, err := s.Disconnect(ctx, as, id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: first}); err != nil || gen != 0 {
		t.Fatalf("the last Disconnect: Gen %d, %v; want zero", gen, err)
	}
	again, gen, err := s.Connect(ctx, as, smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil || again != id || gen <= first {
		t.Fatalf("the re-list: client %d Gen %d, %v; want client %d at a Gen above %d", again, gen, err, id, first)
	}
	if rec, err := s.Record(ctx, as, id); err != nil || len(rec.Reach.Sessions) != 1 {
		t.Fatalf("after the re-list: sessions %v, %v; want this node listed", rec.Reach.Sessions, err)
	}
	if left, err := s.Disconnect(ctx, as, id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen}); err != nil || left != 0 {
		t.Fatalf("the Disconnect at the re-list's Gen: Gen %d, %v; want zero", left, err)
	}
	if rec, err := s.Record(ctx, as, id); err != nil || len(rec.Reach.Sessions) != 0 {
		t.Errorf("after the Disconnect at the re-list's Gen: sessions %v, %v; want the node unlisted", rec.Reach.Sessions, err)
	}
}

// A destroyed client's path is dropped: a callback raised after goes
// nowhere.
func TestADestroyedClientsPathIsDropped(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(ctx, claimed(1000, inLab), id); err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 0)
	none(t, "the destroyed client", cb)
}

// With every open of the lease disconnected, its break names no session but
// the openers' principals, and the adapter sends it on one of its sessions
// of the client signed in as an opener, not on another principal's.
func TestAnSMBBreakOfDisconnectedOpensGoesOnlyToAnOpenersSession(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	pc := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	pc.SignIn(vfs.SessionRef{ID: 2}, mallory.Principal)
	pc.SignIn(vfs.SessionRef{ID: 3}, alice.Principal)
	disconnect(t, s, id, 1)
	s.RecallGrant(g)
	got := calls(t, pc, 1)
	if got[0].Args[0] != (vfs.SessionRef{}) || !reflect.DeepEqual(got[0].Args[1], []vfs.Principal{alice.Principal}) {
		t.Errorf("the break named session %v and openers %v, want none and alice", got[0].Args[0], got[0].Args[1])
	}
	if got[0].Session != (vfs.SessionRef{ID: 3}) {
		t.Errorf("the break went on session %v, want alice's, 3", got[0].Session)
	}
}

// A session re-authenticated as guest is no longer one a disconnected
// open's break goes to: the adapter's table says what it is signed in as
// now, and with no other session of the opener the break is sent nowhere.
func TestABreakSkipsASessionReauthenticatedAsGuest(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	pc := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	pc.SignIn(vfs.SessionRef{ID: 2}, alice.Principal)
	disconnect(t, s, id, 1)
	pc.SignIn(vfs.SessionRef{ID: 2}, vfs.Principal{})
	s.RecallGrant(g)
	calls(t, pc, 1)
	time.Sleep(50 * time.Millisecond)
	if got := breakSessions(pc); len(got) != 0 {
		t.Errorf("the break went on sessions %v, want none", got)
	}
}

// A break whose send to the session holding the open fails, as that
// session ended meanwhile, is sent again once the open is disconnected, on
// the session the adapter then picks, through the same Callbacks.
func TestAFailedBreakGoesToTheNextSession(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	pc := &vfstest.RecordingCallbacks{Answer: func(_ context.Context, c vfstest.Callback) error {
		if c.Args[0] == (vfs.SessionRef{ID: 1}) {
			return errors.New("session gone")
		}
		return nil
	}}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	pc.SignIn(vfs.SessionRef{ID: 2}, alice.Principal)
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	s.RecallGrant(g)
	calls(t, pc, 1)
	disconnect(t, s, id, 1)
	sentOn(t, pc, 2)
	if got := breakSessions(pc); len(got) != 2 || got[0] != (vfs.SessionRef{ID: 1}) || got[1] != (vfs.SessionRef{ID: 2}) {
		t.Errorf("the break went to sessions %v, want 1, then 2 once 1 ended", got)
	}
}

// A null or guest session never connects an SMB client: its Connect is
// ErrInvalid and registers no path.
func TestAGuestOrNullSessionNeverConnectsAClient(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	guest := signedIn(entity.Principal{}, 1)
	guest.Identity = vfs.Identity{Proof: vfs.ProofNone, Client: inLab}
	cb := &vfstest.RecordingCallbacks{}
	if _, _, err := s.Connect(t.Context(), guest, smb("alice-pc", 1), cb); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a guest Connect answered %v, want ErrInvalid", err)
	}
	if _, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), nil); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a Connect with no callbacks answered %v, want ErrInvalid", err)
	}
	if n := len(s.reg.cb); n != 0 {
		t.Errorf("refused Connects registered %d paths", n)
	}
}

// A Connect naming no recovery kind is ErrInvalid: the kind sticks for the
// record's life, and a client of none is never told of a lost shard.
func TestAConnectWithNoRecoveryKindIsRefused(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	info := smb("alice-pc", 1)
	info.Recovery = 0
	if _, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), info, &vfstest.RecordingCallbacks{}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a Connect with no recovery kind answered %v, want ErrInvalid", err)
	}
}

// A delegation's recall goes by its client's path.
func TestADelegationsRecallGoesByItsClientsPath(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
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

// laptop proposes and connects NFSv4 client "laptop" as uid 1000, with cb as
// its path, and answers its ID.
func laptop(t *testing.T, s *shares, cb vfs.Callbacks) vfs.ClientID {
	t.Helper()
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	p, err := s.Propose(t.Context(), claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	id, _, err := s.Connect(t.Context(), claimed(1000, inLab), info, cb)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// delegate opens a fresh file for client cl with a read delegation, and
// answers the delegation.
func delegate(t *testing.T, s *shares, cl vfs.ClientID) openstate.CachingGrant {
	t.Helper()
	res, err := s.tab.Opens().Open(t.Context(), openstate.Caller{Client: cl, Protocol: entity.ProtocolNFSv4},
		openstate.FileKey{Share: s.share, ID: entity.NewFileID()}, openstate.OpenRequest{Access: entity.ReadData, Want: openstate.GrantRead})
	if err != nil || res.Grant == nil {
		t.Fatalf("the delegated open answered %+v, %v", res.Grant, err)
	}
	return *res.Grant
}

// A recall whose send fails is sent again through the path the client
// registers next, and each failure is counted.
func TestAFailedRecallGoesThroughTheClientsNextPath(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	lost := &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error { return errors.New("connection reset") }}
	id := laptop(t, s, lost)
	g := delegate(t, s, id)
	s.RecallGrant(g)
	calls(t, lost, 1)
	fresh := &vfstest.RecordingCallbacks{}
	if again := laptop(t, s, fresh); again != id {
		t.Fatalf("the reconnect answered client %v, want %v", again, id)
	}
	calls(t, fresh, 1)
	if got := breaks(fresh); len(got) != 1 || got[0] != g.ID {
		t.Errorf("the client's new path saw %v, want the recall", fresh.Calls())
	}
	if n := len(lost.Calls()); n != 1 {
		t.Errorf("the failed path was sent the recall %d times, want once", n)
	}
	if n := testutil.ToFloat64(s.callbackErrors.WithLabelValues("RecallGrant")); n != 1 {
		t.Errorf("vfs_callback_errors_total{kind=RecallGrant} is %v, want 1", n)
	}
}

// nowhere is Callbacks whose adapter sends every recall nowhere, finding
// no session to send it on.
type nowhere struct{ *vfstest.RecordingCallbacks }

func (n nowhere) RecallGrant(ctx context.Context, s vfs.SessionRef, openers []vfs.Principal, g vfs.CachingGrant, to vfs.GrantKind) (bool, error) {
	_, err := n.RecordingCallbacks.RecallGrant(ctx, s, openers, g, to)
	return false, err
}

// A recall sent nowhere at first is counted once, however often it is
// asked again.
func TestARecallSentNowhereAtFirstIsCountedOnce(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{}
	id := laptop(t, s, nowhere{cb})
	s.RecallGrant(delegate(t, s, id))
	calls(t, cb, 3)
	if n := testutil.ToFloat64(s.callbackErrors.WithLabelValues("RecallGrant")); n != 1 {
		t.Errorf("vfs_callback_errors_total{kind=RecallGrant} is %v, want 1", n)
	}
}

// valueCallbacks is Callbacks of a type that is not comparable, as an
// adapter's value type holding a func or a map is.
type valueCallbacks struct {
	*vfstest.RecordingCallbacks
	_ map[string]int
}

// A failed recall finds the client's next path by its registration, so
// Callbacks that are not comparable are resent through as any others.
func TestAFailedRecallFindsTheNextPathOfNonComparableCallbacks(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	lost := valueCallbacks{RecordingCallbacks: &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error { return errors.New("connection reset") }}}
	id := laptop(t, s, lost)
	g := delegate(t, s, id)
	s.RecallGrant(g)
	calls(t, lost.RecordingCallbacks, 1)
	fresh := valueCallbacks{RecordingCallbacks: &vfstest.RecordingCallbacks{}}
	laptop(t, s, fresh)
	calls(t, fresh.RecordingCallbacks, 1)
	if got := breaks(fresh.RecordingCallbacks); len(got) != 1 || got[0] != g.ID {
		t.Errorf("the client's new path saw %v, want the recall", fresh.Calls())
	}
}

// A callback with no path to go by is counted.
func TestACallbackWithNoPathIsCounted(t *testing.T) {
	s := newShares(t, nil, nil)
	s.RecallAny(42, 0)
	if n := testutil.ToFloat64(s.callbackErrors.WithLabelValues("RecallAny")); n != 1 {
		t.Errorf("vfs_callback_errors_total{kind=RecallAny} is %v, want 1", n)
	}
}

// A callback the client fails to answer is counted.
func TestAFailedCallbackIsCounted(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error { return errors.New("connection reset") }}
	id, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 0)
	calls(t, cb, 1)
	time.Sleep(20 * time.Millisecond)
	if n := testutil.ToFloat64(s.callbackErrors.WithLabelValues("RecallAny")); n != 1 {
		t.Errorf("vfs_callback_errors_total{kind=RecallAny} is %v, want 1", n)
	}
}

// A client that rebooted is told its old ID ended, through the path it
// registered, and that path no longer receives the old ID's events.
func TestARebootTellsTheEndedClientAndDropsItsPath(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()

	old := &vfstest.RecordingCallbacks{}
	oldID, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), old)
	if err != nil {
		t.Fatal(err)
	}
	fresh := &vfstest.RecordingCallbacks{}
	newID, _, err := s.Connect(ctx, signedIn(alice.Principal, 2), smb("alice-pc", 2), fresh)
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
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("laptop"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	cb := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, claimed(1000, inLab), info, cb)
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
	id, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
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
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("linux-host"), Boot: [8]byte{1}, Protect: true, Recovery: entity.RecoverStatus}

	p, err := s.Propose(ctx, claimed(1000, inLab), info)
	if err != nil {
		t.Fatal(err)
	}
	info.Proposal = &p
	id, _, err := s.Connect(ctx, claimed(1000, inLab), info, &vfstest.RecordingCallbacks{})
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
		info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("linux-host"), Boot: [8]byte{1 + minor}, Adapter: []byte{1, minor}, Recovery: entity.RecoverStatus}
		p, err := s.Propose(ctx, claimed(1000, inLab), info)
		if err != nil {
			t.Fatal(err)
		}
		info.Proposal = &p
		id, _, err := s.Connect(ctx, claimed(1000, inLab), info, &vfstest.RecordingCallbacks{})
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

// nlmHost is an NLM host's client under NSM state state, its recovery the
// restart notice.
func nlmHost(name string, state uint32) vfs.ClientInfo {
	return vfs.ClientInfo{Protocol: entity.ProtocolNLM, Name: []byte(name), Boot: entity.NSMBoot(state), Recovery: entity.RecoverNotice}
}

// lockAndUnlock locks and unlocks a range of a fresh file as client cl,
// leaving it holding nothing.
func lockAndUnlock(t *testing.T, s *shares, cl vfs.ClientID) {
	t.Helper()
	who := openstate.Caller{Client: cl, Protocol: entity.ProtocolNLM}
	file := openstate.FileKey{Share: s.share, ID: entity.NewFileID()}
	o := openstate.LockOwner{Client: cl, Name: []byte("pid 1")}
	r := openstate.ByteRange{Offset: 0, Length: 1}
	if _, err := s.tab.Locks().Lock(t.Context(), who, file, openstate.LockReq{
		Owner: o, Open: openstate.OpenRef{Kind: openstate.RefAnonymous}, Ranges: []openstate.LockRange{{ByteRange: r, Exclusive: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.tab.Locks().Unlock(t.Context(), who, file, openstate.OpenRef{Kind: openstate.RefAnonymous}, o, []openstate.ByteRange{r}, openstate.Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
}

// A client's record keeps the recovery kind its adapter picked, and an NLM
// host's ends once it holds nothing: its next Connect makes a new client,
// and the old one's path is dropped.
func TestAClientRecordKeepsItsRecoveryKind(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	old := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, claimed(1000, inLab), nlmHost("build01", 1), old)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Record(ctx, claimed(1000, inLab), id); err != nil || rec.Recovery != entity.RecoverNotice {
		t.Fatalf("the host's record answered recovery %v, %v; want the notice", rec.Recovery, err)
	}
	lockAndUnlock(t, s, id)
	again, _, err := s.Connect(ctx, claimed(1000, inLab), nlmHost("build01", 1), &vfstest.RecordingCallbacks{})
	if err != nil || again == id {
		t.Fatalf("the idle host's next Connect answered %v, %v; want a new client", again, err)
	}
	s.RecallAny(id, 0)
	none(t, "the idle host's ended client", old)
	if _, ok := s.reg.cb[id]; ok {
		t.Error("the idle host's ended client keeps its path")
	}
}

// Two Connects of one NLM host registering out of order leave the newer
// client's path: the one that got the idle-dropped client's ID registers
// nothing over it.
func TestAnNLMHostsLateRegisterKeepsItsNewerClient(t *testing.T) {
	r := newRegistry()
	live, stale := &vfstest.RecordingCallbacks{}, &vfstest.RecordingCallbacks{}
	r.register(2, "build01", live)
	r.register(1, "build01", stale)
	if r.latest(2) != live || r.nlm["build01"] != 2 {
		t.Errorf("after a late register of the dropped client, the host maps to %v and its new client's path is %v", r.nlm["build01"], r.latest(2))
	}
}

// A host's restart connects it with the restart notice as its recovery.
func TestAHostRestartConnectsItWithTheNotice(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	if err := s.HostRestarted(ctx, claimed(1000, inLab), []byte("build01"), 2); err != nil {
		t.Fatal(err)
	}
	info := nlmHost("build01", 2)
	info.Recovery = entity.RecoverStatus
	id, _, err := s.Connect(ctx, claimed(1000, inLab), info, &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Record(ctx, claimed(1000, inLab), id); err != nil || rec.Recovery != entity.RecoverNotice {
		t.Errorf("the restarted host's record answered recovery %v, %v; want the notice", rec.Recovery, err)
	}
}

// A Connect open state refuses registers no path.
func TestNoPathWithoutAnAdmittedConnect(t *testing.T) {
	s := newShares(t, nil, nil)
	mallory := s.user(t, "mallory", "", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{}
	if _, _, err := s.Connect(t.Context(), signedIn(mallory.Principal, 1), vfs.ClientInfo{Protocol: entity.ProtocolSMB, Recovery: entity.RecoverReconnect}, cb); !errors.Is(err, vfs.ErrInvalid) {
		t.Fatalf("a Connect naming no client answered %v, want ErrInvalid", err)
	}
	if n := len(s.reg.cb); n != 0 {
		t.Errorf("a refused Connect registered %d paths", n)
	}
}

// A lock grant the client gives no answer to is sent again; one it answers
// is not, and a wait that ended ungranted is sent once whatever the answer.
func TestALockGrantUnansweredIsSentAgain(t *testing.T) {
	fastRetry(t)
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
	id, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
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
	lockErrs := s.callbackErrors.WithLabelValues("LockWaitOver")
	if n := testutil.ToFloat64(lockErrs); n != 2 {
		t.Errorf("vfs_callback_errors_total{kind=LockWaitOver} is %v after two unanswered sends, want 2", n)
	}

	silent.Store(1)
	s.LockWaitOver(w, false)
	calls(t, cb, 4)
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 4 {
		t.Errorf("an ungranted wait's end was sent %d times, want once", n-3)
	}
	if n := testutil.ToFloat64(lockErrs); n != 3 {
		t.Errorf("vfs_callback_errors_total{kind=LockWaitOver} is %v after an unanswered wait's end, want 3", n)
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
	id, _, err := f.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
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

// Every error open state names maps to the service's own error for it, open
// state stopped included, which is the draining filesystem's ErrUnavailable.
func TestEveryOpenStateErrorIsMapped(t *testing.T) {
	state := func(k vfs.StateKind) error { return &vfs.StateError{Kind: k} }
	delay := func(c vfs.DelayCause) error { return &vfs.Delay{Cause: c} }
	for _, c := range []struct{ err, want error }{
		{openstate.ErrBadState, state(vfs.StateBadState)},
		{openstate.ErrStaleState, state(vfs.StateStaleState)},
		{openstate.ErrNotYours, state(vfs.StateNotYours)},
		{openstate.ErrOldState, state(vfs.StateOldState)},
		{openstate.ErrStaleClient, state(vfs.StateStaleClient)},
		{openstate.ErrClientInUse, state(vfs.StateClientInUse)},
		{openstate.ErrClientBusy, state(vfs.StateClientBusy)},
		{openstate.ErrNoRecall, state(vfs.StateNoRecall)},
		{openstate.ErrNoReclaim, state(vfs.StateNoReclaim)},
		{openstate.ErrReclaimConflict, state(vfs.StateReclaimConflict)},
		{openstate.ErrBadSeqID, state(vfs.StateBadSeqID)},
		{openstate.ErrLocksHeld, state(vfs.StateLocksHeld)},
		{openstate.ErrCreateInUse, state(vfs.StateCreateInUse)},
		{openstate.ErrRecordChanged, state(vfs.StateRecordChanged)},
		{openstate.ErrRevoked, state(vfs.StateRevoked)},
		{&openstate.RevokedError{Cause: openstate.RevokedGrant}, state(vfs.StateRevoked)},
		{&openstate.RevokedError{Cause: openstate.RevokedByAdmin}, &vfs.StateError{Kind: vfs.StateRevoked, Admin: true}},
		{openstate.ErrInvalid, vfs.ErrInvalid},
		{openstate.ErrGrace, vfs.ErrGrace},
		{openstate.ErrShareViolation, vfs.ErrShareViolation},
		{openstate.ErrInstanceVersion, vfs.ErrShareViolation},
		{openstate.ErrDeletePending, vfs.ErrDeletePending},
		{openstate.ErrAccess, vfs.ErrAccess},
		{openstate.ErrLocked, vfs.ErrLocked},
		{&openstate.LockedError{}, vfs.ErrLocked},
		{openstate.ErrRangeNotLocked, vfs.ErrRangeNotLocked},
		{openstate.ErrLost, vfs.ErrLost},
		{openstate.ErrStopped, vfs.ErrUnavailable},
		{&openstate.DelayError{Cause: openstate.DelayRecall}, delay(vfs.DelayRecall)},
		{&openstate.DelayError{Cause: openstate.DelayResources}, delay(vfs.DelayResources)},
		{&openstate.DelayError{Cause: openstate.DelayFrozen}, delay(vfs.DelayFrozen)},
		{&openstate.DelayError{Cause: openstate.DelayCommit}, delay(vfs.DelayCommit)},
		{&openstate.DelayError{Cause: openstate.DelayConflict}, delay(vfs.DelayConflict)},
	} {
		if got := openErr(c.err); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v maps to %#v, want %#v", c.err, got, c.want)
		}
	}
}

func stateKind(err error, k vfs.StateKind) bool {
	var se *vfs.StateError
	return errors.As(err, &se) && se.Kind == k
}

// Each call on a client open state does not hold reaches open state, and
// answers its refusal.
func TestEachClientCallReachesOpenState(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx, c, gone := t.Context(), claimed(1000, inLab), vfs.ClientID(999)
	for name, err := range map[string]error{
		"AddSession": func() error { _, err := s.AddSession(ctx, c, gone, nil, nil); return err }(),
		"Disconnect": func() error {
			_, err := s.Disconnect(ctx, c, gone, vfs.SessionRef{}, vfs.SessionEnd{Last: true})
			return err
		}(),
		"Renew":            func() error { _, err := s.Renew(ctx, c, gone); return err }(),
		"ReclaimComplete":  s.ReclaimComplete(ctx, c, gone, nil),
		"ReleaseLockOwner": s.ReleaseLockOwner(ctx, c, gone, vfs.LockOwner{Client: gone, Name: []byte("pid 1")}),
	} {
		if !stateKind(err, vfs.StateStaleClient) {
			t.Errorf("%s of a client open state does not hold answered %v, want StaleClient", name, err)
		}
	}
	if err := s.FreeState(ctx, c, gone, vfs.StateRef{}); !stateKind(err, vfs.StateStaleState) {
		t.Errorf("FreeState of no state answered %v, want StaleState", err)
	}
	if errs := s.TestState(ctx, c, gone, []vfs.StateRef{{}}); len(errs) != 1 || !stateKind(errs[0], vfs.StateStaleState) {
		t.Errorf("TestState of no state answered %v, want StaleState", errs)
	}
}

// A host's restart ends the client of its old NSM state, which is told.
func TestAHostRestartEndsItsOldClient(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(t.Context(), claimed(1000, inLab), nlmHost("build01", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HostRestarted(t.Context(), claimed(1000, inLab), []byte("build01"), 2); err != nil {
		t.Fatal(err)
	}
	got := calls(t, cb, 1)
	if r, ok := got[0].Args[0].(vfs.Revocation); got[0].Method != "Revoked" || !ok || r.Client != id {
		t.Errorf("the restarted host's old client saw %v, want its revocation", got)
	}
}

// A disabled user connects no client.
func TestADisabledUserConnectsNoClient(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	alice.Disabled = true
	s.must(t, s.cp.PutUser(t.Context(), alice))
	if _, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), &vfstest.RecordingCallbacks{}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a disabled user's Connect answered %v, want ErrAccess", err)
	}
	if n := len(s.reg.cb); n != 0 {
		t.Errorf("a disabled user's Connect registered %d paths", n)
	}
}

// The latest Connect's path is the client's: a callback raised after goes
// there, carrying what open state raised, and not to an earlier path: a
// break goes through the latest path to the session holding its open.
func TestTheLatestConnectsPathTakesTheClientsCallbacks(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	first, latest := &vfstest.RecordingCallbacks{}, &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 2), smb("alice-pc", 1), latest); err != nil {
		t.Fatal(err)
	}
	s.RecallAny(id, 3)
	if got := calls(t, latest, 1); got[0].Method != "RecallAny" || got[0].Args[0] != 3 {
		t.Errorf("the latest path saw %v, want RecallAny keeping 3", got)
	}

	s.RecallGrant(leased(t, s, id, alice.Principal, 3, entity.NewFileID(), 1))
	calls(t, latest, 2)
	if got := breakSessions(latest); len(got) != 1 || got[0] != (vfs.SessionRef{ID: 3}) {
		t.Errorf("the break went to sessions %v, want 3", got)
	}
	none(t, "the first path", first)
}

// A recall carries the kind it goes to; one of a grant that is gone goes
// nowhere, not to the client's path; and the client's answer to a
// delegation's recall, which has no opener to check, is taken.
func TestADelegationsRecall(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	cb := &vfstest.RecordingCallbacks{}
	id := laptop(t, s, cb)
	g := delegate(t, s, id)
	recalled := g
	recalled.Recall = &openstate.Recall{To: openstate.GrantRead}
	s.RecallGrant(recalled)
	if got := calls(t, cb, 1); got[0].Args[0] != (vfs.SessionRef{}) || got[0].Args[3] != openstate.GrantRead {
		t.Errorf("the recall saw %v, want it to read caching, naming no session", got)
	}
	s.RecallGrant(openstate.CachingGrant{ID: openstate.GrantID{}, Client: id})
	time.Sleep(50 * time.Millisecond)
	if n := len(cb.Calls()); n != 1 {
		t.Errorf("a recall of a grant that is gone reached the client: %v", cb.Calls())
	}

	build01, _, err := s.Connect(ctx, signedIn(alice.Principal, 3), smb("build01", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	var delay *openstate.DelayError
	if _, err := s.tab.Opens().Open(ctx, openstate.Caller{Client: build01, Principal: alice.Principal, Protocol: entity.ProtocolSMB},
		openstate.FileKey{Share: s.share, ID: g.File}, openstate.OpenRequest{Access: entity.WriteData}); !errors.As(err, &delay) {
		t.Fatalf("build01's write open answered %v, want a delay behind the recall", err)
	}
	asLaptop := claimed(1000, inLab)
	asLaptop.Client = id
	if err := s.AckRecall(ctx, asLaptop, g.ID, openstate.GrantNone); err != nil {
		t.Errorf("the client's answer to its delegation's recall: %v", err)
	}
}

// A lock grant the client refuses is handed back to open state, which
// releases the lock: another host then takes the range.
func TestARefusedLockGrantReleasesTheLock(t *testing.T) {
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	host := func(name string, cb *vfstest.RecordingCallbacks) openstate.Caller {
		id, _, err := s.Connect(ctx, claimed(1000, inLab), nlmHost(name, 1), cb)
		if err != nil {
			t.Fatal(err)
		}
		return openstate.Caller{Client: id, Protocol: entity.ProtocolNLM}
	}
	a, b, c := host("a", &vfstest.RecordingCallbacks{}), host("b", &vfstest.RecordingCallbacks{Refuse: true}), host("c", &vfstest.RecordingCallbacks{})
	file := openstate.FileKey{Share: s.share, ID: entity.NewFileID()}
	r := openstate.ByteRange{Offset: 0, Length: 10}
	lock := func(who openstate.Caller, wait openstate.WaitKind) (openstate.LockResult, error) {
		return s.tab.Locks().Lock(ctx, who, file, openstate.LockReq{
			Owner: openstate.LockOwner{Client: who.Client, Name: []byte("pid 1")}, Open: openstate.OpenRef{Kind: openstate.RefAnonymous},
			Ranges: []openstate.LockRange{{ByteRange: r, Exclusive: true}}, Wait: wait,
		})
	}
	if _, err := lock(a, 0); err != nil {
		t.Fatal(err)
	}
	if res, err := lock(b, openstate.WaitCallback); err != nil || !res.Waiting {
		t.Fatalf("b's lock answered %+v, %v; want it waiting", res, err)
	}
	if _, err := s.tab.Locks().Unlock(ctx, a, file, openstate.OpenRef{Kind: openstate.RefAnonymous},
		openstate.LockOwner{Client: a.Client, Name: []byte("pid 1")}, []openstate.ByteRange{r}, openstate.Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := lock(c, 0); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the range b refused stays locked")
}

// A lock grant goes on being sent only while the client has Callbacks: once
// the client ends, the grant is sent no more.
func TestALockGrantStopsAtThePathsEnd(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error { return context.DeadlineExceeded }}
	id, _, err := s.Connect(t.Context(), signedIn(alice.Principal, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	s.LockWaitOver(openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}, true)
	calls(t, cb, 3)
	s.ClientEnded(id, nil, openstate.RevokedLeaseExpired)
	time.Sleep(20 * time.Millisecond)
	n := len(cb.Calls())
	time.Sleep(50 * time.Millisecond)
	if m := len(cb.Calls()); m != n {
		t.Errorf("the grant was sent %d times more after the client ended", m-n)
	}
}

// A break that finds no session to go on is asked again: alice's only
// session ends, her lease's open kept as durable, and a break raised then is
// sent on the session she signs in on again within its deadline.
func TestABreakWithNoSessionGoesToTheNextOneSignedIn(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	pc := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	disconnect(t, s, id, 1)
	s.RecallGrant(g)
	calls(t, pc, 1)
	if got := breakSessions(pc); len(got) != 0 {
		t.Fatalf("with no session the break went on %v, want none", got)
	}
	pc.SignIn(vfs.SessionRef{ID: 2}, alice.Principal)
	if got := sentOn(t, pc, 1); got[0] != (vfs.SessionRef{ID: 2}) {
		t.Errorf("the break went on sessions %v, want alice's new one, 2", got)
	}
}

// A break whose send failed is sent again through Callbacks of a type that
// cannot be compared: its routes are told apart without comparing them.
func TestAFailedBreakThroughIncomparableCallbacksIsSentAgain(t *testing.T) {
	fastRetry(t)
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx := t.Context()
	rec := &vfstest.RecordingCallbacks{Answer: func(_ context.Context, c vfstest.Callback) error {
		if c.Args[0] == (vfs.SessionRef{ID: 1}) {
			return errors.New("session gone")
		}
		return nil
	}}
	pc := valueCallbacks{RecordingCallbacks: rec}
	id, _, err := s.Connect(ctx, signedIn(alice.Principal, 1), smb("alice-pc", 1), pc)
	if err != nil {
		t.Fatal(err)
	}
	rec.SignIn(vfs.SessionRef{ID: 2}, alice.Principal)
	g := leased(t, s, id, alice.Principal, 1, entity.NewFileID(), 1)
	s.RecallGrant(g)
	calls(t, rec, 1)
	time.Sleep(20 * time.Millisecond)
	disconnect(t, s, id, 1)
	sentOn(t, rec, 2)
	if got := breakSessions(rec); len(got) != 2 || got[1] != (vfs.SessionRef{ID: 2}) {
		t.Errorf("the break went to sessions %v, want 1, then 2", got)
	}
}

// An SMB Connect lists this node among the client's sessions, so the client's
// record is not released while it is listed, under the node's current
// epoch: another node's last Disconnect leaves it.
func TestAnSMBConnectListsThisNode(t *testing.T) {
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	ctx, as := t.Context(), signedIn(alice.Principal, 1)
	id, _, err := s.Connect(ctx, as, smb("alice-pc", 1), &vfstest.RecordingCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Record(ctx, as, id); err != nil || len(rec.Reach.Sessions) != 1 || rec.Reach.Sessions[0].Node != (entity.NodeID{}) || rec.Reach.Sessions[0].Epoch == 0 {
		t.Fatalf("after Connect: sessions %v, %v; want this node's, under its epoch", rec.Reach.Sessions, err)
	}
	if _, err := s.tab.Clients().Disconnect(ctx, id, vfs.SessionRef{Node: 1, ID: 1}, vfs.SessionEnd{Last: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, as, id); err != nil {
		t.Errorf("with this node listed, another's last Disconnect released the record: %v", err)
	}
}

// A lock grant the client never answers stops being sent once
// grantResendFor has passed, and at once when the client's record is
// released.
func TestALockGrantStopsBeingSentAtItsDeadlineOrRelease(t *testing.T) {
	fastRetry(t)
	d := grantResendFor
	t.Cleanup(func() { grantResendFor = d })
	s := newShares(t, nil, nil)
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite).Principal
	silent := func(context.Context, vfstest.Callback) error { return context.DeadlineExceeded }
	stops := func(name string, cb *vfstest.RecordingCallbacks, end func()) {
		t.Helper()
		calls(t, cb, 3)
		end()
		time.Sleep(20 * time.Millisecond)
		n := len(cb.Calls())
		time.Sleep(50 * time.Millisecond)
		if m := len(cb.Calls()); m != n {
			t.Errorf("%s: the grant was sent %d times more", name, m-n)
		}
	}

	grantResendFor = 20 * time.Millisecond
	cb := &vfstest.RecordingCallbacks{Answer: silent}
	id, _, err := s.Connect(t.Context(), signedIn(alice, 1), smb("alice-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	s.LockWaitOver(openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}, true)
	stops("past its deadline", cb, func() { time.Sleep(grantResendFor) })

	grantResendFor = time.Hour
	cb = &vfstest.RecordingCallbacks{Answer: silent}
	id, gen, err := s.Connect(t.Context(), signedIn(alice, 2), smb("bob-pc", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	s.LockWaitOver(openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}, true)
	stops("released", cb, func() {
		if _, err := s.Disconnect(t.Context(), signedIn(alice, 2), id, vfs.SessionRef{ID: 2}, vfs.SessionEnd{Last: true, Gen: gen}); err != nil {
			t.Fatal(err)
		}
	})
}

// An NLM host's grant is never given up for a silence: it is still sent
// again past grantResendFor.
func TestAnNLMLockGrantIsResentPastTheBound(t *testing.T) {
	fastRetry(t)
	d := grantResendFor
	t.Cleanup(func() { grantResendFor = d })
	grantResendFor = 20 * time.Millisecond
	s := newShares(t, nil, nil)
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	cb := &vfstest.RecordingCallbacks{Answer: func(context.Context, vfstest.Callback) error { return context.DeadlineExceeded }}
	id, _, err := s.Connect(t.Context(), claimed(1000, inLab), nlmHost("build01", 1), cb)
	if err != nil {
		t.Fatal(err)
	}
	s.LockWaitOver(openstate.LockWaiter{File: entity.NewFileID(), Owner: openstate.LockOwner{Client: id}, Wait: openstate.WaitCallback}, true)
	time.Sleep(grantResendFor + 20*time.Millisecond)
	n := len(cb.Calls())
	time.Sleep(50 * time.Millisecond)
	if m := len(cb.Calls()); m == n {
		t.Error("an NLM host's grant stopped being sent past grantResendFor")
	}
	s.ClientEnded(id, nil, openstate.RevokedLeaseExpired)
}
