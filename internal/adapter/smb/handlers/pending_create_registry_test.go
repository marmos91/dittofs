package handlers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// newTestPendingCreate builds a PendingCreate with a no-op callback and a
// context-cancel pair whose cancel we expose on the returned entry. The
// callback increments calls so tests can assert invocation counts.
func newTestPendingCreate(connID, sessionID, messageID, asyncId uint64, calls *atomic.Int32) *PendingCreate {
	_, cancel := context.WithCancel(context.Background())
	return &PendingCreate{
		ConnID:    connID,
		SessionID: sessionID,
		MessageID: messageID,
		AsyncId:   asyncId,
		Cancel:    cancel,
		Callback: func(_, _, _ uint64, _ types.Status, _ []byte) error {
			calls.Add(1)
			return nil
		},
	}
}

func TestPendingCreateRegistry_RegisterThenUnregister(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	p := newTestPendingCreate(1, 100, 42, 7, &calls)

	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}

	got := r.Unregister(7)
	if got != p {
		t.Errorf("Unregister returned %v, want %v", got, p)
	}
	if r.Len() != 0 {
		t.Errorf("Len after Unregister = %d, want 0", r.Len())
	}
	// Unregister does NOT fire Cancel or Callback — that's the resume-goroutine
	// contract: deliver first, then Unregister.
	if calls.Load() != 0 {
		t.Errorf("Callback fired %d times, want 0", calls.Load())
	}
}

func TestPendingCreateRegistry_UnregisterByMessageID(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	p := newTestPendingCreate(1, 100, 42, 7, &calls)

	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Wrong ConnID must NOT match — MessageID is per-connection.
	if got := r.UnregisterByMessageID(2, 42); got != nil {
		t.Errorf("UnregisterByMessageID(wrong conn) = %v, want nil", got)
	}
	// Correct (ConnID, MessageID) matches and cancels.
	if got := r.UnregisterByMessageID(1, 42); got != p {
		t.Errorf("UnregisterByMessageID = %v, want %v", got, p)
	}
	if r.Len() != 0 {
		t.Errorf("Len after UnregisterByMessageID = %d, want 0", r.Len())
	}
	// A second Unregister must be idempotent.
	if got := r.Unregister(7); got != nil {
		t.Errorf("Unregister after UnregisterByMessageID = %v, want nil", got)
	}
}

func TestPendingCreateRegistry_UnregisterByAsyncId(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	p := newTestPendingCreate(1, 100, 42, 7, &calls)

	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.UnregisterByAsyncId(99); got != nil {
		t.Errorf("UnregisterByAsyncId(missing) = %v, want nil", got)
	}
	if got := r.UnregisterByAsyncId(7); got != p {
		t.Errorf("UnregisterByAsyncId = %v, want %v", got, p)
	}
	if r.Len() != 0 {
		t.Errorf("Len after UnregisterByAsyncId = %d, want 0", r.Len())
	}
}

func TestPendingCreateRegistry_UnregisterAllForSession(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	a := newTestPendingCreate(1, 100, 42, 7, &calls)
	b := newTestPendingCreate(1, 100, 43, 8, &calls)
	c := newTestPendingCreate(1, 200, 44, 9, &calls) // different session

	for _, p := range []*PendingCreate{a, b, c} {
		if err := r.Register(p); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	removed := r.UnregisterAllForSession(100)
	if len(removed) != 2 {
		t.Fatalf("UnregisterAllForSession(100) returned %d entries, want 2", len(removed))
	}
	// Session 200's entry must survive.
	if r.Len() != 1 {
		t.Errorf("Len after UnregisterAllForSession = %d, want 1 (session 200 remains)", r.Len())
	}
	if got := r.Unregister(9); got != c {
		t.Errorf("session 200 entry not found post-teardown: got %v, want %v", got, c)
	}
}

func TestPendingCreateRegistry_RegisterRejectsDuplicateMessageID(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	a := newTestPendingCreate(1, 100, 42, 7, &calls)
	b := newTestPendingCreate(1, 100, 42, 8, &calls) // same (ConnID, MessageID), new AsyncId

	if err := r.Register(a); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	err := r.Register(b)
	if err != ErrDuplicateMessageID {
		t.Fatalf("Register b: got %v, want ErrDuplicateMessageID", err)
	}
	// Original entry must be intact.
	if got := r.Unregister(7); got != a {
		t.Errorf("Original entry not intact: got %v, want %v", got, a)
	}
}

func TestPendingCreateRegistry_RegisterRejectsDuplicateAsyncId(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	a := newTestPendingCreate(1, 100, 42, 7, &calls)
	b := newTestPendingCreate(2, 100, 43, 7, &calls) // different conn+msgid, same AsyncId

	if err := r.Register(a); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	err := r.Register(b)
	if err != ErrDuplicateAsyncId {
		t.Fatalf("Register b: got %v, want ErrDuplicateAsyncId", err)
	}
	if got := r.Unregister(7); got != a {
		t.Errorf("Original entry not intact: got %v, want %v", got, a)
	}
}

func TestPendingCreateRegistry_RegisterOverflow(t *testing.T) {
	r := NewPendingCreateRegistry()
	r.reg.maxOps = 2 // shrink for test speed
	var calls atomic.Int32

	if err := r.Register(newTestPendingCreate(1, 1, 1, 1, &calls)); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(newTestPendingCreate(1, 1, 2, 2, &calls)); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	err := r.Register(newTestPendingCreate(1, 1, 3, 3, &calls))
	if err == nil {
		t.Fatalf("third Register expected ErrTooManyPendingCreates, got nil")
	}
	if r.Len() != 2 {
		t.Errorf("Len after rejected Register = %d, want 2", r.Len())
	}
}

func TestPendingCreateRegistry_UnregisterByMessageID_DifferentConnsCollidingMessageID(t *testing.T) {
	// MessageIDs are scoped per connection in SMB2. Two connections picking the
	// same MessageID value must produce two distinct registry entries, and a
	// CANCEL on conn=1 must not affect conn=2's entry.
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	// Different AsyncIds, different ConnIDs, same MessageID.
	p1 := newTestPendingCreate(1, 100, 42, 7, &calls)
	p2 := newTestPendingCreate(2, 100, 42, 8, &calls)

	if err := r.Register(p1); err != nil {
		t.Fatalf("Register p1: %v", err)
	}
	if err := r.Register(p2); err != nil {
		t.Fatalf("Register p2: %v", err)
	}

	if got := r.UnregisterByMessageID(1, 42); got != p1 {
		t.Errorf("UnregisterByMessageID(conn=1) = %v, want p1", got)
	}
	if r.Len() != 1 {
		t.Errorf("Len after conn=1 cancel = %d, want 1", r.Len())
	}
	if got := r.UnregisterByMessageID(2, 42); got != p2 {
		t.Errorf("UnregisterByMessageID(conn=2) = %v, want p2", got)
	}
}

// newGatedPendingCreate wires a `started` channel onto the test entry so the
// gate behaviour matches what parkCreateOnLeaseBreak actually constructs at
// runtime.
func newGatedPendingCreate(asyncId uint64, calls *atomic.Int32) *PendingCreate {
	p := newTestPendingCreate(1, 100, 42, asyncId, calls)
	p.started = make(chan struct{})
	return p
}

// TestPendingCreateRegistry_MarkStartedClosesGate verifies the public
// MarkStarted entry point unblocks a goroutine waiting on PendingCreate.started.
// This is the race that smb2.compound.compound-break exposes when the resume
// goroutine fires before the compound dispatcher has had a chance to swap
// the Callback for the continue-compound wrapper.
func TestPendingCreateRegistry_MarkStartedClosesGate(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	p := newGatedPendingCreate(7, &calls)

	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	released := make(chan struct{})
	go func() {
		<-p.started
		close(released)
	}()

	// MarkStarted must wake the waiter.
	if !r.MarkStarted(7) {
		t.Fatalf("MarkStarted(7) = false, want true (entry registered)")
	}

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("MarkStarted did not unblock waiter within 1s")
	}

	// Second call is idempotent — does not panic on already-closed channel.
	if !r.MarkStarted(7) {
		t.Errorf("second MarkStarted(7) = false, want true (still registered)")
	}
}

// TestPendingCreateRegistry_CancelReleasesGate verifies CANCEL-style
// unregistration also closes the gate so a resume goroutine racing the
// cancel cannot deadlock waiting for a dispatcher MarkStarted that never
// arrives (CANCEL bypasses the dispatcher).
func TestPendingCreateRegistry_CancelReleasesGate(t *testing.T) {
	cases := []struct {
		name   string
		cancel func(*PendingCreateRegistry, *PendingCreate)
	}{
		{
			name: "UnregisterByMessageID",
			cancel: func(r *PendingCreateRegistry, p *PendingCreate) {
				r.UnregisterByMessageID(p.ConnID, p.MessageID)
			},
		},
		{
			name: "UnregisterByAsyncId",
			cancel: func(r *PendingCreateRegistry, p *PendingCreate) {
				r.UnregisterByAsyncId(p.AsyncId)
			},
		},
		{
			name: "UnregisterAllForSession",
			cancel: func(r *PendingCreateRegistry, p *PendingCreate) {
				r.UnregisterAllForSession(p.SessionID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewPendingCreateRegistry()
			var calls atomic.Int32
			p := newGatedPendingCreate(7, &calls)
			if err := r.Register(p); err != nil {
				t.Fatalf("Register: %v", err)
			}

			released := make(chan struct{})
			go func() {
				<-p.started
				close(released)
			}()

			tc.cancel(r, p)

			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatalf("%s did not release started gate within 1s", tc.name)
			}
		})
	}
}

// TestPendingCreateRegistry_MarkStartedAfterUnregister exercises the
// idempotency of the gate path: once the entry has been removed, MarkStarted
// returns false (the dispatcher already lost the race to CANCEL/teardown,
// which has its own callback fan-out).
func TestPendingCreateRegistry_MarkStartedAfterUnregister(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	p := newGatedPendingCreate(7, &calls)
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister(7)
	if r.MarkStarted(7) {
		t.Errorf("MarkStarted after Unregister = true, want false")
	}
}
