package handlers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// newTestPendingLock builds a PendingLock with a counted-Cancel for assertions.
func newTestPendingLock(connID, sessionID, messageID, asyncId uint64, treeID uint32, cancelCount *atomic.Int32) *PendingLock {
	_, cancel := context.WithCancel(context.Background())
	return &PendingLock{
		ConnID:    connID,
		SessionID: sessionID,
		TreeID:    treeID,
		MessageID: messageID,
		AsyncId:   asyncId,
		OwnerID:   "owner",
		Cancel: func() {
			if cancelCount != nil {
				cancelCount.Add(1)
			}
			cancel()
		},
		Callback: func(uint64, uint64, uint64, types.Status, []byte) error { return nil },
	}
}

func TestPendingLockRegistry_RegisterAndUnregister(t *testing.T) {
	r := NewPendingLockRegistry()
	p := newTestPendingLock(1, 10, 100, 1000, 50, nil)
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	if got := r.Unregister(p.AsyncId); got == nil {
		t.Fatal("Unregister returned nil")
	}
	if r.Len() != 0 {
		t.Fatalf("Len after Unregister = %d, want 0", r.Len())
	}
}

func TestPendingLockRegistry_DuplicateRejection(t *testing.T) {
	r := NewPendingLockRegistry()
	p1 := newTestPendingLock(1, 10, 100, 1000, 50, nil)
	if err := r.Register(p1); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Same (ConnID, MessageID) → ErrDuplicateLockMessageID.
	dupMsg := newTestPendingLock(1, 11, 100, 1001, 51, nil)
	if err := r.Register(dupMsg); err != ErrDuplicateLockMessageID {
		t.Fatalf("dup msg: got %v, want ErrDuplicateLockMessageID", err)
	}

	// Same AsyncId → ErrDuplicateLockAsyncId.
	dupAsync := newTestPendingLock(2, 10, 200, 1000, 50, nil)
	if err := r.Register(dupAsync); err != ErrDuplicateLockAsyncId {
		t.Fatalf("dup asyncid: got %v, want ErrDuplicateLockAsyncId", err)
	}

	// Original entry still present and untouched.
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (no eviction)", r.Len())
	}
}

func TestPendingLockRegistry_UnregisterByMessageIDFiresCancel(t *testing.T) {
	r := NewPendingLockRegistry()
	var cancels atomic.Int32
	p := newTestPendingLock(1, 10, 100, 1000, 50, &cancels)
	_ = r.Register(p)

	if got := r.UnregisterByMessageID(1, 100); got == nil {
		t.Fatal("UnregisterByMessageID returned nil")
	}
	if cancels.Load() != 1 {
		t.Fatalf("cancels = %d, want 1", cancels.Load())
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
}

func TestPendingLockRegistry_UnregisterAllForTree(t *testing.T) {
	r := NewPendingLockRegistry()
	var cancels atomic.Int32
	// Two on tree 50, one on tree 51.
	_ = r.Register(newTestPendingLock(1, 10, 100, 1000, 50, &cancels))
	_ = r.Register(newTestPendingLock(1, 10, 101, 1001, 50, &cancels))
	_ = r.Register(newTestPendingLock(1, 10, 102, 1002, 51, &cancels))

	got := r.UnregisterAllForTree(50)
	if len(got) != 2 {
		t.Fatalf("UnregisterAllForTree returned %d, want 2", len(got))
	}
	if cancels.Load() != 2 {
		t.Fatalf("cancels = %d, want 2", cancels.Load())
	}
	if r.Len() != 1 {
		t.Fatalf("Len after = %d, want 1 (tree 51 entry untouched)", r.Len())
	}
}

func TestPendingLockRegistry_UnregisterAllForSession(t *testing.T) {
	r := NewPendingLockRegistry()
	var cancels atomic.Int32
	_ = r.Register(newTestPendingLock(1, 10, 100, 1000, 50, &cancels))
	_ = r.Register(newTestPendingLock(1, 10, 101, 1001, 51, &cancels))
	_ = r.Register(newTestPendingLock(1, 11, 102, 1002, 52, &cancels))

	got := r.UnregisterAllForSession(10)
	if len(got) != 2 {
		t.Fatalf("UnregisterAllForSession returned %d, want 2", len(got))
	}
	if cancels.Load() != 2 {
		t.Fatalf("cancels = %d, want 2", cancels.Load())
	}
	if r.Len() != 1 {
		t.Fatalf("Len after = %d, want 1 (session 11 entry untouched)", r.Len())
	}
}

func TestPendingLockRegistry_OverflowRejected(t *testing.T) {
	r := NewPendingLockRegistry()
	r.maxOps = 2
	_ = r.Register(newTestPendingLock(1, 10, 100, 1000, 50, nil))
	_ = r.Register(newTestPendingLock(1, 10, 101, 1001, 50, nil))
	overflow := newTestPendingLock(1, 10, 102, 1002, 50, nil)
	if err := r.Register(overflow); err != ErrTooManyPendingLocks {
		t.Fatalf("overflow: got %v, want ErrTooManyPendingLocks", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (overflow not registered)", r.Len())
	}
}
