package state

import (
	"context"
	"errors"
	"slices"
	"sort"
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

	// putGate, when non-nil, blocks every PutClientRecovery until it is closed,
	// so a test can hold one write in flight while another state operation runs.
	putGate chan struct{}

	// putCalls counts PutClientRecovery attempts, including the ones that fail.
	putCalls int

	// putFailuresLeft counts down how many PutClientRecovery calls fail before
	// the store starts succeeding; a test seeds it to make the row write fail
	// exactly N times.
	putFailuresLeft int

	// reclaimFailuresLeft counts down how many RecordReclaimComplete calls fail
	// before the store starts succeeding; a test seeds it to make the persist
	// fail exactly N times.
	reclaimFailuresLeft int
}

func newSpyRecoveryStore() *spyRecoveryStore {
	return &spyRecoveryStore{records: make(map[string]*lock.V4ClientRecoveryRecord)}
}

func (s *spyRecoveryStore) PutClientRecovery(_ context.Context, rec *lock.V4ClientRecoveryRecord) error {
	s.mu.Lock()
	s.putCalls++
	gate := s.putGate
	s.mu.Unlock()

	if gate != nil {
		<-gate
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putFailuresLeft > 0 {
		s.putFailuresLeft--
		return errors.New("backend down")
	}
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
	if s.reclaimFailuresLeft > 0 {
		s.reclaimFailuresLeft--
		return errors.New("backend down")
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

func (s *spyRecoveryStore) snapshotPutCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCalls
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

// waitFor polls cond until it holds, failing with msg after five seconds. The
// recovery-row write runs off sm.mu, so a test that has just taken state must
// wait for it rather than read the store straight away.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForRecordKeys waits until the store holds exactly the given identities.
func waitForRecordKeys(t *testing.T, spy *spyRecoveryStore, want ...string) {
	t.Helper()
	sort.Strings(want)
	var got []string
	waitFor(t, "", func() bool {
		got = spy.snapshotRecordKeys()
		return slices.Equal(got, want)
	})
}

// ---------------------------------------------------------------------------
// Persist on the first OPEN, not on confirm (v4.0)
// ---------------------------------------------------------------------------

func TestClientRecovery_PersistOnFirstOpenV40(t *testing.T) {
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

	// Confirmed and holding nothing: no row yet.
	if puts := spy.snapshotPuts(); len(puts) != 0 {
		t.Fatalf("confirm alone must write no recovery record, got %d", len(puts))
	}

	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitFor(t, "the first OPEN must write the recovery record", func() bool {
		return len(spy.snapshotPuts()) == 1
	})

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

// A persist failure must NOT fail the OPEN that triggered it (best-effort
// durability), and must leave the write to be retried by the next OPEN rather
// than latched as done.
func TestClientRecovery_OpenSucceedsDespitePersistError(t *testing.T) {
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
	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile must succeed despite persist error, got: %v", err)
	}
	if sm.GetClient(res.ClientID) == nil {
		t.Fatal("client must be confirmed in memory despite persist error")
	}
	waitFor(t, "a failed write must leave RecoveryPersisted clear so the next OPEN retries", func() bool {
		sm.mu.RLock()
		defer sm.mu.RUnlock()
		rec := sm.clientRecordLocked(res.ClientID)
		return rec != nil && !rec.RecoveryPersisted
	})

	// The backend comes back: the next OPEN lands the write.
	spy.mu.Lock()
	spy.putErr = nil
	spy.mu.Unlock()
	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 2, []byte("fh2"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile(2): %v", err)
	}
	waitForRecordKeys(t, spy, "client-B")
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
	if n := sm.LoadClientRecovery(context.Background(), true); n != 0 {
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
	// Take state so a durable row exists to be deleted.
	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitForRecordKeys(t, spy, "client-exp")

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

	n := sm.LoadClientRecovery(context.Background(), true)
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

	if n := sm.LoadClientRecovery(context.Background(), true); n != 0 {
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

	if n := sm.LoadClientRecovery(context.Background(), true); n != 0 {
		t.Fatalf("all-complete store should seed 0, got %d", n)
	}
	if sm.IsInGrace() {
		t.Fatal("all-reclaim-complete roster must NOT enter grace")
	}
}

// ---------------------------------------------------------------------------
// armGrace=false: roster is read, verifier gate armed, but no window opened
// ---------------------------------------------------------------------------

func TestClientRecovery_NoReclaimableStateSkipsGrace(t *testing.T) {
	spy := newSpyRecoveryStore()
	verf := [8]byte{0xcd}
	spy.records["idle-client"] = &lock.V4ClientRecoveryRecord{ClientIDString: "idle-client", BootVerifier: verf}

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)

	if n := sm.LoadClientRecovery(context.Background(), false); n != 0 {
		t.Fatalf("seeded %d, want 0 when nothing is reclaimable", n)
	}
	if sm.IsInGrace() {
		t.Fatal("a waitable roster must NOT open the grace window when no state is reclaimable")
	}

	// The verifier snapshot is still armed: it gates CLAIM_PREVIOUS for any
	// prior client and must not depend on whether a window was opened.
	sm.mu.RLock()
	got, ok := sm.bootRecoveryVerifiers["idle-client"]
	sm.mu.RUnlock()
	if !ok {
		t.Fatal("boot verifier snapshot must be taken even when grace is not seeded")
	}
	if got != verf {
		t.Fatalf("boot verifier = %v, want %v", got, verf)
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
	if n := sm.LoadClientRecovery(context.Background(), true); n != 1 {
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
	// First CLAIM_PREVIOUS is the v4.0 reclaim marker; the persist is now
	// asynchronous, so poll for the mark.
	deadline := time.After(5 * time.Second)
	for {
		marks := spy.snapshotReclaims()
		if len(marks) > 0 && marks[len(marks)-1] == "reclaimer" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected RecordReclaimComplete(reclaimer), got %v", marks)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestClientRecovery_ReclaimChangedVerifierRejected(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.records["rebooter"] = &lock.V4ClientRecoveryRecord{ClientIDString: "rebooter", BootVerifier: [8]byte{0x11}}

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	if n := sm.LoadClientRecovery(context.Background(), true); n != 1 {
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
	if n := sm.LoadClientRecovery(context.Background(), true); n != 1 {
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
	// First CREATE_SESSION confirms; it must NOT write a recovery record.
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil, "uid:0"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if puts := spy.snapshotPuts(); len(puts) != 0 {
		t.Fatalf("CREATE_SESSION alone must write no recovery record, got %d", len(puts))
	}

	// Taking an open is what puts the client on the durable roster.
	if _, err := sm.OpenFile(exch.ClientID, []byte("v41-open-owner"), 0, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	key := v41RecoveryKey(owner)
	waitForRecordKeys(t, spy, key)
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
	if n := sm2.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("post-restart seeded %d, want 1", n)
	}
	// Re-establish identity and a session, then RECLAIM_COMPLETE.
	exch2, err := sm2.ExchangeID(owner, verf, 0, nil, "10.0.0.20:2")
	if err != nil {
		t.Fatalf("ExchangeID(2): %v", err)
	}
	cs, _, err := sm2.CreateSession(exch2.ClientID, exch2.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil)
	if err != nil {
		t.Fatalf("CreateSession(2): %v", err)
	}
	_ = cs
	if err := sm2.ReclaimComplete(exch2.ClientID, false); err != nil {
		t.Fatalf("ReclaimComplete: %v", err)
	}
	if sm2.IsInGrace() {
		t.Fatal("grace should early-exit after the only expected v4.1 client RECLAIM_COMPLETEs")
	}
	deadline := time.After(5 * time.Second)
	for {
		marks := spy.snapshotReclaims()
		if len(marks) > 0 && marks[len(marks)-1] == key {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected RecordReclaimComplete(%q), got %v", key, marks)
		case <-time.After(20 * time.Millisecond):
		}
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
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Take state so a durable row exists for DESTROY_CLIENTID to delete.
	if _, err := sm.OpenFile(exch.ClientID, []byte("destroy-open-owner"), 0, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitForRecordKeys(t, spy, v41RecoveryKey(owner))
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

// ---------------------------------------------------------------------------
// Failed reclaim-complete persist is retried in the background
// ---------------------------------------------------------------------------

// A failed reclaim-complete persist must be repaired by the background retry
// without any further protocol activity: the store fails the first write, the
// retry lands, and the durable record carries ReclaimComplete so a second
// restart inside one grace window does not re-wait on the client.
func TestClientRecovery_ReclaimPersistRetriedAfterFailure(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.reclaimFailuresLeft = 1
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	owner := []byte("v41-retry-owner")
	exch, err := sm.ExchangeID(owner, [8]byte{0x33}, 0, nil, "10.0.0.30:1", "uid:0")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil, "uid:0"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Take state so a durable row exists to be marked reclaim-complete.
	if _, err := sm.OpenFile(exch.ClientID, []byte("retry-open-owner"), 0, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitForRecordKeys(t, spy, v41RecoveryKey(owner))
	if err := sm.ReclaimComplete(exch.ClientID, false); err != nil {
		t.Fatalf("ReclaimComplete: %v", err)
	}

	// The first write failed, so the durable record must not be marked yet.
	key := v41RecoveryKey(owner)
	marks := spy.snapshotReclaims()
	if len(marks) != 0 {
		t.Fatalf("no reclaim mark should have landed yet, got %v", marks)
	}

	// The background retry (2s base delay) must repair the write on its own.
	deadline := time.After(10 * time.Second)
	for len(spy.snapshotReclaims()) == 0 {
		select {
		case <-deadline:
			t.Fatal("reclaim-complete persist was never retried")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !spy.records[key].ReclaimComplete {
		t.Fatal("durable record not marked reclaim-complete after retry")
	}
}

// A retry must not stamp ReclaimComplete over a fresh incarnation's durable
// record: once the issuing incarnation no longer holds the key, the pending
// retry is abandoned. A client that re-registers after a restart must still
// send RECLAIM_COMPLETE itself.
func TestClientRecovery_ReclaimPersistRetryAbandonedWhenIncarnationGone(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.reclaimFailuresLeft = 1
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	owner := []byte("v41-stale-retry-owner")
	exch, err := sm.ExchangeID(owner, [8]byte{0x34}, 0, nil, "10.0.0.31:1", "uid:0")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil, "uid:0"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sm.ReclaimComplete(exch.ClientID, false); err != nil {
		t.Fatalf("ReclaimComplete: %v", err)
	}

	// Destroy the client (removes both the in-memory record and the durable
	// one) before the retry fires, then re-register the same owner identity.
	sessions := sm.ListSessionsForClient(exch.ClientID)
	for _, s := range sessions {
		if err := sm.DestroySession(s.SessionID); err != nil {
			t.Fatalf("DestroySession: %v", err)
		}
	}
	if err := sm.DestroyV41ClientID(exch.ClientID); err != nil {
		t.Fatalf("DestroyV41ClientID: %v", err)
	}
	exch2, err := sm.ExchangeID(owner, [8]byte{0x34}, 0, nil, "10.0.0.31:2", "uid:0")
	if err != nil {
		t.Fatalf("ExchangeID(2): %v", err)
	}
	if _, _, err := sm.CreateSession(exch2.ClientID, exch2.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil, "uid:0"); err != nil {
		t.Fatalf("CreateSession(2): %v", err)
	}

	// Wait out the retry window: no mark may land, the fresh incarnation has
	// not sent RECLAIM_COMPLETE and a stale retry must not do it for it.
	time.Sleep(3 * time.Second)
	if marks := spy.snapshotReclaims(); len(marks) != 0 {
		t.Fatalf("stale retry stamped a fresh incarnation's record: %v", marks)
	}
}

// A v4.0 CLAIM_PREVIOUS reclaim must persist its reclaim-complete marker and
// set the in-memory flag, so a failed write is repaired by the background
// retry: the v4.0 path sets ReclaimComplete before scheduling (the flag is
// what a retry re-validates against), and the first successful CLAIM_PREVIOUS
// is the v4.0 analog of RECLAIM_COMPLETE.
func TestClientRecovery_V40ReclaimPersistRetriedAfterFailure(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.reclaimFailuresLeft = 1
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	// Re-establish identity with a matching boot verifier, then reclaim via
	// CLAIM_PREVIOUS during grace.
	verf := [8]byte{0x35}
	spy.records["v40-retry-owner"] = &lock.V4ClientRecoveryRecord{
		ClientIDString: "v40-retry-owner",
		BootVerifier:   verf,
	}
	if n := sm.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}
	if !sm.IsInGrace() {
		t.Fatal("should be in grace")
	}
	res, err := sm.SetClientID("v40-retry-owner", verf, CallbackInfo{}, "10.0.0.32:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("CLAIM_PREVIOUS: %v", err)
	}

	// The first write failed, so no durable mark may have landed yet.
	marks := spy.snapshotReclaims()
	if len(marks) != 0 {
		t.Fatalf("no reclaim mark should have landed yet, got %v", marks)
	}

	// The background retry (2s base delay) must repair the write on its own.
	deadline := time.After(10 * time.Second)
	for len(spy.snapshotReclaims()) == 0 {
		select {
		case <-deadline:
			t.Fatal("v4.0 reclaim-complete persist was never retried")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !spy.records["v40-retry-owner"].ReclaimComplete {
		t.Fatal("durable record not marked reclaim-complete after retry")
	}
}

// A v4.0 retry must not stamp ReclaimComplete over a fresh incarnation's
// durable record either: once the issuing client no longer holds the key, the
// pending entry is dropped and the chain dies.
func TestClientRecovery_V40ReclaimPersistRetryAbandonedWhenClientGone(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.reclaimFailuresLeft = 1
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	verf := [8]byte{0x36}
	spy.records["v40-stale-retry-owner"] = &lock.V4ClientRecoveryRecord{
		ClientIDString: "v40-stale-retry-owner",
		BootVerifier:   verf,
	}
	if n := sm.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}
	res, err := sm.SetClientID("v40-stale-retry-owner", verf, CallbackInfo{}, "10.0.0.33:1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	if _, err := sm.OpenFile(res.ClientID, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("CLAIM_PREVIOUS: %v", err)
	}

	// Drop the client's in-memory record before the retry fires: the issuing
	// client no longer holds the key, so the stale retry must be abandoned.
	sm.mu.Lock()
	delete(sm.clientsByID, res.ClientID)
	sm.mu.Unlock()

	time.Sleep(3 * time.Second)
	if marks := spy.snapshotReclaims(); len(marks) != 0 {
		t.Fatalf("stale v4.0 retry stamped a record after the client was gone: %v", marks)
	}
}

// A re-schedule while a retry chain is live must adopt the chain, not fork a
// second one: two failed persists for the same key leave exactly one pending
// entry, so a down backend cannot pile up chains for one client.
func TestClientRecovery_ReclaimPersistRescheduleAdoptsExistingChain(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.reclaimErr = errors.New("backend down")
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 7)

	owner := []byte("adopt-owner")
	exch, err := sm.ExchangeID(owner, [8]byte{0x37}, 0, nil, "10.0.0.34:1", "uid:0")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	if _, _, err := sm.CreateSession(exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil, "uid:0"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sm.ReclaimComplete(exch.ClientID, false); err != nil {
		t.Fatalf("ReclaimComplete: %v", err)
	}

	// The store is persistently down, so the first chain is live and backing
	// off. A second RECLAIM_COMPLETE would draw COMPLETE_ALREADY, so drive the
	// re-schedule directly through the internal seam the write site uses.
	sm.mu.Lock()
	sm.scheduleReclaimPersistRetryLocked(exch.ClientID, v41RecoveryKey(owner), reclaimPersistRetryBase)
	pendingCount := len(sm.pendingReclaimPersists)
	clientID, delay := sm.pendingReclaimPersists[v41RecoveryKey(owner)].clientID, sm.pendingReclaimPersists[v41RecoveryKey(owner)].delay
	sm.mu.Unlock()

	if pendingCount != 1 {
		t.Fatalf("pending chains = %d, want 1 (a re-schedule must adopt, not fork)", pendingCount)
	}
	if clientID != exch.ClientID {
		t.Errorf("adopted chain points at client %d, want the latest issuer %d", clientID, exch.ClientID)
	}
	if delay != reclaimPersistRetryBase {
		t.Errorf("adopted chain delay = %v, want the fresh %v", delay, reclaimPersistRetryBase)
	}
}

// ---------------------------------------------------------------------------
// The roster reflects reclaimable state, and grace end retires what is left
// ---------------------------------------------------------------------------

// snapshotRecordKeys returns the identity strings the store currently holds.
func (s *spyRecoveryStore) snapshotRecordKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.records))
	for k := range s.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// confirmV40 registers and confirms a v4.0 client, returning its client ID.
func confirmV40(t *testing.T, sm *StateManager, id string, verf [8]byte) uint64 {
	t.Helper()
	res, err := sm.SetClientID(id, verf, CallbackInfo{}, "10.0.0.1:1", "uid:0")
	if err != nil {
		t.Fatalf("SetClientID(%s): %v", id, err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(%s): %v", id, err)
	}
	return res.ClientID
}

// A client that registers and never takes any state must leave nothing behind
// for the next boot to wait on. Before the record moved to the OPEN path every
// confirm wrote a row, so such a client sat on the roster forever.
func TestClientRecovery_StatelessClientIsNotOnTheBootRoster(t *testing.T) {
	spy := newSpyRecoveryStore()

	// First server instance: "opener" takes an open, "idler" only registers.
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	opener := confirmV40(t, sm, "opener", [8]byte{0xa1})
	confirmV40(t, sm, "idler", [8]byte{0xb2})
	if _, err := sm.OpenFile(opener, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	waitForRecordKeys(t, spy, "opener")
	if got := spy.snapshotRecordKeys(); len(got) != 1 || got[0] != "opener" {
		t.Fatalf("durable rows = %v, want only [opener]: a client holding nothing must write no row", got)
	}

	// Restart: only the client that held something is waited on.
	sm2 := NewStateManager(5*time.Second, 30*time.Second)
	sm2.SetClientRecoveryStore(spy, 2)
	if n := sm2.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("boot roster seeded %d clients, want 1 (only the one that held state)", n)
	}
	sm2.mu.RLock()
	roster := sm2.gracePeriod.expectedClientStrings
	onRoster := roster["idler"]
	sm2.mu.RUnlock()
	if onRoster {
		t.Fatal("idler held no state and must not be on the reclaim roster")
	}
}

// The grace window must end as soon as the clients that actually held state
// have reclaimed, instead of running its full duration waiting on a client that
// had nothing to reclaim in the first place.
func TestClientRecovery_GraceExitsEarlyWhenEveryStatefulClientReclaims(t *testing.T) {
	spy := newSpyRecoveryStore()
	verf := [8]byte{0xa1}

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	opener := confirmV40(t, sm, "opener", verf)
	confirmV40(t, sm, "idler", [8]byte{0xb2})
	if _, err := sm.OpenFile(opener, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitForRecordKeys(t, spy, "opener")

	// Restart with a 30s window: only an early exit can end it inside this test.
	sm2 := NewStateManager(5*time.Second, 30*time.Second)
	sm2.SetClientRecoveryStore(spy, 2)
	sm2.LoadClientRecovery(context.Background(), true)
	if !sm2.IsInGrace() {
		t.Fatal("restart with a stateful prior client must open the grace window")
	}

	back := confirmV40(t, sm2, "opener", verf)
	if _, err := sm2.OpenFile(back, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("CLAIM_PREVIOUS reclaim: %v", err)
	}

	if sm2.IsInGrace() {
		t.Fatal("grace must end early once every client that held state has reclaimed, " +
			"not run its full duration waiting on a client that held nothing")
	}
}

// A client that held state and never comes back has its row retired when the
// window ends, while a client that comes back and reclaims keeps its row and
// can still reclaim on the restart after that.
func TestClientRecovery_GraceEndRetiresRowsOfClientsThatNeverReturn(t *testing.T) {
	spy := newSpyRecoveryStore()
	returnerVerf := [8]byte{0xc1}

	// First instance: two clients, both holding an open.
	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	returner := confirmV40(t, sm, "returner", returnerVerf)
	goner := confirmV40(t, sm, "goner", [8]byte{0xd2})
	for _, id := range []uint64{returner, goner} {
		if _, err := sm.OpenFile(id, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
	}
	waitForRecordKeys(t, spy, "returner", "goner")

	// Second instance: only "returner" comes back. A short window so the hard
	// timer, not an early exit, is what ends it.
	sm2 := NewStateManager(5*time.Second, 200*time.Millisecond)
	sm2.SetClientRecoveryStore(spy, 2)
	if n := sm2.LoadClientRecovery(context.Background(), true); n != 2 {
		t.Fatalf("boot roster seeded %d clients, want 2", n)
	}
	back := confirmV40(t, sm2, "returner", returnerVerf)
	if _, err := sm2.OpenFile(back, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("CLAIM_PREVIOUS reclaim: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		keys := spy.snapshotRecordKeys()
		if len(keys) == 1 && keys[0] == "returner" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("durable rows = %v after grace ended, want only [returner]: "+
				"the row of a client that never returned was not retired", keys)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if sm2.IsInGrace() {
		t.Fatal("grace should have ended before the purge ran")
	}

	// The reclaim itself writes nothing, so the row keeps the reclaim-complete
	// mark and the third boot has nothing to wait on. Taking fresh state in this
	// epoch is what re-arms it.
	spy.mu.Lock()
	complete := spy.records["returner"].ReclaimComplete
	spy.mu.Unlock()
	if !complete {
		t.Fatal("the reclaim must leave the durable row marked reclaim-complete")
	}
	if _, err := sm2.OpenFile(back, []byte("owner"), 2, []byte("fh-new"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile(CLAIM_NULL) after grace: %v", err)
	}
	waitFor(t, "state taken in this epoch must re-arm the row with the current epoch", func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		r := spy.records["returner"]
		return r.ServerEpoch == 2 && !r.ReclaimComplete
	})

	sm3 := NewStateManager(5*time.Second, 30*time.Second)
	sm3.SetClientRecoveryStore(spy, 3)
	if n := sm3.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("third boot seeded %d clients, want 1 (only returner)", n)
	}
	back3 := confirmV40(t, sm3, "returner", returnerVerf)
	if _, err := sm3.OpenFile(back3, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_PREVIOUS); err != nil {
		t.Fatalf("returner must still be able to reclaim after surviving a purge: %v", err)
	}
}

// An incarnation that reclaims its opens writes no row, so the row it reclaimed
// against still carries the previous window's reclaim-complete mark. A lock
// taken afterwards is the first state it holds in this epoch, and must re-arm
// the row — otherwise the next restart does not wait on the client and it loses
// both the opens and the locks.
func TestClientRecovery_LockAfterReclaimRearmsTheRow(t *testing.T) {
	spy := newSpyRecoveryStore()
	verf := [8]byte{0xe1}
	fh := []byte("fh")

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	id := confirmV40(t, sm, "locker", verf)
	if _, err := sm.OpenFile(id, []byte("owner"), 1, fh, 3, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitForRecordKeys(t, spy, "locker")

	// Restart. The client reclaims its open and nothing else, which marks the
	// row reclaim-complete without rewriting it.
	sm2 := NewStateManager(5*time.Second, 150*time.Millisecond)
	sm2.SetLockManager(lock.NewManager())
	sm2.SetClientRecoveryStore(spy, 2)
	if n := sm2.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("boot roster seeded %d clients, want 1", n)
	}
	back := confirmV40(t, sm2, "locker", verf)
	open, err := sm2.OpenFile(back, []byte("owner"), 1, fh, 3, 0, types.CLAIM_PREVIOUS)
	if err != nil {
		t.Fatalf("CLAIM_PREVIOUS reclaim: %v", err)
	}
	waitFor(t, "the reclaim must mark the row complete without rewriting it", func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		r := spy.records["locker"]
		return r != nil && r.ReclaimComplete && r.ServerEpoch == 1
	})
	waitFor(t, "grace did not lift", func() bool { return !sm2.IsInGrace() })

	// A byte-range lock, and no further OPEN. This is the client's first state
	// in epoch 2, so the row has to come back onto the next boot's roster.
	if _, err := sm2.LockNew(context.Background(), back, []byte("lock-owner"), 1,
		&open.Stateid, 2, fh, types.WRITE_LT, 0, 100, false, 0); err != nil {
		t.Fatalf("LockNew: %v", err)
	}
	waitFor(t, "a lock taken after a reclaim must re-arm the durable row", func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		r := spy.records["locker"]
		return r != nil && !r.ReclaimComplete && r.ServerEpoch == 2
	})

	sm3 := NewStateManager(5*time.Second, 30*time.Second)
	sm3.SetClientRecoveryStore(spy, 3)
	if n := sm3.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("third boot seeded %d clients, want 1: the lock holder must be waited on", n)
	}
}

// A state operation that arrives while the first row write is in flight is
// suppressed by the latch, so it schedules no retry. If that in-flight write
// then fails, the client would hold state with no durable row and nothing left
// to write one — the next restart would never wait on it. The failed write must
// re-drive itself.
func TestClientRecovery_FailedWriteRetriesWhenAnOperationWasSuppressed(t *testing.T) {
	spy := newSpyRecoveryStore()
	gate := make(chan struct{})
	spy.mu.Lock()
	spy.putGate = gate
	spy.putFailuresLeft = 1
	spy.mu.Unlock()

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	id := confirmV40(t, sm, "suppressed", [8]byte{0xa1})

	// First OPEN starts the write and holds it in flight.
	if _, err := sm.OpenFile(id, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitFor(t, "the first write must be in flight", func() bool { return spy.snapshotPutCalls() == 1 })

	// Second OPEN lands while it is in flight: the latch suppresses it.
	if _, err := sm.OpenFile(id, []byte("owner"), 2, []byte("fh2"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile(2): %v", err)
	}
	waitFor(t, "the suppressed OPEN must be recorded", func() bool {
		sm.mu.RLock()
		defer sm.mu.RUnlock()
		rec := sm.clientRecordLocked(id)
		return rec != nil && rec.recoveryPersistWaiting
	})

	// Release the gate: the write fails, and with the second OPEN already
	// suppressed there is no later operation to retry it.
	close(gate)
	waitForRecordKeys(t, spy, "suppressed")
	if calls := spy.snapshotPutCalls(); calls < 2 {
		t.Fatalf("a failed write with a suppressed operation behind it must re-drive itself, got %d Put calls", calls)
	}
}

// A write that fails with NO operation suppressed leaves the latch clear, so
// the next state operation retries it. The re-drive must not double-write here.
func TestClientRecovery_FailedWriteDoesNotRedriveWithoutSuppression(t *testing.T) {
	spy := newSpyRecoveryStore()
	spy.mu.Lock()
	spy.putFailuresLeft = 1
	spy.mu.Unlock()

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetClientRecoveryStore(spy, 1)
	id := confirmV40(t, sm, "unsuppressed", [8]byte{0xa2})

	if _, err := sm.OpenFile(id, []byte("owner"), 1, []byte("fh"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	waitFor(t, "a failed write must leave the latch clear", func() bool {
		sm.mu.RLock()
		defer sm.mu.RUnlock()
		rec := sm.clientRecordLocked(id)
		return rec != nil && !rec.RecoveryPersisted
	})
	// The one failure is spent; nothing else should have been attempted.
	waitFor(t, "nothing was suppressed, so no re-drive", func() bool {
		return spy.snapshotPutCalls() == 1
	})
	time.Sleep(100 * time.Millisecond)
	if calls := spy.snapshotPutCalls(); calls != 1 {
		t.Fatalf("a failed write with nothing suppressed must not re-drive itself, got %d Put calls", calls)
	}

	// The next state operation is what retries it.
	if _, err := sm.OpenFile(id, []byte("owner"), 2, []byte("fh2"), 1, 0, types.CLAIM_NULL); err != nil {
		t.Fatalf("OpenFile(2): %v", err)
	}
	waitForRecordKeys(t, spy, "unsuppressed")
}

// A client whose only new state in the epoch arrives through LockExisting must
// still re-arm its durable row. LockExisting grants a fresh byte-range interval
// onto a lock state a reclaim already rebuilt, so it never passes through
// LockNew; a hook only on LockNew leaves ReclaimComplete set and the next
// restart skips the client.
func TestClientRecovery_LockExistingAfterReclaimRearmsTheRow(t *testing.T) {
	spy := newSpyRecoveryStore()
	verf := [8]byte{0xe2}
	fh := []byte("fh")

	sm := NewStateManager(5*time.Second, 30*time.Second)
	sm.SetLockManager(lock.NewManager())
	sm.SetClientRecoveryStore(spy, 1)
	id := confirmV40(t, sm, "existing-locker", verf)
	open, err := sm.OpenFile(id, []byte("owner"), 1, fh, 3, 0, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// A first lock, so a lock state exists for the reclaim to rebuild.
	if _, err := sm.LockNew(context.Background(), id, []byte("lock-owner"), 1,
		&open.Stateid, 2, fh, types.WRITE_LT, 0, 100, false, id); err != nil {
		t.Fatalf("LockNew: %v", err)
	}
	waitForRecordKeys(t, spy, "existing-locker")

	// Restart. The client reclaims its open and its lock.
	sm2 := NewStateManager(5*time.Second, 150*time.Millisecond)
	sm2.SetLockManager(lock.NewManager())
	sm2.SetClientRecoveryStore(spy, 2)
	if n := sm2.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("boot roster seeded %d clients, want 1", n)
	}
	back := confirmV40(t, sm2, "existing-locker", verf)
	reopen, err := sm2.OpenFile(back, []byte("owner"), 1, fh, 3, 0, types.CLAIM_PREVIOUS)
	if err != nil {
		t.Fatalf("CLAIM_PREVIOUS reclaim: %v", err)
	}
	reLock, err := sm2.LockNew(context.Background(), back, []byte("lock-owner"), 1,
		&reopen.Stateid, 2, fh, types.WRITE_LT, 0, 100, true, back)
	if err != nil {
		t.Fatalf("reclaiming LockNew: %v", err)
	}
	waitFor(t, "the reclaim must mark the row complete without rewriting it", func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		r := spy.records["existing-locker"]
		return r != nil && r.ReclaimComplete && r.ServerEpoch == 1
	})
	waitFor(t, "grace did not lift", func() bool { return !sm2.IsInGrace() })

	// A second byte range through the EXISTING lock-owner. No further OPEN and
	// no LockNew: this is the client's first new state in epoch 2.
	if _, err := sm2.LockExisting(context.Background(), &reLock.Stateid, 2,
		fh, types.WRITE_LT, 200, 100, false, back); err != nil {
		t.Fatalf("LockExisting: %v", err)
	}
	waitFor(t, "a LockExisting after a reclaim must re-arm the durable row", func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		r := spy.records["existing-locker"]
		return r != nil && !r.ReclaimComplete && r.ServerEpoch == 2
	})

	sm3 := NewStateManager(5*time.Second, 30*time.Second)
	sm3.SetClientRecoveryStore(spy, 3)
	if n := sm3.LoadClientRecovery(context.Background(), true); n != 1 {
		t.Fatalf("third boot seeded %d clients, want 1: the LockExisting holder must be waited on", n)
	}
}
