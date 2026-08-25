package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// capturingNotifier records the LEASE_BREAK_NOTIFICATION values the break
// dispatch path hands to the transport, including the NewEpoch that goes on
// the wire.
type capturingNotifier struct {
	mu     sync.Mutex
	breaks []capturedBreak
}

type capturedBreak struct {
	SessionID    uint64
	LeaseKey     [16]byte
	CurrentState uint32
	NewState     uint32
	Epoch        uint16
}

func (n *capturingNotifier) SendLeaseBreak(sessionID uint64, leaseKey [16]byte, currentState, newState uint32, epoch uint16) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.breaks = append(n.breaks, capturedBreak{
		SessionID:    sessionID,
		LeaseKey:     leaseKey,
		CurrentState: currentState,
		NewState:     newState,
		Epoch:        epoch,
	})
	return nil
}

func (n *capturingNotifier) forSession(sessionID uint64) []capturedBreak {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []capturedBreak
	for _, b := range n.breaks {
		if b.SessionID == sessionID {
			out = append(out, b)
		}
	}
	return out
}

// TestLeaseBreakNewEpoch_OtherClientSameKeyDoesNotSeedIt drives two V2 lease
// clients through the real grant path and reads the NewEpoch the break
// notification for the first one actually carries.
//
// Two clients may present the same 16-byte lease key value on different files
// of one share: the uniqueness rule is per-(client, file), so the value alone
// is not an identity. Each grant seeds the server's epoch counter from the
// epoch that client asked for. A client's break notification must therefore
// carry an epoch that follows from its own grants — a NewEpoch derived from
// another client's counter is one the receiving client can trace to no request
// of its own.
func TestLeaseBreakNewEpoch_OtherClientSameKeyDoesNotSeedIt(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	notifier := &capturingNotifier{}
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, notifier)
	mgr.RegisterBreakCallbacks(lease.NewSMBBreakHandler(leaseMgr, notifier))

	ctx := context.Background()
	const shareName = "share1"
	leaseKey := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	const rh = lock.LeaseStateRead | lock.LeaseStateHandle
	const epochA uint16 = 0x0100
	const epochB uint16 = 0x7000

	respA, err := ProcessLeaseCreateContext(ctx, leaseMgr, encodeV2LeaseContext(leaseKey, rh, epochA),
		lock.FileHandle("file-A"), 1, [16]byte{0xA1}, "smb:1", shareName, false, false, false)
	if err != nil {
		t.Fatalf("client A CREATE: %v", err)
	}
	if respA.Epoch != epochA+1 {
		t.Fatalf("client A response epoch = 0x%x, want 0x%x", respA.Epoch, epochA+1)
	}

	// Client B's first grant on a different file seeds its own counter well
	// above A's.
	respB, err := ProcessLeaseCreateContext(ctx, leaseMgr, encodeV2LeaseContext(leaseKey, rh, epochB),
		lock.FileHandle("file-B"), 2, [16]byte{0xB2}, "smb:2", shareName, false, false, false)
	if err != nil {
		t.Fatalf("client B CREATE: %v", err)
	}
	if respB.Epoch != epochB+1 {
		t.Fatalf("client B response epoch = 0x%x, want 0x%x", respB.Epoch, epochB+1)
	}

	// A conflicting open on A's file breaks A's lease.
	if err := mgr.BreakLeasesOnOpenConflict("file-A", &lock.LockOwner{ClientID: "smb:9"}, lock.BreakReasonDestructive); err != nil {
		t.Fatalf("break client A's lease: %v", err)
	}

	sent := notifier.forSession(1)
	if len(sent) != 1 {
		t.Fatalf("client A received %d break notifications, want 1", len(sent))
	}
	// Per MS-SMB2 §3.3.4.7 the server sets NewEpoch = Epoch + 1 when it
	// dispatches the break, where Epoch is the value this client last saw in
	// its own RqLs response.
	if want := respA.Epoch + 1; sent[0].Epoch != want {
		t.Errorf("client A break NewEpoch = 0x%x, want 0x%x — the epoch came from client B's counter (B was granted 0x%x on another file under the same key value)",
			sent[0].Epoch, want, respB.Epoch)
	}

	// Client B's lease is untouched by a break on another file.
	if got := notifier.forSession(2); len(got) != 0 {
		t.Errorf("client B received %d break notifications for a break on another client's file", len(got))
	}
	if _, epoch, found := leaseMgr.GetLeaseState(ctx, lock.FileHandle("file-B"), shareName, leaseKey); !found || epoch < respB.Epoch {
		t.Errorf("client B lease epoch = 0x%x found=%v, want >= 0x%x", epoch, found, respB.Epoch)
	}
}
