package state

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// spyRecoveryStore is a test double for lock.ClientRecoveryStore that records
// every call and lets a test seed records and inject errors.
type spyRecoveryStore struct {
	mu sync.Mutex

	records map[string]*lock.V4ClientRecoveryRecord

	puts         []*lock.V4ClientRecoveryRecord
	deletes      []string
	reclaimMarks []string
	lists        int
	putErr       error
	deleteErr    error
	listErr      error
	reclaimErr   error
}

func newSpyRecoveryStore() *spyRecoveryStore {
	return &spyRecoveryStore{records: make(map[string]*lock.V4ClientRecoveryRecord)}
}

func (s *spyRecoveryStore) PutClientRecovery(_ context.Context, rec *lock.V4ClientRecoveryRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	cp := *rec
	s.puts = append(s.puts, &cp)
	s.records[rec.ClientIDString] = &cp
	return nil
}

func (s *spyRecoveryStore) DeleteClientRecovery(_ context.Context, clientIDString string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletes = append(s.deletes, clientIDString)
	delete(s.records, clientIDString)
	return nil
}

func (s *spyRecoveryStore) ListClientRecovery(_ context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]*lock.V4ClientRecoveryRecord, 0, len(s.records))
	for _, r := range s.records {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (s *spyRecoveryStore) RecordReclaimComplete(_ context.Context, clientIDString string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reclaimErr != nil {
		return s.reclaimErr
	}
	s.reclaimMarks = append(s.reclaimMarks, clientIDString)
	if r, ok := s.records[clientIDString]; ok {
		r.ReclaimComplete = true
	}
	return nil
}

func (s *spyRecoveryStore) snapshotPuts() []*lock.V4ClientRecoveryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*lock.V4ClientRecoveryRecord(nil), s.puts...)
}

func (s *spyRecoveryStore) snapshotDeletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletes...)
}

func (s *spyRecoveryStore) snapshotReclaims() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reclaimMarks...)
}

// ---------------------------------------------------------------------------
// Persist on confirm (v4.0 SETCLIENTID_CONFIRM)
// ---------------------------------------------------------------------------

func TestClientRecovery_PersistOnConfirmV40(t *testing.T) {
	spy := newSpyRecoveryStore()
	sm := NewStateManager(5 * time.Second)
	sm.SetClientRecoveryStore(spy, 42)

	verf := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	res, err := sm.SetClientID("client-A", verf, CallbackInfo{}, "10.0.0.1:1", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	puts := spy.snapshotPuts()
	if len(puts) != 1 {
		t.Fatalf("expected 1 PutClientRecovery, got %d", len(puts))
	}
	got := puts[0]
	if got.ClientIDString != "client-A" {
		t.Errorf("ClientIDString = %q, want client-A", got.ClientIDString)
	}
	if got.ClientID != res.ClientID {
		t.Errorf("ClientID = %d, want %d", got.ClientID, res.ClientID)
	}
	if got.BootVerifier != verf {
		t.Errorf("BootVerifier = %x, want %x", got.BootVerifier, verf)
	}
	if got.Principal != "uid:1000" {
		t.Errorf("Principal = %q, want uid:1000", got.Principal)
	}
	if got.ServerEpoch != 42 {
		t.Errorf("ServerEpoch = %d, want 42", got.ServerEpoch)
	}
	if got.ConfirmedAt.IsZero() {
		t.Error("ConfirmedAt is zero")
	}
}

// A persist failure must NOT fail the confirm (best-effort durability).
func TestClientRecovery_ConfirmSucceedsDespitePersistError(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.putErr = errors.New("backend down")
	sm := NewStateManager(5 * time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	res, err := sm.SetClientID("client-B", [8]byte{9}, CallbackInfo{}, "10.0.0.2:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID must succeed despite persist error, got: %v", err)
	}
	if sm.GetClient(res.ClientID) == nil {
		t.Fatal("client must be confirmed in memory despite persist error")
	}
}

// nil recovery store => behave as today, no panic, no calls.
func TestClientRecovery_NilStore(t *testing.T) {
	sm := NewStateManager(5 * time.Second)
	res, err := sm.SetClientID("client-C", [8]byte{1}, CallbackInfo{}, "10.0.0.3:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	if sm.HasClientRecoveryStore() {
		t.Fatal("HasClientRecoveryStore should be false with no store wired")
	}
	// Boot-load with no store is a no-op returning 0.
	if n := sm.LoadClientRecovery(context.Background()); n != 0 {
		t.Fatalf("LoadClientRecovery with nil store = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Delete on lease expiry
// ---------------------------------------------------------------------------

func TestClientRecovery_DeleteOnLeaseExpiry(t *testing.T) {
	spy := newSpyRecoveryStore()
	sm := NewStateManager(5 * time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	res, err := sm.SetClientID("client-exp", [8]byte{7}, CallbackInfo{}, "10.0.0.4:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Directly trigger the lease-expiry callback (deterministic, no timer wait).
	sm.onLeaseExpired(res.ClientID)

	dels := spy.snapshotDeletes()
	if len(dels) != 1 || dels[0] != "client-exp" {
		t.Fatalf("expected DeleteClientRecovery(client-exp), got %v", dels)
	}
}

// ---------------------------------------------------------------------------
// Boot load seeds the grace roster; ReclaimComplete records excluded
// ---------------------------------------------------------------------------

func TestClientRecovery_BootLoadSeedsRoster(t *testing.T) {
	spy := newSpyRecoveryStore()
	// Two waitable clients + one already reclaim-complete (must be excluded).
	spy.records["client-1"] = &lock.V4ClientRecoveryRecord{ClientIDString: "client-1", BootVerifier: [8]byte{1}}
	spy.records["client-2"] = &lock.V4ClientRecoveryRecord{ClientIDString: "client-2", BootVerifier: [8]byte{2}}
	spy.records["client-done"] = &lock.V4ClientRecoveryRecord{ClientIDString: "client-done", ReclaimComplete: true}

	sm := NewStateManager(5*time.Second, 5*time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	n := sm.LoadClientRecovery(context.Background())
	if n != 2 {
		t.Fatalf("LoadClientRecovery seeded %d clients, want 2 (reclaim-complete excluded)", n)
	}
	if !sm.IsInGrace() {
		t.Fatal("server should be in grace after boot-load with waitable clients")
	}
	gp := sm.gracePeriod
	if got := len(gp.expectedClientStrings); got != 2 {
		t.Fatalf("expected 2 roster strings, got %d", got)
	}
	if gp.expectedClientStrings["client-done"] {
		t.Error("reclaim-complete client must not be on the roster")
	}
	if !gp.expectedClientStrings["client-1"] || !gp.expectedClientStrings["client-2"] {
		t.Error("waitable clients missing from roster")
	}
}

// Empty store (fresh boot) => no grace, behaves as develop.
func TestClientRecovery_BootLoadEmptySkipsGrace(t *testing.T) {
	spy := newSpyRecoveryStore()
	sm := NewStateManager(5*time.Second, 5*time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	if n := sm.LoadClientRecovery(context.Background()); n != 0 {
		t.Fatalf("empty store should seed 0, got %d", n)
	}
	if sm.IsInGrace() {
		t.Fatal("fresh boot with empty roster must NOT enter grace")
	}
}

// All-reclaim-complete store => nothing waitable => no grace.
func TestClientRecovery_BootLoadAllCompleteSkipsGrace(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.records["c"] = &lock.V4ClientRecoveryRecord{ClientIDString: "c", ReclaimComplete: true}
	sm := NewStateManager(5*time.Second, 5*time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	if n := sm.LoadClientRecovery(context.Background()); n != 0 {
		t.Fatalf("all-complete store should seed 0, got %d", n)
	}
	if sm.IsInGrace() {
		t.Fatal("all-reclaim-complete roster must NOT enter grace")
	}
}

// ---------------------------------------------------------------------------
// Reclaim: matching verifier allowed + early-exits; changed verifier rejected
// ---------------------------------------------------------------------------

func TestClientRecovery_ReclaimMatchingVerifierAndEarlyExit(t *testing.T) {
	spy := newSpyRecoveryStore()
	verf := [8]byte{0xaa}
	spy.records["reclaimer"] = &lock.V4ClientRecoveryRecord{ClientIDString: "reclaimer", BootVerifier: verf}

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	if n := sm.LoadClientRecovery(context.Background()); n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}
	if !sm.IsInGrace() {
		t.Fatal("should be in grace")
	}

	// The reclaiming client re-establishes its identity (fresh numeric clientID)
	// with the SAME boot verifier, then reclaims via CLAIM_PREVIOUS.
	res, err := sm.SetClientID("reclaimer", verf, CallbackInfo{}, "10.0.0.9:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("CLAIM_PREVIOUS with matching verifier should be allowed, got: %v", err)
	}

	// Roster drained by the single reclaimer => grace early-exits.
	if sm.IsInGrace() {
		t.Fatal("grace should early-exit once the only expected client reclaimed")
	}
	// First CLAIM_PREVIOUS is the v4.0 reclaim marker.
	if marks := spy.snapshotReclaims(); len(marks) == 0 || marks[len(marks)-1] != "reclaimer" {
		t.Fatalf("expected RecordReclaimComplete(reclaimer), got %v", marks)
	}
}

func TestClientRecovery_ReclaimChangedVerifierRejected(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.records["rebooter"] = &lock.V4ClientRecoveryRecord{ClientIDString: "rebooter", BootVerifier: [8]byte{0x11}}

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	if n := sm.LoadClientRecovery(context.Background()); n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}

	// Client comes back with a DIFFERENT boot verifier (it rebooted).
	changed := [8]byte{0x22}
	res, err := sm.SetClientID("rebooter", changed, CallbackInfo{}, "10.0.0.10:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	_, err = sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS)
	if !errors.Is(err, ErrNoGrace) {
		t.Fatalf("CLAIM_PREVIOUS with changed verifier must be rejected with ErrNoGrace, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Grace still lifts via the hard timer backstop even if the roster never drains
// ---------------------------------------------------------------------------

func TestClientRecovery_GraceTimerBackstopLifts(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.records["never-comes-back"] = &lock.V4ClientRecoveryRecord{ClientIDString: "never-comes-back", BootVerifier: [8]byte{5}}

	sm := NewStateManager(5*time.Second, 100*time.Millisecond)
	sm.SetClientRecoveryStore(spy, 1)
	if n := sm.LoadClientRecovery(context.Background()); n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}
	if !sm.IsInGrace() {
		t.Fatal("should be in grace")
	}

	// No reclaim happens; the hard timer must still lift grace.
	deadline := time.After(2 * time.Second)
	for sm.IsInGrace() {
		select {
		case <-deadline:
			t.Fatal("grace did not lift via timer backstop")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// v4.1: CREATE_SESSION persists; RECLAIM_COMPLETE marks + drains roster
// ---------------------------------------------------------------------------

func TestClientRecovery_V41PersistAndReclaimComplete(t *testing.T) {
	spy := newSpyRecoveryStore()
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	owner := []byte("v41-owner")
	verf := [8]byte{0x42}
	exch, err := sm.ExchangeID(owner, verf, 0, nil, "10.0.0.20:1", "uid:0")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	// First CREATE_SESSION confirms + persists.
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, types.ChannelAttrs{}, types.ChannelAttrs{}, 0, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	key := v41RecoveryKey(owner)
	puts := spy.snapshotPuts()
	if len(puts) != 1 || puts[0].ClientIDString != key {
		t.Fatalf("expected v4.1 Put keyed by %q, got %v", key, puts)
	}
	if puts[0].BootVerifier != verf || puts[0].Principal != "uid:0" || puts[0].ServerEpoch != 7 {
		t.Errorf("v4.1 record fields wrong: %+v", puts[0])
	}

	// Now simulate a restart roster containing this client, then RECLAIM_COMPLETE.
	sm2 := NewStateManager(5*time.Second, 30*time.Second)
	sm2.SetClientRecoveryStore(spy, 8)
	if n := sm2.LoadClientRecovery(context.Background()); n != 1 {
		t.Fatalf("post-restart seeded %d, want 1", n)
	}
	// Re-establish identity and a session, then RECLAIM_COMPLETE.
	exch2, err := sm2.ExchangeID(owner, verf, 0, nil, "10.0.0.20:2")
	if err != nil {
		t.Fatalf("ExchangeID(2): %v", err)
	}
	cs, _, err := sm2.CreateSession(exch2.ClientID, exch2.SequenceID, 0, types.ChannelAttrs{}, types.ChannelAttrs{}, 0, nil)
	if err != nil {
		t.Fatalf("CreateSession(2): %v", err)
	}
	_ = cs
	if err := sm2.ReclaimComplete(exch2.ClientID); err != nil {
		t.Fatalf("ReclaimComplete: %v", err)
	}
	if sm2.IsInGrace() {
		t.Fatal("grace should early-exit after the only expected v4.1 client RECLAIM_COMPLETEs")
	}
	marks := spy.snapshotReclaims()
	if len(marks) == 0 || marks[len(marks)-1] != key {
		t.Fatalf("expected RecordReclaimComplete(%q), got %v", key, marks)
	}
}

// DESTROY_CLIENTID deletes the v4.1 recovery record.
func TestClientRecovery_V41DestroyDeletes(t *testing.T) {
	spy := newSpyRecoveryStore()
	sm := NewStateManager(5 * time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	owner := []byte("destroy-owner")
	exch, err := sm.ExchangeID(owner, [8]byte{1}, 0, nil, "10.0.0.30:1")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, types.ChannelAttrs{}, types.ChannelAttrs{}, 0, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Destroy requires no active sessions; tear them down first.
	for _, s := range sm.ListSessionsForClient(exch.ClientID) {
		if err := sm.DestroySession(s.SessionID); err != nil {
			t.Fatalf("DestroySession: %v", err)
		}
	}
	if err := sm.DestroyV41ClientID(exch.ClientID); err != nil {
		t.Fatalf("DestroyV41ClientID: %v", err)
	}

	key := v41RecoveryKey(owner)
	dels := spy.snapshotDeletes()
	found := false
	for _, d := range dels {
		if d == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected DeleteClientRecovery(%q), got %v", key, dels)
	}
}
