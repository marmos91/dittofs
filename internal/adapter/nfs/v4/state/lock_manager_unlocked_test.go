package state

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// These tests pin two properties of every StateManager path that calls the
// cross-protocol lock manager: the server-wide state mutex is not held across
// the call, and the state resolved before the call is re-validated before the
// result is committed.
//
// Both are asserted from inside the lock manager itself. A hook runs while the
// manager call is in flight and, from another goroutine, performs an operation
// that needs sm.mu. If sm.mu were still held the hook would block until the
// timeout and fail the test; because it is not, the hook is free to mutate the
// very state the caller resolved, which is exactly the race the re-validation
// has to catch.

// hookedLockManager wraps a realLM lock manager and runs a hook at the point one
// chosen operation reaches it.
type hookedLockManager struct {
	lock.LockManager
	onAddLock    func()
	onRemoveLock func()
	onGrantDeleg func()
	fired        bool
}

// fire runs a hook at most once. The state-freeing paths these hooks trigger
// call the lock manager themselves, so an unguarded hook would re-enter.
func (h *hookedLockManager) fire(hook func()) {
	if hook == nil || h.fired {
		return
	}
	h.fired = true
	hook()
}

func (h *hookedLockManager) AddUnifiedLock(handleKey string, l *lock.UnifiedLock) error {
	h.fire(h.onAddLock)
	return h.LockManager.AddUnifiedLock(handleKey, l)
}

func (h *hookedLockManager) RemoveUnifiedLock(handleKey string, owner lock.LockOwner, offset, length uint64) error {
	h.fire(h.onRemoveLock)
	return h.LockManager.RemoveUnifiedLock(handleKey, owner, offset, length)
}

func (h *hookedLockManager) GrantDelegation(handleKey string, d *lock.Delegation) error {
	h.fire(h.onGrantDeleg)
	return h.LockManager.GrantDelegation(handleKey, d)
}

// mutateUnderStateMutex runs fn on another goroutine and waits for it, failing
// the test if it has not finished in time. fn must be something that needs
// sm.mu, so a caller still holding the mutex is reported as a timeout rather
// than deadlocking the test binary.
func mutateUnderStateMutex(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s blocked while the lock manager was being called: the state mutex is still held across it", what)
	}
}

// TestLockNew_LockManagerCalledWithoutStateMutex proves LOCK reaches the lock
// manager with sm.mu released, and that a LOCK whose state is freed in that
// window is refused instead of committing a seqid bump onto dead state.
func TestLockNew_LockManagerCalledWithoutStateMutex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	realLM := lock.NewManager()

	var clientID uint64
	lm := &hookedLockManager{LockManager: realLM}
	lm.onAddLock = func() {
		mutateUnderStateMutex(t, "the lease sweeper", func() { sm.onLeaseExpired(clientID) })
	}
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	res, err := sm.LockNew(context.Background(),
		clientID, []byte("owner-a"), 1,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err == nil {
		t.Fatalf("LOCK committed against state freed while the lock manager was called: %+v", res)
	}

	// The byte-range lock reached the manager before the state went away, so
	// the refusal has to hand it back; otherwise nothing is left to release it.
	if locks := realLM.ListUnifiedLocks(string(fileHandle)); len(locks) != 0 {
		t.Fatalf("refused LOCK stranded %d lock(s) in the lock manager", len(locks))
	}
}

// TestUnlockFile_LockManagerCalledWithoutStateMutex is the LOCKU counterpart:
// the release reaches the manager unlocked, and the seqid bump is not committed
// onto state freed in that window.
func TestUnlockFile_LockManagerCalledWithoutStateMutex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	realLM := lock.NewManager()

	lm := &hookedLockManager{LockManager: realLM}
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	locked, err := sm.LockNew(context.Background(),
		clientID, []byte("owner-a"), 1,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil || locked.Denied != nil {
		t.Fatalf("setup LOCK failed: err=%v denied=%v", err, locked)
	}

	// Hooked only now: the setup LOCK above must reach the manager unhooked.
	lm.onRemoveLock = func() {
		mutateUnderStateMutex(t, "the lease sweeper", func() { sm.onLeaseExpired(clientID) })
	}
	if _, err := sm.UnlockFile(&locked.Stateid, 2, types.WRITE_LT, 0, 100); err == nil {
		t.Fatal("LOCKU committed a seqid bump against state freed while the lock manager was called")
	}
}

// TestGrantDelegation_LockManagerCalledWithoutStateMutex proves OPEN reaches the
// lock manager's delegation grant with sm.mu released, and that a grant which no
// longer fits the delegation budget is handed back rather than published.
func TestGrantDelegation_LockManagerCalledWithoutStateMutex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(1)
	realLM := lock.NewManager()

	fileHandle := []byte("/export:deleg-file")
	lm := &hookedLockManager{LockManager: realLM}
	lm.onGrantDeleg = func() {
		// Take the last of the budget while the manager call is in flight.
		mutateUnderStateMutex(t, "a competing delegation", func() {
			sm.mu.Lock()
			defer sm.mu.Unlock()
			sm.delegByOther[[types.NFS4_OTHER_SIZE]byte{0xff}] = &DelegationState{ClientID: 999}
		})
	}
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })

	clientID, _, _, _ := setupClientAndOpenState(t, sm)

	if deleg := sm.GrantDelegation(clientID, fileHandle, types.OPEN_DELEGATE_READ); deleg != nil {
		t.Fatal("delegation published past the budget that was taken while the lock manager was called")
	}
	if delegs := realLM.ListDelegations(string(fileHandle)); len(delegs) != 0 {
		t.Fatalf("refused delegation stranded %d delegation(s) in the lock manager", len(delegs))
	}
}

// TestGrantDirDelegation_LockManagerCalledWithoutStateMutex is the directory
// delegation counterpart. Its recommit check re-runs the whole admission
// decision, so the window is closed here by expiring the client's lease.
func TestGrantDirDelegation_LockManagerCalledWithoutStateMutex(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	realLM := lock.NewManager()

	dirFH := []byte("/export:deleg-dir")
	var clientID uint64
	lm := &hookedLockManager{LockManager: realLM}
	lm.onGrantDeleg = func() {
		mutateUnderStateMutex(t, "RemoveClient", func() { sm.RemoveClient(clientID) })
	}
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })

	clientID, _, _, _ = setupClientAndOpenState(t, sm)

	if _, err := sm.GrantDirDelegation(clientID, dirFH, 0xffff); err == nil {
		t.Fatal("directory delegation published for a client removed while the lock manager was called")
	}
	if delegs := realLM.ListDelegations(string(dirFH)); len(delegs) != 0 {
		t.Fatalf("refused delegation stranded %d delegation(s) in the lock manager", len(delegs))
	}
}
