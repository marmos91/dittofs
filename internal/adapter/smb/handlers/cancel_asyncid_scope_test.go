package handlers

import (
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// A CANCEL carrying SMB2_FLAGS_ASYNC_COMMAND is resolved by the AsyncId in its
// header, and that AsyncId is whatever the peer put on the wire. MS-SMB2
// 3.3.5.16 confines the search to Connection.AsyncCommandList — the list of the
// connection the CANCEL arrived on — and 3.3.1.13 only guarantees an AsyncId to
// be unique "among all requests currently processed asynchronously on a
// specified SMB2 transport connection". So an AsyncId that resolves an entry
// parked on a different connection must resolve nothing: the victim's request
// stays parked and completes on its own terms.
//
// Each case registers a victim on connection 1 and fires the cancel from
// connection 2 with the victim's AsyncId.

func TestPendingCreateRegistry_UnregisterByAsyncIdIsConnectionScoped(t *testing.T) {
	r := NewPendingCreateRegistry()
	var calls atomic.Int32
	victim := newTestPendingCreate(1, 10, 100, 7, &calls)
	if err := r.Register(victim); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := r.UnregisterByAsyncId(2, 7); got != nil {
		t.Errorf("UnregisterByAsyncId(conn 2, victim's asyncId) = %v, want nil", got)
	}
	if r.Len() != 1 {
		t.Errorf("Len after foreign cancel = %d, want 1 (victim still parked)", r.Len())
	}

	// The owning connection still cancels it.
	if got := r.UnregisterByAsyncId(1, 7); got != victim {
		t.Errorf("UnregisterByAsyncId(conn 1) = %v, want victim", got)
	}
}

func TestPendingLockRegistry_UnregisterByAsyncIdIsConnectionScoped(t *testing.T) {
	r := NewPendingLockRegistry()
	var cancels atomic.Int32
	victim := newTestPendingLock(1, 10, 100, 1000, 50, &cancels)
	if err := r.Register(victim); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := r.UnregisterByAsyncId(2, 1000); got != nil {
		t.Errorf("UnregisterByAsyncId(conn 2, victim's asyncId) = %v, want nil", got)
	}
	if n := cancels.Load(); n != 0 {
		t.Errorf("victim's Cancel fired %d times on a foreign cancel, want 0", n)
	}
	if r.Len() != 1 {
		t.Errorf("Len after foreign cancel = %d, want 1 (victim still parked)", r.Len())
	}

	if got := r.UnregisterByAsyncId(1, 1000); got != victim {
		t.Errorf("UnregisterByAsyncId(conn 1) = %v, want victim", got)
	}
	if n := cancels.Load(); n != 1 {
		t.Errorf("Cancel fired %d times on the owning cancel, want 1", n)
	}
}

func TestPipeReadRegistry_UnregisterByAsyncIdIsConnectionScoped(t *testing.T) {
	r := NewPipeReadRegistry()
	done := make(chan types.Status, 1)
	victim := newTestPipeRead(1, 10, 100, 1000, done)
	victim.ConnID = 1
	r.Register(victim)

	if got := r.UnregisterByAsyncId(2, 1000); got != nil {
		t.Errorf("UnregisterByAsyncId(conn 2, victim's asyncId) = %v, want nil", got)
	}
	// The MessageID key is per-connection too, so the same cancel arriving
	// without the async flag must miss as well.
	if got := r.UnregisterByMessageID(2, 100); got != nil {
		t.Errorf("UnregisterByMessageID(conn 2, victim's messageID) = %v, want nil", got)
	}
	if got := r.UnregisterByAsyncId(1, 1000); got != victim {
		t.Errorf("UnregisterByAsyncId(conn 1) = %v, want victim", got)
	}
}

func TestNotifyRegistry_UnregisterByAsyncIdIsConnectionScoped(t *testing.T) {
	r := newTestNotifyRegistry()
	victim := &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        10,
		ConnID:           1,
		MessageID:        100,
		AsyncId:          777,
		WatchPath:        "/dir",
		ShareName:        "share",
		CompletionFilter: FileNotifyChangeDirName,
	}
	mustRegister(t, r, victim)

	if got := r.UnregisterByAsyncId(2, 777); got != nil {
		t.Errorf("UnregisterByAsyncId(conn 2, victim's asyncId) = %v, want nil", got)
	}
	if got := r.UnregisterByAsyncId(1, 777); got != victim {
		t.Errorf("UnregisterByAsyncId(conn 1) = %v, want victim", got)
	}
}
