package handlers

import (
	"context"
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
	victim := newTestPipeRead(1, 1, 10, 100, 1000, done)
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

// The inline-retry fallback parks its cancel func in h.pendingLocks rather than
// in a registry, and is taken whenever async parking is unavailable. Its key
// carries the ConnID for the same reason the registries' do — and here the
// collision needs no attacker: MessageIDs are per-connection sequence numbers
// and every connection's window starts near zero, so two clients blocking on
// locks routinely hold the same MessageID at the same time.
//
// This drives the real Handler.Cancel dispatch path rather than the map.
func TestCancel_InlineBlockingLockIsConnectionScoped(t *testing.T) {
	h := &Handler{}
	const messageID = 100

	lockCtx, cancel := context.WithCancel(context.Background())
	h.pendingLocks.Store(lockMsgKey{ConnID: 1, MessageID: messageID}, context.CancelFunc(cancel))

	cancelBody := []byte{4, 0, 0, 0} // StructureSize = 4, Reserved

	// A CANCEL from another connection carrying the same MessageID.
	if _, err := h.Cancel(&SMBHandlerContext{ConnID: 2, MessageID: messageID}, cancelBody); err != nil {
		t.Fatalf("Cancel(conn 2): %v", err)
	}
	select {
	case <-lockCtx.Done():
		t.Fatal("a CANCEL from another connection tore down the inline LOCK")
	default:
	}
	if _, ok := h.pendingLocks.Load(lockMsgKey{ConnID: 1, MessageID: messageID}); !ok {
		t.Fatal("inline LOCK was unregistered by another connection's CANCEL")
	}

	// The owning connection still cancels it.
	if _, err := h.Cancel(&SMBHandlerContext{ConnID: 1, MessageID: messageID}, cancelBody); err != nil {
		t.Fatalf("Cancel(conn 1): %v", err)
	}
	select {
	case <-lockCtx.Done():
	default:
		t.Fatal("the owning connection's CANCEL did not tear down the inline LOCK")
	}
	if _, ok := h.pendingLocks.Load(lockMsgKey{ConnID: 1, MessageID: messageID}); ok {
		t.Fatal("inline LOCK still registered after its own connection cancelled it")
	}
}
